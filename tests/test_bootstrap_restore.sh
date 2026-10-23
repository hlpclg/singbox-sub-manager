#!/usr/bin/env bash
# shellcheck disable=SC2016,SC2317,SC2329
# Manual rollback from the bootstrap previous copy (v0.10 spec §12 "人工回滚").
#
# The commands under test are NOT retyped here: they are extracted from the
# fenced block marked proxyctl-manual-restore in docs/reality-node.md, so the
# documentation and the test cannot drift apart. Only the four production
# paths and the two lock waits at the top of the block are rewritten to a
# temp root (the doc keeps the production paths); everything else runs
# verbatim under bash with real flock(2) locks.
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
DOC="$REPO_ROOT/docs/reality-node.md"
TEST_DIR="$(mktemp -d)"
HOLDER_PIDS=()
BG_PIDS=()
cleanup() {
  local p
  for p in "${BG_PIDS[@]:-}" "${HOLDER_PIDS[@]:-}"; do
    if [[ -n "$p" ]]; then kill "$p" 2>/dev/null || true; fi
  done
  rm -rf "$TEST_DIR"
}
trap cleanup EXIT

pass=0
fail=0
ok() { echo "PASS: $1"; pass=$((pass + 1)); }
bad() { echo "FAIL: $1" >&2; fail=$((fail + 1)); }
expect() {
  local desc="$1"
  shift
  if "$@"; then ok "$desc"; else bad "$desc"; fi
}

sha_of() { sha256sum "$1" | awk '{print $1}'; }
sidecar_of() { tr -d '[:space:]' < "$1"; }
mode_is() { [[ "$(stat -c %a "$1")" == "$2" ]]; }

# ---------------------------------------------------------------------------
# Extract the documented block (exactly one, between the two markers).
# ---------------------------------------------------------------------------

BLOCK_RAW="$TEST_DIR/block.raw"
extract_block() {
  awk '
    /<!-- proxyctl-manual-restore:begin -->/ { inmark = 1; next }
    /<!-- proxyctl-manual-restore:end -->/ { inmark = 0; next }
    inmark && /^```bash$/ { infence = 1; next }
    inmark && /^```$/ { infence = 0; next }
    inmark && infence { print }
  ' "$DOC" > "$BLOCK_RAW"
}

test_doc_contract() {
  extract_block
  expect "doc has exactly one begin marker" \
    bash -c '[[ "$(grep -c "proxyctl-manual-restore:begin" "$1")" == 1 && "$(grep -c "proxyctl-manual-restore:end" "$1")" == 1 ]]' _ "$DOC"
  expect "doc block is non-empty" test -s "$BLOCK_RAW"
  expect "doc block uses the production paths" \
    bash -c 'grep -qx "L1=/run/lock/singbox-sub-manager.lock" "$1" && grep -qx "L3=/run/lock/proxyctl-reality.lock" "$1" && grep -qx "BIN=/usr/local/bin/proxyctl" "$1" && grep -qx "PREV=/var/lib/proxyctl-bootstrap/previous" "$1"' _ "$BLOCK_RAW"
  expect "doc block takes L1 (fd 9, 30 s) before L3 (fd 8, 10 s)" \
    bash -c '[[ "$(grep -n "flock -w 30 9" "$1" | cut -d: -f1)" -lt "$(grep -n "flock -w 10 8" "$1" | cut -d: -f1)" ]]' _ "$BLOCK_RAW"
  expect "doc block verifies the pair before the first mv" \
    bash -c '[[ "$(grep -n "sha256sum \"\$PREV/proxyctl\"" "$1" | head -1 | cut -d: -f1)" -lt "$(grep -n "^mv -f" "$1" | head -1 | cut -d: -f1)" ]]' _ "$BLOCK_RAW"
  expect "doc states that only a pure show keeps the rollback window open" \
    bash -c 'grep -q "只读 \`show\` 不改变实例，回滚窗口保持打开" "$1"' _ "$DOC"
  expect "doc forbids swapping back after any lifecycle change (incl. failed/cancelled/recover)" \
    bash -c 'grep -q "包括失败的和被取消的变更" "$1" && grep -q "不要直接把旧二进制换回去" "$1"' _ "$DOC"
  expect "doc says to stop when unsure" bash -c 'grep -q "无法确认是否满足上述条件时，不要执行，先停下来" "$1"' _ "$DOC"
  expect "doc distinguishes the SIGKILL repair (--replace-proxyctl, same release)" \
    bash -c 'grep -q "同一个目标 Release" "$1" && grep -q "bootstrap-node.sh --replace-proxyctl" "$1"' _ "$DOC"
}

