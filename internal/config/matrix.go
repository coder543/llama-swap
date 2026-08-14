package config

import (
	"fmt"
	"regexp"

	matrixdsl "github.com/mostlygeek/llama-swap/internal/matrix"
	"gopkg.in/yaml.v3"
)

var varKeyPattern = regexp.MustCompile(`^[a-zA-Z0-9.-]{1,32}$`)

// MatrixConfig represents the swap matrix configuration block.
type MatrixConfig struct {
	Capacity   int               `yaml:"capacity"`
	Strategy   string            `yaml:"strategy"`
	Var        map[string]string `yaml:"vars"`
	EvictCosts map[string]int    `yaml:"evict_costs"`
	Sets       OrderedSets       `yaml:"sets"`

	program *matrixdsl.Program
}

// SetEntry is a single named set with its DSL expression.
type SetEntry struct {
	Name string
	DSL  string
}

// OrderedSets preserves YAML definition order of sets (used for tie-breaking).
type OrderedSets []SetEntry

func (os *OrderedSets) UnmarshalYAML(value *yaml.Node) error {
	if value.Kind != yaml.MappingNode {
		return fmt.Errorf("sets must be a mapping")
	}

	entries := make([]SetEntry, 0, len(value.Content)/2)
	for i := 0; i < len(value.Content); i += 2 {
		keyNode := value.Content[i]
		valueNode := value.Content[i+1]

		var name string
		if err := keyNode.Decode(&name); err != nil {
			return fmt.Errorf("failed to decode set name: %w", err)
		}

		var dsl string
		if err := valueNode.Decode(&dsl); err != nil {
			return fmt.Errorf("failed to decode DSL for set %q: %w", name, err)
		}

		entries = append(entries, SetEntry{Name: name, DSL: dsl})
	}

	*os = entries
	return nil
}

// ValidateMatrix validates and compiles a matrix configuration.
func ValidateMatrix(matrix *MatrixConfig, models map[string]ModelConfig) error {
	if matrix.Capacity < 0 {
		return fmt.Errorf("capacity must be a positive integer")
	}
	if matrix.Capacity > 0 {
		return validateCapacityMatrix(matrix, models)
	}
	if matrix.Strategy != "" {
		return fmt.Errorf("strategy requires capacity mode")
	}
	if len(matrix.Sets) == 0 {
		return fmt.Errorf("matrix must define at least one set")
	}

	// Validate var entries
	if matrix.Var != nil {
		for id, modelName := range matrix.Var {
			if !varKeyPattern.MatchString(id) {
				return fmt.Errorf("var key %q must contain only alphanumeric, '-' or '.' characters and be 1-32 characters long", id)
			}
			if _, exists := models[modelName]; !exists {
				return fmt.Errorf("var key %q references unknown model %q", id, modelName)
			}
		}
	}

	// Validate evict_costs
	if matrix.EvictCosts != nil {
		for key, cost := range matrix.EvictCosts {
			if cost <= 0 {
				return fmt.Errorf("evict_cost for %q must be a positive integer, got %d", key, cost)
			}
			if _, ok := resolveMatrixModel(key, matrix.Var, models); !ok {
				return fmt.Errorf("evict_costs: unknown var or model %q", key)
			}
		}
	}

	definitions := make([]matrixdsl.Definition, 0, len(matrix.Sets))
	for _, entry := range matrix.Sets {
		definitions = append(definitions, matrixdsl.Definition{
			Name: entry.Name,
			DSL:  entry.DSL,
		})
	}

	program, err := matrixdsl.Compile(definitions, func(ident string) (string, bool) {
		return resolveMatrixModel(ident, matrix.Var, models)
	})
	if err != nil {
		return err
	}
	matrix.program = program
	return nil
}

func validateCapacityMatrix(matrix *MatrixConfig, models map[string]ModelConfig) error {
	switch matrix.Strategy {
	case "", "cost", "lru":
	default:
		return fmt.Errorf("strategy must be one of: cost, lru")
	}

	if len(matrix.Sets) > 0 || len(matrix.Var) > 0 || len(matrix.EvictCosts) > 0 {
		return fmt.Errorf("capacity mode cannot use vars, sets, or evict_costs; define memory and evictCost on model stanzas")
	}

	for modelID, model := range models {
		if model.Memory <= 0 {
			return fmt.Errorf("model %s memory must be a positive integer when matrix.capacity is set", modelID)
		}
		if model.EvictCost != nil && *model.EvictCost < 0 {
			return fmt.Errorf("model %s evictCost must be >= 0", modelID)
		}
	}

	// A capacity matrix has no DSL program. Clear a previously compiled program
	// in case callers reuse a MatrixConfig value before validating it again.
	matrix.program = nil
	return nil
}

// CapacityMode reports whether this matrix uses the capacity solver instead of
// the set-expression program.
func (m *MatrixConfig) CapacityMode() bool {
	return m != nil && m.Capacity > 0
}

func resolveMatrixModel(ident string, vars map[string]string, models map[string]ModelConfig) (string, bool) {
	if modelName, ok := vars[ident]; ok {
		return modelName, true
	}
	if _, ok := models[ident]; ok {
		return ident, true
	}
	return "", false
}

// ResolvedEvictCosts returns a map of real model name -> evict cost,
// resolving var IDs. Models not listed default to 1.
func (m *MatrixConfig) ResolvedEvictCosts() map[string]int {
	costs := make(map[string]int)
	if m.EvictCosts == nil {
		return costs
	}
	for key, cost := range m.EvictCosts {
		// Resolve var ID if present
		if realName, ok := m.Var[key]; ok {
			costs[realName] = cost
		} else {
			costs[key] = cost
		}
	}
	return costs
}

// Program returns the immutable compiled matrix program.
func (m *MatrixConfig) Program() *matrixdsl.Program {
	return m.program
}
