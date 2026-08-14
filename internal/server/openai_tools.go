package server

import "encoding/json"

func shouldFilterOpenAITools(path string) bool {
	switch path {
	case "/v1/chat/completions", "/v1/responses", "/v/chat/completions", "/v/responses":
		return true
	default:
		return false
	}
}

// filterUnsupportedOpenAITools removes hosted OpenAI tool types that
// llama.cpp does not implement, while preserving ordinary function tools.
func filterUnsupportedOpenAITools(body []byte, path string) ([]byte, int, error) {
	if !shouldFilterOpenAITools(path) {
		return body, 0, nil
	}

	var payload map[string]json.RawMessage
	if err := json.Unmarshal(body, &payload); err != nil {
		return body, 0, nil
	}

	rawTools, found := payload["tools"]
	if !found {
		return body, 0, nil
	}

	var tools []json.RawMessage
	if err := json.Unmarshal(rawTools, &tools); err != nil {
		return body, 0, nil
	}

	filtered := make([]json.RawMessage, 0, len(tools))
	for _, tool := range tools {
		if openAIToolType(tool) == "function" {
			filtered = append(filtered, tool)
		}
	}

	removed := len(tools) - len(filtered)
	if removed == 0 {
		return body, 0, nil
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

	filteredBody, err := json.Marshal(payload)
	if err != nil {
		return nil, 0, err
	}
	return filteredBody, removed, nil
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
