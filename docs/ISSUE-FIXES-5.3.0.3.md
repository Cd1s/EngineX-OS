# EngineX OS 5.3.0.3 发版与 Issue 修复记录（2026-11-07）

- **发布日期**：`2026-11-07`
- **版本范围**：`v5.3.0.2`（`e7622ac`）→ `v5.3.0.3`（构建提交：`38d5e68`；2026-11-08 同版本重发加入 #133，首次构建提交 `5b4687e`）
- **覆盖机型与产物（共 20 个发布资产 + `SHA256SUMS.txt`）**：
  - **8 款一代机完整固件（`*-Update.img`）**：`SC5000`（JP07）、`SC5000M`（JP08）、`PRIME GO`（JP11）、`SC6000`（JP13）、`SC6000M`（JP14）、`PRIME 4`（JC11）、`PRIME 2`（JC16）、`Mixstream Pro`（NH08）
  - **8 款二代机 `/data` 数据包（`*-Data-Bundle.tar.gz`，Beta）**：`SC LIVE 4`（JP21）、`SC LIVE 2`（JP20）、`PRIME 4+`（JC11S）、`PRIME GO+`（JP11S）、`Mixstream Pro+`（NH08S）、`Mixstream Pro Go`（NH10）、`PRIME 4 G2`（JP24，64-bit AZ04）、`Rane SYSTEM ONE`（RMZ2，64-bit AZ04）
  - **4 平台桌面一键安装器（`EngineX-Installer-5.3.0.3-*`）**：Windows 10/11 x64（`.zip`）、macOS Apple Silicon M1–M4（`.zip`）、macOS Intel（`.zip`）、Linux x86_64（`.tar.gz`）
- **在线文档与着陆页**：`docs/index.html` + `docs/site/{content.js,app.js,style.css}`（简中 / 繁中 / English 三语文档站，同步更新 23 张 5.3.0.3 实机/EGLFS 渲染截图）

---

## 1. Issue 与 PR 修复总表（#80–#133）

