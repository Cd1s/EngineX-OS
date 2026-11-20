# EngineX OS 5.3.0.5 Release and Issue Fix Record (Phase D dry-run, 2026-11-20)

> Historical merge records remain below. The Phase D final evidence at the end of this file records the rc4 build from `fad548b9484209710abad7d9ca1ea61e78edd295`, the image checks, the three release gates, and the checklist status. Publication was not performed, and real-deck work remains pending where stated.

- **Date**: Phase D dry-run `2026-11-20` (UTC+7); publication was not performed
- **Scope**: `v5.3.0.4-r5` (`9e827205`) to `v5.3.0.5`, with the rc4 hot-cue/PerfGen, Deezer OmniSource, and RGB Link shadow overview fixes on `agent/5305-rc2`
- **Build commit**: `fad548b9484209710abad7d9ca1ea61e78edd295`
- **Rollback point**: tag `v5.3.0.4-r5` (`9e827205ae331d2d896d97207032556dc4202af5`)
- **Artifacts**: 28 release artifacts plus `RELEASE-TAG.txt`, `SHA256SUMS.txt`, and `SHA256SUMS.txt.sig`, 31 files in total
- **Signature**: the rc4 manifest was verified with the release signature; the private signing key was not copied into this repository or evidence
- **Publication status**: no tag, GitHub Release, issue comment, or push to `main` was performed

---

## 1. Issue 与 PR 修复总表（v5.3.0.4-r5 → v5.3.0.5）

| Issue / PR | 模块 | 原因与修法摘要 | 验证与回归测试 |
| --- | --- | --- | --- |
| **#255 / #239**（原生安装器与签名更新） | `cmd/enginex-installer/`<br>`tools/az01-packer/`<br>`pkg/webui/update*.go` | **原因**：安装器依赖主机上的 Python、fastboot、e2fsprogs、libusb 或 WSL；Release 资产可以被替换，而碟机更新只信任发布页给出的文件。<br>**修法**：安装器用 Go 直接准备镜像并经 USB 传输（Windows 走系统 WinUSB）；每个 Release 的 `SHA256SUMS.txt` 带 Ed25519 签名 `SHA256SUMS.txt.sig`，屏幕更新和安装器先验签再核对 SHA-256，无签名、签名无效、被改动或比碟机版本旧的发布一律拒绝；5.3.0.4 及更早的碟机不做该检查。安装器旁有同版本机型镜像时优先使用并要求同发布的 `SHA256SUMS.txt`、`.sig`、`RELEASE-TAG.txt`。 | 检验：`cmd/enginex-installer` 与 `pkg/webui` 的 Go 测试、`update_sign_test.go`；真机 WinUSB 绑定、USB 传输和刷入后激活需要实机（检查表行 215、216、218）。 |
| **恢复官方固件**（含原厂卡 Logo 的 QML 缓存故障） | `cmd/enginex-installer/restore_stock.go`<br>`tools/ui-mod/`（QML 缓存隔离）<br>`tools/az01-packer/build-installer.sh` | **原因**：EngineX 与原厂共用 QML 磁盘缓存，原厂刷机保留 `/data`，损坏的缓存让原厂启动崩溃（卡 Logo）。<br>**修法**：EngineX 的 QML 缓存放到 `ui/qml-cache`，启动时清理旧共享缓存；`Restore-Official-Firmware.*`（`--restore-stock`）先清共享缓存，再校验官方 5.3.0 校验和与 AZ01 板号后刷入；二代机走 LAN 卸载。回到原厂前请先启动一次 5.3.0.5。 | 检验：`restore_stock` Go 测试、ARM 固件沙箱；真机官方刷入与返回 EngineX 需维护者进入更新模式（行 201、217）。 |
| **蓝牙音箱输出与质量 / 延迟调优** | `tools/ui-mod/engine_ui_hook.c`<br>`tools/ui-mod/build_packs.py`<br>`pkg/webui/` | **原因**：Engine 自带的 A2DP 输出源原先拿到的是静音，面板也无法选择输出，另有环形缓冲回绕问题。<br>**修法**：钩子把 `bt_out=` 所选的输出对以 A2DP 流自身的采样率发给音箱，仅在有音箱收听时发送；原厂蓝牙面板 `输入分配到` 下新增 `输出来源`；网页控制台同步设置。蓝牙约增加 150–250 ms，不用于打拍对齐。 | 检验：UI 包与 C 测试；维护者 2026-11-14 在真实 SC5000 上试听并认可音质；其他机型的 `输出来源` 选项与已连接手机时的面板提示仍待确认（行 187、188）。 |
| **#205**（碟机之间复制设置、USB 配置导入） | `pkg/webui/`<br>`tools/ui-mod/build_packs.py`<br>`tools/ui-mod/i18n.tsv` | **原因**：没有安全的跨碟机复制设置方式；r5 把原厂「加载保存的配置」提示自动答成 No，U 盘配置从未导入。<br>**修法**：来源碟机允许、接收碟机确认后只复制白名单偏好，Player 编号、角色、颜色、Name/Product、网络身份与账号保留本机；撤销按字节恢复；`Profile → Save to my drive` 同时导出 EngineX 偏好；恢复原厂 Yes / No 提示；机身菜单重启改用原厂正常退出以保存 Player 编号。 | 检验：UI 包与 Go 测试；两台真机的 Link / USB / 撤销验收待做（行 200、202、210）。 |
| **#213 / PR #214**（分侧反转转盘） | `pkg/webui/ui_settings.go`<br>`tools/ui-mod/build_packs.py` | **需求**：转盘方向要能按碟机分别设置。<br>**修法**：`设置 → 设备` 与 `:8080` 增加 `反转转盘方向`（双转盘机型左右各一个），即时作用于顶部搓碟、外圈微调、网格 / Loop 微调与 `Shift + 转盘` 跳曲；电动转盘机型隐藏。**2026-11-16 晚的后续**：一体机上两个 QML 引擎之间的状态不同步（左右转盘设置在 `HighSpeedJogWheelAssignment` 上不生效），改为跨引擎轮询；SC5000 / SC6000 这类独立播放器本来就没有这个功能，现在在设置里**隐藏**且忽略它（见 1A）。 | 检验：`test_ui_mod.py`、原生 chroot QML 校验；实机物理转动待做（行 205、276）。 |
| **#258**（所有一体机 `推子回音`） | `tools/ui-mod/engine_ui_hook.c`<br>`tools/ui-mod/build_packs.py` | **原因**：旧的主总线 PCM 回音与原厂逐通道 Transition FX 叠加，造成堆叠回音与卡顿。<br>**修法**：全部 5.3.0 一体机旁路旧回音，使用原厂逐通道 Transition FX；独立播放器保持旁路。 | 检验：C 与 Python 套件；真机一体机音频验收待做（行 190）。 |
| **#263**（电动转盘 4 轨布局） | `tools/ui-mod/build_packs.py` | **原因**：原厂电动模块只对 `Repeater { model: [1, 2] }` 的碟位运行马达并设置 `ExternalPlatterLayerActive`，`4 Decks` / `Paired Player` 下转盘不动、微调无效。<br>**修法**：为本机持有的碟位启用马达与触摸微调。 | 检验：Go / Python 套件；`SC5000M` / `SC6000M` 实机待验（行 186）。 |
| **#261 / PR #266**（Mixstream 打击垫栏） | `tools/ui-mod/build_packs.py` | **原因**：Sweep FX 与 Stems EQ 在 Beat Jump 组打开时互相重叠。<br>**修法**：重排 `Pads.qml`，两者按 Beat Jump 组状态动态切换列数，原生打击垫与跳拍同时可用。 | 检验：UI 包套件（行 204）。 |
| **#238**（碟机本机 Stems 分离） | `internal/stems/separate.go` | **原因**：旧方案只在 250 Hz 与 4 kHz 处切频段，在 MUSDB18 片段上每个分轨都低于 0 dB。<br>**修法**：`LocalRender` 改用 STFT 软掩码分离器，人声 / 贝斯 / 鼓 / 旋律达到固定的信号失真比下限；不是神经网络，仍有串音；人声保持原有的立体声位置；只提供 `hpss` 的电脑 / 中继不再使用（碟机自己的分离得分更高），Stems 状态行写明所用来源；Stems 上传停止进展时释放名额，慢但仍在工作的渲染端保留任务（#243）。实测 60 秒音乐在空闲 SC5000 上约 16.5–17.0 s（人声立体声化前），构建机 armhf 核心上多约 6%。 | 检验：`internal/stems` Go 套件；碟机播放中的耗时未测（行 222）。 |
| **#268**（演出曲目表导出） | `pkg/webui/` | **原因**：无法把已记录的演出导出为曲目表。<br>**修法**：认证后的文本 / CSV 下载，在 Engine 持有曲库时只读原厂 `hm.db` 历史，保留演出边界、播放顺序、重复曲目，处理已提交 WAL 与未知时钟，绝不创建或修改历史库。Remote Library 记录与 Prepare 仍待查。 | 检验：`pkg/webui` 的 `TestTracklist268Export`；真机 SC LIVE 4 对照原厂 History 待做（行 206）。 |
| **3-Band / 概览**（rekordbox 配色、概览高度、跨读取补全） | `tools/ui-mod/build_packs.py`<br>`pkg/prolink/`<br>`pkg/bridge/`<br>`pkg/rekordbox/` | **原因**：`3-Band` 与 rekordbox / CDJ-3000 配色不同；rb-bridge 在 `overviewWaveFormData` 末尾写零，Engine 按未缩放字节只画约三分之一高度，QML 再放大；PWV7 三频段被按错误顺序（中 / 高 / 低）旋转；已播放过的曲目与导出读取超过挂载时限的卡留有零尾。<br>**修法**：配色改为低频蓝、中频橙、高频白，静音中频褪为棕；overview 写入三个频段最大值；PWV7 按低 / 中 / 高读取；先补全概览再读导出，补全可跨挂载续做。 | 检验：`ThreeBandPaletteTest`、eglfs 沙箱；实机对照 CDJ-3000 / rekordbox 与旧导入 U 盘重插待做（行 194–196、203、219）。 |
| **Live Stream 只输出 WAV、按需采集** | `pkg/broadcast/`<br>`pkg/webui/`<br>`tools/ui-mod/` | **原因**：控制台提供 MP3 / OGG，但镜像没有这两种编码器，默认 `mp3` 永远连不上；没有收听者时仍持续采集并重采样，空闲碟机约占 7.3% 单核。<br>**修法**：固定 WAV（16 位、48 kHz、立体声）；旧设置自动转 WAV，Shoutcast 目标不再推送，`/live.mp3` 重定向到 `/live.wav`；没有 `/live.wav` 听众、Icecast 服务器与 LAN 流时不采集。 | 检验：`go test -race ./pkg/broadcast/... ./pkg/webui/...`；真实 Icecast 服务器播放待做（行 197、223）。 |
| **有线 + Wi-Fi 同网段** | `rootfs-overlay/`（路由守护与 sysctl）<br>`pkg/webui/` | **原因**：两台 SC5000 同时走有线与 Wi-Fi 时，Link 曲目走了 Wi-Fi（13–19 MB 曲目加载 7.4–11.3 s）；网线地址晚于 Wi-Fi 到达时，Engine 报 `online check failed` 并断开 Wi-Fi；两张网卡互相应答对方地址。<br>**修法**：同网段时 Link 路由走 `eth0`；路由守护让 Wi-Fi 保持；每个接口 `arp_announce=2`、`arp_ignore=1` 从启动起生效。 | 检验：overlay 套件；两台真机多次开机验证待做（行 192、193、213）。 |
| **#223**（Remote Library 列表封面） | `tools/ui-mod/build_packs.py` | **原因**：`BrowseView` 的 `AlbumArt.qml` 把带来源标签的行都发给网关 `/v1/enginex/art`，而它只认自己列表里的封面，返回 404，列表出现灰块。<br>**修法**：只有 Engine 自己没有封面（`""` 或 `image://streaming/`）时才询问网关，Engine 已有的封面不再被丢弃。 | 检验：Step 2 套件；SC LIVE 4 真机验收待做（行 185）。 |
| **#264 / #272 / #252 / #216 / #246**（大 U 盘与 rekordbox 导入） | `pkg/compat/`<br>`pkg/bridge/`<br>`pkg/enginedb/`<br>`internal/cli/` | **原因**：大型导入重复 `Lstat` 数万次、桥接状态在 FAT32 上可超 4 GiB、挂载时限被占满；Pro DJ Link 暂存库可把存储卡占满（58.6 GiB 卡被占 50 GiB）；一代机的 pack 把原厂的网络盘判断改成恒为真，对方播放器的盘出现 Stems 入口（Engine 无法写入 Link 来源，报「空间不足」）；旧导入补全时重写曲目 / 歌单 ID、丢失碟机上的编辑；旧 MP3 网格偏移。<br>**修法**：解析器缓存路径查询并用 `DirEntry.Type()`；`BridgeAnalysis` 行数按 3 GiB 封顶；暂存库按盘限量与清理；Stems 滑动入口与 Stems 列表只在本机自己的盘上出现（与原厂一致）；按已保存来源记录匹配旧行并保留已播放标记、Cue、Loop 与歌单 ID；MP3 网格迁移与共享挂载时限；导入不再覆盖 Engine 同时对曲库做的改动。 | 检验：Go 套件（`-race`）、覆盖层与打包套件；真机 SD 重插与数据恢复为独立验收（行 189、191、207、211、212、214）。 |
| **#269 / #277 / #279 / #247 / #248**（流媒体） | `pkg/providers/`<br>`pkg/webui/`<br>`tools/ui-mod/` | **原因**：Spotify 载入会弹出载体、TIDAL 登录载体被挤占；收藏夹里出现 TIDAL 专属空白行且无法进入 My Playlists；原生 Ogg 失去 Engine 认识的下载名；音源设置存在先读后写的丢失更新；歌单第二页失败时整批丢弃。<br>**修法**：保留 Spotify 载入与 TIDAL 载体、识别两种 `TIDAL Rising` 拼写并走原厂 `push()`、用载体补丁的 `mp3` 命名标记而 MIME 仍为 `audio/ogg`、两个写入者改用加锁的 `BPMStore.UpdateConfig`、保留已成功获取的有序前缀；后续页面失败时网关显示已加载的行加一条未完成提示并可重试，保留 401/403 与重新登录提示（#248，行 239）。 | 检验：`pkg/providers`、`pkg/webui` 套件（`TestBrowsePartial248KeepsPrefixAndErrorUntilRetry` 等）；NH08 真机浏览 Spotify 与真机中断后续页待做（行 225、226、239）；#247 的 SoundCloud 保存仍待其所有者完成。 |
| **HFS+ U 盘**（弹出、`*-Update.img` 更新） | `pkg/webui/`<br>`rootfs-overlay/`（updater 路径） | **原因**：Engine 持有 HFS+ 曲库时弹出被拒 26 次且不说明原因；原厂 `az01-update` 给更新环境传了空的 `upgrade-file=`（或去掉了文件夹的文件名），因为 HFS+ 视图是 `/media/<卷>` 上的 tmpfs 绑定，原厂向上找根目录找不到 U 盘自己的根。<br>**修法**：保留真实弹出错误并解除缓存曲库；原厂读取 `/tmp/update-file-path` 之前由 `engine-ui-guard update-file` 把 U 盘上的所选文件映射成 U 盘上的路径（符号链接与越出 U 盘的路径保持原样）；正常模式与安全模式（回退到原厂 Engine）都走同一步骤。 | 检验：Go / overlay 套件；真机 HFS+ U 盘更新需实机（行 209、224）。 |
| **影子碟搓碟** | `pkg/webui/shadow*.go`<br>`tools/ui-mod/build_packs.py` | **原因**：用力或极快（超过 14.4 倍）搓碟时，影子副本按对端约 40 ms 前的位置汇报来导向，出现反向与更宽的位移、冻结与跳转。<br>**修法**：沿源碟自己的路径连续跟随，不超出、不反向。 | 检验：Step 2 套件；双 SC5000 真机搓碟待做（行 198、199）。 |
| **空闲碟机负载** | `rootfs-overlay/`（boot-watch） | **原因**：`engine-ui-guard watch` 每秒 fork `cut`、`cat`、`rm`（约 0.50% 单核），30 秒线程巡检读取约 11 万次系统调用（空闲约 1.16% 单核）。<br>**修法**：boot-watch 改用 shell 内建 `read`；线程巡检整个去掉（见下一条）。 | 检验：沙箱（nspawn）；真机空闲时不再有常驻的 `enginex-perf-tune`（行 220、221）。 |
| **#298**（性能调优精简） | `rootfs-overlay/`（`enginex-perf-tune`、`enginex-debloat`） | **原因**：原厂 5.3.0 自己把音频 DMA 中断放在固定核，并给每个核各绑一个实时音频线程和一个流解码线程；我们再把中断、中断线程、Engine 线程和 `workqueue/cpumask` 重新绑核，会让一台碟机载歌时另一台播放卡顿（#298），线程巡检本身还占空闲 CPU；`enginex-debloat --apply-runtime` 也在移动中断，并把音频中断线程设成 FIFO 99。<br>**修法**：这四项（含 debloat 里的那段）交还原厂；CPU `ondemand` 与 1.2 GHz 下限、GPU 从 300 MHz 起按需（电池或 85 C 以上降到最低）、sysctl、U 盘与 SD 预读（LC003）、I/O 调度、USB/SATA 不休眠（CF018/CF001/LC001）保持不变。维护者决定本版不需要播放发热对比测量，现有 CPU/GPU 时钟策略保留；此项不是待验收事项。 | 检验：`tests/test_rootfs_overlay.sh`（中断掩码、`workqueue/cpumask` 保持原值，不调用 `taskset`/`chrt`，不起后台巡检）、`pkg/dbhealer/debloat_test.go`（debloat 的 runtime 段不动中断）；实机线程/中断摆放与四碟播放载歌仍待最终实机验收（行 312）；DMX/LC001 实机检查由维护者决定本版不需要，不列为待办。 |
| **#242**（可选 HTTPS 控制台） | `pkg/webui/`<br>`docs/site/content.js` | **需求**：控制台只有 HTTP。<br>**修法**：网页控制台「碟机显示与音频」里的 `HTTPS console (:8443)` 打开后，首次生成自签名证书（保存于 `/data/enginex/`，权限 `0600`，SAN 覆盖碟机 IP 与主机名，重启复用），在 `:8443` 同步提供 HTTPS；`:8080` HTTP 保持默认且不变，二维码与屏幕链接仅在开启时给出 `https://`。 | 检验：`pkg/webui` 证书与二维码测试（两种主机名）；r5 迁移保留 `console_https` 与证书字节和权限（行 236）。 |
| **#281**（Remote Load 局域网 API） | `pkg/webui/`<br>`tools/ui-mod/build_packs.py`<br>`docs/remote-load-api.md` | **需求**：局域网内的电脑或脚本无法把歌加载到碟机。<br>**修法**：`设置 → 服务 → 远程加载（局域网 API）` 默认关、只能在碟机上打开；开启后用控制台访问码调用 `/api/remote-load`（规范路径 `/v1/enginex/remote-load`），把 `/media` 下硬盘上的歌加载到空闲碟位。正在播放、联机远端碟位、未挂载或内置盘的文件、经 Cloudflare 隧道的请求都被拒绝；按住 CUE 预听、搓碟和搜索（`PlayState` 不变）按播放位置 1.5 秒内是否移动或转盘是否被触碰判为忙；加载前一刻再检查一次，并要求 Engine 的 `Lock Playing Deck` 为开。 | 检验：`pkg/webui` 与 `RemoteLoadBehaviourTest`；SC LIVE 4 真机验收待做（行 237、238）。 |
| **#283**（设置开关行） | `tools/ui-mod/build_packs.py` | **原因**：EngineX 的二值设置行没有跟随原厂的 `checkedExternally`，点按之后不再反映网页控制台所做的改动。<br>**修法**：13 个开关行全部改用 Engine 原厂开关行，保留各自的状态属性、`key=0/1` 写入与显隐条件。 | 检验：`OnOffSwitchCompatibilityTest`、`UiModSourceTest`（5.0.4 / 5.3.0 两套 QRC、三语）；碟机上逐项点按与网页改动待做（行 227）。 |
| **#234**（更名为 `enginex`，最终版）<br>2026-11-16 决定、11-17 合入候选 | `rootfs-overlay/`<br>`pkg/webui/`<br>`tools/az01-packer/`<br>`internal/leave`<br>`docs/mods/src/` | **原因**：内部名称长期沿用旧项目名；维护者决定 5.3.0.5 把旧名彻底去掉，只保留从 5.3.0.4 及更早版本升级所需的一次性迁移。<br>**修法**：服务单元、程序、`/etc/enginex`、`/run/enginex*`、本机接口 `/v1/enginex` 全部用新名；旧的接口别名、数据目录兼容链接、旧套接字、旧缩放环境变量输出、旧证书域名和**降级还原**全部退役。升级时一次性迁移保留真实数据，改写受管理的存储值，只清除我们自己建的兼容链接，**从不删除**不相关的链接或真实目录；旧的还原操作被拒绝且不动任何数据。升级清理在新一代提交之后才运行；输入无效、升级失败或被中断时，原来的 ALSA 加载器、库和数据布局仍可用；一代 r5 镜像 OTA 与二代 r5 数据包 OTA 必须保持通过。官方 Mod 改用当前名称，只有已保存的暂停记录的迁移实现和它的测试保留例外。ALSA 文件名、钩子符号和 ALSA 文件指向的库路径因为每台已装碟机都靠它们加载 Engine，保持冻结。 | 检验：分支 `agent/5305-rename-final` @ `03b98aee`（19 个提交，合入 `40df1f8a`）。命名守卫 2/2、升级契约 4/4、`go test -race` 全部包通过、覆盖层 38/38、整套 UI 测试通过；二代降级还原的测试被改成断言“拒绝且不动数据”。**实机待验**：从 5.3.0.4-r5 与 5.3.0.2 真机升级一次（一代按镜像、二代按数据包），确认旧数据保留、碟机仍能起来（行 231–236）。 |
| **#215**（屏幕 OTA 只提供 `*-Update.img`） | `pkg/webui/update*.go`<br>`tools/az01-packer/release-gate.sh` | **原因**：5.3.0.5 之后旧版 `*-Data-Bundle.tar.gz` 只应服务仍在旧版本的碟机。<br>**修法**：5.3.0.5 的碟机更新器只发现 `*-Update.img`（包括未解锁的二代机），Release 里仍保留 8 个 Data-Bundle 供旧更新器使用；安装器保留对本地 / 显式旧数据包的兼容；门禁文档覆盖 r5 旧路线与更新的签名更新器。`#244`（AZ04 分区大小）**未**随本次合入。 | 检验：`TestUpdateNeverOffersLegacyBundles`、`TestUpdatePicksGen2ImgWhenUnlocked`、重发与签名 OTA 套件；真机重启与进度待做（行 242）。 |
| **#273 / #253**（压缩分析数据与重复插盘识别） | `pkg/enginedb/`<br>`pkg/bridge/` | **原因**：21,352 首曲目的 FAT32 U 盘上，未压缩的 `BridgeAnalysis` 行平均约 337 KB、共约 7.2 GB；重复插盘为匹配已有曲目而重读每个已存的波形。<br>**修法**：`sourceJSON` / `audioJSON` 用复用的 `gzip.BestSpeed` 写入压缩，`enginedb.UnpackAnalysis` 同时读压缩行与旧版明文行并拒绝超过限制的行；唯一来源 ID 加匹配路径时只用身份元数据，旧共享键、移动 / 互换 / 删除只读有界的 `Track.ID` 前缀，绝不读分析尾部。 | 检验：`pkg/enginedb`（`TestBridgeAnalysisRowsAreCompressedAndLossless` 等）、`pkg/bridge` 大 PDB 重复挂载计时；大 U 盘实际识别速度仍需维护者验收（行 240、241）。 |
| **#237 / #245**（音频审计、Live Stream 保存不断听众） | `tools/ui-mod/test_rt_budget.c`<br>`tools/ui-mod/test_rt_callback.c`<br>`pkg/broadcast/` | **原因**：保存任一 `直播推流` 设置会同时重启采集、推流与局域网流，OBS、VLC 要重连；音频路径代码缺少统一的实时性检查（首次切换主 Pioneer FX 模式时清零 488 KB 未触碰的 BSS 并触发缺页，767 µs 超过 667 µs 的块时间）。<br>**修法**：采集与本地听众继续，只重启外部推流和局域网流；测试在计时前先触碰状态，并取三次相同运行里最短的块时间，预算不变；新增检查钩子音频线程函数不做分配、加锁、等待或 I/O 调用。 | 检验：`pkg/broadcast` `TestSettingsChangeKeepsLocalListeners`、`test_rt_budget.c` 与 `test_rt_callback.c`（本机与 armhf，`banned=0 misses=0`）；真机 `/live.wav` 在 VLC 中改设置不重连待做（行 228–230）。 |
| **#285 / PR #287**（PRIME 4 效果选择） | `tools/ui-mod/build_packs.py`<br>`tools/ui-mod/` 效果选择器 | **原因**：连续转动旋钮浏览效果时选择窗口会关闭，通道 OLED 无法对应所选效果，旋钮每格移动的效果数不是一个。<br>**修法**：连续转动重启 600 ms 自动确认计时，原厂到原厂的选择走原生快速路径，OLED 按发布的 Planck 索引显示全部 31 个原厂与 PX 名称；旋钮每个有效格只发一个 `Plus` / `Minus`，与原厂 `incValue` / `decValue` 一致。 | 检验：`UiModSourceTest.test_prime4_fx_selector_browsing_and_oled_mapping_issue285`、`test_prime4_fx_knob_moves_one_effect_per_detent`、`test_fx_selector_sandbox.sh`；真机 PRIME 4 待验（行 246、247）。 |
| **PR #282**（西班牙语界面，作者 Javier Saenz） | `tools/ui-mod/`<br>`pkg/webui/` | **需求**：没有西班牙语。<br>**修法**：`设置 → 设备 → 语言` 新增 `Español`，`lang=es` 选用西班牙语钩子包，控制台与手机页全部文字有西班牙语，未知语言回退英文。 | 检验：`LanguagePackSelectionTest`、`TestConsoleIsFullyTranslated`、`TestEveryPhoneTextHasEveryLanguage`；真机选 Español 重启后查看菜单重音与排版待做（行 248）。 |
| **一体机原厂视图**（#285 后续） | `tools/ui-mod/build_packs.py` | **原因**：一体机上未保存的视图 1–3 被 `{pads: false}` 覆盖，`SHIFT + VIEW` 不再循环；横向波形旁显示曲库时 `LOAD` 不可用；`联机视图` 锁住通道。<br>**修法**：未保存的视图 1–3 交给原厂 `ViewStateController.resetToDefault`，已保存的视图优先；`compactBrowserVisible` 在横向波形加曲库时保持为真；`联机视图` 只在独立播放器（JP07/08/13/14）上提供并生效。 | 检验：`Prime4ViewsTests`（三项）、`test_prime4_views_sandbox.sh`；真机 PRIME 4 / SC LIVE 4 / Mixstream 待验（行 249–251）。 |
| **高速 SD 卡**（实验性，默认关） | `rootfs-overlay/usr/local/bin/enginex-sd-highspeed`<br>`rootfs-overlay/etc/systemd/system/`<br>`pkg/webui/`<br>`tools/ui-mod/build_packs.py` | **需求**：SD 卡槽以 25 MHz 默认速度运行（最高 12.5 MB/s）。<br>**修法**：`设置 → 设备 → 高速 SD 卡模式`（网页控制台「碟机显示与音频」同一开关）打开后，在开机早于 `edisksd` / `engine` 时以及现场切换时，只对 `ff0c0000` 的 SD 控制器置位 caps 的 `0x4`，空闲时只读地重新初始化，碟机播放时延后并提示「将在下次启动或重新插入 SD 卡时生效」，`dmesg` 出现 mmc 错误或卡消失时撤回；PRIME 4 G2 / SYSTEM ONE 不显示。最高 25 MB/s，SC5000 上实测 23 MB/s。 | 检验：`tests/test_rootfs_overlay.sh`、`TestSdHighSpeedDefaultsOffPersistsAndApplies`、`test_sd_high_speed_switch_rk3288_only_and_four_languages`、数据包测试；真机 RK3288 读速与重启后验证待做（行 252）。 |
| **其他** | `pkg/audioheader/`、`pkg/compat/`、`docs/mods/src/`、`pkg/rekordbox/`、`pkg/stems/` | 带 ID3 的 FLAC / WAV / AIFF 与 VBR MP3 元数据；官方 Mod 在无 FFmpeg 的碟机上可用；OneLibrary Cue 位置溢出；Stems 在网络盘上的开关与原厂一致；回到原厂时不再复用 EngineX 的 QML 字节码。 | 检验：各包 Go 套件（含 armhf）。 |

