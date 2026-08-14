package server

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/mostlygeek/llama-swap/internal/config"
	"github.com/tidwall/gjson"
)

func TestServer_FilterUnsupportedOpenAITools(t *testing.T) {
	tests := []struct {
		name        string
		path        string
		body        string
		wantRemoved int
		wantTools   int64
	}{
		{
			name:        "Responses keeps function tools",
			path:        "/v1/responses",
			body:        `{"tools":[{"type":"function","name":"kept"},{"type":"web_search"},{"type":"image_generation"}]}`,
			wantRemoved: 2,
			wantTools:   1,
		},
		{
			name:        "Chat Completions removes empty tool list",
			path:        "/v/chat/completions",
			body:        `{"tools":[{"type":"web_search"}]}`,
			wantRemoved: 1,
			wantTools:   0,
		},
		{
			name:        "Anthropic Messages is unchanged",
			path:        "/v1/messages",
			body:        `{"tools":[{"name":"read_file"}]}`,
			wantRemoved: 0,
			wantTools:   1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, removed, err := filterUnsupportedOpenAITools([]byte(tt.body), tt.path)
			if err != nil {
				t.Fatalf("filterUnsupportedOpenAITools: %v", err)
			}
			if removed != tt.wantRemoved {
				t.Errorf("removed = %d, want %d", removed, tt.wantRemoved)
			}
			if count := gjson.GetBytes(got, "tools.#").Int(); count != tt.wantTools {
				t.Errorf("tools = %d, want %d; body = %s", count, tt.wantTools, got)
			}
		})
	}
}

func TestServer_FilterMiddleware_UpstreamEndpoint(t *testing.T) {
	cfg := config.Config{Models: map[string]config.ModelConfig{
		"model": {
			Filters: config.ModelFilters{Filters: config.Filters{
				SetParams: map[string]any{"temperature": 0.25},
			}},
		},
	}}
	body := `{"model":"model","tools":[{"type":"function","name":"kept"},{"type":"web_search"}]}`
	r := httptest.NewRequest(http.MethodPost, "/upstream/model/v1/responses", io.NopCloser(strings.NewReader(body)))
	r.Header.Set("Content-Type", "application/json")

	var got []byte
	final := http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		var err error
		got, err = io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read body: %v", err)
		}
	})
	CreateFilterMiddleware(cfg)(final).ServeHTTP(httptest.NewRecorder(), r)

	if count := gjson.GetBytes(got, "tools.#").Int(); count != 1 {
		t.Errorf("tools = %d, want 1; body = %s", count, got)
	}
	if temperature := gjson.GetBytes(got, "temperature").Float(); temperature != 0.25 {
		t.Errorf("temperature = %v, want 0.25", temperature)
	}
}
