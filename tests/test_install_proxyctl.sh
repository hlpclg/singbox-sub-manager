#!/bin/bash
# Mock test for install_proxyctl behavior in install-proxy.sh
# Tests if the script correctly downloads and validates proxyctl version

set -euo pipefail

cd "$(dirname "$0")/.."
# We need to source the functions without running the main execution.
# Fortunately, the script uses parameter parsing. So we can just call it with an invalid param or mock the execution check.
# Wait, the script has a safe early exit for root and OS. It's at the very top.
# So we can't just source it on macOS.
# Instead, we will grep the function out.

mkdir -p .agent/tmp
TMP_SCRIPT="$(mktemp .agent/tmp/script.XXXXXX)"
trap 'rm -f "$TMP_SCRIPT"' EXIT

# Extract install_proxyctl and trim() function
cat <<'EOF' > "$TMP_SCRIPT"
#!/bin/bash
log() { echo "[INFO] $1"; }
log_warn() { echo "[WARN] $1"; }
die() { echo "[FATAL] $1"; exit 1; }

mktemp() {
  local template="${1:-}"
  if [[ -z "$template" ]]; then
    command mktemp .agent/tmp/tmp.XXXXXX
  elif [[ "$template" == "/tmp/"* ]]; then
    template=".agent/tmp/$(basename "$template")"
    command mktemp "$template"
  else
    command mktemp "$@"
  fi
}
export -f mktemp

PROXYCTL_BIN="$(mktemp .agent/tmp/proxyctl_bin.XXXXXX)"
PROXYCTL_REPOSITORY="hlpclg/singbox-sub-manager"
PROXYCTL_VERSION="v0.2.2"

curl() {
  local output_file=""
  local is_checksum=0

  while [[ $# -gt 0 ]]; do
    case "$1" in
      -o|--output)
        output_file="$2"
        shift 2
        ;;
      *checksums.txt)
        is_checksum=1
        shift
        ;;
      *)
        shift
        ;;
    esac
  done

  if [[ -n "$output_file" ]]; then
    if [[ $is_checksum -eq 1 ]]; then
      # Generate mock binary
      local tmp_bin
      tmp_bin="$(mktemp)"
      cat <<'INNEREOF' > "$tmp_bin"
#!/bin/bash
if [[ "$1" == "version" ]]; then
  echo "v0.2.2"
  exit 0
fi
exit 1
INNEREOF
      # Generate mock checksum matching our mock binary
      local fake_bin_hash
      fake_bin_hash="$(sha256sum "$tmp_bin" | awk '{print $1}')"
      rm -f "$tmp_bin"
      
      local arch
      case "$(uname -m)" in
        x86_64|amd64) arch="amd64" ;;
        aarch64|arm64) arch="arm64" ;;
      esac
      echo "${fake_bin_hash} *proxyctl-linux-${arch}" > "$output_file"
    else
      # Generate mock binary
      cat <<'INNEREOF' > "$output_file"
#!/bin/bash
if [[ "$1" == "version" ]]; then
  echo "v0.2.2"
  exit 0
fi
exit 1
INNEREOF
      chmod +x "$output_file"
    fi
  fi
}
export -f curl
EOF

awk '/^install_proxyctl\(\) \{/{flag=1} flag; /^}/{if(flag){flag=0; exit}}' install-proxy.sh >> "$TMP_SCRIPT"

cat <<'EOF' >> "$TMP_SCRIPT"
echo "Running install_proxyctl..."
if install_proxyctl; then
  echo "SUCCESS: install_proxyctl verified and installed successfully."
  if [[ -x "$PROXYCTL_BIN" ]]; then
    echo "SUCCESS: proxyctl binary is present and executable at $PROXYCTL_BIN"
    rm -f "$PROXYCTL_BIN"
    exit 0
  else
    echo "ERROR: proxyctl binary is missing."
    exit 1
  fi
else
  echo "ERROR: install_proxyctl failed."
  exit 1
fi
EOF

chmod +x "$TMP_SCRIPT"
bash "$TMP_SCRIPT"
