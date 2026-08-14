package config

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func makeModels(names ...string) map[string]ModelConfig {
	m := make(map[string]ModelConfig)
	for _, name := range names {
		m[name] = ModelConfig{Cmd: "echo " + name}
	}
	return m
}

func TestValidateMatrix_Basic(t *testing.T) {
	models := makeModels("gemma", "qwen", "mistral", "voxtral", "llama70B")

	matrix := MatrixConfig{
		Var: map[string]string{
			"g": "gemma",
			"q": "qwen",
			"m": "mistral",
			"v": "voxtral",
			"L": "llama70B",
		},
		EvictCosts: map[string]int{
			"L": 30,
			"v": 50,
		},
		Sets: OrderedSets{
			{Name: "standard", DSL: "(g | q | m) & v"},
			{Name: "full", DSL: "L"},
		},
	}

	err := ValidateMatrix(&matrix, models)
	require.NoError(t, err)

	result := matrix.Program().Solve("gemma", []string{"voxtral"}, matrix.ResolvedEvictCosts())
	assert.Equal(t, "standard", result.SetName)
	assert.Equal(t, []string{"gemma", "voxtral"}, result.TargetSet)
	assert.Empty(t, result.Evict)

	result = matrix.Program().Solve("llama70B", []string{"voxtral"}, matrix.ResolvedEvictCosts())
	assert.Equal(t, "full", result.SetName)
	assert.Equal(t, []string{"llama70B"}, result.TargetSet)
	assert.Equal(t, []string{"voxtral"}, result.Evict)
}

func TestValidateMatrix_WithRef(t *testing.T) {
	models := makeModels("gemma", "qwen", "mistral", "voxtral", "reranker")

	matrix := MatrixConfig{
		Var: map[string]string{
			"g": "gemma",
			"q": "qwen",
			"m": "mistral",
			"v": "voxtral",
			"e": "reranker",
		},
		Sets: OrderedSets{
			{Name: "llms", DSL: "g | q | m"},
			{Name: "with_tts", DSL: "+llms & v"},
			{Name: "mega", DSL: "+with_tts & e"},
		},
	}

	err := ValidateMatrix(&matrix, models)
	require.NoError(t, err)

	result := matrix.Program().Solve("reranker", []string{"gemma", "voxtral"}, nil)
	assert.Equal(t, "mega", result.SetName)
	assert.Equal(t, []string{"gemma", "reranker", "voxtral"}, result.TargetSet)
	assert.Empty(t, result.Evict)
}

func TestValidateMatrix_DirectAndMixedModelNames(t *testing.T) {
	models := makeModels("gemma", "qwen", "voxtral")

	t.Run("direct model names without vars", func(t *testing.T) {
		matrix := MatrixConfig{
			Sets: OrderedSets{{Name: "combo", DSL: "gemma & voxtral"}},
		}

		err := ValidateMatrix(&matrix, models)
		require.NoError(t, err)
		result := matrix.Program().Solve("gemma", []string{"voxtral"}, nil)
		assert.Equal(t, []string{"gemma", "voxtral"}, result.TargetSet)
	})

	t.Run("mixed vars and model names", func(t *testing.T) {
		matrix := MatrixConfig{
			Var: map[string]string{"g": "gemma"},
			Sets: OrderedSets{
				{Name: "base", DSL: "g | qwen"},
				{Name: "combo", DSL: "+base & voxtral"},
			},
		}

		err := ValidateMatrix(&matrix, models)
		require.NoError(t, err)
		result := matrix.Program().Solve("qwen", []string{"voxtral"}, nil)
		assert.Equal(t, "combo", result.SetName)
		assert.Equal(t, []string{"qwen", "voxtral"}, result.TargetSet)
	})

	t.Run("deduplicates after resolution", func(t *testing.T) {
		matrix := MatrixConfig{
			Var:  map[string]string{"g": "gemma"},
			Sets: OrderedSets{{Name: "combo", DSL: "g & gemma"}},
		}

		err := ValidateMatrix(&matrix, models)
		require.NoError(t, err)
		result := matrix.Program().Solve("gemma", nil, nil)
		assert.Equal(t, []string{"gemma"}, result.TargetSet)
	})

	t.Run("vars take precedence over model names", func(t *testing.T) {
		matrix := MatrixConfig{
			Var:  map[string]string{"qwen": "gemma"},
			Sets: OrderedSets{{Name: "combo", DSL: "qwen"}},
		}

		err := ValidateMatrix(&matrix, models)
		require.NoError(t, err)
		result := matrix.Program().Solve("gemma", nil, nil)
		assert.Equal(t, []string{"gemma"}, result.TargetSet)
	})
}

