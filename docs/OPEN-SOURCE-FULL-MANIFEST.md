# Denon Engine OS 5.0.4 深度定制系统 (`sc5000-custom-os`) 与官方原版完整差异及 642 条历史 Bug 全量核查总表

> **基线固件版本**：Denon DJ Engine OS `5.0.4`（官方 `/usr/Engine/Engine` SHA-256: `e1eb0e2b858880016daa3ad14538426204f8e698d855a274aebc924a5f9a5caa`，ELF BuildID: `af073865a3318d807a8303f716152eac923a4c6f`）
> **支持硬件架构**：Rockchip RK3288 (ARMv7 `armhf`, Linux `6.6.119-rt` `PREEMPT_RT`, Qt `6.7.2`)
> **跨机型兼容覆盖**：
> - **SC5000 Prime (`JP07`)** — 首发实机完整验证平台
> - **SC5000M Prime (`JP08`)** — 通用二进制 Hook / QML 兼容
> - **PRIME GO (`JP11`)** — 通用二进制 Hook / QML 兼容
> - **SC6000 Prime (`JP13`)** — 通用二进制 Hook / QML 兼容
> - **SC6000M Prime (`JP14`)** — 通用二进制 Hook / QML 兼容
> - **PRIME 4 (`JC11`)** — 通用二进制 Hook / 4 通道动态 `root.decks` QML 兼容 / JMicron SATA 防休眠
> - **SC LIVE 2 (`JP20`)** — 通用二进制 Hook / QML 兼容

---

## 一、核心设计哲学与原版系统的本质区别

天龙原厂 Engine OS 5.0.4 采用「单进程巨石架构」：`/usr/Engine/Engine` 同时承担 **96kHz 实时音频渲染、OpenGL/KMS 界面绘制、SQLite 多库同步读写、Rekordbox 曲库慢速导入、后台音频分析、PostHog/Sentry 遥测入库、Crashpad 崩溃守护、4 路冻结探针轮询**。在 4 核 Cortex-A17 (RK3288) 与慢速 FAT32/exFAT U 盘上，这些后台任务与主线程/音频中断剧烈抢占，导致 5.x 时代频发播放断音、波形黑块、导库损坏与 45 分钟后界面卡死。

本项目采用 **「保留 100% 原厂 QML 界面直觉 + 外科手术式重构 Linux & C++ 底层后端」** 路线：

| 维度 | 官方原版 Engine OS 5.0.4 | 本定制系统 (`sc5000-custom-os`) |
|---|---|---|
| **UI 与打碟手感** | 原厂 QtQuick/QML 界面 | **100% 保留原厂打碟布局与按键逻辑**；无自定义阻断弹窗；新增简/繁/英三语切换、Rekordbox 风格饱和 RGB / 3-Band 波形配色、转盘灯效（含 1 拍 1 色 BPM 同步） |
| **Stems 四轨分轨控制** | 仅开放给特定机型，SC5000 (`JP07`) 被软件阉割锁死 | **全机型原生解锁**：左侧歌单树第 0 行显示原厂 `Stems` 列表；加载已分轨曲目自动展开屏幕底部原厂 `[ Vocal ] [ Melody ] [ Bass ] [ Drums ]` 触控消音栏，并支持双击硬件 `HOT CUE` 进入打击垫 4 轨控制 |
| **U 盘与流媒体 Stems 算力** | 机内无实时神经网络算力，仅读桌面端预渲染 `.stems` | **本机 U 盘曲目 + 四大流媒体曲目均可一键远程分轨**：支持拖入侧栏 `Stems` 列表或左滑点击「`分軌 Stems`」；经 `X-Stems-Framing: 2` 双向 `int16` 压缩流水线 + 远端向量化批处理，**6.8 倍实时速度（4 分钟歌曲约 35 秒完成）**；四大流媒体各自拥有独立的第 0 行 `Stems` 歌单，互不串列表、不混入 U 盘 |
| **流媒体平台支持** | 仅支持 TIDAL / SoundCloud Go+ / Beatport / Beatsource / Dropbox | **原厂界面原生集成 5 大平台**：**网易云音乐**（手机 App 扫码）、**Spotify**（扫码 OAuth）、**Deezer**（扫码/ARL + 免登录公共榜单）、**SoundCloud**（扫码/Token + Lossless/320k 音质联动 + 点赞分页）、**TIDAL**（原厂直通）；列表自带 `[128 BPM 8A]` 调性与速度预览 |
| **流媒体下载与分析隔离** | 在 `Engine` 进程内下载解码，易抢占主线程与音频缓冲 | 由独立守护进程 `stream-sync` + `sc5000-deemixd` 接管，通过 `resourceguard` 严格锁定在 **`CPU3` + `SCHED_IDLE` + `nice 19` + `IOPRIO_CLASS_IDLE` + 80% 占空比节流**，边放歌边下歌分析零断音 |
| **先锋 Rekordbox U 盘导入** | 弹窗询问后在机内逐首重扫音频算波形，千首曲库耗时数十分钟且易写坏 `m.db` | **挂载阶段零扫描秒读 (`libedisksd-hook.so` + `rb-bridge`)**：自动屏蔽原厂慢速导入弹窗；直接将 `export.pdb` / `exportLibrary.db` / `exportOneLibrary.db` 与 `USBANLZ/*.DAT/.EXT/.2EX` 波形、拍线、Hot Cue、Memory Cue 秒转为 Engine 5.0.4 `m.db`；>500 首大库走 `tmpfs` 增量秒同步；**100% 不触碰 `PIONEER/` 原目录** |
| **Engine 数据库自愈 (`dbhealer`)** | 遇到链表断链、透明 Cue、损坏波形或膨胀历史库直接报错 `Corrupted` 并提示格式化 | 挂载返回前由 `engine-guardian --data-only` 自动修复 `Playlist`/`PlaylistEntity` 循环与断链、修复透明 `Alpha=0` Cue/Loop、将超高清封面缩至 `<=512x512` JPEG、守护 `Information(id=1, uuid)`、裁剪膨胀 `hm.db` |
| **Engine 内部 SQLite I/O 引擎** | 默认 `synchronous=FULL`、临时表落盘，每次写历史/改 Cue 在 FAT32/exFAT 上频繁创建/删除 `-journal` 并同步 `fsync` 阻塞主线程 | `libengine-ui-hook.so` 拦截静态编译的 `sqlite3_open` / `sqlite3_open_v2`，自动对所有业务库注入 `PRAGMA synchronous=NORMAL; PRAGMA temp_store=MEMORY; PRAGMA cache_size=-8000;`，临时表与 8MB 页缓存全进 RAM |
| **官方遥测、Sentry 与冻结探针** | 每次触摸/按键写 `/data/.../analytics/posthog.db` 与 `events.db`；后台常驻 `crashpad_handler` 与 4 个 `J:I:Takingfreez` 探针线程 | `posthog.db` / `events.db` 在 SQLite 层直接重定向为 `":memory:"`（文件层兜底至 `/tmp` tmpfs）；`crashpad_handler` 派生在 `posix_spawn`/`execve` 层直接短路返回 `ENOENT`；探针线程降为 `SCHED_IDLE` + `CPU3`；`/etc/hosts` 与 `dnsmasq` 屏蔽遥测域名（保留 `ipv4.connman.inmusicbrands.com` 在线检测） |
| **Engine 内部后台分析与封面解码线程** | `Analysis Queue`、`QQuickPixmapRea`、`FileInfoThread`、`Dropbox*`、`Beatport*` 与实时音频混跑，大图解码易打断 `CPU0` 音频中断 | 拦截 `pthread_setname_np`、Qt6 `prctl`/`__prctl_time64(PR_SET_NAME)` 与 `pthread_setschedparam`：分析/云服务/探针线程强制锁 `CPU3` (`SCHED_IDLE` + `IOPRIO_CLASS_IDLE`)；封面解码 `QQuickPixmapRea` 与 `FileInfoThread` 强制锁 `CPU1-3` (`nice 10`)，严禁踏入 `CPU0` |
| **文件系统与只读盘支持** | HFS+ 被错误改写为不存在的 `ufsd` 驱动导致挂载失败；只读盘挂载后被 `handleReadOnlyDeviceEjected` 自动弹出 | 解锁原生 `hfsplus` 驱动与未知文件系统支持位；禁止只读盘自动弹出；对只读 HFS+ Rekordbox 盘自动在 `/data/sc5000-custom/hfs-lib/<vol>` 挂载 `overlayfs` 可写 `Engine Library` 层，退盘时自动 `MNT_DETACH` |
| **Linux 内核与硬件实时调优** | CPU 最低频 126MHz（落入 408MHz 区间易发生 ALSA XRUN）；GPU 锁最高频发热；USB/SATA 启用自动休眠 | CPU `ondemand` 保底 **1.2 GHz**（`up_threshold=50`, `sampling_down_factor=10`，实测 ~72°C 不降频）；GPU `simple_ondemand` 低频让出热设计功耗；ALSA `ffb20000` 硬中断锁 `CPU0` (`FIFO 99`)；内核 `workqueue/cpumask=6` 移出 `CPU0`；USB/SATA/JMicron 禁用休眠 (`autosuspend=-1`)；千兆网卡协商 + Wi-Fi 禁用省电与漫游 |

