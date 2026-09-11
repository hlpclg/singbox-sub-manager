package backup

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"time"
)

const (
	snapshotSchemaVersion = 1
	snapshotMetaName      = "snapshot.json"
	snapshotDirMode       = fs.FileMode(0700)
	snapshotFileMode      = fs.FileMode(0600)
	// SnapshotMaxAge is how long an abandoned pre-restore image is kept
	// before a later restore prunes it.
	SnapshotMaxAge = 7 * 24 * time.Hour
)

// Stable rollback failure reasons.
const (
	FailReadSnapshot = "snapshot_unreadable"
	FailWrite        = "write_failed"
	FailDelete       = "delete_failed"
)

// SnapshotEntry records what one target path looked like before a restore.
// Exists=false means the path was absent, so rolling back means deleting
// whatever the restore created there.
type SnapshotEntry struct {
	Path    string
	Exists  bool
	Mode    fs.FileMode
	UID     uint32
	GID     uint32
	SHA256  string
	Payload string
}

type snapshotEntryJSON struct {
	Path    string `json:"path"`
	Exists  bool   `json:"exists"`
	Mode    string `json:"mode"`
	UID     uint32 `json:"uid"`
	GID     uint32 `json:"gid"`
	SHA256  string `json:"sha256"`
	Payload string `json:"payload"`
}

func (e SnapshotEntry) MarshalJSON() ([]byte, error) {
	return json.Marshal(snapshotEntryJSON{
		Path:    e.Path,
		Exists:  e.Exists,
		Mode:    formatMode(e.Mode),
		UID:     e.UID,
		GID:     e.GID,
		SHA256:  e.SHA256,
		Payload: e.Payload,
	})
}

func (e *SnapshotEntry) UnmarshalJSON(data []byte) error {
	var raw snapshotEntryJSON
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	mode, err := parseMode(raw.Mode)
	if err != nil {
		return err
	}
	e.Path = raw.Path
	e.Exists = raw.Exists
	e.Mode = mode
	e.UID = raw.UID
	e.GID = raw.GID
	e.SHA256 = raw.SHA256
	e.Payload = raw.Payload
	return nil
}

// RollbackSnapshot is the byte-exact pre-restore image of every path a restore
// is about to touch. Dir is where it lives and is not serialized.
type RollbackSnapshot struct {
	SchemaVersion int             `json:"schema_version"`
	TransactionID string          `json:"transaction_id"`
	CreatedAt     time.Time       `json:"created_at"`
	Entries       []SnapshotEntry `json:"entries"`
	Dir           string          `json:"-"`
}

// CaptureOptions configures one pre-restore image.
type CaptureOptions struct {
	DestRoot     string
	SnapshotRoot string
	Paths        []string
	Now          func() time.Time
}

// RollbackOptions configures a rollback. DestRoot must match the root the
// image was captured against; Lease must already be held by the caller, which
// keeps ownership of it.
type RollbackOptions struct {
	DestRoot string
	Lease    LockLease
}

// RollbackResult reports the outcome per path.
type RollbackResult struct {
	Restored       []string
	Deleted        []string
	Failed         []string
	FailureReasons map[string]string
}

