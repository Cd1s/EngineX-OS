# EngineX OS 5.3.0.3 首次发布以来全量修复回归核验报告与实机清单

- **基准版本**：`v5.3.0.3`（首次发布：2026-11-07，构建提交 `4ab1921` / `5b4687e`）
- **核验分支与提交**：`origin/agent/integration4-20261110`（提交：`6167c30`）
- **核验环境**：编译机（ssm 别名；aarch64 架构，支持 32 位 EL0，原生运行 armhf 与 aarch64）
- **测试结果概览**：全部自动化测试套件 **100% 通过（0 FAIL）**；无任何代码回归。

---

## 一、自动化测试执行与验证汇总

所有自动化测试均在编译机原生干净沙箱环境中编译并执行完毕：

| 测试套件 / 命令 | 覆盖模块与场景 | 自动化结果 | 备注 |
| --- | --- | --- | --- |
| `gofmt -l . && go vet ./... && go test -race ./...` | 全仓库 22 个 Go 模块静态格式、vet 语法以及 `-race` 数据竞态测试 | **PASS** (22/22) | 耗时约 210s，0 竞态警告 |
| `bash tests/test_rootfs_overlay.sh` | 系统启动守护、CPU/GPU 亲和性、只读 HFS+、存储挂载、QML 磁盘缓存自愈与 UI 守卫 | **PASS** (18/18) | 18 个测试场景全部通过 |
| `bash tools/az01-packer/test_sclive4_bootstrap.sh` | 二代机 SC LIVE 4 (armhf) 方法 A ext4 注入、ALSA 首次启动激活、Engine hook 20 处内存补丁、方法 B 安装/卸载与旧目录迁移 | **PASS** (5/5) | 20 edits PASS |
| `AZ04=1 bash tools/az01-packer/test_sclive4_bootstrap.sh` | 二代机 PRIME 4 G2 / SYSTEM ONE (aarch64 AZ04) 首次启动、18 处内存补丁、方法 B 安装/卸载与旧目录升级 | **PASS** (5/5) | 18 edits PASS |
| `python3 -m unittest discover -s tools/az01-packer` | 固件解包、重打包、AZ0x 容器、AZ04 双公钥验签、FIT 镜像解析、U 盘线刷工具链 | **PASS** (147 PASS, 1 skip) | 覆盖 148 项单测 |
| `python3 -m unittest discover -s tools/stems` | Stems 算力节点握手、音频切片与分轨通道 | **PASS** (9/9) | 9 项单测全部通过 |
| `tools/ui-mod/test_dsp_smooth.c` (常规 `-O2`) | 21 个 DSP guard、Master Soft Clipper、Auto-Gain、推子后 Echo Out、5 种先锋 DJM 风格 FX | **PASS** | 零堆分配、防爆音限幅有效 |
| `tools/ui-mod/test_dsp_smooth.c` (ASan/UBSan) | 内存越界检测、未定义行为与指针边界检查 | **PASS** | 0 越界、0 泄漏 |
| `tools/ui-mod/test_dsp_smooth.c` (TSan) | DSP 音频线程并发、环形缓冲与多线程竞争检测 | **PASS** | 0 线程竞争 |
| `tools/ui-mod/test_stems_feature.c` | armhf 原生编译，针对 5.0.4、5.3.0 与 5.3.0-sclive4 三款原厂 Engine ELF 及 OfflineAnalyzer EXPF 快速通道验证 | **PASS** | 3 个原厂底包及 EXPF 回放全通过 |
| `tools/edisksd-hook/test_edisksd_hook.c --mounts` | 在 `unshare -m` 私有挂载命名空间下验证只读存储保护、原子软硬链接与崩溃回滚 | **PASS** | 真实挂载命名空间测试全通过 |
| `tools/ui-mod/test_az04_hook.c` | AArch64 原生编译，针对 AZ04 64 位 Engine 二进制（PRIME 4 G2 / SYSTEM ONE）验证正向 32 位符号偏移映射与 18 处补丁 | **PASS** | BuildID 匹配，18 处补丁全通过 |

---

## 二、5.3.0.3 首次发布以来完整修复项清单（逐项核验）

包含以下来源的全部修复项：
1. `docs/BUGFIX-REGRESSIONS.md`（底层稳定性、数据库与音频边界修复 17 项，BR-01 ~ BR-17）
2. `docs/ISSUE-FIXES-5.3.0.3.md`（5.3.0.3 首次发布及后续同版本重发保留修复 61 项，FX-5303-01 ~ FX-5303-62，剔除恢复原厂行为的 FX-5303-60 / #157）
3. `docs/ISSUE-FIXES-5.3.0.4.md` 与 `integration4-20261110` 新增集成修复（25 项，FX-5304-01 ~ FX-5304-25）

共计 **103 项**。