| Issue / PR | 模块 | 改动与修复摘要 | 验证与回归测试 |
| --- | --- | --- | --- |
| **#80** | `tools/ui-mod/build_packs.py`<br>`pkg/webui/ui_settings.go` | **DamskoKill QoL 套件**：① 曲库浏览支持双列并排显示（`Dual-Column Browser`：常驻同时显示 `BPM · 第二字段`）；② 新增 `Sync Guard Tolerance`（`Off / 10% / 15% / 20%`）防止跨大 BPM 误按 Sync；③ 新增 `Beat Jump Pad Bank` 与 `Swap Loop / Beat Jump` 顺序切换 | `TestUISettingsQoLAndFX`、QML 构建校验、实机设置页核验 |
| **#81** | `pkg/webui/bluetooth.go`<br>`pkg/webui/bluetooth_test.go` | **PRIME 4 蓝牙音频方向拆分与配对稳定性**：明确区分 Bluetooth Audio In（A2DP Sink，手机推流进混音通道）与 Bluetooth Audio Out（A2DP Source，碟机推流至蓝牙音箱/耳机），增加 BlueZ 重连退避与状态回显 | `TestBluetoothStatusAndModeSwitch` |
| **#82** | `pkg/webui/nas.go`<br>`pkg/webui/omnisource.go` | **自建音乐服务器原生集成（WebDAV / Subsonic / Navidrome / Plex）**：在 `stream-sync` 与 OmniSource 中支持挂载和浏览私有 NAS 音乐库，歌单与搜索结果直接汇入流媒体浏览器并以本地流直读播放 | `TestNASProvidersAndOmniSource` |
| **#83** | `pkg/broadcast/broadcast.go`<br>`pkg/broadcast/hub.go`<br>`pkg/webui/broadcast.go` | **局域网超低延迟无损音频串流（LAN RTP / Teleport / HTTP PCM-WAV）**：新增零配置局域网无损音频输出，支持 OBS Studio / 直播电脑通过 HTTP `/live.wav`、`/live.mp3` 或 LAN RTP 低延迟采集母带输出 | `TestBroadcastHubAndLANStream` |
| **#84** | `tools/ui-mod/build_packs.py`<br>（`RecMainView.qml`、`TrackList.qml`） | **解除流媒体加载时的内录限制**：覆写 `RecMainView.qml` 的 `isBlockedByStreaming` 与 `TrackList.qml` 的 `StreamingTrackFlags.BlocksRecording` 检查，允许流媒体曲目与本地曲目混音时使用内置录音机录制到 U 盘/SD 卡 | UI 包构建断言、沙箱录音状态检查 |
| **#85** | `pkg/webui/hotcues.go`<br>`tools/ui-mod/build_packs.py` | **单机 Hot Cue 颜色/名称编辑器、Pad LED 调光与 Stems EQ 模式**：Web 控制台支持直接修改曲目 8 个 Hot Cue 的名称与 RGB 颜色；`设置 → 设备 → Pad 灯亮度` 支持 100%–20% 五档调节；`设置 → 播放` 新增 `Stems EQ Mode` | `TestHotCuesEditor`、实机软硬件 Pad 调光验证 |
| **#86** | `pkg/webui/update.go` | **二代机 `/data` 数据包屏幕一键 OTA 升级**（已于 `v5.3.0.2` 重发先行上线，包含于 `v5.3.0.3`）：二代机在 `设置 → 关于 / 更新` 自动匹配 `<PREFIX>-*-Data-Bundle.tar.gz`，校验 `SHA256SUMS` 后原子替换 `/data/enginex` 并热重启服务 | `TestUpdatePicksTheDataBundleOnSecondGeneration`、`TestUpdateAppliesTheDataBundle` |
| **#87** | `pkg/webui/mods.go`<br>`rootfs-overlay/etc/sc5000/engine-ui-guard` | **外置 U 盘 `/mods` 即插即用加载器与安全隔离**：支持从挂载介质的 `/mods` 或 `/EngineX/mods` 加载自定义 QML/脚本扩展，加载前校验语法与路径边界；若触发 Engine 连续崩溃，`engine-ui-guard` 自动隔离外置 mod 并退回安全模式 | `TestModsLoaderAndQuarantine`、`test_rootfs_overlay.sh` |
| **#88** | `tools/ui-mod/pioneer_fx.h`<br>`tools/ui-mod/engine_ui_hook.c`<br>`pkg/webui/ui_settings.go` | **先锋 DJM 风格 Beat FX & Sound Color FX 实时 DSP 模拟**：在 `hook_master_output` 96 kHz 音频回调中实现 `Spiral`、`Pitch Echo`、`Slip Roll`、`Dub Echo`、`Space` 五种算法（默认 `Off`），零堆分配、带有限值防爆音保护，通过 `设置 → 播放 → Color FX Style` 切换 | `test_dsp_smooth`（含 ASan/UBSan/TSan）、`test_stems_feature` |
| **#89** | `cmd/enginex-installer/main.go` | **跨平台零配置一键安装器 `EngineX-Installer`**：自动通过局域网 StageLinQ / Web 控制台发现或 USB Fastboot 识别全系 16 款一代与二代机型，自动下载对应 `.img` 或 `Data-Bundle.tar.gz` 并完成安装 | `cmd/enginex-installer` 全套单测 |
| **#90** | `tools/ui-mod/build_packs.py`<br>（`EmbeddedMain.qml`、`DeckColor.qml`） | **修复四轨视图缺 Layer B 与 Deck 3/4 黑色/白色灯条问题**：开启四轨模式后强制确保 `/GUI/ShowLayerB = true`；修复 `DeckColor.qml` 在 `PlayerColor3/4` 为纯黑 `#ff000000` 或纯白 `#ffffffff` 时无渐变显示黑条的问题，自动继承 `PlayerColor1..4` 或同侧轨道颜色 | 双 SC5000 实机四轨视图核验 |
| **#91** | `tools/ui-mod/build_packs.py` | **修复四轨模式下 SC5000 / SC6000 转盘中心屏卡在 Engine Logo**：修正四轨布局下转盘小屏的轨道绑定与封面/光圈状态同步，正常显示当前层封面与播放进度环 | 双 SC5000 实机核验 |
| **#92** | `pkg/enginedb/artwork.go`<br>`pkg/enginedb/sync.go` | **修复 Engine 5.3.0 局域网 Engine Link 封面缺失**：逆向确认 Engine 5.3.0 要求 `AlbumArt.hash` 为 20 字节原始 SHA-1 BLOB 且同时在 `Engine Library/Database2/Artwork/<base64url_sha1>.jpg` 落盘缩略图文件；导入与同步时自动生成双份数据 | `TestExtractEmbeddedArtwork`、双 SC5000 局域网联调核验 |
| **#93** | `pkg/webui/shadow.go`<br>`tools/ui-mod/build_packs.py` | **修复影子轨道 8 秒循环重载、`(USB N)` 路径后缀解析失败与 Pitch 追赶抖动**：剥离 Engine Link 路径中的 ` (USB N)` / ` (SD)` 设备后缀；过滤 `/data/inMusicTempFiles` 临时路径重触发；播放中影子轨不再高频微调虚拟 Pitch 推子（消除变调器重算引起的波形抖动），直接锁定源机 Pitch 靠晶振同步运行 | `TestShadowTrackPathAndPitch`、双 SC5000 实机核验 |
| **#94** | `pkg/webui/shadow.go`<br>`tools/ui-mod/build_packs.py` | **影子轨道 `<20 ms` 零延迟同步状态机**：源机每 20 ms 采样并在 Play / Pause / CUE 按住试听（`CueLEDState`） / Hot Cue 跳点 / Pitch 变化边沿立即 POST，Go 后端通过 UDP `:50199` 单播推送对端并瞬间唤醒 `/v1/sc5000/shadow?ver=N` 长轮询，同时直连 Engine Link `/RemoteState/<uuid>/.../PlayState` 边沿信号 | `TestShadowUDPPushAndLongPoll`、双 SC5000 实机 `<20 ms` 响应验证 |
| **#95** | `tools/ui-mod/engine_ui_hook.c`<br>`pkg/webui/shadow.go`<br>`tools/ui-mod/build_packs.py` | **影子轨道零重算（`OfflineAnalyzer` `EXPF` 快速通道）与拍线/缩略波形精确同步**：源机提取 `m.db` / `stm.db` 的 `PerformanceData`（`trackData`、`beatData`、`overviewWaveFormData`）随音频写为 `.perf`（`EXPF v1`）；影子机 `OfflineAnalyzer` 拦截 `BeatDetectionRunner::process` 直接发射源机波形与拍线（耗时约 `92 µs`，跳过 15 秒解码分析），且绝不从外部进程写 Engine 独占锁定的 `m.db`；暂停时自动用 `OffsetPhaseForward/Backward` 对齐整拍相位 | `test_stems_feature`（`EXPF` 解析与信号发射单测）、`TestShadowPerfBundle`、双 SC5000 实机核验 |
| **#96** | `pkg/prolink/export.go`<br>`pkg/webui/shadow.go` | **先锋 CDJ-3000 影子桥接重构**：`observe()` 实时捕获 CDJ `Playing` 边沿、`TrackID` 切换与 `0x0b` 绝对位置跳变（`Jumps++`），立即唤醒长轮询；支持将 Rekordbox ANLZ 的 `PQTZ` 拍线与波形转换注入影子轨 `.perf` | `TestProlinkShadowWakeupAndJumps`、OpenProLink 虚拟 CDJ-3000 联调 |
| **#97** | `tools/ui-mod/build_packs.py`<br>`pkg/webui/shadow.go` | **影子轨道同步显示源机 8 个 Hot Cue 与主 CUE 标记**：源机上报 `cuePos` 与 8 个 `cues`（`{i, p, n, c}`），影子轨通过 Engine 原生 `QuickCueNSetPosition`、`QuickCueNName`、`QuickCueNColor` 与 `CuePosition` 属性实时渲染在主波形、缩略图与打击垫框上 | `TestShadowHotCuesSync`、双 SC5000 实机核验 |
| **#98** | `tools/ui-mod/build_packs.py`<br>（`SortMenu.qml`）<br>`pkg/webui/tidal_gateway.go` | **解锁全部流媒体歌单按 BPM、调性（Key）、星级（Rating）、标题、艺人升降序排序**：解除 `SortMenu.qml` 对 `streamingInterfaceController.hasSortMenu` 的限制，并在本地网关与 QML 端持久化 `streamSortRole` / `streamSortOrder` | `TestTidalGatewaySort`、实机流媒体列表排序验证 |
| **#99** | `pkg/webui/views.go`<br>`tools/ui-mod/build_packs.py`<br>（`LayoutViewTab.qml`、`LayoutManagerTab.qml`） | **视图管理器扩展至 `View 1–5`、新增 `Save View`、重启持久化且默认关闭屏幕 Pads**：新增 `GET/POST /v1/sc5000/views`（`views.json`），每个预设视图同时保存原厂布局（台面数、波形数、横竖屏、Library/FX/Pads 开关）与 EngineX 专属预设（波形样式、转盘灯效、UI Scale、双列浏览等）；未保存过的视图默认关闭屏幕虚拟 Pads 以留出更大波形空间 | `TestViewsRoundTrip`、实机与沙箱渲染验证 |
| **#100** | `pkg/prolink/engine_bridge.go`<br>`pkg/prolink/nfs_server.go`<br>`pkg/prolink/export.go` | **将碟机流媒体缓存曲目与歌单通过 Pro DJ Link（dbserver + NFS）共享给先锋 CDJ-3000**：自动把已缓存的流媒体音频汇入虚拟 Rekordbox 导出库与内置 NFSv2 服务，供同局域网的 CDJ-3000 浏览与加载播放 | `TestEngineBridgeStreamingExport`、`TestNFSServerStreamingPaths` |
| **#101** | `pkg/enginedb/performance.go`<br>`pkg/dbhealer/performance.go` | **修复 Rekordbox U 盘导入曲目波形偏矮问题**：反编译 Engine 5.3.0 `trackData` 解析器（`0x17cb3e4`）确认 68 字节 `trackData` 尾部 `vals[3..5]` 为三频段峰值（全 0 时 Engine 使用未归一化默认高度）；在 `rb-bridge` 写入 `PerformanceData` 时精确填入三频段峰值，导入即获得与本机分析一致的饱满波形高度 | `TestPerformanceDataPeaks` |
| **#102** | `docs/index.html`<br>`docs/site/{content.js,app.js,style.css}`<br>`docs/manual/` | **上线简/繁/英三语官方文档站与着陆页（`enginexdj.com`）**：内置全文检索、深浅色主题、交互式界面菜单地图、机型刷机向导、23 张真机/EGLFS 实测截图与常见问题排查 | 静态资源完整性与三语键值对齐检查 |
| **#103** | `tools/az01-packer/az01_packer.py`<br>`tools/az01-packer/build-data-bundle.py`<br>`rootfs-overlay/` | **运行时目录迁移至 `/data/enginex` 并保持 `/data/sc5000-custom` 符号链接向后兼容**：开机自动完成零拷贝迁移并建立相对路径符号链接 `sc5000-custom -> enginex`，一代 `.img` 与二代 `Data-Bundle` 全部统一 | `test_rootfs_overlay.sh`、`test_az01_packer.py`、`test_data_bundle.py` |
| **#104** | `tools/ui-mod/echo_out.h`<br>`tools/ui-mod/engine_ui_hook.c`<br>`tools/ui-mod/build_packs.py` | **用户实机反馈集中打磨（`@djkavika`）**：① 在 `echo_out.h` 实现真正的推子后（Post-Fader）C 音频线程环形缓冲回音尾音（音量骤降 >12 dB 至静音时自动触发尾音淡出），并在 `Fader Echo` 磁贴与混音器设置中支持 `Off / All Faders / Upfader Only` 三态切换（绑定 `PlanckGUI.views.transitionFx.setTriggerSource`）；② Smart Sync 升级为支持半速/倍速（`0.5x / 1x / 2x / 0.25x / 4x`）谐波 BPM 匹配，自动扩宽 Pitch Range 并在关闭 Sync 时恢复原档位；③ 修复流媒体曲目左滑触发 Stems 分轨时的入库 trackId 查找（新增 `/v1/sc5000/stems/track-id`）与隐藏原厂 `StemsDownload.qml` 覆盖入口；④ 语言切换改为实时动态字典绑定（`Math.scI18n`），切换英/简/繁无需重启 Engine | `test_dsp_smooth`、`TestStemsTrackID`、实机验证 |
| **#105** | `pkg/providers/spotify_connect.go`<br>`pkg/providers/spotify.go`<br>`pkg/deemixclient/fetch.go` | **Spotify 双模登录（Spotify Connect 局域网零配置配对 + 手机扫码）与下载容灾回退**：支持局域网 Spotify App 直接点选 `EngineX OS` 配对登录，严格隔离各服务凭据；当原生音频流拉取受阻时自动回退至 Deemix ISRC 无损编目下载 | `TestSpotifyConnectAndFallback` |
| **#106** | `pkg/webui/phone_pages.go`<br>`pkg/webui/ui_login.go` | **Deezer 独立用户 ARL 扫码登录与本地 `deemixd` 自动同步**：`/m/deezer` 手机扫码页顶部提供双语 ARL 指引与输入框，保存后写入 `state/deezer-owner.json` 并立即同步激活本机 `sc5000-deemixd` 无损下载通道 | `TestDeezerLoginSyncsDeemix` |
| **#107** | `tools/ui-mod/echo_out.h`<br>`tools/ui-mod/build_packs.py` | **推子后 DSP Echo Out 尾音与控制中心第 3 行磁贴重构**：控制中心第 3 行标准化为 4 个演出磁贴：`Waveform Style`、`Jog Light`（`Static / Rainbow / Fast / BPM`）、`Fader Echo`（`Off / All / Upfader`）、`Smart Sync`（`Bar / Beat / Tempo`）；`Live Stream` 移入 `设置 → 服务` | `gate-ui-probe.qml`、实机截图与触控核验 |
| **#108** | `pkg/webui/broadcast.go`<br>`pkg/broadcast/hub.go` | **8080 端口零配置 HTTP 实时音频流直出（`/live.mp3` 与 `/live.wav`）**：无需外部 Icecast 服务器，打开 `Live Stream` 开关即可在局域网浏览器、VLC 或 OBS 中直接打开 `http://<碟机IP>:8080/live.mp3` 或 `/live.wav` 监听/推流 | `TestBroadcastLiveHTTP` |
| **#109** | `cmd/enginex-installer/main.go`<br>`README*.md`<br>`docs/site/content.js` | **强化一代机 U 盘直刷指引与 `Computer Update Mode` 说明**：在安装器与三语文档中突出提示一代机（PRIME 4 / SC5000 / SC6000 等）首选 FAT32/exFAT U 盘根目录 `.img` 免电脑直刷 | 安装器单测与文档核对 |
| **#110 / #111** | `cmd/enginex-installer/main.go` | **修复 PRIME GO+ fastboot `product: rk3288-az05-jp11s` 识别失败与免 `fastboot fetch` 引导**：兼容带 SoC/平台前缀的 fastboot product 字符串（提取末尾 `jp11s` 等板号），针对不支持 `fastboot fetch` 的机型提供清晰分步指引 | `TestParseFastbootProductWithPrefix` |
| **#112** | `pkg/webui/omnisource.go`<br>`pkg/webui/omnisource_test.go` | **OmniSource 原生支持 Algoriddim djay 曲库与歌单**：直接解析外置 U 盘/SD 卡上的 djay `MediaLibrary.db` 曲目与播放列表，合并展示在流媒体浏览器中并支持直读播放 | `TestOmniSourceDjayLibrary` |
| **#113** | `rootfs-overlay/usr/local/bin/sc5000-perf-tune`<br>`pkg/webui/system_admin.go` | **USB 数字音频输出支持（USB-A 外接免驱 UAC1/UAC2 DAC 与 USB-B UAC2 声卡模式）**：为缺少物理同轴数字口的机型提供外接 USB 解码器/数字音箱路由检测与配置能力 | `TestSystemAdminAudioRoutes` |
| **#114** | `pkg/webui/system_admin.go`<br>`pkg/webui/tidal_gateway.go`<br>`pkg/dbhealer/reserve.go` | **远程网络盘挂载（SMB / SFTP-SSHFS / WebDAV，含用户名/密码/端口）、SSH 开关、Automix 与 4 TB 大盘优化**：Web 控制台新增远程挂载管理、SSH 服务开关与 Automix 辅助控制；`vfs_cache_pressure=50` 优化 4 TB 大曲库目录项缓存；`pkg/dbhealer/reserve.go` 预留 64 MiB 保护空间防盘满损坏 `m.db`；修复 `/v1/sc5000/{ssh,mounts,automix}` 被 `isTidal()` 拦截的路由优先级 bug | `TestGatewayServesSC5000AdminRoutes`、`TestSystemAdminMountsAndSSH`、`TestReserveSpace` |
| **#115** | `pkg/webui/prolink_diag.go`<br>`rootfs-overlay/usr/local/bin/sc5000-prolink-diag`<br>`docs/PROLINK-CDJ3000-BETA-TEST.md` | **硬件测试者协作套件与 Pro DJ Link 一键自检工具**：新增 `sc5000-prolink-diag` 命令行与 `/v1/sc5000/prolink/diag` 诊断接口（自动检查网卡、UDP 50000–50002、NFS/dbserver、导出库状态）及 CDJ-3000 联调测试手册 | `TestProlinkDiag`、SC5000 实机自检 PASS |
| **#116** | `docs/GEN2-USB-UPDATE-MODE.md`<br>`tools/az01-packer/az0x_packer.py`<br>`tools/az01-packer/az04_packer.py` | **Rane SYSTEM ONE（RMZ2）/ PRIME 4 G2（JP24）与二代机固件更新模式（`1cc5:2027` `az0x-splash` 协议）全链路逆向与文档化**：完整梳理二代机 Stage-2 U-Boot `fastboot` 裁切事实、GPIO/ADC 按键判定、recovery ramdisk 内 `az0x-splash` 自定义 USB 批量传输协议（`get-property`、`get-pubkey`、`data`、`write-part` 帧结构） | `test_az0x_packer.py`、`test_az04_packer.py` |
| **#117** | `pkg/webui/ui_settings.go`<br>`tools/ui-mod/build_packs.py`<br>`tools/az01-packer/build-data-bundle.py` | **全局界面缩放 `UI Scale`（`100% / 95% / 90% / 85% / 80% / 75%`）与二代机数据包联动**：合入社区 PR #117（`@tribalwalker-sketch`），补齐原子写入 `ui-scale.env`、二代机 `boot-activate.sh` 注入与离线 `settings.conf` 解析，并在 QML 侧改为 `uiContainer` + `Overlay.overlay` 同步中心缩放，彻底消除 EGLFS 黑边与弹窗截断 | `TestUIScale`、`TestUIScaleConcurrentWrites`、SC5000 实机帧缓冲核验 |
| **#118** | `tools/splash/splash_tool.py`<br>`docs/assets/boot-splash-enginex.png`<br>`docs/assets/recovery-splash-enginex.png` | **全系 16 款机型开机第一屏（`splash` / `recoverysplash` / `SplashScreen.png`）定制与转换工具链**：对齐官方 5.3.0 扁平白底 UI 视觉风格，支持 BGRA32 raw `.data` 与 U-Boot BMP/gzip 格式双向转换并内置到打包链路 | `tools/splash/test_splash_tool.py` |
| **#119** | `tools/az01-packer/az0x_packer.py`<br>`tools/ui-mod/engine_ui_hook.c` | **二代机 `AZ0x`（RK3288 签名容器）全分区解析/自签打包器与 `az01_apply_image` 研究工具链**：实现 `AZ0x` 容器解析、SHA-256 校验、自定义密钥对签名重打包与 `test_az0x_packer.py` 5 项单测 | `test_az0x_packer.py` |
| **#120** | `pkg/webui/sampler_loop.go`<br>`pkg/webui/tidal_gateway.go` | **将当前轨道的活动 Loop 一键发送到采样器打击垫（`/v1/sc5000/sampler/loop-to-pad`）**：根据当前轨道文件路径、`CurrentLoopInPosition`、`CurrentLoopOutPosition` 与 `SampleRate`，使用 `ffmpeg` 无损切片为临时 WAV 并自动注入 `/Engine/Sampler<N>/Load`（由社区用户 `@tribalwalker-sketch` 提出） | `TestSamplerLoopToPad`、`TestGatewayServesSC5000AdminRoutes` |
| **#121** | `pkg/providers/spotify.go`<br>`pkg/providers/providers_test.go` | **ReccoBeats 曲目音频特征（Energy / Danceability / Valence）集成**：支持为流媒体曲目查询与提取能量度、舞曲度与情绪价指标 | `TestFetchReccoBeatsAudioFeatures`、`TestExtractReccoBeatsAudioFeatures` |
| **#122** | `pkg/enginedb/artwork.go`<br>`pkg/prolink/engine_bridge.go` | **Rekordbox 音频内嵌高清封面提取、先锋 DJM 混音台 On-Air 状态包解析与标签注释同步**：当 ANLZ 无图片时直接从音频文件 ID3v2 `APIC` / FLAC `METADATA_BLOCK_PICTURE` 提取高清封面供转盘中心屏显示；解析 DJM 混音台 UDP 50001 `0x03` On-Air 通道状态包 | `TestExtractEmbeddedArtwork`、`TestParseDJMOnAirPacket` |
| **#123** | `pkg/webui/ui_settings.go`<br>`tools/ui-mod/build_packs.py` | **单机双层四轨模式（`1+2`）与 `four_deck_role=auto` 防呆保护**：严格保持 `auto` 模式下奇数 Player 号映射 `13`（Deck 1+3）、偶数 Player 号映射 `24`（Deck 2+4）的防呆不变量，绝不自动回退 `12` 导致双机互抢影子轨；单台 SC5000/SC6000 用户可显式选 `1+2` 开启单机双层四轨同屏波形 | `TestFourDeckRoleAutoInvariant`、SC5000 实机验证 |
| **#124 / #125 / #126 / #127** | `tools/az01-packer/az04_packer.py`<br>`tools/az01-packer/az04_uboot_patch.py`<br>`docs/GEN2-USB-UPDATE-MODE.md` | **RK3588 64 位二代机（`AZ04`：PRIME 4 G2 / SYSTEM ONE）底层容器工具链**：完成 Stage-2 U-Boot ADC/GPIO 按键映射表逆向（#125）、嵌套 FIT U-Boot 双公钥软验签补丁工具 `az04_uboot_patch.py`（#126）、13 分区 `AZ04` 容器解包/重打包与 zstd recovery ramdisk 处理工具 `az04_packer.py`（#127）及沙箱全分区校验（#124） | `test_az04_packer.py`、`test_az04_uboot_patch.py` |
| **#128 / #130** | `pkg/webui/tailscale.go`<br>`pkg/webui/index.html`<br>`tools/tailscale/build.sh` | **Tailscale / Headscale 用户态（`--tun=userspace-networking`）远程访问支持**：针对原厂内核无 `CONFIG_TUN` 的限制，在 Web 控制台新增「远程访问」卡片，支持按需下载校验 `tailscaled`、填写自建 Headscale 地址或官方 Tailscale 登录链接/一次性 Auth Key（0600 临时文件传递，不留盘、不进命令行），并通过 `tailscale serve --tcp` 安全转发 SSH/SFTP 与 `:8080` 控制台 | `TestTailscaleRejectsBadInput`、`TestTailscaleInstallVerifiesChecksum`、`TestTailscaleLoginServeAndStop`、Headscale 端到端实测 |
| **#129 / #131** | `cmd/enginex-installer/main.go`<br>`cmd/enginex-installer/main_test.go` | **修复 PRIME 4+（`inmusic,jc11s`）在 Windows / Linux 下的发现与 `"Android Fastboot"` 识别问题**：新版 `android-tools` 将处于更新模式的二代机（USB ID `15e4:d007`）输出为 `"Android Fastboot"` 而非板号，现 `fastbootScan` 兼容该格式并通过 `getvar product` 精确提取板号，同时优先使用安装包自带的 `fastboot`；经社区用户 `@RattyDAVE` 在 PRIME 4+ 真机上实测成功刷入并点亮 3-Band 波形 | `TestFastbootScanAndroidFastboot`、用户 PRIME 4+（JC11S）真机验证通过 |
| **#132** | `tools/az01-packer/build-data-bundle.py`（二代机 `boot-activate.sh`）<br>`tools/ui-mod/build_packs.py`<br>`pkg/webui/ui_settings.go` | **修复 PRIME 4+ 等二代机关于页不显示访问码、在线更新报 `GitHub unreachable`**：根因是二代机 `boot-activate.sh` 先拉起 `stream-sync` 等守护进程、最后才 `systemctl restart engine.service`，而 `engine.service` 默认 `KillMode=control-group`，重启时把同一 cgroup 里刚拉起的 `stream-sync` 一起杀掉，About 页拿不到 `/v1/sc5000/ui`，更新检查也无人应答。现在 `boot-activate.sh` 把 `stream-sync` / `rb-bridge` / `sc5000-deemixd` 写成 `/run/systemd/system/enginex-*.service` 独立单元，`99-sc5000.conf` drop-in 用 `Wants=` 拉起它们，重启 Engine 不再波及这些守护进程（当时一并加的 `KillMode=process` 已在 #116 重发中删除，见下一行）；只有 PID 1 是 systemd 时才走 `systemctl`，否则（chroot 沙箱）直接在后台启动；屏幕 OTA 解析 `/data/sc5000-custom` → `/data/enginex` 软链（`f4b0ce6`，编译与测试修正 `5b4687e`）。修复后一代与二代全系机型在 `Settings → About` 稳定显示 `Web Console: http://<IP>:8080` 与 `Access Code: XXXX-XXXX`（输入时忽略连字符与大小写） | `TestGenerateAccessCode`、`TestLoginAccessCodeInputAndLockout`、`gate-ui-probe.qml`、`test_sclive4_bootstrap.sh` |
| **#116（重发）** | `cmd/enginex-installer/main.go`<br>`tools/az01-packer/host-install-usbupdate.py`（新）<br>`tools/az01-packer/host-install-userdata.py`<br>`tools/az01-packer/build-data-bundle.py` | **SYSTEM ONE / PRIME 4 G2 走 `1cc5:2027` 一键刷机、`--ssh-key`、修复关机与访问码**：① 安装器新增模式 C：开机时或等待原厂机时发现 USB `1cc5:2027`（`az0x-splash usbupdate`，无 fastboot），按局域网板号 → `--bundle` 文件名 → USB 产品名判定机型，生成 64 MiB `ext4` `/data` 镜像（`-O encrypt -L data -b 4096`），确认后调用数据包内的 `host-install-usbupdate.py`（pyusb）执行 `AZ0O` `write-part`（`status 5` → 裸流 + ZLP → `status 1`，`data` 返回 `status 3` 时回退 `userdata`），写完提示按电源键重启；协议由 `@SBDJUK` 在 Rane SYSTEM ONE 实机验证。② `host-install-userdata.py --ssh-key` 写入 `/enginex/authorized_keys`（0600），`boot-activate.sh` 在只读根上把它 bind mount 到 `/root/.ssh/authorized_keys` 并启动 `sshd`。③ 删除 `99-sc5000.conf` 的 `KillMode=process`：`engine.service` 是 `Type=forking`，主 PID 是 `Scripts/engine` 脚本，重启时只杀脚本，第一阶段无 hook 的 `/usr/Engine/Engine` 留在屏幕上（关于页无访问码），关机也没人执行 `poweroff`（`@SBDJUK` 实机现象）。门禁说明：一代 `.img` 不受影响；同版本重发不会触发二代机屏幕在线升级提示 | `TestAZModel`、`TestUSBUpdateModeFlashesA64MiBDataImage`、`TestLockedSystemOneWaitsFor1cc5`、`TestUSBUpdateModeRefusals`、`TestUSBFlashRunsTheBundleScript`、`test_host_install_usbupdate.py`（报文与 SYSTEM ONE 抓包逐字节一致）、`test_data_bundle.py`、`test_sclive4_bootstrap.sh` 默认与 `AZ04=1`（bind mount、sshd、无 KillMode） |
| **#129 / #133** | `cmd/enginex-installer/main.go`<br>`cmd/enginex-installer/sigpipe_{unix,windows}.go`<br>`tools/az01-packer/host-install-fastboot.sh` | **安装器刷写时实时显示 fastboot 进度与分区大小提示**（`@RattyDAVE` 建议）：以前输入 `yes` 确认后，几 GB 的 userdata / rootfs 走 USB 发送和写入 eMMC 期间屏幕几分钟没有任何输出。现在 `fbLive` 把 fastboot 的 stdout / stderr 实时转发到控制台，同时保留缓冲输出用于报错；刷写大分区前（二代 `modeB` 与一代 `modeBGen1`）先显示分区名、文件大小（GB/MB/KB/B）和「通过 USB 传输可能需要几分钟，请勿断开连接或关闭电源」提示；启动时忽略 SIGPIPE，`captureWriter` 吞掉控制台管道关闭导致的 EPIPE，控制台断开不会中断刷写；`host-install-fastboot.sh` 刷写前同样打印提示 | `cmd/enginex-installer` 全套单测（含隔离子进程 SIGPIPE 测试） |

