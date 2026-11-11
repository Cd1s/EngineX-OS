# EngineX OS 5.3.0.4 发版与 Issue 修复记录（2026-11-11，RC（待维护者确认发布））

- **日期**：`2026-11-11`
- **状态**：RC（待维护者确认发布；实机线 `agent/shadow-grid-halfwave-20261110` 合并至 `4386093`，沙箱线 `agent/sandbox-main-20261110` 合并至 `2031ae2`）
- **版本范围**：`v5.3.0.3`（tag `cd789f8` / 构建提交 `b941ea4`）→ `v5.3.0.4`（RC 构建提交：`5038e5b`）
- **回滚点**：`v5.3.0.3`（tag `cd789f8`，构建提交 `b941ea4`；同版本更早构建 `37e83ea`、`9616419`）
- **覆盖机型与产物（共 20 个发布资产 + `SHA256SUMS.txt`）**：
  - **8 款一代机完整固件（`*-Update.img`）**：`SC5000`（JP07）、`SC5000M`（JP08）、`PRIME GO`（JP11）、`SC6000`（JP13）、`SC6000M`（JP14）、`PRIME 4`（JC11）、`PRIME 2`（JC16）、`Mixstream Pro`（NH08）
  - **8 款二代机 `/data` 数据包（`*-Data-Bundle.tar.gz`，Beta）**：`SC LIVE 4`（JP21）、`SC LIVE 2`（JP20）、`PRIME 4+`（JC11S）、`PRIME GO+`（JP11S）、`Mixstream Pro+`（NH08S）、`Mixstream Pro Go`（NH10）、`PRIME 4 G2`（JP24，64-bit AZ04）、`Rane SYSTEM ONE`（RMZ2，64-bit AZ04）
  - **4 平台桌面一键安装器（`EngineX-Installer-5.3.0.4-*`）**：Windows 10/11 x64（`.zip`）、macOS Apple Silicon M1–M4（`.zip`）、macOS Intel（`.zip`）、Linux x86_64（`.tar.gz`）
- **在线文档与着陆页**：`docs/index.html` + `docs/site/{content.js,mods.js,app.js,style.css}`（简中 / 繁中 / English 三语文档站，移除发布功能的 `tone: "new"` 标记，递增静态缓存参数至 `?v=15`，并在原生 ARM64/armhf 构建环境基于 `QT_QPA_PLATFORM=eglfs` 与 `chromium-headless-shell` 重拍 13 张三语与官网截图）

---

## 1. Issue 与 PR 修复总表（v5.3.0.3 → v5.3.0.4）

