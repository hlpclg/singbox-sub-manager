#!/usr/bin/env bash
# shellcheck disable=SC2016,SC2317,SC2329
# v0.10 Task 13: isolated integration flow with the REAL proxyctl binary.
#
# Seam investigation (what can be isolated without changing Go production code)
#   - cmd/proxyctl, internal/realitynode, internal/transaction, internal/publish,
#     internal/role and internal/hostlock read NO environment variable and expose
#     NO flag to relocate their roots. Roots (/etc/proxyctl-reality,
#     /var/lib/proxyctl-reality, /usr/local/lib/proxyctl-reality, the unit file,
#     /etc/singbox-sub-manager, /var/lib/singbox-sub-manager, /run/lock/*.lock),
#     the sing-box download URL (pinned to the GitHub release) and the HTTPS
#     verification address (127.0.0.1:443) are compiled in. The Go tests inject
#     them through package variables and structs (newServiceRuntime,
#     newPublishRuntime, defaultNodesPath, realitynode.Env, publish.Env), which a
#     shell test cannot reach.
#   - The seams that DO exist for a real binary: `systemctl` is looked up in
#     PATH (a fake can stand in for systemd), and Go honours SSL_CERT_FILE (a
#     throw-away CA can stand in for Caddy's certificate).
#   So the full lifecycle can only run against the real fixed paths, i.e. as root
#   in a throw-away container. That section is opt-in (PROXYCTL_REAL_FS_TESTS=1),
#   refuses to run if any of those paths already exists or if a real systemd is
#   present, and prints an explicit SKIP (counted) otherwise.
#
# What runs where
#   always (any user, also as root, nothing is touched): the real binary's
#     strict-mode parse errors (exit 2 happens before root/lock/network/write).
#   PROXYCTL_REAL_FS_TESTS=1, root, Linux, network to github.com and the SNI
#   host, openssl and curl installed (use a throw-away container):
#     node side : fresh -> install (real sing-box download, real config,
#                 real local self-check; systemd is a stateful fake systemctl)
#                 -> show -> uninstall -> show -> reinstall -> uninstall --purge
#                 --export-backup; asset layout, modes, identity kept across
#                 uninstall/reinstall, everything gone after purge.
#     center    : legacy nodes.conf -> migrate -> the add command that the node
#                 printed, executed verbatim (--publish) -> idempotent re-paste ->
#                 the printed disable command -> the printed enable command, with
#                 the served subscription files checked over HTTPS against a
#                 throw-away TLS server on 127.0.0.1:443; role refusals of
#                 `node service install` and bootstrap-node.sh on the center.
#
# NOT covered here (kept honest in docs/reality-node-validation.md):
#   real systemd (unit sandboxing, Restart=, journald), the private/loopback
#   dynamic rejection through the tunnel (Task 1 real-host gate), the arm64
#   binary, the real GitHub Release download of bootstrap-node.sh (v0.10.0 is
#   not published), a real terminal Ctrl-C, FlClash and the spec 14.5 two-host
#   run. Fault injection and cancellation of the Go transaction code stay in the
#   Go tests; this file does not mock them.
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
TEST_DIR="$(mktemp -d)"
FIXED_VERSION="v0.10.0"

pass=0
fail=0
skipped=0
ok() { echo "PASS: $1"; pass=$((pass + 1)); }
bad() { echo "FAIL: $1" >&2; fail=$((fail + 1)); }
expect() {
  local desc="$1"
  shift
  if "$@"; then ok "$desc"; else bad "$desc"; fi
}

REAL_ACTIVE=0
SSERVER_PID=""
cleanup() {
  real_cleanup
  if [[ -n "$SSERVER_PID" ]]; then kill "$SSERVER_PID" 2>/dev/null || true; fi
  rm -rf "$TEST_DIR"
}
trap cleanup EXIT

# ---------------------------------------------------------------------------
# The candidate: the real proxyctl, built with the release version.
# ---------------------------------------------------------------------------

CAND=""
resolve_candidate() {
  CAND="${PROXYCTL_REAL_CANDIDATE:-}"
  if [[ -z "$CAND" ]]; then
    if command -v go >/dev/null 2>&1; then
      CAND="$TEST_DIR/proxyctl-real"
      if ! (cd "$REPO_ROOT" && CGO_ENABLED=0 go build -trimpath -ldflags "-X main.Version=$FIXED_VERSION" -o "$CAND" ./cmd/proxyctl); then
        bad "candidate: go build failed"; return 1
      fi
    else
      bad "candidate: set PROXYCTL_REAL_CANDIDATE to a prebuilt proxyctl (go is not available)"; return 1
    fi
  fi
  if [[ ! -x "$CAND" ]]; then bad "candidate: $CAND is not executable"; return 1; fi
  if [[ "$("$CAND" version)" != "$FIXED_VERSION" ]]; then
    bad "candidate: $CAND does not report $FIXED_VERSION (build with -ldflags '-X main.Version=$FIXED_VERSION')"; return 1
  fi
  ok "candidate reports $FIXED_VERSION"
}

