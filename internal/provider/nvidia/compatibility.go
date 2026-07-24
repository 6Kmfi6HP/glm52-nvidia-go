package nvidia

import (
	"encoding/json"
	"fmt"

	"glm52-nvidia/internal/models"

	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
)

type UnsupportedFeatureError struct {
	SourceFormat sdktranslator.Format
	Feature      string
	Model        string
	Suggestion   string
}

func (e *UnsupportedFeatureError) Error() string {
	return fmt.Sprintf(
		"%s is not supported for model %q through the stateless NVIDIA Chat Completions endpoint; %s",
		e.Feature,
		e.Model,
		e.Suggestion,
	)
}

func translateToChat(source sdktranslator.Format, model string, request []byte, stream bool) ([]byte, error) {
	var sourceBody map[string]json.RawMessage
	if err := json.Unmarshal(request, &sourceBody); err != nil {
		return nil, fmt.Errorf("decode source request: %w", err)
	}
	if err := validateSourceCompatibility(source, model, sourceBody); err != nil {
		return nil, err
	}

	translated := sdktranslator.TranslateRequest(source, sdktranslator.FormatOpenAI, model, request, stream)
	if len(translated) == 0 {
		translated = request
	}
	var chatBody map[string]json.RawMessage
	if err := json.Unmarshal(translated, &chatBody); err != nil {
		return nil, fmt.Errorf("decode translated request: %w", err)
	}

	switch source {
	case sdktranslator.FormatOpenAIResponse:
		copyRawFields(chatBody, sourceBody, "temperature", "top_p", "user", "service_tier")
		if text, ok := sourceBody["text"]; ok {
			responseFormat, err := responsesTextFormat(text)
			if err != nil {
				return nil, err
			}
			if len(responseFormat) != 0 {
				chatBody["response_format"] = responseFormat
			}
		}
	case sdktranslator.FormatClaude:
		copyRawFields(chatBody, sourceBody, "temperature", "top_p")
	}

	return json.Marshal(chatBody)
}

func copyRawFields(destination, source map[string]json.RawMessage, fields ...string) {
	for _, field := range fields {
		if value, ok := source[field]; ok {
			destination[field] = value
		}
	}
}

func responsesTextFormat(raw json.RawMessage) (json.RawMessage, error) {
	var text struct {
		Format json.RawMessage `json:"format"`
	}
	if err := json.Unmarshal(raw, &text); err != nil {
		return nil, fmt.Errorf("decode text.format: %w", err)
	}
	if len(text.Format) == 0 || string(text.Format) == "null" {
		return nil, nil
	}

	var format struct {
		Type   string          `json:"type"`
		Name   string          `json:"name"`
		Strict bool            `json:"strict"`
		Schema json.RawMessage `json:"schema"`
	}
	if err := json.Unmarshal(text.Format, &format); err != nil {
		return nil, fmt.Errorf("decode text.format: %w", err)
	}
	if format.Type != "json_schema" {
		return text.Format, nil
	}
	return json.Marshal(struct {
		Type       string `json:"type"`
		JSONSchema struct {
			Name   string          `json:"name"`
			Strict bool            `json:"strict"`
			Schema json.RawMessage `json:"schema"`
		} `json:"json_schema"`
	}{
		Type: "json_schema",
		JSONSchema: struct {
			Name   string          `json:"name"`
			Strict bool            `json:"strict"`
			Schema json.RawMessage `json:"schema"`
		}{
			Name:   format.Name,
			Strict: format.Strict,
			Schema: format.Schema,
		},
	})
}

func validateSourceCompatibility(source sdktranslator.Format, model string, body map[string]json.RawMessage) error {
	switch source {
	case sdktranslator.FormatOpenAIResponse:
		for _, feature := range []string{
			"previous_response_id",
			"background",
			"store",
			"truncation",
			"max_tool_calls",
		} {
			if raw, ok := body[feature]; ok && string(raw) != "null" {
				return unsupported(source, model, feature)
			}
		}
		if err := rejectResponsesTools(source, model, body["tools"]); err != nil {
			return err
		}
		return rejectContentTypes(source, model, body["input"], map[string]bool{
			"input_file":              true,
			"file_search_call":        true,
			"web_search_call":         true,
			"computer_call":           true,
			"computer_call_output":    true,
			"image_generation_call":   true,
			"code_interpreter_call":   true,
			"local_shell_call":        true,
			"local_shell_call_output": true,
			"shell_call":              true,
			"shell_call_output":       true,
			"apply_patch_call":        true,
			"apply_patch_call_output": true,
			"mcp_call":                true,
		})
	case sdktranslator.FormatClaude:
		return rejectContentTypes(source, model, body["messages"], map[string]bool{
			"document":                   true,
			"search_result":              true,
			"server_tool_use":            true,
			"web_search_tool_result":     true,
			"code_execution_tool_result": true,
		})
	default:
		return nil
	}
}

func rejectResponsesTools(source sdktranslator.Format, model string, raw json.RawMessage) error {
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	var tools []struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(raw, &tools); err != nil {
		return fmt.Errorf("decode tools: %w", err)
	}
	for _, tool := range tools {
		switch tool.Type {
		case "function", "custom", "namespace":
		default:
			return unsupported(source, model, "tools."+tool.Type)
		}
	}
	return nil
}

func rejectContentTypes(
	source sdktranslator.Format,
	model string,
	raw json.RawMessage,
	unsupportedTypes map[string]bool,
) error {
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	var value any
	if err := json.Unmarshal(raw, &value); err != nil {
		return fmt.Errorf("decode content: %w", err)
	}
	return walkContentTypes(source, model, value, unsupportedTypes)
}

func walkContentTypes(
	source sdktranslator.Format,
	model string,
	value any,
	unsupportedTypes map[string]bool,
) error {
	switch typed := value.(type) {
	case []any:
		for _, item := range typed {
			if err := walkContentTypes(source, model, item, unsupportedTypes); err != nil {
				return err
			}
		}
	case map[string]any:
		if contentType, ok := typed["type"].(string); ok && unsupportedTypes[contentType] {
			return unsupported(source, model, contentType)
		}
		for _, item := range typed {
			if err := walkContentTypes(source, model, item, unsupportedTypes); err != nil {
				return err
			}
		}
	}
	return nil
}

func validateChatCapabilities(body []byte, model string, capability models.ModelCapability) error {
	var chat map[string]json.RawMessage
	if err := json.Unmarshal(body, &chat); err != nil {
		return fmt.Errorf("decode chat request: %w", err)
	}
	if len(chat["tools"]) != 0 && string(chat["tools"]) != "null" && !capability.ToolCalling {
		return unsupported(sdktranslator.FormatOpenAI, model, "tools")
	}
	if len(chat["response_format"]) != 0 && string(chat["response_format"]) != "null" && !capability.StructuredOutput {
		return unsupported(sdktranslator.FormatOpenAI, model, "response_format")
	}
	if !capability.Vision {
		if err := rejectContentTypes(
			sdktranslator.FormatOpenAI,
			model,
			chat["messages"],
			map[string]bool{"image_url": true, "input_image": true},
		); err != nil {
			return err
		}
	}
	return nil
}

func unsupported(source sdktranslator.Format, model, feature string) error {
	return &UnsupportedFeatureError{
		SourceFormat: source,
		Feature:      feature,
		Model:        model,
		Suggestion:   "remove this field or send an equivalent complete Chat Completions request",
	}
}
