#!/usr/bin/env bash
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
TEST_DIR="$(mktemp -d)"
trap 'rm -rf "$TEST_DIR"' EXIT

EXPECTED_CADDY_KEYRING="/usr/share/keyrings/caddy-stable-archive-keyring.gpg"
EXPECTED_CADDY_APT_LIST="/etc/apt/sources.list.d/caddy-stable.list"
EXPECTED_CADDY_GPG_URL="https://dl.cloudsmith.io/public/caddy/stable/gpg.key"
EXPECTED_CADDY_APT_SOURCE_URL="https://dl.cloudsmith.io/public/caddy/stable/debian.deb.txt"
EXPECTED_CADDY_SIGNING_FINGERPRINT="2F5C3BE9886ACD2913299EFBABA1F9B8875A6661"
EXPECTED_CADDY_REPOSITORY_URL="https://dl.cloudsmith.io/public/caddy/stable/deb/debian"

extract_function() {
  local function_name="$1"
  awk -v name="$function_name" '
    $0 ~ "^" name "\\(\\)[[:space:]]*\\{" { active=1 }
    active {
      print
      line=$0
      opens=gsub(/\{/, "", line)
      line=$0
      closes=gsub(/\}/, "", line)
      depth += opens - closes
      if (depth == 0) exit
    }
  ' "$REPO_ROOT/install-proxy.sh"
}

HARNESS="$TEST_DIR/caddy_harness.sh"
{
  printf '%s\n' '#!/usr/bin/env bash'
  printf '%s\n' 'log() { echo "[INFO] $1"; }'
  printf '%s\n' 'log_warn() { echo "[WARN] $1" >&2; }'
  printf '%s\n' 'log_error() { echo "[ERROR] $1" >&2; }'
  printf '%s\n' 'die() { echo "[FATAL] $1" >&2; exit 1; }'
  for constant in CADDY_KEYRING CADDY_APT_LIST CADDY_GPG_URL CADDY_APT_SOURCE_URL CADDY_SIGNING_FINGERPRINT; do
    line="$(grep -E "^${constant}=\"[^\"]+\"$" "$REPO_ROOT/install-proxy.sh" || true)"
    [[ -n "$line" ]] || { echo "Missing production constant: $constant" >&2; exit 1; }
    printf '%s\n' "$line"
  done
  printf '%s\n' 'CADDY_APT_BACKUP_ORIGINALS=()'
  printf '%s\n' 'CADDY_APT_BACKUP_FILES=()'
  printf '%s\n' 'CADDY_APT_BOOTSTRAP_ORIGINALS=()'
  printf '%s\n' 'CADDY_APT_BOOTSTRAP_FILES=()'
  extract_function validate_caddy_apt_source
  extract_function backup_caddy_apt_state
  extract_function restore_caddy_apt_state
  extract_function discard_caddy_apt_state_backup
  extract_function prepare_caddy_apt_bootstrap
  extract_function restore_caddy_apt_bootstrap
  extract_function apt_update_or_die
  extract_function bootstrap_caddy_apt_before_dependencies
  extract_function install_caddy_apt_source
} > "$HARNESS"

# shellcheck disable=SC1090
source "$HARNESS"

[[ "$CADDY_KEYRING" == "$EXPECTED_CADDY_KEYRING" ]]
[[ "$CADDY_APT_LIST" == "$EXPECTED_CADDY_APT_LIST" ]]
[[ "$CADDY_GPG_URL" == "$EXPECTED_CADDY_GPG_URL" ]]
[[ "$CADDY_APT_SOURCE_URL" == "$EXPECTED_CADDY_APT_SOURCE_URL" ]]
[[ "$CADDY_SIGNING_FINGERPRINT" == "$EXPECTED_CADDY_SIGNING_FINGERPRINT" ]]
declare -F validate_caddy_apt_source >/dev/null
declare -F prepare_caddy_apt_bootstrap >/dev/null
declare -F restore_caddy_apt_bootstrap >/dev/null
declare -F bootstrap_caddy_apt_before_dependencies >/dev/null
declare -F install_caddy_apt_source >/dev/null

