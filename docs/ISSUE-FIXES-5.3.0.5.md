# EngineX OS 5.3.0.5 发版与 Issue 修复记录（草稿，发布日期 【待填】）

> **草稿说明**：标有 `【待填】` 的位置在构建、门禁与发布后由发版人填入，其余内容来自 `agent/5305-integration` 的合并记录与 `docs/REGRESSION-CHECKLIST.md`。构建提交、全部 SHA-256 和门禁结果在构建之前都不存在，请勿提前当作事实引用。

- **日期**：撰写 `2026-11-14`；发布 `【待填】`
- **范围**：`v5.3.0.4-r5`（`9e827205`）→ `v5.3.0.5`（版本号 `5.3.0.5`，tag `v5.3.0.5`），内容来自集成分支 `agent/5305-integration`。
- **构建提交**：`【待填】`
- **回滚点**：tag `v5.3.0.4-r5`（`9e827205`）；一代机与二代机刷回该 Release 的 `*-Update.img`。更早的回滚点见 `ISSUE-FIXES-5.3.0.4.md`。
- **覆盖机型与产物（28 个发布资产 + `SHA256SUMS.txt` + `SHA256SUMS.txt.sig`）**：
  - 8 款一代机 `*-Update.img`、8 款二代机 `*-Update.img`、4 个平台的 `EngineX-Installer-5.3.0.5-*`；
  - 8 个 `*-Data-Bundle.tar.gz` **最后一次**随 Release 提供，仅供 `5.3.0.2` / `5.3.0.3` 二代机的屏幕 OTA 过渡（#215）；`5.3.0.6` 起退役。
- **签名**：从 5.3.0.5 起每个 Release 附 `SHA256SUMS.txt.sig`（Ed25519，私钥只在编译机上）；5.3.0.4 及更早的碟机不验签，需先更新到 5.3.0.5。
- **在线文档**：三份 README 与 `docs/site/content.js`（三语）已更新到 5.3.0.5，实测截图与开机画面预览 PNG 由发版构建负责，本次未改。

---

## 1. Issue 与 PR 修复总表（v5.3.0.4-r5 → v5.3.0.5）

