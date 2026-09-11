#!/usr/bin/env bash
# SC2016: the harness is assembled from single-quoted source lines on purpose.
# SC2317/SC2329: the stubs are invoked indirectly, by the production functions.
# Which of the two a given shellcheck reports depends on its version (0.9
# says SC2317, 0.11 says SC2329), so both are listed.
# Subshell-scope checks (SC2030/SC2031) stay on: this suite depends on them.
# shellcheck disable=SC2016,SC2317,SC2329
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
TEST_DIR="$(mktemp -d)"
trap 'rm -rf "$TEST_DIR"' EXIT

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

HARNESS="$TEST_DIR/harness.sh"
{
  printf '%s\n' '#!/usr/bin/env bash'
  printf '%s\n' 'log() { echo "[INFO] $1"; }'
  printf '%s\n' 'log_warn() { echo "[WARN] $1" >&2; }'
  printf '%s\n' 'log_error() { echo "[ERROR] $1" >&2; }'
  printf '%s\n' 'die() { log_error "$1"; exit 1; }'
  printf '%s\n' 'UPDATE_TMP_FILES=()'
  printf '%s\n' 'UPDATE_SESSION_DIR=""'
  printf '%s\n' 'installer_preflight() { :; }'
  for fn in sha256_file usage fetch_url latest_proxyctl_tag run_proxyctl_health \
            proxyctl_supports_config_snapshot sync_path replace_file_atomic \
            session_meta_get write_session_meta set_session_status \
            create_update_session latest_update_session verify_staged_binary_pair \
            restore_binary_pair restore_config_snapshot prune_successful_sessions \
            update_signal_cleanup cmd_update cmd_rollback main; do
    extract_function "$fn"
  done
} > "$HARNESS"

for required in cmd_update cmd_rollback main replace_file_atomic verify_staged_binary_pair; do
  grep -q "^${required}()" "$HARNESS" || {
    echo "production function $required is missing from the harness" >&2
    exit 1
  }
done

PASSED=0
FAILED=0

fail() {
  echo "  FAIL: $1" >&2
}

# run_case isolates one scenario. A case that aborts part way through - a
# missing directory, an unexpected non-zero command - is reported as a
# failure rather than ending the whole run quietly.
run_case() {
  local name="$1" rc
  set +e
  ( set -euo pipefail; "$name" )
  rc=$?
  set -e
  if [[ "$rc" -eq 0 ]]; then
    echo "$name PASSED"
    PASSED=$((PASSED + 1))
  else
    echo "$name FAILED (exit $rc)" >&2
    FAILED=$((FAILED + 1))
  fi
}

# make_fake_proxyctl writes a stand-in binary whose behaviour the tests steer
# through environment variables, and which records what it was asked to do.
make_fake_proxyctl() {
  local path="$1" version="$2" label="$3"
  mkdir -p "$(dirname "$path")"
  cat > "$path" <<FAKEHEAD
#!/usr/bin/env bash
SELF_LABEL='$label'
SELF_VERSION='$version'
FAKEHEAD
  cat >> "$path" <<'FAKE'
note() { [[ -n "${FAKE_LOG:-}" ]] && printf '%s %s\n' "$SELF_LABEL" "$*" >> "$FAKE_LOG"; return 0; }
caps() {
  if [[ "$SELF_LABEL" == old ]]; then
    printf '%s\n' "${FAKE_CAPS:-full}"
  else
    printf '%s\n' "${FAKE_NEW_CAPS:-full}"
  fi
}
case "${1:-}" in
  version) printf '%s\n' "$SELF_VERSION"; exit 0 ;;
  backup)
    if [[ "${2:-}" == "--help" ]]; then
      [[ "$(caps)" == full ]] && exit 0
      exit 2
    fi
    [[ "$(caps)" == full ]] || { note "backup-on-incapable-binary"; exit 2; }
    out=""
    while [[ $# -gt 0 ]]; do
      case "$1" in --out) out="$2"; shift 2 ;; *) shift ;; esac
    done
    note "backup --out $out"
    [[ "${FAKE_BACKUP:-ok}" == ok ]] || exit 1
    printf 'fake-archive\n' > "$out"
    exit 0
    ;;
  restore)
    if [[ "${2:-}" == "--help" ]]; then
      [[ "$(caps)" == full ]] && exit 0
      exit 2
    fi
    [[ "$(caps)" == full ]] || { note "restore-on-incapable-binary"; exit 2; }
    note "restore ${2:-}"
    [[ "${FAKE_RESTORE:-ok}" == ok ]] || exit 4
    exit 0
    ;;
  health)
    note "health"
    [[ "${FAKE_HEALTH:-ok}" == ok ]] || exit 1
    exit 0
    ;;
