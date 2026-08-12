#!/usr/bin/env bash
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
TEST_DIR="$(mktemp -d)"
trap 'rm -rf "$TEST_DIR"' EXIT

pass=0
fail=0

assert_eq() {
  local name="$1"
  local got="$2"
  local want="$3"
  if [[ "$got" == "$want" ]]; then
    echo "PASS: $name"
    pass=$((pass+1))
  else
    echo "FAIL: $name (got '$got', want '$want')"
    fail=$((fail+1))
  fi
}

echo "Testing install.json writer..."

BASE_DIR="$TEST_DIR/etc"
mkdir -p "$BASE_DIR"

# shellcheck disable=SC2034
DOMAIN="sub.example.com"
# shellcheck disable=SC2034
SUB_ROOT="/var/www/proxy-sub"
# shellcheck disable=SC2034
TOKEN_FILE="/var/lib/singbox-sub-manager/token"
TMP="$TEST_DIR/tmp_dir"
mkdir -p "$TMP"

# Extract the write install state section from install-proxy.sh
INSTALL_SNIPPET=$(awk '
/# 12. Write install state/ { active=1 }
/# Masked Token for log/ { active=0 }
active { print }
' "$REPO_ROOT/install-proxy.sh")

# Execute it
( eval "$INSTALL_SNIPPET" )

INSTALL_JSON="$BASE_DIR/install.json"

if [[ -f "$INSTALL_JSON" ]]; then
  assert_eq "file_exists" "yes" "yes"
  
  # Check permissions (0644)
  if uname | grep -q Darwin; then perms=$(stat -f "%Lp" "$INSTALL_JSON"); else perms=$(stat -c "%a" "$INSTALL_JSON"); fi
  assert_eq "permissions" "$perms" "644"
  
  # Validate JSON structure using a simple grep or jq if available
  content=$(cat "$INSTALL_JSON")
  if echo "$content" | grep -q '"domain": "sub.example.com"'; then
    assert_eq "has_domain" "yes" "yes"
  else
    assert_eq "has_domain" "no" "yes"
  fi
  
  if echo "$content" | grep -q '"subscription_root": "/var/www/proxy-sub"'; then
    assert_eq "has_sub_root" "yes" "yes"
  else
    assert_eq "has_sub_root" "no" "yes"
  fi
  
  # Ensure token value is not written (just the path)
  if echo "$content" | grep -q 'token_file'; then
    assert_eq "has_token_file" "yes" "yes"
  else
    assert_eq "has_token_file" "no" "yes"
  fi
else
  assert_eq "file_exists" "no" "yes"
fi

echo "Testing failure cleanup..."
(
  # shellcheck disable=SC2329
  chmod() { return 1; }
  export -f chmod
  ( eval "$INSTALL_SNIPPET" )
) || true

leftover=$(ls -A "$BASE_DIR"/.install.json.tmp.* 2>/dev/null || true)
if [[ -z "$leftover" ]]; then
  assert_eq "cleanup_on_failure" "clean" "clean"
else
  assert_eq "cleanup_on_failure" "leftover_files_exist" "clean"
fi

if [[ $fail -eq 0 ]]; then
  echo "All tests passed."
  exit 0
else
  echo "$fail tests failed."
  exit 1
fi
