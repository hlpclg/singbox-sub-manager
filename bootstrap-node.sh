#!/usr/bin/env bash
# bootstrap-node.sh: first-time setup of a standalone Reality node host.
#
# Installs the pinned proxyctl release (checksum- and version-verified, atomic
# rename, .sha256 sidecar in the installer's layout), then hands over to
#
#   proxyctl node                          (no arguments, on a terminal)
#   proxyctl node service install ARGS...  (with arguments)
#
# `bootstrap-node.sh --upgrade` (the only argument) is the node-host upgrade
# mode: it replaces an older, verified proxyctl with the pinned release after
# a read-only compatibility check of the candidate against the local Reality
# instance, keeps the previous binary/sidecar pair, and never exec's proxyctl.
#
# Spec: docs/superpowers/specs/2026-09-18-v0.10-reality-node-install-design.md
# §12 (first-install and upgrade mode), §11.5 (L1 is the shared proxyctl
# binary lock), §13.1 (exit codes), §13.2 (timeouts).
set -euo pipefail

# Fixed for this script. Deliberately NOT overridable from the environment or
# the command line: a bootstrap script installs exactly the release it was
# published with.
PROXYCTL_VERSION="v0.10.0"
PROXYCTL_REPOSITORY="hlpclg/singbox-sub-manager"
MIN_NODE_VERSION="v0.10.0"
DOWNLOAD_TIMEOUT=120

# Paths follow the env-override convention of install-proxy.sh and
# merge-nodes.sh, so tests inject temporary paths. Production defaults are the
# fixed system paths of the spec.
LOCK_FILE="${LOCK_FILE:-/run/lock/singbox-sub-manager.lock}"
LOCK_WAIT="${LOCK_WAIT:-30}"
PROXYCTL_BIN="${PROXYCTL_BIN:-/usr/local/bin/proxyctl}"
INSTALL_JSON="${INSTALL_JSON:-/etc/singbox-sub-manager/install.json}"
TOKEN_FILE="${TOKEN_FILE:-/var/lib/singbox-sub-manager/token}"
NODES_CONF="${NODES_CONF:-/etc/singbox-sub-manager/nodes.conf}"
OS_RELEASE_FILE="${OS_RELEASE_FILE:-/etc/os-release}"
SIDECAR="${PROXYCTL_BIN}.sha256"

# Upgrade mode only. Production defaults are the fixed system paths of the
# spec (§4.3, §5.1, §11, §12); tests inject temporary paths the same way.
REALITY_LOCK_FILE="${REALITY_LOCK_FILE:-/run/lock/proxyctl-reality.lock}"
L3_WAIT="${L3_WAIT:-10}"
SHOW_TIMEOUT="${SHOW_TIMEOUT:-15}"
REALITY_STATE_DIR="${REALITY_STATE_DIR:-/var/lib/proxyctl-reality}"
REALITY_CONFIG_DIR="${REALITY_CONFIG_DIR:-/etc/proxyctl-reality}"
REALITY_UNIT_FILE="${REALITY_UNIT_FILE:-/etc/systemd/system/proxyctl-reality.service}"
REALITY_BIN_DIR="${REALITY_BIN_DIR:-/usr/local/lib/proxyctl-reality}"
REALITY_TXN_DIR="${REALITY_TXN_DIR:-/var/lib/proxyctl-reality-txn}"
REALITY_TOMB_GLOB="${REALITY_TOMB_GLOB:-/var/lib/.proxyctl-reality-txn.tomb-*}"
PREVIOUS_DIR="${PREVIOUS_DIR:-/var/lib/proxyctl-bootstrap/previous}"

ARGS=()
REPLACE_PROXYCTL=0
ARCH=""
NEED_INSTALL=0
TMP_FILES=()
BG_PID=""
BG_GROUP=0
SIGNAL_SEEN=0
UPGRADE_OK=0
CAND_BIN=""
CAND_SIDE=""
CAND_HASH=""
OLD_HASH=""
OLD_VERSION=""
RESTORE_BIN=""
RESTORE_SIDE=""

die() {
  echo "ERROR: $*" >&2
  exit 1
}

usage() {
  cat >&2 <<'EOF'
usage:
  sudo bash bootstrap-node.sh
      interactive menu (needs a terminal on stdin and stdout)
  sudo bash bootstrap-node.sh [--replace-proxyctl] --name NAME --server ADDR [--port PORT] [--sni SNI]
      install the Reality node service; the arguments are passed to
      'proxyctl node service install'
  sudo bash bootstrap-node.sh --upgrade
      upgrade an installed, verified older proxyctl to this script's release
      (--upgrade must be the only argument)
  --replace-proxyctl only repairs a proxyctl whose .sha256 file is missing or
  does not match; it never replaces or downgrades a verified proxyctl.
EOF
}

