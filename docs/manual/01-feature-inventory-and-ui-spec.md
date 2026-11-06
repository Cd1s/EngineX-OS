# EngineX OS 全功能盘点与 UI 开关入口规范（Issue #102 · 第 1 部分）

- 盘点基线：`main` @ `b5a4ba8`（版本 5.3.0.2）。扫描范围：`cmd/`、`internal/`、`pkg/`、`tools/ui-mod/`、`tools/az01-packer/`、`tools/stems/`、`tools/edisksd-hook/`、`rootfs-overlay/`、各 README。
- 本文只读扫描，没有改任何代码。所有"改动补丁建议"都在第 6 节，由对应文件的负责人落地。
- 证据写法：`文件:行/符号`。"待实机核实"表示只在代码里看到，没有实机证据。

## 0. 约定与必须先读的更正

**路径约定（Issue #103）**：运行目录主路径写作 **`/data/enginex/`**，兼容旧路径 `/data/sc5000-custom/`（开机迁移后留软链接 `ln -sfn /data/enginex /data/sc5000-custom`）。配置目录 `/etc/enginex`、接口 `/v1/enginex/*` 同步提供别名，旧的 `/etc/sc5000`、`/v1/sc5000/*` 继续可用。本文下面凡写 `<DATA>`，指 `/data/enginex/`。

> 现状：`b5a4ba8` 的代码仍然写死 `/data/sc5000-custom` 和 `/v1/sc5000/*`，共 28 个文件含 `sc5000-custom`（Go、Python、sh、C 都有）。#103 落地之前，本文里的 `<DATA>`、`/v1/enginex/*` 在实机上还不存在。

**对任务描述的三处更正**（照任务原文写会出错）：

| 任务原文 | 仓库实际 |
|---|---|
| Web 控制台地址 `http://<碟机IP>:8080/ui` | 地址是 **`http://<碟机IP>:8080/`**（`pkg/webui/server.go` 的 `GET /`）。`/v1/sc5000/ui` 是碟机本机调用的设置接口，不是网页 |
| Web 入口代码在 `pkg/webui/ui_page.go` | 没有这个文件。页面是 **`pkg/webui/index.html`**（嵌入二进制），登录页是 `server.go` 里的 `consoleLoginPage` |
| Web 控制台有"一键在线 OTA 更新"和"日志诊断" | **都没有**。OTA 只在碟机屏幕（设置 → 关于 / 更新）上，走本机回环接口 `/v1/sc5000/update?apply=1`；`/api/update` 在控制台里只读且需登录，页面没有调用它。全仓没有日志查看页 |

## 1. 支持的 16 款机型

识别方式：开机读 `/sys/firmware/devicetree/base/compatible`，`pkg/webui/update.go` 的表映射到前缀；碟机屏幕上的"检查更新"据此选对应 `.img` 或数据包。

### 1.1 一代机（8 款，`.img` U 盘刷机，3 个镜像组）

| 机型 | 前缀 | 板号 / compatible | 架构 | 镜像组（组内逐字节相同） | 安装方式 | 类别 |
|---|---|---|---|---|---|---|
| Denon DJ SC5000 Prime | `SC5000` | JP07 / `inmusic,jp07` | ARMv7 armhf（RK3288） | SC5000 组 | U 盘刷 `.img`：设置 → 关于 / 更新 → 固件更新模式 | 独立播放机 |
| Denon DJ SC5000M Prime | `SC5000M` | JP08 | armhf | SC5000 组 | 同上 | 独立播放机 |
| Denon DJ PRIME GO | `PRIMEGO` | JP11 | armhf | SC5000 组 | 同上 | 一体机 |
| Denon DJ SC6000 Prime | `SC6000` | JP13 | armhf | SC6000 组 | 同上 | 独立播放机 |
| Denon DJ SC6000M Prime | `SC6000M` | JP14 | armhf | SC6000 组 | 同上 | 独立播放机 |
| Denon DJ PRIME 4 | `PRIME4` | JC11 | armhf | PRIME 4 组 | 同上 | 一体机 |
| Denon DJ PRIME 2 | `PRIME2` | JC16 | armhf | PRIME 4 组 | 同上 | 一体机 |
| Numark Mixstream Pro | `MIXSTREAMPRO` | NH08 | armhf | PRIME 4 组 | 同上 | 一体机 |

- 文件名：`<前缀>-<版本>-EngineX-OS-<品牌型号>-Update.img`（`build-release.sh`）。
- "独立播放机"（`playerBoards`：SC5000/5000M/6000/6000M）没有通道推子：4 碟布局只对它们开放。Echo Out（Upfader Echo Out）按最新定稿对它们**也生效**——暂停切歌时释放 2.2 秒回声尾音（旧版本曾对独立播放机隐藏该开关，以发布版为准）。
- 官方底包 5.3.0（打包用）；原厂 4.3.x、5.0.x 都能直接刷。

### 1.2 二代机（8 款，`/data` 数据包 `Data-Bundle.tar.gz`，Beta）

固件带签名，不接受改过的 `.img`；EngineX 装在 `<DATA>`，不动系统分区。**这些机器不能刷一代的 `.img`。**

| 机型 | 前缀 | 板号 / compatible | Engine 架构 | 安装方式 |
|---|---|---|---|---|
| Denon DJ SC LIVE 4 | `SCLIVE4` | JP21 / `inmusic,jp21` | armhf（基准 Engine） | EngineX-Installer（SSH 或 USB Fastboot）、手动 `install.sh`、host-install-fastboot |
| Denon DJ SC LIVE 2 | `SCLIVE2` | JP20 | armhf | 同上 |
| Denon DJ PRIME 4+ | `PRIME4PLUS` | JC11S | armhf | 同上 |
| Denon DJ PRIME GO+ | `PRIMEGOPLUS` | JP11S | armhf | 同上 |
| Numark Mixstream Pro+ | `MIXSTREAMPROPLUS` | NH08S | armhf | 同上 |
| Numark Mixstream Pro Go | `MIXSTREAMPROGO` | NH10 | armhf | 同上 |
| Denon DJ PRIME 4 G2 | `PRIME4G2` | JP24 | **aarch64**（AZ04，64 位 Engine） | 同上 |
| Rane SYSTEM ONE | `SYSTEMONE` | RMZ2 | **aarch64**（AZ04） | 同上 |

