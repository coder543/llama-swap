package server

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestServer_SystemOne(t *testing.T) {
	response := `{"model":"m1","answers":{"ok":{"type":"noul","noul":0.9}},"usage":{"input_tokens":12,"output_tokens":0}}`
	body := `{"model":"m1","state":{"invoice":{"status":"overdue"}},"questions":{"ok":{"type":"noul","instructions":"Is the invoice overdue?"}}}`
	for _, tc := range []struct{ path, upstreamPath string }{
		{"/v1/systemone", "/v1/systemone"},
		{"/upstream/m1/v1/systemone", "/v1/systemone"},
		{"/v/systemone", "/systemone"},
	} {
		t.Run(tc.path, func(t *testing.T) {
			s := upstreamMetricsServer(t, response)
			s.local.(*stubRouter).serveHTTP = func(w http.ResponseWriter, r *http.Request) {
				assert.Equal(t, tc.upstreamPath, r.URL.Path)
				forwarded, err := io.ReadAll(r.Body)
				require.NoError(t, err)
				assert.JSONEq(t, gjson.Get(body, "state").Raw, gjson.GetBytes(forwarded, "state").Raw)
				assert.JSONEq(t, gjson.Get(body, "questions").Raw, gjson.GetBytes(forwarded, "questions").Raw)
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, response)
			}
			req := httptest.NewRequest(http.MethodPost, tc.path, strings.NewReader(body))
			req.Header.Set("Content-Type", "application/json")
			w := httptest.NewRecorder()
			s.ServeHTTP(w, req)
			require.Equal(t, http.StatusOK, w.Code)
			assert.JSONEq(t, response, w.Body.String())
			entries := metricsEntries(t, s.metrics)
			require.Len(t, entries, 1)
			assert.Equal(t, "m1", entries[0].Model)
			assert.Equal(t, 12, entries[0].Tokens.InputTokens)
			assert.Zero(t, entries[0].Tokens.OutputTokens)
		})
	}
}

func TestServer_SystemOneRequiresAPIKey(t *testing.T) {
	s := upstreamMetricsServer(t, "ok")
	s.cfg.RequiredAPIKeys = []string{"secret"}
	s.routes()
	req := httptest.NewRequest(http.MethodPost, "/v1/systemone", strings.NewReader(`{"model":"m1"}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	s.ServeHTTP(w, req)
	assert.Equal(t, http.StatusUnauthorized, w.Code)
	assert.Empty(t, metricsEntries(t, s.metrics))
}
