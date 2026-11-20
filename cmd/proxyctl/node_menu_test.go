package main

// node_menu_test.go covers plan Task 10: the standalone interactive menu of
// `sudo proxyctl node` (design spec §4.4, §11.2 lock timing, §17.2, §18.4's
// interactive-migration test). Nothing here needs a terminal, root, /run/lock
// or a real host: the menu's TTY/uid/role/state/IP/dispatch dependencies come
// from the injectable nodeMenuEnv, input is a scripted one-line-per-Read
// reader, and the publish flow tests drive the real internal/publish engine
// through the same fake runtime Task 9's tests use. Nothing waits on a clock.

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/hlpclg/singbox-sub-manager/internal/hostlock"
	"github.com/hlpclg/singbox-sub-manager/internal/nodes"
	"github.com/hlpclg/singbox-sub-manager/internal/realitynode"
	"github.com/hlpclg/singbox-sub-manager/internal/role"
)

// scriptedInput feeds exactly one line per Read call and logs every line it
// hands out ("input:<n>"), so a test can order "the user answered" against
// the other events (lock acquisition, publish runtime construction, output).
type scriptedInput struct {
	lines []string
	pos   int
	log   *callLog
}

func (s *scriptedInput) Read(p []byte) (int, error) {
	if s.pos >= len(s.lines) {
		return 0, io.EOF
	}
	line := s.lines[s.pos] + "\n"
	s.pos++
	if s.log != nil {
		s.log.add("input:" + strconv.Itoa(s.pos))
	}
	return copy(p, line), nil
}

func (s *scriptedInput) consumed() int { return s.pos }

// logWriter appends every write to the shared call log as "out:<text>" and
// also keeps the bytes.
type logWriter struct {
	log *callLog
	buf bytes.Buffer
}

func (w *logWriter) Write(p []byte) (int, error) {
	w.log.add("out:" + string(p))
	return w.buf.Write(p)
}

func (w *logWriter) String() string { return w.buf.String() }

type menuFixture struct {
	t          *testing.T
	log        *callLog
	env        nodeMenuEnv
	in         *scriptedInput
	dispatched [][]string
	// real, when true, makes dispatch also call the real cmdNode after
	// recording argv; otherwise dispatch only records and returns dispatchRC.
	real       bool
	dispatchRC int
	roleCalls  int
	stateCalls int
	ipCalls    int
}

func newMenuFixture(t *testing.T, lines ...string) *menuFixture {
	t.Helper()
	m := &menuFixture{t: t, log: &callLog{}}
	m.in = &scriptedInput{lines: lines, log: m.log}
	m.env = nodeMenuEnv{
		stdin:       m.in,
		stdinIsTTY:  func() bool { return true },
		stdoutIsTTY: func(io.Writer) bool { return true },
		geteuid:     func() int { return 0 },
		detectRole:  func() role.Result { m.roleCalls++; return role.Result{Status: role.StatusNo} },
		instanceState: func() realitynode.DetectResult {
			m.stateCalls++
			return realitynode.DetectResult{Status: realitynode.StatusNotInstalled}
		},
		publishPending: func() bool { return false },
		publicIPv4:     func(context.Context) (string, error) { m.ipCalls++; return "203.0.113.7", nil },
		dispatch: func(args []string, stdout, stderr io.Writer) int {
			m.log.add("dispatch:" + strings.Join(args, " "))
			m.dispatched = append(m.dispatched, append([]string(nil), args...))
			if m.real {
				return cmdNode(args, stdout, stderr)
			}
			return m.dispatchRC
		},
	}
	return m
}

func (m *menuFixture) withRole(s role.Status, reasons ...string) *menuFixture {
	m.env.detectRole = func() role.Result { m.roleCalls++; return role.Result{Status: s, Reasons: reasons} }
	return m
}

func (m *menuFixture) withState(s realitynode.InstanceStatus, reasons ...string) *menuFixture {
	m.env.instanceState = func() realitynode.DetectResult {
		m.stateCalls++
		return realitynode.DetectResult{Status: s, Reasons: reasons}
	}
	return m
}

// run installs the fixture as the production menu env, runs `proxyctl node`
// with no arguments through run() and returns the exit code and the text of
// stdout/stderr.
func (m *menuFixture) run() (code int, stdout, stderr string) {
	m.t.Helper()
	orig := newNodeMenuEnv
	newNodeMenuEnv = func() nodeMenuEnv { return m.env }
	m.t.Cleanup(func() { newNodeMenuEnv = orig })
	out := &logWriter{log: m.log}
	errb := &logWriter{log: m.log}
	code = run([]string{"node"}, out, errb)
	return code, out.String(), errb.String()
}

func (m *menuFixture) dispatchedLine(i int) string {
	if i >= len(m.dispatched) {
		return ""
	}
	return strings.Join(m.dispatched[i], " ")
}

// --- TestNodeMenuTTYMatrix ---

