package proxy

import (
	"bytes"
	"compress/flate"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/fxamacker/cbor/v2"
	"github.com/gin-gonic/gin"
	"github.com/klauspost/compress/zstd"
	"github.com/mostlygeek/llama-swap/event"
	"github.com/mostlygeek/llama-swap/proxy/cache"
	"github.com/tidwall/gjson"
)

// zstdEncOptions are the shared zstd encoder options for maximum compression.
var zstdEncOptions = []zstd.EOption{
	zstd.WithEncoderLevel(zstd.SpeedBetterCompression),
}

// zstdDecOptions are the shared zstd decoder options.
var zstdDecOptions = []zstd.DOption{}

// zstdEncPool pools zstd.Encoder instances to reduce allocations.
var zstdEncPool = &sync.Pool{
	New: func() interface{} {
		enc, _ := zstd.NewWriter(nil, zstdEncOptions...)
		return enc
	},
}

// zstdDecPool pools zstd.Decoder instances to reduce allocations.
var zstdDecPool = &sync.Pool{
	New: func() interface{} {
		dec, _ := zstd.NewReader(nil, zstdDecOptions...)
		return dec
	},
}

// compressCapture marshals a ReqRespCapture to CBOR and compresses it with zstd.
// Returns compressed bytes and the original CBOR byte count for logging.
func compressCapture(c *ReqRespCapture) ([]byte, int, error) {
	cborBytes, err := cbor.Marshal(c)
	if err != nil {
		return nil, 0, fmt.Errorf("marshal capture: %w", err)
	}
	zenc := zstdEncPool.Get().(*zstd.Encoder)
	defer zstdEncPool.Put(zenc)
	return zenc.EncodeAll(cborBytes, nil), len(cborBytes), nil
}

// decompressCapture decompresses zstd-compressed CBOR and unmarshals it into a ReqRespCapture.
func decompressCapture(data []byte) (*ReqRespCapture, error) {
	dec := zstdDecPool.Get().(*zstd.Decoder)
	defer zstdDecPool.Put(dec)
	cborBytes, err := dec.DecodeAll(data, nil)
	if err != nil {
		return nil, fmt.Errorf("decompress capture: %w", err)
	}
	var capture ReqRespCapture
	if err := cbor.Unmarshal(cborBytes, &capture); err != nil {
		return nil, fmt.Errorf("unmarshal capture: %w", err)
	}
	return &capture, nil
}

// TokenMetrics holds token usage and performance metrics
type TokenMetrics struct {
	CachedTokens    int     `json:"cache_tokens"`
	InputTokens     int     `json:"input_tokens"`
	GeneratedTokens int     `json:"generated_tokens"`
	ReasoningTokens int     `json:"reasoning_tokens"`
	OutputTokens    int     `json:"output_tokens"`
	PromptPerSecond float64 `json:"prompt_per_second"`
	TokensPerSecond float64 `json:"tokens_per_second"`
}

// ActivityLogEntry represents parsed token statistics from llama-server logs
type ActivityLogEntry struct {
	ID              int          `json:"id"`
	Timestamp       time.Time    `json:"timestamp"`
	Model           string       `json:"model"`
	ReqPath         string       `json:"req_path"`
	RespContentType string       `json:"resp_content_type"`
	RespStatusCode  int          `json:"resp_status_code"`
	Tokens          TokenMetrics `json:"tokens"`
	DurationMs      int          `json:"duration_ms"`
	HasCapture      bool         `json:"has_capture"`
}

type ReqRespCapture struct {
	ID          int               `json:"id"`
	ReqPath     string            `json:"req_path"`
	ReqHeaders  map[string]string `json:"req_headers"`
	ReqBody     []byte            `json:"req_body"`
	RespHeaders map[string]string `json:"resp_headers"`
	RespBody    []byte            `json:"resp_body"`
}

// ActivityLogEvent represents a token metrics event
type ActivityLogEvent struct {
	Metrics ActivityLogEntry
}

func (e ActivityLogEvent) Type() uint32 {
	return ActivityLogEventID // defined in events.go
}

