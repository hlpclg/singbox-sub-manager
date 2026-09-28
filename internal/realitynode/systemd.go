// Package realitynode: systemd.go implements the production
// internal/transaction.Systemd surface (§7.3/§7.4's systemd judgement and
// recovery, driven through systemctl with spec §13.2's "每次 systemctl 调用
// 15 秒") and spec §9.2's local gate ("本机门禁"), exposed as a
// transaction.Gate constructor so Task 7 can wire it into both the forward
// install/reinstall step and Recoverer.Gate (rollback re-validates against
// the pre-transaction manifest — spec: "回滚时目标 manifest 为事务前
// manifest", hence BinaryPath below is a func, not a fixed string).
package realitynode

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/hlpclg/singbox-sub-manager/internal/transaction"
)

// Systemctl is the production implementation of internal/transaction's
// Systemd interface, plus the extra reads (ActiveState, MainPID, Start)
// spec §9.2's gate and §8.1 step 6's forward install need. Every method
// applies systemctlTimeout (15s, spec §13.2) on top of ctx via Runner.
type Systemctl struct {
	// Runner executes "systemctl <args...>"; defaults to ExecRunner{}.
	Runner CommandRunner
}

func (s Systemctl) runner() CommandRunner {
	if s.Runner != nil {
		return s.Runner
	}
	return ExecRunner{}
}

func (s Systemctl) showValue(ctx context.Context, unit, property string) (string, error) {
	res := s.runner().Run(ctx, systemctlTimeout, "systemctl", "show", "-p", property, "--value", unit)
	if res.Err != nil {
		return "", fmt.Errorf("realitynode: systemctl show -p %s %s: %w", property, unit, res.Err)
	}
	if res.ExitCode != 0 {
		return "", fmt.Errorf("realitynode: systemctl show -p %s %s exited %d: %s", property, unit, res.ExitCode, res.Stderr)
	}
	return strings.TrimSpace(res.Stdout), nil
}

func (s Systemctl) runVerb(ctx context.Context, verb, unit string) error {
	res := s.runner().Run(ctx, systemctlTimeout, "systemctl", verb, unit)
	if res.Err != nil {
		return fmt.Errorf("realitynode: systemctl %s %s: %w", verb, unit, res.Err)
	}
	if res.ExitCode != 0 {
		return fmt.Errorf("realitynode: systemctl %s %s exited %d: %s", verb, unit, res.ExitCode, res.Stderr)
	}
	return nil
}

// LoadState implements transaction.Systemd (spec §7.4 step 3: "LoadState为
// loaded时,执行stop和disable").
func (s Systemctl) LoadState(ctx context.Context, unit string) (bool, error) {
	v, err := s.showValue(ctx, unit, "LoadState")
	if err != nil {
		return false, err
	}
	return v == "loaded", nil
}

// UnitFileState implements transaction.Systemd.
func (s Systemctl) UnitFileState(ctx context.Context, unit string) (string, error) {
	return s.showValue(ctx, unit, "UnitFileState")
}

// Stop implements transaction.Systemd.
func (s Systemctl) Stop(ctx context.Context, unit string) error {
	return s.runVerb(ctx, "stop", unit)
}

// SetUnitFileState implements transaction.Systemd: want is "enabled" or
// "disabled".
func (s Systemctl) SetUnitFileState(ctx context.Context, unit, want string) error {
	switch want {
	case "enabled":
		return s.runVerb(ctx, "enable", unit)
	case "disabled":
		return s.runVerb(ctx, "disable", unit)
	default:
		return fmt.Errorf("realitynode: unknown desired UnitFileState %q", want)
	}
}

// DaemonReload implements transaction.Systemd.
func (s Systemctl) DaemonReload(ctx context.Context) error {
	res := s.runner().Run(ctx, systemctlTimeout, "systemctl", "daemon-reload")
	if res.Err != nil {
		return fmt.Errorf("realitynode: systemctl daemon-reload: %w", res.Err)
	}
	if res.ExitCode != 0 {
		return fmt.Errorf("realitynode: systemctl daemon-reload exited %d: %s", res.ExitCode, res.Stderr)
	}
	return nil
}

// Restart implements transaction.Systemd.
func (s Systemctl) Restart(ctx context.Context, unit string) error {
	return s.runVerb(ctx, "restart", unit)
}