func TestNodeMenuTTYMatrix(t *testing.T) {
	t.Run("TTY combinations: only stdin and stdout both TTY open the menu", func(t *testing.T) {
		for _, tc := range []struct {
			name          string
			stdin, stdout bool
			menu          bool
		}{
			{"stdin tty, stdout tty", true, true, true},
			{"stdin tty, stdout piped", true, false, false},
			{"stdin piped, stdout tty", false, true, false},
			{"neither tty", false, false, false},
		} {
			t.Run(tc.name, func(t *testing.T) {
				m := newMenuFixture(t, "0")
				m.env.stdinIsTTY = func() bool { return tc.stdin }
				m.env.stdoutIsTTY = func(io.Writer) bool { return tc.stdout }
				code, out, errb := m.run()
				if tc.menu {
					if code != 0 || !strings.Contains(out, "主机角色：") {
						t.Fatalf("code=%d out=%q err=%q; want the menu and exit 0", code, out, errb)
					}
					return
				}
				if code != 2 {
					t.Fatalf("code=%d, want usage exit 2", code)
				}
				if !strings.Contains(errb, "usage: proxyctl node") || out != "" {
					t.Fatalf("want the usage text on stderr only: out=%q err=%q", out, errb)
				}
				if m.roleCalls+m.stateCalls+m.ipCalls != 0 || len(m.dispatched) != 0 || m.in.consumed() != 0 {
					t.Fatalf("non-TTY must touch nothing: role=%d state=%d ip=%d dispatched=%v consumed=%d",
						m.roleCalls, m.stateCalls, m.ipCalls, m.dispatched, m.in.consumed())
				}
			})
		}
	})

	t.Run("a node subcommand never opens the menu", func(t *testing.T) {
		m := newMenuFixture(t, "0")
		orig := newNodeMenuEnv
		newNodeMenuEnv = func() nodeMenuEnv { t.Fatal("menu env must not be built for `node bogus`"); return m.env }
		t.Cleanup(func() { newNodeMenuEnv = orig })
		var out, errb bytes.Buffer
		if code := run([]string{"node", "bogus"}, &out, &errb); code != 2 {
			t.Fatalf("code=%d, want 2", code)
		}
	})

	t.Run("non-root: sudo hint, exit 1, role and state are not read", func(t *testing.T) {
		m := newMenuFixture(t, "1", "0")
		m.env.geteuid = func() int { return 1000 }
		code, out, errb := m.run()
		if code != 1 {
			t.Fatalf("code=%d, want 1", code)
		}
		if !strings.Contains(errb, "sudo proxyctl node") {
			t.Fatalf("stderr must tell the user to rerun with sudo: %q", errb)
		}
		if strings.Contains(out, "主机角色") {
			t.Fatalf("non-root must not print a role line: %q", out)
		}
		if m.roleCalls != 0 || m.stateCalls != 0 || m.ipCalls != 0 || len(m.dispatched) != 0 || m.in.consumed() != 0 {
			t.Fatalf("non-root must not read role/state/IP, dispatch or read input: role=%d state=%d ip=%d", m.roleCalls, m.stateCalls, m.ipCalls)
		}
	})

	t.Run("role line and every item are shown; unavailable items say why", func(t *testing.T) {
		m := newMenuFixture(t, "0").withRole(role.StatusUncertain, "install.json missing or unreadable", "default nodes.conf does not exist")
		code, out, _ := m.run()
		if code != 0 {
			t.Fatalf("code=%d", code)
		}
		if !strings.Contains(out, "主机角色：订阅中心=不确定(install.json missing or unreadable; default nodes.conf does not exist)；Reality 代理服务=未安装") {
			t.Fatalf("role line wrong:\n%s", out)
		}
		for _, item := range []string{"1) 安装 VLESS Reality 服务", "2) 显示注册命令", "3) 幂等重装", "4) 卸载",
			"5) 添加远程节点", "6) 管理节点", "7) 重新发布订阅", "0) 退出"} {
			if !strings.Contains(out, item) {
				t.Errorf("menu is missing %q", item)
			}
		}
		// Uncertain role: install and the three subscription-center items are
		// unavailable and carry the role reason; reinstall/uninstall are
		// unavailable because of the instance state (not installed).
		for _, tc := range []struct{ item, reason string }{
			{"1) 安装 VLESS Reality 服务", "订阅中心角色不确定"},
			{"5) 添加远程节点", "订阅中心角色不确定"},
			{"6) 管理节点", "订阅中心角色不确定"},
			{"7) 重新发布订阅", "订阅中心角色不确定"},
			{"3) 幂等重装", "未安装"},
			{"4) 卸载", "未安装"},
		} {
			line := lineWith(out, tc.item)
			if !strings.Contains(line, "不可用") || !strings.Contains(line, tc.reason) {
				t.Errorf("%s should be marked unavailable with %q, got %q", tc.item, tc.reason, line)
			}
		}
		if line := lineWith(out, "2) 显示注册命令"); strings.Contains(line, "不可用") {
			t.Errorf("show is always available (it reports the state itself): %q", line)
		}
	})

	t.Run("availability follows the role x state tables", func(t *testing.T) {
		for _, tc := range []struct {
			name        string
			role        role.Status
			state       realitynode.InstanceStatus
			available   []string
			unavailable map[string]string
		}{
			{"node machine, not installed", role.StatusNo, realitynode.StatusNotInstalled,
				[]string{"1)", "2)"}, map[string]string{"3)": "未安装", "4)": "未安装", "5)": "不是订阅中心", "6)": "不是订阅中心", "7)": "不是订阅中心"}},
			{"node machine, installed", role.StatusNo, realitynode.StatusInstalled,
				[]string{"2)", "3)", "4)"}, map[string]string{"1)": "已安装", "5)": "不是订阅中心"}},
			{"node machine, uninstalled but identity kept", role.StatusNo, realitynode.StatusUninstalledKeepIdentity,
				[]string{"2)", "3)", "4)"}, map[string]string{"1)": "已卸载"}},
			{"subscription center, not installed", role.StatusYes, realitynode.StatusNotInstalled,
				[]string{"2)", "5)", "6)", "7)"}, map[string]string{"1)": "本机是订阅中心", "3)": "未安装"}},
			{"subscription center, installed (should not happen) cannot reinstall", role.StatusYes, realitynode.StatusInstalled,
				[]string{"2)", "4)", "5)", "6)", "7)"}, map[string]string{"1)": "已安装", "3)": "本机是订阅中心"}},
			{"inconsistent instance lists its reasons", role.StatusNo, realitynode.StatusInconsistent,
				[]string{"2)"}, map[string]string{"1)": "不一致", "3)": "不一致", "4)": "不一致"}},
			{"unfinished instance transaction", role.StatusNo, realitynode.StatusUnfinishedTxn,
				[]string{"2)"}, map[string]string{"1)": "未完成的事务", "3)": "未完成的事务", "4)": "未完成的事务"}},
		} {
			t.Run(tc.name, func(t *testing.T) {
				m := newMenuFixture(t, "0").withRole(tc.role).withState(tc.state, "unit missing")
				_, out, _ := m.run()
				for _, id := range tc.available {
					if line := lineWith(out, id); line == "" || strings.Contains(line, "不可用") {
						t.Errorf("item %s should be available: %q", id, line)
					}
				}
				for id, reason := range tc.unavailable {
					line := lineWith(out, id)
					if !strings.Contains(line, "不可用") || !strings.Contains(line, reason) {
						t.Errorf("item %s should be unavailable with %q: %q", id, reason, line)
					}
				}
			})
		}
	})

	t.Run("unfinished transactions: recover commands under the role line, no recover item", func(t *testing.T) {
		m := newMenuFixture(t, "0").withState(realitynode.StatusUnfinishedTxn)
		m.env.publishPending = func() bool { return true }
		m.withRole(role.StatusYes)
		_, out, _ := m.run()
		lines := strings.Split(out, "\n")
		roleIdx := -1
		for i, l := range lines {
			if strings.HasPrefix(l, "主机角色：") {
				roleIdx = i
			}
		}
		if roleIdx < 0 || roleIdx+2 >= len(lines) {
			t.Fatalf("no role line:\n%s", out)
		}
		below := lines[roleIdx+1] + "\n" + lines[roleIdx+2]
		if !strings.Contains(below, "sudo proxyctl node service recover") || !strings.Contains(below, "sudo proxyctl node publish --recover") {
			t.Fatalf("both recover commands must sit right below the role line:\n%s", out)
		}
		if strings.Contains(out, "8)") {
			t.Fatalf("the menu must not grow a recovery item:\n%s", out)
		}
		// publish items are unavailable while a publish transaction is pending.
		for _, id := range []string{"5)", "6)", "7)"} {
			if line := lineWith(out, id); !strings.Contains(line, "不可用") || !strings.Contains(line, "发布事务") {
				t.Errorf("item %s should be blocked by the pending publish transaction: %q", id, line)
			}
		}
	})

	t.Run("only the applicable recover command is shown", func(t *testing.T) {
		m := newMenuFixture(t, "0").withState(realitynode.StatusUnfinishedTxn)
		_, out, _ := m.run()
		if !strings.Contains(out, "sudo proxyctl node service recover") || strings.Contains(out, "publish --recover") {
			t.Fatalf("instance transaction only:\n%s", out)
		}
		m = newMenuFixture(t, "0").withRole(role.StatusYes)
		m.env.publishPending = func() bool { return true }
		_, out, _ = m.run()
		if strings.Contains(out, "service recover") || !strings.Contains(out, "sudo proxyctl node publish --recover") {
			t.Fatalf("publish transaction only:\n%s", out)
		}
	})

	t.Run("choosing an unavailable item explains and does not act", func(t *testing.T) {
		m := newMenuFixture(t, "5", "abc", "0").withRole(role.StatusNo)
		code, out, _ := m.run()
		if code != 0 || len(m.dispatched) != 0 {
			t.Fatalf("code=%d dispatched=%v", code, m.dispatched)
		}
		if strings.Count(out, "不可用") < 5 || !strings.Contains(out, "无效的选择") {
			t.Fatalf("expected the reason and an invalid-choice message:\n%s", out)
		}
	})

	t.Run("EOF and quit never act", func(t *testing.T) {
		for _, lines := range [][]string{{}, {"0"}, {"q"}} {
			m := newMenuFixture(t, lines...)
			code, _, _ := m.run()
			if code != 0 || len(m.dispatched) != 0 {
				t.Errorf("lines=%v code=%d dispatched=%v", lines, code, m.dispatched)
			}
		}
	})
}

