package proxy

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"sort"
	"strings"
	"sync"

	"github.com/mostlygeek/llama-swap/proxy/config"
)

var errNoEvictableCapacity = errors.New("no evictable models can free enough capacity")

// MatrixSolver contains pure swap-decision logic with no Process dependencies.
// It is safe for concurrent reads after construction.
type MatrixSolver struct {
	expandedSets []config.ExpandedSet // all valid model combinations
	evictCosts   map[string]int       // real model name -> eviction cost (default 1)
	modelToSets  map[string][]int     // model name -> indices into expandedSets
}

type MatrixEvictionStrategy string

const (
	MatrixEvictionCost MatrixEvictionStrategy = "cost"
	MatrixEvictionLRU  MatrixEvictionStrategy = "lru"
)

// CapacitySolver contains swap-decision logic for capacity-based matrix mode.
type CapacitySolver struct {
	capacity int
	memory   map[string]int
	costs    map[string]int
	strategy MatrixEvictionStrategy
}

type capacityCandidate struct {
	cost   int
	models []string
}

func NewCapacitySolver(capacity int, memory map[string]int, costs map[string]int, strategy MatrixEvictionStrategy) *CapacitySolver {
	if strategy == "" {
		strategy = MatrixEvictionCost
	}
	return &CapacitySolver{
		capacity: capacity,
		memory:   memory,
		costs:    costs,
		strategy: strategy,
	}
}

// NewMatrixSolver builds a solver from expanded sets and eviction costs.
func NewMatrixSolver(expandedSets []config.ExpandedSet, evictCosts map[string]int) *MatrixSolver {
	modelToSets := make(map[string][]int)
	for i, es := range expandedSets {
		for _, model := range es.Models {
			modelToSets[model] = append(modelToSets[model], i)
		}
	}

	return &MatrixSolver{
		expandedSets: expandedSets,
		evictCosts:   evictCosts,
		modelToSets:  modelToSets,
	}
}

// SolveResult describes what the solver decided.
type SolveResult struct {
	Evict     []string // running models that must be stopped
	TargetSet []string // the chosen set of models (for informational purposes)
	SetName   string   // name of the chosen set
	DSL       string   // original DSL expression for the chosen set
	TotalCost int      // total eviction cost
}

// Solve determines which models to evict when a model is requested.
//
// Algorithm:
//  1. If requestedModel is already running, no eviction needed.
//  2. Find all sets containing requestedModel.
//  3. If no sets found, the model runs alone; evict all running models.
//  4. For each candidate set, compute cost = sum of evict_costs for running
//     models NOT in that set.
//  5. Pick lowest cost. Ties broken by definition order (index in expandedSets).
//  6. Return models to evict and the chosen set.
func (s *MatrixSolver) Solve(requestedModel string, runningModels []string) (SolveResult, error) {
	// If already running, nothing to do (but fill in set info for logging)
	if slices.Contains(runningModels, requestedModel) {
		setName, dsl := s.findMatchingSet(requestedModel, runningModels)
		return SolveResult{
			TargetSet: runningModels,
			SetName:   setName,
			DSL:       dsl,
		}, nil
	}

	candidateIndices := s.modelToSets[requestedModel]

	// Model not in any set: runs alone, evict everything
	if len(candidateIndices) == 0 {
		evict := make([]string, len(runningModels))
		copy(evict, runningModels)
		return SolveResult{
			Evict:     evict,
			TargetSet: []string{requestedModel},
		}, nil
	}

	// Find the cheapest candidate set
	bestCost := -1
	bestIdx := -1

	for _, idx := range candidateIndices {
		setModels := s.expandedSets[idx].Models
		cost := 0
		for _, running := range runningModels {
			if !slices.Contains(setModels, running) {
				cost += s.evictCost(running)
			}
		}

		if bestCost < 0 || cost < bestCost || (cost == bestCost && idx < bestIdx) {
			bestCost = cost
			bestIdx = idx
		}
	}

	// Determine which running models to evict
	chosen := s.expandedSets[bestIdx]
	var evict []string
	for _, running := range runningModels {
		if !slices.Contains(chosen.Models, running) {
			evict = append(evict, running)
		}
	}

	return SolveResult{
		Evict:     evict,
		TargetSet: chosen.Models,
		SetName:   chosen.SetName,
		DSL:       chosen.DSL,
		TotalCost: bestCost,
	}, nil
}

// findMatchingSet finds the expanded set that contains all running models.
// Returns the set name and DSL, or empty strings if no match.
func (s *MatrixSolver) findMatchingSet(requestedModel string, runningModels []string) (string, string) {
	for _, idx := range s.modelToSets[requestedModel] {
		set := s.expandedSets[idx]
		allInSet := true
		for _, m := range runningModels {
			if !slices.Contains(set.Models, m) {
				allInSet = false
				break
			}
		}
		if allInSet {
			return set.SetName, set.DSL
		}
	}
	return "", ""
}