// metricsMonitor parses llama-server output for token statistics
type metricsMonitor struct {
	mu         sync.RWMutex
	metrics    []ActivityLogEntry
	maxMetrics int
	nextID     int
	logger     *LogMonitor

	// capture fields
	enableCaptures bool
	captureCache   *cache.Cache // zstd-compressed CBOR of ReqRespCapture

	// upstreamURLFunc returns the upstream llama-server URL for a model ID.
	// Used to call the /tokenize endpoint for reasoning token counts.
	upstreamURLFunc func(modelID string) string
}

// SetUpstreamURLFunc sets the callback that resolves a model ID to its
// upstream llama-server base URL. This is needed to tokenize reasoning
// content for per-category token counts.
func (mp *metricsMonitor) SetUpstreamURLFunc(fn func(modelID string) string) {
	mp.upstreamURLFunc = fn
}

// newMetricsMonitor creates a new metricsMonitor. captureBufferMB is the
// capture buffer size in megabytes; 0 disables captures.
func newMetricsMonitor(logger *LogMonitor, maxMetrics int, captureBufferMB int) *metricsMonitor {
	mm := &metricsMonitor{
		logger:         logger,
		maxMetrics:     maxMetrics,
		enableCaptures: captureBufferMB > 0,
	}
	if captureBufferMB > 0 {
		mm.captureCache = cache.New(captureBufferMB * 1024 * 1024)
	}
	return mm
}

// queueMetrics adds a new metric to the collection without emitting an event.
// Returns the assigned metric ID. Call emitMetric after capture setup.
func (mp *metricsMonitor) queueMetrics(metric ActivityLogEntry) int {
	mp.mu.Lock()
	defer mp.mu.Unlock()

	metric.ID = mp.nextID
	mp.nextID++
	mp.metrics = append(mp.metrics, metric)
	if len(mp.metrics) > mp.maxMetrics {
		mp.metrics = mp.metrics[len(mp.metrics)-mp.maxMetrics:]
	}
	return metric.ID
}

// emitMetric publishes an ActivityLogEvent for the given metric.
func (mp *metricsMonitor) emitMetric(metric ActivityLogEntry) {
	event.Emit(ActivityLogEvent{Metrics: metric})
}

// addCapture compresses and stores a capture in the cache.
// Returns true if the capture was stored, false otherwise.
func (mp *metricsMonitor) addCapture(capture ReqRespCapture) bool {
	if !mp.enableCaptures {
		return false
	}

	compressed, uncompressedBytes, err := compressCapture(&capture)
	if err != nil {
		mp.logger.Warnf("failed to compress capture: %v, skipping", err)
		return false
	}

	if err := mp.captureCache.Add(capture.ID, compressed); err != nil {
		mp.logger.Warnf("capture %d too large (%d bytes), skipping: %v", capture.ID, len(compressed), err)
		return false
	}

	compressionRatio := (1 - float64(len(compressed))/float64(uncompressedBytes)) * 100
	mp.logger.Debugf("Capture %d compressed and saved: %d bytes -> %d bytes (%.1f%% compression)", capture.ID, uncompressedBytes, len(compressed), compressionRatio)
	return true
}

// getCompressedBytes returns the raw compressed bytes for a capture by ID.
func (mp *metricsMonitor) getCompressedBytes(id int) ([]byte, bool) {
	if mp.captureCache == nil {
		return nil, false
	}
	data, err := mp.captureCache.Get(id)
	if err != nil {
		return nil, false
	}
	return data, true
}

// getCaptureByID decompresses and unmarshals a capture by ID.
// Returns nil if the capture is not found or decompression fails.
func (mp *metricsMonitor) getCaptureByID(id int) *ReqRespCapture {
	if mp.captureCache == nil {
		return nil
	}
	data, exists := mp.getCompressedBytes(id)
	if !exists {
		return nil
	}

	capture, err := decompressCapture(data)
	if err != nil {
		mp.logger.Warnf("failed to decompress capture %d: %v", id, err)
		return nil
	}

	return capture
}

// getMetrics returns a copy of the current metrics
func (mp *metricsMonitor) getMetrics() []ActivityLogEntry {
	mp.mu.RLock()
	defer mp.mu.RUnlock()

	result := make([]ActivityLogEntry, len(mp.metrics))
	copy(result, mp.metrics)
	return result
}