- 前置条件：先升到官方 Engine OS 5.3.0。首次装好后，后续版本在碟机屏幕 设置 → 关于 / 更新 → 更新 一键完成（下载 `Data-Bundle`、校验 `SHA256SUMS`、替换文件、重启服务，#86）。
- 卸载：`sh <DATA>/bundle/uninstall.sh && reboot`。
- **AZ04（PRIME 4 G2 / SYSTEM ONE）能力缺口**（`build-data-bundle.py` 的 `AZ04_NOTE`，README 同）：UI 包、流媒体网关（Deezer、SoundCloud、Beatport）和各服务已有；**DSP 防护、0dB 软限幅、自动增益、SQLite 调优、4 碟解锁没有移植**；Stems 保持官方原样；只有 6 处内存补丁；沙箱里没看到实际界面（需要 OP-TEE），待实机核实。
- 6 款 armhf 二代机共用 SC LIVE 4 的 Engine 构建（同 BuildID），功能面与一代机基本一致，但没有实机验证（README 明说）。

### 1.3 EngineX-Installer（`cmd/enginex-installer`，#88/#89）

一键自动发现安装，平台包：Windows x64、macOS arm64、macOS x64、Linux x64。两种模式：A 碟机在局域网且 SSH 开放（经 StageLinQ UDP 51337 或 EngineX Web 界面发现）→ 通过 SSH 推数据包；B 碟机在 Fastboot（USB）→ 生成 `/data` 镜像并刷写（会清空 `/data`，需 `--userdata-backup` 才保留）。一代机的 rootfs 刷写是实验功能。

## 2. 碟机屏幕上的开关入口

### 2.1 入口位置总览

屏幕上 EngineX 的东西分布在 6 处，没有统一的"EngineX"分组：

| 位置 | 内容 |
|---|---|
| 设置 → 设备（Device） | 16 项 EngineX 条目，平铺在原厂条目里，插在 `Player #` 之前（`DeviceTabContent.qml` 补丁） |
| 设置 → 混音台（Mixer） | Upfader Echo Out，插在 Crossfader 校准之前 |
| 设置 → 服务 → 流媒体（Services → Streaming） | 7 个流媒体服务开关；原厂 TIDAL / SoundCloud / Beatport LINK 行被隐藏 |
| 设置 → 关于 / 更新（About / Update） | 版本行（含 Web 控制台地址与访问码）；更新页改查 GitHub |
| 控制中心（Control Center）磁贴 | Waveform Style、Live Stream、Upfader Echo Out、Smart Sync |
| 音源页（Source）与曲库 | 每个流媒体服务一行 + "扫码登录 / 切换账号"；流媒体曲目左滑 Stems |

> 任务书提到的 Layout Manager、Utility 页：仓库没有往这两处加任何 EngineX 条目，只有 `i18n.tsv` 收录了原厂文案（`Layout`、`Control Center` 等标签页名）。下表不含它们。

### 2.2 设置 → 设备：EngineX 条目（按屏幕实际顺序）

`<DATA>/ui/settings.conf` 简称 conf。接口 `POST /v1/sc5000/ui?<参数>`（别名 `/v1/enginex/ui`）仅接受本机回环调用，Web 控制台用 `POST /api/ui`，参数相同。默认值是"conf 里没有这个键"时的取值。

