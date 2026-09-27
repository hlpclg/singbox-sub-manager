package transaction

// This package's tests never use t.Parallel: stepHook, ancestorHook and
// persistHook are process-global test seams, following the same
// convention internal/backup's dirfd_test.go documents for its own hooks.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func newTestRoot(t *testing.T, sub string) (*TrustedRoot, string) {
	t.Helper()
	dir := filepath.Join(t.TempDir(), sub)
	if err := os.Mkdir(dir, 0700); err != nil {
		t.Fatalf("mkdir %s: %v", dir, err)
	}
	root, err := OpenTrustedRoot(dir, os.Getuid())
	if err != nil {
		t.Fatalf("OpenTrustedRoot(%s): %v", dir, err)
	}
	t.Cleanup(func() { root.Close() })
	return root, dir
}

func baseJournal(txnID string) *Journal {
	return &Journal{
		Schema:               CurrentSchema,
		TxnID:                txnID,
		Op:                   "install",
		ProxyctlVersion:      "v0.10.0",
		CreatedAt:            time.Date(2026, 9, 27, 0, 0, 0, 0, time.UTC),
		Phase:                PhaseRunning,
		RollbackFilesDone:    false,
		SingboxVersionBefore: "",
		SingboxVersionAfter:  "1.14.1",
		UnitStateBefore:      UnitState{LoadState: "not-found"},
		DirIDs:               map[string]DirIdentity{},
		RollbackSystemd:      RollbackSystemdNone,
		Steps:                nil,
	}
}

func readFile(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return data
}

func exists(path string) bool {
	_, err := os.Lstat(path)
	return err == nil
}

// --- ParseJournal / Validate ---

func TestParseJournal_UnknownSchema(t *testing.T) {
	j := baseJournal("t1")
	j.Schema = 2
	data := marshalRaw(t, j)
	_, err := ParseJournal(data)
	if !errors.Is(err, ErrUnknownSchema) {
		t.Fatalf("ParseJournal error = %v, want ErrUnknownSchema", err)
	}
}

func marshalRaw(t *testing.T, j *Journal) []byte {
	t.Helper()
	data, err := json.Marshal(j)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return data
}

