# 实际操作及复现入口

工作目录和分析输出均在 `/root/build/sc5000-lab`；首次 `npm view` 意外使用默认 npm 缓存的例外及处理见末节。没有运行 `rea setup`、厂商 ARM 程序、bootloader 或 MCU 操作；未修改系统/用户配置、服务或数据库；未提交或推送。

## 工具取得

执行过 `npm view rea-agents version engines dist.unpackedSize`，返回最新版 6.8.0，要求 Node `^22.23.4 || ^24.22.1 || >=26.12.1`。

```bash
PATH="/usr/local/bin:$PATH" npm_config_cache=/root/build/sc5000-lab/rea-tools/npm-cache npm install --prefix /root/build/sc5000-lab/rea-tools --no-audit --no-fund node@22.23.4 rea-agents@6.8.0
```

实际安装完整输出在 `install.log`。安装命令使用主机 Node 22.23.2，npm 曾输出版本警告；后续所有 REA 分析都显式使用下载的 Node 22.23.4。

通过 GitHub API 查询 Ghidra 最新版 12.2；它要求 JDK 25，因此选择 REA 文档已验证的 12.1.4 + JDK 21。下载元数据在 `ghidra-release.json`、`ghidra-12.1.4-release.json`。Python 调用 `curl -fL --retry 2 <官方 asset URL> -o rea-tools/ghidra-12.1.4.zip`，比较发布元数据 `digest` 后解压并恢复 ZIP 内权限。完整 curl 输出在 `ghidra-download.log`。

首次 REA `open_binary` 返回主机 Java 21 是 runtime-only、没有 javac，原文 `0004-open_binary.json`。通过 Adoptium API 获取 JDK 21，元数据 `jdk-release.json`；Python 调用 `curl -fL --retry 2 <官方 package link> -o rea-tools/jdk21.tar.gz`，比较元数据 `checksum` 后解压。完整输出 `jdk-download.log`。未安装到系统路径。

## REA 原生 MCP

`rea_rpc.py` 是本次使用的本地 stdio JSON-RPC 客户端，只负责保存 REA 的原始请求/响应；没有自行解析或反编译 ARM。每次请求保存 `NNNN-<tool>.json`，`query.py` 另存完整 `structuredContent` / JSON text 为 `*.evidence.json`。`mcp-transcript.jsonl` 是逐消息原始日志；`mcp-stderr.log` 是 REA 日志。两个输出目录的 `launch.json` 记录启动命令和选择的环境设置。

实际启动入口（先后重启过：缺少 JDK、补齐 JDK、带 ops 种子、普通分析）为：

```bash
python3 /root/build/sc5000-lab/rea-results/rea_rpc.py serve
REA_GHIDRA_SEED_FILE=/root/build/sc5000-lab/rea-results/libaz01-seeds.tsv python3 /root/build/sc5000-lab/rea-results/rea_rpc.py serve
GHIDRA_HEADLESS_MAXMEM=4G python3 /root/build/sc5000-lab/rea-results/rea_rpc.py serve
SC5000_REA_OUTPUT=/root/build/sc5000-lab/rea-results/offline python3 /root/build/sc5000-lab/rea-results/rea_rpc.py serve
```

所有服务均是本任务临时前台进程及 lab 内 Unix socket，不注册系统服务，不对网络监听。OfflineAnalyzer 用独立连接，避免重复导入 Engine。没有创建子 agent。

每条调用的**精确参数、顺序、时间及完整返回值**见两份 `mcp-transcript.jsonl`，例如：

```bash
python3 /root/build/sc5000-lab/rea-results/query.py open_binary '{"path":"/root/build/sc5000-lab/5.0.4/tree/usr/Engine/Engine","provider_id":"ghidra"}'
python3 /root/build/sc5000-lab/rea-results/query.py list_procedures '{"detail":"complete"}'
python3 /root/build/sc5000-lab/rea-results/query.py xrefs '{"address":"0x14348"}'
```

种子文件完全由 `0043/0044/0045-read_bytes.evidence.json` 的 LE32 函数指针派生，去重后 28 个地址；使用 `function` 类型，不人为添加厂商符号名。种子改变的是 Ghidra 的临时分析数据库，不改输入字节。

## 独立 REA CLI

