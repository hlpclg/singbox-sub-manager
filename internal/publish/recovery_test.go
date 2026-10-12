package publish

// recovery_test.go: plan Task 9's crash/recovery evidence for publish (design
// spec §7.4-§7.6, §10.4, §14.2 "publish" and "同一路径多个步骤"). Every
// lifecycle point Run reports through publishHook becomes a real OS process
// (this test binary re-exec'd as TestPublishCrashChild) that os.Exit(3)s at
// exactly that point; the parent then recovers through the production entry
// point RunRecover and compares bytes, mode, owner and existence with the
// pre-transaction tree. Primitive-internal crash points (mid-rename, mid-fsync)
// belong to internal/transaction's own matrix (Task 4/5); intermediate
// journal states a lifecycle hook cannot reach are fabricated here by editing
// the journal of a really-crashed transaction.

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/hlpclg/singbox-sub-manager/internal/nodes"
	"github.com/hlpclg/singbox-sub-manager/internal/transaction"
)

const (
	envCrashCase  = "PUBLISH_CRASH_CASE"
	envCrashBase  = "PUBLISH_CRASH_BASE"
	envCrashPoint = "PUBLISH_CRASH_POINT"
	crashExit     = 3
	doneExit      = 7
)

type crashScenario struct {
	name   string
	prep   func(f *fixture)
	params Params
	// finalEnabled computes the nodes expected to be published when the
	// operation commits.
	finalEnabled func(f *fixture) []nodes.Node
}

func crashScenarios() []crashScenario {
	return []crashScenario{
		{
			name:   "add_node",
			params: Params{Mutation: Mutation{Kind: KindAdd, Node: hy2Node("X")}},
			finalEnabled: func(f *fixture) []nodes.Node {
				return []nodes.Node{hy2Node("A"), hy2Node("B"), hy2Node("X")}
			},
		},
		{
			name:         "republish_only",
			params:       Params{Mutation: Mutation{Kind: KindPublish}},
			finalEnabled: func(f *fixture) []nodes.Node { return []nodes.Node{hy2Node("A"), hy2Node("B")} },
		},
		{
			name: "subscription_files_absent",
			prep: func(f *fixture) {
				os.Remove(f.clashPath())
				os.Remove(f.srPath())
			},
			params:       Params{Mutation: Mutation{Kind: KindDisable, Name: "A"}},
			finalEnabled: func(f *fixture) []nodes.Node { return []nodes.Node{hy2Node("B")} },
		},
		{
			name: "only_sr_absent",
			prep: func(f *fixture) { os.Remove(f.srPath()) },
			params: Params{Mutation: Mutation{Kind: KindEdit, Name: "B",
				Edit: Edit{Port: func() *int { p := 9443; return &p }()}}},
			finalEnabled: func(f *fixture) []nodes.Node {
				b := hy2Node("B")
				b.Port = 9443
				return []nodes.Node{hy2Node("A"), b}
			},
		},
	}
}

func scenarioByName(name string) crashScenario {
	for _, s := range crashScenarios() {
		if s.name == name {
			return s
		}
	}
	panic("unknown scenario " + name)
}

// TestPublishCrashChild is the re-exec'd crashing side; a no-op in a normal run.
func TestPublishCrashChild(t *testing.T) {
	name := os.Getenv(envCrashCase)
	if name == "" {
		return
	}
	sc := scenarioByName(name)
	f := newFixtureAt(t, os.Getenv(envCrashBase), false)
	point := os.Getenv(envCrashPoint)
	setHook(func(p string) error {
		if p == point {
			os.Exit(crashExit)
		}
		return nil
	})
	if strings.HasPrefix(point, "recover:") {
		res := RunRecover(context.Background(), f.env, RecoverParams{})
		fmt.Fprintf(os.Stderr, "crash child finished recover without reaching %q: %+v\n", point, res)
		os.Exit(doneExit)
	}
	res := Run(context.Background(), f.env, sc.params)
	fmt.Fprintf(os.Stderr, "crash child finished without reaching %q: exit=%d err=%v cause=%v\n", point, res.ExitCode(), res.Err, res.Cause)
	os.Exit(doneExit)
}

func discoverPoints(t *testing.T, sc crashScenario) []string {
	t.Helper()
	f := newFixture(t)
	if sc.prep != nil {
		sc.prep(f)
	}
	var points []string
	seen := map[string]bool{}
	setHook(func(p string) error {
		if !seen[p] {
			seen[p] = true
			points = append(points, p)
		}
		return nil
	})
	res := Run(context.Background(), f.env, sc.params)
	setHook(func(string) error { return nil })
	if res.ExitCode() != 0 {
		t.Fatalf("discovery run failed: exit=%d err=%v", res.ExitCode(), res.Err)
	}
	return points
}