CAND_ENV=("FLOW=1")
RC=0
run_c() {
  set +e
  timeout 300 env "${CAND_ENV[@]}" "$CAND" "$@" >"$TEST_DIR/out" 2>"$TEST_DIR/err" </dev/null
  RC=$?
  set -e
}
# paste_cmd LINE: run a command line exactly as a node printed it ("sudo
# proxyctl ..."); sudo and proxyctl are shell functions standing for the real
# ones, so the line itself is executed unmodified.
paste_cmd() {
  set +e
  timeout 300 env "${CAND_ENV[@]}" CAND="$CAND" bash -c 'sudo() { "$@"; }; proxyctl() { "$CAND" "$@"; }; '"$1" >"$TEST_DIR/out" 2>"$TEST_DIR/err" </dev/null
  RC=$?
  set -e
}
out_has() { grep -Fq -- "$1" "$TEST_DIR/out"; }
err_has() { grep -Fq -- "$1" "$TEST_DIR/err"; }
rc_is() { [[ "$RC" == "$1" ]]; }
expect_rc() { # DESC RC
  if rc_is "$2"; then ok "$1"; else bad "$1 (rc=$RC, want $2)"; sed 's/^/  err: /' "$TEST_DIR/err" >&2 || true; fi
}
# the executable (non-comment) lines of stdout
cmd_lines() { grep -v '^#' "$TEST_DIR/out" || true; }
cmd_count_is() { [[ "$(grep -vc '^#' "$TEST_DIR/out" || true)" == "$1" ]]; }
one_cmd() { cmd_lines | head -n 1; }
no_secret_in_streams() { # never let a private key look-alike reach output
  ! grep -Eqi 'private[_ -]?key' "$TEST_DIR/out" "$TEST_DIR/err"
}

# ---------------------------------------------------------------------------
# Section 1 (always): strict-mode parse errors of the real binary. Parsing is
# complete before the root check, any lock, any network call or any write
# (spec 4.5, plan G2), so these are safe on any host, including as root.
# ---------------------------------------------------------------------------

test_strict_parse() {
  run_c node service install
  expect_rc "install without --name/--server: 2" 2
  expect "install without --name/--server: says what is missing" err_has "--name and --server are required"

  run_c node service install --name a --server 203.0.113.9 --bogus
  expect_rc "install with an unknown flag: 2" 2

  run_c node service uninstall --export-backup /nonexistent/x.tgz
  expect_rc "uninstall --export-backup without --purge: 2" 2
  expect "uninstall --export-backup without --purge: explained" err_has "only valid together with --purge"

  run_c node service reinstall --singbox-version v1.0.0
  expect_rc "reinstall --singbox-version without --singbox-sha256: 2" 2

  run_c node service show extra
  expect_rc "show with a positional argument: 2" 2

  run_c node publish --abandon
  expect_rc "publish --abandon without --recover: 2" 2

  run_c node add --publish
  expect_rc "add --publish without its fields (no prompt): 2" 2

  run_c node add -publish --type vless-reality --name x
  expect_rc "add -publish (single dash): 2" 2
  expect "add -publish: must be spelled --publish" err_has "must be spelled --publish"
}

# ---------------------------------------------------------------------------
# Section 2 (opt-in): the real lifecycle against the real fixed paths.
# ---------------------------------------------------------------------------

NODE_PATHS=(/etc/proxyctl-reality /var/lib/proxyctl-reality /var/lib/proxyctl-reality-txn /usr/local/lib/proxyctl-reality /etc/systemd/system/proxyctl-reality.service)
CENTER_PATHS=(/etc/singbox-sub-manager /var/lib/singbox-sub-manager /srv/flowsub /var/lib/proxyctl-bootstrap)
LOCK_PATHS=(/run/lock/proxyctl-reality.lock /run/lock/singbox-sub-manager.lock /run/lock/singbox-sub-manager-monitor.lock)
SYSTEMD_RUN_DIR=/run/systemd/system
LOCKS_PREEXISTING=()
MADE_RUN_SYSTEMD=0
MADE_ETC_SYSTEMD_SYSTEM=0
SYSD_STATE="$TEST_DIR/sysd"
FAKEBIN="$TEST_DIR/fakebin"

