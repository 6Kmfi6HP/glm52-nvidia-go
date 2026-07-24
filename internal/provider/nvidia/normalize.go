package nvidia

import (
	"encoding/json"
	"strings"
)

// NormalizeRequestBody applies Playground-compatible defaults:
// - stream: force continuous_usage_stats=false (usage once at end)
// - thinking: fold common client aliases into chat_template_kwargs
func NormalizeRequestBody(body []byte) ([]byte, error) {
	var raw map[string]any
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, err
	}
	normalizeThinkingKwargs(raw)
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

// normalizeThinkingKwargs converts common OpenAI-client thinking aliases into
// NVIDIA/vLLM/SGLang chat_template_kwargs, then strips the aliases so upstream
// only sees the Playground wire format.
//
// Accepted inputs (in priority order for enable_thinking):
//  1. chat_template_kwargs.enable_thinking (already NVIDIA-native)
//  2. thinking.type = enabled|disabled  (Z.AI official / Anthropic-style)
//  3. top-level enable_thinking         (Alibaba Model Studio / DashScope)
//
// clear_thinking / reasoning_effort are merged from the same sources when
// missing from kwargs. If nothing sets enable_thinking, defaults to Playground
// on: enable_thinking=true, clear_thinking=false.
func normalizeThinkingKwargs(raw map[string]any) {
	kw, _ := raw["chat_template_kwargs"].(map[string]any)
	if kw == nil {
		kw = map[string]any{}
	}
	_, hasEnable := kw["enable_thinking"]

	if thinking, ok := raw["thinking"].(map[string]any); ok {
		if !hasEnable {
			if typ, ok := thinking["type"].(string); ok {
				switch strings.ToLower(strings.TrimSpace(typ)) {
				case "enabled", "enable", "on":
					kw["enable_thinking"] = true
					hasEnable = true
				case "disabled", "disable", "off":
					kw["enable_thinking"] = false
					hasEnable = true
				}
			}
		}
		if _, ok := kw["clear_thinking"]; !ok {
			if ct, ok := thinking["clear_thinking"]; ok {
				kw["clear_thinking"] = ct
			}
		}
	}
	delete(raw, "thinking")

	if !hasEnable {
		if et, ok := raw["enable_thinking"]; ok {
			kw["enable_thinking"] = et
			hasEnable = true
		}
	}
	delete(raw, "enable_thinking")

	if _, ok := kw["clear_thinking"]; !ok {
		if ct, ok := raw["clear_thinking"]; ok {
			kw["clear_thinking"] = ct
		}
	}
	delete(raw, "clear_thinking")

	if _, ok := kw["reasoning_effort"]; !ok {
		if re, ok := raw["reasoning_effort"]; ok {
			kw["reasoning_effort"] = re
		}
	}
	delete(raw, "reasoning_effort")

	if !hasEnable {
		kw["enable_thinking"] = true
		if _, ok := kw["clear_thinking"]; !ok {
			kw["clear_thinking"] = false
		}
	} else if enable, ok := kw["enable_thinking"].(bool); ok && enable {
		if _, ok := kw["clear_thinking"]; !ok {
			kw["clear_thinking"] = false
		}
	}

	raw["chat_template_kwargs"] = kw
}
