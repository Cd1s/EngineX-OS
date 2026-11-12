# EngineX OS 发版规则（Release Checklist）

任何人或 AI agent 发布 EngineX OS（包括新版本，以及同一版本的修复重发）都按这份清单做，不跳步。`<VERSION>` 指 `5.0.4.x` 这样的版本号，`<OLD>` 指上一个版本号。

## 0. 先确定发哪种

- **同一版本修复重发**（维护者默认做法：版本号固定，例如一直是 5.0.4.1）：不改版本号，跳过第 1 步，第 5 步用 `--clobber` 覆盖同一个 Release，再把 tag 移到新提交。
- **新版本**：只有维护者明确要求才升版本号。按全部步骤做，新建 `v<VERSION>` tag 与 Release。
- **热修复版本**：在已发布版本后加第五段（`5.3.0.1` → `5.3.0.1.1`，tag `v5.3.0.1.1`，标题 `EngineX OS <VERSION> (Hotfix)`），按「新版本」全部步骤做。碟机的 `newerVersion` 逐段比较，前四段相同时段数多者更新，所以已刷 `5.3.0.1` 的机器能收到推送，也不占用 `5.3.0.2`。`az01_packer.py` 的 `VERSION_RE` 最多接受五段，门禁的 `BASE` 取前三段作为官方底包号。

## 1. 必改文件（三语 README、官网与使用文档、实测截图、版本号）

| 文件 | 要改的地方 |
| --- | --- |
| `rootfs-overlay/etc/sc5000/version.txt` | 整个文件内容就是 `<VERSION>` |
| `README.md`、`README.zh-CN.md`、`README.zh-TW.md` | 第 3 行 hero 图的 `alt="EngineX OS <VERSION>"`；简介里的 `**EngineX OS <VERSION>**`；性能对比表表头里的 `` `EngineX OS <VERSION>` ``；本版本新特性与使用教程章节；「如何安装与升级」里 4 个平台 `EngineX-Installer-<VERSION>`、8 款一代机 `*-Update.img` 与 8 款二代机主下载 `*-Update.img` 的文件名和下载链接（共 20 条主直链，三份 README 同步更新；`5.3.0.4` / `5.3.0.5` Release 资产中同时保留 8 个 `*-Data-Bundle.tar.gz` 供旧版二代机屏幕 OTA 平滑过渡，`5.3.0.6+` 起彻底退役，#215） |
| `docs/index.html`、`docs/site/{content.js,app.js,style.css}` | 官网 `https://enginexdj.com`（独立全屏首页 `Home` + 三栏使用文档 `Docs`）：更新三语 `version: "<VERSION>"`、新功能章节与刷机/使用教程、全部 20 条下载链接（4 个安装器 + 16 个 `*-Update.img`：8 款一代 + 8 款二代），并递增 `docs/index.html`（及 `docs/site/content.js` 如有）里的静态资源缓存参数 `?v=N` |
| `docs/site/assets/shots/*.png`、`docs/assets/screenshots/{zh-CN,zh-TW,en}/*.png` | 凡涉及界面、菜单、控制中心磁贴或版本号变更，必须在编译机上基于 `<VERSION>` 底包通过 `QT_QPA_PLATFORM=eglfs`（`/dev/dri/card0` `virtio-gpu` + Mesa OpenGL，避免 `offscreen` 下 `ColoredImage` 图标空白）重新渲染三语实测截图并同步替换官网与 README 截图（实机硬件专属截图如 `07-bluetooth-audio.png` 保持不变） |
| `docs/assets/hero-banner*.svg` | 三张 banner 现在不写版本号。大版本换视觉或 banner 里出现版本号时才改，三语一起改 |
| `tools/az01-packer/README.md` | 示例命令里的版本号（可选，保持示例与现行版本一致） |

下载链接格式（文件名带版本号，所以每次升版本都要改；全系 16 款一代与二代机型统一使用 `*-Update.img`）：

```
https://github.com/Cd1s/EngineX-OS/releases/latest/download/EngineX-Installer-<VERSION>-<platform>.(zip|tar.gz)
https://github.com/Cd1s/EngineX-OS/releases/latest/download/<PREFIX>-<VERSION>-EngineX-OS-<Model>-Update.img
```