# --- cleanup and signals ------------------------------------------------------

cleanup() {
  trap - EXIT
  if [[ -n "$BG_PID" ]]; then
    # A child started in its own session/process group (the compatibility
    # check) is killed together with all its descendants, and reaped, before
    # anything else (temp files, locks) is touched.
    if [[ "$BG_GROUP" -eq 1 ]]; then
      kill -KILL -- "-$BG_PID" 2>/dev/null || true
    fi
    kill "$BG_PID" 2>/dev/null || true
    wait "$BG_PID" 2>/dev/null || true
    BG_PID=""
    BG_GROUP=0
  fi
  if [[ ${#TMP_FILES[@]} -gt 0 ]]; then
    rm -f -- "${TMP_FILES[@]}"
  fi
}
trap cleanup EXIT
# Closing fd 9 on exit releases L1; INT/TERM just exit with 1 so the EXIT trap
# removes temp files, terminates an in-flight curl/flock and releases the lock.
trap 'exit 1' INT TERM

# wait_bg runs on the pid in BG_PID. The command is started in the background
# and waited for, so a trapped signal interrupts the wait immediately instead
# of after the foreground child finishes.
wait_bg() {
  local rc=0
  wait "$BG_PID" || rc=$?
  BG_PID=""
  BG_GROUP=0
  return "$rc"
}

# bg_arm / bg_disarm bracket the launch of a background child. Bash runs a
# trap only between commands, so a signal could otherwise be handled after the
# child exists but before BG_PID records it, leaving an orphan that nothing
# kills. While armed, a signal only sets a flag; bg_disarm restores the
# "exit 1" handler and then honours a flag that was set in between (the EXIT
# trap sees BG_PID and reaps the child). Used only before replacement starts.
bg_arm() {
  SIGNAL_SEEN=0
  trap 'SIGNAL_SEEN=1' INT TERM
}
bg_disarm() {
  trap 'exit 1' INT TERM
  if [[ "$SIGNAL_SEEN" -ne 0 ]]; then
    exit 1
  fi
}

# shielded runs a command (or function) in a subshell that ignores INT/TERM,
# so a terminal Ctrl-C delivered to the whole foreground process group cannot
# kill it half way through the replacement.
shielded() {
  ( trap '' INT TERM; "$@" )
}

# --- helpers -------------------------------------------------------------------

sha256_file() {
  if command -v sha256sum >/dev/null 2>&1; then
    sha256sum "$1" | awk '{print $1}'
  elif command -v shasum >/dev/null 2>&1; then
    shasum -a 256 "$1" | awk '{print $1}'
  else
    return 1
  fi
}

# parse_version V sets VER_PARTS to (major minor patch); strict vX.Y.Z only,
# so "dev", "v0.10" and pre-release suffixes are all "unparseable".
VER_PARTS=()
parse_version() {
  if [[ "$1" =~ ^v(0|[1-9][0-9]{0,8})\.(0|[1-9][0-9]{0,8})\.(0|[1-9][0-9]{0,8})$ ]]; then
    VER_PARTS=("${BASH_REMATCH[1]}" "${BASH_REMATCH[2]}" "${BASH_REMATCH[3]}")
    return 0
  fi
  VER_PARTS=()
  return 1
}

# version_cmp A B prints -1, 0 or 1 (numeric, per component). Both must parse.
version_cmp() {
  local -a a b
  local i
  parse_version "$1" || return 1
  a=("${VER_PARTS[@]}")
  parse_version "$2" || return 1
  b=("${VER_PARTS[@]}")
  for i in 0 1 2; do
    if ((a[i] < b[i])); then echo -1; return 0; fi
    if ((a[i] > b[i])); then echo 1; return 0; fi
  done
  echo 0
}

# --- host checks ----------------------------------------------------------------

require_root() {
  if [[ "$(id -u)" -ne 0 ]]; then
    die "Please run as root (sudo bash bootstrap-node.sh ...)"
  fi
}

# Same supported range as install-proxy.sh's require_supported_os.
require_supported_os() {
  local os_id os_ver
  [[ -f "$OS_RELEASE_FILE" ]] || die "Could not determine OS"
  # shellcheck disable=SC1090
  os_id="$(. "$OS_RELEASE_FILE" >/dev/null 2>&1; printf '%s' "${ID:-}")"
  # shellcheck disable=SC1090
  os_ver="$(. "$OS_RELEASE_FILE" >/dev/null 2>&1; printf '%s' "${VERSION_ID:-}")"
  if [[ "$os_id" == "ubuntu" ]]; then
    [[ "$os_ver" == "22.04" || "$os_ver" == "24.04" ]] || die "Only Ubuntu 22.04 and 24.04 are supported"
  elif [[ "$os_id" == "debian" ]]; then
    [[ "$os_ver" == "12" ]] || die "Only Debian 12 is supported"
  else
    die "Only Ubuntu and Debian are supported"
  fi
}

require_supported_arch() {
  case "$(uname -m)" in
    x86_64|amd64) ARCH=amd64 ;;
    aarch64|arm64) ARCH=arm64 ;;
    *) die "Unsupported architecture" ;;
  esac
}

