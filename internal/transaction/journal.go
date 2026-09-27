// Package transaction implements the durable journal, safe-path primitives
// and forward execution steps shared by every host-side lifecycle
// operation (install/reinstall/uninstall/purge/recover) and by
// subscription-center publish, per design spec §7
// (docs/superpowers/specs/2026-09-18-v0.10-reality-node-install-design.md).
//
// This file (journal.go) covers §7.1's persisted schema, its read-only
// validation (unknown schema, unknown enum values, and the progress/chain
// legality rules in §7.3's first two paragraphs), and the journal's own
// durable persistence (mkdir transaction area -> write journal.json.tmp ->
// fsync -> rename -> fsync directory, repeated for every update).
//
// Task 4 (see docs/superpowers/plans/2026-09-18-v0.10-reality-node-install.md)
// delivers the schema, the safe-path primitives (paths_unix.go) and the
// forward step execution (steps.go). Rollback construction, recover and
// commit-time forward cleanup are Task 5's scope: this package only needs
// to persist and validate the fields Task 5 will act on.
package transaction

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

// CurrentSchema is the only journal schema this version of proxyctl
// understands. A journal declaring any other value is read-only rejected
// (§7.1 "未知 schema") so that a newer proxyctl's journal is never
// misinterpreted, normalized or overwritten by an older binary.
const CurrentSchema = 1

// Phase is the journal's top-level lifecycle phase (§7.1).
type Phase string

const (
	PhaseRunning     Phase = "running"
	PhaseRollingBack Phase = "rolling_back"
	PhaseCommitted   Phase = "committed"
)

func (p Phase) valid() bool {
	switch p {
	case PhaseRunning, PhaseRollingBack, PhaseCommitted:
		return true
	}
	return false
}

// StepStatus is one step's progress (§7.1 steps[].status).
type StepStatus string

const (
	StatusPending StepStatus = "pending"
	StatusStarted StepStatus = "started"
	StatusDone    StepStatus = "done"
)

func (s StepStatus) valid() bool {
	switch s {
	case StatusPending, StatusStarted, StatusDone:
		return true
	}
	return false
}

// StepKind is a step's primitive kind (§7.2).
type StepKind string

const (
	KindWriteFile           StepKind = "write_file"
	KindMkdir               StepKind = "mkdir"
	KindSetMeta             StepKind = "set_meta"
	KindRemoveFile          StepKind = "remove_file"
	KindRemoveDir           StepKind = "remove_dir"
	KindSnapshot            StepKind = "snapshot"
	KindSystemdDaemonReload StepKind = "systemd_daemon_reload"
	KindSystemdEnable       StepKind = "systemd_enable"
	KindSystemdDisable      StepKind = "systemd_disable"
	KindSystemdStart        StepKind = "systemd_start"
	KindSystemdRestart      StepKind = "systemd_restart"
	KindSystemdStop         StepKind = "systemd_stop"
)

func (k StepKind) valid() bool {
	switch k {
	case KindWriteFile, KindMkdir, KindSetMeta, KindRemoveFile, KindRemoveDir, KindSnapshot,
		KindSystemdDaemonReload, KindSystemdEnable, KindSystemdDisable,
		KindSystemdStart, KindSystemdRestart, KindSystemdStop:
		return true
	}
	return false
}

// isFileOrDirKind reports whether k is one of §7.2's file/directory
// primitives (write_file, mkdir, set_meta, remove_file, remove_dir,
// snapshot) as opposed to a systemd_* kind. rollback_steps (§7.1) may only
// contain file/directory kinds.
func (k StepKind) isFileOrDirKind() bool {
	switch k {
	case KindWriteFile, KindMkdir, KindSetMeta, KindRemoveFile, KindRemoveDir, KindSnapshot:
		return true
	}
	return false
}

func (k StepKind) isSystemdKind() bool { return k.valid() && !k.isFileOrDirKind() }

// RollbackSystemdState is the rollback-phase systemd progress (§7.1
// rollback_systemd).
type RollbackSystemdState string

const (
	RollbackSystemdNone      RollbackSystemdState = "none"
	RollbackSystemdDemoting  RollbackSystemdState = "demoting"
	RollbackSystemdDemoted   RollbackSystemdState = "demoted"
	RollbackSystemdRestoring RollbackSystemdState = "restoring"
	RollbackSystemdRestored  RollbackSystemdState = "restored"
)

