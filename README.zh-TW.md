# EngineX OS — 天龍 Denon DJ Engine OS 5.0.4 全機型通用深度定制系統與高性能後端套件

<p align="center">
  <a href="./README.md">English</a> · <a href="./README.zh-CN.md">简体中文</a> · <b>繁體中文</b> · <a href="./docs/OPEN-SOURCE-FULL-MANIFEST.md">完整開源技術白皮書與 642 條 Bug 核查總表</a>
</p>

<p align="center">
  <img src="https://img.shields.io/badge/基線韌體-Engine_OS_5.0.4-0078ff?style=flat-square" alt="Base Firmware" />
  <img src="https://img.shields.io/badge/即時核心-Linux_6.6.119--rt_(PREEMPT__RT)-00e632?style=flat-square" alt="Kernel" />
  <img src="https://img.shields.io/badge/支援機型-SC5000_%7C_SC6000_%7C_PRIME_4_%7C_PRIME_GO_%7C_SC_LIVE_2-ff8800?style=flat-square" alt="Models" />
  <img src="https://img.shields.io/badge/雙版本架構-本地無伺服器版_(Standalone)_%26_伺服器加速版-5ce4d5?style=flat-square" alt="Editions" />
</p>

**EngineX OS** (`sc5000-custom-os`) 是面向天龍 Denon DJ **Engine OS 5.0.4** 全系獨立播放機與一體機（`SC5000`、`SC5000M`、`PRIME GO`、`SC6000`、`SC6000M`、`PRIME 4`、`SC LIVE 2`）打造的開源定制系統與底層後端重構套件。

核心設計憲法：
> **100% 保留原廠 QML 打碟介面直覺（零學習成本、零阻斷彈窗）+ 外科手術式替換 Linux & C++ 低效後端。**
> 任何習慣 CDJ 或 Denon 的 DJ 走上台看到的都是原汁原味的原廠介面；而在底層，拖慢機器的原廠慢速匯入器、FAT32/exFAT 同步 `fsync` 磁碟鎖、高頻快閃記憶體遙測寫入、408MHz 降頻斷音與機型功能閹割，已被高性能 C `LD_PRELOAD` 記憶體鉤子與 CPU 硬隔離的 Go 守護行程徹底重構。

---

## 一、實機真實功能截圖（SC5000 `JP07` 硬體 KMS `drmgrab` 實拍）

| 原廠 4 軌 Stems 播放與底部原廠消音欄 | 曲庫側邊欄第 0 行原生 `Stems` 歌單 (`0x6d3e00`) |
| :---: | :---: |
| ![Deck Stems Pads Bar](./docs/assets/screenshots/01-deck-stems-official-pads.png) | ![Library Native Stems List](./docs/assets/screenshots/02-library-native-stems-list.png) |
| *100% 原廠 UI 佈局：先鋒 3-Band 波形、Rekordbox 總覽波形自適應拉伸與螢幕底部原廠 `[ Vocal ] [ Melody ] [ Bass ] [ Drums ]` 分軌控制欄* | *解鎖側邊欄第 0 行原廠 `Stems` 列表 (`typeRole = 5`) 與曲目雙軌圖示* |

| 原廠來源頁整合 5 大串流媒體平台 | 播放機螢幕掃碼登入彈窗 |
| :---: | :---: |
| ![5 Streaming Platforms](./docs/assets/screenshots/03-sources-5-streaming-platforms.png) | ![QR Code Login Modal](./docs/assets/screenshots/04-streaming-qr-login.png) |
| *同螢幕支援 USB 3、SD 卡與網易雲音樂、Spotify、Deezer、SoundCloud、TIDAL* | *支援本地無伺服器區域網路掃碼 (`http://<播放機IP>:8080`) 與自建雲端中繼掃碼* |

| 串流媒體獨立 `Stems` 列表、`[BPM / Key]` 預覽與左滑 `Stems` | `EngineX OS` 裝置偏好設定頁（語言、波形、音質、燈效、藍牙） |
| :---: | :---: |
| ![Streaming Playlists & Swipe Stems](./docs/assets/screenshots/05-streaming-playlists-bpm-swipe.png) | ![EngineX OS Device Preferences](./docs/assets/screenshots/06b-preferences-device.png) |
| *四大串流平台各自擁有獨立的第 0 行 `Stems` 列表（互不串列表、不混入隨身碟）、自帶速度/調性預覽欄與左滑藍色 `Stems` 按鈕* | *支援簡/繁/英三語切換、Denon/RGB/3-Band 波形、無損/320k 音質、轉盤燈效與藍牙音訊開關* |

