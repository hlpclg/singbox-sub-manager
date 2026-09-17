package nodes

import (
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"regexp"
	"strings"
)

type Node struct {
	Name    string
	Type    NodeType
	Server  string
	Port    int
	SNI     string
	Enabled bool

	Password     string // hysteria2-only
	ObfsPassword string // hysteria2-only

	UUID      string // vless-reality-only
	PublicKey string // vless-reality-only, Reality public key
	ShortID   string // vless-reality-only, may be empty
}

// NodeType discriminates which protocol-specific fields on Node are
// meaningful. The zero value "" is treated as TypeHysteria2 by
// Validate and by effectiveType, but never appears on disk after a
// write (see Serialize).
type NodeType string

const (
	TypeHysteria2    NodeType = "hysteria2"
	TypeVlessReality NodeType = "vless-reality"
)

// Reality* are the fixed rendering/probing parameters for VLESS
// Reality nodes. They are not configurable per node.
const (
	RealityFlow        = "xtls-rprx-vision"
	RealityFingerprint = "chrome"
	RealitySpiderX     = "/"
	RealityNetwork     = "tcp"
)

// effectiveType maps the zero value to TypeHysteria2; every other
// value (including unknown ones) passes through unchanged.
func effectiveType(t NodeType) NodeType {
	if t == "" {
		return TypeHysteria2
	}
	return t
}

type Format int

const (
	FormatEmpty Format = iota
	FormatLegacy
	FormatSectioned
)

var nameRE = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)
var uuidRE = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)
var shortIDRE = regexp.MustCompile(`^([0-9a-fA-F]{2}){1,8}$`)

// ValidateName enforces the node-name charset (usable as a section header and
// as a Clash proxy name).
func ValidateName(name string) error {
	if name == "" {
		return errors.New("node name is empty")
	}
	if !nameRE.MatchString(name) {
		return fmt.Errorf("invalid node name %q: allowed chars are letters, digits, '.', '_', '-'", name)
	}
	return nil
}

// Validate enforces, for n's effective type (empty Type is treated as
// hysteria2), that exactly one protocol's credential fields are
// populated and that those fields pass format checks. It is the
// single implementation of this rule; every construction, parse, and
// write path in this package and its callers must go through it.
func Validate(n Node) error {
	if err := ValidateName(n.Name); err != nil {
		return err
	}
	if n.Server == "" || n.SNI == "" {
		return fmt.Errorf("node %q: empty required field", n.Name)
	}
	if n.Port < 1 || n.Port > 65535 {
		return fmt.Errorf("node %q: invalid port %d", n.Name, n.Port)
	}

	typ := effectiveType(n.Type)

	type labeledField struct{ label, value string }
	changeMeFields := []labeledField{
		{"Name", n.Name},
		{"Server", n.Server},
		{"SNI", n.SNI},
	}
	switch typ {
	case TypeHysteria2:
		changeMeFields = append(changeMeFields,
			labeledField{"Password", n.Password},
			labeledField{"ObfsPassword", n.ObfsPassword},
		)
	case TypeVlessReality:
		changeMeFields = append(changeMeFields,
			labeledField{"UUID", n.UUID},
			labeledField{"PublicKey", n.PublicKey},
			labeledField{"ShortID", n.ShortID},
		)
	}
	for _, f := range changeMeFields {
		if strings.Contains(f.value, "CHANGE_ME") {
			return fmt.Errorf("node %q: replace placeholder secret in field %s (CHANGE_ME)", n.Name, f.label)
		}
	}

	switch typ {
	case TypeHysteria2:
		if n.Password == "" || n.ObfsPassword == "" {
			return fmt.Errorf("node %q: empty required field", n.Name)
		}
		if n.UUID != "" || n.PublicKey != "" || n.ShortID != "" {
			return fmt.Errorf("node %q: vless-reality fields not allowed on hysteria2 node", n.Name)
		}
	case TypeVlessReality:
		if n.UUID == "" || n.PublicKey == "" {
			return fmt.Errorf("node %q: empty required field", n.Name)
		}
		if n.Password != "" || n.ObfsPassword != "" {
			return fmt.Errorf("node %q: hysteria2 fields not allowed on vless-reality node", n.Name)
		}
		if !uuidRE.MatchString(n.UUID) {
			return fmt.Errorf("node %q: invalid UUID format", n.Name)
		}
		key, err := base64.RawURLEncoding.Strict().DecodeString(n.PublicKey)
		if err != nil || len(key) != 32 {
			return fmt.Errorf("node %q: invalid PublicKey (expected unpadded base64url, e.g. output of `sing-box generate reality-keypair`)", n.Name)
		}
		if n.ShortID != "" && !shortIDRE.MatchString(n.ShortID) {
			return fmt.Errorf("node %q: invalid ShortID (expected up to 16 hex characters)", n.Name)
		}
	default:
		return fmt.Errorf("node %q: unknown type %q", n.Name, n.Type)
	}

	return nil
}

// Load reads a node file in either legacy pipe or sectioned key=value format.
// A missing file or one with no meaningful content returns (nil, FormatEmpty, nil).
func Load(path string) ([]Node, Format, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, FormatEmpty, nil
		}
		return nil, FormatEmpty, err
	}
	lines := strings.Split(string(data), "\n")
	format := detectFormat(lines)
	switch format {
	case FormatEmpty:
		return nil, FormatEmpty, nil
	case FormatSectioned:
		ns, err := parseSectioned(lines)
		return ns, FormatSectioned, err
	default:
		ns, err := parseLegacy(lines)
		return ns, FormatLegacy, err
	}
}

// ParseFile keeps the strict behavior relied on by merge/validate: an empty set
// is an error.
func ParseFile(path string) ([]Node, error) {
	ns, _, err := Load(path)
	if err != nil {
		return nil, err
	}
	if len(ns) == 0 {
		return nil, errors.New("no nodes found")
	}
	return ns, nil
}

func isMeaningful(line string) bool {
	t := strings.TrimSpace(line)
	return t != "" && !strings.HasPrefix(t, "#")
}

func detectFormat(lines []string) Format {
	for _, l := range lines {
		if !isMeaningful(l) {
			continue
		}
		if strings.HasPrefix(strings.TrimSpace(l), "[") {
			return FormatSectioned
		}
		return FormatLegacy
	}
	return FormatEmpty
}
