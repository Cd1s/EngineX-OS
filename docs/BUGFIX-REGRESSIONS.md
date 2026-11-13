# 17 项 Bug 修复与回归验收

所有验证在 IN 的 `/root/build/sc5000-custom-os` 执行；未读取禁止访问的固件树，未 push。

| 项 | 修复 | 回归覆盖 |
| --- | --- | --- |
| 1 | 音频 IRQ CPU0/FIFO99，GPU CPU1，IO CPU2，其余 IRQ CPU1–2；从 IRQ 编号识别被截断的线程名 | `tests/test_rootfs_overlay.sh` 的 IRQ mask、线程 CPU、优先级和重复执行断言 |
| 2 | 忽略路径大小写识别 PDB/SQLite 导出；bridge 后、通知前限时运行 CPU3 healer | Shell 的三类导出、混合大小写目录、执行顺序和 healer 超时用例 |
| 3 | 波形索引乘法使用 uint64，并检查条目大小 | `TestWaveformIndexAndShortAnalysisBoundaries`；386 实际运行和 ARMv7 编译 |
| 4 | 同一事务删除消失的桥接曲目、状态和本库成员，释放旧路径 | `TestDeletedSourceReleasesPathAndMembership`、`TestDeletedSourceCleanupRollsBackOnBadAnalysis` |
| 5 | 安全相对/绝对链接、覆盖非目录链接、guardian/模板服务、FDT SafetyError | az01-packer 的 33 项测试，包含真实 ext2 和完整 overlay 重复注入 |
| 6 | schema 触发器/索引、歌单 ownership 完整性、NULL 尾链、短分析采样数、保留源封面哈希 | schema/performance/boundary 测试及 `TestEOS09ArtworkResizeHashAndOrphan` |
| 7 | 单次 QMP timeout/OSError 后重新连接，持续尝试到 deadline | `sim/qengine/test_smoke.py` 用真实 Unix socket 分别模拟握手和查询超时 |
| 8 | 空文件名、非普通/过大封面、FLAC 范围、合并设备/TTL、启动前校验 | rekordbox、bridge、compat、prolink 和 rb-bridge 的新增回归 |
| 9 | FLAC 位深与声道范围 | `TestDecodeFLACFormatBoundsAndInheritedFields`、`TestDecodeFLACRejectsInvalidOrInconsistentStreamInfo` |
| 10 | MP3 采样率范围 | `TestDecodeMP3SampleRates` |
| 11 | Import 的有限正 overview 步长和采样率范围 | `TestLibraryImportRejectsInvalidAnalysisBounds`，并确认拒绝时不写 Track |
| 12 | 自相关长度和最大 lag，不计算空区间平均数 | `TestOnsetAutocorrelationLastSampleBoundary`、`TestDetectTempoShortestAutocorrelationEnvelope` |
| 13 | foldBPM 拒绝非有限数与非正数 | `TestFoldBPMRejectsInvalidValues`、`TestFoldBPMFiniteOctaves` |
| 14 | 波形空音频防御与 start 下界 | `TestComputeWaveformsRejectsEmptyAudio`、`TestComputeWaveformsSingleSample` |
| 15 | 空下载占位文件继续等待 | `TestDirectoryAdapterWatchesOnlyRequestedID` 的零字节文件超时和后续有效文件完成场景 |
| 16 | float WAV 拒绝 NaN/Inf 及转换溢出 | `TestDecodeFloatWAVRejectsNonFiniteSamples`、`TestDecodeFloat64WAVRejectsFloat32Overflow` |
| 17 | 新歌单曲目合并到会话缓存 | `TestGuestDownloadAnalyzeImportEndToEnd` 连续浏览 p1/p2 后仍能提交 p1 的 s1 |

扫描中额外修复的边界：

- 导出文件检测只匹配相对路径，不把 media 根目录中的 `[]` 等字符或父目录名称当作导出模式；使用 NUL 分隔保留路径字节。