| `EngineX OS 5.0.4` GitHub 倉庫韌體更新檢測（`關於 / 更新`） |
| :---: |
| ![EngineX OS GitHub Update](./docs/assets/screenshots/06-preferences-enginex-github-update.png) |
| *屏蔽原廠 `autoupdate.airmusictech.com` 升級彈窗，直接檢測你的 GitHub 倉庫（`Cd1s/sc5000-custom-os`）* |

---

## 二、雙版本 / 雙模式架構：**本地版（無伺服器 Standalone）** vs **伺服器版（Server Edition）**

針對大量沒有雲端伺服器（VPS）的使用者，**EngineX OS** 將**「本地無伺服器版（Standalone）」與「伺服器加速版（Server-Assisted）」融合在同一套自適應韌體中**：不設定任何伺服器時自動以 **100% 純本機模式**運行；在控制台填入自建伺服器或家裡電腦區域網路位址後，自動升級為**伺服器加速模式**。

### 本地版（無伺服器） vs 伺服器版 功能完整對比表

| 功能模組 | **本地版（無伺服器 Standalone Edition）** | **伺服器版（自建 VPS 或家裡電腦 Server Edition）** | 兩者差異說明 |
| :--- | :--- | :--- | :--- |
| **隨身碟/SD 卡現成 `.stems` 播放與底部 4 軌控制欄** | ✅ **100% 完整支援** | ✅ **100% 完整支援** | 完全一致（`0x842644` + `0x6d3e00` C Hook）。 |
| **隨身碟與串流曲目即時 Stems 分軌計算** | ✅ **支援純本機分軌 (`internal/stems/local.go`)**<br>未設定伺服器時，`stems-processor` 自動鎖在播放機 `CPU3` (`SCHED_IDLE` + `nice 19`) 運行純 Go 因果 IIR 頻帶 + Mid/Side 四軌分離器（**實測 60 秒音訊在播放機上僅需 `2.48 秒` = `24.2×` 即時**，四軌相加 100% 還原原曲，不搶音訊核心！）。 | ✅ **支援遠端高精度分軌 (`worker.py`)**<br>透過 `X-Stems-Framing: 2`（雙向 `int16` + 3 軌殘差還原）串流交由遠端 `sc5000-stemsd` 執行 2048 點 STFT + 二維諧波/打擊樂中值濾波（**Wi-Fi 實測 `6.8×` 即時，算力峰值 `33.8×` 即時**）。 | **演算法精度 vs 零外部硬體**：本地版用輕量級即時 IIR + 中側頻帶分離實現零伺服器機內分軌；伺服器/家裡電腦版運行高算力 2D STFT 諧波/打擊樂中值分離。 |
| **四大串流平台各自獨立的第 0 行 `Stems` 歌單** | ✅ **100% 完整支援** | ✅ **100% 完整支援** | 完全一致（保存在本機 `state/streaming-stems.json`）。 |
| **網易雲音樂（掃碼登入 + 無損 FLAC / 320k 下載）** | ✅ **100% 完整支援（完全不需要伺服器）**<br>手機網易雲 App 直接掃播放機螢幕 QR Code（支援 4G/5G），曲目由播放機直接向網易雲官方下載無損 FLAC / 320k MP3。 | ✅ **100% 完整支援** | 完全一致。 |
| **Deezer（公開歌單 + ARL 登入 + 本機無損下載）** | ✅ **100% 支援（區域網路掃碼 + 本機 `sc5000-deemixd`）**<br>公開榜單免登入即用；手機連同一 Wi-Fi 掃播放機 QR Code (`http://<播放機IP>:8080/m/deezer`) 填入個人頁連結或 `arl` Cookie，音訊由播放機本機 `sc5000-deemixd` (`127.0.0.1:6595`) 直接下載解密無損 FLAC。 | ✅ **100% 支援（跨網 4G/5G 掃碼 + 雲端瀏覽器帳密登入）**<br>額外支援手機在 4G/5G 下跨網掃碼，並提供雲端無頭瀏覽器直接輸入 Deezer 信箱密碼自動提取 ARL。 | **兩個版本的音訊下載與解密 100% 都在播放機本機完成**；伺服器版僅多出跨網掃碼與雲端瀏覽器填帳密輔助。 |
| **Spotify & SoundCloud** | ✅ **100% 支援（區域網路掃碼綁定）**<br>手機連同一 Wi-Fi 掃播放機 QR Code 打開本機綁定頁即可完成授權；音訊全部在播放機本機下載。 | ✅ **100% 支援（4G/5G 自動 HTTPS OAuth 回呼）**<br>手機無需連同一 Wi-Fi，掃碼同意後自動透過 HTTPS 回呼完成綁定。 | 本地版透過同一 Wi-Fi 區域網路頁面綁定；伺服器版支援 4G/5G 自動 HTTPS 回呼。 |
| **先鋒 Rekordbox 零掃描秒讀 (`rb-bridge`)** | ✅ **100% 完整支援** | ✅ **100% 完整支援** | 完全一致（純機內掛載鉤子）。 |
| **資料庫自癒 (`dbhealer`) 與 SQLite 記憶體加速** | ✅ **100% 完整支援** | ✅ **100% 完整支援** | 完全一致。 |
| **642 條 Bug 防禦、1.2GHz 保底、三語/RGB/3-Band 與 GitHub 更新檢測** | ✅ **100% 完整支援** | ✅ **100% 完整支援** | 完全一致。 |

