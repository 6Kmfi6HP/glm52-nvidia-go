//go:build ignore
// +build ignore

package main

import (
	"context"
	"fmt"
	"time"

	"github.com/chromedp/chromedp"
)

// ExtractCaptchaToken loads the NVIDIA Playground, waits for the hCaptcha
// widget, and extracts the captcha token from data-hcaptcha-response.
//
// Usage:
//
//	ctx, cancel := chromedp.NewContext(context.Background())
//	defer cancel()
//	token, err := ExtractCaptchaToken(ctx)
//	client := glm52.New(glm52.WithCaptchaToken(token))
func ExtractCaptchaToken(baseCtx context.Context) (string, error) {
	ctx, cancel := chromedp.NewContext(baseCtx)
	defer cancel()

	ctx, cancelCtx := context.WithTimeout(ctx, 30*time.Second)
	defer cancelCtx()

	var token string

	err := chromedp.Run(ctx,
		chromedp.Navigate("https://build.nvidia.com/z-ai/glm-5.2/playground"),
		chromedp.WaitVisible(`[data-hcaptcha-widget-id]`, chromedp.ByQuery),
		chromedp.WaitReady(`[data-hcaptcha-response]`, chromedp.ByQuery),
		chromedp.Sleep(3*time.Second),
		chromedp.AttributeValue(
			`[data-hcaptcha-widget-id]`,
			"data-hcaptcha-response",
			&token, nil, chromedp.ByQuery,
		),
	)
	if err != nil {
		return "", fmt.Errorf("chromedp extract: %w", err)
	}
	if token == "" {
		// Retry with page reload
		err = chromedp.Run(ctx,
			chromedp.Reload(),
			chromedp.WaitVisible(`[data-hcaptcha-widget-id]`, chromedp.ByQuery),
			chromedp.Sleep(5*time.Second),
			chromedp.AttributeValue(
				`[data-hcaptcha-widget-id]`,
				"data-hcaptcha-response",
				&token, nil, chromedp.ByQuery,
			),
		)
		if err != nil {
			return "", fmt.Errorf("chromedp retry: %w", err)
		}
	}
	if token == "" {
		return "", fmt.Errorf("empty captcha token — page may need manual interaction")
	}
	return token, nil
}
