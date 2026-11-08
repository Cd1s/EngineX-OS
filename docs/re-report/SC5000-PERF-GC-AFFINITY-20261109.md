# SC5000：非影子轨 GC、JobController 与 CPU/IRQ 性能修复

分支：`agent/perf-gc-affinity-20261109`。基线：`de0f64d2aa9c54ee26f6e31aa151f0e557402fdb`（`origin/agent/merge-latest-20261109`）。

本报告描述源码修复和构建验证，尚未部署到碟机；部署及性能复测由 PM 统一安排。依据为 `fleet-share` 的 `runs/20261109-sc5000-stutter/report-DA-full.txt`「结论与根因总览」及根因 1–3 的因果链，并在 Batam 经 `deck L/R` 补充只读核对。

## 根因 1：减少 QML GC 根和无效绑定刷新

| 文件：函数/属性 | 修改 | 预期效果 |
| --- | --- | --- |
| `tools/ui-mod/build_packs.py`：`encoded_dicts`、`render_language`、`SERVICES_MAIN.scDict/scT` | 非当前语言词典保存为 JSON 标量字符串，只解析当前语言；语言包用本包语言初始化 `scLang`。保留每个词条的三种输入写法，以及即时语言切换 | 只保留一份活跃 JS 词典，减少 GC 需要遍历的属性和对象 |
| 同文件：`scCueClock1..4`、`scCueTickFor`、`cue_capsule` | 四轨独立 200 ms 时钟，仅对应轨道播放且开启倒数时运行；每个胶囊只依赖自己的轨道时钟，关闭倒数时不计算 cue | 全部暂停时无 cue 计时；一轨播放不会周期性刷新其它轨道胶囊 |
| 同文件：`scCueProps`、`scNextCueInfo` | 缓存各轨 cue 的 translator；不再每次创建查询闭包、临时 best 对象、签名对象；显示字段未变时直接返回原显示对象 | 减少每秒临时对象、字符串和布局变更；`scTr` 的 translator 缓存仍保留 |
| 同文件：`scApply`、`scSameItems` | `scSources/scLoggedIn` 比较数组长度及每个字符串，内容不变就保留原引用；其它标量由 QML 同值赋值机制避免变更通知 | 相同状态的 3 s 轮询不再触发 ConnectedSource 的 Repeater 重建 |
| 同文件：`scCall`、`scUiPollReply` | 常规 GET 轮询复用一个 XHR 和回调，已有在途请求时不重叠；超过 10 s 则中止旧请求后重试。设置写入仍独立请求 | 降低常态轮询分配，服务暂时挂起后可恢复 |
| 同文件：`art_source`、`scDeckArtUrl/scDeckArtPending` | MicroDeck/AlbumArt 脱离 cue 时钟，由本轨原生曲名/路径绑定触发；仅在已加载、可见、缺封面元数据时每 500 ms 重试，拿到 URL 后停止 | 暂停曲目仍能等到迟到的元数据；播放过程中不再随 cue 时钟反复计算封面 URL |
| 同文件：`scJogWantedDecks/scJogTargets`、jog Timer、`scBpmDecks`、`scStemImport` | jog 的轨道列表由绑定更新，缓存颜色 translator，40 ms 循环去掉列表重建、join 和闭包；BPM 轨道列表缓存；stems 导入复用 2000 个 ID 的数组 | 减少启用 jog 动画及进行 stems 导入时的短命分配；原来按需启停的 Timer 保持按需运行 |

实际生成的三种 5.3.0 语言包均为 **924 个活跃词典条目 / 原 2772 个条目 = 33.333%**。与报告按 TSV 行数估算的 3042 不同，2772 是合并重复输入写法后的实际属性数。非当前语言的文字仍以字符串保留，选择“避免根对象常驻数千个 JS 属性”的方案，以保留即时切换而无需重启；并未宣称消除了所有文字的内存占用。