func lineWith(out, id string) string {
	for _, l := range strings.Split(out, "\n") {
		if strings.Contains(l, id) {
			return l
		}
	}
	return ""
}

// --- node service items: argv, defaults, confirmation ---

func TestMenuServiceActions(t *testing.T) {
	t.Run("install: the detected IPv4 is only a default the user must confirm", func(t *testing.T) {
		m := newMenuFixture(t, "1", "jp1", "", "", "", "y")
		code, out, _ := m.run()
		if code != 0 {
			t.Fatalf("code=%d", code)
		}
		if m.ipCalls != 1 {
			t.Fatalf("public IP probe calls = %d", m.ipCalls)
		}
		if got, want := m.dispatchedLine(0), "service install --name jp1 --server 203.0.113.7 --port 443 --sni www.microsoft.com"; got != want {
			t.Fatalf("dispatched %q, want %q", got, want)
		}
		if !strings.Contains(out, "sudo proxyctl node service install --name 'jp1' --server '203.0.113.7' --port '443' --sni 'www.microsoft.com'") {
			t.Fatalf("equivalent command missing:\n%s", out)
		}
		if !strings.Contains(out, "203.0.113.7") || !strings.Contains(out, "www.microsoft.com") {
			t.Fatalf("defaults must be shown to the user:\n%s", out)
		}
	})

	t.Run("install: the user can override the defaults", func(t *testing.T) {
		m := newMenuFixture(t, "1", "jp1", "198.51.100.4", "8443", "example.org", "y")
		if code, _, _ := m.run(); code != 0 {
			t.Fatalf("code=%d", code)
		}
		if got, want := m.dispatchedLine(0), "service install --name jp1 --server 198.51.100.4 --port 8443 --sni example.org"; got != want {
			t.Fatalf("dispatched %q, want %q", got, want)
		}
	})

	t.Run("install: declining the confirmation dispatches nothing", func(t *testing.T) {
		for _, answer := range []string{"n", "", "no", "x"} {
			m := newMenuFixture(t, "1", "jp1", "", "", "", answer)
			code, out, _ := m.run()
			if code != 0 || len(m.dispatched) != 0 {
				t.Fatalf("answer %q: code=%d dispatched=%v", answer, code, m.dispatched)
			}
			if !strings.Contains(out, "已取消") {
				t.Fatalf("answer %q: a cancellation notice is expected:\n%s", answer, out)
			}
		}
	})

	t.Run("install: EOF before the confirmation dispatches nothing", func(t *testing.T) {
		m := newMenuFixture(t, "1", "jp1", "", "", "")
		code, _, _ := m.run()
		if code != 0 || len(m.dispatched) != 0 {
			t.Fatalf("code=%d dispatched=%v", code, m.dispatched)
		}
	})

	t.Run("install: without a detected IP the server must be typed", func(t *testing.T) {
		m := newMenuFixture(t, "1", "jp1", "", "198.51.100.4", "", "", "y")
		m.env.publicIPv4 = func(context.Context) (string, error) { m.ipCalls++; return "", errors.New("offline") }
		code, out, _ := m.run()
		if code != 0 {
			t.Fatalf("code=%d", code)
		}
		if got, want := m.dispatchedLine(0), "service install --name jp1 --server 198.51.100.4 --port 443 --sni www.microsoft.com"; got != want {
			t.Fatalf("dispatched %q, want %q", got, want)
		}
		if !strings.Contains(out, "不能为空") {
			t.Fatalf("an empty required answer must be rejected:\n%s", out)
		}
	})

	t.Run("install: invalid values are reported before confirmation, exit 2, nothing dispatched", func(t *testing.T) {
		m := newMenuFixture(t, "1", "jp1", "", "70000", "", "y")
		code, out, errb := m.run()
		if len(m.dispatched) != 0 {
			t.Fatalf("dispatched=%v out=%s err=%s", m.dispatched, out, errb)
		}
		if code != 2 {
			t.Fatalf("code=%d, want 2 (a usage-class input error)", code)
		}
	})

	t.Run("show, reinstall and uninstall equivalents", func(t *testing.T) {
		for _, tc := range []struct {
			name  string
			state realitynode.InstanceStatus
			lines []string
			want  string
			shown string
		}{
			{"show", realitynode.StatusInstalled, []string{"2", "y"}, "service show", "sudo proxyctl node service show"},
			{"show default is yes", realitynode.StatusInstalled, []string{"2", ""}, "service show", "sudo proxyctl node service show"},
			{"reinstall", realitynode.StatusInstalled, []string{"3", "y"}, "service reinstall", "sudo proxyctl node service reinstall"},
			{"uninstall keep identity", realitynode.StatusInstalled, []string{"4", "1", "y"}, "service uninstall", "sudo proxyctl node service uninstall"},
			{"purge without export", realitynode.StatusInstalled, []string{"4", "2", "n", "y"}, "service uninstall --purge", "sudo proxyctl node service uninstall --purge"},
			{"purge with export", realitynode.StatusInstalled, []string{"4", "2", "y", "/root/reality backup's.tgz", "y"},
				"service uninstall --purge --export-backup /root/reality backup's.tgz",
				`sudo proxyctl node service uninstall --purge --export-backup '/root/reality backup'\''s.tgz'`},
			{"purge from uninstalled state", realitynode.StatusUninstalledKeepIdentity, []string{"4", "n", "y"}, "service uninstall --purge",
				"sudo proxyctl node service uninstall --purge"},
		} {
			t.Run(tc.name, func(t *testing.T) {
				m := newMenuFixture(t, tc.lines...).withRole(role.StatusNo).withState(tc.state)
				code, out, _ := m.run()
				if code != 0 {
					t.Fatalf("code=%d\n%s", code, out)
				}
				if len(m.dispatched) != 1 || m.dispatchedLine(0) != tc.want {
					t.Fatalf("dispatched %v, want %q", m.dispatched, tc.want)
				}
				if !strings.Contains(out, tc.shown) {
					t.Fatalf("equivalent command %q not shown:\n%s", tc.shown, out)
				}
			})
		}
	})

	t.Run("uninstalled-but-kept offers only the purge choice", func(t *testing.T) {
		m := newMenuFixture(t, "4", "n").withRole(role.StatusNo).withState(realitynode.StatusUninstalledKeepIdentity)
		_, out, _ := m.run()
		if strings.Contains(out, "卸载并保留身份") {
			t.Fatalf("keep-identity uninstall is not allowed from this state:\n%s", out)
		}
		if !strings.Contains(out, "彻底清除") {
			t.Fatalf("the purge choice must be explained:\n%s", out)
		}
		if len(m.dispatched) != 0 {
			t.Fatalf("dispatched=%v", m.dispatched)
		}
	})

	t.Run("a dispatched command's exit code is returned as-is", func(t *testing.T) {
		for _, rc := range []int{0, 1, 4, 5} {
			m := newMenuFixture(t, "2", "y").withState(realitynode.StatusInstalled)
			m.dispatchRC = rc
			if code, _, _ := m.run(); code != rc {
				t.Errorf("code=%d, want the business code %d", code, rc)
			}
		}
	})
}

