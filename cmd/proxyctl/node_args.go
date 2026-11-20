package main

// node_args.go implements design spec §4.5's argument parsing rules,
// shared by every v0.10 strict-mode command. `node service install`,
// `node service reinstall` and `node service show` (Task 7, this file's
// first wiring) are unconditionally strict mode (§4.5: "node service * 和
// node publish 总是严格模式"), so this file's CLI-facing entry points never
// need the mode-detection scan below for their own dispatch. That scan
// (DetermineStrictMode) is still implemented and tested here, because the
// plan requires the strict-argument machinery to be "reusable" by Task 9,
// which will need it to decide whether an unadorned `node add/edit/enable/
// disable/remove` call is running in compat mode (existing behavior,
// untouched) or strict mode (a `--publish`/`-publish` flag token was seen).
// Nothing in this file is wired into the existing compat-mode CRUD parsing
// in node.go; those functions (cmdNodeAdd et al.) are unmodified.
import (
	"fmt"
	"strings"
)

// FlagKind distinguishes how a strict-mode flag's value token is consumed
// (spec §4.5).
type FlagKind int

const (
	// FlagString takes a value, either "--name=VAL" or "--name VAL".
	FlagString FlagKind = iota
	// FlagBool accepts only "--x" (implying true) or "--x=true"/"--x=false"
	// (spec §4.5: "布尔flag...只接受--x或--x=true|false两种写法").
	FlagBool
	// FlagExactBool is FlagBool's stricter sibling for --publish alone
	// (spec §4.5: "其中--publish只接受--publish"): no "=true|false" form,
	// and (enforced by StrictScan, not FlagKind) the token must use the
	// double-dash spelling.
	FlagExactBool
)

// FlagSpec declares one flag a strict-mode command recognizes.
type FlagSpec struct {
	// Name is the flag's bare name, without leading dashes (e.g. "name",
	// "publish").
	Name string
	Kind FlagKind
	// DoubleDashOnly requires the literal token to start with "--", not a
	// single "-" (spec §4.5: "-publish（单横线）会触发严格模式,但严格模式
	// 只接受--publish"). Only --publish uses this in v0.10; every other
	// flag accepts either spelling, matching this codebase's existing
	// convention (extractNodesFlag treats "--nodes"/"-nodes" the same).
	DoubleDashOnly bool
}

// StrictArgs is StrictScan's parsed result.
type StrictArgs struct {
	// Values holds every flag's value by name. For a FlagBool/FlagExactBool
	// flag that was set, Values[name] is "true" or "false".
	Values map[string]string
	// Set reports which flags were explicitly present.
	Set        map[string]bool
	Positional []string
}

// Bool returns whether a.Set[name] and its parsed boolean value.
func (a StrictArgs) Bool(name string) bool {
	return a.Values[name] == "true"
}

// splitFlagToken parses one "-x", "--x", "-x=v" or "--x=v" token into its
// bare name, value (if any) and whether it used "=". doubleDash reports
// whether the token began with "--" (as opposed to a single "-").
func splitFlagToken(tok string) (name, value string, hasEq, doubleDash bool) {
	rest := tok
	if strings.HasPrefix(rest, "--") {
		doubleDash = true
		rest = rest[2:]
	} else if strings.HasPrefix(rest, "-") {
		rest = rest[1:]
	}
	if idx := strings.IndexByte(rest, '='); idx >= 0 {
		return rest[:idx], rest[idx+1:], true, doubleDash
	}
	return rest, "", false, doubleDash
}

// DetermineStrictMode implements spec §4.5's mode-judgment scan for a
// compat-capable command (add/edit/enable/disable/remove/migrate): walk
// args token by token using valueFlags (every flag *this* command
// recognizes that takes a value, so its own value token is correctly
// skipped rather than mistaken for a flag) to decide whether a --publish/
// -publish token appears in *flag position* — "--name --publish" must NOT
// trigger strict mode, because that "--publish" is --name's value, not a
// flag of its own.
//
// This function is not used by Task 7's own commands (`node service *` is
// always strict, per §4.5): it exists so Task 9's compat/strict dispatch
// for `node add/edit/enable/disable/remove` can reuse the same scanning
// primitive this file already tests exhaustively.
func DetermineStrictMode(args []string, valueFlags map[string]bool) bool {
	for i := 0; i < len(args); i++ {
		a := args[i]
		if !strings.HasPrefix(a, "-") {
			continue
		}
		name, _, hasEq, _ := splitFlagToken(a)
		if name == "publish" {
			return true
		}
		if !hasEq && valueFlags[name] {
			i++ // this flag's value token is not itself inspected as a flag
		}
	}
	return false
}

// StrictScanError is returned by StrictScan; every failure maps to exit
// code 2 (spec §4.5), so the CLI layer only needs the message.
type StrictScanError struct{ msg string }

func (e *StrictScanError) Error() string { return e.msg }

func strictErr(format string, args ...interface{}) error {
	return &StrictScanError{msg: fmt.Sprintf(format, args...)}
}

