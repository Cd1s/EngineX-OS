# Engine OS 已知未修复 Bug 清单（给固件 / 数据库修复用）

采集日期：2026-11-02。范围：Denon DJ SC5000 / SC5000M 与同代 Prime 播放器（Prime 4 / 4+ / 2 / GO / GO+、SC6000、SC Live 2/4）在 Engine OS 5.0.0–5.0.4 上的用户报告，以及 4.x 起一直没被发行说明关掉的库格式顽疾。

本文件只收录能对上论坛原帖或官方发行说明的问题。搜索摘要里出现过、但原帖对不上的说法，不写成待修项。

## 版本事实

官方下载页 `https://enginedj.com/downloads` 内嵌的 Engine OS 发行记录（2026-11-02 抓取）：

| 版本 | 发布日 | 覆盖硬件 | 和本清单的关系 |
| --- | --- | --- | --- |
| 4.3.4 | 2025-11-18 | 含 SC5000 / SC5000M | 5.0 之前 SC5000 最后一版稳定分支。发行说明只有 SoundCloud AAC 和杂项稳定性，没有修歌单链表或大库卡顿。 |
| 5.0.0 | 2026-05-13 | 含 SC5000 / SC5000M | RGB 波形；封面从数据库抽到独立 Artwork 目录；声称修好「beat grid 与波形对齐」。 |
| 5.0.1 | 2026-05-15 | 含 SC5000 | 只写了 LC6000 连不上。 |
| 5.0.2 | 2026-06-24 | 含 SC5000 | 修流媒体搜索滚动崩溃、看完 History 后曲目列表不刷新、开机音频设备配置。 |
| 5.0.3 | 2026-07-06 | 含 SC5000 | 修「高分辨率封面加载到非播放盘时打断正在播放的盘」；修从 History 返回后编辑歌单的稳定性。 |
| 5.0.4 | 2026-07-28 | 含 SC5000 / SC5000M | **SC5000 至今能升到的最新 Engine OS。** |
| 5.1.0 / 5.1.1 | 2026-09-01 / 2026-09-28 | 仅 PRIME 4 G2 | 不要当成 SC5000 已修好。 |
| 5.2.0 | 2026-09-23 | 仅 RANE SYSTEM ONE | 同上。其中有一条「加载不可用 TIDAL 曲目会把其他正在播放的 TIDAL 曲目卸掉」，和下面的 Wi-Fi/Tidal 弹轨同类，但没有下发到 SC5000。 |

5.0.4 发行说明原文只列了这些修复：

- 快速连续跳曲时的音频中断。
- 在非播放盘加载高质量 WAV / AIFF / FLAC 时，打断正在播放的盘。
- 中心轮设备（Prime 4、Prime 4+、Prime 2、SC5000、SC6000）加载曲目时的罕见音频中断。
- Sampler 在特定条件下的短点击声。
- SoundCloud Liked Tracks / Playlists 分页。
- Rane System One 从出厂固件升级失败。

桌面端 Engine DJ Desktop 5.1.0（2026-09-01）另有：大库迁移少死机、损坏数据库文件不再卡住启动、Re-import Track Information 不再误删封面。2026-10-09 仍有用户在桌面 5.1.0 + 设备 5.0.4 上把歌单清空，所以这三条不能当成设备侧已修好。

本机对照：`4.3.4/m.db.schema.sql` 与 `5.0.4/m.db.schema.sql` 的唯一结构差异是 `Track` 多了 `albumArtSourceHash CHAR(40)`。`Playlist` / `PlaylistEntity` 的链表字段、唯一约束和触发器没有变。4.x 的歌单顺序损坏不可能被 5.0.4 的 schema 迁移修掉。

## 不要再当开放项重做

这些已经被发行说明点名，或原帖后来自己撤回。回归测试可以做，不要当成新缺陷从头修。

