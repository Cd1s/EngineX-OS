# rb-bridge

Go 实现的只读 Rekordbox 导出解析与 Engine OS 5.0.4 导入器。生成目标是
`Engine Library/Database2/m.db`，使用研究报告的 schema 3.0.2、原厂触发器和
qCompress 格式。不会打开 `PIONEER/` 或音频文件进行写入，不安装服务、不修改固件。

## 使用

```sh
go build -o bin/sc5000-suite ./cmd/sc5000-suite
bin/sc5000-suite rb-bridge --mount-path /media/usb
bin/sc5000-suite rb-bridge --mount-path /media/usb --smart-match 123
bin/sc5000-suite rb-bridge --prolink --bind 0.0.0.0
```

`--smart-match` 参数是参考曲目的 **Rekordbox ID**；默认关闭。推荐列表在根歌单树
末尾追加，以 BPM 差值 ≤ 4、同一 Camelot 编号的大小调或同模式相邻编号筛选。
不会自动分配 Hot Cue；参考 ID 无效或原有根歌单已叫 `[Smart Match]` 时拒绝写入。

单次转换输出 JSON（Added / Updated / Unchanged / PlaylistsChanged / Bypassed），
错误输出到 stderr，退出码为 1。SIGINT / SIGTERM 取消守护进程。
`--mount-path` 先只看 `export.pdb` 的大小和修改时间：与 `Database2/.rb-bridge-stamp` 记录的
上次导入一致且 `m.db` 非空时直接旁路（实机 5966 首的 U 盘约 30 ms），不读导出库、不碰音频文件。
`--watch` 已停用，只打印一行就退出：它在 Engine 打开数据库时反复写入（`SQLITE_BUSY`，
真机上还出现过损坏）。导入由 edisksd 挂载 hook 在 Engine 接管前完成。碟机只读 `/etc`
里的旧单元仍带 `--watch --prolink`，所以这个参数保留为立即退出，而不是报错重启。
不负责挂载文件系统；APFS/HFS+ 驱动、只读盘 overlay 均在本工具范围外。

## 数据与兼容性

| 输入/输出 | 实现行为 |
| --- | --- |
| DeviceSQL export.pdb | 按表页链、数据页标志、13 位 row-offset 计数、36 字节 row-group、live bitmap 读取；拒绝截断、越界、页链环与歌单环 |
| Track / lookup | 解析标题、路径、分析路径、艺术家、专辑、流派、Label、Key、Artwork；Columns 的未知行保持原始字节 |
| PlaylistTree / Entries | 按 ID/ParentID 建树，按源 sequence 排序，保留同名的不同父目录子歌单；导出的智能歌单保留已物化成员 |
| OneLibrary SQLite | 支持 content_id、bpmx100、samplingRate、analysisDataFilePath、playlist_id_parent、sequenceNo 等 OneLibrary 字段及常见数字 ID 的 djmd 导出字段；immutable 只读打开，拒绝未 checkpoint 的 WAL/journal |
| PQTZ | 读取全部时间戳、拍号与 BPM；编码为 Engine default / adjusted beat grids，毫秒按音频采样率换算样本位置 |
| PCOB / PCO2 / PCP2 | 合并 DAT/EXT/2EX 的普通、扩展 Cue；扩展记录优先，保留名称、颜色、Loop 边界、量化分数与 Active 标志 |
| PWV3 / PWV5 / PWV7 | 提取单色、RGB、三频段样本；优先 PWV7、PWV5，保持峰值缩为 1024-bin overview，原始各版本仍保留 |
| PSSI | 读取 mood / bank / phrase 元数据，支持带 XOR mask 的 body；绝不用段落自动生成 Cue |
| NFC / NFD | 逐目录建立懒加载的 NFC + Unicode case-fold 索引；精确字节匹配优先，模糊匹配多义时报错；写入磁盘上的真实拼写 |
| WAV / FLAC | 只读 chunk / STREAMINFO；支持 WAVEFORMATEXTENSIBLE、RF64 ds64，区分容器位宽和 valid bits；跳过大型 FLAC 图片，无需解码音频 |

源库不存在（包括只有原生 Engine Library 的介质）时直接旁路，连锁文件也不创建。
路径拒绝 `..`、符号链接、NUL 和驱动器前缀；输出数据库、journal、WAL、SHM
拒绝符号链接和硬链接，避免 Engine Library 被重定向到 Pioneer 源文件。
这里假定挂载树不会被另一个进程恶意并发替换；这不是不可信多用户目录的沙箱。

## Engine 写入与增量

- 工厂 DDL 从 `docs/re-report/ENGINE-5.0.4-m.db-schema.sql` 嵌入，只移除 SQLite
  自行创建的 sqlite_sequence DDL。已有库仅接受 3.0.2，不自动迁移其他版本。