esac
exit 2
FAKE
  chmod 0755 "$path"
}

# new_case prepares an isolated installation layout and exports the paths the
# production functions read.
new_case() {
  local name="$1"
  CASE_DIR="$TEST_DIR/$name"
  mkdir -p "$CASE_DIR/bin" "$CASE_DIR/state"
  PROXYCTL_BIN="$CASE_DIR/bin/proxyctl"
  UPDATES_DIR="$CASE_DIR/state/updates"
  BACKUPS_DIR="$CASE_DIR/state/backups"
  FAKE_LOG="$CASE_DIR/actions.log"
  DOWNLOAD_TIMEOUT=5
  HEALTH_TIMEOUT=5
  PROXYCTL_REPOSITORY="example/repo"
  export PROXYCTL_BIN UPDATES_DIR BACKUPS_DIR FAKE_LOG DOWNLOAD_TIMEOUT HEALTH_TIMEOUT PROXYCTL_REPOSITORY
  mkdir -p "$BACKUPS_DIR"
  : > "$FAKE_LOG"

  make_fake_proxyctl "$PROXYCTL_BIN" "v0.6.0" old
  OLD_BINARY_TEXT="$(cat "$PROXYCTL_BIN")"
  printf '%s\n' "$(sha256_file_test "$PROXYCTL_BIN")" > "${PROXYCTL_BIN}.sha256"
  chmod 0644 "${PROXYCTL_BIN}.sha256"
  OLD_SIDECAR_TEXT="$(cat "${PROXYCTL_BIN}.sha256")"
}

sha256_file_test() {
  if command -v sha256sum >/dev/null 2>&1; then
    sha256sum "$1" | awk '{print $1}'
  else
    shasum -a 256 "$1" | awk '{print $1}'
  fi
}

# write_new_release stages what a download of the next version would produce.
write_new_release() {
  NEW_BINARY="$CASE_DIR/release/proxyctl-linux"
  mkdir -p "$(dirname "$NEW_BINARY")"
  make_fake_proxyctl "$NEW_BINARY" "v0.7.0" new
  NEW_SHA="$(sha256_file_test "$NEW_BINARY")"
  export NEW_BINARY NEW_SHA
}

# stub_downloader emits the staged release, or a corrupted copy when the test
# asks for one.
stub_downloader() {
  cat <<'STUB'
latest_proxyctl_tag() { printf '%s\n' "${FAKE_TAG:-v0.7.0}"; }
fetch_url() {
  local url="$1" dest="$2"
  case "${FAKE_DOWNLOAD:-ok}" in
    timeout) sleep "${FAKE_DOWNLOAD_DELAY:-5}"; return 1 ;;
    fail) return 1 ;;
  esac
  if [[ "$url" == *checksums.txt ]]; then
    printf '%s  proxyctl-linux-%s\n' "${FAKE_CHECKSUM:-$NEW_SHA}" "$(uname -m | sed 's/x86_64/amd64/;s/aarch64/arm64/;s/arm64/arm64/')" > "$dest"
  else
    cat "$NEW_BINARY" > "$dest"
  fi
  return 0
}
run_proxyctl_health() { "$1" health --json >/dev/null 2>&1; }
STUB
}

# run_update drives cmd_update in a subshell so die() keeps its exit semantics.
run_update() {
  set +e
  (
    set -euo pipefail
    # shellcheck source=/dev/null
    source "$HARNESS"
    eval "$(stub_downloader)"
    cmd_update "$@"
  ) > "$CASE_DIR/stdout" 2> "$CASE_DIR/stderr"
  UPDATE_STATUS=$?
  set -e
}

