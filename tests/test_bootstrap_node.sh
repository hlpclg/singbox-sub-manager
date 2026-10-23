#!/usr/bin/env bash
# shellcheck disable=SC2016,SC2317,SC2329
# bootstrap-node.sh first-install mode (v0.10 spec §12, §11.5, §13.2).
#
# Every case runs the real script as a subprocess against temp paths:
#   - production paths are injected through the same env-override convention
#     install-proxy.sh / merge-nodes.sh use (LOCK_FILE, PROXYCTL_BIN,
#     INSTALL_JSON, TOKEN_FILE, NODES_CONF, OS_RELEASE_FILE, LOCK_WAIT);
#   - root / architecture / network are faked with executables placed first
#     on PATH (id, uname, curl, mv), so the script needs no test-only branch;
#   - the "installed" and "downloaded" proxyctl are shell stubs that record
#     every execution, so "never executed" is an observable fact;
#   - locks are real flock(2) locks held by other processes, with FIFO or
#     /proc-observed barriers instead of sleeps.
# Needs Linux (util-linux flock/script, /proc, GNU stat/mv).
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
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

SCRIPT="$REPO_ROOT/bootstrap-node.sh"
SCRIPT_UNDER_TEST="$SCRIPT"
FIXED_VERSION="v0.10.0"

pass=0
fail=0
ok() { echo "PASS: $1"; pass=$((pass + 1)); }
bad() { echo "FAIL: $1" >&2; fail=$((fail + 1)); }

# expect DESC CMD...: pass when CMD succeeds.
expect() {
  local desc="$1"
  shift
  if "$@"; then ok "$desc"; else bad "$desc"; fi
}

# ---------------------------------------------------------------------------
# Stub proxyctl template. Placeholders are replaced per binary.
# ---------------------------------------------------------------------------

