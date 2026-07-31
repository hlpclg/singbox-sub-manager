package nodes

import (
	"fmt"
	"strconv"
	"strings"
)

func parseLegacy(lines []string) ([]Node, error) {
	var out []Node
	seen := map[string]bool{}
	for i, raw := range lines {
		lineNo := i + 1
		if !isMeaningful(raw) {
			continue
		}
		line := strings.TrimSpace(raw)
		if strings.HasPrefix(line, "[") {
			return nil, fmt.Errorf("line %d: mixed legacy and sectioned format", lineNo)
		}
		parts := strings.Split(line, "|")
		if len(parts) != 6 {
			return nil, fmt.Errorf("line %d: expected 6 fields", lineNo)
		}
		for j := range parts {
			parts[j] = strings.TrimSpace(parts[j])
		}
		port, err := strconv.Atoi(parts[2])
		if err != nil {
			return nil, fmt.Errorf("line %d: invalid port", lineNo)
		}
		n := Node{Name: parts[0], Server: parts[1], Port: port, Password: parts[3], ObfsPassword: parts[4], SNI: parts[5], Enabled: true}
		if seen[n.Name] {
			return nil, fmt.Errorf("line %d: duplicate node name %q", lineNo, n.Name)
		}
		if err := validateFields(n); err != nil {
			return nil, fmt.Errorf("line %d: %w", lineNo, err)
		}
		seen[n.Name] = true
		out = append(out, n)
	}
	return out, nil
}

var sectionKeys = map[string]bool{
	"SERVER": true, "PORT": true, "PASSWORD": true,
	"OBFS_PASSWORD": true, "SNI": true, "ENABLED": true,
}

func parseSectioned(lines []string) ([]Node, error) {
	var out []Node
	seen := map[string]bool{}
	var cur *Node
	var curPort string
	var curEnabled string

	flush := func(lineNo int) error {
		if cur == nil {
			return nil
		}
		if curPort == "" {
			return fmt.Errorf("node %q: missing PORT", cur.Name)
		}
		port, err := strconv.Atoi(curPort)
		if err != nil {
			return fmt.Errorf("node %q: invalid port %q", cur.Name, curPort)
		}
		cur.Port = port
		switch {
		case curEnabled == "":
			cur.Enabled = true // absent defaults to enabled
		case strings.EqualFold(curEnabled, "true"):
			cur.Enabled = true
		case strings.EqualFold(curEnabled, "false"):
			cur.Enabled = false
		default:
			return fmt.Errorf("node %q: invalid ENABLED value %q (want true or false)", cur.Name, curEnabled)
		}
		if err := validateFields(*cur); err != nil {
			return err
		}
		out = append(out, *cur)
		cur, curPort, curEnabled = nil, "", ""
		return nil
	}

	for i, raw := range lines {
		lineNo := i + 1
		if !isMeaningful(raw) {
			continue
		}
		line := strings.TrimSpace(raw)
		if strings.HasPrefix(line, "[") {
			if !strings.HasSuffix(line, "]") {
				return nil, fmt.Errorf("line %d: malformed section header", lineNo)
			}
			if err := flush(lineNo); err != nil {
				return nil, err
			}
			name := strings.TrimSpace(line[1 : len(line)-1])
			if seen[name] {
				return nil, fmt.Errorf("line %d: duplicate node name %q", lineNo, name)
			}
			seen[name] = true
			cur = &Node{Name: name}
			continue
		}
		if cur == nil {
			return nil, fmt.Errorf("line %d: key/value outside a [section]", lineNo)
		}
		key, val, ok := strings.Cut(line, "=")
		if !ok {
			return nil, fmt.Errorf("line %d: expected KEY=VALUE", lineNo)
		}
		key = strings.TrimSpace(key)
		val = strings.TrimSpace(val)
		if !sectionKeys[key] {
			return nil, fmt.Errorf("line %d: unknown key %q", lineNo, key)
		}
		switch key {
		case "SERVER":
			cur.Server = val
		case "PORT":
			curPort = val
		case "PASSWORD":
			cur.Password = val
		case "OBFS_PASSWORD":
			cur.ObfsPassword = val
		case "SNI":
			cur.SNI = val
		case "ENABLED":
			curEnabled = val
		}
	}
	if err := flush(len(lines)); err != nil {
		return nil, err
	}
	return out, nil
}