run_rollback() {
  set +e
  (
    set -euo pipefail
    # shellcheck source=/dev/null
    source "$HARNESS"
    eval "$(stub_downloader)"
    cmd_rollback "$@"
  ) > "$CASE_DIR/stdout" 2> "$CASE_DIR/stderr"
  ROLLBACK_STATUS=$?
  set -e
}

session_dir_of() {
  [[ -d "$UPDATES_DIR" ]] || return 0
  find "$UPDATES_DIR" -mindepth 1 -maxdepth 1 -type d | sort | tail -1
}

meta_value() {
  awk -v k="$2" 'index($0, k "=") == 1 { sub(/^[^=]*=/, ""); print; exit }' "$1"
}

mode_of() {
  if stat -f '%Lp' "$1" >/dev/null 2>&1; then
    stat -f '%Lp' "$1"
  else
    stat -c '%a' "$1"
  fi
}

# --- update: download and verification ------------------------------------

test_install_update_checksum_mismatch() {
  local status=0
  new_case checksum-mismatch
  write_new_release
  FAKE_CHECKSUM="$(printf 'a%.0s' {1..64})"
  export FAKE_CHECKSUM
  run_update
  unset FAKE_CHECKSUM

  [[ "$UPDATE_STATUS" -ne 0 ]] || { fail "a checksum mismatch was accepted"; status=1; }
  [[ "$(cat "$PROXYCTL_BIN")" == "$OLD_BINARY_TEXT" ]] || { fail "the live binary was replaced"; status=1; }
  [[ "$(cat "${PROXYCTL_BIN}.sha256")" == "$OLD_SIDECAR_TEXT" ]] || { fail "the live sidecar was replaced"; status=1; }
  grep -q "Checksum mismatch" "$CASE_DIR/stderr" || { fail "no checksum error reported"; status=1; }
  [[ -z "$(find /tmp -maxdepth 1 -name 'proxyctl-update.*' -newer "$CASE_DIR/actions.log" 2>/dev/null)" ]] || { fail "download temporaries were left behind"; status=1; }
  return "$status"
}

test_install_update_download_timeout() {
  local status=0
  new_case download-timeout
  write_new_release
  FAKE_DOWNLOAD=timeout FAKE_DOWNLOAD_DELAY=0 run_update

  [[ "$UPDATE_STATUS" -ne 0 ]] || { fail "a failed download was treated as success"; status=1; }
  [[ "$(cat "$PROXYCTL_BIN")" == "$OLD_BINARY_TEXT" ]] || { fail "the live binary was replaced"; status=1; }
  [[ -x "$PROXYCTL_BIN" ]] || { fail "the live binary is no longer executable"; status=1; }
  grep -q "Download failed" "$CASE_DIR/stderr" || { fail "no download error reported"; status=1; }
  return "$status"
}

test_install_update_sidecar_replaced_atomically() {
  local status=0
  new_case sidecar-atomic
  write_new_release
  run_update

  [[ "$UPDATE_STATUS" -eq 0 ]] || { fail "the upgrade failed: $(cat "$CASE_DIR/stderr")"; status=1; }
  [[ "$(cat "${PROXYCTL_BIN}.sha256")" == "$NEW_SHA" ]] || { fail "the sidecar does not hold the new checksum"; status=1; }
  [[ "$(sha256_file_test "$PROXYCTL_BIN")" == "$NEW_SHA" ]] || { fail "the binary is not the downloaded one"; status=1; }
  [[ "$(mode_of "$PROXYCTL_BIN")" == "755" ]] || { fail "binary mode is $(mode_of "$PROXYCTL_BIN"), want 755"; status=1; }
  [[ "$(mode_of "${PROXYCTL_BIN}.sha256")" == "644" ]] || { fail "sidecar mode is $(mode_of "${PROXYCTL_BIN}.sha256"), want 644"; status=1; }
  [[ -z "$(find "$CASE_DIR/bin" -name '.proxyctl-replace.*')" ]] || { fail "replacement temporaries were left behind"; status=1; }
  return "$status"
}

# --- update: sessions ------------------------------------------------------

