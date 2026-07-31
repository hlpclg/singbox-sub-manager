package nodes

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Serialize renders nodes in sectioned key=value format with a stable field order.
func Serialize(ns []Node) string {
	var b strings.Builder
	for i, n := range ns {
		if i > 0 {
			b.WriteByte('\n')
		}
		fmt.Fprintf(&b, "[%s]\n", n.Name)
		fmt.Fprintf(&b, "SERVER=%s\n", n.Server)
		fmt.Fprintf(&b, "PORT=%d\n", n.Port)
		fmt.Fprintf(&b, "PASSWORD=%s\n", n.Password)
		fmt.Fprintf(&b, "OBFS_PASSWORD=%s\n", n.ObfsPassword)
		fmt.Fprintf(&b, "SNI=%s\n", n.SNI)
		fmt.Fprintf(&b, "ENABLED=%t\n", n.Enabled)
	}
	return b.String()
}

// WriteFile atomically writes ns in sectioned format at 0600, backing up any
// existing file to path+".bak" first.
func WriteFile(path string, ns []Node) error {
	if existing, err := os.ReadFile(path); err == nil {
		if err := os.WriteFile(path+".bak", existing, 0600); err != nil {
			return fmt.Errorf("backup: %w", err)
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".nodes.*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if _, err := tmp.WriteString(Serialize(ns)); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmpName, 0600); err != nil {
		return err
	}
	return os.Rename(tmpName, path)
}

func Find(ns []Node, name string) (int, bool) {
	for i := range ns {
		if ns[i].Name == name {
			return i, true
		}
	}
	return -1, false
}

func Add(ns []Node, n Node) ([]Node, error) {
	if err := validateFields(n); err != nil {
		return nil, err
	}
	if _, ok := Find(ns, n.Name); ok {
		return nil, fmt.Errorf("node %q already exists", n.Name)
	}
	out := append([]Node(nil), ns...)
	return append(out, n), nil
}

func Replace(ns []Node, oldName string, updated Node) ([]Node, error) {
	if err := validateFields(updated); err != nil {
		return nil, err
	}
	idx, ok := Find(ns, oldName)
	if !ok {
		return nil, fmt.Errorf("node %q not found", oldName)
	}
	if updated.Name != oldName {
		if _, exists := Find(ns, updated.Name); exists {
			return nil, fmt.Errorf("node %q already exists", updated.Name)
		}
	}
	out := append([]Node(nil), ns...)
	out[idx] = updated
	return out, nil
}

func Remove(ns []Node, name string) ([]Node, error) {
	idx, ok := Find(ns, name)
	if !ok {
		return nil, fmt.Errorf("node %q not found", name)
	}
	out := append([]Node(nil), ns[:idx]...)
	return append(out, ns[idx+1:]...), nil
}

func SetEnabled(ns []Node, name string, enabled bool) ([]Node, error) {
	idx, ok := Find(ns, name)
	if !ok {
		return nil, fmt.Errorf("node %q not found", name)
	}
	out := append([]Node(nil), ns...)
	out[idx].Enabled = enabled
	return out, nil
}

func Enabled(ns []Node) []Node {
	var out []Node
	for _, n := range ns {
		if n.Enabled {
			out = append(out, n)
		}
	}
	return out
}