| # | 英文 / 简中 / 繁中 | 作用 | 默认 | 接口参数 | conf 键 | 备注 |
|---|---|---|---|---|---|---|
| 1 | Power → Turn Off / 电源 → 关机 / 電源 → 關機 | 调用原厂关机确认框（#78） | — | — | — | 按钮，不是开关。仅 5.3.0 |
| 2 | Language / 语言 / 語言 | 界面语言 | `en` | `lang=en\|zh-Hans\|zh-Hant`（加 `restart=1`） | `lang` | 切换要重启 Engine，播放中断，屏幕有确认框 |
| 3 | Waveform Style / 波形样式 / 波形樣式 | 波形配色 Denon / RGB / 3-Band | `denon` | `waveform=denon\|rgb\|3band` | `waveform` | 控制中心有同名磁贴 |
| 4 | Streaming Quality / 流媒体音质 / 串流音質 | 下载音质 Lossless / 320kbps | `flac` | `quality=flac\|320k` | `quality` | 下一次下载起生效 |
| 5 | NetEase Download / 网易云下载方式 / 網易雲下載方式 | 官方直下 / Deemix 优先 | `official` | `neteaseMode=official\|deemix` | `netease_mode` | Deemix 优先用来省网易云月度下载额度 |
| 6 | Jog Light Mode / 转盘灯效 / 轉盤燈效 | 固定、迷幻流光、迷幻频闪、迷幻 BPM 同步 | `static` | `jog=static\|rainbow\|fast\|bpm` | `jog` | 只改 `/Engine/Deck<N>/Color`，不写 eMMC |
| 7 | Auto-Analyze Drives / 自动分析插入的设备 / 自動分析插入的裝置 | 插盘 15 s 后触发 Engine 自带分析（最低优先级） | 关 | `autoAnalyze=0\|1` | `auto_analyze` | #55 |
| 8 | Master Soft Clipper / 0dB 防破音软限幅 / 0dB 防破音軟限幅 | 主输出软限幅，峰值压到 -0.1 dBFS 以下 | 关 | `masterLimiter=0\|1` | `master_limiter` | 钩子每秒轮询，立即生效；#58。AZ04 未移植 |
| 9 | Auto-Gain (0 dB Normalize) / 自动增益（0 dB 归一化） / 自動增益（0 dB 正規化） | 把偏小的歌平滑提到接近 0 dBFS，最多 +9 dB，低于 -42 dBFS 的静音不提 | 关 | `autoGain=0\|1` | `auto_gain` | 开启后总会过软限幅；#64。AZ04 未移植 |
| 10 | OmniSource (Drives in Streaming) / OmniSource（在流媒体中显示 U 盘/SD） | U 盘 / SD 的歌单和曲目显示在流媒体浏览器里 | 关 | `omniSource=0\|1` | `omni_source` | #62 |
| 11 | Pro DJ Link Library / Pro DJ Link 曲库 / Pro DJ Link 曲庫 | 读取局域网内 CDJ 上 rekordbox U 盘的曲库 | 关 | `prolinkShare=0\|1` | `prolink_share` | 测试版；#59 |
| 12 | Pro DJ Link Export to CDJ (Beta) | 把本机 U 盘共享给 CDJ 浏览 | 关 | `prolinkExport=0\|1` | `prolink_export` | 实验功能；**简繁中文没有译文**（见 4.1）；状态 `prolinkExportActive` 只读 |
| 13 | 4-Deck Waveform / 四路波形显示 / 四路波形顯示 | 解锁 Engine 原生 4 碟布局 | 关 | `fourDeckWaveform=0\|1`（屏幕走 `scRestartCall`） | `four_deck_waveform` | 只在 SC5000/SC6000 系列显示（`scPlayer`）；切换重启 Engine；#49 |
| 14 | 4-Deck Role / 四轨角色 / 四軌角色 | 本机负责哪两台：自动、1+3、2+4、1+2、1、2 | `auto` | `fourDeckRole=auto\|13\|24\|12\|1\|2` | `four_deck_role` | 仅当 #13 开启时显示；重启 Engine。自动按 Engine 的 Player # 奇偶取 13 / 24 |
| 15 | Live Stream / 直播推流 | 碟机数字内录主输出：电脑免配置直连 `http://<碟机IP>:8080/live.mp3`、`/live.wav`（OBS 媒体源 / VLC / 浏览器）；或向 Shoutcast / Icecast 推流（CPU3 空闲核后台推送并同步歌名） | 关 | `broadcast=0\|1` | 不在 conf，落在 `<DATA>/state/broadcast-config.json` | 直连地址不需任何配置；推流的服务器地址、挂载点和密码在 Web 控制台 `http://<碟机IP>:8080` 的 Live Stream 卡片填。直连地址随下一次发版上线 |
| 16 | Echo Out / Upfader Echo Out / 拉杆切歌自动回音 / 推桿切歌自動回音 | C 语言 DSP Hook 在 44.1 kHz 主输出挂环形延迟缓冲：一体机快速拉下推子、单机播放器暂停切歌时释放 2.2 秒衰减回声尾音，不被推子归零切断，也不误开原厂 FX 灯 | 关 | `upfaderEcho=0\|1` | `upfader_echo` | **同一开关在 3 处出现**，见 4.2 |
| 17 | UI Scale / 界面缩放 / 介面縮放 | 整体缩放 Engine 界面（100%、95%、90%、85%、80%、75%），小屏一体机一屏能看到更多内容 | `100` | `uiScale=100\|95\|90\|85\|80\|75`（屏幕走 `scRestartCall`） | `ui_scale` | 经 `<DATA>/ui-scale.env` 里的 `QT_SCALE_FACTOR` 交给 Engine（drop-in `engine.service.d/70-ui-scale.conf`），重启 Engine 生效；选回 100% 时删掉 env 文件，恢复原厂尺寸 |

### 2.3 设置 → 混音台

| 条目 | 来源 | 说明 |
|---|---|---|
| Upfader Echo Out | EngineX | 与设备页同一开关；补丁**没有 `visible: !root.scPlayer`**，独立播放机上若有混音台页会显示一个无意义的开关，待实机核实 |
| Master Limiter / 原厂主输出限幅 (dB) | 原厂，EngineX 放开 | PRIME GO、Mixstream Pro 原厂隐藏，补丁让 `Config.audio.hasLimiter` 的板子显示；关 / 0–18 dB。与第 8 项并存 |

### 2.4 设置 → 服务 → 流媒体

- 7 个服务开关：网易云音乐、Spotify、Deezer、SoundCloud、TIDAL、Beatport、Apple Music（`sources=`，conf 键 `sources`，默认全开；`known=` 记录已见过的服务，让新版本新增的服务默认开启而用户关掉的保持关闭）。接口 `sources=netease,spotify,...`，未知名字返回 400。
- 当前浏览服务 `scope`（`scope=` / conf `scope`，默认 `deezer`）：在音源页点某个服务时写入，没有设置页入口。
- 隐藏：原厂的 TIDAL、Tidal、SoundCloud、Beatport LINK 行；Featured / Genres / Moods / TIDAL Rising 等行在非 TIDAL 服务下留空。

### 2.5 音源页与曲库

- 音源页每个服务一行；右侧"扫码登录 / 切换账号"（`Scan to log in` / `Switch account`），TIDAL 行没有（走原厂登录）。登录二维码对话框只登录对应服务，**不会打开控制台**（#79）。
- 曲库：流媒体曲目左滑出现第三个动作 `Stems`（分轨）；`Stems` 歌单与曲目 Stems 图标在所有机型解锁；`Stems List` 开关强制可见。
- 一代机 4 碟布局下左右滑加载的是本机角色负责的碟。

### 2.6 设置 → 关于 / 更新

- 版本行：`EngineX OS <版本> (github.com/<仓库>)`，下面两行 **Web Console `http://<IP>:8080`** 和 **Access Code `<访问码>`**（#79）。
- 更新页：模型改成 GitHub Release；"有新版本：EngineX OS x.y.z"、"已是最新"、"无法连接 GitHub"、"未找到发布版本"；点更新弹确认，一代机拉起原厂刷机进度条，二代机走 Data-Bundle。仍保留原厂的 USB/SD 扫描（固件更新模式）。
- 原厂条目被改：`Send Anonymous Usage Statistics` 切换不再触发 Engine 重启（`Analytics.qml` 补丁）；原厂"有新固件"弹窗被抑制；`Sync Time` 在任一碟播放时禁用（RA002，避免调时钟扰动音频）。

### 2.7 控制中心磁贴

