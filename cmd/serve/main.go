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
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"sync"
	"time"

	glm52 "glm52-nvidia"
	"glm52-nvidia/internal/captcha"
)

// Set via -ldflags "-X main.version=v1.2.3" at release build time.
var version = "dev"

func main() {
	addr := flag.String("addr", ":8080", "listen address")
	captchaFlag := flag.String("captcha", "", "one-shot hCaptcha token (consumed on first use)")
	auto := flag.Bool("auto", false, "prewarm captcha tokens via shared Chrome + pool")
	poolSize := flag.Int("pool-size", 3, "ready captcha tokens to keep buffered (-auto)")
	poolWorkers := flag.Int("pool-workers", 2, "concurrent captcha extractors (-auto)")
	maxInflight := flag.Int("max-inflight", 4, "max concurrent upstream streams (0=unlimited)")
	inflightWait := flag.Duration("inflight-wait", 500*time.Millisecond, "how long to wait for an in-flight slot before returning 503 (0=reject immediately)")
	coalesceMs := flag.Int("coalesce-ms", 16, "merge consecutive SSE content deltas within this window (0=off); first token always flushes immediately")
	warmTimeout := flag.Duration("warm-timeout", 3*time.Minute, "wait for at least one pooled captcha before serving (-auto); 0=skip")
	poolTTL := flag.Duration("pool-ttl", 90*time.Second, "discard pooled captcha tokens older than this (-auto)")
	captchaWait := flag.Duration("captcha-wait", 30*time.Second, "max wait for a pooled captcha token per request (0=block until ready); then 503")
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
		auto:         *auto,
		flagCaptcha:  *captchaFlag,
		coalesce:     time.Duration(*coalesceMs) * time.Millisecond,
		inflightWait: *inflightWait,
		captchaWait:  *captchaWait,
		httpClient:   &http.Client{Timeout: 0, Transport: transport},
	}
	if *maxInflight > 0 {
		s.inflight = make(chan struct{}, *maxInflight)
	}

	if *auto {
		// One Chrome process per pool worker — same-Chrome tabs never mount a
		// second hCaptcha widget on this playground.
		browser, err := captcha.NewBrowserGroup(ctx, *poolWorkers)
		if err != nil {
			log.Fatalf("captcha browser: %v", err)
		}
		s.browser = browser
		s.pool = captcha.NewPool(ctx, browser.Extract, captcha.PoolConfig{
			Size:    *poolSize,
			Workers: *poolWorkers,
			TTL:     *poolTTL,
		})
		defer func() {
			s.pool.Close()
			s.browser.Close()
		}()
		log.Printf("captcha pool: size=%d workers=%d chromes=%d ttl=%s captcha-wait=%s",
			*poolSize, *poolWorkers, browser.Len(), *poolTTL, *captchaWait)

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

	log.Printf("serve %s listening on http://localhost%s/v1/chat/completions (coalesce=%s max-inflight=%d)",
		version, *addr, s.coalesce, *maxInflight)
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatal(err)
	}
}

type server struct {
	auto         bool
	coalesce     time.Duration
	httpClient   *http.Client
	inflight     chan struct{} // nil = unlimited
	inflightWait time.Duration // how long to wait for a slot before 503 (0=reject now)
	captchaWait  time.Duration // max wait for pool Take (0=unlimited)

	browser *captcha.BrowserGroup
	pool    *captcha.Pool

	mu          sync.Mutex
	flagCaptcha string // emptied after first successful take
}

