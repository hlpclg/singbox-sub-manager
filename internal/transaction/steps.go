package transaction

// Engine executes design spec §7.2's forward step primitives against a
// Store's journal (§7.1), and implements §7.3's phase=running per-path
// state judgement plus the "未登记临时名" scan. It never constructs
// rollback steps, never recovers a crashed transaction, and never touches
// systemd — those are Task 5's scope (internal/transaction/recovery.go,
// per the plan). Every step this Engine executes must already be present,
// with status=pending and its Temps already registered, in the journal a
// caller built and persisted via BeginTransaction/Update before execution
// starts — per §7.1's "所有临时名...先写入journal,再创建" and "journal构造
// 时必须满足"，planning (which steps, in which order, with which pre/post)
// is the caller's job; this Engine only marks started/done and performs
// the I/O.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"

	"golang.org/x/sys/unix"
)

// Engine ties a Store to the primitive execution methods below.
type Engine struct {
	store          *Store
	schemaDetector SchemaDetector
}

// NewEngine returns an Engine bound to store. store's current journal must
// already be valid (BeginTransaction and OpenTransaction both guarantee
// this).
func NewEngine(store *Store) *Engine { return &Engine{store: store} }

// SchemaDetector parses a dynamic-state file's content (§5.2: secrets.json,
// instance.json — files whose PathState identifies content by a schema
// name instead of a hash) and reports which schema it is, or an error if
// data does not parse as any schema this detector understands. target is
// the step's own Target (the journal key naming the path being compared),
// so one detector can dispatch on which file it is being asked about
// rather than needing a distinct detector per path. This package never
// interprets schema names or file formats itself — that is domain
// knowledge (Task 6+'s internal/realitynode, for secrets.json/
// instance.json specifically) this package only stores and compares.
type SchemaDetector func(target string, data []byte) (schemaName string, err error)

// SetSchemaDetector installs the function verifyPreState/openVerifiedSource
// use to fill in PathState.Schema for a Pre/Post that identifies a file by
// schema name instead of a content hash. It is nil until set, and every
// engine operation whose plan needs a schema comparison fails with
// ErrSchemaDetectorRequired rather than silently skipping the check.
func (e *Engine) SetSchemaDetector(f SchemaDetector) { e.schemaDetector = f }

// ErrSchemaDetectorRequired reports that a step's Pre or Post identifies
// its target by a dynamic-state Schema name, but no SchemaDetector has
// been installed via SetSchemaDetector — this
// must never be silently treated as "no content check needed", since that
// would let write_file/remove_file/snapshot operate on a dynamic-state
// file (secrets.json, instance.json) without ever confirming its content
// actually matches what the plan recorded.
var ErrSchemaDetectorRequired = errors.New("transaction: pre/post identifies content by schema name, but no SchemaDetector is installed")

// maxSchemaDetectionBytes bounds how much of a file this package reads
// into memory to hand to a SchemaDetector — dynamic-state files (secrets.json,
// instance.json) are always small; this is a safety cap against a
// managed path unexpectedly holding something enormous, not a realistic
// limit for its intended targets.
const maxSchemaDetectionBytes = 1 << 20 // 1 MiB

// computeContentField reads f's content into whichever of PathState's two
// mutually exclusive content-identity fields want uses (Validate rejects a
// PathState with both SHA256 and Schema set), filling base and returning
// it. It reads nothing when want carries neither — a Pre/Post with no
// content marker at all has nothing for the caller to compare content
// against anyway (e.g. a step that only cares about a directory's
// metadata, or a file whose content genuinely is not tracked).
func (e *Engine) computeContentField(f *os.File, target string, want PathState, base PathState) (PathState, error) {
	switch {
	case want.SHA256 != "":
		h := sha256.New()
		if _, err := io.Copy(h, f); err != nil {
			return PathState{}, fmt.Errorf("transaction: hash %s: %w", target, err)
		}
		base.SHA256 = hex.EncodeToString(h.Sum(nil))
	case want.Schema != "":
		if e.schemaDetector == nil {
			return PathState{}, ErrSchemaDetectorRequired
		}
		data, err := io.ReadAll(io.LimitReader(f, maxSchemaDetectionBytes+1))
		if err != nil {
			return PathState{}, fmt.Errorf("transaction: read %s for schema detection: %w", target, err)
		}
		if int64(len(data)) > maxSchemaDetectionBytes {
			return PathState{}, fmt.Errorf("transaction: %s exceeds the %d byte schema-detection limit", target, maxSchemaDetectionBytes)
		}
		schemaName, err := e.schemaDetector(target, data)
		if err != nil {
			return PathState{}, fmt.Errorf("transaction: detect schema for %s: %w", target, err)
		}
		base.Schema = schemaName
	}
	return base, nil
}

// Journal returns the engine's current journal.
func (e *Engine) Journal() *Journal { return e.store.Journal() }

// stepHook lets a test observe or interrupt execution at a named point
// immediately after one primitive's syscall, simulating a crash right
// there: a non-nil return aborts the primitive at that point, leaving
// whatever on-disk state the syscalls run so far produced, and whatever
// journal status (started, not yet done) was last persisted. Production
// builds leave it a no-op. This package's tests must not use t.Parallel —
// like internal/backup's hookAfterOpen/hookBeforeMutation/hookAfterMkdirat
// seams it follows the same pattern from, it is process-global state.
var stepHook = func(point string) error { return nil }

// ancestorHook fires once per ancestor level, immediately after that
// level's directory fd is opened and before its identity is compared
// against the journal's dir_ids record (design §7.1's ancestor-chain
// re-verification; test placement mirrors v0.7.1 §6.5's
// hookAfterOpen-before-verification point). A test can use it to replace
// the directory entry at that path (with a symlink, or with a different
// real directory — a fresh inode) between this call's own open and its
// comparison, to prove the comparison is driven by the already-open fd's
// identity, not a second read of the path.
var ancestorHook = func(level int) {}

func containsStr(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}

// requireStep locates step id in the engine's forward Steps (rollback=false)
// or rollback Steps (rollback=true; Task 5 — §7.4's rollback continuation
// executes the file/directory primitives against RollbackSteps using the
// exact same syscall sequences and hook points as the forward primitives),
// and requires it to have kind and status exactly as given, and to name the
// exact same path as mp: mp is validated (ManagedPath.validate — malformed
// components, Keys that do not match RootKey/Comps) and its computed
// TargetKey must equal the step's Target. This is what stops "the path a
// caller safety-checked" and "the path a caller's step record claims to be
// about" from silently diverging — every Engine primitive routes through
// this before touching anything.
func (e *Engine) requireStep(rollback bool, id string, kind StepKind, status StepStatus, mp ManagedPath) (Step, error) {
	if err := mp.validate(); err != nil {
		return Step{}, err
	}
	steps := e.Journal().Steps
	if rollback {
		steps = e.Journal().RollbackSteps
	}
	for _, s := range steps {
		if s.ID != id {
			continue
		}
		if s.Kind != kind {
			return Step{}, fmt.Errorf("transaction: step %s has kind %s, want %s", id, s.Kind, kind)
		}
		// A caller asking for StatusPending (every primitive's own entry
		// point — this package has no other caller of requireStep) also
		// accepts a step already Started: that is exactly recover resuming
		// a step a previous crash left mid-primitive (§7.4 step 4: "started
		// 步骤按§7.2允许的状态幂等续做"; §7.5's committed-phase forward
		// cleanup steps need the identical resumption). Each primitive
		// below is written so re-running it against a Started step is safe
		// (idempotent): Mkdir/RemoveFile/RemoveDir tolerate their syscall
		// already having happened, WriteFile reuses (O_TRUNC, not O_EXCL)
		// rather than requiring a fresh temp name, and SetMeta's chown/chmod
		// are naturally idempotent. A step already Done is never accepted
		// here regardless — recover must never replay a completed
		// primitive.
		if s.Status != status && !(status == StatusPending && s.Status == StatusStarted) {
			return Step{}, fmt.Errorf("transaction: step %s is %s, want %s", id, s.Status, status)
		}
		if want := mp.TargetKey(); s.Target != want {
			return Step{}, fmt.Errorf("transaction: step %s targets %q, but the given ManagedPath resolves to %q", id, s.Target, want)
		}
		return s, nil
	}
	return Step{}, fmt.Errorf("transaction: unknown step id %s", id)
}

