#!/usr/bin/env bash
# shellcheck disable=SC2016,SC2030,SC2031,SC2329
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
SCRIPT="$ROOT/install-proxy.sh"

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

echo "monitor installer contract passed"
