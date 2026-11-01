# SC5000 5.0.4：REA 二进制证据报告

**状态：已收尾。五个目标均取得真实 REA 静态产物；Engine 为精选覆盖。模块 mask 消费语义、schema 数组边界、rbp 实体 schema、USBANLZ 和 JP07 精确映射等未闭合项明确保留为未知。**

原始证据位于 `/root/build/sc5000-lab/rea-results`。本仓附 [完整归一化文本与索引](rea-evidence/README.md)，保留成功结果、参数、Evidence ID和原始文件SHA-256；长日志、原始信封、snapshot和厂商二进制留在lab。本文的 `NNNN-*.evidence.json` 引用按索引的 `source_file` 定位。

## 工具、身份与输入

- 源码仓库：`morluto/rea`，commit `1d343a88e53af51101a408d437641c60b7bb34e4`，package 6.8.0；npm 实际查询的 latest 同为 6.8.0。本次执行的是该 npm 发布包，而非声称已构建仓库。
- 实际运行：lab 内 Node 22.23.4、rea-agents 6.8.0、Ghidra 12.1.4、Temurin 完整 JDK 21.0.13+9。工具包均位于 `../rea-tools/`；Ghidra／JDK 下载分别与官方发布元数据 SHA-256 比对成功。
- 主机 Node 22.23.2 补丁版本不足；主机 Java 21 是 runtime-only，首次 REA 返回 `provider_unavailable`，见 `0004-open_binary.json`。随后只在 lab 内补齐工具。没有运行 `rea setup`，没有修改系统或用户级配置。
- Agent `f2b652a9-dc5f-4c7d-b398-a5747e50d61e`；workspace `wks_47ec13a6655df8ca`；Codex session `01a19804-d418-7270-8276-22f1e2bc6073`；cwd `/root/build/sc5000-lab/rea`。没有派生子 agent。
- 所有分析是静态的。没有执行厂商 ARM 程序、操作 bootloader／MCU、修改输入、接触密钥或提交代码。

| 输入（相对 `5.0.4/tree/usr/`） | SHA-256 |
|---|---|
| `Engine/Engine` | `e1eb0e2b858880016daa3ad14538426204f8e698d855a274aebc924a5f9a5caa` |
| `Engine/OfflineAnalyzer` | `fb4871f11f5969e7dea848192735aed8c29d021aa6ff187ee4e4144113597bab` |
| `Engine/MidiDeviceScanner` | `b65c82063a7c414be7b06f5160488426215badff32cdedea404939a3f03a63ea` |
| `lib/libaz01-prog.so.1.3` | `5cac6b2aa1f6f748d38b1439275667227b5f8f806b06320e63ea1bd507b80dbf` |
| `lib/libaz0x-info.so.0.8` | `62b88d1d0694dc1bdde68f2a2ca714ab0ca044f0ba17c6e006ba9163d4e6acc2` |

精确启动及调用日志见 [COMMANDS.md](rea-evidence/COMMANDS.md)、`mcp-transcript.jsonl` 和 `offline/mcp-transcript.jsonl`。原始 MCP 信封是 `NNNN-<tool>.json`；完整 REA Evidence 是对应 `*.evidence.json`。`EVIDENCE-INDEX.json` 关联操作、参数、目标 digest、Evidence ID、限制及可读伪代码。所有完整输出保留在磁盘，屏幕只显示有界预览。

## 地址与证据等级

下文地址默认是 **Ghidra 分析 VA**，不是文件偏移、运行时地址或厂商符号表 RVA。已取得的 Ghidra `binary_overview` 明确使用 `image_base=0x10000`。这些库的原始 ELF VA 对照为 `Ghidra VA - 0x10000`；例如 `0x1738c` 对应原始 ELF VA `0x738c`。不能把该减法未经 LOAD 映射验证就推广成任意位置的文件偏移转换。PIE 的实际运行基址与 ASLR 未观测。

- **确证（静态观察）**：真实 REA/Ghidra 返回的字符串、符号、引用、字节、函数反编译与直接调用边；仍受 provider 覆盖限制。
- **派生／推断**：依据这些记录解释字段、ops 分派或命名；保留原始地址及计算方法。
- **未知**：没有运行时证据、自动分析未识别、间接调用未解析、未找到实体样本等。没有将未知写成不存在。

