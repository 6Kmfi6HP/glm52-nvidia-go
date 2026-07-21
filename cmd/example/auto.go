package main

import (
	"context"

	"glm52-nvidia/internal/captcha"
)

// extractCaptchaToken loads the NVIDIA Playground, triggers hCaptcha, and
// extracts the token from data-hcaptcha-response.
func extractCaptchaToken(baseCtx context.Context) (string, error) {
	return captcha.Extract(baseCtx)
}
