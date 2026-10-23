#!/usr/bin/env bash
# shellcheck disable=SC2016,SC2317,SC2329
# bootstrap-node.sh --upgrade (v0.10 spec §12 "升级模式", §11.5, §13.1-13.3).
#
# Every case runs the real script as a subprocess against temp paths:
#   - production paths and timeouts are injected through the env overrides the
#     script documents (LOCK_FILE, REALITY_LOCK_FILE, L3_WAIT, SHOW_TIMEOUT,
#     the four REALITY_* asset roots, REALITY_TXN_DIR, REALITY_TOMB_GLOB,
#     PREVIOUS_DIR, ...); version, repository and the 120 s download limit
#     stay hard-coded in the script;
#   - root / architecture / network / rename / fsync are faked with
#     executables placed first on PATH (id, uname, curl, mv, sync, rm), so
#     faults and signals are injected at exact points without test-only
#     branches in the script;
#   - the "installed" and "downloaded" proxyctl are shell stubs that record
#     every execution, so "never executed" and ordering are observable facts;
#   - locks are real flock(2) locks; barriers are FIFOs or /proc observation,
#     never sleeps used as synchronisation. Injected timeouts are <= 1 s.
# The last section runs the REAL current Go candidate against the T2 schema-1
# fixtures; it needs root plus an explicit opt-in because the Go binary reads
# fixed absolute paths (see test_real_candidate).
# Needs Linux (util-linux flock/setsid, /proc, GNU coreutils/findutils).
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
TEST_DIR="$(mktemp -d)"
HOLDER_PIDS=()
BG_PIDS=()
REAL_CREATED=()
REAL_LOCK_PREEXISTED=1
cleanup() {
  local p
  for p in "${BG_PIDS[@]:-}" "${HOLDER_PIDS[@]:-}"; do
    if [[ -n "$p" ]]; then kill "$p" 2>/dev/null || true; fi
  done
  real_cleanup
  rm -rf "$TEST_DIR"
}
trap cleanup EXIT

SCRIPT="$REPO_ROOT/bootstrap-node.sh"
SCRIPT_UNDER_TEST="$SCRIPT"
FIXED_VERSION="v0.10.0"
OLD_VERSION="v0.9.0"

pass=0
fail=0
skipped=0
ok() { echo "PASS: $1"; pass=$((pass + 1)); }
bad() { echo "FAIL: $1" >&2; fail=$((fail + 1)); }
expect() {
  local desc="$1"
  shift
  if "$@"; then ok "$desc"; else bad "$desc"; fi
}

sha_of() { sha256sum "$1" | awk '{print $1}'; }

# ---------------------------------------------------------------------------
# Stub proxyctl. Placeholders are replaced per binary.
#   version: the downloaded candidate (run from a .proxyctl.tmp.* name) prints
#     @VERSION@, the installed copy prints @INSTVER@ (differs only in the
#     "verification fails" case). STUB_VERSION_SIGNAL sends a signal to the
#     bootstrap script from inside the installed copy's `version`.
#   node service show: records the descriptors it inherited and whether it can
#     take L3 and whether a competitor can take L1, then behaves per
#     STUB_SHOW_MODE. stdout is a marker line that must be discarded.
# ---------------------------------------------------------------------------

STUB_TEMPLATE="$TEST_DIR/stub.template"
cat > "$STUB_TEMPLATE" <<'EOF'
#!/usr/bin/env bash
label='@LABEL@'
log_dir='@LOGDIR@'
{ printf '%s' "$label"; printf ' %s' "$@"; printf '\n'; } >> "$log_dir/exec.log"
if [[ "${1:-}" == version ]]; then
  if [[ "$0" == */.proxyctl.tmp.* ]]; then printf '%s\n' '@VERSION@'; exit 0; fi
  if [[ -n "${STUB_VERSION_SIGNAL:-}" && "$label" == NEW ]]; then
    . '@FAKEDIR@/lib.sh'
    send_signal "$STUB_VERSION_SIGNAL"
  fi
  printf '%s\n' '@INSTVER@'
  exit 0
fi
if [[ "${1:-}" == node && "${2:-}" == service && "${3:-}" == --help && $# -eq 3 ]]; then exit @CAP@; fi
if [[ "${1:-}" == node && "${2:-}" == service && "${3:-}" == show && $# -eq 3 ]]; then
  printf 'SECRET-STDOUT-LINE\n'
  echo 'show-diagnostic-line' >&2
  fds=""
  for f in /proc/$$/fd/*; do
    t="$(readlink "$f" 2>/dev/null || true)"
    if [[ "$t" == "${LOCK_FILE:-/nonexistent}" || "$t" == "${REALITY_LOCK_FILE:-/nonexistent}" ]]; then fds="$fds ${f##*/}"; fi
  done
  printf '%s\n' "$fds" > "$log_dir/show.lockfds"
  exec 7>"${REALITY_LOCK_FILE}"
  if flock -n 7; then echo acquired; else echo busy; fi > "$log_dir/show.l3"
  exec 7>&-
  exec 6>"${LOCK_FILE}"
  if flock -n 6; then echo free; else echo busy; fi > "$log_dir/show.l1"
  exec 6>&-
  case "${STUB_SHOW_MODE:-ok}" in
    ok) exit 0 ;;
    fail) echo 'show failed: boom' >&2; exit 3 ;;
    unknown-schema) echo 'error: unsupported manifest schema 2' >&2; exit 1 ;;
    hang)
      trap '' TERM INT
      sleep 300 &
      echo $! > "$log_dir/show.child.pid"
      echo $$ > "$log_dir/show.pid"
      wait
      exit 0
      ;;
    barrier)
      sleep 300 &
      echo $! > "$log_dir/show.child.pid"
      echo $$ > "$log_dir/show.pid"
      echo started > "$STUB_FIFO_STARTED"
      timeout 30 cat "$STUB_FIFO_GO" > /dev/null
      kill "$!" 2>/dev/null || true
      exit 0
      ;;
    mutate-content) printf 'tampered\n' >> "$STUB_MUTATE_TARGET"; exit 0 ;;
    mutate-mode) chmod 0644 "$STUB_MUTATE_TARGET"; exit 0 ;;
    create-file) : > "$STUB_MUTATE_TARGET"; exit 0 ;;
    delete-file) command rm -f "$STUB_MUTATE_TARGET"; exit 0 ;;
    replace-symlink) command rm -f "$STUB_MUTATE_TARGET"; ln -s /nonexistent-target "$STUB_MUTATE_TARGET"; exit 0 ;;
    create-txn) mkdir -p "$STUB_TXN"; exit 0 ;;
    create-tomb) mkdir -p "$STUB_TOMB"; exit 0 ;;
    create-installjson) : > "$STUB_INSTALL_JSON"; exit 0 ;;
  esac
  exit 0
fi
if [[ "${1:-}" == node ]]; then
  printf '%s\0' "$@" > "$log_dir/final.args"
  printf '%s\n' "$label" > "$log_dir/final.label"
  exit 0
fi
exit 99
EOF

# make_stub PATH VERSION CAP_RC LABEL [INSTALLED_VERSION]
make_stub() {
  mkdir -p "$(dirname "$1")"
  sed -e "s|@LABEL@|$4|" -e "s|@LOGDIR@|$C/log|" -e "s|@VERSION@|$2|" \
    -e "s|@INSTVER@|${5:-$2}|" -e "s|@CAP@|$3|" -e "s|@FAKEDIR@|$C/fake|" \
    "$STUB_TEMPLATE" > "$1"
  chmod 0755 "$1"
}

# ---------------------------------------------------------------------------
# Case fixture
# ---------------------------------------------------------------------------