`FUN_...`、Ghidra 的类型、变量名和伪代码都是恢复结果，不是原始源码。函数种子只声明待分析入口，不证明原始函数名或签名。

## libaz01-prog：wire1、v2/v3 ops、签名枚举

### 魔数与版本分派（已确证）

`az01_open_image`：Ghidra `0x1738c`／原 ELF VA `0x738c`，先 `az01_map_file`，再按输入字节选择 ops：

| 输入前四字节 | 选择的表 | Ghidra 表地址 | 原 ELF VA |
|---|---|---|---|
| `AZ01` | `az01_image_v2_ops` | `0x22ae8` | `0x12ae8` |
| `AZ0x` | `az01_image_v3_ops` | `0x22b18` | `0x12b18` |
| `d0 0d fe ed` | `az01_image_fdt_ops` | `0x22ab8` | `0x12ab8` |

来源：`0033-procedure_pseudo_code.evidence.json`（`ev_c0ec451c1ec85843d2709b9737f737ce32e3a182c263bf30093b02c12adfa6ac`）、`0041-list_names.evidence.json`、`0043/0044/0045-read_bytes.evidence.json`。`0039-procedure_callees.evidence.json` 是该 wrapper 的真实直接 callee 集；通过函数指针调用 ops 的行为另由伪代码说明。

两套 parser 都要求磁盘第二个 LE32 字段为 **1**：v2 `FUN_00018ccc` 检查 magic `0x31305a41`（`AZ01`），v3 `FUN_00019538` 检查 `0x78305a41`（`AZ0x`）。来源 `0047`／`0051-procedure_pseudo_code.evidence.json`。因此父任务所称 **wire1 的 AZ01 容器映射到实现名 v2 ops**；磁盘版本字段 1 和实现名 v2/v3 是不同维度。二进制中未观察到字面名称 `wire1`，该名字是对接命名。

v2 parser 可见 NUL 结尾字符串、4 字节长度／对齐处理和最终 8 字节对齐。v3 parser 可见多组 offset/count 的范围检查，以及 `8`、`0x40`、`0x108` 的记录跨度。字段含义要按实际消费函数恢复，不凭恢复变量名确定。

### 签名枚举空项（已确证）

`az01_image_for_each_signature`：Ghidra `0x1779c`／原 ELF VA `0x779c`：

```c
if (*(code **)(*param_1 + 0x2c) != 0)
    return (**(code **)(*param_1 + 0x2c))();
return 0xfffffff5;  // signed -11
```

完整真实 REA CLI Evidence：`libaz01-signature-wrapper.cli.json`，ID `ev_fbd313a9e32c3c654f9c01aa838f885f4cd82d854dc82ab772ecb1318fe50988`。

`0043-read_bytes.evidence.json` 观察的 v2 ops 48 字节在 `+0x2c` 为 **0**；v3 的同槽为 `0x19914`（`0044`）。因此 v2 的签名枚举入口为空，调用 wrapper 返回 `-11`。这不等于“所有验证都跳过”，也不建立固件启动或设备侧接受行为。

### 验证路径及带种子分析

`az01_verify_image`：Ghidra `0x17554`／原 ELF VA `0x7554`，访问 ops `+8` 的间接 verifier，并在成功后记录验证状态。初次 `procedure_callees` 返回空集，不能据此宣称不调用其他函数。

从 REA 读出的三张 ops 表解码 LE32 指针，生成 `libaz01-seeds.tsv`（28 个去重函数入口、没有自造厂商符号名）。普通分析没有识别的 verifier 随后在 **REA 官方种子工作流**中成功恢复。种子来源和哈希在带种子 Evidence 的 analysis profile／limitations 中保留。前后两套 bundle/snapshot 均保存。