改完检查没有漏掉的旧版本号（测试夹具与历史版本对比章节里的旧版本号不用改）：

```sh
git grep -n "<OLD>" -- README.md README.zh-CN.md README.zh-TW.md rootfs-overlay docs/index.html docs/site/content.js docs/assets
```

`tests/`、`test_az01_packer.py` 里的版本号是测试数据，不要改。镜像里的 `/etc/sc5000/version.txt` 由 `build-release.sh` 的 `--set-version` 写入，第 1 步改的仓库文件用于实机热部署和保持一致。

## 2. 测试（只在编译机编译机上跑，本机不编译）

本项目的编译、单测、门禁、打包与组件实验分析主机是 **the build host**（aarch64，支持 32 位 EL0，碟机的 armhf 程序原生运行）。2026-11-05 起不再用 IN。`remote-build` 默认仍连 IN，所以每次都写 `REMOTE_BUILD_HOST=<build host> remote-build ...`。实验与组件结构分析统一使用 **`craft-anything`**（`craft-anything` skill / `craft-anything-cli`）配合新加的 **`deepseek-v4.1-flash`**（`deepseek4.1 flash`）模型在编译机上进行。机器上的固定材料在 `/root/build/sc5000-lab/`：`fw/`（5.0.4 官方底包）、`fw-5.3.0/`、`fw-5.3.0-gen2/`、`5.0.4/qrc`、`5.3.0/qrc`、`5.0.4|5.3.0/tree/usr/Engine/Engine`、`systemone-analysis/`、`deps-sqlite/`（armhf `sqlite3.o`）、`sbin/fsck.hfsplus`、`release-<OLD>/`（上一版 3 个代表 `.img`，供门禁用）。Go 1.27.1 在 `/etc/profile.d/buildtools.sh`。

```sh
gofmt -l . && go vet ./... && go test -race ./...
bash tests/test_rootfs_overlay.sh                        # 必须用 bash
JC11S_ROOTFS="${JC11S_ROOTFS:?先设置为官方 PRIME4PLUS-5.3.0 的解包 rootfs}" \
  bash tools/az01-packer/test_sclive4_bootstrap.sh       # SC LIVE 4 + JC11S，两款均须 PASS
AZ04=1 bash tools/az01-packer/test_sclive4_bootstrap.sh  # 二代机 AZ04 (PRIME 4 G2 / SYSTEM ONE) 沙箱
python3 -m unittest discover -s tools/az01-packer
python3 -m unittest discover -s tools/stems
# C 单测：编译命令写在各文件头部
#   tools/ui-mod/test_dsp_smooth.c（另跑 ASan/UBSan 与 TSan 各一次）
#   tools/ui-mod/test_stems_feature.c（arm-linux-gnueabihf-gcc 编译，编译机上原生运行；参数给两个底包的 Engine）
#   tools/edisksd-hook/test_edisksd_hook.c（--mounts 只在 unshare -m 里跑）
#   tools/ui-mod/test_az04_hook.c（AZ04 64 位 hook：编译机原生 gcc 编译，参数给 AZ04 的 Engine）
```

JC11S fixture 是独立的官方 PRIME4PLUS rootfs，不能用 SC LIVE 4 或 PRIME 4 G2 代替。
准备与校验摘要见 `docs/issue169-prime4plus-investigation.md`。把镜像解包到本次任务
自建的目录后设置 `JC11S_ROOTFS`；不要写入只读的固定实验材料目录。普通 SC LIVE 4
单独检查不设置此变量时会明确 WARN 跳过 JC11S，**发版不得跳过**。若要同时回归真实
发布包的安装器回读，设置 `JC11S_RELEASE_BUNDLE` 为已校验 SHA-256 的 PRIME4PLUS
`*-Update.img`（或旧版 `Data-Bundle.tar.gz` 夹具）后运行 Go 测试；该 fixture 测试覆盖新目录与旧 `sc5000-custom` 目录。

涉及 Factory Reset、`az01-update --erase-data`、空白 `/data` 首次启动的改动，只在编译机的 `unshare -m` + chroot 沙箱里用解包出的 rootfs 测，**绝不在实机触发**。

