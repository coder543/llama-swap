package server

import (
	"context"
	"encoding/json"

	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mostlygeek/llama-swap/internal/chain"
	"github.com/mostlygeek/llama-swap/internal/config"
	"github.com/mostlygeek/llama-swap/internal/logmon"
	"github.com/mostlygeek/llama-swap/internal/swaputil"
	"github.com/tidwall/gjson"
)

func TestServer_ParseMetrics_ChatCompletions(t *testing.T) {
	body := `{"usage":{"prompt_tokens":12,"completion_tokens":7,"prompt_tokens_details":{"cached_tokens":4}}}`
	parsed := gjson.Parse(body)
	entry, err := parseMetrics("m", time.Now(), parsed.Get("usage"), parsed.Get("timings"), parsed.Get("metrics"))
	if err != nil {
		t.Fatalf("parseMetrics: %v", err)
	}
	if entry.Tokens.InputTokens != 12 || entry.Tokens.GeneratedTokens != 7 || entry.Tokens.OutputTokens != 7 || entry.Tokens.CachedTokens != 4 {
		t.Fatalf("tokens = %+v", entry.Tokens)
	}
}

func TestServer_ActivitySourceStripsClientPort(t *testing.T) {
	tests := []struct {
		name       string
		remoteAddr string
		want       string
	}{
		{name: "IPv4", remoteAddr: "192.168.1.10:54321", want: "ip:192.168.1.10"},
		{name: "IPv6", remoteAddr: "[2001:db8::10]:54321", want: "ip:2001:db8::10"},
		{name: "missing port", remoteAddr: "localhost", want: "ip:localhost"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodGet, "/", nil)
			r.RemoteAddr = test.remoteAddr
			if got := activitySource(r); got != test.want {
				t.Fatalf("activitySource() = %q, want %q", got, test.want)
			}
		})
	}
}

func TestServer_ActivitySourceForwardedHeaders(t *testing.T) {
	tests := []struct {
		name  string
		setup func(*http.Request)
		want  string
	}{
		{"x-forwarded-for", func(r *http.Request) {
			r.Header.Set("X-Forwarded-For", "203.0.113.10, 10.0.0.1")
		}, "xff:203.0.113.10"},
		{"x-real-ip", func(r *http.Request) {
			r.Header.Set("X-Real-IP", "203.0.113.20")
		}, "xff:203.0.113.20"},
		{"x-forwarded-for takes priority over x-real-ip", func(r *http.Request) {
			r.Header.Set("X-Forwarded-For", "203.0.113.10")
			r.Header.Set("X-Real-IP", "203.0.113.20")
		}, "xff:203.0.113.10"},
		{"x-forwarded-for whitespace-only first entry falls back to x-real-ip", func(r *http.Request) {
			r.Header.Set("X-Forwarded-For", "   , 10.0.0.1")
			r.Header.Set("X-Real-IP", "203.0.113.20")
		}, "xff:203.0.113.20"},
		{"x-forwarded-for empty first entry falls back to remote addr", func(r *http.Request) {
			r.Header.Set("X-Forwarded-For", ",10.0.0.1")
		}, "ip:192.168.1.10"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodGet, "/", nil)
			r.RemoteAddr = "192.168.1.10:54321"
			test.setup(r)
			if got := activitySource(r); got != test.want {
				t.Fatalf("activitySource() = %q, want %q", got, test.want)
			}
		})
	}
}

func TestServer_ParseMetrics_Timings(t *testing.T) {
	body := `{"timings":{"prompt_n":20,"predicted_n":50,"prompt_per_second":100.0,"predicted_per_second":40.0,"prompt_ms":200,"predicted_ms":1250,"cache_n":8}}`
	parsed := gjson.Parse(body)
	entry, err := parseMetrics("m", time.Now(), parsed.Get("usage"), parsed.Get("timings"), parsed.Get("metrics"))
	if err != nil {
		t.Fatalf("parseMetrics: %v", err)
	}
	if entry.Tokens.InputTokens != 20 || entry.Tokens.GeneratedTokens != 50 || entry.Tokens.OutputTokens != 50 || entry.Tokens.CachedTokens != 8 {
		t.Fatalf("tokens = %+v", entry.Tokens)
	}
	if entry.Tokens.TokensPerSecond != 40.0 || entry.Tokens.PromptPerSecond != 100.0 {
		t.Fatalf("rates = %+v", entry.Tokens)
	}
	if entry.DurationMs != 1450 {
		t.Fatalf("DurationMs = %d, want 1450", entry.DurationMs)
	}
}

func TestServer_ParseMetrics_TabbyAPI(t *testing.T) {
	body := `{"id":"chatcmpl-131b9995cd0c40c99e2ce958083ee406","object":"chat.completion.chunk","choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":83,"prompt_time":2.36,"prompt_tokens_per_sec":35.17,"completion_tokens":192,"completion_time":11.22,"completion_tokens_per_sec":17.11,"total_tokens":275,"total_time":13.63},"model":"SC_6.00bpw_H6_V6"}`
	parsed := gjson.Parse(body)
	entry, err := parseMetrics("m", time.Now(), parsed.Get("usage"), parsed.Get("timings"), parsed.Get("metrics"))
	if err != nil {
		t.Fatalf("parseMetrics: %v", err)
	}
	if entry.Tokens.InputTokens != 83 || entry.Tokens.OutputTokens != 192 {
		t.Fatalf("tokens = %+v", entry.Tokens)
	}
	if got, want := entry.Tokens.PromptPerSecond, 35.17; math.Abs(got-want) > 1e-9 {
		t.Errorf("PromptPerSecond = %v, want %v", got, want)
	}
	if got, want := entry.Tokens.TokensPerSecond, 17.11; math.Abs(got-want) > 1e-9 {
		t.Errorf("TokensPerSecond = %v, want %v", got, want)
	}
	if entry.DurationMs != 13630 {
		t.Errorf("DurationMs = %d, want 13630", entry.DurationMs)
	}
}

