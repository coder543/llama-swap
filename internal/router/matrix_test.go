package router

import (
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/mostlygeek/llama-swap/internal/config"
	"github.com/mostlygeek/llama-swap/internal/logmon"
	"github.com/mostlygeek/llama-swap/internal/process"
)

// newTestMatrix builds a Matrix router from supplied processes, bypassing
// NewMatrix's call to process.New.
func newTestMatrix(t *testing.T, conf config.Config, sets config.OrderedSets, evictCosts map[string]int, processes map[string]process.Process) *Matrix {
	t.Helper()
	models := make(map[string]config.ModelConfig, len(processes))
	for model := range processes {
		models[model] = config.ModelConfig{}
	}
	matrix := &config.MatrixConfig{
		EvictCosts: evictCosts,
		Sets:       sets,
	}
	if err := config.ValidateMatrix(matrix, models); err != nil {
		t.Fatalf("ValidateMatrix: %v", err)
	}

	logger := logmon.NewWriter(io.Discard)
	swapper := &matrixSwapper{
		solver: newMatrixSolver(matrix.Program(), matrix.ResolvedEvictCosts()),
		logger: logger,
	}
	base, err := newBaseRouter("matrix", conf, processes, logger, swapper)
	if err != nil {
		t.Fatalf("newBaseRouter: %v", err)
	}
	base.testProcessed = make(chan struct{}, 64)
	r := &Matrix{baseRouter: base}
	go base.run()
	t.Cleanup(func() {
		if !r.shuttingDown.Load() {
			_ = r.Shutdown(time.Second)
		}
	})
	return r
}

func newTestCapacityMatrix(t *testing.T, capacity int, strategy string, models map[string]config.ModelConfig, processes map[string]process.Process) *Matrix {
	t.Helper()
	conf := config.Config{HealthCheckTimeout: 5, Models: models}
	matrix := &config.MatrixConfig{Capacity: capacity, Strategy: strategy}
	if err := config.ValidateMatrix(matrix, models); err != nil {
		t.Fatalf("ValidateMatrix: %v", err)
	}
	conf.Routing.Router.Use = "matrix"
	conf.Routing.Router.Settings.Matrix = matrix

	memory := make(map[string]int, len(models))
	costs := make(map[string]int)
	for modelID, model := range models {
		memory[modelID] = model.Memory
		if model.EvictCost != nil {
			costs[modelID] = *model.EvictCost
		}
	}
	logger := logmon.NewWriter(io.Discard)
	swapper := &capacitySwapper{
		solver: newCapacitySolver(capacity, memory, costs, capacityStrategy(strategy)),
		logger: logger,
	}
	base, err := newBaseRouter("matrix", conf, processes, logger, swapper)
	if err != nil {
		t.Fatalf("newBaseRouter: %v", err)
	}
	base.testProcessed = make(chan struct{}, 64)
	r := &Matrix{baseRouter: base}
	go base.run()
	t.Cleanup(func() {
		if !r.shuttingDown.Load() {
			_ = r.Shutdown(time.Second)
		}
	})
	return r
}

func baseMatrixConfig() config.Config {
	return config.Config{
		HealthCheckTimeout: 5,
		Matrix:             &config.MatrixConfig{},
	}
}

// TestMatrix_SwapEvictsConflicting verifies that loading a model triggers
// eviction of running models that are not in any shared set with it.
func TestMatrix_SwapEvictsConflicting(t *testing.T) {
	a := newFakeProcess("a")
	a.markReady()
	go a.Run(0) // park a Run goroutine so Stop has something to release

	b := newFakeProcess("b")
	b.autoReady = true

	// Two single-model sets: a and b never coexist, so loading b must evict a.
	sets := config.OrderedSets{
		{Name: "s_a", DSL: "a"},
		{Name: "s_b", DSL: "b"},
	}
	r := newTestMatrix(t, baseMatrixConfig(), sets, nil, map[string]process.Process{"a": a, "b": b})

	w := httptest.NewRecorder()
	r.ServeHTTP(w, newRequest("b"))

	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%q", w.Code, w.Body.String())
	}
	if got := a.stopCalls.Load(); got != 1 {
		t.Errorf("a.stopCalls=%d want 1", got)
	}
	if got := b.runCalls.Load(); got != 1 {
		t.Errorf("b.runCalls=%d want 1", got)
	}
}

