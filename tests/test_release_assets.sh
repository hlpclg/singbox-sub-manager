#!/usr/bin/env bash
# shellcheck disable=SC2016
# v0.10 Task 13 (plan G1/G10, spec §14.4, §15, §18.3): same-tag Release
# integration. Everything here is offline and needs neither git history nor
# root, so it runs unchanged as root and as uid 1000.
#
#   synthetic mode (default)
#     - the three scripts carry the same version, v0.10.0;
#     - ci.yml and release.yml both register every script and every
#       tests/test_*.sh for `bash -n`, ShellCheck and execution (rules below);
#     - release.yml publishes exactly the two binaries, the three scripts and
#       checksums.txt, and its permissions/step order are unchanged in spirit
#       (publish is the last step, after every check, nothing is conditional);
#     - the line of release.yml that writes checksums.txt (marked
#       "release-checksums-command") is EXTRACTED AND EXECUTED here against a
#       synthetic dist directory (two fake binaries plus the three real
#       scripts); the v0.9 parser then finds exactly one entry per binary.
#   DIST_DIR=<dir> mode (release.yml runs it with DIST_DIR=dist)
#     - the same checksum assertions against the real dist directory, plus
#       sha256sum -c, the exact file list, byte equality of the three scripts
#       with the repository, and the version string of both binaries:
#       EXPECT_VERSION (default $GITHUB_REF_NAME) must match exactly. A binary
#       for the host architecture is run directly (`version`); the foreign one
#       (arm64 on an amd64 runner) is run through qemu-user (qemu-aarch64-static
#       or qemu-x86_64-static; release.yml installs qemu-user-static). It is
#       NOT checked through `go version -m`: the release build uses -trimpath,
#       and Go then leaves -ldflags (so -X main.Version) out of the recorded
#       build settings (golang/go#52372). Missing EXPECT_VERSION or a missing
#       emulator FAIL; nothing is skipped.
#
# CI registration rules (checked per workflow file):
#   R1 bash -n    : a command line `bash -n <path>` for each of the three
#                   scripts and each tests/test_*.sh.
#   R2 ShellCheck : <path> is an argument of a `shellcheck` command line for
#                   each of the three scripts and each tests/test_*.sh except
#                   SHELLCHECK_EXEMPT (existing tests that cannot pass
#                   ShellCheck 0.9.0 and may not be edited, spec §14.6).
#   R3 execution  : a command line `[VAR=val ...] bash tests/<name> [args]`
#                   for each tests/test_*.sh, without `||`, `&&`, `|`, `;`.
#                   Per-test extra requirements are in extra_exec_re; they are
#                   additions, never relaxations.
#   R4 coverage   : each of the three scripts is named by at least one test
#                   file, and every such test file satisfies R3 (so each
#                   script is really exercised).
#   R5 no waiver  : no `continue-on-error`, no `|| true` on a registration
#                   line, no `if:` on a step (a conditional step could skip
#                   silently).
# Commented-out lines never count.
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
CI_YML="${CI_YML:-$REPO_ROOT/.github/workflows/ci.yml}"
RELEASE_YML="${RELEASE_YML:-$REPO_ROOT/.github/workflows/release.yml}"
TARGET_VERSION="v0.10.0"
TEST_DIR="$(mktemp -d)"
trap 'rm -rf "$TEST_DIR"' EXIT

SCRIPTS=(bootstrap-node.sh install-proxy.sh merge-nodes.sh)
BINARIES=(proxyctl-linux-amd64 proxyctl-linux-arm64)
DIST_FILES=("${BINARIES[@]}" "${SCRIPTS[@]}")
# Existing tests that ShellCheck 0.9.0 reports findings for (info/warning, no
# disable directive) and that spec §14.6 forbids editing.
SHELLCHECK_EXEMPT=(tests/test_caddy_apt.sh)