CASE_N=0
new_case() {
  CASE_N=$((CASE_N + 1))
  C="$TEST_DIR/c$CASE_N-$1"
  rm -rf "$C"
  mkdir -p "$C/bin" "$C/fake" "$C/etc" "$C/var" "$C/dl" "$C/log" "$C/inst" "$C/vlib"
  BIN="$C/bin/proxyctl"
  SIDE="$BIN.sha256"
  LOCKF="$C/l1.lock"
  L3F="$C/l3.lock"
  PREV="$C/prevroot/proxyctl-bootstrap/previous"
  STATE="$C/inst/state"
  CONF="$C/inst/config"
  UNIT="$C/inst/unit/proxyctl-reality.service"
  LIB="$C/inst/lib"
  TXN="$C/vlib/proxyctl-reality-txn"
  TOMBGLOB="$C/vlib/.proxyctl-reality-txn.tomb-*"
  C_UID=0
  C_UNAME_M=x86_64
  CURL_MODE=ok
  LOCK_WAIT_VAL=2
  L3_WAIT_VAL=0.5
  SHOW_TIMEOUT_VAL=10
  EXTRA_ENV=()
  SCRIPT_UNDER_TEST="$SCRIPT"
  printf 'ID=ubuntu\nVERSION_ID="24.04"\n' > "$C/os-release"

  cat > "$C/fake/lib.sh" <<'EOF'
# script_pid: pid of the top-most bootstrap-node.sh process above this one
# (subshells share the script's command line; the main shell is the highest).
script_pid() {
  local p=$$ top="" arg1 pp
  while [[ -n "$p" && "$p" -gt 1 ]]; do
    arg1="$(tr '\0' '\n' < "/proc/$p/cmdline" 2>/dev/null | sed -n 2p || true)"
    if [[ "$arg1" == "$FAKE_SCRIPT" ]]; then
      top="$p"
    elif [[ -n "$top" ]]; then
      break
    fi
    pp="$(sed 's/^.*) //' "/proc/$p/stat" 2>/dev/null | awk '{print $2}')"
    p="$pp"
  done
  echo "$top"
}
send_signal() { # SIG
  local pid
  pid="$(script_pid)"
  if [[ -z "$pid" ]]; then echo "fake: bootstrap script pid not found" >&2; return 1; fi
  echo "$1 $pid" >> "$FAKE_LOG_DIR/signals.log"
  kill -"$1" "$pid"
}
EOF
  cat > "$C/fake/id" <<'EOF'
#!/usr/bin/env bash
if [[ "${1:-}" == -u && $# -eq 1 ]]; then echo "${FAKE_UID:-0}"; exit 0; fi
command -p id "$@"
EOF
  cat > "$C/fake/uname" <<'EOF'
#!/usr/bin/env bash
if [[ "${1:-}" == -m && $# -eq 1 ]]; then echo "${FAKE_UNAME_M:-x86_64}"; exit 0; fi
command -p uname "$@"
EOF
  # mv: records calls, observes L3 while the previous copy is written, and
  # applies FAKE_MV_RULE ("ACTION:FIELD:GLOB ..."): fail, sigcont-SIG,
  # sigfail-SIG, kill9, zero-after; FIELD is src or dest.
  cat > "$C/fake/mv" <<'EOF'
#!/usr/bin/env bash
. "$FAKE_DIR/lib.sh"
src="${*: -2:1}"
dest="${*: -1}"
printf '%s\n' "$*" >> "$FAKE_LOG_DIR/mv.log"
if [[ "$dest" == "$FAKE_PREV_DIR"/* ]]; then
  if ( exec 5>"$FAKE_L3_FILE"; flock -n 5 ); then echo free; else echo busy; fi >> "$FAKE_LOG_DIR/l3_at_prev_write.log"
fi
after=""
if [[ -n "${FAKE_MV_RULE:-}" ]]; then
  read -ra rules <<<"$FAKE_MV_RULE"
  for rule in "${rules[@]}"; do
    IFS=: read -r act field glob <<<"$rule"
    target="$src"
    if [[ "$field" == dest ]]; then target="$dest"; fi
    # shellcheck disable=SC2053
    [[ "$target" == $glob ]] || continue
    case "$act" in
      fail) echo "fake mv: injected failure for $dest" >&2; exit 1 ;;
      sigcont-*) send_signal "${act#sigcont-}" ;;
      sigfail-*) send_signal "${act#sigfail-}"; exit 1 ;;
      kill9) kill -9 "$(script_pid)"; exit 1 ;;
      zero-after) after="zero" ;;
    esac
  done
fi
command -p mv "$@"
rc=$?
if [[ "$after" == zero && $rc -eq 0 ]]; then printf '%064d\n' 0 > "$dest"; fi
exit $rc
EOF
  # sync: FAKE_SYNC_RULE="ACTION:GLOB" on the last argument: fail, sig-SIG.
  cat > "$C/fake/sync" <<'EOF'
#!/usr/bin/env bash
. "$FAKE_DIR/lib.sh"
printf '%s\n' "$*" >> "$FAKE_LOG_DIR/sync.log"
last="${*: -1}"
if [[ -n "${FAKE_SYNC_RULE:-}" ]]; then
  IFS=: read -r act glob <<<"$FAKE_SYNC_RULE"
  # shellcheck disable=SC2053
  if [[ "$last" == $glob ]]; then
    case "$act" in
      fail) exit 1 ;;
      sig-*) send_signal "${act#sig-}" ;;
    esac
  fi
fi
command -p sync "$@"
EOF
  # rm: FAKE_RM_SIGNAL=SIG fires once, on the first call that names a
  # .proxyctl.* temp file (the script's cleanup), then removes normally.
  cat > "$C/fake/rm" <<'EOF'
#!/usr/bin/env bash
. "$FAKE_DIR/lib.sh"
printf '%s\n' "$*" >> "$FAKE_LOG_DIR/rm.log"
if [[ -n "${FAKE_RM_SIGNAL:-}" && "$*" == *.proxyctl.* && ! -e "$FAKE_LOG_DIR/rm.signalled" ]]; then
  : > "$FAKE_LOG_DIR/rm.signalled"
  send_signal "$FAKE_RM_SIGNAL"
fi
command -p rm "$@"
EOF
  cat > "$C/fake/curl" <<'EOF'
#!/usr/bin/env bash
out="" url="" max=""
while [[ $# -gt 0 ]]; do
  case "$1" in
    -o) out="$2"; shift 2 ;;
    --max-time) max="$2"; shift 2 ;;
    http*) url="$1"; shift ;;
    *) shift ;;
  esac
done
printf '%s|%s|%s\n' "$url" "$max" "$(dirname "$out")" >> "$FAKE_LOG_DIR/curl.log"
name="$(basename "$url")"
case "${FAKE_CURL_MODE:-ok}" in
  fail) exit 22 ;;
  fail-binary) [[ "$name" == proxyctl-linux-* ]] && exit 22 ;;
  fail-sums) [[ "$name" == checksums.txt ]] && exit 22 ;;
  hang)
    echo $$ > "$FAKE_LOG_DIR/curl.pid"
    echo started > "$FAKE_CURL_FIFO"
    exec sleep 300
    ;;
esac
[[ -f "$FAKE_DL_DIR/$name" ]] || exit 22
cp "$FAKE_DL_DIR/$name" "$out"
EOF
  chmod 0755 "$C/fake/id" "$C/fake/uname" "$C/fake/mv" "$C/fake/sync" "$C/fake/rm" "$C/fake/curl"
}

stage_download() { # VERSION CAP_RC [INSTALLED_VERSION]
  make_stub "$C/dl/proxyctl-linux-amd64" "$1" "$2" NEW "${3:-$1}"
  cp "$C/dl/proxyctl-linux-amd64" "$C/dl/proxyctl-linux-arm64"
  write_sums "$C/dl/checksums.txt"
}

write_sums() {
  {
    printf '%s  proxyctl-linux-amd64\n' "$(sha_of "$C/dl/proxyctl-linux-amd64")"
    printf '%s  proxyctl-linux-arm64\n' "$(sha_of "$C/dl/proxyctl-linux-arm64")"
  } > "$1"
}

# make_installed VERSION CAP_RC [valid|missing|mismatch]
make_installed() {
  make_stub "$BIN" "$1" "$2" OLD
  case "${3:-valid}" in
    valid) printf '%s\n' "$(sha_of "$BIN")" > "$SIDE" ;;
    mismatch) printf '%064d\n' 0 > "$SIDE" ;;
    missing) rm -f "$SIDE" ;;
  esac
}

# make_instance: a complete-looking set of instance assets in the four roots.
make_instance() {
  mkdir -p "$STATE" "$CONF" "$(dirname "$UNIT")" "$LIB/versions/1.14.1"
  chmod 0700 "$STATE" "$CONF"
  printf '{"schema":1,"state":"installed"}\n' > "$STATE/manifest.json"
  printf '{"schema":1,"name":"reality"}\n' > "$STATE/instance.json"
  printf '{"schema":1,"uuid":"test"}\n' > "$STATE/secrets.json"
  chmod 0600 "$STATE"/*.json
  printf '{"log":{}}\n' > "$CONF/config.json"
  chmod 0600 "$CONF/config.json"
  printf '[Unit]\nDescription=test\n' > "$UNIT"
  chmod 0644 "$UNIT"
  printf '#!/bin/sh\n' > "$LIB/versions/1.14.1/sing-box"
  chmod 0755 "$LIB/versions/1.14.1/sing-box"
}

# make_root_only PATH KIND: one root present as dir | file | dangling symlink.
make_root_only() {
  local path="$1" kind="$2"
  mkdir -p "$(dirname "$path")"
  case "$kind" in
    dir) mkdir -p "$path" ;;
    file) : > "$path" ;;
    dangling) ln -s /nonexistent-target "$path" ;;
  esac
}

# tree_digest DIR: path/type/owner/mode/target of every entry plus file hashes.
tree_digest() {
  (
    cd "$1"
    find . -printf '%p|%y|%U:%G|%m|%l\n' | LC_ALL=C sort
    find . -type f -exec sha256sum {} + | LC_ALL=C sort
  ) 2>&1 | sha256sum | awk '{print $1}'
}

build_env() {
  BOOT_ENV=(
    "PATH=$C/fake:$PATH"
    "LOCK_FILE=$LOCKF"
    "PROXYCTL_BIN=$BIN"
    "INSTALL_JSON=$C/etc/install.json"
    "TOKEN_FILE=$C/var/token"
    "NODES_CONF=$C/etc/nodes.conf"
    "OS_RELEASE_FILE=$C/os-release"
    "LOCK_WAIT=$LOCK_WAIT_VAL"
    "REALITY_LOCK_FILE=$L3F"
    "L3_WAIT=$L3_WAIT_VAL"
    "SHOW_TIMEOUT=$SHOW_TIMEOUT_VAL"
    "REALITY_STATE_DIR=$STATE"
    "REALITY_CONFIG_DIR=$CONF"
    "REALITY_UNIT_FILE=$UNIT"
    "REALITY_BIN_DIR=$LIB"
    "REALITY_TXN_DIR=$TXN"
    "REALITY_TOMB_GLOB=$TOMBGLOB"
    "PREVIOUS_DIR=$PREV"
    "FAKE_UID=$C_UID"
    "FAKE_UNAME_M=$C_UNAME_M"
    "FAKE_DL_DIR=$C/dl"
    "FAKE_LOG_DIR=$C/log"
    "FAKE_CURL_MODE=$CURL_MODE"
    "FAKE_CURL_FIFO=$C/curl.fifo"
    "FAKE_DIR=$C/fake"
    "FAKE_SCRIPT=$SCRIPT_UNDER_TEST"
    "FAKE_PREV_DIR=$PREV"
    "FAKE_L3_FILE=$L3F"
    "STUB_TXN=$TXN"
    "STUB_TOMB=$C/vlib/.proxyctl-reality-txn.tomb-abc"
    "STUB_INSTALL_JSON=$C/etc/install.json"
    "STUB_FIFO_STARTED=$C/show.started"
    "STUB_FIFO_GO=$C/show.go"
  )
  if [[ ${#EXTRA_ENV[@]} -gt 0 ]]; then BOOT_ENV+=("${EXTRA_ENV[@]}"); fi
}

run_boot() {
  build_env
  set +e
  timeout 120 env "${BOOT_ENV[@]}" bash "$SCRIPT_UNDER_TEST" "$@" >"$C/stdout" 2>"$C/stderr" </dev/null
  RC=$?
  set -e
}

start_boot() {
  build_env
  # A background job of a non-interactive shell starts with SIGINT ignored, and
  # an ignored signal cannot be trapped; a terminal Ctrl-C does reach the
  # script with default disposition, so restore it for the child.
  env --default-signal=INT "${BOOT_ENV[@]}" bash "$SCRIPT_UNDER_TEST" "$@" >"$C/stdout" 2>"$C/stderr" </dev/null &
  BOOT_PID=$!
  BG_PIDS+=("$BOOT_PID")
}

finish_boot() {
  set +e
  wait "$BOOT_PID"
  RC=$?
  set -e
}

# ---------------------------------------------------------------------------
# Assertions
# ---------------------------------------------------------------------------

rc_is() { [[ "$RC" == "$1" ]]; }
stderr_has() { grep -Fq -- "$1" "$C/stderr"; }
no_curl() { [[ ! -s "$C/log/curl.log" ]]; }
no_exec() { [[ ! -s "$C/log/exec.log" ]]; }
executed() { grep -q "^$1\( \|\$\)" "$C/log/exec.log" 2>/dev/null; }
never_executed() { ! executed "$1"; }
mode_is() { [[ "$(stat -c %a "$1")" == "$2" ]]; }
bin_dir_is() { [[ "$(find "$C/bin" -mindepth 1 -maxdepth 1 -printf '%f\n' | sort | tr '\n' ' ')" == "$1" ]]; }
bin_dir_clean() { bin_dir_is "proxyctl proxyctl.sha256 "; }
prev_absent() { [[ ! -e "$PREV" ]]; }
locks_free() { flock -n "$LOCKF" true && flock -n "$L3F" true; }
sidecar_of() { tr -d '[:space:]' < "$1"; }
installed_is_old() { [[ "$(sha_of "$BIN")" == "$OLD_SHA" && "$(sidecar_of "$SIDE")" == "$OLD_SHA" ]]; }
installed_is_new() { [[ "$(sha_of "$BIN")" == "$NEW_SHA" && "$(sidecar_of "$SIDE")" == "$NEW_SHA" ]]; }
inst_unchanged() { [[ "$(tree_digest "$C/inst")" == "$INST_DIGEST" ]]; }
prev_is_old() {
  [[ -f "$PREV/proxyctl" && ! -L "$PREV/proxyctl" && "$(sha_of "$PREV/proxyctl")" == "$OLD_SHA" \
    && "$(sidecar_of "$PREV/proxyctl.sha256")" == "$OLD_SHA" ]]
}
show_executed() { grep -qx "NEW node service show" "$C/log/exec.log" 2>/dev/null; }
no_final_handover() { [[ ! -e "$C/log/final.args" ]]; }
mismatched_pair_new_bin_old_side() {
  [[ "$(sha_of "$BIN")" == "$NEW_SHA" && "$(sidecar_of "$SIDE")" == "$OLD_SHA" ]]
}

proc_alive() { # PID: exists and is not a zombie
  local st
  [[ -r "/proc/$1/stat" ]] || return 1
  st="$(sed 's/^.*) //' "/proc/$1/stat" | cut -c1)"
  [[ "$st" != Z ]]
}
wait_dead() { # PID: bounded wait for a SIGKILLed process to disappear
  local i
  for ((i = 0; i < 250; i++)); do
    if ! proc_alive "$1"; then return 0; fi
    sleep 0.02
  done
  return 1
}
no_stragglers() { # files with pids recorded by stubs
  local f pid
  for f in "$@"; do
    [[ -f "$f" ]] || continue
    pid="$(cat "$f")"
    wait_dead "$pid" || return 1
  done
}

show_failure_context() {
  echo "--- rc=$RC stderr ---" >&2
  sed 's/^/  /' "$C/stderr" >&2 || true
}

expect_rc() { # DESC RC
  if rc_is "$2"; then ok "$1"; else bad "$1 (rc=$RC, want $2)"; show_failure_context; fi
}

# A rejected / failed run before any replacement: everything is as it was.
assert_untouched() { # LABEL
  expect "$1: installed binary and sidecar unchanged" installed_is_old
  expect "$1: no temp files left in the install directory" bin_dir_clean
  expect "$1: no previous copy was created" prev_absent
  expect "$1: instance assets unchanged" inst_unchanged
  expect "$1: both locks are free" locks_free
}

assert_upgraded() { # LABEL
  expect_rc "$1: exit 0" 0
  expect "$1: binary is the new release, sidecar matches" installed_is_new
  expect "$1: binary mode 0755" mode_is "$BIN" 755
  expect "$1: sidecar mode 0644" mode_is "$SIDE" 644
  expect "$1: install directory has only the pair" bin_dir_clean
  expect "$1: previous dir is 0700" mode_is "$PREV" 700
  expect "$1: previous binary 0700" mode_is "$PREV/proxyctl" 700
  expect "$1: previous sidecar 0600" mode_is "$PREV/proxyctl.sha256" 600
  expect "$1: previous pair equals the pre-upgrade bytes" prev_is_old
  expect "$1: instance assets unchanged" inst_unchanged
  expect "$1: hint about the pinned sing-box (show)" stderr_has "sudo proxyctl node service show"
  expect "$1: hint about reinstall" stderr_has "sudo proxyctl node service reinstall"
  expect "$1: proxyctl is never exec'd as a handover" no_final_handover
  expect "$1: old binary was only asked for its version" \
    bash -c '! grep "^OLD " "$1" | grep -qv "^OLD version$"' _ "$C/log/exec.log"
  expect "$1: both locks are free" locks_free
}

snap_inst() { INST_DIGEST="$(tree_digest "$C/inst")"; }

# up_case NAME [OLD_VERSION]: an upgradable host (installed old, valid sidecar).
up_case() {
  new_case "$1"
  stage_download "$FIXED_VERSION" 0
  make_installed "${2:-$OLD_VERSION}" 0 valid
  OLD_SHA="$(sha_of "$BIN")"
  NEW_SHA="$(sha_of "$C/dl/proxyctl-linux-amd64")"
  snap_inst
}

# Lock helpers
hold_lock() { # lock file path; returns after the holder truly holds it
  mkfifo "$C/h.ready" "$C/h.release"
  (
    exec 9>"$1"
    flock 9
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
# wait_flock_child PID: block until PID has a direct child named flock, i.e.
# bootstrap is genuinely waiting for a lock.
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
fifo_wait() { timeout 30 cat "$1" > /dev/null; }

# ---------------------------------------------------------------------------
# Cases
# ---------------------------------------------------------------------------

test_script_contract() {
  new_case script-contract
  expect "script is syntactically valid" bash -n "$SCRIPT"
  expect "PROXYCTL_VERSION is a fixed literal $FIXED_VERSION" \
    grep -qx "PROXYCTL_VERSION=\"$FIXED_VERSION\"" "$SCRIPT"
  expect "version / repository / download timeout are not env-overridable" \
    bash -c '! grep -E "^(PROXYCTL_VERSION|PROXYCTL_REPOSITORY|DOWNLOAD_TIMEOUT)=\"?\\$\{" "$1"' _ "$SCRIPT"
}

# --- argv, host and role gates (before any lock or network) ------------------

test_args_and_gates() {
  local combo
  for combo in "--upgrade --name x" "--name x --upgrade" "--upgrade --replace-proxyctl" \
               "--replace-proxyctl --upgrade" "--upgrade --upgrade" "--upgrade ''"; do
    up_case "args"
    C_UID=1000
    # shellcheck disable=SC2086
    eval run_boot $combo
    expect_rc "'$combo': usage error 2 before root check" 2
    expect "'$combo': nothing downloaded, no lock file, nothing executed" \
      bash -c '[[ ! -s "$1" && ! -e "$2" && ! -s "$3" && ! -e "$4" ]]' _ "$C/log/curl.log" "$LOCKF" "$C/log/exec.log" "$L3F"
  done

  up_case gate-nonroot
  C_UID=1000
  run_boot --upgrade
  expect_rc "non-root: 1" 1
  expect "non-root: no lock, no download" bash -c '[[ ! -e "$1" && ! -s "$2" ]]' _ "$LOCKF" "$C/log/curl.log"

  up_case gate-os
  printf 'ID=fedora\nVERSION_ID="40"\n' > "$C/os-release"
  run_boot --upgrade
  expect_rc "unsupported OS: 1" 1
  expect "unsupported OS: no lock, no download" bash -c '[[ ! -e "$1" && ! -s "$2" ]]' _ "$LOCKF" "$C/log/curl.log"

  up_case gate-arch
  C_UNAME_M=riscv64
  run_boot --upgrade
  expect_rc "unsupported arch: 1" 1
  expect "unsupported arch: no lock, no download" bash -c '[[ ! -e "$1" && ! -s "$2" ]]' _ "$LOCKF" "$C/log/curl.log"

  local f
  for f in etc/install.json var/token etc/nodes.conf; do
    up_case "gate-role"
    : > "$C/$f"
    run_boot --upgrade
    expect_rc "role not 'no' ($f present): 1" 1
    expect "role not 'no' ($f): hint install-proxy.sh update" stderr_has "install-proxy.sh update"
    expect "role not 'no' ($f): no lock taken, no download" bash -c '[[ ! -e "$1" && ! -s "$2" ]]' _ "$LOCKF" "$C/log/curl.log"
    expect "role not 'no' ($f): binary unchanged" installed_is_old
  done
  up_case gate-role-dangling
  ln -s /nonexistent "$C/etc/nodes.conf"
  run_boot --upgrade
  expect_rc "role: dangling nodes.conf symlink counts as present: 1" 1
  expect "role: dangling symlink: no download" no_curl
}

# --- the local-proxyctl table ---------------------------------------------------

test_upgrade_table() {
  up_case table-missing
  rm -f "$BIN" "$SIDE"
  run_boot --upgrade
  expect_rc "proxyctl missing: 1" 1
  expect "proxyctl missing: hint to run without --upgrade" stderr_has "without --upgrade"
  expect "proxyctl missing: no download, nothing created" bash -c '[[ ! -s "$1" && ! -e "$2" ]]' _ "$C/log/curl.log" "$BIN"

  up_case table-sidecar-missing
  make_installed "$OLD_VERSION" 0 missing
  run_boot --upgrade
  expect_rc "sidecar missing: 1" 1
  expect "sidecar missing: hint --replace-proxyctl" stderr_has "--replace-proxyctl"
  expect "sidecar missing: installed binary never executed" no_exec
  expect "sidecar missing: no download" no_curl

  up_case table-sidecar-mismatch
  make_installed "$OLD_VERSION" 0 mismatch
  run_boot --upgrade
  expect_rc "sidecar mismatch: 1" 1
  expect "sidecar mismatch: hint --replace-proxyctl" stderr_has "--replace-proxyctl"
  expect "sidecar mismatch: installed binary never executed" no_exec
  expect "sidecar mismatch: no download" no_curl

  up_case table-bin-dir
  rm -f "$BIN"
  mkdir "$BIN"
  run_boot --upgrade
  expect_rc "proxyctl is a directory: 1" 1
  expect "proxyctl is a directory: no download" no_curl

  up_case table-bin-symlink
  mv "$BIN" "$BIN.real"
  ln -s "$BIN.real" "$BIN"
  run_boot --upgrade
  expect_rc "proxyctl is a symlink: 1" 1
  expect "proxyctl is a symlink: never executed, no download" bash -c '[[ ! -s "$1" && ! -s "$2" ]]' _ "$C/log/exec.log" "$C/log/curl.log"

  up_case table-sidecar-symlink
  mv "$SIDE" "$SIDE.real"
  ln -s "$SIDE.real" "$SIDE"
  run_boot --upgrade
  expect_rc "sidecar is a symlink: 1" 1
  expect "sidecar is a symlink: never executed, no download" bash -c '[[ ! -s "$1" && ! -s "$2" ]]' _ "$C/log/exec.log" "$C/log/curl.log"

  local v
  for v in dev v0.10 v1.2.3-rc1; do
    up_case "table-unparseable"
    make_installed "$v" 0 valid
    OLD_SHA="$(sha_of "$BIN")"
    run_boot --upgrade
    expect_rc "version '$v' unparseable: 1" 1
    expect "version '$v': no download" no_curl
    expect "version '$v': installed unchanged" installed_is_old
    expect "version '$v': no previous copy" prev_absent
  done

  up_case table-same
  make_installed "$FIXED_VERSION" 0 valid
  OLD_SHA="$(sha_of "$BIN")"
  make_instance
  snap_inst
  run_boot --upgrade
  expect_rc "same version: 0" 0
  expect "same version: says already at the version" stderr_has "already at $FIXED_VERSION"
  expect "same version: no download" no_curl
  expect "same version: installed unchanged" installed_is_old
  expect "same version: no previous copy" prev_absent
  expect "same version: install dir clean" bin_dir_clean
  expect "same version: only 'version' was run on the installed binary" \
    bash -c '! grep -qv "^OLD version$" "$1"' _ "$C/log/exec.log"
  expect "same version: instance assets unchanged" inst_unchanged
  expect "same version: locks free" locks_free

  up_case table-newer
  make_installed "v0.11.0" 0 valid
  OLD_SHA="$(sha_of "$BIN")"
  run_boot --upgrade
  expect_rc "newer installed: 1 (no downgrade)" 1
  expect "newer installed: says not downgraded" stderr_has "not downgraded"
  expect "newer installed: no download" no_curl
  expect "newer installed: unchanged" installed_is_old

  up_case table-newer-patch
  make_installed "v0.10.1" 0 valid
  OLD_SHA="$(sha_of "$BIN")"
  run_boot --upgrade
  expect_rc "v0.10.1 > v0.10.0: 1" 1
  expect "v0.10.1: no download" no_curl

  local ov
  for ov in v0.9.0 v0.9.9 v0.8.0; do
    up_case "table-older" "$ov"
    run_boot --upgrade
    assert_upgraded "older $ov upgrades (no instance)"
    expect "older $ov: both downloads use the 120 s limit" \
      bash -c '[[ "$(grep -c "|120|" "$1")" == 2 ]]' _ "$C/log/curl.log"
    expect "older $ov: show skipped (four roots absent)" bash -c '! grep -q " show" "$1"' _ "$C/log/exec.log"
  done

  up_case table-older-arm64
  C_UNAME_M=aarch64
  run_boot --upgrade
  assert_upgraded "arm64 host"
  expect "arm64 host: arm64 asset downloaded" grep -q "proxyctl-linux-arm64|120" "$C/log/curl.log"
}

# --- role / transaction refusals ------------------------------------------------

test_txn_and_role_refusals() {
  up_case refuse-txn
  mkdir -p "$TXN"
  run_boot --upgrade
  expect_rc "pending transaction: 1" 1
  expect "pending transaction: hint recover" stderr_has "sudo proxyctl node service recover"
  expect "pending transaction: no download" no_curl
  expect "pending transaction: unchanged" installed_is_old
  expect "pending transaction: no previous copy, clean dir" bash -c '[[ ! -e "$1" ]]' _ "$PREV"
  expect "pending transaction: locks free" locks_free

  up_case refuse-txn-dangling
  ln -s /nonexistent "$TXN"
  run_boot --upgrade
  expect_rc "pending transaction (dangling symlink): 1" 1
  expect "pending transaction (dangling): no download" no_curl

  up_case refuse-tomb
  mkdir -p "$C/vlib/.proxyctl-reality-txn.tomb-0123abcd"
  run_boot --upgrade
  expect_rc "tombstone: 1" 1
  expect "tombstone: hint recover" stderr_has "sudo proxyctl node service recover"
  expect "tombstone: no download" no_curl
  expect "tombstone: unchanged" installed_is_old
  expect "tombstone: no previous copy" prev_absent
  expect "tombstone: locks free" locks_free

  # Re-checked under the locks: the state may change while L1 is awaited.
  up_case refuse-role-under-lock
  LOCK_WAIT_VAL=20
  hold_lock "$LOCKF"
  start_boot --upgrade
  if wait_flock_child "$BOOT_PID"; then
    : > "$C/etc/install.json"
    release_lock
    finish_boot
    expect_rc "role appears while waiting for L1: 1" 1
    expect "role appears while waiting: no download" no_curl
    expect "role appears while waiting: unchanged" installed_is_old
    expect "role appears while waiting: no previous copy" prev_absent
  else
    release_lock; kill "$BOOT_PID" 2>/dev/null || true; bad "role under lock: bootstrap never waited for L1"
  fi

  up_case refuse-txn-under-lock
  LOCK_WAIT_VAL=20
  hold_lock "$LOCKF"
  start_boot --upgrade
  if wait_flock_child "$BOOT_PID"; then
    mkdir -p "$TXN"
    release_lock
    finish_boot
    expect_rc "transaction appears while waiting for L1: 1" 1
    expect "transaction appears while waiting: no download" no_curl
    expect "transaction appears while waiting: unchanged" installed_is_old
  else
    release_lock; kill "$BOOT_PID" 2>/dev/null || true; bad "txn under lock: bootstrap never waited for L1"
  fi
}

# --- candidate verification order SHA -> version -> help -> show ------------------

test_candidate_verification() {
  local lines

  up_case cand-sha-mismatch
  printf '%064d  proxyctl-linux-amd64\n' 7 > "$C/dl/checksums.txt"
  run_boot --upgrade
  expect_rc "SHA mismatch: 1" 1
  expect "SHA mismatch: candidate never executed" never_executed "NEW"
  assert_untouched "SHA mismatch"

  up_case cand-sums-missing-entry
  : > "$C/dl/checksums.txt"
  run_boot --upgrade
  expect_rc "no checksums entry: 1" 1
  expect "no checksums entry: candidate never executed" never_executed "NEW"
  assert_untouched "no checksums entry"

  up_case cand-sums-duplicate
  cat "$C/dl/checksums.txt" "$C/dl/checksums.txt" > "$C/dl/c2"
  mv "$C/dl/c2" "$C/dl/checksums.txt"
  run_boot --upgrade
  expect_rc "duplicate checksums entry: 1" 1
  assert_untouched "duplicate checksums entry"

  up_case cand-version-wrong
  stage_download "v0.10.1" 0
  run_boot --upgrade
  expect_rc "candidate version mismatch: 1" 1
  lines="$(grep '^NEW' "$C/log/exec.log" | tr '\n' ';')"
  expect "candidate version mismatch: only 'version' ran (got: $lines)" [ "$lines" = "NEW version;" ]
  assert_untouched "candidate version mismatch"

  up_case cand-help-fails
  stage_download "$FIXED_VERSION" 1
  run_boot --upgrade
  expect_rc "candidate without node service: 1" 1
  lines="$(grep '^NEW' "$C/log/exec.log" | tr '\n' ';')"
  expect "candidate help fails: version then help (got: $lines)" [ "$lines" = "NEW version;NEW node service --help;" ]
  assert_untouched "candidate help fails"

  up_case cand-show-fails
  make_instance
  snap_inst
  EXTRA_ENV=("STUB_SHOW_MODE=fail")
  run_boot --upgrade
  expect_rc "candidate show fails: 1" 1
  lines="$(grep '^NEW' "$C/log/exec.log" | tr '\n' ';')"
  expect "order SHA->version->help->show (got: $lines)" \
    [ "$lines" = "NEW version;NEW node service --help;NEW node service show;" ]
  assert_untouched "candidate show fails"

  up_case cand-order-success
  make_instance
  snap_inst
  run_boot --upgrade
  lines="$(grep '^NEW' "$C/log/exec.log" | head -3 | tr '\n' ';')"
  expect "success order: version, help, show (got: $lines)" \
    [ "$lines" = "NEW version;NEW node service --help;NEW node service show;" ]
  expect "success: the 4th NEW call is the post-install version check" \
    bash -c '[[ "$(grep "^NEW" "$1" | sed -n 4p)" == "NEW version" ]]' _ "$C/log/exec.log"
  assert_upgraded "installed-instance upgrade"

  local mode
  for mode in fail-binary fail-sums fail; do
    up_case "cand-download"
    CURL_MODE="$mode"
    run_boot --upgrade
    expect_rc "download failure ($mode): 1" 1
    expect "download failure ($mode): candidate never executed" never_executed "NEW"
    assert_untouched "download failure ($mode)"
  done
}

# --- when show is skipped and when it is not --------------------------------------

test_show_skip_matrix() {
  up_case skip-all-absent
  run_boot --upgrade
  assert_upgraded "four roots absent"
  expect "four roots absent: show never executed" bash -c '! grep -q " show" "$1"' _ "$C/log/exec.log"
  expect "four roots absent: skip reported" stderr_has "skipping the compatibility check"

  local rootvar kind
  for rootvar in STATE CONF UNIT LIB; do
    for kind in dir dangling file; do
      if [[ "$rootvar" == UNIT && "$kind" == dir ]]; then continue; fi
      up_case "skip-only"
      make_root_only "${!rootvar}" "$kind"
      snap_inst
      run_boot --upgrade
      expect "only $rootvar root present ($kind): show IS executed" show_executed
      assert_upgraded "only $rootvar root present ($kind)"
    done
  done

  up_case skip-partial-state-file
  mkdir -p "$STATE"
  : > "$STATE/secrets.json"
  snap_inst
  run_boot --upgrade
  expect "partial asset (one stray file in state dir): show executed" show_executed
  assert_upgraded "partial asset"

  up_case skip-partial-failing
  make_root_only "$UNIT" file
  snap_inst
  EXTRA_ENV=("STUB_SHOW_MODE=fail")
  run_boot --upgrade
  expect_rc "partial install with failing show: 1 (not treated as absent)" 1
  assert_untouched "partial install, failing show"

  up_case skip-stdout-discarded
  make_instance
  snap_inst
  run_boot --upgrade
  expect "show stdout is discarded" bash -c '! grep -q SECRET-STDOUT-LINE "$1" "$2"' _ "$C/stdout" "$C/stderr"
  expect "script stdout stays empty" bash -c '[[ ! -s "$1" ]]' _ "$C/stdout"
}

# --- show failures: nothing replaced ---------------------------------------------

test_show_failures() {
  local mode
  for mode in fail unknown-schema; do
    up_case "showfail"
    make_instance
    snap_inst
    EXTRA_ENV=("STUB_SHOW_MODE=$mode")
    run_boot --upgrade
    expect_rc "show ($mode): 1" 1
    expect "show ($mode): diagnostics kept on stderr" stderr_has "show-diagnostic-line"
    expect "show ($mode): stdout discarded" bash -c '! grep -q SECRET-STDOUT-LINE "$1" "$2"' _ "$C/stdout" "$C/stderr"
    expect "show ($mode): failure explained" stderr_has "Nothing was replaced"
    assert_untouched "show ($mode)"
  done

  up_case showfail-diag-text
  make_instance
  EXTRA_ENV=("STUB_SHOW_MODE=fail")
  run_boot --upgrade
  expect "show failure: the candidate's own error line is preserved" stderr_has "show failed: boom"

  up_case showfail-hang
  make_instance
  snap_inst
  SHOW_TIMEOUT_VAL=0.5
  EXTRA_ENV=("STUB_SHOW_MODE=hang")
  run_boot --upgrade
  expect_rc "show hangs: killed at the injected timeout, 1" 1
  expect "show hangs: timeout reported" stderr_has "did not finish within 0.5s"
  expect "show hangs: stub and its child are dead (reaped before unlock)" \
    no_stragglers "$C/log/show.pid" "$C/log/show.child.pid"
  assert_untouched "show hang"
}

# --- fds, lock handover and ordering (real flock) ----------------------------------

test_lock_handover() {
  up_case lock-handover
  make_instance
  snap_inst
  mkfifo "$C/show.started" "$C/show.go"
  EXTRA_ENV=("STUB_SHOW_MODE=barrier")
  start_boot --upgrade
  if fifo_wait "$C/show.started"; then
    expect "while show runs: competitor flock -n on L1 fails (parent keeps L1)" \
      bash -c '! flock -n "$1" true' _ "$LOCKF"
    expect "while show runs: parent no longer holds L3 (competitor can take it)" \
      bash -c 'flock -n "$1" true' _ "$L3F"
    echo go > "$C/show.go"
    finish_boot
  else
    kill "$BOOT_PID" 2>/dev/null || true; finish_boot; bad "show never started"
  fi
  expect "show inherited neither fd 9 (L1) nor fd 8 (L3)" bash -c '[[ -z "$(tr -d "[:space:]" < "$1")" ]]' _ "$C/log/show.lockfds"
  expect "show could take L3 itself" grep -qx acquired "$C/log/show.l3"
  expect "show saw L1 busy (parent holds it)" grep -qx busy "$C/log/show.l1"
  expect "previous copy written only while the parent holds L3 again" \
    bash -c '[[ -s "$1" ]] && ! grep -qx free "$1"' _ "$C/log/l3_at_prev_write.log"
  assert_upgraded "lock handover"

  up_case lock-retake-blocked
  make_instance
  snap_inst
  mkfifo "$C/show.started" "$C/show.go"
  L3_WAIT_VAL=0.3
  EXTRA_ENV=("STUB_SHOW_MODE=barrier")
  start_boot --upgrade
  if fifo_wait "$C/show.started"; then
    hold_lock "$L3F"
    echo go > "$C/show.go"
    finish_boot
    release_lock
    expect_rc "L3 cannot be re-taken after show: 1" 1
    expect "re-take failure explained" stderr_has "Could not take the Reality instance lock"
    expect "re-take failure: installed unchanged" installed_is_old
    expect "re-take failure: no previous copy" prev_absent
    expect "re-take failure: install dir clean" bin_dir_clean
    expect "re-take failure: instance assets unchanged" inst_unchanged
    expect "re-take failure: locks free" locks_free
  else
    kill "$BOOT_PID" 2>/dev/null || true; finish_boot; bad "show never started"
  fi

  up_case lock-l1-held
  LOCK_WAIT_VAL=0.3
  hold_lock "$LOCKF"
  run_boot --upgrade
  release_lock
  expect_rc "L1 held by another: waits then 1" 1
  expect "L1 held: no download" no_curl
  expect "L1 held: unchanged" installed_is_old
  expect "L1 held: L3 was never taken" bash -c 'flock -n "$1" true' _ "$L3F"

  up_case lock-l3-held
  L3_WAIT_VAL=0.3
  hold_lock "$L3F"
  run_boot --upgrade
  release_lock
  expect_rc "L3 held by another: waits then 1" 1
  expect "L3 held: no download" no_curl
  expect "L3 held: unchanged" installed_is_old
  expect "L3 held: L1 released again" bash -c 'flock -n "$1" true' _ "$LOCKF"
}

# --- state changes while the check runs --------------------------------------------

test_change_during_check() {
  local case_name mode target spec
  # name|show-mode|target relative to the case dir
  local specs=(
    "content|mutate-content|inst/state/manifest.json"
    "mode|mutate-mode|inst/state/secrets.json"
    "create|create-file|inst/config/extra.json"
    "delete|delete-file|inst/config/config.json"
    "symlink|replace-symlink|inst/unit/proxyctl-reality.service"
    "libfile|create-file|inst/lib/versions/1.14.1/extra"
  )
  for spec in "${specs[@]}"; do
    IFS='|' read -r case_name mode target <<<"$spec"
    up_case "change"
    make_instance
    snap_inst
    EXTRA_ENV=("STUB_SHOW_MODE=$mode" "STUB_MUTATE_TARGET=$C/$target")
    run_boot --upgrade
    expect_rc "asset $case_name changed by show: 1" 1
    expect "asset $case_name changed: reported" stderr_has "assets changed during the compatibility check"
    expect "asset $case_name changed: installed unchanged" installed_is_old
    expect "asset $case_name changed: no previous copy" prev_absent
    expect "asset $case_name changed: install dir clean" bin_dir_clean
    expect "asset $case_name changed: locks free" locks_free
  done

  up_case change-txn
  make_instance
  EXTRA_ENV=("STUB_SHOW_MODE=create-txn")
  run_boot --upgrade
  expect_rc "transaction zone created during check: 1" 1
  expect "transaction created: hint recover" stderr_has "recover"
  expect "transaction created: no previous copy" prev_absent
  expect "transaction created: installed unchanged" installed_is_old

  up_case change-tomb
  make_instance
  EXTRA_ENV=("STUB_SHOW_MODE=create-tomb")
  run_boot --upgrade
  expect_rc "tombstone created during check: 1" 1
  expect "tombstone created: no previous copy" prev_absent
  expect "tombstone created: installed unchanged" installed_is_old

  up_case change-role
  make_instance
  EXTRA_ENV=("STUB_SHOW_MODE=create-installjson")
  run_boot --upgrade
  expect_rc "role flips to center during check: 1" 1
  expect "role flip: no previous copy" prev_absent
  expect "role flip: installed unchanged" installed_is_old
  expect "role flip: no temp files" bin_dir_clean
}

# --- previous copy ------------------------------------------------------------------

test_previous_copy() {
  up_case prev-overwrites-older
  mkdir -p "$PREV"
  chmod 0700 "$PREV"
  printf 'older binary\n' > "$PREV/proxyctl"
  printf '%064d\n' 1 > "$PREV/proxyctl.sha256"
  run_boot --upgrade
  assert_upgraded "earlier previous copy is overwritten (latest only)"
  expect "previous dir holds only the pair" \
    bash -c '[[ "$(find "$1" -mindepth 1 -printf "%f\n" | sort | tr "\n" " ")" == "proxyctl proxyctl.sha256 " ]]' _ "$PREV"

  up_case prev-dir-loose-mode
  mkdir -p "$PREV"
  chmod 0755 "$PREV"
  run_boot --upgrade
  assert_upgraded "pre-existing loose previous dir is tightened"

  up_case prev-dir-is-file
  mkdir -p "$(dirname "$PREV")"
  : > "$PREV"
  run_boot --upgrade
  expect_rc "previous path is a file: 1" 1
  expect "previous path is a file: installed unchanged" installed_is_old
  expect "previous path is a file: install dir clean" bin_dir_clean

  up_case prev-dir-is-symlink
  mkdir -p "$(dirname "$PREV")" "$C/elsewhere"
  ln -s "$C/elsewhere" "$PREV"
  run_boot --upgrade
  expect_rc "previous dir is a symlink: 1" 1
  expect "previous dir symlink: nothing written through it" \
    bash -c '[[ -z "$(ls -A "$1")" ]]' _ "$C/elsewhere"
  expect "previous dir symlink: installed unchanged" installed_is_old

  up_case prev-sidecar-is-symlink
  mkdir -p "$PREV"
  ln -s /nonexistent "$PREV/proxyctl.sha256"
  run_boot --upgrade
  expect_rc "previous sidecar is a symlink: 1" 1
  expect "previous sidecar symlink: installed unchanged" installed_is_old

  # Failure between the two previous-copy files: current install untouched.
  up_case prev-fail-between-files
  make_instance
  snap_inst
  EXTRA_ENV=("FAKE_MV_RULE=fail:dest:$PREV/proxyctl.sha256")
  run_boot --upgrade
  expect_rc "previous-copy save fails between the two files: 1" 1
  expect "save fails between files: installed pair unchanged" installed_is_old
  expect "save fails between files: no temp files in the install dir" bin_dir_clean
  expect "save fails between files: no temp files in the previous dir" \
    bash -c '! find "$1" -name ".proxyctl.*" | grep -q .' _ "$PREV"
  expect "save fails between files: the installed pair was never renamed" \
    bash -c '! grep -q " $2\$" "$1" && ! grep -q " $3\$" "$1"' _ "$C/log/mv.log" "$BIN" "$SIDE"
  expect "save fails between files: instance assets unchanged" inst_unchanged
  expect "save fails between files: locks free" locks_free

  # The pair is verified after it was written: a copy that does not match
  # (bit rot, torn write) stops the upgrade before the first rename.
  local victim
  for victim in proxyctl proxyctl.sha256; do
    up_case "prev-verify-$victim"
    EXTRA_ENV=("FAKE_MV_RULE=zero-after:dest:$PREV/$victim")
    run_boot --upgrade
    expect_rc "written previous $victim does not match: 1" 1
    expect "written previous $victim mismatch: reported" stderr_has "does not match the installed pair"
    expect "written previous $victim mismatch: installed unchanged" installed_is_old
    expect "written previous $victim mismatch: install dir clean" bin_dir_clean
    expect "written previous $victim mismatch: installed pair never renamed" \
      bash -c '! grep -q " $2\$" "$1" && ! grep -q " $3\$" "$1"' _ "$C/log/mv.log" "$BIN" "$SIDE"
  done

  up_case prev-fail-first-file
  EXTRA_ENV=("FAKE_MV_RULE=fail:dest:$PREV/proxyctl")
  run_boot --upgrade
  expect_rc "previous-copy save fails on the first file: 1" 1
  expect "save fails on first file: installed unchanged" installed_is_old
  expect "save fails on first file: install dir clean" bin_dir_clean

  up_case prev-fail-fsync
  EXTRA_ENV=("FAKE_SYNC_RULE=fail:$PREV")
  run_boot --upgrade
  expect_rc "directory fsync of the previous dir fails: 1" 1
  expect "fsync fails: installed unchanged" installed_is_old
  expect "fsync fails: install dir clean" bin_dir_clean

  up_case prev-fsync-calls
  run_boot --upgrade
  expect "files and directory are fsynced (sync -- file / sync -- dir)" \
    bash -c 'grep -q "^-- $2/\.proxyctl\.tmp\." "$1" && grep -qx -- "-- $2" "$1"' _ "$C/log/sync.log" "$PREV"

  # A mismatched earlier previous pair never blocks a later upgrade.
  up_case prev-mismatched-leftover
  mkdir -p "$PREV"
  chmod 0700 "$PREV"
  printf 'junk\n' > "$PREV/proxyctl"
  printf '%064d\n' 9 > "$PREV/proxyctl.sha256"
  run_boot --upgrade
  assert_upgraded "mismatched earlier previous pair is replaced by a valid one"
}

# --- replacement faults ---------------------------------------------------------------

test_replacement_faults() {
  local side_tmp='.proxyctl.sha256.tmp.*' bin_tmp='.proxyctl.tmp.*' restore_tmp='.proxyctl.restore.*'

  up_case rep-second-rename-fails
  make_instance
  snap_inst
  EXTRA_ENV=("FAKE_MV_RULE=fail:src:$C/bin/$side_tmp")
  run_boot --upgrade
  expect_rc "sidecar rename fails: restored, 4" 4
  expect "sidecar rename fails: old pair back" installed_is_old
  expect "sidecar rename fails: previous-copy path printed" stderr_has "$PREV"
  expect "sidecar rename fails: install dir clean" bin_dir_clean
  expect "sidecar rename fails: previous copy intact" prev_is_old
  expect "sidecar rename fails: instance assets unchanged" inst_unchanged
  expect "sidecar rename fails: locks free" locks_free
  expect "sidecar rename fails: the new binary really had been renamed in first" \
    bash -c 'grep -q "^-f .*/\.proxyctl\.tmp\.[^ ]* $2\$" "$1"' _ "$C/log/mv.log" "$BIN"

  up_case rep-second-rename-and-restore-fail
  EXTRA_ENV=("FAKE_MV_RULE=fail:src:$C/bin/$side_tmp fail:src:$C/bin/$restore_tmp")
  run_boot --upgrade
  expect_rc "restore also fails: 5" 5
  expect "restore fails: previous-copy path printed" stderr_has "$PREV"
  expect "restore fails: states how to repair" stderr_has "--replace-proxyctl"
  expect "restore fails: binary is new, sidecar still old (mismatched pair)" mismatched_pair_new_bin_old_side
  expect "restore fails: previous copy intact" prev_is_old
  expect "restore fails: install dir has no leftover temps" \
    bash -c '! find "$1" -name ".proxyctl.*" | grep -q .' _ "$C/bin"
  expect "restore fails: locks free" locks_free
  # The mismatched pair left behind is refused by a plain run (never executed).
  : > "$C/log/exec.log"
  EXTRA_ENV=()
  run_boot
  expect_rc "plain run on the mismatched pair: 1" 1
  expect "plain run: installed (new) binary was NOT executed" no_exec
  expect "plain run: points at --replace-proxyctl" stderr_has "--replace-proxyctl"

  up_case rep-first-rename-fails
  EXTRA_ENV=("FAKE_MV_RULE=fail:src:$C/bin/$bin_tmp")
  run_boot --upgrade
  expect_rc "binary rename fails: restored state verified, 4" 4
  expect "binary rename fails: old pair" installed_is_old
  expect "binary rename fails: install dir clean" bin_dir_clean

  up_case rep-verify-hash
  EXTRA_ENV=("FAKE_MV_RULE=zero-after:src:$C/bin/$side_tmp")
  run_boot --upgrade
  expect_rc "post-replace sidecar mismatch: restored, 4" 4
  expect "post-replace sidecar mismatch: old pair back" installed_is_old
  expect "post-replace sidecar mismatch: previous path printed" stderr_has "$PREV"
  expect "post-replace sidecar mismatch: install dir clean" bin_dir_clean

  up_case rep-verify-version
  stage_download "$FIXED_VERSION" 0 "v9.9.9"
  NEW_SHA="$(sha_of "$C/dl/proxyctl-linux-amd64")"
  run_boot --upgrade
  expect_rc "installed binary reports the wrong version: restored, 4" 4
  expect "wrong version after install: old pair back" installed_is_old
  expect "wrong version after install: install dir clean" bin_dir_clean
  expect "wrong version after install: previous copy intact" prev_is_old

  # SIGKILL between the two renames: mismatched pair, plain run refuses,
  # --replace-proxyctl of the SAME release repairs both files.
  up_case rep-sigkill
  make_instance
  snap_inst
  EXTRA_ENV=("FAKE_MV_RULE=kill9:src:$C/bin/$side_tmp")
  run_boot --upgrade
  expect_rc "SIGKILL between the two renames: 137" 137
  expect "SIGKILL: binary is new, sidecar still old" mismatched_pair_new_bin_old_side
  expect "SIGKILL: previous copy complete" prev_is_old
  : > "$C/log/exec.log"
  EXTRA_ENV=()
  run_boot
  expect_rc "plain run after SIGKILL: refuses (1)" 1
  expect "plain run after SIGKILL: mismatched binary not executed" no_exec
  run_boot --upgrade
  expect_rc "--upgrade after SIGKILL: refuses (1)" 1
  expect "--upgrade after SIGKILL: mismatched binary not executed" no_exec
  expect "--upgrade after SIGKILL: points at --replace-proxyctl" stderr_has "--replace-proxyctl"
  run_boot --replace-proxyctl --name n --server 1.2.3.4
  expect_rc "--replace-proxyctl repairs and hands over (final command rc 0)" 0
  expect "--replace-proxyctl repair: both files consistent and new" installed_is_new
  expect "--replace-proxyctl repair: instance assets unchanged" inst_unchanged
  expect "--replace-proxyctl repair: handed over to node service install" \
    bash -c 'grep -qx NEW "$1/final.label" && [[ "$(tr "\0" " " < "$1/final.args")" == "node service install --name n --server 1.2.3.4 " ]]' _ "$C/log"

  up_case rep-sigkill-nontty
  make_instance
  snap_inst
  EXTRA_ENV=("FAKE_MV_RULE=kill9:src:$C/bin/$side_tmp")
  run_boot --upgrade
  expect_rc "SIGKILL (second scenario): 137" 137
  EXTRA_ENV=()
  run_boot --replace-proxyctl
  expect_rc "installed instance + non-TTY, no install args: usage 2 (asserted separately)" 2
  expect "non-TTY: install files nevertheless repaired (separate assertion)" installed_is_new
  expect "non-TTY: usage printed" stderr_has "usage:"
  expect "non-TTY: instance assets unchanged" inst_unchanged
  expect "non-TTY: no node command was run" no_final_handover
  expect "non-TTY: locks free" locks_free
}

# --- signals ------------------------------------------------------------------------------

# check_signal_outcome LABEL EXPECTED_RC OLD|NEW
check_signal_outcome() {
  local label="$1" want_rc="$2" want_state="$3"
  expect_rc "$label: $want_rc" "$want_rc"
  if [[ "$want_state" == OLD ]]; then
    expect "$label: final bytes = pre-upgrade bytes" installed_is_old
  else
    expect "$label: new release stays installed, no rollback" installed_is_new
  fi
  expect "$label: no temp files left" bin_dir_clean
  expect "$label: locks released" locks_free
  expect "$label: instance assets unchanged" inst_unchanged
  expect "$label: no stray stub/curl processes" no_stragglers "$C/log/show.pid" "$C/log/show.child.pid" "$C/log/curl.pid"
}

test_signal_matrix() {
  local sig side_tmp='.proxyctl.sha256.tmp.*' restore_tmp='.proxyctl.restore.*'

  for sig in INT TERM; do
    # during the L1 wait
    up_case "sig-l1-wait"
    LOCK_WAIT_VAL=20
    hold_lock "$LOCKF"
    start_boot --upgrade
    if wait_flock_child "$BOOT_PID"; then
      kill -"$sig" "$BOOT_PID"
      finish_boot
      release_lock
      expect_rc "$sig during L1 wait: 1" 1
      expect "$sig during L1 wait: no download" no_curl
      expect "$sig during L1 wait: unchanged" installed_is_old
    else
      release_lock; kill "$BOOT_PID" 2>/dev/null || true; bad "$sig l1 wait: never waited"
    fi

    # during the L3 wait
    up_case "sig-l3-wait"
    L3_WAIT_VAL=20
    hold_lock "$L3F"
    start_boot --upgrade
    if wait_flock_child "$BOOT_PID"; then
      kill -"$sig" "$BOOT_PID"
      finish_boot
      release_lock
      expect_rc "$sig during L3 wait: 1" 1
      expect "$sig during L3 wait: L1 released" bash -c 'flock -n "$1" true' _ "$LOCKF"
      expect "$sig during L3 wait: unchanged" installed_is_old
    else
      release_lock; kill "$BOOT_PID" 2>/dev/null || true; bad "$sig l3 wait: never waited"
    fi

    # during download
    up_case "sig-download"
    make_instance
    snap_inst
    mkfifo "$C/curl.fifo"
    CURL_MODE=hang
    start_boot --upgrade
    if fifo_wait "$C/curl.fifo"; then
      kill -"$sig" "$BOOT_PID"
      finish_boot
      check_signal_outcome "$sig during download" 1 OLD
      expect "$sig during download: no previous copy" prev_absent
    else
      kill "$BOOT_PID" 2>/dev/null || true; finish_boot; bad "$sig download: curl never started"
    fi

    # during the compatibility check (show running)
    up_case "sig-show"
    make_instance
    snap_inst
    mkfifo "$C/show.started" "$C/show.go"
    EXTRA_ENV=("STUB_SHOW_MODE=barrier")
    start_boot --upgrade
    if fifo_wait "$C/show.started"; then
      kill -"$sig" "$BOOT_PID"
      finish_boot
      check_signal_outcome "$sig during show" 1 OLD
      expect "$sig during show: no previous copy" prev_absent
    else
      kill "$BOOT_PID" 2>/dev/null || true; finish_boot; bad "$sig show: never started"
    fi

    # during the previous-copy save (signal arrives while the second file is renamed)
    up_case "sig-prevsave"
    make_instance
    snap_inst
    EXTRA_ENV=("FAKE_MV_RULE=sigcont-$sig:dest:$PREV/proxyctl.sha256")
    run_boot --upgrade
    check_signal_outcome "$sig during previous-copy save" 1 OLD
    expect "$sig during previous-copy save: no temp left in previous dir" \
      bash -c '! find "$1" -name ".proxyctl.*" 2>/dev/null | grep -q .' _ "$PREV"

    # just before the first rename (during the final fsync of the previous dir)
    up_case "sig-prerename"
    make_instance
    snap_inst
    EXTRA_ENV=("FAKE_SYNC_RULE=sig-$sig:$PREV")
    run_boot --upgrade
    check_signal_outcome "$sig before the first rename" 1 OLD
    expect "$sig before the first rename: installed pair never renamed" \
      bash -c '! grep -q " $2\$" "$1"' _ "$C/log/mv.log" "$BIN"

    # between the two renames: signal while the sidecar rename runs
    up_case "sig-between"
    make_instance
    snap_inst
    EXTRA_ENV=("FAKE_MV_RULE=sigcont-$sig:src:$C/bin/$side_tmp")
    run_boot --upgrade
    check_signal_outcome "$sig between the two renames" 4 OLD
    expect "$sig between the two renames: previous copy kept" prev_is_old

    # between the two renames, and the second rename does not happen at all
    up_case "sig-between-lost"
    make_instance
    snap_inst
    EXTRA_ENV=("FAKE_MV_RULE=sigfail-$sig:src:$C/bin/$side_tmp")
    run_boot --upgrade
    check_signal_outcome "$sig between the renames (sidecar rename lost)" 4 OLD

    # during verification (inside the installed binary's `version`)
    up_case "sig-verify"
    make_instance
    snap_inst
    EXTRA_ENV=("STUB_VERSION_SIGNAL=$sig")
    run_boot --upgrade
    check_signal_outcome "$sig during verification" 4 OLD
    expect "$sig during verification: previous copy kept" prev_is_old
    expect "$sig during verification: signal was really delivered" grep -q "^$sig " "$C/log/signals.log"

    # after the success boundary (signal during the final cleanup)
    up_case "sig-after"
    make_instance
    snap_inst
    EXTRA_ENV=("FAKE_RM_SIGNAL=$sig")
    run_boot --upgrade
    check_signal_outcome "$sig after the success boundary" 0 NEW
    expect "$sig after the boundary: signal was really delivered" grep -q "^$sig " "$C/log/signals.log"
    expect "$sig after the boundary: no rollback rename" \
      bash -c '! grep -q "\.proxyctl\.restore\." "$1"' _ "$C/log/mv.log"
    expect "$sig after the boundary: previous copy kept" prev_is_old
    expect "$sig after the boundary: success reported" stderr_has "Upgraded proxyctl"
  done

  # A second signal during the rollback is ignored: the rollback completes.
  up_case sig-twice
  make_instance
  snap_inst
  EXTRA_ENV=("FAKE_MV_RULE=sigcont-TERM:src:$C/bin/$side_tmp sigcont-INT:src:$C/bin/$restore_tmp")
  run_boot --upgrade
  check_signal_outcome "second signal during the rollback" 4 OLD
}

# --- Task 13 follow-ups: setsid --wait and stale previous-dir temp files -------------------

# A fake `setsid` that behaves as util-linux does when its caller is a process
# group leader: without --wait it forks the real setsid into the background and
# returns 0 at once (so the caller cannot see the child's exit status); with
# --wait it execs the real one, which waits and returns the child's status.
write_leader_setsid() {
  cat > "$C/fake/setsid" <<'EOF'
#!/usr/bin/env bash
printf '%s\n' "$*" >> "$FAKE_LOG_DIR/setsid.log"
real="$(PATH=/usr/bin:/bin:/usr/sbin:/sbin command -v setsid)"
if [[ "${1:-}" == --wait || "${1:-}" == -w ]]; then exec "$real" "$@"; fi
"$real" "$@" </dev/null >/dev/null 2>&1 &
exit 0
EOF
  chmod 0755 "$C/fake/setsid"
}

test_setsid_wait() {
  up_case setsid-wait-failing-show
  make_instance
  snap_inst
  write_leader_setsid
  EXTRA_ENV=("STUB_SHOW_MODE=fail")
  run_boot --upgrade
  expect_rc "setsid as a group leader + failing show: the failure is seen (rc 1)" 1
  expect "setsid as a group leader + failing show: reported as the compatibility check failing" stderr_has "failed with exit code 3"
  expect "setsid as a group leader + failing show: installed pair unchanged" installed_is_old
  expect "setsid as a group leader + failing show: no previous copy" prev_absent
  expect "the check runs through 'setsid --wait timeout'" \
    bash -c 'grep -q "^--wait timeout " "$1"' _ "$C/log/setsid.log"

  up_case setsid-wait-ok-show
  make_instance
  snap_inst
  write_leader_setsid
  run_boot --upgrade
  assert_upgraded "setsid as a group leader + passing show"
  expect "passing show also ran through 'setsid --wait'" \
    bash -c 'grep -q "^--wait timeout " "$1"' _ "$C/log/setsid.log"
}

test_stale_previous_tmp() {
  local other_uid_file="" before_link_target
  up_case prev-stale-tmp
  make_instance
  snap_inst
  mkdir -p "$PREV" "$C/outside"
  chmod 0700 "$PREV"
  printf 'stale-1\n' > "$PREV/.proxyctl.tmp.AAAAAA"
  printf 'stale-2\n' > "$PREV/.proxyctl.tmp.BBBBBB"
  printf 'outside-secret\n' > "$C/outside/target"
  before_link_target="$(sha_of "$C/outside/target")"
  ln -s "$C/outside/target" "$PREV/.proxyctl.tmp.LINK"
  ln -s /nonexistent-stale-target "$PREV/.proxyctl.tmp.DANGLING"
  mkdir -p "$PREV/.proxyctl.tmp.DIR"
  printf 'keep\n' > "$PREV/.proxyctl.tmp.DIR/inner"
  printf 'unrelated\n' > "$PREV/.proxyctl.other"
  printf 'unrelated\n' > "$PREV/proxyctl.tmp.NODOT"
  printf 'unrelated\n' > "$PREV/.proxyctl.tm"
  if [[ "$(command -p id -u)" == 0 ]]; then
    other_uid_file="$PREV/.proxyctl.tmp.OTHERUID"
    printf 'not-mine\n' > "$other_uid_file"
    chown 12345:12345 "$other_uid_file"
  fi
  run_boot --upgrade
  assert_upgraded "stale temp files in the previous dir"
  expect "stale regular temp files are removed" \
    bash -c '[[ ! -e "$1/.proxyctl.tmp.AAAAAA" && ! -e "$1/.proxyctl.tmp.BBBBBB" ]]' _ "$PREV"
  expect "a symlink named like a temp file is kept as a symlink" \
    bash -c '[[ -L "$1/.proxyctl.tmp.LINK" && "$(readlink "$1/.proxyctl.tmp.LINK")" == "$2" ]]' _ "$PREV" "$C/outside/target"
  expect "the symlink target outside the previous dir is untouched" \
    bash -c '[[ "$(sha256sum < "$1" | cut -d" " -f1)" == "$2" ]]' _ "$C/outside/target" "$before_link_target"
  expect "a dangling symlink named like a temp file is kept" \
    bash -c '[[ -L "$1/.proxyctl.tmp.DANGLING" ]]' _ "$PREV"
  expect "a directory named like a temp file and its content are kept" \
    bash -c '[[ -d "$1/.proxyctl.tmp.DIR" && "$(cat "$1/.proxyctl.tmp.DIR/inner")" == keep ]]' _ "$PREV"
  expect "nothing outside the matching regular files was even attempted (no removal warning)" \
    bash -c '! grep -q "could not remove" "$1"' _ "$C/stderr"
  expect "entries that do not match the temp name are kept" \
    bash -c '[[ -f "$1/.proxyctl.other" && -f "$1/proxyctl.tmp.NODOT" && -f "$1/.proxyctl.tm" ]]' _ "$PREV"
  if [[ -n "$other_uid_file" ]]; then
    expect "a temp-named file owned by another user is kept (root run)" \
      bash -c '[[ -f "$1" && "$(stat -c %u "$1")" == 12345 ]]' _ "$other_uid_file"
  else
    echo "NOTE: the other-owner temp-file case needs root to create a foreign-owned file; not run as uid $(command -p id -u)"
  fi

  # Stale cleanup must not weaken the existing guarantee: a failure between
  # the two previous-copy files still leaves the installed pair untouched.
  up_case prev-stale-tmp-then-fail-between-files
  make_instance
  snap_inst
  mkdir -p "$PREV"
  chmod 0700 "$PREV"
  printf 'stale\n' > "$PREV/.proxyctl.tmp.CCCCCC"
  EXTRA_ENV=("FAKE_MV_RULE=fail:dest:$PREV/proxyctl.sha256")
  run_boot --upgrade
  expect_rc "stale temp + save fails between the two files: 1" 1
  expect "stale temp + save fails between files: installed pair unchanged" installed_is_old
  expect "stale temp + save fails between files: install dir clean" bin_dir_clean
  expect "stale temp + save fails between files: the stale file was cleaned before the save" \
    bash -c '[[ ! -e "$1/.proxyctl.tmp.CCCCCC" ]]' _ "$PREV"
  expect "stale temp + save fails between files: installed pair never renamed" \
    bash -c '! grep -q " $2\$" "$1" && ! grep -q " $3\$" "$1"' _ "$C/log/mv.log" "$BIN" "$SIDE"
}

# --- script-level properties that tests cannot observe from outside -----------------------

test_script_structure() {
  new_case structure
  expect "the compatibility check child gets fds 8 and 9 closed" \
    bash -c 'grep -Eq "setsid( --wait)? timeout .*node service show.*8>&- 9>&-" "$1"' _ "$SCRIPT"
  expect "L3 is re-taken after the check and the asset snapshot is compared" \
    bash -c 'grep -q "acquire_l3 \"after the compatibility check\"" "$1" && grep -q "\"\$after\" != \"\$before\"" "$1"' _ "$SCRIPT"
}

# --- real current Go candidate against the T2 schema-1 fixtures -----------------------------
#
# The Go binary has no path injection: `node service show` reads the fixed
# absolute production paths. So this section only runs when ALL of these hold
#   - PROXYCTL_REAL_FS_TESTS=1 (explicit opt-in; use a throw-away container),
#   - euid 0 (the fixtures need root:root ownership and `show` requires root),
#   - a candidate is available: PROXYCTL_REAL_CANDIDATE=/path/to/proxyctl
#     (built with -ldflags "-X main.Version=v0.10.0") or `go` is on PATH,
#   - none of the real paths exist (nothing of the host is ever touched).
# Without the opt-in it prints SKIP. With the opt-in and a missing prerequisite
# it FAILS loudly. Everything it creates is removed by the EXIT cleanup. `show`
# only reads files and never calls systemctl, so no fake systemctl is needed.

REAL_ROOTS=(/var/lib/proxyctl-reality /etc/proxyctl-reality /etc/systemd/system/proxyctl-reality.service /usr/local/lib/proxyctl-reality)
REAL_EXTRA=(/var/lib/proxyctl-reality-txn)

real_cleanup() {
  local p
  for p in "${REAL_ROOTS[@]}"; do
    if [[ " ${REAL_CREATED[*]:-} " == *" $p "* ]]; then rm -rf -- "$p"; fi
  done
  REAL_CREATED=()
}

real_materialise() { # installed|uninstalled
  local src="$REPO_ROOT/internal/realitynode/testdata/v0.10/$1" rel p
  real_cleanup
  REAL_CREATED=("${REAL_ROOTS[@]}")
  while IFS= read -r rel; do
    rel="${rel#./}"
    [[ -n "$rel" && "$rel" != . ]] || continue
    if [[ -d "$src/$rel" ]]; then
      mkdir -p "/$rel"
    else
      mkdir -p "$(dirname "/$rel")"
      cp "$src/$rel" "/$rel"
    fi
  done < <(cd "$src" && find . | LC_ALL=C sort)
  # Exact owner / mode of spec §5.1 (git does not keep them).
  for p in "${REAL_ROOTS[@]}"; do
    if [[ -e "$p" ]]; then chown -R 0:0 "$p"; fi
  done
  if [[ -d /var/lib/proxyctl-reality ]]; then
    chmod 0700 /var/lib/proxyctl-reality
    chmod 0600 /var/lib/proxyctl-reality/*.json
  fi
  if [[ -d /etc/proxyctl-reality ]]; then
    chmod 0700 /etc/proxyctl-reality
    chmod 0600 /etc/proxyctl-reality/config.json
  fi
  if [[ -f /etc/systemd/system/proxyctl-reality.service ]]; then
    chmod 0644 /etc/systemd/system/proxyctl-reality.service
  fi
  if [[ -d /usr/local/lib/proxyctl-reality ]]; then
    chmod 0755 /usr/local/lib/proxyctl-reality /usr/local/lib/proxyctl-reality/versions /usr/local/lib/proxyctl-reality/versions/1.14.1
    chmod 0755 /usr/local/lib/proxyctl-reality/versions/1.14.1/sing-box
  fi
}

real_digest() {
  local r
  for r in "${REAL_ROOTS[@]}"; do
    if [[ ! -e "$r" && ! -L "$r" ]]; then echo "ABSENT $r"; continue; fi
    find -P "$r" -printf '%p|%y|%U:%G|%m|%l\n' | LC_ALL=C sort
    find -P "$r" -type f -exec sha256sum {} + | LC_ALL=C sort
  done | sha256sum | awk '{print $1}'
}
real_assets_unchanged() { [[ "$(real_digest)" == "$REAL_BEFORE" ]]; }

stage_real_download() {
  cp "$REAL_CANDIDATE" "$C/dl/proxyctl-linux-amd64"
  cp "$REAL_CANDIDATE" "$C/dl/proxyctl-linux-arm64"
  chmod 0755 "$C/dl/proxyctl-linux-amd64" "$C/dl/proxyctl-linux-arm64"
  write_sums "$C/dl/checksums.txt"
}

real_case() { # NAME FIXTURE [mutator-function]
  local name="$1" fixture="$2" mutator="${3:-}"
  new_case "real-$name"
  C_UNAME_M="$(uname -m)"
  stage_real_download
  make_installed "$OLD_VERSION" 0 valid
  OLD_SHA="$(sha_of "$BIN")"
  NEW_SHA="$(sha_of "$C/dl/proxyctl-linux-amd64")"
  real_materialise "$fixture"
  if [[ -n "$mutator" ]]; then "$mutator"; fi
  REAL_BEFORE="$(real_digest)"
}

# The script keeps its production defaults for the instance roots, the
# transaction paths, L3 and the show timeout (the Go binary uses the same fixed
# paths); only L1, the install directory and the previous copy are injected.
run_boot_real() {
  build_env
  local env_args=() kv
  for kv in "${BOOT_ENV[@]}"; do
    case "$kv" in
      REALITY_LOCK_FILE=*|REALITY_STATE_DIR=*|REALITY_CONFIG_DIR=*|REALITY_UNIT_FILE=*|REALITY_BIN_DIR=*|REALITY_TXN_DIR=*|REALITY_TOMB_GLOB=*|L3_WAIT=*|SHOW_TIMEOUT=*) ;;
      *) env_args+=("$kv") ;;
    esac
  done
  set +e
  timeout 120 env "${env_args[@]}" bash "$SCRIPT_UNDER_TEST" "$@" >"$C/stdout" 2>"$C/stderr" </dev/null
  RC=$?
  set -e
}

real_upgraded() {
  [[ "$RC" == 0 && "$(sha_of "$BIN")" == "$NEW_SHA" && "$(sidecar_of "$SIDE")" == "$NEW_SHA" ]]
}
real_rejected() {
  [[ "$RC" == 1 && "$(sha_of "$BIN")" == "$OLD_SHA" && "$(sidecar_of "$SIDE")" == "$OLD_SHA" && ! -e "$PREV" ]]
}

real_mut_schema2() { sed -i 's/"schema": 1,/"schema": 2,/' /var/lib/proxyctl-reality/manifest.json; }
real_mut_corrupt() { printf 'x' >> /etc/proxyctl-reality/config.json; }
real_mut_partial() { rm -f /var/lib/proxyctl-reality/secrets.json; }

test_real_candidate() {
  local name p
  if [[ "${PROXYCTL_REAL_FS_TESTS:-}" != 1 ]]; then
    echo "SKIP real-candidate section (opt-in env PROXYCTL_REAL_FS_TESTS=1 not set; it needs root in a throw-away container)"
    skipped=$((skipped + 1))
    return 0
  fi
  # Opt-in present: every missing prerequisite is a hard failure.
  if [[ "$(command -p id -u)" != 0 ]]; then bad "real-candidate: PROXYCTL_REAL_FS_TESTS=1 requires root"; return 0; fi
  for p in "${REAL_ROOTS[@]}" "${REAL_EXTRA[@]}"; do
    if [[ -e "$p" || -L "$p" ]]; then bad "real-candidate: refusing to run, $p already exists on this host"; return 0; fi
  done
  if compgen -G '/var/lib/.proxyctl-reality-txn.tomb-*' >/dev/null; then
    bad "real-candidate: refusing to run, a transaction tombstone already exists"; return 0
  fi
  REAL_CANDIDATE="${PROXYCTL_REAL_CANDIDATE:-}"
  if [[ -z "$REAL_CANDIDATE" ]]; then
    if command -v go >/dev/null 2>&1; then
      REAL_CANDIDATE="$TEST_DIR/proxyctl-real"
      if ! (cd "$REPO_ROOT" && CGO_ENABLED=0 go build -trimpath -ldflags "-X main.Version=$FIXED_VERSION" -o "$REAL_CANDIDATE" ./cmd/proxyctl); then
        bad "real-candidate: go build failed"; return 0
      fi
    else
      bad "real-candidate: set PROXYCTL_REAL_CANDIDATE to a prebuilt proxyctl (go is not available)"; return 0
    fi
  fi
  if [[ ! -x "$REAL_CANDIDATE" ]]; then bad "real-candidate: $REAL_CANDIDATE is not executable"; return 0; fi
  if [[ "$("$REAL_CANDIDATE" version)" != "$FIXED_VERSION" ]]; then
    bad "real-candidate: $REAL_CANDIDATE does not report $FIXED_VERSION (build with -ldflags '-X main.Version=$FIXED_VERSION')"; return 0
  fi
  if [[ ! -e /run/lock/proxyctl-reality.lock ]]; then REAL_LOCK_PREEXISTED=0; fi

  for name in installed uninstalled; do
    real_case "upgrade-$name" "$name"
    run_boot_real --upgrade
    expect "real candidate, $name fixture: upgrade succeeds (show rc 0)" real_upgraded
    expect "real candidate, $name fixture: the compatibility check really ran" stderr_has "node service show"
    expect "real candidate, $name fixture: manifest/instance/secrets and every asset unchanged (bytes, owner, mode)" real_assets_unchanged
    expect "real candidate, $name fixture: previous copy equals the old bytes" prev_is_old
    expect "real candidate, $name fixture: no temp files" bin_dir_clean
    real_cleanup
  done

  real_case "unknown-schema" installed real_mut_schema2
  run_boot_real --upgrade
  expect "real candidate: manifest schema 2 -> rc 1, nothing replaced, no previous copy" real_rejected
  expect "real candidate: unknown schema: assets unchanged" real_assets_unchanged
  real_cleanup

  real_case "corrupted-asset" installed real_mut_corrupt
  run_boot_real --upgrade
  expect "real candidate: corrupted config.json -> rc 1, nothing replaced" real_rejected
  expect "real candidate: corrupted asset: assets unchanged" real_assets_unchanged
  real_cleanup

  real_case "partial-asset" installed real_mut_partial
  run_boot_real --upgrade
  expect "real candidate: missing secrets.json -> rc 1, nothing replaced" real_rejected
  expect "real candidate: partial asset: assets unchanged" real_assets_unchanged
  real_cleanup

  if [[ "$REAL_LOCK_PREEXISTED" -eq 0 ]]; then rm -f /run/lock/proxyctl-reality.lock; fi
}

# ---------------------------------------------------------------------------

main() {
  if ! command -v flock >/dev/null 2>&1; then bad "util-linux flock is required"; return; fi
  if ! command -v setsid >/dev/null 2>&1 || ! command -v timeout >/dev/null 2>&1; then
    bad "setsid and timeout are required"; return
  fi
  test_script_contract
  test_args_and_gates
  test_upgrade_table
  test_txn_and_role_refusals
  test_candidate_verification
  test_show_skip_matrix
  test_show_failures
  test_lock_handover
  test_change_during_check
  test_previous_copy
  test_stale_previous_tmp
  test_setsid_wait
  test_replacement_faults
  test_signal_matrix
  test_script_structure
  test_real_candidate
}

main

echo
echo "bootstrap-upgrade tests: $pass passed, $fail failed, $skipped section(s) skipped"
if [[ "$fail" -ne 0 ]]; then exit 1; fi
