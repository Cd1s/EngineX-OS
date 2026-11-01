# SC5000 / JP07 的 QEngine 模拟封装

`run_qengine.sh` 包装真实上游脚本，不替换 QEMU 或伪造固件启动结果。固定目标为 **Engine OS / ARMv7 (`armhf`) / JP07**，默认只检查条件。所有输入镜像、上游仓库、实例和运行产物都必须在 `/root/build/sc5000-lab`，路径不支持空白、逗号及 shell 特殊字符。

当前核对的上游：`/root/build/sc5000-lab/QEngine`，commit `cf2617e2ae441466f58ee279efe62bae71de61b5`。本封装尚未在此机器上完成真实固件启动；不能据此声称 SC5000 UI、音频、控制面板或 Rekordbox 浏览功能已通过。

## 常用命令

在封装工作树根目录运行。示例都截断终端输出；`pipefail` 保留脚本失败状态，完整运行证据另存 lab。

```bash
set -o pipefail

# 默认动作也是 --check；--prereq 是同义词。只读检查，缺失返回 3。
bash sim/qengine/run_qengine.sh --check \
  --repo /root/build/sc5000-lab/QEngine \
  --firmware /root/build/sc5000-lab/fw/SC5000-5.0.4-Update.img \
  | cut -c1-200 | head -40

# 可先只打印真实上游调用；不要求运行依赖已安装。
bash sim/qengine/run_qengine.sh --create --dry-run \
  --repo /root/build/sc5000-lab/QEngine \
  --firmware /root/build/sc5000-lab/fw/SC5000-5.0.4-Update.img \
  --name jp07-5.0.4 \
  | cut -c1-200 | head -40

# 条件满足后，显式创建；拒绝覆盖已有实例，不提供 --force。
bash sim/qengine/run_qengine.sh --create \
  --firmware /root/build/sc5000-lab/fw/SC5000-5.0.4-Update.img \
  --name jp07-5.0.4 \
  | cut -c1-200 | head -40

# 使用事先准备好的 raw 虚拟 Rekordbox U 盘文件。
# 此路径是示例，封装不会生成、格式化或导出 Rekordbox 音乐库。
bash sim/qengine/run_qengine.sh --smoke --headless --timeout 180 \
  --name jp07-5.0.4 \
  --usb /root/build/sc5000-lab/media/rekordbox.raw \
  | cut -c1-200 | head -40

# 交互观察：VNC 仅监听 127.0.0.1；端口来自 instance.env 的 VNC_DISPLAY。
bash sim/qengine/run_qengine.sh --run --vnc \
  --name jp07-5.0.4 \
  --usb /root/build/sc5000-lab/media/rekordbox.raw \
  | cut -c1-200 | head -40
```

默认 `--headless` 等价于 `--display none`；VNC 用软件渲染。`--display sdl` 可在用户已启动的虚拟 X display 下使用，例如设置 `DISPLAY=:99` 后运行；封装不安装、启动或管理 Xvfb 服务。所有模式向上游传 `--no-gl`，固定 `ACCEL=tcg CPU=cortex-a15`，便于在 IN 的异构 CPU 上执行。`--run` 一直运行到退出或 Ctrl-C；只有 `--smoke` 有 15–900 秒的运行上限。

创建与启动是独立动作：`--run` 和 `--smoke` 不会隐式构建。`--firmware` 在创建时必填；运行时可省略，若填写则必须与实例记录匹配。`--check` 检查创建依赖和现有实例运行条件；没有实例或没有指定 USB 时也会返回非零。需要只查看计划时使用 `--dry-run`，其成功仅表示计划生成成功。

## 上游真实接口与 JP07

| 入口 | 本封装的实际用法 |
| --- | --- |
| `scripts/build_scripts/new_instance.sh` | `--name NAME --device engine --product-code JP07 --firmware 官方镜像` |
| 创建脚本向 builder 传递身份 | 导出 `PRODUCT_CODE=JP07`，并写入 `build/instances/NAME/instance.env` |
| `build_armv7_engine_rootfs.sh` | 当前版本默认 `${PRODUCT_CODE:-JP07}`；封装仍显式指定，避免依赖默认值 |
| `scripts/qemu/run_instance.sh` | `--name NAME --display none/vnc/sdl --no-gl`；读取 `instance.env` 后 exec `run_qemu.sh` |
| `run_qemu.sh` 扩展接口 | `QEMU_BIN` 指向 lab 内临时代理；`SERIAL=file:.../serial.log`；清空继承的 `QEMU_EXTRA_ARGS` |

`--device engine` 并不等于 JP07，也不决定 CPU 架构。上游从固件 rootfs 判断架构；SC5000 应走 `engine|armhf|build_armv7_engine_rootfs.sh|mpc|emmc.img` 这一行。此处的 `mpc` 是上游 ARMv7 数据盘布局名称，不表示把设备身份设置为 MPC。

封装在创建后和每次启动前检查 `DEVICE=engine`、`ARCH=armhf`、`PRODUCT_CODE=JP07`、实例名和各镜像路径，并以只读 `debugfs` 读取 rootfs 的 `/root/fake-dt/inmusic,product-code`，必须确实为 `JP07`。它严格解析 `instance.env`，拒绝额外 shell 语法，然后才允许上游 source。上游仓库及实例应来自可信来源；封装不校验官方签名，也不证明传入固件的来源。

