package server

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/mostlygeek/llama-swap/internal/config"
	"github.com/tidwall/gjson"
)

const (
	reasoningTokenizerTimeout = 30 * time.Second
	maxTokenizerResponseBytes = 4 << 20
)

type reasoningTokenizerTarget struct {
	URL    string
	APIKey string
}

func newReasoningHTTPClient() *http.Client {
	return &http.Client{
		Timeout: reasoningTokenizerTimeout,
		// Tokenizer endpoints are internal, fixed routes. Following redirects is
		// unnecessary and risks forwarding custom authentication headers such as
		// x-api-key to a different origin.
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

// resolveReasoningTokenizerTarget returns the endpoint that tokenizes text with
// the same model that served the request. Local models can be contacted
// directly. Peer models must go through the remote llama-swap's upstream route
// so the configured remote model remains unambiguous.
func resolveReasoningTokenizerTarget(cfg config.Config, modelID string) (reasoningTokenizerTarget, bool) {
	if model, ok := cfg.Models[modelID]; ok {
		endpoint, ok := joinURLPath(model.Proxy, "tokenize")
		return reasoningTokenizerTarget{URL: endpoint}, ok
	}

	peerID, peerModelID, ok := cfg.ResolvePeerModel(modelID)
	if !ok {
		return reasoningTokenizerTarget{}, false
	}
	peer, ok := cfg.Peers[peerID]
	if !ok {
		return reasoningTokenizerTarget{}, false
	}
	endpoint, ok := joinURLPath(peer.Proxy, "upstream", peerModelID, "tokenize")
	if !ok {
		return reasoningTokenizerTarget{}, false
	}
	return reasoningTokenizerTarget{URL: endpoint, APIKey: peer.ApiKey}, true
}

func joinURLPath(base string, elements ...string) (string, bool) {
	parsed, err := url.Parse(strings.TrimSpace(base))
	if err != nil || parsed.Host == "" ||
		(!strings.EqualFold(parsed.Scheme, "http") && !strings.EqualFold(parsed.Scheme, "https")) {
		return "", false
	}
	return parsed.JoinPath(elements...).String(), true
}

// extractReasoningContent extracts non-streaming reasoning text from the
// response envelopes used by llama.cpp, vLLM, Anthropic, and the Responses API.
func extractReasoningContent(body []byte) string {
	if !gjson.ValidBytes(body) {
		return ""
	}
	parsed := gjson.ParseBytes(body)
	for _, candidate := range responseCandidates(parsed) {
		if reasoning := extractReasoningFromCandidate(candidate); reasoning != "" {
			return reasoning
		}
	}
	return ""
}

func extractReasoningFromCandidate(parsed gjson.Result) string {
	if reasoning := parsed.Get("choices.0.message.reasoning_content"); reasoning.Type == gjson.String {
		return reasoning.String()
	}
	if reasoning := parsed.Get("choices.0.message.reasoning"); reasoning.Type == gjson.String {
		return reasoning.String()
	}

	var parts []string
	for _, item := range parsed.Get("content").Array() {
		if item.Get("type").String() != "thinking" {
			continue
		}
		if text := item.Get("text"); text.Type == gjson.String {
			parts = append(parts, text.String())
		}
	}
	if len(parts) > 0 {
		return strings.Join(parts, "")
	}

	for _, item := range parsed.Get("output").Array() {
		if item.Get("type").String() != "reasoning" {
			continue
		}
		for _, field := range []string{"summary", "content"} {
			for _, content := range item.Get(field).Array() {
				if text := content.Get("text"); text.Type == gjson.String {
					parts = append(parts, text.String())
				}
			}
		}
	}
	if len(parts) > 0 {
		return strings.Join(parts, "")
	}

	if reasoning := parsed.Get("reasoning_content"); reasoning.Type == gjson.String {
		return reasoning.String()
	}
	return ""
}

// extractStreamingReasoningContent accumulates reasoning deltas from an SSE
// response in stream order. responseCandidates handles both direct events and
// wrappers whose event body is under data.
func extractStreamingReasoningContent(body []byte) string {
	var parts []string
	prefix := []byte("data:")
	for offset := 0; offset < len(body); {
		nl := bytes.IndexByte(body[offset:], '\n')
		var line []byte
		if nl == -1 {
			line = body[offset:]
			offset = len(body)
		} else {
			line = body[offset : offset+nl]
			offset += nl + 1
		}

		line = bytes.TrimSpace(line)
		if !bytes.HasPrefix(line, prefix) {
			continue
		}
		data := bytes.TrimSpace(line[len(prefix):])
		if len(data) == 0 || bytes.Equal(data, []byte("[DONE]")) || !gjson.ValidBytes(data) {
			continue
		}

		for _, candidate := range responseCandidates(gjson.ParseBytes(data)) {
			if delta := extractReasoningDelta(candidate); delta != "" {
				parts = append(parts, delta)
				break
			}
		}
	}
	return strings.Join(parts, "")
}

func responseCandidates(parsed gjson.Result) []gjson.Result {
	candidates := make([]gjson.Result, 0, len(metricsContainerPaths))
	for _, path := range metricsContainerPaths {
		candidate := parsed
		if path != "" {
			candidate = parsed.Get(path)
		}
		if candidate.Exists() {
			candidates = append(candidates, candidate)
		}
	}
	return candidates
}

func extractReasoningDelta(parsed gjson.Result) string {
	for _, path := range []string{
		"choices.0.delta.reasoning_content",
		"choices.0.delta.reasoning",
		"delta.reasoning_content",
		"delta.thinking",
	} {
		if delta := parsed.Get(path); delta.Type == gjson.String {
			return delta.String()
		}
	}
	switch parsed.Get("type").String() {
	case "response.reasoning_text.delta", "response.reasoning_summary_text.delta":
		if delta := parsed.Get("delta"); delta.Type == gjson.String {
			return delta.String()
		}
	}
	return ""
}

// tokenizeReasoning tries the llama.cpp request shape first and then vLLM's.
// The caller owns the timeout through ctx; target may add configured peer auth.
func tokenizeReasoning(ctx context.Context, client *http.Client, target reasoningTokenizerTarget, reasoning string) (int, bool) {
	if target.URL == "" || reasoning == "" {
		return 0, false
	}
	parsedTarget, err := url.Parse(target.URL)
	if err != nil || parsedTarget.Host == "" ||
		(!strings.EqualFold(parsedTarget.Scheme, "http") && !strings.EqualFold(parsedTarget.Scheme, "https")) {
		return 0, false
	}
	payloads := []any{
		map[string]any{"content": reasoning},
		map[string]any{"prompt": reasoning, "add_special_tokens": false},
	}
	for _, payload := range payloads {
		body, err := json.Marshal(payload)
		if err != nil {
			return 0, false
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, target.URL, bytes.NewReader(body))
		if err != nil {
			return 0, false
		}
		req.Header.Set("Content-Type", "application/json")
		if target.APIKey != "" {
			req.Header.Set("Authorization", "Bearer "+target.APIKey)
			req.Header.Set("x-api-key", target.APIKey)
		}

		resp, err := client.Do(req)
		if err != nil {
			if ctx.Err() != nil {
				return 0, false
			}
			continue
		}
		responseBody, readErr := io.ReadAll(io.LimitReader(resp.Body, maxTokenizerResponseBytes))
		resp.Body.Close()
		if readErr != nil || resp.StatusCode != http.StatusOK || !gjson.ValidBytes(responseBody) {
			continue
		}

		parsed := gjson.ParseBytes(responseBody)
		if tokens := parsed.Get("tokens"); tokens.IsArray() {
			return len(tokens.Array()), true
		}
		if count := parsed.Get("count"); count.Type == gjson.Number {
			return int(count.Int()), true
		}
	}
	return 0, false
}
