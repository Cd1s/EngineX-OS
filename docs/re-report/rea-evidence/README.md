# REA 可携带文本证据

原始证据根目录是 `/root/build/sc5000-lab/rea-results`。厂商二进制、Ghidra临时项目、原始MCP信封/日志和完整snapshot均留在lab。这里保留本次证据文件的成功归一化结果及失败摘要，数量以INDEX为准，供报告引用和模块开发检索，不包含固件镜像。

- [INDEX.json](INDEX.json)：按原始相对文件名定位 `observations.jsonl` 的行号、原文件SHA-256、REA Evidence ID和操作名。
- [EVIDENCE-INDEX.json](EVIDENCE-INDEX.json) 是最终原始索引（267项、231个唯一Evidence ID）；[EVIDENCE-CONSISTENCY.json](EVIDENCE-CONSISTENCY.json) 和 [ARTIFACT-MANIFEST.sha256](ARTIFACT-MANIFEST.sha256) 保留lab端一致性结果与逐文件摘要。manifest中的路径相对lab证据目录，不表示原始大文件已纳入git。
- [observations.jsonl](observations.jsonl)：每行一条记录。成功记录保留全部 `normalized_result`、参数、subject、analysis profile、authority、confidence及limitations；失败记录只保留类型、消息和可重试状态，长错误日志在lab原件。Evidence ID没有重算，此文件是投影，不冒充原始REA信封。
- [RUN-METADATA.json](RUN-METADATA.json)：版本、输入摘要和执行范围；[COMMANDS.md](COMMANDS.md) 为实际命令记录，[launch.json](launch.json) 为父进程启动配置。REA为Java子进程建立的隔离目录与父进程环境不同，见原始审计。
- [module-schema-registry.derived.json](engine-targeted/module-schema-registry.derived.json)：从REA字节记录派生的模块/SQL三元组。两个窗口重叠，`complete`、数组边界及mask语义均未证实，不能直接作为两个完整数据库Schema。
- [qt-meta.derived.json](offline/qt-meta.derived.json)：从REA字节记录按Qt6元对象格式派生的类/信号/方法；派生解释与原生反编译事实分开。

当前运行版本是rea-agents 6.8.0、Node22.23.4、Ghidra12.1.4、完整JDK21.0.13+9，均位于lab。完整复现先依 [COMMANDS.md](COMMANDS.md) 使用lab内缓存、临时和配置路径，不运行全局`rea setup`。Engine默认分析超时、46,501种子分析OOM与两入口targeted成功是不同结果；profile不能互换。

可携带投影共268行：231个唯一Evidence ID及37条诊断摘要。生成器同时读取最终索引中的bundle留存记录，验证原文件SHA-256，对相同Evidence ID核对全部投影字段后去重并保留来源别名。全部231条归一化结果已逐项对比原件；报告中的完整Evidence ID均能在投影中解析。诊断数多于原始索引，是因为另保留了未纳入最终索引的一次CLI失败。

已有原始结果时可重新生成投影：

```bash
python3 docs/re-report/rea-evidence/curate_evidence.py \
  /root/build/sc5000-lab/rea-results docs/re-report/rea-evidence \
  | cut -c1-200 | head -40
```

例如用原文件名定位一项，而不整份打印大文件：

```bash
rg -n 'engine-targeted/0016-xrefs' docs/re-report/rea-evidence/INDEX.json \
  | cut -c1-200 | head -40
```

符号/字符串清单可能包含PLT、EXTERNAL和自动生成标签；`FUN_...`不是厂商原始函数名，分析VA不是运行时地址。间接调用未解析、未命中字符串、未恢复表边界都保持unknown，不据此推导功能不存在。
