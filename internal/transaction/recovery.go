// Package transaction: recovery.go implements design spec §7.4-§7.6 —
// automatic rollback, `service recover` / `publish --recover`, the
// "abandon" manual escape hatch, committed-phase forward cleanup, and
// tombstone deletion — shared by every host-side lifecycle operation and by
// subscription-center publish (Task 5, per the plan). It builds entirely on
// Task 4's journal (journal.go), safe-path primitives (paths_unix.go) and
// forward step execution plus the rollback-step variants added to
// steps.go: this file never touches a raw path string and never bypasses
// the journal's persist-before-execute discipline.
//
// This package remains agnostic to *which* concrete paths, trusted roots or
// systemd unit a given transaction manages — that is domain knowledge only
// the caller (Task 6+'s internal/realitynode for instance lifecycle,
// internal/publish for subscription publish) has. A caller supplies that
// knowledge through PathResolver (target key -> ManagedPath), a Systemd
// implementation (nil when the operation never registers systemd steps,
// e.g. publish) and a Gate function (the post-restart self-check, built by
// the caller around whatever pre-transaction manifest it already has in
// hand).
package transaction

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

// Outcome distinguishes how a Recoverer method concluded. Mapping an
// Outcome (and the sentinel errors below) to a process exit code is left to
// the CLI (Task 6-9, per the plan: "引擎返回可区分的结果/错误类型,CLI映射属
// 于后续Task") — the boundary each maps to under §7.4 is documented for
// convenience only.
type Outcome int

const (
	// OutcomeNoChange: the transaction area was deleted (§7.6) before any
	// managed path had been modified (§7.4's "第一个受管路径被修改之前" —
	// staging directories are never counted, see Step.Provisional). Maps to
	// exit code 1 in the automatic-rollback-on-failure context; a bare
	// `recover` finding nothing to do also reports this.
	OutcomeNoChange Outcome = iota
	// OutcomeRolledBack: a rollback ran to completion just now (whether it
	// was started by this call or was already in progress). Maps to exit
	// code 4 (automatic) or 0 (`recover`).
	OutcomeRolledBack
	// OutcomeCommitted: the journal was already (or, mid-run, became)
	// phase=committed; forward cleanup completed and the transaction area
	// was deleted. Maps to exit code 0.
	OutcomeCommitted
	// OutcomeAbandoned: Abandon succeeded — every managed path was already
	// at its pre-transaction state, so the transaction area was deleted
	// without touching systemd. Maps to exit code 0.
	OutcomeAbandoned
)

func (o Outcome) String() string {
	switch o {
	case OutcomeNoChange:
		return "no_change"
	case OutcomeRolledBack:
		return "rolled_back"
	case OutcomeCommitted:
		return "committed"
	case OutcomeAbandoned:
		return "abandoned"
	default:
		return fmt.Sprintf("Outcome(%d)", int(o))
	}
}

var (
	// ErrTampered reports that §7.3's judgement found at least one managed
	// path, or the set of registered temp names, in a state outside every
	// legal recovery outcome for the journal's current phase — §7.4's
	// "判定为外部篡改": nothing is modified, the transaction area is
	// preserved, and the CLI boundary is exit code 5.
	ErrTampered = errors.New("transaction: recovery detected a state outside every legal outcome for this journal's phase")
	// ErrAbandonNotEligible reports that Abandon's precondition (every
	// managed path already at its pre-transaction state, no unregistered
	// temp names) does not hold. No modification is made; the CLI boundary
	// is exit code 1.
	ErrAbandonNotEligible = errors.New("transaction: abandon requires every managed path to already be at its pre-transaction state")
	// ErrGateFailed reports that §7.4 step 5's post-restart self-check
	// (the caller-supplied Gate, run with the pre-transaction manifest)
	// failed. The transaction area is preserved; the CLI boundary is exit
	// code 5 (§10.4: "本地状态不符" for publish's analogous case; for
	// instance lifecycle this is "回滚后 unit 恢复了但自检不过,需要人工介
	// 入").
	ErrGateFailed = errors.New("transaction: post-restart gate check failed")
	// ErrRollbackTimeout reports that RecoverDetached's rollback budget
	// (§13.2: 120 seconds) expired before rollback finished. The
	// transaction area is preserved; the CLI boundary is exit code 5.
	ErrRollbackTimeout = errors.New("transaction: rollback did not finish within its independent budget")
)

// DefaultRollbackBudget is §13.2's fixed 120-second independent rollback
// budget, used by RecoverDetached when Recoverer.RollbackBudget is zero.
const DefaultRollbackBudget = 120 * time.Second

// PathResolver maps a journal Target key (as recorded in Step.Target, and
// in the same key-space as Journal.DirIDs's ancestor-level keys) to the
// ManagedPath needed to operate on it. The caller is the only party that
// knows the concrete trusted roots and directory layout for a given
// operation; this package stays agnostic to both.
type PathResolver interface {
	Resolve(target string) (ManagedPath, error)
}

// Systemd is the systemd surface recovery needs to drive §7.4 steps 3 and
// 5. unit is always the bare unit file name (e.g. "proxyctl-reality.
// service"), extracted from a systemd_* step's own Target. Every method
// must apply its own §13.2 per-call timeout and reap any child process it
// starts; Recoverer only supplies ctx (derived from the rollback budget)
// and expects each call to return promptly once ctx is done.
type Systemd interface {
	// LoadState reports whether the unit is currently loaded (§7.4 step 3:
	// "LoadState为loaded时,执行stop和disable").
	LoadState(ctx context.Context, unit string) (loaded bool, err error)
	// UnitFileState reads the unit's current UnitFileState (e.g.
	// "enabled", "disabled", or "" when the unit file itself does not
	// exist) — §7.3's systemd judgement needs this live read the same way
	// LiveFileState/LiveDirState need a live filesystem read for any other
	// managed path; recovery never assumes systemd's state from the
	// journal alone.
	UnitFileState(ctx context.Context, unit string) (string, error)
	Stop(ctx context.Context, unit string) error
	// SetUnitFileState brings the unit's UnitFileState to want ("enabled"
	// or "disabled") — the abstraction over systemd's separate enable/
	// disable commands that §7.4 steps 3 and 5 both need generically
	// ("确保...也不再被enable", "把UnitFileState恢复到unit_state_before").
	SetUnitFileState(ctx context.Context, unit, want string) error
	DaemonReload(ctx context.Context) error
	Restart(ctx context.Context, unit string) error
}

