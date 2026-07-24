package nvidia

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"unicode"

	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	clipexec "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
)

func (_ *Executor) CountTokens(
	_ context.Context,
	_ *coreauth.Auth,
	req clipexec.Request,
	_ clipexec.Options,
) (clipexec.Response, error) {
	count, err := estimateRequestTokens(req.Payload)
	if err != nil {
		return clipexec.Response{}, requestErr(http.StatusBadRequest, "invalid json body")
	}
	payload, err := json.Marshal(struct {
		InputTokens int `json:"input_tokens"`
	}{InputTokens: count})
	if err != nil {
		return clipexec.Response{}, fmt.Errorf("encode token count: %w", err)
	}
	return clipexec.Response{Payload: payload}, nil
}

func estimateRequestTokens(payload []byte) (int, error) {
	var request any
	if err := json.Unmarshal(payload, &request); err != nil {
		return 0, fmt.Errorf("decode token count request: %w", err)
	}
	count := estimateJSONTokens(request)
	if count == 0 {
		return 1, nil
	}
	return count, nil
}

func estimateJSONTokens(value any) int {
	switch typed := value.(type) {
	case string:
		return estimateTextTokens(typed)
	case []any:
		count := 1
		for _, item := range typed {
			count += estimateJSONTokens(item)
		}
		return count
	case map[string]any:
		count := 2
		for key, item := range typed {
			switch key {
			case "model", "role", "type":
				continue
			default:
				count += estimateJSONTokens(item)
			}
		}
		return count
	default:
		return 0
	}
}

func estimateTextTokens(text string) int {
	count := 0
	asciiRun := 0
	flushASCII := func() {
		if asciiRun > 0 {
			count += (asciiRun + 3) / 4
			asciiRun = 0
		}
	}
	for _, char := range text {
		switch {
		case unicode.IsSpace(char):
			flushASCII()
		case char <= unicode.MaxASCII:
			asciiRun++
		default:
			flushASCII()
			count++
		}
	}
	flushASCII()
	return count
}
