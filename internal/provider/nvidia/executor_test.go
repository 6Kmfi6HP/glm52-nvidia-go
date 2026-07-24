package nvidia

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"glm52-nvidia/internal/models"

	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	clipexec "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"

	_ "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator/builtin"
)

func TestIsRetryableCaptchaFailure(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   string
		want   bool
	}{
		{
			name:   "nvidia invalid token",
			status: http.StatusBadRequest,
			body:   `{"requestStatus":{"statusCode":"INVALID_REQUEST","statusDescription":"Token is invalid","requestId":"abc"}}`,
			want:   true,
		},
		{
			name:   "case insensitive token",
			status: http.StatusBadRequest,
			body:   `{"requestStatus":{"statusDescription":"token is Invalid"}}`,
			want:   true,
		},
		{
			name:   "generic captcha body",
			status: http.StatusForbidden,
			body:   `{"error":"hcaptcha rejected the token"}`,
			want:   true,
		},
		{
			name:   "missing captcha wording",
			status: http.StatusBadRequest,
			body:   `{"error":"missing-captcha"}`,
			want:   true,
		},
		{
			name:   "unrelated client error",
			status: http.StatusBadRequest,
			body:   `{"requestStatus":{"statusCode":"INVALID_REQUEST","statusDescription":"bad prompt"}}`,
			want:   false,
		},
		{
			name:   "empty body client error",
			status: http.StatusBadRequest,
			body:   ``,
			want:   false,
		},
		{
			name:   "5xx not retryable as captcha",
			status: http.StatusBadGateway,
			body:   `{"error":"hcaptcha is down"}`,
			want:   false,
		},
		{
			name:   "2xx never retryable",
			status: http.StatusOK,
			body:   `{"error":"hcaptcha"}`,
			want:   false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isRetryableCaptchaFailure(tc.status, []byte(tc.body)); got != tc.want {
				t.Fatalf("got %v want %v", got, tc.want)
			}
		})
	}
}

func TestAcquireInflight(t *testing.T) {
	e := NewExecutor(Options{MaxInflight: 1, InflightWait: 0})
	rel1, err := e.acquireInflight(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	_, err = e.acquireInflight(context.Background())
	if err == nil {
		t.Fatal("expected full")
	}
	rel1()
	rel2, err := e.acquireInflight(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	rel2()
}

func TestExecuteUnknownModel(t *testing.T) {
	e := NewExecutor(Options{FlagCaptcha: "tok"})
	_, err := e.Execute(context.Background(), nil, clipexec.Request{
		Model:   "no-such-org/never",
		Payload: []byte(`{"model":"no-such-org/never","messages":[{"role":"user","content":"hi"}]}`),
	}, clipexec.Options{SourceFormat: sdktranslator.FormatOpenAI})
	if err == nil {
		t.Fatal("expected error")
	}
	ae, ok := err.(*coreauth.Error)
	if !ok {
		t.Fatalf("err type %T", err)
	}
	if ae.HTTPStatus != http.StatusBadRequest {
		t.Fatalf("status=%d", ae.HTTPStatus)
	}
	if !strings.Contains(ae.Message, "no-such-org/never") {
		t.Fatalf("msg=%q", ae.Message)
	}
}

func TestExecuteStreamMockUpstream(t *testing.T) {
	var hits int
	var gotToken, gotFunction string
	var gotBody []byte
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		gotToken = r.Header.Get("nv-captcha-token")
		gotFunction = r.Header.Get("nv-function-id")
		gotBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"ok\"}}]}\n\ndata: [DONE]\n\n")
	}))
	defer up.Close()

	info, err := models.Lookup("z-ai/glm-5.2")
	if err != nil {
		t.Fatal(err)
	}

	e := NewExecutor(Options{
		FlagCaptcha: "P1_test",
		HTTPClient:  up.Client(),
		PredictURL:  func(models.ModelInfo) string { return up.URL },
	})

	stream, err := e.ExecuteStream(context.Background(), nil, clipexec.Request{
		Model:   "z-ai/glm-5.2",
		Payload: []byte(`{"model":"z-ai/glm-5.2","messages":[{"role":"user","content":"hi"}],"stream":true}`),
	}, clipexec.Options{
		Stream:       true,
		SourceFormat: sdktranslator.FormatOpenAI,
		Headers:      http.Header{},
	})
	if err != nil {
		t.Fatal(err)
	}
	var payloads [][]byte
	for chunk := range stream.Chunks {
		if chunk.Err != nil {
			t.Fatal(chunk.Err)
		}
		payloads = append(payloads, chunk.Payload)
	}
	if hits != 1 {
		t.Fatalf("hits=%d", hits)
	}
	if gotToken != "P1_test" {
		t.Fatalf("token=%q", gotToken)
	}
	if gotFunction != info.FunctionID {
		t.Fatalf("function-id=%q want %q", gotFunction, info.FunctionID)
	}
	var body map[string]any
	if err := json.Unmarshal(gotBody, &body); err != nil {
		t.Fatal(err)
	}
	if body["stream"] != true {
		t.Fatalf("upstream stream flag=%v", body["stream"])
	}
	joined := string(bytes.Join(payloads, nil))
	if !strings.Contains(joined, "ok") {
		t.Fatalf("payloads=%q", joined)
	}
}