| 磁贴 | 说明 | 键 |
|---|---|---|
| Waveform Style（DENON / RGB / 3BAND） | CDJ-3000 风格三段选择，点一下立即切换，无需重启 | `waveform` |
| Live Stream（OFF / ON） | 同 #15 | `broadcast` |
| Upfader Echo Out（OFF / ON） | 独立播放机隐藏 | `upfader_echo` |
| Smart Sync（Bar / Beat / Tempo，仅 5.3.0） | 写的是 Engine 自己的 `/Client/Preferences/Profile/Application/SyncMode`，**不在 conf**；另有常驻逻辑：开 Sync 后 BPM 跨度超出变速范围就把 Pitch Range 升档，最高 ±100%，只升不降 | Engine 偏好 |

### 2.8 其他"无入口"的行为开关（原厂条目被补丁改变或放开）

| 行为 | 位置 | 说明 |
|---|---|---|
| Silent Cue（静音 Cue） | 设置 → 播放 → Censor Button Behavior | 原厂按板子标志隐藏；补丁在 `/Engine/Deck1/SilentCue` 存在时放开 |
| 波形缩远到 30 秒（#77） | 设置 → 设备 → 旋钮行为 = Waveform Zoom | 一体机原厂 10 秒，钩子写 `Config.ui.waveform.maximumZoomSecs`=30 |
| Stems List 开关 | 曲库设置 | 强制可见 |
| "导入 rekordbox 库"提示 | — | 一律走 Ignore；由 rb-bridge 在挂载时转换 |
| "未安全弹出""不支持的存储设备""Load drive profile?""曲目过长"提示 | — | 去掉或自动选否（#48 等） |

### 2.9 conf 里有、屏幕和 Web 都没有入口的键

| 键 | 作用 | 访问方式 |
|---|---|---|
| `prolink_waveform` | 是否把 Pro DJ Link 的 3-Band 波形用到曲目（默认开，`=0` 关） | 只有接口 `prolinkWaveform=0\|1`。`i18n.tsv` 里有"Pro DJ Link Waveform"译文，但**屏幕条目从未建立**——孤儿翻译 |
| `github_repo` | OTA 检查的仓库（`owner/name`） | 手改 conf；非默认值才写入 |
| `playhead` | 记住播放头位置滑块（0–1） | 屏幕自动写，内部用，不该有入口 |
| `known` | 见 2.4 | 内部 |
| `safe-mode`、`boot-count`（文件） | 连续 3 次 15 s 内崩溃 → 回原厂界面 | Web 控制台显示 `safeMode` 状态；退出安全模式靠语言切换 / `restart=1` |

## 3. Web 控制台（`http://<碟机IP>:8080/`）

### 3.1 访问与安全

- 未登录显示登录页（标题 "Stream Sync"，按浏览器语言显示 英 / 简 / 繁）。访问码保存在 `<DATA>/state/access-token`（0600），**碟机屏幕 设置 → 关于 能看到**；删掉文件再重启即换新码。
- `POST /login` 校验后下发 Cookie `stream_sync`（HttpOnly、SameSite=Strict）；`/api/*` 另接受 `Authorization: Bearer`。写请求要求同源。
- `/v1/sc5000/*`（别名 `/v1/enginex/*`）和 TIDAL 网关只接受本机 `127.0.0.1`，同网的人调不了。
- 流媒体手机页 `/m/<服务>` 用一次性令牌，不是 Cookie，不能进控制台。

### 3.2 页面模块逐个清点（`index.html`，三语切换在右上角 EN / 简体中文 / 繁體中文）

| 模块 | 控件 | 作用 |
|---|---|---|
| 顶栏 | 版本胶囊、队列状态胶囊（`guard`）、语言按钮 | 状态只读 |
| 流媒体 | 7 个服务按钮（网易云、Spotify、Apple Music、SoundCloud、Deezer、Beatport；TIDAL 灰色，提示"到碟机上登录"）；Spotify Client ID 输入 + 保存；访客开关；Deezer 用户输入；账号下拉；浏览 / 重新登录 / 退出；二维码与"在手机上打开"链接；网易云"检查" | 在电脑上登录服务、查看歌单。碟机上登录走屏幕二维码 |
| 歌单 | 歌单 ID 输入 + 打开；曲目列表 | 选曲加入下载队列 |
| Wi-Fi 传歌 | 目标盘下拉、容量条、文件夹名、拖放区、进度条 | 存到 `<盘>/Wi-Fi Drop/<歌单>/`；MP3 / FLAC / WAV / AIFF / M4A / AAC / OGG / OPUS；曲库所在盘的 FLAC / MP3 自动分析并加入同名歌单；不覆盖已有文件，保留 256 MiB |
| 丢失曲目重定位（#74） | "扫描丢失曲目"（只预览）、"一键修复路径"（先备份 `m.db` 再改）、结果列表 | 同盘内被移动或改名的文件，按文件名+大小 → 忽略大小写 → 清理后的文件名匹配；修复后要在碟机上重新加载该盘 |
| 碟机显示与音频 | 流媒体音质、碟机界面语言、波形样式、原厂 4 碟布局（开关）、4 碟分工、拉杆回音、软限幅、自动增益、OmniSource、插盘自动分析、Pro DJ Link 曲库、Pro DJ Link 共享（实验性）共 12 项 | 与屏幕设置同一份 conf。4 碟项和回音项按 `player` 显示 / 隐藏；切 4 碟、切语言会先确认再重启 Engine |
| 下载与分析 | 任务列表 + 最新曲目 RGB 波形画布 | 下载、分析进度 |
| 直播推流（折叠） | 服务器、挂载点、协议（Icecast / Shoutcast）、格式（MP3 / OGG-Opus / WAV）、码率（128–320）、密码；保存、开始 / 停止 | 屏幕上只有开关，参数全在这里 |
| 云端登录服务（折叠） | 中继地址、新密码 | 让移动数据的手机也能登录；`sc5000-relay` 自建 |
| BPM 数据来源（可选，折叠） | SoundCloud Client ID / Secret / oauth_token、GetSongBPM Key | 给没有 BPM 的曲目补预览 |
| 手机扫码打开控制台（折叠） | 二维码 | 同局域网手机打开 |