func (s RollbackSystemdState) valid() bool {
	switch s {
	case RollbackSystemdNone, RollbackSystemdDemoting, RollbackSystemdDemoted, RollbackSystemdRestoring, RollbackSystemdRestored:
		return true
	}
	return false
}

// UnitState is the systemd unit state snapshot recorded before a
// transaction begins (§7.1 unit_state_before) and used by §7.3's systemd
// judgement.
type UnitState struct {
	LoadState     string `json:"load_state"`
	UnitFileState string `json:"unit_file_state"`
	ActiveState   string `json:"active_state"`
}

// DirIdentity is one directory's recorded identity in Journal.DirIDs
// (§7.1): either "does not exist" (Exists=false) or a concrete (dev, ino)
// pair. It is compared, not filesystem-resolved: paths_unix.go re-opens the
// live directory and compares its identity against this recorded value.
type DirIdentity struct {
	Exists bool   `json:"exists"`
	Dev    uint64 `json:"dev,omitempty"`
	Ino    uint64 `json:"ino,omitempty"`
}

// PathState is a "path state" per §7.1: whether a managed file exists, its
// type, size, content hash (or, for a dynamic-state file, its schema name
// instead of a hash), mode and ownership. §7.1 restricts the type a managed
// file's pre/post state may legally hold to "regular file" or "does not
// exist" — anything else observed live is tampering, not a legal state to
// record.
type PathState struct {
	Exists bool   `json:"exists"`
	Type   string `json:"type,omitempty"`
	Size   int64  `json:"size,omitempty"`
	SHA256 string `json:"sha256,omitempty"`
	Schema string `json:"schema,omitempty"`
	Mode   uint32 `json:"mode,omitempty"`
	UID    uint32 `json:"uid,omitempty"`
	GID    uint32 `json:"gid,omitempty"`
	// UnitFileState is only meaningful for a systemd_enable/systemd_disable
	// step's Pre/Post (§7.2: "UnitFileState ∈ {pre, post}"); every other
	// field on such a step's PathState is left zero. It is carried on
	// PathState rather than a separate type so a single per-target chain
	// (validatePathChains, PathChain) and the same Sk.Post==S(k+1).pre
	// equality check work uniformly across file, directory and systemd
	// steps without a parallel struct or comparison path. Execution
	// (reading or setting the real unit file state) is Task 5/6's scope;
	// this schema only needs to store and compare it.
	UnitFileState string `json:"unit_file_state,omitempty"`
}

// PathTypeFile and PathTypeDir are the only Type values PathState uses.
// §7.1 restricts a managed *file's* legal pre/post type to "regular file"
// (PathTypeFile) or Exists=false; PathTypeDir is used only for directory
// steps (mkdir/remove_dir/set_meta on a directory), whose allowed states
// are governed separately by §7.2's mkdir/remove_dir rows.
const (
	PathTypeFile = "file"
	PathTypeDir  = "dir"
)

// Step is one journal step (§7.1 steps[] / rollback_steps[]).
type Step struct {
	ID     string     `json:"id"`
	Kind   StepKind   `json:"kind"`
	Target string     `json:"target"`
	Pre    PathState  `json:"pre"`
	Post   PathState  `json:"post"`
	Status StepStatus `json:"status"`
	Temps  []string   `json:"temps,omitempty"`
}

// Journal is the full §7.1 persisted schema=1 journal.
type Journal struct {
	Schema               int                    `json:"schema"`
	TxnID                string                 `json:"txn_id"`
	Op                   string                 `json:"op"`
	ProxyctlVersion      string                 `json:"proxyctl_version"`
	CreatedAt            time.Time              `json:"created_at"`
	Phase                Phase                  `json:"phase"`
	RollbackFrontier     []StepStatus           `json:"rollback_frontier,omitempty"`
	RollbackSteps        []Step                 `json:"rollback_steps,omitempty"`
	RollbackFilesDone    bool                   `json:"rollback_files_done"`
	SingboxVersionBefore string                 `json:"singbox_version_before,omitempty"`
	SingboxVersionAfter  string                 `json:"singbox_version_after,omitempty"`
	UnitStateBefore      UnitState              `json:"unit_state_before"`
	DirIDs               map[string]DirIdentity `json:"dir_ids"`
	RollbackSystemd      RollbackSystemdState   `json:"rollback_systemd"`
	Steps                []Step                 `json:"steps"`
}