func TestUpstreamRetryOnInvalidToken(t *testing.T) {
	var hits int
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		tok := r.Header.Get("nv-captcha-token")
		if tok == "bad" {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"requestStatus":{"statusCode":"INVALID_REQUEST","statusDescription":"Token is invalid","requestId":"x"}}`))
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"ok\"}}]}\n\ndata: [DONE]\n\n")
	}))
	defer up.Close()

	client := up.Client()
	body := []byte(`{"model":"m","messages":[{"role":"user","content":"hi"}],"stream":true}`)
	tokens := []string{"bad", "good"}
	var upResp *http.Response
	for i, tok := range tokens {
		req, _ := http.NewRequest(http.MethodPost, up.URL, bytes.NewReader(body))
		req.Header.Set("nv-captcha-token", tok)
		resp, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		if resp.StatusCode >= 400 {
			raw, _ := io.ReadAll(resp.Body)
			_ = resp.Body.Close()
			if isRetryableCaptchaFailure(resp.StatusCode, raw) && i+1 < len(tokens) {
				continue
			}
			t.Fatalf("unexpected error body=%s", raw)
		}
		upResp = resp
		break
	}
	if upResp == nil {
		t.Fatal("no successful upstream response")
	}
	if hits != 2 {
		t.Fatalf("hits=%d want 2", hits)
	}
	defer upResp.Body.Close()
	raw, _ := io.ReadAll(upResp.Body)
	if !strings.Contains(string(raw), "ok") {
		t.Fatalf("body=%q", raw)
	}
}

func TestTranslatorClaudeToOpenAIChatShape(t *testing.T) {
	claude := []byte(`{"model":"z-ai/glm-5.2","max_tokens":64,"messages":[{"role":"user","content":"Hi"}],"stream":false}`)
	out := sdktranslator.TranslateRequest(sdktranslator.FormatClaude, sdktranslator.FormatOpenAI, "z-ai/glm-5.2", claude, false)
	if len(out) == 0 {
		t.Fatal("empty translation")
	}
	var raw map[string]any
	if err := json.Unmarshal(out, &raw); err != nil {
		t.Fatalf("not json: %v body=%s", err, out)
	}
	if _, ok := raw["messages"]; !ok {
		t.Fatalf("expected messages in openai chat shape, got %s", out)
	}
	if raw["model"] != "z-ai/glm-5.2" {
		t.Fatalf("model=%v", raw["model"])
	}
}

func TestTranslatorResponsesToOpenAIChatShape(t *testing.T) {
	responses := []byte(`{"model":"z-ai/glm-5.2","input":"Hi","stream":false}`)
	out := sdktranslator.TranslateRequest(sdktranslator.FormatOpenAIResponse, sdktranslator.FormatOpenAI, "z-ai/glm-5.2", responses, false)
	if len(out) == 0 {
		t.Fatal("empty translation")
	}
	var raw map[string]any
	if err := json.Unmarshal(out, &raw); err != nil {
		t.Fatalf("not json: %v body=%s", err, out)
	}
	if _, ok := raw["messages"]; !ok {
		t.Fatalf("expected messages in openai chat shape, got %s", out)
	}
}

func TestRegistryModelsSorted(t *testing.T) {
	ms := RegistryModels()
	if len(ms) == 0 {
		t.Fatal("empty")
	}
	for i := 1; i < len(ms); i++ {
		if ms[i-1].ID > ms[i].ID {
			t.Fatalf("not sorted: %q before %q", ms[i-1].ID, ms[i].ID)
		}
	}
	byID := map[string]bool{}
	for _, m := range ms {
		if m.Type != providerKey {
			t.Errorf("type=%q for %q", m.Type, m.ID)
		}
		byID[m.ID] = true
	}
	for _, want := range []string{"z-ai/glm-5.2", "deepseek-ai/deepseek-v4-pro"} {
		if !byID[want] {
			t.Errorf("missing %q", want)
		}
	}
	if byID["moonshotai/kimi-k2.6"] {
		t.Error("runtime-only model should be absent")
	}
}

func TestExecuteUsesHeaderCaptcha(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("nv-captcha-token") != "from-header" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"1","choices":[{"message":{"role":"assistant","content":"hi"}}]}`))
	}))
	defer up.Close()

	e := NewExecutor(Options{
		HTTPClient: up.Client(),
		PredictURL: func(models.ModelInfo) string { return up.URL },
	})
	hdr := make(http.Header)
	hdr.Set("nv-captcha-token", "from-header")
	resp, err := e.Execute(context.Background(), nil, clipexec.Request{
		Model:   "z-ai/glm-5.2",
		Payload: []byte(`{"model":"z-ai/glm-5.2","messages":[{"role":"user","content":"hi"}]}`),
	}, clipexec.Options{
		SourceFormat: sdktranslator.FormatOpenAI,
		Headers:      hdr,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(resp.Payload), "hi") {
		t.Fatalf("payload=%s", resp.Payload)
	}
}