// TestMatrix_CoexistInSet verifies that a model is not evicted when the target
// shares a set with it (the fast path applies if the target is already ready).
func TestMatrix_CoexistInSet(t *testing.T) {
	a := newFakeProcess("a")
	a.markReady()
	go a.Run(0)

	b := newFakeProcess("b")
	b.autoReady = true

	// Both fit in s_ab, so b's swap should not stop a.
	sets := config.OrderedSets{
		{Name: "s_ab", DSL: "a & b"},
	}
	r := newTestMatrix(t, baseMatrixConfig(), sets, nil, map[string]process.Process{"a": a, "b": b})

	w := httptest.NewRecorder()
	r.ServeHTTP(w, newRequest("b"))

	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%q", w.Code, w.Body.String())
	}
	if got := a.stopCalls.Load(); got != 0 {
		t.Errorf("a.stopCalls=%d want 0 (coexists with b)", got)
	}
	if got := b.runCalls.Load(); got != 1 {
		t.Errorf("b.runCalls=%d want 1", got)
	}
}

// TestMatrix_CoexistingSetParallel verifies that two models that share an
// expanded set load in parallel — the solver returns empty Evict for both,
// the collision predicate clears them, and both swaps run together.
func TestMatrix_CoexistingSetParallel(t *testing.T) {
	a := newFakeProcess("a")
	pb := newFakeProcess("b")

	sets := config.OrderedSets{
		{Name: "s_ab", DSL: "a & b"},
	}
	r := newTestMatrix(t, baseMatrixConfig(), sets, nil, map[string]process.Process{"a": a, "b": pb})

	w1 := httptest.NewRecorder()
	done1 := make(chan struct{})
	go func() {
		r.ServeHTTP(w1, newRequest("a"))
		close(done1)
	}()
	waitProcessed(t, r.testProcessed, 1)

	w2 := httptest.NewRecorder()
	done2 := make(chan struct{})
	go func() {
		r.ServeHTTP(w2, newRequest("b"))
		close(done2)
	}()
	waitProcessed(t, r.testProcessed, 1)

	<-a.runStarted
	<-pb.runStarted

	a.markReady()
	pb.markReady()

	for i, ch := range []chan struct{}{done1, done2} {
		select {
		case <-ch:
		case <-time.After(time.Second):
			t.Fatalf("request %d did not complete", i)
		}
	}
	if got := a.stopCalls.Load(); got != 0 {
		t.Errorf("a.stopCalls=%d want 0 (coexists with b)", got)
	}
	if got := pb.stopCalls.Load(); got != 0 {
		t.Errorf("b.stopCalls=%d want 0 (coexists with a)", got)
	}
}

// TestMatrix_IncompatibleQueues verifies that the second request for a model
// that cannot coexist with the in-flight first model queues until the first
// completes, and then evicts it. This exercises the scheduler folding in-flight
// swap targets into the running set it hands the swapper.
func TestMatrix_IncompatibleQueues(t *testing.T) {
	a := newFakeProcess("a")
	pb := newFakeProcess("b")

	sets := config.OrderedSets{
		{Name: "s_a", DSL: "a"},
		{Name: "s_b", DSL: "b"},
	}
	r := newTestMatrix(t, baseMatrixConfig(), sets, nil, map[string]process.Process{"a": a, "b": pb})

	w1 := httptest.NewRecorder()
	done1 := make(chan struct{})
	go func() {
		r.ServeHTTP(w1, newRequest("a"))
		close(done1)
	}()
	waitProcessed(t, r.testProcessed, 1)

	// B arrives before A transitions to StateStarting. The running set the
	// scheduler builds includes A (an in-flight swap target), so the solver
	// returns evict=[a] and collidesWith forces B to queue.
	w2 := httptest.NewRecorder()
	done2 := make(chan struct{})
	go func() {
		r.ServeHTTP(w2, newRequest("b"))
		close(done2)
	}()
	waitProcessed(t, r.testProcessed, 1)

	if got := pb.runCalls.Load(); got != 0 {
		t.Errorf("b started in parallel: runCalls=%d want 0", got)
	}

	<-a.runStarted
	a.markReady()
	waitProcessed(t, r.testProcessed, 1) // swapDone(a) → b promoted, evicts a
	<-pb.runStarted
	pb.markReady()

	for i, ch := range []chan struct{}{done1, done2} {
		select {
		case <-ch:
		case <-time.After(time.Second):
			t.Fatalf("request %d did not complete", i)
		}
	}
	if got := a.stopCalls.Load(); got != 1 {
		t.Errorf("a.stopCalls=%d want 1 (b's swap must stop a)", got)
	}
}