// crashAt runs the scenario in a child process that dies at point and returns
// the (still crashed) fixture.
func crashAt(t *testing.T, sc crashScenario, point string) *fixture {
	t.Helper()
	base := t.TempDir()
	f := newFixtureAt(t, base, true)
	if sc.prep != nil {
		sc.prep(f)
	}
	f.pre = treeState(t, base, false)
	cmd := exec.Command(os.Args[0], "-test.run=^TestPublishCrashChild$", "-test.timeout=2m")
	cmd.Env = append(os.Environ(), envCrashCase+"="+sc.name, envCrashBase+"="+base, envCrashPoint+"="+point)
	out, err := cmd.CombinedOutput()
	code := 0
	if ee, ok := err.(*exec.ExitError); ok {
		code = ee.ExitCode()
	} else if err != nil {
		t.Fatalf("child: %v", err)
	}
	if code != crashExit {
		t.Fatalf("child exit = %d, want %d (crash at %q)\n%s", code, crashExit, point, out)
	}
	return f
}

func openStore(t *testing.T, f *fixture) (*transaction.Store, func()) {
	t.Helper()
	root, err := transaction.OpenTrustedRoot(f.stateDir, os.Getuid())
	if err != nil {
		t.Fatal(err)
	}
	store, err := transaction.OpenTransaction(root, TxnDirName)
	if err != nil {
		root.Close()
		t.Fatal(err)
	}
	return store, func() { store.Close(); root.Close() }
}

// editJournal applies fn to a clone of the crashed transaction's journal and
// persists it, fabricating an intermediate state a lifecycle hook cannot hit.
func editJournal(t *testing.T, f *fixture, fn func(j *transaction.Journal)) {
	t.Helper()
	store, closeFn := openStore(t, f)
	defer closeFn()
	j := store.Journal().Clone()
	fn(j)
	if err := store.Update(j); err != nil {
		t.Fatalf("fabricated journal is invalid: %v", err)
	}
}

func stepByID(t *testing.T, j *transaction.Journal, id string) *transaction.Step {
	t.Helper()
	for i := range j.Steps {
		if j.Steps[i].ID == id {
			return &j.Steps[i]
		}
	}
	t.Fatalf("no step %q in journal", id)
	return nil
}

func assertRecoveredToPre(t *testing.T, f *fixture, res Result) {
	t.Helper()
	if res.ExitCode() != 0 {
		t.Fatalf("recover exit = %d (err=%v)", res.ExitCode(), res.Err)
	}
	f.assertNoResidue()
	if d := diffTrees(f.pre, treeState(t, f.base, false)); d != "" {
		t.Errorf("recovery did not restore the pre-transaction tree:\n%s", d)
	}
}

// --- TestPublishRecoveryMatrix ---