## 磁盘与文件位置

- 只接受 lab 内可读、非空的普通镜像文件；物理盘、字符设备、指向 lab 外的链接会被拒绝。USB 还必须经 `qemu-img info` 检测为 `raw`，不得复用实例磁盘或固件文件。
- 启动代理追加 `-snapshot`，rootfs 和 data 的改动写到临时快照；USB 使用 `-drive if=none,file=...,format=raw,readonly=on,id=rekordbox`，通过 `qemu-xhci` 和 `usb-storage` 呈现。没有 `usb-host`、物理盘直通或宿主挂载操作。
- 快照运行不持久保存库导入、播放记录或 guest 设置。只读 USB 可能使依赖写入的 Rekordbox 工作流失败，smoke 不测试该工作流。
- 上游实例仍在 `QEngine/build/instances/NAME`，共享 kernel/initrd 在 `QEngine/build`；所有临时代理、日志和 QEMU 临时文件在 `lab/qengine-work/run.XXXXXX`。
- 上游多处硬编码 `mktemp /tmp/...`，仅设置 `TMPDIR` 不足。封装在独立 PATH 中放一个 mktemp 代理，把这些模板改到本次 lab 临时目录；保留上游源文件和命令接口。上游容器内的 `/tmp` 位于 Docker 数据目录。
- 创建前要求 Docker daemon 的 `DockerRootDir` 解析后也在 lab。当前机器返回 `/var/lib/docker`，因此创建会拒绝执行；封装不会迁移 Docker 或修改服务配置。已有实例运行不需要 Docker。

## 依赖与构建边界

运行需要 Bash 4.3+、coreutils (`realpath`/`timeout`/`mktemp`)、Python 3、`flock`、e2fsprogs (`dumpe2fs`/`debugfs`)、`qemu-img`，以及 `qemu-system-aarch64` 或 `qemu-system-arm`。优先选择上游 build 中的 QEMU，其次系统 aarch64、arm。QEMU 需要 ARM virt、virtio、qemu-xhci、usb-storage；实际不支持会在启动日志中失败。SDL 还要求有效 `DISPLAY`，VNC 不要求宿主桌面。

创建额外需要 binwalk 3.x、file、可访问的 Docker daemon、ARMv7 容器执行能力；非 ARMv7 宿主预检 `/proc/sys/fs/binfmt_misc` 下启用且带 `F` 标志的 ARM 注册。此检查不代替实际 ARM 容器执行测试。封装不注册 binfmt、不拉取探测容器、不安装系统依赖。必须先由操作人员准备合适的隔离构建环境；缺失时如实返回错误。

显式 `--create` 会执行真实上游 builder，它会拉取 Debian 容器、编译 shim、生成磁盘和 kernel/initrd，并在**容器内部**执行 apt，部分容器带 `--privileged`。封装不执行宿主系统级 apt，不触及宿主 bootloader、MCU 或系统服务。依赖、容器存储或磁盘空间未准备好时先停留在检查和说明阶段；本次交付未执行构建。后续若升级上游，需重新核对临时文件写入和启动接口。

## 自动 smoke 的判据与证据

`--smoke` 必须同时取得下列证据才返回 0：

1. QMP `query-status` 返回 `running: true`。
2. QMP `query-block` 中 `rekordbox` 的 `inserted.ro` 为 true，`info qtree` 中存在绑定该 drive 的 `usb-storage`。
3. 本次串口日志包含 `Linux version ...`，并包含 `systemd[1]:`、Debian/Engine welcome 或 login 提示等 guest 用户态证据。

仅进程存活、QMP socket 建立、端口监听或打印命令都不算通过。总运行期限为 `--timeout`，结束后向整个启动进程组发送 TERM，最多等 3 秒，必要时 KILL 再等 2 秒；依赖/镜像预检在启动计时前执行，外部探测各自受 5–10 秒限制。超时或提前退出返回 4，不会重试或隐式安装依赖。依赖缺失返回 3，未启动时不会生成假的 smoke 成功证据。

运行目录保留：`launch.log`（完整上游/QEMU 输出）、`serial.log`、`qemu.argv`（最终参数）、`qmp.jsonl`（QMP 原始响应与设备树）、`status.json`（判据及失败原因）。创建保留 `create.log`。QMP socket 仅位于权限为 0700 的本次临时目录；VNC 代理把上游 `:N` 改成 `127.0.0.1:N`。上游默认 SSH 转发仍按自身 `hostfwd=tcp::PORT-:22` 配置。

smoke 的 PASS 只说明 guest 已到用户态且只读虚拟 USB 已连接。它不证明 Engine 服务健康、UI 渲染、Rekordbox 识别、音频输出或物理控制器仿真。上游当前 ARMv7 Engine 路线也明确说明音频及控制面板尚不完整。

## 本次验证

验证 Bash 语法、ShellCheck、真实官方固件参数的创建 dry-run，以及无 QEMU/binwalk 条件下的非零依赖报告。另使用 lab 内明确标记的空白 raw 测试文件验证运行 dry-run 和参数拒绝；该文件没有音乐库，不能据此宣称启动或 Rekordbox 测试通过。未执行上游创建、系统安装或真实 QEMU 启动。