// getMetricsJSON returns metrics as JSON
func (mp *metricsMonitor) getMetricsJSON() ([]byte, error) {
	mp.mu.RLock()
	defer mp.mu.RUnlock()

	if mp.captureCache == nil {
		return json.Marshal(mp.metrics)
	}

	// Make a copy with up-to-date has_capture from cache
	result := make([]ActivityLogEntry, len(mp.metrics))
	for i, m := range mp.metrics {
		m.HasCapture = mp.captureCache.Has(m.ID)
		result[i] = m
	}
	return json.Marshal(result)
}

// Capture field flags for controlling what is saved in ReqRespCapture.
type captureFields uint

const (
	captureNone captureFields = 1 << iota
	captureReqHeaders
	captureReqBody
	captureRespHeaders
	captureRespBody
)

const (
	captureReqAll  = captureReqHeaders | captureReqBody
	captureRespAll = captureRespHeaders | captureRespBody
	captureAll     = captureReqAll | captureRespAll
)

// wrapHandler wraps the proxy handler to extract token metrics.
// captureFields controls what is saved in the ReqRespCapture using bitwise flags.
// upstreamURL is the base URL of the upstream llama-server (for tokenize calls).
// if wrapHandler returns an error it is safe to assume that no
// data was sent to the client
func (mp *metricsMonitor) wrapHandler(
	modelID string,
	writer gin.ResponseWriter,
	request *http.Request,
	captureFields captureFields,
	upstreamURL string,
	next func(modelID string, w http.ResponseWriter, r *http.Request) error,
) error {
	// Capture request body and headers if captures enabled
	var reqBody []byte
	var reqHeaders map[string]string
	if mp.enableCaptures && (captureFields&captureReqBody) != 0 {
		if request.Body != nil {
			var err error
			reqBody, err = io.ReadAll(request.Body)
			if err != nil {
				return fmt.Errorf("failed to read request body for capture: %w", err)
			}
			request.Body.Close()
			request.Body = io.NopCloser(bytes.NewBuffer(reqBody))
		}
	}
	if mp.enableCaptures && (captureFields&captureReqHeaders) != 0 {
		reqHeaders = make(map[string]string)
		for key, values := range request.Header {
			if len(values) > 0 {
				reqHeaders[key] = values[0]
			}
		}
		redactHeaders(reqHeaders)
	}

	recorder := newBodyCopier(writer)

	// Filter Accept-Encoding to only include encodings we can decompress for metrics
	if ae := request.Header.Get("Accept-Encoding"); ae != "" {
		request.Header.Set("Accept-Encoding", filterAcceptEncoding(ae))
	}

	if err := next(modelID, recorder, request); err != nil {
		return err
	}

	// after this point we have to assume that data was sent to the client
	// and we can only log errors but not send them to clients

	// Initialize default metrics - recorded for every request
	tm := ActivityLogEntry{
		Timestamp:       time.Now(),
		Model:           modelID,
		ReqPath:         request.URL.Path,
		RespContentType: recorder.Header().Get("Content-Type"),
		RespStatusCode:  recorder.Status(),
		DurationMs:      int(time.Since(recorder.StartTime()).Milliseconds()),
	}

	if recorder.Status() != http.StatusOK {
		mp.logger.Warnf("non-200 response, recording partial metrics: status=%d, path=%s", recorder.Status(), request.URL.Path)
		tm.ID = mp.queueMetrics(tm)
		mp.emitMetric(tm)
		return nil
	}

	body := recorder.body.Bytes()
	if len(body) == 0 {
		mp.logger.Warn("metrics: empty body, recording minimal metrics")
		tm.ID = mp.queueMetrics(tm)
		mp.emitMetric(tm)
		return nil
	}

	// Decompress if needed
	if encoding := recorder.Header().Get("Content-Encoding"); encoding != "" {
		var err error
		body, err = decompressBody(body, encoding)
		if err != nil {
			mp.logger.Warnf("metrics: decompression failed: %v, path=%s, recording minimal metrics", err, request.URL.Path)
			tm.ID = mp.queueMetrics(tm)
			mp.emitMetric(tm)
			return nil
		}
	}
	if strings.Contains(recorder.Header().Get("Content-Type"), "text/event-stream") {
		if parsed, err := processStreamingResponse(modelID, request.URL.Path, recorder.StartTime(), body); err != nil {
			mp.logger.Warnf("error processing streaming response: %v, path=%s, recording minimal metrics", err, request.URL.Path)
		} else {
			applyObservedStreamingSpeeds(&parsed, recorder.StartTime(), recorder.FirstWriteTime(), recorder.LastWriteTime())
			tm.Tokens = parsed.Tokens
			tm.DurationMs = parsed.DurationMs
		}
	} else {
		if gjson.ValidBytes(body) {
			parsed := gjson.ParseBytes(body)
			usage, timings, performance := findMetricsPayload(parsed, request.URL.Path)

			if usage.Exists() || timings.Exists() || performance.Exists() {
				if parsedMetrics, err := parseMetrics(modelID, recorder.StartTime(), usage, timings, performance); err != nil {
					mp.logger.Warnf("error parsing metrics: %v, path=%s, recording minimal metrics", err, request.URL.Path)
				} else {
					tm.Tokens = parsedMetrics.Tokens
					tm.DurationMs = parsedMetrics.DurationMs
				}
			}
		} else {
			mp.logger.Warnf("metrics: invalid JSON in response body path=%s, recording minimal metrics", request.URL.Path)
		}
	}

	// Build capture if enabled and determine if it will be stored
	var capture *ReqRespCapture
	if mp.enableCaptures {
		var respHeaders map[string]string
		var respBody []byte
		if (captureFields & captureRespHeaders) != 0 {
			respHeaders = make(map[string]string)
			for key, values := range recorder.Header() {
				if len(values) > 0 {
					respHeaders[key] = values[0]
				}
			}
			redactHeaders(respHeaders)
			delete(respHeaders, "Content-Encoding")
		}
		if (captureFields & captureRespBody) != 0 {
			respBody = body
		}
		capture = &ReqRespCapture{
			ReqPath:     request.URL.Path,
			ReqHeaders:  reqHeaders,
			ReqBody:     reqBody,
			RespHeaders: respHeaders,
			RespBody:    respBody,
		}
	}

	metricID := mp.queueMetrics(tm)
	tm.ID = metricID

	// Store capture if enabled
	if capture != nil {
		capture.ID = metricID
		if mp.addCapture(*capture) {
			tm.HasCapture = true
			mp.mu.Lock()
			mp.metrics[len(mp.metrics)-1].HasCapture = true
			mp.mu.Unlock()
		}
	}

	mp.emitMetric(tm)

	// Async: tokenize reasoning content and update reasoning/output split
	if upstreamURL != "" {
		go mp.updateReasoningTokens(metricID, modelID, upstreamURL)
	}

	return nil
}