real_cleanup() {
  local p
  # stop the service the fake systemctl started
  if [[ -s "$SYSD_STATE/pid" ]]; then kill "$(cat "$SYSD_STATE/pid")" 2>/dev/null || true; fi
  if [[ "${REAL_ACTIVE:-0}" == 1 ]]; then
    for p in "${NODE_PATHS[@]}" "${CENTER_PATHS[@]}"; do rm -rf -- "$p"; done
    rm -rf -- /var/lib/.proxyctl-reality-txn.tomb-*
    for p in "${LOCK_PATHS[@]}"; do
      if [[ " ${LOCKS_PREEXISTING[*]:-} " != *" $p "* ]]; then rm -f -- "$p"; fi
    done
    if [[ "$MADE_ETC_SYSTEMD_SYSTEM" == 1 ]]; then rmdir /etc/systemd/system 2>/dev/null || true; fi
    # the run-time systemd dir was verified absent before this run created it
    rmdir "$SYSTEMD_RUN_DIR" 2>/dev/null || true
    if [[ "$MADE_RUN_SYSTEMD" == 1 ]]; then rmdir /run/systemd 2>/dev/null || true; fi
  fi
}

write_fake_systemctl() {
  mkdir -p "$FAKEBIN" "$SYSD_STATE"
  cat > "$FAKEBIN/systemctl" <<'EOF'
#!/bin/bash
# Stateful stand-in for systemctl, only for proxyctl-reality.service: it keeps
# loaded/enabled state, and start/restart really launch the unit's ExecStart
# (the real sing-box with the real generated config). It does NOT emulate any
# systemd sandboxing.
st="$FLOW_SYSD_STATE"; unit=/etc/systemd/system/proxyctl-reality.service
echo "systemctl $*" >> "$st/calls"
alive() { [[ -s "$st/pid" ]] && kill -0 "$(cat "$st/pid")" 2>/dev/null; }
stop_unit() {
  if alive; then
    kill "$(cat "$st/pid")" 2>/dev/null
    for _ in $(seq 1 50); do alive || break; sleep 0.1; done
  fi
  rm -f "$st/pid"
}
start_unit() {
  stop_unit
  cmd="$(sed -n 's/^ExecStart=//p' "$unit")"
  setsid bash -c "echo \$\$ > '$st/pid'; exec $cmd" >"$st/service.log" 2>&1 </dev/null &
  for _ in $(seq 1 50); do [[ -s "$st/pid" ]] && break; sleep 0.1; done
}
case "$1" in
  daemon-reload) ;;
  enable) : > "$st/enabled" ;;
  disable) rm -f "$st/enabled" ;;
  start | restart) start_unit ;;
  stop) stop_unit ;;
  show)
    case "$3" in
      LoadState) if [[ -f "$unit" ]]; then echo loaded; else echo not-found; fi ;;
      UnitFileState) if [[ ! -f "$unit" ]]; then echo; elif [[ -f "$st/enabled" ]]; then echo enabled; else echo disabled; fi ;;
      ActiveState) if alive; then echo active; else echo inactive; fi ;;
      MainPID) if alive; then cat "$st/pid"; else echo 0; fi ;;
    esac ;;
esac
exit 0
EOF
  chmod 0755 "$FAKEBIN/systemctl"
}

fake_active() { [[ -s "$SYSD_STATE/pid" ]] && kill -0 "$(cat "$SYSD_STATE/pid")" 2>/dev/null; }
port_open() { # PORT
  (exec 3<>"/dev/tcp/127.0.0.1/$1") 2>/dev/null
}
wait_port() { # PORT: bounded wait for the listener
  local i
  for ((i = 0; i < 100; i++)); do
    if port_open "$1"; then return 0; fi
    sleep 0.1
  done
  return 1
}
perm() { stat -c '%a:%U:%G' -- "$1"; }
sha_of() { sha256sum "$1" | awk '{print $1}'; }
paths_absent() {
  local p
  for p in "$@"; do
    if [[ -e "$p" || -L "$p" ]]; then echo "still exists: $p" >&2; return 1; fi
  done
}
node_assets_absent() {
  paths_absent "${NODE_PATHS[@]}" && ! compgen -G '/var/lib/.proxyctl-reality-txn.tomb-*' >/dev/null
}
state_files_are() { # "a b c": names directly in /var/lib/proxyctl-reality
  [[ "$(find /var/lib/proxyctl-reality -mindepth 1 -printf '%f\n' | LC_ALL=C sort | tr '\n' ' ')" == "$1 " ]]
}
manifest_state_is() { grep -Eq "\"state\"[[:space:]]*:[[:space:]]*\"$1\"" /var/lib/proxyctl-reality/manifest.json; }
digest_node_assets() {
  find /etc/proxyctl-reality /var/lib/proxyctl-reality /usr/local/lib/proxyctl-reality /etc/systemd/system/proxyctl-reality.service \
    -printf '%p|%y|%U:%G|%m\n' 2>/dev/null | LC_ALL=C sort | sha256sum | awk '{print $1}'
  find /etc/proxyctl-reality /var/lib/proxyctl-reality /etc/systemd/system/proxyctl-reality.service -type f -exec sha256sum {} + 2>/dev/null | LC_ALL=C sort | sha256sum | awk '{print $1}'
}