// Gate re-validates the running service after a restart, using whatever
// pre-transaction manifest or config the caller closed over (§7.4 step 5:
// "用事务前的manifest重新跑§9.2本机门禁"). A Recoverer whose journal never
// registers systemd steps (e.g. publish) never calls this and may leave it
// nil.
type Gate func(ctx context.Context) error

// Recoverer drives §7.4-§7.6 against a single already-open Engine, sharing
// Task 4's forward primitives (and this package's rollback-step variants)
// for every actual filesystem mutation.
type Recoverer struct {
	Engine *Engine

	// Root is the trusted root the transaction area itself lives directly
	// under (e.g. "/var/lib" for an instance transaction, dirname(
	// subscription_root) for a publish transaction) — distinct from any
	// managed *path's* own trusted root, which PathResolver supplies per
	// target. deleteTxnArea renames the transaction area to TombstoneName
	// under this same root.
	Root *TrustedRoot
	// TombstoneName is the transaction area's already-decided tombstone
	// name under Root (§7.6), computed by the caller from its own fixed
	// naming convention and this journal's TxnID (e.g.
	// ".proxyctl-reality-txn.tomb-<txn_id>").
	TombstoneName string

	// Resolve maps every Step.Target this journal uses to the ManagedPath
	// needed to operate on it.
	Resolve PathResolver
	// SnapDir is the transaction area's own snap/ directory, already open
	// — needed only when some rollback step restores file content from a
	// completed snapshot.
	SnapDir *os.File

	// Systemd and Gate are nil when this journal never registers a
	// systemd_* step (publish never does).
	Systemd Systemd
	Gate    Gate

	// RollbackBudget overrides DefaultRollbackBudget for RecoverDetached,
	// mainly so a test can use a short budget instead of waiting 120
	// seconds for real.
	RollbackBudget time.Duration
}

func (r *Recoverer) rollbackBudget() time.Duration {
	if r.RollbackBudget > 0 {
		return r.RollbackBudget
	}
	return DefaultRollbackBudget
}

// commitJournal persists next as the engine's new current journal, reusing
// the same Store.Update every forward/rollback primitive already goes
// through.
func (r *Recoverer) commitJournal(next *Journal) error {
	return r.Engine.store.Update(next)
}

// --- Entry points ---

// Recover resumes whatever phase the journal currently records — running
// (validate, then build and start a rollback), rolling_back (continue it)
// or committed (forward cleanup only) — per §7.4-§7.6. It is the single
// engine-level routine behind both "a forward-executing caller's ctx or a
// step just failed, roll back now" and "service recover / publish
// --recover found a leftover transaction area": §7.4 explicitly runs "同一
// 流程" for both, the only difference being which exit code the CLI maps
// the result to. ctx should already carry whatever budget applies (see
// RecoverDetached for the independent-rollback-after-cancellation case).
func (r *Recoverer) Recover(ctx context.Context) (Outcome, error) {
	j := r.Engine.Journal()
	switch j.Phase {
	case PhaseCommitted:
		if err := r.forwardCleanup(ctx); err != nil {
			return 0, err
		}
		if err := r.deleteTxnArea(ctx); err != nil {
			return 0, err
		}
		return OutcomeCommitted, nil

	case PhaseRunning:
		if err := r.validateRunning(ctx); err != nil {
			return 0, err
		}
		// noneModified only decides which Outcome this call reports — it
		// never skips beginRollback/continueRollback. A journal where
		// nothing but a Provisional staging step (or a snapshot) ever
		// advanced still needs that staging content actually removed
		// (§10.3 step 5 / §10.4: "删除staging"), via the same registered,
		// ancestor-verified steps as any other rollback — beginRollback
		// already builds exactly that (it processes every non-systemd
		// target, Provisional or not) and produces zero rollback steps
		// when there is truly nothing to undo, converging to the same
		// plain "delete the transaction area" outcome either way.
		outcome := OutcomeRolledBack
		if noneModified(j) {
			outcome = OutcomeNoChange
		}
		if err := r.beginRollback(ctx); err != nil {
			return 0, err
		}
		if err := r.continueRollback(ctx); err != nil {
			return 0, err
		}
		return outcome, nil

	case PhaseRollingBack:
		if err := r.validateRollingBack(ctx); err != nil {
			return 0, err
		}

	default:
		return 0, fmt.Errorf("%w: unknown phase %q", ErrTampered, j.Phase)
	}

	if err := r.continueRollback(ctx); err != nil {
		return 0, err
	}
	return OutcomeRolledBack, nil
}

// RecoverDetached implements §13.3's "取消后的独立回滚使用独立context,不受
// 主context取消影响": it derives a fresh context from parent's values but
// not its cancellation, bounds it to the rollback budget (§13.2: 120
// seconds, DefaultRollbackBudget, overridable via Recoverer.RollbackBudget
// for tests), and calls Recover with that instead. A caller whose own
// operation just failed or whose main ctx was just canceled mid-transaction
// uses this; a standalone `recover`/`--recover` invocation, which is not
// itself time-boxed by §13.2, calls Recover directly with its own context.
func (r *Recoverer) RecoverDetached(parent context.Context) (Outcome, error) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(parent), r.rollbackBudget())
	defer cancel()
	outcome, err := r.Recover(ctx)
	if err != nil && errors.Is(err, context.DeadlineExceeded) {
		return outcome, fmt.Errorf("%w: %w", ErrRollbackTimeout, err)
	}
	return outcome, err
}

// Abandon implements §7.4's "人工放弃出口" (`service recover --abandon` /
// `publish --recover --abandon`): it first requires every managed path
// (and systemd chain) to be in a state §7.3 recognizes as legal for the
// current phase — a real ErrTampered condition regardless of context, via
// the same per-path judgement Recover uses (judgePathsRunning /
// judgePathsRollingBack), deliberately *without* Recover's own
// unregistered-temp scan: unlike Recover, a stray temp name here is
// Abandon's own eligibility question (mapped to ErrAbandonNotEligible, not
// ErrTampered — see allAtPreTransactionState below), not tampering. It then
// requires every managed path to already be at its pre-transaction state
// (the forward chain's first step's Pre — not the rollback chain's own
// Pre) with no unregistered temp names, per §7.4: "只有在全部受管路径都已
// 处于事务前状态...且没有未登记临时名时才可用". On success it deletes the
// transaction area, leaving systemd exactly as it currently is (never
// restarting or gating). Committed transactions are never eligible.
func (r *Recoverer) Abandon(ctx context.Context) (Outcome, error) {
	j := r.Engine.Journal()
	switch j.Phase {
	case PhaseCommitted:
		return 0, fmt.Errorf("%w: transaction is already committed", ErrAbandonNotEligible)
	case PhaseRunning:
		if err := r.judgePathsRunning(ctx); err != nil {
			return 0, err
		}
	case PhaseRollingBack:
		if err := r.judgePathsRollingBack(ctx); err != nil {
			return 0, err
		}
	default:
		return 0, fmt.Errorf("%w: unknown phase %q", ErrTampered, j.Phase)
	}

	eligible, err := r.allAtPreTransactionState()
	if err != nil {
		return 0, err
	}
	if !eligible {
		return 0, ErrAbandonNotEligible
	}
	if err := r.deleteTxnArea(ctx); err != nil {
		return 0, err
	}
	return OutcomeAbandoned, nil
}