> **💡 家裡有電腦（Mac / Windows / Linux），沒有雲端伺服器，能用家裡電腦算 Stems 嗎？**
> **完全可以！** 只要家裡電腦和播放機在同一個路由器/Wi-Fi 下，在電腦上運行本倉庫的 `sc5000-stemsd`（或 `sc5000-relay`），然後在播放機網頁控制台 `http://sc5000.lan:8080` 把位址填成 `http://<電腦區域網路IP>:6595`（或設環境變數 `SC5000_STEMS_URL=http://<電腦區域網路IP>:6596`），播放機就會直接調用家裡電腦的 CPU/GPU 算 Stems！我們也建立了 [**Issue #1**](https://github.com/Cd1s/sc5000-custom-os/issues/1) 追蹤零設定區域網路 mDNS 自動探索與 GPU Demucs 模型支援。

---

## 三、修改前 vs 修改後：實機性能對比基準表

所有性能數據均來自 **Denon SC5000 Prime (`JP07`)** 實機測量：

| # | 性能指標 | 修改前（官方原版 Engine OS 5.0.4 / 初版基線） | 修改後（EngineX OS 5.0.4 實機實測） | 性能提升幅度 |
| :-: | :--- | :--- | :--- | :--- |
| **1** | **Stems 分軌耗時與即時倍率**<br>*(4 分鐘 / 240 秒曲目)* | • **原廠**：SC5000 (`JP07`) 被軟體閹割鎖死，零即時分軌算力。<br>• **初版未優化**：耗時 `394.1 秒`（**0.61× 即時**），且結束後必卡死 `120 秒` `i/o timeout`。 | • **伺服器版（`X-Stems-Framing: 2` 雙向 `int16` + 批次中值濾波）**：整曲僅需 **`35.1 秒`（6.8× 即時**，伺服器端計算峰值 **33.8× 即時**，收尾 **0.04 秒**）。<br>• **本地版（播放機 `CPU3` `SCHED_IDLE` 本機分離）**：60 秒音訊僅需 **`2.48 秒`（24.2× 即時）**。 | **提速 11.1 倍 – 39 倍**<br>+ **徹底消滅 120 秒收尾假死** |
| **2** | **Rekordbox 大曲庫首次掛載與二次增量同步**<br>*(~6,000 首曲目隨身碟)* | • **首次插入**：機內逐首重掃音訊算波形耗時 **~8 分鐘**，極易寫壞 `m.db`。<br>• **二次插入（無改動）**：每次掃描耗時 **~15–24 秒**。<br>• **二次插入（新增歌曲 `GL-011`）**：超過 `edisksd` 20 秒掛載預算被強殺回滾，**21/21 測試新增歌曲匯入失敗**。 | • **首次插入 (`>500` 首大庫)**：**`< 2 秒`**（在 `tmpfs` 記憶體盤直接轉換匯出庫，不讀音訊檔案，100% 不碰 `PIONEER/` 原目錄）。<br>• **二次插入（無改動）**：憑 `.rb-bridge-stamp` **`30 毫秒 (0.03s)`** 瞬間放行。<br>• **二次插入（新增歌曲/歌單）**：在 `tmpfs` 上 **`< 1 秒`** 完成增量同步（**成功率 100%**）。 | **二次掛載提速 ~800 倍 (`24s` $\rightarrow$ `0.03s`)**<br>+ **大庫增量同步 100% 成功** |
| **3** | **SoundCloud 106 MB 長 Set 下載耗時**<br>*(1 小時 50 分鐘 DJ Set)* | • **原廠**：僅線上串流，不支援本地快取，慢網易斷音。<br>• **8 路分批基線**：耗時 `60.3 秒`。 | **16 路滑動視窗 Range 平行分片下載**（2 MB 切片，鎖定在 `CPU3` `SCHED_IDLE`）：耗時縮短至 **`43.7 – 45.8 秒`**（跑滿 SC5000 硬體網卡極限 `~2.3 MB/s`）。 | **提速 27.5% (`60.3s` $\rightarrow$ `43.7s`)**<br>跑滿硬體物理頻寬極限 |
| **4** | **SQLite 交易耗時與內部 eMMC 快閃記憶體遙測寫入量** | • **SQLite**：預設 `synchronous=FULL` + 磁碟臨時表，每次打 Cue / 寫歷史在 FAT32/exFAT 上反覆建刪 `-journal` 並同步 `fsync` 阻塞主執行緒。<br>• **遙測**：每次按鍵寫 `/data/.../posthog.db` 與 `events.db` + 4 個 `J:I:Takingfreez` 執行緒每 20 秒刷盤。 | • **SQLite Hook (`0x1cba858`/`0x1cba874`)**：自動注入 `PRAGMA synchronous=NORMAL; temp_store=MEMORY; cache_size=-8000;`（8 MB RAM 頁快取 + 記憶體臨時表）。<br>• **遙測清零**：`posthog.db`/`events.db` 重新導向至 `":memory:"`，`crashpad_handler` 直接短路 (`ENOENT`)。<br>• **實測快閃記憶體寫入**：30 秒高頻切歌下 eMMC 遙測寫入增量為 **`0 位元組`**。 | **快閃記憶體遙測寫入降低 100% (`0 B`)**<br>+ **磁碟 `fsync` 阻塞降低 >95%** |
| **5** | **ALSA 即時音訊 XRUN 斷音率**<br>*(96kHz / 450 幀 / 4.68ms 週期)* | • 閒置時 CPU 降頻至 `126 MHz – 408 MHz`，切歌或解碼大圖瞬間升頻不及引發 ALSA XRUN 爆音。<br>• GPU、USB 與網卡中斷全擠在 `CPU1`；背景分析與封面解碼搶佔 `CPU0`。 | • **CPU 保底鎖頻 `1.2 GHz`**（`ondemand`, `up_threshold=50`, `sampling_down_factor=10`，穩定 `~72°C`）；GPU 設 `simple_ondemand` 低頻讓出熱設計功耗。<br>• **四核硬隔離**：`CPU0` 獨佔 ALSA `ffb20000` 中斷 (`FIFO 99`, `workqueue/cpumask=6`)；`CPU1` 獨佔 GPU/UI；`CPU2` 獨佔 USB/SATA/網卡中斷；`CPU3` 隔離運行 `Analysis Queue`/`Freeze Checker`/`J:I:Takingfreez`/`Dropbox*` (`SCHED_IDLE` + `IOPRIO_CLASS_IDLE`)。 | **ALSA XRUN 斷音率降至 0%**<br>邊下歌分析邊快速切歌零爆音 |
| **6** | **高畫質封面記憶體佔用與長時間運行穩定性 (`CF003` / `N006`)** | • 原廠無尺寸上限，3000×3000 封面解壓為 raw RGBA 佔用 **`~36 MB/張`**，衝垮記憶體頻寬並在雙層打碟 45–120 分鐘後導致搜歌失靈與轉盤螢幕卡頓。 | • `capCover` 與 `dbhealer` 拒絕 `>16MP` 解碼炸彈，將大圖雙線性縮放為 **`<= 512×512` JPEG (`<= 256 KB`)**，單圖解壓記憶體降至 **`<= 1 MB`**。<br>• 縮圖快取設 `128` 張硬上限，所有背景 Map 採用單條安全淘汰；`QQuickPixmapReader` 限制在 `CPU1-3` (`nice 10`)。 | **單圖記憶體開銷降低 >97% (`36 MB` $\rightarrow$ `<= 1 MB`)**<br>數小時演出記憶體零膨脹 |

---

## 四、與官方原版系統的完整改動與 642 條歷史 Bug 狀態總覽

完整技術細節與 642 條歷史缺陷逐條核查表請參閱 [**`docs/OPEN-SOURCE-FULL-MANIFEST.md`**](./docs/OPEN-SOURCE-FULL-MANIFEST.md)。