// --- TestMenuPublishFlow ---

// publishMenuFixture is the real publish engine behind the menu.
func publishMenuFixture(t *testing.T, lines []string, opts publishRuntimeOpts) (*cliFixture, *menuFixture) {
	t.Helper()
	f := installFakePublishRuntime(t, opts)
	withOverriddenDefaultNodesPath(t, f.nodesPath)
	m := newMenuFixture(t, lines...).withRole(role.StatusYes)
	m.log = f.log
	m.in.log = f.log
	m.real = true
	return f, m
}

func TestMenuPublishFlow(t *testing.T) {
	// assertConfirmBarrier checks the §11.2 lock-timing rule: no business
	// runtime (root check, role, locks, env) before the user's final answer,
	// and the L1+L2 acquisition after it.
	assertConfirmBarrier := func(t *testing.T, f *cliFixture, confirmInput int) {
		t.Helper()
		events := f.log.list()
		confirmAt, lockAt := -1, -1
		for i, e := range events {
			if e == "input:"+strconv.Itoa(confirmInput) {
				confirmAt = i
			}
			if strings.HasPrefix(e, "lock:") && lockAt < 0 {
				lockAt = i
			}
		}
		if confirmAt < 0 || lockAt < 0 {
			t.Fatalf("missing confirm/lock events: %v", events)
		}
		for i := 0; i < confirmAt; i++ {
			e := events[i]
			if e == "runtime" || e == "geteuid" || e == "newEnv" || e == "role" || strings.HasPrefix(e, "lock:") || strings.HasPrefix(e, "dispatch:") {
				t.Fatalf("event %q happened before the user confirmed (confirm at %d): %v", e, confirmAt, events)
			}
		}
		if lockAt < confirmAt {
			t.Fatalf("lock taken before the confirmation: %v", events)
		}
		if got := events[lockAt]; got != "lock:L1+L2" {
			t.Fatalf("lock = %q, want L1+L2", got)
		}
	}
	forbidden := func(t *testing.T, m *menuFixture) {
		t.Helper()
		for _, argv := range m.dispatched {
			for _, a := range argv {
				if a == "--nodes" || a == "--output" || strings.HasPrefix(a, "--nodes=") || strings.HasPrefix(a, "--output=") || a == "--token" || a == "--token-file" {
					t.Fatalf("the menu must never put %s on the command line: %v", a, argv)
				}
			}
		}
	}
	roundTrip := func(t *testing.T, m *menuFixture, out string) {
		t.Helper()
		var shown string
		for _, l := range strings.Split(out, "\n") {
			if strings.HasPrefix(l, "sudo proxyctl node ") {
				shown = l
			}
		}
		if shown == "" {
			t.Fatalf("no equivalent command printed:\n%s", out)
		}
		got := proxyctlArgs(t, shown)
		want := append([]string{"node"}, m.dispatched[0]...)
		if strings.Join(got, "\x00") != strings.Join(want, "\x00") {
			t.Fatalf("the printed command does not parse back to the dispatched argv:\nprinted:    %q\ndispatched: %q", got, want)
		}
	}

	t.Run("add remote node publishes through --publish", func(t *testing.T) {
		lines := []string{"5", "jp1", "203.0.113.9", "", "www.microsoft.com", cliUUID, cliPubKey, "ab12", "y"}
		f, m := publishMenuFixture(t, lines, publishRuntimeOpts{})
		code, out, errb := m.run()
		if code != 0 {
			t.Fatalf("code=%d out=%s err=%s", code, out, errb)
		}
		if got := f.nodeNames(); got != "A,B,jp1" {
			t.Fatalf("nodes = %s", got)
		}
		want := "add --publish --type vless-reality --server 203.0.113.9 --port 443 --uuid " + cliUUID + " --public-key " + cliPubKey + " --short-id ab12 --sni www.microsoft.com --name jp1"
		if m.dispatchedLine(0) != want {
			t.Fatalf("dispatched %q\nwant       %q", m.dispatchedLine(0), want)
		}
		// flags before values quoted, node name last for CRUD, '' for empty.
		if !strings.Contains(out, "--server '203.0.113.9' --port '443'") {
			t.Fatalf("values must be single-quoted:\n%s", out)
		}
		roundTrip(t, m, out)
		forbidden(t, m)
		assertConfirmBarrier(t, f, len(lines))
		b, _ := os.ReadFile(filepath.Join(f.tokenDir, "clash.yaml"))
		if !strings.Contains(string(b), "jp1") {
			t.Fatalf("clash.yaml was not republished:\n%s", b)
		}
	})

	t.Run("add with an empty short id prints '' for it", func(t *testing.T) {
		lines := []string{"5", "jp2", "203.0.113.10", "", "www.microsoft.com", cliUUID, cliPubKey, "", "y"}
		f, m := publishMenuFixture(t, lines, publishRuntimeOpts{})
		code, out, errb := m.run()
		if code != 0 {
			t.Fatalf("code=%d out=%s err=%s", code, out, errb)
		}
		if !strings.Contains(out, "--short-id ''") {
			t.Fatalf("empty value must be printed as '':\n%s", out)
		}
		roundTrip(t, m, out)
		if got := f.nodeNames(); got != "A,B,jp2" {
			t.Fatalf("nodes = %s", got)
		}
	})

	t.Run("manage: edit only sends changed fields", func(t *testing.T) {
		// 6 = manage, 1 = first node (A), 1 = edit, then name/server/port/sni
		// prompts (hysteria2 nodes: no uuid/public key/short id), confirm.
		lines := []string{"6", "1", "1", "", "", "8443", "", "y"}
		f, m := publishMenuFixture(t, lines, publishRuntimeOpts{})
		code, out, errb := m.run()
		if code != 0 {
			t.Fatalf("code=%d out=%s err=%s", code, out, errb)
		}
		if got, want := m.dispatchedLine(0), "edit --publish --port 8443 A"; got != want {
			t.Fatalf("dispatched %q, want %q", got, want)
		}
		ns := f.loadNodes()
		if ns[0].Port != 8443 || ns[1].Port != 443 {
			t.Fatalf("ports = %d,%d", ns[0].Port, ns[1].Port)
		}
		roundTrip(t, m, out)
		forbidden(t, m)
		assertConfirmBarrier(t, f, len(lines))
	})

	t.Run("manage: edit with no change is cancelled without dispatch", func(t *testing.T) {
		f, m := publishMenuFixture(t, []string{"6", "1", "1", "", "", "", "", "y"}, publishRuntimeOpts{})
		before := f.snapshot()
		code, _, _ := m.run()
		if code != 0 || len(m.dispatched) != 0 {
			t.Fatalf("code=%d dispatched=%v", code, m.dispatched)
		}
		sameSnapshots(t, before, f.snapshot())
	})

	for _, tc := range []struct {
		name   string
		pick   string // action in the per-node submenu
		node   string // node index in the list
		want   string
		verify func(t *testing.T, f *cliFixture)
	}{
		{"disable", "3", "1", "disable --publish A", func(t *testing.T, f *cliFixture) {
			if ns := f.loadNodes(); ns[0].Enabled || !ns[1].Enabled {
				t.Fatalf("enabled flags = %v,%v", ns[0].Enabled, ns[1].Enabled)
			}
		}},
		{"remove", "4", "2", "remove --publish B", func(t *testing.T, f *cliFixture) {
			if got := f.nodeNames(); got != "A" {
				t.Fatalf("nodes = %s", got)
			}
		}},
	} {
		t.Run("manage: "+tc.name, func(t *testing.T) {
			lines := []string{"6", tc.node, tc.pick, "y"}
			f, m := publishMenuFixture(t, lines, publishRuntimeOpts{})
			code, out, errb := m.run()
			if code != 0 {
				t.Fatalf("code=%d out=%s err=%s", code, out, errb)
			}
			if m.dispatchedLine(0) != tc.want {
				t.Fatalf("dispatched %q, want %q", m.dispatchedLine(0), tc.want)
			}
			tc.verify(t, f)
			roundTrip(t, m, out)
			forbidden(t, m)
			assertConfirmBarrier(t, f, len(lines))
		})
	}

	t.Run("manage: enable a disabled node", func(t *testing.T) {
		lines := []string{"6", "2", "2", "y"}
		f, m := publishMenuFixture(t, lines, publishRuntimeOpts{})
		ns := f.loadNodes()
		ns[1].Enabled = false
		writeMode(t, f.nodesPath, []byte(nodes.Serialize(ns)), 0600)
		code, out, errb := m.run()
		if code != 0 {
			t.Fatalf("code=%d out=%s err=%s", code, out, errb)
		}
		if m.dispatchedLine(0) != "enable --publish B" || !f.loadNodes()[1].Enabled {
			t.Fatalf("dispatched %q nodes=%v", m.dispatchedLine(0), f.loadNodes())
		}
		assertConfirmBarrier(t, f, len(lines))
	})

	t.Run("republish", func(t *testing.T) {
		lines := []string{"7", "y"}
		f, m := publishMenuFixture(t, lines, publishRuntimeOpts{})
		code, out, errb := m.run()
		if code != 0 {
			t.Fatalf("code=%d out=%s err=%s", code, out, errb)
		}
		if m.dispatchedLine(0) != "publish" {
			t.Fatalf("dispatched %q", m.dispatchedLine(0))
		}
		if !strings.Contains(out, "sudo proxyctl node publish") {
			t.Fatalf("equivalent command missing:\n%s", out)
		}
		assertConfirmBarrier(t, f, len(lines))
	})

	t.Run("business errors are reported as-is with the business exit code", func(t *testing.T) {
		// Disabling the only enabled node is refused by the publish engine (1).
		lines := []string{"6", "1", "3", "y"}
		f, m := publishMenuFixture(t, lines, publishRuntimeOpts{})
		ns := f.loadNodes()
		ns[1].Enabled = false
		writeMode(t, f.nodesPath, []byte(nodes.Serialize(ns)), 0600)
		before := f.snapshot()
		var direct, directErr bytes.Buffer
		wantCode := run([]string{"node", "disable", "--publish", "A"}, &direct, &directErr)
		sameSnapshots(t, before, f.snapshot())
		if wantCode != 1 || directErr.Len() == 0 {
			t.Fatalf("the business command itself should refuse: code=%d err=%q", wantCode, directErr.String())
		}
		code, _, errb := m.run()
		if code != wantCode {
			t.Fatalf("menu code=%d, business code=%d", code, wantCode)
		}
		if !strings.Contains(errb, strings.TrimSpace(directErr.String())) {
			t.Fatalf("business stderr not passed through:\nmenu:     %q\nbusiness: %q", errb, directErr.String())
		}
		sameSnapshots(t, before, f.snapshot())
	})

	t.Run("a rolled-back publish returns 4 from the menu", func(t *testing.T) {
		f, m := publishMenuFixture(t, []string{"7", "y"}, publishRuntimeOpts{})
		f.fetcher.status["clash.yaml"] = 503
		before := f.snapshot()
		code, _, _ := m.run()
		if code != 4 {
			t.Fatalf("code=%d, want 4", code)
		}
		// mtimes legitimately differ for restored files: compare content only
		after := f.snapshot()
		for k, v := range before {
			if stripTime(after[k]) != stripTime(v) {
				t.Errorf("%s not restored: %q vs %q", k, v, after[k])
			}
		}
	})

	t.Run("cancelling or EOF before the confirmation: no lock, no write", func(t *testing.T) {
		for _, tc := range []struct {
			name  string
			lines []string
		}{
			{"decline add", []string{"5", "jp1", "203.0.113.9", "", "www.microsoft.com", cliUUID, cliPubKey, "ab12", "n"}},
			{"EOF at the add confirmation", []string{"5", "jp1", "203.0.113.9", "", "www.microsoft.com", cliUUID, cliPubKey, "ab12"}},
			{"EOF in the middle of the add form", []string{"5", "jp1", "203.0.113.9"}},
			{"decline disable", []string{"6", "1", "3", "n"}},
			{"EOF after choosing remove", []string{"6", "1", "4"}},
			{"decline republish", []string{"7", ""}},
			{"EOF at the republish confirmation", []string{"7"}},
			{"quit from the node list", []string{"6", "0"}},
			{"quit from the action submenu", []string{"6", "1", "0"}},
		} {
			t.Run(tc.name, func(t *testing.T) {
				f, m := publishMenuFixture(t, tc.lines, publishRuntimeOpts{})
				before := f.snapshot()
				code, _, errb := m.run()
				if code != 0 {
					t.Fatalf("code=%d err=%s", code, errb)
				}
				if len(m.dispatched) != 0 {
					t.Fatalf("dispatched %v", m.dispatched)
				}
				for _, e := range f.log.list() {
					if e == "runtime" || strings.HasPrefix(e, "lock:") {
						t.Fatalf("business runtime/lock touched before confirmation: %v", f.log.list())
					}
				}
				sameSnapshots(t, before, f.snapshot())
			})
		}
	})

	t.Run("a confirmed action inherits the business cancellation (lock wait, SIGINT)", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		block := make(chan struct{})
		f, m := publishMenuFixture(t, []string{"7", "y"}, publishRuntimeOpts{acquireBlock: block, ctx: ctx, cancel: cancel})
		before := f.snapshot()
		done := make(chan int, 1)
		go func() {
			done <- func() int { code, _, _ := m.run(); return code }()
		}()
		<-block // the business lock wait has been entered, after the confirmation
		cancel()
		if code := <-done; code != 1 {
			t.Fatalf("code=%d, want 1 (cancelled lock wait)", code)
		}
		sameSnapshots(t, before, f.snapshot())
	})
}