func TestPublishRecoveryMatrix(t *testing.T) {
	if os.Getenv(envCrashCase) != "" {
		t.Skip("running as the crash child")
	}

	// 1. every lifecycle point of every scenario
	for _, sc := range crashScenarios() {
		sc := sc
		t.Run("crash/"+sc.name, func(t *testing.T) {
			points := discoverPoints(t, sc)
			must := []string{"began", "registered", "before:mkdir-staging", "after:mkdir-staging", "after:stage-clash", "after:stage-sr", "validated",
				"before:write-clash", "after:write-clash", "before:write-sr", "after:write-sr", "verified-local", "verified-https", "before-commit", "after-commit-flip"}
			have := map[string]bool{}
			for _, p := range points {
				have[p] = true
			}
			for _, m := range must {
				if !have[m] {
					t.Errorf("publish never reported crash point %q (got %v)", m, points)
				}
			}
			if sc.name == "add_node" {
				for _, m := range []string{"before:snap-nodes", "after:snap-nodes", "before:write-nodes", "after:write-nodes"} {
					if !have[m] {
						t.Errorf("publish never reported crash point %q (got %v)", m, points)
					}
				}
			}
			for _, point := range points {
				point := point
				t.Run(strings.NewReplacer(":", "_", "/", "_").Replace(point), func(t *testing.T) {
					f := crashAt(t, sc, point)
					res := RunRecover(context.Background(), f.env, RecoverParams{})
					if point == "after-commit-flip" {
						// committed: recover only finishes the forward cleanup
						if res.ExitCode() != 0 || !res.Recovered || res.RecoveredOutcome != transaction.OutcomeCommitted {
							t.Fatalf("recover after the commit flip: %+v", res)
						}
						f.assertNoResidue()
						f.assertPublished(sc.finalEnabled(f))
						return
					}
					assertRecoveredToPre(t, f, res)
					// a second recover has nothing left to do
					if r2 := RunRecover(context.Background(), f.env, RecoverParams{}); r2.ExitCode() != 0 {
						t.Errorf("second recover exit = %d", r2.ExitCode())
					}
				})
			}
		})
	}

	// 2. intermediate states of each final write_file: pending, started with
	// nothing done, started with the registered temp left behind, started with
	// the target already at post, done
	for _, target := range []string{"nodes", "clash", "sr"} {
		target := target
		sc := scenarioByName("add_node")
		fileOf := func(f *fixture) string {
			switch target {
			case "nodes":
				return f.nodesPath
			case "clash":
				return f.clashPath()
			}
			return f.srPath()
		}
		id := "write-" + target
		t.Run("write_file_states/"+target, func(t *testing.T) {

			t.Run("started_nothing_done", func(t *testing.T) {
				f := crashAt(t, sc, "before:"+id)
				editJournal(t, f, func(j *transaction.Journal) { stepByID(t, j, id).Status = transaction.StatusStarted })
				assertRecoveredToPre(t, f, RunRecover(context.Background(), f.env, RecoverParams{}))
			})
			t.Run("started_temp_left_behind", func(t *testing.T) {
				f := crashAt(t, sc, "before:"+id)
				var temp string
				editJournal(t, f, func(j *transaction.Journal) {
					s := stepByID(t, j, id)
					s.Status = transaction.StatusStarted
					temp = s.Temps[0]
				})
				if err := os.WriteFile(filepath.Join(filepath.Dir(fileOf(f)), temp), []byte("partial write"), 0600); err != nil {
					t.Fatal(err)
				}
				assertRecoveredToPre(t, f, RunRecover(context.Background(), f.env, RecoverParams{}))
			})
			t.Run("started_target_already_post", func(t *testing.T) {
				f := crashAt(t, sc, "after:"+id)
				editJournal(t, f, func(j *transaction.Journal) { stepByID(t, j, id).Status = transaction.StatusStarted })
				assertRecoveredToPre(t, f, RunRecover(context.Background(), f.env, RecoverParams{}))
			})
			t.Run("done", func(t *testing.T) {
				f := crashAt(t, sc, "after:"+id)
				assertRecoveredToPre(t, f, RunRecover(context.Background(), f.env, RecoverParams{}))
			})
		})
	}

	// 3. originally-absent files: the registered temp left behind, then
	// rollback deletes what the publish created
	t.Run("absent_files_started_with_temp", func(t *testing.T) {
		sc := scenarioByName("subscription_files_absent")
		f := crashAt(t, sc, "before:write-clash")
		var temp string
		editJournal(t, f, func(j *transaction.Journal) {
			s := stepByID(t, j, "write-clash")
			s.Status = transaction.StatusStarted
			temp = s.Temps[0]
		})
		os.WriteFile(filepath.Join(f.tokenDir, temp), []byte("x"), 0600)
		assertRecoveredToPre(t, f, RunRecover(context.Background(), f.env, RecoverParams{}))
	})

	// 4. staging directory: the mkdir/remove_dir chain on one path
	t.Run("staging_chain", func(t *testing.T) {
		sc := scenarioByName("add_node")
		stagingOf := func(f *fixture) string {
			ents, _ := os.ReadDir(f.wwwRoot)
			for _, e := range ents {
				if strings.HasPrefix(e.Name(), ".proxyctl-publish-") {
					return filepath.Join(f.wwwRoot, e.Name())
				}
			}
			return ""
		}
		t.Run("mkdir_pending_is_not_tampering", func(t *testing.T) {
			f := crashAt(t, sc, "registered")
			if stagingOf(f) != "" {
				t.Fatal("staging exists before its mkdir ran")
			}
			assertRecoveredToPre(t, f, RunRecover(context.Background(), f.env, RecoverParams{}))
		})
		t.Run("mkdir_started_not_created", func(t *testing.T) {
			f := crashAt(t, sc, "registered")
			editJournal(t, f, func(j *transaction.Journal) { stepByID(t, j, "mkdir-staging").Status = transaction.StatusStarted })
			assertRecoveredToPre(t, f, RunRecover(context.Background(), f.env, RecoverParams{}))
		})
		t.Run("mkdir_started_empty_dir_exists", func(t *testing.T) {
			f := crashAt(t, sc, "registered")
			var name string
			editJournal(t, f, func(j *transaction.Journal) {
				s := stepByID(t, j, "mkdir-staging")
				s.Status = transaction.StatusStarted
				name = filepath.Base(s.Target)
			})
			if err := os.Mkdir(filepath.Join(f.wwwRoot, name), 0755); err != nil {
				t.Fatal(err)
			}
			assertRecoveredToPre(t, f, RunRecover(context.Background(), f.env, RecoverParams{}))
		})
		t.Run("staging_deleted_after_mkdir_done_is_tampering", func(t *testing.T) {
			f := crashAt(t, sc, "after:mkdir-staging")
			s := stagingOf(f)
			if err := os.Remove(s); err != nil {
				t.Fatal(err)
			}
			before := treeState(t, f.base, true)
			journalBefore := readFileT(t, filepath.Join(f.txnDir(), "journal.json"))
			res := RunRecover(context.Background(), f.env, RecoverParams{})
			if res.ExitCode() != 5 || !strings.Contains(res.TxnDir, TxnDirName) {
				t.Fatalf("exit = %d txn=%q err=%v, want 5", res.ExitCode(), res.TxnDir, res.Err)
			}
			assertTreeAndJournalUntouched(t, f, before, journalBefore)
		})
		t.Run("staged_write_started_temp_left_in_staging", func(t *testing.T) {
			f := crashAt(t, sc, "after:mkdir-staging")
			var temp string
			editJournal(t, f, func(j *transaction.Journal) {
				s := stepByID(t, j, "stage-clash")
				s.Status = transaction.StatusStarted
				temp = s.Temps[0]
			})
			if err := os.WriteFile(filepath.Join(stagingOf(f), temp), []byte("partial"), 0600); err != nil {
				t.Fatal(err)
			}
			res := RunRecover(context.Background(), f.env, RecoverParams{})
			assertRecoveredToPre(t, f, res)
			if res.RecoveredOutcome != transaction.OutcomeNoChange {
				t.Errorf("outcome = %v, want no_change", res.RecoveredOutcome)
			}
		})
		t.Run("snapshot_started_temp_left_in_snap", func(t *testing.T) {
			f := crashAt(t, sc, "before:snap-clash")
			var temp string
			editJournal(t, f, func(j *transaction.Journal) {
				s := stepByID(t, j, "snap-clash")
				s.Status = transaction.StatusStarted
				temp = s.Temps[0]
			})
			if err := os.WriteFile(filepath.Join(f.txnDir(), "snap", temp), []byte("partial"), 0600); err != nil {
				t.Fatal(err)
			}
			assertRecoveredToPre(t, f, RunRecover(context.Background(), f.env, RecoverParams{}))
		})
		t.Run("only_provisional_steps_ran_needs_no_snapshot", func(t *testing.T) {
			// staging and its two staged files exist, no snapshot was taken: the
			// rollback plan is just "remove staging" (Provisional steps do not
			// count as a modification, spec §7.4)
			f := crashAt(t, sc, "after:stage-sr")
			if stagingOf(f) == "" {
				t.Fatal("staging missing")
			}
			res := RunRecover(context.Background(), f.env, RecoverParams{})
			assertRecoveredToPre(t, f, res)
			if res.RecoveredOutcome != transaction.OutcomeNoChange {
				t.Errorf("outcome = %v, want no_change (only staging was created)", res.RecoveredOutcome)
			}
		})
		t.Run("committed_cleanup_states", func(t *testing.T) {
			finalEn := sc.finalEnabled(newFixture(t))
			t.Run("unstage_started_file_already_gone", func(t *testing.T) {
				f := crashAt(t, sc, "after-commit-flip")
				staging := stagingOf(f)
				os.Remove(filepath.Join(staging, "clash.yaml"))
				editJournal(t, f, func(j *transaction.Journal) { stepByID(t, j, "unstage-clash").Status = transaction.StatusStarted })
				res := RunRecover(context.Background(), f.env, RecoverParams{})
				if res.ExitCode() != 0 {
					t.Fatalf("exit = %d (%v)", res.ExitCode(), res.Err)
				}
				f.assertNoResidue()
				f.assertPublished(finalEn)
			})
			t.Run("rmdir_started_dir_already_gone", func(t *testing.T) {
				f := crashAt(t, sc, "after-commit-flip")
				staging := stagingOf(f)
				os.Remove(filepath.Join(staging, "clash.yaml"))
				os.Remove(filepath.Join(staging, "sr.txt"))
				os.Remove(staging)
				editJournal(t, f, func(j *transaction.Journal) {
					stepByID(t, j, "unstage-clash").Status = transaction.StatusDone
					stepByID(t, j, "unstage-sr").Status = transaction.StatusDone
					stepByID(t, j, "rmdir-staging").Status = transaction.StatusStarted
				})
				res := RunRecover(context.Background(), f.env, RecoverParams{})
				if res.ExitCode() != 0 {
					t.Fatalf("exit = %d (%v)", res.ExitCode(), res.Err)
				}
				f.assertNoResidue()
				f.assertPublished(finalEn)
			})
		})
	})
}

