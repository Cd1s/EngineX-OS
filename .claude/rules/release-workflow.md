# EngineX OS 发版规则（Release Checklist）

任何人或 AI agent 发布 EngineX OS（包括新版本，以及同一版本的修复重发）都按这份清单做，不跳步。`<VERSION>` 指 `5.0.4.x` 这样的版本号，`<OLD>` 指上一个版本号。

## 0. 先确定发哪种

- **同一版本修复重发**（维护者默认做法：版本号固定，例如一直是 5.0.4.1）：不改版本号，跳过第 1 步，第 5 步用 `--clobber` 覆盖同一个 Release，再把 tag 移到新提交。
- **新版本**：只有维护者明确要求才升版本号。按全部步骤做，新建 `v<VERSION>` tag 与 Release。

## 1. 必改文件（只在新版本时改）

| 文件 | 要改的地方 |
| --- | --- |
| `rootfs-overlay/etc/sc5000/version.txt` | 整个文件内容就是 `<VERSION>` |
| `README.md`、`README.zh-CN.md`、`README.zh-TW.md` | 第 3 行 hero 图的 `alt="EngineX OS <VERSION>"`；简介里的 `**EngineX OS <VERSION>**`；性能对比表表头里的 `` `EngineX OS <VERSION>` ``；「如何安装与升级」里全部 8 个机型的文件名和下载链接（三份 README 各 8 条，共 24 条） |
| `docs/assets/hero-banner*.svg` | 三张 banner 现在不写版本号。大版本换视觉或 banner 里出现版本号时才改，三语一起改 |
| `tools/az01-packer/README.md` | 示例命令里的版本号（可选，保持示例与现行版本一致） |

下载链接格式（文件名带版本号，所以每次升版本都要改）：

```
https://github.com/Cd1s/EngineX-OS/releases/latest/download/<PREFIX>-<VERSION>-EngineX-OS-<Model>-Update.img
```

改完检查没有漏掉的旧版本号（测试夹具里的版本号不用改）：

```sh
git grep -n "<OLD>" -- README.md README.zh-CN.md README.zh-TW.md rootfs-overlay docs/assets
```

`tests/`、`test_az01_packer.py` 里的版本号是测试数据，不要改。镜像里的 `/etc/sc5000/version.txt` 由 `build-release.sh` 的 `--set-version` 写入，第 1 步改的仓库文件用于实机热部署和保持一致。

## 2. 测试（只在编译机上跑，本机不编译）

```sh
gofmt -l . && go vet ./... && go test -race ./...
bash tests/test_rootfs_overlay.sh                 # 必须用 bash
python3 -m unittest discover -s tools/az01-packer
python3 -m unittest discover -s tools/stems
# C 单测：编译命令写在各文件头部
#   tools/ui-mod/test_dsp_smooth.c（另跑 ASan/UBSan 与 TSan 各一次）
#   tools/ui-mod/test_stems_feature.c（armhf + qemu-arm）
#   tools/edisksd-hook/test_edisksd_hook.c（--mounts 只在 unshare -m 里跑）
```

涉及 Factory Reset、`az01-update --erase-data`、空白 `/data` 首次启动的改动，只在编译机的 `unshare -m` + `qemu-arm` 沙箱里用解包出的 rootfs 测，**绝不在实机触发**。

## 3. 提交与构建

1. 改动先提交并推送到 `origin/main`，构建必须对应一个已推送的提交。
2. 用干净目录构建（`remote-build` 同步不会删除远端已删除的文件，旧目录会带上残留）：

```sh
remote-build --name sc5000-release-<短说明> --clean -- \
  'sh tools/az01-packer/build-release.sh <VERSION> <官方固件目录> none <fsck.hfsplus> <新输出目录>'
```

第 3 个参数在公开发布时**必须是 `none`**：公开镜像不能内置任何人的 SSH 公钥（#46）。只有维护者自用的私有镜像才传公钥文件，且私有镜像不能上传到 Release。

