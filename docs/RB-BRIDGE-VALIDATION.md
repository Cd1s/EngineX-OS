# rb-bridge 本地验证记录

环境：IN，Go 1.27.1，linux/amd64。范围为本工作树内的软件和构造 fixture；
没有连接、刷写或在 SC5000 实机上运行。

已通过：

- `go test ./... -v`：5 个核心/集成包通过，31 个 Test、2 个 Fuzz seed 组。
  集成 fixture 包含 DeviceSQL 非数据页、多页/多 row-group、被删除的旧 offset、
  DAT/EXT/2EX、NFD 文件名、智能/同名嵌套歌单及真实 OneLibrary 列名。
- `go test -race ./...`：通过，包含挂载 watcher、发现状态缓存与数据库集成。
- `FuzzAnalysis -fuzztime=3s -parallel=2`：通过。
- `FuzzPDB -fuzztime=3s -parallel=2`：通过，含页边界修正后的复查。
- `GOOS=linux GOARCH=arm GOARM=7 CGO_ENABLED=0 go build -o bin/rb-bridge-armv7 ./cmd/rb-bridge`：通过。
- `file bin/rb-bridge-armv7`：`ELF 32-bit LSB executable, ARM, EABI5 version 1 (SYSV), statically linked`。

关键断言包括：PIONEER 与音频逐文件 SHA-256 不变；Engine-only 介质不产生写入；
空 Hot Cue A 保持 -1，原 B 名称/颜色/位置与 H Loop 保留；Memory Cue / Loop
完整源信息可从 BridgeAnalysis 取回；qCompress 长度、Beatgrid、波形尺寸正确；
无变化第二次导入数据库文件逐字节相同；音频、ANLZ、PDB 记录分别变化时仅更新
对应曲目；推荐结果随源 BPM 变化刷新；嵌套链表和外键完整；错误事务回滚；
原生 Engine 路径冲突不会被接管，原生子歌单不会被级联删除；输出拒绝链接重定向。
复核增加：页内删除空洞、远偏移专辑、PWV4 全长/空预览、大小写 PDB、软链挂载根、
仅 ANLZ 中的 BPM 首导/增量推荐、曲目改名/互换身份稳定、源 WAL/SHM 不写、
混用盘初始化失败回滚至原数据库字节、同起点不同终点的 Memory Loop 均保留。

不同模型家族复核：查IN（Gemini），agent
`bba5fc4a-e490-4279-b27b-edf21dcdbdd5`。首轮指出 7 类问题并逐项修复，
有限复核确认全部修复到位，未发现新的确定阻断 Bug。审查未代替执行测试或实机验收。
Sonnet 审查入口被提供方 refusal_guard 拒绝，未形成审查结果；该 agent 已归档。

这些检查不替代原厂实机验收。Memory-bank 全部 UI 展示、原生 Pro DJ Link
dbserver/NFS 取库、外部高分辨率缓存、通道映射和真实介质上的秒读时间尚未验收；
具体范围见 [使用说明](RB-BRIDGE.md)。

实施前远端备份 ID：
`20261101T172933Z-hostdzire-in-16c32g-rb-bridge-before-implementation-179a7248b7b5-rb-bridge.tar.gz`。