func sha256Hex(t *testing.T, data []byte) string {
	if t != nil {
		t.Helper()
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func TestParseJournal_MalformedJSON(t *testing.T) {
	_, err := ParseJournal([]byte("{not json"))
	if !errors.Is(err, ErrInvalidJournal) {
		t.Fatalf("ParseJournal error = %v, want ErrInvalidJournal", err)
	}
}

func TestParseJournal_UnknownEnumValues(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(j *Journal)
	}{
		{"phase", func(j *Journal) { j.Phase = "flying" }},
		{"rollback_systemd", func(j *Journal) { j.RollbackSystemd = "sideways" }},
		{"step status", func(j *Journal) {
			j.Steps = []Step{{ID: "s1", Kind: KindMkdir, Target: "/a", Status: "confused", Pre: PathState{}, Post: PathState{Exists: true, Type: PathTypeDir}}}
		}},
		{"step kind", func(j *Journal) {
			j.Steps = []Step{{ID: "s1", Kind: "teleport", Target: "/a", Status: StatusPending, Pre: PathState{}, Post: PathState{Exists: true, Type: PathTypeDir}}}
		}},
		{"rollback step systemd kind", func(j *Journal) {
			j.RollbackSteps = []Step{{ID: "r1", Kind: KindSystemdEnable, Target: "/svc", Status: StatusPending}}
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			j := baseJournal("t1")
			c.mutate(j)
			data := marshalRaw(t, j)
			_, err := ParseJournal(data)
			if !errors.Is(err, ErrInvalidJournal) {
				t.Fatalf("ParseJournal error = %v, want ErrInvalidJournal", err)
			}
		})
	}
}

func mkStep(id string, kind StepKind, target string, status StepStatus, pre, post PathState) Step {
	return Step{ID: id, Kind: kind, Target: target, Status: status, Pre: pre, Post: post}
}

func TestProgressPrefix(t *testing.T) {
	fileA := PathState{Exists: false}
	fileB := PathState{Exists: true, Type: PathTypeFile, Mode: 0600, SHA256: "aaaa"}
	fileC := PathState{Exists: true, Type: PathTypeFile, Mode: 0600, SHA256: "bbbb"}

	t.Run("legal forward prefix: done done started pending", func(t *testing.T) {
		j := baseJournal("t1")
		j.Steps = []Step{
			mkStep("s1", KindWriteFile, "/a", StatusDone, fileA, fileB),
			mkStep("s2", KindWriteFile, "/b", StatusDone, fileA, fileB),
			mkStep("s3", KindWriteFile, "/c", StatusStarted, fileA, fileB),
			mkStep("s4", KindWriteFile, "/d", StatusPending, fileA, fileB),
		}
		if err := j.Validate(); err != nil {
			t.Fatalf("Validate: %v", err)
		}
	})

	t.Run("legal: all pending", func(t *testing.T) {
		j := baseJournal("t1")
		j.Steps = []Step{
			mkStep("s1", KindWriteFile, "/a", StatusPending, fileA, fileB),
			mkStep("s2", KindWriteFile, "/b", StatusPending, fileA, fileB),
		}
		if err := j.Validate(); err != nil {
			t.Fatalf("Validate: %v", err)
		}
	})

	t.Run("legal: all done", func(t *testing.T) {
		j := baseJournal("t1")
		j.Steps = []Step{
			mkStep("s1", KindWriteFile, "/a", StatusDone, fileA, fileB),
			mkStep("s2", KindWriteFile, "/b", StatusDone, fileA, fileB),
		}
		if err := j.Validate(); err != nil {
			t.Fatalf("Validate: %v", err)
		}
	})

	t.Run("illegal: pending then done", func(t *testing.T) {
		j := baseJournal("t1")
		j.Steps = []Step{
			mkStep("s1", KindWriteFile, "/a", StatusPending, fileA, fileB),
			mkStep("s2", KindWriteFile, "/b", StatusDone, fileA, fileB),
		}
		if err := j.Validate(); !errors.Is(err, ErrIllegalProgress) {
			t.Fatalf("Validate error = %v, want ErrIllegalProgress", err)
		}
	})

	t.Run("illegal: pending then started", func(t *testing.T) {
		j := baseJournal("t1")
		j.Steps = []Step{
			mkStep("s1", KindWriteFile, "/a", StatusPending, fileA, fileB),
			mkStep("s2", KindWriteFile, "/b", StatusStarted, fileA, fileB),
		}
		if err := j.Validate(); !errors.Is(err, ErrIllegalProgress) {
			t.Fatalf("Validate error = %v, want ErrIllegalProgress", err)
		}
	})

	t.Run("illegal: started then done", func(t *testing.T) {
		j := baseJournal("t1")
		j.Steps = []Step{
			mkStep("s1", KindWriteFile, "/a", StatusStarted, fileA, fileB),
			mkStep("s2", KindWriteFile, "/b", StatusDone, fileA, fileB),
		}
		if err := j.Validate(); !errors.Is(err, ErrIllegalProgress) {
			t.Fatalf("Validate error = %v, want ErrIllegalProgress", err)
		}
	})

	t.Run("illegal: two started", func(t *testing.T) {
		j := baseJournal("t1")
		j.Steps = []Step{
			mkStep("s1", KindWriteFile, "/a", StatusStarted, fileA, fileB),
			mkStep("s2", KindWriteFile, "/b", StatusStarted, fileA, fileB),
		}
		if err := j.Validate(); !errors.Is(err, ErrIllegalProgress) {
			t.Fatalf("Validate error = %v, want ErrIllegalProgress", err)
		}
	})

	t.Run("legal rollback prefix, same rules as forward", func(t *testing.T) {
		j := baseJournal("t1")
		j.Phase = PhaseRollingBack
		j.RollbackFrontier = []StepStatus{StatusDone}
		j.Steps = []Step{mkStep("s1", KindWriteFile, "/a", StatusDone, fileA, fileB)}
		j.RollbackSteps = []Step{
			mkStep("r1", KindWriteFile, "/a", StatusStarted, fileB, fileA),
		}
		if err := j.Validate(); err != nil {
			t.Fatalf("Validate: %v", err)
		}
	})

	t.Run("illegal rollback prefix", func(t *testing.T) {
		j := baseJournal("t1")
		j.Phase = PhaseRollingBack
		j.RollbackFrontier = []StepStatus{StatusDone}
		j.Steps = []Step{mkStep("s1", KindWriteFile, "/a", StatusDone, fileA, fileB)}
		j.RollbackSteps = []Step{
			mkStep("r1", KindWriteFile, "/a", StatusDone, fileB, fileA),
			mkStep("r2", KindWriteFile, "/b", StatusStarted, fileA, fileB),
			mkStep("r3", KindWriteFile, "/c", StatusDone, fileA, fileB),
		}
		if err := j.Validate(); !errors.Is(err, ErrIllegalProgress) {
			t.Fatalf("Validate error = %v, want ErrIllegalProgress", err)
		}
	})

	t.Run("illegal frontier length", func(t *testing.T) {
		j := baseJournal("t1")
		j.Phase = PhaseRollingBack
		j.Steps = []Step{mkStep("s1", KindWriteFile, "/a", StatusDone, fileA, fileB)}
		j.RollbackFrontier = []StepStatus{StatusDone, StatusDone}
		if err := j.Validate(); !errors.Is(err, ErrIllegalProgress) {
			t.Fatalf("Validate error = %v, want ErrIllegalProgress", err)
		}
	})

	t.Run("illegal frontier sequence", func(t *testing.T) {
		j := baseJournal("t1")
		j.Phase = PhaseRollingBack
		j.Steps = []Step{
			mkStep("s1", KindWriteFile, "/a", StatusDone, fileA, fileB),
			mkStep("s2", KindWriteFile, "/b", StatusDone, fileA, fileB),
		}
		j.RollbackFrontier = []StepStatus{StatusPending, StatusDone}
		if err := j.Validate(); !errors.Is(err, ErrIllegalProgress) {
			t.Fatalf("Validate error = %v, want ErrIllegalProgress", err)
		}
	})

	t.Run("illegal: rollback_systemd restoring without rollback_files_done", func(t *testing.T) {
		j := baseJournal("t1")
		j.Phase = PhaseRollingBack
		j.RollbackSystemd = RollbackSystemdRestoring
		j.RollbackFilesDone = false
		if err := j.Validate(); !errors.Is(err, ErrIllegalProgress) {
			t.Fatalf("Validate error = %v, want ErrIllegalProgress", err)
		}
	})

	t.Run("illegal: rollback_files_done true but a rollback step not done", func(t *testing.T) {
		j := baseJournal("t1")
		j.Phase = PhaseRollingBack
		j.RollbackFilesDone = true
		j.RollbackSteps = []Step{mkStep("r1", KindWriteFile, "/a", StatusStarted, fileB, fileA)}
		if err := j.Validate(); !errors.Is(err, ErrIllegalProgress) {
			t.Fatalf("Validate error = %v, want ErrIllegalProgress", err)
		}
	})

	t.Run("legal: rollback_systemd restored with rollback_files_done and all rollback steps done", func(t *testing.T) {
		j := baseJournal("t1")
		j.Phase = PhaseRollingBack
		j.Steps = []Step{mkStep("s1", KindWriteFile, "/a", StatusDone, fileA, fileB)}
		j.RollbackFrontier = []StepStatus{StatusDone}
		j.RollbackSystemd = RollbackSystemdRestored
		j.RollbackFilesDone = true
		j.RollbackSteps = []Step{mkStep("r1", KindWriteFile, "/a", StatusDone, fileB, fileA)}
		if err := j.Validate(); err != nil {
			t.Fatalf("Validate: %v", err)
		}
	})

	t.Run("illegal path chain: pre does not match previous post", func(t *testing.T) {
		j := baseJournal("t1")
		j.Steps = []Step{
			mkStep("s1", KindWriteFile, "/a", StatusDone, fileA, fileB),
			mkStep("s2", KindWriteFile, "/a", StatusStarted, fileC, fileB), // pre should be fileB
		}
		if err := j.Validate(); !errors.Is(err, ErrIllegalProgress) {
			t.Fatalf("Validate error = %v, want ErrIllegalProgress", err)
		}
	})

	t.Run("legal path chain: pre matches previous post", func(t *testing.T) {
		j := baseJournal("t1")
		j.Steps = []Step{
			mkStep("s1", KindWriteFile, "/a", StatusDone, fileA, fileB),
			mkStep("s2", KindWriteFile, "/a", StatusStarted, fileB, fileC),
		}
		if err := j.Validate(); err != nil {
			t.Fatalf("Validate: %v", err)
		}
	})

	t.Run("unknown status rejected even embedded in raw JSON, not just via the Go enum type", func(t *testing.T) {
		// Hand-built JSON, bypassing the Go StepStatus type entirely, to
		// prove ParseJournal's own decoding rejects an unknown status
		// rather than relying on the type system a caller could construct
		// a Journal value around directly.
		raw := `{
			"schema": 1, "txn_id": "t1", "op": "install", "proxyctl_version": "v0.10.0",
			"created_at": "2026-09-27T00:00:00Z", "phase": "running",
			"rollback_files_done": false, "unit_state_before": {"load_state":"not-found","unit_file_state":"","active_state":""},
			"dir_ids": {}, "rollback_systemd": "none",
			"steps": [{"id":"s1","kind":"write_file","target":"/a","status":"almost_done","pre":{"exists":false},"post":{"exists":true,"type":"file"}}]
		}`
		if _, err := ParseJournal([]byte(raw)); !errors.Is(err, ErrInvalidJournal) {
			t.Fatalf("ParseJournal error = %v, want ErrInvalidJournal", err)
		}
	})

	// review P1#12 (phase consistency): a journal claiming phase=running
	// must carry no rollback state at all; phase=rolling_back must at
	// least have a rollback_frontier.
	t.Run("illegal: phase=running carries rollback_frontier", func(t *testing.T) {
		j := baseJournal("t1")
		j.RollbackFrontier = []StepStatus{StatusDone}
		if err := j.Validate(); !errors.Is(err, ErrIllegalProgress) {
			t.Fatalf("Validate error = %v, want ErrIllegalProgress", err)
		}
	})

	t.Run("illegal: phase=running carries rollback_files_done=true", func(t *testing.T) {
		j := baseJournal("t1")
		j.RollbackFilesDone = true
		if err := j.Validate(); !errors.Is(err, ErrIllegalProgress) {
			t.Fatalf("Validate error = %v, want ErrIllegalProgress", err)
		}
	})

	t.Run("illegal: phase=running carries rollback_systemd != none", func(t *testing.T) {
		j := baseJournal("t1")
		j.RollbackSystemd = RollbackSystemdDemoting
		if err := j.Validate(); !errors.Is(err, ErrIllegalProgress) {
			t.Fatalf("Validate error = %v, want ErrIllegalProgress", err)
		}
	})

	t.Run("illegal: phase=running carries rollback_steps", func(t *testing.T) {
		j := baseJournal("t1")
		j.RollbackSteps = []Step{mkStep("r1", KindWriteFile, "/a", StatusDone, fileB, fileA)}
		if err := j.Validate(); !errors.Is(err, ErrIllegalProgress) {
			t.Fatalf("Validate error = %v, want ErrIllegalProgress", err)
		}
	})

	t.Run("illegal: phase=rolling_back with no rollback_frontier at all", func(t *testing.T) {
		j := baseJournal("t1")
		j.Phase = PhaseRollingBack
		if err := j.Validate(); !errors.Is(err, ErrIllegalProgress) {
			t.Fatalf("Validate error = %v, want ErrIllegalProgress", err)
		}
	})

	// review P2#12 (temp name format and uniqueness).
	t.Run("illegal: temp name does not match the .proxyctl-txn-<txn_id>-<seq> format", func(t *testing.T) {
		j := baseJournal("t1")
		j.Steps = []Step{mkStep("s1", KindWriteFile, "/etc/proxyctl-reality/config.json", StatusPending, fileA, fileB)}
		j.Steps[0].Temps = []string{"config.json"}
		if err := j.Validate(); !errors.Is(err, ErrInvalidJournal) {
			t.Fatalf("Validate error = %v, want ErrInvalidJournal (a real filename must never validate as a temp name)", err)
		}
	})

	t.Run("illegal: temp name carries a different txn_id", func(t *testing.T) {
		j := baseJournal("t1")
		j.Steps = []Step{mkStep("s1", KindWriteFile, "/a", StatusPending, fileA, fileB)}
		j.Steps[0].Temps = []string{".proxyctl-txn-other-txn-1"}
		if err := j.Validate(); !errors.Is(err, ErrInvalidJournal) {
			t.Fatalf("Validate error = %v, want ErrInvalidJournal", err)
		}
	})

	t.Run("illegal: temp name has trailing non-digit characters", func(t *testing.T) {
		j := baseJournal("t1")
		j.Steps = []Step{mkStep("s1", KindWriteFile, "/a", StatusPending, fileA, fileB)}
		j.Steps[0].Temps = []string{".proxyctl-txn-t1-1x"}
		if err := j.Validate(); !errors.Is(err, ErrInvalidJournal) {
			t.Fatalf("Validate error = %v, want ErrInvalidJournal", err)
		}
	})

	t.Run("illegal: the same temp name registered by two different steps", func(t *testing.T) {
		j := baseJournal("t1")
		j.Steps = []Step{
			mkStep("s1", KindWriteFile, "/a", StatusDone, fileA, fileB),
			mkStep("s2", KindWriteFile, "/b", StatusPending, fileA, fileB),
		}
		j.Steps[0].Temps = []string{".proxyctl-txn-t1-1"}
		j.Steps[1].Temps = []string{".proxyctl-txn-t1-1"}
		if err := j.Validate(); !errors.Is(err, ErrInvalidJournal) {
			t.Fatalf("Validate error = %v, want ErrInvalidJournal", err)
		}
	})

	t.Run("legal: distinct, well-formed temp names across forward and rollback steps", func(t *testing.T) {
		j := baseJournal("t1")
		j.Steps = []Step{mkStep("s1", KindWriteFile, "/a", StatusPending, fileA, fileB)}
		j.Steps[0].Temps = []string{".proxyctl-txn-t1-1"}
		j.Phase = PhaseRollingBack
		j.RollbackFrontier = []StepStatus{StatusPending}
		j.RollbackSteps = []Step{mkStep("r1", KindWriteFile, "/a", StatusPending, fileA, fileA)}
		j.RollbackSteps[0].Temps = []string{".proxyctl-txn-t1-2"}
		if err := j.Validate(); err != nil {
			t.Fatalf("Validate: %v", err)
		}
	})

	// review round 2, item 1: a PathState must identify content exactly
	// one way.
	t.Run("illegal: pre has both sha256 and schema set", func(t *testing.T) {
		j := baseJournal("t1")
		badPre := PathState{Exists: true, Type: PathTypeFile, Mode: 0600, SHA256: "aaaa", Schema: "instance-v1"}
		j.Steps = []Step{mkStep("s1", KindWriteFile, "/a", StatusPending, badPre, fileB)}
		if err := j.Validate(); !errors.Is(err, ErrInvalidJournal) {
			t.Fatalf("Validate error = %v, want ErrInvalidJournal", err)
		}
	})

	t.Run("illegal: post has both sha256 and schema set", func(t *testing.T) {
		j := baseJournal("t1")
		badPost := PathState{Exists: true, Type: PathTypeFile, Mode: 0600, SHA256: "aaaa", Schema: "instance-v1"}
		j.Steps = []Step{mkStep("s1", KindWriteFile, "/a", StatusPending, fileA, badPost)}
		if err := j.Validate(); !errors.Is(err, ErrInvalidJournal) {
			t.Fatalf("Validate error = %v, want ErrInvalidJournal", err)
		}
	})

	t.Run("legal: schema-only pre/post (dynamic-state file), no sha256 at all", func(t *testing.T) {
		j := baseJournal("t1")
		schemaState := PathState{Exists: true, Type: PathTypeFile, Mode: 0600, Schema: "instance-v1"}
		j.Steps = []Step{mkStep("s1", KindWriteFile, "/a", StatusPending, schemaState, schemaState)}
		if err := j.Validate(); err != nil {
			t.Fatalf("Validate: %v", err)
		}
	})
}

// --- Store durability ---

func TestJournalDurability(t *testing.T) {
	t.Run("interrupted before any journal write leaves only the transaction area", func(t *testing.T) {
		root, rootPath := newTestRoot(t, "root")
		old := persistHook
		persistHook = func(point string) error {
			if point == "before_create" {
				return errFakeCrash
			}
			return nil
		}
		defer func() { persistHook = old }()

		j := baseJournal("t1")
		_, err := BeginTransaction(root, "txn", j)
		if !errors.Is(err, errFakeCrash) {
			t.Fatalf("BeginTransaction error = %v, want errFakeCrash", err)
		}
		txnDir := filepath.Join(rootPath, "txn")
		if !exists(txnDir) {
			t.Fatalf("transaction area %s does not exist", txnDir)
		}
		if exists(filepath.Join(txnDir, journalFileName)) || exists(filepath.Join(txnDir, journalTmpName)) {
			t.Fatalf("no journal file should exist yet")
		}
	})

	t.Run("interrupted after tmp fsync leaves only journal.json.tmp, parseable", func(t *testing.T) {
		root, rootPath := newTestRoot(t, "root")
		old := persistHook
		persistHook = func(point string) error {
			if point == "after_tmp_fsync" {
				return errFakeCrash
			}
			return nil
		}
		defer func() { persistHook = old }()

		j := baseJournal("t1")
		_, err := BeginTransaction(root, "txn", j)
		if !errors.Is(err, errFakeCrash) {
			t.Fatalf("BeginTransaction error = %v, want errFakeCrash", err)
		}
		txnDir := filepath.Join(rootPath, "txn")
		if exists(filepath.Join(txnDir, journalFileName)) {
			t.Fatalf("journal.json must not exist yet")
		}
		tmpPath := filepath.Join(txnDir, journalTmpName)
		if !exists(tmpPath) {
			t.Fatalf("journal.json.tmp must exist")
		}
		parsed, err := ParseJournal(readFile(t, tmpPath))
		if err != nil {
			t.Fatalf("journal.json.tmp must itself already be a complete, parseable journal: %v", err)
		}
		if parsed.TxnID != "t1" {
			t.Fatalf("parsed TxnID = %q, want t1", parsed.TxnID)
		}
	})

	t.Run("update interrupted after tmp fsync leaves journal.json (previous version) and journal.json.tmp (next) side by side", func(t *testing.T) {
		root, rootPath := newTestRoot(t, "root")
		j := baseJournal("t1")
		store, err := BeginTransaction(root, "txn", j)
		if err != nil {
			t.Fatalf("BeginTransaction: %v", err)
		}
		defer store.Close()

		next := store.Journal().Clone()
		next.SingboxVersionAfter = "1.14.2"
		old := persistHook
		persistHook = func(point string) error {
			if point == "after_tmp_fsync" {
				return errFakeCrash
			}
			return nil
		}
		if err := store.Update(next); !errors.Is(err, errFakeCrash) {
			persistHook = old
			t.Fatalf("Update error = %v, want errFakeCrash", err)
		}
		persistHook = old

		txnDir := filepath.Join(rootPath, "txn")
		finalPath := filepath.Join(txnDir, journalFileName)
		if !exists(finalPath) {
			t.Fatalf("journal.json (previous version) must still exist")
		}
		stillPrevious, err := ParseJournal(readFile(t, finalPath))
		if err != nil {
			t.Fatalf("ParseJournal(journal.json): %v", err)
		}
		if stillPrevious.SingboxVersionAfter != "1.14.1" {
			t.Fatalf("journal.json was overwritten before rename; got singbox_version_after=%q", stillPrevious.SingboxVersionAfter)
		}
		tmpPath := filepath.Join(txnDir, journalTmpName)
		if !exists(tmpPath) {
			t.Fatalf("journal.json.tmp (the interrupted next version) must exist")
		}
		nextOnDisk, err := ParseJournal(readFile(t, tmpPath))
		if err != nil {
			t.Fatalf("ParseJournal(journal.json.tmp): %v", err)
		}
		if nextOnDisk.SingboxVersionAfter != "1.14.2" {
			t.Fatalf("journal.json.tmp singbox_version_after = %q, want 1.14.2", nextOnDisk.SingboxVersionAfter)
		}
	})

	t.Run("register, started, primitive, done round trip is fully durable", func(t *testing.T) {
		root, rootPath := newTestRoot(t, "root")
		managedRoot, managedPath := newTestRoot(t, "managed")
		_ = managedPath
		uid, gid := uint32(os.Getuid()), uint32(os.Getgid())

		j := baseJournal("t1")
		j.Steps = []Step{
			{
				ID:     "s1",
				Kind:   KindWriteFile,
				Target: "managed/config",
				Pre:    PathState{Exists: false},
				Post:   PathState{Exists: true, Type: PathTypeFile, Size: 5, Mode: 0600, UID: uid, GID: gid, SHA256: sha256Hex(t, []byte("hello"))},
				Status: StatusPending,
				Temps:  []string{".proxyctl-txn-t1-1"},
			},
		}
		store, err := BeginTransaction(root, "txn", j)
		if err != nil {
			t.Fatalf("BeginTransaction: %v", err)
		}
		defer store.Close()

		engine := NewEngine(store)
		mp := ManagedPath{Root: managedRoot, RootKey: "managed", Leaf: "config"}
		if err := engine.WriteFile(context.Background(), "s1", mp, ".proxyctl-txn-t1-1", []byte("hello")); err != nil {
			t.Fatalf("WriteFile: %v", err)
		}

		finalPath := filepath.Join(rootPath, "txn", journalFileName)
		final, err := ParseJournal(readFile(t, finalPath))
		if err != nil {
			t.Fatalf("ParseJournal: %v", err)
		}
		if final.Steps[0].Status != StatusDone {
			t.Fatalf("step status = %s, want done", final.Steps[0].Status)
		}
		data, err := os.ReadFile(filepath.Join(managedPath, "config"))
		if err != nil {
			t.Fatalf("read managed file: %v", err)
		}
		if string(data) != "hello" {
			t.Fatalf("managed file content = %q, want hello", data)
		}
	})
}

var errFakeCrash = errors.New("fake crash for testing")

// TestSchema1Fixture reads the frozen testdata/schema1/journal.json
// fixture (plan Task 4: "冻结的合法 schema=1 journal 夹具") and proves it
// still parses and validates as a legal schema=1 journal — a future
// version's compatible-read test pins against this same file, so its
// content must never be "fixed" here to make some other change pass. It
// covers a done mkdir, a started write_file (registered temp name, in the
// allowed §7.2 set relative to its own pre/post), and a pending write_file
// on a distinct target with a dynamic-state Schema name instead of a
// SHA256 — exactly one legal instance of each shape this package's schema
// defines.
func TestSchema1Fixture(t *testing.T) {
	data := readFile(t, "testdata/schema1/journal.json")
	j, err := ParseJournal(data)
	if err != nil {
		t.Fatalf("ParseJournal(testdata/schema1/journal.json): %v", err)
	}
	if j.TxnID != "fixture-txn-0001" {
		t.Fatalf("TxnID = %q, want fixture-txn-0001", j.TxnID)
	}
	if j.Phase != PhaseRunning {
		t.Fatalf("Phase = %q, want running", j.Phase)
	}
	if len(j.Steps) != 5 {
		t.Fatalf("len(Steps) = %d, want 5", len(j.Steps))
	}
	byID := map[string]Step{}
	for _, s := range j.Steps {
		byID[s.ID] = s
	}

	if s := byID["s1"]; s.Status != StatusDone {
		t.Fatalf("s1 status = %s, want done", s.Status)
	}
	if s := byID["s2"]; s.Status != StatusStarted {
		t.Fatalf("s2 status = %s, want started", s.Status)
	}
	if s := byID["s3"]; s.Status != StatusPending {
		t.Fatalf("s3 status = %s, want pending", s.Status)
	}
	if s := byID["s3"]; s.Post.Schema != "manifest-v1" {
		t.Fatalf("s3.Post.Schema = %q, want manifest-v1 (dynamic-state schema name, not a hash)", s.Post.Schema)
	}

	// review round 2, item 3: a systemd_enable step with unit_file_state
	// readable on both Pre and Post.
	s4 := byID["s4"]
	if s4.Kind != KindSystemdEnable {
		t.Fatalf("s4.Kind = %q, want systemd_enable", s4.Kind)
	}
	if s4.Status != StatusDone {
		t.Fatalf("s4 status = %s, want done", s4.Status)
	}
	if s4.Pre.UnitFileState != "disabled" || s4.Post.UnitFileState != "enabled" {
		t.Fatalf("s4 unit_file_state = %q -> %q, want disabled -> enabled", s4.Pre.UnitFileState, s4.Post.UnitFileState)
	}

	// review round 2, item 3: a write_file step whose Pre (not just Post)
	// is a schema-type dynamic-state file, progress-legal as pending.
	s5 := byID["s5"]
	if s5.Kind != KindWriteFile {
		t.Fatalf("s5.Kind = %q, want write_file", s5.Kind)
	}
	if s5.Status != StatusPending {
		t.Fatalf("s5 status = %s, want pending", s5.Status)
	}
	if s5.Pre.Schema != "instance-v1" || s5.Post.Schema != "instance-v1" {
		t.Fatalf("s5 schema = %q -> %q, want instance-v1 -> instance-v1", s5.Pre.Schema, s5.Post.Schema)
	}
	if s5.Pre.SHA256 != "" || s5.Post.SHA256 != "" {
		t.Fatalf("s5 sha256 = %q / %q, want both empty (schema-identified, not hash-identified)", s5.Pre.SHA256, s5.Post.SHA256)
	}

	dirID, ok := j.DirIDs["etc/proxyctl-reality"]
	if !ok || !dirID.Exists || dirID.Ino != 123456 {
		t.Fatalf("DirIDs[etc/proxyctl-reality] = %+v, ok=%v, want Exists=true Ino=123456", dirID, ok)
	}
}