func (s *MatrixSolver) evictCost(model string) int {
	if cost, ok := s.evictCosts[model]; ok {
		return cost
	}
	return 1
}

func (s *CapacitySolver) Solve(requestedModel string, runningModels []string, lastUsed map[string]uint64, nonEvictable map[string]bool) (SolveResult, error) {
	requestedMemory, ok := s.memory[requestedModel]
	if !ok {
		return SolveResult{}, fmt.Errorf("model %s has no memory configured", requestedModel)
	}
	requestedMemory = min(requestedMemory, s.capacity)
	if slices.Contains(runningModels, requestedModel) {
		return SolveResult{
			TargetSet: runningModels,
			SetName:   "capacity",
		}, nil
	}

	used := s.totalMemory(runningModels)
	needed := used + requestedMemory - s.capacity
	if needed <= 0 {
		target := append([]string{}, runningModels...)
		target = append(target, requestedModel)
		sort.Strings(target)
		return SolveResult{
			TargetSet: target,
			SetName:   "capacity",
		}, nil
	}

	var evict []string
	evictable := evictableModels(runningModels, nonEvictable)
	switch s.strategy {
	case MatrixEvictionLRU:
		evict = s.solveLRU(evictable, needed, lastUsed)
	default:
		evict = s.solveCost(evictable, needed)
	}
	if len(evict) == 0 {
		return SolveResult{}, errNoEvictableCapacity
	}

	target := make([]string, 0, len(runningModels)-len(evict)+1)
	for _, running := range runningModels {
		if !slices.Contains(evict, running) {
			target = append(target, running)
		}
	}
	target = append(target, requestedModel)
	sort.Strings(target)

	totalCost := 0
	for _, model := range evict {
		totalCost += s.evictCost(model)
	}

	return SolveResult{
		Evict:     evict,
		TargetSet: target,
		SetName:   "capacity",
		TotalCost: totalCost,
	}, nil
}

func (s *CapacitySolver) totalMemory(models []string) int {
	total := 0
	for _, model := range models {
		total += min(s.memory[model], s.capacity)
	}
	return total
}

func evictableModels(runningModels []string, nonEvictable map[string]bool) []string {
	models := make([]string, 0, len(runningModels))
	for _, model := range runningModels {
		if nonEvictable[model] {
			continue
		}
		models = append(models, model)
	}
	return models
}

func (s *CapacitySolver) solveLRU(evictable []string, needed int, lastUsed map[string]uint64) []string {
	models := append([]string{}, evictable...)
	sort.Slice(models, func(i, j int) bool {
		left := lastUsed[models[i]]
		right := lastUsed[models[j]]
		if left == right {
			return models[i] < models[j]
		}
		return left < right
	})

	freed := 0
	var evict []string
	for _, model := range models {
		evict = append(evict, model)
		freed += min(s.memory[model], s.capacity)
		if freed >= needed {
			break
		}
	}
	if freed < needed {
		return nil
	}
	sort.Strings(evict)
	return evict
}

func (s *CapacitySolver) solveCost(runningModels []string, needed int) []string {
	models := append([]string{}, runningModels...)
	sort.Strings(models)

	dp := map[int]capacityCandidate{0: {cost: 0}}
	for _, model := range models {
		next := make(map[int]capacityCandidate, len(dp)*2)
		for freed, existing := range dp {
			if cur, ok := next[freed]; !ok || betterCandidate(existing, cur) {
				next[freed] = existing
			}

			newFreed := min(needed, freed+min(s.memory[model], s.capacity))
			chosen := capacityCandidate{
				cost:   existing.cost + s.evictCost(model),
				models: append(append([]string{}, existing.models...), model),
			}
			if cur, ok := next[newFreed]; !ok || betterCandidate(chosen, cur) {
				next[newFreed] = chosen
			}
		}
		dp = next
	}

	best, ok := dp[needed]
	if !ok {
		return nil
	}
	models = best.models
	sort.Strings(models)
	return models
}

func betterCandidate(left, right capacityCandidate) bool {
	if left.cost != right.cost {
		return left.cost < right.cost
	}
	if len(left.models) != len(right.models) {
		return len(left.models) < len(right.models)
	}
	return strings.Join(left.models, "\x00") < strings.Join(right.models, "\x00")
}

func (s *CapacitySolver) evictCost(model string) int {
	if cost, ok := s.costs[model]; ok {
		return cost
	}
	return s.memory[model]
}