// Start is spec §8.1 step 6's forward "start" (install's first-ever start;
// reinstall uses Restart instead). It is not part of transaction.Systemd
// (recovery never needs to start-from-nothing — a rollback either stops or
// restarts, per §7.4), but Task 7's forward install step needs it, and it
// belongs next to the rest of this systemctl surface.
func (s Systemctl) Start(ctx context.Context, unit string) error {
	return s.runVerb(ctx, "start", unit)
}

// ActiveState reads the unit's current ActiveState (spec §9.2's gate,
// §7.2's judgement for systemd_start/restart/stop — where ActiveState is
// explicitly "不作为篡改依据", i.e. read for the gate, not for tamper
// judgement).
func (s Systemctl) ActiveState(ctx context.Context, unit string) (string, error) {
	return s.showValue(ctx, unit, "ActiveState")
}

// MainPID reads the unit's current MainPID (spec §9.2's gate). It returns
// 0 when the unit reports no main PID (e.g. inactive).
func (s Systemctl) MainPID(ctx context.Context, unit string) (int, error) {
	v, err := s.showValue(ctx, unit, "MainPID")
	if err != nil {
		return 0, err
	}
	pid, perr := strconv.Atoi(v)
	if perr != nil {
		return 0, fmt.Errorf("realitynode: systemctl show -p MainPID %s: unexpected value %q", unit, v)
	}
	return pid, nil
}

var _ transaction.Systemd = Systemctl{}

// --- §9.2 gate ---

// ProcReader abstracts the /proc reads spec §9.2's gate needs, injectable
// so tests never touch a real /proc — production is procfsReader (Linux
// only, see systemd_linux.go); a non-Linux build gets a stub that returns a
// clear error (systemd_other.go) rather than a silently-wrong answer.
type ProcReader interface {
	// TCPListenInode returns the inode of the IPv4 TCP socket in LISTEN
	// state bound to 0.0.0.0:port (spec §9.2: "0.0.0.0:<listen_port>上有
	// TCP监听" — IPv4 only, matching the service config's own
	// listen: "0.0.0.0"), or ok=false if there is none.
	TCPListenInode(port int) (inode uint64, ok bool, err error)
	// PIDHasSocketInode reports whether pid currently holds an open file
	// descriptor for the given socket inode (spec §9.2: "监听socket属于
	// unit的MainPID").
	PIDHasSocketInode(pid int, inode uint64) (bool, error)
	// Exe returns the resolved target of /proc/<pid>/exe (spec §9.2:
	// "/proc/<MainPID>/exe等于目标manifest中当前版本的私有二进制路径").
	Exe(pid int) (string, error)
}

// Clock abstracts time so spec §9.2's up-to-10-second poll can be tested
// deterministically without real sleeping (AGENTS.md: "假时钟验证时间边
// 界，禁止真实等待10/30/120秒充当单元测试"). Production is realClock; tests
// inject a fake whose Sleep advances virtual time instantly.
type Clock interface {
	Now() time.Time
	Sleep(d time.Duration)
}

type realClock struct{}

func (realClock) Now() time.Time        { return time.Now() }
func (realClock) Sleep(d time.Duration) { time.Sleep(d) }

// gatePollBudget is spec §9.2's "最多轮询10秒".
const gatePollBudget = 10 * time.Second

// gatePollInterval is this Task's own implementation choice for how often
// to re-check within the 10-second budget (the spec fixes only the overall
// budget, not a polling cadence).
const gatePollInterval = 100 * time.Millisecond

// GateConfig closes over everything BuildGate's returned transaction.Gate
// needs at call time.
type GateConfig struct {
	// Systemd supplies ActiveState/MainPID; defaults to Systemctl{} when
	// zero-valued is not possible here (interface), so callers must set it
	// — kept as an interface (not the concrete Systemctl) so tests can
	// inject a fake independent of ProcReader/Runner.
	Systemd interface {
		ActiveState(ctx context.Context, unit string) (string, error)
		MainPID(ctx context.Context, unit string) (int, error)
	}
	Unit       string
	ListenPort int
	ConfigPath string
	// BinaryPath returns the target manifest's current private sing-box
	// binary path at call time — a func rather than a fixed string so the
	// same Gate shape can be reused across rollback, where the target is
	// the pre-transaction manifest's binary (spec §7.4 step 5), without
	// rebuilding GateConfig.
	BinaryPath func() string
	Proc       ProcReader
	Runner     CommandRunner
	Clock      Clock

	// PollBudget and PollInterval override gatePollBudget/gatePollInterval
	// (both zero-valued fields use the real 10-second/100-millisecond
	// defaults). Tests use small overrides to keep BuildGate's aggregate-
	// budget enforcement (review finding F2) fast without needing a fake
	// clock for that particular property.
	PollBudget   time.Duration
	PollInterval time.Duration
}