func readFileT(t *testing.T, p string) []byte {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func assertTreeAndJournalUntouched(t *testing.T, f *fixture, before map[string]entryState, journalBefore []byte) {
	t.Helper()
	if d := diffTrees(before, treeState(t, f.base, true)); d != "" {
		t.Errorf("a refused recovery changed the host:\n%s", d)
	}
	if got := readFileT(t, filepath.Join(f.txnDir(), "journal.json")); !bytes.Equal(got, journalBefore) {
		t.Error("a refused recovery modified the journal")
	}
}

// snapOf returns the snap/ path (without the ".snap" suffix) of the named
// snapshot step in the crashed transaction's journal.
func (f *fixture) snapOf(t *testing.T, id string) string {
	t.Helper()
	b := readFileT(t, filepath.Join(f.txnDir(), "journal.json"))
	j, err := transaction.ParseJournal(b)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range j.Steps {
		if s.ID == id && len(s.Temps) == 1 {
			return filepath.Join(f.txnDir(), "snap", s.Temps[0])
		}
	}
	// the crash happened before the snapshot steps ran; return a path that
	// does not exist
	return filepath.Join(f.txnDir(), "snap", "none")
}

func (f *fixture) removeSnapshots(t *testing.T) {
	t.Helper()
	ents, _ := os.ReadDir(filepath.Join(f.txnDir(), "snap"))
	for _, e := range ents {
		os.Remove(filepath.Join(f.txnDir(), "snap", e.Name()))
	}
}

// --- tampering and refusals (spec §7.1, §7.3, §7.4 step 1; §14.2 "不允许的状态") ---

func TestPublishRecoverRefusals(t *testing.T) {
	if os.Getenv(envCrashCase) != "" {
		t.Skip("running as the crash child")
	}
	sc := scenarioByName("add_node")

	refuse := func(t *testing.T, f *fixture, mutate func(t *testing.T, f *fixture)) {
		t.Helper()
		mutate(t, f)
		before := treeState(t, f.base, true)
		journalBefore := readFileT(t, filepath.Join(f.txnDir(), "journal.json"))
		res := RunRecover(context.Background(), f.env, RecoverParams{})
		if res.ExitCode() != 5 {
			t.Fatalf("exit = %d (err=%v), want 5", res.ExitCode(), res.Err)
		}
		if res.TxnDir == "" || !strings.Contains(res.TxnDir, TxnDirName) {
			t.Errorf("TxnDir = %q must name the preserved transaction area", res.TxnDir)
		}
		assertTreeAndJournalUntouched(t, f, before, journalBefore)
	}

	t.Run("a published file was edited externally after the crash", func(t *testing.T) {
		f := crashAt(t, sc, "after:write-clash")
		refuse(t, f, func(t *testing.T, f *fixture) { os.WriteFile(f.clashPath(), []byte("edited"), 0644) })
	})
	t.Run("nodes.conf was edited externally after the crash", func(t *testing.T) {
		f := crashAt(t, sc, "after:write-nodes")
		refuse(t, f, func(t *testing.T, f *fixture) { os.WriteFile(f.nodesPath, []byte("# edited\n"), 0600) })
	})
	t.Run("a not-yet-written file was edited externally (all steps pending)", func(t *testing.T) {
		f := crashAt(t, sc, "before:write-nodes")
		refuse(t, f, func(t *testing.T, f *fixture) { os.WriteFile(f.srPath(), []byte("edited"), 0644) })
	})
	t.Run("unregistered temp in the publish directory", func(t *testing.T) {
		f := crashAt(t, sc, "after:write-clash")
		refuse(t, f, func(t *testing.T, f *fixture) {
			os.WriteFile(filepath.Join(f.tokenDir, ".proxyctl-txn-deadbeef-77"), []byte("x"), 0600)
		})
	})
	t.Run("unregistered temp beside nodes.conf", func(t *testing.T) {
		f := crashAt(t, sc, "after:write-nodes")
		refuse(t, f, func(t *testing.T, f *fixture) {
			os.WriteFile(filepath.Join(filepath.Dir(f.nodesPath), ".proxyctl-txn-deadbeef-78"), []byte("x"), 0600)
		})
	})
	t.Run("unregistered temp inside staging", func(t *testing.T) {
		f := crashAt(t, sc, "after:stage-sr")
		refuse(t, f, func(t *testing.T, f *fixture) {
			ents, _ := os.ReadDir(f.wwwRoot)
			for _, e := range ents {
				if strings.HasPrefix(e.Name(), ".proxyctl-publish-") {
					os.WriteFile(filepath.Join(f.wwwRoot, e.Name(), ".proxyctl-txn-deadbeef-79"), []byte("x"), 0600)
				}
			}
		})
	})
	t.Run("a foreign file that merely looks similar is not a temp", func(t *testing.T) {
		// only the .proxyctl-txn-* format participates (spec §7.3): this one is
		// ignored and the recovery succeeds
		f := crashAt(t, sc, "after:write-clash")
		os.WriteFile(filepath.Join(f.tokenDir, ".nodes.123.tmp"), []byte("x"), 0600)
		res := RunRecover(context.Background(), f.env, RecoverParams{})
		if res.ExitCode() != 0 {
			t.Fatalf("exit = %d (%v)", res.ExitCode(), res.Err)
		}
		if _, err := os.Stat(filepath.Join(f.tokenDir, ".nodes.123.tmp")); err != nil {
			t.Errorf("the foreign file was touched: %v", err)
		}
	})
	t.Run("a published file replaced by a symbolic link", func(t *testing.T) {
		f := crashAt(t, sc, "after:write-clash")
		outside := filepath.Join(f.base, "outside.txt")
		refuse(t, f, func(t *testing.T, f *fixture) {
			os.WriteFile(outside, []byte("do not touch"), 0644)
			os.Remove(f.clashPath())
			os.Symlink(outside, f.clashPath())
		})
		if b := readFileT(t, outside); string(b) != "do not touch" {
			t.Errorf("the link target was modified: %q", b)
		}
	})
	t.Run("unknown journal schema", func(t *testing.T) {
		f := crashAt(t, sc, "after:write-clash")
		refuse(t, f, func(t *testing.T, f *fixture) {
			p := filepath.Join(f.txnDir(), "journal.json")
			b := readFileT(t, p)
			b = bytes.Replace(b, []byte(`"schema":1`), []byte(`"schema":2`), 1)
			os.WriteFile(p, b, 0600)
		})
	})
	t.Run("corrupt journal", func(t *testing.T) {
		f := crashAt(t, sc, "after:write-clash")
		refuse(t, f, func(t *testing.T, f *fixture) {
			os.WriteFile(filepath.Join(f.txnDir(), "journal.json"), []byte("{not json"), 0600)
		})
	})
	t.Run("transaction area with unexpected content and no journal", func(t *testing.T) {
		f := crashAt(t, sc, "began")
		os.Remove(filepath.Join(f.txnDir(), "journal.json"))
		os.WriteFile(filepath.Join(f.txnDir(), "surprise"), []byte("x"), 0600)
		before := treeState(t, f.base, true)
		res := RunRecover(context.Background(), f.env, RecoverParams{})
		if res.ExitCode() != 5 {
			t.Fatalf("exit = %d, want 5", res.ExitCode())
		}
		if d := diffTrees(before, treeState(t, f.base, true)); d != "" {
			t.Errorf("changed:\n%s", d)
		}
	})
	t.Run("transaction area with only journal.json.tmp is a crash during start", func(t *testing.T) {
		f := crashAt(t, sc, "began")
		os.Rename(filepath.Join(f.txnDir(), "journal.json"), filepath.Join(f.txnDir(), "journal.json.tmp"))
		assertRecoveredToPre(t, f, RunRecover(context.Background(), f.env, RecoverParams{}))
	})
	t.Run("an empty transaction area is a crash during start", func(t *testing.T) {
		f := crashAt(t, sc, "began")
		os.Remove(filepath.Join(f.txnDir(), "journal.json"))
		assertRecoveredToPre(t, f, RunRecover(context.Background(), f.env, RecoverParams{}))
	})
	t.Run("a leftover journal.json.tmp beside journal.json is discarded", func(t *testing.T) {
		f := crashAt(t, sc, "after:write-clash")
		os.WriteFile(filepath.Join(f.txnDir(), "journal.json.tmp"), []byte("half"), 0600)
		assertRecoveredToPre(t, f, RunRecover(context.Background(), f.env, RecoverParams{}))
	})
	t.Run("a leftover tombstone is deleted wholesale", func(t *testing.T) {
		f := newFixture(t)
		tomb := filepath.Join(f.stateDir, TombstonePrefix+"abc")
		os.MkdirAll(filepath.Join(tomb, "snap"), 0700)
		os.WriteFile(filepath.Join(tomb, "snap", "secret.snap"), []byte("node credentials"), 0600)
		res := RunRecover(context.Background(), f.env, RecoverParams{})
		if res.ExitCode() != 0 {
			t.Fatalf("exit = %d (%v)", res.ExitCode(), res.Err)
		}
		if _, err := os.Lstat(tomb); !os.IsNotExist(err) {
			t.Errorf("tombstone still there: %v", err)
		}
		if len(res.Notices) == 0 {
			t.Error("deleting a tombstone should be reported")
		}
	})
}

// Ancestor replacement while a publish is running (spec §7.1 "祖先链复验",
// §18.4): the next modification must not run; nothing is written into the
// replacement; and once the real directory is back, recover converges.
func TestPublishAncestorReplacement(t *testing.T) {
	boom := func(f *fixture, at string, swap func(t *testing.T, f *fixture)) func(string) error {
		return func(p string) error {
			if p == at {
				swap(f.t, f)
			}
			return nil
		}
	}
	type swapCase struct {
		name string
		at   string
		swap func(t *testing.T, f *fixture) (decoy string, restore func())
	}
	mkDecoyCopy := func(t *testing.T, src, decoy string) {
		t.Helper()
		if err := os.MkdirAll(decoy, 0755); err != nil {
			t.Fatal(err)
		}
		ents, _ := os.ReadDir(src)
		for _, e := range ents {
			if e.Type().IsRegular() {
				b, _ := os.ReadFile(filepath.Join(src, e.Name()))
				os.WriteFile(filepath.Join(decoy, e.Name()), b, 0644)
			}
		}
	}
	cases := []swapCase{
		{"token dir -> symlink to another dir", "before:write-clash", func(t *testing.T, f *fixture) (string, func()) {
			decoy := filepath.Join(f.base, "decoy-token")
			mkDecoyCopy(t, f.tokenDir, decoy)
			moved := f.tokenDir + ".moved"
			os.Rename(f.tokenDir, moved)
			os.Symlink(decoy, f.tokenDir)
			return decoy, func() { os.Remove(f.tokenDir); os.Rename(moved, f.tokenDir) }
		}},
		{"token dir -> a fresh real directory", "before:write-clash", func(t *testing.T, f *fixture) (string, func()) {
			moved := f.tokenDir + ".moved"
			os.Rename(f.tokenDir, moved)
			mkDecoyCopy(t, moved, f.tokenDir)
			return f.tokenDir, func() { os.RemoveAll(f.tokenDir); os.Rename(moved, f.tokenDir) }
		}},
		{"subscription root -> symlink", "before:write-clash", func(t *testing.T, f *fixture) (string, func()) {
			decoy := filepath.Join(f.base, "decoy-root")
			mkDecoyCopy(t, f.tokenDir, filepath.Join(decoy, testToken))
			moved := f.subRoot + ".moved"
			os.Rename(f.subRoot, moved)
			os.Symlink(decoy, f.subRoot)
			return decoy, func() { os.Remove(f.subRoot); os.Rename(moved, f.subRoot) }
		}},
		{"subscription root -> a fresh real directory", "after:write-clash", func(t *testing.T, f *fixture) (string, func()) {
			moved := f.subRoot + ".moved"
			os.Rename(f.subRoot, moved)
			mkDecoyCopy(t, filepath.Join(moved, testToken), filepath.Join(f.subRoot, testToken))
			return f.subRoot, func() { os.RemoveAll(f.subRoot); os.Rename(moved, f.subRoot) }
		}},
		{"nodes.conf directory -> symlink", "after:snap-sr", func(t *testing.T, f *fixture) (string, func()) {
			dir := filepath.Dir(f.nodesPath)
			decoy := filepath.Join(f.base, "decoy-etc")
			mkDecoyCopy(t, dir, decoy)
			moved := dir + ".moved"
			os.Rename(dir, moved)
			os.Symlink(decoy, dir)
			return decoy, func() { os.Remove(dir); os.Rename(moved, dir) }
		}},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			f := newFixture(t)
			var decoy string
			var restore func()
			var decoyBefore map[string]entryState
			setHook(boom(f, c.at, func(t *testing.T, f *fixture) {
				decoy, restore = c.swap(t, f)
				decoyBefore = treeState(t, decoy, true)
			}))
			res := Run(context.Background(), f.env, Params{Mutation: Mutation{Kind: KindAdd, Node: hy2Node("X")}})
			setHook(func(string) error { return nil })
			if res.ExitCode() != 5 {
				t.Fatalf("exit = %d (err=%v), want 5", res.ExitCode(), res.Err)
			}
			if restore == nil {
				t.Fatal("swap never ran")
			}
			// nothing was written into (or changed in) the replacement
			if d := diffTrees(decoyBefore, treeState(t, decoy, true)); d != "" {
				t.Errorf("the replacement directory was modified:\n%s", d)
			}
			// restore the real directory: the transaction was preserved and
			// converges to the previous publication
			restore()
			rres := RunRecover(context.Background(), f.env, RecoverParams{})
			if rres.ExitCode() != 0 {
				t.Fatalf("recover after restoring the directory: exit = %d (%v)", rres.ExitCode(), rres.Err)
			}
			f.assertNoResidue()
		})
	}
}