func processStreamingResponse(modelID, reqPath string, start time.Time, body []byte) (ActivityLogEntry, error) {
	// Iterate **backwards** through the body looking for the data payload with
	// usage data. This avoids allocating a slice of all lines via bytes.Split.

	// Start from the end of the body and scan backwards for newlines
	pos := len(body)
	for pos > 0 {
		// Find the previous newline (or start of body)
		lineStart := bytes.LastIndexByte(body[:pos], '\n')
		if lineStart == -1 {
			lineStart = 0
		} else {
			lineStart++ // Move past the newline
		}

		line := bytes.TrimSpace(body[lineStart:pos])
		pos = lineStart - 1 // Move position before the newline for next iteration

		if len(line) == 0 {
			continue
		}

		// SSE payload always follows "data:"
		prefix := []byte("data:")
		if !bytes.HasPrefix(line, prefix) {
			continue
		}
		data := bytes.TrimSpace(line[len(prefix):])

		if len(data) == 0 {
			continue
		}

		if bytes.Equal(data, []byte("[DONE]")) {
			// [DONE] line itself contains nothing of interest.
			continue
		}

		if gjson.ValidBytes(data) {
			parsed := gjson.ParseBytes(data)
			usage, timings, performance := findMetricsPayload(parsed, reqPath)

			if usage.Exists() || timings.Exists() || performance.Exists() {
				return parseMetrics(modelID, start, usage, timings, performance)
			}
		}
	}

	return ActivityLogEntry{}, fmt.Errorf("no valid JSON data found in stream")
}

