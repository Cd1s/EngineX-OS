# Deck stuck on the boot screen after installing EngineX

Planned for EngineX 5.3.0.4 (unreleased).

## English

**What happens?** If the main UI never becomes ready for 180 seconds without reported startup progress, or Engine fails three times within five minutes, EngineX starts the original Engine without its preload hook or UI pack. Mods are quarantined. Networking and the web console remain available. Recovery does not run Factory Reset or erase music, libraries or settings.

**How do I send a report without SSH or an access code?** Leave the deck powered on and insert a writable USB stick or SD card. Within about five seconds of mounting, look in its root folder for `EngineX-boot-report-<time>.txt`. Safely eject the drive, review the redacted file on a computer, and attach it to your existing bug report with your model and EngineX version. A copy remains in `/data/enginex/logs/`. Read-only, full or fixed-storage drives cannot receive a report; use a removable USB stick or SD card, preferably FAT32/exFAT. The public `:8080` page shows recovery status, not log contents or an access code.

**How do I retry EngineX?** Fallback survives restarts and power cycles. If you know the Console Access Code, sign in to the web console and use its explicit boot-recovery retry action; playback stops. Quarantined mods stay disabled. Ordinary settings changes do not leave fallback. Installing a new EngineX version also permits a new attempt; reinstalling the same version keeps fallback. If you do not know the code, send the USB report first and wait for a corrected version.

## 简体中文

计划随 EngineX 5.3.0.4 发布，目前尚未发布。

**会发生什么？** 如果主界面在没有启动进展报告的情况下持续 180 秒未就绪，或 Engine 在五分钟内失败三次，EngineX 会启动不带自身预加载钩子和界面包的原厂 Engine，并隔离 Mod。网络和网页控制台继续运行。恢复不会执行恢复出厂设置，也不会擦除音乐、曲库或设置。

**没有 SSH 和访问码，怎样发送报告？** 保持设备通电，插入可写的 U 盘或 SD 卡。挂载后约五秒内，根目录会出现 `EngineX-boot-report-<time>.txt`。安全弹出介质，在电脑上检查已脱敏的文件，然后附到现有问题报告，并注明机型和 EngineX 版本。副本保存在 `/data/enginex/logs/`。只读、已满或被识别为固定盘的介质不会接收报告，请使用可移除 U 盘或 SD 卡，优先选 FAT32/exFAT。公开的 `:8080` 页面只显示恢复状态，不显示日志内容或访问码。

**怎样重试 EngineX？** 回退会跨重启和断电保留。如果知道控制台访问码，请登录网页控制台，使用明确的启动恢复重试操作；播放会中断。被隔离的 Mod 仍保持停用。普通设置修改不会退出回退。安装新的 EngineX 版本也会允许再次尝试；重装相同版本会保留回退。如果不知道访问码，请先发送 USB 报告，等待修正版。

## 繁體中文

計畫隨 EngineX 5.3.0.4 發布，目前尚未發布。

**會發生什麼？** 如果主介面在沒有啟動進展回報的情況下持續 180 秒未就緒，或 Engine 在五分鐘內失敗三次，EngineX 會啟動不帶自身預載入掛鉤和介面包的原廠 Engine，並隔離 Mod。網路和網頁主控台繼續執行。復原不會執行恢復出廠設定，也不會清除音樂、曲庫或設定。

**沒有 SSH 和存取碼，怎樣傳送報告？** 保持裝置通電，插入可寫入的隨身碟或 SD 卡。掛載後約五秒內，根目錄會出現 `EngineX-boot-report-<time>.txt`。安全退出媒體，在電腦上檢查已去識別化的檔案，再附到現有問題回報，並註明機型和 EngineX 版本。副本保存在 `/data/enginex/logs/`。唯讀、已滿或被識別為固定磁碟的媒體不會接收報告，請使用可移除隨身碟或 SD 卡，優先選 FAT32/exFAT。公開的 `:8080` 頁面只顯示復原狀態，不顯示日誌內容或存取碼。

**怎樣重試 EngineX？** 回退會跨重新啟動和斷電保留。如果知道主控台存取碼，請登入網頁主控台，使用明確的啟動復原重試操作；播放會中斷。被隔離的 Mod 仍保持停用。一般設定修改不會退出回退。安裝新的 EngineX 版本也會允許再次嘗試；重裝相同版本會保留回退。如果不知道存取碼，請先傳送 USB 報告，等待修正版。
