#!/usr/bin/env bash
# shellcheck disable=SC2317,SC2329
# SC2317/SC2329: some helpers here are invoked only inside a subshell or a
# background job, which shellcheck can't always trace as "reachable".
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
TEST_DIR="$(mktemp -d)"
trap 'rm -rf "$TEST_DIR"' EXIT

pass=0
fail=0

ok() {
  echo "PASS: $1"
  pass=$((pass + 1))
}

bad() {
  echo "FAIL: $1" >&2
  fail=$((fail + 1))
}

# ---------------------------------------------------------------------------
# install-proxy.sh: reject_if_incompatible_host (spec §11.5)
# ---------------------------------------------------------------------------

extract_function() {
  local function_name="$1"
  awk -v name="$function_name" '
    $0 ~ "^" name "\\(\\)[[:space:]]*\\{" { active=1 }
    active {
      print
      line=$0; opens=gsub(/\{/, "", line)
      line=$0; closes=gsub(/\}/, "", line)
      depth += opens - closes
      if (depth == 0) exit
    }
  ' "$REPO_ROOT/install-proxy.sh"
}

GUARD_HARNESS="$TEST_DIR/guard_harness.sh"
{
  printf '%s\n' '#!/usr/bin/env bash'
  printf '%s\n' 'set -euo pipefail'
  extract_function reject_if_incompatible_host
} > "$GUARD_HARNESS"
grep -q '^reject_if_incompatible_host()' "$GUARD_HARNESS" || {
  echo "reject_if_incompatible_host is missing from install-proxy.sh" >&2
  exit 1
}

guard_env() {
  local d="$1"
  echo "REALITY_UNIT_FILE=$d/unit REALITY_CONFIG_DIR=$d/cfgdir REALITY_STATE_DIR=$d/state REALITY_BIN_DIR=$d/bin REALITY_TXN_DIR=$d/txn REALITY_TOMB_GLOB=$d/tomb-* PUBLISH_TXN_DIR=$d/publish PUBLISH_TOMB_GLOB=$d/ptomb-*"
}

run_guard_case() {
  local name="$1" setup="$2" want_reject="$3"
  local d="$TEST_DIR/guard-$name"
  mkdir -p "$d"
  "$setup" "$d"
  set +e
  # shellcheck disable=SC2046
  env $(guard_env "$d") bash -c "source '$GUARD_HARNESS'; reject_if_incompatible_host" \
    >"$d/stdout" 2>"$d/stderr"
  local status=$?
  set -e
  if [[ "$want_reject" == "reject" ]]; then
    if [[ "$status" -eq 0 ]]; then
      bad "$name (expected rejection, got exit 0)"
    else
      ok "$name"
    fi
  else
    if [[ "$status" -ne 0 ]]; then
      bad "$name (expected success, got exit $status: $(cat "$d/stderr"))"
    else
      ok "$name"
    fi
  fi
}

setup_clear() { :; }
run_guard_case "clear-host-passes" setup_clear pass

setup_reality_state_dir() { mkdir -p "$1/state"; }
run_guard_case "reality-state-dir-rejects" setup_reality_state_dir reject

setup_reality_unit_file() { : > "$1/unit"; }
run_guard_case "reality-unit-file-rejects" setup_reality_unit_file reject

setup_reality_txn_dir() { mkdir -p "$1/txn"; }
run_guard_case "reality-txn-dir-rejects" setup_reality_txn_dir reject

setup_reality_tombstone() { mkdir -p "$1/tomb-abc123"; }
run_guard_case "reality-tombstone-glob-rejects" setup_reality_tombstone reject

setup_publish_txn() { mkdir -p "$1/publish"; }
run_guard_case "publish-txn-dir-rejects" setup_publish_txn reject

setup_publish_tombstone() { mkdir -p "$1/ptomb-xyz"; }
run_guard_case "publish-tombstone-glob-rejects" setup_publish_tombstone reject

# ---------------------------------------------------------------------------
# merge-nodes.sh: L1 -> L2 real cross-process blocking (no sleep barriers:
# a FIFO write only happens after the holder genuinely holds the flock).
# ---------------------------------------------------------------------------

hold_lock_via_fifo() {
  local lock_file="$1" ready_fifo="$2" release_fifo="$3"
  exec 9>"$lock_file"
  flock 9
  echo ready > "$ready_fifo"
  read -r _ < "$release_fifo"
}

merge_env_for() {
  local d="$1"
  printf 'LOCK_FILE=%s LOCK2_FILE=%s PUBLISH_TXN_DIR=%s PUBLISH_TOMB_GLOB=%s TOKEN_FILE=%s SUB_ROOT=%s PROXYCTL_BIN=%s' \
    "$d/l1.lock" "$d/l2.lock" "$d/publish" "$d/tomb-*" "$d/token" "$d/out" "$d/fixed-proxyctl"
}

test_merge_blocks_on_held_lock() {
  local which="$1" # "l1" or "l2"
  local d="$TEST_DIR/merge-lock-$which"
  mkdir -p "$d/out"
  printf '%s\n' 'Node1|1.2.3.4|443|pass123|obfs123|example.com' > "$d/nodes.conf"
  printf '%s\n' 'abcdefghijklmnop' > "$d/token"

  local lock_path="$d/l1.lock"
  [[ "$which" == "l2" ]] && lock_path="$d/l2.lock"

  local ready_fifo="$d/ready.fifo" release_fifo="$d/release.fifo"
  mkfifo "$ready_fifo" "$release_fifo"

  hold_lock_via_fifo "$lock_path" "$ready_fifo" "$release_fifo" &
  local holder_pid=$!
  # Block until the holder genuinely holds the flock (pipe barrier, not a
  # fixed sleep): the write to ready_fifo only happens after `flock 9`
  # returns.
  read -r _ < "$ready_fifo"

  set +e
  # shellcheck disable=SC2046
  env $(merge_env_for "$d") timeout 3 bash "$REPO_ROOT/merge-nodes.sh" "$d/nodes.conf" \
    >"$d/stdout" 2>"$d/stderr"
  local status=$?
  set -e

  # Release the holder and reap it regardless of the assertion outcome.
  echo go > "$release_fifo"
  wait "$holder_pid"

  # merge-nodes.sh's own flock waits up to 30s; bounding this run with
  # `timeout 3` means "blocked" shows up as a nonzero exit here (timeout's
  # 124, or flock's own failure once it gives up) rather than ever
  # succeeding while the lock is held.
  if [[ "$status" -eq 0 ]]; then
    bad "merge-nodes.sh did not block on a held $which"
  else
    ok "merge-nodes.sh blocks while $which is held by another holder"
  fi
}

test_merge_blocks_on_held_lock l1
test_merge_blocks_on_held_lock l2

echo
echo "shared guard tests: $pass passed, $fail failed"
[[ "$fail" -eq 0 ]]
