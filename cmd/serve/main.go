// cmd/serve — OpenAI Chat Completions compatible proxy for GLM-5.2.
//
// The upstream NVIDIA predict API already uses the same request/response
// shape, so this server is a thin header + captcha adapter.
//
// Usage:
//
//	# Fresh captcha via prewarm pool (defaults tuned for TTFT + concurrent fairness)
//	go run ./cmd/serve -auto
//
//	# Override pool / coalesce / in-flight caps
//	go run ./cmd/serve -auto -pool-size=2 -pool-workers=1 -coalesce-ms=0 -max-inflight=8
//
//	# One-shot captcha from flag (consumed on first use)
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
	"net"
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
	auto := flag.Bool("auto", false, "prewarm captcha tokens via shared Chrome + pool")
	poolSize := flag.Int("pool-size", 3, "ready captcha tokens to keep buffered (-auto)")
	poolWorkers := flag.Int("pool-workers", 2, "concurrent captcha extractors (-auto)")
	maxInflight := flag.Int("max-inflight", 4, "max concurrent upstream streams (0=unlimited)")
	coalesceMs := flag.Int("coalesce-ms", 16, "merge consecutive SSE content deltas within this window (0=off); first token always flushes immediately")
	warmTimeout := flag.Duration("warm-timeout", 3*time.Minute, "wait for at least one pooled captcha before serving (-auto); 0=skip")
	flag.Parse()

	if !*auto && *captchaFlag == "" {
		log.Print("warning: no -auto/-captcha; each request must send nv-captcha-token")
	}

	transport := &http.Transport{
		Proxy: http.ProxyFromEnvironment,
		DialContext: (&net.Dialer{
			Timeout:   30 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          100,
		MaxIdleConnsPerHost:   64,
		MaxConnsPerHost:       0,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
		ResponseHeaderTimeout: 120 * time.Second,
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	s := &server{
		auto:        *auto,
		flagCaptcha: *captchaFlag,
		coalesce:    time.Duration(*coalesceMs) * time.Millisecond,
		httpClient:  &http.Client{Timeout: 0, Transport: transport},
	}
	if *maxInflight > 0 {
		s.inflight = make(chan struct{}, *maxInflight)
	}

	if *auto {
		browser, err := captcha.NewBrowser(ctx)
		if err != nil {
			log.Fatalf("captcha browser: %v", err)
		}
		s.browser = browser
		s.pool = captcha.NewPool(ctx, browser.Extract, captcha.PoolConfig{
			Size:    *poolSize,
			Workers: *poolWorkers,
		})
		defer func() {
			s.pool.Close()
			s.browser.Close()
		}()
		log.Printf("captcha pool: size=%d workers=%d", *poolSize, *poolWorkers)

		if *warmTimeout > 0 {
			log.Printf("warming captcha pool (timeout=%s)…", *warmTimeout)
			if err := waitPoolReady(ctx, s.pool, 1, *warmTimeout); err != nil {
				log.Printf("warning: %v — first requests may block on captcha extract", err)
			} else {
				log.Printf("captcha pool ready=%d (TTFT path unblocked)", s.pool.Ready())
			}
		}
	}

	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/chat/completions", s.handleChatCompletions)
	mux.HandleFunc("GET /healthz", s.handleHealthz)

	srv := &http.Server{Addr: *addr, Handler: mux}
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
	}()

	log.Printf("OpenAI-compatible proxy listening on http://localhost%s/v1/chat/completions (coalesce=%s max-inflight=%d)",
		*addr, s.coalesce, *maxInflight)
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatal(err)
	}
}

type server struct {
	auto       bool
	coalesce   time.Duration
	httpClient *http.Client
	inflight   chan struct{} // nil = unlimited

	browser *captcha.Browser
	pool    *captcha.Pool

	mu          sync.Mutex
	flagCaptcha string // emptied after first successful take
}

func (s *server) handleHealthz(w http.ResponseWriter, _ *http.Request) {
	out := map[string]any{"ok": true}
	if s.pool != nil {
		fills, takes, errs := s.pool.Stats()
		out["pool"] = map[string]any{
			"ready":  s.pool.Ready(),
			"fills":  fills,
			"takes":  takes,
			"errors": errs,
		}
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(out)
}

func waitPoolReady(ctx context.Context, pool *captcha.Pool, want int, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	ticker := time.NewTicker(400 * time.Millisecond)
	defer ticker.Stop()
	for {
		if pool.Ready() >= want {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("captcha pool still empty after %s", timeout)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

func (s *server) handleChatCompletions(w http.ResponseWriter, r *http.Request) {
	if s.inflight != nil {
		select {
		case s.inflight <- struct{}{}:
			defer func() { <-s.inflight }()
		default:
			httpError(w, http.StatusServiceUnavailable, "max in-flight upstream streams reached; retry later")
			return
		}
	}

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
	// Anti-buffering hints for reverse proxies / browsers consuming SSE.
	w.Header().Set("Cache-Control", "no-cache, no-transform")
	w.Header().Set("X-Accel-Buffering", "no")
	if ct := upResp.Header.Get("Content-Type"); ct == "" {
		w.Header().Set("Content-Type", "text/event-stream")
	}
	w.WriteHeader(upResp.StatusCode)

	if err := coalesceSSE(w, upResp.Body, s.coalesce); err != nil && r.Context().Err() == nil {
		log.Printf("stream copy: %v", err)
	}
}

// pipeSSE copies upstream bytes to the client, flushing after each write so
// SSE chunks are not held in ResponseWriter / proxy buffers.
func pipeSSE(w http.ResponseWriter, src io.Reader) error {
	flusher, canFlush := w.(http.Flusher)
	buf := make([]byte, 4096)
	for {
		n, readErr := src.Read(buf)
		if n > 0 {
			if _, err := w.Write(buf[:n]); err != nil {
				return err
			}
			if canFlush {
				flusher.Flush()
			}
		}
		if readErr != nil {
			if readErr == io.EOF {
				return nil
			}
			return readErr
		}
	}
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

	if s.pool != nil {
		return s.pool.Take(ctx)
	}
	if s.auto {
		// Fallback if pool failed to start (should not happen).
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