---

## 2. 本次发版前实机联调与深度打磨详情

1. **UI Scale `<100%` 控制中心与弹窗全屏适配（`tools/ui-mod/build_packs.py`）**：
   - **问题**：仅设置环境变量 `QT_SCALE_FACTOR` 在 EGLFS 下会导致 `nativeScreenSize` 与 `defaultScreenSize` 脱节而在屏幕右/下侧留下黑带；而仅缩放 `EmbeddedMain.qml` 的 `uiContainer` 时，承载控制中心抽屉、弹窗和 FX 选择器的 `Overlay.overlay` 以及 `ControlCenter.qml`、`StorageModeOverlay.qml`、`PadModeSelectDialogue.qml`、`ColorWheelDialog.qml`、`DJFxSelectCarousel.qml` 仍按未缩放的 `defaultScreenSize` 计算宽高，缩小时只盖住屏幕左上角，右侧与下方露出半透明遮罩带。
   - **修法**：`uiContainer` 按 `defaultScreenSize * 100 / scUIScale` 扩大逻辑尺寸并设 `scale: scUIScale / 100` 居中缩放；`Overlay.overlay.scale` 动态绑定 `uiContainer.scale`；上述全屏抽屉与弹窗通过 `root.transformed.width / height` 动态跟随逻辑尺寸，底层铺设 `z: -1000` 纯黑背景矩形消除边缘底色，实现 `100%`–`75%` 全屏无死角等比缩放且无需重启 Engine。
