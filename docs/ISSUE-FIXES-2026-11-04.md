# Issue 修复记录（2026-11-04，Agent-10）

基线 `ee102c1`，工作区未提交。所有编译和测试都在 IN（`remote-build`）上执行：`go build/vet/test ./...` 全部通过，`tests/test_rootfs_overlay.sh` 12 组通过，`tools/az01-packer` 42 项、`tools/stems` 9 项通过。碟机上未部署任何改动。

C hook（#17–#24、#37 以及 `dsp_smooth.h` / `engine_ui_hook.c` / `edisksd_hook.c`）由 `1cf9a8a3` 负责，不在本表。

| Issue | 修复 | 回归 |
|---|---|---|
| #2 | `engine-ui-guard` 纳入 `rootfs-overlay`，健康窗口 15 s；新增 `stop`：`SERVICE_RESULT=success`（正常停止/重启）清零计数，崩溃仍计数。`ExecStopPost` 写在 `override.conf`（它的空 `ExecStopPost=` 会清掉 60-ui-hook.conf 里的）；`sc5000-debloat --root` 生成同一份 | `test_rootfs_overlay.sh` guard 用例；`TestUISettingsPersistAndRestart` 覆盖 boot-count |
| #3 | 6 张 SVG 文字重叠/溢出；关于页 `EngineX OS ${versionString} Custom`；默认仓库 `Cd1s/EngineX-OS` | 正常字体与 Verdana/Courier 替换渲染 |
| #4 | 快速导入在指纹为空（刚被 `stageOwnedMoves` 暂存）时写入最终路径；提交前若还有 `.rb-bridge-pending-*` 路径则回滚 | `TestMoveAndSwapPathsRetainEngineIDs` 增加 full→fast、fast→fast |
| #5 | 快速导入全程持有卷锁（与完整导入同一把）；`VACUUM INTO` 快照（含 WAL）；两文件先暂存、写 READY 再改名，下次运行补完中断的发布，并删除旧 `-wal/-shm/-journal` | `TestInterruptedFastPublishIsCompleted` |
| #6 | 快速导入用 `compat.Resolver` 解析卷上的真实拼写（大小写、NFC/NFD）；`mergeByPath` 改用折叠 + NFC | `TestFastImportResolvesTheVolumeSpelling` |
| #7 | post 计时器带启动令牌，pre 作废旧令牌，旧计时器不再清零后来的崩溃计数 | guard 崩溃/健康时序用例 |
| #8 | 手机 Deezer 页不再接受中继地址/密码；中继换主机时丢弃已存密码 | `TestPhoneLoginNeedsTokenAndLinksDeezer`、`TestRelayPasswordIsNeverReadBack` |
| #9 | `/api/ui` 先校验全部字段，再在同一把锁内写中继配置和设置 | 同上（非法 lang + relayUrl 不落盘） |
| #10 | 中继首次启动生成随机密码写入 `initial-password`（0600，日志只给路径）；不再接受 URL 中的密码；旧默认密码下 `?secrets=1` 和 Stems 返回 403 | `TestNoFixedDefaultPassword` |
| #11 | 限流 IPv6 按 /64；满表淘汰最久未活动且未被封的键，不再清空；`-trusted-proxies`；匿名 poll 不再创建会话 | `TestLimiterKeysIPv6ByPrefixAndNeverWipes`、`TestAnonymousPollDoesNotCreateSessions` |
| #12 | 所有平台的登录收尾走同一个加锁的 `complete`（在线、平台、未完成三项检查，配置同步在锁内）；第二个并发回调按成功处理且不改结果；会话文件写失败保持 dirty 重试 | `TestSecondCompletionIsIdempotent` |
| #13 | 版本按数字逐段比较，`dev` 不提示更新；检查用独立 context；未设 `version.txt` 时读镜像里的 `/etc/sc5000/version.txt` | `TestNewerVersion` |
| #14 | BPM 同步失败按 10 s / 30 s / 2 min 重试再回到每小时；传输错误带原因（不含 URL）；控制台访问码存 `<state-dir>/access-token`，只在首次生成时打印 | `TestAccessTokenSurvivesRestart` |
| #15 | Spotify `/items` 只在 404/405 时退回 `/tracks` | `TestSpotifyTracksFallBackOnlyWhenItemsIsGone` |
| #16 | `hasPicture` 按 ID3 帧头遍历 | `TestHasPictureReadsFrameIDs` |
| #17 | `dsp_smooth.h` / `engine_ui_hook.c`：按实例槽位隔离 FX transition/ramp 状态，新增 `0xdaec60` `ChannelDJFx::process` 通道级 owner 绑定（共 20 个跳板） | `test_dsp_smooth` 双通道 20,000 次并发事件、`test_stems_feature` QEMU 20 跳板校验 |
| #18 | `dsp_smooth.h`：从上一帧有效输出平滑过渡到当前输出，覆盖 `TurnOff` 旁路关闭时的首帧跳变 | `test_dsp_smooth` TurnOff 连续性与途中重触发用例 |
| #19 | `engine_ui_hook.c`：`is_telemetry_sqlite_filename()` 改为精确匹配路径末尾 basename（支持 URI query），不再误伤含子串的普通曲库路径 | `test_stems_feature` 曲库路径持久化用例 |
| #20 | `edisksd_hook.c`：为无原生 `Engine Library` 的只读 HFS+ 卷准备可写顶层视图，支持首次建库 | `test_edisksd_hook` 新卷首次建库用例 |
| #21 | `edisksd_hook.c`：保留并复制卷上既有原生 `Engine Library`，不再用空 lowerdir 遮蔽原有曲库与 Cue | `test_edisksd_hook` 原库保留用例 |
| #22 | `edisksd_hook.c`：持久化 HFS+ 缓存按卷 UUID/设备签名分目录隔离，拒绝同身份并发共享 | `test_edisksd_hook` 同名 A/B 盘隔离用例 |
| #23 | `edisksd_hook.c`：卸载前预留恢复树，若父卷 `umount2` 返回 `EBUSY` 失败则自动恢复原视图 | `test_edisksd_hook` 独立 mount namespace 卸载回滚用例 |
| #24 | `edisksd_hook.c`：超时计算改为 64 位毫秒，消除 ARM32 24.8 天 uptime 溢出 | `test_edisksd_hook` 边界与超时回收用例 |
| #25 | Engine 设 `HOME=/tmp/sc5000-engine-home`，`MESA_SHADER_CACHE_MAX_SIZE=32M` | overlay/debloat 一致性断言 |
| #26 | 删除 README 中未实现的 `SC5000_GITHUB_REPO` | — |
| #27 | `go.mod` 与全部 import 改为 `github.com/Cd1s/EngineX-OS` | 全量 `go test ./...` |
| #28 | `install.sh` 拒绝占位 `-public`，升级时保留已装单元；`-public` 改为必填；文档改正默认值 | — |
| #29 | Spotify 已学习的回调按「Client ID + 当前回调地址」记录，内置 Client 也要探测 | `TestSpotifyLearnedCallbackIsPerPublicURL` |
| #30 | 仓库根目录新增标准 `MIT` `LICENSE` 文件 | — |
| #31 | `--spotify-token` 默认跟随 `--state-dir` | — |
| #32 | 打包器按 compatible 支持三组 AZ01 底包（见 `tools/az01-packer/README.md`），兼容 8 字节对齐补零；AZ0x（SC LIVE 2，带签名）明确拒绝；`patch-rootfs --set-version` | `test_models_and_aligned_trailer`、`test_set_version_writes_version_and_new_reset_epoch`；SC6000、PRIME 4 真实底包试打包并回读 |
| #33 | cleanup 在没有 `connmanctl` 时直接退出；`/etc` 刷新 epoch = 版本 + overlay `/etc` 内容哈希，并删除这些文件在上层的旧副本；打包时离线执行 overlay 的 `sc5000-debloat --root` | `test_overlay_debloat_runs_offline` |
| #34 | `Track.bitrate` 改为 kbit/s；已分析曲目重导入时更新文件信息并补缺失波形；歌单尾节点兼容 NULL，只链接一个尾 | `TestEngine504AtomicImport` |
| #35 | `foldBPM` 在窄于一个八度的窗口里取较近的边界；comb 搜索结果夹在窗口内；解码器采样率上限与分析器一致（384 kHz） | `TestFoldBPMStaysInsideANarrowWindow` |
| #36 | `ApplyToProcess` 遵守 `StrictCPU`、跳过已退出线程；ProLink 名称截到首个 NUL；设备表上限 64，补 `LastSeen` | `TestApplyToProcessHonoursStrictCPU`、`TestObserveBoundsTheTableAndStampsLastSeen` |
| #37 | `edisksd_hook.c`：逐级拒绝 symlink、`O_NOFOLLOW` 并核对 mount ID，防止恶意符号链接卸载系统 overlay | `test_edisksd_hook` 卷外哨兵挂载保护用例 |
| #38 | worker 跳过 init 头前的空行，缺字段报明确错误 | `test_blank_lines_before_init_are_skipped` |
| #39 | 已备份并更新 `<relay-host>` 上的 `/opt/sc5000-stems/worker.py`、`/opt/sc5000-stems/sc5000-stemsd` 与 `/usr/local/bin/sc5000-relay` | `<relay-host>` 线上 `healthz` 验证通过 |
| #40 | 掩码不足 4 字节的接口跳过 | — |
| #41 | 502/503/504 且 Engine 的 stdin 一个字节都未交出时，降级到局域网/本机；stdin 复用层把被放弃上传读到的数据转交下一个渲染端 | `TestBusyRelayFallsBackWithoutSplittingTheStream` |
| #42 | 局域网健康探测关闭 keep-alive | — |