// transition moves step id from `from` to `to` and persists the journal —
// §7.1's "步骤执行前在journal标记started并持久化,完成后标记done并持久化" —
// against the forward Steps (rollback=false) or RollbackSteps
// (rollback=true).
func (e *Engine) transition(rollback bool, id string, from, to StepStatus) error {
	j := e.Journal().Clone()
	steps := j.Steps
	if rollback {
		steps = j.RollbackSteps
	}
	for i := range steps {
		if steps[i].ID != id {
			continue
		}
		if steps[i].Status != from {
			return fmt.Errorf("transaction: step %s is %s, not %s", id, steps[i].Status, from)
		}
		steps[i].Status = to
		return e.store.Update(j)
	}
	return fmt.Errorf("transaction: unknown step id %s", id)
}

// ensureStarted transitions step id from Pending to Started, or does
// nothing if it is already Started (the resume case: a previous crash left
// it there) — the idempotent replacement for every primitive's old
// unconditional transition(Pending, Started) call, letting requireStep's
// "Pending or Started" acceptance and this be the sole two places recovery
// resumption logic lives, rather than each primitive re-deriving it.
func (e *Engine) ensureStarted(rollback bool, id string) error {
	steps := e.Journal().Steps
	if rollback {
		steps = e.Journal().RollbackSteps
	}
	for _, s := range steps {
		if s.ID != id {
			continue
		}
		if s.Status == StatusStarted {
			return nil
		}
		return e.transition(rollback, id, StatusPending, StatusStarted)
	}
	return fmt.Errorf("transaction: unknown step id %s", id)
}

// RecordDirIdentity updates the journal's dir_ids entry for key and
// persists it, as its own, independent journal version — §7.1: a
// directory's new (or "does not exist") identity must be durable *before*
// anything treats it as fact, and separately from that same step's own
// started/done transition, so a crash between "the mkdir/rmdir syscall
// completed" and "done" leaves an unambiguous, recover-recognizable middle
// state (dir_ids already updated, status still started) rather than
// skipping straight to a state where done implies dir_ids is current
// without ever having durably said so. Engine.Mkdir and Engine.RemoveDir
// call this themselves, in the fixed order §7.1 requires (mkdir: parent
// fsync -> chmod -> chown -> RecordDirIdentity -> done; rmdir: parent
// fsync -> RecordDirIdentity -> done) — a caller no longer needs to (or
// should) call it separately after them.
func (e *Engine) RecordDirIdentity(key string, id DirIdentity) error {
	j := e.Journal().Clone()
	if j.DirIDs == nil {
		j.DirIDs = map[string]DirIdentity{}
	}
	j.DirIDs[key] = id
	return e.store.Update(j)
}

// CurrentIdentity returns fd's (dev, ino) identity as a DirIdentity ready
// for RecordDirIdentity.
func CurrentIdentity(f *os.File) (DirIdentity, error) { return identityOf(int(f.Fd())) }

// verifyAncestorsOnly re-resolves and re-verifies mp's ancestor chain
// against dirIDs (exactly as resolveVerifiedParentHooked does for a
// caller about to mutate through the returned parent), but is used where
// the actual next operation is an fd-relative fchmod/fchown that does not
// itself need a parent fd at all (Engine.Mkdir's chmod/chown on the
// directory's own already-open fd; Engine.WriteFile's chmod/chown on the
// temp file's own already-open fd) — §7.1 nonetheless lists fchmod and
// fchown among the operations requiring a fresh pre-check,
// so this performs exactly that check and discards the resolved chain.
func verifyAncestorsOnly(mp ManagedPath, dirIDs map[string]DirIdentity) error {
	_, opened, err := resolveVerifiedParentHooked(mp, dirIDs)
	closeAll(opened)
	return err
}

// verifyPreState requires the live state of name under the already
// ancestor-verified parent to equal want exactly, refusing to let the
// caller proceed to its next modifying syscall otherwise:
// without this, e.g. write_file's rename would silently overwrite content
// nothing ever snapshotted, or remove_file would delete a file that is not
// the one the plan recorded as present. For a directory (asDir) it reuses
// statLeafDir, which itself rejects any type other than "directory" or
// "does not exist". For a file it opens the leaf once (openLeafFile) and,
// via computeContentField, fills in whichever content field want uses
// (SHA256 or Schema — a dynamic-state Pre/Post
// that only carries a Schema name is not skipped, it goes through the
// installed SchemaDetector) before comparing — so the type restriction
// §7.1 requires and the content check both happen on the same open,
// rather than a separate, possibly-stale resolution.
func (e *Engine) verifyPreState(parent *os.File, name, target string, want PathState, asDir bool) error {
	var live PathState
	var err error
	if asDir {
		live, err = statLeafDir(parent, name)
	} else {
		var f *os.File
		var base PathState
		f, base, err = openLeafFile(parent, name)
		if err == nil {
			if f == nil {
				live = base // Exists: false
			} else {
				live, err = e.computeContentField(f, target, want, base)
				f.Close()
			}
		}
	}
	if err != nil {
		return err
	}
	if live != want {
		return fmt.Errorf("%w: live state %+v, want %+v", ErrPreStateMismatch, live, want)
	}
	return nil
}

// --- §7.2 primitives ---

// WriteFile executes a write_file step: O_EXCL-create the step's already-
// registered temp name under mp's verified parent, write data, fchmod and
// fchown to the step's Post metadata, fsync, rename to mp.Leaf, fsync the
// parent directory. Per §7.2's ordering, fchmod/fchown happen before
// rename, so the published file never has new content with stale
// metadata. The target's live state is required to equal st.Pre exactly
// both before the temp file is even created and again immediately before
// the rename that actually overwrites/creates it — the
// second check exists because time (and, in principle, another actor)
// passes between the two: writing and fsyncing the temp file, and the two
// ancestor-only re-verifications around its chmod/chown, are not
// instantaneous.
func (e *Engine) WriteFile(ctx context.Context, id string, mp ManagedPath, tmpName string, data []byte) error {
	return e.writeFile(ctx, false, id, mp, tmpName, data)
}

// RollbackWriteFile executes a write_file rollback step (§7.4 step 2: file
// content restored from a completed snapshot) against RollbackSteps,
// otherwise identical to WriteFile — same syscall order, same hook points,
// same pre-state re-verification.
func (e *Engine) RollbackWriteFile(ctx context.Context, id string, mp ManagedPath, tmpName string, data []byte) error {
	return e.writeFile(ctx, true, id, mp, tmpName, data)
}