| Issue / PR | 模块 | 原因与修法摘要 | 验证与回归测试 |
| --- | --- | --- | --- |
| **#255 / #239**（原生安装器与签名更新） | `cmd/enginex-installer/`<br>`tools/az01-packer/`<br>`pkg/webui/update*.go` | **原因**：安装器依赖主机上的 Python、fastboot、e2fsprogs、libusb 或 WSL；Release 资产可以被替换，而碟机更新只信任发布页给出的文件。<br>**修法**：安装器用 Go 直接准备镜像并经 USB 传输（Windows 走系统 WinUSB）；每个 Release 的 `SHA256SUMS.txt` 带 Ed25519 签名 `SHA256SUMS.txt.sig`，屏幕更新和安装器先验签再核对 SHA-256，无签名、签名无效、被改动或比碟机版本旧的发布一律拒绝；5.3.0.4 及更早的碟机不做该检查。安装器旁有同版本机型镜像时优先使用并要求同发布的 `SHA256SUMS.txt`、`.sig`、`RELEASE-TAG.txt`。 | 检验：`cmd/enginex-installer` 与 `pkg/webui` 的 Go 测试、`update_sign_test.go`；真机 WinUSB 绑定、USB 传输和刷入后激活需要实机（检查表行 215、216、218）。 |
| **恢复官方固件**（含原厂卡 Logo 的 QML 缓存故障） | `cmd/enginex-installer/restore_stock.go`<br>`tools/ui-mod/`（QML 缓存隔离）<br>`tools/az01-packer/build-installer.sh` | **原因**：EngineX 与原厂共用 QML 磁盘缓存，原厂刷机保留 `/data`，损坏的缓存让原厂启动崩溃（卡 Logo）。<br>**修法**：EngineX 的 QML 缓存放到 `ui/qml-cache`，启动时清理旧共享缓存；`Restore-Official-Firmware.*`（`--restore-stock`）先清共享缓存，再校验官方 5.3.0 校验和与 AZ01 板号后刷入；二代机走 LAN 卸载。回到原厂前请先启动一次 5.3.0.5。 | 检验：`restore_stock` Go 测试、ARM 固件沙箱；真机官方刷入与返回 EngineX 需维护者进入更新模式（行 201、217）。 |
| **蓝牙音箱输出与质量 / 延迟调优** | `tools/ui-mod/engine_ui_hook.c`<br>`tools/ui-mod/build_packs.py`<br>`pkg/webui/` | **原因**：Engine 自带的 A2DP 输出源原先拿到的是静音，面板也无法选择输出，另有环形缓冲回绕问题。<br>**修法**：钩子把 `bt_out=` 所选的输出对以 A2DP 流自身的采样率发给音箱，仅在有音箱收听时发送；原厂蓝牙面板 `输入分配到` 下新增 `输出来源`；网页控制台同步设置。蓝牙约增加 150–250 ms，不用于打拍对齐。 | 检验：UI 包与 C 测试；维护者 2026-11-14 在真实 SC5000 上试听并认可音质；其他机型的 `输出来源` 选项与已连接手机时的面板提示仍待确认（行 187、188）。 |
| **#205**（碟机之间复制设置、USB 配置导入） | `pkg/webui/`<br>`tools/ui-mod/build_packs.py`<br>`tools/ui-mod/i18n.tsv` | **原因**：没有安全的跨碟机复制设置方式；r5 把原厂「加载保存的配置」提示自动答成 No，U 盘配置从未导入。<br>**修法**：来源碟机允许、接收碟机确认后只复制白名单偏好，Player 编号、角色、颜色、Name/Product、网络身份与账号保留本机；撤销按字节恢复；`Profile → Save to my drive` 同时导出 EngineX 偏好；恢复原厂 Yes / No 提示；机身菜单重启改用原厂正常退出以保存 Player 编号。 | 检验：UI 包与 Go 测试；两台真机的 Link / USB / 撤销验收待做（行 200、202、210）。 |
| **#213 / PR #214**（分侧反转转盘） | `pkg/webui/ui_settings.go`<br>`tools/ui-mod/build_packs.py` | **需求**：转盘方向要能按碟机分别设置。<br>**修法**：`设置 → 设备` 与 `:8080` 增加 `反转转盘方向`（双转盘机型左右各一个），即时作用于顶部搓碟、外圈微调、网格 / Loop 微调与 `Shift + 转盘` 跳曲；电动转盘机型隐藏。 | 检验：`test_ui_mod.py`、原生 chroot QML 校验；实机物理转动待做（行 205）。 |
| **#258**（所有一体机 `推子回音`） | `tools/ui-mod/engine_ui_hook.c`<br>`tools/ui-mod/build_packs.py` | **原因**：旧的主总线 PCM 回音与原厂逐通道 Transition FX 叠加，造成堆叠回音与卡顿。<br>**修法**：全部 5.3.0 一体机旁路旧回音，使用原厂逐通道 Transition FX；独立播放器保持旁路。 | 检验：C 与 Python 套件；真机一体机音频验收待做（行 190）。 |
| **#263**（电动转盘 4 轨布局） | `tools/ui-mod/build_packs.py` | **原因**：原厂电动模块只对 `Repeater { model: [1, 2] }` 的碟位运行马达并设置 `ExternalPlatterLayerActive`，`4 Decks` / `Paired Player` 下转盘不动、微调无效。<br>**修法**：为本机持有的碟位启用马达与触摸微调。 | 检验：Go / Python 套件；`SC5000M` / `SC6000M` 实机待验（行 186）。 |
| **#261 / PR #266**（Mixstream 打击垫栏） | `tools/ui-mod/build_packs.py` | **原因**：Sweep FX 与 Stems EQ 在 Beat Jump 组打开时互相重叠。<br>**修法**：重排 `Pads.qml`，两者按 Beat Jump 组状态动态切换列数，原生打击垫与跳拍同时可用。 | 检验：UI 包套件（行 204）。 |
| **#238**（碟机本机 Stems 分离） | `internal/stems/separate.go` | **原因**：旧方案只在 250 Hz 与 4 kHz 处切频段，在 MUSDB18 片段上每个分轨都低于 0 dB。<br>**修法**：`LocalRender` 改用 STFT 软掩码分离器，人声 / 贝斯 / 鼓 / 旋律达到固定的信号失真比下限；不是神经网络，仍有串音；人声保持原有的立体声位置；只提供 `hpss` 的电脑 / 中继不再使用（碟机自己的分离得分更高），Stems 状态行写明所用来源；Stems 上传停止进展时释放名额，慢但仍在工作的渲染端保留任务（#243）。实测 60 秒音乐在空闲 SC5000 上约 16.5–17.0 s（人声立体声化前），构建机 armhf 核心上多约 6%。 | 检验：`internal/stems` Go 套件；碟机播放中的耗时未测（行 222）。 |
| **#268**（演出曲目表导出） | `pkg/webui/` | **原因**：无法把已记录的演出导出为曲目表。<br>**修法**：认证后的文本 / CSV 下载，在 Engine 持有曲库时只读原厂 `hm.db` 历史，保留演出边界、播放顺序、重复曲目，处理已提交 WAL 与未知时钟，绝不创建或修改历史库。Remote Library 记录与 Prepare 仍待查。 | 检验：`pkg/webui` 的 `TestTracklist268Export`；真机 SC LIVE 4 对照原厂 History 待做（行 206）。 |
| **3-Band / 概览**（rekordbox 配色、概览高度、跨读取补全） | `tools/ui-mod/build_packs.py`<br>`pkg/prolink/`<br>`pkg/bridge/`<br>`pkg/rekordbox/` | **原因**：`3-Band` 与 rekordbox / CDJ-3000 配色不同；rb-bridge 在 `overviewWaveFormData` 末尾写零，Engine 按未缩放字节只画约三分之一高度，QML 再放大；PWV7 三频段被按错误顺序（中 / 高 / 低）旋转；已播放过的曲目与导出读取超过挂载时限的卡留有零尾。<br>**修法**：配色改为低频蓝、中频橙、高频白，静音中频褪为棕；overview 写入三个频段最大值；PWV7 按低 / 中 / 高读取；先补全概览再读导出，补全可跨挂载续做。 | 检验：`ThreeBandPaletteTest`、eglfs 沙箱；实机对照 CDJ-3000 / rekordbox 与旧导入 U 盘重插待做（行 194–196、203、219）。 |
| **Live Stream 只输出 WAV、按需采集** | `pkg/broadcast/`<br>`pkg/webui/`<br>`tools/ui-mod/` | **原因**：控制台提供 MP3 / OGG，但镜像没有这两种编码器，默认 `mp3` 永远连不上；没有收听者时仍持续采集并重采样，空闲碟机约占 7.3% 单核。<br>**修法**：固定 WAV（16 位、48 kHz、立体声）；旧设置自动转 WAV，Shoutcast 目标不再推送，`/live.mp3` 重定向到 `/live.wav`；没有 `/live.wav` 听众、Icecast 服务器与 LAN 流时不采集。 | 检验：`go test -race ./pkg/broadcast/... ./pkg/webui/...`；真实 Icecast 服务器播放待做（行 197、223）。 |
| **有线 + Wi-Fi 同网段** | `rootfs-overlay/`（路由守护与 sysctl）<br>`pkg/webui/` | **原因**：两台 SC5000 同时走有线与 Wi-Fi 时，Link 曲目走了 Wi-Fi（13–19 MB 曲目加载 7.4–11.3 s）；网线地址晚于 Wi-Fi 到达时，Engine 报 `online check failed` 并断开 Wi-Fi；两张网卡互相应答对方地址。<br>**修法**：同网段时 Link 路由走 `eth0`；路由守护让 Wi-Fi 保持；每个接口 `arp_announce=2`、`arp_ignore=1` 从启动起生效。 | 检验：overlay 套件；两台真机多次开机验证待做（行 192、193、213）。 |
| **#223**（Remote Library 列表封面） | `tools/ui-mod/build_packs.py` | **原因**：`BrowseView` 的 `AlbumArt.qml` 把带来源标签的行都发给网关 `/v1/sc5000/art`，而它只认自己列表里的封面，返回 404，列表出现灰块。<br>**修法**：只有 Engine 自己没有封面（`""` 或 `image://streaming/`）时才询问网关，Engine 已有的封面不再被丢弃。 | 检验：Step 2 套件；SC LIVE 4 真机验收待做（行 185）。 |
| **#264 / #272 / #252 / #216 / #246**（大 U 盘与 rekordbox 导入） | `pkg/compat/`<br>`pkg/bridge/`<br>`pkg/enginedb/`<br>`internal/cli/` | **原因**：大型导入重复 `Lstat` 数万次、桥接状态在 FAT32 上可超 4 GiB、挂载时限被占满；Pro DJ Link 暂存库可把存储卡占满（58.6 GiB 卡被占 50 GiB）；一代机的 pack 把原厂的网络盘判断改成恒为真，对方播放器的盘出现 Stems 入口（Engine 无法写入 Link 来源，报「空间不足」）；旧导入补全时重写曲目 / 歌单 ID、丢失碟机上的编辑；旧 MP3 网格偏移。<br>**修法**：解析器缓存路径查询并用 `DirEntry.Type()`；`BridgeAnalysis` 行数按 3 GiB 封顶；暂存库按盘限量与清理；Stems 滑动入口与 Stems 列表只在本机自己的盘上出现（与原厂一致）；按已保存来源记录匹配旧行并保留已播放标记、Cue、Loop 与歌单 ID；MP3 网格迁移与共享挂载时限；导入不再覆盖 Engine 同时对曲库做的改动。 | 检验：Go 套件（`-race`）、覆盖层与打包套件；真机 SD 重插与数据恢复为独立验收（行 189、191、207、211、212、214）。 |
| **#269 / #277 / #279 / #247 / #248**（流媒体） | `pkg/providers/`<br>`pkg/webui/`<br>`tools/ui-mod/` | **原因**：Spotify 载入会弹出载体、TIDAL 登录载体被挤占；收藏夹里出现 TIDAL 专属空白行且无法进入 My Playlists；原生 Ogg 失去 Engine 认识的下载名；音源设置存在先读后写的丢失更新；歌单第二页失败时整批丢弃。<br>**修法**：保留 Spotify 载入与 TIDAL 载体、识别两种 `TIDAL Rising` 拼写并走原厂 `push()`、用载体补丁的 `mp3` 命名标记而 MIME 仍为 `audio/ogg`、两个写入者改用加锁的 `BPMStore.UpdateConfig`、保留已成功获取的有序前缀；后续页面失败时网关显示已加载的行加一条未完成提示并可重试，保留 401/403 与重新登录提示（#248，行 239）。 | 检验：`pkg/providers`、`pkg/webui` 套件（`TestBrowsePartial248KeepsPrefixAndErrorUntilRetry` 等）；NH08 真机浏览 Spotify 与真机中断后续页待做（行 225、226、239）；#247 的 SoundCloud 保存仍待其所有者完成。 |
| **HFS+ U 盘**（弹出、`*-Update.img` 更新） | `pkg/webui/`<br>`rootfs-overlay/`（updater 路径） | **原因**：Engine 持有 HFS+ 曲库时弹出被拒 26 次且不说明原因；原厂 `az01-update` 给更新环境传了空的 `upgrade-file=`（或去掉了文件夹的文件名），因为 HFS+ 视图是 `/media/<卷>` 上的 tmpfs 绑定，原厂向上找根目录找不到 U 盘自己的根。<br>**修法**：保留真实弹出错误并解除缓存曲库；原厂读取 `/tmp/update-file-path` 之前由 `engine-ui-guard update-file` 把 U 盘上的所选文件映射成 U 盘上的路径（符号链接与越出 U 盘的路径保持原样）；正常模式与安全模式（回退到原厂 Engine）都走同一步骤。 | 检验：Go / overlay 套件；真机 HFS+ U 盘更新需实机（行 209、224）。 |
| **影子碟搓碟** | `pkg/webui/shadow*.go`<br>`tools/ui-mod/build_packs.py` | **原因**：用力或极快（超过 14.4 倍）搓碟时，影子副本按对端约 40 ms 前的位置汇报来导向，出现反向与更宽的位移、冻结与跳转。<br>**修法**：沿源碟自己的路径连续跟随，不超出、不反向。 | 检验：Step 2 套件；双 SC5000 真机搓碟待做（行 198、199）。 |
| **空闲碟机负载** | `rootfs-overlay/`（boot-watch、thread sweep） | **原因**：`engine-ui-guard watch` 每秒 fork `cut`、`cat`、`rm`（约 0.50% 单核），30 秒线程巡检读取约 11 万次系统调用（空闲约 1.16% 单核）。<br>**修法**：改用 shell 内建 `read`，已见过且同名的线程跳过。 | 检验：沙箱（nspawn）；真机 `sc5000-perf-tune` 应低于 0.1% 单核（行 220、221）。 |
| **#242**（可选 HTTPS 控制台） | `pkg/webui/`<br>`docs/site/content.js` | **需求**：控制台只有 HTTP。<br>**修法**：网页控制台「碟机显示与音频」里的 `HTTPS console (:8443)` 打开后，首次生成自签名证书（保存于 `/data/enginex/`，权限 `0600`，SAN 覆盖碟机 IP 与主机名，重启复用），在 `:8443` 同步提供 HTTPS；`:8080` HTTP 保持默认且不变，二维码与屏幕链接仅在开启时给出 `https://`。 | 检验：`pkg/webui` 证书与二维码测试（两种主机名）；r5 迁移保留 `console_https` 与证书字节和权限（行 236）。 |
| **#281**（Remote Load 局域网 API） | `pkg/webui/`<br>`tools/ui-mod/build_packs.py`<br>`docs/remote-load-api.md` | **需求**：局域网内的电脑或脚本无法把歌加载到碟机。<br>**修法**：`设置 → 服务 → 远程加载（局域网 API）` 默认关、只能在碟机上打开；开启后用控制台访问码调用 `/api/remote-load`（规范路径 `/v1/enginex/remote-load`），把 `/media` 下硬盘上的歌加载到空闲碟位。正在播放、联机远端碟位、未挂载或内置盘的文件、经 Cloudflare 隧道的请求都被拒绝；按住 CUE 预听、搓碟和搜索（`PlayState` 不变）按播放位置 1.5 秒内是否移动或转盘是否被触碰判为忙；加载前一刻再检查一次，并要求 Engine 的 `Lock Playing Deck` 为开。 | 检验：`pkg/webui` 与 `RemoteLoadBehaviourTest`；SC LIVE 4 真机验收待做（行 237、238）。 |
| **#283**（设置开关行） | `tools/ui-mod/build_packs.py` | **原因**：EngineX 的二值设置行没有跟随原厂的 `checkedExternally`，点按之后不再反映网页控制台所做的改动。<br>**修法**：13 个开关行全部改用 Engine 原厂开关行，保留各自的状态属性、`key=0/1` 写入与显隐条件。 | 检验：`OnOffSwitchCompatibilityTest`、`UiModSourceTest`（5.0.4 / 5.3.0 两套 QRC、三语）；碟机上逐项点按与网页改动待做（行 227）。 |
| **#234**（`sc5000` → `enginex` 更名） | `rootfs-overlay/`<br>`pkg/webui/`<br>`tools/az01-packer/`<br>`docs/mods/src/` | **原因**：内部名称长期使用 `sc5000`。<br>**修法**：服务单元、程序、`/etc/enginex`、`/run/enginex*`、日志标签与 `/v1/enginex` 改用新名称；`/etc/sc5000`（镜像链接）、`/data/sc5000-custom`（数据链接）、`/v1/sc5000`（API 别名）继续可用；ALSA 文件 `99-sc5000-custom.conf`、符号 `sc5000_alsa_boot_hook` 与钩子路径保持不变。5.3.0.2…r5 升级后旧的 `/run` 单元与运行时文件被清除；降级走本版本的 `leave` 步骤，整包原子换回，不在原地启动旧版。TIDAL 合成账号仍发放冻结的 `sc5000-token` / `sc5000-refresh`。 | 检验：`tools/az01-packer` 更名与门禁测试（含 5.3.0.2 基线）、`pkg/webui` TIDAL 与 Live Stream 迁移测试、Minimal Club Mode 暂停迁移测试；真机物理降级仍需验收（行 231–236）。 |
| **#215**（屏幕 OTA 只提供 `*-Update.img`） | `pkg/webui/update*.go`<br>`tools/az01-packer/release-gate.sh` | **原因**：5.3.0.5 之后旧版 `*-Data-Bundle.tar.gz` 只应服务仍在旧版本的碟机。<br>**修法**：5.3.0.5 的碟机更新器只发现 `*-Update.img`（包括未解锁的二代机），Release 里仍保留 8 个 Data-Bundle 供旧更新器使用；安装器保留对本地 / 显式旧数据包的兼容；门禁文档覆盖 r5 旧路线与更新的签名更新器。`#244`（AZ04 分区大小）**未**随本次合入。 | 检验：`TestUpdateNeverOffersLegacyBundles`、`TestUpdatePicksGen2ImgWhenUnlocked`、重发与签名 OTA 套件；真机重启与进度待做（行 242）。 |
| **#273 / #253**（压缩分析数据与重复插盘识别） | `pkg/enginedb/`<br>`pkg/bridge/` | **原因**：21,352 首曲目的 FAT32 U 盘上，未压缩的 `BridgeAnalysis` 行平均约 337 KB、共约 7.2 GB；重复插盘为匹配已有曲目而重读每个已存的波形。<br>**修法**：`sourceJSON` / `audioJSON` 用复用的 `gzip.BestSpeed` 写入压缩，`enginedb.UnpackAnalysis` 同时读压缩行与旧版明文行并拒绝超过限制的行；唯一来源 ID 加匹配路径时只用身份元数据，旧共享键、移动 / 互换 / 删除只读有界的 `Track.ID` 前缀，绝不读分析尾部。 | 检验：`pkg/enginedb`（`TestBridgeAnalysisRowsAreCompressedAndLossless` 等）、`pkg/bridge` 大 PDB 重复挂载计时；大 U 盘实际识别速度仍需维护者验收（行 240、241）。 |
| **#237 / #245**（音频审计、Live Stream 保存不断听众） | `tools/ui-mod/test_rt_budget.c`<br>`tools/ui-mod/test_rt_callback.c`<br>`pkg/broadcast/` | **原因**：保存任一 `直播推流` 设置会同时重启采集、推流与局域网流，OBS、VLC 要重连；音频路径代码缺少统一的实时性检查（首次切换主 Pioneer FX 模式时清零 488 KB 未触碰的 BSS 并触发缺页，767 µs 超过 667 µs 的块时间）。<br>**修法**：采集与本地听众继续，只重启外部推流和局域网流；测试在计时前先触碰状态，并取三次相同运行里最短的块时间，预算不变；新增检查钩子音频线程函数不做分配、加锁、等待或 I/O 调用。 | 检验：`pkg/broadcast` `TestSettingsChangeKeepsLocalListeners`、`test_rt_budget.c` 与 `test_rt_callback.c`（本机与 armhf，`banned=0 misses=0`）；真机 `/live.wav` 在 VLC 中改设置不重连待做（行 228–230）。 |
| **#285 / PR #287**（PRIME 4 效果选择） | `tools/ui-mod/build_packs.py`<br>`tools/ui-mod/` 效果选择器 | **原因**：连续转动旋钮浏览效果时选择窗口会关闭，通道 OLED 无法对应所选效果，旋钮每格移动的效果数不是一个。<br>**修法**：连续转动重启 600 ms 自动确认计时，原厂到原厂的选择走原生快速路径，OLED 按发布的 Planck 索引显示全部 31 个原厂与 PX 名称；旋钮每个有效格只发一个 `Plus` / `Minus`，与原厂 `incValue` / `decValue` 一致。 | 检验：`UiModSourceTest.test_prime4_fx_selector_browsing_and_oled_mapping_issue285`、`test_prime4_fx_knob_moves_one_effect_per_detent`、`test_fx_selector_sandbox.sh`；真机 PRIME 4 待验（行 246、247）。 |
| **PR #282**（西班牙语界面，作者 Javier Saenz） | `tools/ui-mod/`<br>`pkg/webui/` | **需求**：没有西班牙语。<br>**修法**：`设置 → 设备 → 语言` 新增 `Español`，`lang=es` 选用西班牙语钩子包，控制台与手机页全部文字有西班牙语，未知语言回退英文。 | 检验：`LanguagePackSelectionTest`、`TestConsoleIsFullyTranslated`、`TestEveryPhoneTextHasEveryLanguage`；真机选 Español 重启后查看菜单重音与排版待做（行 248）。 |
| **一体机原厂视图**（#285 后续） | `tools/ui-mod/build_packs.py` | **原因**：一体机上未保存的视图 1–3 被 `{pads: false}` 覆盖，`SHIFT + VIEW` 不再循环；横向波形旁显示曲库时 `LOAD` 不可用；`联机视图` 锁住通道。<br>**修法**：未保存的视图 1–3 交给原厂 `ViewStateController.resetToDefault`，已保存的视图优先；`compactBrowserVisible` 在横向波形加曲库时保持为真；`联机视图` 只在独立播放器（JP07/08/13/14）上提供并生效。 | 检验：`Prime4ViewsTests`（三项）、`test_prime4_views_sandbox.sh`；真机 PRIME 4 / SC LIVE 4 / Mixstream 待验（行 249–251）。 |
| **高速 SD 卡**（实验性，默认关） | `rootfs-overlay/usr/local/bin/enginex-sd-highspeed`<br>`rootfs-overlay/etc/systemd/system/`<br>`pkg/webui/`<br>`tools/ui-mod/build_packs.py` | **需求**：SD 卡槽以 25 MHz 默认速度运行（最高 12.5 MB/s）。<br>**修法**：`设置 → 设备 → 高速 SD 卡模式`（网页控制台「碟机显示与音频」同一开关）打开后，在开机早于 `edisksd` / `engine` 时以及现场切换时，只对 `ff0c0000` 的 SD 控制器置位 caps 的 `0x4`，空闲时只读地重新初始化，碟机播放时延后并提示「将在下次启动或重新插入 SD 卡时生效」，`dmesg` 出现 mmc 错误或卡消失时撤回；PRIME 4 G2 / SYSTEM ONE 不显示。最高 25 MB/s，SC5000 上实测 23 MB/s。 | 检验：`tests/test_rootfs_overlay.sh`、`TestSdHighSpeedDefaultsOffPersistsAndApplies`、`test_sd_high_speed_switch_rk3288_only_and_four_languages`、数据包测试；真机 RK3288 读速与重启后验证待做（行 252）。 |
| **其他** | `pkg/audioheader/`、`pkg/compat/`、`docs/mods/src/`、`pkg/rekordbox/`、`pkg/stems/` | 带 ID3 的 FLAC / WAV / AIFF 与 VBR MP3 元数据；官方 Mod 在无 FFmpeg 的碟机上可用；OneLibrary Cue 位置溢出；Stems 在网络盘上的开关与原厂一致；回到原厂时不再复用 EngineX 的 QML 字节码。 | 检验：各包 Go 套件（含 armhf）。 |