// --- TestMenuMigrationConsent ---

const legacyNodesConf = "JP|203.0.113.5|443|pw-JP|ob-JP|www.example.com\nUS|203.0.113.6|443|pw-US|ob-US|www.example.org\n"

func TestMenuMigrationConsent(t *testing.T) {
	setup := func(t *testing.T, lines []string, migrateLock func(ctx context.Context, paths hostlock.Paths, opts hostlock.AcquireOptions, levels ...hostlock.Level) (*hostlock.Lease, error)) (*cliFixture, *menuFixture, *int) {
		t.Helper()
		f, m := publishMenuFixture(t, lines, publishRuntimeOpts{})
		writeMode(t, f.nodesPath, []byte(legacyNodesConf), 0600)
		acquired := 0
		if migrateLock == nil {
			migrateLock = alwaysAcquire(&acquired)
		}
		withOverriddenNodeLockEnv(t, clearNodeLockEnv(t, migrateLock))
		return f, m, &acquired
	}

	for _, action := range []struct {
		name  string
		lines []string // the consent answer is appended
	}{
		{"add", []string{"5"}},
		{"manage", []string{"6"}},
		{"republish", []string{"7"}},
	} {
		for _, refusal := range []struct {
			name  string
			lines []string
		}{
			{"declined", []string{"n"}},
			{"default answer", []string{""}},
			{"EOF", nil},
		} {
			t.Run(action.name+" with legacy nodes.conf: consent "+refusal.name, func(t *testing.T) {
				lines := append(append([]string(nil), action.lines...), refusal.lines...)
				// Anything after the refused consent must stay unread.
				lines = append(lines, "jp1", "203.0.113.9", "y")
				if refusal.lines == nil {
					lines = action.lines
				}
				f, m, acquired := setup(t, lines, nil)
				before := f.snapshot()
				code, out, errb := m.run()
				if code != 1 {
					t.Fatalf("code=%d, want 1 (publishing needs the sectioned format); out=%s err=%s", code, out, errb)
				}
				if len(m.dispatched) != 0 || *acquired != 0 {
					t.Fatalf("a refusal must run nothing: dispatched=%v migrate locks=%d", m.dispatched, *acquired)
				}
				for _, e := range f.log.list() {
					if e == "runtime" || strings.HasPrefix(e, "lock:") {
						t.Fatalf("no registration/publish after a refusal: %v", f.log.list())
					}
				}
				if want := len(action.lines) + len(refusal.lines); refusal.lines != nil && m.in.consumed() != want {
					t.Fatalf("input consumed = %d, want %d (nothing after the refusal)", m.in.consumed(), want)
				}
				if _, err := os.Stat(f.nodesPath + ".bak"); err == nil {
					t.Fatal("a refusal must not create a backup")
				}
				sameSnapshots(t, before, f.snapshot())
				if !strings.Contains(errb, "migrate") {
					t.Fatalf("stderr should say how to migrate later: %q", errb)
				}
			})
		}
	}

	t.Run("the backup path is shown before the consent is read", func(t *testing.T) {
		f, m, _ := setup(t, []string{"7", "n"}, nil)
		m.run()
		events := f.log.list()
		shownAt, answerAt := -1, -1
		for i, e := range events {
			if strings.HasPrefix(e, "out:") && strings.Contains(e, f.nodesPath+".bak") && shownAt < 0 {
				shownAt = i
			}
			if e == "input:2" {
				answerAt = i
			}
		}
		if shownAt < 0 || answerAt < 0 || shownAt > answerAt {
			t.Fatalf("backup path %s.bak must be printed before the answer (shown=%d answer=%d): %v", f.nodesPath, shownAt, answerAt, events)
		}
	})

	t.Run("accepted: migrate under the default lock with the existing .bak semantics, then the action runs", func(t *testing.T) {
		f, m, acquired := setup(t, []string{"7", "y", "y"}, nil)
		code, out, errb := m.run()
		if code != 0 {
			t.Fatalf("code=%d out=%s err=%s", code, out, errb)
		}
		if *acquired != 1 {
			t.Fatalf("default-nodes.conf lock acquisitions for the migration = %d, want 1", *acquired)
		}
		if len(m.dispatched) != 2 || m.dispatchedLine(0) != "migrate" || m.dispatchedLine(1) != "publish" {
			t.Fatalf("dispatched %v, want migrate then publish", m.dispatched)
		}
		bak, err := os.ReadFile(f.nodesPath + ".bak")
		if err != nil || string(bak) != legacyNodesConf {
			t.Fatalf("backup = %q, %v", bak, err)
		}
		if _, format, _ := nodes.Load(f.nodesPath); format != nodes.FormatSectioned {
			t.Fatalf("nodes.conf is not sectioned after the migration")
		}
		// The migration is a separate confirmed step: the L1+L2 publish lock
		// comes only after both answers.
		events := f.log.list()
		last := -1
		lockAt := -1
		for i, e := range events {
			if e == "input:3" {
				last = i
			}
			if strings.HasPrefix(e, "lock:") && lockAt < 0 {
				lockAt = i
			}
		}
		if last < 0 || lockAt < last {
			t.Fatalf("publish lock before the final answer: %v", events)
		}
		b, _ := os.ReadFile(filepath.Join(f.tokenDir, "clash.yaml"))
		if !strings.Contains(string(b), "JP") {
			t.Fatalf("clash.yaml not published:\n%s", b)
		}
	})

	t.Run("migration failure keeps the original and never publishes", func(t *testing.T) {
		failLock := func(context.Context, hostlock.Paths, hostlock.AcquireOptions, ...hostlock.Level) (*hostlock.Lease, error) {
			return nil, errors.New("lock timeout")
		}
		f, m, _ := setup(t, []string{"7", "y", "y", "jp1"}, failLock)
		before := f.snapshot()
		code, _, errb := m.run()
		if code != 1 {
			t.Fatalf("code=%d, want the migrate failure code 1; err=%s", code, errb)
		}
		if len(m.dispatched) != 1 || m.dispatchedLine(0) != "migrate" {
			t.Fatalf("only the migration may run: %v", m.dispatched)
		}
		for _, e := range f.log.list() {
			if e == "runtime" || strings.HasPrefix(e, "lock:") {
				t.Fatalf("a failed migration must not be followed by a publish: %v", f.log.list())
			}
		}
		if m.in.consumed() != 2 {
			t.Fatalf("no prompt after the failed migration: consumed %d", m.in.consumed())
		}
		sameSnapshots(t, before, f.snapshot())
		if b, _ := os.ReadFile(f.nodesPath); string(b) != legacyNodesConf {
			t.Fatalf("original nodes.conf changed: %q", b)
		}
	})

	t.Run("non-interactive commands still refuse to migrate automatically", func(t *testing.T) {
		f, m, acquired := setup(t, []string{"y", "y"}, nil)
		m.env.stdinIsTTY = func() bool { return false }
		before := f.snapshot()
		var out, errb bytes.Buffer
		if code := run(realityAddArgs(), &out, &errb); code != 1 {
			t.Fatalf("code=%d, want 1; err=%s", code, errb.String())
		}
		if !strings.Contains(errb.String(), "migrate") || *acquired != 0 {
			t.Fatalf("expected a migrate hint and no migration: err=%q locks=%d", errb.String(), *acquired)
		}
		sameSnapshots(t, before, f.snapshot())
		// ... and a bare `node` without a TTY is only the usage text.
		orig := newNodeMenuEnv
		newNodeMenuEnv = func() nodeMenuEnv { return m.env }
		defer func() { newNodeMenuEnv = orig }()
		out.Reset()
		errb.Reset()
		if code := run([]string{"node"}, &out, &errb); code != 2 || m.in.consumed() != 0 {
			t.Fatalf("code=%d consumed=%d", code, m.in.consumed())
		}
		sameSnapshots(t, before, f.snapshot())
	})
}