func (e *Engine) writeFile(ctx context.Context, rollback bool, id string, mp ManagedPath, tmpName string, data []byte) error {
	st, err := e.requireStep(rollback, id, KindWriteFile, StatusPending, mp)
	if err != nil {
		return err
	}
	if !containsStr(st.Temps, tmpName) {
		return fmt.Errorf("transaction: step %s: temp name %s is not registered in Temps", id, tmpName)
	}
	// resuming is true only when this step was already Started *before*
	// this call (captured from requireStep's return, ahead of
	// ensureStarted's own possible transition) — i.e. a genuine resume of
	// a step a previous crash left mid-primitive, never a fresh Pending
	// call. Every "the target already looks advanced" shortcut below is
	// gated on this: a fresh call must always find the target exactly at
	// st.Pre, exactly as Task 4 originally required (TestPreStateEnforcement),
	// or fail with ErrPreStateMismatch — it must never silently treat an
	// unexpected pre-existing Post-equal (or, for set_meta, "mid"-equal)
	// state as "someone already did this for me".
	resuming := st.Status == StatusStarted
	if err := e.ensureStarted(rollback, id); err != nil {
		return err
	}

	if resuming {
		// §7.2's allowed-started set for write_file is exactly {target ∈
		// pre, post} — a previous attempt may have already completed the
		// rename (crashed between "write_file:after_dir_fsync" and this
		// step's own done transition). Detect it before touching the temp
		// name at all, so a resumed run never re-creates or re-writes a
		// temp file it does not need.
		wantHash := st.Pre.SHA256 != "" || st.Post.SHA256 != ""
		wantSchema := st.Pre.Schema != "" || st.Post.Schema != ""
		liveNow, lerr := e.LiveFileState(mp, wantHash, wantSchema)
		if lerr != nil {
			return lerr
		}
		if liveNow == st.Post {
			return e.transition(rollback, id, StatusStarted, StatusDone)
		}
		if liveNow != st.Pre {
			return fmt.Errorf("%w: live state %+v, want %+v or %+v", ErrPreStateMismatch, liveNow, st.Pre, st.Post)
		}
	}

	mode := fs.FileMode(st.Post.Mode)
	uid, gid := st.Post.UID, st.Post.GID

	if err := checkCancel(ctx); err != nil {
		return err
	}
	parent, opened, err := resolveVerifiedParentHooked(mp, e.Journal().DirIDs)
	if err != nil {
		closeAll(opened)
		return err
	}
	if err := e.verifyPreState(parent, mp.Leaf, mp.TargetKey(), st.Pre, false); err != nil {
		closeAll(opened)
		return err
	}
	fd, err := unix.Openat(int(parent.Fd()), tmpName, unix.O_CREAT|unix.O_EXCL|unix.O_WRONLY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0600)
	if err != nil && errors.Is(err, unix.EEXIST) && st.Status == StatusStarted {
		// Genuinely resuming (st.Status was already Started when
		// requireStep read it, before ensureStarted's own possible
		// transition) after a crash between this temp file's creation and
		// the rename that consumes it: §7.2 permits the registered temp
		// name to already exist "with any content or metadata" while
		// started, so truncate and rewrite it rather than treating a name
		// only this transaction could ever have created as an unexplained
		// collision. A fresh (Pending-at-entry) call never takes this
		// branch — it keeps the original O_EXCL collision detection.
		fd, err = unix.Openat(int(parent.Fd()), tmpName, unix.O_CREAT|unix.O_TRUNC|unix.O_WRONLY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0600)
	}
	closeAll(opened)
	if err != nil {
		return fmt.Errorf("transaction: create %s: %w", tmpName, err)
	}
	tmp := os.NewFile(uintptr(fd), tmpName)
	defer tmp.Close()
	if err := hook(stepHook, "write_file:after_create"); err != nil {
		return err
	}

	if _, err := tmp.Write(data); err != nil {
		return fmt.Errorf("transaction: write %s: %w", tmpName, err)
	}
	if err := hook(stepHook, "write_file:after_write"); err != nil {
		return err
	}

	if err := checkCancel(ctx); err != nil {
		return err
	}
	if err := verifyAncestorsOnly(mp, e.Journal().DirIDs); err != nil {
		return err
	}
	if err := tmp.Chmod(mode); err != nil {
		return fmt.Errorf("transaction: chmod %s: %w", tmpName, err)
	}
	if err := hook(stepHook, "write_file:after_chmod"); err != nil {
		return err
	}

	if err := checkCancel(ctx); err != nil {
		return err
	}
	if err := verifyAncestorsOnly(mp, e.Journal().DirIDs); err != nil {
		return err
	}
	if err := tmp.Chown(int(uid), int(gid)); err != nil {
		return fmt.Errorf("transaction: chown %s: %w", tmpName, err)
	}
	if err := hook(stepHook, "write_file:after_chown"); err != nil {
		return err
	}

	if err := tmp.Sync(); err != nil {
		return fmt.Errorf("transaction: fsync %s: %w", tmpName, err)
	}
	if err := hook(stepHook, "write_file:after_fsync"); err != nil {
		return err
	}

	if err := checkCancel(ctx); err != nil {
		return err
	}
	parent2, opened2, err := resolveVerifiedParentHooked(mp, e.Journal().DirIDs)
	if err != nil {
		closeAll(opened2)
		return err
	}
	if err := e.verifyPreState(parent2, mp.Leaf, mp.TargetKey(), st.Pre, false); err != nil {
		closeAll(opened2)
		return err
	}
	renameErr := unix.Renameat(int(parent2.Fd()), tmpName, int(parent2.Fd()), mp.Leaf)
	if renameErr != nil {
		closeAll(opened2)
		return fmt.Errorf("transaction: rename %s to %s: %w", tmpName, mp.Leaf, renameErr)
	}
	if err := hook(stepHook, "write_file:after_rename"); err != nil {
		closeAll(opened2)
		return err
	}
	syncErr := parent2.Sync()
	closeAll(opened2)
	if syncErr != nil {
		return fmt.Errorf("transaction: fsync directory for %s: %w", mp.Leaf, syncErr)
	}
	if err := hook(stepHook, "write_file:after_dir_fsync"); err != nil {
		return err
	}
	return e.transition(rollback, id, StatusStarted, StatusDone)
}

// Mkdir executes a mkdir step: Mkdirat -> fsync the parent directory ->
// Fchmod(post mode) -> Fchown -> durably record the new directory's own
// identity -> mark done, per §7.2 and §7.1's "mkdir 完成后...先更新
// journal,把该目录的记录改为新的{dev, ino}". The
// identity is recorded by this call itself, as its own journal version
// (RecordDirIdentity), strictly before the step is transitioned to done —
// a caller no longer records it separately. Ancestor re-verification runs
// again, discarding its own resolved chain, immediately before chmod and
// again before chown, even though both act on the already-
// open new-directory fd rather than by name. It returns the newly created
// directory's open fd.
func (e *Engine) Mkdir(ctx context.Context, id string, mp ManagedPath) (*os.File, error) {
	return e.mkdir(ctx, false, id, mp)
}

// RollbackMkdir executes a mkdir rollback step (§7.4 step 2: recreating a
// directory the forward transaction removed) against RollbackSteps,
// otherwise identical to Mkdir.
func (e *Engine) RollbackMkdir(ctx context.Context, id string, mp ManagedPath) (*os.File, error) {
	return e.mkdir(ctx, true, id, mp)
}

