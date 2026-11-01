# Denon SC5000 Prime (Engine OS 5.0.4) 深度定制系统 (`sc5000-custom-os`) 需求与架构方案

> 状态：**已定稿，多 Agent 并发科研与开发中**
> 目标设备：Denon DJ SC5000 Prime (`sc5000.lan` / `192.168.1.108`，硬件代号 `JP07`，Rockchip RK3288 ARMv7)
> 基础底包：Engine OS 5.0.4 官方固件（`AZ01` 容器 / Yocto Scarthgap / Linux 6.6.119-rt / Qt 6.7.2）
> 仿真测试环境：`Peyton-C/QEngine`（在编译机 `IN` 上模拟 `JP07` 硬件刷机与联调）

---

## 一、最终敲定的核心需求清单

### 1. 固件逆向与 QEngine 仿真开发流
- **基于官方 5.0.4 底包深度定制**：
  - 解包 `SC5000-5.0.4-Update.img`（`AZ01` 格式）与对照 `SC5000-4.3.4-Update.img`（`FIT` 格式）。
  - 全面分析 `/usr/Engine/`、系统启动服务、只读 `rootfs` + `overlayfs` 结构、机型特征码（`JP07`）功能开关。
  - 完整逆向 Engine OS 5.0.4 SQLite 数据库（`m.db` / `p.db`）Schema 及 `PerformanceData`（RGB 波形、Beatgrid 拍线、Cue 点、Loops）二进制结构。
- **QEngine (`Peyton-C/QEngine`) 模拟器先行**：
  - 在编译机 `IN` (the x86 build host) 搭建 `QEngine` 模拟 SC5000 (`JP07`)，所有固件重打包、内核模块、守护进程先在模拟器跑通验证。

### 2. Rekordbox 深度兼容与零扫描秒读 (`rb-bridge`)
- **USB 插入零扫描秒读 (`rb-usb-bridge`)**：
  - 监听 USB/SD 存储挂载，检测到 `PIONEER/rekordbox/export.pdb`（或 `exportLibrary.db`）及 `PIONEER/USBANLZ/*/*.DAT/.EXT/.2EX` 时，直接将先锋现成的 RGB 波形、Beatgrid 拍线、Hot Cue、Memory Cue、调性、BPM 转换为 Engine 5.0.4 的 `m.db` 记录。
  - **免重扫音频**：无需 SC5000 重新解码整首音频算波形，实现 1~2 秒秒读。
  - **增量秒同步**：再次插入 U 盘时仅增量同步新增/修改曲目与歌单。
- **彻底解决“CDJ 能读、SC5000 读不到列表/歌曲”的兼容问题**：
  - 修复 macOS **Unicode NFD/NFC 编码差异**导致的路径找不到文件被静默丢弃问题；
  - 修复多层嵌套歌单夹、同名子歌单、智能歌单（Intelligent Playlists）解析丢失问题；
  - 修复特殊 WAV（如 `WAVEFORMATEXTENSIBLE` 头）与超大内嵌封面 FLAC 引起的曲目跳过问题。
- **支持连接 Rekordbox (`rb-link-bridge`)**：
  - 支持局域网连接 Rekordbox（Pro DJ Link / 远程曲库同步）。
- **明确不做**：**不自动生成/覆盖 Hot Cue A/B**（仅保留原曲目已有的 Cue 点与 Memory Cue 映射）。

### 3. 机内流媒体歌单直连 + Deemix 本地下载 + 机内实时分析 (`stream-sync`)
- **纯机内独立运行（无需依赖局域网电脑算力）**：
  - 所有歌单浏览、点击下载、实时音频分析（BPM、调性、Beatgrid、RGB 波形）与数据库写入**全部在 SC5000 本机独立完成**。
  - 支持在后台一边播放当前 Deck 歌曲，一边点击下载新歌并实时分析，下载分析完成后即可切入加载播放。
- **三大流媒体歌单登录与浏览**：
  - 支持 **Deezer**、**Spotify**、**网易云音乐** 登录与歌单读取（支持二维码扫码/Token 登录）。
- **统一通过 Deemix 接口下载**：
  - Spotify 曲目优先通过 ISRC 精确匹配，网易云音乐通过「清洗标题 + 艺人对齐 + 时长误差 ±2s」精确匹配，调用 Deemix 接口下载无损音频并触发实时分析入库。

### 4. macOS 扩展格式 (HFS+) 与 APFS 原生支持 (`fs-compat`)
- **Mac OS 扩展格式（HFS+，含 Journaled 日志式）**：
  - 编译加载 Linux 6.6.119-rt 的 `hfsplus.ko` + `fsck.hfsplus -f`，支持自动清理脏位并以 `-o force,rw` 满速读写挂载。
- **APFS 格式（Apple File System）**：
  - 交叉编译内核态 `linux-apfs-rw` (`apfs.ko`) 模块，零 FUSE 开销高速读取 APFS 盘；配合 `overlayfs` 将数据库写入重定向到 SD 卡/机内缓存，既能秒读打点又 100% 保护 Mac APFS 分区安全。

### 5. 硬件级流畅度调优与增强功能
- **RK3288 硬件级性能与实时度调优（必做）**：
  - CPU/GPU 锁定 `performance` 定频模式；
  - **核心硬隔离（保障边下歌分析边打碟绝对不卡）**：将 ALSA 实时音频中断锁在 `CPU0`，Qt 6.7 UI 与 60fps 波形渲染锁在 `CPU1-2`，将 Deemix 下载与实时波形/BPM 分析进程通过 `cgroup` + `SCHED_IDLE` + `ionice -c 3` + `taskset` 严格隔离在 `CPU3` 单核运行；
  - 精简无用遥测服务，调优 SQLite `mmap_size` 与 VFS 缓存。
- **机内智能接歌推荐置顶列表（`⚡ Smart Next`，必做）**：
  - 根据当前播放曲目的 BPM 和调性，按 **BPM 差值 ≤ 4 + Camelot 调性兼容 + 能量走向** 动态在机内维护推荐接歌虚拟列表。
- **正面 SD 卡槽作为「永久高速缓存仓」（必做）**：
  - 将正面常驻 SD 卡作为 Deemix 实时下载曲库、流媒体歌单缓存、以及 APFS 外接盘 Overlay 数据库的默认持久化高速仓库。