## 3. 提交与构建（16 款机型 `*-Update.img` + 8 款二代兼容 `*-Data-Bundle.tar.gz` + 4 平台一键安装器，共 28 个产物）

> **二代机分阶段统一迁移规则（#215，维护者确认）**：
> - **`5.3.0.4`（当前版本）**：同时发布 8 款二代 `*-Update.img` 与 8 款二代 `*-Data-Bundle.tar.gz`（共 **28 个产物**：16 个 `*-Update.img` + 8 个 `*-Data-Bundle.tar.gz` + 4 个 `EngineX-Installer`，连同 `SHA256SUMS.txt` 共上传 29 个文件）。`5.3.0.4` 的 `pkg/webui/update.go` 同时识别两者（优先 `-Update.img`，回退 `-Data-Bundle.tar.gz`），`EngineX-Installer` 默认执行 `[1/2] 一键解锁` + `[2/2] 刷入 *-Update.img`（仍兼容 `.tar.gz`），而仍运行 `5.3.0.2` / 未解锁 `5.3.0.3` 的二代机也能直接通过 Release 中的 `*-Data-Bundle.tar.gz` 在屏幕上无感一键 OTA 升级并自动获得 `.img` 解锁。
> - **`5.3.0.5`（下一版过渡桥接版本）**：Release 资产最后一次同时保留 `*-Data-Bundle.tar.gz`（供 `5.3.0.2` / `5.3.0.3` / 早期 `5.3.0.4` 二代机一键 OTA 升到 `5.3.0.5`），但 **`5.3.0.5` 自身的 `pkg/webui/update.go` 将切换为仅检测 `*-Update.img`**。
> - **`5.3.0.6+`（最终完全统一状态）**：从 `build-release.sh` 与 GitHub Release 中彻底退役 `*-Data-Bundle.tar.gz`，全系 16 款机型在 U 盘、屏幕 OTA 与 `EngineX-Installer` 中统一仅使用 `*-Update.img`（届时恢复为 20 个产物）。

1. 改动先提交并推送到 `origin/main`，构建必须对应一个已推送的提交。
2. 用干净目录构建（`remote-build` 同步不会删除远端已删除的文件，旧目录会带上残留）：

```sh
REMOTE_BUILD_HOST=<build host> remote-build --name sc5000-release-<短说明> --clean -- \
  '. /etc/profile.d/buildtools.sh
   OUT=/var/tmp/sc5000-release-<VERSION>
   rm -rf "$OUT"
   sh tools/az01-packer/build-release.sh <VERSION> /root/build/sc5000-lab/fw-5.3.0 none /root/build/sc5000-lab/sbin/fsck.hfsplus "$OUT"
   sh tools/az01-packer/build-installer.sh <VERSION> "$OUT"'
```

`build-release.sh` 抽 qrc 时用 `qemu-arm -L <rootfs>` 跑 Engine 的静态初始化，编译机装了不带 binfmt 的 `qemu-user`（`--no-install-recommends`）。不要装 `qemu-user-binfmt`：注册了 `qemu-arm` 的 binfmt 会盖过原生执行，门禁会退回软模拟。

第 3 个参数在公开发布时**必须是 `none`**：公开镜像不能内置任何人的 SSH 公钥（#46）。只有维护者自用的私有镜像才传公钥文件，且私有镜像不能上传到 Release。

输出目录必须不存在、且不在任何 git 工作区里。`5.3.0.4` / `5.3.0.5` 产出 **8 个一代 `*-Update.img`、8 个二代 `*-Update.img`、8 个二代兼容 `*-Data-Bundle.tar.gz`、4 个平台 `EngineX-Installer-<VERSION>-*` 以及汇总全部 28 个产物的 `SHA256SUMS.txt`**（共 29 个上传文件）。

## 4. 校验镜像

- 三个镜像组各抽一个，`python3 tools/az01-packer/az01_packer.py unpack <img> --output <新目录>` 要输出 `"verified": true`。
- 在解包出的 `rootfs.ext2` 里用 `debugfs -R "cat <路径>"` 核对：`/etc/sc5000/version.txt` 是 `<VERSION>`，本次修改的文件确实在镜像里。
- 同一组的机型 sha256 应相同：