## 1A. 2026-11-15 至 2026-11-18 合入的修复（补录）

时间为碟机团队本地时间（UTC+7）。「合入」指合入集成分支 `agent/5305-integration`（晚间的一批）或候选分支 `agent/5305-rc-candidate`（16 批）；分支名后的提交是来源分支合入时的分支头，括号里的是合并提交。除“真机”标明的以外，结论都来自沙箱、单元测试和发版门禁；**没有任何一项在真机上完成最终验收**。

### 1A-0 11 月 15–16 日合入集成分支、此前没有记录的项目

| Issue / PR（日期） | 模块 | 原因与修法 | 检验、分支与提交、仍需实机 |
| --- | --- | --- | --- |
| **网页控制台重新设计、远程访问、关于页二维码**（11-15 至 11-16） | `pkg/webui/index.html`<br>`pkg/webui/tunnel.go`<br>`pkg/webui/server.go`<br>`tools/ui-mod/build_packs.py` | **需求**：控制台外观老旧；手机在移动网络上登录流媒体需要公网地址；碟机屏幕上没有直接打开控制台的办法。<br>**修法**：控制台仍是单文件页面，重新设计（四语）；碟机 `设置 → 设备` 新增 `远程访问` 开关，用 Cloudflare Quick Tunnel（`*.trycloudflare.com`）给出临时公网地址；`设置 → 关于 / 更新` 最下面显示二维码 `扫码打开控制台`，内容的优先级是设置里的公开地址、隧道地址、局域网地址，**二维码里只有地址，不含访问码**（`/api/console-qr` 同一顺序）；`远程访问` 开关下面有一行提示指向关于页的二维码。隧道转进来的请求仍被本机专用的 `/v1` 接口拒绝（边缘一定会加的请求头）。 | 检验：`pkg/webui/console_qr_test.go` 的二维码用例、`test_ui_mod` 的关于页布局用例；检查表行 253。提交 `5b30515f`（控制台）、`945e6cce` → `7f7d3f8e`（远程访问）、`1b4de164`（二维码移到关于页末尾并加提示，11-16）。维护者 11-16 夜里反馈除网易云弹窗外左碟的检查项正常（其中含关于页二维码）。**实机待验**：公开隧道地址是临时域名，重启或断线后会变；用手机在移动网络上扫码登录一次。 |
| **Apple Music 与 Spotify 音源选择（PR #294，维护者）**（11-16） | `pkg/providers/`<br>`pkg/webui/`<br>`cmd/enginex-amd/` | **需求**：维护者希望碟机能用 Apple Music，并在 Spotify 直接出音频被封后仍能播放。<br>**修法**：Apple Music 通过 **`enginex-amd`** 下载助手把无损 ALAC 转成 FLAC 并缓存在碟机上（助手在 macOS、Windows、Linux 上不用 Docker，二代机可本机运行）；在 `:8080` 控制台填助手地址和令牌，再用手机页 `/m/applemusic` 或控制台登录（Apple ID、密码和六位验证码），也可用助手登录链接 / 二维码；歌单 / 专辑链接、MusicKit JS 和 `media-user-token` 仍用于歌单与曲库浏览；`设置 → 服务 → 流媒体` 控制 EngineX 的 Apple Music，原厂重复的那一行被隐藏。Spotify 直接出音频被 Spotify 关闭（许多账号 “audio key refused”），改为在已登录的 Deezer、网易云、Beatport、Apple Music 或 SoundCloud 上按 ISRC 与标题匹配同一首歌；控制台 Music 部分的 Spotify 音源选择 `auto` / Deezer / 网易云 / Beatport / Apple Music / SoundCloud，选定的服务独占，`auto` 按这个顺序尝试；已缓存的曲目继续可播。录音身份和已缓存格式保持（`bf07ae8c`）。 | 检验：`pkg/providers`、`pkg/webui` 套件；提交 `5e4b5c5e`（合并，11-16）、`bf07ae8c`、`11776193`（手机助手登录与异步登录轮询，11-16 21:07）。后续修复见 1A-2 的 Apple Music 行。**实机**：维护者自己实测过该功能。**限制**：Apple Music 需要订阅；PRIME 4 G2 / SYSTEM ONE 的原生 Apple Music 登录不需要外部后端，其余机型用助手。 |

### 1A-1 晚间合入集成分支（2026-11-16 20:25 之后）

| Issue / PR（日期） | 模块 | 原因与修法 | 检验、分支与提交、仍需实机 |
| --- | --- | --- | --- |
| **#271**（11-16，写回 Cue） | `pkg/bridge/`<br>`pkg/rekordbox/`<br>`pkg/webui/` | **需求**：碟机上改过的热 Cue、记忆 Cue 和 Loop 要写回 rekordbox U 盘。<br>**修法**：`WriteBackCues` 比对 `PerformanceData` 与 U 盘 ANLZ 里的 Cue，原子地更新 `.DAT`（`PCOB`）、`.EXT`（`PCO2`）、有 Cue 段的 `.2EX` 和纯 Go 实现的 SQLCipher 4 `exportLibrary.db` 的 `cue` 表；在卷锁内执行，写前一次性备份到 `/.enginex-backup/<日期>/`；入口 `POST /v1/enginex/rekordbox/writeback` 和四语言控制台卡片。 | 检验：`TestEncryptSQLCipher4AndCLIProof`（含 `sqlcipher` 命令行 `PRAGMA integrity_check`）、`TestRewriteANLZCuesRoundTrip`、`TestWriteBackCuesEndToEndAndBridgeFastReinsert`、拔盘写到一半时原文件完好的用例、`TestHandleRekordboxWriteback`。集成 `14fa37c4`（行 266）。**实机待验**：FAT32 / exFAT U 盘上用 CDJ-3000 / NXS2 加 rekordbox 7 的设备视图核对写回结果。 |
| **总览波形尺寸**（11-16）<br>维护者真机反馈 | `pkg/webui/`<br>`pkg/enginedb/`<br>`tools/ui-mod/` | **原因**：配对 / 四碟模式里对端曲目的小总览又变得很小；U 盘 5,981 行里有 5,710 行的总览末尾是零（频段最大值缺失），挂载补全每次只做约 256 行，2048 行一批在慢卡上永远提交不完、每次挂载重做。<br>**修法**：影子轨使用前先用 `OverviewMaxima` 补全对端总览的零尾，挂载后不写 `m.db`；补全批量固定 256 行并带检查点。**随后**钩子在内存里接管 Qt 的 `qUncompress`（Engine 5.3.0、SC LIVE 4 版和 AZ04 对每个 `PerformanceData` 都调用它），把形如总览的结果里的零尾补成各频段最大值：从任何来源首次加载的曲目就以原厂高度显示，不写数据库；Engine 自己写的尾巴、静音总览、其它数据块和 Engine 5.0.4 都不动。 | 检验：`TestPerfFromBlobsFillsZeroOverviewMaxima`、`TestFillOverviewMaximaSlowWriteProgressesAcrossMountBudgets`、`test_overview_maxima.c`（armhf、AArch64、ASan）、`OverviewMaximaHookTest`；JP07 eglfs 沙箱里 Denon Half / Full 由 32 / 46 px 变为 52 / 78 px，与 Engine 自己分析的库逐像素一致。集成 `b9b998a9`（行 267）、`179947d4`（行 272，提交 `85f84ea0`）。**实机待验**：配对与四碟模式下对端 rekordbox 曲目的总览高度；以前小的 U 盘首次加载就是原厂高度。 |
| **#195 / #207**（11-16，效果） | `tools/ui-mod/`（`pioneer_fx.h`、`reverb_time.h`） | **需求**：PRIME 4 需要更多效果，混响尾音太短。<br>**修法**：FX 选择器末尾新增第 6 款 `Jet Flanger (PX)`（共振更强，扫频按倍频程匀速，低频不进梳状滤波，满湿声在干声 3 dB 内，湿度 0 逐位相同）；原厂 `Reverb`（经典）在 `Parameter` 82% 以内与原厂完全相同，往上尾音变长，100% 时约 28 秒（48 kHz 下 29 秒），关闭效果后的尾音是原厂的，切换效果立即清空，不会进入 DSP 的失控衰减。 | 检验：`test_jet_flanger.c`、混响时间的 C 测试、`test_ui_mod`。集成 `296a3ca6`（行 268、269）。**实机待验**：PRIME 4 上听 `Jet Flanger (PX)` 和长混响；PRIME 4 G2 / SYSTEM ONE 暂不包含。 |
| **#99**（11-16，`Shift + View`） | `tools/ui-mod/build_packs.py` | **原因**：真机控制器上 `Shift + View` 无反应（左 SC5000 带上 `4dd3204f` 和 `3dfbbfba` 后仍然没有）：控制器的 QML 引擎跑在 MIDI 线程里，`trigger()` 只在调用它的对象上触发，状态不会跨引擎传到界面引擎；原厂的界面动作是用 `translator.state = true` 的锁存。<br>**修法**：在控制器引擎里锁存 `/GUI/Scripted/EngineXCycleView`，由界面引擎里原厂的状态变化通知器计数并复位；界面还没取走时按键排队（每次按键对应一步，不合并、不重复）；1.5 秒内界面没取走就撤回锁存和排队，不会过很久才跳一步。单按、双击、长按仍走原厂路径。 | 检验：`test_ui_mod_shift_view`（五视图循环、每种板型策略、重启）；经 4 轮复核（布尔锁存合并两次按键、请求永不过期被先后抓出并修复）。提交 `339d47fd` → `058163f2` → `7528ecbe`，集成 `a36cd4fb`（行 270）。**实机待验**：左 SC5000 上 `Shift + View` 一步一个视图；5.3.0.4-r5 的碟机本来就能切。 |
| **官方 Mod**（11-16）<br>#293 | `docs/mods/src/`<br>`pkg/mods/` | **原因**：随 #234 退役旧接口，Mod 不能再用旧接口兜底；`minimal-club-mode` 在 5.3.0.4 保存的暂停记录里用旧的单元名，升级后无法还原。<br>**修法**：`dmx-studio`、`tailscale-remote`、`sampler-sequencer` 去掉旧接口回退；`minimal-club-mode` 把暂停记录里的旧单元名映射成现在的；`sampler-sequencer`（#293）只触发原厂采样器。 | 检验：全部 Go + `docs/mods` + `test_mods.py`；提交 `3875a9d5`，集成 `4fe5093f`（行 271）。 |
| **#296**（11-17，顶部状态栏 CPU/RAM 负载 Mod） | `docs/mods/src/cpu-ram-monitor/`<br>`docs/mods/src/helper/`<br>`tools/mods/build_official_mods.py` | **需求**：在顶部时间旁显示 CPU 与内存占用。<br>**修法**：新增官方 Mod `cpu-ram-monitor`（1.0.0）：静态 Go helper 在 `127.0.0.1:17840` 读取 `/proc/stat` 与 `/proc/meminfo` 提供 `/status`；`TimeIndicator.qml` 叠加到 `Skins/Embedded/Views/StatusBar/Indicators/TimeIndicator.qml`，每 2 秒刷新显示 `CPU <n>%  RAM <n>%`，helper 未运行时自动隐藏。 | 检验：`go test ./docs/mods/src/... ./pkg/mods/...`、`test_mods.py`、`build_official_mods.py --check`；armhf 实测 60s 轮询 CPU 0.017%、RSS 8716 KiB；JP07 / JC11 eglfs 沙箱截图无遮挡。 |
| **流媒体与 Stems**（11-16）<br>真机反馈 | `tools/ui-mod/build_packs.py`<br>`pkg/webui/`<br>`tools/stems/` | **原因**：(1) 没有任何盘被接受为保存盘时报「没有可用的来源磁盘」；(2) 点了 Stems 的流媒体行，渲染几分钟屏幕毫无反应；(3) 下载不下来的曲目（如网易云对该账号只给试听）没有任何提示；(4) 内部保存盘 `stream-save` 出现在来源列表里；(5) 带有 RTX 50 系显卡的电脑上 Stems 工作进程悄悄退回 CPU。<br>**修法**：(1) 每次请求都检查，已就绪就算成功，不再等 10 秒（`e27646ef`）；(2) 点的那行显示原厂的渲染进度，每首曲目按自己的顺序、百分比与剩余时间（Engine 通过 `enqueueTrackRenderJob` 报出队列的曲目，`7197a0b6`）；(3) 该行显示「下载失败」10 秒，且不占 Engine 的队列；(4) 内部保存盘按路径从来源列表和浏览旋钮中隐藏，浏览器不停留在它上面；(5) 工作进程对 Blackwell（sm_120）改用 `cu128`，`--cuda-check`，退回 CPU 时大声警告（`555722d3` → `f79b5749`）。 | 检验：`StreamingStemsProgressTest` 系列、`InternalStreamingStorageTest`、`tools/stems` 18 项；多轮复核（本地入队被误当成已接受、查询能力在 CUDA 失败后抛错各一次，均已修复）。集成 `179947d4`、`b8b28d9d`（行 273–275）。**实机待验**：左 SC5000 的网易云弹窗不再出现（此前左碟的开机卫士缺 `stream-save`，需带该守卫重新推送）；两台碟机都在放时点 Stems 的提示（见 1A-2 的同一次滑动修复）。 |
| **#213**（11-16，反转转盘）后续 | `tools/ui-mod/build_packs.py`<br>`pkg/webui/` | 见第 1 节 #213 行。一体机 `JP11`、`JP21` 等每 500 毫秒从共享状态同步左右设置到界面引擎；SC5000 / SC6000 这类独立播放器隐藏该设置且忽略它。 | 检验：`test_ui_mod`；`5e04eac9`，集成 `21e59d70`（行 276）。**实机待验**：双转盘一体机上左右分别反转。 |

### 1A-2 合入候选分支（2026-11-17 03:50 至 2026-11-18 06:11，共 16 批）