// --- production helpers ---

func TestFetchPublicIPv4(t *testing.T) {
	serve := func(body string, status int) *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(status)
			io.WriteString(w, body)
		}))
	}
	t.Run("a public IPv4 is returned", func(t *testing.T) {
		s := serve("203.0.113.7\n", 200)
		defer s.Close()
		ip, err := fetchPublicIPv4(context.Background(), s.Client(), s.URL)
		if err != nil || ip != "203.0.113.7" {
			t.Fatalf("ip=%q err=%v", ip, err)
		}
	})
	for _, bad := range []string{"", "not an ip", "2001:db8::1", "10.0.0.1", "127.0.0.1", "169.254.169.254", "192.168.1.1", "0.0.0.0", strings.Repeat("1", 100)} {
		t.Run("rejects "+bad, func(t *testing.T) {
			s := serve(bad, 200)
			defer s.Close()
			if ip, err := fetchPublicIPv4(context.Background(), s.Client(), s.URL); err == nil {
				t.Fatalf("accepted %q as %q", bad, ip)
			}
		})
	}
	t.Run("rejects a non-200 answer", func(t *testing.T) {
		s := serve("203.0.113.7", 503)
		defer s.Close()
		if _, err := fetchPublicIPv4(context.Background(), s.Client(), s.URL); err == nil {
			t.Fatal("503 accepted")
		}
	})
	t.Run("a cancelled context stops the request without waiting", func(t *testing.T) {
		started := make(chan struct{})
		release := make(chan struct{})
		s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			close(started)
			<-release
		}))
		defer s.Close()
		defer close(release)
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan error, 1)
		go func() {
			_, err := fetchPublicIPv4(ctx, s.Client(), s.URL)
			done <- err
		}()
		<-started
		cancel()
		if err := <-done; err == nil {
			t.Fatal("expected an error after cancellation")
		}
	})
}