### 3.3 手机登录页（`/m/<服务>`，也有三语）

| 服务 | 登录方式 |
|---|---|
| 网易云 | 用网易云音乐 App 扫码 |
| Spotify | 方式 A（推荐）：Spotify App「连接设备」里点 `EngineX OS`，约 1 秒绑定，无需 Client ID；方式 B：音源列表点 Spotify 右侧「扫码登录」，手机完成 OAuth 或填凭证。曲目为 320kbps Ogg Vorbis 原生直连，不走 Deemix / 网易云 |
| Deezer | 音源列表点 Deezer 右侧「扫码登录」，手机上粘贴自己的 ARL（页面教程教怎么在电脑上从 Cookie 里复制），自动识别歌单并同步到本机下载引擎解锁 FLAC；或只填资料页链接（只读公开歌单，不能下载） |
| SoundCloud | 用户名或主页链接；私密歌单在"高级"里填 `oauth_token` |
| Beatport | 邮箱 + 密码，只保存令牌 |
| Apple Music | 手机直接 MusicKit 登录；Apple 不允许时给书签脚本，在 music.apple.com 复制 `media-user-token`；不填账号时可粘贴歌单链接 |

### 3.4 只有 API、页面没有入口的能力

`GET/POST /api/prolink`（Pro DJ Link 设备快照与开关）、`GET /api/drives`、`GET /peer/decks`（影子轨道调试）、`GET /api/update`、`GET /api/sessions`。以及 Web 页面**没有**屏幕上有的这些项：Jog Light Mode、NetEase Download、7 个服务开关、`prolinkWaveform`。

## 4. 底层自动守护能力（没有开关，开机即用）

### 4.1 服务与进程

| 能力 | 载体 | 说明 |
|---|---|---|
| 五合一静态二进制 | `sc5000-suite`（多入口：`rb-bridge`、`stream-sync`、`engine-guardian`、`stems-processor`、`sc5000-deemixd`） | 为塞进固定几何的 5.0.4 rootfs，`sc5000-suite-exec` 首次从 `.xz` 解到 `<DATA>/bin`，按内容哈希刷新 |
| `stream-sync` | `stream-sync.service`，`:8080`，CPU3 | Web 控制台、流媒体网关（假装 TIDAL 的本机网关，`/au/oauth2/*`、`/v1/*`）、下载 / 分析队列、OTA、设置接口、影子轨道、Wi-Fi 传歌、重定位 |
| `sc5000-deemixd` | `127.0.0.1:6595` | Deezer 直连下载队列（无损 FLAC / 320k），Deezer 曲目和网易云「Deemix 优先」通道使用；Spotify 走原生直连，不经过它 |
| `rb-bridge` | 挂载钩子 + `rb-bridge.service --prolink` | 挂载时 2 秒内转换 Rekordbox 导出（歌单、Hot Cue、Memory Cue、Loop、拍线）；`--prolink` 只收 Pro DJ Link，不写库；`--watch` 已退役（立即退出） |
| `engine-guardian` | 挂载钩子 `--heal-mount --data-only` | 修复异常拔盘损坏的库数据；`--watch` 已退役，服务不启用 |
| `engine-ui-guard` | `engine.service` 的 pre / post / stop | 连续 3 次 15 s 内崩溃进安全模式；补写 TIDAL 账号条目（避免 5.3.0 的 "Connect to your Engine DJ Profile" 弹窗）；4 碟布局的手柄分配改写与角色解析；每次 Engine 启动后重跑 `sc5000-perf-tune`；退出原因为 Analytics 变更或出厂重置失败时拉起 Engine |
| `sc5000-debloat` | `engine.service` ExecStartPre + 开机 | 有线网口 100 → 1000 Mbps、IRQ 亲和、关闭自动擦除、遥测屏蔽、修脚本 |
| `sc5000-perf-tune` | 开机 + 每次 Engine 启动后 | CPU 1.2 GHz 起 ondemand、GPU 降频、sysctl、I/O 调度、U 盘 SD 预读 1 MiB；IRQ / 音频线程固定 CPU0，UI 在 CPU1-2，后台在 CPU3 |
| 存储 | `edisksd` 钩子 + `sc5000-storage@.service`、`sc5000-storage-discover` | exFAT / NTFS（内核自带）、HFS+（只读，只读盘上叠可写 overlay `Engine Library`）；HFS+ 绝不可写（内核 BUG 会重启并损坏盘）；去掉"未安全弹出"弹窗 |
| SSH / SFTP | `sshd` drop-in，后台 slice | 没有默认密码，密码登录关闭，公开固件不带公钥；公钥放 `<DATA>/authorized_keys` |
| 资源隔离 | `resourceguard`、`sc5000-background.slice` | 后台工作 SCHED_IDLE + nice 19 + 空闲 I/O，不碰音频核 |

### 4.2 Engine 内存钩子（`libengine-ui-hook.so`，`LD_PRELOAD`）

只对 BuildID 精确匹配的 Engine 生效，每处补丁先逐字节校验。

| 能力 | 说明 |
|---|---|
| UI 包替换 | 把 Engine 内嵌的 Qt 资源换成 `<DATA>/ui/packs/<语言>/`；设置、翻译、波形配色、流媒体、控制中心磁贴都在这里 |
| 解锁 | Stems 实时分轨与曲库 Stems 歌单（全机型）、蓝牙 / 蓝牙分配、远程 Stems 处理器、HFS+ 启用、波形缩远 30 秒、Silent Cue |
| 补丁数 | 5.3.0 一代机 16 处；AZ04 只有 6 处 |
| DSP 防爆音 | 21 个挂点：FX 开关 / 切换 128 帧淡变、NaN / Inf 清除、AutoGate 相位清零、Flanger 电平补偿（#54）、SweepFx 限幅、变速 / 跳转 / Nudge 的非法值丢弃、采样器事件、主输出软限幅（第 21 个，#58） |
| SQLite | `synchronous=NORMAL`、`temp_store=MEMORY`、`cache_size=-8000`；遥测库改 `:memory:` |
| 遥测与后台线程 | 屏蔽 PostHog / Crashpad，把分析 / 云 / Sentry 线程降为 idle |
| 官方更新器 | 屏蔽 `autoupdate.airmusictech.com` 解析，更新走 GitHub |
| ALSA | 时间戳从 GETTIMEOFDAY 升到 MONOTONIC_RAW |
| 4 碟布局 | 4 路输出里只让本机角色的两路出声，其余是静音的影子轨道（#49） |
| SC LIVE 4 引导 | 只读根盘机型的一次性 ALSA 引导（#61） |