| Issue / PR | 模块 | 原因与修法摘要 | 验证与回归测试 |
| --- | --- | --- | --- |
| **#49**（影子轨模式、Engine 模式头部/概览镜像、静音影子槽 MASTER 选举排除、显示模式对齐原厂、独立播放器四轨颜色与行为保留） | `pkg/webui/shadow.go`<br>`pkg/webui/shadow_display.go`<br>`pkg/enginedb/display.go`<br>`tools/ui-mod/build_packs.py`<br>`tools/ui-mod/engine_ui_hook.c` | **原因**：① 四轨模式下对端关机/退歌时残留波形，或 `RemoteState` 订阅死锁阻塞流媒体切源，流媒体曲目显示数字缓存文件名；② 影子轨缺乏统一加载策略，拷贝期间跳回 `No Track Loaded`，Engine 模式下影子轨头部读本地静音副本的空白标签/本地分析调性且缺源轨 `Sync`/`LEAD`/`Loop`/`KeyOffset`/`Stems`/`Slip` 状态，本机静音影子槽在 `engineSync` 中可能误参与 `MASTER` 选举，且空 Engine 槽重复解析快照增加开销；③ 显示模式（CDJ 或回退）头部与波形泳道同原厂存在视觉差异，且原厂 `beatData` 概览整秒截断与负采样点 main cue 会被校验拦截；④ 独立播放器（`SC5000 / SC6000`）开启四轨后原厂将其视作四通道一体机，丢失 `Player #`、`Layer A / B` 双层选色盘、`Enable for Dual Deck` 与 `Browse` 按压直载当前层行为。<br>**修法**：① 对端发送 UDP 告别包并在 350 ms 防抖后执行 `Unload` + `UnSync` + 交还 `MASTER`，移除 `RemoteState` 死锁订阅，从源机 `stm.db` 解析真实 `Artist - Title`；② 新增 `Shadow Deck Mode`（`auto` / `engine` / `cdj`）：默认 `Auto` 下只要对端 EngineX 或 CDJ 曲目可拷贝即由本机 Engine 静音加载，无法拷贝的 CDJ 曲目自动回退显示 rekordbox 波形并提示原因，拷贝期间立即显示源曲目信息与原厂 `ProgressRing` 进度环（限制单曲音频拷贝上限 1 GiB，下载前清理过期缓存防 `ENOSPC`，并修复时钟回拨后 POST 恢复）；③ Engine 模式下影子轨头部镜像源轨真实 `Title`、`Artist`、`Key`、`KeyOffset`、`AutoLoop`、`Stems` 图标、`Sync` 绿 BPM 与仅在源轨显示时点亮的 `LEAD`（`ArtistSong.qml` 同时遵循 `Hide Title/Artist`），概览波形绘制源轨 `Slip` 滑行线，Follower 采用均值线弯音追赶，并在 5.3.0 hook 新增 `0x16fbc38` 站点排除本机静音影子槽参与 `MASTER` 选举（`f6b87f8`）及跳过空 Engine 槽重复快照（`4386093`）；④ 显示数据兼容原厂 `beatData` 概览整秒截断与早于 `0` 的 main cue（`fd9c8d5`），`All CDJ Mode` 与 CDJ 回退下头部与波形层全面对齐原厂；⑤ 独立播放器开启四轨后完整恢复 `Player #`、`Deck Color`（`Layer A / B` 双层选色盘）、`Enable for Dual Deck`、`Browse` 按压直载当前层、远程碟机选择、`Engine Connect` 与 `Touch FX Mute`。 | `go test -race ./pkg/webui/... ./pkg/enginedb/...`、`tools/az01-packer/test_ui_mod.py`（103 项 PASS）、`tools/ui-mod/test_shadow_master.c`（144 项原生用例 PASS）；双 SC5000 实机 A1–A11 与双机沙箱录像 S1–S8 验收通过 |
| **#85**（RGB 与单色按键灯亮度分级调光） | `tools/ui-mod/engine_ui_hook.c`<br>`tools/ui-mod/build_packs.py` | **原因**：硬件控制器单色按键（`SYNC`、`KEYLOCK`、`SLIP` 等）仅支持亮/暗两档 MIDI `NoteOn` 状态，此前对 `NoteOn` 缩放会导致单色按键灯状态异常。<br>**修法**：`Settings → Device → Pad LED Brightness`（`100%`–`20%`）对 RGB 彩灯（打击垫、模式键、`Layer`、转盘灯环与屏幕虚拟打击垫）按视觉感知曲线分级调光，单色 `NoteOn` 按键原样透传保持原厂亮度。 | `test_ui_mod.py::test_led_brightness_rgb_and_single_color_passthrough`、`test_az04_hook.c` |
| **#105**（OmniSource 歌单隔离） | `pkg/webui/tidal_gateway.go`<br>`pkg/providers/` | **原因**：开启 OmniSource 后各流媒体平台自身的歌单页混入了 `[USB: ...]` 本地盘与跨平台歌单。<br>**修法**：搜索栏继续跨流媒体与全部 U 盘聚合检索，各流媒体自身的歌单列表严格按当前平台隔离。 | `go test -race ./pkg/providers/... ./pkg/webui/...` |
| **#118**（一代机镜像嵌入定制开机第一屏） | `tools/az01-packer/build-release.sh` | **原因**：`build-release.sh` 打包一代机 `.img` 时漏传 `--splash`，导致刷入后退回原厂开机第一屏。<br>**修法**：在 `build-release.sh` 中传入 `--splash "$REPO/docs/assets/boot-splash-enginex.png"`，并修正发布脚本资产通配符仅选择 20 个公开产物。 | `test_az01_packer.py`、镜像解包校验 |
| **#134**（独立 DMX 剥离为 `dmx-studio` Mod） | `docs/mods/src/dmx-studio/`<br>`tools/mods/build_official_mods.py` | **原因**：核心内置重型 DMX 协议栈增加主二进制体积与常驻复杂度。<br>**修法**：从核心剥离内置 DMX，重构为按需启用的官方开源 Mod `dmx-studio`（支持 Art-Net / sACN / FTDI / uDMX，未启用时 0 内存与 0 后台进程）。 | `docs/mods/test_mods.py`、`build_official_mods.py --check` |
| **#135 / #157**（Auto Loop 旋钮联动、跳拍互换与播放防误触） | `tools/ui-mod/build_packs.py` | **原因**：① JP07 开启 `swap_loop_order` 时 `AutoLoopKnob.qml` 的 `beatJumpEnabled` 判定反向；② 按维护者要求回退 `#147` 10 级几何缩放（`a600428` 恢复原厂 `ZOOM` 与 `Browse` 旋钮行为）时一并撤掉了 `#157` 的转盘网格编辑防误触。<br>**修法**：① 旋转 Auto Loop 旋钮调整 Loop 大小时自动开启 `LinkBeatJumpLoopSize` 联动 Beat Jump，并修复 `swap_loop_order` 判定；② 在保持原厂 `ZOOM` / `Browse` 旋钮与 `DeckAction.toggleGridEdit` 不变的前提下，单独恢复 `Vinyl.qml`（播放中按 `Shift + Vinyl` 不进入 BeatGrid Edit）与 `JogWheel.qml`（BeatGrid Edit 激活时禁用 `Jog Touch`）防护（`42aa60a`）。 | `test_ui_mod.py`、`test_rootfs_overlay.sh`、双 SC5000 实机核验 |
| **#137**（通用 USB MIDI 控制器映射器 Mod `midi-controller-mapper`） | `docs/mods/src/midi-controller-mapper/`<br>`tools/ui-mod/build_packs.py` | **原因**：四轨模式下操作内部 `Deck 3 / 4` 需要频繁切换硬件 `Layer`，外接 Pioneer CDJ-400（USB `PC MODE`）等控制器缺少免切层直控桥接。<br>**修法**：新增可选官方 Mod `midi-controller-mapper`（内置 Pioneer CDJ-400 预设并支持自定义 JSON 预设，严格按预设 `patterns`/`usbIds` 匹配设备），未启用时零后台轮询；启用后 helper 以 `os.nice(19)` 按需 60 Hz 桥接至 QML StateMap（沙箱实测桥接延迟 `p50 ≈ 1.6 ms`），支持 CDJ-400 连续速度转盘积分（`Bn 10`，`64=停止`）、使用原厂 `HARDEXTERNAL (1)` 的变速推子软接管与屏幕 `▲▼`（`SpeedOffsetUp` / `SpeedOffsetDown` / `SpeedNeutral`）指示，以及 USB 断连 / MIDI 风暴自动停用保护。 | `docs/mods/test_mods.py`、`pkg/mods/mods_test.go`、`test_ui_mod.py` |
| **#150**（SoundCloud 点赞回退与 Spotify Connect 双网卡 mDNS） | `pkg/providers/soundcloud.go`<br>`pkg/providers/spotify_connect.go`<br>`rootfs-overlay/etc/avahi/` | **原因**：① SoundCloud OAuth `refresh_token` 过期返回 `HTTP 400` 时无法加载点赞歌单；② 无线/有线双网卡下 Spotify Connect 局域网发现不稳定，且 OAuth 登录后曲目下载偶发 `410`。<br>**修法**：① OAuth 刷新 `HTTP 400` 时自动回退至 App Token 与公开主页接口加载 `Liked Tracks & Sets`；② 内置 Avahi `_spotify-connect._tcp` 与 `stream-sync` 纯 Go `_spotify-connect._tcp.local.` mDNS 响应器（覆盖 `eth0` + `wlan0`）及标准 768-bit DH 公钥，并修复 OAuth 后下载 `410`。 | `go test -race ./pkg/providers/...`、`test_rootfs_overlay.sh` |
| **#161 / #164**（SC LIVE 4 / Mixstream Pro+ / Pro Go 与一代机安装器发现及线刷） | `cmd/enginex-installer/`<br>`tools/az01-packer/az01_packer.py` | **原因**：① Windows `pnputil` 多行输出导致 SC LIVE 4 识别失败；② Mixstream Pro+（NH08S）仅发 UDP 11224 EAAS 广播且与运行中 Engine DJ 争抢端口，USB ID `15e4:103f`/`303f` 及 Mixstream Pro Go `15e4:2059` 未纳入发现表；③ 一代机在 Computer Update Mode 下对 `fastboot getvar product` 返回 `Variable not implemented`，且 Windows/macOS 无 `debugfs` 时 `az01_packer.py unpack` 拒绝解包。<br>**修法**：① 修复 `pnputil` 解析并完善 Zadig / Fastboot GUID 指引；② 支持 `SO_REUSEADDR`/`SO_REUSEPORT` 严格 6 字节 EAAS 候选发现（未验证候选不发 SSH 凭据），兼容 `fastboot 35+`，纳入 NH08S 与 NH10 USB ID，当 `/v1/sc5000/ui` 返回 401/403 时通过 `<title>Stream Sync</title>` 识别已装 EngineX 的碟机，并补齐 Windows WSL 2 + `usbipd-win`（`wsl -u root`）刷机指引；③ 一代机无 `getvar product` 时回退按官方镜像头 USB ID 表识别，并将 `debugfs` 检查移至真正调用它的 `ExtFS.cmd()`。 | `go test -race ./cmd/enginex-installer/...`、`test_az01_packer.py`；报告人在 Mixstream Pro+（NH08S）真机经 WSL + `usbipd-win` 完整刷入验证通过 |
| **#166**（Mixstream Pro / Pro+ / Pro Go 实体 FILTER 旋钮 Sweep FX 与触控跳拍） | `tools/ui-mod/build_packs.py`<br>`pkg/webui/ui_settings.go` | **原因**：Mixstream 系列实体 FILTER 旋钮未绑定原厂 Sweep FX 调制，且缺少触屏 Beat Jump 与独立打击垫跳拍组。<br>**修法**：恢复实体 FILTER 旋钮调制原厂 Filter / Dub Echo / Noise / Filter Roll / Wash（点击各轨 Pads 栏 `SWEEP FX` 打开原厂选择轮盘），恢复触屏 Beat Jump，并在 `Layout → View` 对 Mixstream 系列开放 `Beat Jump Pad Bank`（后跳、前跳、减半、加倍）及 `EQ / STEMS` 切换。 | `test_ui_mod.py`、`tools/az01-packer/mixstream-sandbox.sh` |
| **#168**（PRIME 4 原生硬件 FX 选择器 PX 效果与独立 Time/Rate） | `tools/ui-mod/engine_ui_hook.c`<br>`tools/ui-mod/fx_selector.h`<br>`tools/ui-mod/pioneer_fx.h`<br>`tools/ui-mod/build_packs.py` | **原因**：JC11 PRIME 4 原生硬件 FX 选择器需按单元与分配通道接入 5 个 PX 效果、消除切效/干湿爆音，并接入独立 `Time/Rate` 控制。<br>**修法**：在 JC11 原生 FX 选择器末尾加入 `Spiral (PX)`、`Pitch Echo (PX)`、`Slip Roll (PX)`、`Dub Echo (PX)`、`Space (PX)`，采用 `DIRTY/PREPARING/READY/LIVE` 延迟库状态机与干湿淡入防爆音；hook 原生 Flanger `Rate` 事件分发器（`0xe6a114`，索引 `0–10` 共 11 档，通过 `BEAT −/+` 或触控 Time/Rate 触发），映射为 `index / 10` 独立控制 PX 时间档位（**PX 采用相对档位，不按拍同步**；屏幕/OLED 拍数标签保持原厂显示，仅指示所选相对档位，不代表延迟拍数），`Parameter` 独立控制音色与反馈，`Wet/Dry` 控制干湿比。 | `tools/ui-mod/test_flanger_scope.c`、`tools/ui-mod/test_pioneer_selector.c`、`tools/az01-packer/test_fx_selector_native.c`、`tools/ui-mod/validate_fx_selector.sh` |
| **#169**（PRIME 4+ JC11S 安装激活校验与只读诊断） | `cmd/enginex-installer/`<br>`tools/az01-packer/test_sclive4_bootstrap.sh`<br>`tools/diagnostics/` | **原因**：需确保 PRIME 4+（JC11S）数据包刷入后目标分区挂载点、激活脚本与 ALSA 目录完整无误，并提供免修改排障工具。<br>**修法**：强化安装器与 bootstrap 对 JC11S 数据目录及激活文件的校验（保留旧 `sc5000-custom` 兼容），新增跨平台一键只读诊断脚本与 Pro DJ Link 抓包工具。 | `JC11S_ROOTFS` `test_sclive4_bootstrap.sh`、`cmd/enginex-installer` 单测 |
| **#170**（Mod 压缩包保留可执行权限位） | `pkg/mods/mods.go`<br>`pkg/mods/mods_test.go` | **原因**：Web 控制台解压安装 `.tar.gz` Mod 时统一用 `0644` 写入文件，导致自带 ELF 二进制或辅助脚本的 Mod 丢失可执行权限。<br>**修法**：解压时保留 tar header 中的可执行权限位（`mode & 0111`），同时严格剥离 `setuid`/`setgid`/`sticky` 位。 | `TestInstallPreservesExecutableBits`（`pkg/mods/mods_test.go`） |
| **#171**（SC LIVE 4 AUX 路由至通道 3/4 并保留 MIC2） | `tools/ui-mod/engine_ui_hook.c`<br>`tools/ui-mod/build_packs.py`<br>`docs/issue171-sclive4-aux.md` | **原因**：SC LIVE 4（JP21）原厂 AUX 输入直通主输出，无独立通道 CUE、推子、EQ 与 Sweep FX 控制。<br>**修法**：通过原厂 `Mixer Menu → Channels → Input` 将 AUX 路由至通道 3 或 4，支持通道 CUE、推子、EQ 与 Sweep FX；仅在 AUX 模式且任一通道选 Line-In 时抑制 AUX 直通，MIC2 保持原厂路由；JP21 隐藏未验证的 Bluetooth/Computer 选项。 | `tools/az01-packer/sclive-aux-sandbox.sh`、`test_stems_feature.c`、`test_ui_mod.py` |
| **#175**（UI Scale 缩放下开关机画面全屏铺满） | `tools/ui-mod/build_packs.py` | **原因**：开启 `UI Scale`（如 `75%`）扩大逻辑画布后，`SplashScreen` 与关机遮罩未锚定父容器全屏，在 SC LIVE 4 等机型上仅占屏幕左上约 3/4。<br>**修法**：为开机启动画面与关机遮罩补齐 `parent` 与 `anchors.fill: parent`，在 `75%`–`110%` 缩放下均完整铺满屏幕。 | `test_ui_mod.py`、eglfs 渲染核验 |
| **#182 / PR #183**（旧版 rekordbox 5 Device Library `export.pdb` U 盘兼容） | `pkg/rekordbox/pdb.go`<br>`pkg/rekordbox/library.go`<br>`pkg/rekordbox/sqlite.go`<br>`pkg/prolink/engine_bridge.go` | **原因**：旧版 rekordbox 5 Device Library 导出的 `export.pdb` 中存在未分析曲目 BPM（`0xFFFFFFFF`）、指向不存在曲目的孤立歌单条目、同名歌单及断链父目录，导致导入报错中断。<br>**修法**：在解析边界将未分析 BPM（`0xFFFFFFFF`）记为 `0`，忽略孤立的歌单条目，兼容同名歌单与断链父文件夹，并在共享 PDB writer 边界保持校验。 | `go test -race ./pkg/rekordbox/... ./pkg/enginedb/... ./pkg/prolink/...` |
| **详细日志与脱敏下载** | `pkg/webui/logs.go`<br>`pkg/webui/ui_settings.go`<br>`pkg/webui/index.html`<br>`rootfs-overlay/etc/systemd/system/engine.service.d/60-ui-hook.conf` | **原因**：现场用户遇到问题时不便开启 SSH 抓日志，且全量 `*.debug=true` 会产生海量 Qt 内部渲染日志。<br>**修法**：为一代与二代 Engine 服务配置 `QT_LOGGING_CONF=/data/logging.ini`；Web 控制台新增 `详细日志` 开关（开启时原子写入 `*.debug=true` + `qt.*.debug=false` 并重启 Engine，关闭时删除文件）与 `下载日志` 按钮（导出最近 30 分钟、最多 5,000 条 / 2 MiB 的已脱敏纯文本日志，自动遮盖令牌、ARL、密码、Cookie、签名 URL 与本机访问码）。 | `go test -race ./pkg/webui/...`（`logs_test.go`）、`test_rootfs_overlay.sh`、armhf 与 AZ04 沙箱实测 |
| **流媒体缓存隔离、Beatport 冷缓存与性能调优** | `pkg/providers/`<br>`pkg/deemixclient/`<br>`pkg/webui/`<br>`rootfs-overlay/usr/local/bin/sc5000-perf-tune`<br>`tools/ui-mod/build_packs.py` | **原因**：① 跨平台同名曲目缓存需按音源隔离，Beatport 冷缓存下无损升级需保持命中；② 搜索并发过高或后台自动批量分析会争抢线程，QML 2 轨启动时 `scGcCheck` 在 Deck 1 未就绪前访问空属性，且 `sc5000-perf-tune` 30 秒轮询每个线程 fork 子进程带来偶发帧延迟。<br>**修法**：① 独立各平台下载缓存命名空间（网易云「Deemix 优先」与 Deezer 共享无损缓存），修复 Beatport 冷缓存无损升级，并在 `stream-sync` 已监听 6595 时让 `deemixd` 以状态码 `0` 退出；② 限制并发搜索工作线程、挂起不安全的自动批量盘分析、修复 `scGcCheck` 等待 Deck 1 构建完成并加空属性保护、将 `sc5000-perf-tune` 改为纯 shell 零 fork 扫描。 | `go test -race ./...`、`test_rootfs_overlay.sh`、`test_ui_mod.py` |