test_install_update_success_retains_latest_rollback_session() {
  local status=0
  new_case retain-session
  write_new_release
  run_update

  [[ "$UPDATE_STATUS" -eq 0 ]] || { fail "the upgrade failed: $(cat "$CASE_DIR/stderr")"; status=1; }
  local session
  session="$(session_dir_of)"
  [[ -n "$session" ]] || { fail "no session was kept"; status=1; }
  [[ -f "$session/proxyctl.previous" ]] || { fail "the previous binary was not kept"; status=1; }
  [[ -f "$session/proxyctl.previous.sha256" ]] || { fail "the previous sidecar was not kept"; status=1; }
  [[ "$(meta_value "$session/session.meta" status)" == "success" ]] || { fail "session status is $(meta_value "$session/session.meta" status)"; status=1; }
  [[ "$(cat "$session/proxyctl.previous")" == "$OLD_BINARY_TEXT" ]] || { fail "the staged copy is not the previous binary"; status=1; }
  return "$status"
}

test_install_update_next_success_prunes_previous_session() {
  local status=0
  new_case prune-session
  write_new_release
  run_update
  local first
  first="$(session_dir_of)"
  [[ -n "$first" ]] || { fail "the first upgrade kept no session"; status=1; }

  # A second successful upgrade makes the first one the older success.
  run_update
  local second
  second="$(session_dir_of)"

  [[ "$UPDATE_STATUS" -eq 0 ]] || { fail "the second upgrade failed: $(cat "$CASE_DIR/stderr")"; status=1; }
  [[ "$second" != "$first" ]] || { fail "the second upgrade reused the first session"; status=1; }
  [[ ! -d "$first" ]] || { fail "the older successful session survived"; status=1; }
  [[ -d "$second" ]] || { fail "the newest session was pruned"; status=1; }
  return "$status"
}

test_install_update_session_permissions_and_atomic_metadata() {
  local status=0
  new_case session-permissions
  write_new_release

  # Fail the very first live replacement: the session, its metadata and the
  # staged pair must already be complete at that point.
  set +e
  (
    set -euo pipefail
    # shellcheck source=/dev/null
    source "$HARNESS"
    eval "$(stub_downloader)"
    replace_file_atomic() { return 1; }
    cmd_update
  ) > "$CASE_DIR/stdout" 2> "$CASE_DIR/stderr"
  UPDATE_STATUS=$?
  set -e

  [[ "$UPDATE_STATUS" -ne 0 ]] || { fail "a failed replacement was reported as success"; status=1; }
  local session
  session="$(session_dir_of)"
  [[ -n "$session" ]] || { fail "no session directory was created"; status=1; }
  [[ -f "$session/session.meta" ]] || { fail "metadata was not written before the replacement"; status=1; }
  [[ "$(mode_of "$session")" == "700" ]] || { fail "session mode is $(mode_of "$session"), want 700"; status=1; }
  local f
  for f in session.meta proxyctl.previous proxyctl.previous.sha256 config-snapshot.tar.gz; do
    [[ -f "$session/$f" ]] || { fail "$f is missing from the session"; status=1; continue; }
    [[ "$(mode_of "$session/$f")" == "600" ]] || { fail "$f mode is $(mode_of "$session/$f"), want 600"; status=1; }
  done
  [[ -z "$(find "$session" -name '.session.meta.*')" ]] || { fail "metadata temporaries were left behind"; status=1; }
  [[ "$(cat "$PROXYCTL_BIN")" == "$OLD_BINARY_TEXT" ]] || { fail "the live binary was replaced anyway"; status=1; }
  return "$status"
}

# --- update: rollback on a failed health check ----------------------------

