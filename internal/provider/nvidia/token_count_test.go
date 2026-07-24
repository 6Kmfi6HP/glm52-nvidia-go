package nvidia

import (
	"context"
	"encoding/json"
	"testing"

	clipexec "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
)

func TestCountTokens_returnsApproximationForEveryModel(t *testing.T) {
	tests := []struct {
		name  string
		model string
	}{
		{name: "default GLM", model: "z-ai/glm-5.2"},
		{name: "other registered model", model: "deepseek-ai/deepseek-v4-pro"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			// Given: a Claude count request for any model served by the executor.
			request := clipexec.Request{
				Model: test.model,
				Payload: []byte(`{
					"model":"` + test.model + `",
					"system":"You are concise.",
					"messages":[{"role":"user","content":"解释这个 Go 函数"}]
				}`),
			}

			// When: the local executor counts tokens without an upstream request.
			response, err := NewExecutor(Options{}).CountTokens(
				context.Background(),
				nil,
				request,
				clipexec.Options{SourceFormat: sdktranslator.FormatClaude},
			)
			if err != nil {
				t.Fatal(err)
			}

			// Then: it returns the Claude-compatible shape with a positive estimate.
			var body struct {
				InputTokens int `json:"input_tokens"`
			}
			if err := json.Unmarshal(response.Payload, &body); err != nil {
				t.Fatalf("decode response: %v; payload=%s", err, response.Payload)
			}
			if body.InputTokens <= 0 {
				t.Fatalf("input_tokens=%d", body.InputTokens)
			}
		})
	}
}

func TestEstimateRequestTokens_increasesWithInputSize(t *testing.T) {
	// Given: two valid requests that differ only in user-content length.
	short := []byte(`{"messages":[{"role":"user","content":"hello"}]}`)
	long := []byte(`{"messages":[{"role":"user","content":"hello, explain this function in detail with examples"}]}`)

	// When: both requests are estimated.
	shortCount, err := estimateRequestTokens(short)
	if err != nil {
		t.Fatal(err)
	}
	longCount, err := estimateRequestTokens(long)
	if err != nil {
		t.Fatal(err)
	}

	// Then: the larger prompt has a larger estimate.
	if longCount <= shortCount {
		t.Fatalf("short=%d long=%d", shortCount, longCount)
	}
}

func TestEstimateRequestTokens_rejectsInvalidJSON(t *testing.T) {
	// Given: malformed request JSON.
	request := []byte(`{"messages":`)

	// When: it is estimated.
	_, err := estimateRequestTokens(request)

	// Then: invalid input remains a client-visible error instead of becoming zero.
	if err == nil {
		t.Fatal("expected error")
	}
}