---

## 2. 实机验收与待实机验证项目清单

以下项目在撰写时**没有**实机结论，发布前由维护者或发版人逐项确认；结论写入下方 `【待填】`：

- **蓝牙音箱输出**（行 187、188）：维护者 2026-11-14 在真实 SC5000 上试听并认可音质；其余机型的 `输出来源` 选项、延迟约 150–250 ms、已连接手机等输入设备时面板停止扫描的提示仍待确认。`【待填】`
- **复制设置与 USB 配置导入**（行 200、202、210）：两台碟机的 Link / USB / 撤销验收，Player 编号保留。`【待填】`
- **分侧反转转盘**（行 205）：物理转动搓碟、外圈微调、`Shift + 转盘`。`【待填】`
- **`推子回音`**（行 190）与 **电动转盘 4 轨**（行 186）：一体机真机音频，`SC5000M` / `SC6000M` 真机。`【待填】`
- **有线 + Wi-Fi**（行 192、193、213）：两台 SC5000，三次以上开机，`ip route get <对端碟机>` 为 `dev eth0`，日志无 `online check failed`。`【待填】`
- **3-Band 与概览**（行 194–196、203、219）：对照 CDJ-3000 / rekordbox，旧导入 U 盘重插后 Half / Full 高度。`【待填】`
- **Live Stream**（行 197、223）：真实 Icecast 服务器播放推送；无听众时 `stream-sync` 占用。`【待填】`
- **碟机本机 Stems**（行 222）：播放中的耗时（未测）。`【待填】`
- **签名更新与安装器**（行 215、216、218）：屏幕从 r5 更新到已签名发布、拒绝未签名测试发布；WinUSB 绑定与 USB 传输；双击安装。`【待填】`
- **恢复官方固件**（行 201、217）：真实 SC5000 官方刷入并回到 EngineX。`【待填】`
- **PRIME 4 效果选择与视图**（行 246、247、249–251）：PRIME 4 连续浏览效果、旋钮一格一个效果；`SHIFT + VIEW`、`LOAD` 与 `联机视图` 的显示。`【待填】`
- **西班牙语**（行 248）：选 Español 重启 Engine，检查菜单重音与排版、控制台和手机页切换。`【待填】`
- **高速 SD 卡**（行 252）：RK3288 机型上 `caps`、`ios` 的 `timing spec: 2 (sd high-speed)`、约 23 MB/s 读速，播放中切换的提示，开着重启后的状态。`【待填】`
- **U 盘**（行 209、211、212、214、224、225、226）：HFS+ 弹出与更新、暂存库占用、对方盘无 Stems、SD 重插与用户数据恢复、NH08 Spotify。`【待填】`
- **SC LIVE 4**：Remote Library 封面（行 185）、演出曲目表导出与原厂 History 对照（行 206）。`【待填】`

