package server

import (
	"bufio"
	"bytes"
	"compress/flate"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/mostlygeek/llama-swap/internal/cache"
	"github.com/mostlygeek/llama-swap/internal/event"
	"github.com/mostlygeek/llama-swap/internal/logmon"
	"github.com/mostlygeek/llama-swap/internal/store"
	"github.com/mostlygeek/llama-swap/internal/swaputil"
	"github.com/tidwall/gjson"
)

type TokenMetrics = store.TokenMetrics
type ActivityLogEntry = store.ActivityLogEntry

const (
	maxConcurrentReasoningUpdates = 4
	maxPendingReasoningUpdates    = 64
)

type reasoningUpdate struct {
	entryID   int
	target    reasoningTokenizerTarget
	reasoning string
}

// ActivityLogEvent carries a single activity log entry to event subscribers.
type ActivityLogEvent struct {
	Metrics ActivityLogEntry
}

func (e ActivityLogEvent) Type() uint32 {
	return swaputil.ActivityLogEventID
}

// metricsMonitor parses upstream responses for token statistics, stores
// activity in a store, and (when captures are enabled) stores
// zstd+CBOR-compressed request/response captures in a sized in-memory cache.
type metricsMonitor struct {
	store          *store.Store
	maxMetrics     int
	logger         *logmon.Monitor
	enableCaptures bool
	captureCache   *cache.Cache // zstd-compressed CBOR of ReqRespCapture
	httpClient     *http.Client

	asyncCtx     context.Context
	asyncCancel  context.CancelFunc
	asyncMu      sync.Mutex
	asyncClosed  bool
	asyncStarted bool
	asyncWG      sync.WaitGroup
	asyncJobs    chan reasoningUpdate
	asyncDone    chan struct{}
}

func newMetricsMonitor(logger *logmon.Monitor, maxMetrics int, captureBufferMB int, st *store.Store) *metricsMonitor {
	if maxMetrics <= 0 {
		maxMetrics = 1000
	}
	asyncCtx, asyncCancel := context.WithCancel(context.Background())
	mm := &metricsMonitor{
		logger:         logger,
		store:          st,
		maxMetrics:     maxMetrics,
		enableCaptures: captureBufferMB > 0,
		httpClient:     newReasoningHTTPClient(),
		asyncCtx:       asyncCtx,
		asyncCancel:    asyncCancel,
		asyncJobs:      make(chan reasoningUpdate, maxPendingReasoningUpdates),
		asyncDone:      make(chan struct{}),
	}
	if captureBufferMB > 0 {
		mm.captureCache = cache.New(captureBufferMB * 1024 * 1024)
	}
	return mm
}

// queueMetrics persists a metric and returns the store-assigned row. It
// deliberately does not take the request context: record runs after the
// handler returns, and an aborted request (canceled context) must still be
// recorded — that is exactly when the error entry matters.
func (mp *metricsMonitor) queueMetrics(metric ActivityLogEntry) (ActivityLogEntry, bool) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	stored, err := mp.store.InsertActivity(ctx, metric)
	if err != nil {
		mp.warnf("failed to persist activity metric: %v", err)
		return ActivityLogEntry{}, false
	}
	if mp.store.IsInMemory() {
		if err := mp.store.PruneActivity(ctx, mp.maxMetrics); err != nil {
			mp.warnf("failed to prune activity metrics: %v", err)
		}
	}
	return stored, true
}

// emitMetric publishes an ActivityLogEvent for the given metric.
func (mp *metricsMonitor) emitMetric(metric ActivityLogEntry) {
	event.Emit(ActivityLogEvent{Metrics: metric})
}

func (mp *metricsMonitor) overlayCaptureState(entries []ActivityLogEntry) {
	if mp.captureCache == nil {
		for i := range entries {
			entries[i].HasCapture = false
		}
		return
	}
	for i := range entries {
		entries[i].HasCapture = mp.captureCache.Has(entries[i].ID)
	}
}

