# 复核与验证记录

实现与固件分析在 IN，使用 GPT 家族；安全复核使用严DE（Claude Sonnet），按 [fleet-orchestrator](/root/.agents/skills/fleet-orchestrator/SKILL.md) 调度。未推送、未刷机。

| 工作 | Agent / workspace | 结果 |
|---|---|---|
| QEngine封装，程IN | `a544636f-02d8-4b65-af0d-598d3a1ae2db` / `wks_266ed2461f4e04e5` | Bash/ShellCheck、dry-run与拒绝用例；未实际启动guest；工作区已归档 |
| REA分析，程IN | `f2b652a9-dc5f-4c7d-b398-a5747e50d61e` / `wks_47ec13a6655df8ca` | 版本、输入哈希、真实函数/引用/伪代码及覆盖边界见REA报告；工作区已归档 |
| 安全与REA证据复核，严DE | `1ecacd7d-e4bf-4364-a639-dd39abe70322` / `wks_97a396cc096a28e3` | 源码无P1/P2阻塞；六项REA证据抽查无文档阻塞；没有在DE重跑测试；工作区已归档 |

复核确认外部候选rootfs的扩容/UUID漂移需要额外约束。已在工具中固定原包几何、UUID和特性位，以真实resize2fs与改UUID夹具验证拒绝发布。保护快照覆盖完整长扩展属性，准备脚本使用临时目录生成后rename，均有针对性测试。

最终源码还增加了 `env.bin`、`idbloader`、`MLO`、`trust.img/bin` 拒绝规则。复核包生成早于此项，复核者指出了包的时序差异；最终工作树包含规则并通过拒绝测试。复核另记录卷标/默认挂载选项未比较，但没有证据表明本次启动链依赖这两项，因此不作为阻塞项。

实测结果见 [VALIDATION.json](VALIDATION.json)：24项单元测试，真实5.0.4解包/公钥注入/重打包/重提取哈希/保护快照/geometry/e2fsck检查，以及8份SQLite Schema重建。QEngine仅完成语法、静态检查和dry-run，前置检查如实返回缺失；没有Engine UI、Rekordbox导入或音频实机通过的结论。

REA复核确认签名槽、Offline入口与六float序列化、PerformanceData迁移链、重叠扫描窗口及PIONEER路径发现的证据边界，报告中的10个完整Evidence ID均可检索。两个P3已由主任务修正：原ELF的PT_LOAD映射独立保存为 [地址核查](ENGINE-5.0.4-ELF-ADDRESS-MAP.json)，不再把文件偏移归为REA字符串观察；`trp.db` 改为字节候选，未读取helper比较对象，分类分支仍未知。修订由主任务验证，未声称严DE重新复核了新增映射文件。

复核原始记录在lab的 `sc5000-qa-final.txt` 与 `sc5000-qa-rea-result.txt`。严DE披露其只读复现脚本写在远端复核目录的 `review/repro-scripts/`，超出任务书“仅写rea子目录”的字面范围；没有修改厂商输入。REA首次npm默认cache的范围偏差及共享cache无法完全恢复的限制，另见REA报告末尾审计。