func TestServer_ParseMetrics_OMLX(t *testing.T) {
	tests := []struct {
		name         string
		rates        string
		extra        string
		wantPrompt   float64
		wantGenerate float64
	}{
		{"reported", `,"prompt_tokens_per_second":63.74,"generation_tokens_per_second":66.38`, "", 63.74, 66.38},
		{"zero", `,"prompt_tokens_per_second":0,"generation_tokens_per_second":0`, "", 0, 0},
		{"null", `,"prompt_tokens_per_second":null,"generation_tokens_per_second":null`, "", -1, -1},
		{"missing", "", "", -1, -1},
		{"prompt only", `,"prompt_tokens_per_second":63.74`, "", 63.74, -1},
		{"generation only", `,"generation_tokens_per_second":66.38`, "", -1, 66.38},
		{"non-numeric", `,"prompt_tokens_per_second":"unknown","generation_tokens_per_second":false`, "", -1, -1},
		{"TabbyAPI precedence", `,"prompt_tokens_per_sec":35.17,"completion_tokens_per_sec":17.11,"prompt_tokens_per_second":63.74,"generation_tokens_per_second":66.38`, "", 35.17, 17.11},
		{"TabbyAPI zero precedence", `,"prompt_tokens_per_sec":0,"completion_tokens_per_sec":0,"prompt_tokens_per_second":63.74,"generation_tokens_per_second":66.38`, "", 0, 0},
		{"TabbyAPI string precedence", `,"prompt_tokens_per_sec":"35.17","completion_tokens_per_sec":"17.11","prompt_tokens_per_second":63.74,"generation_tokens_per_second":66.38`, "", 35.17, 17.11},
		{"TabbyAPI null fallback", `,"prompt_tokens_per_sec":null,"completion_tokens_per_sec":null,"prompt_tokens_per_second":63.74,"generation_tokens_per_second":66.38`, "", 63.74, 66.38},
		{"all null", `,"prompt_tokens_per_sec":null,"completion_tokens_per_sec":null,"prompt_tokens_per_second":null,"generation_tokens_per_second":null`, "", -1, -1},
		{"independent generation fallback", `,"prompt_tokens_per_sec":35.17,"prompt_tokens_per_second":63.74,"generation_tokens_per_second":66.38`, "", 35.17, 66.38},
		{"independent prompt fallback", `,"completion_tokens_per_sec":17.11,"prompt_tokens_per_second":63.74,"generation_tokens_per_second":66.38`, "", 63.74, 17.11},
		{"timings precedence", `,"prompt_tokens_per_second":63.74,"generation_tokens_per_second":66.38`, `,"timings":{"prompt_n":56,"predicted_n":8,"cache_n":16,"prompt_per_second":100,"predicted_per_second":40}`, 100, 40},
		{"vLLM precedence", `,"prompt_tokens_per_second":63.74,"generation_tokens_per_second":66.38`, `,"metrics":{"time_to_first_token_ms":200,"mean_itl_ms":10}`, 200, 100},
		{"vLLM over timings", `,"prompt_tokens_per_second":63.74,"generation_tokens_per_second":66.38`, `,"timings":{"prompt_n":56,"predicted_n":8,"cache_n":16,"prompt_per_second":100,"predicted_per_second":40},"metrics":{"time_to_first_token_ms":200,"mean_itl_ms":10}`, 200, 100},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			body := `{"usage":{"prompt_tokens":56,"completion_tokens":8,"prompt_tokens_details":{"cached_tokens":16}` + test.rates + `}` + test.extra + `}`
			for _, streaming := range []bool{false, true} {
				name := "JSON"
				if streaming {
					name = "SSE"
				}
				t.Run(name, func(t *testing.T) {
					var entry parsedActivity
					var err error
					if streaming {
						stream := "data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\n" + "data: " + body + "\n\ndata: [DONE]\n\n"
						entry, err = processStreamingResponse("m", time.Now(), []byte(stream))
					} else {
						parsed := gjson.Parse(body)
						entry, err = parseMetrics("m", time.Now(), parsed.Get("usage"), parsed.Get("timings"), parsed.Get("metrics"))
					}
					if err != nil {
						t.Fatalf("parse metrics: %v", err)
					}
					if entry.Tokens.InputTokens != 56 || entry.Tokens.OutputTokens != 8 || entry.Tokens.CachedTokens != 16 {
						t.Fatalf("tokens = %+v", entry.Tokens)
					}
					if entry.Tokens.PromptPerSecond != test.wantPrompt || entry.Tokens.TokensPerSecond != test.wantGenerate {
						t.Fatalf("rates = %+v, want prompt %v, generation %v", entry.Tokens, test.wantPrompt, test.wantGenerate)
					}
				})
			}
		})
	}
}

func TestServer_MetricsMiddleware_OMLX(t *testing.T) {
	for _, streaming := range []bool{false, true} {
		name := "JSON"
		if streaming {
			name = "SSE"
		}
		t.Run(name, func(t *testing.T) {
			mm := newTestMetricsMonitor(t, logmon.NewWriter(io.Discard), 10, 0)
			cfg := config.Config{Models: map[string]config.ModelConfig{"m": {}}}
			body := `{"usage":{"prompt_tokens":56,"completion_tokens":8,"prompt_tokens_per_second":63.74,"generation_tokens_per_second":66.38}}`
			contentType := "application/json"
			if streaming {
				contentType = "text/event-stream"
				body = "data: " + body + "\n\ndata: [DONE]\n\n"
			}
			handler := chain.New(CreateMetricsMiddleware(mm, cfg)).ThenFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", contentType)
				io.WriteString(w, body)
			})
			r := chatRequest("m")
			w := httptest.NewRecorder()
			handler.ServeHTTP(w, r)
			if w.Code != http.StatusOK || w.Body.String() != body {
				t.Fatalf("response = %d %q, want 200 %q", w.Code, w.Body.String(), body)
			}
			entries := metricsEntries(t, mm)
			if len(entries) != 1 {
				t.Fatalf("want 1 entry, got %d", len(entries))
			}
			entry := entries[0]
			if entry.Model != "m" || entry.Tokens.InputTokens != 56 || entry.Tokens.OutputTokens != 8 || entry.Tokens.PromptPerSecond != 63.74 || entry.Tokens.TokensPerSecond != 66.38 {
				t.Fatalf("recorded activity = %+v", entry)
			}
		})
	}
}

// llama-server timings and TabbyAPI usage rates never appear together, but a
// response carrying both must keep the timings-sourced rates.
func TestServer_ParseMetrics_TabbyAPINotOverwrittenByTimings(t *testing.T) {
	body := `{"usage":{"prompt_tokens":83,"completion_tokens":192,"prompt_tokens_per_sec":35.17,"completion_tokens_per_sec":17.11},"timings":{"prompt_n":20,"predicted_n":50,"prompt_per_second":100.0,"predicted_per_second":40.0,"prompt_ms":200,"predicted_ms":1250}}`
	parsed := gjson.Parse(body)
	entry, err := parseMetrics("m", time.Now(), parsed.Get("usage"), parsed.Get("timings"), parsed.Get("metrics"))
	if err != nil {
		t.Fatalf("parseMetrics: %v", err)
	}
	if entry.Tokens.PromptPerSecond != 100.0 || entry.Tokens.TokensPerSecond != 40.0 {
		t.Fatalf("rates = %+v, want timings rates", entry.Tokens)
	}
}