func (mp *metricsMonitor) warnf(format string, args ...any) {
	if mp.logger != nil {
		mp.logger.Warnf(format, args...)
	}
}

func (mp *metricsMonitor) Close() error {
	mp.asyncMu.Lock()
	if mp.asyncClosed {
		done := mp.asyncDone
		mp.asyncMu.Unlock()
		<-done
		return nil
	}
	mp.asyncClosed = true
	mp.asyncCancel()
	mp.asyncMu.Unlock()
	mp.asyncWG.Wait()
	mp.httpClient.CloseIdleConnections()
	close(mp.asyncDone)
	return nil
}

func (mp *metricsMonitor) queueReasoningUpdate(entryID int, target reasoningTokenizerTarget, reasoning string) {
	mp.asyncMu.Lock()
	if mp.asyncClosed {
		mp.asyncMu.Unlock()
		return
	}
	if !mp.asyncStarted {
		mp.asyncStarted = true
		mp.asyncWG.Add(maxConcurrentReasoningUpdates)
		for range maxConcurrentReasoningUpdates {
			go mp.reasoningUpdateWorker()
		}
	}
	mp.asyncMu.Unlock()

	job := reasoningUpdate{entryID: entryID, target: target, reasoning: reasoning}
	select {
	case mp.asyncJobs <- job:
	case <-mp.asyncCtx.Done():
	}
}

func (mp *metricsMonitor) reasoningUpdateWorker() {
	defer mp.asyncWG.Done()
	for {
		select {
		case job := <-mp.asyncJobs:
			mp.updateReasoningTokens(job.entryID, job.target, job.reasoning)
		case <-mp.asyncCtx.Done():
			return
		}
	}
}

func (mp *metricsMonitor) updateReasoningTokens(entryID int, target reasoningTokenizerTarget, reasoning string) {
	ctx, cancel := context.WithTimeout(mp.asyncCtx, reasoningTokenizerTimeout)
	reasoningTokens, ok := tokenizeReasoning(ctx, mp.httpClient, target, reasoning)
	cancel()
	if !ok {
		return
	}

	ctx, cancel = context.WithTimeout(mp.asyncCtx, 5*time.Second)
	updated, found, err := mp.store.UpdateActivityReasoningTokens(ctx, entryID, reasoningTokens)
	cancel()
	if err != nil {
		mp.warnf("failed to persist reasoning token update for activity %d: %v", entryID, err)
		return
	}
	if !found {
		return
	}
	entries := []ActivityLogEntry{updated}
	mp.overlayCaptureState(entries)
	mp.emitMetric(entries[0])
}

