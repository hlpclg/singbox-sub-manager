//go:build reality_integration

// This file implements review finding F7's second requirement: bind the
// golden fixture (config_test.go's goldenServerConfigParams,
// testdata/probe/server_config.golden.json) to Task 1's real,
// sing-box-1.14.1-verified structure (probe_integration_test.go's
// renderServerConfig), without modifying that existing test file. It
// needs no network access — it only compares two in-process JSON
// structures — and runs standalone with:
//
//	go test -tags reality_integration -run TestGenerateServerConfigMatchesTask1Structure ./internal/realitynode
package realitynode

import (
	"encoding/json"
	"reflect"
	"testing"
)

// TestGenerateServerConfigMatchesTask1Structure asserts that
// GenerateServerConfig's production output is structurally identical
// (through a JSON round trip, so Go/JSON type differences like int vs
// float64 don't produce false mismatches) to probe_integration_test.go's
// renderServerConfig — the function Task 1 used to real-verify this exact
// shape against sing-box 1.14.1 (docs/reality-node-validation.md) — for
// the same parameters.
func TestGenerateServerConfigMatchesTask1Structure(t *testing.T) {
	p := goldenServerConfigParams()

	got, err := GenerateServerConfig(p)
	if err != nil {
		t.Fatalf("GenerateServerConfig: %v", err)
	}
	var gotGeneric interface{}
	if err := json.Unmarshal(got, &gotGeneric); err != nil {
		t.Fatalf("unmarshal GenerateServerConfig output: %v", err)
	}

	want := renderServerConfig(serverConfigParams{
		ListenPort:    p.ListenPort,
		UUID:          p.UUID,
		PrivateKey:    p.PrivateKey,
		ShortID:       p.ShortID,
		SNI:           p.SNI,
		SelfcheckPort: p.SelfcheckPort,
		LocalCIDRs:    p.LocalCIDRs,
	})
	wantBytes, err := json.Marshal(want)
	if err != nil {
		t.Fatalf("marshal renderServerConfig output: %v", err)
	}
	var wantGeneric interface{}
	if err := json.Unmarshal(wantBytes, &wantGeneric); err != nil {
		t.Fatalf("unmarshal renderServerConfig output: %v", err)
	}

	if !reflect.DeepEqual(gotGeneric, wantGeneric) {
		t.Fatalf("GenerateServerConfig does not structurally match Task 1's real-verified renderServerConfig.\n--- got ---\n%s\n--- want ---\n%s", got, wantBytes)
	}
}
