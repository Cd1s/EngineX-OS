<div align="center">

<img src="./docs/assets/hero-banner-zh-CN.svg" alt="EngineX OS 5.3.0.4" width="100%">

<br>

<a href="./README.md"><img src="https://img.shields.io/badge/English-30363d?style=for-the-badge" alt="English"></a>
<a href="./README.zh-CN.md"><img src="https://img.shields.io/badge/%E7%AE%80%E4%BD%93%E4%B8%AD%E6%96%87-00e676?style=for-the-badge" alt="简体中文"></a>
<a href="./README.zh-TW.md"><img src="https://img.shields.io/badge/%E7%B9%81%E9%AB%94%E4%B8%AD%E6%96%87-30363d?style=for-the-badge" alt="繁體中文"></a>

<br>

**[核心功能](#features)** · **[固件下载](#install)** · **[实机画面](#gallery)** · **[重构与占用对比](#benchmarks)** · **[赞赏支持](#sponsor)**

📖 **在线使用文档**：<https://enginexdj.com>

</div>

<br>

**EngineX OS 5.3.0.4** 是基于官方 Engine OS 5.3.0+ 底层重构的定制系统，支持天龙 Denon DJ、Numark 与 Rane 全系 16 款独立设备：
`SC5000` · `SC5000M` · `SC6000` · `SC6000M` · `PRIME 4` · `PRIME 2` · `PRIME GO` · `Mixstream Pro` · `SC LIVE 4` · `SC LIVE 2` · `PRIME 4+` · `PRIME GO+` · `Mixstream Pro+` · `Mixstream Pro Go` · `PRIME 4 G2` · `Rane SYSTEM ONE`。

前八款用 U 盘刷 `.img`；后八款二代机的升级程序会在写入前用软件校验固件签名，改用 `/data` 数据包（Beta，见[如何安装与升级](#install)）。

操作界面与原厂保持一致，直接通过 U 盘刷入即可解锁以下功能。

---

<a id="features"></a>

## 核心功能解锁

### 1. 全机型解锁 4 轨 Stems 实时分轨（人声 / 旋律 / 贝斯 / 鼓组）
突破原厂机型限制，在 **SC5000 / SC6000 / PRIME 全系**上开启曲库侧边栏 `Stems` 歌单与打击垫四轨独立静音：
- 选歌时向左滑动即可拆出 **Vocal（人声）、Melody（旋律）、Bass（贝斯）、Drums（鼓组）** 四轨；
- 打碟时直接按打击垫一键抽掉人声做 Acapella，或单留鼓点混音；
- 不连电脑可直接由碟机本机完成分轨；同一局域网下打开家用电脑或连接云端服务器时，自动调用显卡跑高速 AI 分轨。

### 2. 内置 7 大流媒体平台（手机扫码直接登录）
无需每次演出前把新歌一首首拷进 U 盘：
- 音源菜单直接内置 **网易云音乐、Spotify、Deezer、SoundCloud、TIDAL、Beatport、Apple Music**；
- 手机扫描碟机屏幕上的二维码即可登录个人账号，直接浏览红心歌单、收藏夹并在线搜歌；
- 支持 **无损 FLAC / 320k MP3** 音质切换；播放过的曲目自动缓存在碟机内部存储，再次加载直接秒开，绝不重复下载。
- **各平台独立缓存与 Deemix 智能共享**：Spotify（`spotify-<id>.*`）、Beatport（`beatport-<id>.*`）、SoundCloud（`soundcloud-<id>.*`）以及「官方直连（Official）」模式的网易云音乐（`netease-<id>.*`）各自使用独立的下载通道和专属缓存命名空间；即使同一首歌曾在 Deezer 或网易云下载过，在 Spotify 点播也会走 Spotify 自己的流程重新拉取并独立缓存，互不混用。网易云开启「Deemix 优先」时，底层与 Deezer 同走 Deemix 无损音源，在网易云点播过的歌切到 Deezer 点播同一首（或反之）会直接复用已下载的无损缓存，不重复下载；网易云切回「官方直连」后则与 Deezer 各自独立缓存。
- **SoundCloud Go+ 与非 Set 单曲高清智能路由**：DJ Set / 长混音（超过 10 分钟）始终直连 SoundCloud 官方流，优先取上传者原始 320k / WAV 母带，其次 256k / 160k AAC 或渐进式 MP3。Go+ 专享曲目和非 Set 的普通单曲：已绑定 **SoundCloud Go+** 账号时优先直连官方高音质流；没有 Go+ 时（Go+ 专享曲目官方只给 30 秒试听），自动在本机已登录的其他流媒体（Deezer / 网易云 / Beatport）里匹配并拉取无损 / 320k 音频，缓存为 `soundcloud-<id>.*` 上盘播放；其他流媒体都没登录、或曲目是 SoundCloud 独占作品时直连 SoundCloud 播放，遇到必须 Go+ 的试听曲目又没有任何可用平台，碟机会明确提示扫码登录 SoundCloud Go+ 或其他流媒体账号。
- **Beatport** 使用你自己的账号与订阅：Professional 下载 FLAC 或 256k AAC，Advanced 下载 128k AAC，Essential 在线播放 128k AAC 流；Beatport 曲目同样可以一键拆 Stems。
- **Apple Music**：在手机上粘贴任意歌单或专辑链接即可，不需要账号；再填入 `media-user-token` 就能浏览自己的资料库。播放走 Engine 5.3.0 自带的 Apple Music 音源（EngineX 负责解锁），不再匹配 Deezer 或网易云。

### 3. 先锋 Rekordbox U 盘即插即用（免电脑转换）
平时给先锋 CDJ 导出的 Rekordbox U 盘，直接插上即可上台打碟：
- 插盘 **2 秒内**直接读取全部歌单、Hot Cue、Memory Cue、Loop 循环点与拍线，无需再用电脑端 Engine DJ 重新导库；
- 兼容全格式存储设备：Windows 的 `FAT32` / `exFAT` / `NTFS` 与 Mac 的 `HFS+` / `APFS` 插上直读；
- 自动修复异常拔盘造成的数据库损坏，并去除了“未安全弹出”报错弹窗。

### 4. 完整中文界面与先锋风格 3-Band 波形
- **三语界面**：设置、菜单与提示全面支持 **简体中文 / 繁體中文 / English**（全系 16 款）；
- **3 种波形配色**：在原厂绿波形基础上，新增高对比度 **RGB 波形** 与先锋风格 **3-Band（三频段蓝/橙/白）波形**，可在控制中心 `Waveform Style` 磁贴或 `个人偏好 → Display` 一键切换并双向同步（全系 16 款）。

### 5. 切效果器零爆音，长时间演出更稳定
- 消除原厂固件在开关或切换效果器（FX）时偶发的“咔嗒”爆音，双轨同时开效果器也有平滑淡入过渡；
- 默认关闭原厂后台匿名统计上报与无用磁盘缓存，把全部算力和内存留给音频播放与屏幕触控。

### 6. 智能同步与千兆有线网口
- **智能同步（Smart Sync）**：下拉控制中心一键切换 **Bar / Beat / Tempo** 同步模式；开启 Sync 后，两首曲目 BPM 跨度超出当前变速范围时，自动把变速范围（Pitch Range）升档到够用为止（最高 `±100%`）；
- **千兆有线网口**：有线网口从原厂限制的 `100 Mbps` 解锁至 **1 Gbps（1000 Mbps）全双工满速**，并配 8 MiB TCP 大窗口缓冲；
- **Wi-Fi**：碟机内置 Wi-Fi 模组受限于主板 SDIO 25 MHz SDR12 总线带宽，实测极限约 `9～10 MB/s`，大体积曲库传输建议使用千兆有线网口。


### 7. 5.3.0.4 新功能
- **四轨联动完整卸载与流媒体真实歌名（#49）**：对端碟机关机、重启或退歌时会发送 UDP 告别包，本机在 350 ms 防抖后自动卸载影子轨（`Unload` + `UnSync`，并自动交回 `MASTER`），清掉残留的波形和曲目。移除了可能死锁的 `RemoteState` 订阅，对端关机不再阻塞本机切换流媒体音源。流媒体曲目在影子碟机上显示真实的 `歌手 - 歌名`，不再显示内部数字缓存文件名；
- **界面更丝滑、降低 CPU 占用**：新增 `scTr` QML 属性 translator 缓存，并给 `QuickCue` 加 500 ms 节流，消除按 `MASTER` / `SYNC` 时界面的瞬时卡顿，明显降低 Engine `EMain` GUI 线程的 CPU 占用；
- **歌单左滑只留一个 Stems 按钮**：自动识别原厂已带 `Stem` 动作的本地曲目，去重并本地化为单一的 `分轨 Stems` 按钮，本地、流媒体、局域网曲目左滑后的体验完全一致；
- **Auto Loop 旋钮联动与节拍跳跃互换修复（#135）**：直接转动 Auto Loop 旋钮调整 Loop 大小，并自动开启 `LinkBeatJumpLoopSize` 让节拍跳跃（Beat Jump）大小跟随联动；修复了 JP07 开启 `互换循环 / 节拍跳跃`（`swap_loop_order`）时 `beatJumpEnabled` 的判定；
- **OmniSource 歌单隔离（#105）**：开启 OmniSource（跨源搜索）后，搜索栏仍可同时检索各流媒体和全部 U 盘曲库，但各流媒体自己的歌单页不再混入 `[USB: ...]` 条目和其他平台的歌单；
- **SoundCloud 点赞歌单回退（#150）**：用户 OAuth `refresh_token` 过期、SoundCloud 返回 `HTTP 400` 时，自动回退到 App Token 和公开主页接口，继续加载 `Liked Tracks & Sets`；
- **Spotify Connect 局域网发现（#150）**：内置 Avahi `_spotify-connect._tcp` mDNS 广播，`stream-sync` 另带纯 Go 的 `_spotify-connect._tcp.local.` mDNS 广播器（有线 `eth0` + 无线 `wlan0` 双网卡也能被发现），并使用标准 768-bit Diffie-Hellman 公钥，手机 Spotify App 可在局域网里直接发现 `EngineX OS`；同时修复 OAuth 登录后曲目下载返回 `410` 的问题；
- **流媒体缓存隔离与 Deemix 智能共享**：Spotify、Beatport、SoundCloud 与网易云官方模式各用独立下载通道与缓存命名空间；网易云「Deemix 优先」时与 Deezer 共享无损缓存，互相点播同一首不重复下载；
- **SoundCloud Go+ 与高清智能路由**：长混音 / DJ Set（超过 10 分钟）始终直连 SoundCloud 官方流；非 Set 普通单曲与 Go+ 专享曲目：已绑定 Go+ 优先官方高音质流，未绑定时自动在已登录的其他流媒体（Deezer / 网易云 / Beatport）匹配无损 / 320k 音频，避免 30 秒试听限制；
- **品牌统一与免域名公网连接**：面向用户的组件统一为 `enginex-relay`（原 `sc5000-relay`）、`enginex-stemsd`（原 `sc5000-stemsd`）、`enginex-suite`，运行目录为 `/data/enginex`（保留 `/data/sc5000-custom` 兼容软链接）；支持 Cloudflare Quick Tunnel 免域名一键公网穿透，手机扫码即可远程登录流媒体；中继控制台新增 `已连接碟机集群（Multi-Deck Hub）` 卡片，汇总同一后端下所有碟机的在线状态、机位角色（`13` / `24`）、固件版本和流媒体登录状态，可用碟机访问码或主密码登录；各碟机 `设置 → 关于` 页会实时显示 Stems 算力节点状态；
- **Mixstream Pro / Pro+ / Pro Go（#164、#166）**：实体 FILTER 旋钮控制原厂 Filter / Dub Echo / Noise / Filter Roll / Wash，点击各轨 Pads 栏的 `SWEEP FX` 打开原厂选择轮盘；恢复触屏 Beat Jump，在 `布局 → View` 开启 `Beat Jump Pad Bank` 即可后跳、前跳、减半、加倍；各轨 `EQ / STEMS` 按钮切换通道三段旋钮 Stems EQ；安装器支持严格 EAAS 协议发现二代机并消除端口冲突，兼容 fastboot 35+，完善 Windows WSL 2 + usbipd-win 刷机指引，已在 NH08S 实机完整刷入验证；识别 Mixstream Pro Go（`15e4:2059`）USB 设备；
- **一代机电脑线刷与免 debugfs 解包**：修复 SC5000 / SC6000 / PRIME 4 等一代机在固件更新模式下 fastboot product 缺失时的 USB ID 识别，恢复电脑线刷支持；打包工具在 macOS 与 Windows 上无 debugfs 时也能正常解包镜像；
- **PRIME 4 硬件 FX 选择器（#168）与 PRIME 4+ 激活校验（#169）**：重整 JC11 硬件效果选择器，修复切效果器重置与 Wet/Dry 淡入淡出爆音；强化 PRIME 4+（JC11S）激活文件与数据目录校验；修复 Beatport 无损升级在冷缓存下的命中，并在 `stream-sync` 占用端口时让 `deemixd` 优雅退出。
- **SC LIVE 4 AUX（#171，新功能，待实机验证）**：在原厂 `混音台菜单 → 通道 → 输入` 中把 AUX 送入通道 3 或 4，使用通道 CUE、推子、EQ 和 Sweep FX；仅在 AUX 模式且任一通道选择线路输入时屏蔽 AUX 直通，MIC2 保持原厂路由。两路共享一个 AUX 源，同时推起会叠加。Mixer Menu（Beta）会迁移部分偏好设置，并允许向线路输入通道载歌；JP21 隐藏未验证的 Bluetooth/Computer 选项。详见[路由说明与实机检查](docs/issue171-sclive4-aux.md)；
- **PRIME 4 等一体机连 CDJ-3000 波形（新功能，待实机验证）**：在 PRIME 4 等一体机上把混音台通道切至线路输入（Line In）后，Pro DJ Link 连接的 CDJ-3000 波形会显示在该通道上（按 CDJ 播放机编号从小到大依次对应 Line In 通道从小到大）；SC5000 / SC6000 独立播放机行为不变；
- **开机与关机画面全屏铺满（#175）**：开启界面缩放（UI Scale，非 100% 如 75%）时，开机启动画面与关机遮罩现补齐全屏锚定，在所有缩放比例下均完整铺满屏幕，消除黑边（覆盖 SC LIVE 4 等全系机型）；
- **影子轨细波形显示修复**：暂停状态下刚加载的影子轨不再出现空白（解决切细波形时的渲染时序问题）；细波形切片尚未下载完成时，滚动波形泳道不再将整首歌的概览图拉伸挤入小波形，而是保持干净的拍线与指针显示，就绪后平滑渲染；
- **Mod 安装保留可执行权限（#170）**：Web 控制台安装 Mod 压缩包（`.tar.gz`）时，保留 tar 包中文件的可执行权限位（保留 `0111`，安全过滤 setuid/setgid/sticky 位），自带二进制可执行文件（ELF）或启动程序的 Mod 安装后可正常启动运行；
- **SC5000 / SC6000 开启四轨后保持原厂播放器行为与图层颜色（#49）**：在独立播放器（`SC5000 / SC5000M / SC6000 / SC6000M`）上开启 `控制中心 → 布局 → 四轨波形显示`（`4-Deck Waveform`）后，完整保持原厂播放器行为——各轨颜色按 `设置 → 设备 → 碟机编号 #`（`Player #`）与 `图层（Layer A / B）` 自动映射 `个人偏好 → 显示 → 碟机颜色`（保留 `Layer A / B` 双层选色盘），在曲库按 `Browse` 浏览旋钮直接加载到本机当前层（`Layer A` 或 `Layer B`），并恢复 `设置 → 设备 → 碟机编号 #`（`Player #`）、`个人偏好 → 播放 → 启用双层播放`（`Enable for Dual Deck`，即 `Layer B` 开关）、远程碟机选择、`Engine Connect` 与 `触控效果 (Touch FX)` 的 `Mute` 静音按钮；
- **`全部 CDJ 模式` 与 CDJ 回退时影子轨头部对齐原厂样式（#49）**：当 `控制中心 → 布局 → ENGINEX 视图预设 → 影子轨模式`（`Shadow Deck Mode`）设为 `全部 CDJ 模式`（`All CDJ Mode`），或在 `自动`（`Auto`）模式下遇到无法拷贝音频的 CDJ 曲目回退至显示模式（以及一体机 `Line In` 通道显示 CDJ 波形）时，影子轨头部改用原厂样式：专辑封面（`显示专辑封面`）、滚动歌名与歌手（`隐藏歌名/歌手`）、剩余 / 已播时间（点击可切换）、调性及调性色与锁调 `Key Lock` 图标（先锋称 `Master Tempo`）、开启 `同步`（`Sync`，先锋称 `Beat Sync`）时变绿的大号 `BPM`、`变速范围`（`Pitch Range`，先锋称 `Tempo Range`）与变速百分比（`pitch`）、软接管箭头 `▲▼`、`主控 (LEAD)` 指示（先锋称 `MASTER`）、`循环 (Loop)` 与 `节拍跳跃 (Beat Jump)`、`热标拍数倒数`（`Hot Cue Countdown`）以及曲末 30 秒红闪提醒（`Track End Warning`）。

### 8. 5.3.0.3 新功能
- **Tailscale / Headscale 远程访问（#130）**：Web 控制台的 `远程访问（Tailscale / Headscale）` 卡片按需下载精简版 Tailscale 客户端（约 4.3 MB，下载后校验 SHA-256），用登录链接或一次性 Auth key 登录，只把你勾选的服务（SSH / SFTP、Web 控制台）开放给你自己的 tailnet。不需要公网 IP，也不用改路由器，自建 Headscale 服务器同样可用。默认关闭。碟机内核没有 TUN 设备，所以以用户态模式运行。已用 Headscale 端到端测通；实机播放时的 CPU 和内存占用还没有实测；
- **流媒体歌单排序（#98）**：所有流媒体服务都能打开 Engine 自带的排序菜单，按 BPM、调性、评级、歌名或歌手升序 / 降序排列；
- **流媒体曲目可录音（#84）**：加载流媒体曲目时，Engine 的录音功能不再拒绝录制。录音请仅供个人使用，并遵守流媒体服务的条款；
- **曲目列表显示 BPM（#80）**：`布局 → View → 双栏浏览` 打开后，曲目列表的第二列旁边会显示 BPM；
- **推子回音与智能同步（#104）**：在带混音台的一体机（`PRIME 4 / 4+ / 4 G2`、`PRIME 2`、`PRIME GO / GO+`、`SC LIVE 4 / 2`、`Mixstream Pro / Pro+ / Pro Go`、`SYSTEM ONE`）上，控制中心保留原厂 `Fader Echo` 磁贴（不重复添加自定义磁贴），并与 `设置 → 混音台 → Fader Echo`（`Off` / `All Faders` / `Upfader Only`）和 `个人偏好 → Playback → Echo Out` 双向同步（独立播放器 `SC5000 / SC5000M / SC6000 / SC6000M` 自动隐藏）。开启 Sync 时，智能同步（全系 16 款）还会尝试半速 / 倍速（0.25×–4×）匹配并按需自动升档变速范围（Pitch Range）；
- **控制中心演出磁贴排布**：包含 `Waveform Style` 与 `Smart Sync`（全系 16 款），带 Transition FX 的一体机保留原厂 `Fader Echo` 磁贴；`Live Stream` 移至 `设置 → 服务`；`Waveform Style` 在控制中心与 `个人偏好 → Display` 双向同步，不在 `布局 → View` 重复显示；
- **先锋风格 Beat FX 与 Sound Color FX（#88）**：JC11 PRIME 4（Engine 5.3.0）可在列表末尾选择 `Spiral (PX)`、`Pitch Echo (PX)`、`Slip Roll (PX)`、`Dub Echo (PX)`、`Space (PX)`，按 FX 单元及其分配通道生效。Parameter 可调时间、音色与反馈，Wet/Dry 可调干湿比，原厂效果继续可选；独立 Time/Rate 旋钮暂未接入。AZ04 保留原来的 `Color FX Style` 设置；PRIME 4 实机验证待完成； JP07 SC5000 没有原生 FX 选择器，保留全局 `Color FX Style` 菜单、master 效果和保存的设置。
- **界面缩放（110%–75%）、曲目列表波形开关与打击垫亮度（#153、#117、#85）**：`布局 → View → 界面缩放` 可把 Engine 整个界面在 110% 到 75% 之间无黑边缩放，控制中心和弹窗仍然铺满屏幕；`个人偏好（User Profile）→ 曲库 → 曲目列表波形` 关闭后可隐藏列表里的缩略波形，让歌名与艺人完整展开；`设置 → 设备 → 打击垫亮度`（`100%`–`20%`）：RGB 灯（打击垫、模式键、Layer、转盘灯环、屏幕上的 pad）跟随亮度；`SYNC`、`KEYLOCK`、`SLIP` 等单色键因为硬件只有亮/暗两档，保持原厂亮度；
- **实体三段 EQ 旋钮 Stems 分轨控制与快捷键（#152）**：在带混音台及 Stems EQ 的一体机（`PRIME 4 / 4+`、`PRIME 2`、`PRIME GO / GO+`、`SC LIVE 4 / 2`、`Mixstream Pro / Pro+ / Pro Go`、`SYSTEM ONE`；独立播放器与 `PRIME 4 G2` 自动隐藏）上，通过 `个人偏好 → 播放 → Stems EQ Mode` 或按各通道 **Shift + Cue (PFL)**，即可将实体 `HIGH / MID / LOW` 旋钮切换为 **Stems Level**（`Vocal 人声`、`Melody + Bass 旋律+贝斯`、`Drums 鼓组`），内置原厂软接管（Soft Takeover）；修复 PRIME 2 / PRIME 4 快速双击 `Hot Cue` 时 Stems 模式闪退回 Hot Cue 的问题；
- **播放中按住浏览/缩放旋钮转动转盘防误入网格编辑（#157）**：曲目播放中按住 Browse/Zoom 旋钮（或 Slip/Vinyl）不再误触进入 BeatGrid Edit（此前会导致瞬间跳回 `00:00` 且 Play 键锁死），网格编辑激活时禁用转盘触摸；
- **PRIME 4 / PRIME 4+ 与 SC5000 / SC6000 局域网跨机型四轨联动（#160）**：仅在 4 轨机型（`SC5000 / SC5000M / SC6000 / SC6000M`、`PRIME 4 / PRIME 4+ / PRIME 4 G2`、`SC LIVE 4`；2 轨一体机自动隐藏）开放 `四轨波形显示`、`四轨角色`（`自动`、`1+3`、`2+4`、`1+2`、`3+4`、`1`、`2`）与 `影子轨模式`；`互换循环 / 节拍跳跃` 对带 Auto Loop 旋钮的全系机型开放（`Mixstream Pro` 系列自动隐藏），`Beat Jump 打击垫组` 仅对 `Mixstream Pro / Pro+ / Pro Go` 显示；
- **影子轨模式（待实机验证）**：四轨布局下，只要对端曲目可拷贝，影子轨均由本机静音加载，用与本机轨道相同的原厂组件绘制（与本机显示一模一样）：EngineX 碟机的曲目从对端拷贝，CDJ / Pro DJ Link 设备的曲目从 CDJ 的 U 盘或 SD 卡拷贝。无法拷贝文件的 CDJ 曲目（流媒体、rekordbox Link、CD 或第二 USB、拷贝失败或超时）显示 CDJ 波形，并用一行文字说明原因。`控制中心 → 布局 → ENGINEX 视图预设 → 影子轨模式`：`自动`（所有机型默认；一体机 Line In 显示 CDJ-3000 波形照旧）、`全部 Engine 模式`（所有对端曲目均由本机加载）、`全部 CDJ 模式`（不加载对端音频，一律显示 CDJ 式波形）。
- **CDJ 显示式影子波形**：影子轨显示 CDJ 曲目已缓存的 rekordbox 波形、拍线与 Cue，不下载音频；没有分析数据时只显示元数据。PRIME 4 / PRIME 4+ / PRIME 4 G2、SC LIVE 4 开启 `4-Deck Waveform` 后，3、4 轨显示影子波形，保留本机 1、2 轨；没有对端的轨道仍可本机使用。
  自定义 rb-bridge `--media-cache` 时，WebUI 服务环境中的 `SC5000_PROLINK_MEDIA_CACHE` 必须指向同一目录；两者默认共用目录。关闭 `prolink_share` 也会停止读取显示缓存。
- **修复流媒体下载后卡加载与“已从 Engine DJ 账号断开”报错（#159）**：为本地流媒体网关注入非 SSO 令牌并跳过 150 秒 Engine DJ Profile 等待，彻底解决流媒体曲目已下载落盘却无法加载上轨的问题；
- **5 个保存的视图（#99）**：在布局管理器里点 `保存视图`，把当前布局存为 `View 1`–`View 5` 之一，之后一键切换；
- **流媒体曲目的影子波形**：四轨布局下，另一台播放机加载的流媒体曲目会在你的影子轨道上显示真实波形（取自 Engine 自己的下载与分析结果）。`四轨角色` 设为 `自动` 时，现在固定按 Engine 的 Player 编号分配：奇数号播放机负责 1 / 3 轨，偶数号负责 2 / 4 轨。曲库底部的轨道栏改为从左到右 `1, 2, 3, 4`（原厂是 `1, 3, 2, 4`），只点亮本机负责的两轨，影子轨道置灰，防止误加载（#90、#123）；
- **djay 曲库（#112）**：打开 OmniSource 后，U 盘 / SD 卡上的 Algoriddim djay 曲库，其歌单会出现在流媒体浏览器里；
- **更短的控制台访问码**：Web 控制台访问码改为 8 位 `XXXX-XXXX`（不含 0/O、1/I，不分大小写，横杠可省略）。升级后旧的长访问码会被替换，已登录的浏览器需要重新输入 `设置 → 关于` 里显示的新访问码。同一设备 1 分钟内输错 5 次会被锁定 1 分钟；
- **二代机一键安装器（#89、#110、#129）**：EngineX-Installer 自动识别二代机，包括被新版 `android-tools` 显示为 `Android Fastboot` 的 PRIME 4+，并使用安装包自带的 `fastboot`；
- **二代机显示访问码（#132）**：PRIME 4+ 等二代机的 `设置 → 关于` 之前不显示 Web 控制台地址和访问码，检查更新还报 `GitHub unreachable`，原因是开机时重启 Engine 会连带杀掉刚启动的 EngineX 服务。现在这些服务是独立的 systemd 单元，重启 Engine 不再波及它们。之后加的 `KillMode=process` 会让原厂 Engine 一直占着屏幕、SYSTEM ONE 无法正常关机，已删除（#116）。
- **SYSTEM ONE / PRIME 4 G2 走 USB 安装（#116）**：这两款没有 fastboot，固件更新模式在 USB 上是 `1cc5:2027`。EngineX-Installer 现在会生成 64 MiB 的全新 `/data` 镜像，用官方 `write-part` 协议写入（Rane SYSTEM ONE 实机验证），写完提示你按电源键重启。`host-install-userdata.py` 新增 `--ssh-key`，可开启 root SSH。
- **实验功能，只有 API**：以下功能目前只有 Web 控制台 API，没有控制台卡片或碟机菜单，也还没有在实机上测试：通过 OmniSource 读取 WebDAV / Subsonic（Navidrome）/ Plex 歌单（#82）、Hot Cue 名称与颜色（#85）、U 盘 `/mods` QML 插件与 safe-mode 隔离（#87）、把 Loop 存到 Sampler 打击垫（#120）、局域网 RTP / Teleport 音频以及 USB DAC / USB-B UAC2 输出（#83、#113）。

### 9. 上一版新功能
- **波形可以缩得更远（#77）**：一体机（PRIME 4、PRIME 2、PRIME GO、Mixstream Pro、SC LIVE）的波形最远缩到 30 秒，和 SC5000 / SC6000 一样，不再是原厂的 10 秒，屏幕上能看到更多即将到来的节拍网格。`设置 → 设备 → 旋钮行为` 选 `波形缩放` 后，在打碟界面转浏览旋钮即可缩放；
- **屏幕上直接关机（#78）**：`设置 → 设备 → 电源 → 关机` 会弹出原厂的「关机」确认框，碟机装在 DJ 台里也不用再伸手去摸背后的电源键；
- **丢失曲目自动重定位（#74）**：同一张盘上的文件被移动或改名后，网页控制台会为每首丢失的曲目找到新位置（先按文件名和大小，再按忽略大小写的文件名，最后按清理后的文件名）。「扫描」只预览；「一键修复路径」会先备份 `m.db` 再改写路径。完成后在碟机上重新加载该盘；
- **Apple Music 一键登录（#75）**：手机页面可以直接用 Apple Music 登录（MusicKit JS）；Apple 不允许时，页面会给出一个书签脚本，在 music.apple.com 一键复制你的令牌；
- **碟机上查看控制台访问码（#79）**：`设置 → 关于` 显示网页控制台地址和访问码。流媒体二维码只登录对应的服务，不会打开控制台，俱乐部里扫屏幕二维码的客人拿不到碟机的控制权；
- **静音 Cue 与原厂主输出限幅**：Censor 键可以设成 `静音 Cue`（`设置 → 播放 → Censor 键行为`）；PRIME GO / Mixstream Pro 放开了原厂的 `原厂主输出限幅 (dB)`（`设置 → 混音台`，关 / 0–18 dB），与 EngineX 的 `0dB 防破音软限幅` 并存；
- **Apple Music** 加入流媒体（共 7 个），和其他服务一样扫碟机二维码登录；
- **Wi-Fi 传歌**：在电脑上把 MP3 / FLAC / WAV / AIFF / M4A / AAC / OGG / OPUS 文件拖进碟机网页控制台（`http://<碟机 IP>:8080`），直接存到你选的内置硬盘、SD 卡或 U 盘的 `Wi-Fi Drop/<歌单>/` 下。曲库所在盘上的 FLAC / MP3 会自动分析并加入同名歌单；不会覆盖已有文件，始终保留 256 MiB 空间；
- **OmniSource 统一浏览**：U 盘 / SD 卡里的歌单和曲目直接显示在流媒体浏览器里，和流媒体歌单放在一起，一次搜索两边都能找到；U 盘曲目直接从盘上读取播放，音质不打折（`设置 → OmniSource`，默认关闭）；
- **先锋 CDJ / Pro DJ Link（测试版）**：通过局域网 / Wi-Fi 读取 CDJ 上 rekordbox U 盘的曲库（`Pro DJ Link 曲库`），也能把本机曲库共享给 CDJ（实验功能）。SC5000 与 CDJ 之间始终只有一个 Master：MASTER 可以双向交接；本机当 Master 时会发出 BPM 和拍点，供 CDJ 的 BEAT SYNC 对拍。以上默认全部关闭；
- **更干净的声音**：`0dB 防破音软限幅` 用平滑曲线把主输出压在 -0.1 dBFS 以下，零延迟，-1.5 dBFS 以下的信号原样不动；Flanger 不再让音量下沉（自动补偿到原曲电平，最多 +6 dB）；`拉杆切歌自动回音`（仅带混音台的一体机显示，独立播放器隐藏）在拉下推子时留下回音尾巴；`自动增益（0 dB 归一化）` 把音量偏小的歌平滑地提到接近 0 dBFS（最多 +9 dB，-42 dBFS 以下的静音不提升），之后总会经过软限幅，不会爆音（默认关闭）。共 21 个实时 DSP 挂点，不增加任何延迟；
- **SC5000 / SC6000 原生四轨布局**：在独立播放机上解锁 Engine 自带的四轨界面，声卡仍只用 2 路。每台播放机负责自己角色的两轨（`自动`、`1+3`、`2+4`、`1+2`、`1`、`2`，在 `布局 → View → 四轨角色` 切换），其余各轨是不出声的**影子轨道**，跟随另一台 EngineX 碟机或 CDJ 的曲目、播放 / CUE、位置、速度、Master 和同步；
- 向 Shoutcast / Icecast 服务器 **直播推流**；
- **曲库**：`自动分析插入的设备` 暂不可用，仅保留偏好设置；rekordbox 导出的 U 盘（包括 6.8.5）即使有个别文件损坏也能正常导入，那几首在加载时再分析；选中其他流媒体服务时，不再弹出 Engine 5.3.0 的「Connect to your Engine DJ Profile to use Tidal」提示；
- **Spotify 改用你自己的应用**：Spotify 限制每个应用的用户数和请求量，所以不再内置共用的应用。到 `developer.spotify.com/dashboard` 免费创建一个应用，把网页控制台里显示的 Redirect URI 加进去，再把它的 Client ID 填回控制台（`#70`）；
- **流媒体与影子轨道更稳**：在碟机上开着的流媒体歌单，后台服务重启后照样能加载，不再报「Track not found」（`#73`）；影子轨道显示真实歌名，没有标签的文件也一样，另一台从流媒体浏览器加载的歌也能跟随；加载歌曲时打击垫保持在 HOT CUE（`#71`）。

> **先锋联机说明**：与先锋 CDJ-3000 的联机互通（曲库共享、3-Band 波形、60fps 播放头与 Master / Beat Sync 对拍）是用维护者自研的 [**OpenProLink**](https://github.com/Cd1s/OpenProLink)（内置虚拟 CDJ-3000 模拟器，以及先锋全系 CDJ / XDJ 碟机与 DJM 混音台的协议采集工具 `prolink-probe`）与实机 Denon SC5000 联调验证的。手头暂时没有先锋 CDJ-3000 实体真机，接先锋真机的表现还需要大家实测。欢迎拥有先锋 CDJ-3000、CDJ-2000NXS2、XDJ-AZ / XZ 或先锋混音台（DJM-A9 / DJM-V10 / DJM-900NXS2）真机的玩家前往 [Cd1s/OpenProLink](https://github.com/Cd1s/OpenProLink) 或本仓库提交真机实测反馈与设备 / 混音台协议包信息，共同完善跨品牌曲库、混音台效果器速度同步（Beat FX Sync）、量化（Quantize）与 On-Air 联动！


---

<a id="gallery"></a>

## 实机画面

<table>
<tr>
<td colspan="2" align="center" valign="top">
<img src="./docs/assets/screenshots/4deck-sc5000-cdj3000.png" width="49%" alt="原生 4 轨视图与影子轨道"> <img src="./docs/assets/screenshots/opencdj-3000.png" width="49%" alt="OpenProLink 虚拟 CDJ-3000"><br>
<b>原生 4 轨视图：两台 SC5000 + 一台 CDJ-3000 同一局域网联动</b><br>
<sub>右边的 SC5000 负责第 2、4 轨；第 3 轨是开着 BEAT SYNC 的 CDJ-3000（OpenProLink 模拟器），波形和速度经 Pro DJ Link 实时跟随</sub>
</td>
</tr>
<tr>
<td width="50%" align="center" valign="top">
<img src="./docs/assets/screenshots/zh-CN/01-deck-stems-official-pads.png" width="100%" alt="原生 4 轨 Stems 分轨"><br>
<b>原生 4 轨 Stems 分轨与 3-Band 波形</b><br>
<sub>底部显示人声、旋律、贝斯、鼓组四轨状态，打击垫一键静音</sub>
</td>
<td width="50%" align="center" valign="top">
<img src="./docs/assets/screenshots/zh-CN/02-library-native-stems-list.png" width="100%" alt="曲库 Stems 歌单"><br>
<b>曲库侧边栏 Stems 专属歌单</b><br>
<sub>所有已分轨曲目自动归类，随时筛选调用</sub>
</td>
</tr>
<tr>
<td width="50%" align="center" valign="top">
<img src="./docs/assets/screenshots/zh-CN/03-sources-5-streaming-platforms.png" width="100%" alt="七大流媒体平台"><br>
<b>音源页内置 7 大音乐平台</b><br>
<sub>TIDAL · 网易云 · Spotify · Deezer · SoundCloud · Beatport · Apple Music 与 U 盘并列</sub>
</td>
<td width="50%" align="center" valign="top">
<img src="./docs/assets/screenshots/zh-CN/04-streaming-qr-login.png" width="100%" alt="手机扫码登录"><br>
<b>屏幕二维码扫码登录</b><br>
<sub>手机扫一扫直接同步个人歌单，免输密码</sub>
</td>
</tr>
<tr>
<td width="50%" align="center" valign="top">
<img src="./docs/assets/screenshots/zh-CN/05-streaming-playlists-bpm-swipe.png" width="100%" alt="流媒体歌单与左滑分轨"><br>
<b>在线歌单 BPM / 调性预览与左滑分轨</b><br>
<sub>加载前即可查看曲目 BPM 与调性，左滑一键拆分 Stems</sub>
</td>
<td width="50%" align="center" valign="top">
<img src="./docs/assets/screenshots/zh-CN/06b-preferences-device.png" width="100%" alt="设备设置"><br>
<b>设备设置菜单</b><br>
<sub>直接切换语言、Pad 灯亮度与电源开关</sub>
</td>
</tr>
<tr>
<td width="50%" align="center" valign="top">
<img src="./docs/assets/screenshots/zh-CN/06-preferences-enginex-github-update.png" width="100%" alt="碟机在线检查更新"><br>
<b>设置 → 关于 / 更新：碟机屏幕一键在线升级</b><br>
<sub>检测到新版本时直接点击屏幕上的「更新」，自动拉起原厂刷机进度条完成升级</sub>
</td>
<td width="50%" align="center" valign="top">
<img src="./docs/assets/screenshots/zh-CN/07-bluetooth-audio.png" width="100%" alt="原生蓝牙音频"><br>
<b>原生蓝牙音频输入与播放控制</b><br>
<sub>手机蓝牙推流到碟机，屏幕显示曲目信息，可直接播放/暂停与切歌</sub>
</td>
</tr>
</table>

---

<a id="benchmarks"></a>

## 重构前后性能与占用对比（SC5000 实机实测）

系统经过全面重构与瘦身，所有新增功能仅占极小空间，比原厂固件留出更多可用内存：

| 对比项 | 官方原厂 / 重构前 | 重构后（`EngineX OS 5.3.0.4` 实测） | 实际打碟提升 |
| :--- | :--- | :--- | :--- |
| **刷机包体积 (`*-Update.img`)** | 官方原厂 5.3.0 `147.79 MB`<br>重构前改版 `~210 MB` | **`151.44 MB`**（SC6000 `151.58 MB`，PRIME 4 `151.52 MB`） | **仅比官方原厂大 `3.64 MB`** |
| **纯刷机开机硬盘占用 (`/data`)** | 重构前残留约 `243 MB` | **核心程序 + 三语界面仅占 `17.2 MB`**（含扩展文件系统库共 `45.6 MB`） | 内置闪存多腾出 **近 `200 MB`** 空间 |
| **整机空闲可用内存（总 `2 GB`）** | `~680 – 780 MB` | **`1,042 MB` 空闲可用**（主程序内存直降 `~150 MB`，后台服务合计仅占 **`29.2 MB`**） | **多释放 `~260 MB` 内存**，长时间演出不卡顿 |
| **音频核心被后台抢占率** | 下载或导库时偶发占用 `5% – 25%` | **`0.0%`**：所有下载与分轨任务隔离在独立副核，不碰音频核心 | **96 kHz 声音播放全程零断音** |
| **先锋 Rekordbox U 盘读取速度** | 首次转换约 `8 分钟`，重扫 `15–24 秒` | **首次插盘 `< 2 秒`**，再次插盘 **`0.03 秒`** | 插上 U 盘即插即打，无需等待 |
| **4 分钟歌曲 Stems 分轨耗时** | SC5000 原厂锁死不可用 | 连电脑 / 云端约 **`35 秒`**；碟机本机分轨比播放速度快 **`1.4 倍`** | 演出中途临时拆人声也能快速完成 |
| **大 BPM 跨度同步** | BPM 跨度超出变速范围时 Sync 对不上，需要手动调大变速范围 | **智能同步**自动升档变速范围（最高 `±100%`），控制中心一键切 Bar / Beat / Tempo | 速度相差很大的两首歌也能直接同步 |
| **网络传输速度** | 有线网口限制在 `100 Mbps` | 有线网口 **`1 Gbps`（1000 Mbps）全双工**，8 MiB TCP 缓冲；Wi-Fi 实测约 `9～10 MB/s`（SDIO 25 MHz SDR12 总线） | 大曲库走千兆有线网口传输 |

---

<a id="install"></a>

## 如何安装与升级

### 方式零：全系 16 款机型的 EngineX-Installer

EngineX-Installer 支持一代与二代机，无需输入 IP 或选择机型，通过 Wi-Fi SSH 或 USB Fastboot 识别设备并下载对应的 `.img` 或数据包安装。

- [Windows 10/11 x64](https://github.com/Cd1s/EngineX-OS/releases/latest/download/EngineX-Installer-5.3.0.4-windows-amd64.zip)
- [macOS Apple Silicon M1–M4](https://github.com/Cd1s/EngineX-OS/releases/latest/download/EngineX-Installer-5.3.0.4-macos-arm64.zip)
- [macOS Intel](https://github.com/Cd1s/EngineX-OS/releases/latest/download/EngineX-Installer-5.3.0.4-macos-amd64.zip)
- [Linux x86_64](https://github.com/Cd1s/EngineX-OS/releases/latest/download/EngineX-Installer-5.3.0.4-linux-amd64.tar.gz)

解压后分别运行 `Install-EngineX.bat`、`Install-EngineX.command` 或 `./install-enginex.sh`。网络发现依赖 StageLinQ 或 EngineX Web 界面；SSH 需要 root 权限。Fastboot 需另装 `fastboot` 和 Python 3，二代机镜像制作还需要 e2fsprogs；Windows 的二代机 USB 路径需在有 USB 访问权限的 WSL 中运行 Linux 包。请先备份碟机：Fastboot 可能导致数据丢失，尤其是新建二代机 `/data` 镜像。一代机 rootfs 刷写为实验功能，尚未在全部机型上验证。

### 方式一：U 盘刷机（首次安装）
1. 点击下方对应你机器型号的链接，直接下载升级包（或前往 [**Releases 页面**](https://github.com/Cd1s/EngineX-OS/releases/latest)）：
   - **Denon DJ SC5000 Prime**：[`SC5000-5.3.0.4-EngineX-OS-Denon-SC5000-Prime-JP07-Update.img`](https://github.com/Cd1s/EngineX-OS/releases/latest/download/SC5000-5.3.0.4-EngineX-OS-Denon-SC5000-Prime-JP07-Update.img)
   - **Denon DJ SC5000M Prime**：[`SC5000M-5.3.0.4-EngineX-OS-Denon-SC5000M-Prime-JP08-Update.img`](https://github.com/Cd1s/EngineX-OS/releases/latest/download/SC5000M-5.3.0.4-EngineX-OS-Denon-SC5000M-Prime-JP08-Update.img)
   - **Denon DJ SC6000 Prime**：[`SC6000-5.3.0.4-EngineX-OS-Denon-SC6000-Prime-JP13-Update.img`](https://github.com/Cd1s/EngineX-OS/releases/latest/download/SC6000-5.3.0.4-EngineX-OS-Denon-SC6000-Prime-JP13-Update.img)
   - **Denon DJ SC6000M Prime**：[`SC6000M-5.3.0.4-EngineX-OS-Denon-SC6000M-Prime-JP14-Update.img`](https://github.com/Cd1s/EngineX-OS/releases/latest/download/SC6000M-5.3.0.4-EngineX-OS-Denon-SC6000M-Prime-JP14-Update.img)
   - **Denon DJ PRIME 4**：[`PRIME4-5.3.0.4-EngineX-OS-Denon-PRIME-4-JC11-Update.img`](https://github.com/Cd1s/EngineX-OS/releases/latest/download/PRIME4-5.3.0.4-EngineX-OS-Denon-PRIME-4-JC11-Update.img)
   - **Denon DJ PRIME 2**：[`PRIME2-5.3.0.4-EngineX-OS-Denon-PRIME-2-JC16-Update.img`](https://github.com/Cd1s/EngineX-OS/releases/latest/download/PRIME2-5.3.0.4-EngineX-OS-Denon-PRIME-2-JC16-Update.img)
   - **Denon DJ PRIME GO**：[`PRIMEGO-5.3.0.4-EngineX-OS-Denon-PRIME-GO-JP11-Update.img`](https://github.com/Cd1s/EngineX-OS/releases/latest/download/PRIMEGO-5.3.0.4-EngineX-OS-Denon-PRIME-GO-JP11-Update.img)
   - **Numark Mixstream Pro**：[`MIXSTREAMPRO-5.3.0.4-EngineX-OS-Numark-Mixstream-Pro-NH08-Update.img`](https://github.com/Cd1s/EngineX-OS/releases/latest/download/MIXSTREAMPRO-5.3.0.4-EngineX-OS-Numark-Mixstream-Pro-NH08-Update.img)
2. 将下载好的 `.img` 文件放入 **FAT32 或 exFAT 格式 U 盘的根目录**。
3. 将 U 盘插入碟机（原厂 Engine OS 4.3.x 与 5.0.x 都可以直接刷），进入 **设置 (Settings) → 关于 / 更新 (About / Update) → 固件更新模式 (Firmware Update Mode)**，看到 `Found 5.3.0.4 on USB/SD` 时选 **USB/SD**，机器会自动完成刷入并重启。如果只弹出 *Reboot into firmware update mode?*，说明碟机没找到文件：核对机型，文件名保持原样放在根目录，再拔插一次 U 盘。

### 方式一 b：二代机型（`/data` 数据包，Beta）

**一键自动安装器 EngineX-Installer（推荐）：**无需输入 IP 或选择机型，支持 Wi-Fi SSH 和 USB Fastboot。下载适合电脑的平台包：

- [Windows 10/11 x64](https://github.com/Cd1s/EngineX-OS/releases/latest/download/EngineX-Installer-5.3.0.4-windows-amd64.zip)
- [macOS Apple Silicon M1–M4](https://github.com/Cd1s/EngineX-OS/releases/latest/download/EngineX-Installer-5.3.0.4-macos-arm64.zip)
- [macOS Intel](https://github.com/Cd1s/EngineX-OS/releases/latest/download/EngineX-Installer-5.3.0.4-macos-amd64.zip)
- [Linux x64](https://github.com/Cd1s/EngineX-OS/releases/latest/download/EngineX-Installer-5.3.0.4-linux-amd64.tar.gz)

1. 先将碟机升级到官方 **Engine OS 5.3.0**。
2. 解压安装包：Windows 双击 `Install-EngineX.bat`，macOS 双击 `Install-EngineX.command`，Linux 运行 `./install-enginex.sh`。
3. 安装器自动寻找碟机并下载对应数据包。原厂机在屏幕点 **设置 → 关于 / 更新 → 固件更新模式（Firmware Update Mode）**，重启后用 USB 连电脑；Fastboot 自动识别机型，确认后刷写 `/data`。已开启 root SSH 的机器将电脑和碟机连到同一网络，安装器通过 StageLinQ（UDP 51337）及 EngineX Web 界面扫描发现设备，在 `/data/enginex` 安装，保留曲库和设置。能否发现设备取决于网络配置和 StageLinQ 是否可用。
4. 首次安装后，后续版本可在碟机上通过 Wi-Fi 点击 **设置 → 关于 / 更新 → 更新** 在线升级（#86）。

SSH 需要碟机的 root 访问权限。USB Fastboot 需要另行安装 `fastboot`、Python 3 和 e2fsprogs；Windows 请在有 USB 访问权限的 WSL 中以 root 运行（`wsl -u root`，经 `usbipd-win` 直通）Linux 包。**刷入新的 `/data` 镜像会清空该分区上的音乐、曲库和设置。**请先备份碟机数据，安装前拔下 U 盘和 SD 卡。要保留 `/data`，请通过 `--userdata-backup` 提供该碟机的完整分区镜像。本 Beta 尚未在实机验证。

**SC LIVE 4、SC LIVE 2、PRIME 4+、PRIME GO+、Mixstream Pro+、Mixstream Pro Go、PRIME 4 G2 和 Rane SYSTEM ONE**（#61、#68、#69、#76）没有硬件 eFuse 熔断锁死，原厂升级程序（`az01-update` / `az0x-splash`）是在软件层用 `denon-1` / `rane-1` 公钥校验 `AZ0x` 固件容器签名。在原厂未改装状态下，刷入修改过的 `.img` 或一代机的 `.img`（如 `PRIME4` / `PRIMEGO` / `MIXSTREAMPRO`）会在写入前的 `Verifying`（校验）阶段直接被软件拒签退回，根本不会写入闪存，更不会触发熔断或变砖。首次安装请使用对应二代型号的 `Data-Bundle.tar.gz` 或 `EngineX-Installer`（装入 `/data/enginex`，不改动只读系统分区，卸载即恢复原厂）；首次装好后，后续新版本可直接在碟机屏幕 **设置 → 关于 / 更新** 一键在线 OTA 升级。

<details>
<summary>高级用户：手动命令行安装 / 卸载说明</summary>

下载对应机型的数据包，并用 Release 页的 `SHA256SUMS.txt` 核对 SHA-256：
   - **Denon DJ SC LIVE 4** (JP21): [`SCLIVE4-5.3.0.4-EngineX-OS-Data-Bundle.tar.gz`](https://github.com/Cd1s/EngineX-OS/releases/latest/download/SCLIVE4-5.3.0.4-EngineX-OS-Data-Bundle.tar.gz)
   - **Denon DJ SC LIVE 2** (JP20): [`SCLIVE2-5.3.0.4-EngineX-OS-Data-Bundle.tar.gz`](https://github.com/Cd1s/EngineX-OS/releases/latest/download/SCLIVE2-5.3.0.4-EngineX-OS-Data-Bundle.tar.gz)
   - **Denon DJ PRIME 4+** (JC11S): [`PRIME4PLUS-5.3.0.4-EngineX-OS-Data-Bundle.tar.gz`](https://github.com/Cd1s/EngineX-OS/releases/latest/download/PRIME4PLUS-5.3.0.4-EngineX-OS-Data-Bundle.tar.gz)
   - **Denon DJ PRIME GO+** (JP11S): [`PRIMEGOPLUS-5.3.0.4-EngineX-OS-Data-Bundle.tar.gz`](https://github.com/Cd1s/EngineX-OS/releases/latest/download/PRIMEGOPLUS-5.3.0.4-EngineX-OS-Data-Bundle.tar.gz)
   - **Numark Mixstream Pro+** (NH08S): [`MIXSTREAMPROPLUS-5.3.0.4-EngineX-OS-Data-Bundle.tar.gz`](https://github.com/Cd1s/EngineX-OS/releases/latest/download/MIXSTREAMPROPLUS-5.3.0.4-EngineX-OS-Data-Bundle.tar.gz)
   - **Numark Mixstream Pro Go** (NH10): [`MIXSTREAMPROGO-5.3.0.4-EngineX-OS-Data-Bundle.tar.gz`](https://github.com/Cd1s/EngineX-OS/releases/latest/download/MIXSTREAMPROGO-5.3.0.4-EngineX-OS-Data-Bundle.tar.gz)
   - **Denon DJ PRIME 4 G2** (JP24): [`PRIME4G2-5.3.0.4-EngineX-OS-Data-Bundle.tar.gz`](https://github.com/Cd1s/EngineX-OS/releases/latest/download/PRIME4G2-5.3.0.4-EngineX-OS-Data-Bundle.tar.gz)
   - **Rane SYSTEM ONE** (RMZ2): [`SYSTEMONE-5.3.0.4-EngineX-OS-Data-Bundle.tar.gz`](https://github.com/Cd1s/EngineX-OS/releases/latest/download/SYSTEMONE-5.3.0.4-EngineX-OS-Data-Bundle.tar.gz)

**方法一（推荐）：在机器上安装**。保留你的音乐、设置和曲库。需要机器上的 root shell（SSH 或串口控制台）。

1. 把 `.tar.gz` 拷到 U 盘插到机器上（也可以通过网络拷过去）。
2. 在机器的 shell 里解压并安装（把 `<model>` 和 U 盘名换成你的）：
   ```sh
   cp "/media/<U 盘名>/<model>-5.3.0.4-EngineX-OS-Data-Bundle.tar.gz" /tmp/
   tar -xzf /tmp/<model>-5.3.0.4-EngineX-OS-Data-Bundle.tar.gz -C /tmp
   sh /tmp/<model>-5.3.0.4-EngineX-OS-Data-Bundle/install.sh
   reboot
   ```
3. 重启后 Engine 先以原厂方式启动一次，随后自动以 EngineX OS 重新启动。之后 `设置 → 设备` 里会出现 EngineX 的选项（`语言`、`电源 → 关机` 等）。

**方法二：电脑端 Fastboot 线刷**。只在机器上没有 shell 时使用。需要一台装有 `python3`、`e2fsprogs`（`debugfs`）和 `fastboot` 的 Linux 电脑（或 Windows WSL 以 root 运行）。
- **推荐做法（EngineX-Installer 自动处理）**：直接运行安装器。它能自动识别 fastboot 模式下的碟机，自动建立并格式化出厂默认 `/data` 分区、注入数据包并刷入（**碟机 `/data` 里的音乐、曲库和设置会被清空**，安装器会要求输入 `yes` 确认；外接 U 盘与 SD 卡不受影响）。若已有这台碟机自己的完整 `/data` 备份，可通过 `--userdata-backup <镜像>` 保留原有数据。后续回退原厂只需移除数据包，不依赖初次刷机前的预先备份。
- **手动操作（需已有 `/data` 分区备份）**：
  1. 备份原件不动，在副本上操作：`cp my-userdata-backup.img userdata-enginex.img`
  2. 在电脑上解压数据包，再把它注入副本（两个辅助脚本在包里）：
     ```sh
     tar -xzf <model>-5.3.0.4-EngineX-OS-Data-Bundle.tar.gz
     python3 <model>-5.3.0.4-EngineX-OS-Data-Bundle/host-install-userdata.py userdata-enginex.img --bundle <model>-5.3.0.4-EngineX-OS-Data-Bundle.tar.gz
     ```
  3. 让机器进入 fastboot 模式并用 USB 连上电脑，然后刷入并重启：
     ```sh
     sh <model>-5.3.0.4-EngineX-OS-Data-Bundle/host-install-fastboot.sh userdata-enginex.img
     ```

**方法 3：没有 fastboot 的 SYSTEM ONE / PRIME 4 G2（USB `1cc5:2027`）。** 在碟机上点 `Settings → About / Update → Firmware Update Mode (Reboot)`，接上 USB 后运行 EngineX-Installer，它会自动完成下面几步。这会写入**全新的空** `/data` 分区：内置存储上的音乐、曲库和设置会被清空（外接 U 盘和 SD 卡不受影响）。手动操作（需要 `python3`、`e2fsprogs` 和 `pip3 install pyusb`）：
   ```sh
   mke2fs -t ext4 -O encrypt -L data -b 4096 -F data.img 64M
   python3 <model>-5.3.0.4-EngineX-OS-Data-Bundle/host-install-userdata.py data.img --bundle <model>-5.3.0.4-EngineX-OS-Data-Bundle.tar.gz --ssh-key ~/.ssh/id_ed25519.pub
   python3 <model>-5.3.0.4-EngineX-OS-Data-Bundle/host-install-usbupdate.py data.img
   ```
   `--ssh-key` 可省略。写完按电源键关机再开机。

**卸载**：`sh /data/enginex/bundle/uninstall.sh && reboot`，只删除数据包装上的内容。

</details>

> **PRIME 4 G2 / SYSTEM ONE（64 位 Engine）**：已包含 UI 包、流媒体网关（Deezer、SoundCloud、Beatport）和 EngineX 各项服务。同时带有 16 处 Engine 二进制补丁（含原厂 Apple Music 解锁）、14 处音频与过渡 DSP 防护、0dB 防破音软限幅、自动增益、推子切歌回音（Echo Out）、SQLite 调优，以及 64 位 `OfflineAnalyzer` 的 `.perf` 波形直注（#149）。4 台面解锁只适用于单机播放器；Stems 保持官方固件原样。这份 Engine 启动要用机器的安全芯片（OP-TEE），沙箱里只验证了 hook、内存补丁和 UI 包加载，没看到实际界面。

### 方式二：碟机屏幕一键在线升级（已安装本系统后）
- 连接 Wi-Fi 或网线后，打开碟机 **设置 → 关于 / 更新**；
- 检测到新版本时直接点击屏幕上的 **「更新」** 按钮，确认后碟机将自动进入原厂刷机进度条界面完成下载与升级。
- **二代机**（SC LIVE 4 / 2、PRIME 4+、PRIME GO+、Mixstream Pro+ / Pro Go、PRIME 4 G2、SYSTEM ONE）首次装好 `/data` 数据包后，之后的新版本（如 `5.3.0.4`）同样在 **设置 → 关于 / 更新** 里一键完成：碟机下载对应的 `Data-Bundle.tar.gz`，校验 `SHA256SUMS`，一次性替换文件并重启 EngineX 服务，不会改动系统分区（#86）。

### 选配：用电脑为碟机加速 Stems 分轨
当电脑与碟机处于同一 Wi-Fi / 局域网时，在电脑上运行以下命令，碟机即可自动发现电脑并调用电脑显卡进行高速 AI 分轨（无需在碟机上输入 IP 或密码）：
```sh
# macOS / Linux
tools/stems/run-home-worker.sh

# Windows (PowerShell)
powershell -ExecutionPolicy Bypass -File tools\stems\run-home-worker.ps1 demucs
```

### 流媒体登录：只有碟机也能用
不需要电脑，也不需要服务器。碟机和手机连同一个 Wi-Fi，在音源菜单里点开平台，用手机扫屏幕上的二维码即可。手机上的登录页默认英文，右上角可切换简体中文或繁體中文，每个页面都有分步教程。

| 平台 | 怎么登录 |
| --- | --- |
| 网易云音乐 | 用网易云音乐 App 扫碟机屏幕上的二维码并确认。二维码过期后点一下就会换新的。 |
| Spotify | **方式 A（推荐）**：手机或电脑打开 Spotify App 播放任意歌曲，点右下角「连接设备」音箱图标，选择 **`EngineX OS`** 约 1 秒自动完成账号绑定。**方式 B**：点 Spotify 右侧的「扫码登录」用手机扫码。直接从 Spotify 拉取 320kbps Ogg Vorbis 原生音频播放。 |
| Deezer | 粘贴你的 **ARL**（页面教程演示怎样在电脑上从 deezer.com 复制）。同一次登录为 Deezer 曲目打开 Deemix 无损 FLAC 下载；网易云只有选了「Deemix 优先」才会用到它。Spotify、Beatport、Apple Music 都只从自己的音源播放，不会跨平台兜底；SoundCloud 仅在没有 Go+ 时，才为 Go+ 专享曲目和非 Set 单曲改用已登录的 Deezer / 网易云 / Beatport（见 SoundCloud 一行）。只填资料页链接则只显示公开歌单、不能下载。 |
| SoundCloud | 填用户名或主页链接即可，不用密码、不用 Artist Pro、也不用申请 API。私密歌单在「高级」里粘贴 soundcloud.com 的 `oauth_token`。每首歌按它能提供的最高音质播放：允许下载的用上传者原档，其次 256kbps AAC、160kbps AAC、128kbps MP3。Go+ 专享曲目和非 Set 单曲：已绑定 Go+ 账号直连官方高音质流；没有 Go+ 时改用已登录的 Deezer / 网易云 / Beatport 匹配无损 / 320k 并缓存为 `soundcloud-<id>.*`；超过 10 分钟的 DJ Set 始终直连 SoundCloud。 |
| Apple Music | 已解锁 Engine 5.3.0 自带的 Apple Music 音源，直接从 Apple Music 播放，EngineX 不再去 Deezer 或网易云找替身。手机上仍可粘贴歌单链接，或填 `media-user-token` 浏览自己的资料库。 |
| Beatport | 填 Beatport 邮箱和密码，碟机只保存登录令牌。 |

### 选配：自建登录中继
中继程序名为 `enginex-relay`（兼容旧名 `sc5000-relay`）。中继让用移动数据的手机也能完成登录，并提供 Deezer 网页内登录。在任意有公网 HTTPS 地址的服务器上运行，然后在碟机网页控制台（`http://<碟机 IP>:8080`）填入它的地址和密码：
```sh
go build -o enginex-relay ./cmd/sc5000-relay
./enginex-relay -public https://relay.example.com -listen 0.0.0.0:6595
# 首次控制台密码写在 /var/lib/sc5000-relay/initial-password
```

**没有域名？用 Cloudflare Quick Tunnel 一键公网连接。** 无需购买域名，也不用配置反向代理：
1. **碟机端一键开启**：在碟机 `:8080` 控制台点击「开启免域名公网隧道 (Cloudflare Quick Tunnel)」，系统会按架构自动安装并启动 `cloudflared`，秒级生成 `https://*.trycloudflare.com` 公网 HTTPS 地址。手机用 4G/5G 流量即可远程打开控制台，碟机屏幕上的流媒体登录二维码也会自动使用该公网地址回调。
2. **自建 `enginex-relay` 免域名启动**：运行 `enginex-relay -public auto`（或 `run-home-worker.sh --with-tunnel`），自动通过 Cloudflare Quick Tunnel 生成免域名公网 HTTPS 中继地址，并汇总管理多台碟机。

`trycloudflare.com` 地址在隧道重启后会变化；需要固定地址请改用自己的域名。

---

## 安全说明：演出现场公共网络防护

EngineX OS 按照「和陌生人共用酒吧、Club、音乐节 Wi-Fi」的场景做了安全设计。

- **没有任何默认 SSH 密码。** 密码登录已彻底关闭，公开固件也**不内置任何 SSH 公钥**，你自己没放公钥之前 SSH 根本不开放。用过旧版公开固件的碟机，升级到 5.3.0.1 时会自动清除旧固件带的那把公钥。
- **同一个 Wi-Fi 下的人控制不了你的碟机。** 碟机内部用的所有接口（播放控制、切波形、切语言、在线升级、流媒体登录、Stems）只接受碟机自己（`127.0.0.1`）调用，别人就算和你连同一个网络，也没法远程切你的歌、改设置或重启机器。
- **手机网页控制台必须输入你这台碟机独有的访问码。** 打开 `http://<碟机IP>:8080` 时要输入碟机随机生成的访问码；地址和访问码显示在碟机的 `设置 → 关于` 里；流媒体扫码登录用的是会过期的一次性链接，不会打开控制台。

高级用户：

- 想用 SSH：把你自己的公钥放进 `/data/enginex/authorized_keys`（例如自己构建带公钥的私有镜像），重启后生效。只能用密钥登录，不能用密码。
- 控制台访问码保存在 `/data/enginex/state/access-token`，删掉这个文件再重启碟机就会生成新码。
- 在不信任的网络演出时，U 盘、SD 卡和蓝牙播放都不需要联网，可以直接不连 Wi-Fi。

---

<details>
<summary><b>开发者说明与源码构建（普通 DJ 用户无需查看）</b></summary>

<br>

- 完整 642 条历史问题核查清单：[`docs/OPEN-SOURCE-FULL-MANIFEST.md`](./docs/OPEN-SOURCE-FULL-MANIFEST.md)
- 压测与修复记录：[`docs/ISSUE-FIXES-2026-11-04.md`](./docs/ISSUE-FIXES-2026-11-04.md)
- 贡献与发版规范：[`CONTRIBUTING.md`](./CONTRIBUTING.md)

```sh
# 运行测试
go test -race ./...
bash tests/test_rootfs_overlay.sh

# 构建五合一静态 ARMv7 二进制与 C Hook
GOOS=linux GOARCH=arm GOARM=7 CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o out/sc5000-suite ./cmd/sc5000-suite
sh tools/ui-mod/build.sh
sh tools/edisksd-hook/build.sh
```

</details>

---

<a id="sponsor"></a>

## 赞赏与支持

如果 **EngineX OS** 让你的碟机更好用、在演出或练歌时帮到了你，欢迎赞赏支持或给项目点个 **Star**！

<table>
<tr>
<td width="33%" align="center" valign="top">
<img src="./docs/assets/sponsor-wechat.jpg" height="260" alt="微信赞赏码"><br>
<b>微信赞赏码</b><br>
<sub>微信扫一扫直接赞赏</sub>
</td>
<td width="33%" align="center" valign="top">
<img src="./docs/assets/sponsor-binance-pay.jpg" height="260" alt="Binance Pay 币安收款码"><br>
<b>币安支付 (Binance Pay)</b><br>
<sub>币安扫码或转币安 ID：<code>565380652</code>（0 手续费）</sub>
</td>
<td width="33%" align="center" valign="top">
<img src="./docs/assets/sponsor-evm-usdt.jpg" height="260" alt="USDT EVM 收款码"><br>
<b>USDT / USDC（币安 EVM 全链通用）</b><br>
<sub>支持 BSC · ERC20 · Arbitrum · Optimism · Polygon · Base · opBNB</sub>
</td>
</tr>
</table>

> **币安 ID (Binance UID，币安内转账 0 手续费，点击右侧一键复制)：**
> ```text
> 565380652
> ```
>
> **USDT / USDC 链上收款地址（支持币安所有 EVM 网络：`BSC (BEP-20)` · `Ethereum (ERC-20)` · `Arbitrum One` · `Optimism` · `Polygon` · `Base` · `opBNB` · `Avalanche C-Chain` · `zkSync Era` · `Scroll` · `Linea`）：**
> ```text
> 0x35e0c53dd887b37cd5db0064291f7c5a7442bc91
> ```

---

## Star 增长趋势

<div align="center">
<a href="https://star-history.com/#Cd1s/EngineX-OS&Date">
  <picture>
    <source media="(prefers-color-scheme: dark)" srcset="https://api.star-history.com/svg?repos=Cd1s/EngineX-OS&type=Date&theme=dark" />
    <source media="(prefers-color-scheme: light)" srcset="https://api.star-history.com/svg?repos=Cd1s/EngineX-OS&type=Date" />
    <img alt="Star History Chart" src="https://api.star-history.com/svg?repos=Cd1s/EngineX-OS&type=Date" width="100%" />
  </picture>
</a>
</div>

---

<div align="center">
<sub>MIT License · EngineX OS 是独立的开源社区项目，与 inMusic Brands / Denon DJ / Numark 无关。本仓库仅提供开源补丁与工具源码，不分发任何受版权保护的原厂固件文件。</sub>
</div>
