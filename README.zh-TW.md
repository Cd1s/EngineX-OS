<div align="center">

<img src="./docs/assets/hero-banner-zh-TW.svg" alt="EngineX OS 5.0.4.1" width="100%">

<br>

<a href="./README.md"><img src="https://img.shields.io/badge/English-30363d?style=for-the-badge" alt="English"></a>
<a href="./README.zh-CN.md"><img src="https://img.shields.io/badge/%E7%AE%80%E4%BD%93%E4%B8%AD%E6%96%87-30363d?style=for-the-badge" alt="简体中文"></a>
<a href="./README.zh-TW.md"><img src="https://img.shields.io/badge/%E7%B9%81%E9%AB%94%E4%B8%AD%E6%96%87-00e676?style=for-the-badge" alt="繁體中文"></a>

<br>

**[解鎖功能](#features)** · **[刷機包下載](https://github.com/Cd1s/EngineX-OS/releases/latest)** · **[實機畫面](#gallery)** · **[重構與佔用對比](#benchmarks)** · **[如何安裝與升級](#install)**

</div>

<br>

**EngineX OS 5.0.4.1** 是專為天龍 Denon DJ 與 Numark 獨立播放機打造的強化韌體，支援：
`SC5000` · `SC5000M` · `SC6000` · `SC6000M` · `PRIME 4` · `PRIME 2` · `PRIME GO` · `Mixstream Pro`。

操作手感與原廠完全一致，直接用隨身碟刷入即可解鎖以下刷碟功能。

---

<a id="features"></a>

## 🔓 刷完能多做什麼？

### 1. 🎛 全機型解鎖 4 軌 Stems 即時分軌（人聲 / 旋律 / 貝斯 / 鼓組）
官方將獨立 Stems 分軌限制在部分新機型上。刷入本系統後，**SC5000 / SC6000 / PRIME 全系列**都能直接開啟曲庫裡的 `Stems` 播放清單與打擊墊四軌獨立靜音：
- 選歌時往左一滑就能直接拆出 **Vocal（人聲）、Melody（旋律）、Bass（貝斯）、Drums（鼓組）** 四軌；
- 刷碟時直接按打擊墊一鍵抽掉人聲做 Acapella，或單留鼓點混音；
- 不連電腦也能直接用播放機本機分軌；連上家用電腦或雲端伺服器還能跑全速高品質 AI 分軌。

### 2. 🎵 5 大音樂平台直接上播放機（手機掃碼就能登）
不必再每次演出前把歌一首首拷進隨身碟：
- 音源頁直接內建 **網易雲音樂、Spotify、Deezer、SoundCloud、TIDAL**；
- 拿手機掃一下播放機螢幕上的 QR Code 就能登入自己的帳號，直接讀取個人歌單、收藏夾與線上搜歌；
- 支援 **無損 FLAC / 320k MP3** 音質切換，載入過的歌自動快取在機器裡，第二次點開直接秒進，不重複下載。

### 3. 📀 先鋒 Rekordbox 隨身碟插上秒讀（免電腦轉庫）
平時給先鋒 CDJ 匯出的 Rekordbox 隨身碟，直接插上就能打：
- 插碟 **2 秒內**直接讀出全部播放清單、Hot Cue、Memory Cue、Loop 循環點與拍線，不必再去電腦上開 Engine DJ 重新轉庫；
- 不挑隨身碟格式：Windows 的 `FAT32` / `exFAT` / `NTFS`，Mac 的 `HFS+` / `APFS` 插上都能直接讀；
- 自動修復未正常退碟造成的資料庫損毀，並移除惱人的「未安全退出」警告彈窗。

### 4. 🌈 全中文介面 + 先鋒風格 3-Band 波形 + 轉盤燈效
- **完整中文介面**：設定、選單、提示全面支援 **繁體中文 / 简体中文 / English**；
- **3 種波形配色**：除了原廠綠波形，新增高飽和度 **RGB 波形** 與先鋒風格 **3-Band（三頻段藍/橙/白）波形**，高低頻段落一目了然；
- **轉盤彩燈模式**：支援固定顏色、迷幻流光、頻閃，以及**跟隨目前播放 BPM 拍點同步變色**。

### 5. 🔇 切效果器不爆音，長時間演出更穩
- 修正原廠韌體在開關或切換效果器（FX）時偶發的「咔嗒」爆音問題，雙軌同時開效果器也有平滑淡入過渡；
- 關閉原廠背景寫入快閃記憶體的統計回報與無用快取，把系統算力全部留給聲音播放與觸控畫面。

---

<a id="gallery"></a>

## 📸 實機畫面

<table>
<tr>
<td width="50%" align="center">
<img src="./docs/assets/screenshots/zh-TW/01-deck-stems-official-pads.png" width="100%" alt="原生 4 軌 Stems 分軌"><br>
<b>原生 4 軌 Stems 分軌與 3-Band 波形</b><br>
<sub>底部直接顯示 <code>Vocal · Melody · Bass · Drums</code> 四軌狀態，打擊墊一鍵靜音/恢復</sub>
</td>
<td width="50%" align="center">
<img src="./docs/assets/screenshots/zh-TW/02-library-native-stems-list.png" width="100%" alt="曲庫 Stems 播放清單"><br>
<b>曲庫側邊欄解鎖 Stems 專屬播放清單</b><br>
<sub>所有分好軌的歌曲自動歸類，隨時篩選呼叫</sub>
</td>
</tr>
<tr>
<td align="center">
<img src="./docs/assets/screenshots/zh-TW/03-sources-5-streaming-platforms.png" width="100%" alt="五大串流平台"><br>
<b>音源頁內建 5 大音樂平台</b><br>
<sub>網易雲 · Spotify · Deezer · SoundCloud · TIDAL 與隨身碟、SD 卡並列</sub>
</td>
<td align="center">
<img src="./docs/assets/screenshots/zh-TW/04-streaming-qr-login.png" width="100%" alt="手機掃碼登入"><br>
<b>大螢幕 QR Code 掃碼登入</b><br>
<sub>手機掃一下即可同步個人歌單，免輸入複雜密碼</sub>
</td>
</tr>
<tr>
<td align="center">
<img src="./docs/assets/screenshots/zh-TW/05-streaming-playlists-bpm-swipe.png" width="100%" alt="串流播放清單與左滑分軌"><br>
<b>線上歌單直接顯示 BPM / 調性 + 左滑分軌</b><br>
<sub>還沒載入磁軌就能看到曲庫 BPM 與調性，往左滑即可一鍵拆 Stems</sub>
</td>
<td align="center">
<img src="./docs/assets/screenshots/zh-TW/06b-preferences-device.png" width="100%" alt="裝置設定"><br>
<b>設定選單直接切換語言、波形與燈效</b><br>
<sub>所有新增功能完整融入原廠設定頁</sub>
</td>
</tr>
</table>

<div align="center">
<img src="./docs/assets/screenshots/zh-TW/06-preferences-enginex-github-update.png" width="70%" alt="播放機線上檢查更新"><br>
<b>設定 → 關於 / 更新：播放機一鍵線上升級</b><br>
<sub>有新版本時直接在螢幕上點「更新」，自動喚起原廠刷機進度條完成升級</sub>
</div>

---

<a id="benchmarks"></a>

## ⚡ 重構後有多輕、多快？（SC5000 實機實測對比）

我們對整個系統做了徹底瘦身與重構，把所有擴充功能濃縮進極小的體積裡，**比原廠韌體更省記憶體、反應更快**：

| 對比項目 | 官方原廠 / 重構前 | 重構後（`EngineX OS 5.0.4.1` 實機數據） | 實際刷碟感受 |
| :--- | :--- | :--- | :--- |
| **刷機包大小 (`*-Update.img`)** | 官方原廠 `173.88 MB`<br>重構前改版 `~210 MB` | **`176.70 MB`**（整套客製功能只佔 **`4.1 MB`**） | **只比官方原廠大 `2.8 MB`**，刷機飛快 |
| **純刷機開機硬碟佔用 (`/data`)** | 重構前殘留約 `243 MB` | **核心程式 + 三語介面僅佔 `17.2 MB`**（含擴充檔案系統庫共 `45.6 MB`） | 內建快閃記憶體多騰出 **近 `200 MB`** 空間快取歌曲 |
| **整機可用記憶體（總 `2 GB`）** | `~680 – 780 MB` | **`1,042 MB` 空閒可用**（主程式記憶體直降 `~150 MB`，背景服務合計僅佔 **`29.2 MB`**） | **多釋放 `~260 MB` 記憶體**，連續刷碟幾小時不卡頓 |
| **聲音播放核心被背景搶佔率** | 下載或匯入時偶發佔用 `5% – 25%` | **`0.0%`**：所有下載與分軌任務鎖死在獨立副核心，絕不碰音訊核心 | **聲音播放最高優先權，96 kHz 全程零斷音** |
| **先鋒 Rekordbox 隨身碟讀取速度** | 首次轉換約 `8 分鐘`，插拔重掃 `15–24 秒` | **首次插碟 `< 2 秒`**，再次插碟 **`0.03 秒`** | 上台換班插隨身碟即插即打，不必苦等 |
| **4 分鐘歌曲 Stems 分軌速度** | SC5000 原廠鎖死不可用 | 連電腦 / 雲端約 **`35 秒`**；純靠播放機本機分軌也比播放速度快 **`1.4 倍`** | 演出中途臨時想拆一首歌的人聲也能馬上搞定 |

---

<a id="install"></a>

## 🚀 如何安裝與升級？

### 方式一：隨身碟刷機（首次安裝）
1. 開啟 [**Releases 下載頁**](https://github.com/Cd1s/EngineX-OS/releases/latest)，下載對應你機器型號的升級包：
   - **Denon DJ SC5000 Prime**：`SC5000-5.0.4.1-EngineX-OS-Denon-SC5000-Prime-JP07-Update.img`
   - **Denon DJ SC5000M Prime**：`SC5000M-5.0.4.1-EngineX-OS-Denon-SC5000M-Prime-JP08-Update.img`
   - **Denon DJ SC6000 Prime**：`SC6000-5.0.4.1-EngineX-OS-Denon-SC6000-Prime-JP13-Update.img`
   - **Denon DJ SC6000M Prime**：`SC6000M-5.0.4.1-EngineX-OS-Denon-SC6000M-Prime-JP14-Update.img`
   - **Denon DJ PRIME 4**：`PRIME4-5.0.4.1-EngineX-OS-Denon-PRIME-4-JC11-Update.img`
   - **Denon DJ PRIME 2**：`PRIME2-5.0.4.1-EngineX-OS-Denon-PRIME-2-JC16-Update.img`
   - **Denon DJ PRIME GO**：`PRIMEGO-5.0.4.1-EngineX-OS-Denon-PRIME-GO-JP11-Update.img`
   - **Numark Mixstream Pro**：`MIXSTREAMPRO-5.0.4.1-EngineX-OS-Numark-Mixstream-Pro-NH08-Update.img`
2. 將下載好的 `.img` 檔案直接放進 **FAT32 或 exFAT 格式隨身碟的根目錄**。
3. 隨身碟插上播放機，進入 **設定 (Settings) → 關於 / 更新 (About / Update) → 重新啟動進入韌體更新模式**，機器會自動刷入並重啟。

### 方式二：播放機螢幕直接線上升級（已刷入本系統後）
- 連上 Wi-Fi 或網路線後，開啟播放機 **設定 → 關於 / 更新**；
- 偵測到新版本後直接點擊螢幕上的 **「更新」** 按鈕，確認後播放機就會自動進入原廠刷機進度條畫面完成下載與升級。

### 選配技巧：用自己的電腦幫播放機加速 Stems 分軌
只要電腦與播放機連在同一個 Wi-Fi / 區域網路下，在電腦上執行一行指令，播放機就會自動找到你的電腦並用電腦顯示卡來跑高音質 AI 分軌（不必在播放機上輸入任何 IP 或密碼）：
```sh
# macOS / Linux
tools/stems/run-home-worker.sh

# Windows (PowerShell)
powershell -ExecutionPolicy Bypass -File tools\stems\run-home-worker.ps1 demucs
```

---

<details>
<summary><b>🔧 開發者說明與原始碼建置（一般 DJ 使用者無需查看）</b></summary>

<br>

- 完整 642 條歷史問題核查清單見 [`docs/OPEN-SOURCE-FULL-MANIFEST.md`](./docs/OPEN-SOURCE-FULL-MANIFEST.md)。
- 壓測與修復記錄見 [`docs/ISSUE-FIXES-2026-11-04.md`](./docs/ISSUE-FIXES-2026-11-04.md)。

```sh
# 執行測試
go test -race ./...
bash tests/test_rootfs_overlay.sh

# 建置五合一靜態 ARMv7 二進位檔與 C Hook
GOOS=linux GOARCH=arm GOARM=7 CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o out/sc5000-suite ./cmd/sc5000-suite
sh tools/ui-mod/build.sh
sh tools/edisksd-hook/build.sh
```

</details>

---

<div align="center">
<sub>MIT License · EngineX OS 是獨立的社群開源專案，與 inMusic Brands / Denon DJ / Numark 無關。本儲存庫僅提供開源修補與工具原始碼，不散佈任何受版權保護的原廠韌體檔案。</sub>
</div>