func (e *Engine) mkdir(ctx context.Context, rollback bool, id string, mp ManagedPath) (*os.File, error) {
	st, err := e.requireStep(rollback, id, KindMkdir, StatusPending, mp)
	if err != nil {
		return nil, err
	}
	// See writeFile's identical comment: resuming gates every "the target
	// already looks advanced" shortcut below, so a fresh Pending call keeps
	// Task 4's original guarantee of failing on an unexpected pre-existing
	// directory (TestPreStateEnforcement) instead of mistaking it for its
	// own prior partial attempt.
	resuming := st.Status == StatusStarted
	if err := e.ensureStarted(rollback, id); err != nil {
		return nil, err
	}
	mode := fs.FileMode(st.Post.Mode)
	uid, gid := st.Post.UID, st.Post.GID

	if err := checkCancel(ctx); err != nil {
		return nil, err
	}

	alreadyCreated := false
	if resuming {
		// Resume support (mirrors writeFile's three-way check): a previous
		// crash may have left the directory already fully finished (chmod,
		// chown and dir_ids all done — only this step's own done
		// transition is missing) or already created but not yet
		// chmod/chown'd (§7.2's allowed-started set: "空目录且owner为
		// root:root、mode任意"). Detect both before touching Mkdirat, so a
		// resumed run never re-attempts a syscall that already succeeded.
		liveDir, liveEmpty, derr := e.LiveDirState(mp)
		if derr != nil {
			return nil, derr
		}
		if liveDir == st.Post {
			dirFd, oerr := e.openManagedChildDir(mp)
			if oerr != nil {
				return nil, oerr
			}
			// R5 fix: the pre-transaction dir_ids entry for a brand-new
			// directory is already present as {Exists: false} (registered
			// at transaction start, §7.1) — checking only "is a key
			// present" (the old `!ok` condition) therefore never fires for
			// the single most common case (a genuinely new directory) and
			// dir_ids is left stale forever. Compare the *value*: only
			// skip re-registering when the recorded identity already
			// equals the live one.
			newID, idErr := CurrentIdentity(dirFd)
			if idErr != nil {
				return dirFd, idErr
			}
			if recorded, ok := e.Journal().DirIDs[mp.TargetKey()]; !ok || recorded != newID {
				if err := e.RecordDirIdentity(mp.TargetKey(), newID); err != nil {
					return dirFd, err
				}
			}
			if err := e.transition(rollback, id, StatusStarted, StatusDone); err != nil {
				return dirFd, err
			}
			return dirFd, nil
		}
		alreadyCreated = liveDir.Exists && liveDir.Type == PathTypeDir && liveEmpty &&
			int(liveDir.UID) == os.Getuid() && int(liveDir.GID) == os.Getgid()
		if liveDir.Exists && !alreadyCreated {
			return nil, fmt.Errorf("%w: live directory state %+v is not a legal mkdir resume point", ErrPreStateMismatch, liveDir)
		}
	}

	parent, opened, err := resolveVerifiedParentHooked(mp, e.Journal().DirIDs)
	if err != nil {
		closeAll(opened)
		return nil, err
	}
	if !alreadyCreated {
		if err := e.verifyPreState(parent, mp.Leaf, mp.TargetKey(), st.Pre, true); err != nil {
			closeAll(opened)
			return nil, err
		}
		mkErr := unix.Mkdirat(int(parent.Fd()), mp.Leaf, uint32(mode.Perm()))
		if mkErr != nil {
			closeAll(opened)
			return nil, fmt.Errorf("transaction: mkdir %s: %w", mp.Leaf, mkErr)
		}
		if err := hook(stepHook, "mkdir:after_mkdirat"); err != nil {
			closeAll(opened)
			return nil, err
		}
	}
	dirFd, openErr := openChildDir(parent, mp.Leaf)
	if openErr != nil {
		closeAll(opened)
		return nil, fmt.Errorf("transaction: %s needs manual confirmation after creation: %w", mp.Leaf, openErr)
	}
	syncErr := parent.Sync()
	closeAll(opened)
	if syncErr != nil {
		dirFd.Close()
		return nil, fmt.Errorf("transaction: fsync directory for %s: %w", mp.Leaf, syncErr)
	}
	if err := hook(stepHook, "mkdir:after_parent_fsync"); err != nil {
		return dirFd, err
	}

	if err := checkCancel(ctx); err != nil {
		return dirFd, err
	}
	if err := verifyAncestorsOnly(mp, e.Journal().DirIDs); err != nil {
		return dirFd, err
	}
	if err := dirFd.Chmod(mode); err != nil {
		return dirFd, fmt.Errorf("transaction: chmod %s: %w", mp.Leaf, err)
	}
	if err := hook(stepHook, "mkdir:after_chmod"); err != nil {
		return dirFd, err
	}

	if err := checkCancel(ctx); err != nil {
		return dirFd, err
	}
	if err := verifyAncestorsOnly(mp, e.Journal().DirIDs); err != nil {
		return dirFd, err
	}
	if err := dirFd.Chown(int(uid), int(gid)); err != nil {
		return dirFd, fmt.Errorf("transaction: chown %s: %w", mp.Leaf, err)
	}
	if err := hook(stepHook, "mkdir:after_chown"); err != nil {
		return dirFd, err
	}

	newID, idErr := CurrentIdentity(dirFd)
	if idErr != nil {
		return dirFd, idErr
	}
	if err := e.RecordDirIdentity(mp.TargetKey(), newID); err != nil {
		return dirFd, err
	}
	if err := hook(stepHook, "mkdir:after_identity"); err != nil {
		return dirFd, err
	}
	if err := e.transition(rollback, id, StatusStarted, StatusDone); err != nil {
		return dirFd, err
	}
	return dirFd, nil
}

// SetMeta executes a set_meta step: chown then chmod, per §7.2. Both are
// done without ever resolving mp.Leaf by name at the moment of the actual
// metadata change: chown uses Fchownat's AT_SYMLINK_NOFOLLOW (which the
// classic chown family does support), and chmod opens the leaf O_NOFOLLOW
// and Fchmods that fd (fchmodByFd) instead of calling the classic
// Fchmodat, which has no symlink-safe form at all on kernels older than
// 6.6 (design v0.7.1 §6.3). The target's live state is required to equal
// st.Pre exactly immediately before the chown; mp.Leaf's
// type (file or directory) is taken from st.Pre.Type. The ancestor chain
// is re-verified again, independently, immediately before the chmod.
func (e *Engine) SetMeta(ctx context.Context, id string, mp ManagedPath) error {
	return e.setMeta(ctx, false, id, mp)
}

// RollbackSetMeta executes a set_meta rollback step against RollbackSteps,
// otherwise identical to SetMeta.
func (e *Engine) RollbackSetMeta(ctx context.Context, id string, mp ManagedPath) error {
	return e.setMeta(ctx, true, id, mp)
}

