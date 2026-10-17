package server

import (
	"context"
	"testing"
	"time"

	"github.com/mostlygeek/llama-swap/internal/config"
	"github.com/mostlygeek/llama-swap/internal/process"
	"github.com/mostlygeek/llama-swap/internal/store"
	"github.com/mostlygeek/llama-swap/internal/store/sqlite"
	"github.com/mostlygeek/llama-swap/internal/swaputil"
	"github.com/stretchr/testify/require"
)

func TestServer_StartupTimings(t *testing.T) {
	ctx := context.Background()
	st, err := sqlite.New("")
	require.NoError(t, err)
	t.Cleanup(func() { st.Close() })
	models := map[string]config.ModelConfig{"m": {Cmd: "model --ctx 100", Env: []string{"WORKERS=2"}}}
	history := newStartupTimings(ctx, models, st.Cache(), nil)
	require.Zero(t, history.estimate("m"))
	ready := swaputil.ProcessStateChangeEvent{
		ProcessName: "m", OldState: "starting", NewState: "ready",
	}
	for _, seconds := range []int{10, 20, 30, 100} {
		ready.StartupDuration = time.Duration(seconds) * time.Second
		history.record(ctx, ready)
	}
	require.EqualValues(t, 50_000, history.estimate("m")) // last 3: 20, 30, 100
	for _, state := range []string{"stopped", "stopping", "shutdown"} {
		failed := ready
		failed.NewState = state
		failed.StartupDuration = time.Hour
		history.record(ctx, failed)
	}
	ready.StartupDuration = 0
	history.record(ctx, ready)
	ready.ProcessName = "other"
	ready.StartupDuration = time.Hour
	history.record(ctx, ready)
	require.EqualValues(t, 50_000, history.estimate("m"))
	require.Zero(t, history.estimate("other"))

	reloaded := newStartupTimings(ctx, models, st.Cache(), nil)
	require.EqualValues(t, 50_000, reloaded.estimate("m"))
	changed := map[string]config.ModelConfig{"m": {Cmd: "model --ctx 200", Env: []string{"WORKERS=2"}}}
	require.Zero(t, newStartupTimings(ctx, changed, st.Cache(), nil).estimate("m"))
	changed["m"] = config.ModelConfig{Cmd: "model --ctx 100", Env: []string{"WORKERS=4"}}
	require.Zero(t, newStartupTimings(ctx, changed, st.Cache(), nil).estimate("m"))
}

func TestServer_StartupTimingsIgnoresCorruptCache(t *testing.T) {
	ctx := context.Background()
	st, err := sqlite.New("")
	require.NoError(t, err)
	t.Cleanup(func() { st.Close() })
	mc := config.ModelConfig{Cmd: "model"}
	for _, data := range []string{"invalid", "[-1]", "[0]", "[9223372036854775807]", "[1,2,3,4]"} {
		require.NoError(t, st.Cache().Set(ctx, store.CacheEntry{Key: startupCacheKey("m", mc), Data: []byte(data)}))
		history := newStartupTimings(ctx, map[string]config.ModelConfig{"m": mc}, st.Cache(), nil)
		require.Zero(t, history.estimate("m"), data)
	}
}

func TestServer_StartupPredictionStatus(t *testing.T) {
	local := newStubRouter([]string{"m"}, "")
	s := newTestServer(local, newStubRouter(nil, ""))
	s.cfg.Models = map[string]config.ModelConfig{"m": {Cmd: "model"}}
	s.startup = newStartupTimings(context.Background(), s.cfg.Models, nil, nil)
	s.startup.record(context.Background(), swaputil.ProcessStateChangeEvent{
		ProcessName: "m", OldState: "starting", NewState: "ready", StartupDuration: 10 * time.Second,
	})
	local.running = map[string]process.ProcessState{"m": process.StateStarting}
	local.startingSince = map[string]time.Time{"m": time.Now().Add(-5 * time.Second)}
	status := s.modelStatus()
	require.Len(t, status, 1)
	require.EqualValues(t, 10_000, status[0].EstimatedLoadMs)
	require.GreaterOrEqual(t, status[0].LoadingElapsedMs, int64(5000))
	require.Less(t, status[0].LoadingElapsedMs, int64(6000))
	require.Nil(t, status[0].LoadingProgress)
	progress := 0.25
	local.loading = map[string]*process.LoadingProgress{"m": {Progress: &progress}}
	require.Equal(t, &progress, s.modelStatus()[0].LoadingProgress)
	local.running["m"] = process.StateStopped
	require.Zero(t, s.modelStatus()[0].LoadingElapsedMs)
}