新增 `tools/az01-packer/test_ui_perf.py`：验证字典条目比例、所有旧输入写法及三语输出一致；用 Node 执行实际 QML JS 函数，验证 translator/显示对象复用、暂停条件、只读取目标轨道时钟、数组内容比较，以及 XHR 复用/挂起恢复。原 `test_ui_mod.py` 的完整语言包生成测试也核对每包 1/3 活跃字典条目。

## 根因 2：通用 JobController 不再被误降级

`tools/ui-mod/engine_ui_hook.c:thread_class` 去掉 `J:I:Takingfreez`、`Taking freeze`、`Collecting anal` 这类临时任务名，并对所有 `J:*` 明确返回普通类。保留独立的 Analysis Queue、OfflineAnalyzer、Freeze Checker、Dropbox/Beatport、崩溃处理线程的后台分类；图片/文件信息线程保留原来的轻度降级类别。

L/R 实际均有 4 个 `J:I:Takingfreez` 线程。L 的 TID 为 6900–6903，R 为 2423–2426，当前都是 CPU3 / SCHED_IDLE。`test_az04_hook.c:test_thread_policy` 除分类回归外，还实际调用命名 guard，核对临时任务名不会改变调用线程的亲和度、调度策略、nice 和 I/O 优先级。

新 hook 防止误降级；已被旧 hook 降级的存量线程需在 PM 部署后由正常 Engine 重启重建，本次没有在碟机上尝试修改调度或重启。

## 根因 3：明确四核分配并保护音频优先级

### 部署前实机只读核对

L 主 PID 6642，另两个同名 EMain 是 6904、6915；R 主 PID 1258，另两个是 2428、2447。三个 EMain 都在 CPU1，SceneGraph 在 CPU2。L/R 音频的分配一致：

| 线程/IRQ | 当前 CPU | 当前策略/优先级 | 本次音频处理 |
| --- | --- | --- | --- |
| AudioOut | 3 | RR 49 | 保留 |
| AudioRenderer2 | 2 | RR 49 | 保留 |
| AudioRenderer3 | 1 | RR 49 | 保留 |
| AudioRenderer4 | 0 | RR 49 | 保留 |
| AudioWatcher | 1 | RR 45 | 保留 |
| IRQ 41/42 `ffb20000.dma-controller` | 0 | FIFO 99 | 保留 |
| IRQ 46 VOP/IOMMU、52/53/54 panfrost | 1 | FIFO 50 | 移到 2，FIFO 40 |
| IRQ 51 `ff650000.i2c`、55/56 eth0、58/59/60 USB、65/71/72 dw-mci | 2 | FIFO 50 | 移到 3，FIFO 40 |
| unbound workqueue | 1/2（mask `6`） | 内核调度 | 改到 0/3（mask `9`） |

核对包括 `/proc/interrupts`、每个 IRQ 的 `smp_affinity_list`、L 的全部 Engine task 的 `Cpus_allowed_list`/调度策略，以及相关 IRQ 线程实际策略。L 的触摸 IRQ 累计约 2751 万次、主 eMMC IRQ 约 1044 万次、panfrost-job 约 599 万次，均与报告中的热点一致；这是累计数，不能当作 IRQ/s。

### 新的分配表（待 PM 部署生效）

| CPU | 前台任务 | IRQ | 后台/辅助 | 保留的 RT 音频 |
| --- | --- | --- | --- | --- |
| 0 | — | 音频 DMA，FIFO 99 | 与 3 共享普通辅助线程、OfflineAnalyzer、unbound workqueue | AudioRenderer4，RR 49 |
| 1 | 唯一 GUI 主线程，TID == PID | 不放可移动外设/GPU IRQ | 不放普通 Engine 辅助线程 | AudioRenderer3 RR 49、AudioWatcher RR 45 |
| 2 | SceneGraph | panfrost、VOP、HDMI，FIFO 40 | 不放普通 Engine 辅助线程 | AudioRenderer2，RR 49 |
| 3 | — | 触摸 I2C、eMMC/SD、eth0、USB 等非音频外设，FIFO 40 | 与 0 共享辅助线程、OfflineAnalyzer、workqueue；专用 idle guard 线程仍可限制在 3 | AudioOut，RR 49 |