func (e *Engine) setMeta(ctx context.Context, rollback bool, id string, mp ManagedPath) error {
	st, err := e.requireStep(rollback, id, KindSetMeta, StatusPending, mp)
	if err != nil {
		return err
	}
	// See writeFile's identical comment.
	resuming := st.Status == StatusStarted
	if err := e.ensureStarted(rollback, id); err != nil {
		return err
	}
	mode := fs.FileMode(st.Post.Mode)
	uid, gid := st.Post.UID, st.Post.GID
	asDir := st.Pre.Type == PathTypeDir

	skipChown := false
	if resuming {
		// Resume support (mirrors the other primitives): §7.2's
		// allowed-started set for set_meta is {pre, post, and a "mid"
		// state with post's owner but pre's mode} — a previous crash may
		// have completed the chown already (mid) or both the chown and
		// chmod already (post). Pre is checked first: when pre and mid
		// coincide (this step never actually changes ownership), a live
		// state exactly at pre must still be treated as "nothing done
		// yet", not as "chown already happened to be a no-op".
		mid := st.Pre
		mid.UID, mid.GID = st.Post.UID, st.Post.GID
		var (
			liveNow    PathState
			lerr       error
			wantHash   = st.Pre.SHA256 != "" || st.Post.SHA256 != ""
			wantSchema = st.Pre.Schema != "" || st.Post.Schema != ""
		)
		if asDir {
			liveNow, _, lerr = e.LiveDirState(mp)
		} else {
			liveNow, lerr = e.LiveFileState(mp, wantHash, wantSchema)
		}
		if lerr != nil {
			return lerr
		}
		switch {
		case liveNow == st.Pre:
			// normal path below
		case liveNow == st.Post:
			return e.transition(rollback, id, StatusStarted, StatusDone)
		case liveNow == mid:
			skipChown = true
		default:
			return fmt.Errorf("%w: live state %+v, want %+v, %+v or %+v", ErrPreStateMismatch, liveNow, st.Pre, mid, st.Post)
		}
	}

	if err := checkCancel(ctx); err != nil {
		return err
	}
	if !skipChown {
		parent, opened, err := resolveVerifiedParentHooked(mp, e.Journal().DirIDs)
		if err != nil {
			closeAll(opened)
			return err
		}
		if err := e.verifyPreState(parent, mp.Leaf, mp.TargetKey(), st.Pre, asDir); err != nil {
			closeAll(opened)
			return err
		}
		chownErr := unix.Fchownat(int(parent.Fd()), mp.Leaf, int(uid), int(gid), unix.AT_SYMLINK_NOFOLLOW)
		closeAll(opened)
		if chownErr != nil {
			return fmt.Errorf("transaction: chown %s: %w", mp.Leaf, chownErr)
		}
		if err := hook(stepHook, "set_meta:after_chown"); err != nil {
			return err
		}
	}

	if err := checkCancel(ctx); err != nil {
		return err
	}
	parent2, opened2, err := resolveVerifiedParentHooked(mp, e.Journal().DirIDs)
	if err != nil {
		closeAll(opened2)
		return err
	}
	chmodErr := fchmodByFd(parent2, mp.Leaf, mode)
	closeAll(opened2)
	if chmodErr != nil {
		return fmt.Errorf("transaction: chmod %s: %w", mp.Leaf, chmodErr)
	}
	if err := hook(stepHook, "set_meta:after_chmod"); err != nil {
		return err
	}
	return e.transition(rollback, id, StatusStarted, StatusDone)
}

// RemoveFile executes a remove_file step: unlink then fsync the parent
// directory, per §7.2. The target's live state is required to equal
// st.Pre exactly immediately before the unlink.
func (e *Engine) RemoveFile(ctx context.Context, id string, mp ManagedPath) error {
	return e.removeFile(ctx, false, id, mp)
}

// RollbackRemoveFile executes a remove_file rollback step (§7.4 step 2: the
// target did not exist before the transaction) against RollbackSteps,
// otherwise identical to RemoveFile.
func (e *Engine) RollbackRemoveFile(ctx context.Context, id string, mp ManagedPath) error {
	return e.removeFile(ctx, true, id, mp)
}

func (e *Engine) removeFile(ctx context.Context, rollback bool, id string, mp ManagedPath) error {
	st, err := e.requireStep(rollback, id, KindRemoveFile, StatusPending, mp)
	if err != nil {
		return err
	}
	// See writeFile's identical comment.
	resuming := st.Status == StatusStarted
	if err := e.ensureStarted(rollback, id); err != nil {
		return err
	}

	if resuming {
		// §7.2's allowed-started set for remove_file is {pre,
		// does-not-exist} — a previous crash may have already completed
		// the unlink. Detect that before attempting it again, so a
		// resumed run never re-unlinks (ENOENT) a file that is already
		// gone.
		wantHash := st.Pre.SHA256 != ""
		wantSchema := st.Pre.Schema != ""
		liveNow, lerr := e.LiveFileState(mp, wantHash, wantSchema)
		if lerr != nil {
			return lerr
		}
		if liveNow == st.Post {
			return e.transition(rollback, id, StatusStarted, StatusDone)
		}
		if liveNow != st.Pre {
			return fmt.Errorf("%w: live state %+v, want %+v or %+v", ErrPreStateMismatch, liveNow, st.Pre, st.Post)
		}
	}

	if err := checkCancel(ctx); err != nil {
		return err
	}
	parent, opened, err := resolveVerifiedParentHooked(mp, e.Journal().DirIDs)
	if err != nil {
		closeAll(opened)
		return err
	}
	if err := e.verifyPreState(parent, mp.Leaf, mp.TargetKey(), st.Pre, false); err != nil {
		closeAll(opened)
		return err
	}
	unlinkErr := unix.Unlinkat(int(parent.Fd()), mp.Leaf, 0)
	if unlinkErr != nil {
		closeAll(opened)
		return fmt.Errorf("transaction: unlink %s: %w", mp.Leaf, unlinkErr)
	}
	if err := hook(stepHook, "remove_file:after_unlink"); err != nil {
		closeAll(opened)
		return err
	}
	syncErr := parent.Sync()
	closeAll(opened)
	if syncErr != nil {
		return fmt.Errorf("transaction: fsync directory for %s: %w", mp.Leaf, syncErr)
	}
	if err := hook(stepHook, "remove_file:after_fsync"); err != nil {
		return err
	}
	return e.transition(rollback, id, StatusStarted, StatusDone)
}

// RemoveDir executes a remove_dir step: rmdir -> fsync the parent
// directory -> durably record the directory's new "does not exist"
// identity -> mark done, per §7.1 and §7.2 (the directory must
// already be empty — an OS-level guarantee: a non-empty directory simply
// fails ENOTEMPTY here rather than being handled as a distinct case). The
// target's live state is required to equal st.Pre exactly immediately
// before the rmdir.
func (e *Engine) RemoveDir(ctx context.Context, id string, mp ManagedPath) error {
	return e.removeDir(ctx, false, id, mp)
}

// RollbackRemoveDir executes a remove_dir rollback step (§7.4 step 2: the
// target directory did not exist before the transaction) against
// RollbackSteps, otherwise identical to RemoveDir.
func (e *Engine) RollbackRemoveDir(ctx context.Context, id string, mp ManagedPath) error {
	return e.removeDir(ctx, true, id, mp)
}