输出目录必须不存在、且不在任何 git 工作区里。产出 8 个 `*-Update.img` 和 `SHA256SUMS.txt`。

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

运行位置：ARM 门禁和沙箱**优先放在 ARM 编译机（ssm 别名编译机，简称 ID）上原生跑**。IN 是 x86，`qemu-arm` 软模拟下 Engine 每次启动要 60–90 秒，整树遍历一次 QML 节点要 20–35 秒。注意：`release-gate.sh` 目前会检查 `qemu-arm` binfmt，pkill shim 也按 qemu 的 cmdline 前缀处理，第一次在 ID 上跑之前要先适配这两处并验证一遍（截至 2026-11-05 还没在 ID 上跑过）。ID 上没有官方固件和构建产物时，构建仍在 IN 上做，再把镜像传过去。门禁全部在 `unshare -m -n -p` + `qemu-arm` chroot 里运行，挂载的是两个镜像解包出的**真实 `rootfs.ext2`**，宿主的 `/proc`、`/sys` 不会被写到：

| 阶段 | 检查 |
| --- | --- |
| 静态审计 | 新 rootfs 里所有自定义 unit 的 `Requires/BindsTo/Wants/After/Slice` 目标、`Exec*` 与 `EnvironmentFile`、脚本解释器和脚本里的绝对路径工具、suite 直接调用的工具（`exec.Command`）是否真实存在；`command -v` 可选工具缺失记为 WARN（例如 `iw` 缺失时，关闭 Wi-Fi 省电的分支从不执行）；宿主 systemd 再用 `systemd-analyze verify --root` 把所有自定义 unit 和带 drop-in 的官方 unit 解析一遍（5.3.0.1 第一次构建时它报出了 `Unit var.mount not found`），输出必须为空；overlay 里每个动态 ELF 跑 `ld.so --list`；engine 脚本不再强制 100 Mbps；所有 Engine 脚本过 `sh -n` |
| 上一版在线升级 | 旧 rootfs + 空 `/data`：etc-reset → prepare → stream-sync + Engine（带 UI hook）；本地 TLS GitHub mock 发布新版；`/v1/sc5000/update` 发现新版 → `?apply=1` → Engine 以 `UpdateFromUrl` 退出 → 官方 engine 脚本 → `az01-update --net` 下载并校验新镜像，并进入 Applying；作为对照，翻转一个字节的镜像必须在 Verifying 阶段被拒绝 |
| 新版开机 | 同一个 `/data` 换成新 rootfs：etc-reset → prepare（检查版本号、UI 包、SSH host key 保留）→ `sc5000-debloat --apply-runtime` → `sc5000-perf-tune` → `sshd -t` → safe-mode 下先跑一次原厂 Engine，在 `/data` 留下原厂 QML 缓存（复现 5.3.0.1 实机故障）→ stream-sync / deemixd / rb-bridge / engine-guardian / storage-discover / edisksd + hook → Engine + UI hook |
| 新版功能 | QML 替换数等于 UI 包文件数 + 1（门禁探针）；探针 `tools/az01-packer/gate-ui-probe.qml`（只在沙箱里替换原厂 `QmlWarnings.qml`，不进镜像）读取 Engine 实际渲染的节点：Settings → Services → Streaming 显示包里的 Deezer、SoundCloud 和 Beatport 各一行、原厂 Tidal 隐藏，About 显示 `EngineX OS <VERSION>`，控制中心有波形磁贴；Engine 补丁 16 处（TidalAPI 网关、Stems 等），19 个 DSP guard，SQLite hook 已装，无 QML 加载错误、无崩溃，各守护进程存活；`/v1/sc5000/ui`、`POST /au/oauth2/token`、deemixd `/healthz` 正常；stems-probe 与 `stems-processor` 完成握手和分轨；rb-bridge 转换 Rekordbox 导出；fsck.hfsplus 可执行；下一次在线升级（mock 发布 `<VERSION>` 末位 +1）同样走通 |