| Issue / PR（日期） | 模块 | 原因与修法 | 检验、分支与提交、仍需实机 |
| --- | --- | --- | --- |
| **#298**（11-17）<br>PRIME 4 加载曲目时正在播放的碟机卡顿 | `rootfs-overlay/usr/local/bin/enginex-perf-tune`<br>`tests/test_rootfs_overlay.sh` | **原因（沙箱里能证明的最可能原因）**：`enginex-perf-tune` 的 30 秒核心巡检（`2e79e426`，5.3.0.4-r5 已带）把 Engine 每路解码线程 `Streamer1–4` 和加载线程 `TrackLoad` 也挪到 CPU 0、3；原厂是每路解码、加载各占一个核。另一台碟机加载时，加载、导入、离线分析、USB / SSD / SD 的中断线程（FIFO 40，在 CPU 3）和音频线程挤在同样两个核上；正在播放的碟机只缓冲约 0.8 秒，解码线程被饿 0.8 秒就断音（沙箱里冻结 `Streamer1` 800 ms → 132 ms 静音，Engine 日志里什么都没有）。<br>**修法**：巡检不再移动 `Streamer[1-4]` 和 `TrackLoad[1-4]`，与音频线程一样；其余计划（界面、SceneGraph、其它辅助线程）不变。旧测试断言的正是这个 bug（`Streamer2` 被挪走），现在改为断言它们**不被移动**。 | 检验：`tests/test_rootfs_overlay.sh` 的 perf-tune 用例在旧版失败（`unexpected > <104>`）；沙箱 A/B：加载后 3 秒内 `Streamer1` 的运行队列等待原厂摆放 2.2–6.9 ms，旧巡检 3.5–12.2 ms。分支 `agent/5305-hunt-L1` @ `c6099b9f`（合入 `a626087e`）。**后续（11-18，`agent/5305-perf-slim` @ `3ade30c6`，合入 `09ab2e3f`，检查表行 312）**：这次又把 `enginex-perf-tune` 和 `enginex-debloat` 的中断、中断线程、Engine 线程和 workqueue 绑核，以及重复巡检整个去掉，全部交给原厂；仍保留时钟、GPU、sysctl、预读、I/O 调度器和 USB / SATA 电源的调优（`ondemand` 1.2 GHz 下限、GPU `simple_ondemand` 起点 300 MHz 且电池或超过 85 °C 时降到最低、`bfq` 已有就保留、USB 自动挂起关闭）。所以上面对 `Streamer` / `TrackLoad` 的豁免已经没有用武之地（没有巡检了），旧的“绑核应当发生”断言换成“掩码不变、没有 `taskset` / `chrt`、没有后台巡检器”。**这条仍然是“很可能的原因”**：根因在沙箱里能证明，但没有在 PRIME 4 上证实。**实机待验（必须）**：沙箱不能模拟 RK3288；在 PRIME 4 上从 USB 和内置盘加载曲目，正在播放的那台不再断音。若仍断，备选嫌疑：U 盘预读 1–2 MiB 与 mq-deadline（原厂 128 KB、BFQ）、中断线程布局与原厂相反、1.2 GHz ondemand 下限。 |
| **影子轨：微调、略超前、循环**（11-17）<br>真机反馈：“微调 jog 的时候影子轨道好慢才反应过来”“影子轨道好像快一点点” | `tools/ui-mod/build_packs.py`（仅 QML JS） | **原因**：(1) 稳态略超前：发送端用上包络，包络只以 0.5 ms/s 衰减，约六十次读数里有一次提前约 13 ms，之后约 26 秒偏高，影子最多超前约 12 ms；(2) 微调显示慢：不触摸的微调只移动源碟的 TrackPosition，接收端 8 秒 EMA 的慢环路要几秒才跟上；(3) 影子不镜像循环：循环里最多冲过 LoopOut 约 200 ms 再弹回。<br>**修法**：孤立的一次提前读数最多把线抬高 2 ms，下一次读数确认的真实推动才全额计入（偏差均值 ≤1.5 ms、最大 ≤3 ms）；接收端读原厂的 BeatInfo 镜像（每三个音频块一次）做快速路径，用原厂的 `NudgeForward` / `NudgeBackward` 跟随（不写速度、不 seek），之后慢环路暂停 2 秒；镜像无效、换曲或卸载时释放微调；发送端发送 loopIn / loopOut，接收端每 16 ms 在循环末端折返（冲过 236.5 ms → ≤19 ms）。**影子不画循环区域**（原厂没有接口）。 | 检验：`ShadowNudgeTest`、`ShadowMirrorNudgeTest`（含 7 个子用例的释放测试）、`ShadowLoopTest`，全部在旧版失败；影子类 113 项、`test_shadow_review` 41 项通过。经 3 轮复核（镜像失效和换曲时微调没有释放被抓出并修复）。分支 `agent/5305-shadow-nudge-2` @ `1813c6fb`（合入 `d40dae58`，行 277）。**实机待验**：沙箱的镜像不规则、QML 计时器约 175 ms，只有两台碟机能证明快速路径：碟机日志 `/tmp/enginex-shadow-debug.log` 里出现 `nudge-ready`；微调时影子几十毫秒内跟上；1–2 拍循环时左碟循环、右碟影子不冲过；稳态不超前。 |
| **电脑上的 rekordbox 作为碟机曲库来源**（11-17）<br>维护者反馈：“电脑连接了，碟机没有歌曲选项” | `pkg/prolink/`（`rekordbox_link.go`、`nfs.go`）<br>`pkg/webui/rekordbox_source.go`<br>`tools/ui-mod/`<br>`pkg/webui/index.html` | **原因**：碟机能被 rekordbox 看到，却没有选曲入口。<br>**修法**：开启 `设置 → 服务 → Pro DJ Link` 后，电脑上的 rekordbox 7（`Export` 模式、开启 `LINK`）出现在碟机来源列表里，名为 `rekordbox（<电脑名>）`；点开是 `Tracks`（全部曲目）和各歌单（文件夹显示为 `文件夹 / 歌单`），每行有曲名、艺人、BPM 和调性（每个列表三次请求）。加载曲目：文件路径由 dbserver 给出，先找已挂载的网络共享（控制台 `曲库、网络与系统 → rekordbox 音乐文件夹`，设置键 `rekordbox_share`），找不到就用 NFSv2 从电脑拉到 U 盘 / SD 卡的 `.rekordbox-cache`（遵守 Pro DJ Link 暂存预算，后台边拉边播，15 分钟上限）；都够不到时列表第一行提示“这台电脑没有向 EngineX 共享文件”，点歌立即失败（502），不会卡住。限 MP3 / FLAC / AAC；rekordbox 里的波形、拍线、Cue 暂不带过来，由 Engine 加载后自己分析。底层：dbserver 紧凑标签、空数据块、16 MiB 上限；从 udp/50002 的 `0x29` 发现电脑；以碟机自己的 Pro DJ Link 播放器编号提问（没有就用 5）。 | 检验：`pkg/webui`、`pkg/prolink`、`internal/cli`、`pkg/rekordbox` 带 `-race` 通过，`test_ui_mod` 258 项；`TestReadDBMessageCompactTagList` 在旧代码失败。分支 `agent/5305-rbx-link` @ `4f158359`（合入 `b2ef850e`）。**实机待验（8 步，见分支报告）**：(1) 真机上 `0x29` 的设备名 / 设备号偏移是否与假设一致（抓包只记录了类型和长度）；(2) rekordbox 的 `mountd` / `nfsd` 是否回答碟机（右碟探针没有收到回复，可能要求碟机先“报到”）；(3) NFS 速度（约 3–8 MB/s，差 Wi-Fi 约 2 MB/s，40 MB 的曲目需 5–20 秒），以及 Engine 对慢流和总超时的反应；(4) Engine 浏览界面里的来源、提示行和失败行是否清楚；(5) 两台碟机同时用时各自的播放器编号是否被接受。 |
| **rekordbox 曲目加载失败**（11-17）<br>维护者实机：左碟 `5dd3fcac`，点歌报“曲目下載失敗 / Unknown Error”，日志 `nfs=prolink: no reply from <电脑>:13140` | `pkg/prolink/export.go`<br>`internal/cli/rbbridge.go`<br>`tools/ui-mod/build_packs.py` | **原因**：rekordbox 只给“收到过其状态包”的播放器提供 NFS。碟机已作为 3 号播放器报到（keepalive），但导出只把状态包（`0x0a`）发给 1–6 号播放器，从不发给电脑（17 号），于是 `mountd` / `nfsd` 一律不答。左碟实测：只报到时 portmap 回答、`mountd` 和 `nfsd` 沉默（源端口 1023 或临时端口都一样）；向电脑发状态包后立刻回答，15 秒后仍回答；从未报到的另一地址发同样的状态包仍然沉默。<br>**修法**：导出把 rekordbox 电脑也当成对端，像真机一样给它发状态包；电脑的编号（16–31）不参与选号。电脑上不需要任何设置。<br>**同时**：rb-bridge 不再把每个 Pro DJ Link 包写进日志（30 分钟 25,660 行，约 14 行/秒），只记设备出现 / 消失，逐包输出要 `verbose_log=1`；来源列表里 `rekordbox（<电脑名>）` 排到本机磁盘和 Link 来源之后、流媒体服务之前。<br>**“没有可用的来源磁碟”弹窗**：这台碟机是旧镜像加 deck-push，`engine-ui-guard` 仍是 11-15 的版本，没有注册内置存档盘 `/data/enginex/stream-save`（`bfb3d999`），所以 QML 找不到它，退回原厂弹窗；正式安装的候选版会注册。不需要改产品。 | 检验：`TestExportSendsStatusToRekordbox`（旧代码超时，收不到状态包）、`TestRBBridgeLogsDevicesNotPackets`、`test_ui_mod`（`rekordbox` 排在 `tidal` 前）；`pkg/prolink`、`pkg/webui`、`internal/cli`、`pkg/bridge` 带 `-race` 全部通过。**实机（左碟，`3e1ec613`）**：Engine 自己的加载 `via=nfs`，39,630,846 字节 11.5 秒拷完；网关加载 882 号曲目 `via=nfs`，15,701,725 字节 5.0 秒拷完并完整播放；碟机每 200 ms 给电脑发一个状态包；rb-bridge 两分钟 0 行日志。分支 `agent/5305-rbx-nfs`。 |
| **rekordbox 的 MASTER 与播放器编号**（11-17）<br>维护者实机（左碟，加载修好之后）：rekordbox Link 面板只显示一台播放器 3，且碟机暂停时也亮 MASTER；在 rekordbox 上按 MASTER 没反应 | `pkg/prolink/export.go`<br>`pkg/prolink/prolink.go`<br>`pkg/webui/shadow.go` | **原因**：①导出只答复 1–6 号播放器的 MASTER 请求（`0x26`），rekordbox 是 17 号；②只从播放器的 `0x0a` 读 master，rekordbox 发的是混音器格式的 `0x29`（标志在 `0x27`，交接在 `0x36`），且是广播，绑在网卡地址上的状态端口收不到；③碟机暂停、没人按 MASTER 时，Engine 的 master 也照样声明或向 rekordbox 要 master；④一台碟机只报一个播放器编号。<br>**修法**：答复电脑的请求（`0x27`），读它的 `0x29`，状态端口另开广播地址的接收；暂停且没按 MASTER 的碟位不抢、不要、不收回空出的 master（按 MASTER 照旧；stream-sync 重启后的第一次上报不算按键）；每个 EngineX 碟位报一个播放器，编号同碟位（左台 1 和 3），各自的 keepalive 和状态（播放、BPM、拍），master 碟位的播放器发 master、拍和交接包，第一个播放器负责媒体；CDJ 已占的编号不抢（#198），都被占时退回一个空闲编号；之后开机的 CDJ 认领我们占着的编号（第 2 / 最后阶段），或第 1 阶段时它认得的编号（1–4，CDJ-3000 为 1–6）都满了，就让出一个多出来的碟位（不是 master 碟位，至少留一个），该编号 10 秒内不再拿回；对端按编号区分（同一地址可以有几台播放器）。碟位加载的是 rekordbox 来源的曲目时，状态包像经 Link 加载的 CDJ 一样写明来源：电脑的编号（`0x28`）、槽位 4（`0x29`）、类型 1（`0x2a`）、rekordbox 曲目号（`0x2c`），暂停时也标“已加载”（`0x7b`），rekordbox 的播放器面板因此能显示曲名；本机磁盘的曲目照旧不写。 | 检验：`TestExportHandsMasterToRekordbox`、`TestExportOnePlayerPerDeck`、`TestExportPausedDeckLeavesMasterToRekordbox`、`TestLinkPlayersPerDeckAndPausedMaster`、`TestSimCDJsBootAfterConsoleAndPlayerPair`（PRIME 4 和一对 SC5000 之后开机的 CDJ-2000NXS2 / CDJ-3000），#217 / #198 / 影子碟测试照旧通过。**实机（左碟，`bf2a76bd`）**：导出报 `players 1,3`，两台播放器各发状态；抓包见碟机向 rekordbox 要 master 得到 `0x27`，rekordbox 再要回时拿到 master，碟机让出；加载仍 `via=nfs`。**待维护者看**：rekordbox Link 面板是否显示 1 和 3、在 rekordbox 上按 MASTER 是否移过去。 |
| **单一 Pro DJ Link 开关**（11-17）<br>维护者：“为什么不做一个开关？” | `pkg/webui/`<br>`pkg/prolink/`<br>`internal/cli/`<br>`tools/ui-mod/`<br>`pkg/webui/index.html` | **原因**：以前有两个开关：只读不发包的“库”（`prolink_share`）和测试版的“导出到 CDJ”（`prolink_export`）；只有后者才让碟机向 Pro DJ Link 宣告自己，rekordbox 才会出现 `LINK`，用户很难想到要开另一个。<br>**修法**：只剩一个键 `prolink_share`；旧的 `prolink_export=1` 在读取时迁移；碟机 `设置 → 服务 → Pro DJ Link` 和 `:8080` 控制台是同一个开关（四语言）：开启后宣告本机、浏览 CDJ 与 rekordbox、共享本机 USB / SD；没有 rekordbox U 盘时开关显示“已开启：插入 rekordbox U 盘后自动开始共享”。 | 检验：`pkg/webui`、`pkg/prolink`、`internal/cli` 带 `-race` 通过、47 个 UI 测试通过，3 个新用例在基线失败。分支 `agent/5305-prolink-switch` @ `0ae5a0d6`（合入 `4ef4a1bb`）。**兼容性**：旧的接口参数 `prolinkExport=1` / `export=1` 不再被接受（UI 包和控制台同版本发布）。**实机待验**：开启后 rekordbox 出现 `LINK`；从只开了旧“导出”开关的 5.3.0.4 升级后开关状态保持。 |
| **Pro DJ Link 导出不刷新**（11-17，L4） | `pkg/prolink/export.go` | **原因**：`Exporter.Rebind()` 只看绑定的网卡 IP，忽略 U 盘卸载和 `export.pdb` / 挂载根变化。<br>**修法**：卸载、`export.pdb` 变化时重启导出。 | `TestExportRebindWhenPDBChanges`；`agent/5305-hunt-L4` @ `86688c38`（合入 `1a3a936b`）。**实机待验**：导出运行时插入新的 rekordbox U 盘，CDJ 看到的是更新后的曲库，不用先关再开导出。 |
| **HFS+ 可读写：一代机与 PRIME 4 G2 / SYSTEM ONE**（11-16 至 11-17） | `kernel-modules/`<br>`tools/edisksd-hook/`<br>`tools/az01-packer/`<br>`pkg/dbhealer/`<br>`cmd/`（`engine-guardian`） | **原因**：HFS+ 过去只能只读，因为内核的 `hfsplus` 在 32 位 HIGHMEM 下有 bug：`hfs_bnode_move()` 把“基址 + 偏移”传给 `kunmap_local()`，对齐后落到相邻槽位，下一次映射撞上 `BUG_ON`，`PANIC_ON_OOPS` 直接重启并可能损坏 U 盘上的 B 树节点（上游和 6.18.57 都存在，不是 5.0.4 时代的旧事）；沙箱还复现了同页二次映射加 `memmove` 破坏 B 树的第二个 bug。<br>**修法**：**一代机**：用 6.18.57-rt 源码构建修好的 `hfsplusx.ko`（文件系统名 `hfsplusx`），放在 rootfs 的 `/usr/lib/modules/<内核>/extra/`（内核开启了 LoadPin，只能从 rootfs 加载，`/tmp` 里的模块被拒绝）；`edisksd` 钩子只在 `fsck.hfsplus -n -f` 通过、卷头与日志检查通过（日志为空）时才读写挂载，任何一步失败都回到只读视图；未干净卸载但 `fsck` 干净的卷先 `fsck -y` 标记干净再读写（维护者批准），`fsck -y` 从不被杀，且只在剩余时间够时才启动；读写卷卸载不用 `MNT_DETACH`、设备忙时拒绝（`target is busy`），设备被拔走时才用 lazy；只读视图改为只绑定一次卷根。**PRIME 4 G2 / SYSTEM ONE（arm64，没有 HIGHMEM）**：用原厂 `hfsplus.ko`，同样的 `fsck` 和日志检查，随数据包带静态的 `fsck.hfsplus`；原厂的 Paragon `ufsd` 路径（设备树标志 `inmusic,hfsplus-rw`）若开着则原样放行。 | 检验：QEMU armhf HIGHMEM 里原厂驱动复现 BUG，修好的模块 4338 次操作、`fsck` 干净；`tools/edisksd-hook/test_edisksd_hook.c`（armhf、arm64、ASan / UBSan，含 `--mounts`）、`test_az01_packer`、`test_data_bundle`、`CheckHfsplusxTests`、检查表行 281。分支 `agent/5305-hfs-rw` @ `f250141f`（合入 `98f7241d`）。**真机（左 SC5000，一块草稿盘，2026-11-16）**：`hfsplusx` 压力 4340 次操作 0 错误、`fsck` 干净、只读重挂后 1272 个文件哈希一致；经 `edisksd` 和 Engine，非日志盘和日志为空的日志盘都读写挂载，Engine 在盘上建库；Engine 占用时卸载被拒、盘仍读写；没有模块时回退只读也在机上验证。**仍需实机**：PRIME 4 G2 / SYSTEM ONE 的读写（没有在 AZ04 实机上跑过）；大盘上 `fsck -y` 的耗时；macOS `fsck_hfs` 复查；Engine 写 Cue / Loop / 封面；拔盘；机上加载过的模块是 GCC 14 编的，由发版构建产出的 GCC 13 版没有在机上加载过。 |
| **5.3.0.4 现存风险：干净、未启用日志的 HFS+ 盘会以读写挂载**（11-16 夜间发现，随 5.3.0.5 修复） | `tools/edisksd-hook/`（挂载策略） | **原因**：5.3.0.4（含 r5）的钩子只在 HFS+ 已经是只读时才做只读视图，从不强制只读。macOS 默认的带日志卷会被内核降级成只读，所以平时看不出来；但 `mkfs.hfsplus` 或 `diskutil disableJournal` 格式化的干净、未启用日志的卷，会在内置的有 bug 的驱动上以读写挂载（一代机与二代 32 位机；AZ04 没有 HIGHMEM）。左碟用 5.3.0.5 开发版实测确认：非日志草稿盘经 `edisksd` 挂载就是读写，没有写入就立即卸载；5.3.0.4-r5 的钩子逻辑相同（读代码得出，没有在 r5 碟机上跑）。<br>**修法**：钩子对 `mount()`、`fsopen()`、`fsconfig()` 全部强制只读，除非该机型的读写策略批准（固件的 `libmount 2.39.3` 走新的挂载 API `fsopen` / `fsconfig` / `fsmount`，只拦 `mount(2)` 会被绕过）；二代 32 位机先强制只读，其中通过检查的卷再由 lklfuse 读写挂载（见下面的 lklfuse 一行）。维护者决定：不发 r6、不在 GitHub 上警告，随 5.3.0.5 一并修复。 | 检验：`test_hfs_rw_unit`、`test_hfs_rw_policy`；左 SC5000 真机上无模块时回退只读的路径（`forcing ro via fsopen/fsconfig`）有效，说明新挂载 API 的拦截在真机上生效。同分支 `agent/5305-hfs-rw`。**仍需实机**：二代 32 位机（SC LIVE 4 等）插入非日志 HFS+ 盘，确认走的是 lklfuse 而不是有 bug 的内置驱动。 |
| **HFS+ 盘上的“碟机建的库”搬到盘上**（11-17） | `tools/edisksd-hook/`<br>`pkg/dbhealer/`<br>`cmd/`（`engine-guardian --check-copy`） | **原因**：以前只读的 HFS+ 盘没有自己的 `Engine Library`，碟机把曲库建在 `/data`；盘第一次读写挂载后，如果 Engine 在盘上另起一个空库，已分析好的库就看不见了。<br>**修法**：首次通过读写检查的挂载时，在私有命名空间里读写挂载，把 `/data` 里最新一代的库整体复制到盘根的暂存目录（逐个文件和目录同步），用 `engine-guardian --check-copy` 校验（`PRAGMA integrity_check`、本地曲目的路径相对库目录都留在本盘），再用 `RENAME_NOREPLACE` 改名成 `Engine Library`；盘上已有自己的库就绝不覆盖；任何失败（空间不够需要库大小加 16 MiB、复制超时、盘满）都清掉暂存并回到只读视图加 `/data` 的库，超时的盘到重启前不再重试；成功后 `/data` 那一代保留以便回退。 | 检验：`test_edisksd_hook` 5d 一组（拒绝复制、整体搬、不覆盖自己的库、空间不足、写到一半盘满、超时）、`dbhealer.CheckCopy`、`--check-copy` 的退出码；用真实 HFS+ 镜像和 Engine 做的沙箱证明：同样 2 行 Track、路径无需改写、`fsck` 干净。`f250141f`。**已知限制**：搬运必须在挂载时限内完成（约 17 秒减去 `fsck`），大库在慢盘上会超时、保持只读、重启后重试；二代 32 位机的搬运由 lklfuse 路径完成（见下一行）。**实机待验**：5.3.0.4 分析过的 HFS+ 盘在 5.3.0.5 上显示同样的曲目、Cue 和波形，且之后可写。 |
| **`hfsplusx.ko` 由发版构建产出**（11-17） | `kernel-modules/`<br>`tools/az01-packer/build-release.sh`<br>`check-hfsplusx.sh`<br>`release-gate.sh` | **原因**：发版实验室里没有这个模块；靠人工放一个文件会让每次发版在第 1 步停下，或者带着不匹配的模块发版。<br>**修法**：`kernel-modules/build-release-module.sh` 用校验过 sha256 的固定 6.18.57 和 `-rt8` 补丁源码、碟机内核自己的 IKCONFIG、GCC 13 构建；`build-release.sh` 在没设 `HFSPLUSX_KO` 时自己构建并缓存（冷构建约 6.5 分钟，缓存后 0.06 秒）；找不到模块就停止；`check-hfsplusx.sh` 用 `debugfs` 检查每个一代 rootfs 里恰有一个内核目录、模块在 LoadPin 路径、`vermagic` 与镜像内核一致，并在发版门禁里加一项检查。 | 检验：`CheckHfsplusxTests`；真正的 `build-release.sh` 构建 16 个 `*-Update.img` 加 8 个数据包通过；JP07、JP13、JC11 门禁里这项检查均通过；模块 sha256 `4f162fa3…`，内核 `6.18.57-az01-2026-10-15-rt6`。`9f4834d2`、`efa4183c`，合入 `98f7241d`。**注意**：发版主机需要装 `flex`、`bison`、`bc`、`gcc-13-arm-linux-gnueabihf`，第一次构建需要联网。 |
| **“丢失的 Demo 盘修复”补回**（11-17） | `rootfs-overlay/etc/enginex/engine-ui-guard` | **原因**：内部保存盘 `stream-save` 用 `-extraDriveDir` 登记后，原厂 Engine 不再自己加载随机附带的 Demo 内容。修复 `4b8e7ca8`（登记保存盘时显式加 `-demoDriveDir`）在一次 rebase 里**丢了**，不在任何分支里；L7 的发版门禁因此在 JP07 / JP13 / JC11 上 172 项通过、4 项失败（Demo 来源和封面）。<br>**修法**：补回该修复（先推成 `agent/5305-demo-drive-fix`，现已含在 rename-final 的分支头 `03b98aee` 里，候选分支里有一条保留作者的空 cherry-pick 记录 `df5b14f4`）；Demo 内容存在时显式保留，不存在或保存目录不可用时也能正常启动。 | 检验：Demo 内容存在 / 缺失 / 保存目录不可用的启动测试；JP07 门禁恢复 176 / 0（Demo 来源与封面在两次开机都通过）。 |
| **#299**（11-17）<br>二代机网页控制台设置 SSH 密码返回 500 | `pkg/webui/`<br>`tools/az01-packer/build-data-bundle.py`<br>`rootfs-overlay/` | **原因**：二代机 5.3.0 去掉了 `etc.mount`，`/etc` 只读，控制台的 SSH root 密码调用 `chpasswd` 失败，返回 HTTP 500。机器人给的 PR #302 改成只允许密钥，但我们自己的数据包代码在“已启用且设了密码”时启动 sshd，等于期待密码登录。<br>**修法**：保留密码功能：只把 SHA-512 密码哈希存在 `/data/enginex/ssh-shadow`（权限 0600），开机时绑定到 `/etc/shadow` 之上并传给 sshd；密码和 root 登录选项需手动开启，所选端口和公钥访问保持；哈希或启用失败时不会打开 SSH。 | 检验：`TestSSHGen2PasswordOnReadOnlyEtc299`（覆盖 8 个二代板号）、43 个数据包测试、SC LIVE 4 引导沙箱里用真实 sshd 做密码和公钥登录并在开机时从 `/data` 还原、确认 `/etc` 仍只读（检查表行 286）。分支 `agent/5305-issue-299` @ `dfc01fdf`（合入 `9fd4195f`）。机器人的 PR #302 随本版本发布时关闭。**实机待验**：每一款二代机上的密码登录没有逐一实测。 |
| **#286**（11-17）<br>Mixstream：Pitch Bend 改 Beat Jump，FX 槽位 | `pkg/webui/`<br>`tools/ui-mod/build_packs.py`<br>`pkg/webui/index.html` | **需求**：Mixstream 的 `-` / `+` 推音键想直接前后 Beat Jump；四个实体 FX 键想自选效果。<br>**修法**：`设置 → 设备`（及 `:8080` 控制台）新增 **`Pitch Bend → Beat Jump`**（`pitch_bend_beat_jump`）：开启后未按 Shift 的 `-` / `+` 触发 `BeatJump` 前后跳，`Shift + -` / `+` 仍是速度范围；屏幕上的 Beat Jump 栏默认隐藏，波形多出 6 px（Pads 栏 70 → 64 px）。新增 **`FX 槽位 1–4`**（`mixstream_fx`）：四个实体 FX 键各自从 Mixstream 的 `Echo`、`Flanger`、`Delay`、`Phaser` 里任选；不支持的效果（如 `Reverb`）被拒绝（HTTP 400）。只适用于 Mixstream Pro / Pro+ / Pro Go。 | 检验：`TestMixstreamPitchBendBeatJumpAndFxSlots`、`test_mixstream_pitch_bend_beat_jump_and_fx_slots`、`test_ui_mod` 247 项（后随整批 256 项）、NH08 沙箱 `MIXSTREAM_NATIVE_PASS`（NH10 需要 eFuse，沙箱做不到）。分支 `agent/5305-mixstream-beatjump-fx` @ `dd0872be`（合入 `2a5a1873`，行 278、279）。**布局已合入候选**：Sweep FX 收进 `△` 按钮，Stems EQ 改为 `配置 → 播放` 的每通道切换（检查表行 293）；控制中心磁贴自适应可用高度（行 303）。**实机待验**：Mixstream Pro / Pro+ / Pro Go 上按键与 LED、四个 FX 键的分配。 |
| **控制中心磁贴被底行盖住**（11-17，#286 后续）<br>维护者：第 4、5 行磁贴在底部 `输入源` / `Wi-Fi` / `蓝牙` / `灯光` 一行下面，点不到 | `tools/ui-mod/build_packs.py`<br>`tools/az01-packer/` | **原因**：5.3.0 的控制中心标签页是固定的 `Flow`（256 × 104 磁贴，没有滚动），底行锚定在标签页底部；真实面板 `1280 × 800`（`800 × 1280` 竖屏旋转显示）里只放得下 3 行（12 块）。在各机型自己的 Engine 上实测，14 款 armhf 里有 10 款显示 13–17 块（PRIME 2 13；PRIME 4 / 4+ 14；SC LIVE 4 15；SC LIVE 2 16；Mixstream Pro / Pro+ / Pro Go、PRIME GO / GO+ 17），后几行被盖住，最多 5 块被盖、1 块在标签页外；这比 #286 老，不是 Stems EQ 磁贴造成的。<br>**修法**：磁贴网格整体等比缩小（顶边不动，按缩小后的宽度重算列数），取 1 到 0.5 之间让最后一行落在底行上方一个间距内的最大比例（13–15 块 0.91，16–17 块 0.85）；本来就放得下的机型（SC5000 / SC5000M / SC6000 / SC6000M，11 块）比例为 1，磁贴坐标与原厂逐项相同。同时 Stems EQ 切换从控制中心移出，改成 `配置 → 播放` 里的一行（`Deck 1` / `Deck 2`，仅 Mixstream、5.3.0），行为与写入路径不变。 | `test_ui_mod.py::ControlCenterFitTest` 两项（在旧代码上失败）、`test_stems_eq_switch_is_a_settings_playback_row_on_mixstream_only`；`cc-fit-sandbox.sh` 14 款 armhf 全部通过（修复前 10 款有磁贴被盖）。分支 `agent/5305-cc-fit`。**未跑**：PRIME 4 G2 / SYSTEM ONE（AZ04，沙箱里 Engine 退出）。**实机待验**：Mixstream Pro / SC LIVE 4 上点遍最后几行和底行，以及 `配置 → 播放 → Stems EQ`。 |
| **#255**（11-17）<br>Windows 原生安装器 | `cmd/enginex-installer/`<br>`internal/installer/`<br>`tools/az01-packer/` | **原因**：Windows + NH10 的测试者反馈（纯 Go 安装器、无 WSL）刷机成功，但干净的 Windows 上 WinUSB 绑定、Windows 上 ext4 测试的句柄泄漏、签名密钥权限的 0600 测试、二代机清空内部存储的警告都有缺口。<br>**修法**：Windows 上打开更新设备时，若缺少 WinUSB 绑定或接口注册，由提权的辅助程序只对**选中的、在线的**更新接口（fastboot、Rockchip loader、AZ04 `1cc5:2027`）用 Windows 自带已签名的 WinUSB 驱动补齐；不安装自带或未签名驱动、不开测试签名、不用 WSL，已有可用绑定不提示提权，普通音频 / HID / 存储 / ADB 接口和复合父设备一律拒绝；Rockchip loader 写入仍被禁用，不发任何协议命令；二代机新增中英文确认，说明内部音乐、曲库和设置会被清空；撤销方法写入三份 README 和网站；`build-installer.sh` 默认就是 Go 版。 | 检验：Linux `-race` 全套和 Windows 原生测试均通过（Windows 部分在 Windows 虚拟机上跑，没有在那里编译或装依赖）；六个 Windows 清理失败原来是测试夹具没关文件，已修。分支 `agent/5305-installer-win` @ `86d3fc95`（合入 `d8b2b67e`，行 280）。**实机待验**：干净的 Windows 电脑加一台实机，走提权、绑定、传输和刷入后的激活；原生 Windows 的安装与 UAC 行为只靠交叉编译和 Windows 虚拟机测试，没有在最终候选上复现。 |
| **bug 追查 L3**（11-17）<br>64 位钩子 | `tools/ui-mod/`（DSP 绑定） | **原因**：64 位的 `dsp_bound_owner` 布局不对，且 owner 为空时没有保护。<br>**修法**：改正布局并加空指针保护。 | C、实时回调和 FX 测试通过。分支 `agent/5305-hunt-L3` @ `769e5e31`（合入 `a364f211`）。DJ 操作沙箱（JC11 / JP07 上 18 项操作加截图）**没有跑完**，见 1B。 |
| **bug 追查 L4**（11-17）<br>两台碟机 | 见上面的“Pro DJ Link 导出不刷新”和“影子轨” | 双碟沙箱（shadow-twin）里加载镜像、播放暂停、热 Cue、Beat Jump、Throw、MASTER 双向、影子从不当 MASTER（1.8 秒内收回）、CDJ 显示模式、Paired Player 视图、一侧 Engine 重启均正常；发现并修了导出不刷新和影子不镜像循环两个问题。 | `agent/5305-hunt-L4` @ `86688c38`。 |
| **bug 追查 L5**（11-17）<br>流媒体 | `tools/ui-mod/build_packs.py`<br>`tools/ui-mod/i18n.tsv` | **L5-1**：四碟机器上“所有碟机都在放”的提示说“Both decks…”，并破坏了滑动条文字的绑定 → 四语言新增 `All decks are playing. Free one deck, then tap Stems again`，按碟位数选择，改成响应式绑定。<br>**L5-3**：多首 Stems 下载失败共用一个计时器 → 每行各自计时，满 10 秒各自过期。<br>**L5-4（P2）**：拔掉 U 盘后，内部保存盘已被选中时没有重新检查，“没有来源盘”弹窗要等 10 秒超时才消失 → 立即让 Engine 重新检查。<br>**L5-2**（报告，不改）：原厂 TIDAL 退出登录不清 `tidal-auth.json`，是设计如此。 | 每项都有在基线失败的回归测试；此外并发加载加播放、401 后成功、断网恢复、非 ASCII 搜索、500 字节标题、四碟 Stems 状态、浏览中途换服务全部通过。分支 `agent/5305-hunt-L5` @ `6dd91c7a`（合入 `7dd593e7`；批 2 先合入的是 `32e64b9c`）。 |
| **bug 追查 L6**（11-17）<br>曲库与媒体 | `pkg/bridge/cue_writeback.go` | **L6-1（P2）**：写回 Cue 时以 `immutable` 打开 `m.db`，看不到 Engine 还没检查点的 WAL，报告“无改动” → 读取 WAL。<br>**L6-2（P2）**：U 盘同时有 `exportLibrary.db` 和 `export.pdb`、前者损坏时写回报 SQLCipher 错误 → 回退到 `export.pdb`。<br>覆盖并通过：6000 首曲目首次挂载 847.9 ms、增量 130.3 ms；exFAT 回环；5500 首加 320 个歌单带陈旧的 `.rb-bridge.lock` 仍 0.8 秒转换完。 | `TestWriteBackCuesReadsUncheckpointedWAL`、`TestWriteBackCuesFallsBackToPDBWhenOneLibUnreadable`；分支 `agent/5305-hunt-L6` @ `8688eebe`（合入 `cb53fac8`）。**实机待验**：FAT32 / exFAT 的 rekordbox U 盘上在 Engine 还开着时写回。 |
| **bug 追查 L7**（11-17）<br>升级、设置、网页控制台、OTA | `pkg/webui/` | **修复**：JSON 的时区问题；FX 请求报告成功但没有效果（`6406dfcb`）；无效 JSON 仍会改设置并重启 Engine（P2）。升级、OTA、首次开机、安全模式、交还原厂、恢复出厂（chroot）全部通过。**同时发现**：发版门禁 172 通过 / 4 失败，起因是上面的 Demo 修复丢失。 | 分支 `agent/5305-hunt-L7` @ `4d9a3efa`（合入 `15c9d323`）。 |
| **bug 追查 L8**（11-17）<br>二代、AZ04、Mixstream 机型 | `tools/az01-packer/` 测试 | 仅新增测试：SC LIVE 4 与 PRIME 4 Plus（JC11S）引导沙箱通过、千兆网线断言（且没有被强制成 100 Mbps）、Mixstream NH08 沙箱通过、各机型的设置可见性正确、OTA 对 `*-Update.img` 和 `.tar.gz` 的桥接通过。**沙箱的局限**：NH10 需要 eFuse；AZ04 `/data` 分区大小（#244）仍需要真机。 | `agent/5305-hunt-L8` @ `20a0c1b2`（合入 `d53211ab`）。 |
| **bug 追查 L9**（11-17）<br>发版前整套检查 | `docs/REGRESSION-CHECKLIST.md`<br>预编译钩子<br>`docs/mods/`<br>测试 | **六项**：检查表行 267 的措辞（“KDATA SD”改为“U 盘”，KDATA 是 U 盘不是 SD 卡）；预编译的 `libengine-ui-hook.so`、`libedisksd-hook.so` 没跟上 C 源码（P2，影响镜像）；官方 Mod 包数检查 6 → 7（#293 新增采样序列器）；`sampler-sequencer` 的 gofmt 对齐；两个 `-race` 在 ARM64 上的计时阈值（总览补全预算 1500 → 2500 ms，rb-bridge 未改导出的 200 → 500 ms）。 | 提交 `b7f04d79`、`0db21d4f`、`79134d57`、`c8c4c890`、`43ab39c7`、`bd9a3e5c`；分支 `agent/5305-hunt-L9` @ `bd9a3e5c`（合入 `69bde8c5`）。整套测试通过。 |
| **bug 追查 L10**（11-17）<br>钩子与音频线程代码审查 | `tools/ui-mod/engine_ui_hook.c`<br>`tools/ui-mod/build.sh` | **L10-1（P3）**：手工把 `bt_out=` 改成 2³⁰ 以上会让主回调越界（AZ04 上 Engine 崩溃）→ `pair >= channels / 2`（`88accb5a`）。<br>**L10-3（疑似 P2，蓝牙断续的可能原因）**：SBC 码率池被设成音箱支持的最大值，支持 2..250 的设备会以 1.4 Mbit/s 传输（正常约 325 kbit/s，且与 Wi-Fi 共用同一个无线电）→ 改成 BlueZ 的默认值（44.1 kHz 及以下 53、48 kHz 51，单声道 / 双通道 31 / 29），音箱更低的最大值优先（`a2902d74`）。<br>**L10-2**：钩子惰性绑定，音频线程首次用到 libc 符号才解析 → 链接加 `-z now`（`cbc71ea1`）。<br>另加环形缓冲字段在真实 armhf Engine 上的固定检查（`c490da7c`）。<br>**报告、不改**：L10-4 罕见的渲染采样率会给音箱静音。 | 检验：`test_bt_out`（新循环在旧版失败；12 行质量测试在 armhf 和 AZ04 的 libsbc 上跑）、`test_stems_feature`、实时预算 / 回调、300k 次模糊测试、ASan / TSan 全部干净；`LD_BIND_NOW` 在 5.0.4、5.3.0、SC LIVE 4、AZ04 的 Engine 上全部符号可解析。分支 `agent/5305-hunt-L10` @ `cbc71ea1`（合入 `f17ca381`）。**实机待验（需要真音箱）**：音箱若支持 2..53，旧代码本来就发 53，这次修改不会改变它，不能证明断续就是码率池；步骤是 `journalctl -u engine -f` 里看 `SBC bitpool 53 (sink offers a..b)`，打开 Wi-Fi 流媒体对比；Wi-Fi 打开才断续说明是 2.4 GHz 共存问题。 |
| **#292**（11-17）<br>Engine Remote Library 排序补充 `Date Added` | `tools/ui-mod/build_packs.py`（`SortMenu.qml`）<br>`pkg/webui/ui_settings.go` | **原因**：#98 解除流媒体排序限制时将 `isStreamingProviderWithSort` 的 `menuRoles` 固定为 6 个通用流媒体字段，导致同属 `isCurrentCollectionStreaming` 的 Engine Remote Library 丢失了原厂 `dateImported`（Date Added）。<br>**修复**：仅当 `streamingInterfaceController.selectedProviderDisplayName` 为 `Engine Remote Library` / `EngineRemoteLibrary` 时在 `menuRoles` 中加入 `dateImported`，并在 `uiStreamSortRoles` 中放行 `dateImported`；原厂 QML 与 `enginelibrary.v1` 均无 `dateCreated`，故不添加 `Date Created` | `TestUISettings`、`test_remote_library_sort_menu_includes_date_imported`（实机连接 Engine DJ Desktop 排序重排待实机确认） |
| **#222 后续**（11-18）<br>SC LIVE 4 用户：四碟同时播放时切层要 1 秒左右 | `tools/ui-mod/build_packs.py`<br>`tools/az01-packer/layer-switch-sandbox.sh` | **原因**：原厂每次切层都会重建当前碟的 Pads 视图和表头（约 480 个对象）；界面包给每个 Pads 都加了 5 个只在 Mixstream 上显示的按钮（Sweep FX 三角、Beat Jump 四键），其他机型也会创建，并随每次切层一起重建。沙箱里四碟播放时，包比原厂每次切层多用的 GUI 线程时间：SC LIVE 4 约 17 ms、PRIME 4 约 15 ms、SC5000 约 11 ms；切层的大部分是原厂自己的开销（编译机上每次 45–110 ms，碟机 CPU 约慢 4 倍）。<br>**修法**：这些控件只在 Mixstream 上创建（`Loader` 的 `active`、Beat Jump 关闭时 model 为空）。修后多出约 7.5 / 5 / 7 ms，剩下的分散在各处。 | 检验：`test_ui_mod`（`MixstreamPadsLayoutTest`，旧代码失败）、Mixstream NH08 沙箱通过、切层沙箱（JP21 / JC11 / JP07，原厂对比）；SC LIVE 4 实机切层待用户复测（行 316）。 |
| **二代 32 位机 HFS+ 读写（`lklfuse`）**（11-17） | `tools/edisksd-hook/`<br>`prototypes/hfs-lkl/`<br>`tools/az01-packer/` | **原因**：二代 32 位机（SC LIVE 4、SC LIVE 2、PRIME 4+、PRIME GO+、Mixstream Pro+、Mixstream Pro Go）的内核强制模块签名（`MODULE_SIG_FORCE`）且被 LoadPin 锁定，装不了自己的模块，也不换内核（维护者决定）；内置的 `hfsplus` 有上面说的 HIGHMEM bug，所以过去只能强制只读。<br>**修法**：用户态方案 `lklfuse`（把 Linux 6.12 内核当库用，加 FUSE，armhf 程序，随二代数据包带 `bin/lklfuse` 和静态的 `bin/fsck.hfsplus`）。钩子在 `fsck.hfsplus -n -f` 通过、卷头与日志检查通过后，用 `lklfuse` 在私有暂存挂载里读写挂载（`nice 2`，内存受限：缓存约 32 MiB、地址空间上限 128 MiB），先跑 rb-bridge 和 engine-guardian，再移到 `/media/<卷>`；未干净卸载但 `fsck` 干净的卷先 `fsck -y` 标记干净并复查，带日志的卷只在日志为空时才用 `opts=force`；任何一步失败都回到强制只读；正常弹出会等 `lklfuse` 把数据写完并退出（最多约 15 秒）；拔盘时 lazy 卸载并结束 `lklfuse`，不会卡住；碟机建的库搬到盘上的规则与一代机相同。 | 检验：QEMU armv7 HIGHMEM 里 4338 次操作的压力、预填充、脏弹出、重新插入都 `fsck` 干净，内存 6–17 MiB，卸载比 `lklfuse` 退出早返回 74–1886 ms；`test_edisksd_hook` 的二代策略用例（`fsck` 失败 → 只读、干净 → 读写并等待、未干净 → `fsck -y` → 读写、拔出 → lazy 并结束、lkl 失败 → 只读、搬库；armhf、arm64、ASan / UBSan）；打包器与数据包测试 104 项；SC LIVE 4 与 AZ04 引导沙箱；批 7 的 JP07 门禁 176 / 0；检查表行 281、288。分支 `agent/5305-hfs-lkl-int` @ `f044c5ff`（合入 `bc71d09d`）。**实机待验（没有在任何二代机上跑过）**：干净 / 未干净 / 日志盘、Engine 列表与读写、干净弹出、播放中拔盘；`lklfuse` 以读写方式打开块设备且没用 `-o sync`，RK3288 上的性能没测；写入中突然拔盘仍可能丢失脏缓存。 |
| **#300**（11-17）<br>没有 CIFS 的机型读不了 SMB 共享 | `pkg/webui/`（NAS、`rekordbox_source.go`）<br>`go.mod` | **原因**：一代机没有 CIFS 文件系统，也没有 sshfs / mount.davfs；旧的挂载接口会去执行不存在的挂载，所以 rekordbox 电脑来源也用不了配置好的 SMB 共享。<br>**修法**：纯 Go 的 SMB2/3 客户端（`go-smb2`，BSD-2，不用 cgo），用 NTLMv2 用户名和密码登录：NAS 文件夹按歌单列出（含名字就叫 `root` 的文件夹）、按文件名搜索、下载到原有的缓存；控制台里沿用唯一的 NAS 配置，密码和令牌从不返回；旧的 WebDAV 地址内凭据继续可用且不会出现在响应的地址里；保存的 NAS 设置重启后恢复，连接按需建立。没有 CIFS 的碟机上，挂载接口保留 SMB 条目并校验登录和共享列表，如实返回未挂载加用户态状态。rekordbox 音频的查找顺序：内核挂载的共享 → 经 `/api/mounts` 与 `rekordbox_share` 的 Go SMB → NFS → 原有提示；SMB 与 NFS 共用同一个 `.rekordbox-cache` 预算、边拉边播和 Range；TCP 连接 / 登录 3 秒、读写空闲 30 秒的截止时间，可取消，卡住的服务器不会拖死。 | 检验：`TestSMBNASBrowseSearchAndDownload`、`TestSMBNASAPIAndRebootReload`、`TestSMBRekordboxRangeAndSharedCache`、`TestSMBRekordboxWrongPasswordNotice`、`TestSMBRekordboxServerStopsMidCopy`、`TestSMBMountWithoutCIFSNeverExecutesMount`、`TestSMBDialStalledServerHasDeadline`；批 16 的完整 `go test -race` 里 13 个真实 Samba 用例通过（要求 SMB3，含带重音的文件名）。分支 `agent/5305-smb-userspace` @ `7e7a736f`（合入 `89225d00`，检查表行 295）。**实机待验**：macOS 文件共享上的 SMB 和 Engine 的实际播放。 |
| **rekordbox 电脑来源加载更快、列表行信息完整**（11-17 至 11-18） | `pkg/prolink/nfs.go`<br>`pkg/webui/rekordbox_source.go`<br>`tools/ui-mod/engine_ui_hook.c` | **原因**：NFS 客户端每次只请求一个 8 KiB 数据块并等回复，3–8 MB/s；曲目行没有时长，BPM 和调性要等 Engine 分析后才有；两路同时读时第二路抢不到特权源端口。<br>**修法**：有界的 READ 窗口复用同一个套接字，按 `xid` 匹配回复，只重发超时的请求，把连续字节交给原有的缓存写入；并发的查找和复制在第一路占着特权端口时改用另一个空闲的特权端口，保持必须用特权端口的服务器可用；暂存预算、共享复制、Range 和 15 分钟上限不变，窗口大小由左碟上同一文件的实测选定。曲目行在加载前读取 rekordbox 的艺人、BPM、轮盘调性和时长（时长用菜单 8，按曲目 ID 跨页拼合），列表优先显示 DJ 在 rekordbox 里调整过的 BPM / 调性；一个带构建号和函数序言校验的兼容钩子只在 TIDAL 解码之后找回 rekordbox 的标记，填进 Beatport 用的同一组原生字段，其他流媒体的预览不变。 | 检验：丢包 / 乱序 / 重复 / 短读、取消、193 行列表、畸形标记、armhf 原生异常展开（没有展开表时红灯）的测试；并发查找保留特权源端口的回归用例；检查表行 306。分支 `agent/5305-rbx-speed` @ `4c636fd1`（合入 `9ed808df`）、`1b44298c`。SC5000 帧缓冲与可见的行内容确认加载前就显示 BPM 和调性。**实机待验**：最终候选上重新做一次验收；其他 Engine 构建的 ABI，和与 Engine 已分析值冲突时的显示。 |
| **rekordbox 电脑离线检测，每台电脑单独判断**（11-18） | `pkg/prolink/`<br>`internal/cli/rbbridge.go`<br>`pkg/webui/rekordbox_source.go` | **原因**：关掉 `LINK` 的 rekordbox 仍会继续广播，但它的数据库端口服务已经关闭，碟机却一直列着这个来源；两台电脑时，一台没开 `LINK` 的会遮住另一台可用的曲库。<br>**修法**：用原有的两字节 TCP 查询异步确认曲库是否在提供，有截止时间；最后一次确认成功之后，按 CDJ 同样的 10 秒超时过期，下一次刷新（最多 3 秒）移除来源，重新连接后自动出现；开机时不会从旧记录恢复在线状态；按 IP 单独记录，跳过未就绪的电脑；数据包完全停止时也会发布快照移除；已复制到本机并载入的曲目继续播放。 | 检验：`TestRekordboxPresenceWithTwoComputers`（两种播放器编号顺序）、`TestRekordboxPresenceDropsOnlyExpiredIdleIPs`、`TestRekordboxPresenceRequiresAnOfferedLibrary`、`TestRekordboxPortCancellationAndLinkOff`；分支 `agent/5305-rbx-presence` @ `5bdd0a28`（合入 `0df11db1`）和 `agent/5305-rbx-two-hosts` @ `21d44fb6`（合入 `d9ec58c6`，检查表行 314）。**真机（左碟）**：屏幕抓取确认关掉 `LINK` 后不再出现 rekordbox 来源，尽管电脑的广播一直是新的。**实机待验**：`LINK` 重新打开后重现的时间，以及播放中不被打断。 |
| **一键后端与安装器设置**（11-17） | `deploy/one-click-backend.sh`<br>`cmd/enginex-relay/`<br>`cmd/enginex-installer/` | **需求**：Stems 服务、流媒体二维码登录中继和 Apple Music 助手要分别手工部署。<br>**修法**：在 Debian 12+ / Ubuntu 24.04+ 的 EngineX 源码目录运行 `sudo bash deploy/one-click-backend.sh`，装好并启动 `enginex-relay.service` 和 HPSS Stems 服务；不用域名，全新安装自动开 Quick Tunnel（`--public` 可指向自己的反向代理），首次从后端本机读初始密码再改；重复运行保留已有的地址、密码和令牌，某个组件失败时不会打印成功；已有的 Apple Music 助手文件会被一起启动，中继自动检测它的局域网地址和令牌并同步到碟机，碟机够不到检测到的地址时再配一个可达的 HTTPS 地址；刷机后运行 `enginex-installer --setup-backend`，输入碟机地址、`设置 → 关于 / 更新` 里的控制台访问码和后端地址与密码，经碟机的 `/api/relay` 保存，拒绝重定向，也不在电脑上装任何常驻程序；升级时自动迁移旧服务、凭据和配置，旧命令保留一个版本。 | 检验：`tests/test_backend.sh`（私有挂载与网络命名空间）、`cmd/enginex-installer` 的 `TestSetupBackend*`、中继与 `pkg/webui` 的 `-race`；检查表行 305。分支 `agent/5305-relay-oneclick` @ `3a323cd7`（合入 `d5e52675`），随后维护者的中继更新 `63292fd7`、`4de15e0f`、`0e53ef50` 等已合入。**实机待验**：在预留的测试后端和碟机上迁移一个已有安装，验证二维码登录、HPSS Stems 和可选的助手自动检测与令牌同步；后端部署由维护者负责。 |
| **Apple Music 助手与中继的后续修复（维护者）**（11-17 至 11-18） | `pkg/providers/`<br>`pkg/webui/`<br>`cmd/enginex-relay/` | **修复**：折叠 MusicKit 回退并把 OmniSource 歌单排除在外（`5ced289b`）；恢复个人歌曲、专辑与目录歌单搜索，助手登录完成后同步曲库状态，退出登录会清除旧账号的浏览、曲目和专辑缓存并保留助手配置（`4efbcc88`，检查表行 311）；手机二维码设置助手、中继自动同步（`071fcd63`）、助手令牌带进二维码和跳转（`10a491fe`）；外部助手登录完成后补全碟机二维码和控制台登录（`1b5f3d64`）；复核的 P2：凭据、认证和自动检测（`5a1873dd`）；退出登录的标记在重启或网络抖动后仍然有效，标记里只留令牌的哈希，先持久化再删除所有者文件（`6f07e162`、`6d79d618`）。 | 检验：`TestAppleMusicLibrarySongsAlbumsAndSearchPlaylists`、`TestAppleMusicHelperCredentialsSyncAndDevTokenFallback`、`TestAppleMusicHelperAutoRestoreFavoritesAlbumsAndSearch`、`TestSigningAppleMusicOutClearsMemoryAndKeepsHelper`、`TestAppleMusicForgetSticksUntilNewLogin`、`TestAppleMusicForgetSurvivesDeckRestart`（含原始令牌不出现在标记里、重启和崩溃顺序、重新登录后标记被移除）；批 16 的完整 `go test -race` 通过。**实机待验**：退出登录后重启碟机，确认不会自动重新登录。 |
| **整数 BPM 推子平台**（11-17）<br>`Profile → Display → BPM Decimals` | `tools/ui-mod/build_packs.py`<br>`pkg/webui/ui_settings.go` | **需求**：推子推到整数 BPM 时没有手感。<br>**修法**：不新增设置：`配置 → 显示 → BPM 小数位数`（`BPM Decimals`）设为 `2`（默认）时，用载入曲目的速度和当前的速度范围，在每个整数 BPM 处给两个 ADC 步长的平台；`1` 保持原厂的精确线性映射；两个方向、反向推子、中位和两端保持正确，没有载入曲目或拿不到速度 / 范围时原样通过，曲目元数据里的 BPM 接受逗号小数。 | 检验：`WholeBpmTest`（`test_no_separate_whole_bpm_setting`、`test_built_speed_slider_mapping_and_bridge`）、`DeckHeaderLayoutTest::test_bpm_decimals_display_setting_and_formatting`、`TestUISettingsBpmDecimals`；检查表行 310。分支 `agent/5305-int-bpm` @ `4fe0afbd`（合入 `0d5df9e1`，原始版本 `976071a0`）。**实机待验**：JP07 和 JC11 上在 `1` 与 `2` 之间切换，两个方向推完整个行程，确认只在 `2` 时出现整数平台、中位和两端不变，重启后设置保持。 |
| **每个通道 `FILTER` 旋钮的 Color FX**（11-17，替代原来“隐藏该设置”的方案） | 见 1A-5 | 见 1A-5。注意：任务过程中曾有一个“在不支持的机型上隐藏 Color FX Style 并让保存的值失效”的分支（`agent/5305-colorfx-primego` @ `ed69051e`），**没有合入**；合入的是下面 1A-5 的通道方案。 | 同 1A-5。 |
| **选中 U 盘时弹出“复制设置”错误框**（11-16 夜，维护者在左碟上看到） | `tools/ui-mod/settings_copy.qml`<br>`tools/ui-mod/build_packs.py` | **原因**：选中带偏好配置的 U 盘时，原厂的 `ShowLoadDialog` 是 #205 钩子能听到的唯一信号，钩子去请求 `usb-preview` 生成是 / 否摘要；这个请求失败时回调走了和用户主动复制相同的模态错误框。又因为这台 Engine 的 Qt `XMLHttpRequest` 把所有非 200 回复都报成状态 0 且没有响应体，文字永远是通用的 `Settings request failed`。<br>**修法**：被动预览失败时静默，什么也不弹；有效的 是 / 否预览、用户主动复制的错误和对端的提示不变。 | 检验：`test_ui_mod.py` 新用例在基线失败；分支 `agent/5305-settings-copy-usb` @ `2b85fd5e`（合入 `ce570e12`，检查表行 315）。**实机待验**：选一个带配置的 U 盘不再弹错误框。 |
| **Mixstream 的 Stems EQ 行只在有混音台的机型上构建**（11-18，综合复核 C 的 P1 / P2 / P3） | `tools/ui-mod/build_packs.py`<br>`tools/az01-packer/mixstream-sandbox.sh` | **原因**：`配置 → 播放` 里新增的 Mixstream Stems EQ 行在不可见时也被构造，对没有软件混音台的播放器读取了空的混音台属性。<br>**修法**：改用原厂的条件入口 `EntryBase.PrefsConditionalEntry`（一个 `visible: active` 的 Loader），`active` 就是原来 Mixstream / Stems EQ 的判断；SC5000 / SC5000M / SC6000 / SC6000M 不再构造这些绑定，支持的机型行内布局不变。原生探测原来会把 `false == false` 当成通过，现在向 Engine 的真实 `ListEntry` 注入会发信号的测试翻译器并点击，核对 `0 → 1 → 0` 加另一通道不变。 | 检验：`MixstreamPadsLayoutTest` 新方法（四种播放器板号加 Mixstream 关闭都零读取、两个通道独立切换）在 `11765a5c` 上失败（`Cannot read properties of null (reading 'translator')`）、修复后通过；原生 NH08 `MIXSTREAM_NATIVE_PASS`；检查表行 317。`41061299`。**实机待验**：在有可播放 Stems 曲目的 Mixstream 上两个通道独立切换；SC5000 系列打开 `配置 → 播放` 无报错。 |