> **范围说明**：`#162`（Beat FX 毫秒模式切换）、`#172`（高频滚降补偿）、`#181`（CDJ 风格变速推子软接管解锁）、`#168` 的按拍精确同步，以及原厂 `Sync` 要求超过 `+100%` 变速时影子副本卡在 `+100%` 的跨倍速扩展推迟至 `5.3.0.5`，本次不列入新功能；`#179` 因不合规（未经同意转移数据）已拒绝，不列入。

---

## 2. 实机验收、沙箱录像与待实机验证项目清单

- **正式 RC `5038e5b` 双 SC5000 实机冒烟（R1–R7 全部 PASS）**：
  - **R1（镜像与内建 Hook 核验）**：两台 SC5000 刷入正式 RC 镜像（SHA-256 `5af20f62b4a735dbe68c46a77aebd795dbeefa44c060eb3522cdf51eaedc49d4`），根分区只读，无热挂载，镜像内两份 `libengine-ui-hook.so` SHA-256 均为 `daf0b82d78a0836901c13ff362bc2afdde14b3b20715c0079603832da147e5a3`；About 显示 `EngineX OS 5.3.0.4`，SSH、`Engine`、`stream-sync` 均正常（**PASS**）；
  - **R2（双向四轨）**：`L1/L3` 与 `R2/R4` 源轨与对端副本全部载入并播放，原厂 `Engine` 波形滚动，曲目长度、标题及歌手一致（**PASS**）；
  - **R3（常规 +3% Sync 漂移）**：四槽连续采样约 `99 s`，`LEAD` / `SYNC` 状态正确，绝对漂移 `p95 30.5–36.7 ms`（首尾中位差最大 `8.2 ms`，`0` 新增 seek，**PASS**）；
  - **R4（MASTER 暂停与卸载）**：`L1/R2/L3/R4` 共 `10` 次（`6` 次暂停、`4` 次播放中卸载），影子副本抢 `MASTER` `0/10` 次、旧夺回 `0/10` 次（**PASS**）；
  - **R5（`Hide Title/Artist`）**：本机和影子 header 开启时歌名与歌手均为空，关闭后恢复原值（**PASS**）；
  - **R6（流媒体原厂 `LOAD`）**：使用原厂 `LOAD` 播放 Deezer 前进 `+9.95 s`、SoundCloud 前进 `+9.42 s`（**PASS**）；
  - **R7（空闲后起播与 GC 间隔）**：起播 `86.8 ms`，空闲 GC 间隔 `232.3 s`（约 `232 s`，**PASS**）。