| 镜像组 | 机型（前缀，板号） |
| --- | --- |
| SC5000 | `SC5000` JP07、`SC5000M` JP08、`PRIMEGO` JP11 |
| SC6000 | `SC6000` JP13、`SC6000M` JP14 |
| PRIME 4 | `PRIME4` JC11、`PRIME2` JC16、`MIXSTREAMPRO` NH08 |

## 4b. 发版门禁：上一版在线升级 → 新版开机全功能验收（硬性，不过不发）

教训（5.3.0.1）：官方 5.3.0 删掉了 `etc.mount` / `var.mount`，`sc5000-custom-prepare.service` 上的 `Requires=` 因此失败。从 5.0.4.2 在线升级后，UI 包、版本号和 SSH 全部失效，而单测和镜像抽查都没发现。碟机多在俱乐部现场用，这类问题不能留给用户自己去踩。

教训（5.3.0.1 重发）：Engine 5.3.0 强制打开 Qt 的 QML 磁盘缓存（`QML_FORCE_DISK_CACHE` 盖过我们的 `QML_DISABLE_DISK_CACHE`），缓存放在 `/data/AppDataUser/AIR Music Technology/Engine/.cache/qml`，只按源文件时间戳判断是否过期。实机曾经在没有 UI 包的情况下跑过一次 Engine，留下了原厂缓存；之后 hook 日志照样显示「48 files」，界面却一直是原厂的。门禁以前只数 hook 日志，没看渲染结果，所以没发现。现在 hook 按内容给替换的 qrc 文件打时间戳，门禁先种下原厂旧缓存，再读实际渲染出的节点。

教训（5.3.0.4 候选）：`f6b87f8` 新增 `0x16fbc38` hook 站点排除静音影子槽参与 `MASTER` 选举后，只验了「影子副本不抢 MASTER」，没验早已修好的「两台真碟机之间手动切换 MASTER」，结果双机同时显示 `LEAD` 且按 `MASTER` 键无效。任何对共享机制（`MASTER`/Sync 选举、hook 站点、header 布局、patch 字典）的修改必须同时回归相邻既有行为，不得只测本次单点目标。

- 自定义 unit **不得对官方单元用硬依赖**（`Requires=` / `BindsTo=` / `Requisite=`），因为新底包可能删掉它。挂载用 `RequiresMountsFor=<路径>`（只对底包真有的挂载单元生效），其余用 `Wants=` + `After=`。`rootfs_audit.py` 把违反的写法记为 FAIL。
- **适配新官方底包时，以及每次发版（含同版本重发）**，都在第 4 步之后跑门禁。三个镜像组各跑一次，旧镜像用同一机型的**上一个已发布版本**：

```sh
# 编译机上以 root 运行，在 remote-build 同步过来的仓库根目录
bash tools/az01-packer/release-gate.sh \
  <上一版目录>/SC5000-<OLD>-EngineX-OS-Denon-SC5000-Prime-JP07-Update.img \
  <新输出目录>/SC5000-<VERSION>-EngineX-OS-Denon-SC5000-Prime-JP07-Update.img <VERSION> /var/tmp/sc5000-gate-<短说明>
# SC6000 组用 SC6000 JP13，PRIME 4 组用 PRIME4 JC11；工作目录每次用新的
```

退出码为 0、`results.txt` 里没有 `FAIL`，才能进第 5 步。

运行位置：门禁在编译机上原生跑（armhf 直接执行，不经 `qemu-arm`）。旧的 x86 编译机 IN 上 Engine 每次启动要 60–90 秒，编译机上约 2 秒。`release-gate.sh` 在没有注册 `qemu-arm` binfmt 且 `uname -m` 是 `aarch64`/`arm*` 时原生执行；在 x86 上仍要求带 F 标志的 `qemu-arm` binfmt。门禁全部在 `unshare -m -n -p` + chroot 里运行，挂载的是两个镜像解包出的**真实 `rootfs.ext2`**，宿主的 `/proc`、`/sys` 不会被写到：