// Clone returns a deep copy of j, safe for a caller to mutate before
// handing it back to (*Store).Update.
func (j *Journal) Clone() *Journal {
	if j == nil {
		return nil
	}
	out := *j
	out.RollbackFrontier = append([]StepStatus(nil), j.RollbackFrontier...)
	out.RollbackSteps = cloneSteps(j.RollbackSteps)
	out.Steps = cloneSteps(j.Steps)
	if j.DirIDs != nil {
		out.DirIDs = make(map[string]DirIdentity, len(j.DirIDs))
		for k, v := range j.DirIDs {
			out.DirIDs[k] = v
		}
	}
	return &out
}

func cloneSteps(steps []Step) []Step {
	if steps == nil {
		return nil
	}
	out := make([]Step, len(steps))
	for i, s := range steps {
		out[i] = s
		out[i].Temps = append([]string(nil), s.Temps...)
	}
	return out
}

// Errors returned by ParseJournal and Validate. They are deliberately
// distinguishable (plan Task 4: "返回可区分的错误类型"): a caller (Task 5's
// exit-code mapping) can tell "this proxyctl does not understand this
// journal's schema at all" apart from "this journal is structurally
// malformed" and from "this journal's recorded progress is not a legal
// sequence" — all three are read-only rejections that map to exit code 5,
// but the distinction is useful for diagnostics and for future schema
// migrations.
var (
	// ErrUnknownSchema reports a journal whose schema field is not
	// CurrentSchema. Per §7.1, the caller should be told to use the
	// proxyctl_version recorded in the journal to operate on it instead.
	ErrUnknownSchema = errors.New("transaction: unknown journal schema")
	// ErrInvalidJournal reports structurally malformed JSON, or a known
	// schema with a field holding a value outside its enum (unknown phase,
	// status, kind or rollback_systemd state).
	ErrInvalidJournal = errors.New("transaction: invalid journal")
	// ErrIllegalProgress reports a journal that parses and uses only known
	// enum values, but whose recorded step progress is not a legal
	// sequence per §7.3's first two paragraphs (an illegal status prefix,
	// a broken pre/post chain along some path, or an illegal
	// rollback_systemd / rollback_files_done combination).
	ErrIllegalProgress = errors.New("transaction: illegal step progress")
)

// ParseJournal decodes and fully validates data as a schema=1 journal. It
// never mutates or normalizes: an invalid journal is rejected as-is, with
// no attempt to coerce it into a legal one before reporting the error
// (plan Task 4: "不把非法数据规范化后回写").
func ParseJournal(data []byte) (*Journal, error) {
	var probe struct {
		Schema          int    `json:"schema"`
		ProxyctlVersion string `json:"proxyctl_version"`
	}
	if err := json.Unmarshal(data, &probe); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidJournal, err)
	}
	if probe.Schema != CurrentSchema {
		return nil, fmt.Errorf("%w: %d (written by proxyctl %s)", ErrUnknownSchema, probe.Schema, probe.ProxyctlVersion)
	}

	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var j Journal
	if err := dec.Decode(&j); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidJournal, err)
	}
	if err := j.Validate(); err != nil {
		return nil, err
	}
	return &j, nil
}