func (s *server) handleHealthz(w http.ResponseWriter, _ *http.Request) {
	out := map[string]any{"ok": true}
	if s.pool != nil {
		fills, takes, errs, expired := s.pool.Stats()
		out["pool"] = map[string]any{
			"ready":   s.pool.Ready(),
			"fills":   fills,
			"takes":   takes,
			"errors":  errs,
			"expired": expired,
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
	body, err := io.ReadAll(io.LimitReader(r.Body, 8<<20))
	if err != nil {
		httpError(w, http.StatusBadRequest, "failed to read body")
		return
	}
	_ = r.Body.Close()

	// Normalize stream_options so usage is reported once (OpenAI-compatible).
	// Inject Playground-default thinking kwargs when the client omitted them.
	body, err = normalizeRequestBody(body)
	if err != nil {
		httpError(w, http.StatusBadRequest, "invalid json body")
		return
	}

	clientToken := r.Header.Get("nv-captcha-token")
	// Client-supplied tokens are not retried (caller owns them). Pool/auto can refresh.
	maxAttempts := 1
	if clientToken == "" && (s.pool != nil || s.auto) {
		maxAttempts = 3
	}

	// Hold an in-flight slot only while an upstream request/stream is active —
	// not during captcha extract/Take, which can block for tens of seconds.
	var release func()
	defer func() {
		if release != nil {
			release()
		}
	}()

	reqStart := time.Now()
	var (
		upResp       *http.Response
		captchaWait  time.Duration
		inflightWait time.Duration
		upstreamTTFB time.Duration
	)
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		t0 := time.Now()
		token, err := s.resolveCaptcha(r.Context(), clientToken, attempt == 1)
		captchaWait += time.Since(t0)
		if err != nil {
			log.Printf("timing captcha=%s ready=%d err=%v", captchaWait.Round(time.Millisecond), s.poolReady(), err)
			httpError(w, captchaResolveStatus(err), err.Error())
			return
		}

		t1 := time.Now()
		rel, err := s.acquireInflight(r.Context())
		inflightWait += time.Since(t1)
		if err != nil {
			log.Printf("timing captcha=%s inflight=%s ready=%d err=%v",
				captchaWait.Round(time.Millisecond), inflightWait.Round(time.Millisecond), s.poolReady(), err)
			httpError(w, http.StatusServiceUnavailable, err.Error())
			return
		}
		release = rel

		upReq, err := http.NewRequestWithContext(r.Context(), http.MethodPost, glm52.PredictEndpoint, bytes.NewReader(body))
		if err != nil {
			httpError(w, http.StatusInternalServerError, "failed to create upstream request")
			return
		}
		upReq.Header.Set("Content-Type", "application/json")
		upReq.Header.Set("Accept", "text/event-stream")
		upReq.Header.Set("nv-function-id", glm52.NVFunctionID)
		upReq.Header.Set("nv-captcha-token", token)
		upReq.Header.Set("Origin", "https://build.nvidia.com")
		upReq.Header.Set("Referer", "https://build.nvidia.com/")

		t2 := time.Now()
		upResp, err = s.httpClient.Do(upReq)
		upstreamTTFB = time.Since(t2)
		if err != nil {
			log.Printf("timing captcha=%s inflight=%s upstream_ttfb=%s ready=%d err=%v",
				captchaWait.Round(time.Millisecond), inflightWait.Round(time.Millisecond),
				upstreamTTFB.Round(time.Millisecond), s.poolReady(), err)
			httpError(w, http.StatusBadGateway, fmt.Sprintf("upstream: %v", err))
			return
		}

		if upResp.StatusCode < 400 {
			break
		}

		raw, _ := io.ReadAll(io.LimitReader(upResp.Body, 4<<10))
		status := upResp.StatusCode
		_ = upResp.Body.Close()
		upResp = nil
		release()
		release = nil

		if isInvalidCaptchaToken(raw) && attempt < maxAttempts {
			log.Printf("upstream captcha invalid (attempt %d/%d); fetching a fresh token (captcha=%s upstream_ttfb=%s)",
				attempt, maxAttempts, captchaWait.Round(time.Millisecond), upstreamTTFB.Round(time.Millisecond))
			continue
		}

		if isInvalidCaptchaToken(raw) {
			log.Printf("timing captcha=%s inflight=%s upstream_ttfb=%s status=401 ready=%d captcha_invalid",
				captchaWait.Round(time.Millisecond), inflightWait.Round(time.Millisecond),
				upstreamTTFB.Round(time.Millisecond), s.poolReady())
			httpError(w, http.StatusUnauthorized, "captcha token invalid or expired; retry the request")
			return
		}
		msg := strings.TrimSpace(string(raw))
		if msg == "" {
			msg = "upstream request failed"
		}
		log.Printf("timing captcha=%s inflight=%s upstream_ttfb=%s status=%d ready=%d err=%s",
			captchaWait.Round(time.Millisecond), inflightWait.Round(time.Millisecond),
			upstreamTTFB.Round(time.Millisecond), status, s.poolReady(), truncateLog(msg, 120))
		httpError(w, http.StatusBadGateway, msg)
		return
	}
	if upResp == nil {
		httpError(w, http.StatusUnauthorized, "captcha token invalid or expired; retry the request")
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

	tw := &firstByteWriter{ResponseWriter: w, start: time.Now()}
	streamErr := coalesceSSE(tw, upResp.Body, s.coalesce)
	streamDur := time.Since(tw.start)
	ttftClient := tw.firstByte
	if !tw.sawWrite {
		ttftClient = streamDur
	}
	log.Printf("timing captcha=%s inflight=%s upstream_ttfb=%s client_ttft=%s stream=%s total=%s status=%d ready=%d",
		captchaWait.Round(time.Millisecond),
		inflightWait.Round(time.Millisecond),
		upstreamTTFB.Round(time.Millisecond),
		ttftClient.Round(time.Millisecond),
		streamDur.Round(time.Millisecond),
		time.Since(reqStart).Round(time.Millisecond),
		upResp.StatusCode,
		s.poolReady(),
	)
	if streamErr != nil && r.Context().Err() == nil {
		log.Printf("stream copy: %v", streamErr)
	}
}

func (s *server) poolReady() int {
	if s.pool == nil {
		return -1
	}
	return s.pool.Ready()
}

// firstByteWriter records time-to-first-write (client TTFT after headers).
type firstByteWriter struct {
	http.ResponseWriter
	start     time.Time
	firstByte time.Duration
	sawWrite  bool
}

func (w *firstByteWriter) Write(p []byte) (int, error) {
	if !w.sawWrite && len(p) > 0 {
		w.firstByte = time.Since(w.start)
		w.sawWrite = true
	}
	return w.ResponseWriter.Write(p)
}

func (w *firstByteWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func truncateLog(s string, n int) string {
	s = strings.ReplaceAll(s, "\n", " ")
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// acquireInflight reserves one upstream concurrency slot. release must be called
// exactly once when the upstream request/stream finishes (or immediately on
// error before Do). Returns a no-op release when unlimited.
func (s *server) acquireInflight(ctx context.Context) (release func(), err error) {
	if s.inflight == nil {
		return func() {}, nil
	}
	release = func() { <-s.inflight }

	// Wait up to inflightWait for a slot instead of rejecting immediately;
	// an SSE burst that would otherwise 503 can usually drain within a few
	// hundred ms as in-flight streams finish. 0 keeps the old hard-reject.
	if s.inflightWait <= 0 {
		select {
		case s.inflight <- struct{}{}:
			return release, nil
		default:
			return nil, fmt.Errorf("max in-flight upstream streams reached; retry later")
		}
	}

	timer := time.NewTimer(s.inflightWait)
	defer timer.Stop()
	select {
	case s.inflight <- struct{}{}:
		return release, nil
	case <-timer.C:
		return nil, fmt.Errorf("max in-flight upstream streams reached; retry later")
	case <-ctx.Done():
		return nil, fmt.Errorf("client cancelled before a stream slot opened")
	}
}

// isInvalidCaptchaToken detects NVIDIA's "Token is invalid" captcha failures.
func isInvalidCaptchaToken(raw []byte) bool {
	if len(raw) == 0 {
		return false
	}
	var er glm52.ErrorResponse
	if json.Unmarshal(raw, &er) == nil {
		desc := strings.ToLower(er.RequestStatus.StatusDescription)
		if strings.Contains(desc, "token is invalid") || strings.Contains(desc, "invalid token") {
			return true
		}
		if er.RequestStatus.StatusCode == "INVALID_REQUEST" && strings.Contains(desc, "token") {
			return true
		}
	}
	low := strings.ToLower(string(raw))
	return strings.Contains(low, "token is invalid")
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

func (s *server) resolveCaptcha(ctx context.Context, clientToken string, allowFlag bool) (string, error) {
	if clientToken != "" {
		return clientToken, nil
	}

	if allowFlag {
		s.mu.Lock()
		flagToken := s.flagCaptcha
		if flagToken != "" {
			s.flagCaptcha = "" // consume once — token is single-use upstream
		}
		s.mu.Unlock()
		if flagToken != "" {
			return flagToken, nil
		}
	}

	if s.pool != nil {
		takeCtx := ctx
		var cancel context.CancelFunc
		if s.captchaWait > 0 {
			takeCtx, cancel = context.WithTimeout(ctx, s.captchaWait)
			defer cancel()
		}
		if s.pool.Ready() == 0 {
			waitFor := "indefinitely"
			if s.captchaWait > 0 {
				waitFor = s.captchaWait.String()
			}
			log.Printf("captcha pool empty; waiting up to %s (errors will surface from workers)", waitFor)
		}
		tok, err := s.pool.Take(takeCtx)
		if err != nil {
			if errors.Is(err, context.DeadlineExceeded) {
				fills, takes, errs, expired := s.pool.Stats()
				return "", fmt.Errorf("captcha pool empty after %s (ready=%d fills=%d takes=%d errors=%d expired=%d); retry later",
					s.captchaWait, s.pool.Ready(), fills, takes, errs, expired)
			}
			return "", err
		}
		return tok, nil
	}
	if s.auto {
		return captcha.Extract(ctx)
	}
	return "", fmt.Errorf("captcha token required: send nv-captcha-token, or restart with -captcha / -auto")
}

func captchaResolveStatus(err error) int {
	if err == nil {
		return http.StatusOK
	}
	if errors.Is(err, context.DeadlineExceeded) || strings.Contains(err.Error(), "captcha pool empty after") {
		return http.StatusServiceUnavailable
	}
	if errors.Is(err, context.Canceled) {
		return http.StatusRequestTimeout
	}
	return http.StatusUnauthorized
}

// normalizeRequestBody applies Playground-compatible defaults:
// - stream: force continuous_usage_stats=false (usage once at end)
// - thinking: inject chat_template_kwargs when omitted, null, or empty
func normalizeRequestBody(body []byte) ([]byte, error) {
	var raw map[string]any
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, err
	}
	kw, _ := raw["chat_template_kwargs"].(map[string]any)
	if len(kw) == 0 {
		raw["chat_template_kwargs"] = map[string]any{
			"enable_thinking": true,
			"clear_thinking":  false,
		}
	}
	stream, _ := raw["stream"].(bool)
	if !stream {
		return json.Marshal(raw)
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