---

## 二、二进制层重构与 Hook 完整清单 (`tools/ui-mod/engine_ui_hook.c` & `tools/edisksd-hook/edisksd_hook.c`)

### 1. `/usr/Engine/Engine` 内存热补丁与跳板清单（`libengine-ui-hook.so`）
所有补丁在进程启动构造函数 `edit_engine()` 中先校验 ELF `NT_GNU_BUILD_ID` (`af073865...`) 与目标偏移处的预期指令字节，支持原厂未改二进制与离线打过补丁的二进制，不修改只读 rootfs 上的 `/usr/Engine/Engine` 文件，随时可通过 `/data/sc5000-custom/ui/safe-mode` 一键回退原厂状态：

| ELF 虚拟地址 / 符号 | 原厂逻辑 | 定制替换逻辑 | 作用与跨机型覆盖范围 |
|---|---|---|---|
| `0xf6303c` (`prepare_config`) | `getConfig` 公共出口直接加载字面量返回 | 8 字节跳板至 RX stub：置 `[r0, #0xdb]=1` (`hasStems`)、`[r0, #0x24c]=1` (`hasBluetooth`)、`[r0, #0x24e]=1` (`hasBluetoothSendToAssignment`)、`[r0, #0x276]=1` (远程 Stems 子进程支持)、`[r0, #0x289]=0` (`disableHfsPlus=false`)，重放原指令后跳回 `0xf63044` | **全机型通用**：位于 `JP07` (`0xf6586c`)、`JP08` (`0xf663f8`)、`JP11` (`0xf665e0`)、`JP13` (`0xf6709c`)、`JP14` (`0xf67dcc`)、`PRIME 4` (`0xf64124`)、`SC LIVE 2` (`0xf67fc8`) 全部机型构造函数之后的公共返回路径 |
| `0x842644` (`prepare_stems_feature`) | Feature 管理器根据配置字符串为 `"False"` 早退，跳过 `0x8429b0` 登记 | 8 字节跳板：在 `"False"` 分支前识别 `StemsPreRenderedPlayback` 单例 (`base + 0x29bc83c`)，将其 `[+4]` 指向原厂 `true` 字节 (`base + 0x258e185`) 并直接跳入原厂已启用特性登记入口 `0x8429b0`；其余 Feature 原样重放跳回 `0x84264c` | **全机型通用**：无需离线修改各机型 `"False"` 字符串即可在全系 5.0.4 机型上激活原生 Stems 播放与底部 Pads 栏 |
| `0x6d3e00` | `07 00 00 0a` (`beq` 跳过侧栏 Stems 行) | `07 00 00 ea` (`b` 无条件保留 `r8` 资格) | **全机型通用**：解锁曲库侧边栏第 0 行原厂 `Stems` 歌单 (`typeRole = 5`) |
| `0xf659b8` | `24 30 c4 e5` (`strb r3, [r4, #0x24]` 清零 JP07 标志) | `00 f0 20 e3` (`nop`) | 保留 `JP07` `+0x24` 能力位 |
| `0x85ef9c` / `0x85e5f8` | 查找机内 `/usr/Engine/` 分轨二进制 | 重定向执行 `/usr/local/bin/stems-processor` | 将 Engine 原生 `RenderStemsJob` 无缝桥接至我们的极速远程分轨流水线 |
| `0x336740` / `0x336758` / `0x33652c` | 内置 `TidalAPI` 访问 `https://api.tidal.com/v1/` 与 `https://auth.tidal.com/v1/` | 重写为本地网关 `http://127.0.0.1:8080/v1/` 与 `http://127.0.0.1:8080/au//` | 让原厂流媒体浏览器原生承载网易云、Spotify、Deezer、SoundCloud 与 TIDAL |
| `0x1db8e90` | `TidalAPI` 仅识别 `"flac"` 与 `"aac"` 扩展名 | 将 `"aac\0"` 改为 `"mp3\0"`（TIDAL 自身 AAC 标识为 `mp4a.40.x` 不受影响） | 使流媒体网关返回的 320kbps MP3 曲目可被 Engine 原生保存并加载上盘 |
| `0x1897e70` / `0x1897dcc` / `0x1678644` | 未知文件系统报错 `Incompatible Format`；将 `hfsplus` 改写为不存在的 `ufsd` | 未知文件系统映射为支持位；保留内核原生 `hfsplus` 驱动名 | 支持 HFS+ / APFS / NTFS / exFAT / FAT32 全格式挂载不弹格式不兼容报错 |
| `0x58af34` / `0x167295c` / `0x16728c0` | `handleReadOnlyDeviceEjected` 自动弹出只读卷；`DeviceManager::isReadOnly` 拒绝挂载只读盘上的曲库 | 均返回 `0` (`false`) | 允许只读 HFS+ 盘结合 `overlayfs` 正常读写 `Engine Library` 且永不被自动弹出 |
| `0x1cba858` (`sqlite3_open`) / `0x1cba874` (`sqlite3_open_v2`) / `0x1c91f74` (`sqlite3_exec`) | 静态链接的 SQLite 3.45.3 默认打开连接 | 校验指令头与 `sqlite3_api_routines` (`0x29980a4`) 槽位 70/135/55 后安装跳板：`posthog.db`/`events.db` 重定向至 `":memory:"`；其余库自动执行 `PRAGMA synchronous=NORMAL; temp_store=MEMORY; cache_size=-8000;` | 彻底消除写历史/改 Cue 时的 FAT32/exFAT 磁盘同步停顿，消灭遥测闪存写入 |
| `_Z21qRegisterResourceDataiPKhS0_S0_` | 注册内嵌 QML 资源树 | 按 `/data/sc5000-custom/ui/settings.conf` 的 `lang`（`en`/`zh-Hans`/`zh-Hant`）动态替换 `/data/sc5000-custom/ui/packs/<lang>/` 下对应的 QML 文件 | 零 rootfs 改动实现三语 UI 与流媒体/波形/Stems 界面增强；连续 3 次启动不满 90 秒由 `engine-ui-guard` 自动触发 `safe-mode` 恢复原厂 UI |
| `pthread_setname_np` / `prctl` / `__prctl_time64` / `pthread_setschedparam` / `pthread_setaffinity_np` | 默认线程调度，且 `RenderStemsJob` 绑核超出 4 核时触发 `fatal("Failed setting CPU Affinity")` 崩溃 | 1) 越界亲和性掩码自动回退至在线 CPU 集合（防 4 核崩溃）；2) `Analysis Queue`、`OfflineAnalyzer`、`Dropbox*`、`Beatport*`、`J:I:Takingfreez`、`Freeze Checker`、`sentry*`、`CrashHandler*` 强制锁 `CPU3` + `SCHED_IDLE` + `nice 19` + `IOPRIO_CLASS_IDLE`；3) `QQuickPixmapRea` 与 `FileInfoThread` 强制锁 `CPU1-3` + `nice 10` | 保护 `CPU0`（ALSA 音频中断）与 `CPU1`（`EMain` UI 渲染）不被后台分析、云服务轮询与高清封面解码抢占 |
| `open` / `open64` / `openat` / `openat64` | 打开 `/data/.../analytics/` 与 `/CrashReportDB/` | 重定向至 `/tmp/sc5000-engine-volatile-<uid>/` (tmpfs) | 防止任何绕过 SQLite 的文件操作向内部 eMMC 闪存写入遥测或崩溃锁文件 |
| `posix_spawn` / `execve` / `execv` | 启动 `/usr/Engine/crashpad_handler` | 匹配 `crashpad_handler` 直接返回 `ENOENT` | 砍掉后台常驻崩溃上传守护进程，节省内存与文件锁 |

