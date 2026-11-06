<div align="center">

<img src="./docs/assets/hero-banner-zh-CN.svg" alt="EngineX OS 5.3.0.2" width="100%">

<br>

<a href="./README.md"><img src="https://img.shields.io/badge/English-30363d?style=for-the-badge" alt="English"></a>
<a href="./README.zh-CN.md"><img src="https://img.shields.io/badge/%E7%AE%80%E4%BD%93%E4%B8%AD%E6%96%87-00e676?style=for-the-badge" alt="简体中文"></a>
<a href="./README.zh-TW.md"><img src="https://img.shields.io/badge/%E7%B9%81%E9%AB%94%E4%B8%AD%E6%96%87-30363d?style=for-the-badge" alt="繁體中文"></a>

<br>

**[核心功能](#features)** · **[固件下载](#install)** · **[实机画面](#gallery)** · **[重构与占用对比](#benchmarks)** · **[赞赏支持](#sponsor)**

</div>

<br>

**EngineX OS 5.3.0.2** 是面向天龙 Denon DJ 与 Numark 独立打碟机的增强固件，支持：
`SC5000` · `SC5000M` · `SC6000` · `SC6000M` · `PRIME 4` · `PRIME 2` · `PRIME GO` · `Mixstream Pro`。

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
- **Beatport** 使用你自己的账号与订阅：Professional 下载 FLAC 或 256k AAC，Advanced 下载 128k AAC，Essential 在线播放 128k AAC 流；Beatport 曲目同样可以一键拆 Stems。
- **Apple Music**：在手机上粘贴任意歌单或专辑链接即可，不需要账号；再填入 `media-user-token` 就能浏览自己的资料库。Apple Music 的音频受保护，所以每首歌按 ISRC 或歌名加歌手匹配，从 Deezer（无损 FLAC）或网易云播放。

### 3. 先锋 Rekordbox U 盘即插即用（免电脑转换）
平时给先锋 CDJ 导出的 Rekordbox U 盘，直接插上即可上台打碟：
- 插盘 **2 秒内**直接读取全部歌单、Hot Cue、Memory Cue、Loop 循环点与拍线，无需再用电脑端 Engine DJ 重新导库；
- 兼容全格式存储设备：Windows 的 `FAT32` / `exFAT` / `NTFS` 与 Mac 的 `HFS+` / `APFS` 插上直读；
- 自动修复异常拔盘造成的数据库损坏，并去除了“未安全弹出”报错弹窗。

### 4. 完整中文界面、先锋风格 3-Band 波形与转盘灯效
- **三语界面**：设置、菜单与提示全面支持 **简体中文 / 繁體中文 / English**；
- **3 种波形配色**：在原厂绿波形基础上，新增高对比度 **RGB 波形** 与先锋风格 **3-Band（三频段蓝/橙/白）波形**；
- **转盘彩灯模式**：支持固定颜色、流光渐变、频闪，以及**跟随当前播放 BPM 拍点同步变色**。

### 5. 切效果器零爆音，长时间演出更稳定
- 消除原厂固件在开关或切换效果器（FX）时偶发的“咔嗒”爆音，双轨同时开效果器也有平滑淡入过渡；
- 默认关闭原厂后台匿名统计上报与无用磁盘缓存，把全部算力和内存留给音频播放与屏幕触控。

### 6. 智能同步与千兆有线网口
- **智能同步（Smart Sync）**：下拉控制中心一键切换 **Bar / Beat / Tempo** 同步模式；开启 Sync 后，两首曲目 BPM 跨度超出当前变速范围时，自动把 Pitch Range 升档到够用为止（最高 `±100%`）；
- **千兆有线网口**：有线网口从原厂限制的 `100 Mbps` 解锁至 **1 Gbps（1000 Mbps）全双工满速**，并配 8 MiB TCP 大窗口缓冲；
- **Wi-Fi**：碟机内置 Wi-Fi 模组受限于主板 SDIO 25 MHz SDR12 总线带宽，实测极限约 `9～10 MB/s`，大体积曲库传输建议使用千兆有线网口。


### 7. 5.3.0.2 新功能
- **Apple Music** 加入流媒体（共 7 个），和其他服务一样扫碟机二维码登录；
- **Wi-Fi 传歌**：在电脑上把 MP3 / FLAC / WAV / AIFF / M4A / AAC / OGG / OPUS 文件拖进碟机网页控制台（`http://<碟机 IP>:8080`），直接存到你选的内置硬盘、SD 卡或 U 盘的 `Wi-Fi Drop/<歌单>/` 下。曲库所在盘上的 FLAC / MP3 会自动分析并加入同名歌单；不会覆盖已有文件，始终保留 256 MiB 空间；
- **OmniSource 统一浏览**：U 盘 / SD 卡里的歌单和曲目直接显示在流媒体浏览器里，和流媒体歌单放在一起，一次搜索两边都能找到；U 盘曲目直接从盘上读取播放，音质不打折（`设置 → OmniSource`，默认关闭）；
- **先锋 CDJ / Pro DJ Link（测试版）**：通过局域网 / Wi-Fi 读取 CDJ 上 rekordbox U 盘的曲库（`Pro DJ Link 曲库`），也能把本机曲库共享给 CDJ（实验功能）。SC5000 与 CDJ 之间始终只有一个 Master：MASTER 可以双向交接；本机当 Master 时会发出 BPM 和拍点，供 CDJ 的 BEAT SYNC 对拍。以上默认全部关闭；
- **更干净的声音**：`主输出软限幅` 用平滑曲线把主输出压在 -0.1 dBFS 以下，零延迟，-1.5 dBFS 以下的信号原样不动；Flanger 不再让音量下沉（自动补偿到原曲电平，最多 +6 dB）；`拉杆切歌自动回音` 在拉下推子时留下回音尾巴；`自动增益（0 dB 归一化）` 把音量偏小的歌平滑地提到接近 0 dBFS（最多 +9 dB，-42 dBFS 以下的静音不提升），之后总会经过软限幅，不会爆音（默认关闭）。共 21 个实时 DSP 挂点，不增加任何延迟；
- **SC5000 / SC6000 原生四轨布局**：在独立播放机上解锁 Engine 自带的四轨界面，声卡仍只用 2 路。每台播放机负责自己角色的两轨（`自动`、`1+3`、`2+4`、`1+2`、`1`、`2`，在 `设置 → 设备 → 四轨角色` 或网页控制台切换），其余各轨是不出声的**影子轨道**，跟随另一台 EngineX 碟机或 CDJ 的曲目、播放 / CUE、位置、速度、Master 和同步；
- 向 Shoutcast / Icecast 服务器 **直播推流**；
- **曲库**：`自动分析插入的设备` 在后台分析新插入的 U 盘 / SD 卡上所有未分析的曲目，使用最低优先级，不影响播放；rekordbox 导出的 U 盘（包括 6.8.5）即使有个别文件损坏也能正常导入，那几首在加载时再分析；选中其他流媒体服务时，不再弹出 Engine 5.3.0 的「Connect to your Engine DJ Profile to use Tidal」提示；
- **Spotify 改用你自己的应用**：Spotify 限制每个应用的用户数和请求量，所以不再内置共用的应用。到 `developer.spotify.com/dashboard` 免费创建一个应用，把网页控制台里显示的 Redirect URI 加进去，再把它的 Client ID 填回控制台（`#70`）；
- **流媒体与影子轨道更稳**：在碟机上开着的流媒体歌单，后台服务重启后照样能加载，不再报「Track not found」（`#73`）；影子轨道显示真实歌名，没有标签的文件也一样，另一台从流媒体浏览器加载的歌也能跟随；加载歌曲时打击垫保持在 HOT CUE（`#71`）。

> **先锋联机说明**：与先锋 CDJ-3000 的联机互通（曲库共享、3-Band 波形、60fps 播放头与 Master / Beat Sync 对拍）是用维护者自研的 **OpenCDJ-3000**（Virtual CDJ-3000 硬件协议模拟器）与实机 Denon SC5000 联调验证的。手头暂时没有先锋 CDJ-3000 实体真机，接先锋真机的表现还需要大家实测，非常欢迎提交 Issue 或 PR 反馈！


---

<a id="gallery"></a>

## 实机画面

<table>
<tr>
<td colspan="2" align="center" valign="top">
<img src="./docs/assets/screenshots/4deck-sc5000-cdj3000.png" width="49%" alt="原生 4 轨视图与影子轨道"> <img src="./docs/assets/screenshots/opencdj-3000.png" width="49%" alt="OpenCDJ-3000 虚拟 CDJ-3000"><br>
<b>原生 4 轨视图：两台 SC5000 + 一台 CDJ-3000 同一局域网联动</b><br>
<sub>右边的 SC5000 负责第 2、4 轨；第 3 轨是开着 BEAT SYNC 的 CDJ-3000（OpenCDJ-3000 模拟器），波形和速度经 Pro DJ Link 实时跟随</sub>
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
<img src="./docs/assets/screenshots/zh-CN/03-sources-5-streaming-platforms.png" width="100%" alt="五大流媒体平台"><br>
<b>音源页内置 5 大音乐平台</b><br>
<sub>网易云 · Spotify · Deezer · SoundCloud · TIDAL 与 U 盘并列</sub>
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
<sub>直接切换语言、波形样式、流媒体音质与转盘灯效</sub>
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

| 对比项 | 官方原厂 / 重构前 | 重构后（`EngineX OS 5.3.0.2` 实测） | 实际打碟提升 |
| :--- | :--- | :--- | :--- |
| **刷机包体积 (`*-Update.img`)** | 官方原厂 5.3.0 `147.79 MB`<br>重构前改版 `~210 MB` | **`151.44 MB`**（SC6000 `151.58 MB`，PRIME 4 `151.52 MB`） | **仅比官方原厂大 `3.64 MB`** |
| **纯刷机开机硬盘占用 (`/data`)** | 重构前残留约 `243 MB` | **核心程序 + 三语界面仅占 `17.2 MB`**（含扩展文件系统库共 `45.6 MB`） | 内置闪存多腾出 **近 `200 MB`** 空间 |
| **整机空闲可用内存（总 `2 GB`）** | `~680 – 780 MB` | **`1,042 MB` 空闲可用**（主程序内存直降 `~150 MB`，后台服务合计仅占 **`29.2 MB`**） | **多释放 `~260 MB` 内存**，长时间演出不卡顿 |
| **音频核心被后台抢占率** | 下载或导库时偶发占用 `5% – 25%` | **`0.0%`**：所有下载与分轨任务隔离在独立副核，不碰音频核心 | **96 kHz 声音播放全程零断音** |
| **先锋 Rekordbox U 盘读取速度** | 首次转换约 `8 分钟`，重扫 `15–24 秒` | **首次插盘 `< 2 秒`**，再次插盘 **`0.03 秒`** | 插上 U 盘即插即打，无需等待 |
| **4 分钟歌曲 Stems 分轨耗时** | SC5000 原厂锁死不可用 | 连电脑 / 云端约 **`35 秒`**；碟机本机分轨比播放速度快 **`1.4 倍`** | 演出中途临时拆人声也能快速完成 |
| **大 BPM 跨度同步** | BPM 跨度超出变速范围时 Sync 对不上，需要手动调大 Pitch Range | **智能同步**自动升档 Pitch Range（最高 `±100%`），控制中心一键切 Bar / Beat / Tempo | 速度相差很大的两首歌也能直接同步 |
| **网络传输速度** | 有线网口限制在 `100 Mbps` | 有线网口 **`1 Gbps`（1000 Mbps）全双工**，8 MiB TCP 缓冲；Wi-Fi 实测约 `9～10 MB/s`（SDIO 25 MHz SDR12 总线） | 大曲库走千兆有线网口传输 |

---

<a id="install"></a>

## 如何安装与升级

### 方式一：U 盘刷机（首次安装）
1. 点击下方对应你机器型号的链接，直接下载升级包（或前往 [**Releases 页面**](https://github.com/Cd1s/EngineX-OS/releases/latest)）：
   - **Denon DJ SC5000 Prime**：[`SC5000-5.3.0.2-EngineX-OS-Denon-SC5000-Prime-JP07-Update.img`](https://github.com/Cd1s/EngineX-OS/releases/latest/download/SC5000-5.3.0.2-EngineX-OS-Denon-SC5000-Prime-JP07-Update.img)
   - **Denon DJ SC5000M Prime**：[`SC5000M-5.3.0.2-EngineX-OS-Denon-SC5000M-Prime-JP08-Update.img`](https://github.com/Cd1s/EngineX-OS/releases/latest/download/SC5000M-5.3.0.2-EngineX-OS-Denon-SC5000M-Prime-JP08-Update.img)
   - **Denon DJ SC6000 Prime**：[`SC6000-5.3.0.2-EngineX-OS-Denon-SC6000-Prime-JP13-Update.img`](https://github.com/Cd1s/EngineX-OS/releases/latest/download/SC6000-5.3.0.2-EngineX-OS-Denon-SC6000-Prime-JP13-Update.img)
   - **Denon DJ SC6000M Prime**：[`SC6000M-5.3.0.2-EngineX-OS-Denon-SC6000M-Prime-JP14-Update.img`](https://github.com/Cd1s/EngineX-OS/releases/latest/download/SC6000M-5.3.0.2-EngineX-OS-Denon-SC6000M-Prime-JP14-Update.img)
   - **Denon DJ PRIME 4**：[`PRIME4-5.3.0.2-EngineX-OS-Denon-PRIME-4-JC11-Update.img`](https://github.com/Cd1s/EngineX-OS/releases/latest/download/PRIME4-5.3.0.2-EngineX-OS-Denon-PRIME-4-JC11-Update.img)
   - **Denon DJ PRIME 2**：[`PRIME2-5.3.0.2-EngineX-OS-Denon-PRIME-2-JC16-Update.img`](https://github.com/Cd1s/EngineX-OS/releases/latest/download/PRIME2-5.3.0.2-EngineX-OS-Denon-PRIME-2-JC16-Update.img)
   - **Denon DJ PRIME GO**：[`PRIMEGO-5.3.0.2-EngineX-OS-Denon-PRIME-GO-JP11-Update.img`](https://github.com/Cd1s/EngineX-OS/releases/latest/download/PRIMEGO-5.3.0.2-EngineX-OS-Denon-PRIME-GO-JP11-Update.img)
   - **Numark Mixstream Pro**：[`MIXSTREAMPRO-5.3.0.2-EngineX-OS-Numark-Mixstream-Pro-NH08-Update.img`](https://github.com/Cd1s/EngineX-OS/releases/latest/download/MIXSTREAMPRO-5.3.0.2-EngineX-OS-Numark-Mixstream-Pro-NH08-Update.img)
2. 将下载好的 `.img` 文件放入 **FAT32 或 exFAT 格式 U 盘的根目录**。
3. 将 U 盘插入碟机（原厂 Engine OS 4.3.x 与 5.0.x 都可以直接刷），进入 **设置 (Settings) → 关于 / 更新 (About / Update) → 固件更新模式 (Firmware Update Mode)**，看到 `Found 5.3.0.2 on USB/SD` 时选 **USB/SD**，机器会自动完成刷入并重启。如果只弹出 *Reboot into firmware update mode?*，说明碟机没找到文件：核对机型，文件名保持原样放在根目录，再拔插一次 U 盘。

> **二代机型（SC LIVE 4、SC LIVE 2、PRIME 4+、PRIME GO+、Mixstream Pro+、Mixstream Pro Go、PRIME 4 G2、SYSTEM ONE）：`/data` 数据包，Beta**（#61、#68、#69、#76）。它们的固件带签名校验，不接受改过的 `.img`，所以没有它们的 `.img`。**二代机千万不要刷 `PRIME4` / `PRIMEGO` / `MIXSTREAMPRO` 的镜像。** 改用 `/data` 数据包：在官方 Engine OS 5.3.0 之上加装 EngineX OS（先装好官方 5.3.0），不改动系统分区。前六款跑的是和 SC LIVE 4 同一份 Engine；PRIME 4 G2 和 SYSTEM ONE 共用一份 64 位 Engine。数据包已在官方 SC LIVE 4、Mixstream Pro+ 和 SYSTEM ONE / PRIME 4 G2 的根文件系统上通过沙箱测试，还没在实机上试过，欢迎反馈。
>
> 1. 下载对应机型的数据包：
>    - **Denon DJ SC LIVE 4** (JP21): [`SCLIVE4-5.3.0.2-EngineX-OS-Data-Bundle.tar.gz`](https://github.com/Cd1s/EngineX-OS/releases/latest/download/SCLIVE4-5.3.0.2-EngineX-OS-Data-Bundle.tar.gz)
>    - **Denon DJ SC LIVE 2** (JP20): [`SCLIVE2-5.3.0.2-EngineX-OS-Data-Bundle.tar.gz`](https://github.com/Cd1s/EngineX-OS/releases/latest/download/SCLIVE2-5.3.0.2-EngineX-OS-Data-Bundle.tar.gz)
>    - **Denon DJ PRIME 4+** (JC11S): [`PRIME4PLUS-5.3.0.2-EngineX-OS-Data-Bundle.tar.gz`](https://github.com/Cd1s/EngineX-OS/releases/latest/download/PRIME4PLUS-5.3.0.2-EngineX-OS-Data-Bundle.tar.gz)
>    - **Denon DJ PRIME GO+** (JP11S): [`PRIMEGOPLUS-5.3.0.2-EngineX-OS-Data-Bundle.tar.gz`](https://github.com/Cd1s/EngineX-OS/releases/latest/download/PRIMEGOPLUS-5.3.0.2-EngineX-OS-Data-Bundle.tar.gz)
>    - **Numark Mixstream Pro+** (NH08S): [`MIXSTREAMPROPLUS-5.3.0.2-EngineX-OS-Data-Bundle.tar.gz`](https://github.com/Cd1s/EngineX-OS/releases/latest/download/MIXSTREAMPROPLUS-5.3.0.2-EngineX-OS-Data-Bundle.tar.gz)
>    - **Numark Mixstream Pro Go** (NH10): [`MIXSTREAMPROGO-5.3.0.2-EngineX-OS-Data-Bundle.tar.gz`](https://github.com/Cd1s/EngineX-OS/releases/latest/download/MIXSTREAMPROGO-5.3.0.2-EngineX-OS-Data-Bundle.tar.gz)
>    - **Denon DJ PRIME 4 G2** (JP24): [`PRIME4G2-5.3.0.2-EngineX-OS-Data-Bundle.tar.gz`](https://github.com/Cd1s/EngineX-OS/releases/latest/download/PRIME4G2-5.3.0.2-EngineX-OS-Data-Bundle.tar.gz)
>    - **Rane SYSTEM ONE** (RMZ2): [`SYSTEMONE-5.3.0.2-EngineX-OS-Data-Bundle.tar.gz`](https://github.com/Cd1s/EngineX-OS/releases/latest/download/SYSTEMONE-5.3.0.2-EngineX-OS-Data-Bundle.tar.gz)
> 2. 按包里的 `README-INSTALL.txt` 安装：推荐在机器上运行 `install.sh`（保留你的文件）。fastboot 方式会覆盖整个 `/data` 分区，只能用你自己 `/data` 备份做出来的镜像。
>
> PRIME 4 G2 和 SYSTEM ONE（64 位 Engine）：已包含 UI 包、流媒体网关（Deezer、SoundCloud、Beatport）和 EngineX 各项服务。DSP 防护、主输出限幅与自动增益、SQLite 调优和 4 台面解锁还没移植到 64 位 Engine；Stems 保持官方固件原样。这份 Engine 启动要用机器的安全芯片（OP-TEE），沙箱里只验证了 hook、内存补丁和 UI 包加载，没看到实际界面。

### 方式二：碟机屏幕一键在线升级（已安装本系统后）
- 连接 Wi-Fi 或网线后，打开碟机 **设置 → 关于 / 更新**；
- 检测到新版本时直接点击屏幕上的 **「更新」** 按钮，确认后碟机将自动进入原厂刷机进度条界面完成下载与升级。

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
| Spotify | 第 1 步：点「打开 Spotify 登录」并同意。第 2 步：随后会打开一个以 `127.0.0.1` 开头、无法访问的页面，复制它的地址，点「粘贴并连接」。 |
| Deezer | 粘贴你的 **ARL**（页面教程演示怎样在电脑上从 deezer.com 复制）。同一次登录也为 Spotify、网易云和 Deezer 曲目打开 Deemix 无损 FLAC 下载。只填资料页链接则只显示公开歌单、不能下载。 |
| SoundCloud | 填用户名或主页链接即可，不用密码、不用 Artist Pro、也不用申请 API。私密歌单在「高级」里粘贴 soundcloud.com 的 `oauth_token`。 |
| Beatport | 填 Beatport 邮箱和密码，碟机只保存登录令牌。 |

### 选配：自建登录中继
中继让用移动数据的手机也能完成登录，并提供 Deezer 网页内登录。在任意有公网 HTTPS 地址的服务器上运行，然后在碟机网页控制台（`http://<碟机 IP>:8080`）填入它的地址和密码：
```sh
go build -o sc5000-relay ./cmd/sc5000-relay
./sc5000-relay -public https://relay.example.com -listen 0.0.0.0:6595
# 首次控制台密码写在 /var/lib/sc5000-relay/initial-password
```

---

## 安全说明：演出现场公共网络防护

EngineX OS 按照「和陌生人共用酒吧、Club、音乐节 Wi-Fi」的场景做了安全设计。

- **没有任何默认 SSH 密码。** 密码登录已彻底关闭，公开固件也**不内置任何 SSH 公钥**，你自己没放公钥之前 SSH 根本不开放。用过旧版公开固件的碟机，升级到 5.3.0.2 时会自动清除旧固件带的那把公钥。
- **同一个 Wi-Fi 下的人控制不了你的碟机。** 碟机内部用的所有接口（播放控制、切波形、切语言、在线升级、流媒体登录、Stems）只接受碟机自己（`127.0.0.1`）调用，别人就算和你连同一个网络，也没法远程切你的歌、改设置或重启机器。
- **手机网页控制台必须输入你这台碟机独有的访问码。** 打开 `http://<碟机IP>:8080` 时要输入碟机随机生成的访问码；流媒体扫码登录用的是会过期的一次性链接。

高级用户：

- 想用 SSH：把你自己的公钥放进 `/data/sc5000-custom/authorized_keys`（例如自己构建带公钥的私有镜像），重启后生效。只能用密钥登录，不能用密码。
- 控制台访问码保存在 `/data/sc5000-custom/state/access-token`，删掉这个文件再重启碟机就会生成新码。
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
