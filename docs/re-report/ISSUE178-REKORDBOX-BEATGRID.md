# #178：Rekordbox 节拍网格偏移调查

2026-11-10；基线 `2e3a88bd14f681d45df8f92ed84b60c5b932ecc9`。
[Issue #178](https://github.com/Cd1s/EngineX-OS/issues/178) 没有样本、文件格式、版本、偏移方向或大小，读取时没有评论。

## 结论与改动决定

**尚不能确认 #178 的根因，因此不改时间补偿代码。** 本分支只提交调查记录。

- **实证**：同一点击音轨的有效 LAME 头与无头 MP3，在原厂 Engine OS 5.3.0
  OfflineAnalyzer 中具有不同时间零点；带头版本与 WAV 的波形峰值落在相同 bin，
  无头版本偏后。原厂 trackData 的样本总数也随 gapless 信息改变。
- **推断**：若 Rekordbox 的对应导出使用另一种零点，直接复制时间会产生固定偏移。
  公开转换器有这种历史实测和分情况修正，支持该机制值得调查。
- **未证实**：报告人受影响的文件是否为 MP3、当前 Rekordbox 使用哪个零点、
  是否走了完整桥接导入，以及碟机实际播放解码是否与本次离线分析一致。
  本次没有生成真实 Rekordbox 分析导出，也没有连接实机。

不能把“代码没有补偿”当作根因证明，不能仅见 `.mp3` 或 `LAME` 字样就统一加减 26 ms。
尤其要区分 1105 个样本的起点差与 1152 个样本的 MPEG 帧长：44.1 kHz 下分别约
25.057 和 26.122 ms，48 kHz 下分别约 23.021 和 24 ms。

## 转换路径及其它可能来源

| 检查项 | 现有行为与判断 |
| --- | --- |
| Rekordbox 单位 | [analysis.go](../../pkg/rekordbox/analysis.go) 的 PQTZ、PCPT、PCP2 时间均直接读 BE uint32 毫秒；PQTZ BPM 除以 100。没有额外的 CD 帧换算。 |
| 采样率 | [sync.go](../../pkg/enginedb/sync.go) 的 `readSources` 读 ANLZ 与 `compat.AudioHeader`；WAV/FLAC/AIFF 头可覆盖导出采样率。MP3 返回空 AudioInfo，沿用 PDB/SQLite 导出值；缺失时 `EncodePerformance` 回退 44100。错误/缺失采样率仍需用实际导出排查，通常表现为随位置增长的误差。 |
| 总样本数 / 波形刻度 | MP3 没有容器头校正时，样本总数通常由 `Duration*sr` 推出；PDB 的 Duration 是整秒。它可能改变导入 overview 的时间刻度，仍需与实际 reader 的样本总数核对；这与所有 cue/拍点统一平移不同。尾部 padding 影响总长度，不能直接当成起点补偿。 |
| Cue / loop | [performance.go](../../pkg/enginedb/performance.go) 的 Hot Cue、main cue、loop 起止都用 `float64(ms)*sr/1000`。没有样本整数取整，也没有 MP3 条件或固定平移。 |
| 第一拍 | `beatPositions` 保留每个 BPM 段首拍的时间；`Number-1` 只参与拍序号，不从时间减去一拍。 |
| 毫秒精度、恒定 BPM | 源时间精度为整毫秒。`beatPositions` 用 BPM 等距化同 BPM 段；若累计末拍误差超过 1 ms，改用首末时间求步长。[既有测试](../../pkg/enginedb/beatgrid_test.go) 覆盖 130 BPM、0.01 BPM 精度造成的长曲漂移、变速段重新定位及未知 BPM。正常恒速段的毫秒量化不能解释几十毫秒的整体平移。 |
| 非匀速网格 | 相同 BPM 数值的整段会被等距化；若真实导出在同样 BPM 数值下包含局部移动，它可能改变中间拍点。没有受影响 ANLZ，不能排除此来源；不宣称所有可变 BPM 网格均已验证。 |
| 分析文件合并 | DAT/EXT/2EX 中后读到的非空 PQTZ 可替换前者；PQT2 没有被解释为网格。应保留对应原始分析文件核对，不能用合成 fixture 证明报告人的导出正确。 |
| 快速导入 / 降级 | [bridge.go](../../pkg/bridge/bridge.go) 的大库挂载快速路径（超过 500 首）、读取预算耗尽或读取失败降级，可能不写 PerformanceData，让 Engine 加载时自行分析。需要先确认看到的是复制网格还是 Engine 重算结果。 |
| 已有缓存 | 未变的 stamp / fingerprint 可跳过重导入。若将来证实并修复，必须验证既有导入的更新，不能只测一个新建库。 |

这些是代码事实和剩余排查边界；没有任何一项已被绑定到报告人的文件。
若以后证实需要修正，须覆盖两个 beat grid、Hot/main cue 和 loop 两端，保留源 ANLZ 时间，
并验证既有库刷新；本次没有实现这种尚未定条件、方向和大小的修正。

## 点击音轨与编码器延迟实测

### 方法和控制

IN 生成 8 秒、16-bit mono PCM，44.1/48 kHz 各一份，14 个点击在
0.5、1.0、…、7.0 秒，首样本具有唯一最大值。LAME 3.100 编码：

```sh
lame --silent --cbr -b 192 click-44100.wav click-44100-lame.mp3
lame --silent --cbr -b 192 -t click-44100.wav click-44100-no-tag.mp3
```

48 kHz 同样处理。脚本断言 Info/LAME 头存在或缺失，并断言去掉头帧之后，
带头与 `-t` 版本的所有压缩音频字节完全相同（头帧分别 626、576 字节）。
两个 LAME 头的 encoder delay 都是 576 样本，padding 分别为 1440、1344。
另把 44.1 kHz 文件的 delay/padding 三字节清零，保持音频帧相同；**此控制的 LAME CRC
随之失效**，不能称为有效零延迟编码文件。

IN 上 FFmpeg **7.1.5** 解码为 PCM，对每个点击查峰值，14 个偏移逐样本相同：

```sh
ffmpeg -v error -i click-44100-lame.mp3 -f s16le -acodec pcm_s16le decoded.pcm
```

对每个预期点击前 50 ms 至后 100 ms 的窗口取最大绝对值的 S16 样本位置。

| 采样率 | 有效 LAME 头：首峰样本 / 总样本 | 无头：首峰样本 / 总样本 | 无头相对带头 |
| --- | --- | --- | --- |
| 44100 | 22051 / 352800 | 23156 / 354816 | +1105 样本，25.056689 ms |
| 48000 | 24001 / 384000 | 25106 / 385920 | +1105 样本，23.020833 ms |

源首点击在样本 22050 / 24000；压缩后峰值本身有共同的 1 样本变化。
清零三字节的控制相对有效头推后 576 样本。**这些逐样本数值属于 FFmpeg 7.1 控制组，
不当成原厂 Engine 的逐样本测量。**

### Engine 原厂结果

[4.3.4](ENGINE-4.3.4-needed.txt) 和 [5.0.4](ENGINE-5.0.4-needed.txt) 的依赖列表均有
`libavformat.so.58`、`libavcodec.so.58`、`libavutil.so.56`、`libswresample.so.3`；
仅凭这些列表不能确定延迟处理。

实际在 **Oracle-Batam-arm** 运行原厂 **5.3.0 armhf OfflineAnalyzer**，SHA-256：

```text
2f328a9c87f96bc4e582ea4d382b0a20f56aad9cab7ab9424cd0eb3d943e4ff4
```

它同样依赖上述 FFmpeg SONAME。
使用 qemu-arm 及原厂库，只给本地 loopback worker 发送分析请求；没有运行整套 Engine、
打开用户曲库或使用实机。实验 LD_PRELOAD 仅复制 `BeatDetectionRunner` 的原始 Qt 输出，
并在完成后退出，不替换解码器、音频或分析结果。

该版本请求头是 `<IIQII>`：reserved、command、payload 字节数、payload CRC32、reserved；
command=1。payload 为 16 字节偏好首部加 UTF-8 音频路径。
本批统一使用十六进制首部 `50000000a00000000101000000000000`（前两个小端整数 80/160），
不把全部偏好位的业务语义当成已逆向结论。早期零首部超时的尝试不计入成功结果。

7 次正式分析都收到完整 trackData 和三频段 overview，正常 finished；所有 overview
分块的声明长度、采样率、samples-per-bin 与拼接后的 1024 bins 通过自检。
未要求 RGB 信号：三频段信号 10 已包含本次测量所需的时间和幅值。
信号 9 的原始节拍块一并保留，未解读其字段，不作为网格正确性或精确补偿的证据。

| 采样率 / 格式 | 原厂最终样本总数 | overview 首点击峰值位置（ms） |
| --- | --- | --- |
| 44100 WAV | 352800 | 492.188 |
| 44100 MP3，有效 LAME 头 | 352800 | 492.188 |
| 44100 MP3，无头 | 354816 | 518.571 |
| 48000 WAV | 384000 | 492.188 |
| 48000 MP3，有效 LAME 头 | 384000 | 492.188 |
| 48000 MP3，无头 | 385920 | 518.203 |
| 44100 MP3，delay/padding 清零、CRC 失效 | 354287 | 509.953 |

这里测的是**经过原厂滤波和量化的 overview 峰值**，不是逐样本 PCM 起音点；WAV 的
492.188 ms 不能解释成解码器把源 500 ms 起音提前了 7.812 ms。
WAV/有效头的 bin 宽 7.8125 ms，无头约 7.85 ms。对全部 14 拍，无头相对 WAV 的
峰值差范围为 44.1 kHz **19.241–31.384 ms**、48 kHz **11.797–32.812 ms**。
它们与控制组预测的 1105/sr 相容（每拍误差小于两个 bin），但这个分辨率也不能排除
1152 样本，**不能据此给 Engine 宣称精确的样本修正量**。总样本数则是原厂 trackData 的整数实测。

因此，本批有效 LAME 头版本的 overview 峰值和 trackData 总样本数与 WAV 一致；无头和失效头不能一概而论。
本批未在原厂分析器中测试 FLAC/AIFF，也没有证明实际播放 reader 的寻址行为。

## Rekordbox 一侧的公开证据与限制

- [Mixxx 当前导入源码，固定 commit](https://github.com/mixxxdj/mixxx/blob/489b47a99d7841a27da9c5b7292907bbd1bda3c5/src/library/rekordbox/rekordboxfeature.cpp#L1228)：
  使用 mp3guessenc 分类；FFmpeg 分支只对 case D 减 26 ms，统一用于 cue、loop 和网格。
  [实际减法及应用点](https://github.com/mixxxdj/mixxx/blob/489b47a99d7841a27da9c5b7292907bbd1bda3c5/src/library/rekordbox/rekordboxfeature.cpp#L920)。
  这是 Mixxx 的映射，不是 Engine 的已验证规格。
- [分类源码](https://github.com/mixxxdj/mixxx/blob/489b47a99d7841a27da9c5b7292907bbd1bda3c5/lib/mp3guessenc-0.27.4/tags.c#L515)：
  有效 LAME 为 D，无头为 A，失效 LAME CRC 为 C。本批生成文件经该版本分类器实跑，结果分别为 D/A/C。
- [2019 年直接 Rekordbox/Mixxx 比较](https://github.com/mixxxdj/mixxx/pull/2119#issuecomment-495988292)
  与[后续 FFmpeg 4.1 分类实测](https://github.com/mixxxdj/mixxx/pull/2119#issuecomment-533952875)
  说明差异随解码后端变化；[研究者也明确区分 Traktor→Rekordbox 与其它转换](https://github.com/mixxxdj/mixxx/pull/2119#issuecomment-531777185)。

这些支持“这种跨软件偏移存在”的历史证据；**当前 Rekordbox 版本对本批文件以及报告人
文件的实际时间仍未证实**。没有把历史的 -26 ms 或另一方向的转换公式写入本项目。

## 验证和可复现产物

本次没有改 Go 代码。任务书命令在 Batam 执行两次：

```sh
REMOTE_BUILD_HOST=Oracle-Batam-arm remote-build --name sc5000-S178-20261110 -- \
  'gofmt -l . && go vet ./... && go test -race ./pkg/... ./internal/...'
```

- 两次 gofmt 均无输出，go vet 均通过；bridge、rekordbox、enginedb、compat 及其它相关包通过。
- **全量 race 检查未通过**：首次 `pkg/webui.TestTailscaleLoginServeAndStop` 在
  `tailscale_test.go:172` / `:32` 报竞态，且 `TestTunnelQuickAndNamedModes` 在
  `tunnel_test.go:97` 读到 `installing` 状态失败；第二次仍有后者失败。
- 上述两用例单独 `go test -race ./pkg/webui -run 'Test(TailscaleLoginServeAndStop|TunnelQuickAndNamedModes)$' -count=3`
  通过，不能抹去全量失败。两次全量运行都在未改 Go 源码的基线进行；没有顺手修改无关测试。
- 点击生成、音频帧同一性、7 次原厂完成结果和波形拼接/测量自检通过。

原始文件、原厂 Qt 输出、脚本、摘要、测试日志和逐文件 SHA-256 manifest 已存远程备份库：

```text
backup id:
20261109T190455Z-hostdzire-in-16c32g-S178-rekordbox-beatgrid-evidence-3098c6a4f2e0-S178-evidence.tar.gz
archive sha256:
3098c6a4f2e0ead7f36bda799185823ad49a6e5882000285fc4b16f77115dacb
```

产物不含原厂二进制、密钥或用户音频。`measure.py` 可直接对归档内的原始分块重算表格。
`make_audio.py` 重建点击，要求同目录的 `lame/usr/bin/lame`；可在 IN 的独立 build 目录
用 `apt download lame`、`dpkg-deb -x` 提取，不安装到系统。
`probe.c` 用 `arm-linux-gnueabihf-gcc -shared -fPIC -O2 -o probe.so probe.c -ldl` 在 IN 编译，
把 probe、音轨与 `stock-server.py` 放到脚本指定的 Batam 私有 build 目录，再逐个文件执行
`python3 stock-server.py click-44100-lame.mp3`。脚本绑定已记录的 5.3.0 路径和 Qt 元对象布局，
仅用于本次实验，换原厂版本必须重新核对哈希和布局。

## 需要报告人补充

1. 受影响与正常曲目各一个原始文件；是否为 MP3，编码器、采样率、CBR/VBR，是否编辑过头部。
   不要重新编码样本，否则会改变本次要查的延迟信息。
2. Rekordbox 版本、分析模式，EngineX/Engine OS 版本和机型。
3. 同一曲目的 `export.pdb` / OneLibrary 元数据及对应 DAT、EXT、2EX；可只提供能复现的最小导出。
4. 偏早还是偏晚，开头/中间/结尾各偏多少；cue 与 loop 是否同样偏移，还是只有拍线不对。
5. 导入是默认挂载快速模式还是 `--full`，是否曾手调网格或让 Engine 重新分析。
6. 用同一点击测试文件在当前 Rekordbox 导出拍点/cue/loop，并与 Engine 原生分析及桥接结果对照。
   只有建立这组共同时间基准，才决定受影响条件、补偿方向和数值。