2. **Pad LED 软硬件背光同步调光（`tools/ui-mod/build_packs.py`）**：
   - **修法**：屏幕虚拟打击垫 `Pads.qml` 的 `opacity` 绑定 `scLedBrightness / 100`（`0.2`–`1.0`）；针对运行在独立 QML 引擎中的硬件控制器脚本，在 `GlobalAssignmentConfig.qml` 中每 3 秒读取 `/v1/sc5000/ui` 的 `ledBrightness`，经 `ActionPadRGBLed.qml` 与 `QuadPadRGBLed.qml` 传入 `ColorOutputAssignment.qml`，在 `scScaled()` 中对发往硬件 MCU 的 `outputColor` RGB 分量做等比衰减并在亮度变更时立即重发 `device.sendColor`。
3. **四轨曲库底栏 `1-2-3-4` 顺序、`1/3` 与 `2/4` 角色框高亮及流媒体影子波形同步（`tools/ui-mod/build_packs.py`、`pkg/webui/shadow.go`）**：
   - **底栏与角色框**：原厂四轨模式下底栏 `DeckTabs.qml` 按左/右层顺序排成 `1, 3, 2, 4`，现改为开启四轨时按 `1, 2, 3, 4` 从左到右排列，`MicroDeck.qml` 在歌名前显示 `1 · `..`4 · ` 轨号；本机负责的轨道（如左机 `1/3`、右机 `2/4`）高亮激活，对端影子轨道自动置灰并由 `scDeckLocked` 拦截误加载。
   - **流媒体曲目跨机影子波形**：Engine 5.3.0 加载流媒体曲目时 `TrackNetworkPath` 仅返回 `net://<uuid>/(Unknown)`，不含曲目 ID；现 `scShadowTick` 优先读取 `TrackUri`（如 `streaming://TIDAL/Track/<id>`），`pkg/webui/shadow.go` 收到后从源机的流媒体库 `stm.db` 与下载缓存目录解析出真实音频文件及 `PerformanceData`（`trackData`、`beatData`、`overviewWaveFormData`），生成 `.perf`（`EXPF v1`）交由影子机 `OfflineAnalyzer` 快速通道直接渲染出完整三频段/RGB 波形与拍线。