test_install_update_healthcheck_failure_rolls_back_binary_sidecar_and_config() {
  local status=0
  new_case health-failure
  write_new_release
  FAKE_HEALTH=fail run_update

  [[ "$UPDATE_STATUS" -ne 0 ]] || { fail "a failed health check was reported as success"; status=1; }
  [[ "$(cat "$PROXYCTL_BIN")" == "$OLD_BINARY_TEXT" ]] || { fail "the previous binary was not restored"; status=1; }
  [[ "$(cat "${PROXYCTL_BIN}.sha256")" == "$OLD_SIDECAR_TEXT" ]] || { fail "the previous sidecar was not restored"; status=1; }
  [[ "$(mode_of "$PROXYCTL_BIN")" == "755" ]] || { fail "the restored binary mode is $(mode_of "$PROXYCTL_BIN")"; status=1; }
  [[ "$(mode_of "${PROXYCTL_BIN}.sha256")" == "644" ]] || { fail "the restored sidecar mode is $(mode_of "${PROXYCTL_BIN}.sha256")"; status=1; }
  grep -q "^new restore " "$FAKE_LOG" || { fail "the configuration was not restored by the new binary"; status=1; }
  grep -q "^old restore " "$FAKE_LOG" && { fail "the configuration was restored by the old binary, so the binary was put back too early"; status=1; }

  # The configuration rollback must run after the failed health check and
  # while the new binary is still live.
  local restore_line health_line
  health_line="$(grep -n "^new health$" "$FAKE_LOG" | head -1 | cut -d: -f1)"
  restore_line="$(grep -n "^new restore " "$FAKE_LOG" | head -1 | cut -d: -f1)"
  [[ -n "$health_line" && -n "$restore_line" && "$restore_line" -gt "$health_line" ]] || {
    fail "the configuration restore did not follow the failed health check"
    status=1
  }
  local session
  session="$(session_dir_of)"
  [[ "$(meta_value "$session/session.meta" status)" == "rolled_back" ]] || { fail "session status is $(meta_value "$session/session.meta" status)"; status=1; }
  return "$status"
}

test_install_update_staged_binary_checksum_mismatch_blocks_rollback() {
  local status=0
  new_case staged-corrupt

  # Corrupt the staged copy between staging and the rollback attempt.
  write_new_release
  set +e
  (
    set -euo pipefail
    # shellcheck source=/dev/null
    source "$HARNESS"
    eval "$(stub_downloader)"
    run_proxyctl_health() {
      local session
      session="$(find "$UPDATES_DIR" -mindepth 1 -maxdepth 1 -type d | sort | tail -1)"
      printf 'tampered\n' >> "$session/proxyctl.previous"
      return 1
    }
    cmd_update
  ) > "$CASE_DIR/stdout" 2> "$CASE_DIR/stderr"
  UPDATE_STATUS=$?
  set -e

  [[ "$UPDATE_STATUS" -ne 0 ]] || { fail "the upgrade reported success"; status=1; }
  grep -q "manual attention" "$CASE_DIR/stderr" || { fail "no manual-intervention message"; status=1; }
  [[ "$(sha256_file_test "$PROXYCTL_BIN")" == "$NEW_SHA" ]] || { fail "a corrupted copy was written over the live binary"; status=1; }
  local session
  session="$(session_dir_of)"
  [[ -d "$session" ]] || { fail "the session was removed after a failed rollback"; status=1; }
  [[ "$(meta_value "$session/session.meta" status)" == "rollback_failed" ]] || { fail "session status is $(meta_value "$session/session.meta" status)"; status=1; }
  return "$status"
}

test_install_update_old_binary_without_backup_restore_degrades_safely() {
  local status=0
  new_case no-backup-capability
  write_new_release
  FAKE_CAPS=nobackup run_update

  [[ "$UPDATE_STATUS" -eq 0 ]] || { fail "the binary-only upgrade failed: $(cat "$CASE_DIR/stderr")"; status=1; }
  grep -q "binary-only upgrade" "$CASE_DIR/stderr" || { fail "the degraded mode was not announced"; status=1; }
  grep -q "on-incapable-binary" "$FAKE_LOG" && { fail "a subcommand the old binary lacks was invoked"; status=1; }
  local session
  session="$(session_dir_of)"
  [[ -f "$session/proxyctl.previous" ]] || { fail "no binary rollback copy was staged"; status=1; }
  [[ ! -f "$session/config-snapshot.tar.gz" ]] || { fail "a configuration snapshot appeared without support for one"; status=1; }
  [[ -z "$(meta_value "$session/session.meta" config_snapshot)" ]] || { fail "the metadata claims a snapshot"; status=1; }
  return "$status"
}