// Validate runs every read-only check ParseJournal applies, against a
// Journal already decoded in memory (used by (*Store).Update to reject an
// illegal in-memory update before it is ever written, per "非法组合返回错误
// 且不改任何东西": persistence must not even be attempted).
func (j *Journal) Validate() error {
	if j.Schema != CurrentSchema {
		return fmt.Errorf("%w: %d", ErrUnknownSchema, j.Schema)
	}
	if !j.Phase.valid() {
		return fmt.Errorf("%w: unknown phase %q", ErrInvalidJournal, j.Phase)
	}
	if !j.RollbackSystemd.valid() {
		return fmt.Errorf("%w: unknown rollback_systemd %q", ErrInvalidJournal, j.RollbackSystemd)
	}
	if err := validateSteps(j.Steps, true); err != nil {
		return err
	}
	if err := validateSteps(j.RollbackSteps, false); err != nil {
		return err
	}

	// §7.3 "先检查journal的进度组合": rollback_systemd in
	// {restoring, restored} requires rollback_files_done; rollback_files_done
	// requires every rollback step done.
	if (j.RollbackSystemd == RollbackSystemdRestoring || j.RollbackSystemd == RollbackSystemdRestored) && !j.RollbackFilesDone {
		return fmt.Errorf("%w: rollback_systemd=%s requires rollback_files_done=true", ErrIllegalProgress, j.RollbackSystemd)
	}
	if j.RollbackFilesDone {
		for _, s := range j.RollbackSteps {
			if s.Status != StatusDone {
				return fmt.Errorf("%w: rollback_files_done=true but rollback step %s is %s", ErrIllegalProgress, s.ID, s.Status)
			}
		}
	}

	// rollback_frontier must be a legal forward-sequence snapshot: same
	// length as steps, and itself a legal done*/started?/pending* prefix.
	if j.RollbackFrontier != nil {
		if len(j.RollbackFrontier) != len(j.Steps) {
			return fmt.Errorf("%w: rollback_frontier length %d does not match steps length %d", ErrIllegalProgress, len(j.RollbackFrontier), len(j.Steps))
		}
		if err := validateStatusPrefix(j.RollbackFrontier); err != nil {
			return err
		}
	}

	// Per-path pre/post chaining: journal construction must satisfy
	// S(k+1).pre == Sk.post for steps sharing the same target, in
	// execution order (§7.3).
	if err := validatePathChains(j.Steps); err != nil {
		return err
	}
	if err := validatePathChains(j.RollbackSteps); err != nil {
		return err
	}

	// Phase consistency: a journal that has never entered rollback carries
	// no rollback state at all, and one that has must at least have taken
	// the frontier snapshot (§7.4 step 2 writes phase, rollback_frontier
	// and rollback_steps together in the same persisted version — none of
	// the three ever appears without the other two).
	if j.Phase == PhaseRunning {
		if len(j.RollbackFrontier) != 0 || len(j.RollbackSteps) != 0 || j.RollbackFilesDone || j.RollbackSystemd != RollbackSystemdNone {
			return fmt.Errorf("%w: phase=running must not carry any rollback state", ErrIllegalProgress)
		}
	}
	if j.Phase == PhaseRollingBack && j.RollbackFrontier == nil {
		return fmt.Errorf("%w: phase=rolling_back requires rollback_frontier", ErrIllegalProgress)
	}

	if err := validateTemps(j); err != nil {
		return err
	}
	return nil
}

// tempNamePrefix is §7.3's fixed temp-name format's constant portion, up to
// but not including the txn_id: ".proxyctl-txn-<txn_id>-<decimal_seq>".
const tempNamePrefix = ".proxyctl-txn-"