func (e *Engine) removeDir(ctx context.Context, rollback bool, id string, mp ManagedPath) error {
	st, err := e.requireStep(rollback, id, KindRemoveDir, StatusPending, mp)
	if err != nil {
		return err
	}
	// See writeFile's identical comment.
	resuming := st.Status == StatusStarted
	if err := e.ensureStarted(rollback, id); err != nil {
		return err
	}

	if resuming {
		// §7.2's allowed-started set for remove_dir is {empty dir matching
		// pre's metadata, does-not-exist} — a previous crash may have
		// already completed the rmdir.
		liveDir, _, derr := e.LiveDirState(mp)
		if derr != nil {
			return derr
		}
		if liveDir == st.Post {
			if recorded, ok := e.Journal().DirIDs[mp.TargetKey()]; !ok || recorded.Exists {
				if err := e.RecordDirIdentity(mp.TargetKey(), DirIdentity{Exists: false}); err != nil {
					return err
				}
			}
			return e.transition(rollback, id, StatusStarted, StatusDone)
		}
	}

	if err := checkCancel(ctx); err != nil {
		return err
	}
	parent, opened, err := resolveVerifiedParentHooked(mp, e.Journal().DirIDs)
	if err != nil {
		closeAll(opened)
		return err
	}
	if err := e.verifyPreState(parent, mp.Leaf, mp.TargetKey(), st.Pre, true); err != nil {
		closeAll(opened)
		return err
	}
	rmErr := unix.Unlinkat(int(parent.Fd()), mp.Leaf, unix.AT_REMOVEDIR)
	if rmErr != nil {
		closeAll(opened)
		return fmt.Errorf("transaction: rmdir %s: %w", mp.Leaf, rmErr)
	}
	if err := hook(stepHook, "remove_dir:after_rmdir"); err != nil {
		closeAll(opened)
		return err
	}
	syncErr := parent.Sync()
	closeAll(opened)
	if syncErr != nil {
		return fmt.Errorf("transaction: fsync directory for %s: %w", mp.Leaf, syncErr)
	}
	if err := hook(stepHook, "remove_dir:after_parent_fsync"); err != nil {
		return err
	}
	if err := e.RecordDirIdentity(mp.TargetKey(), DirIdentity{Exists: false}); err != nil {
		return err
	}
	if err := hook(stepHook, "remove_dir:after_identity"); err != nil {
		return err
	}
	return e.transition(rollback, id, StatusStarted, StatusDone)
}

// Snapshot executes a snapshot step: verify the source's live state
// matches st.Pre exactly (content hash included), copy
// its bytes into the transaction area's snap/ directory under the step's
// already-registered temp name, fsync, rename into place, and fsync the
// snap/ directory itself before marking done. Unlike
// every other primitive, its source is a ManagedPath resolved and
// ancestor-verified exactly like any other managed path
// — it does not trust a caller-supplied bare fd. snapDir is the Store's
// own snap/ directory, already created and held open by the caller (its
// own mkdir is a separate step this call does not perform); it is not
// itself a managed path (§7.1: the transaction area is not subject to
// dir_ids/ancestor re-verification, see journal.go's writeTempAndRename
// doc comment for the same point about journal.json itself).
func (e *Engine) Snapshot(ctx context.Context, id string, mp ManagedPath, snapDir *os.File, tmpName string) error {
	st, err := e.requireStep(false, id, KindSnapshot, StatusPending, mp)
	if err != nil {
		return err
	}
	if !containsStr(st.Temps, tmpName) {
		return fmt.Errorf("transaction: step %s: temp name %s is not registered in Temps", id, tmpName)
	}
	if err := e.transition(false, id, StatusPending, StatusStarted); err != nil {
		return err
	}

	if err := checkCancel(ctx); err != nil {
		return err
	}
	parent, opened, err := resolveVerifiedParentHooked(mp, e.Journal().DirIDs)
	if err != nil {
		closeAll(opened)
		return err
	}
	src, err := e.openVerifiedSource(parent, mp.Leaf, mp.TargetKey(), st.Pre)
	closeAll(opened)
	if err != nil {
		return err
	}
	defer src.Close()
	if err := hook(stepHook, "snapshot:after_verified_open"); err != nil {
		return err
	}

	dstFd, err := unix.Openat(int(snapDir.Fd()), tmpName, unix.O_CREAT|unix.O_EXCL|unix.O_WRONLY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0600)
	if err != nil {
		return fmt.Errorf("transaction: create snapshot temp %s: %w", tmpName, err)
	}
	dst := os.NewFile(uintptr(dstFd), tmpName)
	defer dst.Close()
	if err := hook(stepHook, "snapshot:after_create"); err != nil {
		return err
	}

	if _, err := copyFile(dst, src); err != nil {
		return fmt.Errorf("transaction: copy snapshot %s: %w", mp.Leaf, err)
	}
	if err := hook(stepHook, "snapshot:after_copy"); err != nil {
		return err
	}
	if err := dst.Sync(); err != nil {
		return fmt.Errorf("transaction: fsync snapshot %s: %w", tmpName, err)
	}
	if err := hook(stepHook, "snapshot:after_fsync"); err != nil {
		return err
	}

	if err := checkCancel(ctx); err != nil {
		return err
	}
	if err := unix.Renameat(int(snapDir.Fd()), tmpName, int(snapDir.Fd()), tmpName+".snap"); err != nil {
		return fmt.Errorf("transaction: publish snapshot %s: %w", tmpName, err)
	}
	if err := hook(stepHook, "snapshot:after_rename"); err != nil {
		return err
	}
	if err := snapDir.Sync(); err != nil {
		return fmt.Errorf("transaction: fsync snap directory: %w", err)
	}
	if err := hook(stepHook, "snapshot:after_dir_fsync"); err != nil {
		return err
	}
	return e.transition(false, id, StatusStarted, StatusDone)
}

func hook(h func(string) error, point string) error { return h(point) }

func copyFile(dst, src *os.File) (int64, error) {
	return io.Copy(dst, src)
}

// openVerifiedSource opens mp's leaf under the already ancestor-verified
// parent (O_NOFOLLOW|O_NONBLOCK: a FIFO must not be able to
// hang this open), requires it to be a regular file, fills in whichever of
// want's content fields applies (SHA256 or, via the installed
// SchemaDetector, Schema — a dynamic-state source
// is not skipped) while reading it exactly once, and requires the
// resulting full PathState to equal want exactly before
// seeking back to the start and returning it — so Engine.Snapshot's later
// copy reads the very same bytes this check just read, rather than
// re-opening by name and possibly reading something else that raced in
// between (Snapshot's ManagedPath resolution is the ancestor-verification
// half of the same guarantee).
func (e *Engine) openVerifiedSource(parent *os.File, name, target string, want PathState) (*os.File, error) {
	f, base, err := openLeafFile(parent, name)
	if err != nil {
		return nil, err
	}
	if f == nil {
		return nil, fmt.Errorf("%w: %s: %v", ErrPreStateMismatch, name, os.ErrNotExist)
	}
	live, err := e.computeContentField(f, target, want, base)
	if err != nil {
		f.Close()
		return nil, err
	}
	if live != want {
		f.Close()
		return nil, fmt.Errorf("%w: live state %+v, want %+v", ErrPreStateMismatch, live, want)
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		f.Close()
		return nil, fmt.Errorf("transaction: seek %s: %w", name, err)
	}
	return f, nil
}