以下命令使用共同前缀环境 `TMPDIR=.../rea-tools/tmp JAVA_HOME=.../rea-tools/jdk-21.0.13+9 GHIDRA_INSTALL_DIR=.../rea-tools/ghidra_12.1.4_PUBLIC REA_GHIDRA_STARTUP_TIMEOUT_MS=300000`，`rea` 展开为绝对 Node 与 `rea-agents/scripts/rea.mjs`：

```bash
rea decompile /root/build/sc5000-lab/5.0.4/tree/usr/lib/libaz01-prog.so.1.3 0x1779c --provider ghidra --json
rea decompile /root/build/sc5000-lab/5.0.4/tree/usr/Engine/MidiDeviceScanner 0x1dba8 --provider ghidra --json
rea inspect-binary-layout /root/build/sc5000-lab/5.0.4/tree/usr/Engine/Engine --detail summary --json
```

分别保存为 `libaz01-signature-wrapper.cli.json`、`MidiDeviceScanner-amidi-parser.cli.json`、`Engine-layout.cli.json` 及对应 `.log`。前两个成功返回 REA Evidence；第三个因 CLI 不支持 `--detail` 而在解析参数阶段返回 `Unknown flag: --detail`，未执行分析。随后查询实际 `--help` 确认，Engine 地址映射改由 MCP `inspect_native_load_image` 获取。没有用 strings/readelf 替代 REA。

`materialize.py` 把完整伪代码另存到 `pseudocode/` 并建立索引，未改动原始 JSON；这是便于阅读的派生视图。屏幕预览统一使用 `| cut -c1-200 | head -40`，完整输出均保留文件。读取文件前检查了 `wc -l -c`。

## 函数边界恢复与有界分析

OfflineAnalyzer 默认分析成功，Ghidra 日志实测总计 306 秒，其中 `Decompiler Switch Analysis` 267.508 秒。实际证据还显示其 main 地址 `0x29dc4` 未被默认函数清单识别。为恢复这些边界，执行只读脚本：

```bash
python3 rea-results/derive_arm_exidx.py 5.0.4/tree/usr/Engine/OfflineAnalyzer rea-results/offline/exidx-derived.json 0x29dc4 0x34d34 0x48190
python3 rea-results/derive_arm_exidx.py 5.0.4/tree/usr/Engine/Engine rea-results/Engine-exidx-derived.json
```

这里路径在实际命令中均为 `/root/build/sc5000-lab` 下绝对路径。脚本解析标准 ELF32 LE `.ARM.exidx` PREL31（不是 REA Evidence），固定源文件哈希和每条记录的文件偏移／ELF VA／Ghidra VA；生成的入口 TSV 作为 REA 官方 `REA_GHIDRA_SEED_FILE` 输入。尾部展开表结束记录保守排除，去重后 Offline 1,733、Engine 46,501 个种子。种子结果不等于全部成功，实际 created／failed 计数见 profile 和 Ghidra 日志。

后续独立连接设置 `REA_GHIDRA_DISABLED_ANALYZERS='Decompiler Switch Analysis'`，明确减少全局 switch 识别覆盖；仍使用 Ghidra 原生导入、分析、xref、伪代码和调用边。Engine 默认长分析没有为此中断。新连接输出为 `engine-focused/`；Offline 默认结果先正常导出／关闭，再在 `offline/` 启动带种子连接。全部参数及返回保留在对应 JSON-RPC 日志。

Qt 方法名解析是另一个明确标记的派生步骤：`offline/0094/0095/0096-read_bytes.evidence.json` 提供元对象、revision12 方法表和 offset/length 字符串表字节；按 Qt6 格式检查边界后写出 `offline/qt-meta.derived.json`，并与 Ghidra 观察的 `QMetaObject::activate` ID 对应。没有声称 REA 内置 Qt 元对象语义解析器。

一次客户端地址组误选了默认 Engine 连接，首个 Offline 地址的 xref 请求因此排队。Engine 默认分析在 30 分钟期限失败后，该旧连接被清理，该组没有产生可用 REA xref；没有把它用于任何功能结论。正确 Offline 连接上的同组 9 条请求是 `offline/0007` 至 `0015-xrefs`。

Engine 默认 `list_procedures` 实际返回 `provider_timeout`，完整错误含诊断保存在 `0088-list_procedures.json`／`.evidence.json`，日志副本为 `Engine-default-before-deadline.ghidra.log`。只在原始启动已失败后，基于已保存结果和进程启动身份停止旧 stdio 客户端，防止排队 follow-up 再做相同导入；记录 `Engine-default-deadline-watch.json`。带种子、禁用已测慢 analyzer 的 `engine-focused` 连接不受影响。