// CaptureSnapshot copies every listed path byte for byte, together with its
// mode and ownership, into a fresh transaction directory. It must complete
// before a restore writes anything, so a failed restore can always be undone.
func CaptureSnapshot(ctx context.Context, opts CaptureOptions) (*RollbackSnapshot, error) {
	if opts.DestRoot == "" {
		return nil, fmt.Errorf("backup: DestRoot is required")
	}
	if opts.SnapshotRoot == "" {
		return nil, fmt.Errorf("backup: SnapshotRoot is required")
	}
	nowFn := opts.Now
	if nowFn == nil {
		nowFn = time.Now
	}
	createdAt := nowFn().UTC()

	if err := os.MkdirAll(opts.SnapshotRoot, snapshotDirMode); err != nil {
		return nil, fmt.Errorf("backup: create snapshot root: %w", err)
	}
	if err := os.Chmod(opts.SnapshotRoot, snapshotDirMode); err != nil {
		return nil, fmt.Errorf("backup: tighten snapshot root: %w", err)
	}

	id, err := newTransactionID(createdAt)
	if err != nil {
		return nil, err
	}
	dir := filepath.Join(opts.SnapshotRoot, id)
	if err := os.Mkdir(dir, snapshotDirMode); err != nil {
		return nil, fmt.Errorf("backup: create snapshot directory: %w", err)
	}
	if err := os.Chmod(dir, snapshotDirMode); err != nil {
		_ = os.RemoveAll(dir)
		return nil, fmt.Errorf("backup: tighten snapshot directory: %w", err)
	}

	snap := &RollbackSnapshot{
		SchemaVersion: snapshotSchemaVersion,
		TransactionID: id,
		CreatedAt:     createdAt,
		Dir:           dir,
	}

	paths := append([]string(nil), opts.Paths...)
	sort.Strings(paths)
	for i, logical := range paths {
		if err := ctx.Err(); err != nil {
			_ = os.RemoveAll(dir)
			return nil, err
		}
		target, err := resolveUnderRoot(opts.DestRoot, logical)
		if err != nil {
			_ = os.RemoveAll(dir)
			return nil, err
		}
		entry := SnapshotEntry{Path: logical}

		info, err := os.Lstat(target)
		switch {
		case err != nil && os.IsNotExist(err):
			entry.Exists = false
		case err != nil:
			_ = os.RemoveAll(dir)
			return nil, fmt.Errorf("backup: stat %s: %w", logical, err)
		default:
			if !info.Mode().IsRegular() {
				_ = os.RemoveAll(dir)
				return nil, fmt.Errorf("%w: %s", ErrUnsupportedSourceType, logical)
			}
			pl, err := readPayload(target, logical)
			if err != nil {
				_ = os.RemoveAll(dir)
				return nil, err
			}
			entry.Exists = true
			entry.Mode = pl.entry.Mode
			entry.UID = pl.entry.UID
			entry.GID = pl.entry.GID
			entry.SHA256 = pl.entry.SHA256
			entry.Payload = fmt.Sprintf("payload-%04d.bin", i)
			if err := writeFileAtomic(dir, entry.Payload, pl.data, snapshotFileMode, 0, 0, false); err != nil {
				_ = os.RemoveAll(dir)
				return nil, err
			}
		}
		snap.Entries = append(snap.Entries, entry)
	}

	meta, err := json.MarshalIndent(snap, "", "  ")
	if err != nil {
		_ = os.RemoveAll(dir)
		return nil, fmt.Errorf("backup: encode snapshot metadata: %w", err)
	}
	if err := writeFileAtomic(dir, snapshotMetaName, meta, snapshotFileMode, 0, 0, false); err != nil {
		_ = os.RemoveAll(dir)
		return nil, err
	}
	if err := syncDir(dir); err != nil {
		_ = os.RemoveAll(dir)
		return nil, err
	}
	if err := syncDir(opts.SnapshotRoot); err != nil {
		_ = os.RemoveAll(dir)
		return nil, err
	}
	return snap, nil
}

// LoadSnapshot reads an image previously written by CaptureSnapshot.
func LoadSnapshot(dir string) (*RollbackSnapshot, error) {
	data, err := os.ReadFile(filepath.Join(dir, snapshotMetaName))
	if err != nil {
		return nil, fmt.Errorf("backup: read snapshot metadata: %w", err)
	}
	var snap RollbackSnapshot
	if err := json.Unmarshal(data, &snap); err != nil {
		return nil, fmt.Errorf("backup: decode snapshot metadata: %w", err)
	}
	if snap.SchemaVersion != snapshotSchemaVersion {
		return nil, fmt.Errorf("%w: snapshot %d", ErrUnsupportedSchema, snap.SchemaVersion)
	}
	for _, e := range snap.Entries {
		if err := ValidateLogicalPath(e.Path); err != nil {
			return nil, err
		}
	}
	snap.Dir = dir
	return &snap, nil
}