// fchmodByFd sets mode on name under parent without ever resolving name a
// second time and without any by-name chmod call at all: the classic
// fchmodat syscall does not support AT_SYMLINK_NOFOLLOW (fchmodat2, which
// does, needs Linux 6.6+ — see the v0.7.1 design's §6.3 rationale). It
// opens name once (O_NOFOLLOW|O_NONBLOCK: a FIFO must not be
// able to hang this open), requires the opened fd's own fstat to show a
// regular file or a directory (set_meta may target either, per
// st.Pre.Type — anything else, e.g. a FIFO or a device, is
// ErrIllegalPathType) and Fchmods that same fd.
func fchmodByFd(parent *os.File, name string, mode fs.FileMode) error {
	f, st, err := openLeafNoFollow(parent, name)
	if err != nil {
		return fmt.Errorf("transaction: open %s for chmod: %w", name, err)
	}
	defer f.Close()
	if st.Mode&unix.S_IFMT != unix.S_IFREG && st.Mode&unix.S_IFMT != unix.S_IFDIR {
		return fmt.Errorf("%w: %s is neither a regular file nor a directory", ErrIllegalPathType, name)
	}
	return f.Chmod(mode)
}

// resolveVerifiedParentHooked wraps resolveVerifiedParent, invoking
// ancestorHook after each ancestor level's fd is opened (design §7.1's
// per-level ancestor check), for TestAncestorReplacement.
func resolveVerifiedParentHooked(mp ManagedPath, dirIDs map[string]DirIdentity) (*os.File, []*os.File, error) {
	return resolveVerifiedParent(mp, dirIDs, ancestorHook)
}

// --- §7.3 phase=running per-path judgement ---

// DetermineRunningState implements §7.3's phase=running rule for a single
// path's step chain (chain must be PathChain's result: the steps sharing
// one Target, in execution order). It returns nil when live is a legal
// state for that chain; otherwise ErrIllegalProgress (or ErrInvalidJournal
// for a status this package does not otherwise reject earlier). liveEmpty
// is only consulted when the chain's relevant step is a mkdir or
// remove_dir currently `started` — whether the directory has any entries
// is a directory-listing question a bare PathState cannot answer, so the
// caller (which already has to open the directory to answer it) supplies
// it directly.
func DetermineRunningState(chain []Step, live PathState, liveEmpty bool) error {
	if len(chain) == 0 {
		return fmt.Errorf("%w: empty path chain", ErrInvalidJournal)
	}
	lastIdx := -1
	for i, s := range chain {
		if s.Status != StatusPending {
			lastIdx = i
		}
	}
	if lastIdx == -1 {
		if live != chain[0].Pre {
			return fmt.Errorf("%w: path %s: all steps pending but live state does not match the first step's pre", ErrIllegalProgress, chain[0].Target)
		}
		return nil
	}
	sk := chain[lastIdx]
	switch sk.Status {
	case StatusDone:
		if live != sk.Post {
			return fmt.Errorf("%w: path %s: step %s is done but live state does not match its post", ErrIllegalProgress, sk.Target, sk.ID)
		}
		return nil
	case StatusStarted:
		if allowedStarted(sk.Kind, sk.Pre, sk.Post, live, liveEmpty) {
			return nil
		}
		return fmt.Errorf("%w: path %s: step %s is started but live state is not in %s's allowed set", ErrIllegalProgress, sk.Target, sk.ID, sk.Kind)
	default:
		return fmt.Errorf("%w: step %s has unexpected status %s", ErrInvalidJournal, sk.ID, sk.Status)
	}
}

// DetermineRollingBackState implements §7.3's phase=rolling_back rule for a
// single path: rollbackChain is that path's rollback-step chain (PathChain
// applied to Journal.RollbackSteps); forwardChain is the same path's
// forward chain, needed only to read its first step's Pre (the
// pre-transaction state) for a path with no rollback steps at all — §7.3:
// "没有回滚步骤的路径必须保持事务前状态". When rollbackChain is non-empty,
// this is exactly DetermineRunningState applied to the rollback chain
// instead of the forward one (§7.3: "按该路径的回滚步骤链使用上面的running
// 判定规则，pre/post和中间状态都取自回滚原语").
func DetermineRollingBackState(rollbackChain, forwardChain []Step, live PathState, liveEmpty bool) error {
	if len(rollbackChain) == 0 {
		if len(forwardChain) == 0 {
			return fmt.Errorf("%w: empty forward and rollback chains", ErrInvalidJournal)
		}
		if live != forwardChain[0].Pre {
			return fmt.Errorf("%w: path %s: no rollback step for this path, but live state does not match its pre-transaction state", ErrIllegalProgress, forwardChain[0].Target)
		}
		return nil
	}
	return DetermineRunningState(rollbackChain, live, liveEmpty)
}

// allowedStarted reports whether live is one of §7.2's table's allowed
// intermediate states for a step of kind, currently started, with the
// given pre/post. The owner a mkdir's still-empty intermediate directory
// must have is the *current process's* uid/gid — what Mkdirat actually
// assigns a newly created directory is always the caller's own euid/egid,
// never a value this package chooses, so comparing against the live
// process (os.Getuid()/os.Getgid()) is what makes this check correct both
// in production (root, uid/gid 0) and under a non-root test run, rather than hard-coding 0.
func allowedStarted(kind StepKind, pre, post PathState, live PathState, liveEmpty bool) bool {
	switch kind {
	case KindWriteFile:
		return live == pre || live == post
	case KindMkdir:
		if !live.Exists {
			return true
		}
		// §7.2's allowed-started set for mkdir is {不存在; 空目录且owner为
		// root:root、mode任意; post} — every one of these three states is
		// empty. A legitimate run can never have this step's own step
		// still `started` while anything has been placed *inside* the
		// directory it creates: the journal-wide status-prefix invariant
		// (validateStatusPrefix, applied across the whole steps[] array,
		// not per-target) means a later step targeting a child path can
		// only have progressed once this mkdir step itself is `done`, not
		// merely `started`. So a live directory that is not empty — even
		// if its mode/owner already coincidentally equal post (chmod and
		// chown both already ran, just the done transition itself has
		// not) — was populated by something outside this transaction and
		// must be rejected, not accepted as "already at post".
		if !liveEmpty {
			return false
		}
		if live.Type == PathTypeDir && int(live.UID) == os.Getuid() && int(live.GID) == os.Getgid() {
			return true
		}
		return live == post
	case KindSetMeta:
		if live == pre || live == post {
			return true
		}
		mid := pre
		mid.UID, mid.GID = post.UID, post.GID
		return live == mid
	case KindRemoveFile:
		return live == pre || !live.Exists
	case KindRemoveDir:
		if !live.Exists {
			return true
		}
		if !liveEmpty {
			return false
		}
		return live.Type == pre.Type && live.Mode == pre.Mode && live.UID == pre.UID && live.GID == pre.GID
	case KindSnapshot:
		// snapshot never modifies the managed path itself:
		// its target must remain exactly Pre for as long as the step is
		// started.
		return live == pre
	case KindSystemdEnable, KindSystemdDisable:
		// §7.2: "UnitFileState ∈ {pre, post}" — the only field of live a
		// systemd_enable/disable judgement ever inspects; a caller constructing live for one of these steps
		// leaves every other PathState field zero.
		return live.UnitFileState == pre.UnitFileState || live.UnitFileState == post.UnitFileState
	case KindSystemdDaemonReload, KindSystemdStart, KindSystemdRestart, KindSystemdStop:
		// §7.2: daemon_reload is "不检查"; start/restart/stop's
		// ActiveState is explicitly never a tamper basis ("ActiveState 不
		// 作为篡改依据") — no live field is judged for any of these.
		return true
	default:
		return false
	}
}

