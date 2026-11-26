#!/usr/bin/env bash
set -euo pipefail
ROOT=$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)
TMP=$(mktemp -d)
trap 'rm -rf "$TMP"' EXIT
mkdir -p "$TMP/bin"
cat > "$TMP/bin/remote-build" <<'MOCK'
#!/usr/bin/env bash
set -eu
cmd=
while (($#)); do
    case $1 in
        --name|--fetch) shift 2 ;;
        --clean) shift ;;
        --) shift; cmd=$1; break ;;
        *) shift ;;
    esac
done
printf '%s\n' "$cmd" >> "${FAKE_REMOTE_LOG:?}"
if [[ ${FAKE_FAIL_TARGETED:-0} == 1 && $cmd == *"go test -race"* && $cmd == *pkg/webui* ]]; then exit 42; fi
MOCK
# A recording tools/dev/test.sh for the merged trees that carry one (ENGINEX_TEST_CMD).
cat > "$TMP/bin/test.sh" <<'MOCK'
#!/usr/bin/env bash
set -eu
printf 'test.sh %s\n' "$*" >> "${FAKE_REMOTE_LOG:?}"
full=0
for arg in "$@"; do [[ $arg == --full ]] && full=1; done
if [[ ${FAKE_FAIL_TARGETED:-0} == 1 && $full == 0 ]]; then exit 42; fi
if [[ ${FAKE_FAIL_FULL:-0} == 1 && $full == 1 ]]; then exit 43; fi
MOCK
cat > "$TMP/bin/sshctl" <<'MOCK'
#!/bin/sh
exit 0
MOCK
chmod +x "$TMP/bin/remote-build" "$TMP/bin/sshctl" "$TMP/bin/test.sh"

new_repo() {
    local name=$1
    rm -rf "$TMP/$name" "$TMP/$name.git"
    git init --bare -q "$TMP/$name.git"
    git clone -q "$TMP/$name.git" "$TMP/$name"
    git -C "$TMP/$name" config user.name test
    git -C "$TMP/$name" config user.email test@example.invalid
    mkdir -p "$TMP/$name/docs" "$TMP/$name/pkg/webui" "$TMP/$name/tools/dev"
    printf '# checklist\n\n| # | Behaviour |\n| --- | --- |\n| 1 | base |\n' > "$TMP/$name/docs/REGRESSION-CHECKLIST.md"
    printf 'base\n' > "$TMP/$name/shared.txt"
    printf 'docs/REGRESSION-CHECKLIST.md merge=union\n' > "$TMP/$name/.gitattributes"
    git -C "$TMP/$name" add .
    git -C "$TMP/$name" commit -qm base
    git -C "$TMP/$name" switch -q -c agent/5306-integration
    git -C "$TMP/$name" push -qu origin HEAD:agent/5306-integration
    printf '%s\n' "$TMP/$name"
}
run_integrate() {
    local repo=$1
    shift
    FAKE_REMOTE_LOG="$TMP/remote.log" PATH="$TMP/bin:$PATH" SSHCTL="$TMP/bin/sshctl" REMOTE_BUILD_BIN="$TMP/bin/remote-build" \
        ENGINEX_BUILD_HOSTS=local ENGINEX_SKIP_HOST_PROBE=1 GIT_CONFIG_NOSYSTEM=1 GIT_CONFIG_GLOBAL="$TMP/empty.gitconfig" bash -c 'cd "$1" && exec "$2" "${@:3}" --into agent/5306-integration' bash "$repo" "$ROOT/tools/dev/integrate.sh" "$@"
}

clean_repo=$(new_repo clean)
git -C "$clean_repo" switch -q -c feature-clean
printf 'clean merge\n' > "$clean_repo/clean.txt"
git -C "$clean_repo" add clean.txt && git -C "$clean_repo" commit -qm clean
 git -C "$clean_repo" push -qu origin feature-clean
before=$(git -C "$clean_repo" ls-remote origin refs/heads/agent/5306-integration | cut -f1)
run_integrate "$clean_repo" feature-clean
grep -Fq "go test -race -timeout 30m" "$TMP/remote.log"
after=$(git -C "$clean_repo" ls-remote origin refs/heads/agent/5306-integration | cut -f1)
[[ $before != "$after" ]]
git -C "$clean_repo" fetch -q origin agent/5306-integration
git -C "$clean_repo" show origin/agent/5306-integration:clean.txt >/dev/null

duplicate_repo=$(new_repo duplicate)
git -C "$duplicate_repo" switch -q -c target-rows
printf '| 2 | target row | row 1 |\n' >> "$duplicate_repo/docs/REGRESSION-CHECKLIST.md"
printf 'package webui\n\nfunc TargetChecklistFixture() {}\n' > "$duplicate_repo/pkg/webui/target_rows.go"
git -C "$duplicate_repo" add docs/REGRESSION-CHECKLIST.md pkg/webui/target_rows.go && git -C "$duplicate_repo" commit -qm target-row
git -C "$duplicate_repo" push -qu origin HEAD:agent/5306-integration
base=$(git -C "$duplicate_repo" rev-parse HEAD~1)
git -C "$duplicate_repo" switch -q -c feature-row "$base"
mkdir -p "$duplicate_repo/pkg/webui"
printf '| 2 | branch row | row 2 |\n' >> "$duplicate_repo/docs/REGRESSION-CHECKLIST.md"
printf 'package webui\n\nfunc BranchChecklistFixture() {}\n' > "$duplicate_repo/pkg/webui/branch_rows.go"
git -C "$duplicate_repo" add docs/REGRESSION-CHECKLIST.md pkg/webui/branch_rows.go && git -C "$duplicate_repo" commit -qm feature-row
git -C "$duplicate_repo" push -qu origin feature-row
run_integrate "$duplicate_repo" feature-row
git -C "$duplicate_repo" fetch -q origin agent/5306-integration
git -C "$duplicate_repo" show origin/agent/5306-integration:docs/REGRESSION-CHECKLIST.md | grep -Fq '| 3 | branch row | row 3 |'
git -C "$duplicate_repo" show origin/agent/5306-integration:pkg/webui/target_rows.go >/dev/null
git -C "$duplicate_repo" show origin/agent/5306-integration:pkg/webui/branch_rows.go >/dev/null

