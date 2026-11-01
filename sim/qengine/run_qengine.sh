#!/usr/bin/env bash
# SC5000 / JP07 wrapper for the real QEngine instance scripts. Linux only.
set -euo pipefail
export LC_ALL=C
LAB=/root/build/sc5000-lab
REPO=$LAB/QEngine
NAME=jp07-5.0.4
FIRMWARE='' USB='' MODE=check DISPLAY_MODE=none DRY=0
SECONDS_LIMIT=180

die() { printf 'ERROR: %s\n' "$*" >&2; exit 2; }
usage() {
    cat <<'EOF'
用法: run_qengine.sh [--check|--prereq|--create|--run|--smoke] [选项]
  --repo PATH        lab 内的真实 QEngine 仓库
  --firmware PATH    lab 内的官方 SC5000 固件 .img（创建时必填）
  --name NAME        实例名，默认 jp07-5.0.4；固定为 Engine / armhf / JP07
  --usb PATH         lab 内的普通 raw 虚拟 Rekordbox U 盘文件（运行时必填）
  --headless         无显示窗口（默认，等同 --display none）
  --vnc              软件渲染 VNC，仅绑定 127.0.0.1
  --display MODE     none | vnc | sdl（sdl 可配合用户自行启动的 Xvfb）
  --timeout SECONDS  smoke 总运行上限，15..900，默认 180
  --dry-run          只打印计划；不创建、不构建、不启动，不代表运行通过
  --check/--prereq   只检查创建和运行条件；缺失返回非零（默认动作）
  --create          调用上游 new_instance.sh；拒绝覆盖已有实例
  --run             调用上游 run_instance.sh；所有磁盘使用 -snapshot
  --smoke           有界启动检查；保留 QMP、串口、状态和完整启动日志
EOF
}
set_mode() {
    [[ $MODE == check || $MODE == "$1" ]] || die '动作不能组合'
    MODE=$1
}
while (($#)); do
    case "$1" in
        --repo|--firmware|--name|--usb|--display|--timeout)
            if (($# < 2)) || [[ -z $2 || $2 == --* ]]; then die "$1 缺少值"; fi
            case "$1" in
                --repo) REPO=$2;; --firmware) FIRMWARE=$2;; --name) NAME=$2;;
                --usb) USB=$2;; --display) DISPLAY_MODE=$2;; --timeout) SECONDS_LIMIT=$2;;
            esac
            shift 2;;
        --create) set_mode create; shift;;
        --run) set_mode run; shift;;
        --smoke) set_mode smoke; shift;;
        --check|--prereq) set_mode check; shift;;
        --dry-run) DRY=1; shift;;
        --headless) DISPLAY_MODE=none; shift;;
        --vnc) DISPLAY_MODE=vnc; shift;;
        --help|-h) usage; exit 0;;
        *) die "未知参数: $1";;
    esac
done
[[ $(uname -s) == Linux ]] || die '此封装仅支持 Linux'
[[ $NAME =~ ^[A-Za-z0-9][A-Za-z0-9_.-]{0,63}$ ]] || die '实例名格式无效'
[[ $SECONDS_LIMIT =~ ^[0-9]{2,3}$ ]] || die 'timeout 必须是 15..900 的十进制整数'
SECONDS_LIMIT=$((10#$SECONDS_LIMIT))
((SECONDS_LIMIT >= 15 && SECONDS_LIMIT <= 900)) || die 'timeout 必须为 15..900'
case "$DISPLAY_MODE" in none|vnc|sdl) ;; *) die 'display 仅支持 none/vnc/sdl';; esac
command -v realpath >/dev/null || die '缺少 realpath (coreutils)'