该 46,501 种子连接随后实际报告 `resource_constraint`／`java.lang.OutOfMemoryError: Java heap space`，配置 heap 为 4G（`engine-focused/0004-search_strings.evidence.json`），不是成功结果。仅在失败后停止其旧 stdio 连接，记录 `engine-focused/deadline-watch.json`；没有重复全量导入。

最终成功的是另一个只有两个精选入口的连接：

```bash
SC5000_REA_OUTPUT=/root/build/sc5000-lab/rea-results/engine-targeted REA_GHIDRA_DISABLED_ANALYZERS='Decompiler Switch Analysis,ARM Constant Reference Analyzer' REA_GHIDRA_SEED_FILE=/root/build/sc5000-lab/rea-results/engine-targeted/selected-seeds.tsv GHIDRA_HEADLESS_MAXMEM=4G python3 /root/build/sc5000-lab/rea-results/rea_rpc.py serve
```

`derive_arm_literal_refs.py` 先对只读 Engine 的 ARM32 LDR-literal／ADD-PC 形成候选，按 ELF LOAD 和 `.ARM.exidx` 派生入口。最终脚本范围是 32 指令窗口、两种 ADD 操作数顺序；未证明中间寄存器／分支条件，不覆盖 Thumb，因此只用于选点，不能当作 REA xref。两条输入种子 `0x1274568`、`0x130de4c` 经真实 REA 反编译确认后才用于结论。后续均为此已打开会话中的精选查询，没有进一步全局分析。

短名称通过 `read_bytes` 确认；SQL／模块注册项通过 `decode_engine_registries.py` 从 REA 字节解码，不执行 SQL。两个 schema 项是重叠扫描窗口（第二个从第一个后 50 triples 开始）；保留 `complete=false`、边界／mask 语义未验证。最后仅补已有连接的 xref、连接／事务／执行 helper，未闭合则如实保留。

最终 `engine-targeted/complete.bundle.json` 和 `complete.snapshot.json` 保存全部本会话 Evidence 后正常关闭。五个源输入再次 SHA-256 比对原清单，`targets.final.sha256` 与 `targets.sha256` 完全相同。`EVIDENCE-INDEX.json` 包含原始响应文件 SHA-256、完整 Evidence 文件 SHA-256、规范化 normalized_result SHA-256 和 Evidence ID；错误条目的原始诊断仍完整保存在对应文件，索引只取有用摘要。

## 外部配置目录核查

主 agent 提醒 `/root/.config/ghidra` 出现新增目录后，本 agent 未中断现有 Java。实际 `/proc/<owned-pid>/environ` 的选择项及 Java 参数保存为 `live-java-isolation.json`，确认 REA 已给两个 Java 子进程指定各自 lab 内的 `XDG_CONFIG_HOME`／cache／home／tmp。随后也在 `rea_rpc.py` 给未来的 REA 父进程显式指定 lab `XDG_CONFIG_HOME`。

外部目录只读 inode／时间／文件类型清单为 `external-ghidra-inventory-before.json`。外部 `application.log` 明确记录的是 `/tmp/ghs-proj/sr` 和 `/tmp/ghs-scripts/DumpMain.java`、目标 `az01-script-runner`，并非本次 REA 的桥接脚本／目标。没有起始时刻的外部目录清单，不能据此宣称自己拥有该目录，因而没有移动、修改或删除。该 attribution 已回报主 agent，由实际创建该探针的操作者核实处理。

首次 `npm view` 没有设置 `npm_config_cache`，因此实际生成默认目录下的 npm 缓存／日志，属于“仅 lab 写入”约束的偏差。实际 956B 日志的命令、cwd、时间吻合本次任务，已保存原路径／mtime／SHA-256，并将该已确认的新日志移动到 `external-side-effects/npm-view-debug.log`。日志明确显示 registry fetch `cache miss`；没有执行前缓存基线，不能安全还原共享 `_cacache` 的旧值或证明所有内容的排他归属，未删除未知缓存。详见 `npm-default-cache-audit.json`。这不应表述成外部状态已经完全恢复。后续 npm 安装使用 lab cache，进一步 npm 发布元数据通过 curl 直接保存到 lab。