// record parses a completed response body and stores/emits an activity entry.
// Successful requests store a zstd+CBOR capture (when enabled) with cf
// controlling which parts are retained. Failed (non-200) requests capture the
// request only and set ErrorMsg to a description of the failure, so the error
// can be inspected without storing unreadable raw response bytes. reqBody and
// reqHeaders are the request data buffered before dispatch.
func (mp *metricsMonitor) record(modelID string, r *http.Request, recorder *responseBodyCopier, cf captureFields, reqBody []byte, reqHeaders map[string]string, tokenizerTarget reasoningTokenizerTarget) {
	tm := ActivityLogEntry{
		Timestamp:       time.Now(),
		Model:           modelID,
		ReqPath:         r.URL.Path,
		RespContentType: recorder.Header().Get("Content-Type"),
		RespStatusCode:  recorder.Status(),
		DurationMs:      int(time.Since(recorder.StartTime()).Milliseconds()),
	}

	if ctxData, ok := swaputil.ReadContext(r.Context()); ok && len(ctxData.Metadata) > 0 {
		tm.Metadata = make(map[string]string, len(ctxData.Metadata))
		for k, v := range ctxData.Metadata {
			tm.Metadata[k] = v
		}
	}
	if selectorID := selectorFromContext(r.Context()); selectorID != "" {
		if tm.Metadata == nil {
			tm.Metadata = make(map[string]string)
		}
		tm.Metadata["selector"] = selectorID
	}

	queueAndEmit := func() {
		stored, ok := mp.queueMetrics(tm)
		if !ok {
			return
		}
		tm = stored
		mp.emitMetric(tm)
	}

	if recorder.Status() != http.StatusOK {
		mp.logger.Warnf("non-200 response, recording partial metrics: status=%d, path=%s", recorder.Status(), r.URL.Path)
		decoded, decErr := mp.decodeResponseBody(recorder, r.URL.Path)
		tm.ErrorMsg = failedErrorMessage(recorder.Status(), decoded, decErr)
		stored, ok := mp.queueMetrics(tm)
		if !ok {
			return
		}
		tm = stored
		// Capture the request only; the failure is surfaced via ErrorMsg
		// rather than storing the (possibly undisplayable) response body.
		tm.HasCapture = mp.storeCapture(tm.ID, r, recorder, cf&^captureRespBody, reqBody, reqHeaders, nil)
		mp.emitMetric(tm)
		return
	}

	body := recorder.body.Bytes()
	if len(body) == 0 {
		mp.logger.Warn("metrics: empty body, recording minimal metrics")
		queueAndEmit()
		return
	}

	if encoding := recorder.Header().Get("Content-Encoding"); encoding != "" {
		decoded, err := decompressBody(body, encoding)
		if err != nil {
			mp.logger.Warnf("metrics: decompression failed: %v, path=%s, recording minimal metrics", err, r.URL.Path)
			tm.ErrorMsg = fmt.Sprintf("response decompression failed: %v", err)
			queueAndEmit()
			return
		}
		body = decoded
	}

	isStreaming := strings.Contains(recorder.Header().Get("Content-Type"), "text/event-stream")
	reasoningTokensReported := false
	if isStreaming {
		if parsed, err := processStreamingResponse(modelID, recorder.StartTime(), body); err != nil {
			mp.logger.Warnf("error processing streaming response: %v, path=%s, recording minimal metrics", err, r.URL.Path)
		} else {
			tm.Tokens = parsed.Tokens
			tm.DurationMs = parsed.DurationMs
			reasoningTokensReported = parsed.reasoningTokensReported
			applyObservedStreamingSpeeds(&tm, recorder.StartTime(), recorder.FirstWriteTime(), recorder.LastWriteTime())
		}
	} else if gjson.ValidBytes(body) {
		parsed := gjson.ParseBytes(body)
		usage, timings, responseMetrics := findMetricsPayload(parsed, r.URL.Path)

		if usage.Exists() || timings.Exists() || responseMetrics.Exists() {
			if parsedMetrics, err := parseMetrics(modelID, recorder.StartTime(), usage, timings, responseMetrics); err != nil {
				mp.logger.Warnf("error parsing metrics: %v, path=%s, recording minimal metrics", err, r.URL.Path)
			} else {
				tm.Tokens = parsedMetrics.Tokens
				tm.DurationMs = parsedMetrics.DurationMs
				reasoningTokensReported = parsedMetrics.reasoningTokensReported
			}
		}
	} else {
		mp.logger.Warnf("metrics: invalid JSON in response body path=%s, recording minimal metrics", r.URL.Path)
	}

	stored, ok := mp.queueMetrics(tm)
	if !ok {
		return
	}
	tm = stored
	tm.HasCapture = mp.storeCapture(tm.ID, r, recorder, cf, reqBody, reqHeaders, body)
	mp.emitMetric(tm)

	if reasoningTokensReported || tm.Tokens.GeneratedTokens <= 0 || tokenizerTarget.URL == "" {
		return
	}
	var reasoning string
	if isStreaming {
		reasoning = extractStreamingReasoningContent(body)
	} else {
		reasoning = extractReasoningContent(body)
	}
	if reasoning != "" {
		mp.queueReasoningUpdate(tm.ID, tokenizerTarget, reasoning)
	}
}