---

## 3. 全套测试与三组发版门禁结果

- **构建提交**：`【待填】`
- `gofmt -l .`：`【待填】`；`go vet ./...`：`【待填】`；`go test -race ./...`：`【待填】`
- **Python / overlay / UI 包套件**（`tools/az01-packer`、`tools/ui-mod`）：`【待填】`
- **文档审计**（`tools/az01-packer/test_docs_audit.py`）：撰写时在 x86 编译机上通过（5 项）；发布构建后复跑：`【待填】`
- **第 4 步镜像检查**：`【待填】`
- **第 4b 步门禁**（旧镜像 `v5.3.0.4-r5` 在线升级 → 新版开机全功能验收；`JP07`、`JP13`、`JC11` 三组）：`【待填】`
- **签名发布检查**（`sign-release.sh`、`SHA256SUMS.txt.sig` 验证）：`【待填】`

---

## 4. 发布产物与 SHA-256 校验表（v5.3.0.5 全量 28 产物清单）

- **说明**：下表 SHA-256 全部是占位符 `【待填】`，由构建出的 `SHA256SUMS.txt` 逐行填入，**不要**在构建前使用。
- **产物目录**：`【待填】`；`SHA256SUMS.txt` 共 **28 行**（16 个 `*-Update.img` + 8 个 `*-Data-Bundle.tar.gz` + 4 个 `EngineX-Installer-5.3.0.5-*`），连同 `SHA256SUMS.txt` 与 `SHA256SUMS.txt.sig` 共 30 个上传文件。
- **回滚点**：tag `v5.3.0.4-r5`（`9e827205`）。

