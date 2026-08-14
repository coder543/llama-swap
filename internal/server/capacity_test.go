package server

import (
	"encoding/json"
	"testing"

	"github.com/mostlygeek/llama-swap/internal/config"
	"github.com/mostlygeek/llama-swap/internal/process"
)

func TestServer_CapacityTotal(t *testing.T) {
	for _, test := range []struct {
		name   string
		router string
		matrix *config.MatrixConfig
		want   int
	}{
		{name: "capacity router", router: "matrix", matrix: &config.MatrixConfig{Capacity: 115}, want: 115},
		{name: "inactive capacity settings", router: "group", matrix: &config.MatrixConfig{Capacity: 115}},
		{name: "set-expression router", router: "matrix", matrix: &config.MatrixConfig{}},
		{name: "no matrix", router: "matrix"},
		{name: "default router"},
	} {
		t.Run(test.name, func(t *testing.T) {
			s := &Server{cfg: config.Config{Routing: config.RoutingConfig{Router: config.RouterConfig{
				Use: test.router, Settings: config.RouterSettings{Matrix: test.matrix},
			}}}}
			if got := s.capacityTotal(); got != test.want {
				t.Fatalf("capacityTotal() = %d, want %d", got, test.want)
			}
		})
	}
}

func TestServer_ModelStatus_MemoryReservations(t *testing.T) {
	local := newStubRouter([]string{"ready", "loading", "idle"}, "")
	local.running = map[string]process.ProcessState{"ready": process.StateReady, "loading": process.StateStarting}
	s := newTestServer(local, newStubRouter(nil, ""))
	s.cfg.Models = map[string]config.ModelConfig{
		"ready": {Memory: 24, Unlisted: true}, "loading": {Memory: 12}, "idle": {Memory: 8},
	}
	s.cfg.Peers = config.PeerDictionaryConfig{"peer": {Models: []string{"remote"}}}
	s.cfg.Routing.Router = config.RouterConfig{Use: "matrix", Settings: config.RouterSettings{
		Matrix: &config.MatrixConfig{Capacity: 115},
	}}
	want := map[string]int{"ready": 24, "loading": 12, "idle": 8, "peer/remote": 0}
	for _, model := range s.modelStatus() {
		if model.Memory != want[model.Id] {
			t.Errorf("%s memory = %d, want %d", model.Id, model.Memory, want[model.Id])
		}
		delete(want, model.Id)
	}
	if len(want) > 0 {
		t.Fatalf("models missing from status: %v", want)
	}

	s.cfg.Routing.Router.Use = "group"
	for _, model := range s.modelStatus() {
		encoded, err := json.Marshal(model)
		if err != nil {
			t.Fatal(err)
		}
		var record map[string]any
		if err := json.Unmarshal(encoded, &record); err != nil {
			t.Fatal(err)
		}
		if _, found := record["memory"]; found {
			t.Errorf("inactive capacity reservation exposed for %s", model.Id)
		}
	}
}