// Matrix manages processes using solver-based swap logic.
type Matrix struct {
	sync.Mutex
	solver         *MatrixSolver
	capacitySolver *CapacitySolver
	processes      map[string]*Process // all processes keyed by real model name
	config         config.Config
	proxyLogger    *LogMonitor
	upstreamLogger *LogMonitor
	lastUsed       map[string]uint64
	reservations   map[string]int
	activeRequests map[string]int
	useCounter     uint64
	cond           *sync.Cond

	// testDelayFastPath is a test-only hook invoked in the no-eviction path
	// after m.Lock is released but before the request is dispatched to
	// Process.ProxyRequest. Tests use it to park a request at the exact
	// race window to deterministically reproduce the race.
	testDelayFastPath func()
}

// NewMatrix creates a Matrix from config. It creates a Process for every
// model defined in the config (any model can run alone even if not in a set).
func NewMatrix(cfg config.Config, proxyLogger, upstreamLogger *LogMonitor) *Matrix {
	processes := make(map[string]*Process)
	for modelID, modelConfig := range cfg.Models {
		processLogger := NewLogMonitorWriter(upstreamLogger)
		process := NewProcess(modelID, cfg.HealthCheckTimeout, modelConfig, processLogger, proxyLogger)
		processes[modelID] = process
	}

	var solver *MatrixSolver
	var capacitySolver *CapacitySolver
	if cfg.Matrix.Capacity > 0 {
		memory := make(map[string]int, len(cfg.Models))
		costs := make(map[string]int)
		for modelID, modelConfig := range cfg.Models {
			memory[modelID] = modelConfig.Memory
			if modelConfig.EvictCost != nil {
				costs[modelID] = *modelConfig.EvictCost
			}
		}
		capacitySolver = NewCapacitySolver(cfg.Matrix.Capacity, memory, costs, MatrixEvictionStrategy(cfg.Matrix.Strategy))
	} else {
		evictCosts := cfg.Matrix.ResolvedEvictCosts()
		solver = NewMatrixSolver(cfg.ExpandedSets, evictCosts)
	}

	matrix := &Matrix{
		solver:         solver,
		capacitySolver: capacitySolver,
		processes:      processes,
		config:         cfg,
		proxyLogger:    proxyLogger,
		upstreamLogger: upstreamLogger,
		lastUsed:       make(map[string]uint64),
		reservations:   make(map[string]int),
		activeRequests: make(map[string]int),
	}
	matrix.cond = sync.NewCond(&matrix.Mutex)
	return matrix
}

// ProxyRequest handles the swap logic and proxies the request to the model.
func (m *Matrix) ProxyRequest(modelID string, w http.ResponseWriter, r *http.Request) error {
	process, ok := m.processes[modelID]
	if !ok {
		return fmt.Errorf("model %s not found in matrix", modelID)
	}

	m.Lock()
	active, result, err := m.solveWithRetry(r.Context(), modelID)
	if err != nil {
		return err
	}
	needsReservation := needsCapacityReservation(process)

	// Log solver decision
	if len(result.Evict) > 0 {
		m.proxyLogger.Infof("Matrix: model=%s set=%s dsl=%q evict=%v target=%v cost=%d",
			modelID, result.SetName, result.DSL, result.Evict, result.TargetSet, result.TotalCost)
	} else if len(active) == 0 {
		m.proxyLogger.Infof("Matrix: model=%s starting (no models running)", modelID)
	} else {
		m.proxyLogger.Debugf("Matrix: model=%s already running in set=%s dsl=%q", modelID, result.SetName, result.DSL)
	}

	// Evict models that need to be stopped
	if len(result.Evict) > 0 {
		if err := m.waitForEvictionsLocked(r.Context(), result.Evict); err != nil {
			m.Unlock()
			return err
		}

		var wg sync.WaitGroup
		for _, evictModel := range result.Evict {
			delete(m.reservations, evictModel)
			if p, exists := m.processes[evictModel]; exists {
				wg.Add(1)
				go func(p *Process) {
					defer wg.Done()
					p.Stop()
				}(p)
			}
		}
		wg.Wait()
	}

	m.activeRequests[modelID]++
	if needsReservation {
		m.reservations[modelID]++
	}
	m.markUsedLocked(modelID)
	isFastPath := len(result.Evict) == 0
	m.Unlock()

	if isFastPath && m.testDelayFastPath != nil {
		m.testDelayFastPath()
	}

	func() {
		defer m.finishRequest(modelID)
		process.ProxyRequest(w, r)
	}()
	if needsReservation {
		m.clearReservation(modelID)
	}
	return nil
}

