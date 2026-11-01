# sc5000-custom-os

Denon DJ SC5000 Prime (`JP07` / Rockchip RK3288 / Engine OS 5.0.4) 深度定制固件与增强套件。

## 核心模块

1. **`tools/az01-packer/`**：Engine OS 5.0.4 (`AZ01` 容器) 解包、校验、只读 `rootfs` 定制与安全重打包工具（严禁触碰 `uboot` 与 `/usr/Engine/Firmware/` MCU 固件）。
2. **`docs/re-report/`**：Engine OS 5.0.4 相比 4.3.4 的底层架构、`/usr/Engine/` 服务链路、SQLite `m.db` / `p.db` 表结构与 `PerformanceData`（RGB 波形、Beatgrid、Cue）逆向分析文档。
3. **`cmd/rb-bridge/`**：
   - **USB 零扫描秒读 (`rb-usb-bridge`)**：插入 Rekordbox U 盘直接将 `export.pdb` / `exportLibrary.db` 与 `USBANLZ/*.DAT/.EXT/.2EX` 波形秒转为 Engine 5.0.4 `m.db`，修复 macOS NFD/NFC 路径编码丢歌、嵌套歌单夹与特殊 WAV/FLAC 兼容问题，支持增量同步（不自动覆盖 Hot Cue A/B）。
   - **Rekordbox 局域网直连 (`rb-link-bridge`)**：支持 Pro DJ Link / 曲库直连。
   - **`⚡ Smart Next` 智能推荐列表**：基于当前播放曲目的 BPM（≤ 4）与 Camelot 调性动态生成推荐接歌歌单。
4. **`cmd/stream-sync/`**：
   - **机内流媒体歌单 + Deemix 下载 + 实时分析引擎**：支持 Deezer、Spotify（ISRC 精确匹配）、网易云音乐（扫码登录 + 标题/艺人/时长对齐）浏览歌单，调用本地 Deemix 接口下载并在机内 `CPU3`（`SCHED_IDLE` + `ionice` 核心硬隔离）实时生成 RGB 波形与 Beatgrid 入库，后台边下边放零卡顿。
5. **`rootfs-overlay/`**：
   - **macOS 文件系统支持 (`fs-compat`)**：Linux 6.6.119-rt 的 `hfsplus.ko`（含 Journaled 强制读写修复）与 `linux-apfs-rw` (`apfs.ko`) 内核模块 + SD 卡 `overlayfs` 写保护。
   - **RK3288 硬件级流畅度调优 (`perf-tune`)**：CPU/GPU `performance` 锁频、ALSA/UI/后台任务三层 CPU 亲和性隔离、SQLite `mmap` 调优、正面 SD 卡永久高速缓存仓挂载。
6. **`sim/qengine/`**：基于 `Peyton-C/QEngine` 的 QEMU `JP07` 模拟刷机与自动化联调套件。

详见 [REQUIREMENTS_AND_PLAN.md](./REQUIREMENTS_AND_PLAN.md)。

文件系统与硬件调优套件的构建说明见 [kernel-modules/README.md](./kernel-modules/README.md)，
挂载生命周期、systemd/udev 安装及原厂 Engine 接线要求见 [rootfs-overlay/README.md](./rootfs-overlay/README.md)。
可在本地运行 `bash tests/test_rootfs_overlay.sh` 验证存储策略和硬件调优的模拟场景；实机驱动 ABI 与延迟仍需目标固件和设备验证。