### 4.3 联机能力

| 能力 | 说明 |
|---|---|
| Rekordbox U 盘即插即用 | 支持 FAT32 / exFAT / NTFS / HFS+ 直读（见下面 APFS 备注）；rekordbox 6.8.5 导出也能读；个别文件损坏的那几首在加载时再分析；封面迁移（`--migrate-artwork`，20 字节 hash + `Artwork/<b64url>.jpg`） |
| Pro DJ Link 读 CDJ | `prolink_share=1` 后读局域网 CDJ 的 rekordbox 库（NFS 只读）；3-Band 波形、60 fps 播放头 |
| Pro DJ Link 共享给 CDJ | `prolink_export=1`，实验功能；没有 rekordbox U 盘时保持 ON 等待，插盘后自动启动（`retryExport` 每 10 s） |
| Master 交接 | 与 CDJ 之间始终只有一个 Master，双向交接；本机当 Master 时发 BPM 和拍点供 CDJ BEAT SYNC |
| 双机 4 轨与影子轨道 | 两台 SC5000/SC6000 或一台 + 一台 CDJ 同局域网；每台负责自己的两轨，另一台的曲目、播放 / CUE、位置、速度、Master、同步、Hot Cue 在本机以静音影子轨道跟随；延迟见 `sc5000-shadow-decks` 记录 |
| Stems | 三级：云中继 → 局域网电脑（`sc5000-stemsd`，UDP 17891 自动发现，`tools/stems/run-home-worker.*`）→ 碟机本机分轨（比播放快 1.4 倍）；碟机上 `stems-processor` 代替原厂子进程 |
| 流媒体网关 | 本机伪装 TIDAL，覆盖网易云、Spotify、Deezer、SoundCloud、Beatport、Apple Music；Apple Music 音频按 ISRC / 歌名匹配到 Deezer（FLAC）或网易云；Beatport 按订阅档位取 FLAC / 256k AAC / 128k AAC；播放过的曲目缓存在 `/data`，再次加载免下载 |
| 登录中继 | `sc5000-relay`（可选自建）：让手机走移动数据登录，Deezer 网页内登录，Stems 公网入口 |
| 直播推流 | Shoutcast / Icecast |
| 曲库维护 | `dbhealer` 修复 PerformanceData、历史记录；`enginedb` 重定位、智能匹配（Camelot 兼容调性）、`analyzer` 纯 Go 分析 BPM / 拍线 / 调性 / RGB 波形 |
| 在线升级 | 一代机下载 `.img`，校验 AZ01 头尾，调用原厂 `az01-update --net`；二代机下载 `Data-Bundle` 并就地替换，64 MiB 下载上限、128 MiB 解包上限、只接受普通文件 |
| 发版门禁 | `release-gate.sh`（上一版在线升级 → 新版开机全功能验收），不属于运行时功能 |

APFS 备注：README 写"Mac 的 APFS 插上直读"。仓库里 `kernel-modules/` 会构建 `apfs.ko`，但 `rootfs-overlay/etc/modules-load.d/sc5000-filesystems.conf` 只列了 `hfsplus` 和 `overlay`；`docs/RB-BRIDGE.md` 也说"APFS 驱动在本工具范围外"。**APFS 是否真能直读，待实机核实**。

### 4.4 辅助工具（不进固件）

`az01_packer.py`（解包 / 重打包 / patch-rootfs）、`build-release.sh`、`build-data-bundle.py`、`host-install-userdata.py`、`host-install-fastboot.sh`、`rootfs_audit.py`、`inspect_fit.py`、`probe_performance.py`、`unlock_jp07_features.py`、`tools/ui-mod`（`build_packs.py`、`extract_qrc.py`、`qrc_dump.c`、`engine_ui_hook.c`、`dsp_smooth.h`、`master_limiter.h`）、`tools/edisksd-hook`、`tools/stems`（家用 worker）、`tools/drmgrab`（DRM 帧缓冲截屏）、`cmd/stems-probe`、`sim/qengine`。

## 5. UI 规范化审查：发现

### 5.1 结构问题

1. **16 项 EngineX 条目平铺在"设备"页**，夹在原厂条目里，没有分组、没有 EngineX 标识。音质、音频保护、联机、4 碟、直播、关机全混在一个长列表里；用户要找"防爆音"得翻到 Device 页中段。
2. **同类设置分散在不同页**：Streaming Quality / NetEase Download 在设备页，而 7 个服务开关在 服务 → 流媒体；两个限幅（原厂 Master Limiter 在混音台，EngineX 软限幅在设备页）；Upfader Echo 在设备页、混音台、控制中心三处。
3. **Live Stream 开关没有前置条件提示**：屏幕上能点开，但服务器地址和密码只能在 Web 控制台填；没配置就开会静默失败。
4. **有设置但没有入口**：`prolink_waveform`（孤儿翻译）、`github_repo`。
5. **有功能但没有可见状态**：安全模式只有 Web 页面能看出来，屏幕上回原厂界面后用户不知道发生了什么；Stems 当前走云 / 局域网 / 本机、Pro DJ Link 连上了哪些设备，两处都没显示。
6. **Web 控制台缺屏幕上有的项**：Jog Light Mode、NetEase Download、7 个服务开关。反过来 OTA 在 Web 没有，也没有日志 / 诊断页——出了问题用户没有地方导出信息。

### 5.2 命名与措辞不一致

