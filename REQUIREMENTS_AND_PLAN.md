# Denon SC5000 Prime (Engine OS 5.0.4) 深度定制系统 (`sc5000-custom-os`) 需求与架构方案

> 状态：**原始需求蓝图；2026-11-03 已接管，当前进度与实施顺序见 [接续方案](docs/CONTINUATION-PLAN.md)**。
> 本文中的秒读、完整 Cue/波形兼容和功能描述是目标，不代表均已通过实机验收。
> 目标设备：Denon DJ SC5000 Prime (`sc5000.lan` / `192.168.1.108`，硬件代号 `JP07`，Rockchip RK3288 ARMv7)
> 基础底包：Engine OS 5.0.4 官方固件（`AZ01` 容器 / Yocto Scarthgap / Linux 6.6.119-rt / Qt 6.7.2）
> 仿真测试环境：`Peyton-C/QEngine`（在编译机 `IN` 上模拟 `JP07` 硬件刷机与联调）

---

## 零、第一设计宪法：「它首先是一台任何人上来就能放歌的纯粹碟机」

**不要过度魔改 UI，绝不绑定个人私癖，兼容每一个来放歌的朋友（The Music Must Go On）：**

1. **100% 保持原厂操作直觉（零学习成本、零阻断弹窗）**：
   - 不乱改原厂打碟界面布局、不改变旋钮/按键原本的逻辑、不弹任何需要点击确认的自定义拦截框。任何习惯 CDJ 或 Denon 的 DJ 朋友走上台，看到的都是标准、直观的碟机界面，3 秒内就能选歌推推子。
2. **尊重并兼容每一位朋友的 U 盘与使用习惯**：
   - **先锋 Rekordbox 习惯的朋友**：插上 U 盘（无论 FAT32、exFAT、Mac 扩展 HFS+ 还是 APFS），后台 1~2 秒静默把他的 Rekordbox 歌单、文件夹结构、原有的 Hot Cue / Memory Cue、拍线和波形原样呈现；**绝不改乱他的歌单顺序，绝不往他的歌里强塞自动 Cue 点，也绝不破坏他 U 盘原有的 `PIONEER/` 目录**（拔下来插回 CDJ 依然 100% 正常）。
   - **Denon Engine DJ 习惯的朋友**：检测到原生 `Engine Library` 直接原厂直通，零干扰。
   - **没导库直接拿硬盘装歌的朋友**：支持 HFS+ / APFS / exFAT / NTFS / FAT32 全格式即插即读，直接进原厂文件夹目录（Files）选歌就放。
3. **所有增强功能必须「无感透明」或「非侵入式可选」**：
   - 底层做的所有事情（秒读转换、编码修复、内核锁频、APFS 驱动）全部在后台静默完成，前台只体现为**“怎么什么盘都能秒读、怎么一点都不卡”**。
   - 像智能接歌推荐（`Smart Next`）或流媒体下载（Spotify / 网易云 / Deezer），只作为歌单树最底部的可选辅助目录或独立音源呈现，绝不喧宾夺主、绝不遮挡正常歌单。朋友来了甚至可以扫自己的网易云/Spotify 临时拉歌进 `[Guest / 临时加歌]` 列表，音乐永不中断。

---

## 一、核心功能模块清单

### 1. 固件逆向与 QEngine 仿真开发流
- **基于官方 5.0.4 底包定制**：
  - 解包 `SC5000-5.0.4-Update.img`（`AZ01` 容器）与对照 `SC5000-4.3.4-Update.img`（`FIT` 容器）。
  - 全面分析 `/usr/Engine/`、系统启动服务、只读 `rootfs` + `overlayfs` 结构。
  - 完整逆向 Engine OS 5.0.4 SQLite 数据库（`m.db` / `p.db`）Schema 及 `PerformanceData`（RGB 波形、Beatgrid 拍线、Cue 点、Loops）二进制结构。
- **QEngine (`Peyton-C/QEngine`) 模拟器先行**：
  - 在编译机 `IN` (`hostdzire-in-16c32g`) 搭建 `QEngine` 模拟 SC5000 (`JP07`)，所有固件重打包、内核模块、守护进程先在模拟器跑通验证。

### 2. Rekordbox 零扫描秒读与全兼容桥接 (`rb-bridge`)
- **USB 插入零扫描秒读 (`rb-usb-bridge`)**：
  - 监听 USB/SD 存储挂载，检测到 `PIONEER/rekordbox/export.pdb`（或 OneLibrary `exportLibrary.db`）及 `PIONEER/USBANLZ/*/*.DAT/.EXT/.2EX` 时，直接将先锋现成的 RGB 波形、Beatgrid 拍线、Hot Cue、Memory Cue、调性、BPM 转换为 Engine 5.0.4 的 `m.db` 记录。
  - **免重扫音频**：无需 SC5000 重新解码整首音频算波形，实现 1~2 秒秒读。
  - **增量秒同步**：再次插入 U 盘时仅增量同步新增/修改曲目与歌单。
