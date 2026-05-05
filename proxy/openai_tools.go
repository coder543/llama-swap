package proxy

import (
	"encoding/json"
)

func shouldFilterOpenAITools(path string) bool {
	return path == "/v1/chat/completions" || path == "/v1/responses"
}

func filterUnsupportedOpenAITools(bodyBytes []byte, path string) ([]byte, int, error) {
	if !shouldFilterOpenAITools(path) {
		return bodyBytes, 0, nil
	}

	var payload map[string]json.RawMessage
	if err := json.Unmarshal(bodyBytes, &payload); err != nil {
		return bodyBytes, 0, nil
	}

	rawTools, found := payload["tools"]
	if !found {
		return bodyBytes, 0, nil
	}

	var tools []json.RawMessage
	if err := json.Unmarshal(rawTools, &tools); err != nil {
		return bodyBytes, 0, nil
	}

	filtered := make([]json.RawMessage, 0, len(tools))
	for _, tool := range tools {
		if openAIToolType(tool) == "function" {
			filtered = append(filtered, tool)
		}
	}

	removed := len(tools) - len(filtered)
	if removed == 0 {
		return bodyBytes, 0, nil
	}

	if len(filtered) == 0 {
		delete(payload, "tools")
	} else {
		filteredTools, err := json.Marshal(filtered)
		if err != nil {
			return nil, 0, err
		}
		payload["tools"] = filteredTools
	}

	bodyBytes, err := json.Marshal(payload)
	if err != nil {
		return nil, 0, err
	}
	return bodyBytes, removed, nil
}

func openAIToolType(tool json.RawMessage) string {
	var typed struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(tool, &typed); err != nil {
		return ""
	}
	return typed.Type
}