| 阶段 | 检查 |
| --- | --- |
| 静态审计 | 新 rootfs 里所有自定义 unit 的 `Requires/BindsTo/Wants/After/Slice` 目标、`Exec*` 与 `EnvironmentFile`、脚本解释器和脚本里的绝对路径工具、suite 直接调用的工具（`exec.Command`）是否真实存在；`command -v` 可选工具缺失记为 WARN（例如 `iw` 缺失时，关闭 Wi-Fi 省电的分支从不执行）；宿主 systemd 再用 `systemd-analyze verify --root` 把所有自定义 unit 和带 drop-in 的官方 unit 解析一遍（5.3.0.1 第一次构建时它报出了 `Unit var.mount not found`），输出必须为空；overlay 里每个动态 ELF 跑 `ld.so --list`；engine 脚本不再强制 100 Mbps；所有 Engine 脚本过 `sh -n` |
| 上一版在线升级 | 旧 rootfs + 空 `/data`：etc-reset → prepare → stream-sync + Engine（带 UI hook）；本地 TLS GitHub mock 发布新版；`/v1/sc5000/update` 发现新版 → `?apply=1` → Engine 以 `UpdateFromUrl` 退出 → 官方 engine 脚本 → `az01-update --net` 下载并校验新镜像，并进入 Applying；作为对照，翻转一个字节的镜像必须在 Verifying 阶段被拒绝 |
| 新版开机 | 同一个 `/data` 换成新 rootfs：etc-reset → prepare（检查版本号、UI 包、SSH host key 保留）→ `sc5000-debloat --apply-runtime` → `sc5000-perf-tune` → `sshd -t` → safe-mode 下先跑一次原厂 Engine，在 `/data` 留下原厂 QML 缓存（复现 5.3.0.1 实机故障）→ stream-sync / deemixd / rb-bridge / engine-guardian / storage-discover / edisksd + hook → Engine + UI hook |
| 新版功能 | QML 替换数等于 UI 包文件数 + 1（门禁探针）；探针 `tools/az01-packer/gate-ui-probe.qml`（只在沙箱里替换原厂 `QmlWarnings.qml`，不进镜像）读取 Engine 实际渲染的节点：Settings → Services → Streaming 显示包里的 Deezer、SoundCloud 和 Beatport 各一行、原厂 Tidal 隐藏，About 显示 `EngineX OS <VERSION>`，控制中心有波形磁贴；Engine 补丁 16 处（TidalAPI 网关、Stems 等），21 个 DSP guard（含 Flanger 补偿 #54、主输出软限幅 #58），SQLite hook 已装，无 QML 加载错误、无崩溃，各守护进程存活；`/v1/sc5000/ui`、`POST /au/oauth2/token`、deemixd `/healthz` 正常；stems-probe 与 `stems-processor` 完成握手和分轨；rb-bridge 转换 Rekordbox 导出；fsck.hfsplus 可执行；下一次在线升级（mock 发布 `<VERSION>` 末位 +1）同样走通 |

- 预期值随底包变：`GATE_EXPECT_APPLIED`（默认：独立播放器 20、一体机 28，后者多 8 处 #206 录音补丁）、`GATE_EXPECT_DSP`（默认 21）、`GATE_LANG`（默认 `zh-Hans`）。一代 5.3.0 的常驻 hook 站点表增加到 20 项，因此同步更新补丁数默认值。只有底包或 hook 站点表变了才改，并写进变更记录。
- 沙箱里不真正执行、而是由记录 shim 代替的命令：`kexec`、`systemctl`、`reboot`、`encrypt-fs.sh`（对 `/data` 做 fscrypt，失败时会触发 Factory Reset）、`az01-update --erase-data`。`planck-remote-screen` 在沙箱里没有显示，它的 shim 改调 `az01-update --net`，两者用的是同一个 libaz01-prog 下载与校验代码。沙箱里没有 eMMC，`Applying` 之后会报 `unsupported operation`，这是预期结果，记为 INFO。真实刷写和重启仍需按第 8 步在实机上抽查。
- 另外两处是沙箱本身的限制，不是 bug：qemu-user 下 `/proc/PID/cmdline` 前面多出解释器路径，所以门禁在 `/usr/local/sbin` 放了一个去掉这个前缀的 `pkill`（原生运行时没有前缀，shim 原样匹配）；Engine 会把伪造的 ttyS0（IRQ 41）绑到 CPU 0，门禁用 tmpfs 盖住沙箱里的 `/proc/irq`，否则会改到宿主的 IRQ，宿主没有这个 IRQ 时 Engine 直接 abort；没有 udev 时 edisksd 会自行退出，所以门禁只验证 hook 能在 `LD_BIND_NOW=1` 下预加载，挂载流程由 `test_edisksd_hook.c --mounts` 覆盖。
- 证据在工作目录：`results.txt`、`audit.txt`、`old-*.log` / `new-*.log`、`*-shim.log`。摘要写进 `docs/ISSUE-FIXES-*.md`。
- **无回归检查（必做）**：三组门禁通过后、进入第 5 步发布前，必须逐项对照 [`docs/REGRESSION-CHECKLIST.md`](../../docs/REGRESSION-CHECKLIST.md) 核验全部自动化项与沙箱/实机项，并把结果写入 `docs/ISSUE-FIXES-<VERSION>.md`。任何新修复不得破坏旧修复或弱化既有单测；在实机上新发现的 bug 必须在同一轮向 `docs/REGRESSION-CHECKLIST.md` 追加一行。