func findMetricsPayload(parsed gjson.Result, reqPath string) (gjson.Result, gjson.Result, gjson.Result) {
	candidates := []gjson.Result{parsed}
	if data := parsed.Get("data"); data.Exists() {
		candidates = append(candidates, data)
	}
	if response := parsed.Get("response"); response.Exists() {
		candidates = append(candidates, response)
	}
	if response := parsed.Get("data.response"); response.Exists() {
		candidates = append(candidates, response)
	}

	var usage, timings, performance gjson.Result
	for _, candidate := range candidates {
		if !usage.Exists() {
			usage = candidate.Get("usage")
		}
		if !timings.Exists() {
			timings = candidate.Get("timings")
		}
		if !performance.Exists() {
			// vLLM exposes opt-in per-request timing data under "metrics".
			performance = candidate.Get("metrics")
		}

		// extract timings for infill - response is an array, timings are in the last element
		// see #463
		if strings.HasPrefix(reqPath, "/infill") {
			if arr := candidate.Array(); len(arr) > 0 {
				timings = arr[len(arr)-1].Get("timings")
			}
		}
	}

	return usage, timings, performance
}

func parseMetrics(modelID string, start time.Time, usage, timings, performance gjson.Result) (ActivityLogEntry, error) {
	wallDurationMs := int(time.Since(start).Milliseconds())

	// default values
	cachedTokens := -1 // unknown or missing data
	outputTokens := 0
	inputTokens := 0
	reasoningTokens := 0

	// timings data
	tokensPerSecond := -1.0
	promptPerSecond := -1.0
	durationMs := wallDurationMs

	if usage.Exists() {
		if pt := usage.Get("prompt_tokens"); pt.Exists() {
			// v1/chat/completions
			inputTokens = int(pt.Int())
		} else if it := usage.Get("input_tokens"); it.Exists() {
			// v1/messages
			inputTokens = int(it.Int())
		}

		if ct := usage.Get("completion_tokens"); ct.Exists() {
			// v1/chat/completions
			outputTokens = int(ct.Int())
		} else if ot := usage.Get("output_tokens"); ot.Exists() {
			outputTokens = int(ot.Int())
		}

		if ct := usage.Get("cache_read_input_tokens"); ct.Exists() {
			cachedTokens = int(ct.Int())
		} else if ct := usage.Get("prompt_tokens_details.cached_tokens"); ct.Exists() {
			// OpenAI chat/completions, including vLLM with
			// --enable-prompt-tokens-details.
			cachedTokens = int(ct.Int())
		} else if ct := usage.Get("input_tokens_details.cached_tokens"); ct.Exists() {
			// OpenAI Responses API.
			cachedTokens = int(ct.Int())
		}

		if rt := usage.Get("completion_tokens_details.reasoning_tokens"); rt.Exists() {
			reasoningTokens = int(rt.Int())
		} else if rt := usage.Get("output_tokens_details.reasoning_tokens"); rt.Exists() {
			reasoningTokens = int(rt.Int())
		}
	}

	// use llama-server's timing data for tok/sec and duration as it is more accurate
	if timings.Exists() {
		inputTokens = int(timings.Get("prompt_n").Int())
		outputTokens = int(timings.Get("predicted_n").Int())
		promptPerSecond = timings.Get("prompt_per_second").Float()
		tokensPerSecond = timings.Get("predicted_per_second").Float()
		timingsDurationMs := int(timings.Get("prompt_ms").Float() + timings.Get("predicted_ms").Float())
		if timingsDurationMs > durationMs {
			durationMs = timingsDurationMs
		}

		if cachedValue := timings.Get("cache_n"); cachedValue.Exists() {
			cachedTokens = int(cachedValue.Int())
		}
	}

	// vLLM can expose per-request performance data with
	// --enable-per-request-metrics. Prefer the decode-only interval for
	// generation speed so this remains comparable to llama.cpp timings.
	if performance.Exists() {
		if generationMs := performance.Get("generation_time_ms"); generationMs.Exists() &&
			generationMs.Float() > 0 && outputTokens > 1 {
			tokensPerSecond = float64(outputTokens-1) * 1000 / generationMs.Float()
		} else if tps := performance.Get("tokens_per_second"); tps.Exists() && tps.Float() > 0 {
			tokensPerSecond = tps.Float()
		}

		if ttftMs := performance.Get("time_to_first_token_ms"); ttftMs.Exists() && ttftMs.Float() > 0 {
			processedTokens := inputTokens
			if cachedTokens >= 0 && cachedTokens <= processedTokens {
				processedTokens -= cachedTokens
			}
			if processedTokens > 0 {
				// vLLM does not expose a prefill-only duration. TTFT excludes
				// queue time and is the closest per-request measurement.
				promptPerSecond = float64(processedTokens) * 1000 / ttftMs.Float()
			}
		}

		reportedDurationMs := performance.Get("queue_time_ms").Float() +
			performance.Get("time_to_first_token_ms").Float() +
			performance.Get("generation_time_ms").Float()
		if int(reportedDurationMs) > durationMs {
			durationMs = int(reportedDurationMs)
		}
	}

	if reasoningTokens > outputTokens {
		reasoningTokens = outputTokens
	}
	visibleOutputTokens := outputTokens - reasoningTokens

	return ActivityLogEntry{
		Timestamp: time.Now(),
		Model:     modelID,
		Tokens: TokenMetrics{
			CachedTokens:    cachedTokens,
			InputTokens:     inputTokens,
			GeneratedTokens: outputTokens,
			ReasoningTokens: reasoningTokens,
			OutputTokens:    visibleOutputTokens,
			PromptPerSecond: promptPerSecond,
			TokensPerSecond: tokensPerSecond,
		},
		DurationMs: durationMs,
	}, nil
}

