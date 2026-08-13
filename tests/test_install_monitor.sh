#!/usr/bin/env bash
# shellcheck disable=SC2016,SC2030,SC2031,SC2329
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
SCRIPT="$ROOT/install-proxy.sh"

TEST_DIR="$(mktemp -d)"
trap 'rm -rf "$TEST_DIR"' EXIT

# shellcheck disable=SC2016
grep -Fq 'PROXYCTL_VERSION="${PROXYCTL_VERSION:-v0.6.0}"' "$SCRIPT"
grep -Fq 'monitor --help' "$SCRIPT"
grep -Fq 'SuccessExitStatus=2' "$SCRIPT"
grep -Fq 'OnCalendar=*:0/5' "$SCRIPT"
grep -Fq 'Persistent=true' "$SCRIPT"
grep -Fq 'AccuracySec=15s' "$SCRIPT"
grep -Fq 'RandomizedDelaySec=15s' "$SCRIPT"
grep -Fq 'restore_monitor_units()' "$SCRIPT"
grep -Fq 'systemctl is-enabled proxyctl-monitor.timer' "$SCRIPT"
grep -Fq 'systemctl is-active proxyctl-monitor.timer' "$SCRIPT"
grep -Fq 'systemctl disable proxyctl-monitor.timer' "$SCRIPT"
grep -Fq 'systemctl stop proxyctl-monitor.timer' "$SCRIPT"
grep -Fq 'Failed to activate proxyctl-monitor timer.' "$SCRIPT"
grep -Fq 'MONITOR_UNIT_DIR="${MONITOR_UNIT_DIR:-/etc/systemd/system}"' "$SCRIPT"
grep -Fq 'PROXYCTL_VERSION="${PROXYCTL_VERSION:-v0.6.0}"' "$ROOT/merge-nodes.sh"

SNIPPET="$(awk '
/# 11\. Monitor/ { active=1 }
/# 12\. Sysctl/ { active=0 }
active { print }
' "$SCRIPT")"

UNIT_DIR="$TEST_DIR/units"
mkdir -p "$UNIT_DIR"
printf 'old service\n' > "$UNIT_DIR/proxyctl-monitor.service"
printf 'old timer\n' > "$UNIT_DIR/proxyctl-monitor.timer"
CALLS="$TEST_DIR/systemctl.calls"

systemctl() {
  printf '%s\n' "$*" >> "$CALLS"
  if [[ "${MODE:-disabled}" == runtime ]]; then
    case "$1 ${2:-}" in
      "is-enabled proxyctl-monitor.timer") printf 'enabled-runtime\n'; return 0 ;;
      "is-active proxyctl-monitor.timer") printf 'inactive\n'; return 3 ;;
    esac
  fi
  if [[ "${MODE:-disabled}" == query-error ]]; then
    case "$1 ${2:-}" in
      "is-enabled proxyctl-monitor.timer") printf 'unknown-state\n'; return 1 ;;
    esac
  fi
  case "$1 ${2:-}" in
    "is-enabled proxyctl-monitor.timer") printf 'disabled\n'; return 1 ;;
    "is-active proxyctl-monitor.timer") printf 'inactive\n'; return 3 ;;
    "enable --now") return 1 ;;
    *) return 0 ;;
  esac
}

log_error() { :; }
die() { exit 1; }
export -f systemctl log_error die
export CALLS

set +e
( export MONITOR_UNIT_DIR="$UNIT_DIR"; eval "$SNIPPET" )
status=$?
set -e
[[ "$status" -ne 0 ]]
grep -Fxq 'old service' "$UNIT_DIR/proxyctl-monitor.service"
grep -Fxq 'old timer' "$UNIT_DIR/proxyctl-monitor.timer"
grep -Fq 'is-enabled proxyctl-monitor.timer' "$CALLS"
grep -Fq 'is-active proxyctl-monitor.timer' "$CALLS"
grep -Fq 'disable proxyctl-monitor.timer' "$CALLS"
grep -Fq 'stop proxyctl-monitor.timer' "$CALLS"

printf 'transaction rollback test passed\n'

printf 'old service\n' > "$UNIT_DIR/proxyctl-monitor.service"
printf 'old timer\n' > "$UNIT_DIR/proxyctl-monitor.timer"
: > "$CALLS"
set +e
( export MONITOR_UNIT_DIR="$UNIT_DIR" MODE=runtime; eval "$SNIPPET" )
status=$?
set -e
[[ "$status" -ne 0 ]]
grep -Fxq 'old service' "$UNIT_DIR/proxyctl-monitor.service"
grep -Fxq 'old timer' "$UNIT_DIR/proxyctl-monitor.timer"
grep -Fq 'enable --runtime proxyctl-monitor.timer' "$CALLS"
printf 'runtime enablement rollback test passed\n'

printf 'old service\n' > "$UNIT_DIR/proxyctl-monitor.service"
printf 'old timer\n' > "$UNIT_DIR/proxyctl-monitor.timer"
: > "$CALLS"
set +e
( export MONITOR_UNIT_DIR="$UNIT_DIR" MODE=query-error; eval "$SNIPPET" )
status=$?
set -e
[[ "$status" -ne 0 ]]
grep -Fxq 'old service' "$UNIT_DIR/proxyctl-monitor.service"
grep -Fxq 'old timer' "$UNIT_DIR/proxyctl-monitor.timer"
if grep -Eq '^(daemon-reload|enable)' "$CALLS"; then
  exit 1
fi
if compgen -G "$UNIT_DIR/.proxyctl-monitor.*.tmp.*" >/dev/null; then
  exit 1
fi
printf 'state probe failure test passed\n'

echo "monitor installer contract passed"
