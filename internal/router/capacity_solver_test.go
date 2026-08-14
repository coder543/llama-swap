package router

import (
	"fmt"
	"slices"
	"testing"

	"github.com/mostlygeek/llama-swap/internal/router/scheduler"
)

func TestCapacitySolver_FitsWithoutEviction(t *testing.T) {
	solver := newCapacitySolver(40,
		map[string]int{"a": 10, "b": 15, "c": 15}, nil, capacityEvictionCost)

	result := solver.Solve("c", scheduler.PlanningState{Running: []string{"a", "b"}})
	if result.Blocked || len(result.Evict) != 0 {
		t.Fatalf("result=%+v want unblocked with no eviction", result)
	}
	if !slices.Equal(result.TargetSet, []string{"a", "b", "c"}) {
		t.Fatalf("TargetSet=%v want [a b c]", result.TargetSet)
	}
}

func TestCapacitySolver_CostEviction(t *testing.T) {
	t.Run("memory is default cost", func(t *testing.T) {
		solver := newCapacitySolver(40,
			map[string]int{"a": 25, "b": 10, "c": 15}, nil, capacityEvictionCost)
		result := solver.Solve("c", scheduler.PlanningState{Running: []string{"a", "b"}})
		if !slices.Equal(result.Evict, []string{"b"}) || result.TotalCost != 10 {
			t.Fatalf("result=%+v want evict [b] cost 10", result)
		}
	})

	t.Run("explicit cost including zero", func(t *testing.T) {
		solver := newCapacitySolver(40,
			map[string]int{"a": 25, "b": 10, "c": 15},
			map[string]int{"a": 0, "b": 100}, capacityEvictionCost)
		result := solver.Solve("c", scheduler.PlanningState{Running: []string{"a", "b"}})
		if !slices.Equal(result.Evict, []string{"a"}) || result.TotalCost != 0 {
			t.Fatalf("result=%+v want evict [a] cost 0", result)
		}
	})
}

func TestCapacitySolver_CostEvictionRemainsOptimalAboveTwentyFourModels(t *testing.T) {
	memory := map[string]int{"big": 100, "requested": 20}
	costs := map[string]int{"big": 11}
	running := []string{"big"}
	for i := 0; i < 25; i++ {
		modelID := fmt.Sprintf("small-%02d", i)
		memory[modelID] = 1
		costs[modelID] = 1
		running = append(running, modelID)
	}
	solver := newCapacitySolver(125, memory, costs, capacityEvictionCost)

	result := solver.Solve("requested", scheduler.PlanningState{Running: running})
	if !slices.Equal(result.Evict, []string{"big"}) || result.TotalCost != 11 {
		t.Fatalf("result=%+v want evict [big] cost 11", result)
	}
}

func TestCapacitySolver_LRUEviction(t *testing.T) {
	solver := newCapacitySolver(40,
		map[string]int{"a": 25, "b": 10, "c": 15}, nil, capacityEvictionLRU)
	state := scheduler.PlanningState{
		Running:     []string{"a", "b"},
		LastGranted: map[string]uint64{"a": 2, "b": 1},
	}

	result := solver.Solve("c", state)
	if !slices.Equal(result.Evict, []string{"b"}) {
		t.Fatalf("Evict=%v want [b]", result.Evict)
	}
}

func TestCapacitySolver_DeterministicTieBreak(t *testing.T) {
	memory := map[string]int{"a": 10, "b": 10, "target": 25}
	state := scheduler.PlanningState{Running: []string{"b", "a"}}

	for _, strategy := range []capacityStrategy{capacityEvictionCost, capacityEvictionLRU} {
		t.Run(string(strategy), func(t *testing.T) {
			solver := newCapacitySolver(40, memory, nil, strategy)
			result := solver.Solve("target", state)
			if !slices.Equal(result.Evict, []string{"a"}) {
				t.Fatalf("Evict=%v want lexical tie-break [a]", result.Evict)
			}
		})
	}
}

func TestCapacitySolver_SkipsProtectedModels(t *testing.T) {
	tests := []struct {
		name     string
		strategy capacityStrategy
		costs    map[string]int
		lastUsed map[string]uint64
		protect  string
		want     string
	}{
		{
			name:     "cost chooses more expensive idle model",
			strategy: capacityEvictionCost,
			costs:    map[string]int{"a": 0, "b": 100},
			protect:  "a",
			want:     "b",
		},
		{
			name:     "lru skips oldest protected model",
			strategy: capacityEvictionLRU,
			lastUsed: map[string]uint64{"a": 1, "b": 2},
			protect:  "a",
			want:     "b",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			solver := newCapacitySolver(40,
				map[string]int{"a": 25, "b": 10, "c": 15}, tt.costs, tt.strategy)
			result := solver.Solve("c", scheduler.PlanningState{
				Running:     []string{"a", "b"},
				Protected:   map[string]struct{}{tt.protect: {}},
				LastGranted: tt.lastUsed,
			})
			if result.Blocked || !slices.Equal(result.Evict, []string{tt.want}) {
				t.Fatalf("result=%+v want evict [%s]", result, tt.want)
			}
		})
	}
}

func TestCapacitySolver_BlockedWhenProtectedModelsMustBeEvicted(t *testing.T) {
	solver := newCapacitySolver(40,
		map[string]int{"a": 30, "b": 30}, nil, capacityEvictionCost)
	result := solver.Solve("b", scheduler.PlanningState{
		Running:   []string{"a"},
		Protected: map[string]struct{}{"a": {}},
	})
	if !result.Blocked || len(result.Evict) != 0 {
		t.Fatalf("result=%+v want blocked with no partial eviction", result)
	}
}

func TestCapacitySolver_OversizedRequestEvictsEverything(t *testing.T) {
	solver := newCapacitySolver(40,
		map[string]int{"a": 10, "b": 20, "huge": 50}, nil, capacityEvictionCost)
	result := solver.Solve("huge", scheduler.PlanningState{Running: []string{"a", "b"}})
	if !slices.Equal(result.Evict, []string{"a", "b"}) ||
		!slices.Equal(result.TargetSet, []string{"huge"}) {
		t.Fatalf("result=%+v want evict [a b], target [huge]", result)
	}
}
