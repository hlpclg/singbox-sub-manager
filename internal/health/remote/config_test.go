package remote

import (
	"encoding/json"
	"github.com/hlpclg/singbox-sub-manager/internal/nodes"
	"testing"
)

func TestGenerateConfig(t *testing.T) {
	node := nodes.Node{
		Name:         "test-node",
		Server:       "1.2.3.4",
		Port:         443,
		Password:     "pass",
		ObfsPassword: "obfs",
		SNI:          "sni.com",
	}
	port := 10800
	cfgData, err := GenerateConfig(node, port)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var parsed map[string]interface{}
	if err := json.Unmarshal(cfgData, &parsed); err != nil {
		t.Fatalf("invalid json: %v", err)
	}

	// Ensure log level is error or fatal
	logMap, _ := parsed["log"].(map[string]interface{})
	if logMap["level"] != "error" && logMap["level"] != "fatal" {
		t.Errorf("expected log level error or fatal, got %v", logMap["level"])
	}

	// Verify inbound
	inbounds, _ := parsed["inbounds"].([]interface{})
	if len(inbounds) == 0 {
		t.Fatalf("no inbounds")
	}
	inbound := inbounds[0].(map[string]interface{})
	if inbound["type"] != "mixed" || int(inbound["listen_port"].(float64)) != port {
		t.Errorf("inbound mismatch: %v", inbound)
	}

	// Verify outbound
	outbounds, _ := parsed["outbounds"].([]interface{})
	if len(outbounds) == 0 {
		t.Fatalf("no outbounds")
	}
	outbound := outbounds[0].(map[string]interface{})
	if outbound["type"] != "hysteria2" || outbound["server"] != "1.2.3.4" {
		t.Errorf("outbound mismatch: %v", outbound)
	}
}
