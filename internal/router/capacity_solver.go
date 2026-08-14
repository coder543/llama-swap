package router

import (
	"maps"
	"slices"
	"sort"

	"github.com/mostlygeek/llama-swap/internal/logmon"
	"github.com/mostlygeek/llama-swap/internal/router/scheduler"
)

type capacityStrategy string

const (
	capacityEvictionCost capacityStrategy = "cost"
	capacityEvictionLRU  capacityStrategy = "lru"
)

// capacitySolver chooses evictions from a user-defined total capacity and
// per-model memory costs. It has no process dependencies and is safe for
// concurrent reads after construction.
type capacitySolver struct {
	capacity int
	memory   map[string]int
	costs    map[string]int
	strategy capacityStrategy
}

type capacitySolveResult struct {
	solveResult
	Blocked bool
}

type capacityCandidate struct {
	cost   int
	models []string
}

func newCapacitySolver(capacity int, memory, costs map[string]int, strategy capacityStrategy) *capacitySolver {
	if strategy == "" {
		strategy = capacityEvictionCost
	}
	return &capacitySolver{
		capacity: capacity,
		memory:   memory,
		costs:    costs,
		strategy: strategy,
	}
}

func (s *capacitySolver) Solve(target string, state scheduler.PlanningState) capacitySolveResult {
	requestedMemory, ok := s.memory[target]
	if !ok {
		// Configuration validation guarantees this cannot happen for a model
		// handled by the matrix router. Treat it as blocked rather than risking
		// an over-capacity start if an invalid solver is constructed directly.
		return capacitySolveResult{Blocked: true}
	}
	requestedMemory = min(requestedMemory, s.capacity)

	if slices.Contains(state.Running, target) {
		return capacitySolveResult{solveResult: solveResult{
			TargetSet: slices.Clone(state.Running),
			SetName:   "capacity",
		}}
	}

	needed := s.totalMemory(state.Running) + requestedMemory - s.capacity
	if needed <= 0 {
		targetSet := append(slices.Clone(state.Running), target)
		sort.Strings(targetSet)
		return capacitySolveResult{solveResult: solveResult{
			TargetSet: targetSet,
			SetName:   "capacity",
		}}
	}

	eligible := make([]string, 0, len(state.Running))
	for _, model := range state.Running {
		if _, protected := state.Protected[model]; !protected {
			eligible = append(eligible, model)
		}
	}
	if s.totalMemory(eligible) < needed {
		return capacitySolveResult{Blocked: true}
	}

	var evict []string
	switch s.strategy {
	case capacityEvictionLRU:
		evict = s.solveLRU(eligible, needed, state.LastGranted)
	default:
		evict = s.solveCost(eligible, needed)
	}

	targetSet := make([]string, 0, len(state.Running)-len(evict)+1)
	for _, running := range state.Running {
		if !slices.Contains(evict, running) {
			targetSet = append(targetSet, running)
		}
	}
	targetSet = append(targetSet, target)
	sort.Strings(targetSet)

	totalCost := 0
	for _, model := range evict {
		totalCost += s.evictCost(model)
	}

	return capacitySolveResult{solveResult: solveResult{
		Evict:     evict,
		TargetSet: targetSet,
		SetName:   "capacity",
		TotalCost: totalCost,
	}}
}

func (s *capacitySolver) totalMemory(models []string) int {
	total := 0
	for _, model := range models {
		total += min(s.memory[model], s.capacity)
	}
	return total
}

func (s *capacitySolver) solveLRU(eligible []string, needed int, lastGranted map[string]uint64) []string {
	models := slices.Clone(eligible)
	sort.Slice(models, func(i, j int) bool {
		left := lastGranted[models[i]]
		right := lastGranted[models[j]]
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
	sort.Strings(evict)
	return evict
}

// solveCost is an exact sparse dynamic-programming solver. Freed memory is
// capped at needed, so all candidates that make enough room share one terminal
// state. This remains exact with more than a machine-word's worth of models.
func (s *capacitySolver) solveCost(eligible []string, needed int) []string {
	models := slices.Clone(eligible)
	sort.Strings(models)

	dp := map[int]capacityCandidate{0: {}}
	for _, model := range models {
		next := make(map[int]capacityCandidate, len(dp)*2)
		for freed, existing := range dp {
			if current, ok := next[freed]; !ok || betterCapacityCandidate(existing, current) {
				next[freed] = existing
			}

			newFreed := min(needed, freed+min(s.memory[model], s.capacity))
			chosen := capacityCandidate{
				cost:   existing.cost + s.evictCost(model),
				models: append(slices.Clone(existing.models), model),
			}
			if current, ok := next[newFreed]; !ok || betterCapacityCandidate(chosen, current) {
				next[newFreed] = chosen
			}
		}
		dp = next
	}

	best := dp[needed]
	return slices.Clone(best.models)
}

func betterCapacityCandidate(left, right capacityCandidate) bool {
	if left.cost != right.cost {
		return left.cost < right.cost
	}
	if len(left.models) != len(right.models) {
		return len(left.models) < len(right.models)
	}
	return slices.Compare(left.models, right.models) < 0
}

func (s *capacitySolver) evictCost(model string) int {
	if cost, ok := s.costs[model]; ok {
		return cost
	}
	return s.memory[model]
}

// capacitySwapper adapts the capacity solver to the scheduler's speculative
// planning interface and logs only decisions that are actually committed.
type capacitySwapper struct {
	solver *capacitySolver
	logger *logmon.Monitor

	lastTarget string
	lastState  scheduler.PlanningState
	lastResult capacitySolveResult
	lastValid  bool
}

func (p *capacitySwapper) solve(target string, state scheduler.PlanningState) capacitySolveResult {
	if p.lastValid && p.lastTarget == target && planningStatesEqual(p.lastState, state) {
		return p.lastResult
	}
	result := p.solver.Solve(target, state)
	p.lastTarget = target
	p.lastState = clonePlanningState(state)
	p.lastResult = result
	p.lastValid = true
	return result
}

func (p *capacitySwapper) EvictionFor(target string, state scheduler.PlanningState) scheduler.EvictionDecision {
	result := p.solve(target, state)
	return scheduler.EvictionDecision{Evict: result.Evict, Blocked: result.Blocked}
}

func (p *capacitySwapper) OnSwapStart(target string, state scheduler.PlanningState) {
	result := p.solve(target, state)
	switch {
	case len(result.Evict) > 0:
		p.logger.Infof("matrix: model=%s set=capacity evict=%v target=%v cost=%d",
			target, result.Evict, result.TargetSet, result.TotalCost)
	case len(state.Running) == 0:
		p.logger.Infof("matrix: model=%s starting (no models running)", target)
	default:
		p.logger.Debugf("matrix: model=%s already running in set=capacity", target)
	}
}

func clonePlanningState(state scheduler.PlanningState) scheduler.PlanningState {
	return scheduler.PlanningState{
		Running:     slices.Clone(state.Running),
		Protected:   maps.Clone(state.Protected),
		LastGranted: maps.Clone(state.LastGranted),
	}
}

func planningStatesEqual(left, right scheduler.PlanningState) bool {
	return slices.Equal(left.Running, right.Running) &&
		maps.Equal(left.Protected, right.Protected) &&
		maps.Equal(left.LastGranted, right.LastGranted)
}