- **前序双 SC5000 实机全项验收（基于 `24bb83b` 双机 A1–A11）**：
  - **A1–A6、A8–A10**：**PASS**（其中 A8 流媒体载入 `LN109299` 已查明为验收脚本绕过原厂 `LOAD` 直接调底层接口所致，按原厂加载路径复测 **PASS**）；
  - **A11**：由 `f6b87f8` 修复（四轨模式下 `20/20` 补丁生效，排除本机静音影子槽参与 `MASTER` 选举，并由正式 RC `R4` `0/10` 冒烟确认）；
  - **A7（实体按键灯亮度与单色按键透传）**：待维护者目测确认。
- **已知限制（推迟至 `5.3.0.5`）**：
  - 当 `Sync` 需要超过 `+100%` 变速时（如 `71 BPM` 同步到 `144.2 BPM`），影子副本速度卡在 `+100%` 上限（实际约 `142.0 BPM`），偏差 `p95 126 ms`（`126.4 ms`）。
- **双机沙箱正式版录像验收（`shadow-twin` `fa697d4` / RC `5038e5b`，S1–S8 全部 PASS）**：
  - **S1（`s1-four-deck-shadow-fa697d4.mp4`，`25.1s`）**：A 机 Deck 1 载入播放，B 机影子 Deck 1 波形、顶栏（歌名、BPM、调性、时间）与缩略条同原厂同步一致（**PASS**）；
  - **S2（`s2-pitch-sync-bar-align-fa697d4.mp4`，`25.0s`）**：变速 `+3%` 后 `Sync`，`LEAD/SYNC` 指示正确，四条 lane 小节红线纵向逐像素对齐同步滚动（**PASS**）；
  - **S3（`s3-auto-mode-engine-and-cdj-fa697d4.mp4`，`30.6s`）**：`Auto` 模式 Engine 路径顶栏 `LEAD` 同步显示，CDJ 来源走 `display-only` 原厂样式顶栏 + `REMOTE` 缩略条 + 槽内回退提示（**PASS**）；
  - **S4（`s4-hotcue-overview-timeflip-fa697d4.mp4`，`25.4s`）**：原厂样式 Hot Cue 上下三角与黑底白字标签、缩略条底部 cue 图标，点击时间区双向切换 Elapsed/Remaining 且 `timeBar` 同步翻转（**PASS**）；
  - **S5（`s5-hide-title-display-filename-fa697d4.mp4`，`25.6s`）**：开启 `Hide Title/Artist` 时全部 8 个本机与影子顶栏均隐藏标题与歌手为空，关闭后正常切换标题与文件名（**PASS**）；
  - **S6（`s6-decktabs-layer-switch-fa697d4.mp4`，`25.4s`）**：曲库底部 `DeckTabs` 切换 Layer A/B 时仅当前操控层蓝/绿亮起，非当前本机层熄灭，阴影槽保持 `greyOut`（**PASS**）；
  - **S7（`s7-track-end-warning-fa697d4.mp4`，`25.3s`）**：曲目剩余 `<= 30s` 时本机与阴影缩略条同步闪烁红光告警（**PASS**）；
  - **S8（`s8-continuous-play-2min-nodrift-fa697d4.mp4`，`120.6s`）**：四轨连续播放满 120 秒，A/B 两机本机槽与对端影子槽白色播放头及小节红线全程紧锁零漂移（**PASS**）。
