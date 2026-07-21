package captcha

import (
	"context"
	"fmt"
	"time"

	"github.com/chromedp/chromedp"
)

const playgroundURL = "https://build.nvidia.com/z-ai/glm-5.2/playground"

// Extract is a one-shot helper: start Chrome, scrape one token, shut down.
// Prefer Browser + Pool for concurrent serving.
func Extract(baseCtx context.Context) (string, error) {
	b, err := NewBrowser(baseCtx)
	if err != nil {
		return "", err
	}
	defer b.Close()
	return b.Extract(baseCtx)
}

// runExtract assumes ctx is already a chromedp tab context with a timeout.
func runExtract(ctx context.Context) (string, error) {
	var token string
	err := chromedp.Run(ctx,
		chromedp.Navigate(playgroundURL),
		chromedp.WaitReady("body", chromedp.ByQuery),
		chromedp.Evaluate(`Object.defineProperty(navigator, 'webdriver', {get: () => undefined})`, nil),
		chromedp.ActionFunc(func(ctx context.Context) error {
			deadline := time.Now().Add(45 * time.Second)
			for time.Now().Before(deadline) {
				var ready bool
				if err := chromedp.Evaluate(`!!(document.querySelector('[data-hcaptcha-widget-id]') && typeof hcaptcha !== 'undefined')`, &ready).Do(ctx); err != nil {
					return err
				}
				if ready {
					return nil
				}
				if err := chromedp.Sleep(500 * time.Millisecond).Do(ctx); err != nil {
					return err
				}
			}
			return fmt.Errorf("hCaptcha widget not found on page (bot detection or page change?)")
		}),
		chromedp.Sleep(1*time.Second),
		chromedp.Evaluate(`(() => {
			const el = document.querySelector('[data-hcaptcha-widget-id]');
			if (!el || typeof hcaptcha === 'undefined') return '';
			const id = el.getAttribute('data-hcaptcha-widget-id');
			try { hcaptcha.execute(id); } catch (e) {}
			return el.getAttribute('data-hcaptcha-response') || '';
		})()`, &token),
	)
	if err != nil {
		return "", fmt.Errorf("chromedp extract: %w", err)
	}

	if token == "" {
		deadline := time.Now().Add(30 * time.Second)
		for time.Now().Before(deadline) {
			err = chromedp.Run(ctx,
				chromedp.Sleep(1*time.Second),
				chromedp.Evaluate(`(() => {
					const el = document.querySelector('[data-hcaptcha-widget-id]');
					return el ? (el.getAttribute('data-hcaptcha-response') || '') : '';
				})()`, &token),
			)
			if err != nil {
				return "", fmt.Errorf("chromedp poll: %w", err)
			}
			if token != "" {
				break
			}
			_ = chromedp.Run(ctx, chromedp.Evaluate(`(() => {
				const el = document.querySelector('[data-hcaptcha-widget-id]');
				if (!el || typeof hcaptcha === 'undefined') return;
				try { hcaptcha.execute(el.getAttribute('data-hcaptcha-widget-id')); } catch (e) {}
			})()`, nil))
		}
	}

	if token == "" {
		return "", fmt.Errorf("empty captcha token — headless Chrome may be blocked; supply nv-captcha-token instead")
	}
	return token, nil
}