func TestServer_ProcessStreamingResponse_TabbyAPI(t *testing.T) {
	body := []byte("data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\n" +
		"data: {\"id\":\"chatcmpl-131b9995cd0c40c99e2ce958083ee406\",\"object\":\"chat.completion.chunk\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":83,\"prompt_time\":2.36,\"prompt_tokens_per_sec\":35.17,\"completion_tokens\":192,\"completion_time\":11.22,\"completion_tokens_per_sec\":17.11,\"total_tokens\":275,\"total_time\":13.63},\"model\":\"SC_6.00bpw_H6_V6\"}\n\n" +
		"data: [DONE]\n\n")
	entry, err := processStreamingResponse("m", time.Now(), body)
	if err != nil {
		t.Fatalf("processStreamingResponse: %v", err)
	}
	if entry.Tokens.InputTokens != 83 || entry.Tokens.OutputTokens != 192 {
		t.Fatalf("tokens = %+v", entry.Tokens)
	}
	if got, want := entry.Tokens.PromptPerSecond, 35.17; math.Abs(got-want) > 1e-9 {
		t.Errorf("PromptPerSecond = %v, want %v", got, want)
	}
	if got, want := entry.Tokens.TokensPerSecond, 17.11; math.Abs(got-want) > 1e-9 {
		t.Errorf("TokensPerSecond = %v, want %v", got, want)
	}
}

func TestServer_ProcessStreamingResponse(t *testing.T) {
	body := []byte("data: {\"choices\":[{}]}\n\n" +
		"data: {\"usage\":{\"prompt_tokens\":15,\"completion_tokens\":33}}\n\n" +
		"data: [DONE]\n\n")
	entry, err := processStreamingResponse("m", time.Now(), body)
	if err != nil {
		t.Fatalf("processStreamingResponse: %v", err)
	}
	if entry.Tokens.InputTokens != 15 || entry.Tokens.GeneratedTokens != 33 || entry.Tokens.OutputTokens != 33 {
		t.Fatalf("tokens = %+v", entry.Tokens)
	}
}

func TestServer_ProcessStreamingResponse_WrappedResponsesUsage(t *testing.T) {
	body := []byte("data: {\"event\":\"response.completed\",\"data\":{\"response\":{\"usage\":{\"input_tokens\":80,\"output_tokens\":25},\"metrics\":{\"mean_itl_ms\":20}}}}\n\n")
	entry, err := processStreamingResponse("m", time.Now(), body)
	if err != nil {
		t.Fatalf("processStreamingResponse: %v", err)
	}
	if entry.Tokens.InputTokens != 80 || entry.Tokens.GeneratedTokens != 25 || entry.Tokens.OutputTokens != 25 {
		t.Fatalf("tokens = %+v", entry.Tokens)
	}
	if entry.Tokens.TokensPerSecond != 50 {
		t.Errorf("TokensPerSecond = %v, want 50", entry.Tokens.TokensPerSecond)
	}
}

func TestServer_ProcessStreamingResponse_AuthoritativeReasoningZero(t *testing.T) {
	body := []byte("data: {\"response\":{\"usage\":{\"input_tokens\":8,\"output_tokens\":5,\"output_tokens_details\":{\"reasoning_tokens\":0}}}}\n\n")
	entry, err := processStreamingResponse("m", time.Now(), body)
	if err != nil {
		t.Fatalf("processStreamingResponse: %v", err)
	}
	if !entry.reasoningTokensReported {
		t.Fatal("numeric reasoning zero was not treated as authoritative")
	}
	if entry.Tokens.GeneratedTokens != 5 || entry.Tokens.ReasoningTokens != 0 || entry.Tokens.OutputTokens != 5 {
		t.Fatalf("tokens = %+v", entry.Tokens)
	}
}

func TestServer_ProcessStreamingResponse_AuthoritativeTokenZeros(t *testing.T) {
	body := []byte("data: {\"usage\":{\"prompt_tokens\":8,\"completion_tokens\":5,\"prompt_tokens_details\":{\"cached_tokens\":2},\"completion_tokens_details\":{\"reasoning_tokens\":3}}}\n\n" +
		"data: {\"usage\":{\"prompt_tokens\":0,\"completion_tokens\":0,\"prompt_tokens_details\":{\"cached_tokens\":0},\"completion_tokens_details\":{\"reasoning_tokens\":0}}}\n\n")
	entry, err := processStreamingResponse("m", time.Now(), body)
	if err != nil {
		t.Fatalf("processStreamingResponse: %v", err)
	}
	if !entry.reasoningTokensReported {
		t.Fatal("numeric reasoning zero was not treated as authoritative")
	}
	if entry.Tokens.InputTokens != 0 || entry.Tokens.GeneratedTokens != 0 ||
		entry.Tokens.CachedTokens != 0 || entry.Tokens.ReasoningTokens != 0 || entry.Tokens.OutputTokens != 0 {
		t.Fatalf("tokens = %+v", entry.Tokens)
	}
}

func TestServer_ProcessStreamingResponse_NullTokensDoNotOverride(t *testing.T) {
	body := []byte("data: {\"usage\":{\"prompt_tokens\":8,\"completion_tokens\":5,\"prompt_tokens_details\":{\"cached_tokens\":2},\"completion_tokens_details\":{\"reasoning_tokens\":3}}}\n\n" +
		"data: {\"usage\":{\"prompt_tokens\":null,\"completion_tokens\":null,\"prompt_tokens_details\":{\"cached_tokens\":null},\"completion_tokens_details\":{\"reasoning_tokens\":null}}}\n\n")
	entry, err := processStreamingResponse("m", time.Now(), body)
	if err != nil {
		t.Fatalf("processStreamingResponse: %v", err)
	}
	if entry.Tokens.InputTokens != 8 || entry.Tokens.GeneratedTokens != 5 ||
		entry.Tokens.CachedTokens != 2 || entry.Tokens.ReasoningTokens != 3 || entry.Tokens.OutputTokens != 2 {
		t.Fatalf("tokens = %+v", entry.Tokens)
	}
}

func TestServer_RecordMetrics_WrappedResponsesUsage(t *testing.T) {
	mm := newTestMetricsMonitor(t, logmon.NewWriter(io.Discard), 10, 0)
	r := httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	w := httptest.NewRecorder()
	copier := newBodyCopier(w)
	copier.Header().Set("Content-Type", "application/json")
	copier.WriteHeader(http.StatusOK)
	copier.Write([]byte(`{"data":{"response":{"usage":{"input_tokens":120,"output_tokens":45}}}}`))

	mm.record("m", r, copier, 0, nil, nil, reasoningTokenizerTarget{})

	entries := metricsEntries(t, mm)
	if len(entries) != 1 {
		t.Fatalf("want 1 entry, got %d", len(entries))
	}
	if entries[0].Tokens.InputTokens != 120 || entries[0].Tokens.GeneratedTokens != 45 || entries[0].Tokens.OutputTokens != 45 {
		t.Fatalf("tokens = %+v", entries[0].Tokens)
	}
}