func TestValidateMatrix_VarKeyValidation(t *testing.T) {
	models := makeModels("gemma")

	valid := []string{
		"abcdefghijklmnopqrstuvwxyzABCDEF",
		"model-var",
		"model.var",
	}
	for _, key := range valid {
		t.Run("valid "+key, func(t *testing.T) {
			matrix := MatrixConfig{
				Var:  map[string]string{key: "gemma"},
				Sets: OrderedSets{{Name: "s", DSL: key}},
			}
			err := ValidateMatrix(&matrix, models)
			require.NoError(t, err)
		})
	}

	tests := []struct {
		name   string
		alias  string
		errMsg string
	}{
		{"too long", "abcdefghijklmnopqrstuvwxyzABCDEFG", "1-32 characters"},
		{"has underscore", "a_b", "only alphanumeric, '-' or '.'"},
		{"empty", "", "1-32 characters"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			matrix := MatrixConfig{
				Var:  map[string]string{tt.alias: "gemma"},
				Sets: OrderedSets{{Name: "s", DSL: tt.alias}},
			}
			err := ValidateMatrix(&matrix, models)
			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.errMsg)
		})
	}
}

func TestValidateMatrix_AliasReferencesUnknownModel(t *testing.T) {
	models := makeModels("gemma")

	matrix := MatrixConfig{
		Var:  map[string]string{"x": "nonexistent"},
		Sets: OrderedSets{{Name: "s", DSL: "x"}},
	}

	err := ValidateMatrix(&matrix, models)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unknown model")
}

func TestValidateMatrix_EvictCostInvalid(t *testing.T) {
	models := makeModels("gemma")

	t.Run("zero cost", func(t *testing.T) {
		matrix := MatrixConfig{
			Var:        map[string]string{"g": "gemma"},
			EvictCosts: map[string]int{"g": 0},
			Sets:       OrderedSets{{Name: "s", DSL: "g"}},
		}
		err := ValidateMatrix(&matrix, models)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "positive integer")
	})

	t.Run("negative cost", func(t *testing.T) {
		matrix := MatrixConfig{
			Var:        map[string]string{"g": "gemma"},
			EvictCosts: map[string]int{"g": -1},
			Sets:       OrderedSets{{Name: "s", DSL: "g"}},
		}
		err := ValidateMatrix(&matrix, models)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "positive integer")
	})

	t.Run("unknown var or model in evict_costs", func(t *testing.T) {
		matrix := MatrixConfig{
			Var:        map[string]string{"g": "gemma"},
			EvictCosts: map[string]int{"unknown": 5},
			Sets:       OrderedSets{{Name: "s", DSL: "g"}},
		}
		err := ValidateMatrix(&matrix, models)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "unknown var or model")
	})

	t.Run("direct model name in evict_costs", func(t *testing.T) {
		matrix := MatrixConfig{
			EvictCosts: map[string]int{"gemma": 5},
			Sets:       OrderedSets{{Name: "s", DSL: "gemma"}},
		}
		err := ValidateMatrix(&matrix, models)
		require.NoError(t, err)
	})
}

func TestValidateMatrix_CycleDetection(t *testing.T) {
	models := makeModels("gemma")

	matrix := MatrixConfig{
		Var: map[string]string{"g": "gemma"},
		Sets: OrderedSets{
			{Name: "a", DSL: "+b"},
			{Name: "b", DSL: "+a"},
		},
	}

	err := ValidateMatrix(&matrix, models)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "circular reference")
}

func TestValidateMatrix_UndefinedRefTarget(t *testing.T) {
	models := makeModels("gemma")

	matrix := MatrixConfig{
		Var: map[string]string{"g": "gemma"},
		Sets: OrderedSets{
			{Name: "a", DSL: "+nonexistent"},
		},
	}

	err := ValidateMatrix(&matrix, models)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "references undefined set")
}

func TestValidateMatrix_NoSets(t *testing.T) {
	matrix := MatrixConfig{}
	err := ValidateMatrix(&matrix, makeModels("gemma"))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "at least one set")
}

func TestValidateMatrix_CapacityMode(t *testing.T) {
	models := map[string]ModelConfig{
		"gemma": {Cmd: "echo gemma", Memory: 18},
		"qwen":  {Cmd: "echo qwen", Memory: 41, EvictCost: new(0)},
	}
	matrix := MatrixConfig{Capacity: 40, Strategy: "lru"}

	err := ValidateMatrix(&matrix, models)
	require.NoError(t, err)
	assert.True(t, matrix.CapacityMode())
	assert.Nil(t, matrix.Program())
}