- **尚未在对应其他型号物理实机完成人工验收的项目**（均已在原生 ARM64/armhf 环境通过单元/竞态测试、二进制 hook 校验、`shadow-twin` 双机仿真、eglfs KMS 渲染或三组 `release-gate.sh` 门禁）：
  1. **实体按键灯分级调光（A7 / #85）**：待维护者在真机上目测确认 RGB 彩灯调光与单色按键透传。
  2. **SC LIVE 4 AUX 路由至通道 3/4（#171）**：已在 JP21 沙箱（含 null PCM 与硬件 fixture）验证，待 SC LIVE 4 真机低音量核验通道 CUE、EQ、Sweep FX 与 MIC2。
  3. **PRIME 4 原生 FX 选择器 PX 效果与独立 Time/Rate（#168）**：已在 JC11 沙箱与原生 C 单测验证，待 PRIME 4（JC11）真机核验旋钮/按键手感与实时音频过渡。
  4. **Mixstream Pro / Pro+ / Pro Go 实体 FILTER 旋钮 Sweep FX 与触控跳拍（#166）**：已在 NH08 沙箱验证，待 Mixstream 系列真机实测。
  5. **SC LIVE 4 等机型在非 100% `UI Scale` 下的开关机画面铺满（#175）**：已通过 eglfs 渲染验证，待 JP21 真机目视确认。
  6. **PRIME 4 等一体机 Line In 通道映射 CDJ-3000 波形**：已在沙箱验证，待一体机 + CDJ-3000 真机联调。
  7. **通用 USB MIDI 控制器映射器 Mod（`midi-controller-mapper`，#137）**：已在 5.3.0 沙箱完成端到端虚拟 ALSA MIDI 压测（延迟 `p50 ≈ 1.6 ms`），待外接真实 Pioneer CDJ-400（`PC MODE`）硬件验证。
  8. **Web 控制台详细日志开关与脱敏日志下载**：已在隔离 rootfs 沙箱验证 `QT_LOGGING_CONF` 与 `journalctl` 脱敏导出，待一代机与 AZ04 真机核验重启恢复。
  9. **旧版 rekordbox 5 Device Library `export.pdb` 兼容（#182 / PR #183）**：已通过样本库与边界回归单测，尚未用报告人的真实 U 盘在真机上插盘验证。
  10. **PRIME 4+（JC11S）激活校验（#169）与一代机 / SC LIVE 4 / Mixstream Pro Go 安装器 USB 线刷修复（#161、#164）**：NH08S 已由报告人真机刷入验证；JC11S 仍待报告人回传诊断报告，一代机与 JP21/NH10 更新后的安装器自动线刷流程待实机抽验。