pass=0
fail=0
ok() { echo "PASS: $1"; pass=$((pass + 1)); }
bad() { echo "FAIL: $1" >&2; fail=$((fail + 1)); }
expect() {
  local desc="$1"
  shift
  if "$@"; then ok "$desc"; else bad "$desc"; fi
}

in_array() { # NEEDLE ARRAY...
  local n="$1" x
  shift
  for x in "$@"; do [[ "$x" == "$n" ]] && return 0; done
  return 1
}

# ---------------------------------------------------------------------------
# 1. versions
# ---------------------------------------------------------------------------

script_version() { # script -> its built-in default version
  local f="$REPO_ROOT/$1" v
  v="$(sed -n -E 's/^PROXYCTL_VERSION="(\$\{PROXYCTL_VERSION:-)?(v[0-9][^"}]*)\}?"$/\2/p' "$f")"
  [[ "$(printf '%s\n' "$v" | grep -c .)" == 1 ]] || return 1
  printf '%s\n' "$v"
}

test_versions() {
  local s v
  for s in "${SCRIPTS[@]}"; do
    v="$(script_version "$s" || true)"
    expect "$s has exactly one PROXYCTL_VERSION default, $TARGET_VERSION (got '${v:-none}')" \
      test "$v" = "$TARGET_VERSION"
  done
}

# ---------------------------------------------------------------------------
# 2. workflow parsing
# ---------------------------------------------------------------------------

# normalize FILE OUT: "<first physical line>\t<command text>" per logical line;
# backslash continuations joined, blank and comment-only lines dropped.
normalize() {
  awk '
    function trim(s) { sub(/^[ \t]+/, "", s); sub(/[ \t]+$/, "", s); return s }
    {
      t = trim($0)
      if (have) { t = acc " " t; s = accstart } else { s = NR }
      if (t ~ /\\$/) { sub(/[ \t]*\\$/, "", t); acc = t; accstart = s; have = 1; next }
      have = 0
      if (t == "" || t ~ /^#/) next
      printf "%d\t%s\n", s, t
    }
  ' "$1" > "$2"
}

re_escape() { printf '%s' "$1" | sed -E 's/[][\.*^$+?(){}|/]/\\&/g'; }

# first_line NORM REGEX: line number of the first logical line matching.
# The regex travels through the environment: awk -v would process backslash
# escapes in it (gawk and mawk differ), ENVIRON does not.
first_line() {
  RE="$2" awk -F'\t' '$2 ~ ENVIRON["RE"] { print $1; exit }' "$1"
}
has_line() { [[ -n "$(first_line "$1" "$2")" ]]; }

RUN_PREFIX='^(run: )?'
ENV_ASSIGN='([A-Za-z_][A-Za-z0-9_]*=[^ ]+ )*'

bash_n_re() { printf '%sbash -n %s( +#.*)?$' "$RUN_PREFIX" "$(re_escape "$1")"; }
exec_re() { # test path
  printf '%s%sbash %s( [^|&;#<>]*)?( +#.*)?$' "$RUN_PREFIX" "$ENV_ASSIGN" "$(re_escape "$1")"
}

# sc_args NORM: every argument of every ShellCheck command line.
shellcheck_args() {
  awk -F'\t' '
    $2 ~ /^(run: )?(sudo )?shellcheck( |$)/ {
      line = $2
      sub(/^(run: )?(sudo )?shellcheck/, "", line)
      if (line ~ /[|&;]/) next
      n = split(line, a, " ")
      for (i = 1; i <= n; i++) print a[i]
    }
  ' "$1"
}

TEST_FILES=()
for f in "$REPO_ROOT"/tests/test_*.sh; do TEST_FILES+=("tests/$(basename "$f")"); done

# Extra execution requirements (additions to R3), per workflow.
# tests/test_reality_probe.sh refuses to run without --verify-assets.
# tests/test_caddy_apt.sh: the offline unit tests run in ci.yml; release.yml
# runs the APT smoke variant in a container job (CADDY_APT_SMOKE=1, root).
extra_exec_re() { # workflow-kind test-path
  case "$2" in
    tests/test_reality_probe.sh)
      printf '%s%sbash tests/test_reality_probe\\.sh --verify-assets( +#.*)?$' "$RUN_PREFIX" "$ENV_ASSIGN" ;;
    tests/test_caddy_apt.sh)
      if [[ "$1" == release ]]; then
        printf '%sCADDY_APT_SMOKE=1 bash tests/test_caddy_apt\\.sh( +#.*)?$' "$RUN_PREFIX"
      fi ;;
  esac
}

check_registration() { # kind(ci|release) yml
  local kind="$1" yml="$2" norm="$TEST_DIR/$1.norm" s t extra args found
  normalize "$yml" "$norm"

  for s in "${SCRIPTS[@]}"; do
    expect "$kind: R1 bash -n $s" has_line "$norm" "$(bash_n_re "$s")"
  done
  for t in "${TEST_FILES[@]}"; do
    expect "$kind: R1 bash -n $t" has_line "$norm" "$(bash_n_re "$t")"
  done

  args="$TEST_DIR/$kind.shellcheck.args"
  shellcheck_args "$norm" > "$args"
  for s in "${SCRIPTS[@]}"; do
    expect "$kind: R2 shellcheck $s" grep -qxF -- "$s" "$args"
  done
  for t in "${TEST_FILES[@]}"; do
    if in_array "$t" "${SHELLCHECK_EXEMPT[@]}"; then
      expect "$kind: R2 $t is a documented ShellCheck exemption and is not listed as if checked" \
        bash -c '! grep -qxF -- "$1" "$2"' _ "$t" "$args"
    else
      expect "$kind: R2 shellcheck $t" grep -qxF -- "$t" "$args"
    fi
  done

  for t in "${TEST_FILES[@]}"; do
    expect "$kind: R3 executes $t" has_line "$norm" "$(exec_re "$t")"
    extra="$(extra_exec_re "$kind" "$t")"
    if [[ -n "$extra" ]]; then
      expect "$kind: R3 executes $t in its required form" has_line "$norm" "$extra"
    fi
  done

  # R4: every script is named by at least one test, and each such test is
  # registered for execution in this workflow.
  for s in "${SCRIPTS[@]}"; do
    found=0
    for t in "${TEST_FILES[@]}"; do
      if [[ "$t" != tests/test_release_assets.sh ]] && grep -qF -- "$s" "$REPO_ROOT/$t"; then
        found=$((found + 1))
        has_line "$norm" "$(exec_re "$t")" || bad "$kind: R4 $s is referenced by $t which is not executed"
      fi
    done
    expect "$kind: R4 $s is exercised by at least one executed test ($found)" test "$found" -gt 0
  done

  # R5: nothing can silently waive a check.
  expect "$kind: R5 no continue-on-error" bash -c '! grep -qE "^[[:space:]]*continue-on-error:" "$1"' _ "$yml"
  expect "$kind: R5 no step-level if: (a conditional step could skip silently)" \
    bash -c '! grep -qE "^[[:space:]]*(- )?if:" "$1"' _ "$yml"
  expect "$kind: R5 no registration line is followed by || true" \
    bash -c '! grep -E "bash -n |shellcheck |bash tests/" "$1" | grep -qE "\|\|"' _ "$norm"
}

yml_permissions() { # yml -> "key: value" lines of the top-level permissions block
  awk '/^permissions:/ { in_p = 1; next } in_p && /^  [A-Za-z-]+:/ { gsub(/^ +/, ""); print; next } in_p { exit }' "$1"
}

test_workflows() {
  local norm="$TEST_DIR/release.norm" listed expected publish cp_line mark_line dist_line plain_line cd_line
  expect "ci.yml exists" test -f "$CI_YML"
  expect "release.yml exists" test -f "$RELEASE_YML"
  expect "the new shell tests exist" bash -c '[[ -f "$1/tests/test_release_assets.sh" && -f "$1/tests/test_v010_flow.sh" ]]' _ "$REPO_ROOT"

  check_registration ci "$CI_YML"
  check_registration release "$RELEASE_YML"

  expect "ci.yml permissions are still contents: read only" test "$(yml_permissions "$CI_YML")" = "contents: read"
  expect "release.yml permissions are still contents: write only" test "$(yml_permissions "$RELEASE_YML")" = "contents: write"

  # --- release.yml: assets, ordering, no publish on failure ---
  normalize "$RELEASE_YML" "$norm"
  listed="$(awk '
    /files: *\|/ { in_f = 1; next }
    in_f && /^[[:space:]]+[^[:space:]]+[[:space:]]*$/ { gsub(/^[[:space:]]+|[[:space:]]+$/, ""); print; next }
    in_f { exit }
  ' "$RELEASE_YML" | LC_ALL=C sort | tr '\n' ' ')"
  expected="dist/bootstrap-node.sh dist/checksums.txt dist/install-proxy.sh dist/merge-nodes.sh dist/proxyctl-linux-amd64 dist/proxyctl-linux-arm64 "
  expect "release.yml publishes exactly the two binaries, the three scripts and checksums.txt (got: $listed)" \
    test "$listed" = "$expected"
  expect "release.yml has exactly one softprops publish step" \
    test "$(grep -c 'uses: softprops/action-gh-release@' "$RELEASE_YML")" = 1
  expect "release.yml keeps generate_release_notes" grep -q 'generate_release_notes: true' "$RELEASE_YML"

  publish="$(first_line "$norm" '^(- )?uses: softprops/action-gh-release@')"
  # the publish step is the last step: no later step header
  expect "the publish step is the last step of the release job" \
    bash -c '[[ -n "$2" ]] && ! awk -v p="$2" "NR > p && /^      - (name|uses):/ { f = 1 } END { exit !f }" "$1"' _ "$RELEASE_YML" "$publish"

  cp_line="$(first_line "$norm" '^cp install-proxy\.sh merge-nodes\.sh bootstrap-node\.sh dist/?$')"
  cd_line="$(first_line "$norm" '^cd dist$')"
  mark_line="$(awk -F'\t' '$2 ~ /# release-checksums-command$/ { print $1; exit }' "$norm")"
  dist_line="$(first_line "$norm" '^(run: )?([A-Za-z_][A-Za-z0-9_]*=[^ ]+ )*DIST_DIR=dist ([A-Za-z_][A-Za-z0-9_]*=[^ ]+ )*bash tests/test_release_assets\.sh$')"
  plain_line="$(first_line "$norm" '^(run: )?bash tests/test_release_assets\.sh$')"
  expect "release.yml copies the three scripts into dist/ (cp ... dist/)" test -n "$cp_line"
  expect "release.yml has exactly one marked checksums command" \
    test "$(grep -c '# release-checksums-command' "$RELEASE_YML")" = 1
  expect "release.yml verifies dist with DIST_DIR=dist bash tests/test_release_assets.sh" test -n "$dist_line"
  expect "release.yml also runs the synthetic mode of test_release_assets.sh" test -n "$plain_line"
  expect "the DIST_DIR verification passes the tag (EXPECT_VERSION=\"\$GITHUB_REF_NAME\")" \
    grep -qF 'EXPECT_VERSION="$GITHUB_REF_NAME"' "$RELEASE_YML"
  expect "release.yml still runs the amd64 binary and compares its version to the tag" \
    has_line "$norm" '^test "\$\(\./dist/proxyctl-linux-amd64 version\)" = "\$GITHUB_REF_NAME"$'
  expect "order: cp scripts < cd dist < checksums command < DIST_DIR check < publish" \
    bash -c '[[ -n "$1" && -n "$2" && -n "$3" && -n "$4" && -n "$5" && "$1" -lt "$2" && "$2" -lt "$3" && "$3" -lt "$4" && "$4" -lt "$5" ]]' _ \
    "$cp_line" "$cd_line" "$mark_line" "$dist_line" "$publish"
  expect "the checksums command runs after 'cd dist' (the last cd before it)" \
    bash -c 'l="$(awk -F"\t" -v m="$2" "\$1 < m && \$2 ~ /^cd / { c = \$2 } END { print c }" "$1")"; [[ "$l" == "cd dist" ]]' _ "$norm" "${mark_line:-0}"
}

# ---------------------------------------------------------------------------
# 3. checksums.txt (v0.9 parser compatibility)
# ---------------------------------------------------------------------------

# Copied verbatim from `git show df3a4f4:install-proxy.sh` (v0.9.0), the
# expected_count / expected_sha256 assignments (lines 380-381): the way a v0.9
# `install-proxy.sh update` reads checksums.txt. Do not edit.
V09_COUNT_AWK='$1 ~ /^[[:xdigit:]]{64}$/ && ($2 == asset || $2 == "*" asset) { count++ } END { print count+0 }'
V09_SHA_AWK='$1 ~ /^[[:xdigit:]]{64}$/ && ($2 == asset || $2 == "*" asset) { print tolower($1) }'
# The scripts must also run on mawk 1.3.4 20200120 (Debian 12's default awk),
# which has no {n} interval expressions: the same predicate, spelled with
# length() instead. checksum_assertions uses this form so the test runs there.
PORT_PRED='length($1) == 64 && $1 ~ /^[[:xdigit:]]+$/'
PORT_COUNT_AWK="$PORT_PRED"' && ($2 == asset || $2 == "*" asset) { count++ } END { print count+0 }'
PORT_SHA_AWK="$PORT_PRED"' && ($2 == asset || $2 == "*" asset) { print tolower($1) }'
v09_count() { awk -v asset="$1" "$PORT_COUNT_AWK" "$2"; }
v09_sha() { awk -v asset="$1" "$PORT_SHA_AWK" "$2"; }

sha_of() { sha256sum "$1" | awk '{print $1}'; }

# The REAL checksum command from release.yml (the one marked line).
extract_checksums_command() {
  local line
  line="$(grep -F '# release-checksums-command' "$RELEASE_YML" | head -n 1 || true)"
  line="${line%%# release-checksums-command*}"
  printf '%s\n' "$line" | sed -E 's/^[[:space:]]*//; s/^run:[[:space:]]*//; s/[[:space:]]+$//'
}

# A synthetic dist: two fake binaries (not executable programs) and the three
# real scripts, then the real command.
build_synthetic_dist() { # DIR
  local d="$1" b s cmd
  mkdir -p "$d"
  for b in "${BINARIES[@]}"; do printf 'fake %s binary\n' "$b" > "$d/$b"; done
  for s in "${SCRIPTS[@]}"; do cp "$REPO_ROOT/$s" "$d/$s"; done
  cmd="$(extract_checksums_command)"
  [[ -n "$cmd" ]] || return 1
  (cd "$d" && bash -c "$cmd")
}

# checksum_assertions LABEL DIR: the structural contract of DIR/checksums.txt.
checksum_assertions() {
  local label="$1" d="$2" sums="$2/checksums.txt" f n got want
  expect "$label: checksums.txt exists" test -s "$sums"
  expect "$label: checksums.txt has exactly 5 lines (one per file)" test "$(grep -c . "$sums")" = 5
  expect "$label: no entry carries a path (file names only)" bash -c '! awk "{print \$2}" "$1" | grep -q /' _ "$sums"
  for f in "${DIST_FILES[@]}"; do
    n="$(v09_count "$f" "$sums")"
    expect "$label: v0.9 parser finds exactly one entry for $f (got $n)" test "$n" = 1
    got="$(v09_sha "$f" "$sums")"
    want="$(sha_of "$d/$f")"
    expect "$label: the entry for $f carries the file's SHA-256" test "$got" = "$want"
  done
}

test_checksums_synthetic() {
  local d="$TEST_DIR/dist" sums bad_sums b
  expect "the marked release.yml command can be extracted" test -n "$(extract_checksums_command)"
  expect "the extracted command writes checksums.txt in the current directory" \
    bash -c '[[ "$1" == *"> checksums.txt" ]]' _ "$(extract_checksums_command)"
  if ! build_synthetic_dist "$d"; then
    bad "synthetic dist: the release.yml checksums command failed"
    return
  fi
  ok "synthetic dist: the release.yml checksums command ran"
  sums="$d/checksums.txt"
  checksum_assertions "synthetic dist" "$d"
  expect "synthetic dist: sha256sum -c accepts it" bash -c 'cd "$1" && sha256sum -c checksums.txt >/dev/null' _ "$d"

  # failure paths: the v0.9 parser must see a count other than 1.
  for b in "${BINARIES[@]}"; do
    bad_sums="$TEST_DIR/dup-$b.txt"
    { cat "$sums"; grep -F " $b" "$sums" | head -n 1; } > "$bad_sums"
    expect "duplicate $b entry: v0.9 parser count is not 1 (would be rejected)" \
      test "$(v09_count "$b" "$bad_sums")" != 1
    bad_sums="$TEST_DIR/missing-$b.txt"
    grep -vF " $b" "$sums" > "$bad_sums"
    expect "missing $b entry: v0.9 parser count is 0 (would be rejected)" \
      test "$(v09_count "$b" "$bad_sums")" = 0
    bad_sums="$TEST_DIR/path-$b.txt"
    sed "s# $b# dist/$b#" "$sums" > "$bad_sums"
    expect "path-prefixed $b entry: v0.9 parser count is 0 (would be rejected)" \
      test "$(v09_count "$b" "$bad_sums")" = 0
  done

  # the scripts read checksums.txt with the v0.9 semantics, portably.
  local sc
  for sc in "${SCRIPTS[@]}"; do
    expect "$sc: checksum awk programs avoid {n} interval expressions" \
      bash -c '! grep -F "xdigit:]]{" "$1" | grep -q awk' _ "$REPO_ROOT/$sc"
    expect "$sc: checksum awk programs use the portable 64-hex predicate" \
      bash -c '[[ "$(grep -cF -- "$2" "$1")" -ge 2 ]]' _ "$REPO_ROOT/$sc" "$PORT_PRED"
  done
}

# The portable predicate is the v0.9 predicate: same answers on a table of
# lines. The v0.9 text itself needs interval support, so it only takes part
# when this awk has it; the expected answers are fixed either way.
test_checksum_predicate() {
  local h64 f="$TEST_DIR/pred.txt" n
  h64="$(printf 'a%.0s' $(seq 1 64))"
  {
    echo "$h64  good"
    echo "${h64^^}  upper"
    echo "$h64 *star"
    echo "${h64:1}  short63"
    echo "${h64}a  long65"
    echo "${h64/a/g}  nothex"
    echo "$h64  good"
    echo "$h64  dist/path"
  } > "$f"
  expect "portable predicate: good appears twice -> 2" test "$(awk -v asset=good "$PORT_COUNT_AWK" "$f")" = 2
  expect "portable predicate: upper-case hex accepted (lower-cased on output)" \
    test "$(awk -v asset=upper "$PORT_SHA_AWK" "$f")" = "$h64"
  expect "portable predicate: *name accepted" test "$(awk -v asset=star "$PORT_COUNT_AWK" "$f")" = 1
  for n in short63 long65 nothex dist/path path; do
    expect "portable predicate: $n -> 0" test "$(awk -v asset="$n" "$PORT_COUNT_AWK" "$f")" = 0
  done
  if [[ "$(echo aaaa | awk '/^[a]{4}$/ { print "y" }')" == y ]]; then
    for n in good upper star short63 long65 nothex; do
      expect "v0.9 text and portable predicate agree on $n" \
        test "$(awk -v asset="$n" "$V09_COUNT_AWK" "$f")" = "$(awk -v asset="$n" "$PORT_COUNT_AWK" "$f")"
    done
    expect "v0.9 text and portable predicate agree on the extracted SHA" \
      test "$(awk -v asset=upper "$V09_SHA_AWK" "$f")" = "$(awk -v asset=upper "$PORT_SHA_AWK" "$f")"
  else
    echo "NOTE: this awk has no interval expressions; the v0.9 text cannot be compared here"
  fi
}

# ---------------------------------------------------------------------------
# 4. DIST_DIR mode
# ---------------------------------------------------------------------------

host_arch() {
  case "$(uname -m)" in
    x86_64 | amd64) echo amd64 ;;
    aarch64 | arm64) echo arm64 ;;
    *) uname -m ;;
  esac
}