NODE_NAME="flow-n1"
NODE_SERVER="203.0.113.9"
NODE_PORT="8443"

add_cmd_shape_ok() {
  local c="$1"
  [[ "$c" == "sudo proxyctl node add --publish --min-version $FIXED_VERSION --type vless-reality --server '$NODE_SERVER' --port '$NODE_PORT' --uuid '"* \
    && "$c" == *" --name '$NODE_NAME'" ]]
}

ADD_CMD="" DISABLE_CMD="" ENABLE_CMD="" REMOVE_CMD=""

test_node_side() {
  local before_secret tx_digest

  # --- fresh host ---
  expect "fresh: no node assets exist" node_assets_absent
  run_c node service show
  expect_rc "fresh: show -> 1 (no instance)" 1
  run_c node service uninstall
  expect_rc "fresh: uninstall -> 1" 1
  run_c node service recover
  expect_rc "fresh: recover -> 1 (nothing to recover)" 1
  expect "fresh: the refusals created no node assets" node_assets_absent

  # --- install on a host without systemd: refused, nothing written ---
  expect "fresh: this host has no systemd" test ! -d "$SYSTEMD_RUN_DIR"
  run_c node service install --name "$NODE_NAME" --server "$NODE_SERVER" --port "$NODE_PORT"
  expect_rc "no systemd: install -> 1" 1
  expect "no systemd: says why" err_has "systemd is not available"
  expect "no systemd: nothing was written" node_assets_absent

  # --- install (systemd stands in as the fake systemctl) ---
  if [[ ! -d /run/systemd ]]; then MADE_RUN_SYSTEMD=1; fi
  mkdir -p "$SYSTEMD_RUN_DIR"
  if [[ ! -d /etc/systemd/system ]]; then MADE_ETC_SYSTEMD_SYSTEM=1; fi
  CAND_ENV=("FLOW=1" "PATH=$FAKEBIN:$PATH" "FLOW_SYSD_STATE=$SYSD_STATE")

  run_c node service install --name "$NODE_NAME" --server "$NODE_SERVER" --port "$NODE_PORT"
  expect_rc "install: 0" 0
  expect "install: the role line goes to stderr" err_has "subscription center role: no"
  expect "install: stdout holds exactly one executable command" cmd_count_is 1
  ADD_CMD="$(one_cmd)"
  expect "install: the command is the stable add template (--publish --min-version $FIXED_VERSION --type vless-reality, name last)" add_cmd_shape_ok "$ADD_CMD"
  expect "install: no private key in the output" no_secret_in_streams
  expect "install: manifest is installed" manifest_state_is installed
  expect "install: state directory holds exactly manifest/instance/secrets" state_files_are "instance.json manifest.json secrets.json"
  expect "install: state dir 700 root" test "$(perm /var/lib/proxyctl-reality)" = "700:root:root"
  expect "install: secrets.json 600 root" test "$(perm /var/lib/proxyctl-reality/secrets.json)" = "600:root:root"
  expect "install: instance.json 600 root" test "$(perm /var/lib/proxyctl-reality/instance.json)" = "600:root:root"
  expect "install: config dir 700, config.json 600" \
    bash -c '[[ "$(stat -c %a:%U /etc/proxyctl-reality)" == 700:root && "$(stat -c %a:%U /etc/proxyctl-reality/config.json)" == 600:root ]]'
  expect "install: unit file 644 root" test "$(perm /etc/systemd/system/proxyctl-reality.service)" = "644:root:root"
  expect "install: private sing-box binary is 755 and runs" \
    bash -c 'b="$(ls /usr/local/lib/proxyctl-reality/versions/*/sing-box)"; [[ "$(stat -c %a "$b")" == 755 ]] && "$b" version >/dev/null'
  expect "install: the transaction area and tombstones are gone" \
    bash -c '[[ ! -e /var/lib/proxyctl-reality-txn ]] && ! compgen -G "/var/lib/.proxyctl-reality-txn.tomb-*" >/dev/null'
  expect "install: the (fake) service is enabled and the real sing-box is running" fake_active
  expect "install: the node listens on $NODE_PORT" wait_port "$NODE_PORT"
  expect "install: nothing outside the instance was touched (/usr/local/bin/proxyctl is not created)" test ! -e /usr/local/bin/proxyctl

  tx_digest="$(digest_node_assets)"
  run_c node service install --name other --server "$NODE_SERVER" --port "$NODE_PORT"
  expect_rc "install again on an installed host: 1" 1
  expect "install again: assets unchanged" test "$(digest_node_assets)" = "$tx_digest"

  # --- show ---
  run_c node service show
  expect_rc "show (installed): 0" 0
  expect "show (installed): the same add command as install printed" test "$(one_cmd)" = "$ADD_CMD"
  expect "show (installed): the enable alternative is a comment line" out_has "# sudo proxyctl node enable --publish --min-version v0.10.0 '$NODE_NAME'"
  expect "show (installed): no private key in the output" no_secret_in_streams
  expect "show does not change anything" test "$(digest_node_assets)" = "$tx_digest"

  before_secret="$(sha_of /var/lib/proxyctl-reality/secrets.json)"

  # --- uninstall ---
  run_c node service uninstall
  expect_rc "uninstall: 0" 0
  expect "uninstall: one executable command" cmd_count_is 1
  DISABLE_CMD="$(one_cmd)"
  expect "uninstall: it is the disable command" test "$DISABLE_CMD" = "sudo proxyctl node disable --publish --min-version v0.10.0 '$NODE_NAME'"
  expect "uninstall: the remove alternative is a comment line" out_has "# sudo proxyctl node remove --publish --min-version v0.10.0 '$NODE_NAME'"
  expect "uninstall: the shared proxyctl is reported as kept (stderr)" err_has "/usr/local/bin/proxyctl"
  expect "uninstall: the service is stopped, the port closed" bash -c '! (exec 3<>/dev/tcp/127.0.0.1/'"$NODE_PORT"') 2>/dev/null'
  expect "uninstall: unit, config dir and private binary are gone" \
    paths_absent /etc/systemd/system/proxyctl-reality.service /etc/proxyctl-reality /usr/local/lib/proxyctl-reality
  expect "uninstall: identity is kept (manifest/instance/secrets only)" state_files_are "instance.json manifest.json secrets.json"
  expect "uninstall: manifest is uninstalled" manifest_state_is uninstalled
  expect "uninstall: secrets.json unchanged" test "$(sha_of /var/lib/proxyctl-reality/secrets.json)" = "$before_secret"
  run_c node service show
  expect_rc "show (uninstalled, identity kept): 0" 0
  expect "show (uninstalled): the disable command" test "$(one_cmd)" = "$DISABLE_CMD"
  run_c node service uninstall
  expect_rc "uninstall again (already uninstalled): 1" 1

  # --- reinstall ---
  run_c node service reinstall
  expect_rc "reinstall (from uninstalled): 0" 0
  expect "reinstall: one executable command" cmd_count_is 1
  ENABLE_CMD="$(one_cmd)"
  expect "reinstall: it is the enable command" test "$ENABLE_CMD" = "sudo proxyctl node enable --publish --min-version v0.10.0 '$NODE_NAME'"
  expect "reinstall: the commented add alternative is the original add command (same identity, clients keep working)" \
    test "$(grep -E '^# sudo proxyctl node add ' "$TEST_DIR/out" | sed 's/^# //')" = "$ADD_CMD"
  expect "reinstall: secrets.json is the same identity" test "$(sha_of /var/lib/proxyctl-reality/secrets.json)" = "$before_secret"
  expect "reinstall: manifest is installed again" manifest_state_is installed
  expect "reinstall: the (fake) service is active" fake_active
  expect "reinstall: the node listens on $NODE_PORT again" wait_port "$NODE_PORT"
  run_c node service show
  expect "show after reinstall: the add command is unchanged" test "$(one_cmd)" = "$ADD_CMD"

  # --- purge with export ---
  rm -f "$TEST_DIR/export.tgz"
  run_c node service uninstall --purge --export-backup "$TEST_DIR/export.tgz"
  expect_rc "uninstall --purge --export-backup: 0" 0
  REMOVE_CMD="$(one_cmd)"
  expect "purge: the remove command" test "$REMOVE_CMD" = "sudo proxyctl node remove --publish --min-version v0.10.0 '$NODE_NAME'"
  expect "purge: the archive exists with mode 600 (it holds the private key; contents are not printed)" \
    test "$(stat -c %a "$TEST_DIR/export.tgz")" = 600
  expect "purge: the archive lists the identity files" \
    bash -c 'tar -tzf "$1" | grep -q "proxyctl-reality/secrets.json"' _ "$TEST_DIR/export.tgz"
  expect "purge: stderr warns that the archive contains the private key" bash -c 'grep -qi "private key" "$1"' _ "$TEST_DIR/err"
  expect "purge: every asset of spec 5.1 is gone" node_assets_absent
  expect "purge: the service is stopped" bash -c '! (exec 3<>/dev/tcp/127.0.0.1/'"$NODE_PORT"') 2>/dev/null'
  expect "purge: /usr/local/bin/proxyctl was neither created nor touched" test ! -e /usr/local/bin/proxyctl
  run_c node service show
  expect_rc "after purge: show -> 1 (fresh again)" 1
  rm -f "$TEST_DIR/export.tgz"
}