# ---------------------------------------------------------------------------
# Temp root for one case
# ---------------------------------------------------------------------------

new_case() {
  C="$TEST_DIR/$1"
  rm -rf "$C"
  mkdir -p "$C/bin" "$C/prev" "$C/lock" "$C/log"
  BIN="$C/bin/proxyctl"
  SIDE="$BIN.sha256"
  PREV="$C/prev"
  L1F="$C/lock/l1.lock"
  L3F="$C/lock/l3.lock"
  W1=0.3
  W3=0.3
  make_stub "$BIN" "v0.10.0" NEWCUR
  printf '%s\n' "$(sha_of "$BIN")" > "$SIDE"
  CUR_SHA="$(sha_of "$BIN")"
  make_stub "$PREV/proxyctl" "v0.9.0" PREVBIN
  OLD_SHA="$(sha_of "$PREV/proxyctl")"
  printf '%s\n' "$OLD_SHA" > "$PREV/proxyctl.sha256"
  chmod 0700 "$PREV/proxyctl"
  chmod 0600 "$PREV/proxyctl.sha256"
}

make_stub() { # PATH VERSION LABEL: records every execution
  cat > "$1" <<EOF
#!/usr/bin/env bash
printf '%s %s\n' '$3' "\$*" >> '$C/log/exec.log'
if [[ "\${1:-}" == version ]]; then printf '%s\n' '$2'; exit 0; fi
exit 99
EOF
  chmod 0755 "$1"
}

# build_script: the documented block with only paths and waits rewritten.
build_script() {
  RESTORE_SH="$C/restore.sh"
  sed -e "s|^L1=.*|L1=$L1F|" -e "s|^L3=.*|L3=$L3F|" \
      -e "s|^BIN=.*|BIN=$BIN|" -e "s|^PREV=.*|PREV=$PREV|" \
      -e "s|flock -w 30 9|flock -w $W1 9|" -e "s|flock -w 10 8|flock -w $W3 8|" \
      "$BLOCK_RAW" > "$RESTORE_SH"
}

run_restore() {
  build_script
  set +e
  timeout 60 bash "$RESTORE_SH" >"$C/stdout" 2>"$C/stderr" </dev/null
  RC=$?
  set -e
}

start_restore() {
  build_script
  env --default-signal=INT bash "$RESTORE_SH" >"$C/stdout" 2>"$C/stderr" </dev/null &
  R_PID=$!
  BG_PIDS+=("$R_PID")
}
finish_restore() {
  set +e
  wait "$R_PID"
  RC=$?
  set -e
}

hold_lock() {
  mkfifo "$C/h.ready" "$C/h.release"
  (
    exec 7>"$1"
    flock 7
    echo ready > "$C/h.ready"
    read -r _ < "$C/h.release"
  ) &
  HOLD_PID=$!
  HOLDER_PIDS+=("$HOLD_PID")
  read -r _ < "$C/h.ready"
}
release_lock() {
  echo go > "$C/h.release"
  wait "$HOLD_PID" || true
}
wait_flock_child() {
  local i d
  for ((i = 0; i < 500; i++)); do
    for d in /proc/[0-9]*; do
      [[ "$(cat "$d/comm" 2>/dev/null)" == flock ]] || continue
      if grep -q "^PPid:[[:space:]]*$1\$" "$d/status" 2>/dev/null; then return 0; fi
    done
    sleep 0.02
  done
  return 1
}

installed_is_current() { [[ "$(sha_of "$BIN")" == "$CUR_SHA" && "$(sidecar_of "$SIDE")" == "$CUR_SHA" ]]; }
installed_is_prev() { [[ "$(sha_of "$BIN")" == "$OLD_SHA" && "$(sidecar_of "$SIDE")" == "$OLD_SHA" ]]; }
prev_never_executed() { ! grep -q '^PREVBIN' "$C/log/exec.log" 2>/dev/null; }
nothing_executed() { [[ ! -s "$C/log/exec.log" ]]; }
no_temps() { ! find "$C/bin" -name '.proxyctl.*' | grep -q .; }
locks_free() {
  { [[ ! -e "$L1F" ]] || flock -n "$L1F" true; } && { [[ ! -e "$L3F" ]] || flock -n "$L3F" true; }
}
stderr_has() { grep -Fq -- "$1" "$C/stderr"; }

expect_refused() { # LABEL: refused, nothing executed, current install untouched
  if [[ "$RC" -ne 0 ]]; then ok "$1: refused (rc=$RC)"; else bad "$1: must be refused"; fi
  expect "$1: the previous copy was not executed" prev_never_executed
  expect "$1: nothing at all was executed" nothing_executed
  expect "$1: current install not overwritten" installed_is_current
  expect "$1: no temp files left" no_temps
  expect "$1: locks released" locks_free
}