func TestServer_ProcessStreamingResponse_VLLMMetrics(t *testing.T) {
	body := []byte(`data: {"id":"chatcmpl-b7a832cea986aea4","object":"chat.completion.chunk","choices":[],"usage":{"prompt_tokens":14,"total_tokens":166,"completion_tokens":152},"metrics":{"time_to_first_token_ms":70,"mean_itl_ms":10,"tokens_per_second":24.116032676555495}}

data: [DONE]
`)
	entry, err := processStreamingResponse("m", time.Now(), body)
	if err != nil {
		t.Fatalf("processStreamingResponse: %v", err)
	}
	if entry.Tokens.InputTokens != 14 || entry.Tokens.GeneratedTokens != 152 || entry.Tokens.OutputTokens != 152 {
		t.Fatalf("tokens = %+v", entry.Tokens)
	}
	if entry.Tokens.CachedTokens != -1 {
		t.Errorf("CachedTokens = %d, want -1", entry.Tokens.CachedTokens)
	}
	if got, want := entry.Tokens.PromptPerSecond, 200.0; math.Abs(got-want) > 1e-9 {
		t.Errorf("PromptPerSecond = %v, want %v", got, want)
	}
	if entry.Tokens.TokensPerSecond != 100 {
		t.Errorf("TokensPerSecond = %v, want 100", entry.Tokens.TokensPerSecond)
	}
}

func TestServer_ParseMetrics_VLLMMetrics(t *testing.T) {
	body := `{"id":"chatcmpl-abc123","object":"chat.completion","usage":{"prompt_tokens":42,"completion_tokens":128,"total_tokens":170,"prompt_tokens_details":{"cached_tokens":20}},"metrics":{"time_to_first_token_ms":85.2,"generation_time_ms":1240.5,"queue_time_ms":12.3,"mean_itl_ms":9.1,"tokens_per_second":103.2}}`
	parsed := gjson.Parse(body)
	entry, err := parseMetrics("m", time.Now(), parsed.Get("usage"), parsed.Get("timings"), parsed.Get("metrics"))
	if err != nil {
		t.Fatalf("parseMetrics: %v", err)
	}
	if entry.Tokens.InputTokens != 42 || entry.Tokens.GeneratedTokens != 128 || entry.Tokens.OutputTokens != 128 || entry.Tokens.CachedTokens != 20 {
		t.Fatalf("tokens = %+v", entry.Tokens)
	}
	if got, want := entry.Tokens.PromptPerSecond, float64(42-20)/(85.2/1000); math.Abs(got-want) > 1e-9 {
		t.Errorf("PromptPerSecond = %v, want %v", got, want)
	}
	if got, want := entry.Tokens.TokensPerSecond, 1000/9.1; math.Abs(got-want) > 1e-9 {
		t.Errorf("TokensPerSecond = %v, want %v", got, want)
	}
}

func TestServer_ParseMetrics_VLLMSpeculativeDecoding(t *testing.T) {
	body := `{"id":"chatcmpl-abc123","object":"chat.completion","usage":{"prompt_tokens":42,"completion_tokens":128},"metrics":{"mean_itl_ms":9.1,"speculative_decoding":{"mean_acceptance_length":1.7,"draft_acceptance_rate":0.7,"acceptance_histogram":[6,14],"num_spec_steps":20,"num_accepted_draft_tokens":14,"num_draft_tokens":20,"num_spec_tokens":1}}}`
	parsed := gjson.Parse(body)
	entry, err := parseMetrics("m", time.Now(), parsed.Get("usage"), parsed.Get("timings"), parsed.Get("metrics"))
	if err != nil {
		t.Fatalf("parseMetrics: %v", err)
	}
	if entry.Tokens.DraftTokens != 20 || entry.Tokens.DraftAccTokens != 14 {
		t.Fatalf("draft tokens = %+v, want 14/20", entry.Tokens)
	}
}

// A speculative_decoding object missing either counter must leave both unset so
// the acceptance rate is not computed from half the data.
func TestServer_ParseMetrics_VLLMSpeculativeDecodingPartial(t *testing.T) {
	body := `{"usage":{"prompt_tokens":42,"completion_tokens":128},"metrics":{"speculative_decoding":{"num_draft_tokens":20}}}`
	parsed := gjson.Parse(body)
	entry, err := parseMetrics("m", time.Now(), parsed.Get("usage"), parsed.Get("timings"), parsed.Get("metrics"))
	if err != nil {
		t.Fatalf("parseMetrics: %v", err)
	}
	if entry.Tokens.DraftTokens != -1 || entry.Tokens.DraftAccTokens != -1 {
		t.Fatalf("draft tokens = %+v, want -1/-1", entry.Tokens)
	}
}

// vLLM only emits the metrics object on the final streamed chunk.
func TestServer_ProcessStreamingResponse_VLLMSpeculativeDecoding(t *testing.T) {
	body := []byte(`data: {"choices":[{"delta":{"content":"hi"}}]}

data: {"object":"chat.completion.chunk","choices":[],"usage":{"prompt_tokens":11742,"completion_tokens":35},"metrics":{"mean_itl_ms":17.209908293272534,"speculative_decoding":{"mean_acceptance_length":1.7,"draft_acceptance_rate":0.7,"num_spec_steps":20,"num_accepted_draft_tokens":14,"num_draft_tokens":20,"num_spec_tokens":1}}}

data: [DONE]
`)
	entry, err := processStreamingResponse("m", time.Now(), body)
	if err != nil {
		t.Fatalf("processStreamingResponse: %v", err)
	}
	if entry.Tokens.DraftTokens != 20 || entry.Tokens.DraftAccTokens != 14 {
		t.Fatalf("draft tokens = %+v, want 14/20", entry.Tokens)
	}
}

// llama-server timings and vLLM metrics never appear together, but a response
// carrying both must not have its timings-sourced draft counts clobbered.
func TestServer_ParseMetrics_TimingsDraftTokensNotOverwritten(t *testing.T) {
	body := `{"timings":{"prompt_n":20,"predicted_n":50,"draft_n":30,"draft_n_accepted":12},"metrics":{"speculative_decoding":{}}}`
	parsed := gjson.Parse(body)
	entry, err := parseMetrics("m", time.Now(), parsed.Get("usage"), parsed.Get("timings"), parsed.Get("metrics"))
	if err != nil {
		t.Fatalf("parseMetrics: %v", err)
	}
	if entry.Tokens.DraftTokens != 30 || entry.Tokens.DraftAccTokens != 12 {
		t.Fatalf("draft tokens = %+v, want 12/30", entry.Tokens)
	}
}

func TestServer_ProcessStreamingResponse_NoData(t *testing.T) {
	if _, err := processStreamingResponse("m", time.Now(), []byte("data: [DONE]\n\n")); err == nil {
		t.Fatal("expected error for stream with no usage data")
	}
}