| verifier | ops 槽来源 | 关键恢复行为 | 真实 callee Evidence |
|---|---|---|---|
| v2 `0x18eb8`（ELF `0x8eb8`） | `v2_ops+8` | 解析 `PART`／`EOF`、检查 `sha1`、分块 hash、比较摘要 | `0062-procedure_callees.evidence.json` |
| v3 `0x19d5c`（ELF `0x9d5c`） | `v3_ops+8` | 逐项 hash、比较 32B 摘要；经 key-provider callback 取得公钥后调用验签 | `0064-procedure_callees.evidence.json` |

伪代码分别见 `0061`、`0063-procedure_pseudo_code.evidence.json`。v3 观察到直接边 `0x19d5c → 0x12bb4`，后者是 `az01_verify_signature` 的 PLT thunk；其实现 `0x1f094` 的 REA 伪代码 `0046` 显示 `EVP_sha256`、`EVP_DigestVerifyInit` 和 `EVP_DigestVerify(..., signature_length=0x100, ...)`。没有读取或输出任何密钥材料。

## libaz0x-info：平台与设备树

完整函数清单 `0008-list_procedures.evidence.json`：206 项（包含 EXTERNAL 和 PLT／thunk，不能称作 206 个本地实现）。关键已恢复符号：`az0x_info_get 0x12470`、`az0x_info_datum_at 0x1243c`、`az0x_info_datum_key 0x12458`、`az0x_info_datum_value 0x12460`、`az0x_info_datum_category 0x12468`、`az0x_info_free 0x125bc`。

`az0x_info_get` 扫描 `__start_platforms`／`__stop_platforms` 的 callback 表选择平台。其 `procedure_callees`（`0025`）包含直接边 `0x12470 → 0x13ad8`、`0x12470 → 0x13448`；前者的伪代码在 `0021`。

`FUN_00013ad8` 静态观察：

1. 检查 `/sys/firmware/devicetree/base/inmusic,az01-pcb-rev` 或 `inmusic,az05-pcb-rev` 是否存在。
2. 否则打开 `/sys/firmware/devicetree/base/compatible`，最多读 `0x80` 字节，`memmem` 匹配 `inmusic,az01`。

对应字符串 xref：`0x14348 → 0x13b88`、`0x14374 → 0x13b8c`（`0017/0018-xrefs.evidence.json`）；PCB 字符串也有真实引用（`0020`）。设备树可被此库用作平台判定输入是确证事实；具体 JP07／SC5000 产品名映射仍需 Engine 或实体设备数据支持。

还观察到 `serial-number`、`chosen/inmusic,internal-sd-fitted`、`chosen/u-boot,version`、`/etc/az01-release`／`/etc/az0x-release`、efuse、Wi-Fi vendor/device 等路径。它们是静态路径及相应消费函数，未实际读取设备。

## MidiDeviceScanner

真实 REA 函数清单 `0074`：567 项；`0078-search_strings.evidence.json` 是本次 provider 识别到的完整字符串集合。针对 JP07、SC5000、Denon、MIDI、model、device tree、compatible 的检索只返回 `amidi`，这仅是该 provider 定义字符串范围内的阴性结果，不证明程序绝无相关行为。

`amidi`（Ghidra 字符串 `0x260e4`）的 xref 为 `0x149d0`、`0x14aa4`、`0x14ab0`（`0080`）。入口 `0x16e90` 经 `__libc_start_main` 传入 `LAB_00014168`（`0079`）；该 main 指针初始没有被 provider 作为独立函数列出，未伪造 main 的函数边。

独立 REA CLI 反编译 `FUN_0001dba8`，Evidence `MidiDeviceScanner-amidi-parser.cli.json`（`ev_d86dee359e803bcc05640c7fab3c6edb417bc9ea9a317822d1ce6fcba88915b6`），可见 Qt 正则解析 `client ... [type=user,pid=...]`、`Device:`、`in use by` 和端口数字格式。结合 xref 可确证存在文本 MIDI 客户端／设备清单解析器；没有运行 `amidi` 或该程序。

## OfflineAnalyzer：入口、分析链、输出序列化

### 分析覆盖与入口

默认 Ghidra 清单 3,198 项；默认分析完成耗时 306 秒，其中 `Decompiler Switch Analysis` 267.508 秒，见 `offline/default-ghidra.log`。默认入口伪代码传给 `__libc_start_main` 的地址为 `0x29dc4`，但该地址没有作为独立函数列出。