实现位置：

- `rootfs-overlay/usr/local/bin/sc5000-perf-tune:classify`：音频 IRQ→0，GPU/VOP→2，其它可移动 IRQ→3；同时设置 IRQ 掩码和 PREEMPT_RT IRQ 线程亲和度。音频 IRQ 仍 FIFO 99，非音频 IRQ FIFO 40，低于 45/49 的 RT 音频。
- 同文件 `engine_threads`：用 TID==PID 识别真正 GUI 主线程并固定到 1；SceneGraph→2；所有策略为 OTHER/BATCH/IDLE 的辅助线程→0,3，不再只处理原来在 CPU1 的线程，也不因 comm 同为 EMain 而漏掉。FIFO/RR 等实时线程跳过。OfflineAnalyzer 进程及其子线程也覆盖到 0,3。原启动及延迟重扫机制保留。
- 同文件 workqueue 设置：`cpumask=9`，避开 GUI/SceneGraph 的 1/2。
- `rootfs-overlay/etc/default/sc5000-perf`：补齐 `panfrost` 名称，写明四核分配和 IRQ 优先级。
- `tools/ui-mod/engine_ui_hook.c:guard_affinity`：后台类也避开 1/2，防止后续命名或设置亲和度重新放回 GUI/GL 核。
- 同文件 `lower_offline_analyzer`：从在线 CPU 集合选择非 GUI/GL 核，四核机为 0,3；即使从 EMain 继承单核 CPU1 也可以移开，保留 SCHED_IDLE / nice19 / idle I/O。
- `tests/test_rootfs_overlay.sh`：同步真实 IRQ 名称、mask9、主/同名辅助 EMain、其它核的普通工作线程、OfflineAnalyzer 主/子线程，以及 RT 线程不动和非音频 IRQ 40 的断言。

## 验证

按任务明确要求，在 Oracle-Batam-arm 原生执行以下检查，没有安装语言依赖。构建仅写本任务临时目录，没有写入固定实验材料。

| 检查 | 结果 |
| --- | --- |
| `. /etc/profile.d/buildtools.sh; gofmt -l .` | 无输出，格式干净 |
| `go vet ./...` | PASS |
| `go test -race ./...` | PASS，24 个有测试包 |
| `bash tests/test_rootfs_overlay.sh` | PASS，17 项模拟存储/硬件测试 |
| `python3 -m unittest discover -s tools/az01-packer` | PASS，118 项，1 项安全跳过（宿主已有 `/data`，不在共享主机上运行对应真实目标路径安装测试） |
| 完整 `build_packs.py --qrc /root/build/sc5000-lab/5.3.0/qrc` | PASS；en 279 文件、简/繁各 283 文件；每包 924/2772 活跃词典条目；临时 packs 和 tar.gz 已清理 |
| `tools/ui-mod/build.sh`，默认 armhf 交叉编译 | PASS，含目标 glibc 符号版本检查 |
| `CROSS= tools/ui-mod/build.sh`，arm64 shared hook | PASS |
| `gcc -O2 ... test_az04_hook.c -ldl -lpthread` 后运行，输入 SYSTEMONE 5.3.0 实体 Engine | PASS：新增 JobController 策略保护、资源映射/碰撞、LED、BuildID/18 编辑点、SQLite/DSP trampoline 验证 |
| `git diff --check` | PASS |
| 影子轨与文案/版本保护 | 14 个 `scShadow*` 函数与基线逐字一致（合并后 SHA256 `c33841edf019ed682c2407a9d3a5cb293ac9a7fef9418b14c2339ccff6f776ba`）；未改影子轨 QML、i18n.tsv、版本号、遥测默认值 |

正式发布脚本 `tools/az01-packer/build-release.sh` 会从源码重新构建两个架构的 hook；本分支未用本地验证产物覆盖仓库里历史预编译 hook。PM 组包应走该构建流程。

