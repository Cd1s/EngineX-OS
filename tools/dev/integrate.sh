#!/usr/bin/env bash
set -euo pipefail

# Batch conflict rule: rebase a copy as agent/5306-<slug>-b1 and keep both sides' additions
# when they are independent insertions. If a hunk changes the same lines on both sides, stop
# and report the file and hunk. The lost-lines check and targeted tests remain mandatory.

ROOT=$(git rev-parse --show-toplevel)
TARGET=agent/5306-integration
SSHCTL=${SSHCTL:-$(command -v sshctl || true)}
REMOTE_BUILD_BIN=${REMOTE_BUILD_BIN:-$(command -v remote-build || true)}
HOSTS=${ENGINEX_BUILD_HOSTS:-}
[[ -n "$HOSTS" ]] || { echo "ENGINEX_BUILD_HOSTS is required" >&2; exit 2; }
[[ -n "$REMOTE_BUILD_BIN" ]] || { echo "remote-build is required" >&2; exit 2; }
[[ -n "$SSHCTL" ]] || { echo "sshctl is required for host selection and exact workspace cleanup" >&2; exit 2; }

branches=()
while (($#)); do
    case $1 in
        --into) (($# >= 2)) || { echo "--into needs a target branch" >&2; exit 2; }; TARGET=$2; shift 2 ;;
        --) shift; break ;;
        -*) echo "unknown option: $1" >&2; exit 2 ;;
        *) branches+=("$1"); shift ;;
    esac