// Recovery is resumable: a recover that fails midway leaves a rollback plan
// in the journal, and the next recover continues from it instead of
// rewriting what was already restored (spec §7.4 step 4, plan T5/T9).
func TestPublishRecoverResumesWithoutRewriting(t *testing.T) {
	if os.Getenv(envCrashCase) != "" {
		t.Skip("running as the crash child")
	}
	sc := scenarioByName("add_node")
	f := crashAt(t, sc, "after:write-sr")

	// hide the clash snapshot: the rollback restores nodes.conf (shallower
	// path, planned first), then cannot restore clash.yaml
	clashSnap := f.snapOf(t, "snap-clash") + ".snap"
	saved := readFileT(t, clashSnap)
	if err := os.Remove(clashSnap); err != nil {
		t.Fatal(err)
	}
	res := RunRecover(context.Background(), f.env, RecoverParams{})
	if res.ExitCode() != 5 {
		t.Fatalf("first recover: exit = %d, want 5", res.ExitCode())
	}
	if _, err := os.Lstat(f.txnDir()); err != nil {
		t.Fatalf("the transaction area must survive a failed recover: %v", err)
	}
	st1 := mustStat(t, f.nodesPath)
	if got := readFileT(t, f.nodesPath); !bytes.Equal(got, []byte(nodes.Serialize([]nodes.Node{hy2Node("A"), hy2Node("B")}))) {
		t.Fatalf("nodes.conf was not restored by the first (partial) recovery")
	}
	ino1 := inodeOf(st1)

	if err := os.WriteFile(clashSnap, saved, 0600); err != nil {
		t.Fatal(err)
	}
	res = RunRecover(context.Background(), f.env, RecoverParams{})
	assertRecoveredToPre(t, f, res)
	if ino2 := inodeOf(mustStat(t, f.nodesPath)); ino2 != ino1 {
		t.Errorf("nodes.conf was rewritten by the second recover (inode %d -> %d)", ino1, ino2)
	}
}