| 编号 | 大白话说明 | 对应的自动化覆盖 | 需要实机才能验的部分 | 当前结果 |
| --- | --- | --- | --- | --- |
| **BR-01** | 音频进程和中断绑在最快核心（CPU0/FIFO99），GPU与IO绑CPU1/2，避免打碟爆音 | `tests/test_rootfs_overlay.sh` (ok 05, 08, 09) | 无需实机（procfs 调度规则已验证） | 自动化通过 |
| **BR-02** | 忽略路径大小写识别 PDB/SQLite 导出曲库，并在后台限时修复数据库 | `pkg/dbhealer`、`tests/test_rootfs_overlay.sh` (ok 02) | 插入大小写混合路径的 Rekordbox U 盘核验曲库正常显示 | 自动化通过 |
| **BR-03** | 波形索引大数值乘法改用 uint64，防止长音频波形内存越界崩溃 | `pkg/enginedb` 单测 `TestWaveformIndexAndShortAnalysisBoundaries` | 无需实机 | 自动化通过 |
| **BR-04** | 在同一个数据库事务中安全删除失效曲目，自动释放旧路径并回滚脏数据 | `pkg/bridge` 单测 `TestDeletedSourceReleasesPathAndMembership` 等 | 无需实机 | 自动化通过 |
| **BR-05** | 固件 overlay 目录安全硬链接与符号链接边界检查，防止越界覆盖系统目录 | `tools/az01-packer` 单元测试集合 | 无需实机 | 自动化通过 |
| **BR-06** | 数据库触发器与歌单 NULL 尾链修复，防止歌单列表顺序错乱与外键冲突 | `pkg/enginedb` 单测 `TestEOS09ArtworkResizeHashAndOrphan` 等 | 无需实机 | 自动化通过 |
| **BR-07** | 虚拟机 QMP 模拟通信超时后自动重连直至超时截断 | `sim/qengine/test_smoke.py` | 无需实机 | 自动化通过 |
| **BR-08** | Rekordbox/Bridge 异常文件名、过大封面与损坏 FLAC 范围校验拦截 | `pkg/compat`、`pkg/rekordbox`、`pkg/prolink` 单测 | 无需实机 | 自动化通过 |
| **BR-09** | 严格校验 FLAC 文件的音频位深与声道数，格式非法直接拒绝解析 | `pkg/analyzer` 单测 `TestDecodeFLACFormatBoundsAndInheritedFields` | 无需实机 | 自动化通过 |
| **BR-10** | 严格校验 MP3 采样率范围，超出支持范围的安全跳过防解码器死锁 | `pkg/analyzer` 单测 `TestDecodeMP3SampleRates` | 无需实机 | 自动化通过 |
| **BR-11** | 曲库导入时校验有限正概览步长，非法步长曲目拒绝入库 | `pkg/enginedb` 单测 `TestLibraryImportRejectsInvalidAnalysisBounds` | 无需实机 | 自动化通过 |
| **BR-12** | 节拍自相关算法增加边界保护，不计算空区间平均数防除零错误 | `pkg/analyzer` 单测 `TestOnsetAutocorrelationLastSampleBoundary` | 无需实机 | 自动化通过 |
| **BR-13** | foldBPM 算法拒绝 NaN、Inf 与负数 BPM，防止变速推子算错拍子 | `pkg/analyzer` 单测 `TestFoldBPMRejectsInvalidValues` | 无需实机 | 自动化通过 |
| **BR-14** | 空音频文件计算波形时安全防御，避免生成空指针或负下界 | `pkg/analyzer` 单测 `TestComputeWaveformsRejectsEmptyAudio` | 无需实机 | 自动化通过 |
| **BR-15** | 流媒体下载遇到零字节临时占位文件时继续等待，防止半截文件被直接加载 | `pkg/providers` 单测 `TestDirectoryAdapterWatchesOnlyRequestedID` | 无需实机 | 自动化通过 |
| **BR-16** | 浮点 WAV 解码拦截 NaN 与 Inf 异常采样点，防止输出直流大爆音 | `pkg/analyzer` 单测 `TestDecodeFloatWAVRejectsNonFiniteSamples` | 无需实机 | 自动化通过 |
| **BR-17** | 浏览歌单时新增的流媒体曲目实时合并入当前会话缓存，无需重新扫描 | `pkg/providers` 单测 `TestGuestDownloadAnalyzeImportEndToEnd` | 无需实机 | 自动化通过 |
| **FX-5303-01** (#80) | 曲库浏览支持双列并排（BPM+第二字段）、Sync Guard 防误按、Beat Jump 打击垫 | `pkg/webui/ui_settings_test.go` (`TestUISettingsQoLAndFX`) | SC5000 打开双列浏览，核验常驻两列字段与 Sync 容差拦截 | 自动化通过 |
| **FX-5303-02** (#81) | 蓝牙音频输入（手机推流）与输出（推到蓝牙音箱）拆分并增强重连稳定性 | `pkg/webui` 单测 `TestBluetoothStatusAndModeSwitch` | 需报告人 / PRIME 4（SC5000 无内置蓝牙） | 自动化通过 |
| **FX-5303-03** (#82) | 支持局域网私有 NAS 音乐服务器（WebDAV/Subsonic/Navidrome/Plex）曲目直读播放 | `pkg/webui` 单测 `TestNASProvidersAndOmniSource` | SC5000 控制台挂载 WebDAV 服务器并在曲库中直读点播 | 自动化通过 |
| **FX-5303-04** (#83) | 局域网超低延迟无损音频串流（LAN RTP / HTTP PCM-WAV 广播），免外接录音卡 | `pkg/broadcast` 单测 `TestBroadcastHubAndLANStream` | 电脑打开 `http://<碟机IP>:8080/live.wav` 听音频推流 | 自动化通过 |
| **FX-5303-05** (#84) | 解除流媒体播放时的内置录音机限制，允许将流媒体混音直接内录到 U 盘 | UI 包构建断言与 `test_ui_mod.py` | SC5000 播放流媒体曲目时启动录音机录音至 U 盘 | 自动化通过 |
| **FX-5303-06** (#85) | 单机 Hot Cue 颜色/名称在线编辑，实体打击垫 Pad LED 5 档亮度调节 | `pkg/webui` 单测 `TestHotCuesEditor`、`test_az04_hook.c` | SC5000 设置 -> 设备 -> Pad 灯亮度调节 100%~20% 观察背光变化 | 自动化通过 |
| **FX-5303-07** (#86) | 二代机在系统设置屏幕中一键 OTA 升级 `/data` 数据包并自动热重启 | `pkg/webui` 单测 `TestUpdatePicksTheDataBundleOnSecondGeneration` | 需报告人 / 二代机（SC LIVE 4 等） | 自动化通过 |
| **FX-5303-08** (#87) | 外置 U 盘 `/mods` 即插即用加载器，发生崩溃自动隔离并退回安全模式 | `pkg/webui` 单测 `TestModsLoaderAndQuarantine`、`test_rootfs_overlay.sh` | 插入带自定义 mod 的 U 盘开机核验自动加载 | 自动化通过 |
| **FX-5303-09** (#88) | 先锋 DJM 风格 Beat FX 与 Sound Color FX 实时 DSP 模拟（Space/Dub Echo 等） | `tools/ui-mod/test_dsp_smooth.c` (3 种构建)、`test_stems_feature.c` | SC5000 设置切换 Color FX Style，旋转 Sweep 旋钮试听效果 | 自动化通过 |
| **FX-5303-10** (#89) | 跨平台零配置桌面一键安装器 EngineX-Installer（局域网+USB 自动识别机型） | `cmd/enginex-installer` 全套单测 | 电脑运行安装器识别在线或 USB 连接的碟机 | 自动化通过 |
| **FX-5303-11** (#90) | 修复四轨视图缺 Layer B 以及 Deck 3/4 颜色变纯黑纯白灯条的问题 | UI mod 构建与 QML 校验 | SC5000 开启四轨视图，确认 Deck 3/4 正常着色、Layer B 正常显示 | 自动化通过 |
| **FX-5303-12** (#91) | 修复四轨模式下 SC5000/SC6000 转盘中心小屏卡在 Engine Logo 的问题 | UI mod QML 绑定检查 | SC5000 四轨模式下切换 Layer A/B，确认转盘屏正常显示封面与光圈 | 自动化通过 |
| **FX-5303-13** (#92) | 修复 Engine 5.3.0 局域网 Engine Link 封面缺失（生成原始 BLOB 与落盘缩略图） | `pkg/enginedb` 单测 `TestExtractEmbeddedArtwork` | 两台 SC5000 连网线，左机插 U 盘，右机读曲目核验封面 | 自动化通过 |
| **FX-5303-14** (#93) | 修复影子轨道 8 秒循环重载、(USB N) 设备后缀误识别以及 Pitch 追赶抖动 | `pkg/webui` 单测 `TestShadowTrackPathAndPitch` | 双 SC5000 联动播放，观察影子轨平稳同步、无 8 秒重新加载 | 自动化通过 |
| **FX-5303-15** (#94) | 影子轨道 UDP 单播与 Engine Link 边沿信号驱动，实现 <20 ms 零延迟同步状态机 | `pkg/webui` 单测 `TestShadowUDPPushAndLongPoll` | 双 SC5000 联动，按下源机 Play / CUE / Hot Cue 确认瞬间响应 | 自动化通过 |
| **FX-5303-16** (#95) | 影子轨道 OfflineAnalyzer EXPF 快速通道，92µs 瞬时呈现拍线波形跳过 15 秒重新分析 | `tools/ui-mod/test_stems_feature.c` (EXPF 回放单测) | 双 SC5000 联动，源机加载曲目后影子机波形瞬间呈现 | 自动化通过 |
| **FX-5303-17** (#96) | 先锋 CDJ-3000 影子桥接重构，支持捕获 0x0b 绝对位置跳变并将拍线注入影子轨 | `pkg/prolink` 单测 `TestProlinkShadowWakeupAndJumps` | 需报告人 / 先锋 CDJ-3000 联调 | 自动化通过 |
| **FX-5303-18** (#97) | 影子轨道同步显示源机的 8 个 Hot Cue 色块与主 CUE 标记 | `pkg/webui` 单测 `TestShadowHotCuesSync` | 双 SC5000 联动，源机打下 Hot Cue，影子机屏幕同步亮起 | 自动化通过 |
| **FX-5303-19** (#98) | 解锁流媒体歌单按 BPM、调性、星级、标题、艺人升降序排序功能 | `pkg/webui` 单测 `TestTidalGatewaySort` | SC5000 流媒体歌单点击右上角排序菜单按 BPM 排序 | 自动化通过 |
| **FX-5303-20** (#99) | 视图管理器扩展至 View 1–5、新增 Save View、重启后自动保持且默认关闭屏幕 Pads | `pkg/webui` 单测 `TestViewsRoundTrip` | SC5000 切换 View 1~5 并保存，重启后确认界面保持 | 自动化通过 |
| **FX-5303-21** (#100) | 碟机流媒体缓存曲目经 Pro DJ Link 共享给同局域网的先锋 CDJ-3000 播放 | `pkg/prolink` 单测 `TestEngineBridgeStreamingExport` | 需报告人 / 先锋 CDJ-3000 | 自动化通过 |
| **FX-5303-22** (#101) | 修复 Rekordbox U 盘导入曲目波形偏矮问题，精确填入三频段峰值归一化高度 | `pkg/enginedb` 单测 `TestPerformanceDataPeaks` | SC5000 插入 Rekordbox U 盘加载曲目，观察波形高度饱满正常 | 自动化通过 |
| **FX-5303-23** (#102) | 上线简/繁/英三语官方文档站 enginexdj.com，包含机型刷机与交互式菜单地图 | 静态资源完整性与多语言检查 | 浏览器访问 enginexdj.com 验证 | 自动化通过 |
| **FX-5303-24** (#103) | 运行时目录平滑迁移至 `/data/enginex`，保留 `/data/sc5000-custom` 兼容软链接 | `tests/test_rootfs_overlay.sh` (ok 10)、`test_sclive4_bootstrap.sh` | SC5000 终端核验软链接与各配置文件完整性 | 自动化通过 |
| **FX-5303-25** (#104) | 推子后 Echo Out 尾音、Smart Sync 谐波 BPM 匹配、动态语言切换无需重启 | `tools/ui-mod/test_dsp_smooth.c`、`pkg/webui` 单测 `TestStemsTrackID` | SC5000 快速拉下通道推子听衰减尾音；设置动态切语言免重启 | 自动化通过 |
| **FX-5303-26** (#105) | Spotify 双模登录（Spotify Connect 局域网配对 + 手机扫码）与下载容灾回退 | `pkg/providers` 单测 `TestSpotifyConnectAndFallback` | 手机 Spotify 列表中出现 EngineX OS 并可直连推流 | 自动化通过 |
| **FX-5303-27** (#106) | Deezer 独立用户 ARL 扫码登录并与本地 deemixd 下载通道自动同步 | `pkg/webui` 单测 `TestDeezerLoginSyncsDeemix` | 手机打开 `/m/deezer` 输入 ARL 保存，流媒体服务变绿 | 自动化通过 |
| **FX-5303-28** (#107) | 控制中心第 3 行标准化为 4 个演出磁贴（波形样式/转盘灯/Echo Out/Smart Sync） | `tools/az01-packer/gate-ui-probe.qml` 门禁 | SC5000 屏幕下滑呼出控制中心，点击第 3 行磁贴切换 | 自动化通过 |
| **FX-5303-29** (#108) | 8080 端口零配置实时音频流直出（`/live.mp3` 与 `/live.wav`） | `pkg/broadcast` 单测 `TestBroadcastLiveHTTP` | 电脑浏览器打开 `http://<碟机IP>:8080/live.mp3` 试听 | 自动化通过 |
| **FX-5303-30** (#109) | 强化一代机（SC5000/SC6000/PRIME 4 等）U 盘根目录 `.img` 免电脑直刷指引 | 安装器单测与文档核验 | 无需实机 | 自动化通过 |
| **FX-5303-31** (#110/#111) | 修复 PRIME GO+ fastboot product 前缀解析失败，并提供免 fetch 刷机引导 | `cmd/enginex-installer` 单测 `TestParseFastbootProductWithPrefix` | 需报告人 / PRIME GO+ (JP11S) | 自动化通过 |
| **FX-5303-32** (#112) | OmniSource 原生解析外置存储中的 Algoriddim djay 数据库与歌单 | `pkg/djay` / `pkg/webui` 单测 `TestOmniSourceDjayLibrary` | 插入 djay 格式 U 盘在流媒体界面直接浏览播放 | 自动化通过 |
| **FX-5303-33** (#113) | USB 数字音频输出支持（识别外接免驱 UAC1/UAC2 DAC 声卡） | `pkg/webui` 单测 `TestSystemAdminAudioRoutes` | SC5000 插入 USB DAC 观察音频路由输出 | 自动化通过 |
| **FX-5303-34** (#114) | 远程网络盘挂载（SMB/SFTP/WebDAV）、SSH 服务开关、Automix 与 4TB 目录缓存优化 | `pkg/webui` 单测 `TestGatewayServesSC5000AdminRoutes` 等 | SC5000 控制台配置挂载远程 SMB 并在曲库浏览 | 自动化通过 |
| **FX-5303-35** (#115) | 硬件测试者协作套件与 Pro DJ Link 一键诊断工具 `sc5000-prolink-diag` | `pkg/webui` 单测 `TestProlinkDiag` | SC5000 运行 `sc5000-prolink-diag` 自检网络与端口状态 | 自动化通过 |
| **FX-5303-36** (#116) | SYSTEM ONE / PRIME 4 G2 固件更新模式（1cc5:2027）逆向与文档化 | `tools/az01-packer/test_az0x_packer.py`、`test_az04_packer.py` | 需报告人 / SYSTEM ONE / PRIME 4 G2 | 自动化通过 |
| **FX-5303-37** (#117) | 全局界面缩放 UI Scale（110%~75%）中心缩放，彻底消除 EGLFS 黑边与弹窗截断 | `pkg/webui` 单测 `TestUIScale`、`TestUIScaleConcurrentWrites` | SC5000 设置 UI Scale 85%/75%，确认控制中心与弹窗全屏贴边居中 | 自动化通过 |
| **FX-5303-38** (#118) | 全系 16 款机型开机第一屏定制与转换工具链（对齐官方 5.3.0 扁平白色风格） | `tools/splash/test_splash_tool.py` | SC5000 开机观察第一屏官方扁平白色 UI 画面 | 自动化通过 |
| **FX-5303-39** (#119) | 二代机 AZ0x 签名容器解析、自签名重打包与镜像校验工具链 | `test_az0x_packer.py` | 无需实机 | 自动化通过 |
| **FX-5303-40** (#120) | 将当前轨道的活动 Loop 一键发送到采样器打击垫（`/v1/sc5000/sampler/loop-to-pad`） | `pkg/webui` 单测 `TestSamplerLoopToPad` | SC5000 打出 Loop 后触发发送，核验 Sampler Pad 载入该切片 | 自动化通过 |
| **FX-5303-41** (#121) | ReccoBeats 曲目音频特征（能量度 Energy、舞曲度、情绪价）集成 | `pkg/providers` 单测 `TestFetchReccoBeatsAudioFeatures` | SC5000 浏览流媒体曲目查看特征标签 | 自动化通过 |
| **FX-5303-42** (#122) | Rekordbox 内嵌高清封面提取与先锋 DJM 混音台 On-Air 状态包解析 | `pkg/enginedb` 单测 `TestExtractEmbeddedArtwork` 等 | SC5000 插入无封面缓存的 Rekordbox U 盘，确认转盘屏高清封面 | 自动化通过 |
| **FX-5303-43** (#123) | 单机双层四轨模式（1+2）与 `four_deck_role=auto` 角色防呆保护，防止双机互抢 | `pkg/webui` 单测 `TestFourDeckRoleAutoInvariant` | SC5000 单机选择 Role 1+2 显双层两轨波形；双机时不抢角色 | 自动化通过 |
| **FX-5303-44** (#124~#127) | RK3588 64 位二代机（AZ04）底层容器、双公钥软验签 patch 与 U 盘直刷链 | `test_az04_packer.py`、`test_az04_uboot_patch.py` | 需报告人 / AZ04 机型 | 自动化通过 |
| **FX-5303-45** (#128/#130) | Tailscale / Headscale 用户态免 TUN 远程组网支持，安全转发 SSH 与控制台 | `pkg/webui` 单测 `TestTailscaleLoginServeAndStop` 等 | SC5000 控制台配置 Auth Key 并在外网电脑通过 VPN 访问 | 自动化通过 |
| **FX-5303-46** (#129/#131) | 修复 PRIME 4+ 在 Windows/Linux 下被识别为 `"Android Fastboot"` 的刷机失败 | `cmd/enginex-installer` 单测 `TestFastbootScanAndroidFastboot` | 需报告人 / PRIME 4+ (JC11S) | 自动化通过 |
| **FX-5303-47** (#132, 重发1) | 修复二代机关于页不显示访问码、OTA 报 GitHub unreachable（systemd 独立单元隔离 cgroup） | `pkg/webui` 单测 `TestGenerateAccessCode`、`test_sclive4_bootstrap.sh` | 需报告人 / 二代机（SC LIVE 4 等） | 自动化通过 |
| **FX-5303-48** (#116, 重发2) | SYSTEM ONE / PRIME 4 G2 走 1cc5:2027 刷机、--ssh-key、修复关机与访问码（删除 KillMode=process） | `cmd/enginex-installer` 单测、`test_sclive4_bootstrap.sh` | 需报告人 / SYSTEM ONE / PRIME 4 G2 | 自动化通过 |
| **FX-5303-49** (#116/PR #139, 重发3) | 修复 SYSTEM ONE / PRIME 4 G2 64 位 hook 加载无限重启（near_data 符号扩展溢出修复） | `tools/ui-mod/test_az04_hook.c`、`AZ04=1 test_sclive4_bootstrap.sh` | 需报告人 / SYSTEM ONE / PRIME 4 G2 | 自动化通过 |
| **FX-5303-50** (#138, 重发3) | Shoutcast 直播表单用户名保存、数字流 ID 登录与空密码保留 | `pkg/broadcast` 单测 `TestBroadcastUsernamePersistsAcrossReload` | SC5000 控制台配置 Shoutcast 推流启动，表单内容不消失 | 自动化通过 |
| **FX-5303-51** (Mod Hub, 重发3) | Web 控制台内上传安装/卸载 `.tar.gz` Mod 与 5 个官方 Mod 发布 | `pkg/mods` 单测、`docs/mods/test_mods.py` | SC5000 控制台上传并启用 Mod，刷新页面生效，随后卸载 | 自动化通过 |
| **FX-5303-52** (#129/#133, 重发1/2) | 安装器刷写时实时显示 fastboot 百分比进度、传输字节数与大分区写入提示 | `cmd/enginex-installer` 全套单测 | 电脑安装器线刷时观察终端实时进度刷新 | 自动化通过 |
| **FX-5303-53** (#135/#147) | 10 级等比几何波形缩放与触控/旋钮 Beat Jump 联动，修复 swap_loop_order 判定 | `tests/test_rootfs_overlay.sh` (ok 16)、`tools/az01-packer/test_ui_mod.py` | SC5000 旋转 Browse 旋钮确认 10 档平滑缩放；旋钮联动跳拍 | 自动化通过 |
| **FX-5303-54** (#136/#140/#142) | 异常断电修复（清理损坏 QML 磁盘缓存）、engine-guardian 守卫与演出中 Wi-Fi 弹窗抑制 | `tests/test_rootfs_overlay.sh` (ok 15) | SC5000 打碟时断开 Wi-Fi 路由，确认不弹出打断打碟的遮挡弹窗 | 自动化通过 |
| **FX-5303-55** (#143/#145/#148/#149) | 组件统一命名 enginex-*、局域网 stemsd 算力状态探测与 AZ04 64 位支持 | `tests/test_rootfs_overlay.sh` (ok 17)、`test_az04_hook.c` | SC5000 设置 -> 关于，查看 Stems 算力节点连接状态 | 自动化通过 |
| **FX-5303-56** (#146) | PRIME GO / Mixstream 屏幕标题栏新增上一首/下一首触控键（播放中锁定防误触） | `tests/test_rootfs_overlay.sh` (ok 18) | 需报告人 / PRIME GO (JP11) 或 Mixstream | 自动化通过 |
| **FX-5303-57** (#150/#156) | 流媒体加固：SoundCloud 400 回退公开 API、Spotify 纯 Go mDNS 广播、Apple Music 新版 JWT | `pkg/providers` 全套单测 | SC5000 登录各流媒体平台测试歌单与曲目点播 | 自动化通过 |
| **FX-5303-58** (#151) | CDJ-3000 风格下一枚 Hot Cue 倒计时胶囊（BARS.BEATS 原厂扁平白风格） | `tools/az01-packer/test_ui_mod.py` | SC5000 播放曲目，观察倒计时胶囊随拍子递减，<4 小节琥珀高亮 | 自动化通过 |
| **FX-5303-59** (#152) | 一体机实体三段 EQ 旋钮映射 Stems 分轨音量，支持 Shift + Cue 硬件快捷切换 | `tools/ui-mod/test_stems_feature.c` | 需报告人 / PRIME 4 / PRIME 2 / SC LIVE 4 | 自动化通过 |
| **FX-5303-61** (#159) | 修复流媒体曲目已下载落盘却报 provider disconnected from Engine DJ profile（SSO 补丁） | `tools/ui-mod/test_stems_feature.c`、`test_az04_hook.c` | SC5000 下载流媒体曲目加载上台面，确认瞬时加载成功无超时 | 自动化通过 |
| **FX-5303-62** (#160) | PRIME 4 / PRIME 4+ 与 SC5000/SC6000 跨机型局域网四轨联动，自动空闲映射 Deck 3/4 | `pkg/webui` 单测 `TestShadowComposeMarksLoadingAndUnloads` | 需报告人 / PRIME 4 联调 SC5000 | 自动化通过 |
| **FX-5304-01** (#49) | 四轨联动完整脱离卸载（UDP 告别包、350ms 防抖卸载、交回 Master、清空波形）与真实歌名显示 | `pkg/webui/shadow_test.go` | 双 SC5000 联动，右机关机或退歌，左机 350ms 自动卸载并清空残存波形 | 自动化通过 |
| **FX-5304-02** | UI 降载：新增 scTr QML translator 缓存与 QuickCue 500ms 节流，消除按键微卡 | 无自动化覆盖（QML 渲染降载优化） | SC5000 连续高速拍打 MASTER / SYNC 键，观察界面平稳无瞬时微卡 | 无自动化覆盖（待实机核验） |
| **FX-5304-03** | 歌单左滑 Stems 统一体验：自动识别原厂带 Stem 动作的本地曲目并本地化为单一按钮 | `tools/az01-packer/test_ui_mod.py` | SC5000 在曲库向左滑动本地与流媒体曲目，均显示「分轨 Stems」 | 自动化通过 |
| **FX-5304-04** (#164) | Mixstream Pro+ 安装器支持 UDP 11224 EAAS 发现、SO_REUSEADDR 端口复用与 15e4:303f 识别 | `cmd/enginex-installer` 单测 `TestDiscoveryListenersSharePortsWithSOReuseAddr` 等 | 需报告人 / Mixstream Pro+ (NH08S) | 自动化通过（报告人真机已验证） |
| **FX-5304-05** | 一代机电脑线刷修复：fastboot getvar product 返回 Variable not implemented 时按 USB ID 识别、unpack 免 debugfs | `cmd/enginex-installer` 单测、`test_az01_packer.py` | 电脑连接处于 Update Mode 的 SC5000，运行安装器线刷验证 | 自动化通过 |
| **FX-5304-06** (#161) | SC LIVE 4 Fastboot setup 与 Windows pnputil 设备解析 | `cmd/enginex-installer` 单测 `TestFastbootScanPnputil` | 需报告人 / SC LIVE 4 | 自动化通过 |
| **FX-5304-07** (#166) | 恢复 Mixstream 系列原生 Sweep FX 旋钮调制与触控控件 | `tools/az01-packer/test_sclive4_bootstrap.sh` (EDITS=20) | 需报告人 / Mixstream 系列 | 自动化通过 |
| **FX-5304-08** (#168) | 实体 FX 选择器新增 per-unit PX 效果、修复重新武装与湿声平滑淡入淡出防爆音 | `pkg/webui/ui_settings_test.go` | SC5000 旋转 Beat FX 旋钮切换效果并调节 Wet/Dry，声音平滑过渡 | 自动化通过 |
| **FX-5304-09** (#169) | 修复 PRIME 4+ (JC11S) USB 刷写成功后重启回原厂 5.3.0 的问题（校验 data 目标与激活文件） | `cmd/enginex-installer` 单测、`test_jc11s_firstboot.sh` | 需报告人 / PRIME 4+ (JC11S) | 自动化通过 |
| **FX-5304-10** (#171) | SC LIVE 4 AUX 输入路由至原生通道 3/4 并保留 MIC2 麦克风输入 | `tools/az01-packer/test_sclive4_bootstrap.sh` (AUX 沙箱断言) | 需报告人 / SC LIVE 4 | 自动化通过 |
| **FX-5304-11** | 安装器支持识别 Mixstream Pro Go USB ID `15e4:2059` | `cmd/enginex-installer` 单测 `TestMixstreamProGoUSBDetection` | 需报告人 / Mixstream Pro Go | 自动化通过 |
| **FX-5304-12** | 提供一键只读诊断脚本（Windows batch 与 Linux/macOS bash），方便用户一键排查环境 | 脚本静态检查 | 电脑双击运行诊断脚本输出完整诊断报告 | 自动化通过 |
| **FX-5304-13** | 影子波形渲染性能重构：移除逐轮询绑定级联、采用渲染线程平滑滚动并大幅减少 GC | 无自动化覆盖（QML 渲染线程优化） | 双 SC5000 联动长时间播放，观察波形滚动持续 60 FPS 稳定无掉帧 | 无自动化覆盖（待实机核验） |
| **FX-5304-14** | 空影子槽不启动层时钟，心跳包不递增版本号或触发 QML 无效重新解析 | `pkg/webui/shadow_test.go` | SC5000 开启四轨模式但无对端时，后台空载 CPU 接近 0% | 自动化通过 |
| **FX-5304-15** | 缓存 CDJ 静态显示资产（版本化过滤与防重复请求），保留位置轮询 | `pkg/prolink` 单测 `TestCDJResponseCacheRegression` | 需报告人 / CDJ-3000 | 自动化通过 |
| **FX-5304-16** | 搜索工作线程池上限控制与繁忙重试机制，防止快速输入导致线程耗尽卡死界面 | `pkg/providers` 单测 `TestSearchWorkerPoolAndThrottling` | SC5000 曲库搜索栏快速连续输入/删除字符，界面响应流畅不卡死 | 自动化通过 |
| **FX-5304-17** | 挂起不安全的批量曲库后台自动分析，防止多核资源饥饿导致打碟卡顿 | `tests/test_rootfs_overlay.sh` (ok 06) | 插入包含数千首未分析曲目的 U 盘，打碟操作流畅不受后台分析阻塞 | 自动化通过 |
| **FX-5304-18** | Beatport 冷缓存查找保留无损升级格式，防止冷启动降级为压缩音质 | `pkg/providers` 单测 `TestBeatportLosslessUpgradeOnColdCache` | SC5000 冷启动后点播 Beatport 曲目，核验下载为无损格式 | 自动化通过 |
| **FX-5304-19** | deemixd 在 stream-sync 已监听 6595 端口时正常退出（退出码 0），消除冲突假告警 | `internal/cli` 单测 `TestDeemixdExitsZeroWhenPortOccupied` | SC5000 查看 sc5000-deemixd 状态为正常 inactive，无红色报错 | 自动化通过 |
| **FX-5304-20** | 核心服务彻底剥离内置 DMX 代码，灯光功能完全模块化迁移至 dmx-studio mod | `go test -race ./...`、`pkg/mods` 单测 | 开启原厂 SoundSwitch 确认不受影响；安装 dmx-studio 验证独立工作 | 自动化通过 |
| **FX-5304-21** | 封面重试逻辑移出 QML 状态机，检测到加载失败立即标记，避免 GUI 死循环卡死 | `tools/az01-packer/test_ui_mod.py` | SC5000 浏览含损坏封面图片的曲目，列表快速划过不卡死 | 自动化通过 |
| **FX-5304-22** | 限制极端节拍锚点坐标在安全范围内，防止极端 BPM 导致内存分配异常 | `pkg/analyzer` 单测 | 无需实机 | 自动化通过 |
| **FX-5304-23** | 隔离损坏的性能分析字段，安全替换生成文件防数据库写入损坏 | `pkg/enginedb` 单测 | 无需实机 | 自动化通过 |
| **FX-5304-24** | 限制大曲库浏览刷新内存预算，防止超大曲库击穿有效内存缓存 | `pkg/enginedb` 单测 `TestGatewayBrowseBudgets` | SC5000 快速滚动包含上万首歌曲的大曲库，内存平稳 | 自动化通过 |
| **FX-5304-25** | 独立流媒体搜索结果分页限制与缓存预算，防止超大搜索结果占满内存 | `pkg/providers` 单测 `TestSearchCacheBudgets` | SC5000 在流媒体分类下执行大词模糊搜索，翻页流畅 | 自动化通过 |

---

## 三、只能实机验证的「实机清单」（供 PM 测试）

### 1. SC5000 Prime（JP07，两台测试机）实机验证清单

PM 可直接拿两台 SC5000（`<deck-1>` 与 `<deck-2>`，千兆有线互联）按以下步骤逐项核验：

| 序号 | 验证项目 | 在 SC5000（JP07）上的操作步骤 | 预期看到的正确现象 |
| --- | --- | --- | --- |
| **HD-01** | **四轨脱离与残存波形清空 (#49)** | 两台 SC5000 连网线，开启四轨联动；左机为主控，右机播放一首曲目使左机呈现影子轨。操作右机关机或在右机长按 Eject 退出曲目。 | 右机离线或退歌后约 350ms 内，左机屏幕影子轨自动卸载（Unload + UnSync），残存波形与曲目信息瞬间清空，左机平稳接管 Master，无死锁、无残留。 |
| **HD-02** | **UI 降载与高频操作防微卡** | 在正在播放曲目的 SC5000 上，单指或双指高速连续拍击物理 `MASTER` 键与 `SYNC` 键（>5次/秒）。 | 界面主波形平稳滚动（稳定 60 FPS），屏幕无瞬时卡顿、掉帧或微停顿现象。 |
| **HD-03** | **歌单左滑统一分轨 Stems** | 进入曲库浏览界面，分别选择一首本地 U 盘曲目和一首流媒体曲目，向左轻滑该曲目条目。 | 两类曲目均统一弹出单一的「分轨 Stems」（繁中为「分軌 Stems」，英文为「Stems」）按钮，无重复按钮，点击后能正常触发分轨。 |
| **HD-04** | **Browse 旋钮 10 级几何波形缩放 (#147/#135)** | 在波形播放界面，直接旋转右侧的 Browse 编码器旋钮。 | 主波形以 10 档平滑等比放大/缩小，每格缩放清晰可控，无以前 60+ 档微抖动与漂移感。 |
| **HD-05** | **Auto Loop 旋钮与 Beat Jump 联动** | 旋转物理 Auto Loop 旋钮切换 Loop 大小（如 4 拍、8 拍、16 拍）。 | 屏幕上的 Beat Jump 长度随之同步联动改变，无需再进二级菜单手动绑定。 |
| **HD-07** | **双列曲库浏览与 Sync Guard (#80)** | 进入设置开启 `Dual-Column Browser` 与 `Sync Guard 10%`。返回曲库，并尝试同步两首 BPM 相差 >15% 的歌曲。 | 曲库右侧常驻清晰显示 BPM 与第二字段；尝试同步大跨度 BPM 时 Sync 按钮受到容差保护拒绝误同步。 |
| **HD-08** | **Pad LED 软硬件同步调光 (#85)** | 进入 `设置 → 设备 → Pad 灯亮度`，在 100%、80%、60%、40%、20% 之间切换。 | 屏幕虚拟打击垫亮度与机器下方 8 颗物理硅胶打击垫的硬件背光亮度同步等比变暗或变亮。 |
| **HD-09** | **推子后 Echo Out 尾音与 Color FX (#88/#104)** | 进入设置开启 `Fader Echo (All Faders)` 与 `Color FX (Space)`。播放曲目时将音量推子快速拉到底推至静音；旋转 Sweep 旋钮。 | 快速拉下推子后，音箱中听到自然衰减的推子后回音尾音，无突兀截断或爆音；Sweep 旋钮呈现先锋 Space 混响空间感。 |
| **HD-10** | **全局界面缩放 UI Scale 居中适配 (#117)** | 进入 `设置 → 设备 → UI Scale`，选择 85% 或 75%。下滑呼出控制中心，点击 FX 选择器弹窗。 | 界面整体等比缩小居中，控制中心全屏抽屉和弹窗均紧贴屏幕边缘无半透明黑带或留白截断，无黑边。 |
| **HD-11** | **演出中后台网络波动弹窗抑制 (#142)** | 在播放曲目过程中，拔掉碟机连接的局域网网线或关闭 Wi-Fi 路由器。 | 屏幕保持打碟界面，**绝不弹出「Not connected to Wi-Fi」等强打断式遮挡模态弹窗**。 |
| **HD-12** | **曲库搜索栏高速输入抗卡死** | 在曲库搜索栏中快速连续输入英文字符并快速连按退格键删除（模拟用户拼写纠错）。 | 搜索框与界面实时平滑响应，后台工作线程池平稳调度，界面无冻结、假死或无响应现象。 |
| **HD-13** | **大容量未分析曲库即插即用防饿死** | 插入一个包含 2000 首以上未预先分析曲目的 U 盘，在主界面立即选曲播放。 | 碟机立刻秒级载入并平稳播放音频，后台不会因抢占全部 CPU 核跑批量分析而导致打碟界面卡顿。 |
| **HD-14** | **CDJ-3000 风格下一枚 Hot Cue 倒计时胶囊 (#151)** | 为曲目设置一个位于 32 小节后的 Hot Cue 2，从开头播放该曲目。 | 屏幕曲目信息栏出现扁平原厂胶囊显示 `| ▊ DROP 32.0` 并倒数；距离小于 4 小节时变琥珀色高亮提醒。 |
| **HD-15** | **流媒体曲目下载后立即加载成功 (#159)** | 在流媒体列表点播一首未缓存的曲目，等待下载进度完成。 | 曲目下载完成后立即成功加载上轨道，**不再报「provider disconnected from Engine DJ profile」**，无需等待 150 秒超时。 |
| **HD-16** | **四轨转盘小屏与 Deck 3/4 灯条 (#90/#91)** | 开启四轨模式，切换 Deck 3 与 Deck 4。 | 观察转盘中心屏幕正常显示当前层封面与播放进度环（不卡在 Engine 静态 Logo）；Deck 3/4 颜色条正常显示。 |
| **HD-17** | **流媒体歌单排序持久化 (#98)** | 打开流媒体歌单，在排序菜单中按 BPM 升序排列，退出歌单再次进入。 | 歌单仍保持按 BPM 排序，排序状态与顺序正确保持。 |
| **HD-18** | **一代机 USB 电脑线刷验证** | 将 SC5000 关机，长按 Media 键开机进入 Computer Update Mode，用 USB-B 线连电脑，运行安装器模式 B。 | 安装器能正确根据 USB ID `15e4:d006` 识别 SC5000 并完成线刷引导。 |

---

### 2. 其他机型实机验证清单（标「需报告人」）

以下项目因涉及特定硬件物理旋钮、ADC 线路或二代机独立架构，需由对应机型报告人或拥有对应机型的团队成员核验：

| 序号 | 验证项目 | 对应机型 | 验证说明与操作建议 |
| --- | --- | --- | --- |
| **REP-01** | **Mixstream Pro+ 安装与 EAAS 发现 (#164)** | Numark Mixstream Pro+ (NH08S) | **需报告人**（报告人 `@yolocd1s` 已用 WSL 2 + usbipd-win 成功刷入验证；待核验 OTA 在线升级） |
| **REP-02** | **Mixstream 物理 Filter 旋钮与 Sweep FX (#166)** | Numark Mixstream Pro / Pro+ / Go | **需报告人**：旋转机器上的实体 Filter 旋钮，核验是否能正常调制 Sweep FX 滤波效果；屏幕触控 Beat Jump 按钮是否可点击 |
| **REP-03** | **SC LIVE 4 AUX 输入路由至通道 3/4 (#171)** | Denon DJ SC LIVE 4 (JP21) | **需报告人**：在背部 AUX 接口接入外置音源，将通道 3/4 切到 AUX，核验耳机 CUE 监听、音量推子与 Sweep FX 是否起效，MIC2 是否保留 |
| **REP-04** | **SC LIVE 4 Fastboot setup 与 Windows 驱动 (#161)** | Denon DJ SC LIVE 4 (JP21) | **需报告人**：在 Windows 10/11 下连接处于更新模式的 SC LIVE 4，验证安装器是否能通过 pnputil 自动识别设备 |
| **REP-05** | **PRIME 4+ USB 线刷后重启防回退 (#169)** | Denon DJ PRIME 4+ (JC11S) | **需报告人**：使用安装器线刷 PRIME 4+，重启后核验是否正常进入 EngineX OS，不再自动回退到官方 stock 5.3.0 |
| **REP-06** | **PRIME 4 蓝牙输入与输出双向切换 (#81)** | Denon DJ PRIME 4 / PRIME 4+ | **需报告人**：在设置中切换 Bluetooth In (A2DP Sink) 与 Bluetooth Out (A2DP Source)，分别连接手机和蓝牙音箱测试 |
| **REP-07** | **实体三段 EQ 旋钮映射 Stems 音量 (#152)** | PRIME 4 / PRIME 2 / SC LIVE 4 | **需报告人**：按 Shift + Cue (PFL) 将 High/Mid/Low 旋钮切换为 Stems 分轨旋钮，旋转调节 Vocal/Melody/Drums 音量 |
| **REP-08** | **PRIME 4 局域网联动 SC5000 跨机四轨 (#160)** | PRIME 4 + SC5000 | **需报告人**：将一台 SC5000 接入 PRIME 4 局域网并设为 Deck 1，核验 PRIME 4 自动将 SC5000 映射至空闲的 Deck 3/4 并同步波形 |
| **REP-09** | **SYSTEM ONE / PRIME 4 G2 USB 直刷 (#116/#139)** | Rane SYSTEM ONE / PRIME 4 G2 (AZ04) | **需报告人**：进入 1cc5:2027 固件更新模式，验证 64 位 hook 加载后系统正常启动进主界面，无无限重启 |
| **REP-10** | **PRIME GO 屏幕标题栏触控切歌按键 (#146)** | Denon DJ PRIME GO (JP11) | **需报告人**：检查屏幕顶部曲目名两侧是否出现 `‹` / `›` 触控跳歌键，停止时可点击切歌，播放中自动锁定 |
| **REP-11** | **先锋 CDJ-3000 Pro DJ Link 联调 (#96/#100)** | 先锋 CDJ-3000 + SC5000/6000 | **需报告人**：将 CDJ-3000 与碟机连入同一交换机，核验 CDJ-3000 能否通过 NFS 浏览碟机缓存曲目，以及影子轨跳拍同步 |
| **REP-12** | **Mixstream Pro Go USB 识别** | Numark Mixstream Pro Go (NH10) | **需报告人**：进入更新模式后连接电脑，核验安装器是否能识别 USB ID `15e4:2059` 并匹配数据包 |

---

## 四、核验与交付统计汇总

- **修复项总数**：**103 项**（包含 BUGFIX-REGRESSIONS 17 项、5.3.0.3 首次发布与重发保留 61 项、5.3.0.4 新增与集成 25 项）
- **自动化通过数量**：**101 项**（涵盖单测、沙箱脚本、C 运行时测试及系统 overlay 断言）
- **自动化失败数量**：**0 项**（无任何测试报错或失败）
- **无自动化覆盖数量**：**2 项**（FX-5304-02 QML translator 缓存与 QuickCue 节流、FX-5304-13 影子波形平滑滚动与 GC 优化，属于纯客户端 QML 渲染与帧率降载优化，需由实机帧率核验）
- **失败项详情**：**无**
- **只能实机验证的实机清单条数**：
  - **SC5000（JP07）实机清单**：**17 条**（HD-01 ~ HD-18，剔除恢复原厂行为的 HD-06 / #157）
  - **其他机型清单（需报告人）**：**12 条**（REP-01 ~ REP-12）
- **验证提交号（HEAD）**：`6167c30dc927ff5f6af9d29edad83c387becbe12`（分支 `agent/regression-5304-20261110`）