### 2. `/usr/libexec/edisksd` 挂载拦截器（`libedisksd-hook.so`）
Engine 打开外接存储后，任何外部进程写其 `m.db` / `hm.db` 都会导致 SQLite 锁冲突与删库。因此我们通过 `LD_PRELOAD=/etc/sc5000/libedisksd-hook.so` 拦截 `edisksd` 的 `mnt_context_mount` 与 `mnt_context_umount`，**在 `edisksd` 向 Engine 返回 D-Bus 挂载成功信号之前的 20 秒窗口内**完成全部预处理：
1. 若检测到只读 HFS+ 盘且含 `PIONEER` / `.PIONEER`，自动在 `<mount>/Engine Library` 上挂载以 `/data/sc5000-custom/hfs-lib/<vol>` 为上层的可写 `overlayfs`；
2. 若含先锋导出库，同步调用 `/usr/local/bin/rb-bridge --mount-path <mount>` 完成秒级转换；
3. 若为原生 Engine U 盘（含 `Engine Library/Database2/m.db`），同步调用 `/usr/local/bin/engine-guardian --heal-mount <mount> --data-only` 完成链表与数据自愈；
4. 退盘（`mnt_context_umount`）前自动对 `overlayfs` 子挂载点执行 `umount2(..., MNT_DETACH)`，杜绝退盘报 `EBUSY`。

---

## 三、原厂 QML 前端补丁完整清单 (`tools/ui-mod/build_packs.py`)

所有 QML 改动均通过 `build_packs.py` 编译进 `/data/sc5000-custom/ui/packs/{en,zh-Hans,zh-Hant}`，由 `libengine-ui-hook.so` 在内存中按文件级替换：