---

## 3. 测试、发版门禁与真机验证

- **编译主机全量自动化测试（ARM64 原生环境，`--clean` 工作区）**：
  - Go 代码静态检查与竞态单测：`gofmt -l .`（0 差异）、`go vet ./...`、`go test -race ./...`（全部 22 个 Go 包 PASS）；
  - 系统 overlay 与脚本测试：`bash tests/test_rootfs_overlay.sh`（13 项全部 PASS）；
  - 打包器与容器测试：`python3 -m unittest discover -s tools/az01-packer`（88 项 PASS、1 项跳过，覆盖 `az01_packer`、`az0x_packer`、`az04_packer`、`az04_uboot_patch`、`build-data-bundle`、`rootfs_audit`）、`python3 -m unittest discover -s tools/stems`（9 项 PASS）、`python3 -m unittest discover -s tools/splash`（PASS）；
  - C 运行时与 DSP 钩子单测：
    - `tools/ui-mod/test_dsp_smooth.c`：常规 `-O2`、`-fsanitize=address,undefined`（ASan/UBSan）、`-fsanitize=thread`（TSan）三种构建全部 PASS（覆盖 21 个 DSP guard、Echo Out 推子后尾音、5 种 Pioneer FX、Auto-Gain 与 Master Soft Clipper）；
    - `tools/ui-mod/test_stems_feature.c`：armhf 原生编译，针对 `5.0.4`、`5.3.0` 与 `5.3.0-sclive4` 三个原厂 `Engine` 二进制及 `OfflineAnalyzer` `EXPF` 快速通道全部 PASS；
    - `tools/ui-mod/test_az04_hook.c`：AArch64 原生编译，针对 `PRIME 4 G2` / `SYSTEM ONE` 的 64 位 `Engine` 全部 PASS；
    - `tools/edisksd-hook/test_edisksd_hook.c`：含 `unshare -m` 下的 `--mounts` 全部 PASS；
  - 二代机沙箱验收：`bash tools/az01-packer/test_sclive4_bootstrap.sh` 默认（`SC LIVE 4` armhf）与 `AZ04=1`（`PRIME 4 G2` / `SYSTEM ONE` aarch64）的 Method A 注入、首次启动激活（含 `enginex-*.service` 单元）、Engine hook（armhf 16 处 / AZ04 6 处补丁）、Method B 安装/卸载与旧目录升级全部 PASS。
  - `f4b0ce6`（#132）提交时 `pkg/webui` 因多余的 `syscall` 导入无法编译，`TestUpdateAppliesTheDataBundle` 与沙箱 Method B 也未通过；`5b4687e` 修正后上述 Go / Python / overlay / 沙箱测试在编译机重跑全部 PASS。C 单测本次未改动、未重跑。