标准 `.ARM.exidx` PREL31 展开表恰好声明 ELF VA `0x19dc4`（Ghidra `0x29dc4`）。`derive_arm_exidx.py` 只读解析该标准表，保留源哈希、每条表项偏移及计算值到 `offline/exidx-derived.json`，用于生成 REA 官方种子 TSV。这一步是**分析者派生输入**，不是把外部工具结果包装成 REA Evidence。

带种子的第二次分析跳过全局 `Decompiler Switch Analysis`，清单 4,043 项；1,733 条种子中 created 1,617、existing 3、failed 113、unmapped 0。失败和缺少 switch 全局分析覆盖均须保留，不能宣称完整反汇编。两次分析的 `bundle.json`／`snapshot.json` 和日志分别保存。

`offline/0059-procedure_pseudo_code.evidence.json`（`ev_ecf58ba72bd3b92cd9bcc1cb0dd6cc50f250f3278fcc2fcc698bd00088b8fbf6`）实际恢复入口：

1. 创建 `QCoreApplication`，读取 `QCoreApplication::arguments()`。
2. 第一个用户参数按十进制整数转换，缺失／转换失败走失败路径。
3. 创建 `QTcpSocket`，连接 `QHostAddress` enum 2（按 Qt API 解释为 LocalHost），端口取该整数低 16 位。
4. 连接断开／错误信号到 quit，进入 `QCoreApplication::exec()`。

这是一个由调用方提供端口的分析 worker 入口；它没有在 main 中直接把第一个参数当音频文件路径。没有运行它或实际连接 socket。

### 真实处理链和 Qt 输出身份

已在默认及带种子 profile 中取得真实直接边：

```text
0x376f8 → 0x4a984 → 0x495dc
Qt metacall   process 候选   readBlock
```

来源：默认 `offline/0046/0048/0049`、带种子 `offline/0069/0073/0075` 的 callers/callees。函数语义进一步由断言字符串、Qt 元对象和伪代码支持；名称 `process`／`readBlock` 的归属是这些观察的组合解释，不是原始函数符号。

`QMetaObject` `0x14c518` 的 REA 字节证据 `offline/0094/0095/0096-read_bytes.evidence.json`，经 Qt6 revision12 格式解析，确认 class `BeatDetectionRunner`，16 个本地方法、前 12 个为 signals；方法表及 offset/length 字符串表的边界检查和原始六字字段保存在 `offline/qt-meta.derived.json`。

| 本地 ID | Qt 元对象方法名 | 伪代码中的用途 |
|---|---|---|
| 6 | `detectionProgress` | 块处理进度 |
| 8 | `trackDataChunkProcessed` | process 尾部序列化后 activate |
| 9 | `beatDataChunkProcessed` | readBlock 的节拍块 |
| 10 | `overviewWaveChunkProcessed` | 三字节波形块 |
| 11 | `overviewWaveChunkProcessedRGB` | RGB 波形块 |
| 15 | `process`（slot，3 参数） | metacall 分派到分析处理链 |

表的字节来源是 REA；Qt 结构解释是明确标记的派生步骤，没有声称 REA 内置 Qt 元对象语义解析。

### 音频块与波形布局

`FUN_000495dc`（ELF VA `0x395dc`）完整伪代码 `offline/0044`，Evidence `ev_9d85f36a7321345ecf21ca28d61d7a316d0eb68e7d13b0766dc1c7bc73bdf147`：

- 含 `size_t BeatDetectionRunner::readBlock(const ReaderPtr&, size_t)` 断言。
- 通过 reader vtable 读取，每轮限制到 `0x4000` 个样本；随后按 float 样本处理并调用分析对象。不能由恢复的未知 vtable 类型单独确定所有声道／采样精度 ABI。
- 对 overview／RGB 输出创建 `QDataStream`，依次写 `quint64`、`quint64`、`double`，接着写三字节元素向量，再额外写 **3 字节**尾元素，分别 activate 10／11。
- 向量元素数由 `(end-begin)/3` 派生；第一个 64 位量是该数量，第二个值来自对象字段 `+0x50`，double 来自两个字段相乘。第二个量／double 的精确业务语义仍需与消费端和样本共同确认。