func TestPublishRecoverBudgetTimeout(t *testing.T) {
	if os.Getenv(envCrashCase) != "" {
		t.Skip("running as the crash child")
	}
	sc := scenarioByName("add_node")
	f := crashAt(t, sc, "after:write-clash")
	f.env.RollbackBudget = 1 // 1ns: the independent rollback budget expires immediately
	res := RunRecover(context.Background(), f.env, RecoverParams{})
	if res.ExitCode() != 5 || !strings.Contains(res.TxnDir, TxnDirName) {
		t.Fatalf("exit = %d txn=%q err=%v, want 5", res.ExitCode(), res.TxnDir, res.Err)
	}
	f.env.RollbackBudget = 0
	assertRecoveredToPre(t, f, RunRecover(context.Background(), f.env, RecoverParams{}))
}

// A crash inside recover itself (before the engine starts) is just another
// leftover: the next recover converges.
func TestPublishRecoverCrashesThenConverges(t *testing.T) {
	if os.Getenv(envCrashCase) != "" {
		t.Skip("running as the crash child")
	}
	sc := scenarioByName("add_node")
	f := crashAt(t, sc, "after:write-sr")
	cmd := exec.Command(os.Args[0], "-test.run=^TestPublishCrashChild$", "-test.timeout=2m")
	cmd.Env = append(os.Environ(), envCrashCase+"="+sc.name, envCrashBase+"="+f.base, envCrashPoint+"=recover:opening")
	out, err := cmd.CombinedOutput()
	if ee, ok := err.(*exec.ExitError); !ok || ee.ExitCode() != crashExit {
		t.Fatalf("recover child did not crash at recover:opening: %v\n%s", err, out)
	}
	assertRecoveredToPre(t, f, RunRecover(context.Background(), f.env, RecoverParams{}))
}

