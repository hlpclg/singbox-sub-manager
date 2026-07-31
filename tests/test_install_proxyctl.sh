#!/usr/bin/env bash
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
TEST_DIR="$(mktemp -d)"
trap 'rm -rf "$TEST_DIR"' EXIT

sha256_file_test() {
  if command -v sha256sum >/dev/null 2>&1; then
    sha256sum "$1" | awk '{print $1}'
  else
    shasum -a 256 "$1" | awk '{print $1}'
  fi
}

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
  printf '%s\n' 'die() { echo "[FATAL] $1" >&2; return 1; }'
  for fn in sha256_file trim yaml_quote contains_control_chars url_encode_component validate_node_fields write_subscriptions_with_shell install_proxyctl run_proxyctl_merge; do
    extract_function "$fn"
  done
} > "$HARNESS"

grep -q '^sha256_file()' "$HARNESS" || { echo "production sha256_file function is missing" >&2; exit 1; }

NODES_CONF="$TEST_DIR/nodes.conf"
printf '%s\n' '# managed-by: installer' 'Node1|1.2.3.4|443|pass123|obfs123|example.com' > "$NODES_CONF"

make_proxyctl() {
  local path="$1" version="$2" marker="$3" label="$4"
  mkdir -p "$(dirname "$path")"
  cat > "$path" <<EOF
#!/usr/bin/env bash
if [[ "\${1:-}" == version ]]; then
  printf '%s\\n' '$version'
  exit 0
fi
if [[ "\${1:-}" == merge ]]; then
  printf '%s\\n' '$label' >> '$marker'
  out=''
  while [[ \$# -gt 0 ]]; do
    case "\$1" in --output) out="\$2"; shift 2 ;; *) shift ;; esac
  done
  mkdir -p "\$out"
  printf 'generated-by: %s\\n' '$label' > "\$out/clash.yaml"
  printf '%s\\n' '$label' > "\$out/sr.txt"
  exit 0
fi
printf '%s\\n' '$label' >> '$marker'
exit 1
EOF
  chmod +x "$path"
}

make_evil_proxyctl() {
  local path="$1" marker="$2"
  mkdir -p "$(dirname "$path")"
  cat > "$path" <<EOF
#!/usr/bin/env bash
printf 'evil:%s\\n' "\${1:-}" >> '$marker'
if [[ "\${1:-}" == version ]]; then printf 'v0.2.2\\n'; exit 0; fi
exit 90
EOF
  chmod +x "$path"
}

run_install_case() {
  local name="$1" setup_fn="$2" verify_fn="$3"
  local case_dir="$TEST_DIR/$name"
  mkdir -p "$case_dir/fixed" "$case_dir/path" "$case_dir/out" "$case_dir/download"
  (
    export PROXYCTL_BIN="$case_dir/fixed/proxyctl"
    export PROXYCTL_REPOSITORY="hlpclg/singbox-sub-manager"
    export PROXYCTL_VERSION="v0.2.2"
    export PROXYCTL_VALIDATED_BIN=""
    export ORIGINAL_PATH="$PATH"
    export PATH="$case_dir/path:$PATH"
    export CASE_DIR="$case_dir"
    export DOWNLOAD_BINARY="$case_dir/download/proxyctl"
    export CURL_MODE="ok"
    export CHECKSUM_MODE="ok"
    "$setup_fn"

    curl() {
      local output="" url=""
      while [[ $# -gt 0 ]]; do
        case "$1" in -o) output="$2"; shift 2 ;; http*) url="$1"; shift ;; *) shift ;; esac
      done
      [[ "$CURL_MODE" == "404" ]] && return 22
      if [[ "$url" == *checksums.txt ]]; then
        local hash
        hash="$(sha256_file_test "$DOWNLOAD_BINARY")"
        [[ "$CHECKSUM_MODE" == "bad" ]] && hash="0000000000000000000000000000000000000000000000000000000000000000"
        printf '%s  proxyctl-linux-amd64\n' "$hash" > "$output"
        printf '%s  proxyctl-linux-arm64\n' "$hash" >> "$output"
      else
        cp "$DOWNLOAD_BINARY" "$output"
      fi
    }
    export -f curl sha256_file_test
    # shellcheck disable=SC1090
    source "$HARNESS"
    install_proxyctl || true
    "$verify_fn"
  )
  echo "$name PASSED"
}

