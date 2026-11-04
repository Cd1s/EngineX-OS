<div align="center">

<img src="./docs/assets/hero-banner-zh-CN.svg" alt="EngineX OS 5.0.4.1" width="100%">

<br>

<a href="./README.md"><img src="https://img.shields.io/badge/English-30363d?style=for-the-badge" alt="English"></a>
<a href="./README.zh-CN.md"><img src="https://img.shields.io/badge/%E7%AE%80%E4%BD%93%E4%B8%AD%E6%96%87-00e676?style=for-the-badge" alt="简体中文"></a>
<a href="./README.zh-TW.md"><img src="https://img.shields.io/badge/%E7%B9%81%E9%AB%94%E4%B8%AD%E6%96%87-30363d?style=for-the-badge" alt="繁體中文"></a>

<br>

**[解锁功能](#features)** · **[刷机包下载](https://github.com/Cd1s/EngineX-OS/releases/latest)** · **[实机画面](#gallery)** · **[重构与占用对比](#benchmarks)** · **[如何安装与升级](#install)** · **[赞赏支持](#sponsor)**

</div>

<br>

**EngineX OS 5.0.4.1** 是专门给天龙 Denon DJ 和 Numark 独立打碟机做的增强固件，支持：
`SC5000` · `SC5000M` · `SC6000` · `SC6000M` · `PRIME 4` · `PRIME 2` · `PRIME GO` · `Mixstream Pro`。

操作方式和原厂完全一样，直接用 U 盘刷入就能解锁下面这些打碟功能。

---

<a id="features"></a>

## 🔓 刷完能多干什么？

### 1. 🎛 全机型解锁 4 轨 Stems 实时分轨（人声 / 旋律 / 贝斯 / 鼓组）
天龙官方把独立 Stems 分轨锁在了部分新机器上。刷入本系统后，**SC5000 / SC6000 / PRIME 全系**都能直接开启曲库里的 `Stems` 歌单和打击垫四轨独立静音：
- 选歌时往左一滑就能直接拆出 **Vocal（人声）、Melody（旋律）、Bass（贝斯）、Drums（鼓组）** 四轨；
- 打碟时直接按打击垫一键抽掉人声做 Acapella，或者单留鼓点接歌；
- 不连电脑也能用碟机自己分轨；连上家里电脑或云端服务器还能跑满速高质量 AI 分轨。

### 2. 🎵 5 大音乐平台直接上碟机（手机扫码就能登）
不用再每次演出前把歌一首首拷进 U 盘：
- 音源页直接内置 **网易云音乐、Spotify、Deezer、SoundCloud、TIDAL**；
- 拿手机扫一下碟机屏幕上的二维码就能登录自己的账号，直接刷自己的红心歌单、收藏夹和平台搜歌；
- 支持 **无损 FLAC / 320k MP3** 音质切换，加载过的歌自动缓存在碟机里，第二次点开直接秒进，不重复下载。

### 3. 📀 先锋 Rekordbox U 盘插上秒读（不用电脑转库）
平时给先锋 CDJ 导出的 Rekordbox U 盘，直接插上就能打：
- 插盘 **2 秒内**直接读出全部歌单、Hot Cue、Memory Cue、Loop 循环点和拍线，不用再去电脑上开 Engine DJ 重新导库；
- 不挑 U 盘格式：Windows 的 `FAT32` / `exFAT` / `NTFS`，Mac 的 `HFS+` / `APFS` 插上都能直接认；
- 自动修复非正常拔盘导致的数据库损坏，顺手屏蔽了烦人的“未安全弹出”报错弹窗。

### 4. 🌈 全中文界面 + 先锋风格 3-Band 波形 + 转盘灯效
- **完整中文界面**：设置、菜单、提示全面支持 **简体中文 / 繁體中文 / English**；
- **3 种波形配色**：除了原厂绿波形，新增高饱和度 **RGB 波形** 和先锋风格 **3-Band（三频段蓝/橙/白）波形**，看高低频段落更直观；
- **转盘彩灯模式**：支持固定颜色、迷幻流光、频闪，以及**跟随当前播放 BPM 拍点同步变色**。

### 5. 🔇 切效果器不爆音，长时间打碟更稳
- 修复了原厂固件在开关或切换效果器（FX）时偶发的“咔嗒”爆音问题，双轨同时开效果器也有平滑淡入过渡；
- 关掉了原厂后台偷偷写闪存的统计上报和无用缓存，把系统算力全部留给声音播放和屏幕触控。

---

<a id="gallery"></a>

## 📸 实机画面

<table>
<tr>
<td width="50%" align="center">
<img src="./docs/assets/screenshots/zh-CN/01-deck-stems-official-pads.png" width="100%" alt="原生 4 轨 Stems 分轨"><br>
<b>原生 4 轨 Stems 分轨与 3-Band 波形</b><br>
<sub>底部直接显示 <code>Vocal · Melody · Bass · Drums</code> 四轨状态，打击垫一键静音/恢复</sub>
</td>
<td width="50%" align="center">
<img src="./docs/assets/screenshots/zh-CN/02-library-native-stems-list.png" width="100%" alt="曲库 Stems 歌单"><br>
<b>曲库侧边栏解锁 Stems 专属歌单</b><br>
<sub>所有分好轨的歌曲自动归类，随时筛选调用</sub>
</td>
</tr>
<tr>
<td align="center">
<img src="./docs/assets/screenshots/zh-CN/03-sources-5-streaming-platforms.png" width="100%" alt="五大流媒体平台"><br>
<b>音源页内置 5 大音乐平台</b><br>
<sub>网易云 · Spotify · Deezer · SoundCloud · TIDAL 与 U 盘、SD 卡并列</sub>
</td>
<td align="center">
<img src="./docs/assets/screenshots/zh-CN/04-streaming-qr-login.png" width="100%" alt="手机扫码登录"><br>
<b>大屏二维码扫码登录</b><br>
<sub>手机扫一下即可同步个人歌单，免输复杂密码</sub>
</td>
</tr>
<tr>
<td align="center">
<img src="./docs/assets/screenshots/zh-CN/05-streaming-playlists-bpm-swipe.png" width="100%" alt="流媒体歌单与左滑分轨"><br>
<b>在线歌单直接显示 BPM / 调性 + 左滑分轨</b><br>
<sub>还没加载上盘就能看到曲库 BPM 和调性，往左滑即可一键拆 Stems</sub>
</td>
<td align="center">
<img src="./docs/assets/screenshots/zh-CN/06b-preferences-device.png" width="100%" alt="设备设置"><br>
<b>设置菜单直接切换语言、波形与灯效</b><br>
<sub>所有新增功能全部融进原厂设置页，没有任何违和感</sub>
</td>
</tr>
</table>

<div align="center">
<img src="./docs/assets/screenshots/zh-CN/06-preferences-enginex-github-update.png" width="70%" alt="碟机在线检查更新"><br>
<b>设置 → 关于 / 更新：碟机一键在线升级</b><br>
<sub>有新版本时直接在碟机屏幕上点「更新」，自动唤起原厂刷机进度条完成升级</sub>
</div>

---

<a id="benchmarks"></a>

## ⚡ 重构后有多轻、多快？（SC5000 实机实测对比）

我们对整个系统做了彻底瘦身与重构，把所有扩展功能浓缩进极小的体积里，**比原厂固件更省内存、响应更快**：

| 对比项 | 官方原厂 / 重构前 | 重构后（`EngineX OS 5.0.4.1` 实机数据） | 实际打碟感受 |
| :--- | :--- | :--- | :--- |
| **刷机包大小 (`*-Update.img`)** | 官方原厂 `173.88 MB`<br>重构前改版 `~210 MB` | **`176.70 MB`**（整套定制功能只占 **`4.1 MB`**） | **只比官方原厂大 `2.8 MB`**，刷机飞快 |
| **纯刷机开机硬盘占用 (`/data`)** | 重构前残留约 `243 MB` | **核心程序 + 三语界面仅占 `17.2 MB`**（含扩展文件系统库共 `45.6 MB`） | 内置闪存多腾出 **近 `200 MB`** 空间缓存歌曲 |
| **整机空闲可用内存（总 `2 GB`）** | `~680 – 780 MB` | **`1,042 MB` 空闲可用**（主程序内存直降 `~150 MB`，后台服务合计仅占 **`29.2 MB`**） | **多释放 `~260 MB` 内存**，连续打碟几小时不卡顿 |
| **声音播放核心被后台抢占率** | 下载或导库时偶发占用 `5% – 25%` | **`0.0%`**：所有下载与分轨任务锁死在独立副核，绝不碰音频核心 | **声音播放最高优先级，96 kHz 全程零断音** |
| **先锋 Rekordbox U 盘读取速度** | 首次转换约 `8 分钟`，插拔重扫 `15–24 秒` | **首次插盘 `< 2 秒`**，再次插盘 **`0.03 秒`** | 上台换班插 U 盘即插即打，不用干等 |
| **4 分钟歌曲 Stems 分轨速度** | SC5000 原厂锁死不可用 | 连电脑 / 云端约 **`35 秒`**；纯靠碟机自己分轨也比播放速度快 **`1.4 倍`** | 演出中途临时想拆一首歌的人声也能马上搞定 |

---

<a id="install"></a>

## 🚀 如何安装与升级？

### 方式一：U 盘刷机（首次安装）
1. 打开 [**Releases 下载页**](https://github.com/Cd1s/EngineX-OS/releases/latest)，下载对应你机器型号的升级包：
   - **Denon DJ SC5000 Prime**：`SC5000-5.0.4.1-EngineX-OS-Denon-SC5000-Prime-JP07-Update.img`
   - **Denon DJ SC5000M Prime**：`SC5000M-5.0.4.1-EngineX-OS-Denon-SC5000M-Prime-JP08-Update.img`
   - **Denon DJ SC6000 Prime**：`SC6000-5.0.4.1-EngineX-OS-Denon-SC6000-Prime-JP13-Update.img`
   - **Denon DJ SC6000M Prime**：`SC6000M-5.0.4.1-EngineX-OS-Denon-SC6000M-Prime-JP14-Update.img`
   - **Denon DJ PRIME 4**：`PRIME4-5.0.4.1-EngineX-OS-Denon-PRIME-4-JC11-Update.img`
   - **Denon DJ PRIME 2**：`PRIME2-5.0.4.1-EngineX-OS-Denon-PRIME-2-JC16-Update.img`
   - **Denon DJ PRIME GO**：`PRIMEGO-5.0.4.1-EngineX-OS-Denon-PRIME-GO-JP11-Update.img`
   - **Numark Mixstream Pro**：`MIXSTREAMPRO-5.0.4.1-EngineX-OS-Numark-Mixstream-Pro-NH08-Update.img`
2. 把下载好的 `.img` 文件直接放到 **FAT32 或 exFAT 格式 U 盘的根目录**。
3. U 盘插上碟机，进入 **设置 (Settings) → 关于 / 更新 (About / Update) → 重启进入固件更新模式**，机器会自动刷入并重启。

### 方式二：碟机屏幕直接在线升级（已刷入本系统后）
- 连上 Wi-Fi 或网线后，打开碟机 **设置 → 关于 / 更新**；
- 检测到新版本后直接点击屏幕上的 **「更新」** 按钮，确认后碟机就会自动进入原厂刷机进度条界面完成下载与升级。

### 选配技巧：用自己的电脑给碟机加速 Stems 分轨
只要电脑和碟机连在同一个 Wi-Fi / 局域网下，在电脑上运行一行命令，碟机就会自动发现你的电脑并用电脑显卡来跑高音质 AI 分轨（无需在碟机上填任何 IP 或密码）：
```sh
# macOS / Linux
tools/stems/run-home-worker.sh

# Windows (PowerShell)
powershell -ExecutionPolicy Bypass -File tools\stems\run-home-worker.ps1 demucs
```

---

<details>
<summary><b>🔧 开发者说明与源码构建（普通 DJ 用户无需查看）</b></summary>

<br>

- 完整 642 条历史问题核查清单见 [`docs/OPEN-SOURCE-FULL-MANIFEST.md`](./docs/OPEN-SOURCE-FULL-MANIFEST.md)。
- 压测与修复记录见 [`docs/ISSUE-FIXES-2026-11-04.md`](./docs/ISSUE-FIXES-2026-11-04.md)。

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

## ☕ 赞赏与支持

如果 **EngineX OS** 让你的碟机更好用、在演出或练歌时帮到了你，欢迎请作者喝杯咖啡或给项目点个 **Star ⭐** 支持后续维护！

<div align="center">
<img src="./docs/assets/sponsor-wechat.jpg" width="260" alt="微信赞赏码"><br>
<sub><b>微信扫码赞赏（Cheek's 赞赏码）</b></sub>
</div>

---

## 📈 Star 增长趋势

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
<sub>MIT License · EngineX OS 是独立的爱好者开源项目，与 inMusic Brands / Denon DJ / Numark 无关。本仓库仅提供开源补丁与工具源码，不分发任何受版权保护的原厂固件文件。</sub>
</div>