### 一代机型完整固件（`*-Update.img`）

| 镜像组 | 机型 | 板号 | 文件名 | SHA-256 |
| --- | --- | --- | --- | --- |
| SC5000 | Denon DJ SC5000 Prime | JP07 | `SC5000-5.3.0.5-EngineX-OS-Denon-SC5000-Prime-JP07-Update.img` | `【待填】` |
| SC5000 | Denon DJ SC5000M Prime | JP08 | `SC5000M-5.3.0.5-EngineX-OS-Denon-SC5000M-Prime-JP08-Update.img` | `【待填】` |
| SC5000 | Denon DJ PRIME GO | JP11 | `PRIMEGO-5.3.0.5-EngineX-OS-Denon-PRIME-GO-JP11-Update.img` | `【待填】` |
| SC6000 | Denon DJ SC6000 Prime | JP13 | `SC6000-5.3.0.5-EngineX-OS-Denon-SC6000-Prime-JP13-Update.img` | `【待填】` |
| SC6000 | Denon DJ SC6000M Prime | JP14 | `SC6000M-5.3.0.5-EngineX-OS-Denon-SC6000M-Prime-JP14-Update.img` | `【待填】` |
| PRIME 4 | Denon DJ PRIME 4 | JC11 | `PRIME4-5.3.0.5-EngineX-OS-Denon-PRIME-4-JC11-Update.img` | `【待填】` |
| PRIME 4 | Denon DJ PRIME 2 | JC16 | `PRIME2-5.3.0.5-EngineX-OS-Denon-PRIME-2-JC16-Update.img` | `【待填】` |
| PRIME 4 | Numark Mixstream Pro | NH08 | `MIXSTREAMPRO-5.3.0.5-EngineX-OS-Numark-Mixstream-Pro-NH08-Update.img` | `【待填】` |

