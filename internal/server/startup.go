package server

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sync"
	"time"

	"github.com/mostlygeek/llama-swap/internal/config"
	"github.com/mostlygeek/llama-swap/internal/logmon"
	"github.com/mostlygeek/llama-swap/internal/process"
	"github.com/mostlygeek/llama-swap/internal/store"
	"github.com/mostlygeek/llama-swap/internal/swaputil"
)

const startupSampleCount = 3

// startupTimings retains successful load durations. Its own event subscription
// serializes records and cache writes; status readers only take the memory lock.
type startupTimings struct {
	mu      sync.RWMutex
	keys    map[string]string
	samples map[string][]int64
	cache   store.CacheRepository
	logger  *logmon.Monitor
}

func startupCacheKey(id string, mc config.ModelConfig) string {
	// Resolved launch settings distinguish changes to weights, context size,
	// environment, or readiness endpoint without storing credentials in keys.
	launch, _ := json.Marshal(struct {
		Cmd, Proxy, CheckEndpoint string
		Env                       []string
	}{mc.Cmd, mc.Proxy, mc.CheckEndpoint, mc.Env})
	sum := sha256.Sum256(launch)
	return "startup:v1:" + id + ":" + hex.EncodeToString(sum[:16])
}

func newStartupTimings(ctx context.Context, models map[string]config.ModelConfig, cache store.CacheRepository, logger *logmon.Monitor) *startupTimings {
	t := &startupTimings{
		keys:    make(map[string]string),
		samples: make(map[string][]int64),
		cache:   cache,
		logger:  logger,
	}
	for id, mc := range models {
		key := startupCacheKey(id, mc)
		t.keys[id] = key
		if cache == nil {
			continue
		}
		readCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		entry, found, err := cache.Get(readCtx, key)
		cancel()
		if err != nil {
			t.warn("reading", id, err)
			continue
		}
		if !found {
			continue
		}
		var samples []int64
		if json.Unmarshal(entry.Data, &samples) != nil || len(samples) > startupSampleCount {
			continue
		}
		valid := true
		for _, ms := range samples {
			if ms <= 0 || ms > time.Duration(1<<63-1).Milliseconds() {
				valid = false
			}
		}
		if valid {
			t.samples[id] = samples
		}
	}
	return t
}

func (t *startupTimings) estimate(id string) int64 {
	if t == nil {
		return 0
	}
	t.mu.RLock()
	defer t.mu.RUnlock()
	samples := t.samples[id]
	if len(samples) == 0 {
		return 0
	}
	var total int64
	for _, ms := range samples {
		total += ms
	}
	return max(1, total/int64(len(samples)))
}

func (t *startupTimings) record(ctx context.Context, e swaputil.ProcessStateChangeEvent) {
	key, known := t.keys[e.ProcessName]
	if !known || e.OldState != string(process.StateStarting) ||
		e.NewState != string(process.StateReady) || e.StartupDuration <= 0 {
		return
	}
	t.mu.Lock()
	samples := append(t.samples[e.ProcessName], max(1, e.StartupDuration.Milliseconds()))
	if len(samples) > startupSampleCount {
		samples = samples[len(samples)-startupSampleCount:]
	}
	t.samples[e.ProcessName] = samples
	data, _ := json.Marshal(samples)
	t.mu.Unlock()
	if t.cache != nil {
		writeCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		err := t.cache.Set(writeCtx, store.CacheEntry{Key: key, Data: data})
		cancel()
		if err != nil {
			t.warn("saving", e.ProcessName, err)
		}
	}
}

func (t *startupTimings) warn(action, id string, err error) {
	if t.logger != nil {
		t.logger.Warnf("startup timing: %s <%s>: %v", action, id, err)
	}
}
