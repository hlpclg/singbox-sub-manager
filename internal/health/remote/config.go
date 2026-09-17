package remote

import (
	"encoding/json"
	"github.com/hlpclg/singbox-sub-manager/internal/nodes"
)

func GenerateConfig(n nodes.Node, listenPort int) ([]byte, error) {
	if err := nodes.Validate(n); err != nil {
		return nil, err
	}

	var outbound map[string]interface{}
	switch n.Type {
	case nodes.TypeVlessReality:
		outbound = map[string]interface{}{
			"type":        "vless",
			"tag":         "proxy",
			"server":      n.Server,
			"server_port": n.Port,
			"uuid":        n.UUID,
			"flow":        nodes.RealityFlow,
			"tls": map[string]interface{}{
				"enabled":     true,
				"server_name": n.SNI,
				"utls": map[string]interface{}{
					"enabled":     true,
					"fingerprint": nodes.RealityFingerprint,
				},
				"reality": map[string]interface{}{
					"enabled":    true,
					"public_key": n.PublicKey,
					"short_id":   n.ShortID,
				},
			},
		}
	default:
		outbound = map[string]interface{}{
			"type":        "hysteria2",
			"tag":         "proxy",
			"server":      n.Server,
			"server_port": n.Port,
			"password":    n.Password,
			"obfs": map[string]interface{}{
				"type":     "salamander",
				"password": n.ObfsPassword,
			},
			"tls": map[string]interface{}{
				"enabled":     true,
				"server_name": n.SNI,
			},
		}
	}

	cfg := map[string]interface{}{
		"log": map[string]interface{}{
			"level": "error",
		},
		"inbounds": []interface{}{
			map[string]interface{}{
				"type":        "mixed",
				"tag":         "mixed-in",
				"listen":      "127.0.0.1",
				"listen_port": listenPort,
			},
		},
		"outbounds": []interface{}{outbound},
	}
	return json.MarshalIndent(cfg, "", "  ")
}