func TestExecuteTranslatesReasoningEffortForUpstreamModel(t *testing.T) {
	var upstreamBody map[string]any
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&upstreamBody); err != nil {
			t.Errorf("decode upstream request: %v", err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"1","choices":[{"message":{"role":"assistant","content":"ok"}}]}`))
	}))
	defer up.Close()

	executor := NewExecutor(Options{
		FlagCaptcha: "test-token",
		HTTPClient:  up.Client(),
		PredictURL:  func(models.ModelInfo) string { return up.URL },
	})
	_, err := executor.Execute(context.Background(), nil, clipexec.Request{
		Model: "deepseek-ai/deepseek-v4-pro",
		Payload: []byte(`{
			"model":"deepseek-ai/deepseek-v4-pro",
			"messages":[{"role":"user","content":"solve"}],
			"reasoning_effort":"xhigh"
		}`),
	}, clipexec.Options{SourceFormat: sdktranslator.FormatOpenAI})
	if err != nil {
		t.Fatal(err)
	}

	kwargs, ok := upstreamBody["chat_template_kwargs"].(map[string]any)
	if !ok {
		t.Fatalf("upstream chat_template_kwargs missing: %#v", upstreamBody)
	}
	if got := kwargs["reasoning_effort"]; got != "max" {
		t.Fatalf("upstream reasoning_effort=%#v want max", got)
	}
	if _, ok := upstreamBody["reasoning_effort"]; ok {
		t.Fatalf("generic reasoning_effort leaked upstream: %#v", upstreamBody)
	}
}

func TestExecute_sendsResponsesSamplingFieldsToPlayground(t *testing.T) {
	// Given: a capture server standing in for the NVIDIA Chat Completions endpoint.
	var upstreamBody map[string]json.RawMessage
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&upstreamBody); err != nil {
			t.Errorf("decode upstream request: %v", err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"1","choices":[{"message":{"role":"assistant","content":"ok"}}]}`))
	}))
	defer up.Close()
	executor := NewExecutor(Options{
		FlagCaptcha: "test-token",
		HTTPClient:  up.Client(),
		PredictURL:  func(models.ModelInfo) string { return up.URL },
	})

	// When: a Responses request uses Chat-compatible sampling and attribution fields.
	_, err := executor.Execute(context.Background(), nil, clipexec.Request{
		Model: "z-ai/glm-5.2",
		Payload: []byte(`{
			"model":"z-ai/glm-5.2",
			"input":"hello",
			"temperature":0.25,
			"top_p":0.8,
			"user":"user-42",
			"service_tier":"default"
		}`),
	}, clipexec.Options{SourceFormat: sdktranslator.FormatOpenAIResponse})
	if err != nil {
		t.Fatal(err)
	}

	// Then: the captured canonical Chat request retains every compatible field.
	for field, want := range map[string]string{
		"temperature":  "0.25",
		"top_p":        "0.8",
		"user":         `"user-42"`,
		"service_tier": `"default"`,
	} {
		if got := string(upstreamBody[field]); got != want {
			t.Errorf("%s=%s want %s; body=%v", field, got, want, upstreamBody)
		}
	}
	if string(upstreamBody["stream"]) != "false" {
		t.Fatalf("stream=%s", upstreamBody["stream"])
	}
}

func TestExecute_forwardsResponsesStoreWithoutRejecting(t *testing.T) {
	// Given: a Playground server that records whether it was contacted.
	hits := 0
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits++
		w.Header().Set("Content-Type", "application/json")
		if _, err := w.Write([]byte(`{"id":"chatcmpl-1","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`)); err != nil {
			return
		}
	}))
	defer up.Close()
	executor := NewExecutor(Options{
		FlagCaptcha: "test-token",
		HTTPClient:  up.Client(),
		PredictURL:  func(models.ModelInfo) string { return up.URL },
	})

	// When: a Responses request includes store (unsupported by NVIDIA, but must not be rejected).
	_, err := executor.Execute(context.Background(), nil, clipexec.Request{
		Model: "z-ai/glm-5.2",
		Payload: []byte(`{
			"model":"z-ai/glm-5.2",
			"input":"hello",
			"store":true
		}`),
	}, clipexec.Options{SourceFormat: sdktranslator.FormatOpenAIResponse})

	// Then: the request proceeds to NVIDIA instead of returning unsupported_feature.
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if hits != 1 {
		t.Fatalf("upstream hits=%d", hits)
	}
}