// storeCapture assembles a ReqRespCapture for id, honoring the captureFields
// mask, and stores it when captures are enabled. body is the response body to
// capture (already decompressed by the caller); pass nil to omit it. Returns
// true if a capture was stored.
func (mp *metricsMonitor) storeCapture(id int, r *http.Request, recorder *responseBodyCopier, cf captureFields, reqBody []byte, reqHeaders map[string]string, body []byte) bool {
	if !mp.enableCaptures {
		return false
	}
	capture := ReqRespCapture{
		ID:         id,
		ReqPath:    r.URL.Path,
		ReqHeaders: reqHeaders,
	}
	if cf&captureReqBody != 0 {
		capture.ReqBody = reqBody
	}
	if cf&captureRespHeaders != 0 {
		capture.RespHeaders = headerMap(recorder.Header())
		redactHeaders(capture.RespHeaders)
		delete(capture.RespHeaders, "Content-Encoding")
	}
	if cf&captureRespBody != 0 {
		capture.RespBody = body
	}
	return mp.addCapture(capture)
}

// decodeResponseBody returns the buffered response body, decompressing it when
// the upstream set a Content-Encoding we recognize. On decompression failure it
// logs a warning and returns an error so the caller can record a description
// (via ErrorMsg) instead of storing unreadable raw bytes.
func (mp *metricsMonitor) decodeResponseBody(recorder *responseBodyCopier, path string) ([]byte, error) {
	body := recorder.body.Bytes()
	if len(body) == 0 {
		return nil, nil
	}
	encoding := recorder.Header().Get("Content-Encoding")
	if encoding == "" {
		return body, nil
	}
	decoded, err := decompressBody(body, encoding)
	if err != nil {
		mp.logger.Warnf("metrics: response decompression failed: %v, path=%s", err, path)
		return nil, err
	}
	return decoded, nil
}

// errorMessagePaths lists JSON paths where a human-readable error message can
// live across OpenAI- and llama.cpp-style error responses.
var errorMessagePaths = []string{"error.message", "error", "message", "detail"}

// extractErrorMessage pulls a human-readable error string from a JSON error
// response. Returns "" if no message is found or the body is not valid JSON.
func extractErrorMessage(body []byte) string {
	if !gjson.ValidBytes(body) {
		return ""
	}
	parsed := gjson.ParseBytes(body)
	for _, path := range errorMessagePaths {
		v := parsed.Get(path)
		if v.Exists() && v.Type == gjson.String {
			if s := strings.TrimSpace(v.String()); s != "" {
				return s
			}
		}
	}
	return ""
}

// failedErrorMessage builds a human-readable description for a non-200 response.
// It prefers an error message parsed from the (decompressed) body and falls back
// to the HTTP status text. A non-nil decErr indicates the body could not be
// decoded, in which case the decode error is described instead.
func failedErrorMessage(status int, body []byte, decErr error) string {
	const maxLen = 500
	if decErr != nil {
		return fmt.Sprintf("response decode failed: %v", decErr)
	}
	if msg := extractErrorMessage(body); msg != "" {
		if len(msg) > maxLen {
			msg = msg[:maxLen] + "..."
		}
		return msg
	}
	if text := http.StatusText(status); text != "" {
		return fmt.Sprintf("%d %s", status, text)
	}
	return fmt.Sprintf("HTTP %d", status)
}

// metricsContainerPaths lists envelopes used by OpenAI-compatible backends.
// Some wrappers put the event payload under data, while Responses completion
// events then add a response envelope inside it.
var metricsContainerPaths = []string{"", "data", "response", "data.response", "message", "data.message"}