// chainContentKind reports whether any step in chain (forward or rollback)
// identifies its target's content by SHA256, by a dynamic-state Schema
// name, or neither (a directory target, or a file target this chain never
// compares by content) — recovery.go's validation pass uses this to know
// which of PathState's two mutually exclusive content fields a fresh live
// read of that target must fill in, since a given target's whole chain
// consistently uses at most one of them (Validate's contentFieldsValid
// already rejects any single step using both at once).
func chainContentKind(chain []Step) (wantHash, wantSchema bool) {
	for _, s := range chain {
		if s.Pre.SHA256 != "" || s.Post.SHA256 != "" {
			wantHash = true
		}
		if s.Pre.Schema != "" || s.Post.Schema != "" {
			wantSchema = true
		}
	}
	return
}

// isAncestorLegitimatelyAbsent reports whether err is resolveVerifiedParent's
// "this ancestor level is confirmed absent, and that is legitimate" outcome
// — either a plain wrapped os.ErrNotExist (dir_ids itself already recorded
// the level as absent) or errAncestorAbsentAccepted (dir_ids still says
// existing, but an in-progress remove_dir step accounts for the gap) — as
// opposed to ErrAncestorMismatch (a level disagreeing with dir_ids with no
// accepted explanation — a real tamper signal) or any other error.
func isAncestorLegitimatelyAbsent(err error) bool {
	return errors.Is(err, os.ErrNotExist) || errors.Is(err, errAncestorAbsentAccepted)
}

// recoveryAncestorException is the ancestorExceptionFunc LiveFileState/
// LiveDirState supply to resolveVerifiedParent: it is the only place this
// package tolerates §7.1's two "崩溃恢复" carve-outs, and only for
// recovery's own read-only state inspection, never for a primitive that is
// about to mutate anything.
func (e *Engine) recoveryAncestorException(key string, situation ancestorSituation, liveChild *os.File) (bool, error) {
	switch situation {
	case ancestorPresentButRecordedMissing:
		// §7.1: "只有当对应的mkdir...处于started状态,并且该目录为空、属主为
		// root:root时,才把它视为本事务创建的". uid/gid: see
		// allowedStarted's identical reasoning — the *current process's*
		// uid/gid, not a hard-coded 0, so this is correct both under real
		// root and under a non-root test run.
		if !hasStartedStep(e.Journal(), key, KindMkdir) {
			return false, nil
		}
		empty, err := isEmptyDirFd(liveChild)
		if err != nil {
			return false, err
		}
		if !empty {
			return false, nil
		}
		var st unix.Stat_t
		if err := unix.Fstat(int(liveChild.Fd()), &st); err != nil {
			return false, err
		}
		return int(st.Uid) == os.Getuid() && int(st.Gid) == os.Getgid(), nil
	case ancestorMissingButRecordedExists:
		// §7.1: "rmdir之后、journal更新之前崩溃...对应remove_dir处于
		// started...视为合法".
		return hasStartedStep(e.Journal(), key, KindRemoveDir), nil
	default:
		return false, nil
	}
}

// hasStartedStep reports whether j's forward Steps or RollbackSteps
// contain a step targeting target, of kind, currently Started.
func hasStartedStep(j *Journal, target string, kind StepKind) bool {
	check := func(steps []Step) bool {
		for _, s := range steps {
			if s.Target == target && s.Kind == kind && s.Status == StatusStarted {
				return true
			}
		}
		return false
	}
	return check(j.Steps) || check(j.RollbackSteps)
}

// LiveFileState reads the current on-disk state of a file target, for
// recovery's §7.3 per-path judgement (DetermineRunningState /
// DetermineRollingBackState): it re-verifies mp's ancestor chain fresh
// (exactly like every modifying primitive does immediately before its own
// syscall), opens the leaf, and — via the same computeContentField helper
// verifyPreState/openVerifiedSource use — fills whichever of SHA256/Schema
// wantHash/wantSchema (normally chainContentKind's result for this
// target's chain) selects. Neither set means the caller only cares about
// existence/type/mode/ownership, not content.
func (e *Engine) LiveFileState(mp ManagedPath, wantHash, wantSchema bool) (PathState, error) {
	parent, opened, err := resolveVerifiedParentEx(mp, e.Journal().DirIDs, nil, e.recoveryAncestorException)
	if err != nil {
		closeAll(opened)
		if isAncestorLegitimatelyAbsent(err) {
			// An ancestor directory is recorded (and confirmed) as not
			// existing — e.g. its own remove_dir rollback/cleanup step
			// already ran. Anything nested under it, this leaf included,
			// is then vacuously "does not exist" too; this is not a
			// mismatch to report, since the ancestor's own absence was
			// itself already verified against dir_ids.
			return PathState{Exists: false}, nil
		}
		return PathState{}, err
	}
	defer closeAll(opened)
	f, base, err := openLeafFile(parent, mp.Leaf)
	if err != nil {
		return PathState{}, err
	}
	if f == nil {
		return base, nil
	}
	defer f.Close()
	want := PathState{}
	switch {
	case wantHash:
		want.SHA256 = "x"
	case wantSchema:
		want.Schema = "x"
	}
	return e.computeContentField(f, mp.TargetKey(), want, base)
}

// LiveDirState reads the current on-disk state of a directory target, plus
// whether it is empty (mkdir/remove_dir's allowedStarted set depends on
// emptiness), for recovery's §7.3 per-path judgement.
func (e *Engine) LiveDirState(mp ManagedPath) (ps PathState, empty bool, err error) {
	parent, opened, err := resolveVerifiedParentEx(mp, e.Journal().DirIDs, nil, e.recoveryAncestorException)
	if err != nil {
		closeAll(opened)
		if isAncestorLegitimatelyAbsent(err) {
			return PathState{Exists: false}, false, nil
		}
		return PathState{}, false, err
	}
	defer closeAll(opened)
	ps, err = statLeafDir(parent, mp.Leaf)
	if err != nil || !ps.Exists {
		return ps, false, err
	}
	empty, err = isEmptyDir(parent, mp.Leaf)
	return ps, empty, err
}

// openManagedChildDir resolves mp's ancestor chain fresh (ancestor-verified
// against the journal's current dir_ids) and opens mp.Leaf as a directory
// under the verified parent, closing every ancestor fd it opened along the
// way before returning — used where only the leaf directory's own fd is
// needed afterward (Engine.mkdir's already-fully-done resume fast path).
func (e *Engine) openManagedChildDir(mp ManagedPath) (*os.File, error) {
	parent, opened, err := resolveVerifiedParentEx(mp, e.Journal().DirIDs, nil, e.recoveryAncestorException)
	if err != nil {
		closeAll(opened)
		return nil, err
	}
	child, err := openChildDir(parent, mp.Leaf)
	closeAll(opened)
	return child, err
}

// ScanUnregisteredTemps lists every ".proxyctl-txn-*"-formatted entry
// directly under dir that is not present in the journal's currently
// registered temp names (every step's Temps, forward and rollback
// combined) — §7.3's "未登记临时名的检查".
func ScanUnregisteredTemps(dir *os.File, j *Journal) ([]string, error) {
	registered := map[string]bool{}
	for _, s := range j.Steps {
		for _, t := range s.Temps {
			registered[t] = true
		}
	}
	for _, s := range j.RollbackSteps {
		for _, t := range s.Temps {
			registered[t] = true
		}
	}
	return scanUnregisteredTemps(dir, registered)
}