# ---------------------------------------------------------------------------
# Cases
# ---------------------------------------------------------------------------

test_good_restore() {
  new_case good
  run_restore
  expect "good pair: rc 0" test "$RC" -eq 0
  expect "good pair: binary is the previous one" bash -c '[[ "$(sha256sum "$1" | cut -d" " -f1)" == "$2" ]]' _ "$BIN" "$OLD_SHA"
  expect "good pair: sidecar matches the restored binary" bash -c '[[ "$(tr -d "[:space:]" < "$1")" == "$2" ]]' _ "$SIDE" "$OLD_SHA"
  expect "good pair: binary mode 0755" mode_is "$BIN" 755
  expect "good pair: sidecar mode 0644" mode_is "$SIDE" 644
  expect "good pair: the restored binary is executed only after verification (version, once)" \
    bash -c '[[ "$(cat "$1")" == "PREVBIN version" ]]' _ "$C/log/exec.log"
  expect "good pair: the replaced current binary was never executed" bash -c '! grep -q NEWCUR "$1"' _ "$C/log/exec.log"
  expect "good pair: restored version printed" grep -qx v0.9.0 "$C/stdout"
  expect "good pair: previous copy left untouched" \
    bash -c '[[ "$(sha256sum "$1/proxyctl" | cut -d" " -f1)" == "$2" ]] && [[ "$(tr -d "[:space:]" < "$1/proxyctl.sha256")" == "$2" ]]' _ "$PREV" "$OLD_SHA"
  expect "good pair: previous copy modes kept" bash -c '[[ "$(stat -c %a "$1/proxyctl")" == 700 && "$(stat -c %a "$1/proxyctl.sha256")" == 600 ]]' _ "$PREV"
  expect "good pair: no temp files left" no_temps
  expect "good pair: both locks released at the end" locks_free
  expect "good pair: only the two files in the install dir" \
    bash -c '[[ "$(find "$1" -mindepth 1 -printf "%f\n" | sort | tr "\n" " ")" == "proxyctl proxyctl.sha256 " ]]' _ "$C/bin"

  # Atomic replace: the restored file arrives by rename (new inode), and a
  # running reader of the old inode is unaffected.
  new_case atomic
  local ino_before ino_after
  ino_before="$(stat -c %i "$BIN")"
  run_restore
  ino_after="$(stat -c %i "$BIN")"
  expect "atomic: restored by rename (inode replaced, not rewritten in place)" test "$ino_before" != "$ino_after"
}

test_refusals() {
  new_case missing-sidecar
  rm -f "$PREV/proxyctl.sha256"
  run_restore
  expect_refused "missing previous sidecar"

  new_case missing-binary
  rm -f "$PREV/proxyctl"
  run_restore
  expect_refused "missing previous binary"

  new_case empty-sidecar
  : > "$PREV/proxyctl.sha256"
  run_restore
  expect_refused "empty previous sidecar"

  new_case mismatched-hash
  printf '%064d\n' 5 > "$PREV/proxyctl.sha256"
  run_restore
  expect_refused "sidecar hash does not match the binary"
  expect "mismatched hash: says so" stderr_has "不匹配"

  new_case binary-changed
  printf '#!/usr/bin/env bash\necho tampered\n' > "$PREV/proxyctl"
  run_restore
  expect_refused "binary bytes do not match the recorded hash"

  # Exactly what SIGKILL / power loss between the two previous-copy renames
  # leaves: the NEW installed bytes in previous/proxyctl, the old sidecar.
  new_case sigkill-mismatch
  cp "$BIN" "$PREV/proxyctl"
  chmod 0700 "$PREV/proxyctl"
  run_restore
  expect_refused "mismatched pair left by an interrupted previous-copy save"

  new_case prev-binary-symlink
  mv "$PREV/proxyctl" "$PREV/real"
  ln -s "$PREV/real" "$PREV/proxyctl"
  run_restore
  expect_refused "previous binary is a symlink (even to a matching file)"

  new_case prev-sidecar-symlink
  mv "$PREV/proxyctl.sha256" "$PREV/real.sha256"
  ln -s "$PREV/real.sha256" "$PREV/proxyctl.sha256"
  run_restore
  expect_refused "previous sidecar is a symlink"

  new_case prev-binary-dir
  rm -f "$PREV/proxyctl"
  mkdir "$PREV/proxyctl"
  run_restore
  expect_refused "previous binary is a directory"

  new_case cur-binary-symlink
  mv "$BIN" "$BIN.real"
  ln -s "$BIN.real" "$BIN"
  run_restore
  expect "current binary is a symlink: refused" test "$RC" -ne 0
  expect "current binary symlink: previous copy not executed" prev_never_executed
  expect "current binary symlink: link target not overwritten" bash -c '[[ "$(sha256sum "$1.real" | cut -d" " -f1)" == "$2" ]]' _ "$BIN" "$CUR_SHA"
  expect "current binary symlink: still a symlink" test -L "$BIN"

  new_case cur-sidecar-dir
  rm -f "$SIDE"
  mkdir "$SIDE"
  run_restore
  expect "current sidecar is a directory: refused" test "$RC" -ne 0
  expect "current sidecar dir: previous copy not executed" prev_never_executed
  expect "current sidecar dir: current binary not overwritten" bash -c '[[ "$(sha256sum "$1" | cut -d" " -f1)" == "$2" ]]' _ "$BIN" "$CUR_SHA"
}