// --- §7.3 validation over every managed path ---

// noneModified reports §7.4's "第一个受管路径被修改之前" boundary: every
// step that actually touches a managed path's own content/existence
// (write_file/mkdir/set_meta/remove_file/remove_dir) or the host's
// systemd state (any systemd_* step) is still pending. Only two kinds of
// step never count toward this boundary, even when done or started,
// because neither ever changes anything outside the transaction's own
// control:
//   - Provisional steps (publish's own staging directory, §10.3: "staging
//     不计入修改边界") — they touch only the transaction's own
//     not-yet-committed working area.
//   - snapshot steps — §7.2: "snapshot从不修改受管路径本身"; it only
//     copies into the transaction area's own snap/, never the target.
//
// systemd_* steps DO count (review round 2, N1): uninstall's shape stops
// and disables the unit before deleting any file, so a crash there has
// already changed host state -- reporting "nothing was modified" (and,
// worse, skipping the systemd convergence continueRollback would
// otherwise run) would leave a stopped service unrecovered. See
// continueRollback's systemdTouched gate, which must stay consistent with
// this function: a step this function treats as "modified" must be one
// continueRollback will actually converge.
func noneModified(j *Journal) bool {
	for _, s := range j.Steps {
		if s.Provisional || s.Kind == KindSnapshot {
			continue
		}
		if s.Status != StatusPending {
			return false
		}
	}
	return true
}

// distinctTargets returns every distinct Step.Target in steps, in first-
// appearance (execution) order.
func distinctTargets(steps []Step) []string {
	seen := make(map[string]bool, len(steps))
	var out []string
	for _, s := range steps {
		if !seen[s.Target] {
			seen[s.Target] = true
			out = append(out, s.Target)
		}
	}
	return out
}

// chainIsDirectory reports whether chain's target is a directory: any
// mkdir/remove_dir step, or any step whose Pre/Post records Type=dir
// (set_meta acting on a directory), settles it.
func chainIsDirectory(chain []Step) bool {
	for _, s := range chain {
		switch s.Kind {
		case KindMkdir, KindRemoveDir:
			return true
		}
		if s.Pre.Type == PathTypeDir || s.Post.Type == PathTypeDir {
			return true
		}
	}
	return false
}

// liveStateFor reads target's current on-disk state via mp, matching
// whichever content-identity field chain's steps use (chainContentKind) —
// the single entry point validateRunning/validateRollingBack/beginRollback/
// allAtPreTransactionState all use to get a fresh, comparable PathState.
func (r *Recoverer) liveStateFor(mp ManagedPath, chain []Step) (live PathState, empty bool, err error) {
	if chainIsDirectory(chain) {
		return r.Engine.LiveDirState(mp)
	}
	wantHash, wantSchema := chainContentKind(chain)
	live, err = r.Engine.LiveFileState(mp, wantHash, wantSchema)
	return live, false, err
}

// containingDirKey returns the dir_ids-key-space identity of the directory
// containing mp's own leaf — mp.RootKey when mp has no ancestor components,
// otherwise the AncestorKey of its last one. This is always a key
// recoverAncestorException/dir_ids already knows how to judge, since it is
// exactly the parent resolveVerifiedParent itself resolves for mp.
func containingDirKey(mp ManagedPath) string {
	if len(mp.Comps) == 0 {
		return mp.RootKey
	}
	return mp.AncestorKey(len(mp.Comps) - 1)
}

// forEachManagedScanDir calls fn once for every directory recovery must
// scan for stray ".proxyctl-txn-*" files (§7.3's "未登记临时名的检查只扫描
// 受管路径所在的目录") — derived entirely from the journal's own managed
// paths via Resolve and the current dir_ids, never from a caller-supplied,
// possibly-stale list: every non-systemd target's own containing
// directory, plus (for a target whose chain is itself a directory —
// mkdir/remove_dir) that directory's own contents. Every directory is
// resolved and opened fresh, ancestor-verified against the *current*
// dir_ids, at the moment of this call — never a long-held fd from earlier
// in the same recovery pass, which an intervening remove_dir could have
// invalidated. A directory that resolveVerifiedParentEx's tolerated
// absence exception applies to (§7.1's crash-recovery carve-outs — see
// recoveryAncestorException), or a directory-kind target that does not
// currently exist, has nothing to scan and fn is simply not called for
// it: that is not tampering, dir_ids already accounts for the gap.
func (r *Recoverer) forEachManagedScanDir(j *Journal, fn func(dir *os.File) error) error {
	seen := map[string]bool{}
	scanOne := func(mp ManagedPath, key string, wantLeafDir bool) error {
		if seen[key] {
			return nil
		}
		seen[key] = true
		parent, opened, err := resolveVerifiedParentEx(mp, j.DirIDs, nil, r.Engine.recoveryAncestorException)
		if err != nil {
			closeAll(opened)
			if isAncestorLegitimatelyAbsent(err) {
				return nil
			}
			return fmt.Errorf("%w: %w", ErrTampered, err)
		}
		defer closeAll(opened)
		if !wantLeafDir {
			return fn(parent)
		}
		child, err := openChildDir(parent, mp.Leaf)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				return nil
			}
			return fmt.Errorf("%w: %w", ErrTampered, err)
		}
		defer child.Close()
		return fn(child)
	}
	for _, target := range distinctTargets(j.Steps) {
		chain := PathChain(j.Steps, target)
		if chain[0].Kind.isSystemdKind() {
			continue
		}
		mp, err := r.Resolve.Resolve(target)
		if err != nil {
			return fmt.Errorf("%w: resolve %s: %w", ErrTampered, target, err)
		}
		if err := scanOne(mp, containingDirKey(mp), false); err != nil {
			return err
		}
		if chainIsDirectory(chain) {
			if err := scanOne(mp, mp.TargetKey(), true); err != nil {
				return err
			}
		}
	}
	return nil
}