| 问题 | 屏幕 | Web / README | 建议 |
|---|---|---|---|
| 4 碟布局的叫法 | `4-Deck Waveform` / 四路波形显示 | `Native 4-deck layout` / 原厂 4 碟布局；README 写"原生四轨布局" | 这个开关改的是布局（Layout → 4 Decks），不只是波形。统一成 **4-Deck Layout / 4 碟布局 / 4 碟佈局** |
| 4 碟角色的叫法 | `4-Deck Role` / 四轨角色 | `4-deck role` / 4 碟分工 | 统一 **4-Deck Role / 4 碟角色 / 4 碟角色** |
| "轨"与 Stems 撞词 | 四轨角色、README 四轨布局 | Stems 也叫"4 轨分轨" | 4 碟相关一律用"碟 / 台"，"轨"留给 Stems |
| 测试版标记 | 英文 "(Beta)"，中文无 | Web 标签"实验性"；README"测试版" | 统一 **测试版 / 測試版 / Beta**，Pro DJ Link 曲库也算（README 称测试版，屏幕无标记） |
| Pro DJ Link 共享的名称 | 英文 `Export to CDJ (Beta)`，**中文缺译** | `Pro DJ Link 共享本机 U 盘给 CDJ` | 补 `i18n.tsv`；中文统一 **Pro DJ Link 共享给 CDJ（测试版）** |
| 自动增益的说明 | 条目名带 "(0 dB Normalize)" | 说明写"把每首歌峰值拉到 0 dB" | 说明不准确：最多 +9 dB，低于 -42 dBFS 静音不提，总是过软限幅。改说明 |
| 软限幅与原厂限幅 | `Master Soft Clipper` / `Master Limiter` | 中文"0dB 防破音软限幅"与"原厂主输出限幅 (dB)" | 两者并存，用户分不清。放进同一分组，中文加前缀：**EngineX 防破音软限幅** vs **原厂主输出限幅 (dB)**，并加一行说明 |
| 4 碟说明有错 | — | `The hardware plays decks 1 and 3 (LAYER), decks 3 and 4 are silent`；中文"3、4 号台不出声" | **错**：静音的应是 2、4。且这只对 1+3 角色成立，角色是 2+4 时相反。改成"本机播放角色负责的两台，其余两台只显示、不出声" |
| 登录页标题 | — | `Stream Sync`，而主页标题是 `EngineX OS · Console` | 统一为 `EngineX OS · Console` |
| 同义词 | U 盘 / 隨身碟、碟机 / 碟機 | 一致 | 无需改，已符合简繁用语 |

### 5.3 规范化分类方案

目标：原厂条目不动，EngineX 条目按用途分到 6 组，每组有固定位置，屏幕与 Web 用同一套组名和顺序。

| 组 | 组名（英 / 简 / 繁） | 条目 | 屏幕位置 | Web 位置 |
|---|---|---|---|---|
| A | Display / 显示 / 顯示 | Language、Waveform Style、Jog Light Mode | 设置 → 设备（置顶，EngineX 区块） | "碟机显示与音频"卡片的"显示"小节 |
| B | Streaming / 流媒体 / 串流 | 7 个服务开关、Streaming Quality、NetEase Download、OmniSource | 设置 → 服务 → 流媒体（全部集中到这一页） | "流媒体"卡片 + 补上服务开关与 NetEase Download |
| C | Audio Protection / 音频保护 / 音訊保護 | Master Soft Clipper、Auto-Gain、原厂 Master Limiter（不动）、Upfader Echo Out | 设置 → 混音台（EngineX 软限幅与自动增益从设备页移来；Upfader Echo 只留这里和控制中心磁贴） | "音频"小节 |
| D | Library / 曲库 / 曲庫 | Auto-Analyze Drives | 设置 → 曲库（Library） | "曲库"小节 |
| E | Network & CDJ / 网络与 CDJ / 網路與 CDJ | Pro DJ Link Library（测试版）、Pro DJ Link 共享给 CDJ（测试版）、Pro DJ Link 波形 | 设置 → 网络（Network） | "联机"小节 |
| F | Layout / 布局 / 版面 | 4-Deck Layout、4-Deck Role | 设置 → 设备，紧挨 Player # | "4 碟"小节 |
| G | System / 系统 / 系統 | Live Stream（配置未填时置灰并提示"先在 Web 控制台填写服务器"）、Power → Turn Off | 设置 → 设备末尾 | "直播推流"折叠卡片保留 |

控制中心磁贴保留 4 个（Waveform Style、Live Stream、Upfader Echo Out、Smart Sync），它们是演出中要快速点的，不动。

约束：改动只重排 / 改名，不改 conf 键名和接口参数名，所以旧版本和 Web 的调用不会断；`/v1/enginex/*` 与 `/v1/sc5000/*` 两套别名参数保持一致（#103）。

## 6. 精确改动补丁建议（供各负责人落地，本任务没改任何文件）

### 6.1 `tools/ui-mod/i18n.tsv`（追加）

```
Pro DJ Link Export to CDJ (Beta)	Pro DJ Link 共享给 CDJ（测试版）	Pro DJ Link 分享給 CDJ（測試版）
4-Deck Layout	4 碟布局	4 碟版面
4-Deck Role	4 碟角色	4 碟角色
EngineX Soft Limiter (0 dBFS)	EngineX 防破音软限幅	EngineX 防破音軟限幅
```

并把现有 `4-Deck Waveform`（四路波形显示）、`4-Deck Role`（四轨角色）两行改掉，保留旧英文键一个版本以防旧 UI 包回退。（审查脚本里报缺译的 `Denon`、`RGB`、`3-Band`、`320kbps`、语言自称和 `WAITING FOR ` 末尾空格，都是专有名词或解析误报，不用补。）

### 6.2 `tools/ui-mod/build_packs.py`