---

## 3. 全套测试与三组发版门禁结果（RC（待维护者确认发布））

- **全量自动化测试（原生 ARM64/armhf，基于提交 `5038e5b`）**：
  - Go 静态检查与竞态单测：`gofmt -l .`（0 差异）、`go vet ./...`（PASS）、`go test -race ./...`（22 个有测试的 Go 包全部 PASS）；
  - 系统 overlay 测试：`bash tests/test_rootfs_overlay.sh` → `PASS: 21 tests (mocked storage and hardware; no privileged operations)`（含仓库提交的两份 32-bit `libengine-ui-hook.so` 与 `libedisksd-hook.so` 同源码现编结果逐字节一致校验）；
  - 二代机沙箱验收：
    - `JC11S_ROOTFS=/var/tmp/integ5304-jc11s/rootfs bash tools/az01-packer/test_sclive4_bootstrap.sh`（官方 `PRIME4PLUS-5.3.0-Update.img` SHA-256 `bf5489d1...`）→ `=== All SCLIVE4 verification checks PASSED ===` 及 `=== All PRIME4PLUS verification checks PASSED ===`（`JC11S stock mkfs (blank/existing) and fscrypt first boot: PASS`，20 处补丁，UI 包加载）；
    - `AZ04=1 bash tools/az01-packer/test_sclive4_bootstrap.sh` → `=== All PRIME4G2 verification checks PASSED ===`（18 处补丁，UI 包加载）；
  - Python 打包器与分轨单测：
    - `python3 -m unittest discover -s tools/az01-packer` → `Ran 207 tests in 57.757s / OK (skipped=1)`（含 `test_ui_mod.py` 全部 103 项 PASS，覆盖 `PatchDict` 防覆盖检查、`ArtistSong.qml` 双线替换合并与空 Engine 槽空闲轮询跳过）；
    - `python3 -m unittest discover -s tools/stems` → `Ran 9 tests in 2.306s / OK`；
  - C 运行时与 DSP / FX 选择器 / 影子轨 MASTER 单测：
    - `tools/ui-mod/test_shadow_master.c`（armhf 原生，针对 5.3.0 `Engine`）→ `shadow MASTER: 144 native play/stop, host, slot and ABI cases; anchor/build/mode guards PASS`；
    - `tools/ui-mod/test_pioneer_selector.c`（plain/asan/tsan PASS）、`tools/ui-mod/test_dsp_smooth.c`（plain/asan/tsan PASS）、`tools/ui-mod/test_flanger_scope.c`（armhf PASS）、`tools/ui-mod/test_master_golden.c`（AZ04/armhf fast/off PASS）；
    - `tools/ui-mod/test_mixstream_hook.c`（armhf 原生，5.3.0 20 处补丁、5.3.0-sclive4 23 处补丁 PASS）、`tools/ui-mod/test_sclive_aux.c`（armhf 原生 PASS）、`tools/ui-mod/test_gen2_img.c`（PASS）；
    - `tools/ui-mod/test_stems_feature.c`（armhf 原生，针对 `5.0.4`、`5.3.0`、`5.3.0-sclive4` 三个 `Engine` 二进制）→ 全部 PASS；
    - `tools/ui-mod/test_az04_hook.c`（AArch64 原生，针对 AZ04 64 位 `Engine`）→ 全部 PASS；
    - `tools/edisksd-hook/test_edisksd_hook.c`（常规与 `unshare -m --mounts`）→ `edisksd-hook: all requested tests passed`。
