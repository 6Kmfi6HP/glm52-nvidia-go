package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
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

	// Point client at test upstream by temporarily swapping endpoint is hard
	// (const). Instead unit-test the helper path via a local loop mirroring serve logic.
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

func TestNormalizeRequestBody(t *testing.T) {
	in := []byte(`{"stream":true,"messages":[]}`)
	out, err := normalizeRequestBody(in)
	if err != nil {
		t.Fatal(err)
	}
	var raw map[string]any
	if err := json.Unmarshal(out, &raw); err != nil {
		t.Fatal(err)
	}
	opts := raw["stream_options"].(map[string]any)
	if opts["continuous_usage_stats"] != false {
		t.Fatalf("got %#v", opts["continuous_usage_stats"])
	}
	kw := raw["chat_template_kwargs"].(map[string]any)
	if kw["enable_thinking"] != true || kw["clear_thinking"] != false {
		t.Fatalf("thinking kwargs = %#v", kw)
	}
}

func TestNormalizeRequestBodyEmptyOrNullKwargs(t *testing.T) {
	cases := []string{
		`{"stream":false,"chat_template_kwargs":{}}`,
		`{"stream":false,"chat_template_kwargs":null}`,
	}
	for _, in := range cases {
		out, err := normalizeRequestBody([]byte(in))
		if err != nil {
			t.Fatalf("in=%s: %v", in, err)
		}
		var raw map[string]any
		if err := json.Unmarshal(out, &raw); err != nil {
			t.Fatal(err)
		}
		kw := raw["chat_template_kwargs"].(map[string]any)
		if kw["enable_thinking"] != true || kw["clear_thinking"] != false {
			t.Fatalf("in=%s thinking kwargs = %#v", in, kw)
		}
	}
}

func TestNormalizeRequestBodyPreservesThinking(t *testing.T) {
	in := []byte(`{"stream":false,"chat_template_kwargs":{"enable_thinking":false}}`)
	out, err := normalizeRequestBody(in)
	if err != nil {
		t.Fatal(err)
	}
	var raw map[string]any
	if err := json.Unmarshal(out, &raw); err != nil {
		t.Fatal(err)
	}
	kw := raw["chat_template_kwargs"].(map[string]any)
	if kw["enable_thinking"] != false {
		t.Fatalf("should preserve caller kwargs, got %#v", kw)
	}
}

func TestNormalizeThinkingKwargsAliases(t *testing.T) {
	cases := []struct {
		name     string
		in       string
		want     map[string]any
		stripped []string
	}{
		{
			name: "zai thinking enabled",
			in:   `{"stream":false,"thinking":{"type":"enabled","clear_thinking":false}}`,
			want: map[string]any{
				"enable_thinking": true,
				"clear_thinking":  false,
			},
			stripped: []string{"thinking"},
		},
		{
			name: "zai thinking disabled",
			in:   `{"stream":false,"thinking":{"type":"disabled"}}`,
			want: map[string]any{
				"enable_thinking": false,
			},
			stripped: []string{"thinking"},
		},
		{
			name: "top-level enable_thinking false",
			in:   `{"stream":false,"enable_thinking":false}`,
			want: map[string]any{
				"enable_thinking": false,
			},
			stripped: []string{"enable_thinking"},
		},
		{
			name: "top-level enable_thinking true + effort",
			in:   `{"stream":false,"enable_thinking":true,"reasoning_effort":"high"}`,
			want: map[string]any{
				"enable_thinking":  true,
				"clear_thinking":   false,
				"reasoning_effort": "high",
			},
			stripped: []string{"enable_thinking", "reasoning_effort"},
		},
		{
			name: "kwargs wins over aliases",
			in:   `{"stream":false,"chat_template_kwargs":{"enable_thinking":false},"thinking":{"type":"enabled"},"enable_thinking":true}`,
			want: map[string]any{
				"enable_thinking": false,
			},
			stripped: []string{"thinking", "enable_thinking"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out, err := normalizeRequestBody([]byte(tc.in))
			if err != nil {
				t.Fatal(err)
			}
			var raw map[string]any
			if err := json.Unmarshal(out, &raw); err != nil {
				t.Fatal(err)
			}
			for _, key := range tc.stripped {
				if _, ok := raw[key]; ok {
					t.Fatalf("alias %q should be stripped, body=%#v", key, raw)
				}
			}
			kw := raw["chat_template_kwargs"].(map[string]any)
			if len(kw) != len(tc.want) {
				t.Fatalf("kwargs=%#v want %#v", kw, tc.want)
			}
			for k, v := range tc.want {
				if kw[k] != v {
					t.Fatalf("kwargs[%q]=%#v want %#v (full=%#v)", k, kw[k], v, kw)
				}
			}
		})
	}
}

func TestAcquireInflight(t *testing.T) {
	s := &server{
		inflight:     make(chan struct{}, 1),
		inflightWait: 0,
	}
	rel1, err := s.acquireInflight(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.acquireInflight(context.Background())
	if err == nil {
		t.Fatal("expected full")
	}
	rel1()
	rel2, err := s.acquireInflight(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	rel2()
}