func TestValidateMatrix_CapacityModeValidation(t *testing.T) {
	validModels := map[string]ModelConfig{"gemma": {Memory: 10}}
	tests := []struct {
		name    string
		matrix  MatrixConfig
		models  map[string]ModelConfig
		message string
	}{
		{
			name:    "negative capacity",
			matrix:  MatrixConfig{Capacity: -1},
			models:  validModels,
			message: "capacity must be a positive integer",
		},
		{
			name:    "strategy requires capacity",
			matrix:  MatrixConfig{Strategy: "lru", Sets: OrderedSets{{Name: "s", DSL: "gemma"}}},
			models:  validModels,
			message: "strategy requires capacity",
		},
		{
			name:    "unknown strategy",
			matrix:  MatrixConfig{Capacity: 40, Strategy: "random"},
			models:  validModels,
			message: "strategy must be one of",
		},
		{
			name:    "missing model memory",
			matrix:  MatrixConfig{Capacity: 40},
			models:  makeModels("gemma"),
			message: "memory must be a positive integer",
		},
		{
			name:    "negative eviction cost",
			matrix:  MatrixConfig{Capacity: 40},
			models:  map[string]ModelConfig{"gemma": {Memory: 10, EvictCost: new(-1)}},
			message: "evictCost must be >= 0",
		},
		{
			name: "cannot mix DSL fields",
			matrix: MatrixConfig{
				Capacity: 40,
				Sets:     OrderedSets{{Name: "s", DSL: "gemma"}},
			},
			models:  validModels,
			message: "capacity mode cannot use",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidateMatrix(&tt.matrix, tt.models)
			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.message)
		})
	}
}

func TestValidateMatrix_UnknownMapIDInDSL(t *testing.T) {
	models := makeModels("gemma")

	matrix := MatrixConfig{
		Var: map[string]string{"g": "gemma"},
		Sets: OrderedSets{
			{Name: "s", DSL: "g & nonexistent"},
		},
	}

	err := ValidateMatrix(&matrix, models)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unknown var or model")
}

func TestValidateMatrix_ResolvedEvictCosts(t *testing.T) {
	mc := &MatrixConfig{
		Var: map[string]string{
			"g": "gemma",
			"L": "llama70B",
		},
		EvictCosts: map[string]int{
			"L":       30,
			"g":       5,
			"mistral": 10,
		},
	}

	costs := mc.ResolvedEvictCosts()
	assert.Equal(t, 30, costs["llama70B"])
	assert.Equal(t, 5, costs["gemma"])
	assert.Equal(t, 10, costs["mistral"])
}

func TestValidateMatrix_ConfigXOR(t *testing.T) {
	// groups and matrix both defined
	yaml := `
models:
  model1:
    cmd: echo model1
    proxy: http://localhost:8080
groups:
  group1:
    members:
      - model1
matrix:
  sets:
    s: "model1"
`
	_, err := LoadConfigFromReader(strings.NewReader(yaml))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "cannot use both")
}

func TestValidateMatrix_ConfigMatrixOnly(t *testing.T) {
	yaml := `
models:
  gemma:
    cmd: echo gemma
    proxy: http://localhost:8080
  qwen:
    cmd: echo qwen
    proxy: http://localhost:8081
matrix:
  vars:
    g: gemma
    q: qwen
  sets:
    combo: "g | q"
`
	cfg, err := LoadConfigFromReader(strings.NewReader(yaml))
	require.NoError(t, err)
	assert.NotNil(t, cfg.Matrix)
	assert.NotNil(t, cfg.Matrix.Program())
	assert.Equal(t, "matrix", cfg.Routing.Router.Use)
	assert.Same(t, cfg.Matrix.Program(), cfg.Routing.Router.Settings.Matrix.Program())
	// Groups should be empty when matrix is used
	assert.Empty(t, cfg.Groups)
}

func TestValidateMatrix_ConfigCapacityMode(t *testing.T) {
	yaml := `
models:
  gemma:
    cmd: echo gemma ${PORT}
    memory: 18
  qwen:
    cmd: echo qwen ${PORT}
    memory: 22
    evictCost: 0
routing:
  router:
    use: matrix
    settings:
      matrix:
        capacity: 40
        strategy: lru
`
	cfg, err := LoadConfigFromReader(strings.NewReader(yaml))
	require.NoError(t, err)
	require.NotNil(t, cfg.Routing.Router.Settings.Matrix)
	assert.Equal(t, 40, cfg.Routing.Router.Settings.Matrix.Capacity)
	assert.Equal(t, "lru", cfg.Routing.Router.Settings.Matrix.Strategy)
	assert.Equal(t, 18, cfg.Models["gemma"].Memory)
	require.NotNil(t, cfg.Models["qwen"].EvictCost)
	assert.Zero(t, *cfg.Models["qwen"].EvictCost)
}