- `EXTRA_DEVICE_ENTRIES`：`text: "4-Deck Waveform"` → `"4-Deck Layout"`。
- `Live Stream` 条目加 `enabled: root.scBroadcastConfigured`（需要在 `/v1/sc5000/ui` 响应里补一个只读 `broadcastConfigured`，由 `broadcast-config.json` 是否有 URL 决定），并在未配置时用 `root.scT("Set up in the web console first")` 作副标题。
- 混音台页的 Upfader Echo Out 补 `visible: !root.scPlayer`（与设备页、磁贴一致）。设备页那份删除，只留混音台和磁贴。
- `JOG_ENTRY` 拆开：Jog Light Mode 留设备页；Master Soft Clipper、Auto-Gain 移到 `MixerTabContent.qml` 注入块；Auto-Analyze Drives 移到曲库页；两个 Pro DJ Link 条目移到网络页。（一代 5.0.4 与 5.3.0 的页面文件路径不同，沿用 `v53()` 分支。曲库页、网络页是按 `i18n.tsv` 里的标签页名推断的，动手前先在 `extract_qrc.py` 解出的 QML 里确认 `Preferences/` 下有对应的 TabContent 文件。）
- `QUALITY_ENTRY`（Streaming Quality、NetEase Download）和 OmniSource 移到 `Entries/StreamingServices.qml` 注入块，靠近 `STREAM_SWITCHES`。

### 6.3 `pkg/webui/index.html`（`T` 文案表与结构）

```
tFourDeck: ['4-Deck Layout', '4 碟布局', '4 碟版面']
sFourDeck: ['Adds Layout → 4 Decks. This player plays the two decks of its role; the other two only follow and stay silent. Restarts Engine',
            '在 Layout 里加入 4 Decks。本机只播放角色负责的两台，另外两台只显示、不出声。切换会重启 Engine',
            '在 Layout 裡加入 4 Decks。本機只播放角色負責的兩台，另外兩台只顯示、不出聲。切換會重啟 Engine']
tRole:     ['4-Deck Role', '4 碟角色', '4 碟角色']
sGain:     ['Lifts quiet tracks toward 0 dBFS (up to +9 dB), then always through the soft limiter. Silence below -42 dBFS is left alone',
            '把偏小的歌平滑提到接近 0 dBFS（最多 +9 dB），之后总会过软限幅；低于 -42 dBFS 的静音不提升',
            '把偏小的歌平滑提到接近 0 dBFS（最多 +9 dB），之後總會過軟限幅；低於 -42 dBFS 的靜音不提升']
tExport:   [... , 'Pro DJ Link 共享给 CDJ', 'Pro DJ Link 分享給 CDJ']
beta:      ['Beta', '测试版', '測試版']
```

结构上：把"碟机显示与音频"12 项按 5.3 的 A、C、D、E、F 分成小节标题；补 Jog Light Mode、NetEase Download 和 7 个服务开关（`sources`、`jog`、`neteaseMode` 接口本来就有，只缺前端控件）；`prolinkWaveform` 补一个开关（接口已支持）。

### 6.4 `pkg/webui/server.go`

`consoleLoginPage` 的 `<title>Stream Sync</title>` 和 `<h1>` → `EngineX OS · Console`。

### 6.5 新增（不在本任务，列为建议）

- Web 控制台加"诊断"卡片：版本、机型、`safeMode`、四个守护进程是否存活、`journalctl` 最近 200 行（脱敏）、一键导出诊断包。目前出了问题没有任何入口。
- Web 控制台加"更新"卡片：复用 `GET /api/update`，在已登录的控制台里显示新版本，点更新仍然只调回环接口，避免把 OTA 触发权暴露给局域网。
- 屏幕 设置 → 关于 加一行"安全模式：是 / 否"和"Stems 通道：云 / 局域网 / 本机"。

## 7. 路径迁移对本文涉及项的影响（#103）

| 项 | 现路径 | 目标 |
|---|---|---|
| 运行目录 | `/data/sc5000-custom/` | `/data/enginex/`；旧路径保留软链接 |
| UI 包与设置 | `<DATA>/ui/{packs,icons,settings.conf,safe-mode,boot-count}` | 随运行目录迁移 |
| 状态与密钥 | `<DATA>/state/{access-token,broadcast-config.json,spotify-token.json,gateway-tracks.json}`、`authorized_keys`、`ssh_host_ed25519_key` | 随运行目录；权限（0600）、软链接拒绝逻辑要保留 |
| 数据包 | `<DATA>/bundle/`、`<DATA>/version.txt` | `uninstall.sh` 里的 `SC5000_CUSTOM` 默认值要同步改 |
| 配置目录 | `/etc/sc5000/`（`engine-ui-guard`、两个 `.so`、`version.txt`） | 增加 `/etc/enginex` 别名 |
| 本机接口 | `/v1/sc5000/*` | 增加 `/v1/enginex/*`，`index.html` 与 `build_packs.py` 里写死的 `http://127.0.0.1:8080/v1/sc5000/...` 逐步切到新路径 |

迁移要避免的坑：`engine-ui-guard`、`edisksd` 钩子、`rb-bridge`、发版门禁（`release-gate.sh`、`rootfs_audit.py`）、`tests/test_rootfs_overlay.sh` 都硬编码旧路径；开机迁移要放在 `sc5000-custom-prepare` 里、早于 Engine 和 stream-sync 启动；二代机数据包的 `boot-activate.sh` 与 `uninstall.sh` 在旧版本 bundle 上要同时识别两条路径。

## 8. 附：`settings.conf` 键全表

`lang`、`waveform`、`sources`、`known`、`scope`、`quality`、`jog`、`netease_mode`、`playhead`、`four_deck_waveform`、`four_deck_role`、`prolink_waveform`（默认开，仅 `=0` 写入）、`upfader_echo`、`auto_analyze`、`master_limiter`、`auto_gain`、`omni_source`、`prolink_share`、`prolink_export`、`github_repo`。布尔项只在为开时写入 `=1`（`prolink_waveform` 相反，只在为关时写入 `=0`），所以"文件里没有"等于默认值。

只读状态字段（`/v1/sc5000/ui` 与 `/api/ui` 返回，不写入）：`version`、`safeMode`、`loggedIn`、`broadcastEnabled`、`relayUrl`、`deemixPasswordSet`、`prolinkExportActive`、`player`、`fourDeckRoleActive`；仅回环接口额外返回 `accessCode`、`consoleUrl`。
