package render

import (
	"fmt"

	"github.com/hlpclg/singbox-sub-manager/internal/nodes"
)

// reservedNames is the fixed 16-item reserved name list: the 14
// built-in policy group names (design §3.1, §3.2), in order, followed
// by DIRECT and REJECT (design §3.3). Callers must not mutate this
// slice; ReservedNames() returns a copy.
var reservedNames = append(append([]string{}, policyGroupNames...), "DIRECT", "REJECT")

// ReservedNames returns the 16 reserved names (14 policy group names
// plus DIRECT and REJECT) in the fixed order defined by design §3.3.
// Each call returns a new slice; mutating the result does not affect
// subsequent calls.
func ReservedNames() []string {
	out := make([]string, len(reservedNames))
	copy(out, reservedNames)
	return out
}

// CheckNodeNames reports a conflict when an enabled node's name
// exactly matches a reserved name (design §3.3). Disabled nodes are
// ignored. The first conflicting enabled node in input order is
// reported.
func CheckNodeNames(ns []nodes.Node) error {
	reserved := make(map[string]bool, len(reservedNames))
	for _, r := range reservedNames {
		reserved[r] = true
	}
	for _, n := range ns {
		if !n.Enabled {
			continue
		}
		if reserved[n.Name] {
			return fmt.Errorf("node name %q conflicts with a reserved policy group or proxy name", n.Name)
		}
	}
	return nil
}