// Rollback puts every captured path back exactly as it was: original bytes,
// mode and ownership for files that existed, and removal for paths the restore
// created. It never acquires or releases the caller's lease.
func Rollback(ctx context.Context, snapshot *RollbackSnapshot, opts RollbackOptions) (RollbackResult, error) {
	res := RollbackResult{FailureReasons: map[string]string{}}
	if snapshot == nil {
		return res, fmt.Errorf("backup: no snapshot to roll back")
	}
	if opts.DestRoot == "" {
		return res, fmt.Errorf("backup: DestRoot is required")
	}

	var firstErr error
	fail := func(path, reason string, err error) {
		res.Failed = append(res.Failed, path)
		res.FailureReasons[path] = reason
		if firstErr == nil {
			firstErr = err
		}
	}

	for i, entry := range snapshot.Entries {
		if err := ctx.Err(); err != nil {
			reason := FailCanceled
			if err == context.DeadlineExceeded {
				reason = FailDeadlineExceeded
			}
			for _, remaining := range snapshot.Entries[i:] {
				res.Failed = append(res.Failed, remaining.Path)
				res.FailureReasons[remaining.Path] = reason
			}
			if firstErr == nil {
				firstErr = err
			}
			return res, firstErr
		}

		target, err := resolveUnderRoot(opts.DestRoot, entry.Path)
		if err != nil {
			fail(entry.Path, FailUnsafePath, err)
			continue
		}

		if !entry.Exists {
			if err := os.Remove(target); err != nil && !os.IsNotExist(err) {
				fail(entry.Path, FailDelete, fmt.Errorf("backup: remove %s: %w", entry.Path, err))
				continue
			}
			if err := syncDir(filepath.Dir(target)); err != nil {
				fail(entry.Path, FailDelete, err)
				continue
			}
			res.Deleted = append(res.Deleted, entry.Path)
			continue
		}

		data, err := os.ReadFile(filepath.Join(snapshot.Dir, entry.Payload))
		if err != nil {
			fail(entry.Path, FailReadSnapshot, fmt.Errorf("backup: read snapshot payload for %s: %w", entry.Path, err))
			continue
		}
		sum := sha256.Sum256(data)
		if hex.EncodeToString(sum[:]) != entry.SHA256 {
			fail(entry.Path, FailReadSnapshot, fmt.Errorf("%w: snapshot payload for %s", ErrChecksumMismatch, entry.Path))
			continue
		}
		if err := writeFileAtomic(opts.DestRoot, entry.Path, data, entry.Mode, entry.UID, entry.GID, true); err != nil {
			fail(entry.Path, FailWrite, err)
			continue
		}
		res.Restored = append(res.Restored, entry.Path)
	}

	return res, firstErr
}

// CleanupSnapshot removes an image. Call it only once the transaction is over:
// either the restore succeeded and was verified, or the rollback finished.
func CleanupSnapshot(ctx context.Context, snapshot *RollbackSnapshot) error {
	if snapshot == nil || snapshot.Dir == "" {
		return nil
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := os.RemoveAll(snapshot.Dir); err != nil {
		return fmt.Errorf("backup: remove snapshot %s: %w", snapshot.TransactionID, err)
	}
	return nil
}

// PruneSnapshots removes abandoned images older than maxAge. It never touches
// the active transaction and never removes the most recent image, which is the
// one an operator would still need to investigate a failed restore. Removal
// problems are returned as warnings, not errors: pruning must not fail a
// restore.
func PruneSnapshots(snapshotRoot string, now time.Time, maxAge time.Duration, activeID string) []string {
	entries, err := os.ReadDir(snapshotRoot)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return []string{fmt.Sprintf("cannot list snapshots in %s: %v", snapshotRoot, err)}
	}

	type candidate struct {
		name string
		age  time.Time
	}
	var candidates []candidate
	var warnings []string
	for _, e := range entries {
		if !e.IsDir() || e.Name() == activeID {
			continue
		}
		dir := filepath.Join(snapshotRoot, e.Name())
		snap, err := LoadSnapshot(dir)
		if err != nil {
			// Keep anything this package cannot recognise: it is either a
			// damaged image worth investigating or not ours to delete.
			warnings = append(warnings, fmt.Sprintf("keeping unreadable snapshot %s: %v", e.Name(), err))
			continue
		}
		candidates = append(candidates, candidate{name: e.Name(), age: snap.CreatedAt})
	}
	if len(candidates) <= 1 {
		return warnings
	}
	sort.Slice(candidates, func(i, j int) bool { return candidates[i].age.After(candidates[j].age) })

	// candidates[0] is the newest and is always kept for investigation.
	for _, c := range candidates[1:] {
		if now.Sub(c.age) <= maxAge {
			continue
		}
		if err := os.RemoveAll(filepath.Join(snapshotRoot, c.name)); err != nil {
			warnings = append(warnings, fmt.Sprintf("cannot prune stale snapshot %s: %v", c.name, err))
		}
	}
	return warnings
}

func newTransactionID(createdAt time.Time) (string, error) {
	var buf [6]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return "", fmt.Errorf("backup: generate transaction id: %w", err)
	}
	return fmt.Sprintf("%s-%s", createdAt.Format(archiveTimeLayout), hex.EncodeToString(buf[:])), nil
}
