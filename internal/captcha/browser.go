package captcha

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/chromedp/chromedp"
)

// Browser owns one long-lived headless Chrome (ExecAllocator).
// Each Extract call opens a tab, scrapes a one-shot token, then closes the tab.
type Browser struct {
	allocCtx context.Context
	cancel   context.CancelFunc

	mu     sync.Mutex
	closed bool
}

// NewBrowser starts a shared Chrome allocator. Call Close when done.
func NewBrowser(parent context.Context) (*Browser, error) {
	allocOpts := append(chromedp.DefaultExecAllocatorOptions[:],
		chromedp.Flag("headless", true),
		chromedp.Flag("disable-blink-features", "AutomationControlled"),
		chromedp.Flag("enable-automation", false),
		chromedp.Flag("no-first-run", true),
		chromedp.Flag("no-default-browser-check", true),
		chromedp.UserAgent("Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/131.0.0.0 Safari/537.36"),
		chromedp.WindowSize(1280, 900),
	)

	allocCtx, cancel := chromedp.NewExecAllocator(parent, allocOpts...)
	b := &Browser{allocCtx: allocCtx, cancel: cancel}

	// Warm the browser process so the first Extract is not cold-start + navigate.
	tabCtx, tabCancel := chromedp.NewContext(allocCtx)
	defer tabCancel()
	warmCtx, warmCancel := context.WithTimeout(tabCtx, 60*time.Second)
	defer warmCancel()
	if err := chromedp.Run(warmCtx, chromedp.Navigate("about:blank")); err != nil {
		cancel()
		return nil, fmt.Errorf("captcha browser warm: %w", err)
	}
	return b, nil
}

// Extract opens a fresh tab on the shared browser and returns a one-shot token.
func (b *Browser) Extract(ctx context.Context) (string, error) {
	b.mu.Lock()
	closed := b.closed
	alloc := b.allocCtx
	b.mu.Unlock()
	if closed {
		return "", fmt.Errorf("captcha browser closed")
	}

	tabCtx, tabCancel := chromedp.NewContext(alloc)
	defer tabCancel()
	// Propagate caller cancel into the tab.
	stop := context.AfterFunc(ctx, tabCancel)
	defer stop()

	runCtx, cancel := context.WithTimeout(tabCtx, 90*time.Second)
	defer cancel()

	return runExtract(runCtx)
}

// Close shuts down the shared Chrome process.
func (b *Browser) Close() {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return
	}
	b.closed = true
	b.cancel()
}
