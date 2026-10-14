#!/usr/bin/env bash
# bootstrap-node.sh: first-time setup of a standalone Reality node host.
#
# Installs the pinned proxyctl release (checksum- and version-verified, atomic
# rename, .sha256 sidecar in the installer's layout), then hands over to
#
#   proxyctl node                          (no arguments, on a terminal)
#   proxyctl node service install ARGS...  (with arguments)
#
# Spec: docs/superpowers/specs/2026-09-18-v0.10-reality-node-install-design.md
# §12 (first-install mode), §11.5 (L1 is the shared proxyctl binary lock),
# §13.2 (each download is bounded to 120 s).
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

ARGS=()
REPLACE_PROXYCTL=0
ARCH=""
NEED_INSTALL=0
TMP_FILES=()
BG_PID=""

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
  --replace-proxyctl only repairs a proxyctl whose .sha256 file is missing or
  does not match; it never replaces or downgrades a verified proxyctl.
EOF
}

# --- cleanup and signals ------------------------------------------------------

cleanup() {
  trap - EXIT
  if [[ -n "$BG_PID" ]]; then
    kill "$BG_PID" 2>/dev/null || true
    wait "$BG_PID" 2>/dev/null || true
    BG_PID=""
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
  return "$rc"
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
  flock -w "$LOCK_WAIT" 9 &
  BG_PID=$!
  if ! wait_bg; then
    die "Another operation is in progress (could not take the lock within ${LOCK_WAIT}s). Retry later."
  fi
}

# --- proxyctl decision (spec §12 table) -----------------------------------------

# probe_version prints the installed binary's version (never fails). The child
# gets no copy of the L1 descriptor.
probe_version() {
  "$1" version 9>&- 2>/dev/null </dev/null || true
}

# probe_capability: `proxyctl node service --help` must exit 0 (spec §4.5).
probe_capability() {
  "$1" node service --help >/dev/null 2>&1 9>&- </dev/null
}

sidecar_matches() {
  local actual recorded
  [[ -f "$SIDECAR" ]] || return 1
  actual="$(sha256_file "$PROXYCTL_BIN" 2>/dev/null || true)"
  recorded="$(tr -d '[:space:]' < "$SIDECAR" 2>/dev/null | tr 'A-F' 'a-f' || true)"
  [[ -n "$actual" && "$recorded" == "$actual" ]]
}

# decide_proxyctl sets NEED_INSTALL=1 when the fixed version must be
# installed, 0 when the existing proxyctl is confirmed and kept, and exits 1
# for every "stop" row. The installed binary is executed only after its
# sidecar has been verified.
decide_proxyctl() {
  local installed cmp_min cmp_fixed
  NEED_INSTALL=0

  # Anything but a plain regular file at either path would make the rename
  # install unsafe (mv into a directory, replacing a symlink target).
  if [[ -e "$PROXYCTL_BIN" || -L "$PROXYCTL_BIN" ]]; then
    [[ -f "$PROXYCTL_BIN" && ! -L "$PROXYCTL_BIN" ]] \
      || die "$PROXYCTL_BIN is not a regular file; inspect and remove it manually, it is never executed."
  fi
  if [[ -e "$SIDECAR" || -L "$SIDECAR" ]]; then
    [[ -f "$SIDECAR" && ! -L "$SIDECAR" ]] \
      || die "$SIDECAR is not a regular file; inspect and remove it manually."
  fi

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
  curl -fsSL --max-time "$DOWNLOAD_TIMEOUT" -o "$2" "$1" 9>&- </dev/null &
  BG_PID=$!
  wait_bg
}

install_proxyctl() {
  local asset base bin_dir tmp_bin tmp_sums tmp_side
  local expected_count expected actual ver_out recorded

  command -v curl >/dev/null 2>&1 || die "curl is required to download proxyctl"
  asset="proxyctl-linux-${ARCH}"
  base="https://github.com/${PROXYCTL_REPOSITORY}/releases/download/${PROXYCTL_VERSION}"
  bin_dir="$(dirname "$PROXYCTL_BIN")"
  [[ -d "$bin_dir" ]] || install -d -m 0755 "$bin_dir"

  # Temp files live in the target directory so the final step is a rename
  # within one filesystem.
  tmp_bin="$(mktemp "$bin_dir/.proxyctl.tmp.XXXXXX")"
  TMP_FILES+=("$tmp_bin")
  tmp_sums="$(mktemp "$bin_dir/.proxyctl.checksums.tmp.XXXXXX")"
  TMP_FILES+=("$tmp_sums")
  tmp_side="$(mktemp "$bin_dir/.proxyctl.sha256.tmp.XXXXXX")"
  TMP_FILES+=("$tmp_side")

  echo "Downloading proxyctl $PROXYCTL_VERSION (linux-$ARCH)" >&2
  download "$base/$asset" "$tmp_bin" || die "Failed to download $asset"
  download "$base/checksums.txt" "$tmp_sums" || die "Failed to download checksums.txt"

  expected_count="$(awk -v file="$asset" '$1 ~ /^[[:xdigit:]]{64}$/ && ($2 == file || $2 == "*" file) { count++ } END { print count+0 }' "$tmp_sums")"
  expected="$(awk -v file="$asset" '$1 ~ /^[[:xdigit:]]{64}$/ && ($2 == file || $2 == "*" file) { print tolower($1) }' "$tmp_sums")"
  if [[ "$expected_count" != "1" || -z "$expected" ]]; then
    die "checksums.txt must contain exactly one entry for $asset (found $expected_count)"
  fi
  actual="$(sha256_file "$tmp_bin" 2>/dev/null || true)"
  [[ -n "$actual" && "$actual" == "$expected" ]] || die "SHA256 mismatch for the downloaded $asset"

  chmod 0755 "$tmp_bin"
  ver_out="$(probe_version "$tmp_bin")"
  [[ "$ver_out" == "$PROXYCTL_VERSION" ]] \
    || die "Downloaded proxyctl reports version '${ver_out}', expected $PROXYCTL_VERSION"
  probe_capability "$tmp_bin" || die "Downloaded proxyctl does not support 'node service'"

  printf '%s\n' "$expected" > "$tmp_side"
  chmod 0644 "$tmp_side"

  # Binary first, sidecar second, both renames inside the target directory.
  mv -f "$tmp_bin" "$PROXYCTL_BIN" || die "Cannot install $PROXYCTL_BIN"
  if ! mv -f "$tmp_side" "$SIDECAR"; then
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
  if [[ "$upgrade" -eq 1 ]]; then
    if [[ $# -ne 1 ]]; then
      echo "ERROR: --upgrade must be the only argument" >&2
      usage
      exit 2
    fi
    # Interim (v0.10 Task 11): upgrade mode ships with Task 12.
    echo "ERROR: --upgrade is not available in this build" >&2
    exit 2
  fi

  # 2. Host checks, then the role pre-check: no lock, no download on refusal.
  require_root
  require_supported_os
  require_supported_arch
  require_role_no

  # 3. L1 for the whole decision and install; re-check the role inside it.
  acquire_l1
  require_role_no
  decide_proxyctl
  if [[ "$NEED_INSTALL" -eq 1 ]]; then
    install_proxyctl
  fi

  # 4. Release L1 before exec so proxyctl can take it itself (a held fd would
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
