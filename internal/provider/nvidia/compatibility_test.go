package nvidia

import (
	"encoding/json"
	"testing"

	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"

	_ "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator/builtin"
)

func TestTranslateToChat_preservesResponsesCompatibleFields(t *testing.T) {
	// Given: a Responses request containing fields that Chat Completions can express.
	request := []byte(`{
		"model":"z-ai/glm-5.2",
		"input":"hello",
		"temperature":0.25,
		"top_p":0.8,
		"user":"user-42",
		"service_tier":"default",
		"text":{"format":{"type":"json_schema","name":"answer","strict":true,"schema":{"type":"object"}}}
	}`)

	// When: the request is translated to the canonical Chat format.
	got, err := translateToChat(sdktranslator.FormatOpenAIResponse, "z-ai/glm-5.2", request, false)
	if err != nil {
		t.Fatal(err)
	}

	// Then: compatible scalar fields and structured output retain their machine-readable shape.
	var body map[string]json.RawMessage
	if err := json.Unmarshal(got, &body); err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"temperature", "top_p", "user", "service_tier"} {
		if _, ok := body[field]; !ok {
			t.Errorf("translated request lost %q: %s", field, got)
		}
	}
	var responseFormat struct {
		Type       string `json:"type"`
		JSONSchema struct {
			Name   string          `json:"name"`
			Strict bool            `json:"strict"`
			Schema json.RawMessage `json:"schema"`
		} `json:"json_schema"`
	}
	if err := json.Unmarshal(body["response_format"], &responseFormat); err != nil {
		t.Fatal(err)
	}
	if responseFormat.Type != "json_schema" || responseFormat.JSONSchema.Name != "answer" ||
		!responseFormat.JSONSchema.Strict || len(responseFormat.JSONSchema.Schema) == 0 {
		t.Fatalf("response_format=%s", body["response_format"])
	}
}

func TestTranslateToChat_preservesClaudeTemperatureAndTopP(t *testing.T) {
	// Given: Claude parameters whose translator currently treats as mutually exclusive.
	request := []byte(`{
		"model":"z-ai/glm-5.2",
		"max_tokens":32,
		"temperature":0.2,
		"top_p":0.7,
		"messages":[{"role":"user","content":"hello"}]
	}`)

	// When: the request is translated to canonical Chat.
	got, err := translateToChat(sdktranslator.FormatClaude, "z-ai/glm-5.2", request, false)
	if err != nil {
		t.Fatal(err)
	}

	// Then: both independent sampling controls are present.
	var body map[string]json.RawMessage
	if err := json.Unmarshal(got, &body); err != nil {
		t.Fatal(err)
	}
	if string(body["temperature"]) != "0.2" || string(body["top_p"]) != "0.7" {
		t.Fatalf("sampling fields were not preserved: %s", got)
	}
}

func TestTranslateToChat_ignoresUnsupportedPlatformFeatures(t *testing.T) {
	tests := []struct {
		name    string
		format  sdktranslator.Format
		request string
	}{
		{
			name:    "responses store",
			format:  sdktranslator.FormatOpenAIResponse,
			request: `{"model":"z-ai/glm-5.2","input":"hello","store":true}`,
		},
		{
			name:    "responses state",
			format:  sdktranslator.FormatOpenAIResponse,
			request: `{"model":"z-ai/glm-5.2","input":"hello","previous_response_id":"resp_1"}`,
		},
		{
			name:    "responses hosted tool",
			format:  sdktranslator.FormatOpenAIResponse,
			request: `{"model":"z-ai/glm-5.2","input":"hello","tools":[{"type":"web_search_preview"}]}`,
		},
		{
			name:    "responses input file",
			format:  sdktranslator.FormatOpenAIResponse,
			request: `{"model":"z-ai/glm-5.2","input":[{"role":"user","content":[{"type":"input_file","file_id":"file_1"}]}]}`,
		},
		{
			name:    "claude document",
			format:  sdktranslator.FormatClaude,
			request: `{"model":"z-ai/glm-5.2","max_tokens":32,"messages":[{"role":"user","content":[{"type":"document","source":{"type":"base64","data":"AA=="}}]}]}`,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			// Given: a source request that includes platform-only fields.
			// When: it is translated to Chat Completions.
			got, err := translateToChat(test.format, "z-ai/glm-5.2", []byte(test.request), false)

			// Then: translation succeeds; unsupported fields are dropped by translation.
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if len(got) == 0 {
				t.Fatal("empty translated body")
			}
		})
	}
}