# --- center -----------------------------------------------------------------

CENTER_DOMAIN="sub.flowtest"
CENTER_TOKEN="flowtoken_0123456789abcdef"
SUBROOT="/srv/flowsub/sub"

setup_center() {
  mkdir -p /etc/singbox-sub-manager /var/lib/singbox-sub-manager "$SUBROOT/$CENTER_TOKEN"
  chmod 0755 /etc/singbox-sub-manager /srv/flowsub "$SUBROOT"
  chmod 0700 /var/lib/singbox-sub-manager "$SUBROOT/$CENTER_TOKEN"
  printf '%s\n' "$CENTER_TOKEN" > /etc/singbox-sub-manager/token
  chmod 0600 /etc/singbox-sub-manager/token
  printf '{"domain":"%s","subscription_root":"%s","token_file":"/etc/singbox-sub-manager/token"}\n' "$CENTER_DOMAIN" "$SUBROOT" > /etc/singbox-sub-manager/install.json
  chmod 0644 /etc/singbox-sub-manager/install.json
  # a legacy (flat) nodes.conf with two hysteria2 nodes
  {
    printf 'HY2-A|198.51.100.20|443|hy2passwordAAAAAAAAAAAAAAAA|obfspasswordBBBBBBBBBBBB|www.bing.com\n'
    printf 'HY2-B|198.51.100.21|443|hy2passwordCCCCCCCCCCCCCCCC|obfspasswordDDDDDDDDDDDD|www.bing.com\n'
  } > /etc/singbox-sub-manager/nodes.conf
  chmod 0600 /etc/singbox-sub-manager/nodes.conf
  printf 'placeholder\n' > "$SUBROOT/$CENTER_TOKEN/clash.yaml"
  printf 'placeholder\n' > "$SUBROOT/$CENTER_TOKEN/sr.txt"
  chmod 0644 "$SUBROOT/$CENTER_TOKEN/clash.yaml" "$SUBROOT/$CENTER_TOKEN/sr.txt"

  # throw-away CA/leaf and a TLS server on the loopback address publish verifies
  openssl req -x509 -newkey rsa:2048 -nodes -days 1 -subj "/CN=$CENTER_DOMAIN" \
    -addext "subjectAltName=DNS:$CENTER_DOMAIN" -addext "basicConstraints=critical,CA:TRUE" \
    -keyout "$TEST_DIR/tls.key" -out "$TEST_DIR/tls.pem" >/dev/null 2>&1
  (cd "$SUBROOT" && exec openssl s_server -accept 127.0.0.1:443 -cert "$TEST_DIR/tls.pem" -key "$TEST_DIR/tls.key" -WWW >"$TEST_DIR/s_server.log" 2>&1) &
  SSERVER_PID=$!
  wait_port 443
}