test_install_update_interrupt_preserves_staged_binary() {
  local status=0
  new_case interrupt
  write_new_release

  local marker="$CASE_DIR/downloading"
  set +e
  (
    set -euo pipefail
    # shellcheck source=/dev/null
    source "$HARNESS"
    eval "$(stub_downloader)"
    fetch_url() {
      printf 'started\n' > "$marker"
      # Waiting on a background job lets the trap run the moment the signal
      # arrives, the way an interrupted download behaves.
      sleep 30 &
      wait $!
      return 1
    }
    cmd_update
  ) > "$CASE_DIR/stdout" 2> "$CASE_DIR/stderr" &
  local pid=$!

  local waited=0
  while [[ ! -f "$marker" && "$waited" -lt 100 ]]; do
    sleep 0.1
    waited=$((waited + 1))
  done
  kill -TERM "$pid" 2>/dev/null
  wait "$pid"
  UPDATE_STATUS=$?
  set -e

  [[ "$UPDATE_STATUS" -eq 130 ]] || { fail "the interrupted upgrade exited $UPDATE_STATUS, want 130"; status=1; }
  grep -q "Interrupted" "$CASE_DIR/stderr" || { fail "the interruption was not reported"; status=1; }
  [[ -z "$(find /tmp -maxdepth 1 -name 'proxyctl-update.*' -newer "$CASE_DIR/downloading" 2>/dev/null)" ]] || { fail "download temporaries survived the signal"; status=1; }
  local session
  session="$(session_dir_of)"
  [[ -n "$session" && -d "$session" ]] || { fail "the session was removed on the signal"; status=1; }
  [[ -f "$session/proxyctl.previous" ]] || { fail "the staged binary was removed on the signal"; status=1; }
  [[ -f "$session/session.meta" ]] || { fail "the session metadata was removed on the signal"; status=1; }
  [[ "$(cat "$PROXYCTL_BIN")" == "$OLD_BINARY_TEXT" ]] || { fail "the live binary changed before the download finished"; status=1; }
  return "$status"
}

# --- rollback --------------------------------------------------------------

test_install_rollback_no_snapshot_errors() {
  local status=0
  new_case rollback-empty
  run_rollback

  [[ "$ROLLBACK_STATUS" -ne 0 ]] || { fail "an empty rollback reported success"; status=1; }
  grep -q "no update session" "$CASE_DIR/stderr" || { fail "no explanation was given"; status=1; }

  run_rollback --to does-not-exist
  [[ "$ROLLBACK_STATUS" -ne 0 ]] || { fail "an unknown --to target reported success"; status=1; }
  grep -q "No update session or backup archive" "$CASE_DIR/stderr" || { fail "no explanation for the unknown target"; status=1; }
  return "$status"
}

test_install_rollback_config_only_reports_binary_unchanged() {
  local status=0
  new_case rollback-config-only
  printf 'archive\n' > "$BACKUPS_DIR/backup-20260909T100000Z.tar.gz"
  run_rollback --to backup-20260909T100000Z.tar.gz

  [[ "$ROLLBACK_STATUS" -eq 0 ]] || { fail "the configuration rollback failed: $(cat "$CASE_DIR/stderr")"; status=1; }
  grep -q "^old restore .*backup-20260909T100000Z.tar.gz$" "$FAKE_LOG" || { fail "restore was not called with the archive"; status=1; }
  grep -q "binary was not changed" "$CASE_DIR/stdout" || { fail "the unchanged binary was not reported"; status=1; }
  [[ "$(cat "$PROXYCTL_BIN")" == "$OLD_BINARY_TEXT" ]] || { fail "the binary was changed"; status=1; }
  return "$status"
}

test_install_rollback_restores_session_binary_and_config() {
  local status=0
  new_case rollback-session
  write_new_release
  run_update
  [[ "$UPDATE_STATUS" -eq 0 ]] || { fail "the upgrade failed: $(cat "$CASE_DIR/stderr")"; status=1; }
  : > "$FAKE_LOG"

  run_rollback
  [[ "$ROLLBACK_STATUS" -eq 0 ]] || { fail "the rollback failed: $(cat "$CASE_DIR/stderr")"; status=1; }
  [[ "$(cat "$PROXYCTL_BIN")" == "$OLD_BINARY_TEXT" ]] || { fail "the previous binary was not restored"; status=1; }
  [[ "$(cat "${PROXYCTL_BIN}.sha256")" == "$OLD_SIDECAR_TEXT" ]] || { fail "the previous sidecar was not restored"; status=1; }
  grep -q " restore " "$FAKE_LOG" || { fail "the session snapshot was not restored"; status=1; }
  return "$status"
}

