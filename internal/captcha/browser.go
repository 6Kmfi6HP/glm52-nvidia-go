package captcha

import (
	"context"
	"fmt"
	"os"
	"sync"
	"time"

	"github.com/chromedp/chromedp"
)

// Browser owns one long-lived headless Chrome and one sticky playground tab.
//
// chromedp allocates the OS Chrome process on the first Run and ties it to that
// context (exec.CommandContext). Canceling a timeout used for that first Run
// kills Chrome — so we keep browserCtx alive until Close.
//
// After the playground is warm, Extract only calls hcaptcha.reset+execute
// (~300ms steady-state in cmd/captchaopt) instead of a full Navigate (~6–10s).
type Browser struct {
	browser context.Context
	cancel  context.CancelFunc // allocator
	bCancel context.CancelFunc // browser tab / process owner

	mu     sync.Mutex
	closed bool
	warmed bool
}

// NewBrowser starts a shared Chrome process and warms the playground page.
// Call Close when done.
//
// Container hints:
//   - CHROME_PATH: absolute path to chromium/chrome binary
//   - CHROMEDP_NO_SANDBOX=1: add --no-sandbox and --disable-dev-shm-usage
//   - CHROMEDP_ALLOW_IMAGES=1: re-enable image loading (default is off). Pictures
//     are unnecessary for this invisible hCaptcha widget, so images are blocked
//     by default to cut per-navigate RAM/bandwidth; re-enable only if a future
//     site change makes image decode required for token extraction.
func NewBrowser(parent context.Context) (*Browser, error) {
	allocOpts := append(chromedp.DefaultExecAllocatorOptions[:],
		chromedp.Flag("headless", true),
		chromedp.Flag("disable-blink-features", "AutomationControlled"),
		chromedp.Flag("enable-automation", false),
		chromedp.Flag("no-first-run", true),
		chromedp.Flag("no-default-browser-check", true),
		// Resource-saving flags that do not affect hCaptcha token extraction:
		// the tab only needs JS to fire hcaptcha.execute() and read one attribute.
		chromedp.Flag("disable-gpu", true),
		chromedp.Flag("disable-extensions", true),
		chromedp.Flag("disable-background-networking", true),
		chromedp.Flag("disable-default-apps", true),
		chromedp.Flag("disable-sync", true),
		chromedp.Flag("disable-translate", true),
		chromedp.Flag("disable-component-extensions-with-background-pages", true),
		chromedp.UserAgent("Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/131.0.0.0 Safari/537.36"),
		chromedp.WindowSize(1280, 900),
	)
	if path := os.Getenv("CHROME_PATH"); path != "" {
		allocOpts = append(allocOpts, chromedp.ExecPath(path))
	}
	if os.Getenv("CHROMEDP_NO_SANDBOX") == "1" {
		allocOpts = append(allocOpts,
			chromedp.Flag("no-sandbox", true),
			chromedp.Flag("disable-dev-shm-usage", true),
		)
	}
	// Images are blocked by default (verified end-to-end: a token extracted with
	// imagesEnabled=false is accepted by the upstream predict API → HTTP 200).
	// CHROMEDP_ALLOW_IMAGES=1 opts back in for debugging unexpected hCaptcha change.
	if os.Getenv("CHROMEDP_ALLOW_IMAGES") != "1" {
		allocOpts = append(allocOpts,
			chromedp.Flag("blink-settings", "imagesEnabled=false"),
		)
	}

	allocCtx, allocCancel := chromedp.NewExecAllocator(parent, allocOpts...)
	browser, bCancel := chromedp.NewContext(allocCtx)

	// Allocate Chrome on browserCtx with no canceling timeout (CommandContext
	// would kill the process if the first-Run context is canceled).
	if err := chromedp.Run(browser, chromedp.Navigate("about:blank")); err != nil {
		bCancel()
		allocCancel()
		return nil, fmt.Errorf("captcha browser alloc: %w", err)
	}

	b := &Browser{
		browser: browser,
		cancel:  allocCancel,
		bCancel: bCancel,
	}

	// Warm playground once so Extract can skip Navigate in the steady state.
	warmCtx, warmCancel := context.WithTimeout(browser, 90*time.Second)
	defer warmCancel()
	if err := warmPlayground(warmCtx); err != nil {
		b.Close()
		return nil, fmt.Errorf("captcha browser warm: %w", err)
	}
	b.warmed = true
	return b, nil
}

// Extract returns a one-shot captcha token from the sticky playground tab.
// Concurrent callers are serialized (one tab); steady-state cost is a reset+execute.
func (b *Browser) Extract(ctx context.Context) (string, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return "", fmt.Errorf("captcha browser closed")
	}

	runCtx, cancel := context.WithTimeout(b.browser, 90*time.Second)
	defer cancel()
	// Propagate caller cancel.
	stop := context.AfterFunc(ctx, cancel)
	defer stop()

	if !b.warmed {
		token, err := navigateAndExecute(runCtx)
		if err != nil {
			return "", err
		}
		b.warmed = true
		return token, nil
	}

	token, err := executeOnly(runCtx)
	if err == nil {
		return token, nil
	}
	// Page may have broken (navigation, bot wall, widget gone) — full recover.
	token, navErr := navigateAndExecute(runCtx)
	if navErr != nil {
		b.warmed = false
		return "", fmt.Errorf("sticky execute failed (%v); re-navigate failed: %w", err, navErr)
	}
	b.warmed = true
	return token, nil
}

// Close shuts down the shared Chrome process.
func (b *Browser) Close() {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return
	}
	b.closed = true
	b.warmed = false
	if b.bCancel != nil {
		b.bCancel()
	}
	b.cancel()
}