### 1A-3 综合复核后的修复（2026-11-17 07:00 起）

两次综合复核对象是候选 `1dc05caa`（相对集成分支 310 个文件）：钩子 / HFS+ / 网络解析方向 **通过**（3 条 P3 备注）；Go / 网页 / 打包方向、UI 包 / 翻译方向各 **不通过**，结论如下，全部已修复并带回归测试。

| 来源（日期） | 模块 | 原因与修法 | 检验、分支与提交、仍需实机 |
| --- | --- | --- | --- |
| **P2：`.rekordbox-cache` 清理会跟随符号链接，删除不相关文件**（11-17） | `pkg/webui/rekordbox_source.go` | **原因**：暂存逻辑对 `<盘>/.rekordbox-cache` 列目录、淘汰、写入时不看它是什么。<br>**修法**：新增 `rbxOwnCache`：缓存目录必须不存在，或用 `Lstat` 确认是目录且与盘根在同一个设备上；符号链接、普通文件、挂载上来的其他卷一律跳过，不列、不淘汰、不写入。 | `TestRekordboxCacheLinkIsNotFollowed`、`TestRekordboxCacheIsOnTheDrivesVolume`（需要挂载权限）；`4ba7f0d8`。局限：先 `Lstat` 再用，对恶意本地进程在检查之后换目录不是原子防御。 |
| **P2：rekordbox 缓存另有一份暂存预算**（11-17） | 同上 | **修法**：`.rekordbox-cache` 与 `.prolink-cache` 的字节合计占用同一个每盘预算；腾空间只淘汰旧的 rekordbox 副本，Pro DJ Link 暂存库不被碰。 | `TestRekordboxCacheSharesTheDrivesStagingBudget`；`b9e34d41`（行 282、283）。 |
| **P2：设置 JSON 接受后面跟着垃圾的数据**（11-17） | `pkg/webui/ui_settings.go` | **修法**：第一个值之后再次解码必须是文件结尾（后面的空白可以）；先于任何合并、派发和 Engine 重启，仍在原来的请求体上限内。 | `TestMalformedJSONSettingsDoNotApplyOrRestart` 加了后接垃圾和第二个对象两例（400、设置不变、不重启）；`fdc7a7d3`（行 284）。 |
| **P3：`sampler-sequencer` 包不再能复现自己的源码**（11-17） | `docs/mods/` | **修法**：用 Go 1.27.1 重建（其余六个包逐字节相同）；sha256 `28ccc2f8…`。 | `test_packaged_sources_are_the_committed_sources`、`build_official_mods.py --check`；`bc24b00f`（行 285）。分支 `agent/5305-cr-go-fixes` @ `bc24b00f`（合入 `7a3b0d5f`）。 |
| **P2：Stems“所有碟机都在放”的提示晚一次滑动才出现**（11-17） | `tools/ui-mod/build_packs.py` | **原因**：原厂的滑动项在触发动作**之前**先读提示文本，我们的补丁在动作里才设置 `scStemBusy`，所以第一次滑动显示旧文字。**修法**：在 Stems 分支里设置完 `scStemBusy` 后立刻重读一次提示列表，同一次滑动、两碟和四碟布局、所有语言下文字就是对的。 | `test_busy_message_shows_on_the_swipe_that_found_every_deck_playing`（含“忙 / 流媒体 / 忙 / 本地”的连续序列）；`50c351d7`（行 287）。**实机待验（必须）**：所有碟机都在放时滑动一次，再空出一台碟机滑动一次；节点测试按原厂顺序建模，但没有证明 Qt 在真机 QML 里立即读取脏绑定。 |
| **P2：`engine-ui-guard` 迁移脚本查找**（11-17） | `tests/test_rootfs_overlay.sh`<br>`tools/az01-packer/test_ui_mod.py` | **复核有误**：一代镜像里 `/usr/libexec/enginex-migration` 就在；二代数据包里卫士与迁移脚本并排安装，`dirname "$0"` 的回退正是为这种布局而设，所以产品代码不改。真正的问题是测试主机上没有 `/usr/libexec` 的那份，43 个子用例失败。**修法**：测试自己设置迁移脚本路径，并在 `ok 15` 里固定默认路径、一代与二代并排布局。 | `5521fe54`；合并成 `4da52987`。整套 UI 测试 269 项通过、覆盖层 38 项通过。 |
| **P3 备注**（钩子 / HFS+ 复核） | — | 3 条：一条纵深防御建议、一条容错说明、一条检查表编号核对，均不改产品。 | 钩子方向复核 **通过**：armhf / arm64 / ASan 的 `--mounts`、实时预算与回调（8400 块 0 次错过，最差 174.2 µs 对 666.6 µs 预算）、覆盖层 38 项、Pro DJ Link `-race`。 |