## 核实不成立 / 架构边界说明

- **#43 重采样延迟**：不成立。julius 0.2.8 `resample_frac` 用对称 sinc 核（`arange(-width, width+old_sr)`，左补 `width`），零群延迟；melody 也是碟机自己用 input − 其余三轨重建。
- **#32 中的 `--target-model` 与 `unlock_jp07_features.py`**：不提供跨组改写目标型号（会把一组的 rootfs 刷到另一组硬件上）。`unlock_jp07_features.py` 的偏移是 JP07 配置分支专用，其他型号走 hook 的通用 `getConfig` 入口；没有逐型号逆向结果前不新增偏移。
- **#33 第 3 点运行时 IRQ 被还原**：在实机上未复现。碟机脚本已打补丁，IRQ 亲和性和千兆网口都符合预期。

## 5.0.4.1 发布打包（同日追加）

- `sc5000-suite` 现在包含五个工具：rb-bridge、stream-sync、engine-guardian、stems-processor、sc5000-deemixd。后两个的代码移到 `internal/stemsproc`、`internal/deemixd`，`cmd/` 下的独立入口仍可编译。`rootfs-overlay` 新增两个包装脚本，以及 `sc5000-deemixd.service`。
- `sc5000-custom-prepare` 每个版本把 `ui-packs.tar.xz` 解到 `/data/sc5000-custom/ui` 一次，清除 safe-mode 和计数，并写入版本号；`engine.service` 排在它之后启动。
- 「检查更新」按设备的 compatible 匹配 Release 里本机型的升级包，交给 Engine 自带的在线升级（`updateOnlineForUrl`）。
- 新增根目录 `LICENSE`（MIT）。
- `tools/az01-packer/build-release.sh` 在 IN 上生成 `/root/build/sc5000-lab/release-5.0.4.1/` 下的 8 个镜像。每个都经过 `info` 校验，校验和见 `SHA256SUMS.txt`；三组的 sha256 各自相同。没有刷入碟机。