- Track 的 INSERT 让工厂触发器创建 PerformanceData，随后 **UPDATE** 二进制列。
  路径相对于 Engine Library，例如 `../MUSIC/Café.wav`，不是相对于 Database2。
- trackData 为 44 字节；overviewWaveFormData 为 3099 字节；beatData 使用报告的
  混合字节序。trackData / overview / beatData / quickCues 有 4 字节 BE 长度 + zlib；
  loops 不压缩。只有同时具备拍线和波形时设置 isAnalyzed。
- `Bridge*` 表都在 `Database2/.rb-bridge-state.db`，不在 `m.db`：Engine attach 时会比对 schema，并删除它不认识的表。
- BridgeState 记录实际路径、文件 size / mtime、PDB 原行与解析元数据哈希、分析文件
  和封面的 size / mtime。未变曲目不读取 ANLZ 内容或音频头，不更新 PerformanceData。
  分析文件单独发生变化也会更新其曲目。保留时间戳且等长的外部修改不属于此检测模型。
- 源路径、size/mtime 与分析文件预检在创建输出目录前完成。Schema 初始化、Track、
  PerformanceData、歌单更新在同一事务内；同盘 flock 防止两个桥接器并发。
  错误连同桥接表初始化一起回滚；首次失败可能留下空数据库文件与锁文件。
- 已拥有曲目的 Rekordbox ID 保持稳定时，改名/移动会保留原 Engine Track ID；
  多曲目互换路径先在同一事务暂存再更新，不留下旧路径幽灵行。
- PlaylistEntity 使用 nextEntityId 单向链表，尾部为 0；重复成员按 Engine 原生唯一
  约束保留第一次出现，完整源成员序列另存。歌单结构或推荐结果未变化时不重建歌单。
- 只管理有 BridgeState / BridgePlaylist 所有权的记录；原生路径/歌单名称冲突时报错。
  如果导入文件夹中出现原生 Engine 子歌单，拒绝可能级联删除它的重建。
  源中移除的曲目不自动删除 Engine Track 行，避免删除播放端编辑；源歌单成员会同步。
- BridgeAnalysis 保留完整曲目、所有源 Cue/Loop、各版波形、段落与音频头元数据。
  BridgeLibrary 保留源歌单属性/规则、Columns 及歌单同步指纹。
  无 RGB 扩展的旧 Cue 使用 Pioneer 隐式默认色；显式 RGB 优先，源颜色代码另存。

## 原厂显示与局域网边界

Engine 原生 quickCues 只支持 8 槽（A–H），空槽位置为 -1。这些槽只写源 Hot Cue，
绝不拿 Memory Cue、段落或智能匹配填入 A/B。第一个源 Memory Cue 映射为 main cue；
全部 Memory Cues、完整 UTF-8 名称和颜色另保存在 BridgeAnalysis。Engine label 只有
255 字节；超长标签的原文保留在扩展表。8 个原生 Loop 槽优先保持 Hot Loop 的槽位，
再填入 Memory Loops，多余 Loop 也保留扩展表。**扩展表的全部 Memory Cues / Loops
并不等于原厂 UI 能全部显示**；尚无该原厂界面的无损 Memory-bank 映射证据。

