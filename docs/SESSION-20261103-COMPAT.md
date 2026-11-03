# SC5000 兼容性与播放稳定性迭代

本轮接续基线：`a7161f0` 加每拍一次的 UI 修改。用户指定通过 Paseo，由 GPT-6.1 Sol 用 REA 补充逆向，Opus 5.5 开发。完整源快照在 IN 独立仓库 `sc5000-compat-20261103`，快照提交 `fe56ca9`；不会覆盖原有构建目录或用户未提交改动。

## 本轮目标

1. 真正的 Stems 播放列表：可以选中，只显示有分轨的歌曲并可以加载，不能以排序按钮替代。
2. 转盘灯每拍变色一次，取消之前每半拍一次的周期。
3. Rekordbox 大库保留 Cue、Loop、拍线与波形；核实原生数据映射，消除依靠 UI 倍数补偿的做法。冷盘首导、完整缓存复用与源变化检测分别验证，不把省略分析的 fast 状态称为完整缓存。
4. 播放稳定性最高优先：用户确认单首普通 U 盘歌曲也会偶发卡音，不依赖双 Deck 场景。没有复现和修复前后证据时，不能声称已消除卡音。

## Paseo 分工

| 职位与模型 | Agent | 工作树 | 范围 |
| --- | --- | --- | --- |
| 程IN / GPT-6.1 Sol | `e098b37e-f903-40f7-b5db-2e003edbd872` | `/root/build/sc5000-compat-20261103-rea-stems` | REA 补查 Stems 列表与能力消费路径 |
| 程IN / GPT-6.1 Sol | `e9e03abb-eb4d-4ba6-b7e9-eaec7228eca8` | `/root/build/sc5000-compat-20261103-rea-waveform` | REA 补查原生波形格式与 Rekordbox 映射 |
| 高IN / Opus 5.5 | `e4e285aa-a087-4d69-bb47-483cd3a94954` | `/root/build/sc5000-compat-20261103-opus` | 功能开发、回归、ARMv7 产物 |
| 程IN / GPT-6.1 Sol | `52ca715c-a951-4129-9e55-47013feff76b` | `/root/build/sc5000-compat-20261103-audio` | 只读卡音诊断、采集器和因果边界 |

既有固件结构与数据库逆向直接复用；原始厂商输入只读，新增 REA 输出使用各自目录。所有构建/依赖操作只在 IN。代理默认不提交、不推送、不部署，不触碰密钥。

## 实机基线，2026-11-03 13:40–13:44 UTC

用户已确认有线直连 en5 对应 SC5000，并授权首次登记主机指纹；已按精确指纹验证建立连接。此次使用独立临时 SSM 配置，没有覆盖或发布原有存在待提交变更的共享 vault。

- Engine 文件 SHA-256：`342b611bd71fd09d9fda8013f19dccfba47ee4f0f772a959058bdd19d74d5413`。
- UI hook SHA-256：`0cdafcf0245f215ed4d03a66b03e626e05f00835f161bb41b48dfa18cd5dc2cb`。
- Suite SHA-256：`2caeaa43467a53b7fc18ec3dfd793bb5394423255eae990c46b9dd8c3d2ec451`。
- lab 指定路径 Engine 实际为 `e1eb0e2b858880016daa3ad14538426204f8e698d855a274aebc924a5f9a5caa`，不能不经字节关系核对就把其偏移当作实机补丁。
- 音频 PCM：JP07 card2/pcm0p，S32_LE、4 通道、96 kHz、period 450 frames、buffer 900 frames，约 4.69/9.38 ms。
- 实际 AudioOut 固定 CPU3、SCHED_RR 49；AudioRenderer2/3/4 分别固定 CPU2/1/0、SCHED_RR 49。音频 DMA IRQ45/46 在 CPU0、SCHED_FIFO 99。
- `engine.service` 实际属于 `system.slice`；`stream-sync` 是 nice19、CPU3、`sc5000-background.slice`。仓库里写了 Engine slice 不等于实机生效，原厂播放线程也使用 CPU3，不能直接把整个 Engine 限制到 0–2。
- CPU governor 为 ondemand，up_threshold 90、sampling_rate 10 ms；首个快照为 1.608 GHz、温度约 76–77°C。温控 trip 113°C；这不证明当时发生了温度降频。
- RT 限额为每 1000 ms 允许 950 ms；内核在 boot 24066.66 s 记录过 `RT throttling activated`。该消息可能只输出一次，不能以日志没有新增推断没有再次节流。
- 30 个采样点实际约 38 s，PCM 均为 RUNNING。delay 范围 614–892、avail 8–286、avail_max 363–647；此窗口未捕到用户所说的卡音。采样遍历线程会产生观察开销，后续采集应缓存 TID，避免每秒派生大量进程。

原始文本已传到 IN 快照仓库的 `out/LIVE-BASELINE.txt`、`THREAD-BASELINE-NOTES.txt`、`thread-stats.txt`、`audio-samples-30s.txt`。这里记录的是观察，不是根因或修复结论。

## 本轮前置备份

- 源快照：`20261103T133243Z-cheekmacbookair-sc5000-before-rea-compat-20261103-2e24fcaf323f-sc5000-compat-source-20261103.tar.gz`。
- 首次主机信任登记前 known_hosts：`20261103T133949Z-cheekmacbookair-sc5000-before-first-host-trust-20261103-c3f146438cdc-known_hosts`。

截至这份基线记录，实机操作仅有只读采样和截图，没有重启 Engine 或修改其调度/固件。后续部署和复核记录须在实现完成后追加。