// TestMatrixSolver_TieBreakDefinitionOrder pins the solver's tie-break rule:
// when multiple candidate sets have equal eviction cost, the earlier-defined
// set wins.
func TestMatrixSolver_TieBreakDefinitionOrder(t *testing.T) {
	s := newTestMatrixSolver(t, config.OrderedSets{
		{Name: "first", DSL: "a & b"},
		{Name: "second", DSL: "a & c"},
	}, nil, "a", "b", "c")

	// No models running, request "a": both sets have cost 0 and contain a.
	// Definition order: "first" wins.
	result := s.Solve("a", nil)
	if result.SetName != "first" {
		t.Errorf("SetName=%q want %q", result.SetName, "first")
	}
}

// TestMatrixSolver_EvictCostsPreferred verifies that higher evict costs steer
// the solver toward a cheaper set.
func TestMatrixSolver_EvictCostsPreferred(t *testing.T) {
	// b is expensive to evict; c is cheap. Request "a" with both b and c
	// running. The solver should pick the set that keeps b.
	s := newTestMatrixSolver(t, config.OrderedSets{
		{Name: "a_with_c", DSL: "a & c"}, // would evict b (cost 10)
		{Name: "a_with_b", DSL: "a & b"}, // would evict c (cost 1)
	}, map[string]int{"b": 10, "c": 1}, "a", "b", "c")

	result := s.Solve("a", []string{"b", "c"})
	if result.SetName != "a_with_b" {
		t.Errorf("SetName=%q want %q (keep expensive b)", result.SetName, "a_with_b")
	}
	if len(result.Evict) != 1 || result.Evict[0] != "c" {
		t.Errorf("Evict=%v want [c]", result.Evict)
	}
}

func TestMatrix_CapacityEvictsByCost(t *testing.T) {
	a := newFakeProcess("a")
	a.markReady()
	b := newFakeProcess("b")
	b.markReady()
	c := newFakeProcess("c")
	c.autoReady = true
	models := map[string]config.ModelConfig{
		"a": {Memory: 25, EvictCost: new(100)},
		"b": {Memory: 10, EvictCost: new(0)},
		"c": {Memory: 15},
	}
	r := newTestCapacityMatrix(t, 40, "cost", models,
		map[string]process.Process{"a": a, "b": b, "c": c})

	w := httptest.NewRecorder()
	r.ServeHTTP(w, newRequest("c"))
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%q", w.Code, w.Body.String())
	}
	if got := a.stopCalls.Load(); got != 0 {
		t.Errorf("a.stopCalls=%d want 0", got)
	}
	if got := b.stopCalls.Load(); got != 1 {
		t.Errorf("b.stopCalls=%d want 1", got)
	}
}