func TestMetricsMonitor_RecordMetadata(t *testing.T) {
	mm := newTestMetricsMonitor(t, nil, 10, 0)
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"usage":{}}`))
	r = r.WithContext(swaputil.SetContext(r.Context(), swaputil.ReqContextData{
		ModelID:  "m",
		Metadata: map[string]string{"client": "web", "trace": "abc"},
	}))

	w := httptest.NewRecorder()
	copier := newBodyCopier(w)
	copier.WriteHeader(http.StatusOK)
	copier.Write([]byte(`{"usage":{"prompt_tokens":1,"completion_tokens":2}}`))

	mm.record("m", r, copier, 0, nil, nil, reasoningTokenizerTarget{})

	entries := metricsEntries(t, mm)
	if len(entries) != 1 {
		t.Fatalf("want 1 entry, got %d", len(entries))
	}
	if entries[0].Metadata["client"] != "web" {
		t.Errorf("client = %q, want web", entries[0].Metadata["client"])
	}
	if entries[0].Metadata["trace"] != "abc" {
		t.Errorf("trace = %q, want abc", entries[0].Metadata["trace"])
	}
}

// TestMetricsMonitor_RecordClientClosed covers #1029: a client that hangs up
// before a response is written must not be filed as a successful (empty-body)
// metric. It is recorded with the 499 sentinel and a client-cancelled
// ErrorMsg, and skips the capture the upstream-failure path would store.
func TestMetricsMonitor_RecordClientClosed(t *testing.T) {
	mm := newTestMetricsMonitor(t, logmon.NewWriter(io.Discard), 10, 5)
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)

	w := httptest.NewRecorder()
	copier := newBodyCopier(w)
	// Nothing is ever written: this is the cold-load cancellation shape.
	copier.MarkStatus(swaputil.StatusClientClosedRequest)

	mm.record("m", r, copier, captureAll, []byte("req"), nil, reasoningTokenizerTarget{})

	entries := metricsEntries(t, mm)
	if len(entries) != 1 {
		t.Fatalf("want 1 entry, got %d", len(entries))
	}
	entry := entries[0]
	if entry.RespStatusCode != swaputil.StatusClientClosedRequest {
		t.Errorf("status = %d, want %d", entry.RespStatusCode, swaputil.StatusClientClosedRequest)
	}
	if entry.ErrorMsg != "client disconnected before response" {
		t.Errorf("error_msg = %q, want client-cancelled message", entry.ErrorMsg)
	}
	if entry.HasCapture {
		t.Error("a cancelled request has no response to capture")
	}
}

// TestServer_MetricsMiddleware_ClientClosed checks the middleware derives the
// sentinel for a handler that returns without writing, and that the marker
// propagates outward to the access-log recorder so both agree.
func TestServer_MetricsMiddleware_ClientClosed(t *testing.T) {
	mm := newTestMetricsMonitor(t, logmon.NewWriter(io.Discard), 10, 0)
	cfg := config.Config{Models: map[string]config.ModelConfig{"m": {}}}

	proxylog := logmon.NewWriter(io.Discard)
	handler := chain.New(
		CreateRequestLogMiddleware(proxylog),
		CreateMetricsMiddleware(mm, cfg),
	).ThenFunc(func(w http.ResponseWriter, r *http.Request) {})

	ctx, cancel := context.WithCancel(context.Background())
	body := `{"model":"m","messages":[{"role":"user","content":"hi"}]}`
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body)).WithContext(ctx)
	r.Header.Set("Content-Type", "application/json")
	r.RemoteAddr = "192.168.1.1:5000"
	cancel()

	handler.ServeHTTP(httptest.NewRecorder(), r)

	entries := metricsEntries(t, mm)
	if len(entries) != 1 {
		t.Fatalf("want 1 entry, got %d", len(entries))
	}
	if entries[0].RespStatusCode != swaputil.StatusClientClosedRequest {
		t.Errorf("activity status = %d, want %d", entries[0].RespStatusCode, swaputil.StatusClientClosedRequest)
	}
	if line := string(proxylog.GetHistory()); !strings.Contains(line, "499 0") {
		t.Errorf("access log %q should report 499", line)
	}
}

// TestServer_MetricsMiddleware_ServerSideCancelIsNotClientClosed guards the
// distinction #1029's fix depends on. The inflight middleware derives a
// cancellable context that POST /api/inflight/{id}/cancel fires, so testing
// "is this request's context done?" would report an operator cancelling a
// request from the UI as a client that hung up.
func TestServer_MetricsMiddleware_ServerSideCancelIsNotClientClosed(t *testing.T) {
	mm := newTestMetricsMonitor(t, logmon.NewWriter(io.Discard), 10, 0)
	cfg := config.Config{Models: map[string]config.ModelConfig{"m": {}}}

	proxylog := logmon.NewWriter(io.Discard)
	// The handler stands in for a dispatch that is cancelled mid-flight and
	// answers the still-connected client, as the proxy ErrorHandler does.
	handler := chain.New(
		CreateRequestLogMiddleware(proxylog),
		CreateMetricsMiddleware(mm, cfg),
	).ThenFunc(func(w http.ResponseWriter, r *http.Request) {
		derived, cancel := context.WithCancel(r.Context())
		defer cancel()
		cancel() // the operator cancels it
		r = r.WithContext(derived)
		if swaputil.MarkClientClosed(w, r) {
			return
		}
		w.WriteHeader(http.StatusBadGateway)
	})

	body := `{"model":"m","messages":[{"role":"user","content":"hi"}]}`
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	r.RemoteAddr = "192.168.1.1:5000"

	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)

	if w.Code != http.StatusBadGateway {
		t.Errorf("client got %d, want %d: a connected client must be answered", w.Code, http.StatusBadGateway)
	}
	entries := metricsEntries(t, mm)
	if len(entries) != 1 {
		t.Fatalf("want 1 entry, got %d", len(entries))
	}
	if entries[0].RespStatusCode == swaputil.StatusClientClosedRequest {
		t.Error("a server-side cancel must not be recorded as a client disconnect")
	}
	if line := string(proxylog.GetHistory()); strings.Contains(line, "499") {
		t.Errorf("access log %q should not report 499 for a connected client", line)
	}
}

func TestMetricsMonitor_RecordFailedRequestCapture(t *testing.T) {
	mm := newTestMetricsMonitor(t, logmon.NewWriter(io.Discard), 10, 5)
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	reqHeaders := map[string]string{"content-type": "application/json"}

	w := httptest.NewRecorder()
	copier := newBodyCopier(w)
	copier.Header().Set("Content-Type", "application/json")
	copier.WriteHeader(http.StatusBadGateway)
	copier.Write([]byte(`{"error":{"message":"model unavailable"}}`))

	reqBody := []byte(`{"model":"m","messages":[]}`)
	mm.record("m", r, copier, captureAll, reqBody, reqHeaders, reasoningTokenizerTarget{})

	entries := metricsEntries(t, mm)
	if len(entries) != 1 {
		t.Fatalf("want 1 entry, got %d", len(entries))
	}
	entry := entries[0]
	if entry.RespStatusCode != http.StatusBadGateway {
		t.Errorf("status = %d, want %d", entry.RespStatusCode, http.StatusBadGateway)
	}
	if entry.ErrorMsg != "model unavailable" {
		t.Errorf("error_msg = %q, want extracted message", entry.ErrorMsg)
	}
	if !entry.HasCapture {
		t.Fatal("failed request should capture the request so it can be inspected")
	}

	got := mm.getCaptureByID(entry.ID)
	if got == nil {
		t.Fatal("capture not found")
	}
	if string(got.ReqBody) != `{"model":"m","messages":[]}` {
		t.Errorf("req body = %q", got.ReqBody)
	}
	if len(got.RespBody) != 0 {
		t.Errorf("resp body stored for failed request (len=%d); want none", len(got.RespBody))
	}
	if got.RespHeaders["Content-Type"] != "application/json" {
		t.Errorf("resp Content-Type = %q", got.RespHeaders["Content-Type"])
	}
}

func TestMetricsMonitor_RecordFailedRequestStatusFallback(t *testing.T) {
	// Non-JSON error body: ErrorMsg falls back to the HTTP status text.
	mm := newTestMetricsMonitor(t, logmon.NewWriter(io.Discard), 10, 5)
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)

	w := httptest.NewRecorder()
	copier := newBodyCopier(w)
	copier.WriteHeader(http.StatusBadGateway)
	copier.Write([]byte("<html>upstream down</html>"))

	mm.record("m", r, copier, captureAll, nil, nil, reasoningTokenizerTarget{})

	entries := metricsEntries(t, mm)
	if len(entries) != 1 {
		t.Fatalf("want 1 entry, got %d", len(entries))
	}
	if entries[0].ErrorMsg != "502 Bad Gateway" {
		t.Errorf("error_msg = %q, want status text", entries[0].ErrorMsg)
	}
}

func TestMetricsMonitor_RecordFailedRequestCaptureDisabled(t *testing.T) {
	mm := newTestMetricsMonitor(t, logmon.NewWriter(io.Discard), 10, 0) // captures disabled
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)

	w := httptest.NewRecorder()
	copier := newBodyCopier(w)
	copier.WriteHeader(http.StatusInternalServerError)
	copier.Write([]byte(`{"error":"boom"}`))

	mm.record("m", r, copier, captureAll, []byte("req"), nil, reasoningTokenizerTarget{})

	entries := metricsEntries(t, mm)
	if len(entries) != 1 {
		t.Fatalf("want 1 entry, got %d", len(entries))
	}
	if entries[0].HasCapture {
		t.Fatal("captures disabled, HasCapture should be false")
	}
	// ErrorMsg is independent of whether captures are enabled.
	if entries[0].ErrorMsg != "boom" {
		t.Errorf("error_msg = %q, want boom", entries[0].ErrorMsg)
	}
	if mm.getCaptureByID(entries[0].ID) != nil {
		t.Fatal("no capture should be stored when disabled")
	}
}

func TestMetricsMonitor_RecordDecompressionFailureSetsErrorMsg(t *testing.T) {
	mm := newTestMetricsMonitor(t, logmon.NewWriter(io.Discard), 10, 5)
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)

	w := httptest.NewRecorder()
	copier := newBodyCopier(w)
	copier.Header().Set("Content-Encoding", "gzip")
	copier.WriteHeader(http.StatusOK)
	copier.Write([]byte("not-really-gzip"))

	mm.record("m", r, copier, captureAll, []byte("req"), nil, reasoningTokenizerTarget{})

	entries := metricsEntries(t, mm)
	if len(entries) != 1 {
		t.Fatalf("want 1 entry, got %d", len(entries))
	}
	if entries[0].ErrorMsg == "" {
		t.Fatal("expected ErrorMsg for decompression failure")
	}
	// Raw bytes must not be stored when the body could not be decoded.
	if entries[0].HasCapture {
		t.Fatal("decompression failure should not store a capture")
	}
}

func TestMetricsMonitor_DecodeResponseBody(t *testing.T) {
	mm := newTestMetricsMonitor(t, logmon.NewWriter(io.Discard), 10, 5)

	// No Content-Encoding: body returned unchanged.
	w := httptest.NewRecorder()
	copier := newBodyCopier(w)
	copier.Write([]byte("plain"))
	got, err := mm.decodeResponseBody(copier, "/p")
	if err != nil || string(got) != "plain" {
		t.Fatalf("plain body = %q, err = %v", got, err)
	}

	// Bogus gzip payload: returns an error and no body (no raw bytes kept).
	w2 := httptest.NewRecorder()
	copier2 := newBodyCopier(w2)
	copier2.Header().Set("Content-Encoding", "gzip")
	copier2.Write([]byte("not-really-gzip"))
	got, err = mm.decodeResponseBody(copier2, "/p")
	if err == nil {
		t.Fatal("expected decompression error")
	}
	if got != nil {
		t.Errorf("expected nil body on failure, got %q", got)
	}
}

func TestServer_ExtractErrorMessage(t *testing.T) {
	cases := []struct {
		name string
		body string
		want string
	}{
		{"openai object", `{"error":{"message":"rate limited"}}`, "rate limited"},
		{"string error", `{"error":"bad request"}`, "bad request"},
		{"message field", `{"message":"nope"}`, "nope"},
		{"detail field", `{"detail":"oops"}`, "oops"},
		{"object error ignored", `{"error":{"code":42}}`, ""},
		{"no error", `{"usage":{}}`, ""},
		{"invalid json", `not-json`, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := extractErrorMessage([]byte(tc.body)); got != tc.want {
				t.Errorf("extractErrorMessage = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestServer_ParseMetrics_Infill(t *testing.T) {
	// /infill responses are arrays; timings live in the last element.
	body := `[{"content":"a"},{"content":"b","timings":{"prompt_n":5,"predicted_n":9,"prompt_ms":10,"predicted_ms":20}}]`
	parsed := gjson.Parse(body)
	timings := parsed.Get("timings")
	if arr := parsed.Array(); len(arr) > 0 {
		timings = arr[len(arr)-1].Get("timings")
	}
	entry, err := parseMetrics("m", time.Now(), parsed.Get("usage"), timings, parsed.Get("metrics"))
	if err != nil {
		t.Fatalf("parseMetrics: %v", err)
	}
	if entry.Tokens.InputTokens != 5 || entry.Tokens.GeneratedTokens != 9 || entry.Tokens.OutputTokens != 9 {
		t.Fatalf("tokens = %+v", entry.Tokens)
	}
}

func TestServer_ParseMetrics_ReasoningTokens(t *testing.T) {
	tests := []struct {
		name              string
		usage             string
		wantGenerated     int
		wantReasoning     int
		wantOutput        int
		wantAuthoritative bool
	}{
		{
			name:              "chat completions",
			usage:             `{"completion_tokens":21,"completion_tokens_details":{"reasoning_tokens":8}}`,
			wantGenerated:     21,
			wantReasoning:     8,
			wantOutput:        13,
			wantAuthoritative: true,
		},
		{
			name:              "responses",
			usage:             `{"output_tokens":12,"output_tokens_details":{"reasoning_tokens":3}}`,
			wantGenerated:     12,
			wantReasoning:     3,
			wantOutput:        9,
			wantAuthoritative: true,
		},
		{
			name:              "authoritative zero",
			usage:             `{"completion_tokens":5,"completion_tokens_details":{"reasoning_tokens":0}}`,
			wantGenerated:     5,
			wantReasoning:     0,
			wantOutput:        5,
			wantAuthoritative: true,
		},
		{
			name:              "null is missing",
			usage:             `{"completion_tokens":5,"completion_tokens_details":{"reasoning_tokens":null}}`,
			wantGenerated:     5,
			wantReasoning:     0,
			wantOutput:        5,
			wantAuthoritative: false,
		},
		{
			name:              "reasoning is clamped",
			usage:             `{"completion_tokens":5,"completion_tokens_details":{"reasoning_tokens":8}}`,
			wantGenerated:     5,
			wantReasoning:     5,
			wantOutput:        0,
			wantAuthoritative: true,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			entry, err := parseMetrics("m", time.Now(), gjson.Parse(test.usage), gjson.Result{}, gjson.Result{})
			if err != nil {
				t.Fatalf("parseMetrics: %v", err)
			}
			if entry.Tokens.GeneratedTokens != test.wantGenerated ||
				entry.Tokens.ReasoningTokens != test.wantReasoning ||
				entry.Tokens.OutputTokens != test.wantOutput {
				t.Fatalf("tokens = %+v", entry.Tokens)
			}
			if entry.reasoningTokensReported != test.wantAuthoritative {
				t.Fatalf("reasoningTokensReported = %v, want %v", entry.reasoningTokensReported, test.wantAuthoritative)
			}
		})
	}
}

func TestServer_ExtractReasoningContent(t *testing.T) {
	tests := []struct {
		name string
		body string
		want string
	}{
		{"chat reasoning_content", `{"choices":[{"message":{"reasoning_content":"think"}}]}`, "think"},
		{"chat reasoning", `{"choices":[{"message":{"reasoning":"think"}}]}`, "think"},
		{"anthropic", `{"content":[{"type":"thinking","text":"one"},{"type":"text","text":"answer"},{"type":"thinking","text":"two"}]}`, "onetwo"},
		{"responses wrapped", `{"data":{"response":{"output":[{"type":"reasoning","content":[{"type":"reasoning_text","text":"one"},{"type":"reasoning_text","text":"two"}]}]}}}`, "onetwo"},
		{"responses summary", `{"output":[{"type":"reasoning","summary":[{"type":"summary_text","text":"why"}]}]}`, "why"},
		{"native", `{"reasoning_content":"think"}`, "think"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := extractReasoningContent([]byte(test.body)); got != test.want {
				t.Fatalf("extractReasoningContent = %q, want %q", got, test.want)
			}
		})
	}
}

func TestServer_ExtractStreamingReasoningContent(t *testing.T) {
	body := []byte("data: {\"choices\":[{\"delta\":{\"reasoning_content\":\"one\"}}]}\n\n" +
		"data: {\"delta\":{\"thinking\":\"two\"}}\n\n" +
		"data: {\"data\":{\"type\":\"response.reasoning_text.delta\",\"delta\":\"three\"}}\n\n" +
		"data: [DONE]\n\n")
	if got := extractStreamingReasoningContent(body); got != "onetwothree" {
		t.Fatalf("extractStreamingReasoningContent = %q", got)
	}
}

func TestServer_ResolveReasoningTokenizerTarget(t *testing.T) {
	cfg := config.Config{
		Models: map[string]config.ModelConfig{
			"local":  {Proxy: "http://127.0.0.1:8080/api/"},
			"unsafe": {Proxy: "ftp://127.0.0.1/models"},
		},
		Peers: config.PeerDictionaryConfig{
			"remote": {
				Proxy:  "https://peer.example/base/",
				ApiKey: "secret",
				Models: []string{"org/model"},
			},
		},
	}

	tests := []struct {
		name       string
		model      string
		wantURL    string
		wantAPIKey string
		wantOK     bool
	}{
		{"local", "local", "http://127.0.0.1:8080/api/tokenize", "", true},
		{"peer fully qualified", "remote/org/model", "https://peer.example/base/upstream/org/model/tokenize", "secret", true},
		{"peer unique bare name", "org/model", "https://peer.example/base/upstream/org/model/tokenize", "secret", true},
		{"unsupported local scheme", "unsafe", "", "", false},
		{"unknown", "missing", "", "", false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			target, ok := resolveReasoningTokenizerTarget(cfg, test.model)
			if ok != test.wantOK || target.URL != test.wantURL || target.APIKey != test.wantAPIKey {
				t.Fatalf("target = %+v, ok=%v", target, ok)
			}
		})
	}
}

func TestServer_TokenizeReasoning_VLLMFallbackAndPeerAuth(t *testing.T) {
	var requests atomic.Int32
	tokenizer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if r.Header.Get("Authorization") != "Bearer secret" || r.Header.Get("x-api-key") != "secret" {
			t.Errorf("auth headers = %q, %q", r.Header.Get("Authorization"), r.Header.Get("x-api-key"))
		}
		var payload map[string]any
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Errorf("decode payload: %v", err)
			http.Error(w, "bad payload", http.StatusBadRequest)
			return
		}
		if _, llamaRequest := payload["content"]; llamaRequest {
			http.Error(w, "use vLLM shape", http.StatusBadRequest)
			return
		}
		if payload["prompt"] != "reasoning" || payload["add_special_tokens"] != false {
			t.Errorf("payload = %+v", payload)
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"count":3}`))
	}))
	defer tokenizer.Close()

	got, ok := tokenizeReasoning(context.Background(), tokenizer.Client(), reasoningTokenizerTarget{
		URL:    tokenizer.URL,
		APIKey: "secret",
	}, "reasoning")
	if !ok || got != 3 {
		t.Fatalf("tokenizeReasoning = %d, %v", got, ok)
	}
	if requests.Load() != 2 {
		t.Fatalf("requests = %d, want 2", requests.Load())
	}
}