- **同版本重发（#133，构建 `38d5e68`）回归**：编译机 `--clean` 目录重跑 `gofmt -l .`（0 差异）、`go vet ./...`、`go test -race ./cmd/enginex-installer/... ./pkg/webui/...`、`tests/test_rootfs_overlay.sh`（13 PASS）、`test_sclive4_bootstrap.sh` 默认与 `AZ04=1`（全部 PASS）、`python3 -m unittest discover -s tools/az01-packer`（88 PASS、1 跳过）；三个代表镜像解包均为 `"verified": true`，`/etc/sc5000/version.txt` 为 `5.3.0.3`，不含内置 SSH 公钥。
- **同版本重发（#116，构建 `9616419`）回归**：编译机 `--clean` 目录跑 `gofmt -l .`（0 差异）、`go vet ./...`、`go test -race ./...`（全部 PASS）、`tests/test_rootfs_overlay.sh`（13 PASS）、`test_sclive4_bootstrap.sh` 默认与 `AZ04=1`（全部 PASS）、`python3 -m unittest discover -s tools/az01-packer`（92 PASS、1 跳过）；构建从 GitHub 克隆 `9616419`，三个代表镜像解包均为 `"verified": true`、版本 `5.3.0.3`，`/root/.ssh/authorized_keys` 只有与 `v5.3.0.2` 相同的原厂 `cert-authority` 条目。门禁：三个镜像组（JP07 / JP13 / JC11）以已发布 `v5.3.0.2` 镜像为旧版并行跑 `release-gate.sh`，三组各 **94 PASS / 0 FAIL**。
- **发版门禁（`tools/az01-packer/release-gate.sh`）**：
  - 三个一代镜像组（`SC5000` JP07、`SC6000` JP13、`PRIME 4` JC11）分别从已发布的 `v5.3.0.2` 镜像在线升级至 `v5.3.0.3` 并完成开机全功能验收，三组各 **94 PASS / 0 FAIL**（首次构建 `5b4687e`；2026-11-08 同版本重发构建 `38d5e68` 以已发布 `v5.3.0.2` 的 JP07 / JP13 / JC11 镜像为旧版重跑，三组仍各 94 PASS / 0 FAIL）（静态审计 `systemd-analyze verify` 0 错误、在线升级下载与篡改 1 字节拒绝、原厂 QML 缓存种入后新 UI 包强制重编、控制中心第 3 行 4 个新磁贴渲染、关于页 `EngineX OS 5.3.0.3` 与访问码渲染、16 处 Engine 补丁、21 个 DSP guard、Stems 分轨、Rekordbox 转换、下一次在线升级 mock 全部通过）。