func TestMatrix_CapacityLRURefreshesOnGrant(t *testing.T) {
	a := newFakeProcess("a")
	a.markReady()
	b := newFakeProcess("b")
	b.markReady()
	c := newFakeProcess("c")
	c.autoReady = true
	models := map[string]config.ModelConfig{
		"a": {Memory: 25},
		"b": {Memory: 10},
		"c": {Memory: 15},
	}
	r := newTestCapacityMatrix(t, 40, "lru", models,
		map[string]process.Process{"a": a, "b": b, "c": c})

	for _, modelID := range []string{"a", "b", "a"} {
		w := httptest.NewRecorder()
		r.ServeHTTP(w, newRequest(modelID))
		if w.Code != http.StatusOK {
			t.Fatalf("request %s status=%d body=%q", modelID, w.Code, w.Body.String())
		}
	}

	w := httptest.NewRecorder()
	r.ServeHTTP(w, newRequest("c"))
	if got := a.stopCalls.Load(); got != 0 {
		t.Errorf("a.stopCalls=%d want 0 (most recently granted)", got)
	}
	if got := b.stopCalls.Load(); got != 1 {
		t.Errorf("b.stopCalls=%d want 1 (least recently granted)", got)
	}
}

func TestMatrix_CapacityChoosesIdleAlternativeToBusyCheapModel(t *testing.T) {
	a := newFakeProcess("a")
	a.markReady()
	a.serveBlock = make(chan struct{})
	b := newFakeProcess("b")
	b.markReady()
	c := newFakeProcess("c")
	c.autoReady = true
	models := map[string]config.ModelConfig{
		"a": {Memory: 25, EvictCost: new(0)},
		"b": {Memory: 10, EvictCost: new(100)},
		"c": {Memory: 15},
	}
	r := newTestCapacityMatrix(t, 40, "cost", models,
		map[string]process.Process{"a": a, "b": b, "c": c})

	aDone := make(chan struct{})
	go func() {
		defer close(aDone)
		r.ServeHTTP(httptest.NewRecorder(), newRequest("a"))
	}()
	waitSignal(t, a.serveStarted, "a ServeHTTP")

	w := httptest.NewRecorder()
	r.ServeHTTP(w, newRequest("c"))
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%q", w.Code, w.Body.String())
	}
	if got := a.stopCalls.Load(); got != 0 {
		t.Errorf("a.stopCalls=%d want 0 while serving", got)
	}
	if got := b.stopCalls.Load(); got != 1 {
		t.Errorf("b.stopCalls=%d want 1 as idle alternative", got)
	}

	close(a.serveBlock)
	waitSignal(t, aDone, "a request completion")
}

func TestMatrix_CapacityCountsActiveSwapTarget(t *testing.T) {
	a := newFakeProcess("a")
	b := newFakeProcess("b")
	b.autoReady = true
	models := map[string]config.ModelConfig{
		"a": {Memory: 30},
		"b": {Memory: 30},
	}
	r := newTestCapacityMatrix(t, 40, "cost", models,
		map[string]process.Process{"a": a, "b": b})

	aDone := make(chan struct{})
	go func() {
		defer close(aDone)
		r.ServeHTTP(httptest.NewRecorder(), newRequest("a"))
	}()
	waitProcessed(t, r.testProcessed, 1)

	bDone := make(chan struct{})
	go func() {
		defer close(bDone)
		r.ServeHTTP(httptest.NewRecorder(), newRequest("b"))
	}()
	waitProcessed(t, r.testProcessed, 1)
	if got := b.runCalls.Load(); got != 0 {
		t.Fatalf("b.runCalls=%d want 0 while a swap holds capacity", got)
	}

	a.markReady()
	waitSignal(t, aDone, "a request completion")
	waitSignal(t, bDone, "b request completion")
}

func newTestMatrixSolver(t *testing.T, sets config.OrderedSets, evictCosts map[string]int, modelNames ...string) *matrixSolver {
	t.Helper()
	models := make(map[string]config.ModelConfig, len(modelNames))
	for _, model := range modelNames {
		models[model] = config.ModelConfig{}
	}
	matrix := &config.MatrixConfig{
		EvictCosts: evictCosts,
		Sets:       sets,
	}
	if err := config.ValidateMatrix(matrix, models); err != nil {
		t.Fatalf("ValidateMatrix: %v", err)
	}
	return newMatrixSolver(matrix.Program(), matrix.ResolvedEvictCosts())
}