### 1A-4 大型 rekordbox 曲库导入（#301）

- **#301（rekordbox 导入重新设计，候选已合入，待实机验收）**：rekordbox 6.8.5 导出的 477 GB FAT32 U 盘（约 2 万首、323 个歌单）首次导入在挂载窗口里被反复杀掉，大库永远转不完。分支 `agent/5305-issue-301` 现在的做法：能在挂载窗口内放下的导出（SC5000 上约 6–7k 首以内）首插即从导出发布全部歌单、曲目和导出数据库里的热 Cue / Memory Cue（信任导出路径、批量写库），拍线和波形在之后插入时补齐，不重挂；更大的先显示原厂 OneLibrary，后台以低优先级构建，完成后等没有碟机再用这个盘（载着它的曲目，不论是否在播放，或正在读它的音频；Engine Link 对端会被询问）并安静 30 秒，再用 udev remove + add 无感重挂自动发布，不需要重新插入（等待期间提示条显示等待状态，不再提示“弹出后重新插入”；真正的重挂失败才报错；沿用 500 ms 的观察、5 秒的受阻重试和 1 秒的有界对端查询；弹出或拔盘取消发布；`agent/5305-cr-b2-fixes` @ `fe9592a0`），之后后台补拍线和波形、再按同样条件重挂一次；大曲库的更新也在后台副本上进行，发布时合并 Engine 期间写入的播放记录、准备列表和碟机上的编辑；手动弹出的盘不会被自动重挂；拔出载有曲目的只读 HFS+ 盘不再留下卸不掉的挂载。2 万首实测（限速 477 GB 镜像，原生 armhf）：带 Cue 首发 11.75 秒；SC5000 预计 1k 约 4–5 秒、5k 约 10–11 秒、2 万首约 31–35 秒（走后台加重挂）。SC5000 上的时间是估算，不是实测；**实机待验**：首插时间、后台导入、无感重挂和播放中拔盘。

### 1A-5 通道 Color FX

- **PRIME GO Color FX 错误（修复已合入候选，待实机验收）**：用户在 PRIME GO 上打开 `Color FX Style` 后曲目立即带上回声，关掉才停。**原因**：#88 的 `Color FX Style`（`pioneer_fx`）在除 PRIME 4 和独立播放器以外的机型上以固定混音深度一直作用于主输出，没有任何旋钮能调小。**修法**（分支 `agent/5305-colorfx-channel`，基于候选 `89225d00`）：钩子不再让 `pioneer_fx` 驱动主输出（任何机型）；改为挂在原厂每通道的 Sweep FX 上（一代一体机的 5.3.0 Engine 里 `SweepFxControl::setValue` `0xd8ba3c` 与 `ChannelSweepFx::process` `0xec127c`；二代 32 位机 SC LIVE 2 / 4、PRIME 4+、PRIME GO+、Mixstream Pro+ / Pro Go 共用的 `5.3.0-sclive4` Engine（BuildID `b946c3b8…`）里是代码相同的 `0xd9619c` 与 `0xecdbd4`）：选了风格且 `FILTER` 旋钮离开原厂的中位带（0.495–0.505）时，用 PX 的 DSP 代替原厂效果，转得越多越深；中位带内和选 `关` 时原厂原样运行，各通道互不影响。`Color FX Style` 只在钩子的就绪标记 `/run/enginex-colorfx.ready` 新鲜时显示（UI 包和 `pkg/webui` 都查它，不写机型清单），PRIME 4 的 PX 选择器不变，PRIME 4 上已存的风格保留。**测试**：`test_color_fx_hook.c`、`test_rt_callback.c`（armhf）、`test_ms_master_hook.c` 第 2 节、`TestColorFXStyleFollowsTheChannelKnobHook`、`test_fx_ms_ui.py`、FX 沙箱驱动 `test_colorfx_native.c`（JC11 / JC16 / JP11 / NH08 / JP07 用原厂 5.3.0 Engine，JP21 / JP20 / JC11S / NH08S / NH10 用原厂二代 Engine，离线）、`test_sclive4_bootstrap.sh` 第 2 阶段，回归清单第 304 行。**未在实机验证**；AZ04（PRIME 4 G2 / SYSTEM ONE）没有沙箱，不在本版本。

### 1A-6 热点倒数按网格拍线跳变（#151 后续）

- **热点倒数（`BARS.BEATS`）不跟 BPM / 网格（实机报告，2026-11-17 下午，Remote Library 曲目；修复待实机验收）**：用户反馈“热点倒数好像不跟 BPM，我调了节拍网格也还是不跟”。**原因**（沙箱里复现，JP07，真实 128 BPM 曲目、热点偏离网格 0.45 拍）：倒数是“到热点的拍距”取整，数字变化的时刻由**热点自己的位置**决定，不是播放头越过网格线的时刻；热点不在网格线上时（不开量化、导入的热点、或事后调了网格），数字在两条拍线之间跳（`BeatNumber` 2.451→2.557 时 8.1→7.4，2.984→3.091 越过拍线却不变）；用 Beat Grid Edit 平移网格后跳变时刻一点没动。BPM 修改、减半 / 加倍本来就会反映，推音高也不改拍数（对的），问题只在跳变相位。**修法**：`tools/ui-mod/build_packs.py` 里一个共享函数 `scCueBeatsLeft`，热点算在离它最近的网格线上；本机轨道用 Engine 的 `BeatNumber` 当播放头在拍内的位置（每个 200 ms 节拍多读一个属性，缓存不变），影子轨道用来源的拍表；热点恰在网格线上时结果与以前完全一样；热点的线在播放头后面而热点仍在前面时显示 `0.1`。**已知限制**：Engine 只给播放头处的速度，网格有锚点（速度不恒定）时，跨过锚点的远距离倒数按播放头处速度估算，播放头进入热点所在段后精确；影子轨道用来源的完整拍表，不受此限。**检验**：`ShadowDisplayTest` 新增 4 项（旧代码上 3 项失败，“热点在网格线上不变”那项两边都过），原 `#151` 影子测试全部保持通过；JP07 沙箱探针（`tools/az01-packer/cue-countdown-sandbox.sh`）在旧代码上 9 处断言失败（热点偏离网格、平移网格、减半速度三段扫描各自失败），修复后 0 处。JC11 沙箱里曲目分析不完成，没有跑出结果（与 JP07 同一份 Engine 和共享 QML / JS）。分支 `agent/5305-cue-countdown`。**实机待验**：设一个偏离网格的热点，播放接近它看数字是否正好在拍线上变；打开 Beat Grid Edit 平移网格后再看。

### 1A-6B RC2 补录（2026-11-19）

| 范围（日期） | 原因与修法 | 检验、分支与提交、仍需实机 |
| --- | --- | --- |
| **rekordbox Link、USB 与影子轨加载分析**（11-19） | Link 曲目在加载时直接复用 rekordbox 的网格、热 Cue、概览和封面，不再让 Engine 重分析；Pro DJ Link、USB 和影子轨（MP3、AAC/M4A）共用一个起始偏移规则，旧版转换行只校准一次并保留碟机编辑。影子轨同时修复另一台碟机加载后概览黑色或延迟的问题。 | `RekordboxAnalysisSitesTest`、`ShadowDisplayTest`、`test_rekordbox_analysis.c`；rc2 分支 `agent/5305-rc2` @ `d02500d4` 及其合入的 rekordbox / shadow 提交。**实机待验**：Link 与 USB 加载时网格、Cue、概览和封面即时出现，影子轨概览配色一致。 |
| **U 盘曲库更新安全**（11-19） | 更新读不出大量文件名（例如错误字符集）时不删除或重写任何曲库内容，并把任务留到下一次挂载重试。 | `rb-bridge` / USB 更新回归测试；rc2 提交 `ea7b581f`。**实机待验**：插入含异常文件名的 U 盘后曲库不变，下一次挂载可继续。 |
| **#172 Hi-Fi Output**（11-19） | 第一代 SC5000 / SC6000 独立播放器继续保持默认关闭的可选高保真输出，三语 README 与网站表格同步列出测量结果。 | 三语 README 表格、网站文档审计；rc2 已合入候选。**实机待验**：`Profile → Playback → Hi-Fi Output` 开关与输出确认。 |
| **Auto-Gain 余量与采样率一致性**（11-19） | 减少泵动，安静曲目提升到约 `-4.4 dBFS`，最多 `+9 dB` 并带削波保护；44.1 / 48 / 96 kHz 使用同一时序。 | Auto-Gain 单测与 44.1/48/96 kHz rate-scaling 回归；rc2 提交 `836f5157`。**实机待验**：不同采样率曲目切换时增益平稳且不削波。 |
| **Mixstream Pro / PRIME GO 四垫页面**（11-19） | Stems 固定在 pad page 1，`CUE` 返回 cue page，避免四垫机型切页后落入错误页面。 | `test_ui_mod.py` pad-page 回归；rc2 分支 `agent/5305-pads-qml-fix`。**实机待验**：Mixstream Pro / PRIME GO 实体垫页和 CUE 返回路径。 |
| **Windows 一键安装器识别 PRIME 4 G2**（11-19） | Firmware Update Mode 的 PRIME 4 G2 设备识别路径纳入 Windows 安装器枚举。 | Windows installer discovery tests；rc2 提交 `7e27deea`。**实机待验**：PRIME 4 G2 进入 Firmware Update Mode 后被识别。 |
| **#301 后台发布继续运行**（11-19） | 重新插拔或被拒绝的 Eject 不再把自动发布任务置为终止；任务等待并在下一次挂载或可用时继续。 | `TestIssue301WaitsThenRemountsAutomatically`、bridge/cli race 套件；候选 `agent/5305-cr-b2-fixes` @ `fe9592a0`。**实机待验**：播放中不重新插入也能完成发布。 |
| **文档合入与菜单路径**（11-19） | 合入 `origin/agent/5305-release-docs-final`（`37c3ab8e`），将当前 rc2 事实补入三语 README、网站 What's New 和匹配功能段；菜单路径按 `tools/ui-mod/i18n.tsv` 与手册核对。 | 本分支 `agent/5305-rc2-docs`；合并提交与本次文档提交。`node --check docs/site/content.js`、文档审计和 `git diff --check` 待本分支完成后运行。 |

