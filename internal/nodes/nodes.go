package nodes

import (
	"errors"
	"fmt"
	"os"
	"regexp"
	"strings"
)

type Node struct {
	Name         string
	Server       string
	Port         int
	Password     string
	ObfsPassword string
	SNI          string
	Enabled      bool
}

type Format int

const (
	FormatEmpty Format = iota
	FormatLegacy
	FormatSectioned
)

var nameRE = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)

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

// validateFields runs the shared field checks used by both parsers.
func validateFields(n Node) error {
	if err := ValidateName(n.Name); err != nil {
		return err
	}
	if n.Server == "" || n.Password == "" || n.ObfsPassword == "" || n.SNI == "" {
		return fmt.Errorf("node %q: empty required field", n.Name)
	}
	if n.Port < 1 || n.Port > 65535 {
		return fmt.Errorf("node %q: invalid port %d", n.Name, n.Port)
	}
	if strings.Contains(n.Password, "CHANGE_ME") || strings.Contains(n.ObfsPassword, "CHANGE_ME") {
		return fmt.Errorf("node %q: replace placeholder secrets (CHANGE_ME)", n.Name)
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