### 二代机型统一升级镜像（`*-Update.img`，#215 主选格式）

| 架构组 | 机型 | 板号 | 文件名 | SHA-256 |
| --- | --- | --- | --- | --- |
| AZ0x (armhf) | Denon DJ SC LIVE 4 | JP21 | `SCLIVE4-5.3.0.5-EngineX-OS-Denon-SC-LIVE-4-JP21-Update.img` | `【待填】` |
| AZ0x (armhf) | Denon DJ SC LIVE 2 | JP20 | `SCLIVE2-5.3.0.5-EngineX-OS-Denon-SC-LIVE-2-JP20-Update.img` | `【待填】` |
| AZ0x (armhf) | Denon DJ PRIME 4+ | JC11S | `PRIME4PLUS-5.3.0.5-EngineX-OS-Denon-PRIME-4-Plus-JC11S-Update.img` | `【待填】` |
| AZ0x (armhf) | Denon DJ PRIME GO+ | JP11S | `PRIMEGOPLUS-5.3.0.5-EngineX-OS-Denon-PRIME-GO-Plus-JP11S-Update.img` | `【待填】` |
| AZ0x (armhf) | Numark Mixstream Pro+ | NH08S | `MIXSTREAMPROPLUS-5.3.0.5-EngineX-OS-Numark-Mixstream-Pro-Plus-NH08S-Update.img` | `【待填】` |
| AZ0x (armhf) | Numark Mixstream Pro Go | NH10 | `MIXSTREAMPROGO-5.3.0.5-EngineX-OS-Numark-Mixstream-Pro-Go-NH10-Update.img` | `【待填】` |
| AZ04 (aarch64) | Denon DJ PRIME 4 G2 | JP24 | `PRIME4G2-5.3.0.5-EngineX-OS-Denon-PRIME-4-G2-JP24-Update.img` | `【待填】` |
| AZ04 (aarch64) | Rane SYSTEM ONE | RMZ2 | `SYSTEMONE-5.3.0.5-EngineX-OS-Rane-SYSTEM-ONE-RMZ2-Update.img` | `【待填】` |