// StrictScan implements spec §4.5's strict-mode parsing rules against a
// fixed set of recognized flags and an exact required positional count:
//   - unknown flag, duplicate flag, or a value-flag missing its value: error.
//   - a FlagBool/FlagExactBool flag only accepts "--x" or "--x=true|false"
//     (FlagExactBool: "--x" only, and only the double-dash spelling);
//     "--x false" (space-separated) leaves "false" as a positional
//     argument, exactly like §4.5's "--enabled false" example.
//   - positional argument count must equal wantPositional exactly.
//
// Flags may appear before or after positional arguments (spec: "flag可以
// 出现在节点名之前或之后"). StrictScan does not itself enforce the "-开头
// 节点名拒绝" or "--publish与--nodes同时出现" or "--abandon位置" rules,
// which are about a specific flag's *value* or about *which* command a
// flag appears in rather than generic token scanning — callers apply
// RejectLeadingDash and their own co-occurrence checks after a successful
// scan, keeping this function reusable across every strict command's
// different combination of such rules (spec: "严格参数扫描也供T9复用").
func StrictScan(args []string, specs []FlagSpec, wantPositional int) (StrictArgs, error) {
	byName := make(map[string]FlagSpec, len(specs))
	for _, s := range specs {
		byName[s.Name] = s
	}

	out := StrictArgs{Values: map[string]string{}, Set: map[string]bool{}}
	for i := 0; i < len(args); i++ {
		tok := args[i]
		if !strings.HasPrefix(tok, "-") {
			out.Positional = append(out.Positional, tok)
			continue
		}
		name, value, hasEq, doubleDash := splitFlagToken(tok)
		spec, ok := byName[name]
		if !ok {
			return StrictArgs{}, strictErr("unknown flag: %s", tok)
		}
		if spec.DoubleDashOnly && !doubleDash {
			return StrictArgs{}, strictErr("unknown flag: %s (must be spelled --%s)", tok, name)
		}
		if out.Set[name] {
			return StrictArgs{}, strictErr("duplicate flag: --%s", name)
		}

		switch spec.Kind {
		case FlagBool, FlagExactBool:
			if hasEq {
				if spec.Kind == FlagExactBool {
					return StrictArgs{}, strictErr("--%s does not accept a value", name)
				}
				if value != "true" && value != "false" {
					return StrictArgs{}, strictErr("--%s=%s: must be true or false", name, value)
				}
				out.Values[name] = value
			} else {
				out.Values[name] = "true"
			}
			out.Set[name] = true
		case FlagString:
			if hasEq {
				out.Values[name] = value
			} else {
				if i+1 >= len(args) {
					return StrictArgs{}, strictErr("flag needs an argument: --%s", name)
				}
				i++
				out.Values[name] = args[i]
			}
			out.Set[name] = true
		default:
			return StrictArgs{}, strictErr("internal: unknown flag kind for --%s", name)
		}
	}

	if len(out.Positional) != wantPositional {
		return StrictArgs{}, strictErr("expected %d positional argument(s), got %d", wantPositional, len(out.Positional))
	}
	return out, nil
}

// RejectLeadingDash implements spec §4.5's "以-开头的节点名在严格模式下一
// 律拒绝": callers apply this to any value meant to be a node name (§8.1's
// --name, and CRUD's positional node name once Task 9 wires strict mode in).
func RejectLeadingDash(value string) error {
	if strings.HasPrefix(value, "-") {
		return strictErr("value must not start with '-': %q", value)
	}
	return nil
}

// IsHelpOnly reports whether args is exactly one "--help" or "-h" token
// (spec §4.5: "作为唯一参数出现时,输出该子命令的usage,返回0"). When --help/
// -h appears together with other arguments, it is deliberately NOT
// special-cased here: callers never add "help"/"h" to their FlagSpec list,
// so StrictScan naturally reports it as an unknown flag (exit 2), matching
// "与其他参数同时出现时,按未知flag处理,返回2".
func IsHelpOnly(args []string) bool {
	return len(args) == 1 && (args[0] == "--help" || args[0] == "-h")
}

// CheckPublishNodesConflict implements spec §4.5's "--publish与--nodes同时
// 出现返回2": the publish flow always works on the standard nodes.conf, so a
// caller-chosen --nodes path is a usage error. It is a post-scan rule so the
// same StrictScan serves commands that do not know --nodes at all (there the
// flag is simply unknown).
func CheckPublishNodesConflict(a StrictArgs) error {
	if a.Set["publish"] && a.Set["nodes"] {
		return strictErr("--publish cannot be combined with --nodes")
	}
	return nil
}

// CheckAbandonPlacement implements spec §4.5's "--abandon只能出现在node
// service recover或node publish --recover中": allowedHere is true only for
// those two commands (Task 8 wires the recover command, Task 9 `publish
// --recover`); everywhere else a scanned --abandon is a usage error.
func CheckAbandonPlacement(a StrictArgs, allowedHere bool) error {
	if a.Set["abandon"] && !allowedHere {
		return strictErr("--abandon is only valid with `node service recover` or `node publish --recover`")
	}
	return nil
}