## 5.0.4.1 在线更新收尾（同日追加，Opus 5.5）

- 设置页「更新」按钮先弹原厂 `FirmwareUpdateAvailable` 同款确认框（写法与 qrc 里 `FirmwareUpdateAvailableMessageBox.qml` 一致），确认后由 stream-sync 写 `/tmp/update-file-url` 和 `/tmp/engine-quit-reason=UpdateFromUrl`，再给 Engine 发 SIGTERM。原厂 `/usr/Engine/Scripts/engine` 随后启动 `/usr/bin/planck-remote-screen --update <URL>`：libaz01-prog 用 curl 下载（跟随 GitHub 302）到 `/data/az01-update.img`，校验后 `reboot update`。
- **Engine 的进程名是 `EMain`**，原来的 `pkill -x Engine` 匹配不到，会退回自下载 + `az01-update --file`。改为 `pkill -f '^/usr/Engine/Engine( |$)'`。`pkill` 成功后清掉 `updating`（刷写失败、Engine 被拉起时可以重试）；失败时删除两个 `/tmp` 文件，避免 Engine 之后崩溃时意外进入刷写。
- 关于页版本号改用 stream-sync 报的已安装版本（`/data` 与镜像 `/etc` 中较新的那个）。
- 版本号固定为 5.0.4.1 后，重打包的镜像版本号不变：`sc5000-custom-prepare` 改为同时比较 UI 包归档的 sha256（`.pack-id`），`sc5000-suite-exec` 改为比较 `sc5000-suite.xz` 的 sha256（`sc5000-suite.id`），不再靠 mtime。同一镜像下手工部署的包和二进制仍然保留。
- `engine.service` 加 `QML_DISABLE_DISK_CACHE=1`：#25 让 HOME 可写后 Qt 开始在 `/tmp/sc5000-engine-home/.cache/.../qmlcache` 缓存 `.qmlc`（实机 918 个）。UI hook 替换 qrc 内容但保留时间戳，缓存一直有效，切换语言或换包后重启 Engine 仍用旧编译结果，直到整机重启。关掉缓存即恢复 #25 之前的行为。
- `build-release.sh` 对三组构建逐个 `wait`（`wait a b c` 只返回最后一个的状态）。
- 实机 SC5000 端到端两轮：临时把两个 `version.txt` 设为 5.0.4（`/etc` 那份用 bind mount，不写 overlay 上层）→ `?apply=1` → 日志 `Engine quit with reason: UpdateFromUrl` → planck 下载约 15 s → `verifying image... / preparing upgrade environment...` → 重启刷写约 1 分钟 → 两个版本号 5.0.4.1、`engine.service` active、`NRestarts=0`、`/ui` 的 `version` 与 `/update` 的 `currentVersion` 都是 5.0.4.1、`updateAvailable:false`。关于页截图显示 `EngineX OS 5.0.4.1 Custom` 与 `已是最新 (5.0.4.1)`；确认框点「更新」的路径也在实机走过。
- 原厂 5.0.4 没有 `/usr/share/planck/update-firmware-from-web-background-270.data`（4.3.4 有），planck 刷机界面没有背景图，不影响刷写。
- 实机 `/etc` overlay 上层有一份手工放的 `sc5000/libengine-ui-hook.so`（与仓库相同），会遮住新镜像的 hook，已移除（备份在 agent-backup）。
- Release `v5.0.4.1` 的 8 个镜像已覆盖上传。sha256：SC5000/SC5000M/PRIME GO `b0f43a46…aaca`，SC6000/SC6000M `d7f88604…0011`，PRIME 4/PRIME 2/Mixstream Pro `489fdb5b…26d4`。
- 回滚点：Release 旧资产已被覆盖；回滚方式是在 IN 上 checkout `adc1fa1` 重新执行 `build-release.sh` 并重新上传。

