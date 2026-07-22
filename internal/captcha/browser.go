package captcha

import (
	"context"
	"fmt"
	"log"
	"os"
	"strings"
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
// After the playground is warm, Extract fires hcaptcha.execute({async:true})
// and polls the response (~300ms steady-state) instead of a full Navigate
// (~6–10s). After stickyMaxIdle without a successful extract, the next Extract
// re-navigates instead of burning the sticky timeout on a stale widget.
//
// For parallel pool fills use NewBrowserGroup (separate Chrome processes):
// a second tab in the same Chrome never mounts the hCaptcha widget on this site.
type Browser struct {
	browser context.Context
	cancel  context.CancelFunc // allocator
	bCancel context.CancelFunc // browser tab / process owner

	mu     sync.Mutex
	closed bool
	warmed bool
	lastOK time.Time
}

// stickyMaxIdle is how long a warm playground tab is trusted for sticky execute.
// Longer idle (user paused mid-chat) often leaves the widget unable to mint tokens.
const stickyMaxIdle = 60 * time.Second

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
	// Drop chromedp's noisy "unhandled … event" logs (e.g. TopLayerElementsUpdated).
	// They are CDP events the library does not model; they do not abort Run.
	browser, bCancel := chromedp.NewContext(allocCtx, chromedp.WithErrorf(quietChromedpErrorf))

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
	b.lastOK = time.Now()
	return b, nil
}

// Extract returns a one-shot captcha token from the sticky playground tab.
// Concurrent callers are serialized (one tab); steady-state cost is execute({async:true}).
func (b *Browser) Extract(ctx context.Context) (string, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return "", fmt.Errorf("captcha browser closed")
	}

	// Sticky execute is normally ~300ms; bound it tightly so a hung widget
	// fails fast and re-navigate can start (pool Take otherwise waits with
	// no tokens).
	needNav := !b.warmed || time.Since(b.lastOK) > stickyMaxIdle
	if needNav {
		token, err := b.runExtract(ctx, 90*time.Second, navigateAndExecute)
		if err != nil {
			b.warmed = false
			return "", err
		}
		b.warmed = true
		b.lastOK = time.Now()
		return token, nil
	}

	token, err := b.runExtract(ctx, 15*time.Second, executeOnly)
	if err == nil {
		b.lastOK = time.Now()
		return token, nil
	}
	// Page may have broken (navigation, bot wall, widget gone) — full recover.
	token, navErr := b.runExtract(ctx, 90*time.Second, navigateAndExecute)
	if navErr != nil {
		b.warmed = false
		return "", fmt.Errorf("sticky execute failed (%v); re-navigate failed: %w", err, navErr)
	}
	b.warmed = true
	b.lastOK = time.Now()
	return token, nil
}

func (b *Browser) runExtract(ctx context.Context, limit time.Duration, fn func(context.Context) (string, error)) (string, error) {
	runCtx, cancel := context.WithTimeout(b.browser, limit)
	defer cancel()
	stop := context.AfterFunc(ctx, cancel)
	defer stop()
	return fn(runCtx)
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

// quietChromedpErrorf suppresses known-benign CDP events chromedp has not
// wired into its DOM/page switch (logged as ERROR otherwise).
func quietChromedpErrorf(format string, args ...any) {
	msg := fmt.Sprintf(format, args...)
	if strings.HasPrefix(msg, "unhandled node event") || strings.HasPrefix(msg, "unhandled page event") {
		return
	}
	log.Printf("ERROR: "+format, args...)
}
