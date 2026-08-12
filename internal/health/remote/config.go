package remote

import (
	"encoding/json"
	"github.com/hlpclg/singbox-sub-manager/internal/nodes"
)

func GenerateConfig(n nodes.Node, listenPort int) ([]byte, error) {
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
		"outbounds": []interface{}{
			map[string]interface{}{
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
			},
		},
	}
	return json.MarshalIndent(cfg, "", "  ")
}