- **第 4 步镜像抽查与 hook 一致性核对（三组代表镜像）**：
  - `SC5000`（JP07）、`SC6000`（JP13）、`PRIME4`（JC11）分别用 `az01_packer.py unpack` 解包均为 `"verified": true`，`rootfs.ext2` 内 `/etc/sc5000/version.txt` = `5.3.0.4`，`60-ui-hook.conf` 含 `Environment=QT_LOGGING_CONF=/data/logging.ini`，同组机型 `.img` SHA-256 完全一致；
  - `build-release.sh` 在 `hooks32` 编译后将新编 32-bit `libengine-ui-hook.so` 与 `libedisksd-hook.so` 覆盖写入 `$STAGE`：解包 SC5000 组 `rootfs.ext2` 核对 `/etc/sc5000/libengine-ui-hook.so` 与 `/usr/local/lib/sc5000/libengine-ui-hook.so` 的 SHA-256 均为 `daf0b82d78a0836901c13ff362bc2afdde14b3b20715c0079603832da147e5a3`，与源码现编结果及仓库 `rootfs-overlay/` 提交的两份副本完全一致。
- **第 4b 步三组发版门禁（`tools/az01-packer/release-gate.sh`，旧版基准：`/root/build/sc5000-lab/release-5.3.0.3/`，RC（待维护者确认发布））**：
  - **SC5000 组（JP07）**：**93 PASS / 0 FAIL / 5 INFO**（退出码 0；静态审计 58 文件 0 FAIL / 14 WARN，`systemd-analyze verify` 17 单元 0 错误；5.3.0.3 → 5.3.0.4 在线升级与 1 字节篡改拒签通过；970 个原厂旧 `.qmlc` 缓存种入后新 UI 包替换 343+1 文件；默认二轨启动 19 处常驻 Engine 补丁（开启四轨时含 `0x16fbc38` 共 20 处）、21 个 DSP guard、SQLite hook、Stems、Rekordbox、下次 5.3.0.5 在线升级全部通过）；
  - **SC6000 组（JP13）**：**93 PASS / 0 FAIL / 5 INFO**（退出码 0）；
  - **PRIME 4 组（JC11）**：**93 PASS / 0 FAIL / 5 INFO**（退出码 0）。

---

## 4. RC 构建产物与 SHA-256 校验表（RC（待维护者确认发布））

- **构建提交**：`5038e5b`（分支 `agent/integration-5304-20261111`，RC（待维护者确认发布）；`build-release.sh` 公钥参数 `none`，公开镜像不含任何内置 SSH 公钥）
- **产物目录**：`/var/tmp/sc5000-rc5304-5038e5b`（前两版预 RC 目录 `/var/tmp/sc5000-rc5304-pre-24bb83b`、`/var/tmp/sc5000-rc5304-pre-6e1b4be` 按要求保留）
- **回滚点**：`v5.3.0.3`（tag `cd789f8`，构建提交 `b941ea4`）

### 一代机型完整固件（`*-Update.img`）

| 镜像组 | 机型 | 板号 | 文件名 | SHA-256 |
| --- | --- | --- | --- | --- |
| SC5000 | Denon DJ SC5000 Prime | JP07 | `SC5000-5.3.0.4-EngineX-OS-Denon-SC5000-Prime-JP07-Update.img` | `5af20f62b4a735dbe68c46a77aebd795dbeefa44c060eb3522cdf51eaedc49d4` |
| SC5000 | Denon DJ SC5000M Prime | JP08 | `SC5000M-5.3.0.4-EngineX-OS-Denon-SC5000M-Prime-JP08-Update.img` | `5af20f62b4a735dbe68c46a77aebd795dbeefa44c060eb3522cdf51eaedc49d4` |
| SC5000 | Denon DJ PRIME GO | JP11 | `PRIMEGO-5.3.0.4-EngineX-OS-Denon-PRIME-GO-JP11-Update.img` | `5af20f62b4a735dbe68c46a77aebd795dbeefa44c060eb3522cdf51eaedc49d4` |
| SC6000 | Denon DJ SC6000 Prime | JP13 | `SC6000-5.3.0.4-EngineX-OS-Denon-SC6000-Prime-JP13-Update.img` | `6fe0c88162a189e441598a04e6691ca3e0dcfbddd1fb62f251794719fe5b51b6` |
| SC6000 | Denon DJ SC6000M Prime | JP14 | `SC6000M-5.3.0.4-EngineX-OS-Denon-SC6000M-Prime-JP14-Update.img` | `6fe0c88162a189e441598a04e6691ca3e0dcfbddd1fb62f251794719fe5b51b6` |
| PRIME 4 | Denon DJ PRIME 4 | JC11 | `PRIME4-5.3.0.4-EngineX-OS-Denon-PRIME-4-JC11-Update.img` | `ac67eefeb6353b926182882670d3d4f43fd11f5aa6f24c95f38a448c7df635e9` |
| PRIME 4 | Denon DJ PRIME 2 | JC16 | `PRIME2-5.3.0.4-EngineX-OS-Denon-PRIME-2-JC16-Update.img` | `ac67eefeb6353b926182882670d3d4f43fd11f5aa6f24c95f38a448c7df635e9` |
| PRIME 4 | Numark Mixstream Pro | NH08 | `MIXSTREAMPRO-5.3.0.4-EngineX-OS-Numark-Mixstream-Pro-NH08-Update.img` | `ac67eefeb6353b926182882670d3d4f43fd11f5aa6f24c95f38a448c7df635e9` |