served() { # PATH-after-token -> body served over HTTPS
  curl -fsS --cacert "$TEST_DIR/tls.pem" --resolve "$CENTER_DOMAIN:443:127.0.0.1" "https://$CENTER_DOMAIN/$CENTER_TOKEN/$1"
}
served_lacks() { # FILE NEEDLE: the document is served (HTTPS 200) and does not contain NEEDLE
  local body
  body="$(served "$1")" || return 1
  [[ -n "$body" ]] && ! grep -Fq -- "$2" <<<"$body"
}
nodes_has() { grep -Fq -- "$1" /etc/singbox-sub-manager/nodes.conf; }
node_enabled_in_conf() { # name flag
  awk -v n="[$1]" -v want="ENABLED=$2" '$0 == n { f = 1; next } /^\[/ { f = 0 } f && $0 == want { found = 1 } END { exit !found }' /etc/singbox-sub-manager/nodes.conf
}
publish_clean() {
  [[ ! -e /var/lib/singbox-sub-manager/publish ]] && ! compgen -G '/var/lib/singbox-sub-manager/.publish*' >/dev/null \
    && ! compgen -G '/var/lib/singbox-sub-manager/*tomb*' >/dev/null
}

test_center_side() {
  local clash_after_add sr_after_add nodes_after_add bak_digest uuid

  setup_center
  CAND_ENV=("FLOW=1" "PATH=$FAKEBIN:$PATH" "FLOW_SYSD_STATE=$SYSD_STATE" "SSL_CERT_FILE=$TEST_DIR/tls.pem")

  # --- legacy nodes.conf: publish refuses, migrate converts ---
  run_c node publish
  expect_rc "legacy nodes.conf: publish -> 1" 1
  expect "legacy nodes.conf: points at migrate" err_has "proxyctl node migrate"
  run_c node migrate
  expect_rc "migrate: 0" 0
  expect "migrate: reports the backup" out_has "backup at /etc/singbox-sub-manager/nodes.conf.bak"
  expect "migrate: nodes.conf is sectioned now" nodes_has "[HY2-A]"
  expect "migrate: the backup holds the legacy bytes" grep -Fq 'HY2-A|198.51.100.20|443|' /etc/singbox-sub-manager/nodes.conf.bak
  bak_digest="$(sha_of /etc/singbox-sub-manager/nodes.conf.bak)"
  run_c node migrate
  expect_rc "migrate again: 0 (nothing to do)" 0

  # --- role refusals on the center ---
  mkdir -p "$SYSTEMD_RUN_DIR"
  run_c node service install --name x --server 203.0.113.50 --port 9443
  expect_rc "center: node service install -> 1 (role is not 'no')" 1
  expect "center: node service install wrote no node assets" node_assets_absent
  run_c node service show
  expect_rc "center: node service show -> 1" 1
  set +e
  env "${CAND_ENV[@]}" LOCK_FILE=/run/lock/singbox-sub-manager.lock bash "$REPO_ROOT/bootstrap-node.sh" >"$TEST_DIR/out" 2>"$TEST_DIR/err" </dev/null
  RC=$?
  set -e
  expect_rc "center: bootstrap-node.sh -> 1 (refused)" 1
  expect "center: bootstrap points at install-proxy.sh update" err_has "install-proxy.sh update"
  expect "center: bootstrap installed nothing (no /usr/local/bin/proxyctl, no bootstrap dir)" \
    bash -c '[[ ! -e /usr/local/bin/proxyctl && ! -e /var/lib/proxyctl-bootstrap ]]'

  # --- register: the add command exactly as the node printed it ---
  paste_cmd "$ADD_CMD"
  expect_rc "paste the node's add command (--publish): 0" 0
  expect "add: published 3 enabled nodes, verified over HTTPS" out_has "published 3 enabled node(s) to clash.yaml and sr.txt (verified over HTTPS)"
  expect "add: nodes.conf has the node (vless-reality)" nodes_has "[$NODE_NAME]"
  uuid="$(printf '%s' "$ADD_CMD" | sed -E "s/.*--uuid '([^']+)'.*/\\1/")"
  expect "add: nodes.conf carries the node's uuid and server" \
    bash -c 'grep -Fq "$1" /etc/singbox-sub-manager/nodes.conf && grep -Fq "SERVER='"$NODE_SERVER"'" /etc/singbox-sub-manager/nodes.conf' _ "$uuid"
  expect "add: clash.yaml on disk contains the node" bash -c 'grep -Fq "$1" "$2"' _ "$NODE_NAME" "$SUBROOT/$CENTER_TOKEN/clash.yaml"
  expect "add: sr.txt on disk contains the node's uuid" bash -c 'grep -Fq "$1" "$2"' _ "$uuid" "$SUBROOT/$CENTER_TOKEN/sr.txt"
  expect "add: the HTTPS-served clash.yaml equals the file on disk" \
    bash -c '[[ "$(sha256sum < "$1" | cut -d" " -f1)" == "$2" ]]' _ "$SUBROOT/$CENTER_TOKEN/clash.yaml" "$(served clash.yaml | sha256sum | cut -d' ' -f1)"
  expect "add: the HTTPS-served sr.txt equals the file on disk" \
    bash -c '[[ "$(sha256sum < "$1" | cut -d" " -f1)" == "$2" ]]' _ "$SUBROOT/$CENTER_TOKEN/sr.txt" "$(served sr.txt | sha256sum | cut -d' ' -f1)"
  expect "add: the publish path does not touch the .bak" test "$(sha_of /etc/singbox-sub-manager/nodes.conf.bak)" = "$bak_digest"
  expect "add: no transaction area or tombstone is left" publish_clean
  expect "add: clash.yaml and sr.txt keep the pre-existing owner/mode" \
    bash -c '[[ "$(stat -c %U:%a "$1")" == root:644 && "$(stat -c %U:%a "$2")" == root:644 ]]' _ "$SUBROOT/$CENTER_TOKEN/clash.yaml" "$SUBROOT/$CENTER_TOKEN/sr.txt"
  clash_after_add="$(sha_of "$SUBROOT/$CENTER_TOKEN/clash.yaml")"
  sr_after_add="$(sha_of "$SUBROOT/$CENTER_TOKEN/sr.txt")"
  nodes_after_add="$(sha_of /etc/singbox-sub-manager/nodes.conf)"

  # --- idempotent re-paste ---
  paste_cmd "$ADD_CMD"
  expect_rc "paste the same add command again: 0" 0
  expect "re-paste: nodes.conf unchanged" test "$(sha_of /etc/singbox-sub-manager/nodes.conf)" = "$nodes_after_add"
  expect "re-paste: clash.yaml unchanged" test "$(sha_of "$SUBROOT/$CENTER_TOKEN/clash.yaml")" = "$clash_after_add"

  # --- --min-version is enforced ---
  paste_cmd "sudo proxyctl node disable --publish --min-version v99.0.0 '$NODE_NAME'"
  expect_rc "a --min-version newer than this proxyctl: refused (1)" 1
  expect "min-version: nodes.conf unchanged" test "$(sha_of /etc/singbox-sub-manager/nodes.conf)" = "$nodes_after_add"

  # --- disable (from the node's uninstall output) ---
  paste_cmd "$DISABLE_CMD"
  expect_rc "paste the node's disable command: 0" 0
  expect "disable: published 2 enabled nodes" out_has "published 2 enabled node(s)"
  expect "disable: the node is ENABLED=false in nodes.conf" node_enabled_in_conf "$NODE_NAME" false
  expect "disable: the served clash.yaml no longer lists the node" served_lacks clash.yaml "$uuid"
  expect "disable: the served sr.txt no longer lists the node" served_lacks sr.txt "$uuid"
  expect "disable: the hysteria2 nodes stay" nodes_has "[HY2-B]"
  expect "disable: no transaction area or tombstone is left" publish_clean

  # --- enable (from the node's reinstall output) ---
  paste_cmd "$ENABLE_CMD"
  expect_rc "paste the node's enable command: 0" 0
  expect "enable: published 3 enabled nodes" out_has "published 3 enabled node(s)"
  expect "enable: the node is ENABLED=true again" node_enabled_in_conf "$NODE_NAME" true
  expect "enable: clash.yaml is byte-identical to the one after add (client-visible state restored)" \
    test "$(sha_of "$SUBROOT/$CENTER_TOKEN/clash.yaml")" = "$clash_after_add"
  expect "enable: sr.txt is byte-identical to the one after add" test "$(sha_of "$SUBROOT/$CENTER_TOKEN/sr.txt")" = "$sr_after_add"
  expect "enable: nodes.conf is byte-identical to the one after add" test "$(sha_of /etc/singbox-sub-manager/nodes.conf)" = "$nodes_after_add"

  # --- remove (from the node's purge output) ---
  paste_cmd "$REMOVE_CMD"
  expect_rc "paste the node's remove command: 0" 0
  expect "remove: published 2 enabled nodes" out_has "published 2 enabled node(s)"
  expect "remove: the node is gone from nodes.conf" bash -c '! grep -Fq "[$1]" /etc/singbox-sub-manager/nodes.conf' _ "$NODE_NAME"
  run_c node publish --recover
  expect_rc "publish --recover with nothing pending: 0" 0
}