// checkUnregisteredTemps requires every managed path's own containing
// directory (forEachManagedScanDir) to contain no ".proxyctl-txn-*" entry
// outside the journal's currently registered temp names (§7.3's "未登记临
// 时名的检查").
func (r *Recoverer) checkUnregisteredTemps() error {
	j := r.Engine.Journal()
	return r.forEachManagedScanDir(j, func(dir *os.File) error {
		stray, err := ScanUnregisteredTemps(dir, j)
		if err != nil {
			return fmt.Errorf("%w: %w", ErrTampered, err)
		}
		if len(stray) > 0 {
			return fmt.Errorf("%w: unregistered temp file(s) %v in %s", ErrTampered, stray, dir.Name())
		}
		return nil
	})
}

// systemdUnitFor extracts the bare unit name from a systemd-kind chain's
// shared Target (every step targeting one systemd unit shares the same
// Target string — see systemdUnit, which does the same extraction against
// a whole journal rather than a single chain).
func systemdUnitFor(target string) string {
	if idx := strings.LastIndex(target, "/"); idx >= 0 {
		return target[idx+1:]
	}
	return target
}

// judgeSystemdChain implements §7.3's "systemd状态" judgement for one
// systemd-kind chain: read the unit's live UnitFileState and apply the
// same DetermineRunningState chain rule §7.2 already defines for
// systemd_enable/disable (started ∈ {pre, post}; done ⇒ post; all pending
// ⇒ chain's first pre) — ActiveState is never inspected, matching §7.2's
// "ActiveState不作为篡改依据". judge=false (rolling_back with
// rollback_systemd ∈ {demoting, ...}) means §7.3 explicitly does not judge
// this chain at all right now; that is not "pass", it is "not applicable".
func (r *Recoverer) judgeSystemdChain(ctx context.Context, chain []Step, judge bool) error {
	if !judge {
		return nil
	}
	if r.Systemd == nil {
		return fmt.Errorf("%w: journal registers a systemd step for %s but Recoverer.Systemd is nil", ErrTampered, chain[0].Target)
	}
	unit := systemdUnitFor(chain[0].Target)
	liveState, err := r.Systemd.UnitFileState(ctx, unit)
	if err != nil {
		return fmt.Errorf("%w: read UnitFileState for %s: %w", ErrTampered, unit, err)
	}
	live := PathState{UnitFileState: liveState}
	if err := DetermineRunningState(chain, live, false); err != nil {
		return fmt.Errorf("%w: %w", ErrTampered, err)
	}
	return nil
}

// judgePathsRunning implements §7.3's phase=running judgement over every
// managed path, including systemd chains — but *not* the unregistered-temp
// scan (see validateRunning, which adds that). Split out so Abandon can
// run this same per-path legality check (a real ErrTampered condition
// regardless of context) while handling a stray temp name as its own
// eligibility question (§7.4: "否则返回1") rather than as tampering.
func (r *Recoverer) judgePathsRunning(ctx context.Context) error {
	j := r.Engine.Journal()
	for _, target := range distinctTargets(j.Steps) {
		chain := PathChain(j.Steps, target)
		if chain[0].Kind.isSystemdKind() {
			if err := r.judgeSystemdChain(ctx, chain, true); err != nil {
				return err
			}
			continue
		}
		mp, err := r.Resolve.Resolve(target)
		if err != nil {
			return fmt.Errorf("%w: resolve %s: %w", ErrTampered, target, err)
		}
		live, empty, err := r.liveStateFor(mp, chain)
		if err != nil {
			return fmt.Errorf("%w: %w", ErrTampered, err)
		}
		if err := DetermineRunningState(chain, live, empty); err != nil {
			return fmt.Errorf("%w: %w", ErrTampered, err)
		}
	}
	return nil
}

// validateRunning implements §7.3's phase=running judgement over every
// managed path (including systemd chains) plus the unregistered-temp scan
// — the "先校验" §7.4 step 1 performs before ever touching phase or any
// path.
func (r *Recoverer) validateRunning(ctx context.Context) error {
	if err := r.judgePathsRunning(ctx); err != nil {
		return err
	}
	return r.checkUnregisteredTemps()
}

// judgePathsRollingBack is validateRollingBack's per-path judgement
// without the unregistered-temp scan — see judgePathsRunning's identical
// reasoning for why Abandon needs this split.
func (r *Recoverer) judgePathsRollingBack(ctx context.Context) error {
	j := r.Engine.Journal()
	judgeSystemd := j.RollbackSystemd == RollbackSystemdNone
	for _, target := range distinctTargets(j.Steps) {
		fchain := PathChain(j.Steps, target)
		if fchain[0].Kind.isSystemdKind() {
			if err := r.judgeSystemdChain(ctx, fchain, judgeSystemd); err != nil {
				return err
			}
			continue
		}
		rchain := PathChain(j.RollbackSteps, target)
		mp, err := r.Resolve.Resolve(target)
		if err != nil {
			return fmt.Errorf("%w: resolve %s: %w", ErrTampered, target, err)
		}
		chainForContent := fchain
		if len(rchain) > 0 {
			chainForContent = append(append([]Step{}, fchain...), rchain...)
		}
		live, empty, err := r.liveStateFor(mp, chainForContent)
		if err != nil {
			return fmt.Errorf("%w: %w", ErrTampered, err)
		}
		if err := DetermineRollingBackState(rchain, fchain, live, empty); err != nil {
			return fmt.Errorf("%w: %w", ErrTampered, err)
		}
	}
	return nil
}

// validateRollingBack implements §7.3's phase=rolling_back judgement over
// every managed path (including systemd chains, judged only while
// rollback_systemd is still "none" — §7.3: "rollback_systemd为demoting或
// 之后的任何值:UnitFileState与ActiveState都不作为篡改依据") plus the
// unregistered-temp scan.
func (r *Recoverer) validateRollingBack(ctx context.Context) error {
	if err := r.judgePathsRollingBack(ctx); err != nil {
		return err
	}
	return r.checkUnregisteredTemps()
}