func findMetricsPayload(parsed gjson.Result, reqPath string) (usage, timings, responseMetrics gjson.Result) {
	for _, path := range metricsContainerPaths {
		candidate := parsed
		if path != "" {
			candidate = parsed.Get(path)
		}
		if !candidate.Exists() {
			continue
		}

		if !usage.Exists() {
			usage = candidate.Get("usage")
		}
		if !timings.Exists() {
			timings = candidate.Get("timings")
		}
		if !responseMetrics.Exists() {
			responseMetrics = candidate.Get("metrics")
		}

		// /infill responses are arrays; timings live in the last element (#463).
		if !timings.Exists() && strings.HasPrefix(reqPath, "/infill") {
			if arr := candidate.Array(); len(arr) > 0 {
				timings = arr[len(arr)-1].Get("timings")
			}
		}
	}
	return
}

type usageTokenCounts struct {
	input, generated, cached, reasoning              int64
	inputReported, generatedReported, cachedReported bool
	reasoningReported                                bool
}

func (u usageTokenCounts) anyReported() bool {
	return u.inputReported || u.generatedReported || u.cachedReported || u.reasoningReported
}

func firstNumericField(value gjson.Result, paths ...string) (int64, bool) {
	for _, path := range paths {
		if field := value.Get(path); field.Type == gjson.Number {
			return field.Int(), true
		}
	}
	return 0, false
}

// extractUsageTokens reads token counts from a usage object, handling field-name
// differences across endpoints. Per-field reported flags distinguish an
// authoritative numeric zero from a missing or null value.
func extractUsageTokens(usage gjson.Result) usageTokenCounts {
	if !usage.Exists() {
		return usageTokenCounts{}
	}
	input, inputReported := firstNumericField(usage, "prompt_tokens", "input_tokens")
	generated, generatedReported := firstNumericField(usage, "completion_tokens", "output_tokens")
	cached, cachedReported := firstNumericField(
		usage,
		"cache_read_input_tokens",
		"input_tokens_details.cached_tokens",
		"prompt_tokens_details.cached_tokens",
	)
	reasoning, reasoningReported := firstNumericField(
		usage,
		"completion_tokens_details.reasoning_tokens",
		"output_tokens_details.reasoning_tokens",
	)
	return usageTokenCounts{
		input:             input,
		generated:         generated,
		cached:            cached,
		reasoning:         reasoning,
		inputReported:     inputReported,
		generatedReported: generatedReported,
		cachedReported:    cachedReported,
		reasoningReported: reasoningReported,
	}
}

type parsedActivity struct {
	ActivityLogEntry
	reasoningTokensReported bool
}

func processStreamingResponse(modelID string, start time.Time, body []byte) (parsedActivity, error) {
	var (
		inputTokens, generatedTokens, reasoningTokens int64
		cachedTokens                                  int64 = -1
		reasoningTokensReported                       bool
		hasAny                                        bool
		timings                                       gjson.Result
		responseMetrics                               gjson.Result
	)

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
		if len(line) == 0 || !bytes.HasPrefix(line, prefix) {
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

		u, t, m := findMetricsPayload(parsed, "")
		if counts := extractUsageTokens(u); counts.anyReported() {
			hasAny = true
			if counts.inputReported {
				inputTokens = counts.input
			}
			if counts.generatedReported {
				generatedTokens = counts.generated
			}
			if counts.cachedReported {
				cachedTokens = counts.cached
			}
			if counts.reasoningReported {
				reasoningTokens = counts.reasoning
				reasoningTokensReported = true
			}
		}
		if t.Exists() {
			timings = t
			hasAny = true
		}
		if m.Exists() {
			responseMetrics = m
			hasAny = true
		}
	}

	if !hasAny {
		return parsedActivity{}, fmt.Errorf("no valid JSON data found in stream")
	}

	return buildMetrics(modelID, start, inputTokens, generatedTokens, cachedTokens, reasoningTokens, reasoningTokensReported, timings, responseMetrics), nil
}

func parseMetrics(modelID string, start time.Time, usage, timings, responseMetrics gjson.Result) (parsedActivity, error) {
	counts := extractUsageTokens(usage)
	cached := int64(-1)
	if counts.cachedReported {
		cached = counts.cached
	}
	return buildMetrics(
		modelID,
		start,
		counts.input,
		counts.generated,
		cached,
		counts.reasoning,
		counts.reasoningReported,
		timings,
		responseMetrics,
	), nil
}