### 二代机型过渡兼容数据包（`*-Data-Bundle.tar.gz`，供旧版 `5.3.0.2` / 未解锁 `5.3.0.3` 屏幕 OTA 升级）

| 架构组 | 机型 | 板号 | 文件名 | SHA-256 |
| --- | --- | --- | --- | --- |
| AZ0x (armhf) | Denon DJ SC LIVE 4 | JP21 | `SCLIVE4-5.3.0.5-EngineX-OS-Data-Bundle.tar.gz` | `【待填】` |
| AZ0x (armhf) | Denon DJ SC LIVE 2 | JP20 | `SCLIVE2-5.3.0.5-EngineX-OS-Data-Bundle.tar.gz` | `【待填】` |
| AZ0x (armhf) | Denon DJ PRIME 4+ | JC11S | `PRIME4PLUS-5.3.0.5-EngineX-OS-Data-Bundle.tar.gz` | `【待填】` |
| AZ0x (armhf) | Denon DJ PRIME GO+ | JP11S | `PRIMEGOPLUS-5.3.0.5-EngineX-OS-Data-Bundle.tar.gz` | `【待填】` |
| AZ0x (armhf) | Numark Mixstream Pro+ | NH08S | `MIXSTREAMPROPLUS-5.3.0.5-EngineX-OS-Data-Bundle.tar.gz` | `【待填】` |
| AZ0x (armhf) | Numark Mixstream Pro Go | NH10 | `MIXSTREAMPROGO-5.3.0.5-EngineX-OS-Data-Bundle.tar.gz` | `【待填】` |
| AZ04 (aarch64) | Denon DJ PRIME 4 G2 | JP24 | `PRIME4G2-5.3.0.5-EngineX-OS-Data-Bundle.tar.gz` | `【待填】` |
| AZ04 (aarch64) | Rane SYSTEM ONE | RMZ2 | `SYSTEMONE-5.3.0.5-EngineX-OS-Data-Bundle.tar.gz` | `【待填】` |