- **真机验证**：
  - **两台 Denon DJ SC5000 Prime（JP07，`<deck-1>` 与 `<deck-2>`，千兆有线局域网 + OpenProLink 虚拟 CDJ-3000）**：
    - 验证双机四轨模式（`<deck-1>` Role `1+3`、`<deck-2>` Role `2+4`）：曲库底栏 `1-2-3-4` 顺序与角色高亮正确；流媒体曲目与本地曲目跨机影子轨波形、拍线、8 个 Hot Cue、主 CUE、`<20 ms` 启停与按住 CUE 试听同步无误；
    - 验证 `UI Scale`（`85%` / `75%` → `100%`）控制中心与弹窗全屏无黑边、三语免重启即时切换、Layout Manager `View 1–5` 保存与恢复、Pad LED 软硬件调光、推子后 `Echo Out` 尾音与 `Color FX Style`。
  - **Denon DJ PRIME 4+（JC11S，社区用户 `@RattyDAVE` 真机）**：
    - 使用更新后的 `EngineX-Installer` 通过 USB Fastboot（兼容 `"Android Fastboot"` 输出）一键生成并刷入 `PRIME4PLUS` 数据包，真机正常启动进入 EngineX OS 并开启 3-Band 波形。

---

## 4. 发布构建产物与 SHA-256 校验表

- **构建提交**：`9616419`（2026-11-08 第二次同版本重发 #116，编译机上从 GitHub 新克隆的源码目录，`build-release.sh` 公钥参数 `none`，公开镜像不含任何内置 SSH 公钥）
- **回滚点**：同版本上一次构建为 `38d5e68`（tag 原在 `937b082`），再上一次为 `5b4687e`（tag `df06655`）；上一版本为 `v5.3.0.2`（提交 `e7622ac` / 构建提交 `72f4e34`）

### 一代机型完整固件（`*-Update.img`）