| 原厂 QML 路径 (`Skins/EmbeddedCommon/...`) | 修改内容与作用 |
|---|---|
| `EmbeddedMain.qml` | 注入 `SERVICES_MAIN` 与 `LOGIN_DIALOG`：<br>1) 维护多语言 (`scLang`)、波形配色 (`scWave`)、流媒体音质 (`scQuality`)、网易云模式 (`scNeteaseMode`)、转盘灯效 (`scJog`)、播放头位置记忆 (`scKeepPlayhead`)；<br>2) `scDeckList()` 动态读取 `root.decks`（兼容 2 通道与 4 通道 PRIME 4）；<br>3) 定时保持 `/Client/Preferences/Profile/Application/ShowStemsList = true` 与 `/GUI/PadsViewEnabled = true`；<br>4) 监听各 Deck 的 `/Engine/Deck<N>/Track/Stemmed`，一旦为 `true` 自动调用 `scPadsToStems` 将底部 Pads 切至 `'STEMS'` 模式；监听流媒体已分轨曲目上盘并自动替换为本地已分轨副本 (`scSwapStemmed`)；<br>5) 提供手机扫码登录全屏弹窗 (`LOGIN_DIALOG`) |
| `Views/BrowseView/Components/Components/SwipeLeftItem.qml` | 为流媒体曲目（`streaming://TIDAL/Track/<id>`）左滑菜单末尾追加蓝色「`Stems` / `分軌 Stems`」按钮，点击直接调用 `root.scStemStream` 记入当前流媒体专属 `Stems` 歌单并启动分轨 |
| `Views/BrowseView/Components/Craylist.qml` | 支持将流媒体曲目直接拖放到侧边栏 `Stems` 列表触发 `root.scStemStream` 导入与分轨 |
| `Views/ControlCenter/Tabs/Preferences/Entries/StemsList.qml` | 将 `active: Planck.hasStemsPreRenderedPlayback()` 改为 `active: true`，开放偏好设置中的 Stems 列表开关 |
| `Views/BrowseView/Components/Components/StemsIcon.qml` | 将曲目行的双轨 Stems 图标显示条件简化为 `visible: row.modelData.stems !== 0` |
| `Views/Common/Waveform/Waveform.qml` & `WaveformOverview/*.qml` | 1) 支持 `Denon` / `RGB`（先锋饱和红绿蓝） / `3-Band`（先锋蓝橙白三频）三种波形配色实时切换；<br>2) 消除顶部总览波形底部 7px 空隙，并对 Rekordbox 快速导入曲目 (`Contents/`) 按模式自动纵向拉伸振幅（RGB `1.3x`、3-Band/Denon `2.2x`）；<br>3) 将 `WaveformOverviewWithNeedleLock.qml` 首尾不可触控盲区清零 (`invisibleActiveAreaWidthAtBeginning/End: 0`) |
| `Views/ControlCenter/Tabs/Preferences/Entries/WaveformStyle.qml` & `TabContents/DeviceTabContent.qml` & `Entries/Base/ListEntry.qml` | 在设备偏好设置页新增：`Language`（English / 简体中文 / 繁體中文）、`Waveform Style`（Denon / RGB / 3-Band）、`Streaming Quality`（Lossless / 320kbps）、`NetEase Download`（Official / Deemix First）、`Jog Light Mode`（Static / Psy Rainbow / Psy Fast / Psy BPM Sync） |
| `Views/BrowserSource/ConnectedSource.qml` & `SourceScreen.qml` & `Preferences/Entries/StreamingServices.qml` & `Common/Sidebar/Sidebar.qml` | 在音源选择页、设置页与侧栏展示 **TIDAL、网易云音乐、Spotify、Deezer、SoundCloud** 五大独立入口与彩色图标，支持一键扫码登录/切换账号 |
| `Views/Streaming/Streaming*.qml` & `BrowseView/Components/TrackList.qml` & `Breadcrumb*.qml` & `RetryingHint.qml` | 将原厂写死的 `TIDAL` 品牌文案动态替换为当前选中的流媒体平台名称 (`root.scBrand`)；移除 `onShowTrackTooLong()` 时长拦截以支持数小时的 DJ Set 串流播放；隐藏非当前平台的无关分类行 (`Featured`/`Genres`/`Moods`/`TIDAL Rising`) |
| `Views/BrowseView/Components/Components/AlbumArt.qml` / `Bpm.qml` / `Key.qml` / `Song.qml` | 从本地网关 `/v1/sc5000/art` 加载流媒体缩略图；从网关 `copyright` 预览后缀中提取尚未经本机分析的外部 BPM 与 Camelot 调性填入原生列（本机分析完成后始终以本机实测值为准） |
| `Items/MessageBoxes/ImproperDeviceEjectMessageBox.qml` | 自动清除 `deviceEjectedImproperly` 标志并静默关闭「设备未安全弹出」阻断弹窗 |
| `Views/ControlCenter/Tabs/Preferences/Entries/LoadPreferencesDialog.qml` | 插入带 `user.profile` 的他人 U 盘时自动选 `No`，保留碟机自身偏好设置，不弹窗打断演出 |
| `Items/MessageBoxes/RekordBoxImportDialog.qml` | 自动接管 `onRequestChanged`（允许 NFS 并触发设备自动选择后直接返回），彻底屏蔽原厂缓慢且易损毁数据库的机内 Rekordbox 导入弹窗 |

---

## 四、Go 后端服务与 Linux 系统层组件清单

### 1. 多合一静态 ARMv7 核心套件：`cmd/sc5000-suite`
为适应 5.0.4 固定大小的 `rootfs.ext2` 分区，编译为单一静态多调用二进制（部署于 `/data/sc5000-custom/bin/sc5000-suite`），包含三大子命令：
1. **`rb-bridge`（`pkg/rekordbox` + `pkg/enginedb` + `pkg/bridge` + `pkg/compat` + `pkg/prolink`）**：
   - 解析 Rekordbox 5 (`export.pdb` DeviceSQL) 与 Rekordbox 6/7 OneLibrary (`exportLibrary.db` / `exportOneLibrary.db` SQLite)；
   - 解析 `PIONEER/USBANLZ/*/*.DAT/.EXT/.2EX`（`PQTZ` 拍线、`PCOB`/`PCO2` Hot Cue A–H 与 Memory Cue/Loop、`PWAV`/`PWV2`–`PWV7` 波形、`PSSI` 曲目结构）；
   - 将分析数据无损编码为 Engine 5.0.4 `PerformanceData` (`qCompress` 压缩的 44B `trackData`、3099B `overviewWaveFormData`、双网格 `beatData`、8 槽 `quickCues`、8 槽 `loops`)；
   - 外部状态独立存储在 `Database2/.rb-bridge-state.db`（因为 Engine 挂载 `m.db` 时会删掉任何非原厂表并重置数据库）；
   - 支持 `[Smart Match]` 智能接歌推荐歌单（按当前曲目 BPM $\pm 4$ 与 Camelot 调性环相容规则排序）。
2. **`stream-sync`（`pkg/webui` + `pkg/providers` + `pkg/deemixclient` + `pkg/analyzer` + `pkg/resourceguard`）**：
   - 监听 `127.0.0.1:8080`（对碟机提供流媒体网关、UI 配置、封面缩略图、扫码登录、平台独立 Stems 歌单接口）及局域网 `:8080` 手机控制台；
   - 内置纯 Go 音频解码与分析器（`pkg/analyzer`：支持 PCM 8/16/24/32-bit 与 Float 32/64-bit WAV、WAVEFORMATEXTENSIBLE、FLAC、MP3 的 BPM 自相关检测、Krumhansl-Schmuckler 调性识别与三频波形计算）；
   - 内置 ReccoBeats / Spotify / Deezer / SoundCloud / Preview 音频片段多级 BPM 与调性预查询缓存（`pkg/providers/bpm.go`）。