# --- dispatch --------------------------------------------------------------

test_install_update_dispatches_before_domain_validation() {
  local status=0
  new_case dispatch-update
  local out
  out="$(
    set -euo pipefail
    # shellcheck source=/dev/null
    source "$HARNESS"
    cmd_install() { echo "cmd_install $*"; }
    cmd_update() { echo "cmd_update $*"; }
    cmd_rollback() { echo "cmd_rollback $*"; }
    main update
  )"
  [[ "$out" == "cmd_update " || "$out" == "cmd_update" ]] || { fail "update dispatched to '$out'"; status=1; }

  # The installation path keeps its existing syntax.
  out="$(
    set -euo pipefail
    # shellcheck source=/dev/null
    source "$HARNESS"
    cmd_install() { echo "cmd_install $*"; }
    cmd_update() { echo "cmd_update $*"; }
    cmd_rollback() { echo "cmd_rollback $*"; }
    main sub.example.com admin@example.com
  )"
  [[ "$out" == "cmd_install sub.example.com admin@example.com" ]] || { fail "install dispatched to '$out'"; status=1; }

  out="$(
    set -euo pipefail
    # shellcheck source=/dev/null
    source "$HARNESS"
    cmd_install() { echo "cmd_install $*"; }
    cmd_update() { echo "cmd_update $*"; }
    cmd_rollback() { echo "cmd_rollback $*"; }
    main install --domain sub.example.com
  )"
  [[ "$out" == "cmd_install --domain sub.example.com" ]] || { fail "explicit install dispatched to '$out'"; status=1; }
  return "$status"
}

test_install_rollback_dispatches_to_cmd_rollback() {
  local status=0
  new_case dispatch-rollback
  local out
  out="$(
    set -euo pipefail
    # shellcheck source=/dev/null
    source "$HARNESS"
    cmd_install() { echo "cmd_install $*"; }
    cmd_update() { echo "cmd_update $*"; }
    cmd_rollback() { echo "cmd_rollback $*"; }
    main rollback --to session-1
  )"
  [[ "$out" == "cmd_rollback --to session-1" ]] || { fail "rollback dispatched to '$out'"; status=1; }

  # An invalid --to must not fall through into the installation body.
  set +e
  (
    set -euo pipefail
    # shellcheck source=/dev/null
    source "$HARNESS"
    cmd_install() { echo "cmd_install $*"; }
    main rollback --to
  ) > "$CASE_DIR/stdout" 2> "$CASE_DIR/stderr"
  local rc=$?
  set -e
  [[ "$rc" -ne 0 ]] || { fail "a missing --to value was accepted"; status=1; }
  grep -q "Missing value for --to" "$CASE_DIR/stderr" || { fail "no message for the missing --to value"; status=1; }
  grep -q "cmd_install" "$CASE_DIR/stdout" && { fail "a bad rollback fell through to the installer"; status=1; }
  return "$status"
}

# --- retention of failed and interrupted sessions -------------------------

test_install_update_success_keeps_failed_sessions() {
  local status=0
  new_case keep-failed
  write_new_release

  # A session left behind by an earlier failure, and one left mid-flight.
  local failed="$UPDATES_DIR/20260101T000000Z"
  local stalled="$UPDATES_DIR/20260102T000000Z"
  mkdir -p "$failed" "$stalled"
  printf 'schema_version=1\nstatus=failed\n' > "$failed/session.meta"
  printf 'schema_version=1\nstatus=in_progress\n' > "$stalled/session.meta"

  run_update
  [[ "$UPDATE_STATUS" -eq 0 ]] || { fail "the upgrade failed: $(cat "$CASE_DIR/stderr")"; status=1; }
  [[ -d "$failed" ]] || { fail "a failed session was pruned by a later success"; status=1; }
  [[ -d "$stalled" ]] || { fail "an interrupted session was pruned by a later success"; status=1; }
  return "$status"
}