# run_version ARCH FILE: raw stdout of `FILE version`, run natively when ARCH is
# the host architecture, otherwise through qemu-user. Fails when it cannot run.
run_version() {
  local arch="$1" f="$2" q qarch
  if [[ "$arch" == "$(host_arch)" ]]; then
    "$f" version 2>/dev/null
    return
  fi
  case "$arch" in
    amd64) qarch=x86_64 ;;
    arm64) qarch=aarch64 ;;
    *) return 127 ;;
  esac
  q="$(command -v "qemu-$qarch-static" || command -v "qemu-$qarch" || true)"
  [[ -n "$q" ]] || return 127
  "$q" "$f" version 2>/dev/null
}

test_dist() {
  local d="$1" want="${EXPECT_VERSION:-${GITHUB_REF_NAME:-}}" f listing expected arch run how
  expect "DIST_DIR $d is a directory" test -d "$d"
  expect "DIST_DIR mode has the expected version (EXPECT_VERSION or GITHUB_REF_NAME)" test -n "$want"
  [[ -d "$d" && -n "$want" ]] || return 0

  listing="$(find "$d" -mindepth 1 -maxdepth 1 -printf '%f\n' | LC_ALL=C sort | tr '\n' ' ')"
  expected="$(printf '%s\n' "${DIST_FILES[@]}" checksums.txt | LC_ALL=C sort | tr '\n' ' ')"
  expect "dist holds exactly the five files and checksums.txt (got: $listing)" test "$listing" = "$expected"
  for f in "${DIST_FILES[@]}" checksums.txt; do
    expect "dist/$f is a regular non-empty file" bash -c '[[ -f "$1" && ! -L "$1" && -s "$1" ]]' _ "$d/$f"
  done

  checksum_assertions "dist" "$d"
  expect "dist: sha256sum -c checksums.txt succeeds" bash -c 'cd "$1" && sha256sum -c checksums.txt >/dev/null' _ "$d"
  for f in "${SCRIPTS[@]}"; do
    expect "dist/$f is byte-identical to the repository script" cmp -s "$d/$f" "$REPO_ROOT/$f"
  done
  expect "the tag $want equals the scripts' built-in version $TARGET_VERSION" test "$want" = "$TARGET_VERSION"

  for arch in amd64 arm64; do
    f="$d/proxyctl-linux-$arch"
    run="$(run_version "$arch" "$f"; printf x)" || true
    if [[ "$arch" == "$(host_arch)" ]]; then how="natively"; else how="through qemu-user"; fi
    expect "dist: proxyctl-linux-$arch run $how prints exactly '$want' (got '${run%x}')" \
      test "$run" = "$want"$'\n'x
  done
}

main() {
  test_versions
  test_workflows
  test_checksums_synthetic
  if [[ -n "${DIST_DIR:-}" ]]; then
    test_dist "$DIST_DIR"
  fi
}

main

echo
echo "release-assets tests: $pass passed, $fail failed"
if [[ "$fail" -ne 0 ]]; then exit 1; fi
