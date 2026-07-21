// cmd/serve — OpenAI Chat Completions compatible proxy for GLM-5.2.
//
// The upstream NVIDIA predict API already uses the same request/response
// shape, so this server is a thin header + captcha adapter.
//
// Usage:
//
//	# Fresh captcha per request (chromedp)
//	go run ./cmd/serve -auto
//
//	# One-shot captcha from flag (consumed on first request)
//	go run ./cmd/serve -captcha "P1_..."
//
//	# Or pass a one-shot token per request:
//	curl -H 'nv-captcha-token: P1_...' ...
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"time"

	glm52 "glm52-nvidia"
	"glm52-nvidia/internal/captcha"
)

func main() {
	addr := flag.String("addr", ":8080", "listen address")
	captchaFlag := flag.String("captcha", "", "one-shot hCaptcha token (consumed on first use)")
	auto := flag.Bool("auto", false, "extract a fresh captcha token per request via chromedp")
	flag.Parse()

	if !*auto && *captchaFlag == "" {
		log.Print("warning: no -auto/-captcha; each request must send nv-captcha-token")
	}

	s := &server{
		auto:         *auto,
		flagCaptcha:  *captchaFlag,
		httpClient:   &http.Client{Timeout: 120 * time.Second},
	}

	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/chat/completions", s.handleChatCompletions)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	srv := &http.Server{Addr: *addr, Handler: mux}
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
	}()

	log.Printf("OpenAI-compatible proxy listening on http://localhost%s/v1/chat/completions", *addr)
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatal(err)
	}
}

type server struct {
	auto        bool
	httpClient  *http.Client

	mu          sync.Mutex
	flagCaptcha string // emptied after first successful take
}

func (s *server) handleChatCompletions(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, 8<<20))
	if err != nil {
		httpError(w, http.StatusBadRequest, "failed to read body")
		return
	}
	_ = r.Body.Close()

	// Normalize stream_options so usage is reported once (OpenAI-compatible).
	body, err = normalizeUsageOnce(body)
	if err != nil {
		httpError(w, http.StatusBadRequest, "invalid json body")
		return
	}

	token, err := s.resolveCaptcha(r.Context(), r)
	if err != nil {
		httpError(w, http.StatusUnauthorized, err.Error())
		return
	}

	upReq, err := http.NewRequestWithContext(r.Context(), http.MethodPost, glm52.PredictEndpoint, bytes.NewReader(body))
	if err != nil {
		httpError(w, http.StatusInternalServerError, "failed to create upstream request")
		return
	}
	upReq.Header.Set("Content-Type", "application/json")
	upReq.Header.Set("Accept", "text/event-stream")
	upReq.Header.Set("nv-function-id", glm52.NVFunctionID)
	upReq.Header.Set("nv-captcha-token", token) // one captcha → one upstream call
	upReq.Header.Set("Origin", "https://build.nvidia.com")
	upReq.Header.Set("Referer", "https://build.nvidia.com/")

	upResp, err := s.httpClient.Do(upReq)
	if err != nil {
		httpError(w, http.StatusBadGateway, fmt.Sprintf("upstream: %v", err))
		return
	}
	defer upResp.Body.Close()

	copyHeader(w.Header(), upResp.Header)
	w.WriteHeader(upResp.StatusCode)
	_, _ = io.Copy(w, upResp.Body)
}

func (s *server) resolveCaptcha(ctx context.Context, r *http.Request) (string, error) {
	if t := r.Header.Get("nv-captcha-token"); t != "" {
		return t, nil
	}

	s.mu.Lock()
	flagToken := s.flagCaptcha
	if flagToken != "" {
		s.flagCaptcha = "" // consume once — token is single-use upstream
	}
	s.mu.Unlock()
	if flagToken != "" {
		return flagToken, nil
	}

	if s.auto {
		return captcha.Extract(ctx)
	}
	return "", fmt.Errorf("captcha token required: send nv-captcha-token, or restart with -captcha / -auto")
}

// normalizeUsageOnce forces continuous_usage_stats=false so token usage
// appears at most once in the stream (final chunk when include_usage is set).
func normalizeUsageOnce(body []byte) ([]byte, error) {
	var raw map[string]any
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, err
	}
	stream, _ := raw["stream"].(bool)
	if !stream {
		return body, nil
	}
	opts, _ := raw["stream_options"].(map[string]any)
	if opts == nil {
		opts = map[string]any{}
		raw["stream_options"] = opts
	}
	if _, ok := opts["include_usage"]; !ok {
		opts["include_usage"] = true
	}
	opts["continuous_usage_stats"] = false
	return json.Marshal(raw)
}

func copyHeader(dst, src http.Header) {
	for k, vs := range src {
		switch http.CanonicalHeaderKey(k) {
		case "Content-Length", "Transfer-Encoding", "Connection":
			continue
		}
		for _, v := range vs {
			dst.Add(k, v)
		}
	}
}

func httpError(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"error": map[string]any{
			"message": msg,
			"type":    "proxy_error",
		},
	})
}
