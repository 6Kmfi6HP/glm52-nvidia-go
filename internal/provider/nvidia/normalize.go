package nvidia

import (
	"encoding/json"
)

// NormalizeRequestBody applies Playground-compatible defaults:
// - stream: force continuous_usage_stats=false (usage once at end)
// - thinking: fold common client aliases into chat_template_kwargs
func NormalizeRequestBody(body []byte) ([]byte, error) {
	var raw map[string]any
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, err
	}
	normalizeThinking(raw)
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