父任务 Demo 的 `3099 = 24 + 1025*3` 与“24B 头、1024 个三字节元素、额外 3B”结构吻合；本次未重新解压 Demo 数据，因此这种对应关系是跨证据推断。按标准 Qt 默认字节序解释为 `>QQd` 与父任务样本相符，但本次没有运行所带 Qt 库验证其运行设置。

### 当前 track IPC 与 Demo 旧样本的区别

`FUN_000e83d0`（ELF VA `0xd83d0`）是真实 serializer，`offline/0082-procedure_pseudo_code.evidence.json`，ID `ev_4cbb822ad37feeffb095c29a06ea00c599c8f4da03ce6af07cf9d6129071cc08`；`offline/0090` 显示其唯一已解析 caller 为 `0x4a984`。process 在 `0x4bdf0` 调用它，随后 activate 8，对应 `trackDataChunkProcessed`。

恢复的写入顺序为：`double`、`qint64`、`qint32`，然后 **6 次 float overload**，内存来源偏移 `0x14/0x18/0x1c/0x20/0x24/0x28`。`offline/0089-procedure_info` 独立确认相关 PLT thunk 是 `QDataStream::operator<<(float)`。

这不能直接断言等于父任务 Demo 的 44B `>dQI3d`。QDataStream 的浮点精度设置、当前 IPC 和落盘形式之间的转换、以及旧 Demo 数据与新固件分析输出的版本关系，尚未全部验证。后续模块应保留显式格式／版本，不以单个旧样本硬编码当前 worker ABI。

另一个候选序列化器 `0xe6c6c` 的 REA 伪代码（`offline/0081`）有两个 double、bool、两个 64 位数量及原始字节数组；当前没有解析到 direct caller（`offline/0092`），因此不把它未经消费关系验证就标成 Demo `beatData` 的完整格式。未知尾字节的意义也未被本次静态证据闭合。

### 分析协议

`FUN_00047b00`（ELF VA `0x37b00`），`offline/0020`，ID `ev_20c6905f296aed725d958dd2b62a9251c8fae9aabefcc2e15edf32d3df8f429f`：检查可读字节，peek/read `0x18` 字节头，按头内长度等 payload 就绪，再计算 CRC。代码可见 CRC 多项式 `0x04c11db7`、初值 `0xffffffff` 和最终取反比较；失败打印 `airOfflineAnalysisProtocol::nextCommand: CRC check failed...`，忽略该命令。

已确证长度／CRC 机制；完整头各字段的精确偏移、命令 ID 枚举、QDataStream 版本和重连／错误策略仍须与发送端对齐。没有把恢复不完整的局部变量布局当作协议规格。

## Engine

### 三种不同覆盖状态

| 连接 | 实际结果 | 可用于结论的范围 |
|---|---|---|
| 默认完整自动分析 | `provider_timeout`，启动限额 1,800,000 ms；`0088-list_procedures.evidence.json` | 只保留失败和诊断，不声称得到完整函数清单／调用图 |
| `engine-focused/`：46,501 个 EHABI 去重种子 | `resource_constraint`，报告 `java.lang.OutOfMemoryError: Java heap space`，配置 JVM heap 4G；`0004-search_strings.evidence.json` | 未就绪，不使用其未完成结果，不声称全量恢复成功 |
| `engine-targeted/`：两个精选种子 | **成功**，种子 `0x1274568`、`0x130de4c`，使用 Ghidra 原生查询、反编译、字节及引用 | 下述所有 Engine 正向结论来自这个成功连接 |

精选连接关闭了默认启用的 `Decompiler Switch Analysis`／`ARM Constant Reference Analyzer` 选项，保留 profile 和日志；它不是默认全覆盖的替代证明。日志仍统计到少量同名分析工作（3.448／0.580 秒），因此不能宣称所有此类调度完全没有执行。其默认自动分析日志总计 355 秒；两条种子均成功创建。`0011-binary_overview.evidence.json` 观察 image base `0x10000`、38 个段、19,785 条过程和 42,470 个定义字符串；计数包含 EXTERNAL／thunk，也不代表全部真实函数已识别。