# Regression: cloudsmith now ships a `deb-src` line alongside `deb` in
# debian.deb.txt. validate_caddy_apt_source must accept that exact pair,
# stay backward compatible with a lone `deb`, and still reject anything else.
validate_source_regression() {
  local dir="$TEST_DIR/validate-source" f
  mkdir -p "$dir"
  local deb="deb [signed-by=$CADDY_KEYRING] $EXPECTED_CADDY_REPOSITORY_URL any-version main"
  local deb_src="deb-src [signed-by=$CADDY_KEYRING] $EXPECTED_CADDY_REPOSITORY_URL any-version main"
  local rogue="deb [signed-by=$CADDY_KEYRING] https://evil.example.com/x any-version main"

  f="$dir/deb-and-deb-src.list"
  printf '# Source: Caddy\n\n%s\n\n%s\n' "$deb" "$deb_src" > "$f"
  validate_caddy_apt_source "$f" || { echo "validate rejected official deb+deb-src" >&2; exit 1; }

  f="$dir/deb-only.list"
  printf '%s\n' "$deb" > "$f"
  validate_caddy_apt_source "$f" || { echo "validate rejected legacy deb-only" >&2; exit 1; }

  f="$dir/deb-src-rogue.list"
  printf '%s\n%s\n%s\n' "$deb" "$deb_src" "$rogue" > "$f"
  ! validate_caddy_apt_source "$f" || { echo "validate accepted rogue entry" >&2; exit 1; }

  f="$dir/deb-src-only.list"
  printf '%s\n' "$deb_src" > "$f"
  ! validate_caddy_apt_source "$f" || { echo "validate accepted deb-src without deb" >&2; exit 1; }

  echo "validate-source-accepts-official-deb-src PASSED"
}
validate_source_regression

run_failure_scenario() {
  local name="$1"
  local failure="$2"
  local root="$TEST_DIR/$name"
  mkdir -p "$root/etc/apt/sources.list.d" "$root/usr/share/keyrings"
  CADDY_APT_LIST="$root/etc/apt/sources.list.d/caddy-stable.list"
  CADDY_KEYRING="$root/usr/share/keyrings/caddy-stable-archive-keyring.gpg"
  printf 'old source\n' > "$CADDY_APT_LIST"
  printf 'old key\n' > "$CADDY_KEYRING"

  (
    curl() {
      local url="" output=""
      while [[ $# -gt 0 ]]; do
        case "$1" in
          -o) output="$2"; shift 2 ;;
          http*) url="$1"; shift ;;
          *) shift ;;
        esac
      done
      [[ "$failure" == "key-download" && "$url" == "$CADDY_GPG_URL" ]] && return 22
      [[ "$failure" == "source-download" && "$url" == "$CADDY_APT_SOURCE_URL" ]] && return 22
      if [[ "$url" == "$CADDY_GPG_URL" ]]; then
        printf 'downloaded key\n' > "$output"
      else
        if [[ "$failure" == "bad-signed-by" ]]; then
          printf 'deb [signed-by=/wrong/key.gpg] %s any-version main\n' "$EXPECTED_CADDY_REPOSITORY_URL" > "$output"
        elif [[ "$failure" == "bad-url" ]]; then
          printf 'deb [signed-by=%s] https://example.invalid stable main\n' "$CADDY_KEYRING" > "$output"
        elif [[ "$failure" == "extra-source" ]]; then
          printf 'deb [signed-by=%s] %s any-version main\n' "$CADDY_KEYRING" "$EXPECTED_CADDY_REPOSITORY_URL" > "$output"
          printf 'deb https://example.invalid stable main\n' >> "$output"
        else
          printf 'deb [signed-by=%s] %s any-version main\n' "$CADDY_KEYRING" "$EXPECTED_CADDY_REPOSITORY_URL" > "$output"
        fi
      fi
    }
    gpg() {
      if [[ " $* " == *" --show-keys "* ]]; then
        local fingerprint="$CADDY_SIGNING_FINGERPRINT"
        [[ "$failure" == "bad-fingerprint" ]] && fingerprint="0000000000000000000000000000000000000000"
        printf 'fpr:::::::::%s:\n' "$fingerprint"
        return 0
      fi
      [[ "$failure" == "dearmor" ]] && return 1
      local output=""
      while [[ $# -gt 0 ]]; do
        case "$1" in --output) output="$2"; shift 2 ;; *) shift ;; esac
      done
      printf 'new key\n' > "$output"
    }
    install_caddy_apt_source
  ) >/dev/null 2>&1 && { echo "$name unexpectedly succeeded" >&2; return 1; }

  [[ "$(cat "$CADDY_APT_LIST")" == "old source" ]]
  [[ "$(cat "$CADDY_KEYRING")" == "old key" ]]
  ! find "$(dirname "$CADDY_APT_LIST")" "$(dirname "$CADDY_KEYRING")" -type f -name '*.tmp.*' -print -quit | grep -q .
  echo "$name PASSED"
}

run_failure_scenario key-download-preserves-existing key-download
run_failure_scenario source-download-preserves-existing source-download
run_failure_scenario fingerprint-preserves-existing bad-fingerprint
run_failure_scenario signed-by-preserves-existing bad-signed-by
run_failure_scenario repository-url-preserves-existing bad-url
run_failure_scenario extra-active-entry-preserves-existing extra-source
run_failure_scenario dearmor-preserves-existing dearmor

