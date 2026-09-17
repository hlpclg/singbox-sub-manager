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

func TestGenerateConfigVlessReality(t *testing.T) {
	node := nodes.Node{
		Name: "test-reality", Type: nodes.TypeVlessReality, Server: "1.2.3.4", Port: 443, SNI: "sni.com",
		UUID: "12345678-1234-1234-1234-123456789abc", PublicKey: "AAECAwQFBgcICQoLDA0ODxAREhMUFRYXGBkaGxwdHh8", ShortID: "0123456789abcdef",
	}
	cfgData, err := GenerateConfig(node, 10800)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var parsed map[string]interface{}
	if err := json.Unmarshal(cfgData, &parsed); err != nil {
		t.Fatalf("invalid json: %v", err)
	}
	outbounds, _ := parsed["outbounds"].([]interface{})
	if len(outbounds) == 0 {
		t.Fatalf("no outbounds")
	}
	outbound := outbounds[0].(map[string]interface{})

	if outbound["type"] != "vless" || outbound["server"] != "1.2.3.4" || outbound["uuid"] != node.UUID {
		t.Errorf("outbound basic fields mismatch: %v", outbound)
	}
	if outbound["flow"] != nodes.RealityFlow {
		t.Errorf("outbound flow = %v, want %v", outbound["flow"], nodes.RealityFlow)
	}
	if _, hasNetwork := outbound["network"]; hasNetwork {
		t.Errorf("outbound must not set \"network\" (see design §7): %v", outbound)
	}
	tls, _ := outbound["tls"].(map[string]interface{})
	if tls["enabled"] != true || tls["server_name"] != "sni.com" {
		t.Errorf("tls fields mismatch: %v", tls)
	}
	utls, _ := tls["utls"].(map[string]interface{})
	if utls["enabled"] != true || utls["fingerprint"] != nodes.RealityFingerprint {
		t.Errorf("utls fields mismatch: %v", utls)
	}
	reality, _ := tls["reality"].(map[string]interface{})
	if reality["enabled"] != true || reality["public_key"] != node.PublicKey || reality["short_id"] != node.ShortID {
		t.Errorf("reality fields mismatch: %v", reality)
	}
}

func TestGenerateConfigRejectsInvalidNode(t *testing.T) {
	node := nodes.Node{Name: "bad", Type: nodes.TypeVlessReality, Server: "1.2.3.4", Port: 443, SNI: "s"} // missing UUID/PublicKey
	if _, err := GenerateConfig(node, 10800); err == nil {
		t.Fatal("expected GenerateConfig to reject an invalid node")
	}
}