// buildMetrics composes an ActivityLogEntry from accumulated token counts and
// optional llama-server timings (which override input/output and provide rates)
// or vLLM response metrics.
func buildMetrics(
	modelID string,
	start time.Time,
	inputTokens, generatedTokens, cachedTokens, reasoningTokens int64,
	reasoningTokensReported bool,
	timings, responseMetrics gjson.Result,
) parsedActivity {
	wallDurationMs := int(time.Since(start).Milliseconds())
	durationMs := wallDurationMs
	tokensPerSecond := -1.0
	promptPerSecond := -1.0
	draftTokens := -1
	draftAccTokens := -1

	if timings.Exists() {
		inputTokens = timings.Get("prompt_n").Int()
		generatedTokens = timings.Get("predicted_n").Int()
		promptPerSecond = timings.Get("prompt_per_second").Float()
		tokensPerSecond = timings.Get("predicted_per_second").Float()
		timingsDurationMs := int(timings.Get("prompt_ms").Float() + timings.Get("predicted_ms").Float())
		if timingsDurationMs > durationMs {
			durationMs = timingsDurationMs
		}
		if cachedValue := timings.Get("cache_n"); cachedValue.Exists() {
			cachedTokens = cachedValue.Int()
		}
		if timings.Get("draft_n").Exists() && timings.Get("draft_n_accepted").Exists() {
			draftTokens = int(timings.Get("draft_n").Int())
			draftAccTokens = int(timings.Get("draft_n_accepted").Int())
		}
	}
	if timeToFirstToken := responseMetrics.Get("time_to_first_token_ms"); timeToFirstToken.Exists() && timeToFirstToken.Float() > 0 {
		cachedForRate := cachedTokens
		if cachedForRate < 0 || cachedForRate > inputTokens {
			cachedForRate = 0
		}
		promptPerSecond = float64(inputTokens-cachedForRate) / (timeToFirstToken.Float() / 1000)
	}
	if meanInterTokenLatency := responseMetrics.Get("mean_itl_ms"); meanInterTokenLatency.Exists() && meanInterTokenLatency.Float() > 0 {
		tokensPerSecond = 1000 / meanInterTokenLatency.Float()
	} else if generationTime := responseMetrics.Get("generation_time_ms"); generationTime.Exists() && generationTime.Float() > 0 && generatedTokens > 1 {
		tokensPerSecond = float64(generatedTokens-1) * 1000 / generationTime.Float()
	} else if reportedSpeed := responseMetrics.Get("tokens_per_second"); reportedSpeed.Exists() && reportedSpeed.Float() > 0 {
		tokensPerSecond = reportedSpeed.Float()
	}

	generatedTokens, reasoningTokens, outputTokens := splitGeneratedTokens(generatedTokens, reasoningTokens)
	return parsedActivity{
		ActivityLogEntry: ActivityLogEntry{
			Timestamp: time.Now(),
			Model:     modelID,
			Tokens: TokenMetrics{
				CachedTokens:    int(cachedTokens),
				DraftTokens:     draftTokens,
				DraftAccTokens:  draftAccTokens,
				InputTokens:     int(inputTokens),
				GeneratedTokens: int(generatedTokens),
				ReasoningTokens: int(reasoningTokens),
				OutputTokens:    int(outputTokens),
				PromptPerSecond: promptPerSecond,
				TokensPerSecond: tokensPerSecond,
			},
			DurationMs: durationMs,
		},
		reasoningTokensReported: reasoningTokensReported,
	}
}

func splitGeneratedTokens(generatedTokens, reasoningTokens int64) (generated, reasoning, output int64) {
	generated = max(generatedTokens, 0)
	reasoning = min(max(reasoningTokens, 0), generated)
	return generated, reasoning, generated - reasoning
}