### 1A-7 最后一批（2026-11-18 上午）

| Issue / 复核（日期） | 模块 | 原因与修法 | 检验、分支与提交、仍需实机 |
| --- | --- | --- | --- |
| **#301 自动发布**（11-18，维护者否决了“要重新插入”） | `internal/cli/`<br>`tools/ui-mod/` | **原因**：综合复核 B2 的做法把一次观察到的对端传输永久当作否决，沉默的 Engine Link 对端也要求重新插入；维护者明确不接受。<br>**修法**：标记只记录观察开始时间和最近一次未确认的外发音频读取，不再是永久否决；沉默的原厂 / CDJ 对端不再永远阻止；30 秒安静期（相当于 60 个观察周期）；等待状态在四种语言里显示；重挂助手交接后刷新状态，旧导入任务不会再把 `publishing` 盖回 `waiting`；挂载和提供两个观察线程在取消时先汇合再返回。 | 检验：`TestIssue301WaitsThenRemountsAutomatically`（4 个子用例在基线全部失败）、`internal/cli` 带 `-race`、`UsbPreparingFeedbackTest` 4 项、bridge / cli / enginedb / rekordbox / webui 整套 `-race`。分支 `agent/5305-cr-b2-fixes` @ `fe9592a0`（独立生产复核通过）。**实机待验**：大库完成后不重新插入就换成转换好的曲库；播放中的碟机不被打断。 |
| **#309**（11-18）<br>SC LIVE 4 上 Wi-Fi 省电没关 | `pkg/`（纯 Go 的 nl80211）<br>`rootfs-overlay/`（debloat）<br>`internal/cli/`（stream-sync） | **原因**：debloat 只在有 `iw` 时才运行 `iw dev wlan0 set power_save off`；没有原厂 `iw` 的机型（包括 SC LIVE 4）这一步被悄悄跳过。对 25 个官方镜像（11 种不同的 rootfs）的审计：只有 4.3.4 带 `iw`。<br>**修法**：保留纯 Go 的 nl80211 读写省电状态（校验对齐、序号、协议族、内核发送方与 ACK / errno，套接字 3 秒超时）和显式的 suite 小程序，开机时应用；原厂 `iw` 存在时仍优先用原厂（3 秒超时），缺失才回退；stream-sync 同样先原厂后回退，等 wlan0 起来后应用一次。丢弃了覆盖整个 `iw` 的包装脚本和周期性重应用。 | 检验：独立复核（Sonnet）v2 通过；纯 Go netlink、stock-first 运行时测试、authorized ARM build host 上的整套必需检查通过。分支 `agent/5305-issue-309` @ `0e137273`，候选 `1fc18b2c`；开机顺序修正（Wi-Fi 回退排在运行时准备之后，`enginex-debloat.service`）候选 `375e412f`。**实机待验**：SC LIVE 4 上 `iw dev wlan0 get power_save` 为 off。 |
| **综合复核 B1 的 Apple Music 与中继修复**（11-17 至 11-18） | `pkg/providers/`<br>`pkg/webui/`<br>`cmd/enginex-relay/` | **F1**：在碟机上退出 Apple Music 后，下载助手仍在登录，下一次状态轮询又把凭据同步回来 → 记住“已退出”，直到在碟机上显式登录助手。**F2**：中继拉取把碟机的助手地址换成另一个，却保留旧令牌，旧令牌发给了新主机 → 地址不同且没带令牌时清除本地令牌。**F3**：持有手机登录二维码的人可在碟机没配置助手时设置助手并推送到中继 → 手机设置只在碟机本地，且“是否已配置”与写入在同一把锁里。**F4**：中继控制台点保存会丢掉自动探测到的令牌 → 不再用探测值预填，保存后仍自动。**F6**：自动探测的地址范围 `10/8`、`172.16/12`、`192.168/16`、`100.64/10`，其余拒绝。 | 复核记录：B1 第二轮 F1–F4、F6 全部关闭（8 个并行的 `setup` 请求只有 1 个被接受）。含在候选 `c7ca61e0`。仍有两条低级别备注（N1、N2）见复核报告。 |
| **综合复核 B1/F5：两台 rekordbox 电脑**（11-18） | `pkg/webui/rekordbox_source.go` | **原因**：第一台在广播但没开 `LINK` 的电脑让 `rekordboxHost` 直接返回空，遮住后面真正提供曲库的电脑；所有 IP 共用一个确认槽。<br>**修法**：确认与查询状态按 IP 记在同一把锁下，选择时跳过未就绪的电脑；沿用“第一台就绪的曲库”，不加选择器。 | 检验：两个真实 TCP 测试服务在两个回环地址上，对调播放器编号的两个用例；在基线上 `off_first` 失败（`ready computer hidden`）。分支 `agent/5305-rbx-two-hosts` @ `21d44fb6`，含在候选 `d9ec58c6`。 |
| **综合复核 C：Mixstream 的 `Stems EQ` 行**（11-18） | `tools/ui-mod/build_packs.py` | 只属于 Mixstream 的行不再在没有软件混音器的播放机上创建绑定（见检查表行 317）。 | `MixstreamPadsLayoutTest`；含在候选 `41061299`。 |

### 1A-8 已合入候选（#304 `c63edf0f`、#305 `73091d1a`，候选 `375e412f`）


| Issue（日期） | 模块 | 原因与修法 | 检验、分支与提交、仍需实机 |
| --- | --- | --- | --- |
| **#304**（11-18）<br>已登录 Engine DJ 账号时载入流媒体曲目出现“Tidal Disconnected” | `tools/ui-mod/engine_ui_hook.c` | **原因**：原厂流媒体控制器独立于我们的 TIDAL 网关接收账号凭据更新；凭据被清空时会把我们本地提供的承载账号当成云端管理的 TIDAL 来断开。#159 的补丁只强制了已连接 / 未连接的取值，拦不住这条独立的回调。Gen-1 与 SC LIVE 4 的原厂代码路径相同。<br>**修法**：钩子（沿用按 BuildID 选择的补丁表）只替换 TIDAL 的“个人资料单点登录能力”槽，对本地承载账号返回 false，真正的 TIDAL（作用域 `tidal`）仍返回原厂的 true；不改网关路由、合成令牌、已存的真实 TIDAL 凭据、个人资料会话和登录界面。 | 检验：新的原生 ELF 回归在 Gen-1、SC LIVE 4、AZ04 上通过；对旧候选钩子失败（退出码 134）；右碟用修好的钩子做凭据更新回调：`profile=true carrier=true`（旧钩子是 `carrier=false`）；检查表行 318。分支 `agent/5305-issue-304` @ `0ca2d877`（`f174c22c`）。**实机待验**：原始的“载入流媒体曲目”复现路径没有在真机上完整重放。 |
| **#305**（11-18）<br>碟机顶栏对所有服务都显示 TIDAL 图标 | `tools/ui-mod/build_packs.py` | **原因**：Engine 对所有服务的曲目都报 `Track/Source = TIDAL`（它们都走 TIDAL 槽），顶栏画的就是 TIDAL 图标，`Track Info` 的 `Source` 行也写 TIDAL；Engine 没有告诉界面真正的提供方。<br>**修法**：界面包里按当前选中的服务显示该服务自己的图标和名字（Deezer、Spotify、Apple Music、网易云、SoundCloud、Beatport、rekordbox）；真正的 TIDAL、其他来源和本地曲目不变；不做网络请求，不碰音频路径。**局限**：跟随“当前选中的服务”，不是每首曲目各自的事实。 | 检验：界面包测试；有截图。分支 `agent/5305-issue-305` @ `bf3e5b2f`（只改界面包，仅 5.3.0）。**实机待验**。 |


### 1A-9 RC3 fixes (2026-11-19)

| Fix | Cause and change | Verification and real-deck status |
| --- | --- | --- |
| **Hot Cue retention and rekordbox shadow edit refresh** | A capped BridgeAnalysis state database can prune the analysis row even while BridgeState holds the track's full fingerprint. The rebuild now retains deck-set Hot Cues in that case. A changed PerfGen also refreshes rekordbox cue/grid sidecars on the shadow copy while reusing cached audio. | `pkg/bridge::TestLargeReexportKeepsDeckCueWhenBridgeAnalysisCapped`, `pkg/webui::TestShadowPerfGenRefreshesWithoutDownload`, and `TestShadowRekordboxEditChangesPerfGenerationWithoutNewAudioKey`; checklist rows 359 and 360 are **pending real-deck run**. |
| **Deezer OmniSource isolation** | Deezer's OmniSource scope keeps USB/SD, NAS, djay, and self-hosted playlists while excluding playlists owned by other streaming services. | `pkg/webui::TestOmniSourceDeezerPlaylistsExcludeOtherStreamingServices` and `TestOmniSourceListsDrivePlaylistsInDeezerScope`; checklist row 358 is covered by automation, with real-deck status recorded in the final walk. |


## 1B. 没有合入 5.3.0.5 的工作（不得写成已有功能）

以下是整理时（候选分支头 `3e4c7a3a`）**不在候选里**的内容，用户文档和发布说明里不得写成已有功能。

- **拔盘后播放不中断**：只做了可行性研究，**没有任何实现**。结论：原厂 Engine 本来就会把载入的曲目复制到 `/data/inMusicTempFiles` 并播放副本，沙箱里拔盘 8 次（vfat / exFAT / HFS+）播放都没有停；缺口是 exFAT 在复制完成前拔盘会无声地显示播放中；合成的“移除再添加”可以在播放中不可见地重新挂载（#301 的“无感重挂”据此成立）。研究中发现的“拔出载有曲目的带日志 HFS+ 盘会留下卸不掉的挂载”已在 #301 里修复（见 1A-4）。
- **#244（PRIME 4 G2 / SYSTEM ONE 的 `/data` 分区大小）**：不在 5.3.0.5，需要这两款机型用户的只读分区测量。
- **#286 的第 3、4 项**：没做（第 4 项是维护者的决定）；第 1、2 项已合入。
- **rekordbox 来源的 WAV / AIFF、rekordbox Link 即时波形 / Cue / 拍线**：不在 5.3.0.5（维护者决定，除非 09:45 前另行通知）；只有任务书或分支，没有合入。
- **综合复核 B2 的修复队列**：在候选之外，另行复核。
- **碟机侧的两个已取消方案**：电脑上预处理 U 盘（`agent/5305-usb-prep-wip`，已停止）和“在不支持的机型上隐藏 Color FX Style”（`agent/5305-colorfx-primego`，被通道方案取代）都不会合并。

---

## 2. 实机验收与待实机验证项目清单

以下项目在本次 dry-run 中**没有**实机结论；结论仍为 `pending real-deck run`，由维护者或发版人后续逐项确认：

- **蓝牙音箱输出**（行 187、188）：维护者 2026-11-14 在真实 SC5000 上试听并认可音质；其余机型的 `输出来源` 选项、延迟约 150–250 ms、已连接手机等输入设备时面板停止扫描的提示仍待确认。`pending real-deck run`
- **复制设置与 USB 配置导入**（行 200、202、210）：两台碟机的 Link / USB / 撤销验收，Player 编号保留。`pending real-deck run`
- **分侧反转转盘**（行 205）：物理转动搓碟、外圈微调、`Shift + 转盘`。`pending real-deck run`
- **`推子回音`**（行 190）与 **电动转盘 4 轨**（行 186）：一体机真机音频，`SC5000M` / `SC6000M` 真机。`pending real-deck run`
- **有线 + Wi-Fi**（行 192、193、213）：两台 SC5000，三次以上开机，`ip route get <对端碟机>` 为 `dev eth0`，日志无 `online check failed`。`pending real-deck run`
- **3-Band 与概览**（行 194–196、203、219）：对照 CDJ-3000 / rekordbox，旧导入 U 盘重插后 Half / Full 高度。`pending real-deck run`
- **Live Stream**（行 197、223）：真实 Icecast 服务器播放推送；无听众时 `stream-sync` 占用。`pending real-deck run`
- **碟机本机 Stems**（行 222）：播放中的耗时（未测）。`pending real-deck run`
- **签名更新与安装器**（行 215、216、218）：屏幕从 r5 更新到已签名发布、拒绝未签名测试发布；WinUSB 绑定与 USB 传输；双击安装。`pending real-deck run`
- **恢复官方固件**（行 201、217）：真实 SC5000 官方刷入并回到 EngineX。`pending real-deck run`
- **PRIME 4 效果选择与视图**（行 246、247、249–251）：PRIME 4 连续浏览效果、旋钮一格一个效果；`SHIFT + VIEW`、`LOAD` 与 `联机视图` 的显示。`pending real-deck run`
- **西班牙语**（行 248）：选 Español 重启 Engine，检查菜单重音与排版、控制台和手机页切换。`pending real-deck run`
- **高速 SD 卡**（行 252）：RK3288 机型上 `caps`、`ios` 的 `timing spec: 2 (sd high-speed)`、约 23 MB/s 读速，播放中切换的提示，开着重启后的状态。`pending real-deck run`
- **U 盘**（行 209、211、212、214、224、225、226）：HFS+ 弹出与更新、暂存库占用、对方盘无 Stems、SD 重插与用户数据恢复、NH08 Spotify。`pending real-deck run`
- **SC LIVE 4**：Remote Library 封面（行 185）、演出曲目表导出与原厂 History 对照（行 206）。`pending real-deck run`
- **夜间补录的验收项（2026-11-17）**：
  - **#298**：PRIME 4 上从 USB 和内置盘加载曲目，正在播放的另一台碟机不再断音；若仍断，按 1A-2 里列的备选嫌疑逐项排查。`pending real-deck run`
  - **影子轨微调、超前、循环**（行 277）：两台碟机，微调时影子几十毫秒内跟上，稳态不超前，1–2 拍循环不冲过；日志出现 `nudge-ready`。`pending real-deck run`
  - **rekordbox 来源 / 单一 Pro DJ Link 开关**：Mac 上的 rekordbox 7（Export 模式、LINK）出现在碟机来源列表，曲目和歌单可浏览，共享文件夹和 NFS 两条加载路径、慢速加载、两台碟机同时用；开启开关后 rekordbox 出现 `LINK`（需要插入 rekordbox U 盘）。`pending real-deck run`
  - **HFS+ 读写**：一代机（草稿盘）已在左 SC5000 上实测一轮；仍需：AZ04 实机、macOS `fsck_hfs` 复查、大盘 `fsck -y` 耗时、碟机建的库搬到盘上；**二代 32 位机（lklfuse）完全没有真机结果**：干净 / 未干净 / 日志盘、Engine 读写、干净弹出、播放中拔盘。`pending real-deck run`
  - **#271 写回 Cue**（行 266）：CDJ-3000 / NXS2 加 rekordbox 7 的设备视图核对。`pending real-deck run`
  - **Stems**：左碟网易云弹窗消失（需带 `stream-save` 的卫士）；所有碟机都在放时滑动提示在**同一次滑动**出现（行 287）。`pending real-deck run`
  - **#299**：每款二代机上用密码和公钥登录。`pending real-deck run`
  - **Mixstream**（行 278、279）：`Pitch Bend → Beat Jump` 的按键与 LED、四个 FX 键的分配。`pending real-deck run`
  - **Windows 安装器**（行 280）：干净 Windows 电脑加实机。`pending real-deck run`
  - **从 5.3.0.4-r5 和 5.3.0.2 真机升级**（行 231–236，#234 最终版）。`pending real-deck run`
  - **蓝牙**：支持更宽码率范围的音箱，对比旧钩子；Wi-Fi 流媒体打开与关闭各听 10 分钟。`pending real-deck run`
  - **#301 大型 rekordbox U 盘**（行 276 一带的 #301 行）：首插几秒内可用、更大的库后台导入、完成后等没有碟机再用该盘并安静 30 秒后自动无感重挂（不重新插入）；播放中拔盘、手动弹出的盘不重挂、HFS+ 拔出不留死挂载。`pending real-deck run`
  - **Color FX（通道 `FILTER` 旋钮）**：PRIME 4、PRIME 2、PRIME GO、Mixstream Pro 以及二代 SC LIVE 2 / 4、PRIME 4+、PRIME GO+、Mixstream Pro+ / Pro Go 上，旋钮离开中位带才起效，中位带和选 `关` 时与原厂一致；AZ04 隐藏此项。`pending real-deck run`
  - **rekordbox 来源**：`LINK` 关闭后来源消失、重新打开后重现的时间；NFS 加载速度和 Engine 对慢流的反应；两台碟机同时用。`pending real-deck run`
  - **CDJ-3000 联机（#198）**：真实 CDJ-3000（固件 3.2.2）不再提示“已连接到固件较旧的播放器”，碟机作为 5 / 6 号时不被踢掉。`pending real-deck run`
  - **Mixstream △ 按钮与 Stems EQ**：Sweep FX 的 `△` 按钮、`配置 → 播放` 里每通道的 Stems EQ 切换、控制中心磁贴全部可见。`pending real-deck run`
  - **Apple Music**：退出登录后重启不自动重新登录；PRIME 4 G2 / SYSTEM ONE 的原生登录；经手机二维码的助手登录。`pending real-deck run`
  - **SMB**：macOS 文件共享上的 SMB 共享和 Engine 的实际播放。`pending real-deck run`
  - **整数 BPM 平台**（`配置 → 显示 → BPM 小数位数`）、**切层速度**（#222 后续）、**选 U 盘不再弹复制设置错误框**。`pending real-deck run`
  - **维护者明确“不需要”**的项目（不是待办）：DMX / LC001 的碟机检查和播放发热测量；独立的 MIDI、原厂摆放和播放 / 加载检查不因此取消。

---

## 3. Tests and three release gates

Phase C step 1 completed the required release-workflow test set on `2cec585e`: formatting, Go vet/tests with race detection, the rootfs overlay test, both bootstrap fixtures including JC11S and AZ04, and the `tools/az01-packer` and `tools/stems` unit suites. C sources were unchanged since `dfb5ee46`, so the C tests were skipped. The requested rc3 recheck on `ed3b5fa70ef5f9c7a78a7dcc4a96d26c715a28d7` also passed: `gofmt -l .`, `go vet ./pkg/webui`, and `go test -race ./pkg/webui`.

- **JP07 / SC5000 group**: `release-gate.sh` exit code 0, `176 PASS, 0 FAIL`; static audit `65 files, 0 FAIL, 9 WARN`; 20 Engine patches and 21 DSP guards.
- **JP13 / SC6000 group**: `release-gate.sh` exit code 0, `176 PASS, 0 FAIL`; static audit `65 files, 0 FAIL, 9 WARN`; 20 Engine patches and 21 DSP guards.
- **JC11 / PRIME 4 group**: `release-gate.sh` exit code 0, `176 PASS, 0 FAIL`; static audit `65 files, 0 FAIL, 9 WARN`; 29 Engine patches and 21 DSP guards.
- Each gate used the published `v5.3.0.4-r5` representative image after its SHA-256 was checked against that release manifest. The expected sandbox `unsupported operation` line is caused by the absent eMMC fixture and did not fail a gate.

The rc3 build, image checks, signing check, and gates ran on the authorized ARM build host. No real deck or production service was touched.

---

## 4. Release artifacts and SHA-256 table (all 28 v5.3.0.5 artifacts)

- **Manifest**: the table below comes from the signed rc3 `SHA256SUMS.txt`; it has 29 entries (28 artifacts plus `RELEASE-TAG.txt`), and the output contains 31 files including `SHA256SUMS.txt` and `SHA256SUMS.txt.sig`.
- **Artifact directory**: `/var/tmp/enginex-release-5.3.0.5-rc3` on the authorized ARM build host.
- **Rollback point**: tag `v5.3.0.4-r5` (`9e827205ae331d2d896d97207032556dc4202af5`).

### First-generation firmware images (`*-Update.img`)

| Group | Model | Board | File | SHA-256 |
| --- | --- | --- | --- | --- |
| SC5000 | Denon DJ SC5000 Prime | JP07 | `SC5000-5.3.0.5-EngineX-OS-Denon-SC5000-Prime-JP07-Update.img` | `03079438dcbd347333b4984276e98e3ed8613c76a7412accaaa887f7d18ffc30` |
| SC5000 | Denon DJ SC5000M Prime | JP08 | `SC5000M-5.3.0.5-EngineX-OS-Denon-SC5000M-Prime-JP08-Update.img` | `03079438dcbd347333b4984276e98e3ed8613c76a7412accaaa887f7d18ffc30` |
| SC5000 | Denon DJ PRIME GO | JP11 | `PRIMEGO-5.3.0.5-EngineX-OS-Denon-PRIME-GO-JP11-Update.img` | `03079438dcbd347333b4984276e98e3ed8613c76a7412accaaa887f7d18ffc30` |
| SC6000 | Denon DJ SC6000 Prime | JP13 | `SC6000-5.3.0.5-EngineX-OS-Denon-SC6000-Prime-JP13-Update.img` | `259120f2475acb6eab3cc984ccbe64b0983af3e4dd7923ae813bcc7e6226e647` |
| SC6000 | Denon DJ SC6000M Prime | JP14 | `SC6000M-5.3.0.5-EngineX-OS-Denon-SC6000M-Prime-JP14-Update.img` | `259120f2475acb6eab3cc984ccbe64b0983af3e4dd7923ae813bcc7e6226e647` |
| PRIME 4 | Denon DJ PRIME 4 | JC11 | `PRIME4-5.3.0.5-EngineX-OS-Denon-PRIME-4-JC11-Update.img` | `d52a9bb1873a3402577a3152c0fe7f631abbad17e89d678a07b7ed2cff744fb6` |
| PRIME 4 | Denon DJ PRIME 2 | JC16 | `PRIME2-5.3.0.5-EngineX-OS-Denon-PRIME-2-JC16-Update.img` | `d52a9bb1873a3402577a3152c0fe7f631abbad17e89d678a07b7ed2cff744fb6` |
| PRIME 4 | Numark Mixstream Pro | NH08 | `MIXSTREAMPRO-5.3.0.5-EngineX-OS-Numark-Mixstream-Pro-NH08-Update.img` | `d52a9bb1873a3402577a3152c0fe7f631abbad17e89d678a07b7ed2cff744fb6` |

### Second-generation unified update images (`*-Update.img`, #215 primary format)

