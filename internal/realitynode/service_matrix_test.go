package realitynode

// service_matrix_test.go: the plan's named TestServiceLifecycleMatrix — for
// install and both reinstall sources, one row per exit class of spec §13.1:
//
//	0  committed
//	1  failed before the first managed path was modified
//	4  automatically rolled back to the pre-transaction state
//	5  transaction begun but recovery could not be confirmed (needs a human)
//
// Each row asserts the exit code (Result.ExitCode, the one mapping
// cmd/proxyctl applies) and the final on-disk state: bytes, mode, owner and
// existence of every managed path for 1 and 4, the installed state for 0,
// and the preserved transaction area for 5. The rows reuse the constructions
// of the dedicated commit/rollback/failure tests in this package; those
// tests keep the finer-grained assertions.
import (
	"context"
	"crypto/tls"
	"errors"
	"os"
	"testing"
	"time"
)

type matrixRun struct {
	env   *Env
	roots Roots
	exec  func() Result
}

type matrixCase struct {
	name     string
	wantExit int
	build    func(t *testing.T) matrixRun
}

func TestServiceLifecycleMatrix(t *testing.T) {
	failingRun := func(t *testing.T) string { return buildFakeSingboxScript(t, "1.14.1") } // `run` exits 1
	working := func(t *testing.T) string { return buildWorkingFakeSingbox(t, "1.14.1", false).path }
	installParams := func(t *testing.T) InstallParams {
		return InstallParams{Name: "jp1", Server: "203.0.113.10", Port: freeTCPPort(t), SNI: "example.com"}
	}
	installed := func(t *testing.T, script string, active bool) (*Env, *fakeServiceSystemd, Roots) {
		env, sysd, roots := testEnv(t, script, "1.14.1")
		env.DataPath.ClientStartGrace = 5 * time.Second
		writeInstalledFixtureOpts(t, roots, fixtureOpts{binContent: readBytes(t, script), listenPort: freeTCPPort(t)})
		if active {
			sysd.loaded, sysd.unitFileState, sysd.activeState = true, "enabled", "active"
		}
		return env, sysd, roots
	}
	uninstalled := func(t *testing.T, script string) (*Env, *fakeServiceSystemd, Roots) {
		env, sysd, roots := testEnv(t, script, "1.14.1")
		env.DataPath.ClientStartGrace = 5 * time.Second
		writeUninstalledFixtureOpts(t, roots, fixtureOpts{listenPort: freeTCPPort(t)})
		return env, sysd, roots
	}
	reinstall := func(env *Env) func() Result {
		return func() Result { return Reinstall(context.Background(), env, ReinstallParams{}) }
	}

	cases := []matrixCase{
		// ---------------- install ----------------
		{"install/committed", 0, func(t *testing.T) matrixRun {
			env, _, roots := testEnv(t, working(t), "1.14.1")
			env.DataPath.ClientStartGrace = 5 * time.Second
			p := installParams(t)
			return matrixRun{env, roots, func() Result { return Install(context.Background(), env, p) }}
		}},
		{"install/precheck failure (dest unreachable)", 1, func(t *testing.T) matrixRun {
			env, _, roots := testEnv(t, working(t), "1.14.1")
			env.TLSDialer = func(context.Context, string, string, *tls.Config) (*tls.Conn, error) {
				return nil, errors.New("simulated: dest unreachable")
			}
			p := installParams(t)
			return matrixRun{env, roots, func() Result { return Install(context.Background(), env, p) }}
		}},
		{"install/cancelled after the transaction began but before any modification", 1, func(t *testing.T) matrixRun {
			env, _, roots := testEnv(t, working(t), "1.14.1")
			p := installParams(t)
			ctx, cancel := context.WithCancel(context.Background())
			t.Cleanup(cancel)
			setHook(t, func(point string) error {
				if point == "downloaded" {
					cancel()
				}
				return nil
			})
			return matrixRun{env, roots, func() Result { return Install(ctx, env, p) }}
		}},
		{"install/selfcheck fails -> rolled back", 4, func(t *testing.T) matrixRun {
			env, _, roots := testEnv(t, failingRun(t), "1.14.1")
			p := installParams(t)
			return matrixRun{env, roots, func() Result { return Install(context.Background(), env, p) }}
		}},
		{"install/rollback cannot be confirmed -> needs intervention", 5, func(t *testing.T) matrixRun {
			env, sysd, roots := testEnv(t, failingRun(t), "1.14.1")
			sysd.stopErr = errors.New("simulated: systemctl stop failed")
			p := installParams(t)
			return matrixRun{env, roots, func() Result { return Install(context.Background(), env, p) }}
		}},

		// ---------------- reinstall, source: installed ----------------
		{"reinstall(installed)/committed", 0, func(t *testing.T) matrixRun {
			env, _, roots := installed(t, working(t), true)
			return matrixRun{env, roots, reinstall(env)}
		}},
		{"reinstall(installed)/reused binary fails sing-box check", 1, func(t *testing.T) matrixRun {
			env, _, roots := installed(t, buildWorkingFakeSingbox(t, "1.14.1", true).path, true)
			return matrixRun{env, roots, reinstall(env)}
		}},
		{"reinstall(installed)/selfcheck fails -> rolled back", 4, func(t *testing.T) matrixRun {
			env, _, roots := installed(t, failingRun(t), true)
			return matrixRun{env, roots, reinstall(env)}
		}},
		{"reinstall(installed)/rollback cannot be confirmed -> needs intervention", 5, func(t *testing.T) matrixRun {
			env, sysd, roots := installed(t, failingRun(t), true)
			sysd.stopErr = errors.New("simulated: systemctl stop failed")
			return matrixRun{env, roots, reinstall(env)}
		}},

		// ---------------- reinstall, source: uninstalled, identity retained ----------------
		{"reinstall(uninstalled)/committed", 0, func(t *testing.T) matrixRun {
			env, _, roots := uninstalled(t, working(t))
			return matrixRun{env, roots, reinstall(env)}
		}},
		{"reinstall(uninstalled)/listen_port taken -> refused at recheck", 1, func(t *testing.T) matrixRun {
			env, _, roots := testEnv(t, working(t), "1.14.1")
			l, port := freeLoopbackListener(t)
			_ = l
			writeUninstalledFixtureOpts(t, roots, fixtureOpts{listenPort: port})
			return matrixRun{env, roots, reinstall(env)}
		}},
		{"reinstall(uninstalled)/selfcheck fails -> rolled back", 4, func(t *testing.T) matrixRun {
			env, _, roots := uninstalled(t, failingRun(t))
			return matrixRun{env, roots, reinstall(env)}
		}},
		{"reinstall(uninstalled)/rollback cannot be confirmed -> needs intervention", 5, func(t *testing.T) matrixRun {
			env, sysd, roots := uninstalled(t, failingRun(t))
			sysd.stopErr = errors.New("simulated: systemctl stop failed")
			return matrixRun{env, roots, reinstall(env)}
		}},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			m := tc.build(t)
			before := treeSnapshot(t, m.roots)
			stateBefore := DetectInstanceState(m.env.FS, m.roots).Status

			res := m.exec()

			if got := res.ExitCode(); got != tc.wantExit {
				t.Fatalf("exit code = %d, want %d\nresult: %+v\ncause: %v", got, tc.wantExit, res, res.Cause)
			}
			switch tc.wantExit {
			case 0:
				assertCommitted(t, res)
				assertInstalledState(t, m.env)
				assertNoTxnArea(t, m.roots)
				if res.Stdout == "" {
					t.Errorf("a committed run must print its registration block")
				}
			case 1:
				if !res.PreTransaction || res.Err == nil || res.Stdout != "" {
					t.Errorf("want a pre-transaction failure with an error and no stdout, got %+v", res)
				}
				assertTreeEqual(t, before, treeSnapshot(t, m.roots))
				assertNoTxnArea(t, m.roots)
				if st := DetectInstanceState(m.env.FS, m.roots).Status; st != stateBefore {
					t.Errorf("state changed: %v -> %v", stateBefore, st)
				}
			case 4:
				if res.Err != nil || res.PreTransaction || !res.HasOutcome || res.Outcome.String() != "rolled_back" || res.Cause == nil || res.Stdout != "" {
					t.Errorf("want a rolled-back result with a cause and no stdout, got %+v", res)
				}
				assertTreeEqual(t, before, treeSnapshot(t, m.roots))
				assertNoTxnArea(t, m.roots)
				if st := DetectInstanceState(m.env.FS, m.roots).Status; st != stateBefore {
					t.Errorf("state changed: %v -> %v", stateBefore, st)
				}
			case 5:
				if res.PreTransaction || res.Err == nil || res.TxnDir != m.roots.TxnDir || res.Stdout != "" {
					t.Errorf("want a needs-intervention result naming the transaction area, got %+v", res)
				}
				if _, err := os.Stat(m.roots.TxnDir); err != nil {
					t.Errorf("the transaction area must be preserved: %v", err)
				}
				if st := DetectInstanceState(m.env.FS, m.roots).Status; st != StatusUnfinishedTxn {
					t.Errorf("state = %v, want the unfinished-transaction state", st)
				}
			}
		})
	}
}