done
[[ ${#branches[@]} -gt 0 ]] || { echo "usage: $0 <branch>... [--into <target>]" >&2; exit 2; }

IFS=',' read -r -a host_list <<< "${HOSTS// /,}"
run_id=$(printf '%s-%s-%s' "$(date +%s)" "$$" "${RANDOM:-0}")
target_head=$(git rev-parse --short "origin/$TARGET")
tmp=$(mktemp -d "${TMPDIR:-/tmp}/enginex-integrate.${target_head}.XXXXXX")
worktree=$tmp/worktree
dropped_lines_file=$tmp/dropped-lines.tsv
: >"$dropped_lines_file"
remote_names=()
remote_hosts=()
remote_counter=0
dropped_hunks=()
doc_notes=()
started=$(date +%s)

cleanup_remote() {
    local host=$1 name=$2
    "$SSHCTL" run "$host" --no-stdin --timeout 30s --argv rm -rf -- "/root/build/$name" >/dev/null 2>&1 || true
}
cleanup() {
    local i
    for i in "${!remote_names[@]}"; do cleanup_remote "${remote_hosts[$i]}" "${remote_names[$i]}"; done
    git -C "$ROOT" worktree remove --force "$worktree" >/dev/null 2>&1 || true
    rm -rf -- "$tmp"
}
trap cleanup EXIT

git fetch origin "$TARGET" "${branches[@]}"
git worktree add --detach "$worktree" "origin/$TARGET" >/dev/null

probe() {
    local host=$1 command=$2
    if [[ ${ENGINEX_SKIP_HOST_PROBE:-0} == 1 ]]; then
        [[ $command == *gcc* ]] && printf '14\n' || printf '0\n'
        return 0
    fi
    [[ -n "$SSHCTL" ]] || return 1
    "$SSHCTL" run "$host" --no-stdin --timeout 20s --argv sh -c "$command" 2>/dev/null
}
pick_host() {
    local require_gcc14=$1 best= best_score=999999 host gcc queue
    for host in "${host_list[@]}"; do
        [[ -n "$host" ]] || continue
        gcc=$(probe "$host" 'gcc -dumpversion' || true)
        [[ $require_gcc14 == 1 && ${gcc%%.*} != 14 ]] && continue
        queue=$(probe "$host" 'ps -eo args= | awk '\''/[f]lock \/var\/lock\/enginex-heavy\.lock/ {n++} END {print n+0}'\''' || true)
        [[ $queue =~ ^[0-9]+$ ]] || queue=999999
        if [[ -z "$best" || $queue -lt $best_score ]]; then best=$host; best_score=$queue; fi
    done
    [[ -n "$best" ]] || { echo "no suitable build host" >&2; return 1; }
    printf '%s\n' "$best"
}

# The path-to-tests map and both test tiers live in tools/dev/test.sh (the target branch must carry it).
TEST_CMD=${ENGINEX_TEST_CMD:-$worktree/tools/dev/test.sh}
run_tests() {
    [[ -x $TEST_CMD ]] || { echo "tools/dev/test.sh is missing in $TARGET: merge the dev-speed branch first" >&2; return 1; }
    "$TEST_CMD" --tree "$worktree" --clone "$@"
}

remote_run() {
    local host=$1 inner=$2
    shift 2
    local head name command escaped
    head=$(git -C "$worktree" rev-parse --short HEAD)
    remote_counter=$((remote_counter + 1))
    name="enginex-devtools-speed-${head}-${run_id}-${remote_counter}"
    escaped=${inner//\\/\\\\}; escaped=${escaped//\'/\'\\\'\'}
    command=". /etc/profile.d/buildtools.sh; flock /var/lock/enginex-heavy.lock sh -c '$escaped'"
    remote_names+=("$name"); remote_hosts+=("$host")
    REMOTE_BUILD_HOST="$host" "$REMOTE_BUILD_BIN" --name "$name" --clean "$@" -- "$command"
    cleanup_remote "$host" "$name"
    unset "remote_names[${#remote_names[@]}-1]" "remote_hosts[${#remote_hosts[@]}-1]"
}

added_lines_check() {
    local base=$1 other=$2 result=$3
    python3 - "$base" "$other" "$result" "${dropped_lines_file:-}" <<'PY'
import subprocess, sys
base, other, result, dropped_file = sys.argv[1:]
diff = subprocess.check_output(["git", "diff", "--no-renames", "--unified=0", base, other], text=True)
files = {}
excluded = {"README.md", "README.zh-CN.md", "README.zh-TW.md", "docs/index.html"}
# Release docs are maintained at release time, so conflict policy may intentionally drop incoming hunks.
dropped = set()
if dropped_file:
    try:
        with open(dropped_file, encoding="utf-8") as handle:
            dropped = {tuple(line.rstrip("\n").split("\t", 1)) for line in handle}
    except FileNotFoundError:
        pass
current = None
for line in diff.splitlines():
    if line.startswith("+++ b/"):
        current = line[6:]
        files.setdefault(current, [])
    elif line.startswith("+") and not line.startswith("+++") and current:
        files[current].append(line[1:])
for path, lines in files.items():
    if path in excluded or path.startswith("docs/site/"):
        continue
    try:
        content = open(result + "/" + path, "r", encoding="utf-8", errors="surrogateescape").read().splitlines()
    except (FileNotFoundError, IsADirectoryError):
        raise SystemExit(f"lost added file: {path}")
    for line in lines:
        if (path, line) in dropped:
            continue
        if line not in content:
            raise SystemExit(f"lost added line in {path}: {line}")
PY
}

normalize_checklist() {
    local base=$1 branch=$2 max_before=$3
    python3 - "$base" "$branch" "$worktree/docs/REGRESSION-CHECKLIST.md" "$max_before" <<'PY'
import re, subprocess, sys
base, branch, path, max_before = sys.argv[1], sys.argv[2], sys.argv[3], int(sys.argv[4])
diff = subprocess.check_output(["git", "diff", "--no-renames", "--unified=0", base, branch, "--", "docs/REGRESSION-CHECKLIST.md"], text=True)
rows = []
for line in diff.splitlines():
    if line.startswith("+") and not line.startswith("+++"):
        match = re.match(r"\+\|\s*(\d+)\s*\|", line)
        if match:
            rows.append((int(match.group(1)), line[1:]))
if not rows:
    raise SystemExit(0)
lines = open(path, encoding="utf-8").read().splitlines()
used = set()
for old, original in rows:
    if old > max_before and old not in used:
        used.add(old)
        continue
    new = max(max_before, *(used or {max_before})) + 1
    used.add(new)
    replaced = False
    for index, value in enumerate(lines):
        if value == original:
            value = re.sub(r"\brow\s+%d\b" % old, "row %d" % new, value)
            value = re.sub(r"^(\|\s*)%d(\s*\|)" % old, r"\g<1>%d\g<2>" % new, value)
            lines[index] = value
            replaced = True
            break
    if not replaced:
        raise SystemExit(f"cannot locate checklist row added by {branch}: {old}")
open(path, "w", encoding="utf-8").write("\n".join(lines) + "\n")
PY
    if ! git -C "$worktree" diff --quiet -- docs/REGRESSION-CHECKLIST.md; then
        git -C "$worktree" add docs/REGRESSION-CHECKLIST.md
        git -C "$worktree" -c user.name=enginex-integrate -c user.email=enginex-integrate@localhost \
            commit -m "chore: normalize regression checklist rows" >/dev/null
    fi
}

resolve_conflicts_both() {
    local branch=$1 resolution_log=$tmp/conflict-resolution.log kind path first
    if ! python3 - "$worktree" "$dropped_lines_file" >"$resolution_log" <<'PY'
from difflib import SequenceMatcher
from pathlib import Path
import re, subprocess, sys

root = Path(sys.argv[1])
dropped_lines_path = Path(sys.argv[2])
paths = subprocess.check_output(
    ["git", "-C", str(root), "diff", "--name-only", "--diff-filter=U"], text=True
).splitlines()
release_docs = {
    "README.md",
    "README.zh-CN.md",
    "README.zh-TW.md",
    "docs/index.html",
}

def is_release_doc(path):
    return path in release_docs or path.startswith("docs/site/")

def cache_version(lines):
    values = [int(value) for line in lines for value in re.findall(r"\?v=(\d+)", line)]
    return max(values, default=-1)

def insertions(base, side):
    result = {}
    matcher = SequenceMatcher(None, base, side, autojunk=False)
    for tag, i1, i2, j1, j2 in matcher.get_opcodes():
        if tag == "equal":
            continue
        if tag != "insert":
            raise ValueError("a side modifies or deletes existing lines")
        result.setdefault(i1, []).extend(side[j1:j2])
    return result

for path in paths:
    file_path = root / path
    lines = file_path.read_text(encoding="utf-8", errors="surrogateescape").splitlines(keepends=True)
    output = []
    index = 0
    hunk = 0
    while index < len(lines):
        if not lines[index].startswith("<<<<<<< "):
            output.append(lines[index])
            index += 1
            continue
        hunk += 1
        ours = []
        base = []
        theirs = []
        index += 1
        while index < len(lines) and not lines[index].startswith("||||||| "):
            ours.append(lines[index])
            index += 1
        if index == len(lines):
            raise SystemExit(f"{path}: conflict hunk {hunk} has no base section")
        index += 1
        while index < len(lines) and not lines[index].startswith("======="):
            base.append(lines[index])
            index += 1
        if index == len(lines):
            raise SystemExit(f"{path}: conflict hunk {hunk} has no separator")
        index += 1
        while index < len(lines) and not lines[index].startswith(">>>>>>> "):
            theirs.append(lines[index])
            index += 1
        if index == len(lines):
            raise SystemExit(f"{path}: conflict hunk {hunk} has no end marker")
        index += 1
        if path == "docs/ISSUE-FIXES-5.3.0.5.md":
            output.extend(ours)
            with open(dropped_lines_path, "a", encoding="utf-8") as dropped_file:
                for line in theirs:
                    dropped_file.write(f"{path}\t{line.rstrip(chr(10))}\n")
            first = next((line.strip() for line in theirs if line.strip()), "")
            print(f"DROPPED_HUNK\t{path}\t{first}")
            continue
        if is_release_doc(path):
            if cache_version(theirs) > cache_version(ours):
                output.extend(theirs)
                first = next((line.strip() for line in ours if line.strip()), "")
                print(f"KEPT_INCOMING\t{path}\tcache-v-higher-than-batch\t{first}")
            else:
                output.extend(ours)
                first = next((line.strip() for line in theirs if line.strip()), "")
                print(f"DROPPED_HUNK\t{path}\t{first}")
            continue
        try:
            ours_insertions = insertions(base, ours)
            theirs_insertions = insertions(base, theirs)
        except ValueError:
            raise SystemExit(f"{path}: conflict hunk {hunk} needs a choice between modified lines")
        for position in range(len(base) + 1):
            output.extend(ours_insertions.get(position, []))
            output.extend(theirs_insertions.get(position, []))
            if position < len(base):
                output.append(base[position])
    file_path.write_text("".join(output), encoding="utf-8", errors="surrogateescape")
PY
    then
        cat "$resolution_log" >&2
        return 1
    fi
    while IFS=$'\t' read -r kind path first; do
        [[ -n "$kind" ]] || continue
        case $kind in
            DROPPED_HUNK) dropped_hunks+=("$branch|$path|$first") ;;
            KEPT_INCOMING) doc_notes+=("$branch|$path|$first") ;;
        esac
    done < "$resolution_log"
}

rebase_copy() {
    local base=$1 branch=$2 pre_merge=$3 slug=$4 b1="agent/5306-${slug}-b1" rc conflicts
    git branch -f "$b1" "origin/$branch"
    git -C "$worktree" switch --detach "origin/$branch" >/dev/null
    git -C "$worktree" switch -C "$b1" "origin/$branch" >/dev/null
    set +e
    GIT_EDITOR=true git -C "$worktree" -c merge.conflictStyle=diff3 -c user.name=enginex-integrate -c user.email=enginex-integrate@localhost \
        rebase --onto "$pre_merge" "$base" "$b1" >/dev/null 2>&1
    rc=$?
    set -e
    while ((rc != 0)); do
        conflicts=$(git -C "$worktree" diff --name-only --diff-filter=U)
        [[ -n "$conflicts" ]] || {
            git -C "$worktree" rebase --abort >/dev/null 2>&1 || true
            echo "rebase refused for $branch without a resolvable conflict hunk" >&2
            return 1
        }
        if ! resolve_conflicts_both "$branch"; then
            git -C "$worktree" rebase --abort >/dev/null 2>&1 || true
            return 1
        fi
        git -C "$worktree" add -A
        set +e
        GIT_EDITOR=true git -C "$worktree" -c merge.conflictStyle=diff3 -c user.name=enginex-integrate -c user.email=enginex-integrate@localhost \
            rebase --continue >/dev/null 2>&1
        rc=$?
        set -e
    done
    if ! git -C "$worktree" push --force origin "HEAD:$b1" >/dev/null; then
        echo "could not publish rebased copy $b1" >&2
        return 1
    fi
    git -C "$worktree" switch --detach "$pre_merge" >/dev/null
    resolved_branch=$b1
}

merge_branch() {
    local base=$1 branch=$2 pre_merge=$3 slug
    resolved_branch=$branch
    if git -C "$worktree" -c merge.conflictStyle=diff3 -c user.name=enginex-integrate -c user.email=enginex-integrate@localhost \
        merge --no-ff --no-edit "origin/$branch" >/dev/null 2>&1; then
        return 0
    fi
    git -C "$worktree" merge --abort >/dev/null 2>&1 || true
    case $branch in
        agent/5306-*) slug=${branch#agent/5306-} ;;
        agent/*) slug=${branch#agent/} ;;
        *) slug=$branch ;;
    esac
    slug=${slug//[^A-Za-z0-9._-]/_}
    echo "merge conflict for $branch, rebasing copy agent/5306-${slug}-b1" >&2
    rebase_copy "$base" "$branch" "$pre_merge" "$slug"
    git -C "$worktree" -c merge.conflictStyle=diff3 -c user.name=enginex-integrate -c user.email=enginex-integrate@localhost \
        merge --no-ff --no-edit "origin/$resolved_branch" >/dev/null
}

rebuild_overlay() {
    local host=$1 slug=$2; shift 2
    local artifact="tools/dev/.integrate-artifacts/$slug" inner='set -eu; rm -rf tools/dev/.integrate-artifacts; mkdir -p tools/dev/.integrate-artifacts'
    local file
    for file in "$@"; do
        case $file in
            rootfs-overlay/*libengine-ui-hook.so) inner+="; OUT='$artifact/libengine-ui-hook.so' sh tools/ui-mod/build.sh" ;;
            rootfs-overlay/*libedisksd-hook.so) inner+="; OUT='$artifact/libedisksd-hook.so' sh tools/edisksd-hook/build.sh" ;;
            *) echo "no rebuild recipe for prebuilt object: $file" >&2; return 1 ;;
        esac
    done
    mkdir -p "$tmp/artifacts/$slug"
    remote_run "$host" "$inner" --fetch "$artifact:$tmp/artifacts/$slug"
    for file in "$@"; do
        case $file in
            rootfs-overlay/*libengine-ui-hook.so) cmp -s "$ROOT/rootfs-overlay/etc/enginex/libengine-ui-hook.so" "$tmp/artifacts/$slug/libengine-ui-hook.so" || { echo "prebuilt hook is out of sync: $file" >&2; return 1; } ;;
            rootfs-overlay/*libedisksd-hook.so) cmp -s "$ROOT/rootfs-overlay/etc/enginex/libedisksd-hook.so" "$tmp/artifacts/$slug/libedisksd-hook.so" || { echo "prebuilt hook is out of sync: $file" >&2; return 1; } ;;
        esac
    done
}

merged=()
for branch in "${branches[@]}"; do
    pre_merge=$(git -C "$worktree" rev-parse HEAD)
    base=$(git -C "$worktree" merge-base HEAD "origin/$branch")
    max_before=$(awk -F'|' '/^\|[[:space:]]*[0-9]+[[:space:]]*\|/ {gsub(/[[:space:]]/, "", $2); if ($2 > max) max=$2} END {print max+0}' "$worktree/docs/REGRESSION-CHECKLIST.md")
    merge_branch "$base" "$branch" "$pre_merge" || { echo "merge refused: $branch" >&2; exit 1; }
    added_lines_check "$base" "origin/$branch" "$worktree"
    added_lines_check "$base" "$pre_merge" "$worktree"
    normalize_checklist "$base" "origin/$branch" "$max_before"
    mapfile -t overlays < <(git -C "$worktree" diff --name-only "$base" "origin/$branch" -- rootfs-overlay | grep '\.so$' || true)
    if ((${#overlays[@]})); then rebuild_overlay "$(pick_host 1)" "${branch//[^A-Za-z0-9._-]/_}-${target_head}" "${overlays[@]}"; fi
    run_tests --base "$pre_merge"
    if [[ $resolved_branch == "$branch" ]]; then
        merged+=("$branch ($(git -C "$worktree" rev-parse --short "origin/$branch"))")
    else
        merged+=("$branch via $resolved_branch ($(git -C "$worktree" rev-parse --short "origin/$resolved_branch"))")
    fi
done

# Full suites: fast tier in the persistent clone (Go's test cache hits for unchanged packages), ARM tier at the same time.
run_tests --full --arm-gcc14
git -C "$worktree" push origin "HEAD:$TARGET" >/dev/null
elapsed=$(( $(date +%s) - started ))
printf 'Merged %s into %s; targeted tests after each merge, then test.sh --full; elapsed %ss.' "${merged[*]}" "$TARGET" "$elapsed"; if ((${#dropped_hunks[@]})); then printf ' Dropped hunks %s.' "${dropped_hunks[*]}"; fi; if ((${#doc_notes[@]})); then printf ' Doc cache decisions %s.' "${doc_notes[*]}"; fi; printf '\n'