func applyObservedStreamingSpeeds(metric *ActivityLogEntry, start, firstWrite, lastWrite time.Time) {
	if metric.Tokens.PromptPerSecond < 0 && firstWrite.After(start) {
		processedTokens := metric.Tokens.InputTokens
		if cachedTokens := metric.Tokens.CachedTokens; cachedTokens >= 0 && cachedTokens <= processedTokens {
			processedTokens -= cachedTokens
		}
		if processedTokens > 0 {
			metric.Tokens.PromptPerSecond = float64(processedTokens) / firstWrite.Sub(start).Seconds()
		}
	}

	if metric.Tokens.TokensPerSecond < 0 &&
		metric.Tokens.GeneratedTokens > 1 &&
		lastWrite.After(firstWrite) {
		metric.Tokens.TokensPerSecond = float64(metric.Tokens.GeneratedTokens-1) / lastWrite.Sub(firstWrite).Seconds()
	}
}

// extractReasoningContent extracts reasoning content from a JSON response body.
// Checks OpenAI, Anthropic, and native llama.cpp response formats.
func extractReasoningContent(body []byte) string {
	if !gjson.ValidBytes(body) {
		return ""
	}
	parsed := gjson.ParseBytes(body)

	// OpenAI format: choices[0].message.reasoning_content
	if rc := parsed.Get("choices.0.message.reasoning_content"); rc.Exists() {
		return rc.String()
	}
	if rc := parsed.Get("choices.0.message.reasoning"); rc.Exists() {
		return rc.String()
	}

	// Anthropic format: content[].text where type is thinking
	if contentArr := parsed.Get("content"); contentArr.Exists() {
		for _, item := range contentArr.Array() {
			if item.Get("type").String() == "thinking" {
				if text := item.Get("text"); text.Exists() {
					return text.String()
				}
			}
		}
	}

	// OpenAI Responses format: output items with reasoning_text content.
	if outputArr := parsed.Get("output"); outputArr.Exists() {
		var reasoningParts []string
		for _, item := range outputArr.Array() {
			if item.Get("type").String() != "reasoning" {
				continue
			}
			for _, content := range item.Get("content").Array() {
				if text := content.Get("text"); text.Exists() {
					reasoningParts = append(reasoningParts, text.String())
				}
			}
		}
		if len(reasoningParts) > 0 {
			return strings.Join(reasoningParts, "")
		}
	}

	// Native llama.cpp format: reasoning_content at root
	if rc := parsed.Get("reasoning_content"); rc.Exists() {
		return rc.String()
	}

	return ""
}