func (m *Matrix) solveWithRetry(ctx context.Context, modelID string) ([]string, SolveResult, error) {
	for {
		active := m.activeModels()
		result, err := m.solveLocked(modelID, active)
		if errors.Is(err, errNoEvictableCapacity) {
			if err := m.waitForChangeLocked(ctx); err != nil {
				m.Unlock()
				return nil, SolveResult{}, err
			}
			continue
		}
		if err != nil {
			m.Unlock()
			return nil, SolveResult{}, fmt.Errorf("matrix solver error: %w", err)
		}
		return active, result, nil
	}
}

func (m *Matrix) solveLocked(modelID string, active []string) (SolveResult, error) {
	if m.capacitySolver != nil {
		return m.capacitySolver.Solve(modelID, active, m.lastUsed, m.inflightModels())
	}
	return m.solver.Solve(modelID, active)
}

func (m *Matrix) inflightModels() map[string]bool {
	inflight := make(map[string]bool)
	for modelID, count := range m.activeRequests {
		if count > 0 {
			inflight[modelID] = true
		}
	}
	for modelID, process := range m.processes {
		if process.InFlightRequests() > 0 {
			inflight[modelID] = true
		}
	}
	return inflight
}

func (m *Matrix) waitForEvictionsLocked(ctx context.Context, models []string) error {
	for {
		blocked := false
		for _, modelID := range models {
			if m.activeRequests[modelID] > 0 {
				blocked = true
				break
			}
		}
		if !blocked {
			return nil
		}
		if err := m.waitForChangeLocked(ctx); err != nil {
			return err
		}
	}
}

func (m *Matrix) waitForChangeLocked(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	done := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			m.Lock()
			m.cond.Broadcast()
			m.Unlock()
		case <-done:
		}
	}()
	m.cond.Wait()
	close(done)
	return ctx.Err()
}

func (m *Matrix) finishRequest(modelID string) {
	m.Lock()
	defer m.Unlock()
	if m.activeRequests[modelID] <= 1 {
		delete(m.activeRequests, modelID)
	} else {
		m.activeRequests[modelID]--
	}
	m.cond.Broadcast()
}

func (m *Matrix) markUsedLocked(modelID string) {
	m.useCounter++
	m.lastUsed[modelID] = m.useCounter
}

func needsCapacityReservation(process *Process) bool {
	state := process.CurrentState()
	return state == StateStopped || state == StateShutdown
}

func (m *Matrix) clearReservation(modelID string) {
	m.Lock()
	defer m.Unlock()
	if m.reservations[modelID] <= 1 {
		delete(m.reservations, modelID)
		return
	}
	m.reservations[modelID]--
}

// StopProcesses stops all running processes.
func (m *Matrix) StopProcesses(strategy StopStrategy) {
	m.Lock()
	defer m.Unlock()

	var wg sync.WaitGroup
	for _, process := range m.processes {
		wg.Add(1)
		go func(p *Process) {
			defer wg.Done()
			switch strategy {
			case StopImmediately:
				p.StopImmediately()
			default:
				p.Stop()
			}
		}(process)
	}
	wg.Wait()
}

// StopProcess stops a single process by model ID.
func (m *Matrix) StopProcess(modelID string, strategy StopStrategy) error {
	process, ok := m.processes[modelID]
	if !ok {
		return fmt.Errorf("process not found for %s", modelID)
	}

	switch strategy {
	case StopImmediately:
		process.StopImmediately()
	default:
		process.Stop()
	}
	return nil
}

// Shutdown shuts down all processes.
func (m *Matrix) Shutdown() {
	var wg sync.WaitGroup
	for _, process := range m.processes {
		wg.Add(1)
		go func(p *Process) {
			defer wg.Done()
			p.Shutdown()
		}(process)
	}
	wg.Wait()
}

// RunningModels returns model names currently in an active (non-stopped) state.
func (m *Matrix) RunningModels() []string {
	m.Lock()
	defer m.Unlock()
	return m.runningModels()
}

// runningModels returns running model names (caller must hold lock).
func (m *Matrix) runningModels() []string {
	var running []string
	for id, process := range m.processes {
		if process.CurrentState() != StateStopped && process.CurrentState() != StateShutdown {
			running = append(running, id)
		}
	}
	sort.Strings(running)
	return running
}

func (m *Matrix) activeModels() []string {
	active := m.runningModels()
	for modelID := range m.reservations {
		if !slices.Contains(active, modelID) {
			active = append(active, modelID)
		}
	}
	sort.Strings(active)
	return active
}

// GetProcess returns the Process for a model.
func (m *Matrix) GetProcess(modelID string) (*Process, bool) {
	p, ok := m.processes[modelID]
	return p, ok
}

// HasModel returns true if the model is managed by this matrix.
func (m *Matrix) HasModel(modelID string) bool {
	_, ok := m.processes[modelID]
	return ok
}