# Upstream writes unquoted shell assignments; QEMU_EXTRA_ARGS also word-splits.
# Constrain paths instead of pretending to support unsafe quoting there.
lab_path() {
    local p
    p=$(realpath -m -- "$1") || die "路径解析失败: $1"
    [[ $p == "$LAB/"* && $p =~ ^[A-Za-z0-9_./+-]+$ ]] ||
        die "路径必须位于 $LAB 内，且不含空白、逗号或 shell 字符: $1"
    printf '%s\n' "$p"
}
regular_image() {
    [[ -f $1 && -s $1 && ! -b $1 && ! -c $1 && -r $1 ]] ||
        die "必须是非空、可读的普通镜像文件，绝不接受物理盘: $1"
}
REPO=$(lab_path "$REPO")
[[ -d $REPO ]] || die "QEngine 仓库不存在: $REPO"
BUILD=$(lab_path "$REPO/build")
INSTANCE=$(lab_path "$BUILD/instances/$NAME")
ENV_FILE=$INSTANCE/instance.env
NEW=$REPO/scripts/build_scripts/new_instance.sh
RUN=$REPO/scripts/qemu/run_instance.sh
for script in "$NEW" "$RUN" "$REPO/scripts/qemu/run_qemu.sh" \
              "$REPO/scripts/qemu/arch_devices.sh" "$REPO/scripts/qemu/display_modes.sh"; do
    [[ -f $script && -r $script ]] || die "上游脚本不存在或不可读: $script"
done
for script in "$NEW" "$RUN" "$REPO/scripts/qemu/run_qemu.sh"; do
    [[ -x $script ]] || die "上游启动脚本不可执行: $script"
done
[[ -z $FIRMWARE ]] || { FIRMWARE=$(lab_path "$FIRMWARE"); regular_image "$FIRMWARE"; }
[[ -z $USB ]] || { USB=$(lab_path "$USB"); regular_image "$USB"; }
[[ $MODE != create || -n $FIRMWARE ]] || die '--create 必须指定 --firmware'
[[ $MODE != run && $MODE != smoke || -n $USB ]] || die '运行必须指定 --usb 普通 raw 镜像'
[[ $MODE != create || ! -e $INSTANCE ]] || die '实例目录已存在；请使用新名字，封装不覆盖或强制重建'

CREATE_CMD=("$NEW" --name "$NAME" --device engine --product-code JP07 --firmware "${FIRMWARE:-<官方固件.img>}")
RUN_CMD=("$RUN" --name "$NAME" --display "$DISPLAY_MODE" --no-gl)
printf 'MODE=%s PRODUCT_CODE=JP07 ARCH=armhf DISPLAY=%s\n' "$MODE" "$DISPLAY_MODE"
printf '创建: '; printf '%q ' "${CREATE_CMD[@]}"; printf '\n'
printf '启动: '; printf '%q ' "${RUN_CMD[@]}"; printf '\n'
printf '磁盘: -snapshot; USB: %s (format=raw,readonly=on); VNC: loopback only\n' "${USB:-<虚拟Rekordbox.img>}"
if ((DRY)); then
    printf 'DRY-RUN: 未检查运行依赖或启动；上游临时文件与运行证据将放在 %s/qengine-work。\n' "$LAB"
    printf '创建需要 binwalk 3、Docker ARMv7/binfmt、lab 内 DockerRootDir；不会执行宿主 apt。\n'
    exit 0
fi

MISSING=0
missing() { printf 'MISSING: %s\n' "$*" >&2; MISSING=1; }
need() { command -v "$1" >/dev/null 2>&1 || missing "命令 $1"; }
for tool in python3 timeout dumpe2fs debugfs flock mktemp qemu-img; do need "$tool"; done
if [[ -n $USB ]] && command -v qemu-img >/dev/null && command -v timeout >/dev/null && command -v python3 >/dev/null; then
    USB_FORMAT=$(timeout 10 qemu-img info --output=json "$USB" 2>/dev/null | python3 -c \
        'import json,sys; print(json.load(sys.stdin)["format"])' 2>/dev/null) || USB_FORMAT=
    [[ $USB_FORMAT == raw ]] || missing 'USB 镜像必须为 raw 格式（不能把 qcow2 当 raw）'
fi
QEMU_REAL=
for candidate in "$BUILD/qemu-virgl/prefix/bin/qemu-system-aarch64" qemu-system-aarch64 qemu-system-arm; do
    if command -v "$candidate" >/dev/null 2>&1; then QEMU_REAL=$(command -v "$candidate"); break; fi
done
if [[ $MODE != create ]]; then
    [[ -n $QEMU_REAL ]] || missing 'qemu-system-aarch64 或 qemu-system-arm（没有启动虚拟机）'
    [[ -n $USB ]] || missing '--usb 尚未指定'
    [[ $DISPLAY_MODE != sdl || -n ${DISPLAY:-} ]] || missing 'SDL 需要 DISPLAY；自行准备 Xvfb 或使用 --headless/--vnc'