// allAtPreTransactionState reports whether every managed path is currently
// at its pre-transaction state (forward chain's first Pre) with no
// unregistered temp names — Abandon's precondition (§7.4).
func (r *Recoverer) allAtPreTransactionState() (bool, error) {
	j := r.Engine.Journal()
	for _, target := range distinctTargets(j.Steps) {
		fchain := PathChain(j.Steps, target)
		if fchain[0].Kind.isSystemdKind() {
			continue
		}
		mp, err := r.Resolve.Resolve(target)
		if err != nil {
			return false, fmt.Errorf("%w: resolve %s: %w", ErrTampered, target, err)
		}
		live, _, err := r.liveStateFor(mp, fchain)
		if err != nil {
			return false, fmt.Errorf("%w: %w", ErrTampered, err)
		}
		if live != fchain[0].Pre {
			return false, nil
		}
	}
	strayFound := false
	if err := r.forEachManagedScanDir(j, func(dir *os.File) error {
		stray, err := ScanUnregisteredTemps(dir, j)
		if err != nil {
			return fmt.Errorf("%w: %w", ErrTampered, err)
		}
		if len(stray) > 0 {
			strayFound = true
		}
		return nil
	}); err != nil {
		return false, err
	}
	if strayFound {
		return false, nil
	}
	return true, nil
}

// --- §7.4 step 2: constructing the rollback plan ---

// nextTempSeq returns the next unused temp-name sequence number for j's own
// txn_id, continuing past every sequence number already registered on any
// forward or rollback step — so a newly constructed rollback write_file
// step's temp name can never collide with one a forward step already
// registered.
func nextTempSeq(j *Journal) int {
	prefix := tempNamePrefix + j.TxnID + "-"
	max := 0
	scan := func(steps []Step) {
		for _, s := range steps {
			for _, t := range s.Temps {
				if n, ok := parseTempSeq(t, prefix); ok && n > max {
					max = n
				}
			}
		}
	}
	scan(j.Steps)
	scan(j.RollbackSteps)
	return max + 1
}

func parseTempSeq(name, prefix string) (int, bool) {
	s := strings.TrimPrefix(name, prefix)
	if s == name || s == "" {
		return 0, false
	}
	n := 0
	for _, c := range s {
		if c < '0' || c > '9' {
			return 0, false
		}
		n = n*10 + int(c-'0')
	}
	return n, true
}

func targetDepth(target string) int { return strings.Count(target, "/") }

// beginRollback implements §7.4 step 2: for every managed path with any
// non-pending forward step, compare its already-validated current live
// state against its pre-transaction state; a path already there gets no
// rollback step at all. Everything else gets exactly one rollback
// primitive taking it from its current live state directly to the
// pre-transaction state (never replaying intermediate forward versions),
// ordered so parent directories are created before their children and
// children are removed before their parents. The whole plan, plus
// phase=rolling_back, rollback_frontier and rollback_files_done=false, is
// persisted together as a single new journal version before any rollback
// modification runs (§7.1: "持久化之前不执行任何回滚修改").
func (r *Recoverer) beginRollback(ctx context.Context) error {
	if err := checkCancel(ctx); err != nil {
		return err
	}
	j := r.Engine.Journal()
	seq := nextTempSeq(j)
	var toRemove, toCreate []Step
	n := 0
	for _, target := range distinctTargets(j.Steps) {
		chain := PathChain(j.Steps, target)
		if chain[0].Kind.isSystemdKind() {
			continue // systemd rollback is §7.4 steps 3/5, not a rollback_steps entry
		}
		mp, err := r.Resolve.Resolve(target)
		if err != nil {
			return fmt.Errorf("%w: resolve %s: %w", ErrTampered, target, err)
		}
		isDir := chainIsDirectory(chain)
		live, _, err := r.liveStateFor(mp, chain)
		if err != nil {
			return fmt.Errorf("%w: %w", ErrTampered, err)
		}
		preTxn := chain[0].Pre
		if live == preTxn {
			continue // already at pre-transaction state: no rollback step
		}
		n++
		rs := Step{ID: fmt.Sprintf("rollback-%d", n), Target: target, Pre: live, Post: preTxn, Status: StatusPending}
		switch {
		case !preTxn.Exists:
			if isDir {
				rs.Kind = KindRemoveDir
			} else {
				rs.Kind = KindRemoveFile
			}
			toRemove = append(toRemove, rs)
		case isDir:
			// A directory whose pre-transaction state existed: either it
			// is currently missing (mkdir it back) or only its metadata
			// differs (set_meta).
			if !live.Exists {
				rs.Kind = KindMkdir
			} else {
				rs.Kind = KindSetMeta
			}
			toCreate = append(toCreate, rs)
		default:
			// A file whose pre-transaction state existed (content and/or
			// metadata must be restored from its completed snapshot).
			rs.Kind = KindWriteFile
			rs.Temps = []string{TempName(j.TxnID, seq)}
			seq++
			toCreate = append(toCreate, rs)
		}
	}
	sort.SliceStable(toRemove, func(i, k int) bool { return targetDepth(toRemove[i].Target) > targetDepth(toRemove[k].Target) })
	sort.SliceStable(toCreate, func(i, k int) bool { return targetDepth(toCreate[i].Target) < targetDepth(toCreate[k].Target) })
	rollbackSteps := append(toRemove, toCreate...)

	frontier := make([]StepStatus, len(j.Steps))
	for i, s := range j.Steps {
		frontier[i] = s.Status
	}

	nj := j.Clone()
	nj.Phase = PhaseRollingBack
	nj.RollbackFrontier = frontier
	nj.RollbackSteps = rollbackSteps
	nj.RollbackFilesDone = len(rollbackSteps) == 0
	return r.commitJournal(nj)
}

// --- §7.4 steps 3-6: driving rollback to completion ---

// journalHasSystemd reports whether the journal's own (forward, frozen
// once rolling_back begins) Steps register any systemd_* step at all —
// i.e. whether this operation's shape ever needs the demote/restore
// sequence in the first place (publish never registers one).
func journalHasSystemd(j *Journal) bool {
	for _, s := range j.Steps {
		if s.Kind.isSystemdKind() {
			return true
		}
	}
	return false
}

// systemdUnit extracts the bare unit name from the journal's own systemd_*
// step Target (e.g. "systemd/proxyctl-reality.service" -> "proxyctl-
// reality.service") — every systemd_* step in a given journal names the
// same single unit (§4's single-instance scope).
func systemdUnit(j *Journal) string {
	for _, s := range j.Steps {
		if s.Kind.isSystemdKind() {
			if idx := strings.LastIndex(s.Target, "/"); idx >= 0 {
				return s.Target[idx+1:]
			}
			return s.Target
		}
	}
	return ""
}