conflict_repo=$(new_repo conflict)
git -C "$conflict_repo" switch -q -c target-change
printf 'target line\n' > "$conflict_repo/shared.txt"
git -C "$conflict_repo" add shared.txt && git -C "$conflict_repo" commit -qm target-change
 git -C "$conflict_repo" push -qu origin HEAD:agent/5306-integration
base=$(git -C "$conflict_repo" rev-parse HEAD~1)
git -C "$conflict_repo" switch -q -c feature-loss "$base"
printf 'branch line\n' > "$conflict_repo/shared.txt"
git -C "$conflict_repo" add shared.txt && git -C "$conflict_repo" commit -qm feature-loss
 git -C "$conflict_repo" push -qu origin feature-loss
before=$(git -C "$conflict_repo" ls-remote origin refs/heads/agent/5306-integration | cut -f1)
if run_integrate "$conflict_repo" feature-loss; then echo 'conflict merge unexpectedly passed' >&2; exit 1; fi
after=$(git -C "$conflict_repo" ls-remote origin refs/heads/agent/5306-integration | cut -f1)
[[ $before == "$after" ]]

fail_repo=$(new_repo targeted)
git -C "$fail_repo" switch -q -c feature-fail
printf 'package webui\n\nfunc FailingLaneFixture() {}\n' > "$fail_repo/pkg/webui/integrate_fixture.go"
git -C "$fail_repo" add pkg/webui/integrate_fixture.go && git -C "$fail_repo" commit -qm targeted-failure
 git -C "$fail_repo" push -qu origin feature-fail
before=$(git -C "$fail_repo" ls-remote origin refs/heads/agent/5306-integration | cut -f1)
if FAKE_FAIL_TARGETED=1 run_integrate "$fail_repo" feature-fail; then echo 'targeted failure unexpectedly passed' >&2; exit 1; fi
after=$(git -C "$fail_repo" ls-remote origin refs/heads/agent/5306-integration | cut -f1)
[[ $before == "$after" ]]

# With tools/dev/test.sh: one targeted run per merged branch against its pre-merge head, then one full run.
: > "$TMP/remote.log"
runner_repo=$(new_repo runner)
git -C "$runner_repo" switch -q -c feature-runner
printf 'runner\n' > "$runner_repo/runner.txt"
git -C "$runner_repo" add runner.txt && git -C "$runner_repo" commit -qm runner
git -C "$runner_repo" push -qu origin feature-runner
before=$(git -C "$runner_repo" ls-remote origin refs/heads/agent/5306-integration | cut -f1)
ENGINEX_TEST_CMD="$TMP/bin/test.sh" run_integrate "$runner_repo" feature-runner
after=$(git -C "$runner_repo" ls-remote origin refs/heads/agent/5306-integration | cut -f1)
[[ $before != "$after" ]]
[[ $(grep -c '^test.sh ' "$TMP/remote.log") == 2 ]]
grep -Eq '^test.sh --tree .* --base [0-9a-f]{40} --exclusive$' "$TMP/remote.log"
grep -Eq '^test.sh --tree .* --full --exclusive --no-cache$' "$TMP/remote.log"
! grep -q 'go test -race' "$TMP/remote.log"
for mode in TARGETED FULL; do
    fail_repo=$(new_repo "runner-fail-$mode")
    git -C "$fail_repo" switch -q -c feature-fail
    printf 'fail\n' > "$fail_repo/fail.txt"
    git -C "$fail_repo" add fail.txt && git -C "$fail_repo" commit -qm fail
    git -C "$fail_repo" push -qu origin feature-fail
    before=$(git -C "$fail_repo" ls-remote origin refs/heads/agent/5306-integration | cut -f1)
    rc=0
    if [[ $mode == TARGETED ]]; then
        FAKE_FAIL_TARGETED=1 ENGINEX_TEST_CMD="$TMP/bin/test.sh" run_integrate "$fail_repo" feature-fail || rc=$?
    else
        FAKE_FAIL_FULL=1 ENGINEX_TEST_CMD="$TMP/bin/test.sh" run_integrate "$fail_repo" feature-fail || rc=$?
    fi
    [[ $rc != 0 ]] || { echo "test.sh $mode failure unexpectedly passed" >&2; exit 1; }
    after=$(git -C "$fail_repo" ls-remote origin refs/heads/agent/5306-integration | cut -f1)
    [[ $before == "$after" ]]
done
printf 'integrate.sh clean merge, refused conflict, refused targeted failure, test.sh targeted+full runs, and refused test.sh targeted and full failures passed\n'