setup_valid_download() {
  make_proxyctl "$DOWNLOAD_BINARY" v0.2.2 "$CASE_DIR/good.marker" fixed-download
}
verify_valid_download() {
  [[ -x "$PROXYCTL_BIN" ]]
  [[ -f "${PROXYCTL_BIN}.sha256" ]]
  [[ "$(cat "${PROXYCTL_BIN}.sha256")" == "$(sha256_file_test "$PROXYCTL_BIN")" ]]
  make_evil_proxyctl "$CASE_DIR/path/proxyctl" "$CASE_DIR/evil.marker"
  export PATH="$CASE_DIR/path:$ORIGINAL_PATH"
  run_proxyctl_merge "$NODES_CONF" "$CASE_DIR/out"
  grep -Fxq fixed-download "$CASE_DIR/good.marker"
  [[ ! -e "$CASE_DIR/evil.marker" ]]
  grep -Fq 'generated-by: fixed-download' "$CASE_DIR/out/clash.yaml"
}
run_install_case valid-download setup_valid_download verify_valid_download

setup_valid_existing() {
  make_proxyctl "$PROXYCTL_BIN" v0.2.2 "$CASE_DIR/good.marker" fixed-existing
  sha256_file_test "$PROXYCTL_BIN" > "${PROXYCTL_BIN}.sha256"
  CURL_MODE=404
}
verify_valid_existing() {
  [[ "$PROXYCTL_VALIDATED_BIN" == "$PROXYCTL_BIN" ]]
  run_proxyctl_merge "$NODES_CONF" "$CASE_DIR/out"
  grep -Fxq fixed-existing "$CASE_DIR/good.marker"
}
run_install_case valid-existing setup_valid_existing verify_valid_existing

setup_bad_sidecar() {
  make_proxyctl "$PROXYCTL_BIN" v0.2.2 "$CASE_DIR/old.marker" old-fixed
  printf '%064d\n' 0 > "${PROXYCTL_BIN}.sha256"
  make_proxyctl "$DOWNLOAD_BINARY" v0.2.2 "$CASE_DIR/new.marker" new-download
}
verify_bad_sidecar() {
  [[ ! -e "$CASE_DIR/old.marker" ]]
  run_proxyctl_merge "$NODES_CONF" "$CASE_DIR/out"
  grep -Fxq new-download "$CASE_DIR/new.marker"
}
run_install_case bad-sidecar-redownloads setup_bad_sidecar verify_bad_sidecar

setup_bad_existing_version() {
  make_proxyctl "$PROXYCTL_BIN" v0.1.0 "$CASE_DIR/old.marker" old-fixed
  sha256_file_test "$PROXYCTL_BIN" > "${PROXYCTL_BIN}.sha256"
  make_proxyctl "$DOWNLOAD_BINARY" v0.2.2 "$CASE_DIR/new.marker" new-download
}
verify_bad_existing_version() {
  run_proxyctl_merge "$NODES_CONF" "$CASE_DIR/out"
  [[ ! -e "$CASE_DIR/old.marker" ]]
  grep -Fxq new-download "$CASE_DIR/new.marker"
}
run_install_case bad-existing-version-redownloads setup_bad_existing_version verify_bad_existing_version

