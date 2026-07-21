package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestIsInvalidCaptchaToken(t *testing.T) {
	cases := []struct {
		name string
		body string
		want bool
	}{
		{
			name: "nvidia invalid",
			body: `{"requestStatus":{"statusCode":"INVALID_REQUEST","statusDescription":"Token is invalid","requestId":"abc"}}`,
			want: true,
		},
		{
			name: "case insensitive",
			body: `{"requestStatus":{"statusDescription":"token is Invalid"}}`,
			want: true,
		},
		{
			name: "other error",
			body: `{"requestStatus":{"statusCode":"INVALID_REQUEST","statusDescription":"bad prompt"}}`,
			want: false,
		},
		{
			name: "empty",
			body: ``,
			want: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isInvalidCaptchaToken([]byte(tc.body)); got != tc.want {
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
	var err error
	for i, tok := range tokens {
		req, _ := http.NewRequest(http.MethodPost, up.URL, bytes.NewReader(body))
		req.Header.Set("nv-captcha-token", tok)
		upResp, err = client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		if upResp.StatusCode >= 400 {
			raw, _ := io.ReadAll(upResp.Body)
			_ = upResp.Body.Close()
			if isInvalidCaptchaToken(raw) && i+1 < len(tokens) {
				continue
			}
			t.Fatalf("unexpected error body=%s", raw)
		}
		break
	}
	defer upResp.Body.Close()
	if hits != 2 {
		t.Fatalf("hits=%d want 2", hits)
	}
	raw, _ := io.ReadAll(upResp.Body)
	if !strings.Contains(string(raw), "ok") {
		t.Fatalf("body=%q", raw)
	}
}

func TestNormalizeUsageOnce(t *testing.T) {
	in := []byte(`{"stream":true,"messages":[]}`)
	out, err := normalizeUsageOnce(in)
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
}