// continueRollback drives §7.4 steps 3 through 6 from wherever the journal
// currently stands (RollbackSystemd/RollbackFilesDone already reflect any
// prior partial attempt): systemd demotion, then file recovery (skipped
// entirely once RollbackFilesDone), then systemd restoration, then
// deleting the transaction area.
//
// Systemd is touched (demoteSystemd/restoreSystemd) whenever the journal
// registers any systemd step *and* the modification boundary was crossed
// at all (!noneModified) — §7.4 steps 3/5 and §14.2's F1 attach no further
// condition on the systemd steps' *own* status. Gating on "did the systemd
// step itself already start" (an earlier, incorrect version of this gate —
// plan Task 5 review round 3's P1) would skip the entire demote/restore
// sequence for a reinstall that already wrote the new binary (write_file
// done) while systemd_enable/restart were still pending, leaving a stale
// or half-restarted service unrecovered. noneModified is stable across
// repeated calls (it only inspects the frozen forward Steps, never
// RollbackSystemd/RollbackFilesDone), so this gate agrees on every
// recover invocation for a given journal, including a direct
// phase=rolling_back re-entry. A journal whose modification boundary was
// never crossed (noneModified — review round 2 N1) still makes zero
// systemd calls, even though it registers systemd steps.
func (r *Recoverer) continueRollback(ctx context.Context) error {
	j := r.Engine.Journal()
	runSystemd := journalHasSystemd(j) && !noneModified(j)
	if runSystemd {
		if err := r.demoteSystemd(ctx); err != nil {
			return err
		}
	}
	if !r.Engine.Journal().RollbackFilesDone {
		if err := r.recoverFiles(ctx); err != nil {
			return err
		}
	}
	if runSystemd {
		if err := r.restoreSystemd(ctx); err != nil {
			return err
		}
	}
	return r.deleteTxnArea(ctx)
}

// demoteSystemd implements §7.4 step 3. recover re-entering with
// RollbackSystemd already "demoted" or later skips it entirely; re-entering
// with "demoting" resumes it (Stop/SetUnitFileState are idempotent).
func (r *Recoverer) demoteSystemd(ctx context.Context) error {
	j := r.Engine.Journal()
	if j.RollbackSystemd != RollbackSystemdNone && j.RollbackSystemd != RollbackSystemdDemoting {
		return nil
	}
	unit := systemdUnit(j)
	if j.RollbackSystemd == RollbackSystemdNone {
		nj := j.Clone()
		nj.RollbackSystemd = RollbackSystemdDemoting
		if err := r.commitJournal(nj); err != nil {
			return err
		}
	}
	if err := checkCancel(ctx); err != nil {
		return err
	}
	loaded, err := r.Systemd.LoadState(ctx, unit)
	if err != nil {
		return err
	}
	if loaded {
		if err := r.Systemd.Stop(ctx, unit); err != nil {
			return err
		}
		if err := r.Systemd.SetUnitFileState(ctx, unit, "disabled"); err != nil {
			return err
		}
	}
	nj2 := r.Engine.Journal().Clone()
	nj2.RollbackSystemd = RollbackSystemdDemoted
	return r.commitJournal(nj2)
}

// restoreSystemd implements §7.4 step 5. recover re-entering with
// RollbackSystemd=="restoring" redoes daemon-reload/enable-state/restart
// (and the gate) from scratch; re-entering with "restored" only reruns the
// gate (or reconfirms stop), never restarting again.
//
// R6: unit_state_before.LoadState is only "loaded" when the unit already
// existed *before this transaction started* (reinstall's shape). For
// install's shape, the unit never existed before, so unit_state_before is
// {LoadState: "not-found", UnitFileState: "", ActiveState: "inactive"} —
// by the time file recovery (step 4) has restored the unit file itself
// back to "does not exist", there is no meaningful UnitFileState to
// restore it *to*, and nothing was ever running to restart, stop, or gate.
// daemon-reload alone (which makes systemd notice the now-deleted unit
// file) is still required and still runs; SetUnitFileState/Restart/Stop/
// Gate are all skipped in that case (see finishRestore).
func (r *Recoverer) restoreSystemd(ctx context.Context) error {
	j := r.Engine.Journal()
	unit := systemdUnit(j)
	if j.RollbackSystemd == RollbackSystemdRestored {
		return r.finishRestore(ctx, unit, true)
	}
	if j.RollbackSystemd != RollbackSystemdRestoring {
		nj := j.Clone()
		nj.RollbackSystemd = RollbackSystemdRestoring
		if err := r.commitJournal(nj); err != nil {
			return err
		}
	}
	if err := checkCancel(ctx); err != nil {
		return err
	}
	if err := r.Systemd.DaemonReload(ctx); err != nil {
		return err
	}
	if j.UnitStateBefore.LoadState == "loaded" {
		if err := r.Systemd.SetUnitFileState(ctx, unit, j.UnitStateBefore.UnitFileState); err != nil {
			return err
		}
	}
	return r.finishRestore(ctx, unit, false)
}

func (r *Recoverer) finishRestore(ctx context.Context, unit string, alreadyRestored bool) error {
	j := r.Engine.Journal()
	switch {
	case j.UnitStateBefore.LoadState != "loaded":
		// install's shape: the unit never existed before this transaction.
		// daemon-reload already ran; there is nothing running to restart,
		// stop, or gate.
	case j.UnitStateBefore.ActiveState == "active":
		if !alreadyRestored {
			if err := checkCancel(ctx); err != nil {
				return err
			}
			if err := r.Systemd.Restart(ctx, unit); err != nil {
				return err
			}
		}
		if r.Gate != nil {
			if err := r.Gate(ctx); err != nil {
				return fmt.Errorf("%w: %w", ErrGateFailed, err)
			}
		}
	default:
		if err := r.Systemd.Stop(ctx, unit); err != nil {
			return err
		}
	}
	if !alreadyRestored {
		nj := r.Engine.Journal().Clone()
		nj.RollbackSystemd = RollbackSystemdRestored
		return r.commitJournal(nj)
	}
	return nil
}

// snapshotBytes locates the forward snapshot step recorded for target
// (§7.1 requires one whenever an existing path is modified or removed —
// exactly the paths beginRollback ever builds a write_file rollback step
// for) and reads its completed content back from SnapDir.
func (r *Recoverer) snapshotBytes(target string) ([]byte, error) {
	j := r.Engine.Journal()
	for _, s := range j.Steps {
		if s.Kind != KindSnapshot || s.Target != target {
			continue
		}
		if len(s.Temps) == 0 {
			return nil, fmt.Errorf("transaction: snapshot step %s for %s has no registered temp name", s.ID, target)
		}
		if r.SnapDir == nil {
			return nil, fmt.Errorf("transaction: rollback needs the snapshot for %s but Recoverer.SnapDir is nil", target)
		}
		return readFileAt(r.SnapDir, s.Temps[0]+".snap")
	}
	return nil, fmt.Errorf("transaction: no completed snapshot step found for %s; cannot restore its content", target)
}