func (c GateConfig) clock() Clock {
	if c.Clock != nil {
		return c.Clock
	}
	return realClock{}
}

func (c GateConfig) proc() ProcReader {
	if c.Proc != nil {
		return c.Proc
	}
	return procfsReader{}
}

func (c GateConfig) runner() CommandRunner {
	if c.Runner != nil {
		return c.Runner
	}
	return ExecRunner{}
}

func (c GateConfig) pollBudget() time.Duration {
	if c.PollBudget > 0 {
		return c.PollBudget
	}
	return gatePollBudget
}

func (c GateConfig) pollInterval() time.Duration {
	if c.PollInterval > 0 {
		return c.PollInterval
	}
	return gatePollInterval
}

// BuildGate implements spec §9.2 as a transaction.Gate: poll (up to 10
// seconds) for ActiveState=active, a TCP listener on 0.0.0.0:<listen_port>
// owned by the unit's MainPID, and that MainPID's /proc/<pid>/exe matching
// the target binary; once all four hold, run "sing-box check -c
// <configPath>" once (its own execTimeout, spec §13.2, outside the poll
// budget) and require exit 0.
//
// Each attempt's own systemd calls (ActiveState/MainPID) are bounded by
// however much of the poll budget remains, not by their own full 15-second
// systemctl timeout in isolation (review finding F2): without this, a
// single hung systemd call could by itself consume up to 15 seconds —
// well past this gate's overall 10-second budget, and past the 120-second
// independent-rollback budget's own accounting (spec §13.2, which assumes
// this gate finishes within its stated figure).
func BuildGate(cfg GateConfig) transaction.Gate {
	return func(ctx context.Context) error {
		clock := cfg.clock()
		budget := cfg.pollBudget()
		interval := cfg.pollInterval()
		deadline := clock.Now().Add(budget)
		var lastErr error
		for {
			remaining := deadline.Sub(clock.Now())
			if remaining <= 0 {
				return fmt.Errorf("realitynode: gate did not become healthy within %s: %w", budget, lastErr)
			}
			attemptCtx, cancel := context.WithTimeout(ctx, remaining)
			err := gateAttempt(attemptCtx, cfg)
			cancel()
			if err == nil {
				lastErr = nil
				break
			}
			lastErr = err
			if !clock.Now().Before(deadline) {
				return fmt.Errorf("realitynode: gate did not become healthy within %s: %w", budget, lastErr)
			}
			select {
			case <-ctx.Done():
				return ctx.Err()
			default:
			}
			clock.Sleep(interval)
		}
		binPath := cfg.BinaryPath()
		return VerifyConfig(ctx, cfg.runner(), binPath, cfg.ConfigPath)
	}
}

func gateAttempt(ctx context.Context, cfg GateConfig) error {
	active, err := cfg.Systemd.ActiveState(ctx, cfg.Unit)
	if err != nil {
		return err
	}
	if active != "active" {
		return fmt.Errorf("unit %s ActiveState=%q, want active", cfg.Unit, active)
	}

	pid, err := cfg.Systemd.MainPID(ctx, cfg.Unit)
	if err != nil {
		return err
	}
	if pid <= 0 {
		return fmt.Errorf("unit %s has no MainPID", cfg.Unit)
	}

	proc := cfg.proc()
	inode, ok, err := proc.TCPListenInode(cfg.ListenPort)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("no TCP listener on 0.0.0.0:%d", cfg.ListenPort)
	}

	owns, err := proc.PIDHasSocketInode(pid, inode)
	if err != nil {
		return err
	}
	if !owns {
		return fmt.Errorf("listening socket on port %d is not owned by MainPID %d", cfg.ListenPort, pid)
	}

	exe, err := proc.Exe(pid)
	if err != nil {
		return err
	}
	want := cfg.BinaryPath()
	if exe != want {
		return fmt.Errorf("MainPID %d exe %q does not match target binary %q", pid, exe, want)
	}
	return nil
}