// extractStreamingReasoningContent iterates through SSE events and accumulates
// all reasoning_content fragments.
func extractStreamingReasoningContent(body []byte) string {
	var reasoningParts []string

	pos := len(body)
	for pos > 0 {
		lineStart := bytes.LastIndexByte(body[:pos], '\n')
		if lineStart == -1 {
			lineStart = 0
		} else {
			lineStart++
		}

		line := bytes.TrimSpace(body[lineStart:pos])
		pos = lineStart - 1

		if len(line) == 0 {
			continue
		}

		prefix := []byte("data:")
		if !bytes.HasPrefix(line, prefix) {
			continue
		}
		data := bytes.TrimSpace(line[len(prefix):])

		if len(data) == 0 || bytes.Equal(data, []byte("[DONE]")) {
			continue
		}

		if !gjson.ValidBytes(data) {
			continue
		}

		parsed := gjson.ParseBytes(data)

		// OpenAI streaming: choices[0].delta.reasoning_content
		if rc := parsed.Get("choices.0.delta.reasoning_content"); rc.Exists() && rc.String() != "" {
			reasoningParts = append(reasoningParts, rc.String())
		}
		if rc := parsed.Get("choices.0.delta.reasoning"); rc.Exists() && rc.String() != "" {
			reasoningParts = append(reasoningParts, rc.String())
		}

		// Anthropic streaming: delta.reasoning_content or delta.thinking
		if rc := parsed.Get("delta.reasoning_content"); rc.Exists() && rc.String() != "" {
			reasoningParts = append(reasoningParts, rc.String())
		}
		if rc := parsed.Get("delta.thinking"); rc.Exists() && rc.String() != "" {
			reasoningParts = append(reasoningParts, rc.String())
		}

		// OpenAI Responses streaming, both direct events and wrapped events.
		for _, candidate := range []gjson.Result{parsed, parsed.Get("data")} {
			if candidate.Get("type").String() == "response.reasoning_text.delta" {
				if delta := candidate.Get("delta"); delta.Exists() && delta.Type == gjson.String {
					reasoningParts = append(reasoningParts, delta.String())
				}
			}
		}
	}

	// Reverse parts since we iterated backwards through the stream
	for i, j := 0, len(reasoningParts)-1; i < j; i, j = i+1, j-1 {
		reasoningParts[i], reasoningParts[j] = reasoningParts[j], reasoningParts[i]
	}
	return strings.Join(reasoningParts, "")
}

// tokenizeReasoning calls the upstream /tokenize endpoint to count tokens in
// reasoning content. Returns the token count or -1 on error.
func tokenizeReasoning(upstreamURL, reasoningContent string) int {
	if upstreamURL == "" || reasoningContent == "" {
		return -1
	}

	contentJSON, err := json.Marshal(reasoningContent)
	if err != nil {
		return -1
	}
	requestBodies := []string{
		fmt.Sprintf(`{"content": %s}`, contentJSON),                             // llama.cpp
		fmt.Sprintf(`{"prompt": %s, "add_special_tokens": false}`, contentJSON), // vLLM
	}
	for _, reqBody := range requestBodies {
		resp, err := http.Post(upstreamURL+"/tokenize", "application/json", bytes.NewBufferString(reqBody))
		if err != nil {
			continue
		}
		body, readErr := io.ReadAll(resp.Body)
		resp.Body.Close()
		if readErr != nil || resp.StatusCode != http.StatusOK || !gjson.ValidBytes(body) {
			continue
		}

		parsed := gjson.ParseBytes(body)
		if tokens := parsed.Get("tokens"); tokens.Exists() && tokens.IsArray() {
			return len(tokens.Array())
		}
		if count := parsed.Get("count"); count.Exists() {
			return int(count.Int())
		}
	}

	return -1
}

// updateReasoningTokens extracts reasoning content from a capture, tokenizes it,
// and updates the metrics entry with the reasoning token count.
func (mp *metricsMonitor) updateReasoningTokens(entryID int, modelID string, upstreamURL string) {
	if mp.captureCache == nil {
		return
	}

	// Prefer an exact count supplied by the upstream usage object.
	mp.mu.RLock()
	for i := range mp.metrics {
		if mp.metrics[i].ID == entryID && mp.metrics[i].Tokens.ReasoningTokens > 0 {
			mp.mu.RUnlock()
			return
		}
	}
	mp.mu.RUnlock()

	capture := mp.getCaptureByID(entryID)
	if capture == nil || len(capture.RespBody) == 0 {
		return
	}

	var reasoningContent string
	isStreaming := strings.Contains(capture.RespHeaders["Content-Type"], "text/event-stream")
	if isStreaming {
		reasoningContent = extractStreamingReasoningContent(capture.RespBody)
	} else {
		reasoningContent = extractReasoningContent(capture.RespBody)
	}

	if reasoningContent == "" {
		return
	}

	reasoningTokens := tokenizeReasoning(upstreamURL, reasoningContent)
	if reasoningTokens < 0 {
		return
	}

	mp.mu.Lock()
	// Find the entry by ID
	var found bool
	for i := range mp.metrics {
		if mp.metrics[i].ID == entryID {
			generated := mp.metrics[i].Tokens.GeneratedTokens
			if reasoningTokens > generated {
				reasoningTokens = generated
			}
			mp.metrics[i].Tokens.ReasoningTokens = reasoningTokens
			mp.metrics[i].Tokens.OutputTokens = generated - reasoningTokens
			found = true
			break
		}
	}
	mp.mu.Unlock()

	if !found {
		return
	}

	// Read back the updated entry and emit
	mp.mu.RLock()
	for i := range mp.metrics {
		if mp.metrics[i].ID == entryID {
			mp.emitMetric(mp.metrics[i])
			break
		}
	}
	mp.mu.RUnlock()
}