// rollbackKeepSet returns every temp name registered by a rollback step —
// the only temp names still meaningful once phase=rolling_back. A forward
// step's own registered temp name is never needed again by rollback time:
// either the forward step reached done (its temp was already consumed by
// its own rename, so the name no longer exists as a file regardless) or it
// never did (crashed before rename — the leftover file is exactly the
// "已登记临时名的幂等删除" garbage §7.4 step 4 requires deleting before its
// parent directory's own remove_dir can succeed, since a forward step that
// will never run again can no longer claim that name).
func rollbackKeepSet(j *Journal) map[string]bool {
	keep := make(map[string]bool)
	for _, s := range j.RollbackSteps {
		for _, t := range s.Temps {
			keep[t] = true
		}
	}
	return keep
}

// cleanupLeftoverTemps deletes every ".proxyctl-txn-*" entry found under a
// managed path's own containing directory (forEachManagedScanDir — always
// resolved and opened fresh, ancestor-verified against the *current*
// dir_ids at the moment of each unlink, exactly like every other mutation
// in this package) that is not registered by a rollback step
// (rollbackKeepSet) — §7.4 step 4's "已登记临时名的幂等删除必须先于其父目
// 录的remove_dir": run once at the start of recoverFiles, before any
// remove_dir rollback step executes, so a stray leftover (including one
// still nominally "registered" by a forward step that will never run
// again) never blocks that directory's own rmdir.
func (r *Recoverer) cleanupLeftoverTemps(ctx context.Context) error {
	j := r.Engine.Journal()
	keep := rollbackKeepSet(j)
	return r.forEachManagedScanDir(j, func(dir *os.File) error {
		if err := checkCancel(ctx); err != nil {
			return err
		}
		stray, err := scanUnregisteredTemps(dir, keep)
		if err != nil {
			return err
		}
		for _, name := range stray {
			if err := unix.Unlinkat(int(dir.Fd()), name, 0); err != nil && !errors.Is(err, unix.ENOENT) {
				return fmt.Errorf("transaction: remove leftover temp %s in %s: %w", name, dir.Name(), err)
			}
		}
		return nil
	})
}

// recoverFiles implements §7.4 step 4: continue (or start) rollback_steps
// in their persisted order, skipping done steps and idempotently resuming
// started ones (the same Engine.Rollback* primitives forward execution
// uses, which tolerate a step already partway through — see steps.go).
// Once every step is done, every leftover registered temp name is gone,
// and a final full validateRollingBack confirms every managed path is
// exactly at its pre-transaction state, rollback_files_done is persisted
// true.
func (r *Recoverer) recoverFiles(ctx context.Context) error {
	if err := r.cleanupLeftoverTemps(ctx); err != nil {
		return err
	}
	for _, rs := range r.Engine.Journal().RollbackSteps {
		if rs.Status == StatusDone {
			continue
		}
		if err := checkCancel(ctx); err != nil {
			return err
		}
		mp, err := r.Resolve.Resolve(rs.Target)
		if err != nil {
			return fmt.Errorf("%w: resolve %s: %w", ErrTampered, rs.Target, err)
		}
		switch rs.Kind {
		case KindMkdir:
			f, ferr := r.Engine.RollbackMkdir(ctx, rs.ID, mp)
			if f != nil {
				f.Close()
			}
			if ferr != nil {
				return ferr
			}
		case KindSetMeta:
			if err := r.Engine.RollbackSetMeta(ctx, rs.ID, mp); err != nil {
				return err
			}
		case KindRemoveFile:
			if err := r.Engine.RollbackRemoveFile(ctx, rs.ID, mp); err != nil {
				return err
			}
		case KindRemoveDir:
			if err := r.Engine.RollbackRemoveDir(ctx, rs.ID, mp); err != nil {
				return err
			}
		case KindWriteFile:
			data, derr := r.snapshotBytes(rs.Target)
			if derr != nil {
				return derr
			}
			if len(rs.Temps) == 0 {
				return fmt.Errorf("transaction: rollback step %s has no registered temp name", rs.ID)
			}
			if err := r.Engine.RollbackWriteFile(ctx, rs.ID, mp, rs.Temps[0], data); err != nil {
				return err
			}
		default:
			return fmt.Errorf("transaction: rollback step %s has unsupported kind %s", rs.ID, rs.Kind)
		}
	}
	if err := r.cleanupLeftoverTemps(ctx); err != nil {
		return err
	}
	if err := r.validateRollingBack(ctx); err != nil {
		return err
	}
	nj := r.Engine.Journal().Clone()
	nj.RollbackFilesDone = true
	return r.commitJournal(nj)
}

// --- §7.5 committed-phase forward cleanup ---

// forwardCleanup implements §7.5: resume any forward step still not done
// (registered by the original operation before it committed — old-version
// or staging-directory deletions only; write_file/snapshot are never
// expected here and are rejected explicitly rather than silently
// mishandled) using the ordinary forward Engine primitives, which tolerate
// a step already partway through exactly like the rollback path does.
func (r *Recoverer) forwardCleanup(ctx context.Context) error {
	for _, s := range r.Engine.Journal().Steps {
		if s.Status == StatusDone {
			continue
		}
		if err := checkCancel(ctx); err != nil {
			return err
		}
		mp, err := r.Resolve.Resolve(s.Target)
		if err != nil {
			return fmt.Errorf("transaction: resolve %s for committed-phase cleanup: %w", s.Target, err)
		}
		switch s.Kind {
		case KindMkdir:
			f, ferr := r.Engine.Mkdir(ctx, s.ID, mp)
			if f != nil {
				f.Close()
			}
			if ferr != nil {
				return ferr
			}
		case KindSetMeta:
			if err := r.Engine.SetMeta(ctx, s.ID, mp); err != nil {
				return err
			}
		case KindRemoveFile:
			if err := r.Engine.RemoveFile(ctx, s.ID, mp); err != nil {
				return err
			}
		case KindRemoveDir:
			if err := r.Engine.RemoveDir(ctx, s.ID, mp); err != nil {
				return err
			}
		default:
			return fmt.Errorf("transaction: committed-phase cleanup does not support resuming step %s of kind %s (§7.5 cleanup is deletions only)", s.ID, s.Kind)
		}
	}
	return nil
}

// --- §7.6 transaction-area deletion and tombstone recovery ---