## 5. Tag 与 GitHub Release

在编译机上直接上传全部 29 个文件（28 个产物 + `SHA256SUMS.txt`；那边 `gh` 已登录 `Cd1s`，不用把镜像拉回本机）：

```sh
# 资产清单以 SHA256SUMS.txt 为准（5.3.0.4 / 5.3.0.5 共 28 个：16 个 *-Update.img + 8 个 *-Data-Bundle.tar.gz + 4 个 EngineX-Installer-*）
# 新版本
gh release create v<VERSION> --repo Cd1s/EngineX-OS --title "EngineX OS <VERSION>" \
  --notes-file notes.md --target <提交> --latest \
  $(awk '{print $2}' SHA256SUMS.txt) SHA256SUMS.txt
# 同一版本修复重发（注意：--clobber 只覆盖同名文件；5.3.0.6+ 退役 *-Data-Bundle.tar.gz 时若覆盖旧同版本需用 gh release delete-asset 清理残留资产）
gh release upload v<VERSION> --repo Cd1s/EngineX-OS \
  $(awk '{print $2}' SHA256SUMS.txt) SHA256SUMS.txt --clobber
```

- Release Notes 必须包含完整的 **28 个产物 SHA-256 对照表**（8 款一代 `*-Update.img`、8 款二代 `*-Update.img`、8 款二代过渡兼容 `*-Data-Bundle.tar.gz`、4 平台 `EngineX-Installer`；`5.3.0.6+` 退役 `.tar.gz` 后为 20 个），每次重发都要换成新值。
- tag 指向记录这次发布的提交：`git tag -f v<VERSION> <提交> && git push -f origin v<VERSION>`（同一版本重发时移动 tag）。
- 新版本要设为 `--latest`，README 与官网 `enginexdj.com` 的 `releases/latest/download/...` 链接才会指向它。

## 6. 发布后核对与 Issue 通知

```sh
gh release view v<VERSION> --repo Cd1s/EngineX-OS --json assets -q '.assets[] | "\(.digest) \(.name)"'
```

- 每个资产的 digest 与 `SHA256SUMS.txt`、Release Notes 表格、`docs/ISSUE-FIXES-<VERSION>.md` 完全一致（`5.3.0.4` / `5.3.0.5` 共 29 个资产）。
- 新版本时，抽查 README 与官网 `enginexdj.com` 里的下载链接：`curl -sIL <链接> | grep -i "^HTTP"` 最终是 200。
- 在 `docs/ISSUE-FIXES-<VERSION>.md` 记录：日期、范围、原因、验证、构建提交、全部 28 个产物 sha256、门禁结果、回滚点（上一次构建的提交）、实机影响。
- 在本次发布解决或包含的所有关联 GitHub Issues 下逐条留言告知 `v<VERSION>` 已发布、修复点摘要与下载/升级方式。

## 7. 隐私与版权

