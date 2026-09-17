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
		n := Node{Name: parts[0], Type: TypeHysteria2, Server: parts[1], Port: port, Password: parts[3], ObfsPassword: parts[4], SNI: parts[5], Enabled: true}
		if seen[n.Name] {
			return nil, fmt.Errorf("line %d: duplicate node name %q", lineNo, n.Name)
		}
		if err := Validate(n); err != nil {
			return nil, fmt.Errorf("line %d: %w", lineNo, err)
		}
		seen[n.Name] = true
		out = append(out, n)
	}
	return out, nil
}

var commonKeys = map[string]bool{"SERVER": true, "PORT": true, "SNI": true, "ENABLED": true}
var hysteria2Keys = map[string]bool{"PASSWORD": true, "OBFS_PASSWORD": true}
var vlessRealityKeys = map[string]bool{"UUID": true, "PUBLIC_KEY": true, "SHORT_ID": true}

func typeSpecificKeys(t NodeType) map[string]bool {
	if t == TypeVlessReality {
		return vlessRealityKeys
	}
	return hysteria2Keys
}

func parseSectioned(lines []string) ([]Node, error) {
	var out []Node
	seenNames := map[string]bool{}
	var cur *Node
	var curPort string
	var curEnabled string
	var curType NodeType
	var typeKnown bool
	var keysSeen map[string]bool
	var atFirstKey bool

	flush := func(lineNo int) error {
		if cur == nil {
			return nil
		}
		if !typeKnown {
			return fmt.Errorf("line %d: section %q has no keys", lineNo, cur.Name)
		}
		cur.Type = curType
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
		if err := Validate(*cur); err != nil {
			return err
		}
		out = append(out, *cur)
		cur, curPort, curEnabled = nil, "", ""
		curType, typeKnown, keysSeen, atFirstKey = "", false, nil, false
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
			if seenNames[name] {
				return nil, fmt.Errorf("line %d: duplicate node name %q", lineNo, name)
			}
			seenNames[name] = true
			cur = &Node{Name: name}
			keysSeen = map[string]bool{}
			atFirstKey = true
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

		if atFirstKey {
			atFirstKey = false
			if key == "TYPE" {
				switch NodeType(val) {
				case TypeHysteria2, TypeVlessReality:
					curType = NodeType(val)
				case "":
					return nil, fmt.Errorf("line %d: empty TYPE value", lineNo)
				default:
					return nil, fmt.Errorf("line %d: unknown TYPE %q", lineNo, val)
				}
				typeKnown = true
				keysSeen["TYPE"] = true
				continue
			}
			curType = TypeHysteria2
			typeKnown = true
			// fall through: this line's key is handled by the generic
			// dispatch below, same as any other implicit-mode key.
		}

		if key == "TYPE" {
			if keysSeen["TYPE"] {
				return nil, fmt.Errorf("line %d: duplicate key %q in section %q", lineNo, "TYPE", cur.Name)
			}
			return nil, fmt.Errorf("line %d: TYPE must be the first key in section %q", lineNo, cur.Name)
		}
		if keysSeen[key] {
			return nil, fmt.Errorf("line %d: duplicate key %q in section %q", lineNo, key, cur.Name)
		}
		if !commonKeys[key] && !typeSpecificKeys(curType)[key] {
			return nil, fmt.Errorf("line %d: unknown key %q for type %q", lineNo, key, curType)
		}
		keysSeen[key] = true

		switch key {
		case "SERVER":
			cur.Server = val
		case "PORT":
			curPort = val
		case "SNI":
			cur.SNI = val
		case "ENABLED":
			curEnabled = val
		case "PASSWORD":
			cur.Password = val
		case "OBFS_PASSWORD":
			cur.ObfsPassword = val
		case "UUID":
			cur.UUID = val
		case "PUBLIC_KEY":
			cur.PublicKey = val
		case "SHORT_ID":
			cur.ShortID = val
		}
	}
	if err := flush(len(lines)); err != nil {
		return nil, err
	}
	return out, nil
}