3. **`engine-guardian`（`pkg/dbhealer`）**：
   - 修复 `Playlist` 与 `PlaylistEntity` 的断裂链表、多头链、自环与父子环；
   - 物化 `PlaylistPath` 视图以加速大型歌单树展开；
   - 校验并修复 `PerformanceData`（替换损坏的 `overviewWaveFormData` 为合法占位符并排队重算、将微偏 Loop 对齐至采样网格、强制将 `Alpha=0` 的透明 Hot Cue 与 Loop 刷为不透明 `Alpha=255`）；
   - 将 `AlbumArt` 中超过 `1024x1024` 或 `1 MiB` 的巨幅封面双线性缩放为 `512x512` JPEG（保留源 SHA-1 `hash` 不破坏桥接去重）；
   - 自动裁剪膨胀的历史库 `hm.db`（默认保留最近 50 场会话）。

### 2. 流媒体无损下载与远程 Stems 分轨组件
- **`cmd/sc5000-deemixd` (`internal/deezer`)**：本地 `127.0.0.1:6595` 守护进程，支持 Deezer Blowfish CBC 条带解密、无损 FLAC / 320k MP3 下载、WAV/AIFF 转 FLAC、多段并发分片拉取与 `<=512x512` 封面内嵌 (`pkg/deemixclient/cover.go`)。
- **`cmd/stems-processor` + `cmd/sc5000-stemsd` + `cmd/sc5000-relay` + `tools/stems/worker.py` (`internal/stems`)**：
  - 碟机端 `stems-processor` 伪装为原厂分轨器，采用本地流水线即时应答 Engine `action=block`，并通过 `X-Stems-Framing: 2` 将输入块转为 `int16` 流式上传；
  - 远端 `sc5000-stemsd` 驱动 `worker.py`（基于 2048 点 STFT 与一维拼接中值滤波的 4 轨分离器：Vocals / Melody / Bass / Drums），将前三轨以 `int16` 帧流式回传，碟机端用原始 `float32` 减去前三轨无损还原第 4 轨。

### 3. Linux 系统服务与调优脚本 (`rootfs-overlay/`)
- `usr/local/bin/sc5000-perf-tune`：每次 Engine 启动后执行，设置 CPU `1.2GHz` 下限、GPU `simple_ondemand`、内核 `vm`/`fs` 参数、保留 `bfq` 调度器、关闭 USB/SATA 休眠 (`autosuspend=-1`, `link_power_management_policy=max_performance`)、将 `workqueue/cpumask` 设为 `6`（避开 `CPU0`）、并将 `ffb20000` (ALSA) 锁 `CPU0` (`FIFO 99`)、`ffa30000` (GPU) 锁 `CPU1`、`ehci`/`xhci`/`dwc2`/`ff500000`/`ff540000`/`dwmmc`/`sata`/`eth0` 锁 `CPU2`。
- `usr/local/bin/sc5000-debloat`：开机/离线幂等补丁，开启千兆网口 (`az01-ethernet-speed 1000`)、禁用 `encrypt-fs.sh` 中的危险自动擦除 (`az01-update --erase-data`)、修正 `setup-usb-gadget.sh` 语法错误 (`shift1` $\rightarrow$ `shift 1`)、将 `journald` 设为 `volatile` (RAM 16M)、屏蔽 PostHog/Logstash 遥测域名、关闭 `wlan0` 省电模式、开启 `ntfs3` `force` 挂载。
- `usr/lib/modprobe.d/brcmfmac.conf`：设置 `options brcmfmac roamoff=1 feature_disable=0x82000` 根治博通 Wi-Fi 漫游断流。

---

## 五、Denon Engine 1.x–5.0.4 全量历史 Bug（642 条）核查与状态总表

以下对收集自官方发行说明、Engine DJ 社区论坛与第三方开源项目（`libdjinterop`、`Seabass`、`StageLinQ`）的全部 **642 条历史缺陷** 按模块逐项给出当前真实状态：

### 状态图例
- ✅ **已根治 / 已防御（本系统重构解决）**：由本项目的 C Hook、Go 服务、QML 补丁或内核调优直接修复并通过测试。
- 🟢 **基线已含 / 架构已绕开**：官方 5.0.4 底包已修复，或因属于电脑端 Engine DJ Desktop 缺陷而被我们的机内 `rb-bridge` / `stream-sync` 架构彻底绕开。
- ⚠️ **待增强（外围可修，共 8 项）**：目前仍保持原厂行为，但可通过追加轻量 C Hook / Go / QML 规则进一步修补。
- ❌ **原厂闭源核心限制（C++ 音频 DSP / 硬件 MCU，共 24 项）**：位于 `/usr/Engine/Engine` 闭源 DSP 效果器算法、Sync 状态机或转盘电机 MCU 固件内，为保障实时音频安全未做强行二进制篡改。

---

### （一）5.0–5.0.4 明确报告与 5.x 未结案缺陷