fi
if [[ $MODE == create || $MODE == check ]]; then
    for tool in binwalk qemu-img docker file; do need "$tool"; done
    [[ -n $FIRMWARE ]] || missing '--firmware 尚未指定'
    if command -v binwalk >/dev/null && command -v timeout >/dev/null; then
        BW_VERSION=$(timeout 5 binwalk --version 2>&1) || BW_VERSION=
        [[ $BW_VERSION =~ [[:space:]]3\. ]] || missing 'binwalk 3.x（无法确认版本）'
    fi
    if command -v docker >/dev/null && command -v timeout >/dev/null; then
        DOCKER_ROOT=$(timeout 10 docker info --format '{{.DockerRootDir}}' 2>/dev/null) || DOCKER_ROOT=
        if [[ -z $DOCKER_ROOT ]]; then
            missing 'Docker daemon 不可达'
        elif [[ $(realpath -m -- "$DOCKER_ROOT") != "$LAB/"* ]]; then
            missing "DockerRootDir=$DOCKER_ROOT 不在 lab；拒绝把容器大文件写到 lab 外，不修改 Docker 配置"
        fi
    fi
    case "$(uname -m)" in
        armv7l|armv8l) ;;
        *)
            BINFMT_OK=0
            for entry in /proc/sys/fs/binfmt_misc/*; do
                [[ -f $entry && ${entry##*/} == *arm* ]] || continue
                if grep -q '^enabled$' "$entry" && grep -q '^flags:.*F' "$entry"; then BINFMT_OK=1; fi
            done
            ((BINFMT_OK)) || missing '宿主没有启用带 F 标志的 ARM binfmt；不运行 privileged 注册/安装'
            ;;
    esac
fi

declare -A CFG=()
validate_instance() {
    local line key value resolved identity
    [[ -f $ENV_FILE && ! -L $ENV_FILE ]] || { missing "实例配置不存在或是符号链接: $ENV_FILE"; return; }
    # Never source unvalidated shell: the upstream launcher will source this file.
    while IFS= read -r line || [[ -n $line ]]; do
        [[ -z $line || $line == \#* ]] && continue
        if [[ ! $line =~ ^([A-Z_]+)=([A-Za-z0-9_./:+-]+)$ ]]; then
            missing 'instance.env 含不支持的 shell 语法'; return
        fi
        key=${BASH_REMATCH[1]}; value=${BASH_REMATCH[2]}
        case "$key" in
            INSTANCE_NAME|DEVICE|FIRMWARE_IMG|DISPLAY_MODE|ROOTFS_IMG|DATA_IMG|ROOT_UUID|ARCH|PRODUCT_CODE|KERNEL_IMG|INITRD_IMG|SSH_PORT|VNC_DISPLAY) ;;
            *) missing "instance.env 未知键: $key"; return;;
        esac
        [[ ! -v CFG[$key] ]] || { missing "instance.env 重复键: $key"; return; }
        CFG[$key]=$value
    done < "$ENV_FILE"
    [[ ${CFG[INSTANCE_NAME]:-} == "$NAME" && ${CFG[DEVICE]:-} == engine && \
       ${CFG[ARCH]:-} == armhf && ${CFG[PRODUCT_CODE]:-} == JP07 ]] ||
        missing '实例必须为指定名字、DEVICE=engine、ARCH=armhf、PRODUCT_CODE=JP07'
    for key in ROOTFS_IMG DATA_IMG KERNEL_IMG INITRD_IMG FIRMWARE_IMG; do
        value=${CFG[$key]:-}
        [[ -n $value ]] || { missing "instance.env 缺少 $key"; continue; }
        resolved=$(lab_path "$value")
        regular_image "$resolved"
        [[ $value == "$resolved" ]] || missing "$key 必须是 lab 内规范绝对路径（避免符号链接或相对路径）"
        [[ -z $USB || $resolved != "$USB" ]] || missing 'USB 不能复用实例磁盘、内核或固件'
    done
    [[ -z $FIRMWARE || ${CFG[FIRMWARE_IMG]:-} == "$FIRMWARE" ]] || missing '固件参数与已有实例记录不匹配'
    [[ ${CFG[SSH_PORT]:-} =~ ^[0-9]{4,5}$ ]] || missing 'SSH_PORT 格式无效'
    [[ ${CFG[VNC_DISPLAY]:-} =~ ^[0-9]{1,2}$ ]] || missing 'VNC_DISPLAY 格式无效'
    if command -v debugfs >/dev/null && command -v timeout >/dev/null && [[ -n ${CFG[ROOTFS_IMG]:-} ]]; then
        identity=$(timeout 10 debugfs -R 'cat /root/fake-dt/inmusic,product-code' "${CFG[ROOTFS_IMG]}" 2>/dev/null) || identity=
        [[ $identity == JP07 ]] || missing 'rootfs 内 /root/fake-dt/inmusic,product-code 不是 JP07'
    fi
}
if [[ $MODE != create ]]; then validate_instance; fi
((MISSING == 0)) || exit 3
if [[ $MODE == check ]]; then
    printf 'CHECK PASS: 静态与依赖检查通过；未启动虚拟机，未测试 ARM Docker 容器执行。\n'
    exit 0