| Architecture | Model | Board | File | SHA-256 |
| --- | --- | --- | --- | --- |
| AZ0x (armhf) | Denon DJ SC LIVE 4 | JP21 | `SCLIVE4-5.3.0.5-EngineX-OS-Denon-SC-LIVE-4-JP21-Update.img` | `32f9e920afbf52c92961fd759f8a5a497ddc4f5c337a5db9c9a0d08f00a93978` |
| AZ0x (armhf) | Denon DJ SC LIVE 2 | JP20 | `SCLIVE2-5.3.0.5-EngineX-OS-Denon-SC-LIVE-2-JP20-Update.img` | `da21eab755ec7dc4e9f0b940166e2b2d49302951ab0e91074e99ba9e43caacaa` |
| AZ0x (armhf) | Denon DJ PRIME 4+ | JC11S | `PRIME4PLUS-5.3.0.5-EngineX-OS-Denon-PRIME-4-Plus-JC11S-Update.img` | `11b522f26cdb0b8c51db4d253ba4884dabd9d062d31e8f9abca8d4bdd437bb2d` |
| AZ0x (armhf) | Denon DJ PRIME GO+ | JP11S | `PRIMEGOPLUS-5.3.0.5-EngineX-OS-Denon-PRIME-GO-Plus-JP11S-Update.img` | `3fab5a5540532961b466ae1ca9d267a54642f3bb2042f9b2516af36ff8fa3010` |
| AZ0x (armhf) | Numark Mixstream Pro+ | NH08S | `MIXSTREAMPROPLUS-5.3.0.5-EngineX-OS-Numark-Mixstream-Pro-Plus-NH08S-Update.img` | `9347c3abda6f8289afb7e373e848b92bbd7f1b44c600298be7df466c830feb7f` |
| AZ0x (armhf) | Numark Mixstream Pro Go | NH10 | `MIXSTREAMPROGO-5.3.0.5-EngineX-OS-Numark-Mixstream-Pro-Go-NH10-Update.img` | `d9362b521ebb717a6563d9f6014e0824001759e419a97645f396c390e5a38252` |
| AZ04 (aarch64) | Denon DJ PRIME 4 G2 | JP24 | `PRIME4G2-5.3.0.5-EngineX-OS-Denon-PRIME-4-G2-JP24-Update.img` | `d1af491bd694cbfcd3efdfac68377ab5ad661f8c0b5e28d9f495be1cdd6fb4ba` |
| AZ04 (aarch64) | Rane SYSTEM ONE | RMZ2 | `SYSTEMONE-5.3.0.5-EngineX-OS-Rane-SYSTEM-ONE-RMZ2-Update.img` | `245368f9e67a98ea51985d6ce335ecc5e50434cdf142724f3d86ee27c6f86397` |

### Second-generation legacy OTA data bundles (`*-Data-Bundle.tar.gz`)

| Architecture | Model | Board | File | SHA-256 |
| --- | --- | --- | --- | --- |
| AZ0x (armhf) | Denon DJ SC LIVE 4 | JP21 | `SCLIVE4-5.3.0.5-EngineX-OS-Data-Bundle.tar.gz` | `edd6cd5d293b6f91a9c36f46e06854079599d79fe987a24fda21978644b92534` |
| AZ0x (armhf) | Denon DJ SC LIVE 2 | JP20 | `SCLIVE2-5.3.0.5-EngineX-OS-Data-Bundle.tar.gz` | `636b537c7118883852a7749589ff5e200ea175b09835a640fa5fa5cecfe3afd5` |
| AZ0x (armhf) | Denon DJ PRIME 4+ | JC11S | `PRIME4PLUS-5.3.0.5-EngineX-OS-Data-Bundle.tar.gz` | `52ab1ebfbcd5eb688b6253a1ddda47f8a59018d8fe74e4483fc8623cb85bdf68` |
| AZ0x (armhf) | Denon DJ PRIME GO+ | JP11S | `PRIMEGOPLUS-5.3.0.5-EngineX-OS-Data-Bundle.tar.gz` | `70d901d475b92c0437e76c0fad99c423b525cc513687653e19820172794c9406` |
| AZ0x (armhf) | Numark Mixstream Pro+ | NH08S | `MIXSTREAMPROPLUS-5.3.0.5-EngineX-OS-Data-Bundle.tar.gz` | `552b71f1f659cf3d11d5e9bb48c681648f6a5c0767bf19af0fe33a65a8847414` |
| AZ0x (armhf) | Numark Mixstream Pro Go | NH10 | `MIXSTREAMPROGO-5.3.0.5-EngineX-OS-Data-Bundle.tar.gz` | `cc1fb9df0e40653429f6a06686be6c56ed2a236f8ad50b958a4e9bfb64f058bd` |
| AZ04 (aarch64) | Denon DJ PRIME 4 G2 | JP24 | `PRIME4G2-5.3.0.5-EngineX-OS-Data-Bundle.tar.gz` | `063d93c9b9c3b56f5091772f4ca6191d92f98677ef33c7c53c187b0999b33b1d` |
| AZ04 (aarch64) | Rane SYSTEM ONE | RMZ2 | `SYSTEMONE-5.3.0.5-EngineX-OS-Data-Bundle.tar.gz` | `edf4e0181f80afcaca717de186269004826754683e20c57defa6bd18a1cd57e1` |

### Cross-platform one-click installers (`EngineX-Installer`)

| Platform | File | SHA-256 |
| --- | --- | --- |
| Windows 10/11 x64 | `EngineX-Installer-5.3.0.5-windows-amd64.zip` | `34353e8d711c0110cd412c0dee67320d682a5725e428a410dbe880ed58bd4673` |
| macOS Apple Silicon (M1–M4) | `EngineX-Installer-5.3.0.5-macos-arm64.zip` | `a7f879a9c8c634852724bdfe1e51803a723ee90128c3975bb376c9eed618861a` |
| macOS Intel | `EngineX-Installer-5.3.0.5-macos-amd64.zip` | `99d7d3631581432ddc08941b0e8f3815436975cea52546796b9e8a407fbc4bcd` |
| Linux x86_64 | `EngineX-Installer-5.3.0.5-linux-amd64.tar.gz` | `f1feb8362c90841888ca657a9e7f675242e46ba5f3f686850cd5325aeb60b8b6` |



---

## 5. 实机影响与升级说明

- **一代机（8 款 `*-Update.img`）**：已刷 `5.3.0.4`（含 r2–r5）或更早版本的碟机，发布后可在 `设置 → 关于 / 更新` 收到 5.3.0.5 在线升级；也可把对应 `*-Update.img` 放入 FAT32 / exFAT U 盘根目录，经 `设置 → 关于 / 更新 → 固件更新模式 → 选项` 选 `USB/SD`。
- **二代机**：已解锁的二代机在屏幕上更新，或用 U 盘 `*-Update.img`；`5.3.0.2` / 未解锁 `5.3.0.3` 的二代机继续通过随附的 `*-Data-Bundle.tar.gz` 屏幕 OTA（最后一次，`5.3.0.6` 起退役）；原厂二代机首次安装用 `EngineX-Installer`，会清空机身内置存储，请先备份。
- **5.3.0.4 及更早的碟机不验签**：先更新到 5.3.0.5，之后的更新才会验签；已签名但比碟机当前版本旧的发布会被拒绝（同版本可重装修复），有意降级用安装器的 `--image` / `--bundle`。
- **回到原厂**：先启动一次 5.3.0.5 再正常关机，然后运行 `Restore-Official-Firmware.*`；原厂已卡在 Logo 且无法 SSH 时，先用安装器刷入 EngineX 并让它启动，再运行恢复工具（原厂刷机保留 `/data`，旧 QML 缓存不会因再刷一次原厂而消失）。
- **配置与数据保留**：升级保留 `/data/enginex` 下的流媒体登录态、歌单缓存、视图预设、已装 Mod 与 SSH 公钥；`Profile → Save to my drive` 现在也保存 EngineX 偏好。
- **HFS+（新）**：一代机和 PRIME 4 G2 / SYSTEM ONE 上 Mac 格式的 U 盘 / SD 卡可读写（检查通过的卷才可写，否则仍是只读视图）；**二代 32 位机（SC LIVE 4、SC LIVE 2、PRIME 4+、PRIME GO+、Mixstream Pro+、Mixstream Pro Go）通过 `lklfuse` 读写**：检查通过的卷才可写，弹出会等数据写完（最多约 15 秒），播放中突然拔盘仍可能丢失脏缓存；没有在真机上验证过。5.3.0.4（含 r5）上干净、未启用日志的 HFS+ 盘会以读写挂载在有 bug 的内置驱动上（见 1A-2），更新到 5.3.0.5 修复。
- **已知限制**：二代 32 位机的 HFS+ 读写（`lklfuse`）没有在真机上验证；rekordbox 大库的后台发布会等没有碟机再用该盘并安静 30 秒后自动进行（不需要重新插入），重新挂载时列表会短暂消失一次（#301，待实机验收）；一体机的 Color FX 由各通道 `FILTER` 旋钮控制，AZ04 隐藏此项，实际旋钮路径待实机验收；Apple Music 需要订阅；蓝牙约增加 150–250 ms，不适合打拍对齐；碟机本机 Stems 不是神经网络，仍有串音；`Live Stream` 只有 WAV；`#247` 的 SoundCloud 保存、`#268` 的 Remote Library 记录与 Prepare 仍未完成；`#253` 大 U 盘识别速度需实机计时；`#244`（PRIME 4 G2 / SYSTEM ONE 数据分区大小）**不在**本版本，需要这两款机型用户的只读分区测量（#291）；`#298` 的修复是“很可能的原因”，没有在 PRIME 4 上证实。
- **已知问题（Spotify）**：许多账号（包括较早的 Premium 账号）直接播放 Spotify 会失败，提示 “audio key refused”。Spotify 更改了服务器，所有基于 librespot 的客户端都无法获得音频密钥（librespot-org/librespot#1649，2026-11-07 已锁定，没有已知修复）。5.3.0.5 因此关闭了 Spotify 的直接音频，登录、歌单、收藏和搜索仍可用，播放时按你选的音源（`auto` 或 Deezer / 网易云 / Beatport / Apple Music / SoundCloud）在已登录的服务上匹配同一首歌，`auto` 按这个顺序尝试；没有登录任何一个匹配服务时无法播放。

---

## 6. 本次修复的 Issue 清单

#205、#213、#214（PR）、#216、#222（BH-GF3-12 / BH-GC2-10，另有切层速度的后续）、#223、#233、#237、#238、#239、#242、#243、#246、#245、#247、#248、#252、#253（识别速度待实机验收）、#285、#287（PR）、#282（PR，Javier Saenz）、#255、#258、#261、#263、#264、#266（PR）、#268（仅导出已有历史）、#269、#272、#273（PR）、#274、#277、#279、#281、#283。同时合入 #234 更名（最终版）、#215 的 OTA 桥接（不含 #244）、#236 的后台负载收敛、#294（PR，Apple Music 与 Spotify 音源选择）。

**11 月 16 日至 18 日补录**：#99（`Shift + View`）、#151 后续（热点倒数）、#195 / #207（效果）、#198（CDJ-3000 联机）、#271（写回 Cue）、#286（四项里的第 1、2 项）、#292（Remote Library 的 `Date Added`）、#293（Mod）、#296（`cpu-ram-monitor` Mod）、#298（加载时卡顿，需实机确认）、#299（二代机 SSH 密码）、#300（没有 CIFS 的机型读 SMB）、#301（大型 rekordbox 曲库导入与自动发布，待实机验收）、#309（所有碟机关闭 Wi-Fi 省电）、#255 的 Windows 部分、#213 的跨引擎修复。

部分完成或待实机验收的 Issue 不应在发布时直接关闭：`#247`、`#252`、`#268`、`#253`、`#298`（等实机确认）、`#286`（第 3、4 项没做）、`#301`（待实机验收）；`#244` 保持打开；`#304`、`#305` 若最终没有合入则保持打开（见 1A-8）。机器人的 PR #302（二代机仅密钥）被 #299 的密码方案取代，PR #310（#309 的机器人方案）只取了思路，发布时都关闭。

## Phase D final evidence (2026-11-20)

This section is the authoritative Phase D record for build commit `fad548b9484209710abad7d9ca1ea61e78edd295`. The clean rc4 output is `/var/tmp/enginex-release-5.3.0.5-rc4` on the authorized ARM build host and contains 28 release artifacts plus `RELEASE-TAG.txt`, `SHA256SUMS.txt`, and `SHA256SUMS.txt.sig` (31 files total). The manifest has 29 entries. `SHA256SUMS.txt` has SHA-256 `64a29a8313f2dd0936cdd41fd6bd8df2b0b379d4f33e7454685d9d6aed4bf92`, and `SHA256SUMS.txt.sig` has SHA-256 `df456df780f239da19e8d369025c886335eba7b232f6b3ba42497bb0952253f8`.

No tag, GitHub Release, issue comment, or push to `main` was performed. The Phase B output `/var/tmp/enginex-release-5.3.0.5` and Phase C output `/var/tmp/enginex-release-5.3.0.5-rc3` remain untouched. No real deck or production service was touched.

### SHA-256 manifest

| File | SHA-256 |
| --- | --- |
| `MIXSTREAMPRO-5.3.0.5-EngineX-OS-Numark-Mixstream-Pro-NH08-Update.img` | `9730b049da24b3a40fcaab479969be1902968d10319409a29b71a81264849c6b` |
| `MIXSTREAMPROGO-5.3.0.5-EngineX-OS-Numark-Mixstream-Pro-Go-NH10-Update.img` | `40b45f9175987fb2890e272ed2112d6624451ebbab4e741c6a1e0b281169c68f` |
| `MIXSTREAMPROPLUS-5.3.0.5-EngineX-OS-Numark-Mixstream-Pro-Plus-NH08S-Update.img` | `7d87664ce596f9f4aeca7d2a5b41e73d02ad13cfc16a40e76137fff4620bbed2` |
| `PRIME2-5.3.0.5-EngineX-OS-Denon-PRIME-2-JC16-Update.img` | `9730b049da24b3a40fcaab479969be1902968d10319409a29b71a81264849c6b` |
| `PRIME4-5.3.0.5-EngineX-OS-Denon-PRIME-4-JC11-Update.img` | `9730b049da24b3a40fcaab479969be1902968d10319409a29b71a81264849c6b` |
| `PRIME4G2-5.3.0.5-EngineX-OS-Denon-PRIME-4-G2-JP24-Update.img` | `3d9031627a24803bd96d8f9a8929828e004a52b30487e231b5a61314e46563bf` |
| `PRIME4PLUS-5.3.0.5-EngineX-OS-Denon-PRIME-4-Plus-JC11S-Update.img` | `06cc45c978a91271c0f309fb1337916cd1fa8186725ff0ab8a59838c929704d5` |
| `PRIMEGO-5.3.0.5-EngineX-OS-Denon-PRIME-GO-JP11-Update.img` | `47fd1f436fd1f5f76afa4ef07986d941e261615a20dd8e3d7b8e4451a901d7d2` |
| `PRIMEGOPLUS-5.3.0.5-EngineX-OS-Denon-PRIME-GO-Plus-JP11S-Update.img` | `3ffa6ae6185cd0e2c201b7ef26d6653c2843e803c925043ff4fd96e7af50314c` |
| `SC5000-5.3.0.5-EngineX-OS-Denon-SC5000-Prime-JP07-Update.img` | `47fd1f436fd1f5f76afa4ef07986d941e261615a20dd8e3d7b8e4451a901d7d2` |
| `SC5000M-5.3.0.5-EngineX-OS-Denon-SC5000M-Prime-JP08-Update.img` | `47fd1f436fd1f5f76afa4ef07986d941e261615a20dd8e3d7b8e4451a901d7d2` |
| `SC6000-5.3.0.5-EngineX-OS-Denon-SC6000-Prime-JP13-Update.img` | `0c9e25aeecea0c62e9ba5126e771fdbc2ee98f1db49f22e268ed741353cafedf` |
| `SC6000M-5.3.0.5-EngineX-OS-Denon-SC6000M-Prime-JP14-Update.img` | `0c9e25aeecea0c62e9ba5126e771fdbc2ee98f1db49f22e268ed741353cafedf` |
| `SCLIVE2-5.3.0.5-EngineX-OS-Denon-SC-LIVE-2-JP20-Update.img` | `bf1eff87485c296532d0748a88066f77fb689f9584cd70cf56359888ebce606b` |
| `SCLIVE4-5.3.0.5-EngineX-OS-Denon-SC-LIVE-4-JP21-Update.img` | `c2f26a9c000cc200a5fa8a3c4993f2612f3a30409844ebf9d64ad0697baf4aab` |
| `SYSTEMONE-5.3.0.5-EngineX-OS-Rane-SYSTEM-ONE-RMZ2-Update.img` | `cea1f0723bff2e89b551a38f77b46448cda118b96a874f9d5b09a3c9bc7bd645` |
| `MIXSTREAMPROGO-5.3.0.5-EngineX-OS-Data-Bundle.tar.gz` | `d0db19b1ba1bb7b7cb413582c6c91b33510898995dd0168ef1513a658c67913e` |
| `MIXSTREAMPROPLUS-5.3.0.5-EngineX-OS-Data-Bundle.tar.gz` | `00271b7d77d3284ed8985b3c9b1bc8abfb10e6bae1e7b60ea4a8045faeebd432` |
| `PRIME4G2-5.3.0.5-EngineX-OS-Data-Bundle.tar.gz` | `15ebd957d9230d3b8b1f8e218e898a596086b0fd8c10db30d819542cde6e8aa8` |
| `PRIME4PLUS-5.3.0.5-EngineX-OS-Data-Bundle.tar.gz` | `e3f2172adccca11cc776be11029e7737528c9d6ccb1308baff17f1a72bbb1442` |
| `PRIMEGOPLUS-5.3.0.5-EngineX-OS-Data-Bundle.tar.gz` | `903c2da8f242ea0fe7ce8056c1fcc1a266ae8a2ef62a92249ffb0501bc51ace7` |
| `SCLIVE2-5.3.0.5-EngineX-OS-Data-Bundle.tar.gz` | `ee497f96f7dd1ac0b91d619a967d10ed3da7385fa4dbaf9791fe9d81ffbe7eb4` |
| `SCLIVE4-5.3.0.5-EngineX-OS-Data-Bundle.tar.gz` | `ca8c0713ebb903a9d627bf1c37b198ed6bc3ee209e9f2b052640e9ebcdad0532` |
| `SYSTEMONE-5.3.0.5-EngineX-OS-Data-Bundle.tar.gz` | `c97bd21c67add84a603e4e3adac1972bcd34bccdaa615ed18e7d762288dbdd96` |
| `EngineX-Installer-5.3.0.5-linux-amd64.tar.gz` | `5670191cbe5ca3fd1974dbfd57a126eb9b3beaf1a8126e728adfe14c8423879b` |
| `EngineX-Installer-5.3.0.5-macos-amd64.zip` | `11c23b3be208da444c4e27e9a41a161bf2297a11777ef504ba182511471c5b0d` |
| `EngineX-Installer-5.3.0.5-macos-arm64.zip` | `eb52fa07d3a5cfa06e8a8a2f2d5e836e1e8106f32252d99412a07d70566e7925` |
| `EngineX-Installer-5.3.0.5-windows-amd64.zip` | `e2ceb59d535c863adf7a080658ffa7df900466074f94c1bfa8731a7b728cc8ca` |

Manifest-only `RELEASE-TAG.txt`: `369bd4c0f3141cf1d1d0bee66890a06ffad4d8636971f5d303c240395948352e`.

### Image checks

- SC5000 JP07, SC6000 JP13, and PRIME 4 JC11 unpack checks returned `verified: true`; each embedded version is `5.3.0.5` and the release tag is `v5.3.0.5`.
- Gen-1 group hashes matched across every member: SC5000 `47fd1f436fd1f5f76afa4ef07986d941e261615a20dd8e3d7b8e4451a901d7d2`, SC6000 `0c9e25aeecea0c62e9ba5126e771fdbc2ee98f1db49f22e268ed741353cafedf`, and PRIME 4 `9730b049da24b3a40fcaab479969be1902968d10319409a29b71a81264849c6b`.
- The decoded Gen-1 splash images and all checked Gen-2 `img/SplashScreen.png` files are 1280×800 RGB and visibly show `5.3.0.5`.
- The published `v5.3.0.4-r5` JP07, JP13, and JC11 gate inputs were downloaded and verified against that release manifest before the gates ran.

### Tests and release gates

Phase D step 1 passed the required release-workflow checks at `fad548b9484209710abad7d9ca1ea61e78edd295`. `gofmt -l .` produced no output, `go vet ./...` passed, the C tests passed for `test_stems_feature.c` against the 5.0.4, 5.3.0, and 5.3.0-sclive4 Engines, `test_az04_hook.c`, native and armhf `test_rt_budget.c`, native and armhf `test_rt_callback.c` including both `selftest` runs, `test_dsp_smooth.c` with ASan/UBSan and TSan, and `test_edisksd_hook.c`. The `tools/az01-packer` Python suite passed all 572 tests with one expected skip, and both bootstrap sandboxes passed for JC11S and AZ04. Go code was unchanged since the Phase C verification, so the Go race suite was not repeated.

| Group | Gate directory | Exit | Results | Engine patches | DSP guards |
| --- | --- | ---: | --- | ---: | ---: |
| JP07 / SC5000 | `/var/tmp/enginex-gate-5305-rc4-jp07` | 0 | 176 PASS, 0 FAIL | 20 | 21 |
| JP13 / SC6000 | `/var/tmp/enginex-gate-5305-rc4-jp13` | 0 | 176 PASS, 0 FAIL | 20 | 21 |
| JC11 / PRIME 4 | `/var/tmp/enginex-gate-5305-rc4-jc11` | 0 | 176 PASS, 0 FAIL | 29 | 21 |

All three static audits reported 65 files, 0 FAIL, and 9 expected WARN. The sandbox `unsupported operation` INFO line is the expected missing-eMMC condition.

### REGRESSION-CHECKLIST walk and deck impact

The automated and sandbox checklist items are covered by the Phase D tests, image checks, and release gates. Rows 359, 360, and 361 are **pending real-deck run**. The RGB Link shadow overview fix is included in rc4 and row 361 records its real-deck check. The remaining real-deck items, including USB/OTA installation, physical controls and audio, CDJ-3000 and rekordbox Link, Bluetooth, HFS+ hotplug/eject, SMB playback, and native installer hardware paths, remain **pending real-deck run** unless covered by an existing dated hardware report.

The rc4 output directory remains on the authorized ARM build host for maintainer review. Gate, unpack, old-image, and splash scratch directories are disposable after this evidence is committed. The step-9 post-release cleanup was not run.


## Phase E final evidence (2026-11-20)

This section is the authoritative Phase E record for build commit `1b05b8d159eff23c9698053bef780810fcb26a57`. The clean rc5 output is `/var/tmp/enginex-release-5.3.0.5-rc5` on the authorized ARM build host. It contains 28 release artifacts plus `RELEASE-TAG.txt`, `SHA256SUMS.txt`, and `SHA256SUMS.txt.sig`, for 31 files in total. Publication was intentionally not performed. No real deck or production service was touched.

- **Rollback point**: tag `v5.3.0.4-r5` (`9e827205ae331d2d896d97207032556dc4202af5`)
- **Manifest**: `SHA256SUMS.txt` has SHA-256 `ef8a6547ce9b00bbce85d18802c2e2255ad14cb16bca5e860ec4c893de2ed09b`
- **Signature**: `SHA256SUMS.txt.sig` has SHA-256 `4f818c6b3a3850c8171b44d4d24d46e92231eab72175a723e1e44df02e55848a` and verified against the manifest
- **Publication status**: no tag, GitHub Release, issue comment, or push to `main` was performed