// decompressBody decompresses the body based on Content-Encoding header
func decompressBody(body []byte, encoding string) ([]byte, error) {
	switch strings.ToLower(strings.TrimSpace(encoding)) {
	case "gzip":
		reader, err := gzip.NewReader(bytes.NewReader(body))
		if err != nil {
			return nil, err
		}
		defer reader.Close()
		return io.ReadAll(reader)
	case "deflate":
		reader := flate.NewReader(bytes.NewReader(body))
		defer reader.Close()
		return io.ReadAll(reader)
	default:
		return body, nil // Return as-is for unknown/no encoding
	}
}

// responseBodyCopier records the response body and writes to the original response writer
// while also capturing it in a buffer for later processing
type responseBodyCopier struct {
	gin.ResponseWriter
	body       *bytes.Buffer
	tee        io.Writer
	start      time.Time
	firstWrite time.Time
	lastWrite  time.Time
}

func newBodyCopier(w gin.ResponseWriter) *responseBodyCopier {
	bodyBuffer := &bytes.Buffer{}
	return &responseBodyCopier{
		ResponseWriter: w,
		body:           bodyBuffer,
		tee:            io.MultiWriter(w, bodyBuffer),
		start:          time.Now(),
	}
}

func (w *responseBodyCopier) Write(b []byte) (int, error) {
	now := time.Now()
	if w.firstWrite.IsZero() {
		w.firstWrite = now
	}
	w.lastWrite = now

	// Single write operation that writes to both the response and buffer
	return w.tee.Write(b)
}

func (w *responseBodyCopier) WriteHeader(statusCode int) {
	w.ResponseWriter.WriteHeader(statusCode)
}

func (w *responseBodyCopier) Header() http.Header {
	return w.ResponseWriter.Header()
}

func (w *responseBodyCopier) StartTime() time.Time {
	return w.start
}

func (w *responseBodyCopier) FirstWriteTime() time.Time {
	return w.firstWrite
}

func (w *responseBodyCopier) LastWriteTime() time.Time {
	return w.lastWrite
}

// sensitiveHeaders lists headers that should be redacted in captures
var sensitiveHeaders = map[string]bool{
	"authorization":       true,
	"proxy-authorization": true,
	"cookie":              true,
	"set-cookie":          true,
	"x-api-key":           true,
}

// redactHeaders replaces sensitive header values in-place with "[REDACTED]"
func redactHeaders(headers map[string]string) {
	for key := range headers {
		if sensitiveHeaders[strings.ToLower(key)] {
			headers[key] = "[REDACTED]"
		}
	}
}

// filterAcceptEncoding filters the Accept-Encoding header to only include
// encodings we can decompress (gzip, deflate). This respects the client's
// preferences while ensuring we can parse response bodies for metrics.
func filterAcceptEncoding(acceptEncoding string) string {
	if acceptEncoding == "" {
		return ""
	}

	supported := map[string]bool{"gzip": true, "deflate": true}
	var filtered []string

	for part := range strings.SplitSeq(acceptEncoding, ",") {
		// Parse encoding and optional quality value (e.g., "gzip;q=1.0")
		encoding, _, _ := strings.Cut(strings.TrimSpace(part), ";")
		if supported[strings.ToLower(encoding)] {
			filtered = append(filtered, strings.TrimSpace(part))
		}
	}

	return strings.Join(filtered, ", ")
}