fi

# All new wrapper files, QEMU snapshots, logs and upstream mktemp calls stay in lab.
WORK_ROOT=$(lab_path "$LAB/qengine-work")
mkdir -p "$WORK_ROOT"
WORK=$(mktemp -d "$WORK_ROOT/run.XXXXXX")
mkdir -p "$WORK/tmp" "$WORK/bin"
export TMPDIR=$WORK/tmp
export QENGINE_TMP=$TMPDIR QENGINE_MKTEMP
QENGINE_MKTEMP=$(command -v mktemp)
cat > "$WORK/bin/mktemp" <<'SH'
#!/usr/bin/env bash
set -euo pipefail
args=()
for arg in "$@"; do
    case "$arg" in /tmp/*) arg="$QENGINE_TMP/${arg#/tmp/}";; esac
    args+=("$arg")
done
exec "$QENGINE_MKTEMP" "${args[@]}"
SH
chmod 700 "$WORK/bin/mktemp"
export PATH=$WORK/bin:$PATH
printf '证据/临时目录: %s\n' "$WORK"
if [[ $MODE == create ]]; then
    # New directory only: do not overwrite/rebuild existing instance configuration.
    "${CREATE_CMD[@]}" > "$WORK/create.log" 2>&1 || {
        printf 'CREATE FAILED: 见 %s/create.log（不自动重试或覆盖实例）\n' "$WORK" >&2; exit 4;
    }
    validate_instance
    ((MISSING == 0)) || exit 3
    printf 'CREATE PASS: 实例及 rootfs JP07 身份已核对；未启动。日志: %s/create.log\n' "$WORK"
    exit 0
fi

export QENGINE_QEMU_REAL=$QEMU_REAL QENGINE_USB=$USB QENGINE_WORK=$WORK
cat > "$WORK/bin/qengine-qemu" <<'SH'
#!/usr/bin/env bash
set -euo pipefail
args=(); boot=0
while (($#)); do
    case "$1" in
        -machine) boot=1; args+=("$1" "$2"); shift 2;;
        -vnc) args+=("$1" "127.0.0.1${2}"); shift 2;;
        *) args+=("$1"); shift;;
    esac
done
if ((boot)); then
    args+=(-snapshot
        -drive "if=none,file=$QENGINE_USB,format=raw,readonly=on,id=rekordbox"
        -device qemu-xhci,id=rekordbox-xhci
        -device usb-storage,bus=rekordbox-xhci.0,drive=rekordbox
        -monitor none -qmp "unix:$QENGINE_WORK/qmp.sock,server=on,wait=off")
    printf '%q ' "$QENGINE_QEMU_REAL" "${args[@]}" > "$QENGINE_WORK/qemu.argv"
    printf '\n' >> "$QENGINE_WORK/qemu.argv"
fi
exec "$QENGINE_QEMU_REAL" "${args[@]}"
SH
chmod 700 "$WORK/bin/qengine-qemu"
# Override inherited debug hooks: every launched drive is defined above/upstream.
export QEMU_BIN=$WORK/bin/qengine-qemu QEMU_EXTRA_ARGS='' SERIAL=file:$WORK/serial.log
export BUILD_DIR=$BUILD ACCEL=tcg CPU=cortex-a15
unset VIRGL ROOTFS_IMG DATA_IMG KERNEL_IMG INITRD_IMG ARCH DEVICE DISPLAY_MODE
# RUN_CMD already contains the selected display. Parent values cannot redirect disks.
if [[ $MODE == run ]]; then
    printf '启动日志写入 %s/launch.log；串口写入 serial.log；Ctrl-C 结束。\n' "$WORK"
    exec "${RUN_CMD[@]}" > "$WORK/launch.log" 2>&1
fi

# A process staying alive alone is not a boot test. Require QMP + read-only USB +
# guest kernel AND userspace evidence, then terminate the whole launch group.
exec python3 - "$WORK" "$SECONDS_LIMIT" "${RUN_CMD[@]}" <<'PY'
import json
import os
from pathlib import Path
import re
import signal
import socket
import subprocess
import sys
import time

work = Path(sys.argv[1])
limit = int(sys.argv[2])
deadline = time.monotonic() + limit
result = {"result": "FAIL", "scope": "guest-userspace-and-readonly-usb", "timeout_seconds": limit}
proc = None
sock = None
stream = None

def receive():
    while time.monotonic() < deadline:
        line = stream.readline()
        if not line:
            raise RuntimeError("QMP closed")
        msg = json.loads(line)
        with (work / "qmp.jsonl").open("a") as out:
            out.write(json.dumps(msg) + "\n")
        if "event" not in msg:
            return msg
    raise TimeoutError("smoke deadline")

def command(name, arguments=None):
    request = {"execute": name}
    if arguments is not None:
        request["arguments"] = arguments
    sock.sendall((json.dumps(request) + "\r\n").encode())
    response = receive()
    if "error" in response:
        raise RuntimeError(str(response["error"]))
    return response["return"]

try:
    signal.signal(signal.SIGTERM, lambda *_: (_ for _ in ()).throw(KeyboardInterrupt()))
    with (work / "launch.log").open("wb") as log:
        proc = subprocess.Popen(sys.argv[3:], stdout=log, stderr=subprocess.STDOUT, start_new_session=True)
    result["launcher_pid"] = proc.pid
    while time.monotonic() < deadline:
        if proc.poll() is not None:
            raise RuntimeError(f"launcher exited early: {proc.returncode}")
        if sock is None and (work / "qmp.sock").exists():
            candidate = socket.socket(socket.AF_UNIX)
            candidate.settimeout(min(1.0, max(0.01, deadline - time.monotonic())))
            try:
                candidate.connect(str(work / "qmp.sock"))
            except OSError:
                candidate.close()
            else:
                sock = candidate
                stream = sock.makefile("rb")
                receive()  # greeting
                command("qmp_capabilities")
        if sock is not None:
            sock.settimeout(min(1.0, max(0.01, deadline - time.monotonic())))
            status = command("query-status")
            blocks = command("query-block")
            readonly_usb = any(b.get("device") == "rekordbox" and
                               b.get("inserted", {}).get("ro") is True for b in blocks)
            tree = command("human-monitor-command", {"command-line": "info qtree"})
            usb_device = 'drive = "rekordbox"' in tree and 'usb-storage' in tree
            serial = work / "serial.log"
            text = serial.read_text(errors="replace") if serial.exists() else ""
            kernel = bool(re.search(r"Linux version \d", text))
            userspace = bool(re.search(r"systemd\[1\]:|Welcome to (?:Debian|Engine)|"
                                       r"login:", text))
            result.update(qmp_status=status, readonly_usb=readonly_usb,
                          usb_device=usb_device, kernel_console=kernel, userspace_console=userspace)
            if status.get("running") is True and readonly_usb and usb_device and kernel and userspace:
                result["result"] = "PASS"
                break
        time.sleep(min(0.5, max(0.0, deadline - time.monotonic())))
    else:
        result["reason"] = "deadline reached before all evidence was present"
except (Exception, KeyboardInterrupt) as exc:
    result["reason"] = f"{type(exc).__name__}: {exc}"
finally:
    if stream is not None:
        stream.close()
    if sock is not None:
        sock.close()
    if proc is not None:
        try:
            os.killpg(proc.pid, signal.SIGTERM)
            proc.wait(timeout=3)
        except subprocess.TimeoutExpired:
            os.killpg(proc.pid, signal.SIGKILL)
            proc.wait(timeout=2)
        except ProcessLookupError:
            pass
        result["exit_after_cleanup"] = proc.returncode
    (work / "status.json").write_text(json.dumps(result, ensure_ascii=False, indent=2) + "\n")
print(f"SMOKE {result['result']}: {work / 'status.json'}")
print("只验证 guest 用户态启动与 QEMU 只读 USB；不代表 Engine UI、音频或 Rekordbox 已通过。")
sys.exit(0 if result["result"] == "PASS" else 4)
PY