- PlaylistPath 缓存、脏缓存回退和旧缓存升级保持厂商 position 顺序及 NULL 标题语义，逐行对照原始视图验证。
- 节拍抛物线插值限制在半帧内，非法 comb-search 速度区间在迭代前拒绝，避免零 BPM 的无限相位循环。

- 非 Linux 开发环境正确执行 `--disable-guard`，不再在禁用护栏时直接报平台不支持；通过显式文件集运行便携实现的回归测试。

- 分析器写入相对 Engine Library 的 Track.path，并用同一路径识别已有曲目，保持锁定节拍与 cues。

- 冷插拔扫描忽略 eMMC boot0/boot1/RPMB 辅助节点，避免内部设备使发现服务误报失败。

- 多条原生歌单链以 NULL 结尾时，直接归零会触发唯一约束；先连接这些链并保留链内顺序，再归零。
- 被删除的实际歌单可能残留 ownership 行；同时检查行数和实际歌单引用。
- 清理本库旧曲目时保留外库 UUID 下相同 trackId 的成员。
- FLAC 帧继承 STREAMINFO 字段，并拒绝帧/流格式不一致；拒绝浮点转 float32 溢出和 Analyze 非法采样率。
- 实际读取的封面在文件增长后超过 32 MiB 时也拒绝写入。
- `TestHealedArtworkKeepsBridgeDeduplication` 验证 healer 缩图后再次导入仍只有一条封面记录。

## 验证

以下命令通过：

```sh
. /etc/profile.d/buildtools.sh && go test -race ./...
. /etc/profile.d/buildtools.sh && GOOS=linux GOARCH=arm GOARM=7 CGO_ENABLED=0 go test ./pkg/enginedb ./pkg/rekordbox ./pkg/compat -c -o /dev/null
python3 -m unittest discover -s tools/az01-packer -v
bash tests/test_rootfs_overlay.sh
bash -n sim/qengine/run_qengine.sh rootfs-overlay/usr/local/bin/enginex-storage-mount rootfs-overlay/usr/local/bin/enginex-perf-tune rootfs-overlay/usr/local/bin/enginex-debloat
```

另外逐个执行上述脚本的 `bash -n`（bash 的多个文件参数并不会逐一解析），并通过全仓七个 Shell 脚本（含无扩展名的 mount.hfsplus）的语法检查。

Python 打包器 33 项、Shell 32 个场景及其调用的 QMP 双阶段超时回归通过。
额外通过 `go vet ./...`、386 三包测试及三个命令程序的 ARMv7 `CGO_ENABLED=0` 构建；`file` 确认为静态 ARM ELF。

非 Linux 护栏分支通过以下显式文件集在宿主机执行回归（Go 显式文件集不受 build tag 筛选）：

```sh
. /etc/profile.d/buildtools.sh && go test -race pkg/resourceguard/resourceguard.go pkg/resourceguard/guard_other.go pkg/resourceguard/guard_other_test.go
```

## 交叉复核记录

实现为 GPT61，独立复核为严DE（S55），全部测试与构建在 IN 执行。

- 程IN 分工：analyzer `ccae8343-34ea-4819-8013-42d473d24b2e`；packer `55d49740-1208-4b8d-a2b7-ef004727daff`。
- 严DE Go 主干走读：`e99bc8d2-fc01-4f64-87aa-d408f0ed4782`；确证的 NULL 尾链、分析器路径和 eMMC 辅助节点问题已修复。
- 严DE Python/QMP 最终复核：`2b66430d-b6b0-45a7-9334-8754e871b33e`，通过。
- 严DE 平台/路径及追加边界复核：`4dd85311-e8ed-459b-a4b9-a82b739c27c7`，最终 Go v3 和相对导出路径检测通过。
- 严DE 以系统 SQLite 3.46.1 对照 3000 组随机森林，最终歌单 view 与厂商 view 一致；主线另以项目使用的 modernc 驱动运行 50 组、每组 24 节点的持久回归，覆盖 clean/dirty/NULL 尾链/刷新，全部通过。
- 复核为代码走读及独立最小实验；Go race、Python、Shell 与 ARMv7 验证结果来自 IN 实际执行。