func TestServer_TokenizeReasoning_DoesNotFollowRedirects(t *testing.T) {
	var redirectedRequests atomic.Int32
	redirectTarget := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		redirectedRequests.Add(1)
		if r.Header.Get("Authorization") != "" || r.Header.Get("x-api-key") != "" {
			t.Errorf("redirect leaked auth headers: %q, %q", r.Header.Get("Authorization"), r.Header.Get("x-api-key"))
		}
		w.Write([]byte(`{"tokens":[1]}`))
	}))
	defer redirectTarget.Close()

	var tokenizerRequests atomic.Int32
	tokenizer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tokenizerRequests.Add(1)
		if r.Header.Get("Authorization") != "Bearer secret" || r.Header.Get("x-api-key") != "secret" {
			t.Errorf("auth headers = %q, %q", r.Header.Get("Authorization"), r.Header.Get("x-api-key"))
		}
		http.Redirect(w, r, redirectTarget.URL, http.StatusTemporaryRedirect)
	}))
	defer tokenizer.Close()

	client := newReasoningHTTPClient()
	defer client.CloseIdleConnections()
	if got, ok := tokenizeReasoning(context.Background(), client, reasoningTokenizerTarget{
		URL:    tokenizer.URL,
		APIKey: "secret",
	}, "reasoning"); ok || got != 0 {
		t.Fatalf("tokenizeReasoning = %d, %v", got, ok)
	}
	if tokenizerRequests.Load() != 2 {
		t.Fatalf("tokenizer requests = %d, want 2", tokenizerRequests.Load())
	}
	if redirectedRequests.Load() != 0 {
		t.Fatalf("redirected requests = %d, want 0", redirectedRequests.Load())
	}
}

