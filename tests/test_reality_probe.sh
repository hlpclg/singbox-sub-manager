#!/usr/bin/env bash
# Task 1 of the v0.10 plan (docs/superpowers/plans/2026-09-18-v0.10-reality-node-install.md).
#
# --verify-assets: downloads both pinned sing-box release tarballs and
# checks their SHA256 against the values pinned in the plan and in
# internal/realitynode/probe_integration_test.go. This is the portion of
# Task 1 that needs only network access, not root or a real systemd host,
# so it is safe to run in normal CI (no real systemd is pretended here).
#
# The Go-side real-runtime gates (real handshake, real routing rejection,
# static systemd-analyze verify) live in
# internal/realitynode/probe_integration_test.go under the
# reality_integration build tag; run them with:
#
#   CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go test -c -tags reality_integration \
#     -o realitynode.test ./internal/realitynode
#   ./realitynode.test -test.v \
#     -test.run '^Test(PinnedReleaseProbe|RealityUnitProbe|RealityDataPathProbe|RealityRoutingProbe)$'
#
# The plan requires that binary be built with the Go 1.22 container and
# executed on real Debian 12 / Ubuntu 22.04 / Ubuntu 24.04 VMs (amd64 and
# arm64). Neither a Go 1.22 container nor a real systemd VM was available
# where this script was authored; see docs/reality-node-validation.md for
# exactly what ran instead and why.
set -euo pipefail

VERSION="1.14.1"
BASE_URL="https://github.com/SagerNet/sing-box/releases/download/v${VERSION}"
AMD64_ASSET="sing-box-${VERSION}-linux-amd64.tar.gz"
ARM64_ASSET="sing-box-${VERSION}-linux-arm64.tar.gz"
AMD64_SHA256="12cb2816b52febb356f6a885b740cc8758c3f30b8ae0ca8edba80f0d2d35343f"
ARM64_SHA256="6060b42fa84c5dcaeae1799af7f61b0f1ae4855d9d5ddc9e02baba17154b3ae2"

usage() {
  echo "Usage: $0 --verify-assets" >&2
  exit 2
}

TMP_DIR=""
cleanup() {
  # A trap whose last command exits non-zero (e.g. TMP_DIR still empty,
  # so the guard short-circuits false) clobbers the script's real exit
  # code, including an explicit `exit 2` from usage(). Always return 0.
  if [[ -n "$TMP_DIR" ]]; then
    rm -rf "$TMP_DIR"
  fi
  return 0
}
trap cleanup EXIT

verify_assets() {
  TMP_DIR="$(mktemp -d)"
  local tmp="$TMP_DIR"

  local asset sha url dst got
  for pair in "${AMD64_ASSET}:${AMD64_SHA256}" "${ARM64_ASSET}:${ARM64_SHA256}"; do
    asset="${pair%%:*}"
    sha="${pair##*:}"
    url="${BASE_URL}/${asset}"
    dst="${tmp}/${asset}"
    echo "Downloading ${asset}..."
    curl -sSL -o "${dst}" "${url}"
    got="$(sha256sum "${dst}" | awk '{print $1}')"
    if [[ "${got}" != "${sha}" ]]; then
      echo "FAIL: ${asset} SHA256 mismatch: got ${got}, want ${sha}" >&2
      echo "STOP: pinned release no longer matches the plan; report before proceeding." >&2
      exit 1
    fi
    echo "PASS: ${asset} SHA256 matches pinned value (${sha})"
  done
}

[[ $# -eq 1 && "$1" == "--verify-assets" ]] || usage
verify_assets