- **5.0.0–5.0.3 加载曲目时播放盘掉音 / 卡顿。** 主帖 [69065](https://community.enginedj.com/t/engine-os-5-0-x-audio-stutter-and-lag-when-loading-tracks/69065)（2026-05-15 开帖，2026-08-10 最后回复，341 帖）。5.0.3 修高分辨率封面，5.0.4 修高质量文件加载和中心轮设备。2026-07-28 有用户写 5.0.4 菜单明显变快。Reddit [r/DenonPrime 1v8yimp](https://www.reddit.com/r/DenonPrime/comments/1v8yimp/engineos_504_just_dropped_lots_of_audio_fixes/) 是在转述这批音频修复，不是新的未修复报告。
- **5.0.0 后 LC6000 连不上 SC6000。** [68991](https://community.enginedj.com/t/lc6000-not-connecting-after-5-0-0-upgrade/68991)（2026-05-13）。5.0.1 发行说明已写修复。
- **4.3.1 波形视觉漂移，声音仍对齐。** [63314](https://community.enginedj.com/t/engine-os-4-3-1-visual-drifting-of-beatgrid/63314)（2025-04-23，帖子已关闭）。5.0.0 发行说明写了 “Fixed alignment issues between beat grids and waveforms.” 没有 5.0.4 之后的复现帖。
- **5.0.1 之后拍线看起来偏半拍。** [70386](https://community.enginedj.com/t/5-0-0-beatgrids-after-update-1-2-off/70386)（2026-08-15）。楼主 2026-08-22 撤回：控制器上正常，只是电脑上 Engine Prime 的波形看起来怪。同帖有人说更新后丢过已改的 grid，那是备份问题，不是这条显示 bug。
- **5.0.4 后 SC5000 的 Engine Library 不出现在电脑源列表。** [70272](https://community.enginedj.com/t/5-0-4-possible-bug-enginedj-library-no-longer-accesible-over-wifi/70272)（2026-08-03）。楼主后来写明是 macOS 27 开发者预览版的网络权限，Beta 5 后恢复。不是固件缺陷。
- **左滑直接 Prepare 变成加载到盘 1。** [69119](https://community.enginedj.com/t/engine-5-0-bugs-problems/69119)（2026-05-16）。官方回复这是 4.6.0 起的有意改动。

## 仍应修的项

状态都是：SC5000 最新可刷版本 5.0.4（2026-07-28）的发行说明没有点名，且有原帖在该日期之前未关闭，或在该日期之后仍有人复现。

### EOS-01 歌单链表断裂，导出因 PlaylistEntity 主键/唯一约束失败，歌单丢曲

- 现象：Sync Manager 导出到 Prime 4 失败。日志是往 `PlaylistEntity` 插入时撞上约束（楼主写成 PlaytlistEntity）。用户侧就是歌单缺曲、整表导出中止。官方支持文档把这类问题归到 Engine Library 损坏，恢复办法是删掉驱动器上的 `Database2` 再整库重导。
- 触发条件：桌面与硬件来回同步、导出中途拔盘、从其他软件导入后产生重复行、两次写入各自分配了相同 `id`。4.x 就有，5.0.4 的表结构没变，所以 5.0 没有修。
- 底层原因推断（高，本机 schema 可复核）：
  - `PlaylistEntity` 用 `nextEntityId` 做单向链表，链尾为 0。`Playlist` 用 `nextListId` 做同样的事，并且有插入/删除触发器维护指针。
  - `PlaylistEntity` 只有 `trigger_before_delete_PlaylistEntity`，而且 `WHEN OLD.trackId > 0`。`trackId <= 0` 的行被删时，前驱的 `nextEntityId` 不会改写，链直接断。
  - 没有插入触发器。顺序完全靠应用层先写对。写到一半断电，就会留下重复 `nextEntityId`、环、或多个头。
  - 唯一约束是 `(listId, databaseUuid, trackId)`。同一首歌不能在同一歌单出现两次；导入器若按「再插一行」而不是「已存在则跳过」，整次导出回滚，看起来像整张歌单丢失。
  - `Track` 有触发器禁止回收已删除的 `id`。`PlaylistEntity` 没有同等保护。桌面显式插入旧 `id` 时会撞主键。
- 链接与日期：
  - [Export Fails due to duplicate PlaylistEntity.Id](https://community.enginedj.com/t/export-fails-due-to-duplicate-playlistentity-id/37860)，2021-11-29，1 帖，未关闭。
  - [Engine DJ: Fixing and preventing Engine Library corruption](https://support.enginedj.com/support/solutions/articles/69000815206-engine-dj-fixing-and-preventing-engine-library-corruption)，官方支持文，把修复定义为删 `Database2` 重导。
- 固件 / 数据库层怎么修：
  1. 打开 `m.db` / `hm.db` 后、对 UI 暴露歌单之前，按 `listId` 走链。检查：环、悬空 `nextEntityId`、多个头、未被走到的行、同一个 `nextEntityId` 被两行指向。
  2. 坏了就重建：先按还能走通的顺序，再把孤儿按 `id` 接上，尾节点 `nextEntityId = 0`。用一个事务，不要边浏览边修。
  3. 删掉 `trigger_before_delete_PlaylistEntity` 的 `trackId > 0` 条件，让所有删除都改写前驱。
  4. 导入时不要用外部 `id` 覆盖本地 `AUTOINCREMENT`。先按 `(listId, databaseUuid, trackId)` 查找，已存在则只校正 `nextEntityId`。
  5. `Playlist.nextListId` 做同样的开库校验。`C_NEXT_LIST_ID_UNIQUE_FOR_PARENT` 在两个子列表指向同一个 next 时会让后续插入直接失败。

### EOS-02 播放中报数据库损坏，歌单被清空，曲目行大幅减少

- 现象：演出中弹出 Disk Corrupted。文件还在，Traktor / Serato / Lexicon 仍能读到歌单，只有 Engine 的 `.db` 坏。一例：5 万曲、1500+ 歌单，损坏后 `m.db` 的 `PRAGMA integrity` 仍是 OK，但只剩 2854 条 Track、0 条 Playlist、0 条 PlaylistEntity，schema 显示 3.0.2。桌面报「数据库由更新的软件创建」，升到桌面 5.1.0 后错误还在。SC5000 上更早的表现是：新盘被设备说 corrupt，Prime GO 和桌面却不这么说，同时开机加载数据库要 2–3 分钟。
- 触发条件：exFAT 移动 SSD 上的 `Engine Library/Database2`；桌面与设备版本交错写入；演出中写历史/分析；中途断电或拔盘。SC5000 从 2023 年就有「假 corrupt + 慢加载」，2026-10 在 5.0.4 上仍能把歌单写成 0。
- 底层原因推断（中高）：
  - SQLite 在 exFAT 上没有可靠的文件锁。桌面 Sync 和设备历史写入同时开库，容易写出「integrity_check 通过，但业务表被截断」的半新库。
  - 设备把「schema 版本比自己新」或「某张表行数异常」显示成 Corrupt Database。5.0.4 字符串里有 `Corrupt Database:`、`Device's database is corrupted`、`database.corruption`、`onNewDevicesFound: device is corrupt:`，路径在 `DevicesController.cpp` 约 2669–2689 行、`LibrarianController.cpp` 约 2276 行。
  - 5.0.0 把封面从 `AlbumArt` 大字段迁到独立目录。迁移中断时，应用层可能重建空 `Playlist`，而不是保留旧链表。
- 链接与日期：
  - [I’m Done With Engine DJ OS. Total Disaster](https://community.enginedj.com/t/i-m-done-with-engine-dj-os-total-disaster/71200)，2026-10-09，Prime GO+，当时设备 5.0.4，随后桌面/设备都试过 5.1.0，歌单仍是 0。
  - [SC5000 ISSUES: Freezing, slow database loading, streaming quality mismatch, corrupt databases](https://community.enginedj.com/t/sc5000-issues-freezing-slow-database-loading-streaming-quality-mismatch-issue-corrupt-databases/51261)，2023-09-08。新硬盘、新库，SC5000 仍报 corrupt，Prime GO 不报。
  - [DB is corrupt. Please reformat and rebuild this drive](https://community.enginedj.com/t/db-is-corrupt-please-reformat-and-rebuild-this-drive/5022)，2017-07-29 开帖，2021-06-15 仍有回复。这是同一句错误的老帖。
- 固件 / 数据库层怎么修：
  1. 把「schema 较新」「Playlist 行数为 0 但 Track 还有几千行」「integrity_check 失败」分成三个不同对话框。只有第三种才允许提示格式化。
  2. schema 较新时只读打开，禁止 `DROP`/`CREATE` 歌单表。不要为了让旧固件能写而清空 `PlaylistEntity`。
  3. 写 `m.db` 时用单独的临时库加原子替换（写完 `fsync`，再 `rename` 盖过 `m.db`）。exFAT 上不要依赖 WAL。
  4. 检测到 Playlist 为 0 且存在 `m.db` 同目录备份或 `*.db-journal` 时，先尝试回放 journal，不要直接展示空库。
  5. SC5000 不要在每次滚动歌单时重开库。51261 的「夜里多次重新慢加载」很像浏览路径上又跑了一次全表校验。

### EOS-03 SC5000 放大波形间歇空白，拍线和 Cue 还在

- 现象：SC5000 Prime，音乐在外置 USB SSD（Samsung T7/T9），没有内置 SATA。从 5.0.1 升到 5.0.2 后，放大（zoomed）波形有时整段是黑的。BPM、调性、时长、Cue、拍线和小节线仍在。有的曲正常。同一 U 盘在 5.0.1 没有这个问题。另有人在 4.3.4 的 SC6000 上看到波形局部空白，并伴随音频 glitch，整库重置后好转。
- 触发条件：5.0.2 及之后的波形渲染路径；部分曲目，不是全库；与 5.0.0 起的库/波形数据格式变化同时出现。5.0.3 和 5.0.4 的发行说明只写了音频中断，没有写波形贴图。
- 底层原因推断（中）：
  - 5.0.0 增加 RGB 波形。5.0.4 字符串里有 `WaveformOverviewRGBPraline.frag.qsb`、`WaveformOverviewRGBButterfly.frag.qsb`，以及 `WaveformBuilder.cpp`。空白区仍画得出 beat marker，说明网格数据在，失败的是波形纹理上传或解码。
  - `PerformanceData.overviewWaveFormData` 是 Qt `qCompress` 格式（4 字节原始长度 + zlib）。解压失败、长度为 0、或 RGB 三色缓冲和三频缓冲尺寸不一致时，如果渲染器把失败当成「空纹理」而不是回退到 RMS，用户看到的就是黑底加拍线。
  - 本仓库 `Engine-Library-Format.md` 仍把 `highResolutionWaveFormData` / `overviewWaveFormData` 标为 TODO。5.0 的 RGB 缓冲布局不能假设和 4.3.4 的三频缓冲相同。
- 链接与日期：
  - [Engine OS 5.0.2 - SC5000 Prime intermittent blank waveform](https://community.enginedj.com/t/engine-os-5-0-2-sc5000-prime-intermittent-blank-waveform-after-update-worked-correctly-on-5-0-1/69726)，2026-06-26，2 帖，未关闭。第二帖 2026-06-27 补充 4.3.4 SC6000 也有局部空白。
- 固件 / 数据库层怎么修：
  1. 解压 `overviewWaveFormData` 失败时，异步用 `WaveformBuilder` 在设备上重算，先画三频 RMS，不要画空纹理。重算不要占音频回调。
  2. RGB shader 编译或上屏超时则本曲回退三频，并记住这个回退，避免每帧重试。
  3. 给波形 blob 做长度和魔数检查。坏 blob 标 `isAnalyzed` 的波形位为未生成，不要把整条 `PerformanceData` 判废，以免 Cue 和拍线一起丢。
  4. 用 5.0.1 导出的库和 5.0.2+ 导出的库各做一个对照：同一 FLAC，只换 `overviewWaveFormData`，确认是渲染器还是分析器写坏了缓冲。

### EOS-04 SC5000 在 5.0.1 后浏览、列表试听、触盘变慢，Wi-Fi 反复断开

- 现象：两台 SC5000 都这样。5.0.1 相对 4.3，Compilation 列表里试听比先加载到盘再听还慢。Wi-Fi 从局域网反复断开。触盘刮盘时曲目像冻住，但 pitch 还能加减。也有用户说自己的 SC5000 在 5.0.1 反而更顺，所以这不是每台必现，但是原帖没有后续说 5.0.4 已好。
- 触发条件：5.0.1 固件；列表预听；Wi-Fi 开着；触盘。楼主准备退回 4.3，因为不用 Stems，RGB 只是锦上添花。
- 底层原因推断（中）：
  - 列表预听和 Wi-Fi、波形生成抢同一个线程。`PlaylistPath` 视图是递归 CTE，排序时对每一行再跑子查询（`5.0.4/m.db.schema.sql` 里的 `PlaylistPath`）。大库上这会堵住 UI。
  - 触盘无响应而 pitch 仍有效，像是主线程在等数据库或波形，音频时钟还在跑。
  - Wi-Fi 栈在库扫描期间被饿死，表现为关联断开，不一定是射频问题。
- 链接与日期：
  - [SC5000 Extremely SLOW after latest firmware update](https://community.enginedj.com/t/sc5000-extremely-slow-after-latest-firmware-update/69462)，2026-06-06 开帖，2026-06-23 最后回复，7 帖，未关闭。楼主写的是 5.0.1。
- 固件 / 数据库层怎么修：
  1. 列表预听走独立解码器，限制优先级低于正在播放的盘，并且不要在预听开始时同步刷新 RGB 波形。
  2. 歌单树不要每次展开都跑 `PlaylistPath`。开库时把 `(id, parentListId, nextListId, position)` 物化成普通表，只在歌单编辑后重建。
  3. Wi-Fi 重连放到独立定时器，库扫描不能取消已关联的连接。
  4. 触盘线程不要等待 librarian 锁。拿不到新波形时继续用上一帧。

### EOS-05 长演出后界面掉帧，转盘显示短冻，声音不断

- 现象：5.0.4，SC Live 4，演出 2–2.5 小时后，切换收藏或在曲目列表和波形之间切换变慢。转盘显示瞬间冻住、掉帧。声音不卡。库已经做过 Library Optimization 和备份，盘是 NVMe SSD。楼主判断是机内软件，不是盘上的数据。SC5000 同为 5.0.4 中心轮/转盘显示设备，同一套 UI 进程。
- 触发条件：5.0.4 连续运行数小时；来回切换 Library 与 Performance；与开机即慢（EOS-04）不同，这是随时间变坏。
- 底层原因推断（中）：波形纹理、封面解码或历史写入在泄漏缓存，主线程每几小时做一次越来越贵的同步。音频线程没被拖死，所以不是 EOS-03 那种解码饿死。
- 链接与日期：
  - [EngineDJ 5.0.4 (SC Live 4) Visually sluggish after long session](https://community.enginedj.com/t/enginedj-5-0-4-sc-live-4-visually-sluggish-after-long-session/70478)，2026-08-23 开帖，2026-10-11 仍有回复，12 帖，未关闭。晚于 5.0.4 发布日。
- 固件 / 数据库层怎么修：
  1. 给波形纹理和封面位图设固定容量的缓存，按时长或曲目数淘汰，不要按「本次开机加载过的全部曲目」保留。
  2. `hm.db` 的 `HistorylistEntity` 追加放到低优先级写线程，每 N 秒一批，不要在切换视图时 `fsync`。
  3. UI 帧预算超过一帧就把当次封面/波形上传推迟，转盘继续画上一帧。这是显示冻结的直接止血。

### EOS-06 部分 FLAC 放到约 2 秒或 15 秒后无声，波形仍在走

- 现象：约 1.2 万曲里至少 3 首 FLAC，只播出约 2 秒就停。桌面端和 Prime 4+ 独立模式都这样。转成 MP3 就正常。另一用户有一首 Bandcamp FLAC 正好放到 15 秒后全无声，波形仍显示在走。不是 5.0.4 已经写进说明的「加载到另一盘时打断当前盘」。
- 触发条件：特定 FLAC 帧布局或元数据；5.x 桌面和解码器；与文件是否在曲库里被分析过无关，波形已经画完。
- 底层原因推断（中）：解码器遇到非子集 FLAC、异常 block size 或内嵌图片帧后把流标为结束，UI 仍按 `Track.length` 走播放头。工作人员 2026-07-19 回复 “it’s also on the list already”，但 5.0.4 和桌面 5.1.0 的公开说明都没有这条。
- 链接与日期：
  - [Engine OS 5.x: Some FLAC files stop playing after about 2 seconds](https://community.enginedj.com/t/engine-os-5-x-some-flac-files-stop-playing-after-about-2-seconds-desktop-standalone/69942)，2026-07-11 开帖，2026-07-19 最后回复，16 帖，未关闭。原帖举例 `lifelike, Kris Menace - What Time Is Love (Live at SSL Mix).flac`。
- 固件 / 数据库层怎么修：
  1. FLAC 解码错误时跳过坏帧并继续，不要把解码器 EOF 当成曲目结束。连续坏帧超过阈值再停，并在日志里记下 frame number。
  2. 忽略 PICTURE / PADDING 元数据块，不要把它们送进音频缓冲。封面走 EOS-09 的独立路径。
  3. 用上述两首能稳定复现的文件做回归。修好的标志是播放时长等于 `Track.length`，而不是「另一盘加载时当前盘不再响」。那是 5.0.4 已经声称修好的另一件事。

### EOS-07 自动循环多圈之后偏离拍线，量化不拉回

- 现象：两首歌拍线已对齐、Sync 打开。A 为主导，B 跟同步。A 上一个已保存的自动循环（例如 8 拍）触发后，循环每多走一圈，相位就越偏，画面上的拍线对不齐。关 Sync、改用 Cue 或 Beat Jump 才能混。有人在 Prime 4、Prime 4+、Prime 4 G2（Engine 5.1.1）、SC6000、Prime GO（Engine 4.1.0）以及桌面端都复现。量化打开时设定的循环也不会在每一圈被拉回。
- 触发条件：已保存的 loop 被重复触发；两盘同时播放且 Sync 开；循环长度本身可以是任意拍数。4.1.0 到 5.1.1 都有，所以不是 5.0 引入后已修的回归。
- 底层原因推断（中高，和本机格式一致）：
  - `beatData` 里的拍线是采样点 `double`，不是整数毫秒。BPM 应由首尾 marker 的采样差计算（见 `Engine-Library-Format.md`）。
  - 循环若用四舍五入后的 BPM 去累加毫秒，每圈会剩一个小数采样。社区回复也指向「BPM 小数位不是 100%」。量化只在设定循环时吸一次，播放中不再按 adjusted beatgrid 重锚。
  - 5.0.0 修的是波形和拍线的绘制对齐，不是循环相位累加。
- 链接与日期：
  - [Auto Loop loses beat grid sync after triggering when playing two tracks](https://community.enginedj.com/t/auto-loop-loses-beat-grid-sync-after-triggering-when-playing-two-tracks/71431)，2026-10-28，11 帖，未关闭。回复明确写这在 Engine 4.1.0 和 5.1.1 上都有，而且报了很多年。
- 固件 / 数据库层怎么修：
  1. 循环终点用采样点计算：`loopEnd = loopStart + beats * samplesPerBeat`，`samplesPerBeat` 来自 adjusted beatgrid 的 marker，不要用显示用的两位小数 BPM。
  2. 每一圈回到起点时，若 Quantize 开着，把播放头吸到最近的拍线采样点，而不是累加上一圈的浮点误差。
  3. `loops` blob 里若只存了毫秒，读出来后立刻换成采样点再参与混音。不要在音频回调里做毫秒到采样的往返。

### EOS-08 Wi-Fi 断开或 TIDAL 中途把已加载曲目弹出

- 现象：Prime 4 连上 Wi-Fi 放 TIDAL，曲目突然被切断，屏幕上的曲目信息消失，像没加载过。一个晚上可以发生一到两次，手机热点信号好时也会发生。2026-10-26 有 Prime GO+ 用户补充：已完整加载的 TIDAL 曲会在任意时刻被弹出，另一盘等待中的 TIDAL 曲也一起被卸掉，TIDAL 源会短时间从设备上消失。换过 Wi-Fi、MiFi、热点，仍随机发生。
- 触发条件：Wi-Fi 流媒体，尤其是 TIDAL；不要求信号真的差。5.0.4 没有这条修复。5.2.0 只在 System One 上修了「加载不可用 TIDAL 曲目会卸掉其他正在播放的 TIDAL 曲」，没有覆盖 SC5000 / Prime 4 / GO+。
- 底层原因推断（中）：断线处理函数把「当前源不可达」实现成「卸载该账号的全部已加载曲并移除源」。播放中的缓冲被当成登录态的一部分一起清掉。
- 链接与日期：
  - [Wifi is unstable, disconnecting, Tidal issue](https://community.enginedj.com/t/wifi-is-unstable-disconnecting-tidal-issue/66680)，2025-11-30 开帖，2026-10-26 仍有新复现，8 帖，未关闭。
- 固件 / 数据库层怎么修：
  1. 传输失败只影响还没下完的缓冲。已经可播的曲目保持在盘上，直到缓冲耗尽再提示。
  2. 不要在一次 socket 错误里移除流媒体源。重连成功后复用原会话。
  3. 把 5.2.0 那条「不可用 TIDAL 曲目不要卸载其他正在播放的 TIDAL 曲」移植到 SC5000 的 5.0.4 代码路径。发行说明已经承认这是缺陷，只是没发给旧硬件。

### EOS-09 5.0 封面迁移之后，盘面不显示封面，或封面引用把播放拖慢

- 现象：5.0.4 当天就有 SC Live 4 用户写盘面上没有封面。5.0.0 起封面不再放在数据库大字段里，而放到独立 Artwork 目录，迁移发生在导入、打包、导出、桌面 Cleanup、以及设备 Folder View 加载时。5.0.3 只修了「高分辨率封面加载到盘上会打断音频」，没有修「封面不出现」。主帖 69065 里，用户用清掉 `m.db` 封面引用的办法换稳定性，说明 5.0 的封面查找会在加载路径上做重活。
- 触发条件：4.x 库升到 5.0；Cleanup 没跑完；`albumArtId` / `albumArt` / `albumArtSourceHash` 与 Artwork 目录不一致；设备只看到旧库、桌面已迁移。
- 底层原因推断（高，schema 可复核）：
  - 5.0.4 的 `Track` 比 4.3.4 多了 `albumArtSourceHash CHAR(40)`。哈希对不上时，如果代码把封面当成缺失并在加载时同步重算哈希或重解码嵌入图片，就会既不显示、又拖慢播放。
  - `AlbumArt` 仍是 `id, hash, albumArt BLOB`，外键 `ON DELETE RESTRICT`。迁移只复制不删除时，库继续带着大 blob；迁移删了行又没更新 `albumArtId` 时，外键或悬空 id 会让加载失败。
  - 桌面 5.1.0 修的是 Re-import 误删封面，管不到设备开库时的查找。
- 链接与日期：
  - 5.0.0 发行说明，2026-05-13，写明封面改存独立目录。
  - [69065 第 364 帖](https://community.enginedj.com/t/engine-os-5-0-x-audio-stutter-and-lag-when-loading-tracks/69065/364)，2026-07-28，Simonster：5.0.4 的 SC Live 4 盘面没有封面。
  - 5.0.3 发行说明，2026-07-06，只修高分辨率封面造成的音频中断。
- 固件 / 数据库层怎么修：
  1. 盘面封面只做显示。找不到文件或哈希不符时显示占位图，禁止在音频加载路径上重算哈希或解码原图。
  2. 查找顺序：Artwork 目录里的 `albumArtSourceHash` 文件，其次 `AlbumArt` blob，其次嵌入标签。任何一步失败都继续播放。
  3. 解码限制在 256 边长以内，放到 UI 线程之外。5.0.3 已经证明大图会打断音频，设备侧应拒绝在热路径解码原尺寸图。
  4. 不要为了修封面去 `UPDATE Track SET albumArtId = NULL` 这种会写脏用户库的办法。那是用户自救，不是固件修复。

### EOS-10 录音约 10 分钟后报 USB 速度不够，换快盘仍出现

- 现象：Prime 4+ 升到 5.0.4 后，录到 USB 大约 10 分钟就停，提示 USB 速度不够。换过 U 盘，也试过播放盘和录音盘分开，仍然如此。SDXC 卡录音可以录完。5.0.4 字符串原文是：`Recording was stopped because drive speed is too slow to record. Please record to another drive.`
- 触发条件：5.0.4 独立模式录音到 USB。同类报错在 2023-01-03 的 Prime GO / Prime 2 上就有，固件过了 1.60 之后出现，所以这是老判断逻辑，5.0.4 把它又触发了。
- 底层原因推断（中）：速度判断用了太短的窗口，或把 UI/库写入造成的一次延迟算进了录音盘吞吐。快闪存也会被误判。SD 槽没走同一条检查，所以 SD 能录完。
- 链接与日期：
  - [Prime 4+ Recording Issue after 5.0.4 Update](https://community.enginedj.com/t/prime-4-recording-issue-after-5-0-4-update/71340)，2026-10-19，2026-10-30 仍有回复，未关闭。
  - [Recording "USB speed too slow"](https://community.enginedj.com/t/recording-usb-speed-too-slow/46315)，2023-01-03，Prime GO 与 Prime 2，1.60 之后。
- 固件 / 数据库层怎么修：
  1. 只有录音环形缓冲持续欠载才停，不要用单次 `stat` 或短于数秒的写入探针。
  2. 欠载阈值用「连续 N 秒低于实时码率」，N 至少覆盖一次 exFAT 的分配延迟。
  3. 停录前先把已写入的 WAV 尾部写完，避免用户以为整段录音无效。
  4. 日志记下实测字节/秒和当时是否同时在写 `hm.db`。如果历史库写入和录音写同一把盘锁，把历史写入挪走。

### EOS-11 响一声之后主输出无声，重启即恢复

- 现象：Prime 4+，Engine OS 5.0.4 独立模式，插着 Rekordbox U 盘。混音约一小时后一声很大的 pop，主输出消失，主 VU 掉到 0。其他控制看起来正常。重启后立刻恢复。发生过两次。
- 触发条件：5.0.4 长时间播放后；不需要 Engine 库，Rekordbox U 盘也会发生。所以不是 EOS-02 的数据库损坏。
- 底层原因推断（低到中）：音频图或 USB 声卡时钟在一次 underrun 后把主总线静音，没有自动重开。重启会重新打开设备，所以一重启就好。5.0.4 修的是加载曲目时的中断，不是这声 pop 之后的永久静音。
- 链接与日期：
  - [Prime 4+ Master Audio Cuts Out After Loud Pop](https://community.enginedj.com/t/prime-4-master-audio-cuts-out-after-loud-pop/70447)，2026-08-20，1 帖，未关闭。
- 固件 / 数据库层怎么修：
  1. 主输出 underrun 或 xrun 之后重新准备播放设备，不要把静音状态留到下次重启。
  2. pop 的来源先查键锁定切换、三盘以上同时播放、以及效果器切入。4.3.0 发行说明曾经修过「3 轨以上罕见 pop/click」和效果器 pop，说明这条路径以前就脆。
  3. 只有一条用户报告，先做成可开关的看门狗，确认不会在正常曲目结尾误重启声卡。

### EOS-12 HFS+ 主库不能试听，exFAT 上的库写入仍然脆弱

- 现象：Mac 上把主库放在 USB 总线的 HFS+ 盘时，Engine DJ 2.x 起就不能把曲目加载到盘面试听、打 Cue。1.x 可以。APFS 盘走雷电可以，同一盘改走 USB 就复现。帖子一直开到 2025-05-05。设备侧固件字符串要求内置盘格式化成 exFAT（`Please format the internal drive exFAT`）。exFAT 又正是 EOS-02 那种「文件都在、只有 Engine 数据库坏」的现场格式。
- 触发条件：桌面端通过 USB 打开 HFS+ 主库；设备或桌面把 SQLite 写在 exFAT 上并在使用中拔出。
- 底层原因推断（中）：桌面把「USB 上的盘」一律当成要交给 Prime 硬件的卷，从而拒绝在 HFS+ 上打开可写音频路径。设备侧则只保证 exFAT 只读浏览，不保证 exFAT 上的 SQLite 事务。
- 链接与日期：
  - [No longer able to have my master library on HFS+ Disk](https://community.enginedj.com/t/no-longer-able-to-have-my-master-library-on-hfs-disk/35731)，2021-10-08 开帖，2025-05-05 最后回复，31 帖，未关闭。5.0 和 5.1 的发行说明都没有提 HFS+。
  - 与 EOS-02 的 71200（2026-10-09，SanDisk Portable SSD）是同一类写入风险。
- 固件 / 数据库层怎么修：
  1. 设备对 HFS+ 保持只读是可以的，但不要在只读卷上创建 `-journal`。检测到只读就用 `immutable=1` 打开，避免挂起。
  2. exFAT 上的数据库写入用 EOS-02 的原子替换，不要在浏览期间持续写 `m.db`。
  3. 大目录不要在插入时 `stat` 每一首再决定是否 corrupt。先读 `m.db`，播放时再校验单个文件是否存在。51261 的开机 2–3 分钟很像全盘扫描。

### EOS-13 桌面 5.0.0 的 Sync Manager 不在 USB / SD 上重建歌单目录

- 现象：Engine DJ Desktop 5.0.0，播放器 Engine OS 5.0.3。约 26567 曲。从 4.3.x 升到 5.0.0 后，同步到 U 盘和 SD 卡时音频文件会拷上，但 Sync Manager 右侧的歌单目录是空的，关掉再开也是空的。内置 SSD 正常。
- 触发条件：目标是 USB 闪存或 SD，不是 SSD；桌面 5.0.0 的 Sync Manager；曲目属于多个歌单时更容易看出来。5.0.4 设备发行说明和桌面 5.1.0 说明都没有写这条。
- 底层原因推断（中）：5.0 同时改了封面目录和歌单导出。USB/SD 的导出路径只写了音频和 `m.db`，没有写桌面用来显示的歌单文件夹镜像；SSD 路径仍写了。设备若只认 `m.db` 链表，文件夹空不一定等于设备上没歌，但用户看到的就是「歌单没了」。应两边都修：桌面把镜像写全，设备在镜像缺失时仍从 `Playlist`/`PlaylistEntity` 列出曲目。
- 链接与日期：
  - [Engine DJ 5.0.0 — Sync Manager does not create playlist folder structure on USB / SD](https://community.enginedj.com/t/engine-dj-5-0-0-sync-manager-does-not-create-playlist-folder-structure-on-usb-sd-drives-works-on-ssd/70166)，2026-07-27 开帖，2026-08-16 最后回复，16 帖，未关闭。
- 固件 / 数据库层怎么修：
  1. 设备浏览不要依赖导出的文件夹树。`Playlist` + `nextListId` + `PlaylistEntity` + `nextEntityId` 才是顺序的来源。
  2. 文件夹缺失时仍显示数据库里的歌单，并在日志里记「mirror missing」，不要显示空源。
  3. 若固件自己也会把播放列表写回 U 盘，写入时要同时维护链表和文件夹，并且整次写入放在一个事务里。写了一半就拔盘，就是 EOS-01。

### EOS-14 历史库膨胀把 SC5000 拖到冻结

- 现象：Engine DJ 5.0.1，SC5000 + SC6000M，约 6700 曲，Engine Library 约 1.91 GB。插入 U 盘后 Sync Manager 要大约 18 分钟才打开。SC5000 在高强度双层使用中越来越慢，最后屏幕冻住，要重启。帖子后来被标成已解决，解决方式是社区协助清理，不是 5.0.2 之后的固件说明。5.0.2 只修了「看完 History 后曲目列表不刷新」。
- 触发条件：多年份的 `hm.db`，里面有大量重复历史列表；5.0.1 的同步和设备浏览会读这张库。
- 底层原因推断（中）：`hm.db` 里有完整的 `Track`、`PerformanceData`、`Playlist`、`PlaylistEntity`，另外还有 `Historylist` / `HistorylistEntity`。设备若在开机或每次插入时把历史和主库做一次匹配，行数上去之后主线程会冻住。清理历史能好转，说明缺的是设备侧的上限和后台化，不是某一首曲目的解码。
- 链接与日期：
  - [Engine DJ 5.0.1 – Severe Sync Manager Slowdown, Library Performance Issues & SC5000 Freezes](https://community.enginedj.com/t/bug-report-engine-dj-5-0-1-severe-sync-manager-slowdown-library-performance-issues-sc5000-freezes/69200)，2026-05-20。2026-05-25 管理员写发帖人已靠他人协助解决。这不能当成固件已修。
  - 同周还有 [SC Live 4 Engine 5.0.1 Random Freeze](https://community.enginedj.com/t/sc-live-4-engine-5-0-1-random-freeze/69257)，2026-05-24，5 帖，未关闭。5.0.2–5.0.4 没有「随机冻结」这一条。
- 固件 / 数据库层怎么修：
  1. 播放和浏览只打开 `m.db`。`hm.db` 延后打开，并且只追加当前场次。
  2. 历史会话设上限，或只保留最近 N 场。超出的行由后台任务删除，不在 UI 线程做。
  3. 插入驱动器时不要为了 Sync 先把全部 `HistorylistEntity` 拉进内存。

### EOS-15 流媒体音质不一致导致 SC5000 功能异常

- 现象：2023-09 的 SC5000 帖把 “streaming quality mismatch” 和冻结、假 corrupt、慢加载写在一起。SC5000 出现，Prime GO 不出现。原帖没有把对话框全文贴进文字，后续回复猜测是两台机器的流媒体音质档位不一致，导致 instant double 一类功能拒绝执行。
- 触发条件：SC5000 使用流媒体；与另一台 Engine 设备的音质设置不同。5.0.x 发行说明没有这条。
- 底层原因推断（低）：设备把可播放的音质档位存在本地设置里，Link 或 instant double 时要求两边档位相同，不一致就弹错并中止，而不是降到两边都支持的档位。证据只有一个帖子标题和一张没摘录下来的截图，修的时候要先复现对话框。
- 链接与日期：
  - [51261](https://community.enginedj.com/t/sc5000-issues-freezing-slow-database-loading-streaming-quality-mismatch-issue-corrupt-databases/51261)，2023-09-08。
- 固件 / 数据库层怎么修：
  1. 音质不一致时自动选两边都有的最高档，只在确实没有共同档位时才报错。
  2. 这个报错不能走 EOS-02 的 corrupt 对话框。

## 建议的修复顺序

给下一轮固件修改用。每一项都先做能在本机 schema 或字符串上对上的部分，再谈音频回归。

1. EOS-01、EOS-02、EOS-13：开库校验和修复链表；corrupt 对话框不要清空歌单；USB/SD 没有文件夹镜像时仍读 `m.db`。
2. EOS-03、EOS-09：波形解压失败要回退；封面失败只影响显示。
3. EOS-06：FLAC 坏帧跳过，用帖里的曲目做回归。
4. EOS-04、EOS-05、EOS-14：浏览和历史移出主线程，长运行要有缓存上限。
5. EOS-07：循环用采样点重锚。
6. EOS-08、EOS-10、EOS-11：断线不卸已加载曲；录音误报速度；主输出 xrun 后重开设备。
7. EOS-12、EOS-15：只读文件系统和流媒体档位。证据比前几项弱，放在后面。

## 本机可复核的锚点

不要改这些文件来「修 bug」。它们只是对照。

- `5.0.4/m.db.schema.sql` 与 `4.3.4/m.db.schema.sql`：`PlaylistEntity.nextEntityId`、删除触发器的 `trackId > 0` 条件、`PlaylistPath` 视图、`Track.albumArtSourceHash` 只在 5.0.4 出现。
- `5.0.4/hm.db.schema.sql`：`Historylist`、`HistorylistEntity`，以及另一份同样的 `PlaylistEntity`。
- `5.0.4/Engine.strings`：`Corrupt Database:`、`Device's database is corrupted`、`Recording was stopped because drive speed is too slow to record.`、`Please format the internal drive exFAT`、`WaveformBuilder.cpp`、`WaveformOverviewRGB*.frag.qsb`。
- `Engine-Library-Format.md`：`beatData` 用采样点 `double`；`quickCues` 的位置也是采样点 `double`。修 EOS-07 时不要改成毫秒累加。

## 证据边界

- 帖子标题、创建日、最后回复日、回复数来自 `community.enginedj.com` 的 topic JSON，抓取日 2026-11-02。
- 发行说明来自 `https://enginedj.com/downloads` 页面内嵌的 `engineOsReleasesCollection`，同一天抓取。
- 没有把 341 帖的音频主帖逐帖读完。5.0.4 之后若只在该主帖的后半段出现、而发行说明和独立帖都没提到的症状，本清单不收录。
- 搜索摘要里「Engine 用毫秒存拍线」与本机 `beatData` 格式相反，已丢弃。
- 5.1.x / 5.2.0 的修复不要写进 SC5000 的 5.0.4 已修列表。那些包不下发到 SC5000。
