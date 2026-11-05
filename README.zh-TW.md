<div align="center">

<img src="./docs/assets/hero-banner-zh-TW.svg" alt="EngineX OS 5.3.0.2" width="100%">

<br>

<a href="./README.md"><img src="https://img.shields.io/badge/English-30363d?style=for-the-badge" alt="English"></a>
<a href="./README.zh-CN.md"><img src="https://img.shields.io/badge/%E7%AE%80%E4%BD%93%E4%B8%AD%E6%96%87-30363d?style=for-the-badge" alt="简体中文"></a>
<a href="./README.zh-TW.md"><img src="https://img.shields.io/badge/%E7%B9%81%E9%AB%94%E4%B8%AD%E6%96%87-00e676?style=for-the-badge" alt="繁體中文"></a>

<br>

**[核心功能](#features)** · **[韌體下載](#install)** · **[實機畫面](#gallery)** · **[重構與佔用對比](#benchmarks)** · **[讚賞支持](#sponsor)**

</div>

<br>

**EngineX OS 5.3.0.2** 是專為天龍 Denon DJ 與 Numark 獨立播放機打造的強化韌體，支援：
`SC5000` · `SC5000M` · `SC6000` · `SC6000M` · `PRIME 4` · `PRIME 2` · `PRIME GO` · `Mixstream Pro`。

操作介面與原廠完全一致，直接透過隨身碟刷入即可解鎖以下功能。

---

<a id="features"></a>

## 核心功能解鎖

### 1. 全機型解鎖 4 軌 Stems 即時分軌（人聲 / 旋律 / 貝斯 / 鼓組）
突破原廠機型限制，在 **SC5000 / SC6000 / PRIME 全系列**上開啟曲庫側邊欄 `Stems` 播放清單與打擊墊四軌獨立靜音：
- 選歌時向左滑動即可拆出 **Vocal（人聲）、Melody（旋律）、Bass（貝斯）、Drums（鼓組）** 四軌；
- 刷碟時直接按打擊墊一鍵抽掉人聲做 Acapella，或單留鼓點混音；
- 不連電腦可直接由播放機本機完成分軌；同一區域網路下開啟家用電腦或連接雲端伺服器時，自動呼叫顯示卡執行高速 AI 分軌。

### 2. 內建 6 大串流平台（手機掃碼直接登入）
不必每次演出前把新歌一首首拷進隨身碟：
- 音源選單直接內建 **網易雲音樂、Spotify、Deezer、SoundCloud、TIDAL、Beatport**；
- 手機掃描播放機螢幕上的 QR Code 即可登入個人帳號，直接瀏覽個人歌單、收藏夾並線上搜歌；
- 支援 **無損 FLAC / 320k MP3** 音質切換；播放過的曲目自動快取在播放機內部儲存，再次載入直接秒開，絕不重複下載。
- **Beatport** 使用你自己的帳號與訂閱：Professional 下載 FLAC 或 256k AAC，Advanced 下載 128k AAC，Essential 線上播放 128k AAC 串流；Beatport 曲目同樣可以一鍵拆 Stems。

### 3. 先鋒 Rekordbox 隨身碟隨插即用（免電腦轉庫）
平時給先鋒 CDJ 匯出的 Rekordbox 隨身碟，直接插上即可上台演出：
- 插碟 **2 秒內**直接讀取全部播放清單、Hot Cue、Memory Cue、Loop 循環點與拍線，不必再用電腦版 Engine DJ 重新轉庫；
- 相容全格式儲存裝置：Windows 的 `FAT32` / `exFAT` / `NTFS` 與 Mac 的 `HFS+` / `APFS` 插上直讀；
- 自動修復未正常退碟造成的資料庫損毀，並移除「未安全退出」警告彈窗。

### 4. 完整中文介面、先鋒風格 3-Band 波形與轉盤燈效
- **三語介面**：設定、選單與提示全面支援 **繁體中文 / 简体中文 / English**；
- **3 種波形配色**：在原廠綠波形基礎上，新增高對比度 **RGB 波形** 與先鋒風格 **3-Band（三頻段藍/橙/白）波形**；
- **轉盤彩燈模式**：支援固定顏色、流光漸變、頻閃，以及**跟隨目前播放 BPM 拍點同步變色**。

### 5. 切效果器零爆音，長時間演出更穩定
- 消除原廠韌體在開關或切換效果器（FX）時偶發的「咔嗒」爆音，雙軌同時開效果器也有平滑淡入過渡；
- 預設關閉原廠背景匿名統計回報與無用磁碟快取，把全部算力與記憶體留給音訊播放與螢幕觸控。

### 6. 智慧同步與 Gigabit 有線網路
- **智慧同步（Smart Sync）**：下拉控制中心一鍵切換 **Bar / Beat / Tempo** 同步模式；開啟 Sync 後，兩首曲目 BPM 差距超出目前變速範圍時，自動把 Pitch Range 升檔到足夠為止（最高 `±100%`）；
- **Gigabit 有線網路**：有線網路孔從原廠限制的 `100 Mbps` 解鎖至 **1 Gbps（1000 Mbps）全雙工滿速**，並配 8 MiB TCP 大視窗緩衝；
- **Wi-Fi**：播放機內建 Wi-Fi 模組受限於主機板 SDIO 25 MHz SDR12 匯流排頻寬，實測極限約 `9～10 MB/s`，大容量曲庫傳輸建議使用 Gigabit 有線網路孔。


### 7. 5.3.0.2 新功能
- **OmniSource 統一瀏覽**：隨身碟 / SD 卡裡的歌單和曲目直接顯示在串流瀏覽器中，和串流歌單放在一起，一次搜尋兩邊都找得到；隨身碟曲目直接從碟上讀取播放，音質不打折（`設定 → OmniSource`，預設關閉）；
- **先鋒 CDJ / Pro DJ Link（測試版）**：CDJ-3000 / CDJ-2000NXS2 接入 Line-In 通道時同步顯示它的波形與拍線；透過區域網路 / Wi-Fi 讀取 CDJ 上 rekordbox 隨身碟的曲庫（`Pro DJ Link 曲庫`）；把本機曲庫分享給 CDJ 為實驗功能。以上預設全部關閉；
- **更乾淨的聲音**：`主輸出軟限幅` 以平滑曲線把主輸出壓在 -0.1 dBFS 以下，零延遲，-1.5 dBFS 以下的訊號原樣不動；Flanger 不再讓音量下沉（自動補償到原曲電平，最多 +6 dB）；`推桿切歌自動回音` 在拉下推桿時留下回音尾巴；`自動增益（0 dB 正規化）` 把音量偏小的歌平滑地提到接近 0 dBFS（最多 +9 dB，-42 dBFS 以下的靜音不提升），之後一定經過軟限幅，不會爆音（預設關閉）。共 21 個即時 DSP 掛點，不增加任何延遲；
- **四路波形顯示**，以及向 Shoutcast / Icecast 伺服器 **直播推流**；
- **曲庫**：`自動分析插入的裝置` 在背景分析新插入的隨身碟 / SD 卡上所有未分析的曲目，使用最低優先權，不影響播放；rekordbox 匯出的隨身碟（包含 6.8.5）即使有個別檔案損壞也能正常匯入，那幾首在載入時再分析；選取其他串流服務時，不再跳出 Engine 5.3.0 的「Connect to your Engine DJ Profile to use Tidal」提示。

---

<a id="gallery"></a>

## 實機畫面

<table>
<tr>
<td width="50%" align="center" valign="top">
<img src="./docs/assets/screenshots/zh-TW/01-deck-stems-official-pads.png" width="100%" alt="原生 4 軌 Stems 分軌"><br>
<b>原生 4 軌 Stems 分軌與 3-Band 波形</b><br>
<sub>底部顯示人聲、旋律、貝斯、鼓組四軌狀態，打擊墊一鍵靜音</sub>
</td>
<td width="50%" align="center" valign="top">
<img src="./docs/assets/screenshots/zh-TW/02-library-native-stems-list.png" width="100%" alt="曲庫 Stems 播放清單"><br>
<b>曲庫側邊欄 Stems 專屬播放清單</b><br>
<sub>所有已分軌曲目自動歸類，隨時篩選呼叫</sub>
</td>
</tr>
<tr>
<td width="50%" align="center" valign="top">
<img src="./docs/assets/screenshots/zh-TW/03-sources-5-streaming-platforms.png" width="100%" alt="五大串流平台"><br>
<b>音源頁內建 5 大音樂平台</b><br>
<sub>網易雲 · Spotify · Deezer · SoundCloud · TIDAL 與隨身碟並列</sub>
</td>
<td width="50%" align="center" valign="top">
<img src="./docs/assets/screenshots/zh-TW/04-streaming-qr-login.png" width="100%" alt="手機掃碼登入"><br>
<b>螢幕 QR Code 掃碼登入</b><br>
<sub>手機掃一掃直接同步個人歌單，免輸入密碼</sub>
</td>
</tr>
<tr>
<td width="50%" align="center" valign="top">
<img src="./docs/assets/screenshots/zh-TW/05-streaming-playlists-bpm-swipe.png" width="100%" alt="串流播放清單與左滑分軌"><br>
<b>線上歌單 BPM / 調性預覽與左滑分軌</b><br>
<sub>載入前即可查看曲目 BPM 與調性，左滑一鍵拆分 Stems</sub>
</td>
<td width="50%" align="center" valign="top">
<img src="./docs/assets/screenshots/zh-TW/06b-preferences-device.png" width="100%" alt="裝置設定"><br>
<b>裝置設定選單</b><br>
<sub>直接切換語言、波形樣式、串流音質與轉盤燈效</sub>
</td>
</tr>
<tr>
<td width="50%" align="center" valign="top">
<img src="./docs/assets/screenshots/zh-TW/06-preferences-enginex-github-update.png" width="100%" alt="播放機線上檢查更新"><br>
<b>設定 → 關於 / 更新：播放機螢幕一鍵線上升級</b><br>
<sub>偵測到新版本時直接點擊螢幕上的「更新」，自動拉起原廠刷機進度條完成升級</sub>
</td>
<td width="50%" align="center" valign="top">
<img src="./docs/assets/screenshots/zh-TW/07-bluetooth-audio.png" width="100%" alt="原生藍牙音訊"><br>
<b>原生藍牙音訊輸入與播放控制</b><br>
<sub>手機藍牙串流到播放機，螢幕顯示曲目資訊，可直接播放/暫停與切歌</sub>
</td>
</tr>
</table>

---

<a id="benchmarks"></a>

## 重構前後效能與佔用對比（SC5000 實機實測）

系統經過全面重構與瘦身，所有新增功能僅佔極小空間，比原廠韌體留出更多可用記憶體：

| 對比項目 | 官方原廠 / 重構前 | 重構後（`EngineX OS 5.3.0.2` 實測） | 實際刷碟提升 |
| :--- | :--- | :--- | :--- |
| **刷機包體積 (`*-Update.img`)** | 官方原廠 5.3.0 `147.79 MB`<br>重構前改版 `~210 MB` | **`151.44 MB`**（SC6000 `151.58 MB`，PRIME 4 `151.52 MB`） | **僅比官方原廠大 `3.64 MB`** |
| **純刷機開機硬碟佔用 (`/data`)** | 重構前殘留約 `243 MB` | **核心程式 + 三語介面僅佔 `17.2 MB`**（含擴充檔案系統庫共 `45.6 MB`） | 內建快閃記憶體多騰出 **近 `200 MB`** 空間 |
| **整機可用記憶體（總 `2 GB`）** | `~680 – 780 MB` | **`1,042 MB` 空閒可用**（主程式記憶體直降 `~150 MB`，背景服務合計僅佔 **`29.2 MB`**） | **多釋放 `~260 MB` 記憶體**，長時間演出不卡頓 |
| **音訊核心被背景搶佔率** | 下載或匯入時偶發佔用 `5% – 25%` | **`0.0%`**：所有下載與分軌任務隔離在獨立副核心，不碰音訊核心 | **96 kHz 聲音播放全程零斷音** |
| **先鋒 Rekordbox 隨身碟讀取速度** | 首次轉換約 `8 分鐘`，重掃 `15–24 秒` | **首次插碟 `< 2 秒`**，再次插碟 **`0.03 秒`** | 插上隨身碟即插即打，無需等待 |
| **4 分鐘歌曲 Stems 分軌耗時** | SC5000 原廠鎖死不可用 | 連電腦 / 雲端約 **`35 秒`**；播放機本機分軌比播放速度快 **`1.4 倍`** | 演出中途臨時拆人聲也能快速完成 |
| **大 BPM 差距同步** | BPM 差距超出變速範圍時 Sync 對不上，需要手動調大 Pitch Range | **智慧同步**自動升檔 Pitch Range（最高 `±100%`），控制中心一鍵切 Bar / Beat / Tempo | 速度相差很大的兩首歌也能直接同步 |
| **網路傳輸速度** | 有線網路孔限制在 `100 Mbps` | 有線網路孔 **`1 Gbps`（1000 Mbps）全雙工**，8 MiB TCP 緩衝；Wi-Fi 實測約 `9～10 MB/s`（SDIO 25 MHz SDR12 匯流排） | 大曲庫走 Gigabit 有線網路孔傳輸 |

---

<a id="install"></a>

## 如何安裝與升級

### 方式一：隨身碟刷機（首次安裝）
1. 點擊下方對應你機器型號的連結，直接下載升級包（或前往 [**Releases 頁面**](https://github.com/Cd1s/EngineX-OS/releases/latest)）：
   - **Denon DJ SC5000 Prime**：[`SC5000-5.3.0.2-EngineX-OS-Denon-SC5000-Prime-JP07-Update.img`](https://github.com/Cd1s/EngineX-OS/releases/latest/download/SC5000-5.3.0.2-EngineX-OS-Denon-SC5000-Prime-JP07-Update.img)
   - **Denon DJ SC5000M Prime**：[`SC5000M-5.3.0.2-EngineX-OS-Denon-SC5000M-Prime-JP08-Update.img`](https://github.com/Cd1s/EngineX-OS/releases/latest/download/SC5000M-5.3.0.2-EngineX-OS-Denon-SC5000M-Prime-JP08-Update.img)
   - **Denon DJ SC6000 Prime**：[`SC6000-5.3.0.2-EngineX-OS-Denon-SC6000-Prime-JP13-Update.img`](https://github.com/Cd1s/EngineX-OS/releases/latest/download/SC6000-5.3.0.2-EngineX-OS-Denon-SC6000-Prime-JP13-Update.img)
   - **Denon DJ SC6000M Prime**：[`SC6000M-5.3.0.2-EngineX-OS-Denon-SC6000M-Prime-JP14-Update.img`](https://github.com/Cd1s/EngineX-OS/releases/latest/download/SC6000M-5.3.0.2-EngineX-OS-Denon-SC6000M-Prime-JP14-Update.img)
   - **Denon DJ PRIME 4**：[`PRIME4-5.3.0.2-EngineX-OS-Denon-PRIME-4-JC11-Update.img`](https://github.com/Cd1s/EngineX-OS/releases/latest/download/PRIME4-5.3.0.2-EngineX-OS-Denon-PRIME-4-JC11-Update.img)
   - **Denon DJ PRIME 2**：[`PRIME2-5.3.0.2-EngineX-OS-Denon-PRIME-2-JC16-Update.img`](https://github.com/Cd1s/EngineX-OS/releases/latest/download/PRIME2-5.3.0.2-EngineX-OS-Denon-PRIME-2-JC16-Update.img)
   - **Denon DJ PRIME GO**：[`PRIMEGO-5.3.0.2-EngineX-OS-Denon-PRIME-GO-JP11-Update.img`](https://github.com/Cd1s/EngineX-OS/releases/latest/download/PRIMEGO-5.3.0.2-EngineX-OS-Denon-PRIME-GO-JP11-Update.img)
   - **Numark Mixstream Pro**：[`MIXSTREAMPRO-5.3.0.2-EngineX-OS-Numark-Mixstream-Pro-NH08-Update.img`](https://github.com/Cd1s/EngineX-OS/releases/latest/download/MIXSTREAMPRO-5.3.0.2-EngineX-OS-Numark-Mixstream-Pro-NH08-Update.img)
2. 將下載好的 `.img` 檔案放進 **FAT32 或 exFAT 格式隨身碟的根目錄**。
3. 將隨身碟插入播放機（原廠 Engine OS 4.3.x 與 5.0.x 都可以直接刷），進入 **設定 (Settings) → 關於 / 更新 (About / Update) → 韌體更新模式 (Firmware Update Mode)**，看到 `Found 5.3.0.2 on USB/SD` 時選 **USB/SD**，機器會自動完成刷入並重啟。如果只跳出 *Reboot into firmware update mode?*，代表播放機沒找到檔案：核對機型，檔名保持原樣放在根目錄，再拔插一次隨身碟。

### 方式二：播放機螢幕一鍵線上升級（已安裝本系統後）
- 連接 Wi-Fi 或網路線後，開啟播放機 **設定 → 關於 / 更新**；
- 偵測到新版本時直接點擊螢幕上的 **「更新」** 按鈕，確認後播放機將自動進入原廠刷機進度條畫面完成下載與升級。

### 選配：用電腦幫播放機加速 Stems 分軌
當電腦與播放機處於同一 Wi-Fi / 區域網路時，在電腦上執行以下指令，播放機即可自動找到電腦並呼叫電腦顯示卡進行高速 AI 分軌（不必在播放機上輸入 IP 或密碼）：
```sh
# macOS / Linux
tools/stems/run-home-worker.sh

# Windows (PowerShell)
powershell -ExecutionPolicy Bypass -File tools\stems\run-home-worker.ps1 demucs
```

### 串流登入：只有播放機也能用
不需要電腦，也不需要伺服器。播放機和手機連同一個 Wi-Fi，在音源選單點開平台，用手機掃螢幕上的 QR Code 即可。手機上的登入頁預設英文，右上角可切換简体中文或繁體中文，每個頁面都有分步教學。

| 平台 | 怎麼登入 |
| --- | --- |
| 網易雲音樂 | 用網易雲音樂 App 掃播放機螢幕上的 QR Code 並確認。QR Code 過期後點一下就會換新的。 |
| Spotify | 第 1 步：點「開啟 Spotify 登入」並同意。第 2 步：隨後會開啟一個以 `127.0.0.1` 開頭、無法連線的頁面，複製它的網址，點「貼上並連接」。 |
| Deezer | 貼上你的 **ARL**（頁面教學示範怎樣在電腦上從 deezer.com 複製）。同一次登入也為 Spotify、網易雲和 Deezer 曲目開啟 Deemix 無損 FLAC 下載。只填個人頁面連結則只顯示公開歌單、不能下載。 |
| SoundCloud | 填使用者名稱或主頁連結即可，不用密碼、不用 Artist Pro、也不用申請 API。私人歌單在「進階」裡貼上 soundcloud.com 的 `oauth_token`。 |
| Beatport | 填 Beatport 電子郵件和密碼，播放機只儲存登入權杖。 |

### 選配：自建登入中繼
中繼讓用行動網路的手機也能完成登入，並提供 Deezer 網頁內登入。在任何有公網 HTTPS 位址的伺服器上執行，然後在播放機網頁控制台（`http://<播放機 IP>:8080`）填入它的位址和密碼：
```sh
go build -o sc5000-relay ./cmd/sc5000-relay
./sc5000-relay -public https://relay.example.com -listen 0.0.0.0:6595
# 首次控制台密碼寫在 /var/lib/sc5000-relay/initial-password
```

---

## 安全說明：演出現場公共網路防護

EngineX OS 依照「和陌生人共用酒吧、Club、音樂節 Wi-Fi」的情境做了安全設計。

- **沒有任何預設 SSH 密碼。** 密碼登入已徹底關閉，公開韌體也**不內建任何 SSH 公鑰**，你自己沒放公鑰之前 SSH 根本不開放。用過舊版公開韌體的播放機，升級到 5.3.0.2 時會自動清除舊韌體帶的那把公鑰。
- **同一個 Wi-Fi 下的人控制不了你的播放機。** 播放機內部使用的所有介面（播放控制、切換波形、切換語言、線上升級、串流登入、Stems）只接受播放機自己（`127.0.0.1`）呼叫，別人就算和你連同一個網路，也無法遠端切你的歌、改設定或重新啟動機器。
- **手機網頁控制台必須輸入你這台播放機獨有的存取碼。** 開啟 `http://<播放機IP>:8080` 時要輸入播放機隨機產生的存取碼；串流掃碼登入使用的是會過期的一次性連結。

進階使用者：

- 想用 SSH：把你自己的公鑰放進 `/data/sc5000-custom/authorized_keys`（例如自行建置帶公鑰的私有映像檔），重新開機後生效。只能用金鑰登入，不能用密碼。
- 控制台存取碼儲存在 `/data/sc5000-custom/state/access-token`，刪除這個檔案再重新開機就會產生新碼。
- 在不信任的網路演出時，隨身碟、SD 卡和藍牙播放都不需要連網，可以直接不連 Wi-Fi。

---

<details>
<summary><b>開發者說明與原始碼建置（一般 DJ 使用者無需查看）</b></summary>

<br>

- 完整 642 條歷史問題核查清單：[`docs/OPEN-SOURCE-FULL-MANIFEST.md`](./docs/OPEN-SOURCE-FULL-MANIFEST.md)
- 壓測與修復記錄：[`docs/ISSUE-FIXES-2026-11-04.md`](./docs/ISSUE-FIXES-2026-11-04.md)
- 貢獻與發版規範：[`CONTRIBUTING.md`](./CONTRIBUTING.md)

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

<a id="sponsor"></a>

## 讚賞與支持

如果 **EngineX OS** 讓你的播放機更好用、在演出或練歌時幫到了你，歡迎讚賞支持或給專案點個 **Star**！

<table>
<tr>
<td width="33%" align="center" valign="top">
<img src="./docs/assets/sponsor-wechat.jpg" height="260" alt="微信讚賞碼"><br>
<b>微信讚賞碼</b><br>
<sub>微信掃一掃直接讚賞</sub>
</td>
<td width="33%" align="center" valign="top">
<img src="./docs/assets/sponsor-binance-pay.jpg" height="260" alt="Binance Pay 幣安收款碼"><br>
<b>幣安支付 (Binance Pay)</b><br>
<sub>幣安掃碼或轉幣安 ID：<code>565380652</code>（0 手續費）</sub>
</td>
<td width="33%" align="center" valign="top">
<img src="./docs/assets/sponsor-evm-usdt.jpg" height="260" alt="USDT EVM 收款碼"><br>
<b>USDT / USDC（幣安 EVM 全鏈通用）</b><br>
<sub>支援 BSC · ERC20 · Arbitrum · Optimism · Polygon · Base · opBNB</sub>
</td>
</tr>
</table>

> **幣安 ID (Binance UID，幣安內轉帳 0 手續費，點擊右側一鍵複製)：**
> ```text
> 565380652
> ```
>
> **USDT / USDC 鏈上收款位址（支援幣安所有 EVM 網路：`BSC (BEP-20)` · `Ethereum (ERC-20)` · `Arbitrum One` · `Optimism` · `Polygon` · `Base` · `opBNB` · `Avalanche C-Chain` · `zkSync Era` · `Scroll` · `Linea`）：**
> ```text
> 0x35e0c53dd887b37cd5db0064291f7c5a7442bc91
> ```

---

## Star 成長趨勢

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
<sub>MIT License · EngineX OS 是獨立的社群開源專案，與 inMusic Brands / Denon DJ / Numark 無關。本儲存庫僅提供開源修補與工具原始碼，不散佈任何受版權保護的原廠韌體檔案。</sub>
</div>
