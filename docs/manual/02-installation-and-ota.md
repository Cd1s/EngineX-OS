# 02 · 安装、刷机与 OTA 在线升级 / Install, Flash & OTA Update / 安裝、刷機與 OTA 線上升級

> 简体中文 · English · 繁體中文，每一节按这个顺序对照。
> Every section is given in this order: 简体中文 · English · 繁體中文.
>
> 本文以 **EngineX OS 5.3.0.4** 为例；文件名里的版本号会随新版本变化，始终以 [Releases](https://github.com/Cd1s/EngineX-OS/releases/latest) 页面为准。
> Examples use **EngineX OS 5.3.0.4**; the version in file names changes with each release, so trust the [Releases](https://github.com/Cd1s/EngineX-OS/releases/latest) page.
> 本文以 **EngineX OS 5.3.0.4** 為例；檔名中的版本號會隨新版本變動，一律以 [Releases](https://github.com/Cd1s/EngineX-OS/releases/latest) 頁面為準。

## 目录 / Contents / 目錄

1. [先看自己的机器属于哪一类 / Which group is your deck / 先確認機型類別](#1)
2. [方法一：EngineX-Installer 一键安装（原厂二代机首次必用）/ Method 1 / 方法一](#2)
3. [方法二：U 盘离线刷入 `*-Update.img`（一代机直刷 / 已解锁二代机通用）/ Method 2 / 方法二](#3)
4. [方法三：碟机屏幕一键 OTA 升级 / Method 3 / 方法三](#4)
5. [常见报错与急救 / Troubleshooting / 常見錯誤與急救](#5)

---

<a id="1"></a>

## 1. 先看自己的机器属于哪一类 / Which group is your deck / 先確認機型類別

**简体中文**：EngineX OS 一共支持 16 款机型，分两代，**现已全部统一使用单文件 `*-Update.img` 升级包作为主安装与升级格式**（`5.3.0.4`/`5.3.0.5` Release 中同时保留 `*-Data-Bundle.tar.gz` 兼容包供旧版 `5.3.0.2`/`5.3.0.3` 二代机屏幕一键 OTA 过渡，`#215`）。不同机型的 `*-Update.img` **不能混用**。

**English**: EngineX OS supports 16 units in two generations, **all now unified on per-model `*-Update.img` packages** (`*-Data-Bundle.tar.gz` is also included alongside `*-Update.img` in `5.3.0.4`/`5.3.0.5` Releases so older `5.3.0.2`/`5.3.0.3` Gen-2 decks can one-tap OTA upgrade on screen, `#215`). Model files **must not be mixed**.

**繁體中文**：EngineX OS 共支援 16 款機型，分兩代，**現已全部統一使用單檔案 `*-Update.img` 升級包作為主安裝與升級格式**（`5.3.0.4`/`5.3.0.5` Release 中同時保留 `*-Data-Bundle.tar.gz` 相容包供舊版 `5.3.0.2`/`5.3.0.3` 二代機螢幕一鍵 OTA 過渡，`#215`）。不同機型的 `*-Update.img` **不可混用**。

| 代 / Gen | 机型 / Models / 機型 | 板号 / Board | 发布产物 / Release file / 發佈檔案 | 可用方法 / Methods / 可用方法 |
| :-- | :-- | :-- | :-- | :-- |
| 第一代 / 1st / 第一代 | SC5000 · SC5000M · SC6000 · SC6000M · PRIME 4 · PRIME 2 · PRIME GO · Mixstream Pro | JP07 · JP08 · JP13 · JP14 · JC11 · JC16 · JP11 · NH08 | `*-Update.img` 完整固件 / firmware / 完整韌體 | 方法二（U 盘直刷 / USB stick / USB 隨身碟）、方法一（安装器 / installer / 安裝器）、方法三（屏幕 OTA） |
| 第二代 / 2nd / 第二代 | SC LIVE 4 · SC LIVE 2 · PRIME 4+ · PRIME GO+ · Mixstream Pro+ · Mixstream Pro Go · PRIME 4 G2 · Rane SYSTEM ONE | JP21 · JP20 · JC11S · JP11S · NH08S · NH10 · JP24 · RMZ2 | `*-Update.img`（128 字节 SHA-256 头部 + `/data` 升级包） | **原厂机首次**：方法一（`EngineX-Installer` 一键解锁 + 刷入）<br>**解锁后每次更新**：方法二（U 盘免电脑刷入）或方法三（屏幕一键 OTA） |

> 第二代的 PRIME 4+ / PRIME GO+ / Mixstream Pro+ 与第一代的 PRIME 4 / PRIME GO / Mixstream Pro 名字很像，但**文件不能互刷**。
> The "+" models look like the first-generation ones but their files are **not interchangeable**.
> 第二代的「+」機型與第一代名稱相近，但**檔案不可互刷**。

**简体中文**

- **第一代**：固件没有签名保护，原厂状态下即可直接用 U 盘或安装器刷入 `*-Update.img`。
- **第二代**：没有硬件 eFuse 熔断锁死，运行**软件签名校验固件**（`AZ0x` 容器，密钥记录为 `denon-1` / `rane-1`），系统分区只读。原厂升级程序（`az01-update` / `az0x-splash`）在写入前先做软件验签，未解锁的原厂机直接刷入改版或一代机的 `.img` 会在 `Verifying`（校验）阶段直接退回，不会写入闪存、更不会变砖。**首次安装**请连接电脑运行一次 `EngineX-Installer`——它会自动完成 **`[1/2] 一键解锁二代 .img 升级链`** 与 **`[2/2] 刷入 *-Update.img`**（仅写入可写的 `/data/enginex` 分区，绝不改动只读系统分区）；**一旦解锁完成**（已有二代 EngineX 安装开机也会自动写入 `gen2-unlocked`），启动钩子会为 `az01-update --file`、`planck-remote-screen --update` 与 `az01-update --erase-data` 同步挂载验签/解包钩子，后续版本即可像一代机一样直接把 `*-Update.img` 放进 U 盘在碟机屏幕升级，或在屏幕一键 OTA 在线升级（甚至恢复出厂设置后仍保留解锁与 EngineX OS），也可随时回退原厂。
- **第二代的限制（请知悉）**：PRIME 4 G2 / SYSTEM ONE（64 位 Engine）现已对齐 16 处 Engine 补丁、14 处 DSP 防护、0dB 软限幅、自动增益、Echo Out、SQLite 调优和 `OfflineAnalyzer` `.perf` 直注（#149）；4-deck 解锁仅限单机播放器，Stems 保持原厂行为。
- 请认准对应二代型号前缀（`SCLIVE4` / `SCLIVE2` / `PRIME4PLUS` / `PRIMEGOPLUS` / `MIXSTREAMPROPLUS` / `MIXSTREAMPROGO` / `PRIME4G2` / `SYSTEMONE`）的 `*-Update.img`，或直接使用 `EngineX-Installer` 自动识别。

**English**

- **1st generation**: the firmware is not signature-locked, so a plain `*-Update.img` flashes directly from a USB stick or the installer even on stock firmware.
- **2nd generation**: has no hardware eFuse lock, and runs **software-verified signed firmware** (`AZ0x` container, key record `denon-1` / `rane-1`) with a read-only system partition. On an un-unlocked stock unit, a custom or first-generation `.img` is declined at the `Verifying` step without touching flash. **For the first install on a stock unit**, connect USB and run `EngineX-Installer` once — it automatically performs **`[1/2] One-Click Unlock`** of the 2nd-gen `.img` update chain and **`[2/2] Flashes *-Update.img`** into `/data/enginex` without touching the system partition. **Once unlocked** (existing 2nd-gen EngineX installs also seed `gen2-unlocked` automatically on boot), the boot activator injects the preload hook across `az01-update --file`, `planck-remote-screen --update`, and `az01-update --erase-data`, so every subsequent update installs from a USB stick (`*-Update.img`) or one-tap on-screen OTA just like a 1st-gen unit, survives Factory Reset, and can be reverted to stock anytime.
- **Limits of the 2nd generation (be aware)**: PRIME 4 G2 / SYSTEM ONE (64-bit Engine) include all 16 Engine patches, 14 DSP guards, the 0 dB Master Soft Clipper, auto gain, Echo Out, SQLite tuning and the `OfflineAnalyzer` `.perf` injection (#149); the 4-deck unlock is for standalone players only and Stems stay stock.
- Always match your exact 2nd-gen model prefix (`SCLIVE4` / `SCLIVE2` / `PRIME4PLUS` / `PRIMEGOPLUS` / `MIXSTREAMPROPLUS` / `MIXSTREAMPROGO` / `PRIME4G2` / `SYSTEMONE`) or let `EngineX-Installer` auto-detect it.

**繁體中文**

- **第一代**：韌體沒有簽章保護，原廠狀態下即可直接用隨身碟或安裝器刷入 `*-Update.img`。
- **第二代**：沒有硬體 eFuse 熔斷鎖死，執行**軟體驗證簽章韌體**（`AZ0x` 容器，金鑰記錄為 `denon-1` / `rane-1`），系統分割區唯讀。原廠更新程式（`az01-update` / `az0x-splash`）在寫入前先做軟體驗簽，未解鎖的原廠機直接刷入改版或一代機的 `.img` 會在 `Verifying`（驗證）階段直接退回，不會寫入快閃記憶體、更不會變磚。**首次安裝**請連接電腦執行一次 `EngineX-Installer`——它會自動完成 **`[1/2] 一鍵解鎖二代 .img 升級鏈`** 與 **`[2/2] 刷入 *-Update.img`**（僅寫入可寫入的 `/data/enginex` 分割區，絕不改動唯讀系統分割區）；**一旦解鎖完成**（既有二代 EngineX 安裝開機也會自動寫入 `gen2-unlocked`），啟動掛鉤會為 `az01-update --file`、`planck-remote-screen --update` 與 `az01-update --erase-data` 同步掛載驗簽/解包掛鉤，後續版本即可像一代機一樣直接把 `*-Update.img` 放進隨身碟於螢幕升級，或於螢幕一鍵 OTA 線上升級（甚至恢復原廠設定後仍保留解鎖與 EngineX OS），也可隨時還原原廠。
- **第二代的限制（請知悉）**：PRIME 4 G2 / SYSTEM ONE（64 位元 Engine）現已對齊 16 處 Engine 補丁、14 處 DSP 防護、0dB 軟限幅、自動增益、Echo Out、SQLite 調校與 `OfflineAnalyzer` `.perf` 直注（#149）；4-deck 解鎖僅限單機播放器，Stems 維持原廠行為。
- 請認準對應二代機型前綴（`SCLIVE4` / `SCLIVE2` / `PRIME4PLUS` / `PRIMEGOPLUS` / `MIXSTREAMPROPLUS` / `MIXSTREAMPROGO` / `PRIME4G2` / `SYSTEMONE`）的 `*-Update.img`，或直接使用 `EngineX-Installer` 自動識別。

> **运行目录 / Runtime directory / 執行目錄（#103）**
> 简体中文：标准运行目录统一为 **`/data/enginex/`**。旧版使用的 `/data/sc5000-custom` 会在开机时**自动无损迁移**到 `/data/enginex`，并保留软链接 `ln -sfn /data/enginex /data/sc5000-custom`，旧路径、旧脚本和旧教程继续可用。本文所有路径均以 `/data/enginex/` 为准；若你的碟机还是旧版，看到的是 `/data/sc5000-custom` 属正常现象。
> English: The standard runtime directory is now **`/data/enginex/`**. The old `/data/sc5000-custom` is **migrated losslessly at boot** and kept as a symlink (`ln -sfn /data/enginex /data/sc5000-custom`), so old paths, scripts and guides keep working. All paths here use `/data/enginex/`; on a deck still running an older release you will see `/data/sc5000-custom`, which is expected.
> 繁體中文：標準執行目錄統一為 **`/data/enginex/`**。舊版使用的 `/data/sc5000-custom` 會在開機時**自動無損遷移**至 `/data/enginex`，並保留符號連結 `ln -sfn /data/enginex /data/sc5000-custom`，舊路徑、舊腳本與舊教學仍可使用。本文所有路徑以 `/data/enginex/` 為準；若碟機仍是舊版，看到 `/data/sc5000-custom` 屬正常現象。

---

<a id="2"></a>

## 2. 方法一：EngineX-Installer 一键安装（推荐）/ Method 1: EngineX-Installer (recommended) / 方法一：EngineX-Installer 一鍵安裝（推薦）

**简体中文**：安装器会自己找碟机、判断型号、下载匹配的文件并校验，不需要你输入 IP、也不需要选型号。

**English**: The installer finds the deck, identifies the model, downloads the matching file and verifies it. You enter no IP and pick no model.

**繁體中文**：安裝器會自行找碟機、判斷機型、下載對應檔案並校驗，不需輸入 IP，也不必選機型。

### 2.1 下载 / Download / 下載

| 电脑 / Computer / 電腦 | 压缩包 / Archive / 壓縮檔 | 启动 / Start / 啟動 |
| :-- | :-- | :-- |
| Windows 10/11 x64 | `EngineX-Installer-5.3.0.4-windows-amd64.zip` | 双击 `Install-EngineX.bat` |
| macOS Apple Silicon (M1–M4) | `EngineX-Installer-5.3.0.4-macos-arm64.zip` | 双击 `Install-EngineX.command` |
| macOS Intel | `EngineX-Installer-5.3.0.4-macos-amd64.zip` | 双击 `Install-EngineX.command` |
| Linux x86_64 | `EngineX-Installer-5.3.0.4-linux-amd64.tar.gz` | `./install-enginex.sh` |

下载地址 / Download URL / 下載網址：`https://github.com/Cd1s/EngineX-OS/releases/latest/download/<上表文件名 / file name above / 上表檔名>`

**简体中文**：先解压再启动。窗口里的提示是中英双语；程序结束后窗口会停住，按回车才关闭，方便你看结果。

**English**: Extract first, then start. Messages are bilingual. The window stays open after the run until you press Enter, so you can read the result.

**繁體中文**：先解壓縮再啟動。視窗提示為中英雙語；程式結束後視窗會停住，按 Enter 才關閉，方便查看結果。

### 2.2 装之前先做 / Before you start / 開始前

**简体中文**

1. 备份碟机上的音乐、曲库和设置，尤其是用模式 B 时。
2. 拔掉碟机上的 U 盘和 SD 卡。
3. **第二代机型**：碟机必须先升到官方 **Engine OS 5.3.0**。
4. 电脑要能上网（程序从 GitHub 下载并校验 SHA-256）。

**English**

1. Back up the deck's music, library and settings, especially for Mode B.
2. Unplug USB sticks and SD cards from the deck.
3. **2nd-generation units**: update to official **Engine OS 5.3.0** first.
4. The computer needs internet access (the installer downloads from GitHub and checks the SHA-256).

**繁體中文**

1. 備份碟機上的音樂、曲庫與設定，使用模式 B 時尤其重要。
2. 拔掉碟機上的 USB 隨身碟與 SD 卡。
3. **第二代機型**：碟機須先升級到官方 **Engine OS 5.3.0**。
4. 電腦需可連網（程式從 GitHub 下載並校驗 SHA-256）。

### 2.3 它怎么选模式 / How it picks a mode / 如何選擇模式

**简体中文**：启动后，如果电脑已经通过 USB 看到处于 Fastboot（模式 B）或 `1cc5:2027` USB 更新模式（模式 C，`PRIME 4 G2` / `SYSTEM ONE`）的碟机，直接走对应的 USB 解锁刷入流程；否则在局域网里找碟机，走**模式 A**。在所有模式中，二代机统一呈现两阶段流程：**`[1/2] Unlocking 2nd-gen .img update chain`** → **`[2/2] Flashing *-Update.img`**。

**English**: At start, if a deck in Fastboot (Mode B) or `1cc5:2027` USB update mode (Mode C, `PRIME 4 G2` / `SYSTEM ONE`) is already visible over USB, it goes straight to the USB unlock-and-flash path; otherwise it looks for decks on the LAN and uses **Mode A**. Across all three modes, 2nd-gen units run a clear two-phase workflow: **`[1/2] Unlocking 2nd-gen .img update chain`** → **`[2/2] Flashing *-Update.img`**.

**繁體中文**：啟動後，若電腦已透過 USB 看到處於 Fastboot（模式 B）或 `1cc5:2027` USB 更新模式（模式 C，`PRIME 4 G2` / `SYSTEM ONE`）的碟機，直接走對應的 USB 解鎖刷入流程；否則在區域網路中找碟機，走**模式 A**。在所有模式中，二代機統一呈現兩階段流程：**`[1/2] Unlocking 2nd-gen .img update chain`** → **`[2/2] Flashing *-Update.img`**。

### 2.4 模式 A：正常开机、不插 USB 线 / Mode A: deck running, no USB cable / 模式 A：正常開機、不接 USB 線

**适用 / Use when / 適用**：碟机已开机、与电脑在同一个路由器，且碟机已允许 root SSH（或已在运行 EngineX OS）。
Deck powered on, same router as the computer, and root SSH is available (or EngineX OS is already running).
碟機已開機、與電腦在同一台路由器，且已可使用 root SSH（或已在執行 EngineX OS）。

**简体中文：它做了什么**

1. **找碟机**（不用你查 IP）：
   - 监听 **StageLinQ（UDP 51337）**广播约 1.5 秒，碟机会把自己的板号作为软件名广播出来；
   - 同时扫描电脑所在的 **/24 网段**，找响应 `http://<IP>:8080/v1/sc5000/ui` 的 EngineX Web UI。
   - 其它 StageLinQ 设备（如调音台、软件）会被跳过。
2. 通过 SSH（用户 `root`）登录：依次尝试 `~/.ssh/id_ed25519`、`id_rsa`、`id_ecdsa`，再试密码（`--password` 指定的，以及 `denon`——用于已被 FreeMyPrime 解锁的机器）。
3. 读取 `/sys/firmware/devicetree/base/compatible` 得到板号，判断型号和代别。
4. **第二代**：自动发现并下载该型号的 `<PREFIX>-<VERSION>-EngineX-OS-<Model>-Update.img`（核对 GitHub SHA-256、128 字节 `ENGINEX_DATA_BUNDLE_IMG_V1` SHA-256 头部与包内 `SHA256SUMS`），执行 **`[1/2] 解锁二代 .img 升级链`** 与 **`[2/2] 刷入 *-Update.img`**，通过 SSH 流式部署到 `/data/enginex`、读回 `version.txt` 核对版本并运行 `boot-activate.sh`，Engine 重启后即是 EngineX OS。**音乐、曲库、设置保留**。
5. **第一代**：下载 `*-Update.img`（必须有 GitHub SHA-256），要求你输入 `yes` 确认，传到碟机 `/data/az01-update.img`，校验后调用 `az01-update --file` 刷入。

**English: what it does**

1. **Find the deck** (no IP lookup): it listens for **StageLinQ (UDP 51337)** for about 1.5 s (decks announce their board code as the software name) and in parallel scans the computer's **/24 subnet** for the EngineX Web UI at `http://<IP>:8080/v1/sc5000/ui`. Other StageLinQ devices are skipped.
2. It logs in over SSH as `root`: keys `~/.ssh/id_ed25519`, `id_rsa`, `id_ecdsa`, then passwords (`--password`, and `denon` for FreeMyPrime-unlocked decks).
3. It reads `/sys/firmware/devicetree/base/compatible` to get the board and generation.
4. **2nd generation**: downloads the model's `<PREFIX>-<VERSION>-EngineX-OS-<Model>-Update.img` (checking GitHub's SHA-256, the 128-byte `ENGINEX_DATA_BUNDLE_IMG_V1` SHA-256 header, and the payload's `SHA256SUMS`), runs **`[1/2] Unlocking 2nd-gen .img update chain`** and **`[2/2] Flashing *-Update.img`** over SSH into `/data/enginex`, reads `version.txt` back, and runs `boot-activate.sh`; Engine restarts as EngineX OS. **Music, library and settings stay.**
5. **1st generation**: downloads the `*-Update.img` (a GitHub SHA-256 is required), asks you to type `yes`, uploads it to `/data/az01-update.img`, verifies it and calls `az01-update --file`.

**繁體中文：它做了什麼**

1. **找碟機**（不必查 IP）：監聽 **StageLinQ（UDP 51337）**約 1.5 秒，碟機會以板號作為軟體名稱廣播；同時掃描電腦所在 **/24 網段**，尋找回應 `http://<IP>:8080/v1/sc5000/ui` 的 EngineX Web UI。其他 StageLinQ 裝置會被略過。
2. 以 `root` 透過 SSH 登入：依序嘗試 `~/.ssh/id_ed25519`、`id_rsa`、`id_ecdsa`，再試密碼（`--password` 指定者，以及用於 FreeMyPrime 已解鎖機器的 `denon`）。
3. 讀取 `/sys/firmware/devicetree/base/compatible` 取得板號，判斷機型與世代。
4. **第二代**：自動發現並下載該機型的 `<PREFIX>-<VERSION>-EngineX-OS-<Model>-Update.img`（核對 GitHub SHA-256、128 位元組 `ENGINEX_DATA_BUNDLE_IMG_V1` SHA-256 標頭與包內 `SHA256SUMS`），執行 **`[1/2] 解鎖二代 .img 升級鏈`** 與 **`[2/2] 刷入 *-Update.img`**，經 SSH 串流部署到 `/data/enginex`、讀回 `version.txt` 核對版本並執行 `boot-activate.sh`；Engine 重啟後即為 EngineX OS。**音樂、曲庫、設定保留。**
5. **第一代**：下載 `*-Update.img`（必須有 GitHub SHA-256），要求輸入 `yes` 確認，傳到碟機 `/data/az01-update.img`，校驗後呼叫 `az01-update --file` 刷入。

**注意 / Notes / 注意**

- 公开版镜像**不内置任何 SSH 公钥**，原厂机也默认没有可用的 root SSH。所以一台从没解锁过的机器，模式 A 登录不上——这时程序会自动转入下面的模式 B 等待。
  The public image ships **no SSH key**, and a stock deck has no usable root SSH. A never-unlocked deck therefore cannot be logged into; the installer then switches to the Fastboot wait of Mode B.
  公開版映像**不內建任何 SSH 公鑰**，原廠機也沒有可用的 root SSH。從未解鎖的機器無法登入，此時安裝器會自動轉入模式 B 的等待流程。
- 如果 SSH 能连上但**认证失败**，程序会停下并提示用 `--key` 或 `--password`，不会继续。
  If SSH connects but **authentication fails**, it stops and tells you to use `--key` or `--password`.
  若 SSH 可連上但**認證失敗**，程式會停止並提示改用 `--key` 或 `--password`。
- 电脑与碟机不在同一个 /24 网段（如访客 Wi-Fi 隔离、不同 VLAN）时可能找不到，请接到同一个路由器。
  Decks on a different /24 subnet (guest Wi-Fi isolation, VLANs) may not be found; join the same router.
  電腦與碟機不在同一 /24 網段（訪客 Wi-Fi 隔離、不同 VLAN）時可能找不到，請連到同一台路由器。

### 2.5 模式 B：USB 线 + Fastboot / Mode B: USB cable + Fastboot / 模式 B：USB 線 + Fastboot

**什么时候用 / When / 何時使用**

- **简体中文**：原厂第二代机、从没装过、没有 SSH，这是首次安装的方式；或者模式 A 找到了碟机却登不上 SSH。
- **English**: A stock 2nd-generation deck with no SSH, installed for the first time; or Mode A found the deck but cannot log in.
- **繁體中文**：原廠第二代機、從未安裝過、沒有 SSH，這是首次安裝的方式；或模式 A 找到碟機卻無法登入 SSH。

> ⚠️ **为什么电脑刷机工具看不到 PRIME GO / SC5000？（`#104`） / Why can't the computer tool see my PRIME GO / SC5000? / 為什麼電腦刷機工具看不到 PRIME GO / SC5000？**
>
> - **推荐：免电脑 U 盘直接刷。/ Recommended: USB stick, no computer. / 推薦：免電腦隨身碟直接刷。** 一代机（SC5000 / SC6000 / PRIME GO / PRIME 4 / PRIME 2）无需连接电脑，把 `*-Update.img` 放进 FAT32 / exFAT U 盘根目录插到碟机，进入 `Settings → Update → Reboot` 即可更新或恢复固件。/ First-generation units need no computer: put the `*-Update.img` in the root of a FAT32 / exFAT stick, plug it in and go to `Settings → Update → Reboot` to update or recover. / 一代機無需連接電腦，把 `*-Update.img` 放進 FAT32 / exFAT 隨身碟根目錄插到碟機，進入 `Settings → Update → Reboot` 即可更新或還原韌體。
> - **电脑 USB 线刷模式 / Computer USB flashing mode / 電腦 USB 線刷模式：** 正常开机打碟时，电脑**绝对看不到**刷机设备！必须先在碟机屏幕点 **`Settings → Update → Reboot to Computer Update Mode`**（重启进入蓝屏电脑图标的 Fastboot 模式），再用 USB-B 线连电脑运行刷机工具。/ While the deck runs normally the computer **never** sees a flashing device. First tap **`Settings → Update → Reboot to Computer Update Mode`** on the deck (it reboots into Fastboot mode with the blue-screen computer icon), then connect a USB-B cable and run the tool. / 正常開機打碟時，電腦**絕對看不到**刷機裝置！必須先在碟機螢幕點 **`Settings → Update → Reboot to Computer Update Mode`**（重啟進入藍屏電腦圖示的 Fastboot 模式），再以 USB-B 線連接電腦執行刷機工具。

**进入 Fastboot / Enter Fastboot / 進入 Fastboot**

**简体中文**：每款机型的统一做法是在碟机屏幕上进入固件升级模式，再接 USB：

1. 在碟机上点 **Settings → Update → Reboot to Computer Update Mode**（旧版菜单：Settings → About / Update → Firmware Update Mode）；
2. 碟机重启进入蓝屏电脑图标的 Fastboot 模式后，用 USB-B 线连接电脑；
3. 启动安装器，它会自动轮询 `fastboot devices` 等待连接（最长 10 分钟）。

> 本仓库没有记录各机型进入 Fastboot 的硬件组合键，所以这里不提供，以免误导。请使用屏幕菜单这条路径。

**English**: The same on-screen route works for every model, then connect USB:

1. On the deck tap **Settings → Update → Reboot to Computer Update Mode** (older menus: Settings → About / Update → Firmware Update Mode);
2. once it has rebooted into Fastboot mode (blue-screen computer icon), connect a USB-B cable to the computer;
3. start the installer; it polls `fastboot devices` and waits for up to 10 minutes.

> This repository records no per-model hardware key combination for Fastboot, so none is given here rather than guessing. Use the on-screen route.

**繁體中文**：各機型統一在碟機螢幕上進入韌體升級模式，再接 USB：

1. 在碟機上點 **Settings → Update → Reboot to Computer Update Mode**（舊版選單：Settings → About / Update → Firmware Update Mode）；
2. 碟機重啟進入藍屏電腦圖示的 Fastboot 模式後，以 USB-B 線連接電腦；
3. 啟動安裝器，它會自動輪詢 `fastboot devices` 等待連線（最長 10 分鐘）。

> 本專案未記載各機型進入 Fastboot 的硬體組合鍵，因此不在此列出以免誤導。請使用螢幕選單路徑。

**简体中文：模式 B / 模式 C 做了什么（第二代首次解锁 + 刷入）**

1. 模式 B 通过 `fastboot getvar product` 读出板号对照型号表（如 `jp21` → SC LIVE 4）；`PRIME 4 G2`（JP24）与 `SYSTEM ONE`（RMZ2）无 fastboot，走模式 C（`1cc5:2027` `usbupdate`）。**未知板号一律不刷**。
2. 下载并校验对应型号的 `<PREFIX>-<VERSION>-EngineX-OS-<Model>-Update.img`（校验 128 字节 `ENGINEX_DATA_BUNDLE_IMG_V1` SHA-256 头部与包内 `SHA256SUMS`）。
3. 依次展示并完成 **`[1/2] Unlocking 2nd-gen .img update chain`** 与 **`[2/2] Flashing *-Update.img`**：查询分区大小（优先 `data`，否则 `userdata`；模式 C 为 64 MiB `data` 镜像），并用 `host-install-userdata.py`（经 `debugfs`）将 `*-Update.img` 载荷、`gen2-unlocked` 解锁标记与 ALSA 启动钩子写进去：
   - **推荐（模式 B）**：加 `--userdata-backup 你自己这台碟机的data备份.img`，在备份的**副本**上注入，**保留你的音乐和设置**；
   - 不加该参数：程序会新建一个**全新的空 `/data` 镜像**，刷入后**碟机上的音乐、曲库、设置全部清空**。此时程序会特别警告并要求你输入 `yes`。
   > 注：碟机原厂 U-Boot 的 fastboot 协议不支持导出分区（`fastboot fetch` 未实现），因此在未获得 shell 权限的原厂机上无法直接通过 fastboot 备份 `/data`。首次通过模式 B / C 刷机若无预先提取的备份，将刷入全新分区（机身 `/data` 里的音乐、曲库与设置清空，外接 U 盘/SD 卡不受影响）。**完成首次解锁后，后续所有升级均可直接插 U 盘刷 `*-Update.img` 或在屏幕一键 OTA，不再覆盖 `/data` 分区！** 二代机回退原厂并不依赖预先备份：EngineX 仅改动 `/data` 分区，刷机后可通过本地脚本（`/data/enginex/bundle/uninstall.sh`）、Web 控制台回退按钮或网络运行安装器 `--uninstall` 参数一键恢复原厂启动（保留用户曲库、设置与 SSH 密钥，重启后进入原厂 Engine OS）。
4. 模式 B 执行 `fastboot flash <分区> 镜像` 后 `fastboot reboot`；模式 C 执行 `write-part` 写入后手动按电源键关机再开机。
5. 首次开机先以原厂状态启动一次，随后 Engine 会自动重启并载入 EngineX OS。

> 模式 B / C 不会开启 SSH。公开版没有内置 SSH 密钥，如要用 SSH 需自行加入公钥（见 README 的 Security 一节）。

**English: what Mode B / Mode C does (2nd generation first-time unlock + flash)**

1. Mode B reads the board code via `fastboot getvar product` (for example `jp21` → SC LIVE 4), while `PRIME 4 G2` (JP24) and `SYSTEM ONE` (RMZ2) use Mode C (`1cc5:2027` `usbupdate`). **An unknown board is never flashed.**
2. It downloads and verifies the model's `<PREFIX>-<VERSION>-EngineX-OS-<Model>-Update.img` (checking both the 128-byte `ENGINEX_DATA_BUNDLE_IMG_V1` SHA-256 header and the payload's `SHA256SUMS`).
3. It runs **`[1/2] Unlocking 2nd-gen .img update chain`** → **`[2/2] Flashing *-Update.img`**: reads the target partition size (`data` if reported, else `userdata`; 64 MiB `data` image in Mode C) and injects the `*-Update.img` payload, `gen2-unlocked` flag, and ALSA boot hook via `host-install-userdata.py` (`debugfs`):
   - **Recommended (Mode B)**: pass `--userdata-backup <a backup image of this deck's own data>`; it injects into a **copy** and **keeps your music and settings**;
   - without it the installer makes a **brand-new empty `/data` image**, and **all music, library and settings on the deck are erased**. It warns loudly and asks you to type `yes`.
   > Note: Stock inMusic U-Boot fastboot does not implement partition dumping (`fastboot fetch` is not implemented). On a locked stock deck without previous shell access, `/data` cannot be pulled over fastboot. The first Mode B / C install without an existing backup initializes a fresh partition (music, library and settings on the deck's `/data` are erased; external USB/SD content is untouched). **Once unlocked, every future update installs straight from a USB stick (`*-Update.img`) or on-screen OTA without wiping `/data`!** Reverting to stock on Gen 2 does not require a pre-install backup: because EngineX OS only touches `/data` while system partitions remain untouched, you can revert anytime via `/data/enginex/bundle/uninstall.sh`, the Web Console "Revert to Stock" button, or `EngineX-Installer --uninstall` over the network.
4. Mode B runs `fastboot flash <partition> <image>` then `fastboot reboot`; Mode C writes via `write-part` and you power-cycle the deck with the power button.
5. The first boot runs stock Engine once, then Engine restarts by itself with EngineX OS.

> Mode B / C does not enable SSH. The public build ships no SSH key; to use SSH you add your own public key (see the Security section of the README).

**繁體中文：模式 B / 模式 C 做了什麼（第二代首次解鎖 + 刷入）**

1. 模式 B 透過 `fastboot getvar product` 讀出板號並對照機型表（如 `jp21` → SC LIVE 4）；`PRIME 4 G2`（JP24）與 `SYSTEM ONE`（RMZ2）無 fastboot，走模式 C（`1cc5:2027` `usbupdate`）。**未知板號一律不刷**。
2. 下載並校驗對應機型的 `<PREFIX>-<VERSION>-EngineX-OS-<Model>-Update.img`（校驗 128 位元組 `ENGINEX_DATA_BUNDLE_IMG_V1` SHA-256 標頭與包內 `SHA256SUMS`）。
3. 依序展示並完成 **`[1/2] Unlocking 2nd-gen .img update chain`** 與 **`[2/2] Flashing *-Update.img`**：查詢分割區大小（有回報大小用 `data`，否則用 `userdata`；模式 C 為 64 MiB `data` 映像），並用 `host-install-userdata.py`（經 `debugfs`）寫入 `*-Update.img` 載荷、`gen2-unlocked` 解鎖標記與 ALSA 啟動 hook：
   - **建議（模式 B）**：加上 `--userdata-backup <這台碟機自己的 data 備份映像>`，在備份的**副本**上注入，**保留音樂與設定**；
   - 不加此參數：程式會建立**全新的空 `/data` 映像**，刷入後**碟機上的音樂、曲庫、設定全部清空**；程式會明確警告並要求輸入 `yes`。
   > 註：播放機原廠 U-Boot 的 fastboot 協定不支援匯出分割區（未實作 `fastboot fetch`），因此在未取得 shell 權限的原廠機上無法直接透過 fastboot 備份 `/data`。首次透過模式 B / C 刷機若無預先提取的備份，將刷入全新分割區（機身 `/data` 裡的音樂、曲庫與設定清空，外接隨身碟/SD 卡不受影響）。**完成首次解鎖後，後續所有升級皆可直接插隨身碟刷 `*-Update.img` 或於螢幕一鍵 OTA，不再覆寫 `/data` 分割區！** 二代機回退原廠並不依賴預先備份：EngineX 僅改動 `/data` 分割區，刷機後可透過本機指令稿（`/data/enginex/bundle/uninstall.sh`）、Web 控制台回退按鈕或網路執行安裝器 `--uninstall` 參數一鍵還原原廠開機。
4. 模式 B 執行 `fastboot flash <分割區> <映像>` 後 `fastboot reboot`；模式 C 執行 `write-part` 寫入後手動按電源鍵關機再開機。
5. 首次開機先以原廠狀態啟動一次，之後 Engine 會自動重啟並載入 EngineX OS。

> 模式 B / C 不會開啟 SSH。公開版不內建 SSH 金鑰；如需使用 SSH，請自行加入公鑰（見 README 的 Security 一節）。

**第一代走模式 B / 1st generation in Mode B / 第一代走模式 B**

- 中：安装器能识别并走 Fastboot 刷 rootfs，但这条路**标注为实验性**，没有在所有型号上验证。第一代请优先用方法二（U 盘）。
- EN: The installer can unlock and flash the rootfs of a 1st-generation deck over Fastboot, but that path is **experimental** and not validated on all models. Prefer Method 2 (USB stick).
- 繁：安裝器可對第一代走 Fastboot 刷 rootfs，但此路徑**屬實驗性**，並未在所有機型驗證。第一代請優先使用方法二（USB 隨身碟）。

### 2.6 Fastboot 依赖 / Fastboot requirements / Fastboot 需求

| 项目 / Item / 項目 | 说明 / Detail / 說明 |
| :-- | :-- |
| `fastboot` | Android platform-tools，需在 PATH 中 / must be in PATH / 須在 PATH 中 |
| Python 3 | 注入 `/data` 镜像用 / injects the `/data` image / 用於注入 `/data` 映像 |
| e2fsprogs | `debugfs`、`mke2fs`（新建镜像时）/ (and `mke2fs` for a new image) / （新建映像時需 `mke2fs`） |
| Windows | 请在 **WSL** 中用 Linux 包，并通过 `usbipd` 直通 USB；WSL 中须安装 `fastboot`、`python3` 与 `e2fsprogs`，并以 `wsl -u root` 运行（避免直通 USB 节点权限不足）/ use the Linux package in **WSL** with USB forwarded via `usbipd`; install `fastboot`, `python3`, and `e2fsprogs` in WSL, and run as root via `wsl -u root` (usbipd device nodes require root) / 請在 **WSL** 中使用 Linux 版，並透過 `usbipd` 直通 USB；WSL 中須安裝 `fastboot`、`python3` 與 `e2fsprogs`，並以 `wsl -u root` 執行（避免直通 USB 節點權限不足） |

### 2.7 常用参数 / Useful flags / 常用參數

| 参数 / Flag / 參數 | 作用 / Effect / 作用 |
| :-- | :-- |
| `--yes` | 不再询问确认（慎用）/ skip confirmations (use with care) / 不再詢問確認（慎用） |
| `--image <文件>` / `--bundle <文件>` | 使用本地 `*-Update.img`（同时兼容旧版 `.tar.gz`），文件名前缀必须与型号一致，否则拒绝 / use a local `*-Update.img` (legacy `.tar.gz` stays compatible); a wrong-model prefix is refused / 使用本機 `*-Update.img`（相容舊版 `.tar.gz`），檔名前綴須與機型相符，否則拒絕 |
| `--userdata-backup <文件>` | 模式 B：在你自己的 `/data` 备份上注入，保留数据 / Mode B: inject into your own `/data` backup / 模式 B：在自己的 `/data` 備份上注入，保留資料 |
| `--key <私钥>` | 模式 A：指定 SSH 私钥 / SSH private key / 指定 SSH 私鑰 |
| `--password <密码>` | 模式 A：SSH root 密码 / SSH root password / SSH root 密碼 |

---

<a id="3"></a>

## 3. 方法二：U 盘离线刷入 `*-Update.img`（一代机直刷 / 已解锁二代机通用） / Method 2: offline USB `*-Update.img` flash (1st-gen & unlocked 2nd-gen) / 方法二：USB 隨身碟離線刷入 `*-Update.img`（第一代直刷 / 已解鎖第二代通用）

**第一代 8 款原厂固件（4.3.x 与 5.0.x+）可直接刷入；第二代 8 款在通过方法一完成过一次解锁（或已运行旧版 EngineX OS）后，同样可直接用 U 盘免电脑刷入。**
**All 8 first-generation models flash directly from stock 4.3.x / 5.0.x+; all 8 second-generation models use the exact same USB flow once unlocked with Method 1 (or upgraded from an earlier EngineX OS install).**
**第一代 8 款原廠 4.3.x / 5.0.x+ 可直接刷入；第二代 8 款經方法一完成首次解鎖（或已執行舊版 EngineX OS）後，同樣可直接以隨身碟免電腦刷入。**

> ⚠️ **为什么电脑刷机工具看不到 PRIME GO / SC5000？（`#104`） / Why can't the computer tool see my PRIME GO / SC5000? / 為什麼電腦刷機工具看不到 PRIME GO / SC5000？**
>
> - **推荐：免电脑 U 盘直接刷。/ Recommended: USB stick, no computer. / 推薦：免電腦隨身碟直接刷。** 一代机与已解锁的二代机无需连接电脑，把对应机型的 `*-Update.img` 放进 FAT32 / exFAT U 盘根目录插到碟机，进入 `Settings → About / Update → Firmware Update Mode → Options → USB/SD` 即可更新固件。/ 1st-gen and unlocked 2nd-gen units need no computer: put the matching `*-Update.img` in the root of a FAT32 / exFAT stick, plug it in and open `Settings → About / Update → Firmware Update Mode → Options → USB/SD` to update. / 一代機與已解鎖的二代機無需連接電腦，把對應機型的 `*-Update.img` 放進 FAT32 / exFAT 隨身碟根目錄插到碟機，進入 `Settings → About / Update → Firmware Update Mode → Options → USB/SD` 即可更新韌體。
> - **电脑 USB 线刷模式 / Computer USB flashing mode / 電腦 USB 線刷模式：** 正常开机打碟时，电脑**绝对看不到**刷机设备！必须先在碟机屏幕点 **`Settings → About / Update → Firmware Update Mode → Options`**（重启进入蓝屏电脑图标的固件更新模式），再用 USB 线连电脑运行刷机工具。/ While the deck runs normally the computer **never** sees a flashing device. First tap **`Settings → About / Update → Firmware Update Mode → Options`** on the deck (it reboots into Firmware Update Mode with the blue-screen computer icon), then connect a USB cable and run the tool. / 正常開機打碟時，電腦**絕對看不到**刷機裝置！必須先在碟機螢幕點 **`Settings → About / Update → Firmware Update Mode → Options`**（重啟進入藍屏電腦圖示的韌體更新模式），再以 USB 線連接電腦執行刷機工具。

**简体中文**

1. **选对文件**。到 [Releases](https://github.com/Cd1s/EngineX-OS/releases/latest) 下载你机型对应的 `*-Update.img`（共 16 款）：

   | 代 / Gen | 机型 | 完整文件名（以 `5.3.0.4` 为例） |
   | :-- | :-- | :-- |
   | 一代 / 1st | SC5000 | `SC5000-5.3.0.4-EngineX-OS-Denon-SC5000-Prime-JP07-Update.img` |
   | 一代 / 1st | SC5000M | `SC5000M-5.3.0.4-EngineX-OS-Denon-SC5000M-Prime-JP08-Update.img` |
   | 一代 / 1st | PRIME GO | `PRIMEGO-5.3.0.4-EngineX-OS-Denon-PRIME-GO-JP11-Update.img` |
   | 一代 / 1st | SC6000 | `SC6000-5.3.0.4-EngineX-OS-Denon-SC6000-Prime-JP13-Update.img` |
   | 一代 / 1st | SC6000M | `SC6000M-5.3.0.4-EngineX-OS-Denon-SC6000M-Prime-JP14-Update.img` |
   | 一代 / 1st | PRIME 4 | `PRIME4-5.3.0.4-EngineX-OS-Denon-PRIME-4-JC11-Update.img` |
   | 一代 / 1st | PRIME 2 | `PRIME2-5.3.0.4-EngineX-OS-Denon-PRIME-2-JC16-Update.img` |
   | 一代 / 1st | Mixstream Pro | `MIXSTREAMPRO-5.3.0.4-EngineX-OS-Numark-Mixstream-Pro-NH08-Update.img` |
   | 二代 / 2nd | SC LIVE 4 | `SCLIVE4-5.3.0.4-EngineX-OS-Denon-SC-LIVE-4-JP21-Update.img` |
   | 二代 / 2nd | SC LIVE 2 | `SCLIVE2-5.3.0.4-EngineX-OS-Denon-SC-LIVE-2-JP20-Update.img` |
   | 二代 / 2nd | PRIME 4+ | `PRIME4PLUS-5.3.0.4-EngineX-OS-Denon-PRIME-4-Plus-JC11S-Update.img` |
   | 二代 / 2nd | PRIME GO+ | `PRIMEGOPLUS-5.3.0.4-EngineX-OS-Denon-PRIME-GO-Plus-JP11S-Update.img` |
   | 二代 / 2nd | Mixstream Pro+ | `MIXSTREAMPROPLUS-5.3.0.4-EngineX-OS-Numark-Mixstream-Pro-Plus-NH08S-Update.img` |
   | 二代 / 2nd | Mixstream Pro Go | `MIXSTREAMPROGO-5.3.0.4-EngineX-OS-Numark-Mixstream-Pro-Go-NH10-Update.img` |
   | 二代 / 2nd | PRIME 4 G2 | `PRIME4G2-5.3.0.4-EngineX-OS-Denon-PRIME-4-G2-JP24-Update.img` |
   | 二代 / 2nd | Rane SYSTEM ONE | `SYSTEMONE-5.3.0.4-EngineX-OS-Rane-SYSTEM-ONE-RMZ2-Update.img` |

2. 把 U 盘格式化为 **FAT32 或 exFAT**，把 `*-Update.img` **原名**复制到 U 盘**根目录**（不要放进文件夹、不要改名）。
3. （可选）核对 SHA-256：与 Release 页面的 `SHA256SUMS.txt` 比较。
4. U 盘插入碟机。
5. 在碟机上打开 **Settings → About / Update → Firmware Update Mode → Options**。
6. 看到 `Found 5.3.0.4 on USB/SD` 时选择 **USB/SD**，等待校验安装完成并自动重启（已解锁的二代机仅更新 `/data/enginex`，保留全部音乐、曲库与设置）。
7. 首次启动稍慢，属正常。重启后在 `Settings → About / Update` 里能看到 `EngineX OS` 和版本号。

> 如果碟机只问 *Reboot into firmware update mode?*，说明它**没有认到匹配文件**：检查型号前缀是否选对（如 `PRIME4` vs `PRIME4PLUS`）、文件名是否保持原样且在根目录，然后拔出 U 盘再插回。原厂未解锁的二代机必须先用方法一解锁一次。
> 刷机过程中不要断电、不要拔 U 盘。

**English**

1. **Pick the right file.** Download the `*-Update.img` for your exact model from [Releases](https://github.com/Cd1s/EngineX-OS/releases/latest) (see all 16 file names in the table above).
2. Format a USB stick as **FAT32 or exFAT** and copy the `*-Update.img` to its **root folder** with the **original name** (no sub-folder, no renaming).
3. (Optional) Compare the SHA-256 with `SHA256SUMS.txt` on the Release page.
4. Plug the stick into the deck.
5. On the deck open **Settings → About / Update → Firmware Update Mode → Options**.
6. When it shows `Found 5.3.0.4 on USB/SD`, choose **USB/SD** and wait for the verification, install, and automatic reboot (on unlocked 2nd-gen decks, only `/data/enginex` is updated while music, library, and settings are preserved).
7. The first boot is a little slower; that is normal. Afterwards `Settings → About / Update` shows `EngineX OS` and the version.

> If the deck only asks *Reboot into firmware update mode?*, it **did not see a matching file**: check the model prefix (for example `PRIME4` vs `PRIME4PLUS`), keep the original file name in the root folder, then unplug and replug the stick. Stock un-unlocked 2nd-gen units must run Method 1 once first.
> Do not cut power or remove the stick during the flash.

**繁體中文**

1. **選對檔案**。到 [Releases](https://github.com/Cd1s/EngineX-OS/releases/latest) 下載對應機型的 `*-Update.img`（全系 16 款完整檔名見上表）。
2. 把隨身碟格式化為 **FAT32 或 exFAT**，將 `*-Update.img` 以**原檔名**複製到隨身碟**根目錄**（不放資料夾、不改名）。
3. （可選）與 Release 頁面的 `SHA256SUMS.txt` 核對 SHA-256。
4. 將隨身碟插入碟機。
5. 在碟機上開啟 **Settings → About / Update → Firmware Update Mode → Options**。
6. 看到 `Found 5.3.0.4 on USB/SD` 時選擇 **USB/SD**，等待校驗安裝完成並自動重啟（已解鎖的二代機僅更新 `/data/enginex`，保留全部音樂、曲庫與設定）。
7. 首次開機稍慢屬正常。之後在 `Settings → About / Update` 可看到 `EngineX OS` 與版本號。

> 若碟機只詢問 *Reboot into firmware update mode?*，代表它**沒有讀到相符檔案**：請確認機型前綴（如 `PRIME4` 與 `PRIME4PLUS`）、檔名保持原樣且位於根目錄，再拔出隨身碟重新插入。原廠未解鎖的二代機須先以方法一解鎖一次。
> 刷機過程中請勿斷電或拔除隨身碟。

---

<a id="4"></a>

## 4. 方法三：碟机屏幕一键 OTA 在线升级 / Method 3: one-tap on-deck OTA / 方法三：碟機螢幕一鍵 OTA 線上升級

**前提 / Prerequisite / 前提**：已经装好 EngineX OS（任一方法），碟机通过 Wi-Fi 或网线能访问 GitHub。
EngineX OS is already installed (any method) and the deck can reach GitHub over Wi-Fi or Ethernet.
已安裝 EngineX OS（任一方法），且碟機經 Wi-Fi 或網路線可連到 GitHub。

### 4.1 操作 / Steps / 操作

**简体中文**

1. 碟机连接 Wi-Fi 或网线。
2. 打开 **Settings → About / Update**，碟机会检查最新 Release。
3. 发现新版本（例如 `5.3.0.4`）时，点 **Update** 并确认。
4. 等待进度走完。**不要断电。**
5. 完成后在 `Settings → About / Update` 确认版本号已变。

也可以在同一局域网的手机/电脑浏览器打开 `http://<碟机IP>:8080`（访问码显示在碟机 `Settings → About / Update`），在网页控制台里做同样的检查与升级。

**English**

1. Connect the deck to Wi-Fi or Ethernet.
2. Open **Settings → About / Update**; the deck checks the latest Release.
3. When a newer version (for example `5.3.0.4`) is found, tap **Update** and confirm.
4. Wait for the progress to finish. **Do not cut power.**
5. Check the version under `Settings → About / Update`.

You can do the same from the web console at `http://<deck IP>:8080` on the same LAN (the access code is shown on the deck under `Settings → About / Update`).

**繁體中文**

1. 碟機連接 Wi-Fi 或網路線。
2. 開啟 **Settings → About / Update**，碟機會檢查最新 Release。
3. 發現新版本（例如 `5.3.0.4`）時，點 **Update** 並確認。
4. 等待進度跑完，**請勿斷電**。
5. 完成後在 `Settings → About / Update` 確認版本號已更新。

也可在同一區域網路的手機/電腦瀏覽器開啟 `http://<碟機IP>:8080`（存取碼顯示於碟機 `Settings → About / Update`），在網頁主控台執行相同的檢查與升級。

### 4.2 一代机与二代机的区别 / 1st vs 2nd generation / 第一代與第二代的差異

| | 第一代 / 1st / 第一代 | 第二代 / 2nd / 第二代（#215） |
| :-- | :-- | :-- |
| 下载的东西 / Downloads / 下載內容 | `<前缀>-<版本>-EngineX-OS-<机型>-Update.img` | `<前缀>-<版本>-EngineX-OS-<机型>-Update.img`（128 字节 `ENGINEX_DATA_BUNDLE_IMG_V1` SHA-256 头部 + `/data` 升级载荷） |
| 怎么装 / How it installs / 安裝方式 | 进入原厂全屏升级界面，下载、校验、刷入系统分区并重启 / stock full-screen updater downloads, verifies, flashes and reboots / 進入原廠全螢幕升級介面，下載、校驗、刷入並重啟 | 由挂载了 `LD_PRELOAD` 钩子的升级链路校验 128 字节 SHA-256 头部与包内 `SHA256SUMS`，原子更新 `/data/enginex` 并重启进入新版；**不动只读系统分区** / preload-hooked updater verifies the 128-byte SHA-256 header and `SHA256SUMS`, atomically updates `/data/enginex` and restarts; **the read-only system partition is never touched** / 由掛載 `LD_PRELOAD` 掛鉤的升級鏈校驗 128 位元組 SHA-256 標頭與包內 `SHA256SUMS`，原子更新 `/data/enginex` 並重啟；**不動唯讀系統分割區** |
| 谁来识别型号 / Model match / 機型比對 | 读设备树板号，按前缀选文件 / board code from the device tree picks the prefix / 讀取裝置樹板號，依前綴選檔 | 同左；全系 16 款各有精确前缀，二代机绝不会拿到一代的镜像 / same; all 16 models match their exact prefix so a 2nd-gen deck never picks a 1st-gen image / 同左；全系 16 款各有精確前綴，二代機絕不會拿到第一代映像 |

### 4.3 什么时候会提示更新 / When an update is offered / 何時會提示更新

**简体中文**

- 版本号按数字逐段比较：`5.3.0.4` 比 `5.3.0.3` 新；热修复 `5.3.0.4.1` 也比 `5.3.0.4` 新；旧版本永远不会被推送。
- 碟机读不到当前版本（显示 `dev`、没有 `version.txt`）时，**不会**声称有更新。
- Release 里必须有**你机型对应的 `*-Update.img`** 才会提供下载。
- 检查失败时会显示 `GitHub unreachable`（网络问题）或 `no release found`（没有 Release 或仓库私有）；失败后约一分钟重试。

**English**

- Versions are compared number by number: `5.3.0.4` is newer than `5.3.0.3`; a hotfix `5.3.0.4.1` is also newer than `5.3.0.4`; an older release is never offered.
- If the deck cannot read its current version (`dev`, no `version.txt`) it **never** claims an update.
- The Release must contain **the `*-Update.img` for your model** before a download is offered.
- A failed check shows `GitHub unreachable` (network) or `no release found` (no release, or a private repo); it retries after about a minute.

**繁體中文**

- 版本號逐段以數字比較：`5.3.0.4` 比 `5.3.0.3` 新；熱修復 `5.3.0.4.1` 也比 `5.3.0.4` 新；舊版本不會被推送。
- 碟機讀不到目前版本（顯示 `dev`、沒有 `version.txt`）時，**不會**宣稱有更新。
- Release 中必須有**對應你機型的 `*-Update.img`** 才會提供下載。
- 檢查失敗會顯示 `GitHub unreachable`（網路問題）或 `no release found`（沒有 Release 或儲存庫為私有）；失敗後約一分鐘重試。

> **第二代提醒 / 2nd-gen note / 第二代提醒**：源码里记着一条已知的未验证点——服务重启这一步在真机上尚未确认。如果升级完成后一分钟左右屏幕没有切到新版，请手动重启碟机一次。
> The source notes one unconfirmed point: the final service restart has not been confirmed on a real deck. If the screen does not switch to the new version about a minute after completion, reboot the deck once.
> 原始碼註記一個尚未驗證的點：最後的服務重啟步驟尚未在實機確認。若完成後約一分鐘畫面仍未切換到新版，請手動重啟碟機一次。

---

<a id="5"></a>

## 5. 常见报错与急救 / Troubleshooting / 常見錯誤與急救

### 5.1 安装器启动问题 / Installer won't start / 安裝器無法啟動

**macOS**

- 中：双击 `Install-EngineX.command` 被 Gatekeeper 拦截（"无法验证开发者"）时：在 Finder 里**右键 → 打开**，再点"打开"；或在终端执行 `xattr -dr com.apple.quarantine <解压目录>` 后重试。
- EN: If Gatekeeper blocks `Install-EngineX.command`, **right-click → Open** in Finder and confirm, or run `xattr -dr com.apple.quarantine <extracted folder>` and retry.
- 繁：若 Gatekeeper 攔截 `Install-EngineX.command`，在 Finder 中**右鍵 → 開啟**並確認，或在終端機執行 `xattr -dr com.apple.quarantine <解壓縮資料夾>` 後重試。
- 若提示没有执行权限 / if it is not executable / 若提示無執行權限：`chmod +x Install-EngineX.command`

**Windows**

- 中：SmartScreen 提示时点"更多信息 → 仍要运行"。杀毒软件拦截请把解压目录加入白名单。
- EN: On a SmartScreen warning choose "More info → Run anyway". If antivirus blocks it, whitelist the extracted folder.
- 繁：遇到 SmartScreen 警告時點「更多資訊 → 仍要執行」；若被防毒攔截，請將解壓縮資料夾加入白名單。

**Linux**：`chmod +x install-enginex.sh`，必要时用 `sudo` 以便访问 USB / use `sudo` if USB access is needed / 必要時用 `sudo` 以便存取 USB。

### 5.2 Windows 的 Fastboot 驱动 / Fastboot driver on Windows / Windows 的 Fastboot 驅動

- 中：安装器的 Fastboot 路径在 Windows 上要求在 **WSL** 里用 Linux 包，并让 WSL 能访问 USB（例如 usbipd-win）。若 `fastboot devices` 看不到碟机，可用 Zadig 为该 USB 设备安装 **WinUSB** 驱动后再试。驱动与 WSL 的 USB 直通属于通用做法，**本仓库没有逐机型验证**；先用 `fastboot devices` 单独确认能看到设备，再启动安装器。
- EN: On Windows the Fastboot path expects the Linux package inside **WSL** with USB passthrough (for example usbipd-win). If `fastboot devices` shows nothing, install the **WinUSB** driver for that USB device with Zadig and retry. Driver and WSL passthrough steps are generic practice and **not verified per model in this repository**; first confirm `fastboot devices` lists the deck, then start the installer.
- 繁：Windows 上 Fastboot 路徑需在 **WSL** 中使用 Linux 版，並讓 WSL 可存取 USB（例如 usbipd-win）。若 `fastboot devices` 看不到碟機，可用 Zadig 為該 USB 裝置安裝 **WinUSB** 驅動後重試。驅動與 WSL USB 直通屬通用做法，**本專案未逐機型驗證**；請先確認 `fastboot devices` 能列出碟機，再啟動安裝器。

### 5.3 安装器报错对照 / Installer messages / 安裝器訊息對照

| 提示 / Message / 訊息 | 原因与处理 / Cause & fix / 原因與處理 |
| :-- | :-- |
| `没有发现碟机` / `no deck found` | 碟机没开机、不在同一 /24 网段，或是未解锁原厂机。开机并接同一路由器；或进入 Firmware Update Mode 后接 USB（模式 B）。/ Power on, join the same router, or boot into Firmware Update Mode and use USB (Mode B). / 開機並連到同一路由器，或進入 Firmware Update Mode 後接 USB（模式 B）。 |
| `SSH 登录失败 / SSH login failed` | SSH 可达但认证失败。用 `--key` 或 `--password`。/ Use `--key` or `--password`. / 改用 `--key` 或 `--password`。 |
| `处于原厂未解锁状态，需要 fastboot 但没找到` | PATH 里没有 `fastboot`。安装 Android platform-tools。/ Install Android platform-tools. / 安裝 Android platform-tools。 |
| `等待 USB Fastboot 超时` / `timed out waiting for fastboot` | 10 分钟内没看到设备。换 USB 线/端口，确认碟机在 Firmware Update Mode。/ Change cable/port, confirm Firmware Update Mode. / 更換 USB 線/連接埠，確認碟機處於 Firmware Update Mode。 |
| `未知型号 … 不刷写` / `unknown board, nothing flashed` | 板号不在支持表里，安装器拒绝刷写，属保护行为。/ Protective refusal; the board is not supported. / 板號不在支援表中，安裝器拒絕刷寫，屬保護行為。 |
| `wrong model refused` | `--image` / `--bundle` 的文件名前缀与碟机型号不符。换对应型号的 `*-Update.img`。/ The `--image` / `--bundle` prefix does not match the deck. / `--image` / `--bundle` 檔名前綴與機型不符。 |
| `SHA256SUMS mismatch … damaged` / `download SHA-256 mismatch` | 下载损坏或被篡改。删除文件重下，不要强行安装。/ Delete and re-download; do not force it. / 刪除後重新下載，勿強行安裝。 |
| `读不到 data 分区大小` | 无法确定新镜像大小。用 `--userdata-backup` 提供备份镜像。/ Pass `--userdata-backup`. / 以 `--userdata-backup` 提供備份映像。 |
| `需要 python3 和 e2fsprogs` | 缺 Python 3 / `debugfs` / `mke2fs`。安装后重试（Windows 用 WSL）。/ Install them (WSL on Windows). / 安裝後重試（Windows 用 WSL）。 |
| `installed version reads back as …` | 安装后读回的版本与包不符，安装器不会继续激活。保留输出并重试一次。/ The installer does not activate; retry once and keep the output. / 安裝器不會繼續啟用；保留輸出並重試一次。 |

### 5.4 刷机后保留用户配置 / Keeping your settings / 刷機後保留使用者設定

**简体中文**

- **第二代（已解锁 U 盘刷 `*-Update.img`、屏幕 OTA、模式 A）**：只更新 `/data/enginex` 和一个 ALSA 配置文件，音乐、曲库、设置完整保留。
- **第二代（原厂首次模式 B / 模式 C）**：整个 `/data` 分区会被替换。模式 B 推荐用 `--userdata-backup` 指向你自己这台碟机的 `/data` 完整备份；不加则会写入全新的空 `/data` 分区，请务必先备份音乐与曲库。
- **第一代（U 盘 / OTA）**：刷的是系统镜像。仍请先自行备份重要的音乐与曲库（USB/SD 上的内容不受影响）。
- 刷机前请先拔掉 U 盘和 SD 卡，避免误操作。

**English**

- **2nd gen (unlocked USB-stick `*-Update.img`, on-screen OTA, Mode A)**: only `/data/enginex` and one ALSA config file are written; music, library and settings stay intact.
- **2nd gen (first-time stock Mode B / Mode C)**: the whole `/data` partition is replaced. In Mode B, pass `--userdata-backup` pointing at a full backup of this deck's own `/data` if available; otherwise a fresh `/data` image is written and internal `/data` storage is erased, so back up first.
- **1st gen (USB stick / OTA)**: you flash the system image. Still back up important music and library first (content on USB/SD is unaffected).
- Unplug USB sticks and SD cards before USB cable flashing.

**繁體中文**

- **第二代（已解鎖隨身碟刷 `*-Update.img`、螢幕 OTA、模式 A）**：只更新 `/data/enginex` 與一個 ALSA 設定檔，音樂、曲庫、設定完整保留。
- **第二代（原廠首次模式 B / 模式 C）**：整個 `/data` 分割區會被取代。模式 B 建議以 `--userdata-backup` 指向這台碟機自己的 `/data` 完整備份；不加則會寫入全新的空 `/data` 分割區，請務必先備份音樂與曲庫。
- **第一代（USB 隨身碟 / OTA）**：刷入的是系統映像。仍請先備份重要音樂與曲庫（USB/SD 上的內容不受影響）。
- 線刷前請先拔除 USB 隨身碟與 SD 卡。

### 5.5 回退原厂 / Back to stock / 回退原廠

**第二代 / 2nd gen / 第二代**

二代机系统分区（rootfs）为只读且从未被修改，恢复原厂启动只需移除数据包与 ALSA 引导钩子（保留用户曲库、设置与 SSH 密钥）：
- **Web 控制台一键回退**：浏览器打开碟机 IP，在 Web 控制台点击「Revert to Stock」（或发送 `POST /v1/sc5000/uninstall-gen2`）。
- **安装器网络卸载**：电脑端运行 `EngineX-Installer --uninstall`（需碟机在同一局域网并可访问 SSH）。
- **本地 shell 脚本**：
```sh
# 在碟机的 root shell 里 / in the deck's root shell / 在碟機的 root shell 中
sh /data/enginex/bundle/uninstall.sh && reboot
```

- 中：只移除数据包新增的内容，系统分区本来就没被改过。
- EN: It removes only what the bundle added; the system partition was never modified. You can also revert in one click via Web Console ("Revert to Stock"), `POST /v1/sc5000/uninstall-gen2`, or `EngineX-Installer --uninstall` over the network (SSH).
- 繁：只移除資料包新增的內容，系統分割區本來就未被改動。亦可透過 Web 控制台（「Revert to Stock」）、`POST /v1/sc5000/uninstall-gen2` 或 `EngineX-Installer --uninstall`（經網路 SSH）一鍵還原。
- 无法连接网络或 shell 时：经 Fastboot（AZ04 机型经 1cc5:2027 usbupdate）刷回你自己的原始 `/data` 分区备份。/ With no shell or network access: flash back your original `/data` backup via Fastboot (or 1cc5:2027 usbupdate on AZ04). / 無法連線網路或 shell 時：經 Fastboot（AZ04 機型經 1cc5:2027 usbupdate）刷回自己的原始 `/data` 分割區備份。

**第一代 / 1st gen / 第一代**

- 中：从 Denon DJ / inMusic 官方渠道下载你机型的原厂 Engine OS 固件，用与方法二相同的步骤（U 盘根目录 + Firmware Update Mode）刷回。本仓库不分发原厂固件。
- EN: Download the stock Engine OS firmware for your model from the official Denon DJ / inMusic channel and flash it with the same steps as Method 2 (USB root + Firmware Update Mode). This repository does not distribute stock firmware.
- 繁：自 Denon DJ / inMusic 官方管道下載對應機型的原廠 Engine OS 韌體，以方法二相同步驟（隨身碟根目錄 + Firmware Update Mode）刷回。本專案不散布原廠韌體。

### 5.6 刷机后进不了系统 / Deck won't boot properly / 刷機後無法正常開機

- 中：第一代：重新进入 Firmware Update Mode，再刷一次 `.img`，或刷回原厂固件。第二代：先试 `uninstall.sh`；连 shell 都进不去时，用模式 B 刷回原始 `/data` 备份。
- EN: 1st gen: re-enter Firmware Update Mode and flash the `.img` again, or flash stock. 2nd gen: try `uninstall.sh` first; if you have no shell, flash back the original `/data` backup in Mode B.
- 繁：第一代：重新進入 Firmware Update Mode 再刷一次 `.img`，或刷回原廠韌體。第二代：先試 `uninstall.sh`；連 shell 都進不去時，以模式 B 刷回原始 `/data` 備份。

> **绝对不要 / Never / 絕對不要**：在真机上触发 Factory Reset，或运行 `az01-update --erase-data`。
> Never trigger Factory Reset or `az01-update --erase-data` on a real deck.
> 不要在實機上觸發 Factory Reset，或執行 `az01-update --erase-data`。

---

*相关 / Related / 相關*：[`README.md`](../../README.md) · [`README.zh-CN.md`](../../README.zh-CN.md) · [`README.zh-TW.md`](../../README.zh-TW.md) · `cmd/enginex-installer/main.go` · `pkg/webui/update.go`