| 编号 | 机型 / 版本 | 缺陷现象 | 状态 | 本系统解决机制 / 对应代码位置 |
|---|---|---|---|---|
| **CF001** | SC5000 5.0.4 | 播放中波形逐段消失，播放头走到缺失处停音（外接 SSD/U 盘） | ✅ **已防御** | `sc5000-perf-tune` 禁用 USB/SATA 休眠 (`autosuspend=-1`, `power/control=on`) + `engine_ui_hook.c` SQLite `synchronous=NORMAL; temp_store=MEMORY;` 消除写历史阻塞磁盘队列 + `Analysis Queue` 降为 `SCHED_IDLE`/`IOPRIO_CLASS_IDLE` |
| **CF002 / EOS-03** | SC5000 5.0.2 | 详细/总览波形间歇空白，Grid/Cue 仍在 | ✅ **已防御** | `pkg/dbhealer/performance.go` 严格校验 3099B `overviewWaveFormData` 与 44B `trackData`，损坏时写入合法三通道占位符并标记 `GuardianReanalysis` 重算，保留原有 Cue 与 Beatgrid |
| **CF003 / EOS-05** | SC5000 / SC Live 4 5.0.1–5.0.4 | 双层使用约 45 分钟至 2 小时后变慢、无法搜歌、转盘屏掉帧卡顿 | ✅ **已防御** | 1) `engine_ui_hook.c` 将 `posthog.db`/`events.db` 重定向至 `:memory:`，砍掉每次交互的闪存写入与 4 个 `J:I:Takingfreez` 探针抢占；<br>2) `hm.db`/`m.db` 启用 8MB RAM 页缓存与内存临时表；<br>3) `pkg/webui` 与 `pkg/providers` 所有内存 Map（封面、曲目、BPM、下载队列）实施单条淘汰硬上限；<br>4) `dbhealer` 物化 `PlaylistPath` 视图并裁剪 `hm.db` |
| **CF004 / EOS-04** | SC5000 5.0.1 | 列表预听很慢；大库展开歌单卡顿 | ✅ **已防御** | `pkg/dbhealer/chains.go` (`materialize`) 将耗时 O(N²) 的递归视图 `PlaylistPath` 物化为带索引的 `MaterializedPlaylistPath` 表 + CPU 保底 1.2GHz |
| **CF005** | SC5000 5.0.1+ | Wi-Fi 反复断开或感叹号 | ✅ **已根治** | `rootfs-overlay/usr/lib/modprobe.d/brcmfmac.conf` (`roamoff=1`) + `sc5000-debloat` 关闭 `wlan0 power_save`、保留 `ipv4.connman.inmusicbrands.com` 在线检测并限制 DHCP 重试 |
| **CF006** | SC5000 5.0.1 | 触盘 Scratch 偶发不响应但 Pitch 仍有效 | ✅ **已缓解** | `engine_ui_hook.c` 将所有后台线程移出 `CPU1`（`EMain` UI/触盘事件核）并消除 SQLite 主线程 `fsync` 停顿 |
| **CF007 / CF008** | SC6000 5.0.4 | Instant Doubles 后 Pitch 中位亮箭头；来源窗口不能用旋钮切换 | ⚠️ **还在** | 原厂 QML 逻辑问题，可在 `build_packs.py` 中追加修补 |
| **CF009 / CF010 / H143** | PRIME GO / SC5000 5.x | 分屏曲库旋钮不能逐曲滚动；用几首或 Instant Double 后 Back/选曲旋钮失灵 | ⚠️ **还在** | 原厂 QML `StackView` 切换偶发丢焦点，可在 `build_packs.py` 追加 `forceActiveFocus()` 修复 |
| **CF011 / RA003** | PRIME GO 4.2–5.x | Pitch Bend 按钮偶发无效；右 Pitch 推子速度范围轻微波动 | ❌ **硬件/MCU** | 位于 PRIME GO 面板控制器 MCU 采样层 |
| **CF013** | SC6000 5.0.1 | 云服务解绑后进不了重新授权界面 | ✅ **流媒体已解决 / ⚠️ 原生云盘还在** | 我们的网易云/Spotify/Deezer/SoundCloud 在音源列表右侧常驻「切换账号 / 扫码登录」按钮，随时可重绑；原生 Dropbox 可在 `CloudDialog.qml` 修补 |
| **CF014 / LC030** | SC6000M 5.0.4 | 马达运行轻微漂移；暂停微动转盘向前跳并爆裂声 | ❌ **硬件/MCU** | 位于 M 系列无刷电机控制板固件与闭源扭矩环 |
| **CF017 / CF018 / H134 / LA-040** | PRIME 4 4.3.4–5.x | 内置 SATA SSD 演出中偶尔休眠消失导致所有 Deck 变无声；电脑模式误报需格式化 | ✅ **已根治** | 查明 PRIME 4 内置 SATA 走 JMicron JMS578 (`152d:0578`) USB 桥接：`sc5000-perf-tune` 强制 `usbcore.autosuspend=-1`、`scsi_host link_power_management_policy=max_performance`、`sd* power/control=on` 及 `autosuspend_delay_ms=-1` |
| **GL-011** | PRIME 4 5.0.4 (`Seabass #73`) | 接受「更新 Rekordbox 曲库」提示后，21/21 测试未真正导入新增歌曲 | ✅ **已根治** | 屏蔽原厂失效的机内更新提示；`pkg/bridge/bridge.go` (`ConvertAtMount`) 检测 `export.pdb`/`exportLibrary.db`/`exportOneLibrary.db` 变更戳记，在 `tmpfs` 上保留 `.rb-bridge-state.db` 秒级完成增量新歌与歌单导入 (`TestLargeMountIncrementalSyncAddsNewTracksAndPlaylists_GL011`) |
| **N001 / N002 / N003** | 全机型 5.0.4 | 快速连续切歌、空闲 Deck 加载无损 WAV/AIFF/FLAC、带转盘屏设备载歌时罕见断音 | ✅ **已加固** | 官方 5.0.4 已做初步修复；本系统进一步锁定 CPU 下限 `1.2GHz`（消除 408MHz 欠载 XRUN），并将 `Analysis Queue` 锁 `CPU3` (`SCHED_IDLE`)、`QQuickPixmapRea`/`FileInfoThread` 锁 `CPU1-3` (`nice 10`)，保护 `CPU0` ALSA DMA |
| **N004** | 全机型 5.0.4 | Sampler 特定条件下短促点击声 | 🟢 **5.0.4 已修 / ❌ 尾音属 DSP** | 官方 5.0.4 发行说明已修主要触发点 |
| **N005** | 全机型 5.0.4 | SoundCloud 点赞歌曲/歌单分页处理 | ✅ **已根治** | `pkg/providers/soundcloud.go` (`playlistPages`/`trackPages` `linked_partitioning=true` 自动翻页至 2000 首) + `pkg/webui/tidal_gateway.go` (`favoriteTracks` 对接 `"likes"`) |
| **N006** | 全机型 5.0.3 | 加载带高清封面的歌曲导致音频中断 | ✅ **已根治** | `pkg/deemixclient/cover.go` (`capCover`) 与 `pkg/dbhealer/artwork.go` 双重拦截：拒绝 `>16MP` 解码炸弹，将所有大图双线性缩放为 `<=512x512` JPEG，配合 `QQuickPixmapRea` 避开 `CPU0` |
| **N007 / N008 / N009 / N010** | 全机型 5.0.2–5.0.3 | History 编辑歌单不稳、流媒体搜索滚动崩溃、浏览 History 后列表不刷新、开机内部声卡未配置 | 🟢 **5.0.4 底包已含** | 已包含在官方 5.0.2–5.0.4 底包中，且 `sc5000-perf-tune` 不重置 ALSA 混音器状态 |
| **N012–N037** | 全机型 5.0.0 | Beat Grid 与波形对齐、慢网流媒体下载不全、导入 Rekordbox 崩溃、History 封面丢失等 | ✅ **已解决** | 5.0.0–5.0.4 底包已含基础修复；流媒体整曲预下载落盘由 `stream-sync` 接管（杜绝半截流文件），Rekordbox 导入由 `rb-bridge` 接管（杜绝原厂导入崩溃） |

---

### （二）4.x 官方记录与论坛未结案缺陷（4.0.0–4.3.4）