rollback_root="$TEST_DIR/rollback"
mkdir -p "$rollback_root/etc/apt/sources.list.d" "$rollback_root/usr/share/keyrings"
CADDY_APT_LIST="$rollback_root/etc/apt/sources.list.d/caddy-stable.list"
CADDY_KEYRING="$rollback_root/usr/share/keyrings/caddy-stable-archive-keyring.gpg"
printf 'old source\n' > "$CADDY_APT_LIST"
printf 'old key\n' > "$CADDY_KEYRING"
curl() {
  local output=""
  while [[ $# -gt 0 ]]; do case "$1" in -o) output="$2"; shift 2 ;; *) shift ;; esac; done
  [[ "$output" == *key* ]] && printf 'downloaded key\n' > "$output" || printf 'deb [signed-by=%s] %s any-version main\n' "$CADDY_KEYRING" "$EXPECTED_CADDY_REPOSITORY_URL" > "$output"
}
gpg() {
  if [[ " $* " == *" --show-keys "* ]]; then printf 'fpr:::::::::%s:\n' "$CADDY_SIGNING_FINGERPRINT"; return; fi
  local output=""; while [[ $# -gt 0 ]]; do case "$1" in --output) output="$2"; shift 2 ;; *) shift ;; esac; done
  printf 'new key\n' > "$output"
}
mv() {
  local destination="${!#}"
  if [[ "$destination" == "$CADDY_APT_LIST" && "${mv_fail_once:-0}" == "1" ]]; then
    mv_fail_once=0
    return 1
  fi
  command mv "$@"
}
mv_fail_once=1
install_caddy_apt_source >/dev/null 2>&1 && { echo "activation-rollback unexpectedly succeeded" >&2; exit 1; }
[[ -f "$CADDY_APT_LIST" && -f "$CADDY_KEYRING" ]]
grep -Fxq 'old source' "$CADDY_APT_LIST"
grep -Fxq 'old key' "$CADDY_KEYRING"
unset -f mv curl gpg
echo "activation-rollback-preserves-existing PASSED"

bootstrap_root="$TEST_DIR/bootstrap"
mkdir -p "$bootstrap_root/etc/apt/sources.list.d" "$bootstrap_root/usr/share/keyrings"
CADDY_APT_LIST="$bootstrap_root/etc/apt/sources.list.d/caddy-stable.list"
CADDY_KEYRING="$bootstrap_root/usr/share/keyrings/caddy-stable-archive-keyring.gpg"
printf 'deb [signed-by=%s] %s any-version main\n' "$CADDY_KEYRING" "$EXPECTED_CADDY_REPOSITORY_URL" > "$CADDY_APT_LIST"
printf 'old key\n' > "$CADDY_KEYRING"
apt-get() {
  if [[ -e "$CADDY_APT_LIST" ]]; then
    echo 'NO_PUBKEY ABA1F9B8875A6661' >&2
    return 100
  fi
  return 0
}
LOG_FILE="$bootstrap_root/install.log"
bootstrap_caddy_apt_before_dependencies
[[ ! -e "$CADDY_APT_LIST" && ! -e "$CADDY_KEYRING" ]]
restore_caddy_apt_bootstrap
[[ -f "$CADDY_APT_LIST" && -f "$CADDY_KEYRING" ]]
unset -f apt-get
echo "bootstrap-quarantines-and-restores-bad-source PASSED"

bootstrap_rollback_root="$TEST_DIR/bootstrap-rollback"
mkdir -p "$bootstrap_rollback_root/etc/apt/sources.list.d" "$bootstrap_rollback_root/usr/share/keyrings"
CADDY_APT_LIST="$bootstrap_rollback_root/etc/apt/sources.list.d/caddy-stable.list"
CADDY_KEYRING="$bootstrap_rollback_root/usr/share/keyrings/caddy-stable-archive-keyring.gpg"
printf 'old source\n' > "$CADDY_APT_LIST"
printf 'old key\n' > "$CADDY_KEYRING"
prepare_caddy_apt_bootstrap
curl() {
  local output=""; while [[ $# -gt 0 ]]; do case "$1" in -o) output="$2"; shift 2 ;; *) shift ;; esac; done
  [[ "$output" == *key* ]] && printf 'downloaded key\n' > "$output" || printf 'deb [signed-by=%s] %s any-version main\n' "$CADDY_KEYRING" "$EXPECTED_CADDY_REPOSITORY_URL" > "$output"
}
gpg() {
  if [[ " $* " == *" --show-keys "* ]]; then printf 'fpr:::::::::%s:\n' "$CADDY_SIGNING_FINGERPRINT"; return; fi
  local output=""; while [[ $# -gt 0 ]]; do case "$1" in --output) output="$2"; shift 2 ;; *) shift ;; esac; done
  printf 'new key\n' > "$output"
}
install_caddy_apt_source
restore_caddy_apt_bootstrap
[[ -f "$CADDY_APT_LIST" && -f "$CADDY_KEYRING" ]]
grep -Fxq 'old source' "$CADDY_APT_LIST"
grep -Fxq 'old key' "$CADDY_KEYRING"
unset -f curl gpg
echo "bootstrap-restores-state-after-post-activation-failure PASSED"

success_root="$TEST_DIR/success"
mkdir -p "$success_root/etc/apt/sources.list.d" "$success_root/usr/share/keyrings"
CADDY_APT_LIST="$success_root/etc/apt/sources.list.d/caddy-stable.list"
CADDY_KEYRING="$success_root/usr/share/keyrings/caddy-stable-archive-keyring.gpg"
printf 'old source\n' > "$CADDY_APT_LIST"
printf 'old key\n' > "$CADDY_KEYRING"
curl() {
  local url="" output=""
  while [[ $# -gt 0 ]]; do
    case "$1" in -o) output="$2"; shift 2 ;; http*) url="$1"; shift ;; *) shift ;; esac
  done
  if [[ "$url" == "$CADDY_GPG_URL" ]]; then
    printf 'downloaded key\n' > "$output"
  else
    printf 'deb [signed-by=%s] %s any-version main\n' "$CADDY_KEYRING" "$EXPECTED_CADDY_REPOSITORY_URL" > "$output"
  fi
}
gpg() {
  if [[ " $* " == *" --show-keys "* ]]; then
    printf 'fpr:::::::::%s:\n' "$CADDY_SIGNING_FINGERPRINT"
    return 0
  fi
  local output=""
  while [[ $# -gt 0 ]]; do case "$1" in --output) output="$2"; shift 2 ;; *) shift ;; esac; done
  printf 'new key\n' > "$output"
}
install_caddy_apt_source
[[ "$(cat "$CADDY_KEYRING")" == "new key" ]]
grep -Fq "$EXPECTED_CADDY_REPOSITORY_URL" "$CADDY_APT_LIST"
echo "atomic-success PASSED"

if [[ "${CADDY_APT_SMOKE:-0}" == "1" ]]; then
  [[ "$(id -u)" -eq 0 ]] || { echo "CADDY_APT_SMOKE requires root" >&2; exit 1; }
  unset -f curl gpg
  # Restore the real production paths after isolated tests.
  CADDY_KEYRING="$EXPECTED_CADDY_KEYRING"
  CADDY_APT_LIST="$EXPECTED_CADDY_APT_LIST"
  CADDY_GPG_URL="$EXPECTED_CADDY_GPG_URL"
  CADDY_APT_SOURCE_URL="$EXPECTED_CADDY_APT_SOURCE_URL"
  CADDY_SIGNING_FINGERPRINT="$EXPECTED_CADDY_SIGNING_FINGERPRINT"
  rm -f "$EXPECTED_CADDY_KEYRING"
  printf 'deb [signed-by=%s] %s any-version main\n' "$EXPECTED_CADDY_KEYRING" "$EXPECTED_CADDY_REPOSITORY_URL" > "$EXPECTED_CADDY_APT_LIST"
  if apt-get update -y >/dev/null 2>&1; then
    echo "bad Caddy source did not make apt-get update fail" >&2
    exit 1
  fi
  bootstrap_caddy_apt_before_dependencies
  [[ ! -e "$EXPECTED_CADDY_APT_LIST" && ! -e "$EXPECTED_CADDY_KEYRING" ]]
  install_caddy_apt_source || { restore_caddy_apt_bootstrap; exit 1; }
  grep -Fxq "deb [signed-by=$EXPECTED_CADDY_KEYRING] $EXPECTED_CADDY_REPOSITORY_URL any-version main" "$EXPECTED_CADDY_APT_LIST"
  gpg --show-keys --with-colons "$EXPECTED_CADDY_KEYRING" |
    awk -F: -v expected="$EXPECTED_CADDY_SIGNING_FINGERPRINT" '$1 == "fpr" && $10 == expected { found=1 } END { exit(found ? 0 : 1) }'
  apt_update_or_die || { restore_caddy_apt_bootstrap; exit 1; }
  discard_caddy_apt_state_backup "$CADDY_APT_BOOTSTRAP_DIR"
  CADDY_APT_BOOTSTRAP_DIR=""
  echo "real-container-smoke PASSED"
fi

echo "All Caddy APT tests passed"