// --- --abandon (spec §7.4 "人工放弃出口") ---

func TestPublishRecoverAbandon(t *testing.T) {
	if os.Getenv(envCrashCase) != "" {
		t.Skip("running as the crash child")
	}
	sc := scenarioByName("add_node")

	t.Run("all paths at their pre-transaction state: abandon succeeds", func(t *testing.T) {
		f := crashAt(t, sc, "registered")
		res := RunRecover(context.Background(), f.env, RecoverParams{Abandon: true})
		assertRecoveredToPre(t, f, res)
	})
	t.Run("a modified path: abandon refuses and changes nothing", func(t *testing.T) {
		f := crashAt(t, sc, "after:write-clash")
		before := treeState(t, f.base, true)
		journalBefore := readFileT(t, filepath.Join(f.txnDir(), "journal.json"))
		res := RunRecover(context.Background(), f.env, RecoverParams{Abandon: true})
		if res.ExitCode() != 1 || !res.PreTransaction {
			t.Fatalf("exit = %d (%v), want 1", res.ExitCode(), res.Err)
		}
		assertTreeAndJournalUntouched(t, f, before, journalBefore)
	})
	t.Run("staging still exists: not yet at the pre-transaction state", func(t *testing.T) {
		f := crashAt(t, sc, "after:mkdir-staging")
		before := treeState(t, f.base, true)
		journalBefore := readFileT(t, filepath.Join(f.txnDir(), "journal.json"))
		res := RunRecover(context.Background(), f.env, RecoverParams{Abandon: true})
		if res.ExitCode() != 1 {
			t.Fatalf("exit = %d, want 1", res.ExitCode())
		}
		assertTreeAndJournalUntouched(t, f, before, journalBefore)
	})
	t.Run("a committed transaction is never abandoned", func(t *testing.T) {
		f := crashAt(t, sc, "after-commit-flip")
		res := RunRecover(context.Background(), f.env, RecoverParams{Abandon: true})
		if res.ExitCode() != 1 {
			t.Fatalf("exit = %d, want 1", res.ExitCode())
		}
	})
}

// --- recover preconditions ---

func TestPublishRecoverPreconditions(t *testing.T) {
	f := newFixture(t)
	if _, err := PrecheckRecover(f.env, RecoverParams{MinVersion: "v0.10.0"}); err != nil {
		t.Fatalf("PrecheckRecover: %v", err)
	}
	f.env.ProxyctlVersion = "dev"
	if res := RunRecover(context.Background(), f.env, RecoverParams{MinVersion: "v0.10.0"}); res.ExitCode() != 1 {
		t.Fatalf("dev build with --min-version: exit = %d, want 1", res.ExitCode())
	}
	f.env.ProxyctlVersion = "v0.10.0"
	os.Remove(f.installJS)
	if res := RunRecover(context.Background(), f.env, RecoverParams{}); res.ExitCode() != 1 {
		t.Fatalf("missing install.json: exit = %d, want 1", res.ExitCode())
	}
}

func inodeOf(fi os.FileInfo) uint64 { return fi.Sys().(*syscall.Stat_t).Ino }