没有 SC5000 实机/QEngine 验收数据。5.0.4 报告中 trackData 尾部三项、overview
通道顺序、beatData 扩展尾部仍有未知语义，本实现以零值/已知布局写入；不能据此
宣称原厂不重扫、滚动波形颜色完全一致，或任意容量真实 U 盘 1–2 秒完成。
完整高分辨率源波形留在 BridgeAnalysis；未实现外部 OverviewData/*.rgb 缓存生成。

`--prolink` 被动监听 UDP 50000 的 announcements 与 50002 的 player status，输出
设备名、地址、播放源设备/槽位和 track ID；不发送包、不抢占播放器编号。
`prolinkShare`（`settings.conf` 的 `prolink_share=1`）允许从 CDJ 的 NFSv2 导出读取曲库。
发现 Rekordbox 不代表已经读取其电脑收藏库；加密 OneLibrary 解密仍不支持。

### 反向导出到 CDJ（#63，2026-11-05）

`stream-sync` 的 **Pro DJ Link Export to CDJ (Beta)** 开关默认关闭。关闭时导出组件
没有监听 socket、公告、goroutine 或定时器；原有被动发现继续独立工作。开启前先挂载
含 `PIONEER/rekordbox/export.pdb` 的 U 盘；多个盘选择 `/media` 下排序后的第一个。
设置可用 `POST /v1/sc5000/ui?prolinkExport=1` 或
`POST /v1/sc5000/prolink?export=1` 修改，也可在启动前写入 `prolink_export=1`。
改为 `0` 会同步关闭全部导出连接。`GET /v1/sc5000/prolink` 的 `export` 是保存的意愿，
`exportState` 报告实际监听地址和编号；启动失败不会阻止普通控制台启动。

开启后提供只读 UDP portmapper/MOUNTv1/NFSv2（`/C/`），TCP 12523 的端口查询、
dbserver 握手、Root/Track/Playlist 菜单、分页和曲目文件路径；ANLZ 路径从 NFS 上的
export.pdb 获取。先监听 2.5 秒，
再选择未占用的编号 4→3→2→1；同号外部公告会立即让号，四个编号全占用时停止公告。
USB 媒体详情在 UDP 50002 广播并响应查询，并向已发现的 peer 单播空闲状态。被动发现与导出共享广播端口，目录访问
固定在打开的 USB root 内，拒绝写入、越界及符号链接。移除或改变 export.pdb 后停止
公告和旧媒体读取；更换盘后需关闭再开启，以重新读取曲库。

协议依据：[Deep Symmetry dbserver](https://djl-analysis.deepsymmetry.org/djl-analysis/track_metadata.html)、
[菜单](https://djl-analysis.deepsymmetry.org/djl-analysis/menus.html)、
[媒体详情](https://djl-analysis.deepsymmetry.org/djl-analysis/media.html)。
这仍是实验功能：环回测试覆盖真实 TCP/UDP、编号冲突及音频/ANLZ 的 NFS 读取，
不能替代真实 CDJ 的浏览、加载和播放验收。逐字节抓包证据与机型限制见
[协议核对记录](PROLINK-EXPORT-WIRE.md)。未实现其他 dbserver 菜单、波形/封面二进制
查询、排序选项或 Pioneer 的完整编号协商。
没有本地 Rekordbox export 的空盘或纯 Engine 盘，也能导入 `.prolink-cache/` 的暂存 CDJ 曲库，并保留 Native 曲目。
SQLiteAdapter 可替换版本相关的解析逻辑，无法识别/加密的库明确报错，不改写源库。
只读 SQLite 使用 immutable，防止 `mode=ro` 仍创建/修改 SHM；有非空 WAL/journal
的活动库必须先取得正常 checkpoint 的导出快照，工具不会操作源 journal。

## 验证

```sh
go test ./... -v
GOOS=linux GOARCH=arm GOARM=7 CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o bin/sc5000-suite ./cmd/sc5000-suite
file bin/sc5000-suite
```

端到端 fixture 独立构造 DeviceSQL 数据/非数据页、DAT / EXT / 2EX、NFD 日文/韩文/
重音文件名、Extensible WAV、大封面 FLAC、实际 OneLibrary 字段、嵌套同名歌单与
智能歌单。断言源哈希不变、Hot Cue A 未设置且原 B/H 保留、Memory 元数据不丢失、
双 Beatgrid、波形长度、链表、三类增量变化、推荐边界与刷新、无变化数据库字节不变、
事务回滚、原生库旁路、原生子歌单保护及输出重定向拒绝。另含 UDP fixture 和 fuzz seeds。
复核回归还包含页内删除空洞、远偏移专辑、PWV4 全长/空预览、大写 PDB、改名/路径
互换、仅 ANLZ 中的 BPM 首次与增量推荐、Memory Loop 不同终点及反向 Active 合并。

格式参考（不将旧版 schema 当作 5.0.4 DDL）：

- [本仓库 5.0.4 研究报告](re-report/ENGINE-5.0.4-ANALYSIS.md)
- [Deep Symmetry DeviceSQL / ANLZ definitions](https://github.com/Deep-Symmetry/crate-digger/tree/main/src/main/kaitai)
- [Engine Library path reference](https://github.com/mixxxdj/mixxx/wiki/Engine-Library-Format)
- [libdjinterop key enum / OneLibrary schema](https://github.com/xsco/libdjinterop)

## 挂载时的速度

`rb-bridge --mount-path` 在 `export.pdb` 的大小和修改时间没变、`m.db` 仍在时立即跳过（`Database2/.rb-bridge-stamp`）。

大曲库（超过 500 首）第一次导入时走快速模式：只读 `export.pdb`，在 tmpfs 上建 `m.db` 再拷回。没有热点、循环、波形和
封面，Engine 载入曲目时自己分析。完整导入用 `rb-bridge --mount-path <盘> --full`，在 FAT32 U 盘上 6000 首约 8 分钟。

带 rekordbox 导出的盘挂载时不再运行 `engine-guardian --heal-mount`（6000 首约 15 秒），只有纯 Engine 盘才运行。
这类盘因此失去挂载时的自动修复；数据库出问题时，手动运行 `engine-guardian --heal-mount <盘> --data-only`。

`export.pdb` 变了而曲库已存在时走增量同步，要逐个检查文件，6000 首约 24 秒。这超过挂载钩子 20 秒的预算，所以会被
中止并回滚，旧库保持不变，需要手动运行 `rb-bridge --mount-path <盘>`。