- **彻底解决“CDJ 能读、SC5000 读不到列表/歌曲”的兼容问题**：
  - 修复 macOS **Unicode NFD/NFC 编码差异**导致的路径找不到文件被静默丢弃问题；
  - 修复多层嵌套歌单夹、同名子歌单、智能歌单（Intelligent Playlists）解析丢失问题；
  - 修复特殊 WAV（如 `WAVEFORMATEXTENSIBLE` 头）与超大内嵌封面 FLAC 引起的曲目跳过问题。
- **支持连接 Rekordbox (`rb-link-bridge`)**：
  - 支持局域网连接电脑 Rekordbox（Pro DJ Link / 远程曲库同步）。
- **保持原汁原味**：**不自动生成或覆盖任何 Hot Cue**，完全尊重 DJ 原有的打点习惯。

### 3. 机内流媒体歌单 + Deemix 本地下载 + 机内实时分析 (`stream-sync`)
- **纯机内独立运行（无需依赖外部电脑算力）**：
  - 歌单浏览、点击下载、实时音频分析（BPM、调性、Beatgrid、RGB 波形）与数据库写入**全部在 SC5000 本机完成**。
  - 支持后台一边播放当前 Deck 歌曲，一边点击下载新歌并实时分析，下载分析完成后即可在歌单中加载切入。
- **支持主人与访客的三大流媒体歌单（Deezer / Spotify / 网易云音乐）**：
  - 支持二维码扫码或 Token 登录；朋友来玩也可以用手机扫一下把自己的网易云/Spotify 歌单同步显示出来，临时点歌下载到常驻 SD 卡的 `[Stream / 临时下载]` 目录，随下随放。
- **统一通过 Deemix 接口下载**：
  - Spotify 曲目优先通过 ISRC 精确匹配，网易云音乐通过「清洗标题 + 艺人对齐 + 时长误差 ±2s」精确匹配，调用本地 Deemix 接口下载无损音频并触发实时分析入库。

### 4. 全平台文件系统无感支持 (`fs-compat`)
- **Mac OS 扩展格式（HFS+，含 Journaled 日志式）**：
  - 编译加载 Linux 6.6.119-rt 的 `hfsplus.ko` + `fsck.hfsplus -f`，支持自动清理脏位并以 `-o force,rw` 满速读写挂载。
- **APFS 格式（Apple File System）**：
  - 交叉编译内核态 `linux-apfs-rw` (`apfs.ko`) 模块，零 FUSE 开销高速读取 APFS 盘；配合 `overlayfs` 将数据库写入重定向到 SD 卡/机内缓存，既能让拿 Mac APFS 硬盘来的朋友秒读放歌，又 100% 保护朋友的 APFS 硬盘不被写坏。
- **FAT32 / exFAT / NTFS 全兼容**：确保 Windows 和 Mac 用户的任何 U 盘/移动硬盘插上就能用。

### 5. 硬件级流畅度调优与非侵入式辅助 (`perf-tune`)
- **RK3288 硬件级性能与实时度调优（必做）**：
  - CPU/GPU 锁定 `performance` 定频模式；
  - **核心硬隔离（保障边下歌分析边打碟绝对不卡）**：将 ALSA 实时音频中断锁在 `CPU0`，Qt 6.7 UI 与 60fps 波形渲染锁在 `CPU1-2`，将 Deemix 下载与实时波形/BPM 分析进程通过 `cgroup` + `SCHED_IDLE` + `ionice -c 3` + `taskset` 严格隔离在 `CPU3` 单核运行；
  - 精简无用遥测服务，调优 SQLite `mmap_size` 与 VFS 缓存。
- **非侵入式智能接歌辅助歌单（`[Smart Match]`，可选子目录）**：
  - 在歌单树底部提供一个可选的 `[Smart Match]` 辅助列表（按当前播放曲目 BPM 差值 ≤ 4 + Camelot 调性兼容动态筛选），不改变任何原有歌单的排序，不用的人完全无感。
- **正面 SD 卡槽作为「常驻曲库与高速缓存仓」**：
  - 将正面常驻 SD 卡作为流媒体实时下载曲库、访客临时下歌区、以及只读外接盘（如 APFS）Overlay 数据库的默认持久化高速仓库。

### 6. Stems 远程分轨（2026-11-03 新增，验收见 [STEMS-REMOTE-ACCEPTANCE-20261103](docs/STEMS-REMOTE-ACCEPTANCE-20261103.md)）
- **原厂操作不变**：加载已完成分轨的曲目，双按 HOT CUE 模式键进入 Stems pad（上排 1–4），默认四轨全开。SC5000 上的前提条件按 JP07 逆向证据验收，不照搬其他机型。
- **远端分轨**：Engine 原生分轨子进程经 `stems-processor` 转到 clawbot 的 `sc5000-stemsd`；完成/失败信号走应用层结束帧，进度、取消与失败如实上报。
- **在线曲预取**（用户 2026-11-03 新增）：在线曲尽量由 clawbot 远端下载并预先分轨缓存，再整首交付碟机；只用用户已授权的音源/账号。
- **USB 曲**：只在必要时上传一次；缓存命中不重新计算，缓存键与失效规则写入验收文档。
- **诚实标记**：「排队/处理中/已有 .stems 文件」不等于可播；可播以实机加载、四轨独立静音/独奏为准。当前分离器为频谱 HPSS 近似，不是神经网络模型。