### 二代机型 `/data` 数据包（`*-Data-Bundle.tar.gz`，Beta）

| 架构组 | 机型 | 板号 | 文件名 | SHA-256 |
| --- | --- | --- | --- | --- |
| AZ0x (armhf) | Denon DJ SC LIVE 4 | JP21 | `SCLIVE4-5.3.0.4-EngineX-OS-Data-Bundle.tar.gz` | `7799e20ec38e87de666e38a72aa3de4e6c2ea9aef23f015e65eedb2b61902d61` |
| AZ0x (armhf) | Denon DJ SC LIVE 2 | JP20 | `SCLIVE2-5.3.0.4-EngineX-OS-Data-Bundle.tar.gz` | `8d8e36356596e97f75307ded9a284adb741f051f1acbea0d4daefb0ff04cfcb4` |
| AZ0x (armhf) | Denon DJ PRIME 4+ | JC11S | `PRIME4PLUS-5.3.0.4-EngineX-OS-Data-Bundle.tar.gz` | `652b28895e6857627413326ee3cbaf5b45614ef060aeba6d0f8684396b8dc6cd` |
| AZ0x (armhf) | Denon DJ PRIME GO+ | JP11S | `PRIMEGOPLUS-5.3.0.4-EngineX-OS-Data-Bundle.tar.gz` | `684df38190d41b62d8a23dae2ae4ef070e0456cf98020c03480d2c0775783da1` |
| AZ0x (armhf) | Numark Mixstream Pro+ | NH08S | `MIXSTREAMPROPLUS-5.3.0.4-EngineX-OS-Data-Bundle.tar.gz` | `d5aae5100833bb6e27d0d01a921d65b37b4fdf91d3783f786db31ac076a85232` |
| AZ0x (armhf) | Numark Mixstream Pro Go | NH10 | `MIXSTREAMPROGO-5.3.0.4-EngineX-OS-Data-Bundle.tar.gz` | `681b9964b3185618ac246d1cd65aa613ea8705051187f7c9ba2a1948080e0748` |
| AZ04 (aarch64) | Denon DJ PRIME 4 G2 | JP24 | `PRIME4G2-5.3.0.4-EngineX-OS-Data-Bundle.tar.gz` | `3ee7aeba1b0adb90b919db4ea36fbb6a99572a4ff429196c089dda5cf64de540` |
| AZ04 (aarch64) | Rane SYSTEM ONE | RMZ2 | `SYSTEMONE-5.3.0.4-EngineX-OS-Data-Bundle.tar.gz` | `b5af5a1fc18ffc218c163b52029a28ff004c90c8c107a25937b6c5f1807403b7` |

### 跨平台一键安装器（`EngineX-Installer`）

| 平台 | 文件名 | SHA-256 |
| --- | --- | --- |
| Windows 10/11 x64 | `EngineX-Installer-5.3.0.4-windows-amd64.zip` | `0e4d80cd4cb7ea009d2034d1a06c2ed428b7e5f3e9ca9f504baef3d2fd143a23` |
| macOS Apple Silicon (M1–M4) | `EngineX-Installer-5.3.0.4-macos-arm64.zip` | `76bace82c0781d4f45cbe40c30ff619803f9fd1b693f791abe1adfce1754d34b` |
| macOS Intel | `EngineX-Installer-5.3.0.4-macos-amd64.zip` | `ac10b18807cc6f201cb053cd0a0a83da5fc515cd3d19381cce8084bb0a1325c9` |
| Linux x86_64 | `EngineX-Installer-5.3.0.4-linux-amd64.tar.gz` | `36a510a1f74eaa6a32e22ade6e8736dce563f49bd8bdf09cea4c94300b96e2b4` |

---

## 5. 实机影响与升级说明

- **一代机（8 款 `.img`）**：已刷 `5.3.0.3`、`5.3.0.2`、`5.3.0.1.x` 或 `5.0.4.2` 的设备在正式发布后可于 `设置 → 关于 / 更新` 直接收到 `5.3.0.4` 在线升级推送；首次安装可将对应 `.img` 放入 FAT32/exFAT U 盘根目录免电脑直刷，或进入 `Computer Update Mode` 用 `EngineX-Installer-5.3.0.4` 线刷。
- **二代机（8 款 `Data-Bundle`）**：已安装 `5.3.0.2` / `5.3.0.3` 数据包的二代机可在屏幕 `设置 → 关于 / 更新` 一键在线升级至 `5.3.0.4`；首次安装可使用 `EngineX-Installer-5.3.0.4`（Windows 二代机推荐按文档使用 `WSL 2 + usbipd-win` 以 root 运行 Linux 版安装器）。
- **配置与数据保留**：升级保留 `/data/enginex`（及 `/data/sc5000-custom` 兼容软链）下的流媒体登录态、歌单缓存、视图预设、已安装 Mod 与 SSH 公钥。回滚时一代机刷回 `v5.3.0.3` 对应 `.img`，二代机重装 `v5.3.0.3` `Data-Bundle.tar.gz`。