setup_path_binary_ignored() {
  make_evil_proxyctl "$CASE_DIR/path/proxyctl" "$CASE_DIR/evil.marker"
  make_proxyctl "$DOWNLOAD_BINARY" v0.2.2 "$CASE_DIR/good.marker" fixed-download
}
verify_path_binary_ignored() {
  run_proxyctl_merge "$NODES_CONF" "$CASE_DIR/out"
  [[ ! -e "$CASE_DIR/evil.marker" ]]
  grep -Fxq fixed-download "$CASE_DIR/good.marker"
}
run_install_case path-binary-ignored setup_path_binary_ignored verify_path_binary_ignored

setup_wrong_download_version() {
  make_proxyctl "$DOWNLOAD_BINARY" v0.1.0 "$CASE_DIR/wrong.marker" wrong-download
}
verify_shell_fallback() {
  [[ -z "$PROXYCTL_VALIDATED_BIN" ]]
  run_proxyctl_merge "$NODES_CONF" "$CASE_DIR/out"
  [[ -s "$CASE_DIR/out/clash.yaml" ]]
  [[ -s "$CASE_DIR/out/sr.txt" ]]
}
run_install_case wrong-download-version setup_wrong_download_version verify_shell_fallback

setup_checksum_mismatch() {
  make_proxyctl "$DOWNLOAD_BINARY" v0.2.2 "$CASE_DIR/wrong.marker" wrong-checksum
  CHECKSUM_MODE=bad
}
run_install_case checksum-mismatch setup_checksum_mismatch verify_shell_fallback

setup_http_404() {
  make_proxyctl "$DOWNLOAD_BINARY" v0.2.2 "$CASE_DIR/unreachable.marker" unreachable
  CURL_MODE=404
}
run_install_case http-404 setup_http_404 verify_shell_fallback

setup_download_failure_with_evil_path() {
  make_evil_proxyctl "$CASE_DIR/path/proxyctl" "$CASE_DIR/evil.marker"
  make_proxyctl "$DOWNLOAD_BINARY" v0.2.2 "$CASE_DIR/unreachable.marker" unreachable
  CURL_MODE=404
}
verify_download_failure_with_evil_path() {
  verify_shell_fallback
  [[ ! -e "$CASE_DIR/evil.marker" ]]
}
run_install_case download-failure-ignores-path setup_download_failure_with_evil_path verify_download_failure_with_evil_path

# Execute merge-nodes.sh as a separate process with only an evil PATH proxyctl.
merge_dir="$TEST_DIR/merge-nodes-isolation"
mkdir -p "$merge_dir/bin" "$merge_dir/out"
printf '%s\n' 'Node1|1.2.3.4|443|pass123|obfs123|example.com' > "$merge_dir/nodes.conf"
printf '%s\n' 'abcdefghijklmnop' > "$merge_dir/token"
make_evil_proxyctl "$merge_dir/bin/proxyctl" "$merge_dir/evil.marker"
cat > "$merge_dir/bin/id" <<'EOF'
#!/usr/bin/env bash
[[ "${1:-}" == "-u" ]] && { echo 0; exit 0; }
exec /usr/bin/id "$@"
EOF
cat > "$merge_dir/bin/curl" <<'EOF'
#!/usr/bin/env bash
exit 22
EOF
chmod +x "$merge_dir/bin/id" "$merge_dir/bin/curl"
set +e
PATH="$merge_dir/bin:$PATH" PROXYCTL_BIN="$merge_dir/fixed-proxyctl" TOKEN_FILE="$merge_dir/token" SUB_ROOT="$merge_dir/out" \
  bash "$REPO_ROOT/merge-nodes.sh" "$merge_dir/nodes.conf" >"$merge_dir/stdout" 2>"$merge_dir/stderr"
merge_status=$?
set -e
[[ "$merge_status" -ne 0 ]]
[[ ! -e "$merge_dir/evil.marker" ]]
grep -Fq '无法下载或校验 proxyctl' "$merge_dir/stderr" || {
  echo "unexpected merge-nodes.sh error:" >&2
  sed 's/^/  /' "$merge_dir/stderr" >&2
  exit 1
}
echo "merge-nodes-path-isolation PASSED"

echo "All 10 proxyctl scenarios passed"
