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
| **#238**（碟机本机 Stems 分离） | `internal/stems/separate.go` | **原因**：旧方案只在 250 Hz 与 4 kHz 处切频段，在 MUSDB18 片段上每个分轨都低于 0 dB。<br>**修法**：`LocalRender` 改用 STFT 软掩码分离器，人声 / 贝斯 / 鼓 / 旋律达到固定的信号失真比下限；不是神经网络，仍有串音；人声保持原有的立体声位置；Stems 上传卡住或极慢时按期限释放名额与工作进程（#243）。实测 60 秒音乐在空闲 SC5000 上约 16.5–17.0 s（人声立体声化前），构建机 armhf 核心上多约 6%。 | 检验：`internal/stems` Go 套件；碟机播放中的耗时未测（行 222）。 |
| **#268**（演出曲目表导出） | `pkg/webui/` | **原因**：无法把已记录的演出导出为曲目表。<br>**修法**：认证后的文本 / CSV 下载，在 Engine 持有曲库时只读原厂 `hm.db` 历史，保留演出边界、播放顺序、重复曲目，处理已提交 WAL 与未知时钟，绝不创建或修改历史库。Remote Library 记录与 Prepare 仍待查。 | 检验：`pkg/webui` 的 `TestTracklist268Export`；真机 SC LIVE 4 对照原厂 History 待做（行 206）。 |
| **3-Band / 概览**（rekordbox 配色、概览高度、跨读取补全） | `tools/ui-mod/build_packs.py`<br>`pkg/prolink/`<br>`pkg/bridge/`<br>`pkg/rekordbox/` | **原因**：`3-Band` 与 rekordbox / CDJ-3000 配色不同；rb-bridge 在 `overviewWaveFormData` 末尾写零，Engine 按未缩放字节只画约三分之一高度，QML 再放大；PWV7 三频段被按错误顺序（中 / 高 / 低）旋转；已播放过的曲目与导出读取超过挂载时限的卡留有零尾。<br>**修法**：配色改为低频蓝、中频橙、高频白，静音中频褪为棕；overview 写入三个频段最大值；PWV7 按低 / 中 / 高读取；先补全概览再读导出，补全可跨挂载续做。 | 检验：`ThreeBandPaletteTest`、eglfs 沙箱；实机对照 CDJ-3000 / rekordbox 与旧导入 U 盘重插待做（行 194–196、203、219）。 |
| **Live Stream 只输出 WAV、按需采集** | `pkg/broadcast/`<br>`pkg/webui/`<br>`tools/ui-mod/` | **原因**：控制台提供 MP3 / OGG，但镜像没有这两种编码器，默认 `mp3` 永远连不上；没有收听者时仍持续采集并重采样，空闲碟机约占 7.3% 单核。<br>**修法**：固定 WAV（16 位、48 kHz、立体声）；旧设置自动转 WAV，Shoutcast 目标不再推送，`/live.mp3` 重定向到 `/live.wav`；没有 `/live.wav` 听众、Icecast 服务器与 LAN 流时不采集。 | 检验：`go test -race ./pkg/broadcast/... ./pkg/webui/...`；真实 Icecast 服务器播放待做（行 197、223）。 |
| **有线 + Wi-Fi 同网段** | `rootfs-overlay/`（路由守护与 sysctl）<br>`pkg/webui/` | **原因**：两台 SC5000 同时走有线与 Wi-Fi 时，Link 曲目走了 Wi-Fi（13–19 MB 曲目加载 7.4–11.3 s）；网线地址晚于 Wi-Fi 到达时，Engine 报 `online check failed` 并断开 Wi-Fi；两张网卡互相应答对方地址。<br>**修法**：同网段时 Link 路由走 `eth0`；路由守护让 Wi-Fi 保持；每个接口 `arp_announce=2`、`arp_ignore=1` 从启动起生效。 | 检验：overlay 套件；两台真机多次开机验证待做（行 192、193、213）。 |
| **#223**（Remote Library 列表封面） | `tools/ui-mod/build_packs.py` | **原因**：`BrowseView` 的 `AlbumArt.qml` 把带来源标签的行都发给网关 `/v1/sc5000/art`，而它只认自己列表里的封面，返回 404，列表出现灰块。<br>**修法**：只有 Engine 自己没有封面（`""` 或 `image://streaming/`）时才询问网关，Engine 已有的封面不再被丢弃。 | 检验：Step 2 套件；SC LIVE 4 真机验收待做（行 185）。 |
| **#264 / #272 / #252 / #216 / #246**（大 U 盘与 rekordbox 导入） | `pkg/compat/`<br>`pkg/bridge/`<br>`pkg/enginedb/`<br>`internal/cli/` | **原因**：大型导入重复 `Lstat` 数万次、桥接状态在 FAT32 上可超 4 GiB、挂载时限被占满；Pro DJ Link 暂存库可把存储卡占满（58.6 GiB 卡被占 50 GiB）；一代机的 pack 把原厂的网络盘判断改成恒为真，对方播放器的盘出现 Stems 入口（Engine 无法写入 Link 来源，报「空间不足」）；旧导入补全时重写曲目 / 歌单 ID、丢失碟机上的编辑；旧 MP3 网格偏移。<br>**修法**：解析器缓存路径查询并用 `DirEntry.Type()`；`BridgeAnalysis` 行数按 3 GiB 封顶；暂存库按盘限量与清理；Stems 滑动入口与 Stems 列表只在本机自己的盘上出现（与原厂一致）；按已保存来源记录匹配旧行并保留已播放标记、Cue、Loop 与歌单 ID；MP3 网格迁移与共享挂载时限；导入不再覆盖 Engine 同时对曲库做的改动。 | 检验：Go 套件（`-race`）、覆盖层与打包套件；真机 SD 重插与数据恢复为独立验收（行 189、191、207、211、212、214）。 |
| **#269 / #277 / #279 / #247 / #248**（流媒体） | `pkg/providers/`<br>`pkg/webui/`<br>`tools/ui-mod/` | **原因**：Spotify 载入会弹出载体、TIDAL 登录载体被挤占；收藏夹里出现 TIDAL 专属空白行且无法进入 My Playlists；原生 Ogg 失去 Engine 认识的下载名；音源设置存在先读后写的丢失更新；歌单第二页失败时整批丢弃。<br>**修法**：保留 Spotify 载入与 TIDAL 载体、识别两种 `TIDAL Rising` 拼写并走原厂 `push()`、用载体补丁的 `mp3` 命名标记而 MIME 仍为 `audio/ogg`、两个写入者改用加锁的 `BPMStore.UpdateConfig`、保留已成功获取的有序前缀。 | 检验：`pkg/providers`、`pkg/webui` 套件；NH08 真机浏览 Spotify 待做（行 225、226）；#248 的界面提示与 #247 的 SoundCloud 保存仍待其所有者完成。 |
| **HFS+ U 盘**（弹出、`*-Update.img` 更新） | `pkg/webui/`<br>`rootfs-overlay/`（updater 路径） | **原因**：Engine 持有 HFS+ 曲库时弹出被拒 26 次且不说明原因；原厂 `az01-update` 给更新环境传了空的 `upgrade-file=`（或去掉了文件夹的文件名），因为 HFS+ 视图是 `/media/<卷>` 上的 tmpfs 绑定，原厂向上找根目录找不到 U 盘自己的根。<br>**修法**：保留真实弹出错误并解除缓存曲库；原厂读取 `/tmp/update-file-path` 之前由 `engine-ui-guard update-file` 把 U 盘上的所选文件映射成 U 盘上的路径（符号链接与越出 U 盘的路径保持原样）；正常模式与安全模式（回退到原厂 Engine）都走同一步骤。 | 检验：Go / overlay 套件；真机 HFS+ U 盘更新需实机（行 209、224）。 |
| **影子碟搓碟** | `pkg/webui/shadow*.go`<br>`tools/ui-mod/build_packs.py` | **原因**：用力或极快（超过 14.4 倍）搓碟时，影子副本按对端约 40 ms 前的位置汇报来导向，出现反向与更宽的位移、冻结与跳转。<br>**修法**：沿源碟自己的路径连续跟随，不超出、不反向。 | 检验：Step 2 套件；双 SC5000 真机搓碟待做（行 198、199）。 |
| **空闲碟机负载** | `rootfs-overlay/`（boot-watch、thread sweep） | **原因**：`engine-ui-guard watch` 每秒 fork `cut`、`cat`、`rm`（约 0.50% 单核），30 秒线程巡检读取约 11 万次系统调用（空闲约 1.16% 单核）。<br>**修法**：改用 shell 内建 `read`，已见过且同名的线程跳过。 | 检验：沙箱（nspawn）；真机 `sc5000-perf-tune` 应低于 0.1% 单核（行 220、221）。 |
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
- **已知限制**：蓝牙约增加 150–250 ms，不适合打拍对齐；碟机本机 Stems 不是神经网络，仍有串音；`Live Stream` 只有 WAV；`#248` 的界面提示与 `#247` 的 SoundCloud 保存、`#268` 的 Remote Library 记录与 Prepare 仍未完成；`#253` 大 U 盘识别速度需实机计时。

---

## 6. 本次修复的 Issue 清单

#205、#213、#214（PR）、#216、#222（BH-GF3-12 / BH-GC2-10）、#223、#233、#238、#239、#243、#246、#247、#248（部分，界面提示未完成）、#252、#255、#258、#261、#263、#264、#266（PR）、#268（仅导出已有历史）、#269、#272、#274、#277、#279。

部分完成的 Issue 不应在发布时直接关闭：`#248`、`#247`、`#252`、`#268`、`#253`。