两条精选入口的定位先用只读 ARM32 literal/ADD-PC 和 EHABI 表生成**待验证候选**（`Engine-literal-candidates.derived.json`），随后确实通过 REA 反编译验证。候选脚本不是 REA xref，也没有把其候选边当作真实调用边。后续按用户要求没有再启动全局分析。

### PerformanceData 代码引用及迁移路径（确证）

- 字符串 Ghidra VA `0x1db8a50`，原 ELF VA `0x1da8a50`。文件偏移也为 `0x1da8a50`，但这个等值是父任务单独读取原始ELF的PT_LOAD核实所得，不是REA字符串观察提供的偏移，见 [地址映射核查](ENGINE-5.0.4-ELF-ADDRESS-MAP.json)。该段 `p_vaddr=p_offset=0`；相反SQL注册窗口所在数据段的文件偏移比原ELF VA小`0x1000`，不能统一相减换算。
- REA `xrefs` 实际返回 `0x130e3a0`，`resolve_containing_procedure` 实际返回 `FUN_0130de4c`，入口 Ghidra VA `0x130de4c`／原 ELF VA `0x12fde4c`。
- 证据 `engine-targeted/0016-xrefs`、`0017-resolve_containing_procedure`、`0008-procedure_pseudo_code`。伪代码 ID `ev_02ae4d48cedd580ed8f162fc554eed2729b3491d990ec09f3abe452afa3f03a7`。

该函数不是孤立保存 DDL 的资源块。它含迁移分支，构造的 SQL 列表包含：`INSERT INTO _Track SELECT * FROM Track`、`DROP VIEW PerformanceData`、`DROP TABLE Track`、重建 Track、`CREATE TABLE PerformanceData`，再从 `_Track` 复制旧音频分析／cue 等列到新表，同时建立索引与 trigger。

真实 callee 集含 `0x1276ae4`（`0009`）；这个公共执行器的 REA 伪代码 `0056` 按 `0x20` 步长迭代 SQL 对象，经 connection 的 `+8` 虚方法逐项执行，并有事务收尾／错误处理。其内部虚方法的完整动态实现未解析，不能把虚调用写成已观测到的直接 sqlite 函数边。

`PerformanceData` 的真实 DDL 字段为：`trackId` 主键、`trackData`、`overviewWaveFormData`、`beatData`、`quickCues`、`loops`、`thirdPartySourceId`、`activeOnLoadLoops`，并对 `Track(id)` 声明 DELETE／UPDATE CASCADE 外键。完整 SQL 原文在 `0004-search_strings` 及 `0051-read_bytes` 中。

这确证存在“从旧 Track blob 列迁到独立 PerformanceData 表”的代码路径；没有执行迁移，也没有由此断定所有版本／所有模块都使用相同文件布局，更没有把父任务“Demo 无 p.db”扩大成全产品恒真结论。

### Information、短数据库文件名与模块值

`Information` 的 Ghidra VA `0x1db78f8` 在 REA `search_strings` 中确证；原 ELF VA `0x1da78f8`，对应文件偏移经上述独立PT_LOAD核查也为 `0x1da78f8`。字段为 `id`、`uuid`、`schemaVersionMajor/Minor/Patch`、`currentPlayedIndiciator`（原拼写）、`lastRekordBoxLibraryImportReadCounter`。该地址的 `xrefs` 在此有限 profile 下为空；没有把“空”表述成没有消费代码。

`rbm.db`／`rbp.db` 短字面量没有被本次 Ghidra 定义字符串检索返回。REA `read_bytes` 则取得完整 ASCII 字节，且 `FUN_01274568` 的真实伪代码对对应地址做 `memcmp`／文件名比较。**该函数是目录／迁移兼容性检查，不是数据库打开构造器。** 它的真实 callers 为 `0x5a9cb0` 和 `0x124b720`（`0007`），这两个 caller 的完整 REA 伪代码亦已保存（`0023`／`0025`）。

`0031-read_bytes` 观察到 12B 模块描述项 `{数值, 名称长度, 名称指针}`，`0033-read_bytes` 验证名称内容；解码保留原始值，不把它们误读为 schema 版本：

