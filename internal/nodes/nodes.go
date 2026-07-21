package nodes

import (
	"bufio"
	"fmt"
	"os"
	"strconv"
	"strings"
)

type Node struct {
	Name         string
	Server       string
	Port         int
	Password     string
	ObfsPassword string
	SNI          string
}

func ParseFile(path string) ([]Node, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	var out []Node
	s := bufio.NewScanner(f)
	lineNo := 0
	seen := map[string]bool{}
	for s.Scan() {
		lineNo++
		line := strings.TrimSpace(s.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		parts := strings.Split(line, "|")
		if len(parts) != 6 {
			return nil, fmt.Errorf("line %d: expected 6 fields", lineNo)
		}
		for i := range parts {
			parts[i] = strings.TrimSpace(parts[i])
		}
		port, err := strconv.Atoi(parts[2])
		if err != nil || port < 1 || port > 65535 {
			return nil, fmt.Errorf("line %d: invalid port", lineNo)
		}
		if seen[parts[0]] {
			return nil, fmt.Errorf("line %d: duplicate node name %q", lineNo, parts[0])
		}
		if parts[0] == "" || parts[1] == "" || parts[3] == "" || parts[4] == "" || parts[5] == "" {
			return nil, fmt.Errorf("line %d: empty field", lineNo)
		}
		if strings.Contains(parts[3], "CHANGE_ME") || strings.Contains(parts[4], "CHANGE_ME") {
			return nil, fmt.Errorf("line %d: replace placeholder secrets", lineNo)
		}
		seen[parts[0]] = true
		out = append(out, Node{Name: parts[0], Server: parts[1], Port: port, Password: parts[3], ObfsPassword: parts[4], SNI: parts[5]})
	}
	if err := s.Err(); err != nil {
		return nil, err
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("no nodes found")
	}
	return out, nil
}