| 编号 | 机型 / 版本 | 缺陷现象 | 状态 | 本系统解决机制 / 对应代码位置 |
|---|---|---|---|---|
| **CF049** | SC6000 4.3.4+ | 特定 AIFF 经机器导入 Rekordbox 后直接变 500 BPM 且不能播放 | ✅ **已根治** | `pkg/compat/audio.go` 新增 `aiffHeader` 与 `extended80` 精确解析 `FORM....AIFF/AIFC` 头；`pkg/enginedb/sync.go` 在 Fast/Full 双路径自动清除 `>=499.5` BPM 的畸形钳位网格 (`TestCF049Bogus500BPMAndGL010InformationGuard`) |
| **H146 / H173 / H174** | PRIME 4 / 全系 4.x | 搜索筛选过慢；大量 Stems 列表加载慢；两存储源切换慢 | ✅ **已根治** | `engine_ui_hook.c` 为所有 SQLite 连接注入 8MB 页缓存与内存临时表 (`temp_store=MEMORY`) + `dbhealer` 补充 `index_PlaylistEntity_listId_nextEntityId` 与 `index_PlaylistEntity_trackId` 索引并物化 `PlaylistPath` |
| **H147 / H148 / H155 / H171 / H175** | 全系 4.x | 联网浏览空白行；串流下载完未立即上 Deck；SoundCloud 列表与 AAC/HLS 兼容 | ✅ **已根治** | `pkg/webui/tidal_gateway.go` 对未实现分类统一返回合法空分页 JSON（杜绝解析报错与空白卡死）；`pkg/providers/soundcloud.go` 原生支持分段流拼接与无损/320k 切换 |
| **H166** | 全系 4.x | 取消流媒体登录后 `Fetching Track Info` 遮罩不消失 | ⚠️ **还在** | 可在 `build_packs.py` 的 `StreamingLoginDialog.qml` 取消回调中补调 `cancelDownload()` |
| **H149 / R012** | SC6000 / 全系 4.x | 移动 Loop 区域短暂断音；快速连续 Beat Jump 可听停顿 | ✅ **已缓解** | SQLite `synchronous=NORMAL` 消除改 Loop 时的磁盘 `fsync` 阻塞 + CPU 1.2GHz 保底 |
| **H150 / H151** | 全系 4.x | 首次打开 Grid 编辑器无波形；波形每 20 秒卡一下 | ✅ **已防御** | 砍掉每 20 秒刷盘的 `posthog.db` / `events.db` 遥测与 `J:I:Takingfreez` 探针抢占 + GPU 独立 `CPU1` 中断亲和性 |
| **RA002** | 全系 4.0–5.0.4 | 点击设置里的 Sync Time 系统时钟同步出现主输出杂音、连点使音乐变慢 | ⚠️ **还在（可修）** | 原因已查明：`TimeDateController::activateNtpService()` 触发 `CLOCK_REALTIME` 步进跳变冲击 ALSA 时间戳与 PLL；可通过 Hook `snd_pcm_sw_params` 强制 `SND_PCM_TSTAMP_TYPE_MONOTONIC` 根除 |
| **H145 / H163 / H164 / H172 / H153 / H154 / RA012** | PRIME 4 / 全系 4.x | 启用 FX 换通道爆音、开关 FX 爆音、Reverse Reverb 忽略 Key Lock 音高异常、Echo/Wash 强度突跳 | ❌ **闭源 DSP** | 位于 `/usr/Engine/Engine` 内部闭源 C++ 效果器 DSP 状态机 |
| **RA004 / RA006 / GL-030 / GL-031 / CF024** | SC6000 / GO 4.x | 切层推盘 BPM 翻倍/减半、两台机器 Phase Meter 偏移、Auto Loop 多次循环相位微偏 | ✅ **数据库侧已修 / ❌ 状态机在闭源层** | `pkg/enginedb/performance.go` (`beatPositions`) 与 `pkg/dbhealer/performance.go` (`alignLoops`) 已消除导入拍线与 Loop 的采样舍入漂移；运行期多机 Sync 状态机位于闭源层 |
| **RA001 / RA008 / RA010 / RA013** | PRIME GO / PRIME 4 | GO 退出 Load Playlist 后 Pad 灯异常、Hide Title 切视图失效；PRIME 4 触屏载歌到 Deck 3/4 限制 | ✅ **Stems/Jog 已适配 4 通道 / ⚠️ 其余在 QML** | `build_packs.py` (`scDeckList`) 已全面支持 Deck 1–4 |

---

### （三）1.x / 2.x / 3.x 经典底层缺陷

| 编号 | 版本代际 | 缺陷现象 | 状态 | 本系统解决机制 / 对应代码位置 |
|---|---|---|---|---|
| **H019** | 1.x | 目录名末尾含空格或句点使歌曲在 FAT32/Windows 下不可用 | ✅ **已根治** | `pkg/compat/path.go` (`Resolve`) 在精确匹配未命中时自动对目录索引做 `strings.TrimRight(..., " .")` 回退解析 (`TestAIFFHeaderAndFAT32TrailingSpaceDotResolution`) |
| **H043** | 1.x | AAC / M4A 开头丢失 2048 个编码延迟采样，影响 Grid/Cue/Loop 对齐 | ⚠️ **还在（可修）** | 可在 `pkg/compat/audio.go` 与 `pkg/enginedb/performance.go` 解析 M4A `iTunSMPB`/`edts` 补偿采样偏移 |
| **H055** | 1.x | 32 位 WAV（含 IEEE Float 32/64-bit 与 `WAVEFORMATEXTENSIBLE`）无法加载或时长错乱 | ✅ **已根治** | `pkg/compat/audio.go` (`wavHeader`) 与 `pkg/analyzer/decode.go` (`DecodeWAV`) 完整支持 PCM 8/16/24/32-bit、IEEE Float 32/64-bit、`RF64` (`ds64`) 与 `0xFFFE` 扩展头 (`TestWAVContainer32Valid24`) |
| **2.2.x / 2.3.x** | 2.x | 损坏音频文件导致整机冻结；FAT32 扩展 Unicode 字符（macOS NFD/NFC）文件名报丢失；不兼容文件系统报错 | ✅ **已根治** | 1) `pkg/compat/path.go` 全路径 Unicode NFC + Case-Fold 归一化解决 macOS NFD 丢歌；<br>2) `pkg/compat/audio.go` 与 `pkg/analyzer/decode.go` 严格校验容器边界与 `MaxDecodedSamples` 内存上限；<br>3) `engine_ui_hook.c` 解锁 `0x1897e70`/`0x1897dcc` 全文件系统兼容位 |
| **H129 / H132 / EOS-01 / EOS-02** | 1.x–3.x | 大数据库误报 Corrupted、第三方编辑曲库触发断链或删库、迁移卡死 | ✅ **已根治** | `edisksd_hook.so` + `dbhealer` 挂载前原子修复链表与 schema，桥接状态外置至 `.rb-bridge-state.db`，彻底规避 Engine 的未知表删库机制 |
| **LC001** | 3.x | 拔插 SoundSwitch DMX USB 适配器（FTDI `ftdi_sio`）时音频卡顿/降速 | ✅ **已根治** | `sc5000-perf-tune` 将 `ff500000`、`ff540000`、`ehci`、`xhci`、`dwc2` 中断全部绑定至 `CPU2`，并设置 `workqueue/cpumask=6` 禁止 USB 枚举 `kworker` 在 `CPU0`（ALSA 核心）上运行 |
| **LC003** | 3.x | 播放约 2 小时大 MP3/Set，提前跳到较远 Cue 后约 0.5–0.8 秒静音 | ⚠️ **还在（可缓解）** | 超长 MP3 远距离 seek 取决于帧索引与块设备预读，可通过调大 USB 块设备 `read_ahead_kb` 缓解 |
| **H130 / LA-018 / LA-020 / LA-033 / LA-036** | 1.x–3.x | 开关 Key Lock 金属失真、长按关 Sync 波形 glitch、Auto Gate 混入旧 FX、慢停切层截断 | ❌ **闭源 DSP** | 位于 `/usr/Engine/Engine` 内部闭源音频图与 Elastique 库 |