test_lock_order() {
  # L1 held by another process: the block waits for L1 and then gives up; it
  # never reaches L3 (the L3 lock file is not even opened) and changes nothing.
  new_case l1-held
  hold_lock "$L1F"
  run_restore
  release_lock
  expect "L1 held: restore fails after the wait" test "$RC" -ne 0
  expect "L1 held: L3 was never touched (L1 is taken first)" bash -c '[[ ! -e "$1" ]]' _ "$L3F"
  expect "L1 held: current install untouched, nothing executed" installed_is_current
  expect "L1 held: nothing executed" nothing_executed

  # L3 held by another process: the block already holds L1 while it waits.
  new_case l3-held
  hold_lock "$L3F"
  run_restore
  release_lock
  expect "L3 held: restore fails after the wait" test "$RC" -ne 0
  expect "L3 held: current install untouched" installed_is_current
  expect "L3 held: nothing executed" nothing_executed
  expect "L3 held: L1 released after the failure" bash -c 'flock -n "$1" true' _ "$L1F"

  # While blocked on L3, the block holds L1 (L1 -> L3 order); once L3 is
  # released it completes.
  new_case l3-wait-holds-l1
  W3=20
  hold_lock "$L3F"
  start_restore
  if wait_flock_child "$R_PID"; then
    expect "blocked on L3: L1 is already held (competitor flock -n on L1 fails)" bash -c '! flock -n "$1" true' _ "$L1F"
    expect "blocked on L3: nothing replaced yet" installed_is_current
    release_lock
    finish_restore
    expect "after L3 is released the restore completes: rc 0" test "$RC" -eq 0
    expect "after L3 is released: previous restored" installed_is_prev
    expect "after the run both locks are free" locks_free
  else
    release_lock; kill "$R_PID" 2>/dev/null || true; bad "restore never waited for L3"
  fi

  # While the restore runs it holds both locks: a competitor on either fails.
  new_case holds-both
  local ready="$C/ready.fifo" go="$C/go.fifo"
  mkfifo "$ready" "$go"
  # Hook the restore with a `mv` shim that pauses at the first rename.
  mkdir -p "$C/shim"
  cat > "$C/shim/mv" <<EOF
#!/usr/bin/env bash
if [[ ! -e '$C/shim.fired' ]]; then
  : > '$C/shim.fired'
  echo reached > '$ready'
  timeout 30 cat '$go' > /dev/null
fi
exec /bin/mv "\$@"
EOF
  chmod 0755 "$C/shim/mv"
  build_script
  env --default-signal=INT "PATH=$C/shim:$PATH" bash "$RESTORE_SH" >"$C/stdout" 2>"$C/stderr" </dev/null &
  R_PID=$!
  BG_PIDS+=("$R_PID")
  if timeout 30 cat "$ready" > /dev/null; then
    expect "mid-restore: L1 is held by the restore" bash -c '! flock -n "$1" true' _ "$L1F"
    expect "mid-restore: L3 is held by the restore" bash -c '! flock -n "$1" true' _ "$L3F"
    echo go > "$go"
    finish_restore
    expect "mid-restore barrier: rc 0" test "$RC" -eq 0
    expect "mid-restore barrier: previous restored" installed_is_prev
  else
    kill "$R_PID" 2>/dev/null || true; finish_restore; bad "restore never reached the first rename"
  fi
}

main() {
  if ! command -v flock >/dev/null 2>&1; then bad "util-linux flock is required"; return; fi
  test_doc_contract
  test_good_restore
  test_refusals
  test_lock_order
}

main

echo
echo "bootstrap-restore tests: $pass passed, $fail failed"
if [[ "$fail" -ne 0 ]]; then exit 1; fi