## 拿不准

1. **四核全部已有 RT 音频，没有完全不放 GUI/SceneGraph/RT 音频的第五个核。** “音频 RT 优先级与独立性不能变差”和“外设核完全无 RT 音频”无法同时满足。按 PM「不用等回答，按原则决定并记入报告」的指示，保留五个 RT 音频线程所有原有单核分配/优先级，选择 CPU3 承载外设 IRQ，并将非音频 IRQ 从 FIFO50 降到 FIFO40。CPU1/2 是尽量干净的 GUI/GL 核，仍有原有音频 RT 抢占；不能称为完全隔离。
2. **FIFO40 是保护音频的默认选择，真实外设延迟须复测。** 预计可减少非音频 IRQ 抢占音频，但网络、触摸、eMMC、GPU 完成通知会等到音频 RT 让出 CPU；检查触摸响应、网络吞吐、GPU 帧耗时以及 ALSA xrun。不可移动/managed/per-CPU IRQ 的亲和度写入可能被内核拒绝，脚本按原规则 best effort，不改变其 RT 音频策略。
3. **GC 根缩小不等于全部 GC 问题已消除。** 原影子轨高频采样/HTTP 路径按任务严格保留，仍可能贡献分配；未做实机 A/B 就不承诺 GC 周期或时长能达到某个数值。新 art 元数据绑定及按需重试，也应在播放、暂停换歌和慢元数据场景确认。
4. **线程亲和度的持久性要在实际工作流核对。** 原启动/延迟重扫覆盖 Engine 启动后创建的线程；需验证后续加载、流媒体重连、分析进程新建时，Engine 是否再次主动改写辅助线程掩码。若存在，应以新的实机采样作为后续持续策略的依据，避免未经验证改动更多音频调度路径。

## PM 部署后复测

由查ID 使用原 `runs/20261109-sc5000-stutter/` 的同一套采样脚本，在 L/R 相同固件、曲目、播放状态、时长和操作顺序下对比：

- **GC**：EMain 的 `QV4::MemoryManager::runGC` 样本占比、尖峰间隔、每次停顿时长、p95/p99。基线 L 7.75 s / R 9.85 s，约 110–150 ms；分别测全暂停、单轨播放和多轨播放。预期尖峰变少/变短，数值以复测为准。
- **调度**：EMain/SceneGraph 的 `Δnonvoluntary_ctxt_switches / Δ秒`，与原 0.25 s/60 s 窗口及抢占分析对齐；另看 GUI 帧间隔和 SceneGraph 离开 CPU 的时长，不能把切换计数直接等同于耗时。预期删除无效 EMain 辅助线程和外设竞争后下降。
- **负载**：1/5/15 分钟 load average、各核 busy/idle/IRQ、D 状态 workqueue 数量；同样比较空闲/播放/触摸/浏览/加载场景，避免把 D 状态负载下降误认为单纯 CPU 利用率下降。
- **功能与音频**：JobController 正常搜索/加载任务的策略、nice、I/O 类别和 queue length；五个音频线程的原优先级/亲和度、ALSA xrun；触摸/eMMC/eth0/USB 实际 IRQ mask，panfrost/VOP 在 CPU2，workqueue mask9；暂停后的 cue 不周期跳动，暂停换曲的封面能补齐，三语即时切换文案一致，ConnectedSource 无每 3 s 周期重建。

## 可恢复备份与采样

修改前 WebDAV 备份：

- `20261108T220823Z-Oracle-Batam-arm-before-perf-gc-affinity-20261109-83f83cb5d516-sc5000-perf-20261109-before.tar.gz`
- `20261108T221613Z-Oracle-Batam-arm-before-perf-ui-assertions-20261109-4e895fc658da-test_ui_mod.py`

本任务只读采样、验证摘要和审查结果交付到 `fleet-share` 的 `runs/20261109-sc5000-perf-fix/`。没有推送 main、创建 tag/Release/评论、刷机或向碟机部署。
