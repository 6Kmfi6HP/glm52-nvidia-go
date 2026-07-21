package main

import (
	"context"
	"fmt"
	"time"

	"github.com/chromedp/chromedp"
)

const playgroundURL = "https://build.nvidia.com/z-ai/glm-5.2/playground"

// extractCaptchaToken loads the NVIDIA Playground, triggers hCaptcha, and
// extracts the token from data-hcaptcha-response.
func extractCaptchaToken(baseCtx context.Context) (string, error) {
	allocOpts := append(chromedp.DefaultExecAllocatorOptions[:],
		chromedp.Flag("headless", true),
		chromedp.Flag("disable-blink-features", "AutomationControlled"),
		chromedp.Flag("enable-automation", false),
		chromedp.Flag("no-first-run", true),
		chromedp.Flag("no-default-browser-check", true),
		chromedp.UserAgent("Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/131.0.0.0 Safari/537.36"),
		chromedp.WindowSize(1280, 900),
	)

	allocCtx, allocCancel := chromedp.NewExecAllocator(baseCtx, allocOpts...)
	defer allocCancel()

	ctx, cancel := chromedp.NewContext(allocCtx)
	defer cancel()

	ctx, cancelCtx := context.WithTimeout(ctx, 90*time.Second)
	defer cancelCtx()

	var token string
	err := chromedp.Run(ctx,
		chromedp.Navigate(playgroundURL),
		chromedp.WaitReady("body", chromedp.ByQuery),
		// Hide webdriver flag before waiting on captcha.
		chromedp.Evaluate(`Object.defineProperty(navigator, 'webdriver', {get: () => undefined})`, nil),
		// Invisible hCaptcha is not visible — poll for DOM presence, not WaitVisible.
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

	// Poll until token appears (execute is async).
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
			// Re-trigger execute periodically in case the first call was ignored.
			_ = chromedp.Run(ctx, chromedp.Evaluate(`(() => {
				const el = document.querySelector('[data-hcaptcha-widget-id]');
				if (!el || typeof hcaptcha === 'undefined') return;
				try { hcaptcha.execute(el.getAttribute('data-hcaptcha-widget-id')); } catch (e) {}
			})()`, nil))
		}
	}

	if token == "" {
		return "", fmt.Errorf("empty captcha token — headless Chrome may be blocked; try -captcha with a manual token")
	}
	return token, nil
}