| 镜像组 | 机型 | 板号 | 文件名 | SHA-256 |
| --- | --- | --- | --- | --- |
| SC5000 | Denon DJ SC5000 Prime | JP07 | `SC5000-5.3.0.3-EngineX-OS-Denon-SC5000-Prime-JP07-Update.img` | `acbd7a9d80dba5b9d4199fab9977e0c14e82749981749e4e12f02e58ef98befa` |
| SC5000 | Denon DJ SC5000M Prime | JP08 | `SC5000M-5.3.0.3-EngineX-OS-Denon-SC5000M-Prime-JP08-Update.img` | `acbd7a9d80dba5b9d4199fab9977e0c14e82749981749e4e12f02e58ef98befa` |
| SC5000 | Denon DJ PRIME GO | JP11 | `PRIMEGO-5.3.0.3-EngineX-OS-Denon-PRIME-GO-JP11-Update.img` | `acbd7a9d80dba5b9d4199fab9977e0c14e82749981749e4e12f02e58ef98befa` |
| SC6000 | Denon DJ SC6000 Prime | JP13 | `SC6000-5.3.0.3-EngineX-OS-Denon-SC6000-Prime-JP13-Update.img` | `013aedb54a1f33ee0ce89075536aa2aece675f856eb74123f85b9e072f393407` |
| SC6000 | Denon DJ SC6000M Prime | JP14 | `SC6000M-5.3.0.3-EngineX-OS-Denon-SC6000M-Prime-JP14-Update.img` | `013aedb54a1f33ee0ce89075536aa2aece675f856eb74123f85b9e072f393407` |
| PRIME 4 | Denon DJ PRIME 4 | JC11 | `PRIME4-5.3.0.3-EngineX-OS-Denon-PRIME-4-JC11-Update.img` | `d20c0b46a61defeef29b6155e7ff603fa16b1882e26fde94eeedc3f33b6546c7` |
| PRIME 4 | Denon DJ PRIME 2 | JC16 | `PRIME2-5.3.0.3-EngineX-OS-Denon-PRIME-2-JC16-Update.img` | `d20c0b46a61defeef29b6155e7ff603fa16b1882e26fde94eeedc3f33b6546c7` |
| PRIME 4 | Numark Mixstream Pro | NH08 | `MIXSTREAMPRO-5.3.0.3-EngineX-OS-Numark-Mixstream-Pro-NH08-Update.img` | `d20c0b46a61defeef29b6155e7ff603fa16b1882e26fde94eeedc3f33b6546c7` |

### 二代机型 `/data` 数据包（`*-Data-Bundle.tar.gz`，Beta）

| 架构组 | 机型 | 板号 | 文件名 | SHA-256 |
| --- | --- | --- | --- | --- |
| AZ0x (armhf) | Denon DJ SC LIVE 4 | JP21 | `SCLIVE4-5.3.0.3-EngineX-OS-Data-Bundle.tar.gz` | `8d220bd405ee34c45ca62fc5f93196bd1211506826f2040d7ed665283302fb6a` |
| AZ0x (armhf) | Denon DJ SC LIVE 2 | JP20 | `SCLIVE2-5.3.0.3-EngineX-OS-Data-Bundle.tar.gz` | `f3cd56f924e99206acd7418a601dd67c4b5fe95ccc490e8ff625ef6a06f1c050` |
| AZ0x (armhf) | Denon DJ PRIME 4+ | JC11S | `PRIME4PLUS-5.3.0.3-EngineX-OS-Data-Bundle.tar.gz` | `96e2be1a8c34ee84a61cf300350ee87bea2070e7434880419cd4c7f0fe047910` |
| AZ0x (armhf) | Denon DJ PRIME GO+ | JP11S | `PRIMEGOPLUS-5.3.0.3-EngineX-OS-Data-Bundle.tar.gz` | `eff6411100c0e9eee7a5fbf721d027c3b76d84bbf903c342d01530d619433213` |
| AZ0x (armhf) | Numark Mixstream Pro+ | NH08S | `MIXSTREAMPROPLUS-5.3.0.3-EngineX-OS-Data-Bundle.tar.gz` | `d578301c672cc92eab0a1b64f20b5ce7f9c2fa4b5a0acb2ece8ee9506485961b` |
| AZ0x (armhf) | Numark Mixstream Pro Go | NH10 | `MIXSTREAMPROGO-5.3.0.3-EngineX-OS-Data-Bundle.tar.gz` | `d7f99f0e95ded49e641a0cef9cc7a53d274bd4807501bcec42c31c3801a72187` |
| AZ04 (aarch64) | Denon DJ PRIME 4 G2 | JP24 | `PRIME4G2-5.3.0.3-EngineX-OS-Data-Bundle.tar.gz` | `54f31361977b87de9834218ec29afa914675e8c46c1c83900f8f9d8f33763a75` |
| AZ04 (aarch64) | Rane SYSTEM ONE | RMZ2 | `SYSTEMONE-5.3.0.3-EngineX-OS-Data-Bundle.tar.gz` | `c0e9eb8cbe6328c5e60886a2ef34ad135a9d4495bbb8541aeecc56df155c53b3` |

### 跨平台一键安装器（`EngineX-Installer`）

| 平台 | 文件名 | SHA-256 |
| --- | --- | --- |
| Windows 10/11 x64 | `EngineX-Installer-5.3.0.3-windows-amd64.zip` | `9f5bf502d2955e6250c634f068a6ba271568a5ebf3cdf1ef9067669f55b50a05` |
| macOS Apple Silicon (M1–M4) | `EngineX-Installer-5.3.0.3-macos-arm64.zip` | `3d32c15a82170038c2ad0d316ad2213a483566cf55dea6174e0043d7f4700740` |
| macOS Intel | `EngineX-Installer-5.3.0.3-macos-amd64.zip` | `f282229cdab3971c559dd699d381b0e00f333cd9c98135690bb41d8902787080` |
| Linux x86_64 | `EngineX-Installer-5.3.0.3-linux-amd64.tar.gz` | `8e3673c111a1f1898b3627b58ad20ab634b58d623ea3440fa2e12b18b214969c` |

---

## 5. 实机影响与升级说明

- **一代机（8 款 `.img`）**：已刷 `5.3.0.2`、`5.3.0.1.1`、`5.3.0.1` 或 `5.0.4.2` 的设备可在 `设置 → 关于 / 更新` 直接收到 `5.3.0.3` 在线升级推送；首次安装直接将对应 `.img` 拷入 FAT32/exFAT U 盘根目录刷入。
- **二代机（8 款 `Data-Bundle`）**：已安装含 `#86` 的 `5.3.0.2` 数据包的二代机可在屏幕 `设置 → 关于 / 更新` 一键在线升级至 `5.3.0.3`；首次安装或早期 `5.3.0.2` 数据包用户可使用电脑端 `EngineX-Installer-5.3.0.3` 一键自动识别安装。
- **配置与数据保留**：升级过程自动将旧 `/data/sc5000-custom` 零拷贝迁移至 `/data/enginex` 并保留兼容软链，原有的流媒体登录态、歌单缓存、视图预设与 SSH 公钥全部保留。
