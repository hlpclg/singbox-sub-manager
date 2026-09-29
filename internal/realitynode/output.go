// Package realitynode: output.go implements design spec §10.1's node-machine
// paste output: the four stable CLI templates (add/enable/disable/remove),
// single-quote shell escaping, and the "exactly one executable line, every
// alternative fully commented out" block shape §8.1-§8.3 each command's
// stdout must produce (Task 7).
package realitynode

import (
	"fmt"
	"strconv"
	"strings"
)

// MinVersion is spec §3/§10.1's fixed "--min-version" value every node-
// machine-generated command carries: "节点机输出的所有订阅中心命令都带
// --min-version v0.10.0".
const MinVersion = "v0.10.0"

// ShellQuote single-quotes s for verbatim, safe inclusion in a POSIX shell
// command line (spec §4.5: "参数值统一用单引号做shell引用，空值写作”"),
// escaping any embedded single quote as '\” (close quote, escaped quote,
// reopen quote) — the standard POSIX technique, since single quotes admit
// no escape sequence of their own.
func ShellQuote(s string) string {
	if s == "" {
		return "''"
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// RegisterParams are the substituted values for the "add" template (spec
// §10.1), read from instance.json/secrets.json plus the live-derived public
// key (§8.3: "公钥实时推导").
type RegisterParams struct {
	Server     string
	ListenPort int
	UUID       string
	PublicKey  string
	ShortID    string
	SNI        string
	Name       string
}

// AddCommand renders spec §10.1's add template, flags before the node name
// (spec: "本版本生成的所有命令...一律把flag放在节点名之前").
func AddCommand(p RegisterParams) string {
	return fmt.Sprintf(
		"sudo proxyctl node add --publish --min-version %s --type vless-reality --server %s --port %s --uuid %s --public-key %s --short-id %s --sni %s --name %s",
		MinVersion,
		ShellQuote(p.Server),
		ShellQuote(strconv.Itoa(p.ListenPort)),
		ShellQuote(p.UUID),
		ShellQuote(p.PublicKey),
		ShellQuote(p.ShortID),
		ShellQuote(p.SNI),
		ShellQuote(p.Name),
	)
}

// EnableCommand, DisableCommand and RemoveCommand render spec §10.1's other
// three templates: "--publish --min-version v0.10.0 '<name>'", the node
// name as a trailing positional argument.
func EnableCommand(name string) string  { return nameOnlyCommand("enable", name) }
func DisableCommand(name string) string { return nameOnlyCommand("disable", name) }
func RemoveCommand(name string) string  { return nameOnlyCommand("remove", name) }

func nameOnlyCommand(verb, name string) string {
	return fmt.Sprintf("sudo proxyctl node %s --publish --min-version %s %s", verb, MinVersion, ShellQuote(name))
}

// Block is one spec §10.1 output block: a leading comment, exactly one
// executable command, and an optional fully-commented-out alternative
// (comment explaining when to use it, then the alternative command itself
// prefixed with "# ").
type Block struct {
	// LeadHint is an optional extra "# ..." line printed before the
	// purpose comment (spec §8.3's uninstalled-show case: "一行#注释说明
	// 服务未运行" precedes the recommended command's own purpose comment).
	LeadHint string
	Purpose  string // purpose comment for the recommended command
	Command  string // the one executable line

	// AltHint and AltCommand are empty together (no alternative) or both
	// set (spec §8.2/§8.3's "以及整行注释的备选...命令").
	AltHint    string
	AltCommand string
}

// Render produces the block's full text, one line per element, ending
// with a trailing newline. Exactly one line (Command) does not start with
// "#" — the invariant §14.1's paste tests assert.
func (b Block) Render() string {
	var lines []string
	if b.LeadHint != "" {
		lines = append(lines, "# "+b.LeadHint)
	}
	lines = append(lines, "# "+b.Purpose)
	lines = append(lines, b.Command)
	if b.AltCommand != "" {
		lines = append(lines, "# "+b.AltHint)
		lines = append(lines, "# "+b.AltCommand)
	}
	return strings.Join(lines, "\n") + "\n"
}

// InstallOutput implements spec §8.1's success output: just the add
// command, no alternative (a fresh install has no prior registration state
// to offer an alternative for).
func InstallOutput(p RegisterParams) string {
	return Block{
		Purpose: "register this node on the subscription center",
		Command: AddCommand(p),
	}.Render()
}

// ReinstallFromInstalledOutput implements spec §8.2's output when reinstall
// ran against an "already installed" instance: the add command alone
// (re-adding an unchanged, enabled node is a safe no-op per §10.3, so no
// alternative is offered).
func ReinstallFromInstalledOutput(p RegisterParams) string {
	return Block{
		Purpose: "re-register this node (safe to repeat; a matching enabled node is left unchanged)",
		Command: AddCommand(p),
	}.Render()
}

// ReinstallFromUninstalledOutput implements spec §8.2's output when
// reinstall recovered an "uninstalled but identity retained" instance: the
// enable command (for a node the subscription center previously disabled),
// plus a commented-out alternative add command (for a node the subscription
// center previously removed entirely).
func ReinstallFromUninstalledOutput(p RegisterParams) string {
	return Block{
		Purpose:    "re-enable this node if the subscription center previously disabled it",
		Command:    EnableCommand(p.Name),
		AltHint:    "if the subscription center previously removed this node entirely, use this instead",
		AltCommand: AddCommand(p),
	}.Render()
}

// ShowInstalledOutput implements spec §8.3's output for an installed
// instance: the add command, plus a commented-out alternative enable
// command for the case where the subscription center previously disabled
// this node.
func ShowInstalledOutput(p RegisterParams) string {
	return Block{
		Purpose:    "register this node on the subscription center",
		Command:    AddCommand(p),
		AltHint:    "if the subscription center previously disabled this node, use this instead",
		AltCommand: EnableCommand(p.Name),
	}.Render()
}

// ShowUninstalledOutput implements spec §8.3's output for an "uninstalled
// but identity retained" instance: a leading comment noting the service is
// not running, the disable command, plus a commented-out alternative remove
// command.
func ShowUninstalledOutput(name string) string {
	return Block{
		LeadHint:   "this node's service is not running (uninstalled, identity retained)",
		Purpose:    "disable this node on the subscription center",
		Command:    DisableCommand(name),
		AltHint:    "if you want to remove this node from the subscription entirely, use this instead",
		AltCommand: RemoveCommand(name),
	}.Render()
}