- 提交、Release Notes、日志里不出现私钥、Token、Deezer ARL、密码、维护者的私有域名和内网主机名。私有域名清单不写进仓库，发版前向维护者确认并 `git grep` 为 0 命中。
- 不提交官方固件、解包的 rootfs、原厂二进制（`.gitignore` 已拦截 `*.img`、`*.ext2`、`*.payload` 等）。

## 8. 实机（可选）

- 实机只做热部署和只读核对；部署前先备份被替换的文件。
- 实机验收时必须跑完 [`docs/REGRESSION-CHECKLIST.md`](../../docs/REGRESSION-CHECKLIST.md) 中的实机必检项（包括标有 `[NO AUTOMATED TEST]` 的跨机手动 `MASTER` 互切、原厂 `LOAD` 流媒体播放、按键灯目测等），并将结果写入 `docs/ISSUE-FIXES-<VERSION>.md`。
- 测碟机端在线更新时，要把实机两个 `version.txt`（`/data/sc5000-custom/version.txt` 和 `/etc/sc5000/version.txt`）临时改成比 `<VERSION>` 旧的版本才会出现更新提示；`/etc` 那份用 bind mount 临时文件，重启即恢复。

## 9. 发版后与日常编译垃圾清理（必做，防磁盘与 tmpfs 爆满）

每次发版上传完成后（或大规模沙箱/门禁测试结束后），**必须立即清理编译机（及 x86 编译机、本机 `/tmp`）上的编译、门禁、解包临时目录与 Go 缓存**，只保留 `/root/build/sc5000-lab/` 固定实验材料与当前最新版的 3 个门禁基准 `.img`：

```sh
sshctl run "$BUILD_HOST" --no-stdin --argv sh -c '
set -eu
# 1. 将本次发布的 3 个代表镜像（JP07、JP13、JC11）存入 sc5000-lab/release-<VERSION> 供下次门禁作 <OLD> 基准，并删除旧版 release-* / databundle-*
mkdir -p /root/build/sc5000-lab/release-<VERSION>
cp -f <新输出目录>/SC5000-<VERSION>-EngineX-OS-Denon-SC5000-Prime-JP07-Update.img \
      <新输出目录>/SC6000-<VERSION>-EngineX-OS-Denon-SC6000-Prime-JP13-Update.img \
      <新输出目录>/PRIME4-<VERSION>-EngineX-OS-Denon-PRIME-4-JC11-Update.img \
      <新输出目录>/SHA256SUMS.txt \
      /root/build/sc5000-lab/release-<VERSION>/
for r in /root/build/sc5000-lab/release-* /root/build/sc5000-lab/databundle-*; do
  [ "$r" = "/root/build/sc5000-lab/release-<VERSION>" ] && continue
  rm -rf "$r"
done

# 2. 清理 /root/build 下除 sc5000-lab 外的所有 remote-build 临时工作区
for d in /root/build/*; do
  [ "$d" = "/root/build/sc5000-lab" ] && continue
  rm -rf "$d"
done

# 3. 清理 /var/tmp 与 /tmp 下的所有门禁、沙箱、解包临时目录（保留 systemd-private-* 与活跃会话）
for p in /var/tmp/*; do
  case "$(basename "$p")" in
    systemd-private-*) continue ;;
    *) rm -rf "$p" ;;
  esac
done
for p in /tmp/*; do
  case "$(basename "$p")" in
    systemd-private-*|tmux-*|ssh-*) continue ;;
    *) rm -rf "$p" ;;
  esac
done

# 4. 清理 Go 编译与测试缓存并核对剩余空间（根分区可用空间应 >140G，/tmp 接近 0%）
. /etc/profile.d/buildtools.sh && go clean -cache -testcache
df -h / /tmp /var/tmp
'
```

- **禁止删除的固定实验目录（`/root/build/sc5000-lab/`）**：`fw/`、`fw-5.3.0/`、`fw-5.3.0-gen2/`、`5.0.4/`、`5.3.0/`、`sclive-5.0.4/`、`sclive4-rea61-followup/`、`systemone-analysis/`（含已解包的 `rootfs/`）、`deps-sqlite/`、`sbin/`、`deep-symmetry-archive/` 以及当前最新版的 `release-<VERSION>/`。