| 描述项数值 | m 侧名称（实际表项） | p 侧字面量／分类候选（见下方限制） |
|---|---|---|
| `0x1` | `m.db` | `p.db` |
| `0x2` | `fsm.db` | `fsp.db` |
| `0x4` | `itm.db` | `itp.db` |
| `0x8` | `sm.db` | `sp.db` |
| `0x10` | `trm.db` | `trp.db`（字节候选；分类分支未闭合） |
| `0x20` | `stm.db` | `stp.db` |
| `0x40` | `rbm.db` | `rbp.db` |
| `0x100` | `hm.db` | 本次未闭合 |

p 侧名称来自另一段 REA 字节和代码比较；其中 `trp.db` 仅确认字节存在，对应长度分支调用helper并引用尚未读取的对象，未证明它实际识别此名称。表中并列不等于已经证明 p 文件内部 schema 或 p 侧单独的 module 枚举。名称前缀对应具体产品源（如 rb 与 RekordBox）的解释仍需消费关系佐证，不能只靠缩写扩展。

### SQL 注册项：已读到结构，mask 与边界仍未知

REA `search_bytes` 在加载内存中找到指向 Information SQL 的指针表项（`0045`）；`0050-read_bytes` 固定注册项窗口字节，`0051` 固定完整 SQL 区域。按 LE32 三元组 `{长度, SQL 指针, 数值}` 解码得到以下**观察**：

| 实际 SQL 项 | 三元组数值 |
|---|---|
| Information、AlbumArt、Track、PerformanceData、Playlist、PlaylistEntity | `0x1ff` |
| Pack、PreparelistEntity、Smartlist | `0x1` |
| Historylist、HistorylistEntity | `0x100` |

`0x40` 位包含于 `0x1ff` 是数学事实；“此数值就是 `mask & module` 的 schema 筛选规则”**尚未由消费循环闭合**，因此不将此表升级成已证 rbm 表集。SQLite 实体 rbm／rbp 未取得，也没有虚构其 PRAGMA 或运行时创建结果。

`module-schema-registry.derived.json` 的两个起点为 `0x293ba04` 与 `0x293bc5c`；后者恰好是前者之后 50 个 12B 项。当前读出的 128／78 项是**重叠扫描窗口**，不是两张独立、完整 schema。二者结束于读取范围边界 `0x293c004`，不是已证明的数组终点；窗口中也有重复 Information／不同 Track DDL，可能属于不同版本布局，具体分界未证。文件保留 `complete=false`、`array_boundaries_verified=false`、`mask_semantics_verified=false`；按 `entry_ghidra_va` 去重。

最终只补查了现有连接的注册表 xref 和邻近连接／事务／SQL 执行消费者：`0067/0068-xrefs` 在此 profile 下为空；`0053/0056/0059/0062/0073/0076/0078/0080/0082/0084` 等伪代码提供连接、事务及执行链，却没有证明上述数组边界和 mask 规则。对应缺口保留，不用猜测填充。

### RekordBox 外部入口与内部缓存的区别

REA `0036-procedure_pseudo_code`（`ev_c8a95d27881a76e16e492f19f6def632510f917b44ab90bddb9e3348d34fcfea`）确证静态构造器 `0x307088`／ELF `0x2f7088`：

- 将 `./PIONEER/rekordbox/export.pdb` 初始化到全局对象 `0x2aa1e34`。
- 将 `.PIONEER/rekordbox/export.pdb` 初始化到全局对象 `0x2aa1e40`。

消费函数 `0x17d520c`／ELF `0x17c520c` 的 REA 伪代码 `0065`（`ev_cf0f808f9297c4c255600f3506c0e510928c816eae564bf04c9c5d63a67cb6f5`）先检查目录，再拼接两个对象并用 `QFileInfo::isFile()` 检查普通／隐藏 PIONEER 路径。这证明代码具有具体 export.pdb 路径定位行为，强于仅发现字面量；未建立整个 PDB parser 的完整调用图。

`0013-search_strings` 返回 78 个 RekordBox 相关观察，包括 ImportJobController、Controller、导入计数／设置、数据库不可用／更新失败文本。`0xac7e6c` 的真实伪代码 `0041` 中确有 `Can't get rekordbox database` 分支及后续库访问／事务辅助调用；其中动态对象的确切 rbm／rbp 文件绑定尚未闭合。