STUB_TEMPLATE="$TEST_DIR/stub.template"
cat > "$STUB_TEMPLATE" <<'EOF'
#!/usr/bin/env bash
label='@LABEL@'
log_dir='@LOGDIR@'
{ printf '%s' "$label"; printf ' %s' "$@"; printf '\n'; } >> "$log_dir/exec.log"
if [[ "${1:-}" == version ]]; then printf '%s\n' '@VERSION@'; exit 0; fi
if [[ "${1:-}" == node && "${2:-}" == service && "${3:-}" == --help && $# -eq 3 ]]; then exit @CAP@; fi
if [[ "${1:-}" == node ]]; then
  printf '%s\0' "$@" > "$log_dir/final.args"
  printf '%s\n' "$label" > "$log_dir/final.label"
  fds=""
  for f in /proc/$$/fd/*; do
    t="$(readlink "$f" 2>/dev/null || true)"
    if [[ "$t" == "${LOCK_FILE:-/nonexistent}" ]]; then fds="$fds ${f##*/}"; fi
  done
  printf '%s\n' "$fds" > "$log_dir/final.lockfds"
  exec 7>"${LOCK_FILE}"
  if flock -n 7; then echo acquired; else echo busy; fi > "$log_dir/final.lock"
  exit @FINALRC@
fi
exit 99
EOF

# make_stub PATH VERSION CAP_RC LABEL [FINAL_RC]
make_stub() {
  mkdir -p "$(dirname "$1")"
  sed -e "s|@LABEL@|$4|" -e "s|@LOGDIR@|$C/log|" -e "s|@VERSION@|$2|" \
    -e "s|@CAP@|$3|" -e "s|@FINALRC@|${5:-0}|" "$STUB_TEMPLATE" > "$1"
  chmod 0755 "$1"
}

sha_of() { sha256sum "$1" | awk '{print $1}'; }

# ---------------------------------------------------------------------------
# Case fixture
# ---------------------------------------------------------------------------

new_case() {
  C="$TEST_DIR/$1"
  rm -rf "$C"
  mkdir -p "$C/bin" "$C/fake" "$C/etc" "$C/var" "$C/dl" "$C/log"
  BIN="$C/bin/proxyctl"
  SIDE="$BIN.sha256"
  LOCKF="$C/l1.lock"
  C_UID=0
  C_UNAME_M=x86_64
  CURL_MODE=ok
  LOCK_WAIT_VAL=30
  EXTRA_ENV=()
  SCRIPT_UNDER_TEST="$SCRIPT"
  printf 'ID=ubuntu\nVERSION_ID="24.04"\n' > "$C/os-release"

  cat > "$C/fake/id" <<'EOF'
#!/usr/bin/env bash
if [[ "${1:-}" == -u && $# -eq 1 ]]; then echo "${FAKE_UID:-0}"; exit 0; fi
exec command -p id "$@"
EOF
  cat > "$C/fake/uname" <<'EOF'
#!/usr/bin/env bash
if [[ "${1:-}" == -m && $# -eq 1 ]]; then echo "${FAKE_UNAME_M:-x86_64}"; exit 0; fi
exec command -p uname "$@"
EOF
  cat > "$C/fake/mv" <<'EOF'
#!/usr/bin/env bash
printf '%s\n' "$*" >> "$FAKE_LOG_DIR/mv.log"
command -p mv "$@"
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
  chmod 0755 "$C/fake/id" "$C/fake/uname" "$C/fake/mv" "$C/fake/curl"
}

# stage_download VERSION CAP_RC: valid amd64+arm64 assets and checksums.txt.
stage_download() {
  make_stub "$C/dl/proxyctl-linux-amd64" "$1" "$2" NEW
  cp "$C/dl/proxyctl-linux-amd64" "$C/dl/proxyctl-linux-arm64"
  write_sums "$C/dl/checksums.txt"
}

write_sums() {
  {
    printf '%s  proxyctl-linux-amd64\n' "$(sha_of "$C/dl/proxyctl-linux-amd64")"
    printf '%s  proxyctl-linux-arm64\n' "$(sha_of "$C/dl/proxyctl-linux-arm64")"
  } > "$1"
}

# make_installed VERSION CAP_RC [valid|missing|mismatch]: a stub at $BIN.
make_installed() {
  make_stub "$BIN" "$1" "$2" OLD
  case "${3:-valid}" in
    valid) printf '%s\n' "$(sha_of "$BIN")" > "$SIDE" ;;
    mismatch) printf '%064d\n' 0 > "$SIDE" ;;
    missing) rm -f "$SIDE" ;;
  esac
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
    "FAKE_UID=$C_UID"
    "FAKE_UNAME_M=$C_UNAME_M"
    "FAKE_DL_DIR=$C/dl"
    "FAKE_LOG_DIR=$C/log"
    "FAKE_CURL_MODE=$CURL_MODE"
    "FAKE_CURL_FIFO=$C/curl.fifo"
  )
  if [[ ${#EXTRA_ENV[@]} -gt 0 ]]; then BOOT_ENV+=("${EXTRA_ENV[@]}"); fi
}

run_boot() {
  build_env
  set +e
  env "${BOOT_ENV[@]}" bash "$SCRIPT_UNDER_TEST" "$@" >"$C/stdout" 2>"$C/stderr" </dev/null
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

# run_tty STDOUT_MODE ARGS...: run under a pseudo terminal. STDOUT_MODE is
# "tty" (stdin and stdout both TTYs) or "redirected" (stdout goes to a file
# inside the pty, so only stdin is a TTY).
run_tty() {
  local mode="$1"
  shift
  build_env
  local wrapper="$C/tty_run.sh"
  {
    printf '#!/usr/bin/env bash\n'
    printf 'exec env'
    printf ' %q' "${BOOT_ENV[@]}"
    printf ' bash %q' "$SCRIPT_UNDER_TEST"
    if [[ $# -gt 0 ]]; then printf ' %q' "$@"; fi
    if [[ "$mode" == redirected ]]; then printf ' >%q' "$C/tty_stdout"; fi
    printf '\n'
  } > "$wrapper"
  set +e
  script -qefc "bash $wrapper" /dev/null > "$C/tty_script_out" 2>&1 </dev/null
  RC=$?
  set -e
}

# ---------------------------------------------------------------------------
# Assertions (read the case state)
# ---------------------------------------------------------------------------

rc_is() { [[ "$RC" == "$1" ]]; }
stderr_has() { grep -Fq -- "$1" "$C/stderr"; }
no_curl() { [[ ! -s "$C/log/curl.log" ]]; }
no_exec() { [[ ! -s "$C/log/exec.log" ]]; }
executed() { grep -q "^$1 " "$C/log/exec.log" 2>/dev/null; }
never_executed() { ! executed "$1"; }
final_ran() { [[ -f "$C/log/final.args" ]]; }
final_not_ran() { [[ ! -f "$C/log/final.args" ]]; }
bin_unchanged() { [[ "$(sha_of "$BIN")" == "$1" ]]; }
lock_free() { flock -n "$LOCKF" true; }
bin_dir_is() { [[ "$(find "$C/bin" -mindepth 1 -maxdepth 1 -printf '%f\n' | sort | tr '\n' ' ')" == "$1" ]]; }
installed_matches_download() {
  [[ -f "$BIN" && "$(sha_of "$BIN")" == "$(sha_of "$C/dl/proxyctl-linux-amd64")" ]] \
    && [[ "$(cat "$SIDE")" == "$(sha_of "$BIN")" ]]
}
final_args_are() { # expected args as separate parameters
  local expected="$C/log/expected.args"
  printf '%s\0' "$@" > "$expected"
  cmp -s "$expected" "$C/log/final.args"
}
mode_is() { [[ "$(stat -c %a "$1")" == "$2" ]]; }

show_failure_context() {
  echo "--- rc=$RC stderr ---" >&2
  sed 's/^/  /' "$C/stderr" >&2 || true
}

expect_rc() { # DESC RC
  if rc_is "$2"; then ok "$1"; else bad "$1 (rc=$RC, want $2)"; show_failure_context; fi
}

# ---------------------------------------------------------------------------
# Lock helpers
# ---------------------------------------------------------------------------

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

# wait_flock_child PID: block until PID has a direct child process named
# flock, i.e. bootstrap is genuinely waiting for the lock.
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

# ---------------------------------------------------------------------------
# Cases
# ---------------------------------------------------------------------------

# --- script basics ----------------------------------------------------------

test_script_contract() {
  new_case script-contract
  expect "script exists and is syntactically valid" bash -n "$SCRIPT"
  expect "PROXYCTL_VERSION is a fixed literal $FIXED_VERSION" \
    grep -qx "PROXYCTL_VERSION=\"$FIXED_VERSION\"" "$SCRIPT"
}

# --- first install -----------------------------------------------------------

test_first_install() {
  new_case first-install
  stage_download "$FIXED_VERSION" 0
  EXTRA_ENV=("PROXYCTL_VERSION=v9.9.9" "PROXYCTL_REPOSITORY=evil/other")
  run_boot --name 'node a' --server 1.2.3.4
  expect_rc "first install: exec'd proxyctl result passes through" 0
  expect "first install: asset and checksums fetched from the fixed Release, env ignored" \
    bash -c '
      grep -qx "https://github.com/hlpclg/singbox-sub-manager/releases/download/v0.10.0/proxyctl-linux-amd64|120|'"$C"'/bin" "$1" &&
      grep -qx "https://github.com/hlpclg/singbox-sub-manager/releases/download/v0.10.0/checksums.txt|120|'"$C"'/bin" "$1" &&
      [[ "$(wc -l < "$1")" -eq 2 ]]' _ "$C/log/curl.log"
  expect "first install: binary installed and sidecar matches" installed_matches_download
  expect "first install: binary mode 0755" mode_is "$BIN" 755
  expect "first install: sidecar mode 0644" mode_is "$SIDE" 644
  expect "first install: sidecar is 64 lowercase hex plus newline" \
    bash -c '[[ "$(cat "$1")" =~ ^[0-9a-f]{64}$ ]] && [[ "$(wc -l < "$1")" -eq 1 ]]' _ "$SIDE"
  expect "first install: only proxyctl and its sidecar exist in the target dir" \
    bin_dir_is "proxyctl proxyctl.sha256 "
  expect "first install: downloaded binary ran version then capability probe" \
    bash -c 'grep -qx "NEW version" "$1" && grep -qx "NEW node service --help" "$1"' _ "$C/log/exec.log"
  expect "first install: exec'd proxyctl is node service install with the args" \
    final_args_are node service install --name 'node a' --server 1.2.3.4
  expect "first install: temp files and installs are in the same dir (rename, not copy)" \
    bash -c '
      n=0
      while read -r src dst; do
        n=$((n + 1))
        [[ "$(dirname "$src")" == "$(dirname "$dst")" ]] || exit 1
      done < <(sed "s/^-f //" "$1")
      [[ "$n" -eq 2 ]]' _ "$C/log/mv.log"
  expect "first install: binary renamed before sidecar" \
    bash -c 'head -1 "$1" | grep -q "proxyctl\$" && tail -1 "$1" | grep -q "proxyctl.sha256\$"' _ "$C/log/mv.log"
  expect "first install: exec'd proxyctl got L1 with flock -n" \
    grep -qx acquired "$C/log/final.lock"
  expect "first install: exec'd proxyctl inherited no fd on the L1 lock file" \
    bash -c '[[ -z "$(tr -d "[:space:]" < "$1")" ]]' _ "$C/log/final.lockfds"
}

test_first_install_arm64() {
  new_case first-install-arm64
  C_UNAME_M=aarch64
  stage_download "$FIXED_VERSION" 0
  run_boot --name n --server 1.2.3.4
  expect_rc "arm64: first install succeeds" 0
  expect "arm64: requests the arm64 asset" \
    grep -q 'proxyctl-linux-arm64|120|' "$C/log/curl.log"
  expect "arm64: never requests the amd64 asset" \
    bash -c '! grep -q "proxyctl-linux-amd64" "$1"' _ "$C/log/curl.log"
}

test_checksum_star_form() {
  new_case checksum-star
  stage_download "$FIXED_VERSION" 0
  {
    printf '%s  *proxyctl-linux-amd64\n' "$(sha_of "$C/dl/proxyctl-linux-amd64")"
    printf '%064d  proxyctl-linux-arm64\n' 7
  } > "$C/dl/checksums.txt"
  run_boot --name n --server 1.2.3.4
  expect_rc "checksums.txt '*file' form is accepted" 0
}

# --- decision table -----------------------------------------------------------

test_row_sidecar_missing() {
  new_case row-sidecar-missing
  make_installed "$FIXED_VERSION" 0 missing
  stage_download "$FIXED_VERSION" 0
  local before
  before="$(sha_of "$BIN")"
  run_boot --name n --server 1.2.3.4
  expect_rc "sidecar missing, no flag: stop with 1" 1
  expect "sidecar missing, no flag: installed binary never executed" no_exec
  expect "sidecar missing, no flag: nothing downloaded" no_curl
  expect "sidecar missing, no flag: binary unchanged" bin_unchanged "$before"
  expect "sidecar missing, no flag: stderr points at --replace-proxyctl" \
    stderr_has "--replace-proxyctl"
  expect "sidecar missing, no flag: lock released" lock_free
}

test_row_sidecar_mismatch() {
  new_case row-sidecar-mismatch
  make_installed "$FIXED_VERSION" 0 mismatch
  stage_download "$FIXED_VERSION" 0
  local before
  before="$(sha_of "$BIN")"
  run_boot --name n --server 1.2.3.4
  expect_rc "sidecar mismatch, no flag: stop with 1" 1
  expect "sidecar mismatch, no flag: installed binary never executed" no_exec
  expect "sidecar mismatch, no flag: nothing downloaded" no_curl
  expect "sidecar mismatch, no flag: binary unchanged" bin_unchanged "$before"
}

test_row_replace_repairs() {
  local kind
  for kind in missing mismatch; do
    new_case "row-replace-$kind"
    make_installed "$FIXED_VERSION" 0 "$kind"
    stage_download "$FIXED_VERSION" 0
    run_boot --replace-proxyctl --name n --server 1.2.3.4
    expect_rc "sidecar $kind + --replace-proxyctl: installs fixed version and execs" 0
    expect "sidecar $kind + --replace-proxyctl: bad installed binary never executed" \
      never_executed OLD
    expect "sidecar $kind + --replace-proxyctl: replaced by the verified download" \
      installed_matches_download
    expect "sidecar $kind + --replace-proxyctl: flag is stripped from the exec args" \
      final_args_are node service install --name n --server 1.2.3.4
  done
}

test_row_unparseable() {
  local v
  for v in dev v0.10 v0.10.0-rc1 garbage ''; do
    new_case "row-unparseable-${v:-empty}"
    make_installed "$v" 0 valid
    stage_download "$FIXED_VERSION" 0
    local before
    before="$(sha_of "$BIN")"
    run_boot --replace-proxyctl --name n --server 1.2.3.4
    expect_rc "valid sidecar, version '${v}': stop with 1 (even with --replace-proxyctl)" 1
    expect "valid sidecar, version '${v}': nothing downloaded" no_curl
    expect "valid sidecar, version '${v}': binary unchanged" bin_unchanged "$before"
    expect "valid sidecar, version '${v}': no node command run" final_not_ran
  done
}

test_row_same_version_keep() {
  new_case row-same-keep
  make_installed "$FIXED_VERSION" 0 valid
  stage_download "$FIXED_VERSION" 0
  local before
  before="$(sha_of "$BIN")"
  run_boot --name n --server 1.2.3.4
  expect_rc "same version with capability: keep and exec" 0
  expect "same version with capability: no download" no_curl
  expect "same version with capability: binary unchanged" bin_unchanged "$before"
  expect "same version with capability: the installed binary is what ran" \
    bash -c '[[ "$(cat "$1")" == OLD ]]' _ "$C/log/final.label"
  expect "same version with capability: capability probe was run" \
    grep -qx "OLD node service --help" "$C/log/exec.log"
  expect "same version with capability: exec'd proxyctl got L1 and no inherited fd" \
    bash -c 'grep -qx acquired "$1" && [[ -z "$(tr -d "[:space:]" < "$2")" ]]' _ "$C/log/final.lock" "$C/log/final.lockfds"
}

test_row_same_version_no_capability() {
  local flag
  for flag in none replace; do
    new_case "row-same-nocap-$flag"
    make_installed "$FIXED_VERSION" 1 valid
    stage_download "$FIXED_VERSION" 0
    local before
    before="$(sha_of "$BIN")"
    if [[ "$flag" == replace ]]; then run_boot --replace-proxyctl --name n --server 1.2.3.4
    else run_boot --name n --server 1.2.3.4; fi
    expect_rc "same version without capability ($flag): stop with 1" 1
    expect "same version without capability ($flag): nothing downloaded" no_curl
    expect "same version without capability ($flag): binary unchanged" bin_unchanged "$before"
    expect "same version without capability ($flag): no node command run" final_not_ran
  done
}

test_row_newer() {
  local v flag
  for v in v0.10.1 v0.11.0 v1.0.0 v0.10.10; do
    for flag in none replace; do
      new_case "row-newer-$v-$flag"
      make_installed "$v" 0 valid
      stage_download "$FIXED_VERSION" 0
      local before
      before="$(sha_of "$BIN")"
      if [[ "$flag" == replace ]]; then run_boot --replace-proxyctl --name n --server 1.2.3.4
      else run_boot --name n --server 1.2.3.4; fi
      expect_rc "installed $v newer than $FIXED_VERSION ($flag): stop with 1" 1
      expect "installed $v newer ($flag): hint to run sudo proxyctl node" \
        stderr_has "sudo proxyctl node"
      expect "installed $v newer ($flag): not told to downgrade or upgrade" \
        bash -c '! grep -q -- "--upgrade" "$1"' _ "$C/stderr"
      expect "installed $v newer ($flag): nothing downloaded" no_curl
      expect "installed $v newer ($flag): binary not replaced or downgraded" bin_unchanged "$before"
      expect "installed $v newer ($flag): no node command run" final_not_ran
    done
  done
}

# The fixed version is a literal in the script; the "installed is older than
# the fixed version but not below v0.10.0" row is only reachable when the
# fixed version is above v0.10.0, so run a patched copy of the script.
test_row_older_than_fixed() {
  local installed flag
  for installed in v0.10.0 v0.10.1 v0.10.9; do
    for flag in none replace; do
      new_case "row-older-$installed-$flag"
      SCRIPT_UNDER_TEST="$C/bootstrap-patched.sh"
      sed 's/^PROXYCTL_VERSION=.*/PROXYCTL_VERSION="v0.10.10"/' "$SCRIPT" > "$SCRIPT_UNDER_TEST" || true
      grep -qx 'PROXYCTL_VERSION="v0.10.10"' "$SCRIPT_UNDER_TEST" || { bad "patched copy has v0.10.10"; continue; }
      make_installed "$installed" 0 valid
      stage_download v0.10.10 0
      local before
      before="$(sha_of "$BIN")"
      if [[ "$flag" == replace ]]; then run_boot --replace-proxyctl --name n --server 1.2.3.4
      else run_boot --name n --server 1.2.3.4; fi
      expect_rc "installed $installed older than fixed v0.10.10 ($flag): stop with 1" 1
      expect "installed $installed older ($flag): hint to run bootstrap-node.sh --upgrade" \
        stderr_has "sudo bash bootstrap-node.sh --upgrade"
      expect "installed $installed older ($flag): nothing downloaded" no_curl
      expect "installed $installed older ($flag): binary unchanged" bin_unchanged "$before"
    done
  done
  new_case row-same-v01010
  SCRIPT_UNDER_TEST="$C/bootstrap-patched.sh"
  sed 's/^PROXYCTL_VERSION=.*/PROXYCTL_VERSION="v0.10.10"/' "$SCRIPT" > "$SCRIPT_UNDER_TEST" || true
  make_installed v0.10.10 0 valid
  run_boot --name n --server 1.2.3.4
  expect_rc "numeric (not lexical) compare: v0.10.10 equals fixed v0.10.10, kept" 0
  expect "numeric compare: kept without download" no_curl
}

test_row_below_min_installs() {
  local v
  for v in v0.9.0 v0.9.99 v0.2.2 v0.0.1; do
    new_case "row-below-$v"
    make_installed "$v" 1 valid
    stage_download "$FIXED_VERSION" 0
    run_boot --name n --server 1.2.3.4
    expect_rc "installed $v below v0.10.0: fixed version installed, exec ok" 0
    expect "installed $v below v0.10.0: replaced with the verified download" installed_matches_download
    expect "installed $v below v0.10.0: old binary only asked for its version" \
      bash -c '[[ "$(grep -c "^OLD " "$1")" -eq 1 ]] && grep -qx "OLD version" "$1"' _ "$C/log/exec.log"
    expect "installed $v below v0.10.0: node command ran from the new binary" \
      bash -c '[[ "$(cat "$1")" == NEW ]]' _ "$C/log/final.label"
  done
}

test_non_regular_targets() {
  new_case target-dir
  mkdir "$BIN"
  stage_download "$FIXED_VERSION" 0
  run_boot --name n --server 1.2.3.4
  expect_rc "binary path is a directory: refuse with 1" 1
  expect "binary path is a directory: nothing downloaded" no_curl

  new_case target-symlink
  make_installed "$FIXED_VERSION" 0 valid
  mv "$BIN" "$C/real-proxyctl"
  ln -s "$C/real-proxyctl" "$BIN"
  stage_download "$FIXED_VERSION" 0
  run_boot --replace-proxyctl --name n --server 1.2.3.4
  expect_rc "binary path is a symlink: refuse with 1 even with --replace-proxyctl" 1
  expect "binary path is a symlink: never executed, nothing downloaded" \
    bash -c '[[ ! -s "$1" && ! -s "$2" ]]' _ "$C/log/exec.log" "$C/log/curl.log"

  new_case sidecar-dir
  make_installed "$FIXED_VERSION" 0 valid
  rm -f "$SIDE"
  mkdir "$SIDE"
  stage_download "$FIXED_VERSION" 0
  run_boot --replace-proxyctl --name n --server 1.2.3.4
  expect_rc "sidecar path is a directory: refuse with 1" 1
  expect "sidecar path is a directory: nothing executed or downloaded" \
    bash -c '[[ ! -s "$1" && ! -s "$2" ]]' _ "$C/log/exec.log" "$C/log/curl.log"
}

# --- host checks ------------------------------------------------------------

test_not_root() {
  new_case not-root
  C_UID=1000
  stage_download "$FIXED_VERSION" 0
  run_boot --name n --server 1.2.3.4
  expect_rc "non-root: 1" 1
  expect "non-root: mentions root/sudo" stderr_has "root"
  expect "non-root: no download, no lock file created, nothing executed" \
    bash -c '[[ ! -s "$1" && ! -e "$2" && ! -s "$3" ]]' _ "$C/log/curl.log" "$LOCKF" "$C/log/exec.log"
}

test_os_matrix() {
  local spec id ver want
  for spec in "ubuntu 22.04 ok" "ubuntu 24.04 ok" "debian 12 ok" \
              "ubuntu 20.04 no" "ubuntu 25.04 no" "debian 11 no" "debian 13 no" \
              "fedora 40 no" "alpine 3.19 no"; do
    read -r id ver want <<<"$spec"
    new_case "os-$id-$ver"
    printf 'ID=%s\nVERSION_ID="%s"\n' "$id" "$ver" > "$C/os-release"
    stage_download "$FIXED_VERSION" 0
    run_boot --name n --server 1.2.3.4
    if [[ "$want" == ok ]]; then
      expect_rc "OS $id $ver accepted" 0
    else
      expect_rc "OS $id $ver rejected with 1" 1
      expect "OS $id $ver rejected: no download" no_curl
    fi
  done
  new_case os-missing-file
  rm -f "$C/os-release"
  stage_download "$FIXED_VERSION" 0
  run_boot --name n --server 1.2.3.4
  expect_rc "missing os-release rejected with 1" 1
  expect "missing os-release: no download" no_curl
}

test_arch_matrix() {
  local m
  for m in armv7l i686 ppc64le riscv64 s390x; do
    new_case "arch-$m"
    C_UNAME_M="$m"
    stage_download "$FIXED_VERSION" 0
    run_boot --name n --server 1.2.3.4
    expect_rc "arch $m rejected with 1" 1
    expect "arch $m rejected: no download" no_curl
  done
}

test_role_rejections() {
  local spec label
  for spec in install nodes token install+nodes install+token nodes+token all; do
    new_case "role-$spec"
    stage_download "$FIXED_VERSION" 0
    label="$spec"
    [[ "$spec" == *install* || "$spec" == all ]] && : > "$C/etc/install.json"
    [[ "$spec" == *nodes* || "$spec" == all ]] && : > "$C/etc/nodes.conf"
    [[ "$spec" == *token* || "$spec" == all ]] && : > "$C/var/token"
    run_boot --name n --server 1.2.3.4
    expect_rc "role not 'no' ($label): rejected with 1" 1
    expect "role not 'no' ($label): points at install-proxy.sh update" \
      stderr_has "install-proxy.sh update"
    expect "role not 'no' ($label): no download, no lock, nothing executed" \
      bash -c '[[ ! -s "$1" && ! -e "$2" && ! -s "$3" ]]' _ "$C/log/curl.log" "$LOCKF" "$C/log/exec.log"
  done
  new_case role-dangling-symlink
  stage_download "$FIXED_VERSION" 0
  ln -s "$C/nowhere" "$C/etc/nodes.conf"
  run_boot --name n --server 1.2.3.4
  expect_rc "dangling nodes.conf symlink still counts as present: rejected" 1
  expect "dangling symlink: no download" no_curl
}

test_role_rechecked_inside_l1() {
  new_case role-recheck
  stage_download "$FIXED_VERSION" 0
  LOCK_WAIT_VAL=30
  hold_lock "$LOCKF"
  start_boot --name n --server 1.2.3.4
  if wait_flock_child "$BOOT_PID"; then
    : > "$C/etc/install.json"
    release_lock
    finish_boot
    expect_rc "role changed while waiting for L1: rejected with 1" 1
    expect "role changed while waiting for L1: no download" no_curl
    expect "role changed while waiting for L1: nothing installed" bin_dir_is ""
    expect "role changed while waiting for L1: lock released" lock_free
  else
    release_lock
    finish_boot || true
    bad "bootstrap never reached the L1 wait"
  fi
}

# --- locking, cancellation ---------------------------------------------------

test_l1_timeout() {
  new_case l1-timeout
  stage_download "$FIXED_VERSION" 0
  LOCK_WAIT_VAL=1
  hold_lock "$LOCKF"
  run_boot --name n --server 1.2.3.4
  release_lock
  expect_rc "L1 held by another process: wait times out with 1" 1
  expect "L1 timeout: reports another operation in progress" stderr_has "in progress"
  expect "L1 timeout: no download, nothing installed, nothing executed" \
    bash -c '[[ ! -s "$1" && ! -s "$2" ]]' _ "$C/log/curl.log" "$C/log/exec.log"
  expect "L1 timeout: target dir untouched" bin_dir_is ""
}

test_l1_wait_succeeds_after_release() {
  new_case l1-wait-ok
  stage_download "$FIXED_VERSION" 0
  hold_lock "$LOCKF"
  start_boot --name n --server 1.2.3.4
  if wait_flock_child "$BOOT_PID"; then
    release_lock
    finish_boot
    expect_rc "L1 released while waiting: bootstrap proceeds" 0
    expect "L1 released while waiting: installed" installed_matches_download
  else
    release_lock
    finish_boot || true
    bad "bootstrap never reached the L1 wait"
  fi
}

test_signal_in_lock_wait() {
  new_case signal-lock-wait
  stage_download "$FIXED_VERSION" 0
  hold_lock "$LOCKF"
  start_boot --name n --server 1.2.3.4
  if wait_flock_child "$BOOT_PID"; then
    kill -TERM "$BOOT_PID"
    finish_boot
    release_lock
    expect_rc "SIGTERM during the L1 wait: 1 immediately" 1
    expect "SIGTERM during the L1 wait: nothing downloaded or installed" \
      bash -c '[[ ! -s "$1" ]]' _ "$C/log/curl.log"
    expect "SIGTERM during the L1 wait: no leftover waiting flock process" \
      bash -c '! grep -l "^PPid:[[:space:]]*'"$BOOT_PID"'\$" /proc/[0-9]*/status 2>/dev/null | grep -q .'
  else
    release_lock
    kill "$BOOT_PID" 2>/dev/null || true
    finish_boot || true
    bad "bootstrap never reached the L1 wait"
  fi
}

test_signal_in_download() {
  local sig
  for sig in TERM INT; do
    new_case "signal-download-$sig"
    stage_download "$FIXED_VERSION" 0
    CURL_MODE=hang
    mkfifo "$C/curl.fifo"
    start_boot --name n --server 1.2.3.4
    if timeout 10 cat "$C/curl.fifo" >/dev/null; then
      kill -"$sig" "$BOOT_PID"
      finish_boot
      expect_rc "SIG$sig during download: 1" 1
      expect "SIG$sig during download: temp files deleted, nothing installed" bin_dir_is ""
      expect "SIG$sig during download: in-flight curl terminated" \
        bash -c '! kill -0 "$(cat "$1")" 2>/dev/null' _ "$C/log/curl.pid"
      expect "SIG$sig during download: lock released" lock_free
      expect "SIG$sig during download: no node command run" final_not_ran
    else
      kill "$BOOT_PID" 2>/dev/null || true
      finish_boot || true
      bad "SIG$sig during download: bootstrap never started the download"
    fi
  done
}

# --- download / verification failures -----------------------------------------

# failure_case NAME SETUP: the bad-sidecar installed binary must survive any
# download/verification failure byte for byte, and no temp file may remain.
failure_case() {
  local name="$1" setup="$2" want_stderr="${3:-}"
  new_case "fail-$name"
  make_installed "$FIXED_VERSION" 0 mismatch
  local bin_before side_before
  bin_before="$(sha_of "$BIN")"
  side_before="$(sha_of "$SIDE")"
  stage_download "$FIXED_VERSION" 0
  "$setup"
  run_boot --replace-proxyctl --name n --server 1.2.3.4
  expect_rc "$name: exit 1" 1
  expect "$name: installed binary unchanged" bin_unchanged "$bin_before"
  expect "$name: installed sidecar unchanged" \
    bash -c '[[ "$(sha256sum < "$1" | cut -d" " -f1)" == "$2" ]]' _ "$SIDE" "$side_before"
  expect "$name: no temp files left in the target dir" bin_dir_is "proxyctl proxyctl.sha256 "
  expect "$name: no node command run" final_not_ran
  expect "$name: lock released" lock_free
  [[ -z "$want_stderr" ]] || expect "$name: stderr explains" stderr_has "$want_stderr"
}

setup_curl_fail() { CURL_MODE=fail; }
setup_curl_fail_binary() { CURL_MODE="fail-binary"; }
setup_curl_fail_sums() { CURL_MODE="fail-sums"; }
setup_checksum_mismatch() { printf '%064d  proxyctl-linux-amd64\n' 5 > "$C/dl/checksums.txt"; }
setup_checksum_zero_lines() { printf '%s  other-file\n' "$(sha_of "$C/dl/proxyctl-linux-amd64")" > "$C/dl/checksums.txt"; }
setup_checksum_two_lines() {
  local h
  h="$(sha_of "$C/dl/proxyctl-linux-amd64")"
  printf '%s  proxyctl-linux-amd64\n%s  proxyctl-linux-amd64\n' "$h" "$h" > "$C/dl/checksums.txt"
}
setup_checksum_two_different_lines() {
  printf '%s  proxyctl-linux-amd64\n%064d  *proxyctl-linux-amd64\n' \
    "$(sha_of "$C/dl/proxyctl-linux-amd64")" 3 > "$C/dl/checksums.txt"
}
setup_version_mismatch() { stage_download v0.10.1 0; }
setup_version_empty() { make_stub "$C/dl/proxyctl-linux-amd64" "" 0 NEW; write_sums "$C/dl/checksums.txt"; }
setup_capability_fail() { stage_download "$FIXED_VERSION" 1; }

test_download_failures() {
  failure_case download-fails setup_curl_fail
  failure_case binary-download-fails setup_curl_fail_binary
  failure_case checksums-download-fails setup_curl_fail_sums
  failure_case checksum-mismatch setup_checksum_mismatch
  failure_case checksum-no-line setup_checksum_zero_lines
  failure_case checksum-duplicate-lines setup_checksum_two_lines
  failure_case checksum-conflicting-lines setup_checksum_two_different_lines
  failure_case version-mismatch setup_version_mismatch
  failure_case version-empty setup_version_empty
  failure_case capability-probe-fails setup_capability_fail

  new_case first-install-download-fails
  CURL_MODE=fail
  stage_download "$FIXED_VERSION" 0
  run_boot --name n --server 1.2.3.4
  expect_rc "first install, download fails: 1" 1
  expect "first install, download fails: target dir empty" bin_dir_is ""

  new_case download-timeout-flag
  stage_download "$FIXED_VERSION" 0
  run_boot --name n --server 1.2.3.4
  expect "each download is bounded to 120 seconds" \
    bash -c '[[ "$(cut -d"|" -f2 "$1" | sort -u | tr "\n" " ")" == "120 " ]]' _ "$C/log/curl.log"
}

# --- arguments ------------------------------------------------------------------

test_arg_passthrough() {
  new_case args-passthrough
  make_installed "$FIXED_VERSION" 0 valid
  run_boot --replace-proxyctl --name 'a b' '' "it's" '--server' '1.2.3.4' --replace-proxyctl \
    '$(touch /tmp/never)' '*' $'line1\nline2' -n ' lead ' '--sni=x y'
  expect_rc "args: run succeeds" 0
  expect "args: --replace-proxyctl stripped, the rest byte-for-byte (spaces, quotes, empty, newline, glob)" \
    final_args_are node service install --name 'a b' '' "it's" '--server' '1.2.3.4' \
      '$(touch /tmp/never)' '*' $'line1\nline2' -n ' lead ' '--sni=x y'
  expect "args: no shell expansion side effect" bash -c '[[ ! -e /tmp/never ]]'

  new_case args-final-rc
  make_installed "$FIXED_VERSION" 0 valid
  make_stub "$BIN" "$FIXED_VERSION" 0 OLD 7
  printf '%s\n' "$(sha_of "$BIN")" > "$SIDE"
  run_boot --name n --server 1.2.3.4
  expect_rc "args: exit status of the exec'd proxyctl is the script's status" 7
}

test_upgrade_args() {
  local combo
  for combo in "--upgrade --name x" "--name x --upgrade" "--upgrade --replace-proxyctl" \
               "--replace-proxyctl --upgrade" "--upgrade --upgrade" "--upgrade ''"; do
    new_case "upgrade-combo-${combo// /_}"
    C_UID=1000
    stage_download "$FIXED_VERSION" 0
    # shellcheck disable=SC2086
    eval run_boot $combo
    expect_rc "'$combo': usage error 2 before the root check" 2
    expect "'$combo': nothing downloaded, no lock taken, nothing executed" \
      bash -c '[[ ! -s "$1" && ! -e "$2" && ! -s "$3" ]]' _ "$C/log/curl.log" "$LOCKF" "$C/log/exec.log"
  done
}

test_no_args_non_tty() {
  new_case noargs-nontty
  stage_download "$FIXED_VERSION" 0
  run_boot
  expect_rc "no args, not a TTY: usage and exit 2" 2
  expect "no args, not a TTY: usage text printed" stderr_has "usage"
  expect "no args, not a TTY: proxyctl was still installed (usage comes after confirming it)" \
    installed_matches_download
  expect "no args, not a TTY: no node command run" final_not_ran
  expect "no args, not a TTY: lock released" lock_free

  new_case replace-only-repairs-then-usage
  make_installed "$FIXED_VERSION" 0 mismatch
  stage_download "$FIXED_VERSION" 0
  run_boot --replace-proxyctl
  expect_rc "--replace-proxyctl alone, not a TTY: usage, 2" 2
  expect "--replace-proxyctl alone: install files nevertheless repaired" installed_matches_download
  expect "--replace-proxyctl alone: bad binary never executed" never_executed OLD
}

test_tty() {
  if ! command -v script >/dev/null 2>&1; then bad "util-linux 'script' is required for the TTY cases"; return; fi

  new_case tty-menu
  make_installed "$FIXED_VERSION" 0 valid
  run_tty tty
  expect_rc "no args, stdin and stdout TTYs: exec proxyctl node (menu), exit 0" 0
  expect "no args, TTY: exec'd as exactly 'proxyctl node'" final_args_are node
  expect "no args, TTY: exec'd proxyctl got L1, no inherited fd" \
    bash -c 'grep -qx acquired "$1" && [[ -z "$(tr -d "[:space:]" < "$2")" ]]' _ "$C/log/final.lock" "$C/log/final.lockfds"

  new_case tty-with-args
  make_installed "$FIXED_VERSION" 0 valid
  run_tty tty --replace-proxyctl --name n --server 1.2.3.4
  expect_rc "args on a TTY: install, exit 0" 0
  expect "args on a TTY: node service install with the stripped args" \
    final_args_are node service install --name n --server 1.2.3.4

  new_case tty-stdout-redirected
  make_installed "$FIXED_VERSION" 0 valid
  run_tty redirected
  expect_rc "no args, stdin TTY but stdout not: usage, 2" 2
  expect "stdout not a TTY: node command not run" final_not_ran
}

# --- shell hygiene ---------------------------------------------------------------

test_no_instance_assets_written() {
  new_case no-instance-assets
  stage_download "$FIXED_VERSION" 0
  run_boot --name n --server 1.2.3.4
  expect "bootstrap writes only the binary and sidecar (plus the lock file and test logs)" \
    bash -c '
      cd "$1"
      extra="$(find . -mindepth 1 \( -path ./fake -o -path ./dl -o -path ./log \) -prune -o -type f -print \
        | grep -v -e "^./bin/proxyctl$" -e "^./bin/proxyctl.sha256$" -e "^./l1.lock$" \
                  -e "^./os-release$" -e "^./stdout$" -e "^./stderr$" || true)"
      [[ -z "$extra" ]]' _ "$C"
}

main() {
  test_script_contract
  test_first_install
  test_first_install_arm64
  test_checksum_star_form
  test_row_sidecar_missing
  test_row_sidecar_mismatch
  test_row_replace_repairs
  test_row_unparseable
  test_row_same_version_keep
  test_row_same_version_no_capability
  test_row_newer
  test_row_older_than_fixed
  test_row_below_min_installs
  test_non_regular_targets
  test_not_root
  test_os_matrix
  test_arch_matrix
  test_role_rejections
  test_role_rechecked_inside_l1
  test_l1_timeout
  test_l1_wait_succeeds_after_release
  test_signal_in_lock_wait
  test_signal_in_download
  test_download_failures
  test_arg_passthrough
  test_upgrade_args
  test_no_args_non_tty
  test_tty
  test_no_instance_assets_written
}

main

echo
echo "bootstrap-node tests: $pass passed, $fail failed"
[[ "$fail" -eq 0 && "$pass" -gt 0 ]]