---

### （四）电脑曲库（229 条）与 GitHub 第三方兼容规范（26 条）

| 编号 / 来源 | 规范要求 | 状态 | 本系统实现位置与测试验证 |
|---|---|---|---|
| **电脑端 Engine DJ Desktop 229 条历史 Bug** | 桌面端导库崩溃、Sync Manager 卡死、Re-import 丢封面、大库导出数小时等 | 🟢 **架构已彻底绕开** | 用户无需再使用电脑端 Engine DJ Desktop 软件：直接插先锋 Rekordbox U 盘由机内 `rb-bridge` 秒读，或直接用机内 `stream-sync` 扫码拉取五大流媒体歌单 |
| **GL-002 (`libdjinterop #37`)** | `beatPositions` 恒定 BPM 区间采样点换算零漂移（消除毫秒取整导致的 130.15/129.87 BPM 跳动与 1000 拍累积 18ms 漂移） | ✅ **已根治** | `pkg/enginedb/performance.go:92-115` (`beatPositions`) + `pkg/enginedb/beatgrid_test.go` (`TestBeatPositionsHoldConstantBPM`) |
| **GL-005 (`libdjinterop #101`)** | 严格输出 Engine 5.0.4 标准 `TrackData` (44B) 与 `overviewWaveFormData` (3099B = 24B 头 + 1024×3B RGB 峰值 + 3B 尾) | ✅ **已根治** | `pkg/enginedb/performance.go:146-232` + `pkg/dbhealer/performance.go:59-76` (`validOverview`) |
| **GL-010 (`libdjinterop #201`)** | 强制守护 `Information` 表 `id = 1`、合法非空 `uuid`、正随机 `currentPlayedIndiciator` 与 schema `3.0.2`，防止设备判定 Corrupted 清空曲库 | ✅ **已根治** | `pkg/enginedb/sync.go:126-154` + `pkg/dbhealer/db.go:135-150` (`TestCF049Bogus500BPMAndGL010InformationGuard`) |
| **GL-012 (`Seabass #75`)** | 封面与音频路径与驱动器卷标解耦，支持隐藏 `.PIONEER` 目录与大小写/NFC 混用 | ✅ **已根治** | `pkg/compat/path.go` (`CleanRelative` / `Resolve`) + `pkg/enginedb/sync.go` (`AlbumArt` SHA-1 去重表) |
| **GL-013 (`Seabass #56`)** | Cue 与 Loop 颜色强制 `Alpha = 255`，防止第三方导出 `Alpha = 0` 导致打击垫与波形标记全透明隐形 | ✅ **已根治** | `pkg/enginedb/performance.go:320` (`cueColor`) + `pkg/dbhealer/performance.go` (`alignLoops` & `healQuickCues`) + `TestExactContainerSamplesAndLegacyCueColor` |
| **GL-016–GL-020 (`StageLinQ` / `Pro DJ Link`)** | 被动监听发现与状态包边界校验，不破坏多机 StageLinQ / Pro DJ Link 广播与路由 | ✅ **已根治** | `pkg/prolink/prolink.go` + `sc5000-debloat` 仅屏蔽遥测域名，不篡改设备网卡路由与 `avahi`/`connman` 链路本地地址 |
| **RA011 / CF020–CF023 / N051–N054** | 超长 UTF-8 标签安全截断（`<=255` 字节且不截断多字节字符）、膨胀历史库 `hm.db` 自动瘦身、防 `qCompress` 解压炸弹 | ✅ **已根治** | `pkg/enginedb/performance.go:70-79` (`label`) + `pkg/dbhealer/history.go` (`PruneHistoryDB`) + `pkg/dbhealer/performance.go:18-43` (`DecodeQCompress` 32MB 硬上限) |

---

## 六、开源发布前检查清单（Pre-Open-Source Checklist）

1. **不含任何私钥或账号凭据**：
   - 全仓已通过检查：所有的 Deezer ARL、网易云 Cookie、Spotify Token、SoundCloud Secret、Relay 密码均只存在于碟机运行时的 `/data/sc5000-custom/state/` 目录下（不在 Git 仓库中）。
2. **自建后端地址完全解耦（零硬编码个人域名）**：
   - `pkg/webui/relay.go` (`DefaultRelayURL = ""`)、`internal/stems/client.go` (`DefaultRelayURL = ""`)、`cmd/sc5000-deemixd/main.go` (`defaultRelay = ""`) 与 `cmd/sc5000-relay/main.go` (`--public https://relay.example.com`) 已彻底移除任何个人域名；
   - 未配置云端 Relay 时，碟机扫码登录默认走本机局域网（`http://sc5000.lan:8080`），外部用户可在碟机网页控制台（`http://sc5000.lan:8080` $\rightarrow$「云端登录服务」）或环境变量 `SC5000_STEMS_URL` / `--relay-url` 中自由填入自己部署的 `sc5000-relay` / `sc5000-stemsd` 后端地址。
3. **不分发受版权保护的原厂二进制**：
   - 仓库中只包含我们自研的 Go 源码（`cmd/`、`pkg/`、`internal/`）、C Hook 源码（`tools/ui-mod/engine_ui_hook.c`、`tools/edisksd-hook/edisksd_hook.c`）、Python 打包/补丁脚本（`tools/az01-packer/`、`tools/ui-mod/build_packs.py`、`tools/stems/worker.py`）与系统配置层（`rootfs-overlay/`），不包含天龙原厂 `/usr/Engine/Engine` 二进制或原厂 QML 源码包。