针对 `exportLibrary.db`／`USBANLZ` 的 REA 定义字符串检索没有命中；这只是已定义字符串范围的阴性结果，不能证明不访问动态生成或来自 PDB 字段的分析路径。没有足够证据把 `exportLibrary.db` 写成此版本已确认入口；USBANLZ 的解析／应用行为也保留未知。`export.pdb`、内部 `rbm/rbp` 名称和未证的其他格式不得混为一个数据库契约。

## 给 Opus5.5 的模块对接建议与剩余问题

1. **分析 worker 接口**：以 Offline main 的数字端口／LocalHost 连接、24B 头和 CRC 观察建立接口边界；完整命令 ID／头偏移待发送端确认。
2. **track／waveform codec**：分开保存“Demo 旧样本格式”和“当前 worker IPC 观察”。overview 的 24B 头和尾部 3B 像素有静态对应；track 六 float 字段不能被旧样本的三个 double 静默覆盖。
3. **library／schema 模块**：保存 SQL 原文、声明地址、迁移路径和候选模块数值。rbm/rbp 文件名已证，实体表集／适用 mask／版本窗口未证；应保持显式 unknown，而不是生成虚构 schema。
4. **RekordBox 导入**：先对接已证的两个 export.pdb 定位分支；USBANLZ、exportLibrary.db、PDB 字段消费及内部缓存绑定各自保留验证任务。
5. **硬件身份**：消费 libaz0x-info 的设备树事实；AZ01／AZ05 平台判断与 JP07／SC5000 产品映射分开。JP07 精确映射本次未闭合。
6. **固件容器读取**：磁盘版本字段、实现 ops 名称和签名枚举能力分开建模。AZ01+wire version1 走 v2 ops、签名槽为空返回 -11 已证；没有由此推出设备端接受／启动或所有验证旁路。

## 当前限制

- 所有“真实调用边”来自 `procedure_callees`／`procedure_callers` 或明确的指令引用；ops 表解码是标明来源的派生关系，不冒充直接 call。
- Ghidra 首次分析中出现 ARM／Thumb 指令冲突、无法解析 constructor 的 p-code 警告和部分 C++ vtable 应用失败。日志保留；伪代码可能丢失类型、返回值或间接控制流，不能当作可编译原始源码。
- 没有运行时验证、实体硬件／GUI 连接或厂商调试符号，未尝试把反编译结果解释成完整 ABI。
- Demo 数据库 schema／压缩布局和根目录文件缺失情况来自父任务的独立实测；本报告不把这些尚未在 REA 中重做的观察归为本次 REA 产物。

## 工具副作用与范围核查

REA 实际 Java 子进程的 `XDG_CONFIG_HOME`、cache、`user.home`、tmp 都在各自 lab 临时目录（`live-java-isolation.json`）；新建的父进程也额外显式设置 lab `XDG_CONFIG_HOME`。主 agent 提醒的外部 `/root/.config/ghidra` 经只读检查，日志记录的是 `/tmp/ghs-scripts/DumpMain.java`／`az01-script-runner`，并非本次 REA 桥接目标。已保存外部目录 inode／时间清单及最小日志摘录，未以时间接近就认领、移动或删除其他运行的配置。缺少执行前基线，不能宣称该外部目录已恢复；已将归属线索回报主 agent 核实。

范围审计还确认首次 `npm view` 遗漏 cache 选项，实际使用默认 npm 缓存，违反本任务“仅 lab 写入”的限定。已按精确命令／cwd／时间确认本次新增的 956B debug log，将它移到 `external-side-effects/npm-view-debug.log`，保留原路径和 hash。共享 `_cacache` 没有执行前基线，不能安全还原旧值或证明排他归属，故未删除未知内容；这项外部缓存状态尚未完全恢复。之后的 npm 安装均使用 lab cache。完整记录 `npm-default-cache-audit.json`／[COMMANDS.md](rea-evidence/COMMANDS.md)。没有把这一偏差隐去或声称所有外部状态未变。