func TestServer_TokenizeReasoning_RejectsUnsupportedScheme(t *testing.T) {
	if got, ok := tokenizeReasoning(context.Background(), newReasoningHTTPClient(), reasoningTokenizerTarget{
		URL: "ftp://peer.example/tokenize",
	}, "reasoning"); ok || got != 0 {
		t.Fatalf("tokenizeReasoning = %d, %v", got, ok)
	}
}

func TestMetricsMonitor_ReasoningUpdateWithoutCaptures(t *testing.T) {
	tokenizer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"tokens":[1,2,3]}`))
	}))
	defer tokenizer.Close()

	mm := newTestMetricsMonitor(t, logmon.NewWriter(io.Discard), 10, 0)
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	w := httptest.NewRecorder()
	copier := newBodyCopier(w)
	copier.Header().Set("Content-Type", "application/json")
	copier.Write([]byte(`{"choices":[{"message":{"reasoning_content":"think"}}],"usage":{"prompt_tokens":2,"completion_tokens":7}}`))
	mm.record("m", r, copier, captureAll, nil, nil, reasoningTokenizerTarget{URL: tokenizer.URL})

	deadline := time.Now().Add(time.Second)
	for {
		entries := metricsEntries(t, mm)
		if len(entries) == 1 && entries[0].Tokens.ReasoningTokens == 3 {
			if entries[0].Tokens.GeneratedTokens != 7 || entries[0].Tokens.OutputTokens != 4 {
				t.Fatalf("tokens = %+v", entries[0].Tokens)
			}
			if entries[0].HasCapture {
				t.Fatal("capture should remain disabled")
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("reasoning update did not persist: %+v", entries)
		}
		time.Sleep(time.Millisecond)
	}
}

func TestMetricsMonitor_AuthoritativeReasoningZeroSkipsTokenizer(t *testing.T) {
	called := make(chan struct{}, 1)
	tokenizer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called <- struct{}{}
		w.Write([]byte(`{"tokens":[1]}`))
	}))
	defer tokenizer.Close()

	mm := newTestMetricsMonitor(t, nil, 10, 0)
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	w := httptest.NewRecorder()
	copier := newBodyCopier(w)
	copier.Header().Set("Content-Type", "application/json")
	copier.Write([]byte(`{"choices":[{"message":{"reasoning_content":"should not tokenize"}}],"usage":{"completion_tokens":5,"completion_tokens_details":{"reasoning_tokens":0}}}`))
	mm.record("m", r, copier, 0, nil, nil, reasoningTokenizerTarget{URL: tokenizer.URL})

	select {
	case <-called:
		t.Fatal("tokenizer was called for an authoritative reasoning zero")
	case <-time.After(50 * time.Millisecond):
	}
	entries := metricsEntries(t, mm)
	if len(entries) != 1 || entries[0].Tokens.GeneratedTokens != 5 ||
		entries[0].Tokens.ReasoningTokens != 0 || entries[0].Tokens.OutputTokens != 5 {
		t.Fatalf("entries = %+v", entries)
	}
}

func TestMetricsMonitor_ReasoningUpdatesAreConcurrencyBounded(t *testing.T) {
	started := make(chan struct{}, maxConcurrentReasoningUpdates+1)
	release := make(chan struct{})
	tokenizer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		started <- struct{}{}
		<-release
		w.Write([]byte(`{"tokens":[1]}`))
	}))
	defer tokenizer.Close()

	mm := newTestMetricsMonitor(t, nil, 10, 0)
	target := reasoningTokenizerTarget{URL: tokenizer.URL}
	for range maxConcurrentReasoningUpdates + 1 {
		entry, ok := mm.queueMetrics(ActivityLogEntry{Tokens: TokenMetrics{
			GeneratedTokens: 5,
			OutputTokens:    5,
		}})
		if !ok {
			t.Fatal("queueMetrics failed")
		}
		mm.queueReasoningUpdate(entry.ID, target, "think")
	}
	for range maxConcurrentReasoningUpdates {
		select {
		case <-started:
		case <-time.After(time.Second):
			t.Fatal("timed out waiting for tokenizer requests")
		}
	}
	select {
	case <-started:
		t.Fatalf("more than %d tokenizer requests ran concurrently", maxConcurrentReasoningUpdates)
	case <-time.After(50 * time.Millisecond):
	}
	close(release)
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("queued tokenizer request did not run after a worker became available")
	}
	deadline := time.Now().Add(time.Second)
	for {
		entries := metricsEntries(t, mm)
		updated := len(entries) == maxConcurrentReasoningUpdates+1
		for _, entry := range entries {
			updated = updated && entry.Tokens.ReasoningTokens == 1 && entry.Tokens.OutputTokens == 4
		}
		if updated {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("queued reasoning updates did not persist: %+v", entries)
		}
		time.Sleep(time.Millisecond)
	}
	if err := mm.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

// TestServer_MetricsMiddleware_UpstreamAudioCaptureSkipsRespBody verifies that
// an /upstream/<model>/v1/audio/speech request uses the path-specific capture
// mask (headers only) rather than falling back to captureAll.
func TestServer_MetricsMiddleware_UpstreamAudioCaptureSkipsRespBody(t *testing.T) {
	mm := newTestMetricsMonitor(t, logmon.NewWriter(io.Discard), 100, 5)
	cfg := config.Config{Models: map[string]config.ModelConfig{"m1": {}}}

	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "audio/mpeg")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("BINARY-AUDIO-DATA"))
	})
	handler := CreateMetricsMiddleware(mm, cfg)(inner)

	req := httptest.NewRequest(http.MethodPost, "/upstream/m1/v1/audio/speech", strings.NewReader(`{"model":"m1"}`))
	handler.ServeHTTP(httptest.NewRecorder(), req)

	entries := metricsEntries(t, mm)
	if len(entries) == 0 {
		t.Fatal("no metrics recorded")
	}
	last := entries[len(entries)-1]
	if !last.HasCapture {
		t.Fatal("expected capture to be stored")
	}
	cap := mm.getCaptureByID(last.ID)
	if cap == nil {
		t.Fatal("capture not found")
	}
	if len(cap.RespBody) != 0 {
		t.Errorf("RespBody stored for /upstream audio route (len=%d); want path-specific mask to skip body", len(cap.RespBody))
	}
	if len(cap.RespHeaders) == 0 {
		t.Error("RespHeaders not stored; want captureRespHeaders mask")
	}
}