### Tests and build

- The clean checkout was fixed at `1b05b8d159eff23c9698053bef780810fcb26a57`. `gofmt -l .` produced no output and `go vet ./...` passed.
- The ARM `go test -race ./...` run completed every package except `pkg/bridge`, where the known slow `TestExportSelectionMTimeAndFallback` timed out after 600 seconds inside SQLCipher PBKDF2. The same test and `TestDefaultMountCompressionThroughputDelta` passed once under x86 `-race` on IN in 102.477 seconds.
- `tests/test_rootfs_overlay.sh` passed with `bash`. The JC11S and AZ04 bootstrap sandboxes passed. `tools/az01-packer` passed 574 tests with one expected skip. `tools/stems` passed 18 tests. No az01-packer or stems test failed, so there was no such failure to compare against `28912744`.
- The C tests passed after correcting an initial test-wrapper argument error in the callback harness. `test_stems_feature.c` passed for the 5.0.4, 5.3.0, and 5.3.0-sclive4 Engines on armhf. `test_az04_hook.c`, native and armhf `test_rt_budget.c`, native and armhf `test_rt_callback.c` including both `selftest` runs, `test_dsp_smooth.c` native plus ASan/UBSan and TSan, and native and armhf `test_edisksd_hook.c` including both `--mounts` runs all passed.
- `build-release.sh`, `build-installer.sh`, and `sign-release.sh` ran from scratch in that order. The final signed manifest contains 29 entries and the output contains 31 files.

### Image checks

- The SC5000 JP07, SC6000 JP13, and PRIME 4 JC11 representative images unpacked with `verified: true`. Their embedded version and release tag are `5.3.0.5` and `v5.3.0.5`.
- Gen-1 group SHA-256 values matched across each group. SC5000 is `0ac5f0ab702d4fe2fa8d0361939871e60a87bee2641303065f2c44324011aea7`, SC6000 is `ce4a193654e1508dff51c4a6b4c8b9bff7aefe515ce068c3063043949df982f3`, and PRIME 4 is `5aa7cbe7d297439b686c6aa82aded63cff2ad674699b924f6f35ca61519dd6f9`.
- All three decoded Gen-1 splash images and all eight extracted Gen-2 `img/SplashScreen.png` files are `1280x800 RGB`. The Gen-2 splash SHA-256 is `e1719e47b0abca9c5cf98722649ce2dab827a10861e0d680b738a4a3750b50f0`.

### Release gates

Each gate used the published `v5.3.0.4-r5` representative image after its SHA-256 was checked against that release manifest.

| Group | Gate directory | Exit | Results | Static audit | Engine patches | DSP guards |
| --- | --- | ---: | --- | --- | ---: | ---: |
| JP07 / SC5000 | `/var/tmp/enginex-gate-5305-rc5-jp07` | 0 | 176 PASS, 0 FAIL | 65 files, 0 FAIL, 9 WARN | 20 | 21 |
| JP13 / SC6000 | `/var/tmp/enginex-gate-5305-rc5-jp13` | 0 | 176 PASS, 0 FAIL | 65 files, 0 FAIL, 9 WARN | 20 | 21 |
| JC11 / PRIME 4 | `/var/tmp/enginex-gate-5305-rc5-jc11` | 0 | 176 PASS, 0 FAIL | 65 files, 0 FAIL, 9 WARN | 29 | 21 |

The expected sandbox `unsupported operation` line reports the absent eMMC fixture and did not fail any gate.

### SHA-256 manifest

The table below reproduces all 28 release artifact hashes from the signed manifest. `RELEASE-TAG.txt` is included as the manifest-only entry.

| File | SHA-256 |
| --- | --- |
| `MIXSTREAMPRO-5.3.0.5-EngineX-OS-Numark-Mixstream-Pro-NH08-Update.img` | `5aa7cbe7d297439b686c6aa82aded63cff2ad674699b924f6f35ca61519dd6f9` |
| `MIXSTREAMPROGO-5.3.0.5-EngineX-OS-Numark-Mixstream-Pro-Go-NH10-Update.img` | `93851bd63bae8634597dd6b6326b2041545014e4de6819597c4eae2f21db5f27` |
| `MIXSTREAMPROPLUS-5.3.0.5-EngineX-OS-Numark-Mixstream-Pro-Plus-NH08S-Update.img` | `3195803fbd99438f66ee71662df808d3e7e88022f71f699db240c2089d0a5486` |
| `PRIME2-5.3.0.5-EngineX-OS-Denon-PRIME-2-JC16-Update.img` | `5aa7cbe7d297439b686c6aa82aded63cff2ad674699b924f6f35ca61519dd6f9` |
| `PRIME4-5.3.0.5-EngineX-OS-Denon-PRIME-4-JC11-Update.img` | `5aa7cbe7d297439b686c6aa82aded63cff2ad674699b924f6f35ca61519dd6f9` |
| `PRIME4G2-5.3.0.5-EngineX-OS-Denon-PRIME-4-G2-JP24-Update.img` | `16bc57b63653bca44f1d49e898147d02363f72157e33ac7d6fe0f43c16722c19` |
| `PRIME4PLUS-5.3.0.5-EngineX-OS-Denon-PRIME-4-Plus-JC11S-Update.img` | `f31f00d1f1562a8bb59810849ee0b58f8f1bd5479b6f44e19fbd1428e2937829` |
| `PRIMEGO-5.3.0.5-EngineX-OS-Denon-PRIME-GO-JP11-Update.img` | `0ac5f0ab702d4fe2fa8d0361939871e60a87bee2641303065f2c44324011aea7` |
| `PRIMEGOPLUS-5.3.0.5-EngineX-OS-Denon-PRIME-GO-Plus-JP11S-Update.img` | `5625b58d57d75ef87113926b51a0455635fd4da70952cbfcb6dc9676b3fe4338` |
| `SC5000-5.3.0.5-EngineX-OS-Denon-SC5000-Prime-JP07-Update.img` | `0ac5f0ab702d4fe2fa8d0361939871e60a87bee2641303065f2c44324011aea7` |
| `SC5000M-5.3.0.5-EngineX-OS-Denon-SC5000M-Prime-JP08-Update.img` | `0ac5f0ab702d4fe2fa8d0361939871e60a87bee2641303065f2c44324011aea7` |
| `SC6000-5.3.0.5-EngineX-OS-Denon-SC6000-Prime-JP13-Update.img` | `ce4a193654e1508dff51c4a6b4c8b9bff7aefe515ce068c3063043949df982f3` |
| `SC6000M-5.3.0.5-EngineX-OS-Denon-SC6000M-Prime-JP14-Update.img` | `ce4a193654e1508dff51c4a6b4c8b9bff7aefe515ce068c3063043949df982f3` |
| `SCLIVE2-5.3.0.5-EngineX-OS-Denon-SC-LIVE-2-JP20-Update.img` | `57d4364a75780fed65288f849bddf39b6b396021d5026cc04279a935b5155ef8` |
| `SCLIVE4-5.3.0.5-EngineX-OS-Denon-SC-LIVE-4-JP21-Update.img` | `8fdeba39f6697c3901dfa4108cddbd7412e32864bda9e1bfca2443b1b1a3928e` |
| `SYSTEMONE-5.3.0.5-EngineX-OS-Rane-SYSTEM-ONE-RMZ2-Update.img` | `7c242e0a2f103b16c1c025252eab0dca0a235a785c6a9f7a751f3534b97505ba` |
| `MIXSTREAMPROGO-5.3.0.5-EngineX-OS-Data-Bundle.tar.gz` | `956dd64fd6162dcf2d2b1bcf2147f2cfa2a79324b8e32a7f056de83007a79b4f` |
| `MIXSTREAMPROPLUS-5.3.0.5-EngineX-OS-Data-Bundle.tar.gz` | `6d56dade959380f86d0337aa8f26db5d6f04c651dde6cc1c8756b6c8f5ee8497` |
| `PRIME4G2-5.3.0.5-EngineX-OS-Data-Bundle.tar.gz` | `b9425d07d4a9388309a98d5ed2eb9c15c33635eeed616cb1b09165e34d3dd559` |
| `PRIME4PLUS-5.3.0.5-EngineX-OS-Data-Bundle.tar.gz` | `dc0e444d285b060ec42adb49296b8fddcd60ee1372a2d76f2ebe3c647b33d7e2` |
| `PRIMEGOPLUS-5.3.0.5-EngineX-OS-Data-Bundle.tar.gz` | `55ad54cd82c27605cf4d41c24ebffd8ac097f952e0381894dd76f0ad0970c8ac` |
| `SCLIVE2-5.3.0.5-EngineX-OS-Data-Bundle.tar.gz` | `a61a08d6c9fc45124b375e9de192ac6df0fe5d72028511ea23b28328676abfa6` |
| `SCLIVE4-5.3.0.5-EngineX-OS-Data-Bundle.tar.gz` | `f16e752878fa8228da69faf22fa66cbb7ca173847373e83a56a44f90965b2189` |
| `SYSTEMONE-5.3.0.5-EngineX-OS-Data-Bundle.tar.gz` | `d8a555cdbf33d6517d3849ddfdb081f964b7006f115b3ba3edea2cf7dc0c8f68` |
| `EngineX-Installer-5.3.0.5-linux-amd64.tar.gz` | `bf126c4cf1d4609fa673e7bc53ead274d54610c76b6857b4da1052781d498fb9` |
| `EngineX-Installer-5.3.0.5-macos-amd64.zip` | `99c225f93f8fe26a3c4b54cfd1f5d3c4aee4c3904a79eca8227677350a2cb114` |
| `EngineX-Installer-5.3.0.5-macos-arm64.zip` | `065c2e97a21efd85f4fc6611899e068ec2eb71b2070f43d5edade525930304b6` |
| `EngineX-Installer-5.3.0.5-windows-amd64.zip` | `04d53367fb592cfe46afd04fb65ed13850ba104513b2b9f8afbd63e225db045a` |
| `RELEASE-TAG.txt` | `369bd4c0f3141cf1d1d0bee66890a06ffad4d8636971f5d303c240395948352e` |

### REGRESSION-CHECKLIST walk and deck impact

The automated and sandbox checklist items are covered by the Phase E tests, image checks, and release gates. Rows 359, 360, 361, 362, and 363 remain **pending real-deck run** for release sign-off. The remaining real-deck items, including USB and OTA installation, physical controls and audio, CDJ-3000 and rekordbox Link, Bluetooth, HFS+ hotplug and eject, SMB playback, and native installer hardware paths, also remain **pending real-deck run** unless covered by a dated hardware report.

The rc5 output directory remains on the authorized ARM build host for maintainer review. Earlier release directories remain untouched. The step-9 post-release cleanup was not run.


## Phase F final evidence (2026-11-20)

This section is the authoritative Phase F sandbox record for build commit `79d6e2415802c85c743ed80dc1499bb9b767a23b`. The clean rc6 output is `/var/tmp/enginex-release-5.3.0.5-rc6` on Oracle-Batam-arm. It contains 28 release artifacts plus `RELEASE-TAG.txt`, `SHA256SUMS.txt`, and `SHA256SUMS.txt.sig`, for 31 files in total. The maintainer's decks were offline after a power loss, so no real-deck run was possible. Publication was intentionally not performed.

- **Rollback point**: tag `v5.3.0.4-r5` (`9e827205ae331d2d896d97207032556dc4202af5`)
- **Manifest**: `SHA256SUMS.txt` SHA-256 `47625b2a19c5f072ecfbb2de37cd0ad77f4d4e7bfb9975e0c1e303a269c81668`
- **Signature**: `SHA256SUMS.txt.sig` SHA-256 `6509c57df9759faa4a0fc4b58195e6886746d8b41a89365da05b34559b9eb28d` and verified against the manifest
- **RELEASE-TAG.txt**: SHA-256 `369bd4c0f3141cf1d1d0bee66890a06ffad4d8636971f5d303c240395948352e`, content `v5.3.0.5`
- **Publication status**: no tag, GitHub Release, issue comment, or push to `main` was performed

### Branches and merge verification

The eject branch `ddd5cbc7` and the streaming-BPM branch were merged with `--no-ff`. The streaming branch received Terra's requested cache fix `c042f7d2` plus the regression test `405ec28c` and stale fixture updates `a9fd56d6`. Every added line from both side branches is present in the merge result. `origin/main` is an ancestor of the candidate.

### Tests

- The corrected `tools/az01-packer` suite on the final streaming branch passed 576 tests with one expected skip.
- The full ARM run passed both JC11S and AZ04 bootstrap sandboxes, the stems suite, the C stems feature, AZ04 hook, native and armhf RT budget, DSP plain/ASan/UBSan/TSan, and native and armhf edisksd hook tests with both `--mounts` runs. The first aggregate run recorded two tooling issues: the prebuilt edisksd hook was stale after the eject C change, and the callback wrapper had an incorrect linker-flag composition. Rebuilding the committed hook and rerunning the corrected callback harness produced a clean overlay result (`37 tests`) and native/armhf callback plus both self-tests passed.
- The final Python suite was `576 tests`, one expected skip, exit 0.
- ARM `gofmt -l .` and `go vet ./...` passed. ARM `go test -race ./...` completed every package except the known slow `pkg/bridge::TestExportSelectionMTimeAndFallback`, which timed out after 600 seconds in SQLCipher PBKDF2. The x86 targeted rerun of `TestExportSelectionMTimeAndFallback` and `TestDefaultMountCompressionThroughputDelta` passed in 99.335 seconds.
- The C hook generated from `tools/edisksd-hook/build.sh` is committed at `rootfs-overlay/etc/enginex/libedisksd-hook.so`.

### Build and image checks

- `build-release.sh`, `build-installer.sh`, and `sign-release.sh` ran from scratch in that order. The signed manifest has 29 entries and the output has 31 files.
- Representative SC5000 JP07, SC6000 JP13, and PRIME 4 JC11 images unpacked with `verified: true`; each carries version `5.3.0.5` and release tag `v5.3.0.5`.
- Gen-1 group SHA-256 values matched across each group: SC5000 `1287257d14df8995b6661cc4b945ff35fc998196160b155872e1eb744708452d`, SC6000 `1e6864b2eab413398144dfa803e0548caf874aa3de463fd3b9d8485768eb9d6d`, PRIME 4 `fb3dde6d2430fa9b0caca690e15d04b4c1e73611149084a4b9cbe41137e92761`.
- All three decoded Gen-1 splash images are `1280x800 RGB` and visibly show `5.3.0.5`. All eight extracted Gen-2 splash images are `1280x800 RGB` with common SHA-256 `e1719e47b0abca9c5cf98722649ce2dab827a10861e0d680b738a4a3750b50f0`.

### Release gates

Each gate used the published `v5.3.0.4-r5` representative image after checking it against that release's manifest.

| Group | Gate directory | Exit | Results | Static audit | Engine patches | DSP guards |
| --- | --- | ---: | --- | --- | ---: | ---: |
| JP07 / SC5000 | `/var/tmp/enginex-gate-5305-rc6-jp07` | 0 | 176 PASS, 0 FAIL | 65 files, 0 FAIL, 9 WARN | 20 | 21 |
| JP13 / SC6000 | `/var/tmp/enginex-gate-5305-rc6-jp13` | 0 | 176 PASS, 0 FAIL | 65 files, 0 FAIL, 9 WARN | 20 | 21 |
| JC11 / PRIME 4 | `/var/tmp/enginex-gate-5305-rc6-jc11` | 0 | 176 PASS, 0 FAIL | 65 files, 0 FAIL, 9 WARN | 29 | 21 |

The expected sandbox `unsupported operation` line is the absent-eMMC condition and did not fail a gate.

### SHA-256 manifest

| File | SHA-256 |
| --- | --- |
| `MIXSTREAMPRO-5.3.0.5-EngineX-OS-Numark-Mixstream-Pro-NH08-Update.img` | `fb3dde6d2430fa9b0caca690e15d04b4c1e73611149084a4b9cbe41137e92761` |
| `MIXSTREAMPROGO-5.3.0.5-EngineX-OS-Numark-Mixstream-Pro-Go-NH10-Update.img` | `180399f955e732a0d65ed009db27b5da9b100b401647977cbfdc8650b9ea9342` |
| `MIXSTREAMPROPLUS-5.3.0.5-EngineX-OS-Numark-Mixstream-Pro-Plus-NH08S-Update.img` | `27992b1ff6aadcffbbc6e8098be2ada291ad43bcf492af52a41d45f1bf1c6e6d` |
| `PRIME2-5.3.0.5-EngineX-OS-Denon-PRIME-2-JC16-Update.img` | `fb3dde6d2430fa9b0caca690e15d04b4c1e73611149084a4b9cbe41137e92761` |
| `PRIME4-5.3.0.5-EngineX-OS-Denon-PRIME-4-JC11-Update.img` | `fb3dde6d2430fa9b0caca690e15d04b4c1e73611149084a4b9cbe41137e92761` |
| `PRIME4G2-5.3.0.5-EngineX-OS-Denon-PRIME-4-G2-JP24-Update.img` | `704c8f4a0fbf8e4815018c12380cdc4204c82d488863579aa3fca40d32f87e82` |
| `PRIME4PLUS-5.3.0.5-EngineX-OS-Denon-PRIME-4-Plus-JC11S-Update.img` | `ec91e1428b9f310893b212ab53dccb935659f959f30080e1e7ba120387f98183` |
| `PRIMEGO-5.3.0.5-EngineX-OS-Denon-PRIME-GO-JP11-Update.img` | `1287257d14df8995b6661cc4b945ff35fc998196160b155872e1eb744708452d` |
| `PRIMEGOPLUS-5.3.0.5-EngineX-OS-Denon-PRIME-GO-Plus-JP11S-Update.img` | `364c7082bc243bc0d12d75c8b79b3bbe6a7bb8b6eda0f6bb3071e330480766db` |
| `SC5000-5.3.0.5-EngineX-OS-Denon-SC5000-Prime-JP07-Update.img` | `1287257d14df8995b6661cc4b945ff35fc998196160b155872e1eb744708452d` |
| `SC5000M-5.3.0.5-EngineX-OS-Denon-SC5000M-Prime-JP08-Update.img` | `1287257d14df8995b6661cc4b945ff35fc998196160b155872e1eb744708452d` |
| `SC6000-5.3.0.5-EngineX-OS-Denon-SC6000-Prime-JP13-Update.img` | `1e6864b2eab413398144dfa803e0548caf874aa3de463fd3b9d8485768eb9d6d` |
| `SC6000M-5.3.0.5-EngineX-OS-Denon-SC6000M-Prime-JP14-Update.img` | `1e6864b2eab413398144dfa803e0548caf874aa3de463fd3b9d8485768eb9d6d` |
| `SCLIVE2-5.3.0.5-EngineX-OS-Denon-SC-LIVE-2-JP20-Update.img` | `bced53c96bba18ac4ddf06a56bfd09e119670cc4346ad117da13e44d3eb38ddd` |
| `SCLIVE4-5.3.0.5-EngineX-OS-Denon-SC-LIVE-4-JP21-Update.img` | `207dd572677c22dc3b96ce3bd1f34bfa9c14d97ec04d254f4a9dba256a073fcd` |
| `SYSTEMONE-5.3.0.5-EngineX-OS-Rane-SYSTEM-ONE-RMZ2-Update.img` | `78b0cd3b706d25f9b4463c307f6c748edd5aa0b2c512dfd228016b07c76c6280` |
| `MIXSTREAMPROGO-5.3.0.5-EngineX-OS-Data-Bundle.tar.gz` | `918bf4913fab122fa38d760d2d7fd20525e28420b74c723f092c4fc7048e8404` |
| `MIXSTREAMPROPLUS-5.3.0.5-EngineX-OS-Data-Bundle.tar.gz` | `95dabce4a99d4db34548eeba5a6e044bbf8b84608d196b099bd5c167f0cd1d5d` |
| `PRIME4G2-5.3.0.5-EngineX-OS-Data-Bundle.tar.gz` | `7bd0af92d008998044c5fe43fa615d7a675619bdf476a1d89a229fef18b0aaf4` |
| `PRIME4PLUS-5.3.0.5-EngineX-OS-Data-Bundle.tar.gz` | `7bd01f22e4f32bbeabbf2789c8ca3ad9274cda0aca584861f9d49c518df4af32` |
| `PRIMEGOPLUS-5.3.0.5-EngineX-OS-Data-Bundle.tar.gz` | `1fe29859be694981df9eccca3e88599f37eac1d2c411d5d265e1cb4262325eca` |
| `SCLIVE2-5.3.0.5-EngineX-OS-Data-Bundle.tar.gz` | `6b1685a35b64b5c478ee0d87189b71adf544dd7e07388ee6bed2005d0d517095` |
| `SCLIVE4-5.3.0.5-EngineX-OS-Data-Bundle.tar.gz` | `5f799aa012a8140ed95403c66dc15bb913a9f79a5dbba996abc117fc67f2e7bb` |
| `SYSTEMONE-5.3.0.5-EngineX-OS-Data-Bundle.tar.gz` | `01a37d9700c77cae3a89f40bb2a7dd1efcecc3b0c043f4cfbb8997866c119768` |
| `EngineX-Installer-5.3.0.5-linux-amd64.tar.gz` | `fc593b63af32ebba67e8c3370616a0dc1e663f6a68efc86a8b4f2eb8933fc451` |
| `EngineX-Installer-5.3.0.5-macos-amd64.zip` | `d358790922449645aeda525ca59f218fd476c233e16d7b595136b70e422368c4` |
| `EngineX-Installer-5.3.0.5-macos-arm64.zip` | `ab5f4ab8676f5f507a1dc627d26c132ad0782215f955fa8df9dabc4e817b44be` |
| `EngineX-Installer-5.3.0.5-windows-amd64.zip` | `0cd69e84839d7d51ca3014bb7de5d7b158e9474b6c7d9356b9eae4b5e4dcda69` |
| `RELEASE-TAG.txt` | `369bd4c0f3141cf1d1d0bee66890a06ffad4d8636971f5d303c240395948352e` |

### REGRESSION-CHECKLIST walk and real-deck scope

Rows 359–368 are covered here only by unit, QML, C or release-gate sandbox evidence and remain **pending real-deck run**. Row 1 manual cross-unit MASTER handoff also remains pending because it requires two physical decks. The same applies to physical eject and HFS+ behavior, streaming LOAD/audio playback, streamed BPM/grid on real tracks, shadow service icons, CDJ-3000 and rekordbox Link, physical controls, Bluetooth, native installer hardware paths, and every other checklist item marked for real hardware.

Phase F completed the documentation checks required by release-workflow step 1. No `tone: "new"` feature entries remain in `docs/site/content.js`; static cache parameters were bumped to `?v=67`/`?v=37`, and the three READMEs have matching 16 image links and 4 installer links. Historical `5.3.0.4` references remain only where they describe the previous baseline or historical documentation.

The rc6 output and gate evidence remain on Oracle-Batam-arm. Earlier release directories remain untouched. Step-9 cleanup was not run.