# Spec §4.2, role "no": install.json, the default token file and the default
# nodes.conf are all absent. Anything else is "yes" or "uncertain" and is
# refused (a dangling symlink still counts as present).
require_role_no() {
  local path present=()
  for path in "$INSTALL_JSON" "$TOKEN_FILE" "$NODES_CONF"; do
    if [[ -e "$path" || -L "$path" ]]; then
      present+=("$path")
    fi
  done
  if [[ ${#present[@]} -gt 0 ]]; then
    {
      echo "ERROR: this host already has subscription-center files, so it is not a standalone node host:"
      printf '  %s\n' "${present[@]}"
      echo "To upgrade a subscription center, run: sudo bash install-proxy.sh update"
      echo "If nodes.conf was created by mistake on a node host, delete it and retry."
      echo "A subscription center installed before v0.4 has no install.json: run install-proxy.sh install once to create it."
    } >&2
    exit 1
  fi
}

# --- L1 (shared proxyctl binary lock, spec §11.5) -------------------------------

acquire_l1() {
  if ! exec 9>"$LOCK_FILE"; then
    die "Cannot open the lock file $LOCK_FILE"
  fi
  # The wait runs in the background so SIGINT/SIGTERM end it at once; the lock
  # belongs to the open file description, which the parent shell keeps.
  bg_arm
  flock -w "$LOCK_WAIT" 9 &
  BG_PID=$!
  bg_disarm
  if ! wait_bg; then
    die "Another operation is in progress (could not take the lock within ${LOCK_WAIT}s). Retry later."
  fi
}

# L3 (Reality instance lock, spec §11): fd 8, upgrade mode only, always taken
# after L1. WHAT is the context for the error message.
acquire_l3() {
  if ! exec 8>"$REALITY_LOCK_FILE"; then
    die "Cannot open the lock file $REALITY_LOCK_FILE"
  fi
  bg_arm
  flock -w "$L3_WAIT" 8 &
  BG_PID=$!
  bg_disarm
  if ! wait_bg; then
    exec 8>&-
    die "Could not take the Reality instance lock within ${L3_WAIT}s ($1). Nothing was replaced; retry later."
  fi
}

# --- proxyctl decision (spec §12 table) -----------------------------------------

# probe_version prints the installed binary's version (never fails). The child
# gets no copy of the L1 descriptor.
probe_version() {
  "$1" version 8>&- 9>&- 2>/dev/null </dev/null || true
}

# probe_capability: `proxyctl node service --help` must exit 0 (spec §4.5).
probe_capability() {
  "$1" node service --help >/dev/null 2>&1 8>&- 9>&- </dev/null
}

sidecar_matches() {
  local actual recorded
  [[ -f "$SIDECAR" ]] || return 1
  actual="$(sha256_file "$PROXYCTL_BIN" 2>/dev/null || true)"
  recorded="$(tr -d '[:space:]' < "$SIDECAR" 2>/dev/null | tr 'A-F' 'a-f' || true)"
  [[ -n "$actual" && "$recorded" == "$actual" ]]
}

# Anything but a plain regular file at either path would make the rename
# install unsafe (mv into a directory, replacing a symlink target).
require_regular_install_files() {
  if [[ -e "$PROXYCTL_BIN" || -L "$PROXYCTL_BIN" ]]; then
    [[ -f "$PROXYCTL_BIN" && ! -L "$PROXYCTL_BIN" ]] \
      || die "$PROXYCTL_BIN is not a regular file; inspect and remove it manually, it is never executed."
  fi
  if [[ -e "$SIDECAR" || -L "$SIDECAR" ]]; then
    [[ -f "$SIDECAR" && ! -L "$SIDECAR" ]] \
      || die "$SIDECAR is not a regular file; inspect and remove it manually."
  fi
}

# decide_proxyctl sets NEED_INSTALL=1 when the fixed version must be
# installed, 0 when the existing proxyctl is confirmed and kept, and exits 1
# for every "stop" row. The installed binary is executed only after its
# sidecar has been verified.
decide_proxyctl() {
  local installed cmp_min cmp_fixed
  NEED_INSTALL=0

  require_regular_install_files

  if [[ ! -e "$PROXYCTL_BIN" ]]; then
    NEED_INSTALL=1
    return 0
  fi

  if ! sidecar_matches; then
    if [[ "$REPLACE_PROXYCTL" -eq 1 ]]; then
      echo "Existing $PROXYCTL_BIN has a missing or mismatched .sha256 file; it is not executed and will be replaced by $PROXYCTL_VERSION." >&2
      NEED_INSTALL=1
      return 0
    fi
    die "$PROXYCTL_BIN has a missing or mismatched $SIDECAR, so it is not executed. To reinstall $PROXYCTL_VERSION over it, rerun with --replace-proxyctl."
  fi

  installed="$(probe_version "$PROXYCTL_BIN")"
  if ! parse_version "$installed"; then
    die "The installed proxyctl reports version '${installed}', which cannot be compared; refusing to touch it."
  fi

  cmp_min="$(version_cmp "$installed" "$MIN_NODE_VERSION")"
  if [[ "$cmp_min" == "-1" ]]; then
    echo "Installed proxyctl $installed predates node support; installing $PROXYCTL_VERSION." >&2
    NEED_INSTALL=1
    return 0
  fi

  cmp_fixed="$(version_cmp "$installed" "$PROXYCTL_VERSION")"
  if [[ "$cmp_fixed" == "0" ]]; then
    probe_capability "$PROXYCTL_BIN" \
      || die "Installed proxyctl $installed does not support 'node service' (capability probe failed)."
    return 0
  fi
  if [[ "$cmp_fixed" == "1" ]]; then
    die "Installed proxyctl $installed is newer than this script's $PROXYCTL_VERSION and is not replaced or downgraded. Run: sudo proxyctl node"
  fi
  die "Installed proxyctl $installed is older than this script's $PROXYCTL_VERSION and is not replaced. To upgrade it, run: sudo bash bootstrap-node.sh --upgrade"
}


# --- install ----------------------------------------------------------------------

download() { # URL DEST
  bg_arm
  curl -fsSL --max-time "$DOWNLOAD_TIMEOUT" -o "$2" "$1" 8>&- 9>&- </dev/null &
  BG_PID=$!
  bg_disarm
  wait_bg
}

# fetch_candidate downloads and verifies the pinned release (shared by the
# first-install and upgrade modes). On success CAND_BIN is a verified 0755
# temp binary, CAND_SIDE a 0644 temp sidecar holding CAND_HASH. Order:
# mktemp -> download proxyctl + checksums.txt -> SHA256 -> `version` ->
# `node service --help`. Any failure dies (1); the EXIT trap removes the temps.
fetch_candidate() {
  local asset base bin_dir tmp_sums
  local expected_count expected actual ver_out

  command -v curl >/dev/null 2>&1 || die "curl is required to download proxyctl"
  asset="proxyctl-linux-${ARCH}"
  base="https://github.com/${PROXYCTL_REPOSITORY}/releases/download/${PROXYCTL_VERSION}"
  bin_dir="$(dirname "$PROXYCTL_BIN")"
  [[ -d "$bin_dir" ]] || install -d -m 0755 "$bin_dir"

  # Temp files live in the target directory so the final step is a rename
  # within one filesystem.
  CAND_BIN="$(mktemp "$bin_dir/.proxyctl.tmp.XXXXXX")"
  TMP_FILES+=("$CAND_BIN")
  tmp_sums="$(mktemp "$bin_dir/.proxyctl.checksums.tmp.XXXXXX")"
  TMP_FILES+=("$tmp_sums")
  CAND_SIDE="$(mktemp "$bin_dir/.proxyctl.sha256.tmp.XXXXXX")"
  TMP_FILES+=("$CAND_SIDE")

  echo "Downloading proxyctl $PROXYCTL_VERSION (linux-$ARCH)" >&2
  download "$base/$asset" "$CAND_BIN" || die "Failed to download $asset"
  download "$base/checksums.txt" "$tmp_sums" || die "Failed to download checksums.txt"

  expected_count="$(awk -v file="$asset" '$1 ~ /^[[:xdigit:]]{64}$/ && ($2 == file || $2 == "*" file) { count++ } END { print count+0 }' "$tmp_sums")"
  expected="$(awk -v file="$asset" '$1 ~ /^[[:xdigit:]]{64}$/ && ($2 == file || $2 == "*" file) { print tolower($1) }' "$tmp_sums")"
  if [[ "$expected_count" != "1" || -z "$expected" ]]; then
    die "checksums.txt must contain exactly one entry for $asset (found $expected_count)"
  fi
  actual="$(sha256_file "$CAND_BIN" 2>/dev/null || true)"
  [[ -n "$actual" && "$actual" == "$expected" ]] || die "SHA256 mismatch for the downloaded $asset"

  chmod 0755 "$CAND_BIN"
  ver_out="$(probe_version "$CAND_BIN")"
  [[ "$ver_out" == "$PROXYCTL_VERSION" ]] \
    || die "Downloaded proxyctl reports version '${ver_out}', expected $PROXYCTL_VERSION"
  probe_capability "$CAND_BIN" || die "Downloaded proxyctl does not support 'node service'"

  printf '%s\n' "$expected" > "$CAND_SIDE"
  chmod 0644 "$CAND_SIDE"
  CAND_HASH="$expected"
}

install_proxyctl() {
  local recorded

  fetch_candidate

  # Binary first, sidecar second, both renames inside the target directory.
  mv -f "$CAND_BIN" "$PROXYCTL_BIN" || die "Cannot install $PROXYCTL_BIN"
  if ! mv -f "$CAND_SIDE" "$SIDECAR"; then
    die "Installed $PROXYCTL_BIN but could not write $SIDECAR; rerun with --replace-proxyctl to repair."
  fi
  hash -r 2>/dev/null || true
  # The two renamed files are gone from their temp names; drop the rest.
  rm -f -- "${TMP_FILES[@]}"
  TMP_FILES=()

  recorded="$(tr -d '[:space:]' < "$SIDECAR")"
  [[ "$(sha256_file "$PROXYCTL_BIN" 2>/dev/null || true)" == "$recorded" ]] \
    || die "Post-install check failed: $PROXYCTL_BIN does not match $SIDECAR; rerun with --replace-proxyctl."
  [[ "$(probe_version "$PROXYCTL_BIN")" == "$PROXYCTL_VERSION" ]] \
    || die "Post-install check failed: $PROXYCTL_BIN does not report $PROXYCTL_VERSION; rerun with --replace-proxyctl."
  echo "Installed proxyctl $PROXYCTL_VERSION at $PROXYCTL_BIN" >&2
}

# --- upgrade mode (spec §12 "升级模式") -------------------------------------------

# Spec §4.3: a pending instance transaction (zone or tombstone) means the
# instance must be recovered by the version that wrote it before any upgrade.
require_no_pending_txn() {
  if [[ -e "$REALITY_TXN_DIR" || -L "$REALITY_TXN_DIR" ]] || compgen -G "$REALITY_TOMB_GLOB" >/dev/null; then
    die "A Reality instance transaction is pending (${REALITY_TXN_DIR} or a tombstone exists). Run 'sudo proxyctl node service recover' first; nothing was changed."
  fi
}

# decide_upgrade: the upgrade table. Exits 0 (already at the version), 1 (every
# refusal) or returns to continue with the upgrade. The installed binary is
# executed only after its sidecar has been verified.
decide_upgrade() {
  local installed cmp

  require_regular_install_files
  if [[ ! -e "$PROXYCTL_BIN" ]]; then
    die "$PROXYCTL_BIN does not exist, so there is nothing to upgrade. Run without --upgrade for the first installation: sudo bash bootstrap-node.sh"
  fi
  if ! sidecar_matches; then
    die "$PROXYCTL_BIN has a missing or mismatched $SIDECAR, so it is not executed. Run without --upgrade and use --replace-proxyctl to reinstall $PROXYCTL_VERSION over it."
  fi
  installed="$(probe_version "$PROXYCTL_BIN")"
  if ! parse_version "$installed"; then
    die "The installed proxyctl reports version '${installed}', which cannot be compared; refusing to touch it."
  fi
  cmp="$(version_cmp "$installed" "$PROXYCTL_VERSION")"
  if [[ "$cmp" == "0" ]]; then
    echo "proxyctl is already at $PROXYCTL_VERSION; nothing to do." >&2
    exit 0
  fi
  if [[ "$cmp" == "1" ]]; then
    die "Installed proxyctl $installed is newer than this script's $PROXYCTL_VERSION and is not downgraded."
  fi
  OLD_VERSION="$installed"
  OLD_HASH="$(sha256_file "$PROXYCTL_BIN")" || die "Cannot hash $PROXYCTL_BIN"
}

reality_roots() {
  printf '%s\n' "$REALITY_STATE_DIR" "$REALITY_CONFIG_DIR" "$REALITY_UNIT_FILE" "$REALITY_BIN_DIR"
}

# Spec §4.3 "未安装": the four asset roots are all absent. A dangling symlink
# or any other leftover counts as present.
instance_assets_absent() {
  local root
  while IFS= read -r root; do
    if [[ -e "$root" || -L "$root" ]]; then
      return 1
    fi
  done < <(reality_roots)
  return 0
}

# snapshot_assets prints a fingerprint of every path under the four roots:
# path, type, owner, mode, and content hash (regular files) or link target.
snapshot_assets() {
  local root listing path meta extra
  while IFS= read -r root; do
    printf 'ROOT %s\n' "$root"
    if [[ ! -e "$root" && ! -L "$root" ]]; then
      printf 'ABSENT\n'
      continue
    fi
    listing="$(find -P "$root" 2>/dev/null | LC_ALL=C sort)" || return 1
    while IFS= read -r path; do
      meta="$(stat -c '%F|%u:%g|%a' -- "$path" 2>/dev/null)" || return 1
      extra=""
      if [[ -L "$path" ]]; then
        extra="$(readlink -- "$path")" || return 1
      elif [[ -f "$path" ]]; then
        extra="$(sha256_file "$path")" || return 1
      fi
      printf '%s|%s|%s\n' "$path" "$meta" "$extra"
    done <<<"$listing"
  done < <(reality_roots)
}

# compat_check: spec §12 read-only compatibility check of the candidate.
# Nothing to verify when no instance asset exists. Otherwise: L3 handover (the
# parent keeps L1 and releases L3; the child has neither fd 9 nor fd 8 and
# takes L3 itself), `node service show` must exit 0 within SHOW_TIMEOUT, then
# the parent re-takes L3 and re-checks role, transaction state and that no
# instance asset changed.
compat_check() {
  local before after rc=0

  if instance_assets_absent; then
    echo "No Reality instance assets found; skipping the compatibility check." >&2
    return 0
  fi
  command -v setsid >/dev/null 2>&1 || die "setsid is required for the compatibility check"
  command -v timeout >/dev/null 2>&1 || die "timeout is required for the compatibility check"

  before="$(snapshot_assets)" || die "Cannot read the Reality instance assets for the compatibility check"
  echo "Checking that proxyctl $PROXYCTL_VERSION can read the existing Reality instance (node service show)." >&2

  exec 8>&-
  # The child runs in its own session so the whole process tree can be killed;
  # timeout enforces SHOW_TIMEOUT. Neither fd 9 (L1) nor fd 8 (L3) is inherited.
  bg_arm
  BG_GROUP=1
  setsid timeout -s KILL "$SHOW_TIMEOUT" "$CAND_BIN" node service show >/dev/null 8>&- 9>&- </dev/null &
  BG_PID=$!
  bg_disarm
  wait_bg || rc=$?
  if [[ "$rc" -ne 0 ]]; then
    if [[ "$rc" -eq 124 || "$rc" -eq 137 ]]; then
      die "The compatibility check ('node service show' of the new proxyctl) did not finish within ${SHOW_TIMEOUT}s. Nothing was replaced."
    fi
    die "The compatibility check ('node service show' of the new proxyctl) failed with exit code $rc; the new proxyctl cannot read the existing Reality instance. Nothing was replaced."
  fi

  acquire_l3 "after the compatibility check"
  require_role_no
  require_no_pending_txn
  after="$(snapshot_assets)" || die "Cannot read the Reality instance assets after the compatibility check"
  if [[ "$after" != "$before" ]]; then
    die "The Reality instance assets changed during the compatibility check. Nothing was replaced; retry."
  fi
}

# write_atomic SRC DEST MODE: temp file in DEST's directory, fsync, rename.
write_atomic() {
  local src="$1" dest="$2" mode="$3" tmp
  tmp="$(mktemp "$(dirname "$dest")/.proxyctl.tmp.XXXXXX")" || return 1
  TMP_FILES+=("$tmp")
  cat -- "$src" > "$tmp" || return 1
  chmod "$mode" "$tmp" || return 1
  sync -- "$tmp" || return 1
  mv -f "$tmp" "$dest" || return 1
}

# save_previous: spec §12. Only the latest pair is kept; each file is written
# atomically, then the directory is fsynced and the pair is verified to match
# each other (and the installed pair) before the replacement may start.
save_previous() {
  local prev_bin="$PREVIOUS_DIR/proxyctl" prev_side="$PREVIOUS_DIR/proxyctl.sha256"
  local parent p h r created=0

  parent="$(dirname "$PREVIOUS_DIR")"
  for p in "$parent" "$PREVIOUS_DIR"; do
    if [[ -L "$p" ]] || { [[ -e "$p" ]] && [[ ! -d "$p" ]]; }; then
      die "$p is not a directory; inspect it manually. Nothing was replaced."
    fi
  done
  if [[ ! -d "$parent" ]]; then
    install -d -m 0700 "$parent" || die "Cannot create $parent"
    created=1
  fi
  if [[ ! -d "$PREVIOUS_DIR" ]]; then
    install -d -m 0700 "$PREVIOUS_DIR" || die "Cannot create $PREVIOUS_DIR"
    created=1
  fi
  [[ "$(stat -c %u -- "$PREVIOUS_DIR")" == "$EUID" ]] \
    || die "$PREVIOUS_DIR is not owned by the current user; inspect it manually. Nothing was replaced."
  chmod 0700 "$PREVIOUS_DIR" || die "Cannot set the mode of $PREVIOUS_DIR"
  for p in "$prev_bin" "$prev_side"; do
    if [[ -L "$p" ]] || { [[ -e "$p" ]] && [[ ! -f "$p" ]]; }; then
      die "$p is not a regular file; inspect it manually. Nothing was replaced."
    fi
  done

  write_atomic "$PROXYCTL_BIN" "$prev_bin" 0700 || die "Cannot save the previous proxyctl to $PREVIOUS_DIR. Nothing was replaced."
  write_atomic "$SIDECAR" "$prev_side" 0600 || die "Cannot save the previous proxyctl.sha256 to $PREVIOUS_DIR. Nothing was replaced."
  sync -- "$PREVIOUS_DIR" || die "Cannot sync $PREVIOUS_DIR. Nothing was replaced."
  if [[ "$created" -eq 1 ]]; then
    sync -- "$parent" || die "Cannot sync $parent. Nothing was replaced."
  fi

  h="$(sha256_file "$prev_bin" 2>/dev/null || true)"
  r="$(tr -d '[:space:]' < "$prev_side" 2>/dev/null | tr 'A-F' 'a-f' || true)"
  if [[ -z "$h" || "$h" != "$r" || "$h" != "$OLD_HASH" || ! -f "$prev_bin" || ! -f "$prev_side" || -L "$prev_bin" || -L "$prev_side" ]]; then
    die "The saved previous pair in $PREVIOUS_DIR does not match the installed pair. Nothing was replaced."
  fi
}

# stage_restore prepares the rollback files from the verified previous pair
# before the replacement starts, so restoring is just two renames.
stage_restore() {
  local bin_dir
  bin_dir="$(dirname "$PROXYCTL_BIN")"
  RESTORE_BIN="$(mktemp "$bin_dir/.proxyctl.restore.XXXXXX")" || die "Cannot stage the rollback files. Nothing was replaced."
  TMP_FILES+=("$RESTORE_BIN")
  RESTORE_SIDE="$(mktemp "$bin_dir/.proxyctl.restore.XXXXXX")" || die "Cannot stage the rollback files. Nothing was replaced."
  TMP_FILES+=("$RESTORE_SIDE")
  cat -- "$PREVIOUS_DIR/proxyctl" > "$RESTORE_BIN" || die "Cannot stage the rollback files. Nothing was replaced."
  cat -- "$PREVIOUS_DIR/proxyctl.sha256" > "$RESTORE_SIDE" || die "Cannot stage the rollback files. Nothing was replaced."
  chmod 0755 "$RESTORE_BIN" || die "Cannot stage the rollback files. Nothing was replaced."
  chmod 0644 "$RESTORE_SIDE" || die "Cannot stage the rollback files. Nothing was replaced."
  [[ "$(sha256_file "$RESTORE_BIN" 2>/dev/null || true)" == "$OLD_HASH" ]] \
    || die "The staged rollback binary does not match the installed one. Nothing was replaced."
}

# verify_installed: the installed binary matches the sidecar and the new
# release, and reports the pinned version.
verify_installed() {
  local actual recorded
  actual="$(shielded sha256_file "$PROXYCTL_BIN" 2>/dev/null || true)"
  recorded="$(tr -d '[:space:]' < "$SIDECAR" 2>/dev/null || true)"
  [[ -n "$actual" && "$actual" == "$recorded" && "$actual" == "$CAND_HASH" ]] || return 1
  [[ "$(shielded probe_version "$PROXYCTL_BIN")" == "$PROXYCTL_VERSION" ]]
}

# rollback_previous restores binary + sidecar from the staged copy of the
# previous pair and verifies the result: exit 4 when restored, 5 when not.
rollback_previous() {
  local h s
  echo "The upgrade did not complete; restoring the previous proxyctl." >&2
  shielded mv -f "$RESTORE_BIN" "$PROXYCTL_BIN" || true
  shielded mv -f "$RESTORE_SIDE" "$SIDECAR" || true
  h="$(shielded sha256_file "$PROXYCTL_BIN" 2>/dev/null || true)"
  s="$(tr -d '[:space:]' < "$SIDECAR" 2>/dev/null || true)"
  if [[ -n "$h" && "$h" == "$OLD_HASH" && "$s" == "$OLD_HASH" ]]; then
    echo "Restored proxyctl $OLD_VERSION. The previous copy is kept in $PREVIOUS_DIR." >&2
    exit 4
  fi
  {
    echo "ERROR: could not restore the previous proxyctl; $PROXYCTL_BIN and $SIDECAR may not match."
    echo "The previous copy is in $PREVIOUS_DIR. Do not execute $PROXYCTL_BIN until it is repaired:"
    echo "rerun this release's bootstrap-node.sh with --replace-proxyctl, or restore the pair from the previous copy (docs/reality-node.md)."
  } >&2
  exit 5
}

# replace_and_verify: from the first rename until verification completes the
# signal handler only records the signal (SIGNAL_SEEN). Bash runs traps between
# commands, never inside one, and the success decision below is one single
# assignment, so a signal is either seen before the decision (rollback, exit 4
# or 5) or after it (cleanup only, exit 0); nothing can interleave.
replace_and_verify() {
  local failed=0

  trap 'SIGNAL_SEEN=1' INT TERM
  # A signal that arrived while the handler was still "exit 1" has already
  # exited; one recorded since then still precedes the first rename.
  if [[ "$SIGNAL_SEEN" -ne 0 ]]; then exit 1; fi

  shielded mv -f "$CAND_BIN" "$PROXYCTL_BIN" || failed=1
  if [[ "$failed" -eq 0 && "$SIGNAL_SEEN" -eq 0 ]]; then
    shielded mv -f "$CAND_SIDE" "$SIDECAR" || failed=1
  fi
  if [[ "$failed" -eq 0 && "$SIGNAL_SEEN" -eq 0 ]]; then
    verify_installed || failed=1
  fi
  if [[ "$failed" -eq 1 ]]; then
    UPGRADE_OK=0
  else
    UPGRADE_OK=$(( SIGNAL_SEEN == 0 ))
  fi

  if [[ "$UPGRADE_OK" -ne 1 ]]; then
    rollback_previous
  fi
  hash -r 2>/dev/null || true
  {
    echo "Upgraded proxyctl $OLD_VERSION -> $PROXYCTL_VERSION at $PROXYCTL_BIN (previous copy: $PREVIOUS_DIR)."
    echo "The sing-box version pinned by this proxyctl may differ from the one the Reality instance uses."
    echo "Run 'sudo proxyctl node service show' to check, and 'sudo proxyctl node service reinstall' if needed."
  } >&2
  exec 8>&- 9>&-
  exit 0
}

run_upgrade() {
  acquire_l1
  acquire_l3 "waiting for the instance lock"
  require_role_no
  require_no_pending_txn
  decide_upgrade
  fetch_candidate
  compat_check
  save_previous
  stage_restore
  replace_and_verify
}

# --- main ---------------------------------------------------------------------------

main() {
  local arg

  # 1. Parse the whole argv first; usage errors are 2 and happen before the
  #    root check, any lock or any network access.
  local upgrade=0
  for arg in "$@"; do
    case "$arg" in
      --upgrade) upgrade=1 ;;
      --replace-proxyctl) REPLACE_PROXYCTL=1 ;;
      *) ARGS+=("$arg") ;;
    esac
  done
  if [[ "$upgrade" -eq 1 && $# -ne 1 ]]; then
    echo "ERROR: --upgrade must be the only argument" >&2
    usage
    exit 2
  fi

  # 2. Host checks, then the role pre-check: no lock, no download on refusal.
  require_root
  require_supported_os
  require_supported_arch
  require_role_no

  # 3. Upgrade mode has its own sequence and never exec's proxyctl.
  if [[ "$upgrade" -eq 1 ]]; then
    run_upgrade
    # run_upgrade always exits; never fall through into the first-install path.
    # shellcheck disable=SC2317
    exit 1
  fi

  # 4. L1 for the whole decision and install; re-check the role inside it.
  acquire_l1
  require_role_no
  decide_proxyctl
  if [[ "$NEED_INSTALL" -eq 1 ]]; then
    install_proxyctl
  fi

  # 5. Release L1 before exec so proxyctl can take it itself (a held fd would
  #    block it, spec §11.5), then hand over.
  exec 9>&-
  trap - EXIT INT TERM
  if [[ ${#ARGS[@]} -eq 0 ]]; then
    if [[ -t 0 && -t 1 ]]; then
      exec "$PROXYCTL_BIN" node
    fi
    usage
    exit 2
  fi
  exec "$PROXYCTL_BIN" node service install "${ARGS[@]}"
}

main "$@"
