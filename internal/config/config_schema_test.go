package config

import (
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/google/jsonschema-go/jsonschema"
	"gopkg.in/yaml.v3"
)

// TestConfig_ExampleMatchesSchema validates that config.example.yaml conforms to
// config-schema.json. Both files live at the repository root.
func TestConfig_ExampleMatchesSchema(t *testing.T) {
	const examplePath = "../../config.example.yaml"

	exampleBytes, err := os.ReadFile(examplePath)
	if err != nil {
		t.Fatalf("reading %s: %v", examplePath, err)
	}

	if err := resolveConfigSchema(t).Validate(configSchemaInstance(t, exampleBytes)); err != nil {
		t.Fatalf("config.example.yaml does not match config-schema.json:\n%v", err)
	}
}

func TestConfigSchema_CapacityRequiresModelMemory(t *testing.T) {
	tests := []struct {
		name    string
		yaml    string
		wantErr bool
	}{
		{
			name: "legacy capacity missing memory",
			yaml: `
models:
  a:
    cmd: echo a
    memory: 10
  b:
    cmd: echo b
matrix:
  capacity: 20
`,
			wantErr: true,
		},
		{
			name: "legacy capacity all models have memory",
			yaml: `
models:
  a:
    cmd: echo a
    memory: 10
  b:
    cmd: echo b
    memory: 10
matrix:
  capacity: 20
`,
		},
		{
			name: "canonical active capacity missing memory",
			yaml: `
models:
  a:
    cmd: echo a
routing:
  router:
    use: matrix
    settings:
      matrix:
        capacity: 20
`,
			wantErr: true,
		},
		{
			name: "canonical active capacity all models have memory",
			yaml: `
models:
  a:
    cmd: echo a
    memory: 10
routing:
  router:
    use: matrix
    settings:
      matrix:
        capacity: 20
`,
		},
		{
			name: "canonical inactive capacity does not require memory",
			yaml: `
models:
  a:
    cmd: echo a
routing:
  router:
    use: group
    settings:
      matrix:
        capacity: 20
`,
		},
		{
			name: "canonical set matrix does not require memory",
			yaml: `
models:
  a:
    cmd: echo a
routing:
  router:
    use: matrix
    settings:
      matrix:
        sets:
          default: a
`,
		},
	}

	resolved := resolveConfigSchema(t)
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := resolved.Validate(configSchemaInstance(t, []byte(tt.yaml)))
			if tt.wantErr && err == nil {
				t.Fatal("schema validation succeeded, want missing memory error")
			}
			if tt.wantErr && !strings.Contains(err.Error(), "memory") {
				t.Fatalf("schema validation failed for an unexpected reason: %v", err)
			}
			if !tt.wantErr && err != nil {
				t.Fatalf("schema validation failed: %v", err)
			}
		})
	}
}

func resolveConfigSchema(t *testing.T) *jsonschema.Resolved {
	t.Helper()

	const schemaPath = "../../config-schema.json"
	schemaBytes, err := os.ReadFile(schemaPath)
	if err != nil {
		t.Fatalf("reading %s: %v", schemaPath, err)
	}

	var schema jsonschema.Schema
	if err := json.Unmarshal(schemaBytes, &schema); err != nil {
		t.Fatalf("unmarshalling schema: %v", err)
	}

	resolved, err := schema.Resolve(&jsonschema.ResolveOptions{
		BaseURI: "https://github.com/mostlygeek/llama-swap/",
	})
	if err != nil {
		t.Fatalf("resolving schema: %v", err)
	}
	return resolved
}

func configSchemaInstance(t *testing.T, yamlBytes []byte) any {
	t.Helper()

	// Convert YAML to a JSON-like value so numbers and keys match what the
	// validator expects.
	var yamlValue any
	if err := yaml.Unmarshal(yamlBytes, &yamlValue); err != nil {
		t.Fatalf("unmarshalling config yaml: %v", err)
	}
	jsonBytes, err := json.Marshal(yamlValue)
	if err != nil {
		t.Fatalf("converting config to json: %v", err)
	}
	var instance any
	if err := json.Unmarshal(jsonBytes, &instance); err != nil {
		t.Fatalf("unmarshalling config json: %v", err)
	}
	return instance
}