test_real_flow() {
  local p
  if [[ "${PROXYCTL_REAL_FS_TESTS:-}" != 1 ]]; then
    echo "SKIP real-filesystem flow (opt-in env PROXYCTL_REAL_FS_TESTS=1 not set; it needs root, network and a throw-away container)"
    skipped=$((skipped + 1))
    return 0
  fi
  # Opt-in present: every missing prerequisite is a hard failure.
  if [[ "$(uname -s)" != Linux ]]; then bad "real flow: Linux is required"; return 0; fi
  if [[ "$(command -p id -u)" != 0 ]]; then bad "real flow: PROXYCTL_REAL_FS_TESTS=1 requires root"; return 0; fi
  for p in openssl curl setsid timeout tar; do
    if ! command -v "$p" >/dev/null 2>&1; then bad "real flow: $p is required"; return 0; fi
  done
  for p in "${NODE_PATHS[@]}" "${CENTER_PATHS[@]}" /usr/local/bin/proxyctl "$SYSTEMD_RUN_DIR"; do
    if [[ -e "$p" || -L "$p" ]]; then bad "real flow: refusing to run, $p already exists on this host (use a throw-away container)"; return 0; fi
  done
  if compgen -G '/var/lib/.proxyctl-reality-txn.tomb-*' >/dev/null; then
    bad "real flow: refusing to run, a transaction tombstone already exists"; return 0
  fi
  if port_open 443 || port_open "$NODE_PORT"; then bad "real flow: 127.0.0.1:443 or :$NODE_PORT is already in use"; return 0; fi
  for p in "${LOCK_PATHS[@]}"; do
    if [[ -e "$p" ]]; then LOCKS_PREEXISTING+=("$p"); fi
  done
  REAL_ACTIVE=1
  write_fake_systemctl
  test_node_side
  test_center_side
}

main() {
  if resolve_candidate; then
    test_strict_parse
    test_real_flow
  fi
}

main

echo
echo "v010-flow tests: $pass passed, $fail failed, $skipped section(s) skipped"
if [[ "$fail" -ne 0 ]]; then exit 1; fi