### 跨平台一键安装器（`EngineX-Installer`）

| 平台 | 文件名 | SHA-256 |
| --- | --- | --- |
| Windows 10/11 x64 | `EngineX-Installer-5.3.0.5-windows-amd64.zip` | `【待填】` |
| macOS Apple Silicon (M1–M4) | `EngineX-Installer-5.3.0.5-macos-arm64.zip` | `【待填】` |
| macOS Intel | `EngineX-Installer-5.3.0.5-macos-amd64.zip` | `【待填】` |
| Linux x86_64 | `EngineX-Installer-5.3.0.5-linux-amd64.tar.gz` | `【待填】` |



---

## 5. 实机影响与升级说明

- **一代机（8 款 `*-Update.img`）**：已刷 `5.3.0.4`（含 r2–r5）或更早版本的碟机，发布后可在 `设置 → 关于 / 更新` 收到 5.3.0.5 在线升级；也可把对应 `*-Update.img` 放入 FAT32 / exFAT U 盘根目录，经 `设置 → 关于 / 更新 → 固件更新模式 → 选项` 选 `USB/SD`。
- **二代机**：已解锁的二代机在屏幕上更新，或用 U 盘 `*-Update.img`；`5.3.0.2` / 未解锁 `5.3.0.3` 的二代机继续通过随附的 `*-Data-Bundle.tar.gz` 屏幕 OTA（最后一次，`5.3.0.6` 起退役）；原厂二代机首次安装用 `EngineX-Installer`，会清空机身内置存储，请先备份。
- **5.3.0.4 及更早的碟机不验签**：先更新到 5.3.0.5，之后的更新才会验签；已签名但比碟机当前版本旧的发布会被拒绝（同版本可重装修复），有意降级用安装器的 `--image` / `--bundle`。
- **回到原厂**：先启动一次 5.3.0.5 再正常关机，然后运行 `Restore-Official-Firmware.*`；原厂已卡在 Logo 且无法 SSH 时，先用安装器刷入 EngineX 并让它启动，再运行恢复工具（原厂刷机保留 `/data`，旧 QML 缓存不会因再刷一次原厂而消失）。
- **配置与数据保留**：升级保留 `/data/enginex` 下的流媒体登录态、歌单缓存、视图预设、已装 Mod 与 SSH 公钥；`Profile → Save to my drive` 现在也保存 EngineX 偏好。
- **已知限制**：蓝牙约增加 150–250 ms，不适合打拍对齐；碟机本机 Stems 不是神经网络，仍有串音；`Live Stream` 只有 WAV；`#247` 的 SoundCloud 保存、`#268` 的 Remote Library 记录与 Prepare 仍未完成；`#253` 大 U 盘识别速度需实机计时；`#244`（PRIME 4 G2 / SYSTEM ONE 数据分区大小）**不在**本版本，需要这两款机型用户的只读分区测量（#291）。
- **已知问题（Spotify）**：许多账号（包括较早的 Premium 账号）播放 Spotify 会失败，提示 “audio key refused”。Spotify 更改了服务器，所有基于 librespot 的客户端都无法获得音频密钥（librespot-org/librespot#1649，2026-11-07 已锁定，没有已知修复）；碟机上无法修复，请改用 TIDAL、Beatport、SoundCloud 或 Deezer。来源菜单里仍保留 Spotify 入口

---

## 6. 本次修复的 Issue 清单

#205、#213、#214（PR）、#216、#222（BH-GF3-12 / BH-GC2-10）、#223、#233、#237、#238、#239、#242、#243、#246、#245、#247、#248、#252、#253（识别速度待实机验收）、#285、#287（PR）、#282（PR，Javier Saenz）、#255、#258、#261、#263、#264、#266（PR）、#268（仅导出已有历史）、#269、#272、#273（PR）、#274、#277、#279、#281、#283。同时合入 `#234` 更名、`#215` 的 OTA 桥接（不含 `#244`）和 `#236` 的后台负载收敛。

部分完成或待实机验收的 Issue 不应在发布时直接关闭：`#247`、`#252`、`#268`、`#253`；`#244` 保持打开。