// deleteTxnArea implements §7.6: rename the transaction area to its
// already-registered tombstone name (fsyncing Root after the rename), then
// recursively delete the tombstone wholesale.
func (r *Recoverer) deleteTxnArea(ctx context.Context) error {
	if err := checkCancel(ctx); err != nil {
		return err
	}
	dirName := r.Engine.store.dirName
	if err := unix.Renameat(int(r.Root.f.Fd()), dirName, int(r.Root.f.Fd()), r.TombstoneName); err != nil {
		return fmt.Errorf("transaction: rename transaction area to tombstone: %w", err)
	}
	if err := r.Root.f.Sync(); err != nil {
		return fmt.Errorf("transaction: fsync %s after tombstone rename: %w", r.Root.Path(), err)
	}
	return removeAllUnder(r.Root.f, r.TombstoneName)
}

// DeleteTombstone deletes a leftover tombstone directory (§7.6) wholesale,
// without interpreting its contents (it may hold private-key snapshots) —
// for a caller that finds one from a previous crash between the rename and
// the recursive delete completing, before it even looks for a normal
// (non-tombstoned) transaction area. Returns (wrapped) os.ErrNotExist when
// no such directory exists.
func DeleteTombstone(root *TrustedRoot, tombstoneName string) error {
	return removeAllUnder(root.f, tombstoneName)
}

// RecoverEmptyTransactionArea implements §7.6's "recover发现事务区存在,但
// 没有journal.json" rule, for a caller that has not yet been able to open
// a Store (OpenTransaction requires journal.json to exist). Try this
// first whenever the transaction area directory itself exists but
// OpenTransaction fails because journal.json is missing:
//   - the area is empty, or holds only journal.json.tmp: it is deleted
//     wholesale and this returns nil — §7.6's own words for exactly this
//     case are "说明崩溃发生在事务开始阶段,没有任何受管路径被修改。整体删
//     除事务区,返回0": recover (the caller of this function) reports 0,
//     the same as any other successful recover. This is a different exit
//     code from §7.4's boundary for a *fresh* command's own failure before
//     any modification (exit 1) — that boundary is Recover's own
//     OutcomeNoChange, reached through OpenTransaction succeeding, never
//     through this function.
//   - journal.json exists after all (a benign race with a concurrent
//     writer): returns ErrJournalPresent so the caller retries
//     OpenTransaction instead.
//   - anything else is present: returns ErrTampered, wrapping os.ErrNotExist
//     never happens here — a missing transaction area is the caller's own
//     concern before calling this at all.
func RecoverEmptyTransactionArea(root *TrustedRoot, dirName string) error {
	dirFd, err := openChildDir(root.f, dirName)
	if err != nil {
		return err
	}
	defer dirFd.Close()
	names, err := readDirNamesOf(dirFd)
	if err != nil {
		return fmt.Errorf("transaction: list %s: %w", dirName, err)
	}
	for _, n := range names {
		if n == journalFileName {
			return ErrJournalPresent
		}
	}
	for _, n := range names {
		if n != journalTmpName {
			return fmt.Errorf("%w: transaction area %s contains unexpected entry %q with no journal.json", ErrTampered, dirName, n)
		}
	}
	return removeAllUnder(root.f, dirName)
}

// ErrJournalPresent is RecoverEmptyTransactionArea's signal that
// journal.json exists after all — the caller should use OpenTransaction
// (and then Recoverer.Recover) instead.
var ErrJournalPresent = errors.New("transaction: journal.json exists; use OpenTransaction instead")

// readDirNamesOf lists dir's own entries using a dedicated fd reopened via
// ".", mirroring scanUnregisteredTemps's own reasoning: never disturb a
// directory-stream position some other open fd for the same directory
// might be relying on.
func readDirNamesOf(dir *os.File) ([]string, error) {
	fd, err := unix.Openat(int(dir.Fd()), ".", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	f := os.NewFile(uintptr(fd), dir.Name())
	defer f.Close()
	return f.Readdirnames(-1)
}

// removeAllUnder recursively deletes the entry named name directly under
// parent, entirely through openat-relative operations from parent's own
// already-open fd (never by re-resolving a string path from some root —
// the same safety property every other mutation in this package has). A
// non-directory entry is simply unlinked. Missing entries at any level are
// tolerated (ENOENT), since §7.6 recovery may re-run this against a
// deletion that a previous crash partially completed.
func removeAllUnder(parent *os.File, name string) error {
	fd, err := unix.Openat(int(parent.Fd()), name, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		if errors.Is(err, unix.ENOTDIR) {
			if uerr := unix.Unlinkat(int(parent.Fd()), name, 0); uerr != nil && !errors.Is(uerr, unix.ENOENT) {
				return fmt.Errorf("transaction: remove %s: %w", name, uerr)
			}
			return nil
		}
		if errors.Is(err, unix.ENOENT) {
			return nil
		}
		return fmt.Errorf("transaction: open %s for recursive delete: %w", name, err)
	}
	dir := os.NewFile(uintptr(fd), name)
	entries, rerr := dir.Readdirnames(-1)
	dir.Close()
	if rerr != nil {
		return fmt.Errorf("transaction: list %s for recursive delete: %w", name, rerr)
	}
	childFd, err := unix.Openat(int(parent.Fd()), name, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return fmt.Errorf("transaction: reopen %s for recursive delete: %w", name, err)
	}
	childParent := os.NewFile(uintptr(childFd), name)
	defer childParent.Close()
	for _, e := range entries {
		var st unix.Stat_t
		if serr := unix.Fstatat(int(childParent.Fd()), e, &st, unix.AT_SYMLINK_NOFOLLOW); serr != nil {
			if errors.Is(serr, unix.ENOENT) {
				continue
			}
			return fmt.Errorf("transaction: stat %s/%s for recursive delete: %w", name, e, serr)
		}
		if st.Mode&unix.S_IFMT == unix.S_IFDIR {
			if err := removeAllUnder(childParent, e); err != nil {
				return err
			}
		} else if uerr := unix.Unlinkat(int(childParent.Fd()), e, 0); uerr != nil && !errors.Is(uerr, unix.ENOENT) {
			return fmt.Errorf("transaction: remove %s/%s: %w", name, e, uerr)
		}
	}
	if err := unix.Unlinkat(int(parent.Fd()), name, unix.AT_REMOVEDIR); err != nil && !errors.Is(err, unix.ENOENT) {
		return fmt.Errorf("transaction: rmdir %s for recursive delete: %w", name, err)
	}
	return nil
}
