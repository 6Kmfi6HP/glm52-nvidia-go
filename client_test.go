package glm52

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"testing"
)

func TestBuildRequestUsesCaptchaAuthentication(t *testing.T) {
	client := New(WithCaptchaToken("test-captcha-token"))
	chatRequest := &ChatRequest{
		Model: DefaultModel,
		Messages: []Message{
			{Role: RoleUser, Content: "Hello"},
		},
	}

	req, err := client.buildRequest(context.Background(), chatRequest)
	if err != nil {
		t.Fatalf("buildRequest() error = %v", err)
	}

	if req.Method != http.MethodPost {
		t.Errorf("method = %q, want %q", req.Method, http.MethodPost)
	}
	if req.URL.String() != PredictEndpoint {
		t.Errorf("URL = %q, want %q", req.URL.String(), PredictEndpoint)
	}

	wantHeaders := map[string]string{
		"Content-Type":     "application/json",
		"Accept":           "text/event-stream",
		"nv-function-id":   NVFunctionID,
		"nv-captcha-token": "test-captcha-token",
		"Origin":           "https://build.nvidia.com",
		"Referer":          "https://build.nvidia.com/",
	}
	for name, want := range wantHeaders {
		if got := req.Header.Get(name); got != want {
			t.Errorf("header %q = %q, want %q", name, got, want)
		}
	}
	if got := req.Header.Get("Authorization"); got != "" {
		t.Errorf("Authorization header = %q, want empty", got)
	}

	body, err := io.ReadAll(req.Body)
	if err != nil {
		t.Fatalf("read request body: %v", err)
	}
	var got ChatRequest
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("decode request body: %v", err)
	}
	if got.Model != chatRequest.Model {
		t.Errorf("body model = %q, want %q", got.Model, chatRequest.Model)
	}
	if len(got.Messages) != 1 || got.Messages[0] != chatRequest.Messages[0] {
		t.Errorf("body messages = %#v, want %#v", got.Messages, chatRequest.Messages)
	}
}