test_install_rollback_ignores_incomplete_sessions() {
  local status=0
  new_case incomplete-session
  write_new_release
  run_update
  [[ "$UPDATE_STATUS" -eq 0 ]] || { fail "the upgrade failed: $(cat "$CASE_DIR/stderr")"; status=1; }

  # A later upgrade that died before writing its metadata must not become the
  # target of a bare rollback.
  local incomplete="$UPDATES_DIR/29991231T235959Z"
  mkdir -p "$incomplete"
  printf 'half\n' > "$incomplete/proxyctl.previous"

  : > "$FAKE_LOG"
  run_rollback
  [[ "$ROLLBACK_STATUS" -eq 0 ]] || { fail "the rollback failed: $(cat "$CASE_DIR/stderr")"; status=1; }
  [[ "$(cat "$PROXYCTL_BIN")" == "$OLD_BINARY_TEXT" ]] || { fail "the rollback did not use the last complete session"; status=1; }
  return "$status"
}

test_install_update_production_paths_carry_timeouts() {
  local status=0
  # The stubs replace these two functions in every scenario, so assert on the
  # production source that the bounded forms are still there.
  local src
  src="$(awk '/^fetch_url\(\)/,/^}/' "$REPO_ROOT/install-proxy.sh")"
  [[ "$src" == *'--max-time'* ]] || { fail "fetch_url lost its download timeout"; status=1; }
  src="$(awk '/^run_proxyctl_health\(\)/,/^}/' "$REPO_ROOT/install-proxy.sh")"
  [[ "$src" == *'timeout '* ]] || { fail "run_proxyctl_health lost its timeout wrapper"; status=1; }
  [[ "$src" == *'health --json'* ]] || { fail "run_proxyctl_health no longer runs health --json"; status=1; }
  return "$status"
}

test_install_help_works_without_root() {
  local status=0
  new_case help-without-root
  local out rc
  set +e
  out="$(bash "$REPO_ROOT/install-proxy.sh" --help 2>&1)"
  rc=$?
  set -e
  [[ "$rc" -eq 0 ]] || { fail "--help exited $rc for a non-root caller"; status=1; }
  [[ "$out" == *"install-proxy.sh update"* ]] || { fail "--help does not document the subcommands: $out"; status=1; }
  [[ "$out" != *"run as root"* ]] || { fail "--help demanded root"; status=1; }

  set +e
  out="$(bash "$REPO_ROOT/install-proxy.sh" --domain 2>&1)"
  rc=$?
  set -e
  [[ "$rc" -ne 0 ]] || { fail "a missing --domain value was accepted"; status=1; }
  [[ "$out" == *"Missing value for --domain"* ]] || { fail "argument errors no longer reach a non-root caller: $out"; status=1; }
  return "$status"
}

run_case test_install_update_checksum_mismatch
run_case test_install_update_download_timeout
run_case test_install_update_sidecar_replaced_atomically
run_case test_install_update_success_retains_latest_rollback_session
run_case test_install_update_next_success_prunes_previous_session
run_case test_install_update_session_permissions_and_atomic_metadata
run_case test_install_update_healthcheck_failure_rolls_back_binary_sidecar_and_config
run_case test_install_update_staged_binary_checksum_mismatch_blocks_rollback
run_case test_install_update_old_binary_without_backup_restore_degrades_safely
run_case test_install_update_interrupt_preserves_staged_binary
run_case test_install_rollback_no_snapshot_errors
run_case test_install_rollback_config_only_reports_binary_unchanged
run_case test_install_rollback_restores_session_binary_and_config
run_case test_install_update_dispatches_before_domain_validation
run_case test_install_rollback_dispatches_to_cmd_rollback
run_case test_install_update_success_keeps_failed_sessions
run_case test_install_rollback_ignores_incomplete_sessions
run_case test_install_update_production_paths_carry_timeouts
run_case test_install_help_works_without_root

if [[ "$FAILED" -ne 0 ]]; then
  echo "$FAILED scenario(s) failed" >&2
  exit 1
fi
echo "All $PASSED update/rollback scenarios passed"
