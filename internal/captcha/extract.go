package captcha

import (
	"context"
	"fmt"
	"time"

	"github.com/chromedp/cdproto/network"
	"github.com/chromedp/chromedp"
)

const playgroundURL = "https://build.nvidia.com/z-ai/glm-5.2/playground"

// Extract is a one-shot helper: start Chrome, scrape one token, shut down.
// Prefer Browser + Pool for concurrent serving.
func Extract(baseCtx context.Context) (string, error) {
	b, err := NewBrowser(baseCtx, BrowserConfig{})
	if err != nil {
		return "", err
	}
	defer b.Close()
	return b.Extract(baseCtx)
}

// blockedAssetPatterns skips CSS/fonts/media/images during playground navigate.
// Scripts stay unblocked so hCaptcha can still run. Chosen via cmd/captchaopt
// (block+fast): ~33% faster extract than baseline navigate, 100% upstream accept.
var blockedAssetPatterns = []*network.BlockPattern{
	{URLPattern: "*://*:*/*.css", Block: true},
	{URLPattern: "*://*:*/*.woff", Block: true},
	{URLPattern: "*://*:*/*.woff2", Block: true},
	{URLPattern: "*://*:*/*.ttf", Block: true},
	{URLPattern: "*://*:*/*.otf", Block: true},
	{URLPattern: "*://*:*/*.eot", Block: true},
	{URLPattern: "*://*:*/*.mp4", Block: true},
	{URLPattern: "*://*:*/*.webm", Block: true},
	{URLPattern: "*://*:*/*.mp3", Block: true},
	{URLPattern: "*://*:*/*.png", Block: true},
	{URLPattern: "*://*:*/*.jpg", Block: true},
	{URLPattern: "*://*:*/*.jpeg", Block: true},
	{URLPattern: "*://*:*/*.gif", Block: true},
	{URLPattern: "*://*:*/*.webp", Block: true},
	{URLPattern: "*://*:*/*.svg", Block: true},
	{URLPattern: "*://*:*/*.ico", Block: true},
}

func warmPlayground(ctx context.Context) error {
	return chromedp.Run(ctx,
		network.Enable(),
		network.SetBlockedURLs().WithURLPatterns(blockedAssetPatterns),
		chromedp.Navigate(playgroundURL),
		chromedp.WaitReady("body", chromedp.ByQuery),
		chromedp.Evaluate(`Object.defineProperty(navigator, 'webdriver', {get: () => undefined})`, nil),
		chromedp.WaitReady(`[data-hcaptcha-widget-id]`, chromedp.ByQuery),
		waitHCaptchaReady(),
	)
}

func navigateAndExecute(ctx context.Context) (string, error) {
	if err := warmPlayground(ctx); err != nil {
		return "", fmt.Errorf("chromedp navigate: %w", err)
	}
	return executeOnly(ctx)
}

// executeOnly assumes the sticky tab is already on the playground with hCaptcha ready.
// Mirrors NVIDIA Playground: execute({async:true}) then read response (no reset).
// chromedp cannot await that Promise, so we fire execute and poll the attribute.
func executeOnly(ctx context.Context) (string, error) {
	var prev, token string
	if err := chromedp.Run(ctx, chromedp.Evaluate(`(() => {
		const el = document.querySelector('[data-hcaptcha-widget-id]');
		return el ? (el.getAttribute('data-hcaptcha-response') || '') : '';
	})()`, &prev)); err != nil {
		return "", fmt.Errorf("chromedp read prev: %w", err)
	}

	if err := chromedp.Run(ctx, chromedp.Evaluate(execJS(), &token)); err != nil {
		return "", fmt.Errorf("chromedp execute: %w", err)
	}
	if token == "" || token == prev {
		var err error
		token, err = pollTokenUntilChange(ctx, prev)
		if err != nil {
			return "", err
		}
	}
	if token == "" {
		return "", fmt.Errorf("empty captcha token — headless Chrome may be blocked; supply nv-captcha-token instead")
	}
	if token == prev {
		return "", fmt.Errorf("captcha token did not refresh after execute({async:true})")
	}
	return token, nil
}

// execJS matches NVIDIA Playground: hcaptcha.execute(id, {async:true}).
// Must stay synchronous for chromedp.Evaluate — awaiting an async IIFE here
// resolves to {} (CDP/chromedp awaitPromise mishandles the nested thenable).
// Go pollTokenUntilChange waits for data-hcaptcha-response / getResponse.
func execJS() string {
	return `(() => {
		const el = document.querySelector('[data-hcaptcha-widget-id]');
		if (!el || typeof hcaptcha === 'undefined') return '';
		const id = el.getAttribute('data-hcaptcha-widget-id');
		try { hcaptcha.execute(id, { async: true }); } catch (e) {}
		try {
			const t = typeof hcaptcha.getResponse === 'function' ? hcaptcha.getResponse(id) : '';
			if (typeof t === 'string' && t) return t;
		} catch (e) {}
		return el.getAttribute('data-hcaptcha-response') || '';
	})()`
}

func waitHCaptchaReady() chromedp.Action {
	return chromedp.ActionFunc(func(ctx context.Context) error {
		deadline := time.Now().Add(30 * time.Second)
		for time.Now().Before(deadline) {
			var ready bool
			if err := chromedp.Evaluate(`typeof hcaptcha !== 'undefined'`, &ready).Do(ctx); err != nil {
				return err
			}
			if ready {
				return nil
			}
			if err := chromedp.Sleep(100 * time.Millisecond).Do(ctx); err != nil {
				return err
			}
		}
		return fmt.Errorf("hcaptcha global not ready (bot detection or page change?)")
	})
}

func pollTokenUntilChange(ctx context.Context, prev string) (string, error) {
	var token string
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		// Read first: executeOnly may have completed between its initial
		// Evaluate and this call, so a fixed pre-read sleep adds pure latency.
		if err := chromedp.Run(ctx, chromedp.Evaluate(`(() => {
			const el = document.querySelector('[data-hcaptcha-widget-id]');
			return el ? (el.getAttribute('data-hcaptcha-response') || '') : '';
		})()`, &token)); err != nil {
			return "", fmt.Errorf("chromedp poll: %w", err)
		}
		if token != "" && token != prev {
			return token, nil
		}
		if err := chromedp.Sleep(50 * time.Millisecond).Do(ctx); err != nil {
			return "", fmt.Errorf("chromedp poll: %w", err)
		}
	}
	return token, nil
}