## 全链路压测与修复（同日追加，PE-TestController · Opus 5.5 统筹）

四个专项 Agent 并行：REA（GPT-6.1 Sol，闭源 DSP 逆向 + 独立复核）、Deck-Audio（真机双轨压测）、Stems-Stream（流媒体/Stems/QEMU）、Fix（Sonnet 5.5，修复实现）。

- **关于页**：`Version.qml` 显示 `EngineX OS 5.0.4.1`，去掉 `Custom`（上一节截图记录的是去掉之前的状态）。
- **Stems 403**（实机 `stems-processor` 空输入握手 `backend status 403`）：relay 状态文件里 `passwordIsDefault=true` 是旧版本留下的过期标志，实际密码早已不是默认值；c4408fc 起 `requireStemsAuth` 对该标志返回 403，`?secrets=1` 同样被拒（碟机每 30 s 一次 `GET /api/config 403`）。线上处理：经管理 API 用原密码重设一次清掉标志，凭据未变、服务未重启；握手恢复 exit 0 / 26 字节，relay `POST /v1/engine 200`。代码：`sc5000-relay` 启动时按 hash 重算该标志（`TestStaleDefaultPasswordFlagIsCleared`）。relay 新二进制尚未部署到 <relay-host>，下次升级 relay 时生效。
- **DSP（`dsp_smooth.h` / `engine_ui_hook.c`）**：逆向确认 transition 的 owner 是 `ChannelDJFx`，而 `0x15fa188` 收到的是各效果器 `+0x68` 的 common 子对象，由多个 RR49 渲染线程调用、原地处理、frames 无上限。原来全进程只有一个 ramp 计数，一条 common 会吞掉或重置另一条的去咔嗒斜坡。现在：全局 generation 广播 + 每个 common 一个槽（32），槽在 enter→原厂处理→post→exit 期间由只尝试、不等待的 busy 标志独占；忙、或表满且无过期槽时，该块只做有限值净化。只快照别名输入的前 128 帧，`hook_fx_common_chain` 栈帧 16440 B → 1060 B，超过 2048 帧的块也能 ramp。REA 第一轮复核指出两处槽生命周期竞态（回收抢走在途槽、先发布后初始化），busy 协议闭合后第二轮无阻断。实机读 hook 的 `state`（.so vaddr `0x20164`）确认 transition 与 common tail 路径在真机被执行。
- **hook 二进制可复现**：仓库旧 `libengine-ui-hook.so`（75244 B，`bffc55…`）与其源码用 `build.sh` 重编的结果不一致；现在提交的 `007ad00b…` 由 IN 上 `build.sh` 构建，两次构建 sha256 相同。
- **stream-sync**：没有前置 SD Engine Library 时（SD 卡按卷标挂在 `/media/SD`；按用户要求不自动扫描 `/media/*` 写库），每个流媒体文件都会排一个只能以 `pipeline is not configured` 结束的导入任务；现在不再排入，播放不受影响。
- **Go 小优化**：Stems pipeline 用 `slices.Grow` 去掉每块临时分配；网易云请求不再回落到无超时的 `http.DefaultClient`；封面 LRU 不再拖住旧底层数组。IN 上原有两个旧测试残留（`cmd/sc5000-deemixd/main_test.go`、`cmd/stems-processor/main_test.go`）导致全量测试构建失败，已删除。
- **验证（全部在 IN 或真机上）**：Go 全量 `vet` + 测试 19 个包通过；`test_dsp_smooth`（ASan/UBSan/TSan）与 `test_stems_feature`（armhf + qemu-arm，12 项）通过；`tests/test_rootfs_overlay.sh` 13 项、az01-packer 单测通过；抽查 SC5000 镜像内 hook 为 `007ad00b…`、`version.txt` 为 5.0.4.1、`Version.qml` 无 `Custom`。
- **真机压测（SC5000，双轨同播 + 每秒约 25 次 UI/StateMap 操作：跳点、Loop、Sync、Tempo、Slip、波形/灯效切换，加 Touch FX 驱动）**：共 12 轮，所有轮次 `NRestarts=0`、ALSA 始终 RUNNING、运行期 journal 无崩溃/xrun 行、RSS 835–843 MB 平稳、线程 94。`avail_max` 超过 buffer（900 帧，写指针落后 2–6 帧，< 0.1 ms）的亚毫秒欠载：无驱动 0 次；有效的驱动轮次 0.7–1.7 次/分，新 hook、关闭 DSP 组、common-chain 直通三种构建之间没有可区分的差异（最终验收 run8 为 0.69 次/分）。一轮 6.1 次/分的离群值没有复现；另一轮 10.8 次/分作废（dev eval 通道在 Engine 重启后会重放上一次脚本，驱动跑了两份）。诊断计数显示 `0x15fa188` 每个 period 调用一次（213 次/秒、225 帧、100% in-place），包装层开销约为周期的 0.05%。欠载来源未定位（遗留，与 hook 无关）。FX transition 由 Touch FX 的 `touched` 边沿推动（12.6 次/秒压测下 hook 稳定、busy 无残留）；common 槽峰值 1/32，32 槽容量与 128 帧 ramp 在实机上没有被观测到。Sweep FX、EQ Kill、硬件 Hot Cue 键、Jog 等硬件输入 eval 无法注入，没有覆盖；Hot Cue 用 `SetSongPosition` 跳点模拟；没有听感验证。
- **Stems / 流媒体**：真实 239.7 s 曲目经实机 `stems-processor` 远端分轨 203.6 s（1.18× 实时，含测试管道开销），本机回退 168.2 s（1.43×），四轨峰值非零、非有限值 0；Deck 1 上 Vocal/Melody/Bass/Drums 四个 State 可独立关闭和恢复（未听测）。网易云、Spotify、Deezer（FLAC）、SoundCloud 的真实歌单、搜索和获取均 200；TIDAL 原生登录链路、BPM/Key 入库（无 SD 曲库）、经 Engine 渲染队列生成 `.stems` 没有覆盖。QEMU 对比因 IN 缺模拟器与实例未执行。
- **最终实机状态（20:01）**：正式 hook `007ad00b…`、`sc5000-suite` `52307c36…`、生产 UI 包；关于页截图显示 `EngineX OS 5.0.4.1` 与 `已是最新 (5.0.4.1)`；`/v1/sc5000/update` 为 `updateAvailable:false`；Stems 握手 exit 0 / 26 字节；`NRestarts=0`，dmesg/journal 无 xrun。dev UI 包备份 `20261104T125903Z-…-727378e18909-…`。
- **Release**：8 个镜像由 `3eac455` 在 IN 上构建并覆盖上传到 `v5.0.4.1`。sha256：SC5000/SC5000M/PRIME GO `32dcbeed…64fb`，SC6000/SC6000M `ba0988ec…fc58`，PRIME 4/PRIME 2/Mixstream Pro `57c42b49…d039`。Release 标题与说明去掉 `Custom`，tag `v5.0.4.1` 移到本次发布提交。
- **实机部署**：hook 与 `sc5000-suite` 部署前已存 agent-backup（hook `20261104T112132Z-…-bffc55eba1c8-…`、suite `20261104T112140Z-…-b50ab25a3d37-…`；104 relay 配置 `20261104T105742Z-…`，碟机 deemix 配置 `20261104T105738Z-…`）。回滚：从备份取回旧 hook/suite 覆盖并重启 Engine/stream-sync；Release 回滚为在 IN 上 checkout `7ef5b92` 重跑 `build-release.sh` 后重新上传。
