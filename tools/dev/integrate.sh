#!/usr/bin/env bash
set -euo pipefail

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
remote_names=()
remote_hosts=()
remote_counter=0
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
    python3 - "$base" "$other" "$result" <<'PY'
import subprocess, sys
base, other, result = sys.argv[1:]
diff = subprocess.check_output(["git", "diff", "--no-renames", "--unified=0", base, other], text=True)
files = {}
current = None
for line in diff.splitlines():
    if line.startswith("+++ b/"):
        current = line[6:]
        files.setdefault(current, [])
    elif line.startswith("+") and not line.startswith("+++") and current:
        files[current].append(line[1:])
for path, lines in files.items():
    try:
        content = open(result + "/" + path, "r", encoding="utf-8", errors="surrogateescape").read().splitlines()
    except (FileNotFoundError, IsADirectoryError):
        raise SystemExit(f"lost added file: {path}")
    for line in lines:
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

resolve_checklist_merge() {
    local base=$1 branch=$2 rows=$tmp/checklist.rows files line
    git -C "$worktree" merge --no-ff --no-edit "origin/$branch" >/dev/null 2>&1 && return 0
    git -C "$worktree" merge --abort >/dev/null 2>&1 || true
    files=$(git -C "$worktree" diff --name-only "$base" "origin/$branch")
    [[ $files == docs/REGRESSION-CHECKLIST.md ]] || return 1
    python3 - "$base" "origin/$branch" "$rows" <<'PY'
import re, subprocess, sys
base, branch, output = sys.argv[1:]
diff = subprocess.check_output(["git", "diff", "--no-renames", "--unified=0", base, branch, "--", "docs/REGRESSION-CHECKLIST.md"], text=True)
added = [line[1:] for line in diff.splitlines() if line.startswith("+") and not line.startswith("+++")]
if not added or any(not re.match(r"\|\s*\d+\s*\|", line) for line in added):
    raise SystemExit(1)
open(output, "w", encoding="utf-8").write("\n".join(added) + "\n")
PY
    git -C "$worktree" merge --no-ff -s ours --no-edit "origin/$branch" >/dev/null
    while IFS= read -r line; do printf '%s\n' "$line" >> "$worktree/docs/REGRESSION-CHECKLIST.md"; done < "$rows"
    git -C "$worktree" add docs/REGRESSION-CHECKLIST.md
    git -C "$worktree" -c user.name=enginex-integrate -c user.email=enginex-integrate@localhost \
        commit -m "chore: preserve checklist rows during merge" >/dev/null
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
    resolve_checklist_merge "$base" "$branch" || { echo "merge refused: $branch" >&2; exit 1; }
    added_lines_check "$base" "origin/$branch" "$worktree"
    added_lines_check "$base" "$pre_merge" "$worktree"
    normalize_checklist "$base" "origin/$branch" "$max_before"
    mapfile -t overlays < <(git -C "$worktree" diff --name-only "$base" "origin/$branch" -- rootfs-overlay | grep '\.so$' || true)
    if ((${#overlays[@]})); then rebuild_overlay "$(pick_host 1)" "${branch//[^A-Za-z0-9._-]/_}-${target_head}" "${overlays[@]}"; fi
    run_tests --base "$pre_merge"
    merged+=("$branch ($(git -C "$worktree" rev-parse --short "origin/$branch"))")
done

# Full suites: fast tier in the persistent clone (Go's test cache hits for unchanged packages), ARM tier at the same time.
run_tests --full --arm-gcc14
git -C "$worktree" push origin "HEAD:$TARGET" >/dev/null
elapsed=$(( $(date +%s) - started ))
printf 'Merged %s into %s; targeted tests after each merge, then test.sh --full; elapsed %ss.\n' "${merged[*]}" "$TARGET" "$elapsed"
