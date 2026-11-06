# 02 · 安装、刷机与 OTA 在线升级 / Install, Flash & OTA Update / 安裝、刷機與 OTA 線上升級

> 简体中文 · English · 繁體中文，每一节按这个顺序对照。
> Every section is given in this order: 简体中文 · English · 繁體中文.
>
> 本文以 **EngineX OS 5.3.0.2** 为例；文件名里的版本号会随新版本变化，始终以 [Releases](https://github.com/Cd1s/EngineX-OS/releases/latest) 页面为准。
> Examples use **EngineX OS 5.3.0.2**; the version in file names changes with each release, so trust the [Releases](https://github.com/Cd1s/EngineX-OS/releases/latest) page.
> 本文以 **EngineX OS 5.3.0.2** 為例；檔名中的版本號會隨新版本變動，一律以 [Releases](https://github.com/Cd1s/EngineX-OS/releases/latest) 頁面為準。

## 目录 / Contents / 目錄

1. [先看自己的机器属于哪一类 / Which group is your deck / 先確認機型類別](#1)
2. [方法一：EngineX-Installer 一键安装（推荐）/ Method 1 / 方法一](#2)
3. [方法二：一代机 U 盘离线刷 `.img` / Method 2 / 方法二](#3)
4. [方法三：碟机屏幕一键 OTA 升级 / Method 3 / 方法三](#4)
5. [常见报错与急救 / Troubleshooting / 常見錯誤與急救](#5)

---

<a id="1"></a>

## 1. 先看自己的机器属于哪一类 / Which group is your deck / 先確認機型類別

**简体中文**：EngineX OS 一共支持 16 款机型，分两代，**安装方式不同，不能混用**。

**English**: EngineX OS supports 16 units in two generations. **The install methods differ and must not be mixed.**

**繁體中文**：EngineX OS 共支援 16 款機型，分兩代，**安裝方式不同，不可混用**。

| 代 / Gen | 机型 / Models / 機型 | 板号 / Board | 安装物 / What you install / 安裝檔 | 可用方法 / Methods / 可用方法 |
| :-- | :-- | :-- | :-- | :-- |
| 第一代 / 1st / 第一代 | SC5000 · SC5000M · SC6000 · SC6000M · PRIME 4 · PRIME 2 · PRIME GO · Mixstream Pro | JP07 · JP08 · JP13 · JP14 · JC11 · JC16 · JP11 · NH08 | `*-Update.img` 固件 / firmware / 韌體 | 方法二（U 盘 / USB stick / USB 隨身碟）、方法一（安装器 / installer / 安裝器） |
| 第二代 / 2nd / 第二代 | SC LIVE 4 · SC LIVE 2 · PRIME 4+ · PRIME GO+ · Mixstream Pro+ · Mixstream Pro Go · PRIME 4 G2 · Rane SYSTEM ONE | JP21 · JP20 · JC11S · JP11S · NH08S · NH10 · JP24 · RMZ2 | `*-Data-Bundle.tar.gz` 数据包 / data bundle / 資料包 | 方法一（只能用安装器 / installer only / 僅能用安裝器） |

> 第二代的 PRIME 4+ / PRIME GO+ / Mixstream Pro+ 与第一代的 PRIME 4 / PRIME GO / Mixstream Pro 名字很像，但**文件不能互刷**。
> The "+" models look like the first-generation ones but their files are **not interchangeable**.
> 第二代的「+」機型與第一代名稱相近，但**檔案不可互刷**。

**简体中文**

- **第一代**：固件没有签名保护，可以直接刷 `.img`（U 盘或安装器均可）。
- **第二代**：运行**签名固件**（AZ0x 容器，密钥记录为 `denon-1` / `rane-1`），系统分区只读，不能刷第三方 `.img`。EngineX OS 改为把一个**数据包**装进可写的 `/data` 分区（`/data/enginex`），开机时由一个启动钩子在原厂 Engine 上加载 EngineX 的 UI 包、服务和 hook。原厂系统分区不被改写，卸载可还原（`uninstall.sh`）。
- **第二代的限制（请知悉）**：效果**并不与第一代完全相同**。例如 PRIME 4 G2 / SYSTEM ONE（64 位 Engine）目前只带 UI 包、流媒体网关与 EngineX 服务；DSP guard、Master Soft Clipper、自动增益、SQLite 调优和 4-deck 解锁尚未移植，Stems 保持原厂行为。第二代整体仍是 **Beta**，README 注明尚未在真机上完整验证。
- **绝对不要**把 `PRIME4` / `PRIMEGO` / `MIXSTREAMPRO` 的 `.img` 刷到第二代机器上。

**English**

- **1st generation**: the firmware is not signature-locked, so a plain `.img` works (USB stick or the installer).
- **2nd generation**: runs **signed firmware** (AZ0x container, key record `denon-1` / `rane-1`) with a read-only system partition, so a third-party `.img` cannot be flashed. EngineX OS instead installs a **data bundle** into the writable `/data` partition (`/data/enginex`); at boot a hook loads EngineX's UI packs, services and hook library on top of the stock Engine. The stock system partition is never rewritten, and `uninstall.sh` removes the bundle.
- **Limits of the 2nd generation (be aware)**: the result is **not identical** to the 1st generation. For example PRIME 4 G2 / SYSTEM ONE (64-bit Engine) currently get the UI packs, the streaming gateway and EngineX services only; DSP guards, Master Soft Clipper, auto gain, SQLite tuning and the 4-deck unlock are not ported yet, and Stems stay as stock. The whole 2nd-generation path is still **Beta**; the README says it has not been fully tested on real hardware.
- **Never** flash a `PRIME4` / `PRIMEGO` / `MIXSTREAMPRO` `.img` onto a 2nd-generation unit.

**繁體中文**

- **第一代**：韌體沒有簽章保護，可直接刷 `.img`（USB 隨身碟或安裝器皆可）。
- **第二代**：執行**簽章韌體**（AZ0x 容器，金鑰記錄為 `denon-1` / `rane-1`），系統分割區唯讀，無法刷第三方 `.img`。EngineX OS 改為把**資料包**裝進可寫入的 `/data` 分割區（`/data/enginex`），開機時由啟動 hook 在原廠 Engine 上載入 EngineX 的 UI 包、服務與 hook。原廠系統分割區不會被改寫，可用 `uninstall.sh` 移除。
- **第二代的限制（請知悉）**：效果**並不與第一代完全相同**。例如 PRIME 4 G2 / SYSTEM ONE（64 位元 Engine）目前只含 UI 包、串流閘道與 EngineX 服務；DSP guard、Master Soft Clipper、自動增益、SQLite 調校與 4-deck 解鎖尚未移植，Stems 維持原廠行為。第二代整體仍為 **Beta**，README 註明尚未在實機完整驗證。
- **絕對不要**把 `PRIME4` / `PRIMEGO` / `MIXSTREAMPRO` 的 `.img` 刷到第二代機器上。

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
| Windows 10/11 x64 | `EngineX-Installer-5.3.0.2-windows-amd64.zip` | 双击 `Install-EngineX.bat` |
| macOS Apple Silicon (M1–M4) | `EngineX-Installer-5.3.0.2-macos-arm64.zip` | 双击 `Install-EngineX.command` |
| macOS Intel | `EngineX-Installer-5.3.0.2-macos-amd64.zip` | 双击 `Install-EngineX.command` |
| Linux x86_64 | `EngineX-Installer-5.3.0.2-linux-amd64.tar.gz` | `./install-enginex.sh` |

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

**简体中文**：启动后，如果电脑已经通过 USB 看到处于 Fastboot 的碟机，直接走**模式 B**；否则在局域网里找碟机，走**模式 A**。

**English**: At start, if a deck in Fastboot is already visible over USB, it goes straight to **Mode B**; otherwise it looks for decks on the LAN and uses **Mode A**.

**繁體中文**：啟動後，若電腦已透過 USB 看到處於 Fastboot 的碟機，直接走**模式 B**；否則在區域網路中找碟機，走**模式 A**。

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
4. **第二代**：下载该型号的 `Data-Bundle.tar.gz`（核对 GitHub 提供的 SHA-256 与包内 `SHA256SUMS`），通过 SSH 流式传到碟机并运行 `install.sh`，读回 `version.txt` 核对版本，再运行 `boot-activate.sh`，Engine 重启后即是 EngineX OS。**音乐、曲库、设置保留**。
5. **第一代**：下载 `.img`（必须有 GitHub SHA-256），要求你输入 `yes` 确认，传到碟机 `/data/az01-update.img`，校验后调用 `az01-update --file` 刷入。

**English: what it does**

1. **Find the deck** (no IP lookup): it listens for **StageLinQ (UDP 51337)** for about 1.5 s (decks announce their board code as the software name) and in parallel scans the computer's **/24 subnet** for the EngineX Web UI at `http://<IP>:8080/v1/sc5000/ui`. Other StageLinQ devices are skipped.
2. It logs in over SSH as `root`: keys `~/.ssh/id_ed25519`, `id_rsa`, `id_ecdsa`, then passwords (`--password`, and `denon` for FreeMyPrime-unlocked decks).
3. It reads `/sys/firmware/devicetree/base/compatible` to get the board and generation.
4. **2nd generation**: downloads the model's `Data-Bundle.tar.gz` (checks GitHub's SHA-256 and the bundle's own `SHA256SUMS`), streams it over SSH, runs `install.sh`, reads `version.txt` back, then runs `boot-activate.sh`; Engine restarts as EngineX OS. **Music, library and settings stay.**
5. **1st generation**: downloads the `.img` (a GitHub SHA-256 is required), asks you to type `yes`, uploads it to `/data/az01-update.img`, verifies it and calls `az01-update --file`.

**繁體中文：它做了什麼**

1. **找碟機**（不必查 IP）：監聽 **StageLinQ（UDP 51337）**約 1.5 秒，碟機會以板號作為軟體名稱廣播；同時掃描電腦所在 **/24 網段**，尋找回應 `http://<IP>:8080/v1/sc5000/ui` 的 EngineX Web UI。其他 StageLinQ 裝置會被略過。
2. 以 `root` 透過 SSH 登入：依序嘗試 `~/.ssh/id_ed25519`、`id_rsa`、`id_ecdsa`，再試密碼（`--password` 指定者，以及用於 FreeMyPrime 已解鎖機器的 `denon`）。
3. 讀取 `/sys/firmware/devicetree/base/compatible` 取得板號，判斷機型與世代。
4. **第二代**：下載該機型的 `Data-Bundle.tar.gz`（核對 GitHub 提供的 SHA-256 與包內 `SHA256SUMS`），經 SSH 串流傳到碟機並執行 `install.sh`，讀回 `version.txt` 核對版本，再執行 `boot-activate.sh`；Engine 重啟後即為 EngineX OS。**音樂、曲庫、設定保留。**
5. **第一代**：下載 `.img`（必須有 GitHub SHA-256），要求輸入 `yes` 確認，傳到碟機 `/data/az01-update.img`，校驗後呼叫 `az01-update --file` 刷入。

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

**简体中文：模式 B 做了什么（第二代）**

1. `fastboot getvar product` 读出板号，对照型号表（如 `jp21` → SC LIVE 4），**未知板号一律不刷**。
2. 下载并校验对应型号的数据包。
3. 查询分区大小，确定刷哪个分区：优先 `data`，否则 `userdata`。
4. 准备 `/data` 镜像并用 `host-install-userdata.py`（经 `debugfs`）把数据包和 ALSA 启动钩子写进去：
   - **推荐**：加 `--userdata-backup 你自己这台碟机的data备份.img`，在备份的**副本**上注入，**保留你的音乐和设置**；
   - 不加该参数：程序会新建一个**全新的空 `/data` 镜像**，刷入后**碟机上的音乐、曲库、设置全部清空**。此时程序会特别警告并要求你输入 `yes`。
5. `fastboot flash <分区> 镜像`，然后 `fastboot reboot`。
6. 首次开机先以原厂状态启动一次，随后 Engine 会自动重启并载入 EngineX OS。

> 模式 B 不会开启 SSH。公开版没有内置 SSH 密钥，如要用 SSH 需自行加入公钥（见 README 的 Security 一节）。

**English: what Mode B does (2nd generation)**

1. `fastboot getvar product` reads the board code and maps it to a model (for example `jp21` → SC LIVE 4). **An unknown board is never flashed.**
2. It downloads and verifies the model's bundle.
3. It reads the partition size and picks the target: `data` if it reports a size, else `userdata`.
4. It prepares the `/data` image and injects the bundle and the ALSA boot hook with `host-install-userdata.py` (through `debugfs`):
   - **Recommended**: pass `--userdata-backup <a backup image of this deck's own data>`; it injects into a **copy** and **keeps your music and settings**;
   - without it the installer makes a **brand-new empty `/data` image**, and **all music, library and settings on the deck are erased**. It warns loudly and asks you to type `yes`.
5. `fastboot flash <partition> <image>`, then `fastboot reboot`.
6. The first boot runs stock Engine once, then Engine restarts by itself with EngineX OS.

> Mode B does not enable SSH. The public build ships no SSH key; to use SSH you add your own public key (see the Security section of the README).

**繁體中文：模式 B 做了什麼（第二代）**

1. `fastboot getvar product` 讀出板號並對照機型表（如 `jp21` → SC LIVE 4），**未知板號一律不刷**。
2. 下載並校驗對應機型的資料包。
3. 查詢分割區大小，決定刷哪個分割區：有回報大小用 `data`，否則用 `userdata`。
4. 準備 `/data` 映像，並用 `host-install-userdata.py`（經 `debugfs`）寫入資料包與 ALSA 啟動 hook：
   - **建議**：加上 `--userdata-backup <這台碟機自己的 data 備份映像>`，在備份的**副本**上注入，**保留音樂與設定**；
   - 不加此參數：程式會建立**全新的空 `/data` 映像**，刷入後**碟機上的音樂、曲庫、設定全部清空**；程式會明確警告並要求輸入 `yes`。
5. `fastboot flash <分割區> <映像>`，然後 `fastboot reboot`。
6. 首次開機先以原廠狀態啟動一次，之後 Engine 會自動重啟並載入 EngineX OS。

> 模式 B 不會開啟 SSH。公開版不內建 SSH 金鑰；如需使用 SSH，請自行加入公鑰（見 README 的 Security 一節）。

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
| Windows | 请在 **WSL** 中用 Linux 包，并让 WSL 能访问 USB / use the Linux package in **WSL** with USB access / 請在 **WSL** 中使用 Linux 版，並讓 WSL 可存取 USB |

### 2.7 常用参数 / Useful flags / 常用參數

| 参数 / Flag / 參數 | 作用 / Effect / 作用 |
| :-- | :-- |
| `--yes` | 不再询问确认（慎用）/ skip confirmations (use with care) / 不再詢問確認（慎用） |
| `--bundle <文件>` | 使用本地数据包，文件名前缀必须与型号一致，否则拒绝 / use a local bundle; a wrong-model name is refused / 使用本機資料包，檔名前綴須與機型相符，否則拒絕 |
| `--userdata-backup <文件>` | 模式 B：在你自己的 `/data` 备份上注入，保留数据 / Mode B: inject into your own `/data` backup / 模式 B：在自己的 `/data` 備份上注入，保留資料 |
| `--key <私钥>` | 模式 A：指定 SSH 私钥 / SSH private key / 指定 SSH 私鑰 |
| `--password <密码>` | 模式 A：SSH root 密码 / SSH root password / SSH root 密碼 |

---

<a id="3"></a>

## 3. 方法二：一代机 U 盘离线刷入 `.img` / Method 2: 1st-gen offline USB flash / 方法二：第一代 USB 隨身碟離線刷入

**仅限第一代 8 款。/ 1st generation only. / 僅限第一代 8 款。** 原厂固件 4.3.x 与 5.0.x 均可直接刷入。/ Stock 4.3.x and 5.0.x both work. / 原廠 4.3.x 與 5.0.x 皆可直接刷入。

> ⚠️ **为什么电脑刷机工具看不到 PRIME GO / SC5000？（`#104`） / Why can't the computer tool see my PRIME GO / SC5000? / 為什麼電腦刷機工具看不到 PRIME GO / SC5000？**
>
> - **推荐：免电脑 U 盘直接刷。/ Recommended: USB stick, no computer. / 推薦：免電腦隨身碟直接刷。** 一代机（SC5000 / SC6000 / PRIME GO / PRIME 4 / PRIME 2）无需连接电脑，把 `*-Update.img` 放进 FAT32 / exFAT U 盘根目录插到碟机，进入 `Settings → Update → Reboot` 即可更新或恢复固件。/ First-generation units need no computer: put the `*-Update.img` in the root of a FAT32 / exFAT stick, plug it in and go to `Settings → Update → Reboot` to update or recover. / 一代機無需連接電腦，把 `*-Update.img` 放進 FAT32 / exFAT 隨身碟根目錄插到碟機，進入 `Settings → Update → Reboot` 即可更新或還原韌體。
> - **电脑 USB 线刷模式 / Computer USB flashing mode / 電腦 USB 線刷模式：** 正常开机打碟时，电脑**绝对看不到**刷机设备！必须先在碟机屏幕点 **`Settings → Update → Reboot to Computer Update Mode`**（重启进入蓝屏电脑图标的 Fastboot 模式），再用 USB-B 线连电脑运行刷机工具。/ While the deck runs normally the computer **never** sees a flashing device. First tap **`Settings → Update → Reboot to Computer Update Mode`** on the deck (it reboots into Fastboot mode with the blue-screen computer icon), then connect a USB-B cable and run the tool. / 正常開機打碟時，電腦**絕對看不到**刷機裝置！必須先在碟機螢幕點 **`Settings → Update → Reboot to Computer Update Mode`**（重啟進入藍屏電腦圖示的 Fastboot 模式），再以 USB-B 線連接電腦執行刷機工具。

**简体中文**

1. **选对文件**。到 [Releases](https://github.com/Cd1s/EngineX-OS/releases/latest) 下载你机型对应的 `.img`：

   | 机型 | 文件名前缀 |
   | :-- | :-- |
   | SC5000 | `SC5000-…-JP07-Update.img` |
   | SC5000M | `SC5000M-…-JP08-Update.img` |
   | SC6000 | `SC6000-…-JP13-Update.img` |
   | SC6000M | `SC6000M-…-JP14-Update.img` |
   | PRIME 4 | `PRIME4-…-JC11-Update.img` |
   | PRIME 2 | `PRIME2-…-JC16-Update.img` |
   | PRIME GO | `PRIMEGO-…-JP11-Update.img` |
   | Mixstream Pro | `MIXSTREAMPRO-…-NH08-Update.img` |

2. 把 U 盘格式化为 **FAT32 或 exFAT**，把 `.img` **原名**复制到 U 盘**根目录**（不要放进文件夹、不要改名）。
3. （可选）核对 SHA-256：与 Release 页面的 `SHA256SUMS.txt` 比较。
4. U 盘插入碟机。
5. 在碟机上打开 **Settings → About / Update → Firmware Update Mode**。
6. 碟机重启后若显示 `Found 5.3.0.2 on USB/SD`，选择 **USB/SD**，等待安装完成并自动重启。
7. 首次启动稍慢，属正常。重启后在 `Settings → About` 里能看到 `EngineX OS` 和版本号。

> 如果碟机只问 *Reboot into firmware update mode?*，说明它**没有认到文件**：检查型号是否选对、文件名是否保持原样且在根目录，然后拔出 U 盘再插回。
> 刷机过程中不要断电、不要拔 U 盘。

**English**

1. **Pick the right file.** Download the `.img` for your model from [Releases](https://github.com/Cd1s/EngineX-OS/releases/latest) (see the table above for the file-name prefixes).
2. Format a USB stick as **FAT32 or exFAT** and copy the `.img` to its **root folder** with the **original name** (no sub-folder, no renaming).
3. (Optional) Compare the SHA-256 with `SHA256SUMS.txt` on the Release page.
4. Plug the stick into the deck.
5. On the deck open **Settings → About / Update → Firmware Update Mode**.
6. After the reboot, when it shows `Found 5.3.0.2 on USB/SD`, choose **USB/SD** and wait for the install and automatic reboot.
7. The first boot is a little slower; that is normal. Afterwards `Settings → About` shows `EngineX OS` and the version.

> If the deck only asks *Reboot into firmware update mode?*, it **did not see the file**: check the model, keep the original file name in the root folder, then unplug and replug the stick.
> Do not cut power or remove the stick during the flash.

**繁體中文**

1. **選對檔案**。到 [Releases](https://github.com/Cd1s/EngineX-OS/releases/latest) 下載對應機型的 `.img`（檔名前綴見上表）。
2. 把隨身碟格式化為 **FAT32 或 exFAT**，將 `.img` 以**原檔名**複製到隨身碟**根目錄**（不放資料夾、不改名）。
3. （可選）與 Release 頁面的 `SHA256SUMS.txt` 核對 SHA-256。
4. 將隨身碟插入碟機。
5. 在碟機上開啟 **Settings → About / Update → Firmware Update Mode**。
6. 重啟後若顯示 `Found 5.3.0.2 on USB/SD`，選擇 **USB/SD**，等待安裝完成並自動重啟。
7. 首次開機稍慢屬正常。之後在 `Settings → About` 可看到 `EngineX OS` 與版本號。

> 若碟機只詢問 *Reboot into firmware update mode?*，代表它**沒有讀到檔案**：請確認機型、檔名保持原樣且位於根目錄，再拔出隨身碟重新插入。
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
3. 发现新版本（例如 `5.3.0.3`）时，点 **Update** 并确认。
4. 等待进度走完。**不要断电。**
5. 完成后在 `Settings → About` 确认版本号已变。

也可以在同一局域网的手机/电脑浏览器打开 `http://<碟机IP>:8080`（访问码显示在碟机 `Settings → About`），在网页控制台里做同样的检查与升级。

**English**

1. Connect the deck to Wi-Fi or Ethernet.
2. Open **Settings → About / Update**; the deck checks the latest Release.
3. When a newer version (for example `5.3.0.3`) is found, tap **Update** and confirm.
4. Wait for the progress to finish. **Do not cut power.**
5. Check the version under `Settings → About`.

You can do the same from the web console at `http://<deck IP>:8080` on the same LAN (the access code is shown on the deck under `Settings → About`).

**繁體中文**

1. 碟機連接 Wi-Fi 或網路線。
2. 開啟 **Settings → About / Update**，碟機會檢查最新 Release。
3. 發現新版本（例如 `5.3.0.3`）時，點 **Update** 並確認。
4. 等待進度跑完，**請勿斷電**。
5. 完成後在 `Settings → About` 確認版本號已更新。

也可在同一區域網路的手機/電腦瀏覽器開啟 `http://<碟機IP>:8080`（存取碼顯示於碟機 `Settings → About`），在網頁主控台執行相同的檢查與升級。

### 4.2 一代机与二代机的区别 / 1st vs 2nd generation / 第一代與第二代的差異

| | 第一代 / 1st / 第一代 | 第二代 / 2nd / 第二代（#86） |
| :-- | :-- | :-- |
| 下载的东西 / Downloads / 下載內容 | `<前缀>-<版本>-…-Update.img` | `<前缀>-<版本>-EngineX-OS-Data-Bundle.tar.gz`（≤ 64 MB） |
| 怎么装 / How it installs / 安裝方式 | 进入原厂全屏升级界面，下载、校验、刷入并重启 / stock full-screen updater downloads, verifies, flashes and reboots / 進入原廠全螢幕升級介面，下載、校驗、刷入並重啟 | 解包到临时目录，`install.sh` 校验 `SHA256SUMS` 后一次性替换，再重启 EngineX 服务；**不动系统分区** / unpacked to a scratch dir, `install.sh` checks `SHA256SUMS` and swaps files in one step, then EngineX services restart; **the system partition is not touched** / 解開至暫存目錄，`install.sh` 校驗 `SHA256SUMS` 後一次替換，再重啟 EngineX 服務；**不動系統分割區** |
| 谁来识别型号 / Model match / 機型比對 | 读设备树板号，按前缀选文件 / board code from the device tree picks the prefix / 讀取裝置樹板號，依前綴選檔 | 同左；第二代有独立前缀，不会拿到一代的镜像 / same; 2nd-gen has its own prefixes so it never gets a 1st-gen image / 同左；第二代有獨立前綴，不會拿到第一代映像 |

### 4.3 什么时候会提示更新 / When an update is offered / 何時會提示更新

**简体中文**

- 版本号按数字逐段比较：`5.3.0.3` 比 `5.3.0.2` 新；热修复 `5.3.0.2.1` 也比 `5.3.0.2` 新；旧版本永远不会被推送。
- 碟机读不到当前版本（显示 `dev`、没有 `version.txt`）时，**不会**声称有更新。
- Release 里必须有**你机型对应的文件**才会提供下载。
- 检查失败时会显示 `GitHub unreachable`（网络问题）或 `no release found`（没有 Release 或仓库私有）；失败后约一分钟重试。

**English**

- Versions are compared number by number: `5.3.0.3` is newer than `5.3.0.2`; a hotfix `5.3.0.2.1` is also newer than `5.3.0.2`; an older release is never offered.
- If the deck cannot read its current version (`dev`, no `version.txt`) it **never** claims an update.
- The Release must contain **the file for your model** before a download is offered.
- A failed check shows `GitHub unreachable` (network) or `no release found` (no release, or a private repo); it retries after about a minute.

**繁體中文**

- 版本號逐段以數字比較：`5.3.0.3` 比 `5.3.0.2` 新；熱修復 `5.3.0.2.1` 也比 `5.3.0.2` 新；舊版本不會被推送。
- 碟機讀不到目前版本（顯示 `dev`、沒有 `version.txt`）時，**不會**宣稱有更新。
- Release 中必須有**對應你機型的檔案**才會提供下載。
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
| `wrong model refused` | `--bundle` 的文件名前缀与碟机型号不符。换对应型号的包。/ The `--bundle` prefix does not match the deck. / `--bundle` 檔名前綴與機型不符。 |
| `SHA256SUMS mismatch … damaged` / `download SHA-256 mismatch` | 下载损坏或被篡改。删除文件重下，不要强行安装。/ Delete and re-download; do not force it. / 刪除後重新下載，勿強行安裝。 |
| `读不到 data 分区大小` | 无法确定新镜像大小。用 `--userdata-backup` 提供备份镜像。/ Pass `--userdata-backup`. / 以 `--userdata-backup` 提供備份映像。 |
| `需要 python3 和 e2fsprogs` | 缺 Python 3 / `debugfs` / `mke2fs`。安装后重试（Windows 用 WSL）。/ Install them (WSL on Windows). / 安裝後重試（Windows 用 WSL）。 |
| `installed version reads back as …` | 安装后读回的版本与包不符，安装器不会继续激活。保留输出并重试一次。/ The installer does not activate; retry once and keep the output. / 安裝器不會繼續啟用；保留輸出並重試一次。 |

### 5.4 刷机后保留用户配置 / Keeping your settings / 刷機後保留使用者設定

**简体中文**

- **第二代（模式 A、OTA）**：只写入 `/data/enginex` 和一个 ALSA 配置文件，音乐、曲库、设置保留。
- **第二代（模式 B）**：整个 `/data` 分区会被替换。**一定要**用 `--userdata-backup` 指向你自己这台碟机的 `/data` 完整备份；绝不要用空镜像或别人的镜像，否则数据全丢。
- **第一代（U 盘 / OTA）**：刷的是系统镜像。仍请先自行备份重要的音乐与曲库（USB/SD 上的内容不受影响）。
- 刷机前请先拔掉 U 盘和 SD 卡，避免误操作。

**English**

- **2nd gen (Mode A, OTA)**: only `/data/enginex` and one ALSA config file are written; music, library and settings stay.
- **2nd gen (Mode B)**: the whole `/data` partition is replaced. **Always** pass `--userdata-backup` pointing at a full backup of this deck's own `/data`; never use an empty or someone else's image, or the data is lost.
- **1st gen (USB stick / OTA)**: you flash the system image. Still back up important music and library first (content on USB/SD is unaffected).
- Unplug USB sticks and SD cards before flashing.

**繁體中文**

- **第二代（模式 A、OTA）**：只寫入 `/data/enginex` 與一個 ALSA 設定檔，音樂、曲庫、設定保留。
- **第二代（模式 B）**：整個 `/data` 分割區會被取代。**務必**以 `--userdata-backup` 指向這台碟機自己的 `/data` 完整備份；切勿使用空映像或他人映像，否則資料全失。
- **第一代（USB 隨身碟 / OTA）**：刷入的是系統映像。仍請先備份重要音樂與曲庫（USB/SD 上的內容不受影響）。
- 刷機前請先拔除 USB 隨身碟與 SD 卡。

### 5.5 回退原厂 / Back to stock / 回退原廠

**第二代 / 2nd gen / 第二代**

```sh
# 在碟机的 root shell 里 / in the deck's root shell / 在碟機的 root shell 中
sh /data/enginex/bundle/uninstall.sh && reboot
```

- 中：只移除数据包新增的内容，系统分区本来就没被改过。
- EN: It removes only what the bundle added; the system partition was never modified.
- 繁：只移除資料包新增的內容，系統分割區本來就未被改動。
- 没有 shell 时：用模式 B 的原理，刷回你自己 `/data` 的**原始备份**（未注入的那一份）。/ With no shell, flash back your **original, un-injected** `/data` backup via Fastboot. / 沒有 shell 時，經 Fastboot 刷回你**未注入的原始** `/data` 備份。

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