func applyObservedStreamingSpeeds(metric *ActivityLogEntry, start, firstWrite, lastWrite time.Time) {
	if metric.Tokens.PromptPerSecond < 0 && firstWrite.After(start) {
		processedTokens := metric.Tokens.InputTokens
		if cached := metric.Tokens.CachedTokens; cached >= 0 && cached <= processedTokens {
			processedTokens -= cached
		}
		if processedTokens > 0 {
			metric.Tokens.PromptPerSecond = float64(processedTokens) / firstWrite.Sub(start).Seconds()
		}
	}
	if metric.Tokens.TokensPerSecond < 0 && metric.Tokens.GeneratedTokens > 1 && lastWrite.After(firstWrite) {
		metric.Tokens.TokensPerSecond = float64(metric.Tokens.GeneratedTokens-1) / lastWrite.Sub(firstWrite).Seconds()
	}
}

// decompressBody decompresses the body based on the Content-Encoding header.
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
		return body, nil
	}
}

// filterAcceptEncoding filters Accept-Encoding to only gzip/deflate so response
// bodies remain decompressible for metrics parsing.
func filterAcceptEncoding(acceptEncoding string) string {
	if acceptEncoding == "" {
		return ""
	}

	supported := map[string]bool{"gzip": true, "deflate": true}
	var filtered []string
	for part := range strings.SplitSeq(acceptEncoding, ",") {
		encoding, _, _ := strings.Cut(strings.TrimSpace(part), ";")
		if supported[strings.ToLower(encoding)] {
			filtered = append(filtered, strings.TrimSpace(part))
		}
	}
	return strings.Join(filtered, ", ")
}

// responseBodyCopier tees the upstream response to the client while buffering
// it for metrics parsing. Status defaults to 200 until WriteHeader is called.
type responseBodyCopier struct {
	http.ResponseWriter
	body        *bytes.Buffer
	tee         io.Writer
	status      int
	wroteHeader bool
	start       time.Time
	firstWrite  time.Time
	lastWrite   time.Time
}

func newBodyCopier(w http.ResponseWriter) *responseBodyCopier {
	buf := &bytes.Buffer{}
	return &responseBodyCopier{
		ResponseWriter: w,
		body:           buf,
		tee:            io.MultiWriter(w, buf),
		status:         http.StatusOK,
		start:          time.Now(),
	}
}

func (w *responseBodyCopier) Write(b []byte) (int, error) {
	if !w.wroteHeader {
		w.WriteHeader(http.StatusOK)
	}
	// On a protocol upgrade (e.g. websocket) the body is raw framed data, not a
	// metrics-parseable response, so write straight to the client without
	// buffering a copy we can't use.
	if w.status == http.StatusSwitchingProtocols {
		return w.ResponseWriter.Write(b)
	}
	now := time.Now()
	if w.firstWrite.IsZero() {
		w.firstWrite = now
	}
	w.lastWrite = now
	return w.tee.Write(b)
}

func (w *responseBodyCopier) WriteHeader(statusCode int) {
	if w.wroteHeader {
		return
	}
	w.wroteHeader = true
	w.status = statusCode
	w.ResponseWriter.WriteHeader(statusCode)
}

// Flush forwards to the underlying writer so streaming responses still flush.
func (w *responseBodyCopier) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// Hijack forwards to the underlying writer so httputil.ReverseProxy can take
// over the connection for websocket upgrades.
func (w *responseBodyCopier) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	if hj, ok := w.ResponseWriter.(http.Hijacker); ok {
		return hj.Hijack()
	}
	return nil, nil, fmt.Errorf("underlying ResponseWriter does not support hijacking")
}

func (w *responseBodyCopier) Status() int               { return w.status }
func (w *responseBodyCopier) StartTime() time.Time      { return w.start }
func (w *responseBodyCopier) FirstWriteTime() time.Time { return w.firstWrite }
func (w *responseBodyCopier) LastWriteTime() time.Time  { return w.lastWrite }
