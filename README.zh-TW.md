<div align="center">

<img src="./docs/assets/hero-banner-zh-TW.svg" alt="EngineX OS 5.3.0.4" width="100%">

<br>

<a href="./README.md"><img src="https://img.shields.io/badge/English-30363d?style=for-the-badge" alt="English"></a>
<a href="./README.zh-CN.md"><img src="https://img.shields.io/badge/%E7%AE%80%E4%BD%93%E4%B8%AD%E6%96%87-30363d?style=for-the-badge" alt="简体中文"></a>
<a href="./README.zh-TW.md"><img src="https://img.shields.io/badge/%E7%B9%81%E9%AB%94%E4%B8%AD%E6%96%87-00e676?style=for-the-badge" alt="繁體中文"></a>

<br>

**[核心功能](#features)** · **[韌體下載](#install)** · **[實機畫面](#gallery)** · **[重構與佔用對比](#benchmarks)** · **[讚賞支持](#sponsor)**

📖 **線上使用文件**：<https://enginexdj.com>

</div>

<br>

**EngineX OS 5.3.0.4** 是基於官方 Engine OS 5.3.0+ 底層重構的客製系統，支援天龍 Denon DJ、Numark 與 Rane 全系 16 款獨立設備：
`SC5000` · `SC5000M` · `SC6000` · `SC6000M` · `PRIME 4` · `PRIME 2` · `PRIME GO` · `Mixstream Pro` · `SC LIVE 4` · `SC LIVE 2` · `PRIME 4+` · `PRIME GO+` · `Mixstream Pro+` · `Mixstream Pro Go` · `PRIME 4 G2` · `Rane SYSTEM ONE`。

前八款用隨身碟刷 `.img`；後八款二代機的更新程式會在寫入前用軟體驗證韌體簽章，改用 `/data` 資料包（Beta，見[如何安裝與升級](#install)）。

操作介面與原廠完全一致，直接透過隨身碟刷入即可解鎖以下功能。

---

<a id="features"></a>

## 核心功能解鎖

### 1. 全機型解鎖 4 軌 Stems 即時分軌（人聲 / 旋律 / 貝斯 / 鼓組）
突破原廠機型限制，在 **SC5000 / SC6000 / PRIME 全系列**上開啟曲庫側邊欄 `Stems` 播放清單與打擊墊四軌獨立靜音：
- 選歌時向左滑動即可拆出 **Vocal（人聲）、Melody（旋律）、Bass（貝斯）、Drums（鼓組）** 四軌；
- 刷碟時直接按打擊墊一鍵抽掉人聲做 Acapella，或單留鼓點混音；
- 不連電腦可直接由播放機本機完成分軌；同一區域網路下開啟家用電腦或連接雲端伺服器時，自動呼叫顯示卡執行高速 AI 分軌。

### 2. 內建 7 大串流平台（手機掃碼直接登入）
不必每次演出前把新歌一首首拷進隨身碟：
- 音源選單直接內建 **網易雲音樂、Spotify、Deezer、SoundCloud、TIDAL、Beatport、Apple Music**；
- 手機掃描播放機螢幕上的 QR Code 即可登入個人帳號，直接瀏覽個人歌單、收藏夾並線上搜歌；
- 支援 **無損 FLAC / 320k MP3** 音質切換；播放過的曲目自動快取在播放機內部儲存，再次載入直接秒開，絕不重複下載。
- **各平台獨立快取與 Deemix 智慧共享**：Spotify（`spotify-<id>.*`）、Beatport（`beatport-<id>.*`）、SoundCloud（`soundcloud-<id>.*`）以及「官方直下（Official）」模式的網易雲音樂（`netease-<id>.*`）各自使用獨立的下載通道與專屬快取命名空間；即使同一首歌曾在 Deezer 或網易雲下載過，在 Spotify 點播也會走 Spotify 自己的流程重新拉取並獨立快取，互不混用。網易雲開啟「Deemix 優先」時，底層與 Deezer 同走 Deemix 無損音源，在網易雲點播過的歌切到 Deezer 點播同一首（或反之）會直接重用已下載的無損快取，不重複下載；網易雲切回「官方直下」後則與 Deezer 各自獨立快取。
- **SoundCloud Go+ 與非 Set 單曲高清智慧路由**：DJ Set / 長混音（超過 10 分鐘）一律直連 SoundCloud 官方串流，優先取上傳者原始 320k / WAV 母帶，其次 256k / 160k AAC 或漸進式 MP3。Go+ 專享曲目與非 Set 的普通單曲：已綁定 **SoundCloud Go+** 帳號時優先直連官方高音質串流；沒有 Go+ 時（Go+ 專享曲目官方只給 30 秒試聽），自動在本機已登入的其他串流（Deezer / 網易雲 / Beatport）中比對並拉取無損 / 320k 音訊，快取為 `soundcloud-<id>.*` 上碟播放；其他串流都沒登入、或曲目是 SoundCloud 獨占作品時直連 SoundCloud 播放，遇到必須 Go+ 的試聽曲目又沒有任何可用平台，播放機會明確提示掃碼登入 SoundCloud Go+ 或其他串流帳號。
- **Beatport** 使用你自己的帳號與訂閱：Professional 下載 FLAC 或 256k AAC，Advanced 下載 128k AAC，Essential 線上播放 128k AAC 串流；Beatport 曲目同樣可以一鍵拆 Stems。
- **Apple Music**：在手機上貼上任意歌單或專輯連結即可，不需要帳號；再填入 `media-user-token` 就能瀏覽自己的資料庫。播放走 Engine 5.3.0 內建的 Apple Music 音源（EngineX 負責解鎖），不再比對 Deezer 或網易雲。

### 3. 先鋒 Rekordbox 隨身碟隨插即用（免電腦轉庫）
平時給先鋒 CDJ 匯出的 Rekordbox 隨身碟，直接插上即可上台演出：
- 插碟 **2 秒內**直接讀取全部播放清單、Hot Cue、Memory Cue、Loop 循環點與拍線，不必再用電腦版 Engine DJ 重新轉庫；
- 相容全格式儲存裝置：Windows 的 `FAT32` / `exFAT` / `NTFS` 與 Mac 的 `HFS+` / `APFS` 插上直讀；
- 自動扣除帶 `Xing` / `Info` 與 `LAME` 標頭的 MP3 編碼延遲（44.1 kHz 下約 26–51 ms），使拍線、Hot Cue、Memory Cue 與 Loop 與音訊瞬態精準對齊（WAV / FLAC / AIFF 與無標頭 MP3 維持不變，`#178`）；
- 自動修復未正常退碟造成的資料庫損毀，並移除「未安全退出」警告彈窗。

### 4. 完整中文介面與先鋒風格 3-Band 波形
- **三語介面**：設定、選單與提示全面支援 **繁體中文 / 简体中文 / English**（全系 16 款）；
- **3 種波形配色**：在原廠綠波形基礎上，新增高對比度 **RGB 波形** 與先鋒風格 **3-Band（三頻段藍/橙/白）波形**，可於控制中心 `波形樣式`（`Waveform Style`）磁貼或 `設定檔 → 顯示 → 波形樣式` 一鍵切換並雙向同步（全系 16 款）。

### 5. 切效果器零爆音，長時間演出更穩定
- 消除原廠韌體在開關或切換效果器（FX）時偶發的「咔嗒」爆音，雙軌同時開效果器也有平滑淡入過渡；
- 預設關閉原廠背景匿名統計回報與無用磁碟快取，把全部算力與記憶體留給音訊播放與螢幕觸控。

### 6. 智慧同步與 Gigabit 有線網路
- **智慧同步（Smart Sync）**：下拉控制中心一鍵切換 **Bar / Beat / Tempo** 同步模式；開啟 Sync 後，兩首曲目 BPM 差距超出目前變速範圍時，自動把變速範圍（Pitch Range）升檔到足夠為止（最高 `±100%`）；
- **Gigabit 有線網路**：有線網路孔從原廠限制的 `100 Mbps` 解鎖至 **1 Gbps（1000 Mbps）全雙工滿速**，並配 8 MiB TCP 大視窗緩衝；
- **Wi-Fi**：播放機內建 Wi-Fi 模組受限於主機板 SDIO 25 MHz SDR12 匯流排頻寬，實測極限約 `9～10 MB/s`，大容量曲庫傳輸建議使用 Gigabit 有線網路孔。


### 7. 5.3.0.4 新功能
- **詳細日誌與日誌下載**：網頁主控台的播放機設定新增 **詳細日誌**（預設關），切換會重新啟動 Engine，播放會中斷。點擊旁邊的 **下載日誌**，儲存最近 30 分鐘已脫敏的純文字日誌（最多 5,000 筆 / 2 MiB），不需 SSH；重現問題後請關閉詳細日誌。
- **四軌聯動完整卸載與串流真實歌名（#49）**：對端播放機關機、重啟或退歌時會發送 UDP 告別封包，本機在 350 ms 防抖後自動卸載影子軌（`Unload` + `UnSync`，並自動交回 `MASTER`），清掉殘留的波形與曲目。移除了可能死鎖的 `RemoteState` 訂閱，對端關機不再阻塞本機切換串流音源。串流曲目在影子播放機上顯示真實的 `歌手 - 歌名`，不再顯示內部數字快取檔名；
- **介面更流暢、降低 CPU 佔用**：新增 `scTr` QML 屬性 translator 快取，並給 `QuickCue` 加 500 ms 節流，消除按 `MASTER` / `SYNC` 時介面的瞬間卡頓，明顯降低 Engine `EMain` GUI 執行緒的 CPU 佔用；
- **歌單左滑只留一個 Stems 按鈕**：自動辨識原廠已帶 `Stem` 動作的本機曲目，去重並在地化為單一的 `分軌 Stems` 按鈕，本機、串流、區域網路曲目左滑後的體驗完全一致；
- **原廠「連動節拍跳躍與 Loop 長度」解鎖與節拍跳躍互換修復（#135）**：於 `設定檔 → Cue / Loop → 連動節拍跳躍與 Loop 長度` 解鎖了原廠沒有這一項的機型（開啟後預設 Loop 長度與節拍跳躍長度同步連動，由使用者自選開/關，EngineX 不改寫該值）；並修復獨立開關 `版面 → ENGINEX 視圖預設 → 互換循環 / 節拍跳躍`（互換旋鈕的循環與跳拍功能）在 JP07 上的 `beatJumpEnabled` 判定；
- **OmniSource 歌單隔離（#105）**：開啟 OmniSource（跨源搜尋）後，搜尋列仍可同時檢索各串流與全部隨身碟曲庫，但各串流自己的歌單頁不再混入 `[USB: ...]` 項目與其他平台的歌單；
- **SoundCloud 按讚歌單回退（#150）**：使用者 OAuth `refresh_token` 過期、SoundCloud 回傳 `HTTP 400` 時，自動回退到 App Token 與公開個人頁介面，繼續載入 `Liked Tracks & Sets`；
- **Spotify Connect 區域網路探索（#150）**：內建 Avahi `_spotify-connect._tcp` mDNS 廣播，`stream-sync` 另帶純 Go 的 `_spotify-connect._tcp.local.` mDNS 廣播器（有線 `eth0` + 無線 `wlan0` 雙網卡也能被發現），並使用標準 768-bit Diffie-Hellman 公鑰，手機 Spotify App 可在區域網路中直接發現 `EngineX OS`；同時修復 OAuth 登入後曲目下載回傳 `410` 的問題；
- **串流快取隔離與 Deemix 智慧共享**：Spotify、Beatport、SoundCloud 與網易雲「官方直下」模式各自使用獨立下載通道與專屬快取命名空間；網易雲「Deemix 優先」時與 Deezer 共享無損快取，互相點播同一首不重複下載；
- **SoundCloud Go+ 與高清智慧路由**：長混音 / DJ Set（超過 10 分鐘）一律直連 SoundCloud 官方串流；非 Set 普通單曲與 Go+ 專享曲目：已綁定 Go+ 優先官方高音質串流，未綁定時自動在已登入的其他串流（Deezer / 網易雲 / Beatport）比對無損 / 320k 音訊，避免 30 秒試聽限制；
- **品牌統一與免網域公網連線**：面向使用者的組件統一為 `enginex-relay`（原 `sc5000-relay`）、`enginex-stemsd`（原 `sc5000-stemsd`）、`enginex-suite`，執行目錄為 `/data/enginex`（保留 `/data/sc5000-custom` 相容符號連結）；支援 Cloudflare Quick Tunnel 免網域一鍵公網穿透，手機掃碼即可遠端登入串流；中繼主控台新增 `已連線播放機叢集（Multi-Deck Hub）` 卡片，彙整同一後端下所有播放機的上線狀態、機位角色（`13` / `24`）、韌體版本與串流登入狀態，可用播放機存取碼或主密碼登入；各播放機 `設定 → 關於 / 更新` 頁會即時顯示 Stems 算力節點狀態；
- **Mixstream Pro / Pro+ / Pro Go（#164、#166）**：實體 FILTER 旋鈕控制原廠 Filter / Dub Echo / Noise / Filter Roll / Wash，點擊各軌打擊墊列的 `濾波效果`（`SWEEP FX`）開啟原廠選擇輪盤；恢復觸控 Beat Jump，於 `版面 → ENGINEX 視圖預設 → 節拍跳躍打擊墊`（`Beat Jump Pad Bank`）開啟即可後跳、前跳、減半、加倍；各軌 `EQ / 分軌`（`EQ / STEMS`）按鈕切換通道三段旋鈕 Stems EQ；安裝器支援嚴格 EAAS 協定探索二代機並消除連接埠衝突，相容 fastboot 35+，完善 Windows WSL 2 + usbipd-win 刷機指引，已在 NH08S 實機完整刷入驗證；識別 Mixstream Pro Go（`15e4:2059`）USB 裝置；
- **一代機電腦線刷與免 debugfs 解包**：修復 SC5000 / SC6000 / PRIME 4 等一代機在韌體更新模式下 fastboot product 缺失時的 USB ID 識別，恢復電腦線刷支援；打包工具在 macOS 與 Windows 上無 debugfs 時也能正常解包映像；
- **PRIME 4 硬體 FX 選擇器與 PX 獨立 Time/Rate 調節（#168，待實機驗證）及 PRIME 4+ 啟用校驗（#169）**：重整 JC11 硬體效果選擇器，末尾新增 5 個 PX 效果（`Spiral (PX)` / `Pitch Echo (PX)` / `Slip Roll (PX)` / `Dub Echo (PX)` / `Space (PX)`）；接入原生 `Time/Rate` 事件（`0–10` 共 11 檔，可透過 `BEAT −/+` 鍵或觸控 Time/Rate 獨立調節 PX 時間檔位；PX 採用相對檔位，不依拍子同步，螢幕/OLED 拍數標籤僅代表所選相對檔位），`Parameter` 獨立調音色與回授，`Wet/Dry` 調乾濕比，並修復切效果器重設與乾濕淡入爆音；強化 PRIME 4+（JC11S）啟用檔案與資料目錄校驗；修復 Beatport 無損升級在冷快取下的命中，並在 `stream-sync` 佔用連接埠時讓 `deemixd` 優雅退出；
- **影子軌模式（`自動` / `全部 Engine 模式` / `全部 CDJ 模式`）、載入進度環與 Engine 模式頭部鏡像（#49，雙 SC5000 實機已驗證；一體機 / CDJ 實機待驗證）**：四軌版面下，只要對端曲目可複製，影子軌均由本機靜音載入，用與本機軌道相同的原廠元件繪製（與本機顯示一模一樣）：EngineX 播放機的曲目從對端複製，CDJ / Pro DJ Link 裝置的曲目從 CDJ 的 USB 隨身碟或 SD 卡複製。複製期間影子軌立即顯示來源曲目的歌名、演出者、封面、BPM、調性與長度，波形區顯示原廠進度環（`ProgressRing`）；Engine 模式影子軌頭部完整鏡像來源播放機的真實歌名、演出者、調性與移調（`Key Offset`）、自動循環（`Auto Loop`）、`Stems` 圖示、同步（`Sync`）變綠的大號 `BPM` 以及僅在來源軌顯示時點亮的 `主控 (LEAD)` 指示，概覽波形同步繪製來源軌 `滑行模式 (Slip)` 滑行線。無法複製檔案的 CDJ 曲目（串流、rekordbox Link、CD 或第二 USB、複製失敗或逾時）自動回退顯示 CDJ 波形，並用一行文字說明原因。`版面 → ENGINEX 視圖預設 → 影子軌模式`：`自動`（所有機型預設；一體機 Line In 顯示 CDJ-3000 波形照舊）、`全部 Engine 模式`（所有對端曲目均由本機載入）、`全部 CDJ 模式`（不載入對端音訊，一律顯示 CDJ 式波形）；
- **CDJ 顯示式影子波形**：影子軌顯示 CDJ 曲目已快取的 rekordbox 波形、拍線與 Cue，不下載音訊；沒有分析資料時只顯示中繼資料。PRIME 4 / PRIME 4+ / PRIME 4 G2、SC LIVE 4 開啟 `4-Deck Waveform` 後，3、4 軌顯示影子波形，保留本機 1、2 軌；沒有對端的軌道仍可本機使用。自訂 rb-bridge `--media-cache` 時，WebUI 服務環境中的 `SC5000_PROLINK_MEDIA_CACHE` 必須指向同一目錄；兩者預設共用目錄。關閉 `prolink_share` 也會停止讀取顯示快取；
- **Rekordbox MP3 編碼延遲自動對齊（#178）與舊版 rekordbox 5 Device Library 相容（#182，待回報人真實隨身碟驗證）**：轉換帶 `Xing` / `Info` 與 `LAME` 標頭的 MP3 時自動依真實取樣率扣除解碼起點差（44.1 kHz 下約 26–51 ms），使拍線、Hot Cue、Memory Cue 與 Loop 對齊音訊瞬態（WAV / FLAC / AIFF 與無標頭 MP3 維持不變，已匯入過的隨身碟再次插入時僅自動重新轉換受影響的 MP3，#178）；支援讀取舊版 rekordbox 5 Device Library（`export.pdb`）匯出的隨身碟，未分析 BPM（`0xFFFFFFFF`）記為 `0`，忽略孤立歌單項目，同名歌單與斷鏈父目錄自動相容（#182）；
- **打擊墊與按鍵燈亮度分級調光（#85）**：`設定 → 裝置 → 打擊墊亮度`（`100%`–`20%`）讓 RGB 燈（打擊墊、模式鍵、`Layer`、轉盤燈環、螢幕上的 pad）跟隨亮度調光；`SYNC`、`KEYLOCK`、`SLIP` 等單色鍵因為硬體只有亮/暗兩檔，保持原廠亮度；
- **SC LIVE 4 AUX（#171，待實機驗證）**：在原廠 `混音器選單 → 通道 → 輸入` 中把 AUX 送入通道 3 或 4，使用通道 CUE、推桿、EQ 與 Sweep FX；僅在 AUX 模式且任一通道選擇線路輸入時封鎖 AUX 直通，MIC2 保持原廠路由。兩路共用一個 AUX 來源，同時推起會疊加。Mixer Menu（Beta）會搬移部分偏好設定，並允許向線路輸入通道載歌；JP21 隱藏未驗證的 Bluetooth/Computer 選項。詳見[路由說明與實機檢查](docs/issue171-sclive4-aux.md)；
- **PRIME 4 等一體機連 CDJ-3000 波形（待實機驗證）**：在 PRIME 4 等一體機上將混音台通道切至線路輸入（Line In）後，Pro DJ Link 連接的 CDJ-3000 波形會顯示在該通道上（依 CDJ 播放機編號從小到大依序對應 Line In 通道從小到大）；SC5000 / SC6000 獨立播放機行為不變；
- **開機與關機畫面全螢幕鋪滿（#175）**：開啟介面縮放（UI Scale，非 100% 如 75%）時，開機啟動畫面與關機遮罩現補齊全螢幕錨定，於所有縮放比例下皆完整鋪滿螢幕，消除黑邊（涵蓋 SC LIVE 4 等全系機型）；
- **影子軌細波形顯示修復**：暫停狀態下剛載入的影子軌不再出現空白（解決切細波形時的渲染時序問題）；細波形切片尚未下載完成時，滾動波形泳道不再將整首歌的概覽圖拉伸擠入小波形，而是維持乾淨的拍線與指標顯示，就緒後平滑渲染；
- **Mod 外掛中心區域網路修復（#202）、可執行權限保留（#170）與 `dmx-studio` Mod（#134）**：Web 主控台 Mod 外掛中心請求改走帶存取碼驗證與同源檢查的 `/api/mods` 與 `/api/mods/upload`，修復區域網路瀏覽器存取回傳 `403`（#202）；安裝 `.tar.gz` Mod 時保留檔案可執行權限位（`0111`，過濾 setuid/setgid/sticky 位，#170）；獨立多域 DMX512 自核心剝離為按需開源 Mod `dmx-studio`（未啟用時 0 記憶體與 0 背景行程，#134）；
- **SC5000 / SC6000 開啟四軌後保持原廠播放機行為、圖層顏色與 `MASTER` 燈暗亮值（#49）**：在獨立播放機（`SC5000 / SC5000M / SC6000 / SC6000M`）上開啟 `版面 → ENGINEX 視圖預設 → 四軌波形顯示`（`4-Deck Waveform`）後，完整保持原廠播放機行為——各軌顏色依 `設定 → 裝置 → 碟機編號 #`（`Player #`）與 `Layer A / B` 自動映射 `設定檔 → 碟機顏色`（`Profile → Deck Colors`，保留 `Layer A / B` 雙層選色盤），於曲庫按 `Browse` 瀏覽旋鈕直接載入至本機目前圖層（`Layer A` 或 `Layer B`），並恢復 `設定 → 裝置 → 碟機編號 #`（`Player #`）、`版面 → 碟機行為 → Layer B`（`Layout → DECK BEHAVIOR → Layer B`，關閉時右側提示 `啟用雙層播放`）、`設定 → 混音器 → 取樣器輸出`（`Settings → Mixer → Sampler Output`：`Layer A / Layer B`）、遠端碟機選擇、`Engine Connect` 與 `觸控效果 (Touch FX)` 的 `Mute` 靜音按鈕；四軌獨立播放機的 `MASTER` 按鍵燈依本軌主控狀態點亮，全系非主控 `MASTER` 燈亮度維持原廠暗亮值，轉盤操控維持原廠行為；
- **`全部 CDJ 模式` 與 CDJ 回退時影子軌頭部及波形層對齊原廠樣式（#49）**：當 `版面 → ENGINEX 視圖預設 → 影子軌模式`（`Shadow Deck Mode`）設為 `全部 CDJ 模式`（`All CDJ Mode`），或在 `自動`（`Auto`）模式下遇到無法複製音訊的 CDJ 曲目回退至顯示模式（以及一體機 `Line In` 通道顯示 CDJ 波形）時，影子軌頭部與波形層全面對齊原廠樣式：專輯封面（`版面 → 演出配置 → 顯示專輯封面`）、滾動歌名與歌手（`設定 → 裝置 → 隱藏歌名/歌手` 與 `設定檔 → 顯示 → 曲目標題`）、剩餘 / 已播時間（點擊可切換）、跟隨原廠顯示設定的調性與調性色及鎖調 `Key Lock` 圖示（先鋒稱 `Master Tempo`）、開啟 `同步`（`Sync`，先鋒稱 `Beat Sync`）時變綠的大號 `BPM`、`變速範圍`（`Pitch Range`，先鋒稱 `Tempo Range`）與變速百分比（`pitch`）、軟接管箭頭 `▲▼`、`主控 (LEAD)` 指示（先鋒稱 `MASTER`）、`循環 (Loop)` 與 `節拍跳躍 (Beat Jump)`、`熱標拍數倒數`（`Hot Cue Countdown`）以及依 `設定檔 → 顯示 → 曲目結束提醒`（`Track End Warning`）設定時間觸發的曲末紅閃；波形泳道與概覽條對齊原廠變速跨度、2.2× / 1.3× 底邊錨定高度、時間進度條（`TimeBar`）、上下熱標三角（`HotCueTriangle`）與標籤、描邊小節號及原廠底欄作用態；
- **通用 USB MIDI 控制器映射器 Mod（`midi-controller-mapper`，#137，待實機驗證）**：將外部 USB MIDI 控制器（首發內建 **Pioneer CDJ-400** `PC MODE` 預設，支援自訂 JSON 預設）直接映射至四軌模式下的內部 `Deck 3` 或 `Deck 4`，無須切換硬體 Layer。未啟用時橋接完全停止（零背景輪詢）；啟用後 helper 以最低排程優先級 `os.nice(19)` 按需 60 Hz 同步（5.3.0 沙箱自測橋接延遲 `p50 ≈ 1.6 ms`），支援 CDJ-400 連續速度轉盤積分（`Bn 10`，`64=停止`）、帶螢幕 `▲` / `▼`（`SpeedOffsetUp` / `SpeedOffsetDown` / `SpeedNeutral`）指示的變速推桿軟接管，以及 USB 熱拔插 / MIDI 風暴自動停用保護（註：CDJ-400 標準 USB MIDI 模式不接收外部 Note/CC LED 回傳，狀態與 `▲▼` 指示直接顯示於播放機螢幕上）；
- **官方 Mod 1.1.0 靜態 Go 輔助程式與就地覆蓋升級（#208、#134）**：此前官方 Mod（`dmx-studio`、`jog-logo-studio`、`sampler-pad-studio`、`minimal-club-mode`、`midi-controller-mapper`）使用 Python 輔助程式，因全系 16 款播放機皆無 Python 而無法啟動（Mixstream Pro+ NH08S 實機回報）。現全部改為雙架構靜態 Go 二進位檔（一代與二代 armhf 用 `bin/enginex-mod-helper-armv7`，PRIME 4 G2 / SYSTEM ONE 用 `bin/enginex-mod-helper-arm64`），由 busybox `sh` `start.sh` 依 `uname -m` 選擇，維持原有本機 HTTP 介面與 `Panel.qml` 不變；其中 `dmx-studio` 的 uDMX 改為直連 Linux `usbfs`（免 `libusb`），FTDI/OpenDMX 維持 #208 實機驗證參數（`termios2` 250000 8N2、120 µs break / 12 µs MAB、約 38 Hz，對音訊零干擾）。於 `:8080` Web 主控台直接上傳 `1.1.0` `.tar.gz` 即可就地覆蓋舊版 `1.0.0` 並保留啟用狀態與設定。Mod 開發者注意：播放機無 Python、無編譯器、無套件管理器，請參考範本 [`docs/mods/src/helper-template/`](docs/mods/src/helper-template/) 使用 `CGO_ENABLED=0` 編譯 `GOARCH=arm GOARM=7` 與 `arm64` 靜態二進位檔；
- **PX 效果毫秒時間模式與 `設定檔 → 播放` FX 設定收口（#162，預設關閉，待實機驗證）**：`設定檔 → 播放 → 先鋒效果延遲模式 (PX)`（`PX Delay Time Mode`：`預設（關閉）` / `毫秒（ms）`，僅於 5.3.0 PRIME 4 或 `先鋒風格效果 (Color FX)` 選中 `Spiral` / `Pitch Echo` / `Dub Echo` 時顯示，與 `:8080` Web 主控台開關同步）為延遲類 PX 效果 `Spiral (PX)`、`Pitch Echo (PX)` 與 `Dub Echo (PX)` 啟用共用的獨立 `10–4000 ms` 延遲（開啟後展開下方 `先鋒效果延遲 (ms)` 檔位，隱藏項保留已存值並依關閉處理；無原生 `(PX)` 選擇器的 15 款機型上 `先鋒風格效果 (Color FX)` 以固定混音深度直接作用於主輸出）；PRIME 4 的 `Time/Rate`（`10–4000 ms` 共 11 檔）、`時間（ms）` 讀數與觸控 `−/+`（步進 `10 ms`）共用同一毫秒值，原廠 Beat FX 維持依節拍（Beats）運作，`Slip Roll (PX)` 與 `Space (PX)` 維持原有時間行為（[範圍與驗證](docs/issue162-px-ms.md)）；
- **軌道頂欄第 2 列最寬定欄與固定熱標拍數倒數（#49、#151）**：雙軌主視圖、四軌分組與分欄微縮列（`GroupedMini` / `SplitMini`）及影子軌頂欄第 2 列右側速度組依最寬文字 `±100` 與 `-100.00%` 即時鎖定欄寬並右對齊範圍標籤（非緊湊視圖維持原廠寬度不變），推桿移動零抖動；`熱標拍數倒數` 固定槽位永不移位，`BARS.BEATS` 拍數始終完整顯示、超長熱標名稱優先右側省略，且 `Auto Loop` / `Beat Jump` 不再擠佔歌名寬度；
- **影子副本超 `+100%` 變速 `Sync` 跟隨、相位拉入追蹤與靜音影子槽 `MASTER` 排除（#49）**：排除本機靜音影子槽參與 `MASTER` 選舉，追蹤來源軌原生 `Sync` 相位拉拽斜坡並校驗播放態尋道落地；當來源播放機在原廠 `Sync` 下跨倍速執行超過 `+100%` 推桿上限（`SpeedState` 顯示截斷於 `+100.0%`）時，發送端回報精確 BPM 比值，Engine 模式影子副本透過原廠 `Sync` 突破推桿上限跟隨並維持歌曲位置閉環，來源軌退出同步或退歌時乾淨釋放且不搶 `MASTER`；
- **SC LIVE 4 等二代機唯讀根分割區 Stems 分軌器偵測（#176，待實機確認分軌）**：於根分割區唯讀的二代資料包機型上，將 `Engine` 行程對 `/usr/local/bin/stems-processor` 的 `statx` 與 `open` 檢查回退至 `/data` 資料包內的可執行包裝器，點選 **Generate Stems** 不再誤判未安裝而跳回設定頁（見[原因與實機範圍](docs/issue176-sclive4-stems.md)）；
- **二代機 `engine.service` 執行環境對齊（#177）**：二代 `/data` 資料包於啟動前建立 `0700` `/tmp/sc5000-engine-home`，並在 `engine.service` drop-in 注入 `HOME=/tmp/sc5000-engine-home`、`MESA_SHADER_CACHE_MAX_SIZE=32M` 與 `QML_DISABLE_DISK_CACHE=1`，與一代韌體完全一致；
- **啟動復原安全網與隨身碟脫敏報告匯出（#140）**：開機 180 秒無介面就緒且無啟動進展，或 300 秒內連續失敗 3 次後，自動持久退回至不載入 EngineX 掛鉤與介面包的原廠 Engine 並隔離外置 Mod（不恢復原廠設定，不清除曲庫）；插入可寫入隨身碟或 SD 卡後自動匯出脫敏啟動報告 `EngineX-boot-report-<time>.txt`，`:8080` 登入頁公開顯示復原狀態，登入後可點 `重試 EngineX`；
- **中國大陸與全球 NTP 時間同步、斷電時鐘保持與離線時區記憶**：為 `設定 → 裝置 → 時間/日期 → 同步時間` 設定中國大陸與全球 NTP 池，於無電池 RTC 機型斷電重啟後透過單調時鐘推進維持時間下界，並防止斷電離線開機初期自動退回 `UTC` 覆蓋已儲存的 `設定 → 裝置 → 時區`（手動選擇 `UTC` 仍正常保留）；
- **一代機首次開機抑制無效 Stems 下載彈窗與客製開機第一屏（#118）**：一代機 `5.3.0` 映像標記內建 Stems 為出廠已安裝，空白 `/data` 首次開機不再彈出帶無效 `Download` 按鈕的提示框；一代機 `.img` 發佈包嵌入 EngineX 客製開機第一屏。

### 8. 5.3.0.3 新功能
- **Tailscale / Headscale 遠端存取（#130）**：Web 控制台的 `遠端存取（Tailscale / Headscale）` 卡片按需下載精簡版 Tailscale 用戶端（約 4.3 MB，下載後校驗 SHA-256），以登入連結或一次性 Auth key 登入，只把你勾選的服務（SSH / SFTP、Web 控制台）開放給你自己的 tailnet。不需要公網 IP，也不用改路由器，自架 Headscale 伺服器同樣可用。預設關閉。播放機核心沒有 TUN 裝置，所以以使用者空間模式執行。已用 Headscale 端對端測通；實機播放時的 CPU 與記憶體占用尚未實測；
- **串流歌單排序（#98）**：所有串流服務都能開啟 Engine 內建的排序選單，依 BPM、調性、評等、歌名或歌手遞增 / 遞減排列；
- **串流曲目可錄音（#84、#206）**：具錄音功能的一體機（`PRIME 4 / 4+ / 4 G2`, `PRIME 2`, `PRIME GO / GO+`, `SC LIVE 4 / 2`, `Mixstream Pro / Pro+ / Pro Go`, `SYSTEM ONE`）在已載入串流曲目時可以開始錄音，錄音過程中也能繼續載入串流曲目（獨立播放器沒有錄音功能）。串流服務的條款可能不允許保留其曲目的錄音，請僅供個人使用；
- **曲目列表顯示 BPM（#80）**：開啟 `版面 → ENGINEX 視圖預設 → 雙欄瀏覽` 後，曲目列表第二欄旁會顯示 BPM；
- **推桿回音與智慧同步（#104）**：在帶混音台的一體機（`PRIME 4 / 4+ / 4 G2`、`PRIME 2`、`PRIME GO / GO+`、`SC LIVE 4 / 2`、`Mixstream Pro / Pro+ / Pro Go`、`SYSTEM ONE`）上，控制中心保留原廠 `推桿回音`（`Fader Echo`）磁貼（不重複新增自訂磁貼），並與 `設定 → 混音器 → 推桿回音`（`關` / `全部推桿` / `僅豎推`）與 `設定檔 → 播放 → 回聲淡出`（`Echo Out`）雙向同步（獨立播放機 `SC5000 / SC5000M / SC6000 / SC6000M` 自動隱藏）。開啟 Sync 時，智慧同步（全系 16 款）也會嘗試半速 / 倍速（0.25×–4×）匹配並依需求自動升檔變速範圍（Pitch Range）；
- **控制中心演出磁貼排佈**：包含 `波形樣式`（`Waveform Style`）與 `智慧同步`（`Smart Sync`，全系 16 款），帶 Transition FX 的一體機保留原廠 `推桿回音`（`Fader Echo`）磁貼；`直播推流`（`Live Stream`）移至 `設定 → 服務`；`波形樣式` 於控制中心與 `設定檔 → 顯示 → 波形樣式` 雙向同步，不在 `版面 → ENGINEX 視圖預設` 重複顯示；
- **先鋒風格 Beat FX 與 Sound Color FX（#88、#168）**：JC11 PRIME 4（Engine 5.3.0）可於列表末尾選擇 `Spiral (PX)`、`Pitch Echo (PX)`、`Slip Roll (PX)`、`Dub Echo (PX)`、`Space (PX)`，按 FX 單元及其分配通道生效。`Time/Rate`（`0–10` 共 11 檔，透過 `BEAT −/+` 或觸控 Time/Rate 調節）獨立控制 PX 時間檔位（PX 採用相對檔位，不依拍子同步；螢幕/OLED 拍數標籤僅代表所選相對檔位），`Parameter` 可調音色與回授，`Wet/Dry` 可調乾濕比，原廠效果繼續可選。AZ04 保留原本的 `先鋒風格效果 (Color FX)` 設定；PRIME 4 實機驗證待完成；JP07 SC5000 沒有原生 FX 選擇器，保留全域 `先鋒風格效果 (Color FX)` 選單、master 效果及儲存的設定；
- **介面縮放（110%–75%）、曲目列表波形開關與打擊墊亮度（#153、#117、#85）**：`版面 → ENGINEX 視圖預設 → 介面縮放` 可把 Engine 整個介面在 110% 到 75% 之間無黑邊縮放，控制中心與彈窗仍然鋪滿螢幕；`設定檔 → 曲庫 → 曲目列表波形` 關閉後可隱藏列表裡的縮略波形，讓歌名與藝人完整展開；`設定 → 裝置 → 打擊墊亮度`（`100%`–`20%`）：RGB 燈（打擊墊、模式鍵、Layer、轉盤燈環、螢幕上的 pad）跟隨亮度；`SYNC`、`KEYLOCK`、`SLIP` 等單色鍵因為硬體只有亮/暗兩檔，保持原廠亮度；
- **實體三段 EQ 旋鈕 Stems 分軌控制與快捷鍵（#152）**：在帶混音台及 Stems EQ 的一體機（`PRIME 4 / 4+`、`PRIME 2`、`PRIME GO / GO+`、`SC LIVE 4 / 2`、`Mixstream Pro / Pro+ / Pro Go`、`SYSTEM ONE`；獨立播放機與 `PRIME 4 G2` 自動隱藏）上，透過 `設定檔 → 播放 → 分軌 EQ 模式`（`Stems EQ Mode`）或按各通道 **Shift + Cue (PFL)**，即可將實體 `HIGH / MID / LOW` 旋鈕切換為 **Stems Level**（`Vocal 人聲`、`Melody + Bass 旋律+貝斯`、`Drums 鼓組`），內建原廠軟接管（Soft Takeover）；修復 PRIME 2 / PRIME 4 快速雙擊 `Hot Cue` 時 Stems 模式閃退回 Hot Cue 的問題；
- **PRIME 4 / PRIME 4+ 與 SC5000 / SC6000 區域網路跨機型四軌連動（#160）**：僅於 4 軌機型（`SC5000 / SC5000M / SC6000 / SC6000M`、`PRIME 4 / PRIME 4+ / PRIME 4 G2`、`SC LIVE 4`；2 軌一體機自動隱藏）開放 `四軌波形顯示`、`四軌角色`（`自動`、`1+3`、`2+4`、`1+2`、`3+4`、`1`、`2`）與 `影子軌模式`；`互換循環 / 節拍跳躍` 對帶 Auto Loop 旋鈕的全系機型開放（`Mixstream Pro` 系列自動隱藏），`節拍跳躍打擊墊`（`Beat Jump Pad Bank`）僅對 `Mixstream Pro / Pro+ / Pro Go` 顯示；
- **修復串流下載後卡載入與「已從 Engine DJ 帳號中斷連線」報錯（#159）**：為本機串流閘道注入非 SSO 權杖並跳過 150 秒 Engine DJ Profile 等待，徹底解決串流曲目已下載落盤卻無法載入上軌的問題；
- **5 個儲存的視圖（#99）**：在 `版面` 頁點 `儲存視圖`，把目前版面存為 `視圖 1`–`視圖 5` 之一，之後一鍵切換；
- **串流曲目的影子波形**：四軌版面下，另一台播放機載入的串流曲目會在你的影子軌道上顯示真實波形（取自 Engine 自己的下載與分析結果）。`四軌角色` 設為 `自動` 時，現在固定依 Engine 的 Player 編號分配：奇數號播放機負責 1 / 3 軌，偶數號負責 2 / 4 軌。曲庫底部的軌道欄改為從左到右 `1, 2, 3, 4`（原廠是 `1, 3, 2, 4`），只點亮本機負責的兩軌，影子軌道反灰，防止誤載入（#90、#123）；
- **djay 曲庫（#112）**：開啟 OmniSource 後，隨身碟 / SD 卡上的 Algoriddim djay 曲庫，其歌單會出現在串流瀏覽器裡；
- **更短的控制台存取碼**：Web 控制台存取碼改為 8 位 `XXXX-XXXX`（不含 0/O、1/I，不分大小寫，橫線可省略）。升級後舊的長存取碼會被替換，已登入的瀏覽器需要重新輸入 `設定 → 關於 / 更新` 裡顯示的新存取碼。同一裝置 1 分鐘內輸錯 5 次會被鎖定 1 分鐘；
- **二代機一鍵安裝器（#89、#110、#129）**：EngineX-Installer 自動辨識二代機，包括被新版 `android-tools` 顯示為 `Android Fastboot` 的 PRIME 4+，並使用安裝包內建的 `fastboot`；
- **二代機顯示存取碼（#132）**：PRIME 4+ 等二代機的 `設定 → 關於 / 更新` 之前不顯示 Web 控制台位址與存取碼，檢查更新還報 `GitHub unreachable`，原因是開機時重啟 Engine 會連帶殺掉剛啟動的 EngineX 服務。現在這些服務是獨立的 systemd 單元，重啟 Engine 不再波及它們。之後加的 `KillMode=process` 會讓原廠 Engine 一直佔著螢幕、SYSTEM ONE 無法正常關機，已刪除（#116）。
- **SYSTEM ONE / PRIME 4 G2 走 USB 安裝（#116）**：這兩款沒有 fastboot，韌體更新模式在 USB 上是 `1cc5:2027`。EngineX-Installer 現在會產生 64 MiB 的全新 `/data` 映像，用官方 `write-part` 協定寫入（Rane SYSTEM ONE 實機驗證），寫完提示你按電源鍵重啟。`host-install-userdata.py` 新增 `--ssh-key`，可開啟 root SSH。
- **實驗功能，只有 API**：以下功能目前只有 Web 控制台 API，沒有控制台卡片或播放機選單，也尚未在實機上測試：透過 OmniSource 讀取 WebDAV / Subsonic（Navidrome）/ Plex 歌單（#82）、Hot Cue 名稱與顏色（#85）、隨身碟 `/mods` QML 外掛與 safe-mode 隔離（#87）、把 Loop 存到 Sampler 打擊墊（#120）、區域網路 RTP / Teleport 音訊以及 USB DAC / USB-B UAC2 輸出（#83、#113）。

### 9. 上一版新功能
- **波形可以縮得更遠（#77）**：一體機（PRIME 4、PRIME 2、PRIME GO、Mixstream Pro、SC LIVE）的波形最遠縮到 30 秒，和 SC5000 / SC6000 一樣，不再是原廠的 10 秒，螢幕上能看到更多即將到來的節拍網格。`設定 → 裝置 → 旋鈕行為` 選 `波形縮放` 後，在打碟介面轉瀏覽旋鈕即可縮放；
- **螢幕上直接關機（#78）**：`設定 → 裝置 → 電源 → 關機` 會跳出原廠的「關機」確認框，碟機裝在 DJ 台裡也不用再伸手去摸背後的電源鍵；
- **遺失曲目自動重新定位（#74）**：同一張磁碟上的檔案被移動或改名後，網頁控制台會為每首遺失的曲目找到新位置（先比對檔名和大小，再比對忽略大小寫的檔名，最後比對清理後的檔名）。「掃描」只預覽；「一鍵修復路徑」會先備份 `m.db` 再改寫路徑。完成後在碟機上重新載入該磁碟；
- **Apple Music 一鍵登入（#75）**：手機頁面可以直接用 Apple Music 登入（MusicKit JS）；Apple 不允許時，頁面會提供一個書籤腳本，在 music.apple.com 一鍵複製你的權杖；
- **碟機上查看控制台存取碼（#79）**：`設定 → 關於 / 更新` 顯示網頁控制台網址和存取碼。串流 QR Code 只登入對應的服務，不會開啟控制台，俱樂部裡掃螢幕 QR Code 的客人拿不到碟機的控制權；
- **靜音 Cue 與原廠主輸出限幅**：Censor 鍵可以設成 `靜音 Cue`（`設定檔 → 播放 → Censor 鍵行為`）；PRIME GO / Mixstream Pro 開放了原廠的 `原廠主輸出限幅 (dB)`（`設定 → 混音器`，關 / 0–18 dB），與 EngineX 的 `0dB 防破音軟限幅` 並存；
- **Apple Music** 加入串流服務（共 7 個），和其他服務一樣掃碟機 QR Code 登入；
- **Wi-Fi 傳歌**：在電腦上把 MP3 / FLAC / WAV / AIFF / M4A / AAC / OGG / OPUS 檔案拖進碟機網頁控制台（`http://<碟機 IP>:8080`），直接存到你選的內建硬碟、SD 卡或隨身碟的 `Wi-Fi Drop/<歌單>/` 下。曲庫所在磁碟上的 FLAC / MP3 會自動分析並加入同名歌單；不會覆蓋既有檔案，始終保留 256 MiB 空間；
- **OmniSource 統一瀏覽**：隨身碟 / SD 卡裡的歌單和曲目直接顯示在串流瀏覽器中，和串流歌單放在一起，一次搜尋兩邊都找得到；隨身碟曲目直接從碟上讀取播放，音質不打折（`設定檔 → 曲庫 → OmniSource（在串流中顯示 USB/SD）`，預設關閉）；
- **先鋒 CDJ / Pro DJ Link（測試版）**：透過區域網路 / Wi-Fi 讀取 CDJ 上 rekordbox 隨身碟的曲庫（`設定 → 服務 → Pro DJ Link 曲庫`），也能把本機曲庫分享給 CDJ（`設定 → 服務 → Pro DJ Link 匯出至 CDJ（測試版）`）。SC5000 與 CDJ 之間始終只有一個 Master：MASTER 可以雙向交接；本機當 Master 時會送出 BPM 與拍點，供 CDJ 的 BEAT SYNC 對拍。以上預設全部關閉；
- **更乾淨的聲音**：`0dB 防破音軟限幅` 以平滑曲線把主輸出壓在 -0.1 dBFS 以下，零延遲，-1.5 dBFS 以下的訊號原樣不動；Flanger 不再讓音量下沉（自動補償到原曲電平，最多 +6 dB）；`回聲淡出`（`Echo Out`，僅帶混音台的一體機顯示，獨立播放機隱藏）在拉下推桿時留下回音尾巴；`自動增益（0 dB 正規化）` 把音量偏小的歌平滑地提到接近 0 dBFS（最多 +9 dB，-42 dBFS 以下的靜音不提升），之後一定經過軟限幅，不會爆音（預設關閉）。共 21 個即時 DSP 掛點，不增加任何延遲；
- **雙碟機配對 2 軌視圖（#201，待實機驗證）**：兩台 SC5000 / SC6000 聯機時，在 `版面 → ENGINEX 視圖預設 → 雙碟機配對`（緊接 `四軌波形顯示`）開啟後，螢幕只顯示兩條全高波形：上面是本機目前 Layer（Layer 鍵照常在 Layer A / Layer B 之間切換），下面是另一台碟機目前軌的影子軌並跟隨對方的 Layer 切換；不用的軌隱藏，沒有對方時第 2 條顯示 `未連接配對碟機`。`四軌角色` 需為 `自動`、`1+3`、`2+4`、`1` 或 `2`；預設關閉，2 軌視圖維持原廠（本機 Layer A / Layer B）。
- **SC5000 / SC6000 原生四軌版面**：在獨立播放機上解鎖 Engine 內建的四軌介面，音效卡仍只用 2 路。每台播放機負責自己角色的兩軌（`自動`、`1+3`、`2+4`、`1+2`、`3+4`、`1`、`2`，在 `版面 → ENGINEX 視圖預設 → 四軌角色` 切換），其餘各軌是不出聲的**影子軌道**，跟隨另一台 EngineX 碟機或 CDJ 的曲目、播放 / CUE、位置、速度、Master 與同步；
- 向 Shoutcast / Icecast 伺服器 **直播推流**；
- **曲庫**：rekordbox 匯出的隨身碟（包含 6.8.5）即使有個別檔案損壞也能正常匯入，那幾首在載入時再分析；選取其他串流服務時，不再跳出 Engine 5.3.0 的「Connect to your Engine DJ Profile to use Tidal」提示；
- **Spotify 改用你自己的應用**：Spotify 限制每個應用的使用者數和請求量，所以不再內建共用的應用。到 `developer.spotify.com/dashboard` 免費建立一個應用，把網頁控制台裡顯示的 Redirect URI 加進去，再把它的 Client ID 填回控制台（`#70`）；
- **串流與影子軌道更穩**：在播放機上開著的串流歌單，背景服務重啟後照樣能載入，不再出現「Track not found」（`#73`）；影子軌道顯示真實歌名，沒有標籤的檔案也一樣，另一台從串流瀏覽器載入的歌也能跟隨；載入歌曲時打擊墊保持在 HOT CUE（`#71`）。

> **先鋒連線說明**：與先鋒 CDJ-3000 的連線互通（曲庫分享、3-Band 波形、60fps 播放頭與 Master / Beat Sync 對拍）是以維護者自研的 [**OpenProLink**](https://github.com/Cd1s/OpenProLink)（內建虛擬 CDJ-3000 模擬器，以及先鋒全系 CDJ / XDJ 碟機與 DJM 混音台的協定擷取工具 `prolink-probe`）與實機 Denon SC5000 聯調驗證。手邊暫時沒有先鋒 CDJ-3000 實體真機，接先鋒真機的表現還需要大家實測。歡迎擁有先鋒 CDJ-3000、CDJ-2000NXS2、XDJ-AZ / XZ 或先鋒混音台（DJM-A9 / DJM-V10 / DJM-900NXS2）真機的玩家前往 [Cd1s/OpenProLink](https://github.com/Cd1s/OpenProLink) 或本倉庫提交真機實測回報與設備 / 混音台協定封包資訊，共同完善跨品牌曲庫、混音台效果器速度同步（Beat FX Sync）、量化（Quantize）與 On-Air 連動！


---

<a id="gallery"></a>

## 實機畫面

<table>
<tr>
<td colspan="2" align="center" valign="top">
<img src="./docs/assets/screenshots/4deck-sc5000-cdj3000.png" width="49%" alt="原生 4 軌視圖與影子軌道"> <img src="./docs/assets/screenshots/opencdj-3000.png" width="49%" alt="OpenProLink 虛擬 CDJ-3000"><br>
<b>原生 4 軌視圖：兩台 SC5000 + 一台 CDJ-3000 同一區域網路連動</b><br>
<sub>右邊的 SC5000 負責第 2、4 軌；第 3 軌是開著 BEAT SYNC 的 CDJ-3000（OpenProLink 模擬器），波形和速度經 Pro DJ Link 即時跟隨</sub>
</td>
</tr>
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
<img src="./docs/assets/screenshots/zh-TW/03-sources-5-streaming-platforms.png" width="100%" alt="七大串流平台"><br>
<b>音源頁內建 7 大音樂平台</b><br>
<sub>TIDAL · 網易雲 · Spotify · Deezer · SoundCloud · Beatport · Apple Music 與隨身碟並列</sub>
</td>
<td width="50%" align="center" valign="top">
<img src="./docs/assets/screenshots/zh-TW/04-streaming-qr-login.png" width="100%" alt="手機掃碼登入"><br>
<b>螢幕 QR Code 掃碼登入</b><br>
<sub>手機掃一掃直接同步個人歌單，免輸入密碼</sub>
</td>
</tr>
<tr>
<td width="50%" align="center" valign="top">
<img src="./docs/assets/screenshots/zh-TW/08-control-center.png" width="100%" alt="控制中心"><br>
<b>下拉控制中心</b><br>
<sub>量化、智慧刷碟、波形樣式與智慧同步一屏直達</sub>
</td>
<td width="50%" align="center" valign="top">
<img src="./docs/assets/screenshots/zh-TW/11-layout-manager.png" width="100%" alt="版面視圖 1–5"><br>
<b>版面頁（視圖 1–5 與 EngineX 視圖預設）</b><br>
<sub>5 組預設視圖、熱標拍數倒數、介面縮放、雙碟機配對、四軌角色與影子軌模式</sub>
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
<sub>直接切換語言、Pad 燈亮度與電源開關</sub>
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

| 對比項目 | 官方原廠 / 重構前 | 重構後（`EngineX OS 5.3.0.4` 實測） | 實際刷碟提升 |
| :--- | :--- | :--- | :--- |
| **刷機包體積 (`*-Update.img`)** | 官方原廠 5.3.0 `147.79 MB`<br>重構前改版 `~210 MB` | **`151.44 MB`**（SC6000 `151.58 MB`，PRIME 4 `151.52 MB`） | **僅比官方原廠大 `3.64 MB`** |
| **純刷機開機硬碟佔用 (`/data`)** | 重構前殘留約 `243 MB` | **核心程式 + 三語介面僅佔 `17.2 MB`**（含擴充檔案系統庫共 `45.6 MB`） | 內建快閃記憶體多騰出 **近 `200 MB`** 空間 |
| **整機可用記憶體（總 `2 GB`）** | `~680 – 780 MB` | **`1,042 MB` 空閒可用**（主程式記憶體直降 `~150 MB`，背景服務合計僅佔 **`29.2 MB`**） | **多釋放 `~260 MB` 記憶體**，長時間演出不卡頓 |
| **音訊核心被背景搶佔率** | 下載或匯入時偶發佔用 `5% – 25%` | **`0.0%`**：所有下載與分軌任務隔離在獨立副核心，不碰音訊核心 | **96 kHz 聲音播放全程零斷音** |
| **先鋒 Rekordbox 隨身碟讀取速度** | 首次轉換約 `8 分鐘`，重掃 `15–24 秒` | **首次插碟 `< 2 秒`**，再次插碟 **`0.03 秒`** | 插上隨身碟即插即打，無需等待 |
| **4 分鐘歌曲 Stems 分軌耗時** | SC5000 原廠鎖死不可用 | 連電腦 / 雲端約 **`35 秒`**；播放機本機分軌比播放速度快 **`1.4 倍`** | 演出中途臨時拆人聲也能快速完成 |
| **大 BPM 差距同步** | BPM 差距超出變速範圍時 Sync 對不上，需要手動調大變速範圍 | **智慧同步**自動升檔變速範圍（最高 `±100%`），控制中心一鍵切 Bar / Beat / Tempo | 速度相差很大的兩首歌也能直接同步 |
| **網路傳輸速度** | 有線網路孔限制在 `100 Mbps` | 有線網路孔 **`1 Gbps`（1000 Mbps）全雙工**，8 MiB TCP 緩衝；Wi-Fi 實測約 `9～10 MB/s`（SDIO 25 MHz SDR12 匯流排） | 大曲庫走 Gigabit 有線網路孔傳輸 |

---

<a id="install"></a>

## 如何安裝與升級

### 方式零：全系列 16 款機型的 EngineX-Installer

EngineX-Installer 支援一代與二代機，不必輸入 IP 或選擇機型，透過 Wi-Fi SSH 或 USB Fastboot 辨識設備並下載對應的 `.img` 或資料包安裝。

- [Windows 10/11 x64](https://github.com/Cd1s/EngineX-OS/releases/latest/download/EngineX-Installer-5.3.0.4-windows-amd64.zip)
- [macOS Apple Silicon M1–M4](https://github.com/Cd1s/EngineX-OS/releases/latest/download/EngineX-Installer-5.3.0.4-macos-arm64.zip)
- [macOS Intel](https://github.com/Cd1s/EngineX-OS/releases/latest/download/EngineX-Installer-5.3.0.4-macos-amd64.zip)
- [Linux x86_64](https://github.com/Cd1s/EngineX-OS/releases/latest/download/EngineX-Installer-5.3.0.4-linux-amd64.tar.gz)

解壓後分別執行 `Install-EngineX.bat`、`Install-EngineX.command` 或 `./install-enginex.sh`。網路探索依賴 StageLinQ 或 EngineX Web 介面；SSH 需要 root 權限。Fastboot 需另外安裝 `fastboot` 和 Python 3，二代機映像製作還需要 e2fsprogs；Windows 的二代機 USB 路徑需在有 USB 存取權限的 WSL 中執行 Linux 套件。請先備份播放機：Fastboot 可能造成資料遺失，尤其是新建二代機 `/data` 映像檔。一代機 rootfs 刷寫為實驗功能，尚未在全部機型上驗證。

### 方式一：隨身碟刷機（首次安裝）
1. 點擊下方對應你機器型號的連結，直接下載升級包（或前往 [**Releases 頁面**](https://github.com/Cd1s/EngineX-OS/releases/latest)）：
   - **Denon DJ SC5000 Prime**：[`SC5000-5.3.0.4-EngineX-OS-Denon-SC5000-Prime-JP07-Update.img`](https://github.com/Cd1s/EngineX-OS/releases/latest/download/SC5000-5.3.0.4-EngineX-OS-Denon-SC5000-Prime-JP07-Update.img)
   - **Denon DJ SC5000M Prime**：[`SC5000M-5.3.0.4-EngineX-OS-Denon-SC5000M-Prime-JP08-Update.img`](https://github.com/Cd1s/EngineX-OS/releases/latest/download/SC5000M-5.3.0.4-EngineX-OS-Denon-SC5000M-Prime-JP08-Update.img)
   - **Denon DJ SC6000 Prime**：[`SC6000-5.3.0.4-EngineX-OS-Denon-SC6000-Prime-JP13-Update.img`](https://github.com/Cd1s/EngineX-OS/releases/latest/download/SC6000-5.3.0.4-EngineX-OS-Denon-SC6000-Prime-JP13-Update.img)
   - **Denon DJ SC6000M Prime**：[`SC6000M-5.3.0.4-EngineX-OS-Denon-SC6000M-Prime-JP14-Update.img`](https://github.com/Cd1s/EngineX-OS/releases/latest/download/SC6000M-5.3.0.4-EngineX-OS-Denon-SC6000M-Prime-JP14-Update.img)
   - **Denon DJ PRIME 4**：[`PRIME4-5.3.0.4-EngineX-OS-Denon-PRIME-4-JC11-Update.img`](https://github.com/Cd1s/EngineX-OS/releases/latest/download/PRIME4-5.3.0.4-EngineX-OS-Denon-PRIME-4-JC11-Update.img)
   - **Denon DJ PRIME 2**：[`PRIME2-5.3.0.4-EngineX-OS-Denon-PRIME-2-JC16-Update.img`](https://github.com/Cd1s/EngineX-OS/releases/latest/download/PRIME2-5.3.0.4-EngineX-OS-Denon-PRIME-2-JC16-Update.img)
   - **Denon DJ PRIME GO**：[`PRIMEGO-5.3.0.4-EngineX-OS-Denon-PRIME-GO-JP11-Update.img`](https://github.com/Cd1s/EngineX-OS/releases/latest/download/PRIMEGO-5.3.0.4-EngineX-OS-Denon-PRIME-GO-JP11-Update.img)
   - **Numark Mixstream Pro**：[`MIXSTREAMPRO-5.3.0.4-EngineX-OS-Numark-Mixstream-Pro-NH08-Update.img`](https://github.com/Cd1s/EngineX-OS/releases/latest/download/MIXSTREAMPRO-5.3.0.4-EngineX-OS-Numark-Mixstream-Pro-NH08-Update.img)
2. 將下載好的 `.img` 檔案放進 **FAT32 或 exFAT 格式隨身碟的根目錄**。
3. 將隨身碟插入播放機（原廠 Engine OS 4.3.x 與 5.0.x 都可以直接刷），進入 **設定 (Settings) → 關於 / 更新 (About / Update) → 韌體更新模式 (Firmware Update Mode)**，看到 `Found 5.3.0.4 on USB/SD` 時選 **USB/SD**，機器會自動完成刷入並重啟。如果只跳出 *Reboot into firmware update mode?*，代表播放機沒找到檔案：核對機型，檔名保持原樣放在根目錄，再拔插一次隨身碟。

### 方式一 b：二代機型（`/data` 資料包，Beta）

**一鍵自動安裝器 EngineX-Installer（推薦）：**不必輸入 IP 或選擇機型，支援 Wi-Fi SSH 和 USB Fastboot。下載適合電腦的平台套件：

- [Windows 10/11 x64](https://github.com/Cd1s/EngineX-OS/releases/latest/download/EngineX-Installer-5.3.0.4-windows-amd64.zip)
- [macOS Apple Silicon M1–M4](https://github.com/Cd1s/EngineX-OS/releases/latest/download/EngineX-Installer-5.3.0.4-macos-arm64.zip)
- [macOS Intel](https://github.com/Cd1s/EngineX-OS/releases/latest/download/EngineX-Installer-5.3.0.4-macos-amd64.zip)
- [Linux x64](https://github.com/Cd1s/EngineX-OS/releases/latest/download/EngineX-Installer-5.3.0.4-linux-amd64.tar.gz)

1. 先將播放機升級至官方 **Engine OS 5.3.0**。
2. 解壓安裝套件：Windows 雙擊 `Install-EngineX.bat`，macOS 雙擊 `Install-EngineX.command`，Linux 執行 `./install-enginex.sh`。
3. 安裝器自動尋找播放機並下載對應資料包。原廠機在螢幕點選 **設定 → 關於 / 更新 → 韌體更新模式（Firmware Update Mode）**，重開機後用 USB 連接電腦；Fastboot 自動辨識機型，確認後刷寫 `/data`。已開啟 root SSH 的機器將電腦與播放機連上同一網路，安裝器透過 StageLinQ（UDP 51337）及 EngineX Web 介面掃描找到設備，在 `/data/enginex` 安裝，保留曲庫與設定。能否找到設備取決於網路設定和 StageLinQ 是否可用。
4. 首次安裝後，後續版本可在播放機上透過 Wi-Fi 點選 **設定 → 關於 / 更新 → 更新** 線上升級（#86）。

SSH 需要播放機的 root 存取權限。USB Fastboot 需要另外安裝 `fastboot`、Python 3 和 e2fsprogs；Windows 請在有 USB 存取權限的 WSL 中以 root 執行（`wsl -u root`，經 `usbipd-win` 直通）Linux 套件。**刷入新的 `/data` 映像檔會清空該分割區上的音樂、曲庫和設定。**請先備份播放機資料，安裝前拔除隨身碟和 SD 卡。要保留 `/data`，請透過 `--userdata-backup` 提供該播放機的完整分割區映像檔。本 Beta 尚未在實機驗證。

**SC LIVE 4、SC LIVE 2、PRIME 4+、PRIME GO+、Mixstream Pro+、Mixstream Pro Go、PRIME 4 G2 和 Rane SYSTEM ONE**（#61、#68、#69、#76）沒有硬體 eFuse 熔斷鎖死，原廠更新程式（`az01-update` / `az0x-splash`）是在軟體層用 `denon-1` / `rane-1` 公鑰驗證 `AZ0x` 韌體容器簽章。在原廠未改裝狀態下，刷入修改過的 `.img` 或一代機的 `.img`（如 `PRIME4` / `PRIMEGO` / `MIXSTREAMPRO`）會在寫入前的 `Verifying`（驗證）階段直接被軟體拒簽退回，根本不會寫入快閃記憶體，更不會觸發熔斷或變磚。首次安裝請使用對應二代機型的 `Data-Bundle.tar.gz` 或 `EngineX-Installer`（裝入 `/data/enginex`，不改動唯讀系統分割區，移除即恢復原廠）；首次裝好後，後續新版本可直接在播放機螢幕 **設定 → 關於 / 更新** 一鍵線上 OTA 升級。

<details>
<summary>進階使用者：手動指令安裝 / 移除說明</summary>

下載對應機型的資料包，並用 Release 頁的 `SHA256SUMS.txt` 核對 SHA-256：
   - **Denon DJ SC LIVE 4** (JP21): [`SCLIVE4-5.3.0.4-EngineX-OS-Data-Bundle.tar.gz`](https://github.com/Cd1s/EngineX-OS/releases/latest/download/SCLIVE4-5.3.0.4-EngineX-OS-Data-Bundle.tar.gz)
   - **Denon DJ SC LIVE 2** (JP20): [`SCLIVE2-5.3.0.4-EngineX-OS-Data-Bundle.tar.gz`](https://github.com/Cd1s/EngineX-OS/releases/latest/download/SCLIVE2-5.3.0.4-EngineX-OS-Data-Bundle.tar.gz)
   - **Denon DJ PRIME 4+** (JC11S): [`PRIME4PLUS-5.3.0.4-EngineX-OS-Data-Bundle.tar.gz`](https://github.com/Cd1s/EngineX-OS/releases/latest/download/PRIME4PLUS-5.3.0.4-EngineX-OS-Data-Bundle.tar.gz)
   - **Denon DJ PRIME GO+** (JP11S): [`PRIMEGOPLUS-5.3.0.4-EngineX-OS-Data-Bundle.tar.gz`](https://github.com/Cd1s/EngineX-OS/releases/latest/download/PRIMEGOPLUS-5.3.0.4-EngineX-OS-Data-Bundle.tar.gz)
   - **Numark Mixstream Pro+** (NH08S): [`MIXSTREAMPROPLUS-5.3.0.4-EngineX-OS-Data-Bundle.tar.gz`](https://github.com/Cd1s/EngineX-OS/releases/latest/download/MIXSTREAMPROPLUS-5.3.0.4-EngineX-OS-Data-Bundle.tar.gz)
   - **Numark Mixstream Pro Go** (NH10): [`MIXSTREAMPROGO-5.3.0.4-EngineX-OS-Data-Bundle.tar.gz`](https://github.com/Cd1s/EngineX-OS/releases/latest/download/MIXSTREAMPROGO-5.3.0.4-EngineX-OS-Data-Bundle.tar.gz)
   - **Denon DJ PRIME 4 G2** (JP24): [`PRIME4G2-5.3.0.4-EngineX-OS-Data-Bundle.tar.gz`](https://github.com/Cd1s/EngineX-OS/releases/latest/download/PRIME4G2-5.3.0.4-EngineX-OS-Data-Bundle.tar.gz)
   - **Rane SYSTEM ONE** (RMZ2): [`SYSTEMONE-5.3.0.4-EngineX-OS-Data-Bundle.tar.gz`](https://github.com/Cd1s/EngineX-OS/releases/latest/download/SYSTEMONE-5.3.0.4-EngineX-OS-Data-Bundle.tar.gz)

**方法一（推薦）：在機器上安裝**。保留你的音樂、設定和曲庫。需要機器上的 root shell（SSH 或序列埠主控台）。

1. 把 `.tar.gz` 複製到隨身碟插到機器上（也可以透過網路複製過去）。
2. 在機器的 shell 裡解壓並安裝（把 `<model>` 和隨身碟名稱換成你的）：
   ```sh
   cp "/media/<隨身碟名稱>/<model>-5.3.0.4-EngineX-OS-Data-Bundle.tar.gz" /tmp/
   tar -xzf /tmp/<model>-5.3.0.4-EngineX-OS-Data-Bundle.tar.gz -C /tmp
   sh /tmp/<model>-5.3.0.4-EngineX-OS-Data-Bundle/install.sh
   reboot
   ```
3. 重開機後 Engine 先以原廠方式啟動一次，隨後自動以 EngineX OS 重新啟動。之後 `設定 → 裝置` 裡會出現 EngineX 的選項（`語言`、`電源 → 關機` 等）。

**方法二：電腦端 Fastboot 線刷**。只在機器上沒有 shell 時使用。需要一台裝有 `python3`、`e2fsprogs`（`debugfs`）和 `fastboot` 的 Linux 電腦（或 Windows WSL 以 root 執行）。
- **建議做法（EngineX-Installer 自動處理）**：直接執行安裝器。它能自動識別 fastboot 模式下的播放機，自動建立並格式化出廠預設 `/data` 分割區、注入資料包並刷入（**播放機 `/data` 裡的音樂、曲庫與設定會被清空**，安裝器會要求輸入 `yes` 確認；外接隨身碟與 SD 卡不受影響）。若已有這台播放機自己的完整 `/data` 備份，可透過 `--userdata-backup <映像>` 保留原有資料。後續還原原廠只需移除資料包，不依賴初次刷機前的預先備份。
- **手動操作（需已有 `/data` 分割區備份）**：
  1. 備份原檔不動，在副本上操作：`cp my-userdata-backup.img userdata-enginex.img`
  2. 在電腦上解壓資料包，再把它注入副本（兩個輔助腳本在包裡）：
     ```sh
     tar -xzf <model>-5.3.0.4-EngineX-OS-Data-Bundle.tar.gz
     python3 <model>-5.3.0.4-EngineX-OS-Data-Bundle/host-install-userdata.py userdata-enginex.img --bundle <model>-5.3.0.4-EngineX-OS-Data-Bundle.tar.gz
     ```
  3. 讓機器進入 fastboot 模式並用 USB 連上電腦，然後刷入並重開機：
     ```sh
     sh <model>-5.3.0.4-EngineX-OS-Data-Bundle/host-install-fastboot.sh userdata-enginex.img
     ```

**方法 3：沒有 fastboot 的 SYSTEM ONE / PRIME 4 G2（USB `1cc5:2027`）。** 在播放機上開啟 `設定 → 關於 / 更新 → 韌體更新模式` 點 `選項` 重開機進入更新模式，接上 USB 後執行 EngineX-Installer，它會自動完成下面幾步。這會寫入**全新的空** `/data` 分割區：內建儲存上的音樂、曲庫與設定會被清空（外接隨身碟與 SD 卡不受影響）。手動操作（需要 `python3`、`e2fsprogs` 與 `pip3 install pyusb`）：
   ```sh
   mke2fs -t ext4 -O encrypt -L data -b 4096 -F data.img 64M
   python3 <model>-5.3.0.4-EngineX-OS-Data-Bundle/host-install-userdata.py data.img --bundle <model>-5.3.0.4-EngineX-OS-Data-Bundle.tar.gz --ssh-key ~/.ssh/id_ed25519.pub
   python3 <model>-5.3.0.4-EngineX-OS-Data-Bundle/host-install-usbupdate.py data.img
   ```
   `--ssh-key` 可省略。寫完按電源鍵關機再開機。

**移除**：`sh /data/enginex/bundle/uninstall.sh && reboot`，只刪除資料包裝上的內容。

</details>

> **PRIME 4 G2 / SYSTEM ONE（64 位元 Engine）**：已包含 UI 包、串流閘道（Deezer、SoundCloud、Beatport）和 EngineX 各項服務。同時帶有 18 處 Engine 二進位補丁（含原廠 Apple Music 解鎖）、14 處音訊與過渡 DSP 防護、0dB 防破音軟限幅、自動增益、回聲淡出（Echo Out）、SQLite 調校，以及 64 位元 `OfflineAnalyzer` 的 `.perf` 波形直注（#149）。4 台面解鎖只適用於單機播放器；Stems 保持官方韌體原樣。這份 Engine 啟動要用機器的安全晶片（OP-TEE），沙箱裡只驗證了 hook、記憶體補丁和 UI 包載入，沒看到實際介面。

### 方式二：播放機螢幕一鍵線上升級（已安裝本系統後）
- 連接 Wi-Fi 或網路線後，開啟播放機 **設定 → 關於 / 更新**；
- 偵測到新版本時直接點擊螢幕上的 **「更新」** 按鈕，確認後播放機將自動進入原廠刷機進度條畫面完成下載與升級。
- **二代機**（SC LIVE 4 / 2、PRIME 4+、PRIME GO+、Mixstream Pro+ / Pro Go、PRIME 4 G2、SYSTEM ONE）首次裝好 `/data` 資料包後，之後的新版本（如 `5.3.0.4`）同樣在 **設定 → 關於 / 更新** 裡一鍵完成：播放機下載對應的 `Data-Bundle.tar.gz`，校驗 `SHA256SUMS`，一次性替換檔案並重啟 EngineX 服務，不會改動系統分割區（#86）。

### 選配：用電腦開啟高音質神經網路 Stems 分軌
播放機本機分軌為基礎頻譜模式。如需高音質神經網路分軌（預設自動安裝並啟用 `htdemucs`，支援顯示卡 / Apple MPS / CPU），在與播放機同一 Wi-Fi / 區域網路的電腦上解壓 `EngineX-Installer`，執行以下一行指令即可（原始碼倉庫下對應 `tools/stems/`；末尾加 `hpss` 可改用輕量頻譜模式）。播放機會透過區域網路自動探索電腦（UDP 17891 / mDNS，不必輸入 IP 或密碼），並於 **設定檔 → 曲庫** 與 **設定 → 關於 / 更新** 顯示 `高音質分軌：<主機名> (htdemucs)`：
```sh
# macOS / Linux（於解壓後的 EngineX-Installer 目錄；原始碼下為 tools/stems/run-home-worker.sh）
./stems/run-home-worker.sh

# Windows PowerShell（於解壓後的 EngineX-Installer 目錄；原始碼下為 tools\stems\run-home-worker.ps1）
powershell -ExecutionPolicy Bypass -File .\stems\run-home-worker.ps1
```

### 串流登入：只有播放機也能用
不需要電腦，也不需要伺服器。播放機和手機連同一個 Wi-Fi，在音源選單點開平台，用手機掃螢幕上的 QR Code 即可。手機上的登入頁預設英文，右上角可切換简体中文或繁體中文，每個頁面都有分步教學。

| 平台 | 怎麼登入 |
| --- | --- |
| 網易雲音樂 | 用網易雲音樂 App 掃播放機螢幕上的 QR Code 並確認。QR Code 過期後點一下就會換新的。 |
| Spotify | **方式 A（推薦）**：手機或電腦開啟 Spotify App 播放任意歌曲，點右下角「連接裝置」喇叭圖示，選擇 **`EngineX OS`** 約 1 秒自動完成帳號綁定。**方式 B**：點 Spotify 右側的「掃碼登入」用手機掃碼。直接從 Spotify 拉取 320kbps Ogg Vorbis 原生音訊播放。 |
| Deezer | 貼上你的 **ARL**（頁面教學示範怎樣在電腦上從 deezer.com 複製）。同一次登入為 Deezer 曲目開啟 Deemix 無損 FLAC 下載；網易雲只有選了「Deemix 優先」才會用到它。Spotify、Beatport、Apple Music 都只從自己的音源播放，不會跨平台備援；SoundCloud 僅在沒有 Go+ 時，才為 Go+ 專享曲目與非 Set 單曲改用已登入的 Deezer / 網易雲 / Beatport（見 SoundCloud 一行）。只填個人頁面連結則只顯示公開歌單、不能下載。 |
| SoundCloud | 填使用者名稱或主頁連結即可，不用密碼、不用 Artist Pro、也不用申請 API。私人歌單在「進階」裡貼上 soundcloud.com 的 `oauth_token`。每首歌依它能提供的最高音質播放：允許下載的用上傳者原檔，其次 256kbps AAC、160kbps AAC、128kbps MP3。Go+ 專享曲目與非 Set 單曲：已綁定 Go+ 帳號直連官方高音質串流；沒有 Go+ 時改用已登入的 Deezer / 網易雲 / Beatport 比對無損 / 320k 並快取為 `soundcloud-<id>.*`；超過 10 分鐘的 DJ Set 一律直連 SoundCloud。 |
| Apple Music | 已解鎖 Engine 5.3.0 內建的 Apple Music 音源，直接從 Apple Music 播放，EngineX 不再去 Deezer 或網易雲找替身。手機上仍可貼上歌單連結，或填 `media-user-token` 瀏覽自己的資料庫。 |
| Beatport | 填 Beatport 電子郵件和密碼，播放機只儲存登入權杖。 |

### 選配：自建登入中繼
中繼程式名為 `enginex-relay`（相容舊名 `sc5000-relay`）。中繼讓用行動網路的手機也能完成登入，並提供 Deezer 網頁內登入。在任何有公網 HTTPS 位址的伺服器上執行，然後在播放機網頁控制台（`http://<播放機 IP>:8080`）填入它的位址和密碼：
```sh
go build -o enginex-relay ./cmd/sc5000-relay
./enginex-relay -public https://relay.example.com -listen 0.0.0.0:6595
# 首次控制台密碼寫在 /var/lib/sc5000-relay/initial-password
```

**沒有網域？用 Cloudflare Quick Tunnel 一鍵公網連線。** 不需購買網域，也不用設定反向代理：
1. **播放機端一鍵開啟**：在播放機 `:8080` 主控台點擊「開啟免網域公網通道 (Cloudflare Quick Tunnel)」，系統會依架構自動安裝並啟動 `cloudflared`，數秒內產生 `https://*.trycloudflare.com` 公網 HTTPS 位址。手機用 4G/5G 行動網路即可遠端開啟主控台，播放機螢幕上的串流登入 QR Code 也會自動使用該公網位址回呼。
2. **自建 `enginex-relay` 免網域啟動**：執行 `enginex-relay -public auto`（或 `run-home-worker.sh --with-tunnel`），自動透過 Cloudflare Quick Tunnel 產生免網域公網 HTTPS 中繼位址，並彙整管理多台播放機。

`trycloudflare.com` 位址在通道重啟後會改變；需要固定位址請改用自己的網域。

---

## 安全說明：演出現場公共網路防護

EngineX OS 依照「和陌生人共用酒吧、Club、音樂節 Wi-Fi」的情境做了安全設計。

- **沒有任何預設 SSH 密碼。** 密碼登入已徹底關閉，公開韌體也**不內建任何 SSH 公鑰**，你自己沒放公鑰之前 SSH 根本不開放。用過舊版公開韌體的播放機，升級到 5.3.0.1 時會自動清除舊韌體帶的那把公鑰。
- **同一個 Wi-Fi 下的人控制不了你的播放機。** 播放機內部使用的所有介面（播放控制、切換波形、切換語言、線上升級、串流登入、Stems）只接受播放機自己（`127.0.0.1`）呼叫，別人就算和你連同一個網路，也無法遠端切你的歌、改設定或重新啟動機器。
- **手機網頁控制台必須輸入你這台播放機獨有的存取碼。** 開啟 `http://<播放機IP>:8080` 時要輸入播放機隨機產生的存取碼；網址和存取碼顯示在播放機的 `設定 → 關於 / 更新` 裡；串流掃碼登入使用的是會過期的一次性連結，不會開啟控制台。

進階使用者：

- 想用 SSH：把你自己的公鑰放進 `/data/enginex/authorized_keys`（例如自行建置帶公鑰的私有映像檔），重新開機後生效。只能用金鑰登入，不能用密碼。
- 控制台存取碼儲存在 `/data/enginex/state/access-token`，刪除這個檔案再重新開機就會產生新碼。
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