// isValidTempName reports whether name matches the exact format §7.3
// requires for this journal's own txn_id: ".proxyctl-txn-<txn_id>-" followed
// by one or more decimal digits and nothing else. A temp name that happens
// to look plausible but carries a different (or no) txn_id, or trailing
// non-digit characters, is rejected — this is what stops a journal from
// registering an arbitrary real filename (e.g. "config.json") as a "temp
// name" that a later recover would then be entitled to delete as leftover.
func isValidTempName(name, txnID string) bool {
	prefix := tempNamePrefix + txnID + "-"
	seq := strings.TrimPrefix(name, prefix)
	if seq == name || seq == "" {
		return false
	}
	for _, r := range seq {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// validateTemps requires every step's (forward and rollback combined)
// registered temp name to match isValidTempName for this journal's own
// txn_id, and requires every temp name to be registered by at most one
// step journal-wide — §7.1's "所有临时名...先写入journal" implies each one
// names a single reserved slot, never shared or reused across steps.
func validateTemps(j *Journal) error {
	seen := map[string]string{}
	check := func(steps []Step) error {
		for _, s := range steps {
			for _, t := range s.Temps {
				if !isValidTempName(t, j.TxnID) {
					return fmt.Errorf("%w: step %s registers a temp name %q that does not match the required .proxyctl-txn-<txn_id>-<seq> format", ErrInvalidJournal, s.ID, t)
				}
				if owner, dup := seen[t]; dup {
					return fmt.Errorf("%w: temp name %q is registered by both step %s and step %s", ErrInvalidJournal, t, owner, s.ID)
				}
				seen[t] = s.ID
			}
		}
		return nil
	}
	if err := check(j.Steps); err != nil {
		return err
	}
	return check(j.RollbackSteps)
}

func validateSteps(steps []Step, allowSystemd bool) error {
	seenIDs := make(map[string]bool, len(steps))
	for _, s := range steps {
		if s.ID == "" {
			return fmt.Errorf("%w: step with empty id", ErrInvalidJournal)
		}
		if seenIDs[s.ID] {
			return fmt.Errorf("%w: duplicate step id %s", ErrInvalidJournal, s.ID)
		}
		seenIDs[s.ID] = true
		if !s.Kind.valid() {
			return fmt.Errorf("%w: unknown step kind %q", ErrInvalidJournal, s.Kind)
		}
		if !allowSystemd && s.Kind.isSystemdKind() {
			return fmt.Errorf("%w: rollback step %s uses systemd kind %q (§7.1: rollback_steps only uses file/directory primitives)", ErrInvalidJournal, s.ID, s.Kind)
		}
		if !s.Status.valid() {
			return fmt.Errorf("%w: unknown step status %q", ErrInvalidJournal, s.Status)
		}
		if !s.Pre.typeValid() {
			return fmt.Errorf("%w: step %s has an invalid pre type %q", ErrInvalidJournal, s.ID, s.Pre.Type)
		}
		if !s.Post.typeValid() {
			return fmt.Errorf("%w: step %s has an invalid post type %q", ErrInvalidJournal, s.ID, s.Post.Type)
		}
		if !s.Pre.contentFieldsValid() {
			return fmt.Errorf("%w: step %s's pre has both sha256 and schema set", ErrInvalidJournal, s.ID)
		}
		if !s.Post.contentFieldsValid() {
			return fmt.Errorf("%w: step %s's post has both sha256 and schema set", ErrInvalidJournal, s.ID)
		}
	}
	statuses := make([]StepStatus, len(steps))
	for i, s := range steps {
		statuses[i] = s.Status
	}
	return validateStatusPrefix(statuses)
}

// typeValid restricts PathState.Type, when Exists is true, to the two
// values this schema knows about; Type is ignored (and must be empty) when
// Exists is false, since a non-existent path has no type.
func (p PathState) typeValid() bool {
	if !p.Exists {
		return p.Type == ""
	}
	return p.Type == PathTypeFile || p.Type == PathTypeDir
}

// contentFieldsValid rejects a PathState that records both a content hash
// and a dynamic-state schema name — §5.2 identifies a managed file's
// content exactly one way (hash for an immutable asset, schema name for a
// dynamic-state one, never both), so a journal claiming both is malformed
// rather than merely ambiguous.
func (p PathState) contentFieldsValid() bool {
	return p.SHA256 == "" || p.Schema == ""
}

// validateStatusPrefix enforces §7.3's "done* started? pending*" shape on
// a sequence of statuses, in the order they were recorded (execution
// order): any number of done, then at most one started, then any number of
// pending. It does not special-case "the last non-pending step" — every
// status is checked, so a pending followed by a started or done, a started
// followed by a done, multiple started, or an unknown status are all
// rejected regardless of what a "last non-pending" reading might otherwise
// tolerate.
func validateStatusPrefix(statuses []StepStatus) error {
	stage := 0 // 0 = still in the done* run, 1 = the optional started seen, 2 = in the pending* run
	for i, s := range statuses {
		switch s {
		case StatusDone:
			if stage != 0 {
				return fmt.Errorf("%w: status %d is done after a started or pending step", ErrIllegalProgress, i)
			}
		case StatusStarted:
			if stage != 0 {
				return fmt.Errorf("%w: status %d is an extra started step", ErrIllegalProgress, i)
			}
			stage = 1
		case StatusPending:
			stage = 2
		default:
			return fmt.Errorf("%w: unknown status %q at index %d", ErrInvalidJournal, s, i)
		}
	}
	return nil
}

// validatePathChains groups steps by Target, preserving execution order
// within each group, and checks that consecutive steps on the same path
// chain pre/post exactly (§7.3: "S(k+1) 的 pre 等于 Sk 的 post；不满足的
// journal 在事务开始前就是实现错误").
func validatePathChains(steps []Step) error {
	last := make(map[string]Step)
	for _, s := range steps {
		if prev, ok := last[s.Target]; ok {
			if prev.Post != s.Pre {
				return fmt.Errorf("%w: path %s: step %s's pre does not match step %s's post", ErrIllegalProgress, s.Target, s.ID, prev.ID)
			}
		}
		last[s.Target] = s
	}
	return nil
}

// PathChain returns the steps whose Target equals target, in the order
// they appear in steps (which must already be execution order) — the
// per-path chain S1..Sn that §7.3's running/rolling_back judgement is
// defined over.
func PathChain(steps []Step, target string) []Step {
	var out []Step
	for _, s := range steps {
		if s.Target == target {
			out = append(out, s)
		}
	}
	return out
}

// --- Durable persistence (§7.1's transaction-area/journal lifecycle) ---

// journalFileName and journalTmpName are the fixed names §7.1 requires.
const (
	journalFileName = "journal.json"
	journalTmpName  = "journal.json.tmp"
)

// Store is a transaction area's own directory handle, bound to the
// exclusive-owner-verified TrustedRoot it was created under, plus the
// current in-memory Journal it holds. Every mutation goes through Update,
// which performs §7.1's fixed sequence: marshal -> write journal.json.tmp
// -> fsync -> rename to journal.json -> fsync directory.
type Store struct {
	dir     *os.File // open fd for the transaction area directory itself
	dirName string   // the transaction area's own name under its parent
	journal *Journal
}

// Journal returns the Store's current in-memory journal. The caller must
// not mutate the returned value in place; call Clone first.
func (s *Store) Journal() *Journal { return s.journal }

// Dir returns the open fd for the transaction area directory, for a
// caller (steps.go) that needs to create files under it (dl/, snap/) or
// scan it during recover.
func (s *Store) Dir() *os.File { return s.dir }

// BeginTransaction creates a new transaction area named dirName (mode
// 0700) directly under root, then durably persists the first version of j
// (§7.1 steps 1-3: mkdir -> write journal.json.tmp+fsync -> rename+fsync
// directory). j.Schema must already be CurrentSchema and j must already
// pass Validate(); BeginTransaction does not fill in any field on the
// caller's behalf.
//
// mkdir itself follows the same ancestor-chain re-verification as any
// other managed mutation (paths_unix.go's prepareMutation), checked
// against root alone (there are no ancestor components below root here) —
// consistent with §7.1 applying the same rule to every "创建临时文件、
// rename、unlink、mkdir、rmdir、fchmod、fchown" call.
func BeginTransaction(root *TrustedRoot, dirName string, j *Journal) (*Store, error) {
	if err := j.Validate(); err != nil {
		return nil, err
	}
	dirFd, err := mkdirManaged(root, dirName, 0700)
	if err != nil {
		return nil, err
	}
	// The transaction area's directory entry itself must be durable before
	// anything is written inside it — otherwise a crash could lose the
	// mkdir entirely (root's directory block never made it to disk) while
	// journal.json (written and fsynced next, inside a directory fd that
	// still resolves fine in-memory even though its parent entry did not
	// survive) or later managed-path writes already had.
	if err := root.f.Sync(); err != nil {
		dirFd.Close()
		return nil, fmt.Errorf("transaction: fsync trusted root after creating %s: %w", dirName, err)
	}
	s := &Store{dir: dirFd, dirName: dirName}
	if err := s.persist(j); err != nil {
		dirFd.Close()
		return nil, err
	}
	return s, nil
}

// OpenTransaction opens an existing transaction area named dirName under
// root (used by recover, Task 5) and parses its current journal.json. It
// does not accept a journal.json.tmp-only or empty area — callers that
// need to distinguish "no journal.json yet" (§7.6's recover rule for a
// crash during transaction start) do so before calling this, by listing
// the directory themselves.
func OpenTransaction(root *TrustedRoot, dirName string) (*Store, error) {
	dirFd, err := openChildDir(root.f, dirName)
	if err != nil {
		return nil, err
	}
	data, err := readFileAt(dirFd, journalFileName)
	if err != nil {
		dirFd.Close()
		return nil, err
	}
	j, err := ParseJournal(data)
	if err != nil {
		dirFd.Close()
		return nil, err
	}
	return &Store{dir: dirFd, dirName: dirName, journal: j}, nil
}

// Close releases the Store's own directory fd. It does not remove
// anything on disk.
func (s *Store) Close() error { return s.dir.Close() }

// Update replaces the Store's journal with next, first fully validating it
// (an illegal next is rejected before anything is written, touching
// neither the journal file nor any managed path), then durably persisting
// it via the same tmp-fsync-rename-fsync sequence BeginTransaction used.
func (s *Store) Update(next *Journal) error {
	if err := next.Validate(); err != nil {
		return err
	}
	return s.persist(next)
}

// persist is §7.1's "journal 的每次更新": write journal.json.tmp, fsync it,
// rename to journal.json, fsync the directory. It assumes next already
// validated.
func (s *Store) persist(next *Journal) error {
	data, err := json.Marshal(next)
	if err != nil {
		return fmt.Errorf("transaction: marshal journal: %w", err)
	}
	if err := writeTempAndRename(s.dir, journalTmpName, journalFileName, data, 0600); err != nil {
		return err
	}
	// Store a copy, not the caller's own pointer: the caller (an Engine
	// method building the next version via Journal().Clone()) must be free
	// to keep mutating or discarding its local variable afterward without
	// that being able to reach back into what the Store now considers
	// durable and current.
	s.journal = next.Clone()
	return nil
}

// writeTempAndRename implements the fixed "O_EXCL-created tmp name -> write
// -> fsync -> rename -> fsync directory" sequence used both for the
// journal itself (§7.1) and, with a caller-registered temp name, by
// steps.go's write_file primitive (§7.2). tmpName here is fixed
// (journal.json.tmp), not one of the per-step registered temp names in
// Step.Temps — the journal file itself is not a "managed path" subject to
// §7.1's ancestor-chain/dir_ids re-verification, since it lives directly
// in the transaction area this Store already holds an exclusive,
// continuously-open fd for.
func writeTempAndRename(dir *os.File, tmpName, finalName string, data []byte, mode os.FileMode) error {
	dirFd := int(dir.Fd())
	if err := hook(persistHook, "before_create"); err != nil {
		return err
	}
	// A retry loop is unnecessary here: tmpName is fixed, and only this
	// Store (holding the sole fd for this transaction area) ever writes
	// to it, so an O_TRUNC create-or-truncate is both correct and simpler
	// than createStagingFile's O_EXCL-plus-random-name dance, which exists
	// there to prove a *fresh* inode among concurrent writers of the same
	// directory. A leftover journal.json.tmp from a previous crash (§7.6:
	// "recover 直接丢弃") is exactly what this call is expected to
	// overwrite.
	fd, err := unix.Openat(dirFd, tmpName, unix.O_CREAT|unix.O_TRUNC|unix.O_WRONLY|unix.O_NOFOLLOW|unix.O_CLOEXEC, uint32(mode.Perm()))
	if err != nil {
		return fmt.Errorf("transaction: create %s: %w", tmpName, err)
	}
	tmp := os.NewFile(uintptr(fd), tmpName)
	// O_TRUNC reuses a pre-existing directory entry's inode and mode as-is
	// (only O_CREAT's own mode argument applies when the call is the one
	// creating the file); a stale journal.json.tmp left at some other mode
	// by an older version, a different umask, or external tampering would
	// otherwise keep that mode. Fchmod unconditionally, on the fd (never by
	// name), to the mode this call actually wants regardless of what was
	// there before.
	if err := tmp.Chmod(mode); err != nil {
		tmp.Close()
		return fmt.Errorf("transaction: chmod %s: %w", tmpName, err)
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return fmt.Errorf("transaction: write %s: %w", tmpName, err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return fmt.Errorf("transaction: fsync %s: %w", tmpName, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("transaction: close %s: %w", tmpName, err)
	}
	if err := hook(persistHook, "after_tmp_fsync"); err != nil {
		return err
	}
	if err := unix.Renameat(dirFd, tmpName, dirFd, finalName); err != nil {
		return fmt.Errorf("transaction: rename %s to %s: %w", tmpName, finalName, err)
	}
	if err := hook(persistHook, "after_rename"); err != nil {
		return err
	}
	return dir.Sync()
}

// persistHook lets a test observe or interrupt journal persistence
// (BeginTransaction's initial write and every Store.Update) at a named
// point, simulating a crash right there — the same pattern as steps.go's
// stepHook. Production builds leave it a no-op; this package's tests must
// not use t.Parallel.
var persistHook = func(point string) error { return nil }

// readFileAt reads name directly under dir (O_NOFOLLOW), without following
// a symbolic link.
func readFileAt(dir *os.File, name string) ([]byte, error) {
	fd, err := unix.Openat(int(dir.Fd()), name, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, fmt.Errorf("transaction: open %s: %w", name, err)
	}
	f := os.NewFile(uintptr(fd), name)
	defer f.Close()
	data, err := io.ReadAll(f)
	if err != nil {
		return nil, fmt.Errorf("transaction: read %s: %w", name, err)
	}
	return data, nil
}