- 预期值随底包变：`GATE_EXPECT_APPLIED`（默认 16）、`GATE_EXPECT_DSP`（默认 19）、`GATE_LANG`（默认 `zh-Hans`）。只有底包或 hook 站点表变了才改，并写进变更记录。
- 沙箱里不真正执行、而是由记录 shim 代替的命令：`kexec`、`systemctl`、`reboot`、`encrypt-fs.sh`（对 `/data` 做 fscrypt，失败时会触发 Factory Reset）、`az01-update --erase-data`。`planck-remote-screen` 在沙箱里没有显示，它的 shim 改调 `az01-update --net`，两者用的是同一个 libaz01-prog 下载与校验代码。沙箱里没有 eMMC，`Applying` 之后会报 `unsupported operation`，这是预期结果，记为 INFO。真实刷写和重启仍需按第 8 步在实机上抽查。
- 另外两处是沙箱本身的限制，不是 bug：qemu-user 下 `/proc/PID/cmdline` 前面多出解释器路径，所以门禁在 `/usr/local/sbin` 放了一个去掉这个前缀的 `pkill`；没有 udev 时 edisksd 会自行退出，所以门禁只验证 hook 能在 `LD_BIND_NOW=1` 下预加载，挂载流程由 `test_edisksd_hook.c --mounts` 覆盖。
- 证据在工作目录：`results.txt`、`audit.txt`、`old-*.log` / `new-*.log`、`*-shim.log`。摘要写进 `docs/ISSUE-FIXES-*.md`。

## 5. Tag 与 GitHub Release

在编译机上直接上传（那边 `gh` 已登录，不用把 1.5 GB 拉回本机）：

```sh
# 新版本
gh release create v<VERSION> --repo Cd1s/EngineX-OS --title "EngineX OS <VERSION>" \
  --notes-file notes.md --target <提交> *.img SHA256SUMS.txt
# 同一版本修复重发
gh release upload v<VERSION> --repo Cd1s/EngineX-OS *.img SHA256SUMS.txt --clobber
```

- Release Notes 必须有 8 个机型的 SHA-256 对照表（机型、板号、文件名、SHA-256），每次重发都要换成新值。
- tag 指向记录这次发布的提交：`git tag -f v<VERSION> <提交> && git push -f origin v<VERSION>`（同一版本重发时移动 tag）。
- 新版本要设为 latest，README 的 `releases/latest/download/...` 链接才会指向它。

## 6. 发布后核对

```sh
gh release view v<VERSION> --repo Cd1s/EngineX-OS --json assets -q '.assets[] | "\(.digest) \(.name)"'
```

- 每个资产的 digest 与 `SHA256SUMS.txt`、Release Notes 表格一致。
- 新版本时，抽查 README 里的下载链接：`curl -sIL <链接> | grep -i "^HTTP"` 最终是 200。
- 在 `docs/ISSUE-FIXES-*.md` 记录：日期、范围、原因、验证、构建提交、三组 sha256、回滚点（上一次构建的提交）、实机影响。

## 7. 隐私与版权

- 提交、Release Notes、日志里不出现私钥、Token、Deezer ARL、密码、维护者的私有域名和内网主机名。私有域名清单不写进仓库，发版前向维护者确认并 `git grep` 为 0 命中。
- 不提交官方固件、解包的 rootfs、原厂二进制（`.gitignore` 已拦截 `*.img`、`*.ext2`、`*.payload` 等）。

## 8. 实机（可选）

- 实机只做热部署和只读核对；部署前先备份被替换的文件。
- 测碟机端在线更新时，要把实机两个 `version.txt`（`/data/sc5000-custom/version.txt` 和 `/etc/sc5000/version.txt`）临时改成比 `<VERSION>` 旧的版本才会出现更新提示；`/etc` 那份用 bind mount 临时文件，重启即恢复。
