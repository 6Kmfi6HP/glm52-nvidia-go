// cmd/example/main.go — Example usage of the glm52 Go client.
//
// Usage:
//
//	# Captcha token mode (reverse-engineered)
//	go run . -captcha "P1_..."
//
//	# Auto-extract captcha token via chromedp (no manual token needed)
//	go run . -auto
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"time"

	glm52 "glm52-nvidia"
)

func main() {
	captcha := flag.String("captcha", "", "hCaptcha token (reverse-engineered mode)")
	auto := flag.Bool("auto", false, "Auto-extract captcha token via chromedp")
	prompt := flag.String("prompt", "Explain the meaning of life in one sentence.", "prompt to send")
	stream := flag.Bool("stream", true, "use streaming")
	smoothMs := flag.Int("smooth-ms", 12, "typewriter delay per rune for stream output (0=off)")
	flag.Parse()

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()

	// --- Build client ---
	var client *glm52.Client

	switch {
	case *auto:
		token, err := extractCaptchaToken(ctx)
		if err != nil {
			log.Fatalf("Failed to auto-extract captcha token: %v", err)
		}
		fmt.Printf("✓ Extracted captcha token: %s...\n", token[:30])
		client = glm52.New(glm52.WithCaptchaToken(token))

	case *captcha != "":
		client = glm52.New(glm52.WithCaptchaToken(*captcha))

	default:
		log.Fatal("Specify -captcha or -auto")
	}

	// --- Send request ---
	messages := []glm52.Message{
		{Role: "system", Content: "You are a helpful assistant."},
		{Role: "user", Content: *prompt},
	}

	if *stream {
		fmt.Print("\n=== Streaming response ===\n\n")
		smooth := time.Duration(*smoothMs) * time.Millisecond
		var lastUsage *glm52.Usage
		err := client.StreamChat(ctx, messages, func(chunk glm52.StreamChunk) {
			if chunk.Error != nil {
				log.Printf("Stream error: %v", chunk.Error)
				return
			}
			writeSmooth(chunk.Content, smooth)
			if chunk.Usage != nil {
				lastUsage = chunk.Usage
			}
		})
		if err != nil {
			log.Fatalf("Stream chat failed: %v", err)
		}
		fmt.Println()
		if lastUsage != nil {
			fmt.Printf("\n[Usage: %d prompt + %d completion = %d total]\n",
				lastUsage.PromptTokens, lastUsage.CompletionTokens, lastUsage.TotalTokens)
		}
	} else {
		resp, err := client.Chat(ctx, messages)
		if err != nil {
			log.Fatalf("Chat failed: %v", err)
		}
		fmt.Print("\n=== Response ===\n\n")
		if len(resp.Choices) > 0 {
			fmt.Println(resp.Choices[0].Message.Content)
		}
		if resp.Usage != nil {
			fmt.Printf("\n[Usage: %d prompt + %d completion = %d total]\n",
				resp.Usage.PromptTokens, resp.Usage.CompletionTokens, resp.Usage.TotalTokens)
		}
	}
}
