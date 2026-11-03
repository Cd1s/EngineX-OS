# HOT CUE / Stems 现场诊断 — 2026-11-03

## 当前用户要求

- 双按 HOT CUE 无法切到 Stems；点击 Stems 后曲目加载异常，列表中的歌曲短暂出现后消失。
- 网易云、Spotify、Deezer 继续使用现有 Deemix 优先取音，新增计算机端预下载、分析与分轨，再交回碟机。
- 6.1 Sol 负责设备行为分析；Opus 5.5 只按已确认的开发规格实现。

## 实机证据

目标 SC5000 / JP07，Engine SHA-256：
`342b611bd71fd09d9fda8013f19dccfba47ee4f0f772a959058bdd19d74d5413`。

在旧进程 PID25208 下按已核对的对象布局只读 `/proc/25208/mem`：
最终 Stems 播放可用字节为 `00`。没有写内存或执行模式/授权 setter。

用户随后明确允许重启一次 Engine 加载只读诊断 UI。
实际停止发生在 **2026-11-03 22:58:21 +07**，新 Engine PID5952 启动于约22:58:25。
3分钟诊断 Timer 从22:58:40起只记录状态变化，然后自动停止。

| 读取项 | 现场值 |
| --- | --- |
| `Config.device.isJP07` | true |
| primary pad modes | CUES / LOOPS / ROLL / SLICER |
| secondary pad modes | STEMS / AUTO / SAMPLER / FIXED |
| pad selector type | Rgb |
| embedded rendering capability | true |
| `Planck.hasStemsPreRenderedPlayback()` | false，全部采样一致 |
| Deck1 当前曲目 | A day by atmosphere supreme（3117） |
| `SongLoaded` | true |
| `Track/Stemmed` | false |
| `Pads/View` | CUES |

用户确认已经按两次 HOT CUE、没有切换；同一采样窗口没有出现 STEMS 模式。
后续切换到 Molly、Everyday、再回到3117时，原曲均能处于 SongLoaded=true，
Stemmed仍为false、Pad仍为CUES。没有把这些普通音频加载成功当作分轨加载成功。

当前 USB `m.db` 前四页的 Information UUID为
`157073fc-71ee-4995-9980-777ebb41b3ad`；3117 `.stems` 文件名使用同一UUID，
文件19310922字节、mtime22:22。这个有限核查没有发现明显库UUID漂移，
也不替代容器解码与四轨听感验收。

## 可下结论与尚未闭合项

- 正确的原厂二次按键映射已经存在。分轨渲染入口和分轨播放可用状态是两项独立条件。
- 当前播放可用值false、当前轨道未发布为Stemmed，与Pad资格检查拒绝切换相符。
  不能仅靠显示一个按钮或改同键映射修复完整链路。
- **尚未证明**当前false的全部运行时来源、生成的加密容器可用性、列表刷新后曲目消失的直接原因。
  不将这几项写成同一个已修好的问题，也不由Stemmed=false单独认定文件损坏。
- 当前部署的顶部Stems入口是先前的排序替代入口；真正的列表选择、查询条件与加载链由Sol继续核对。

## 诊断变更已收尾

临时UI只读现有Planck/Config属性，不改模式、曲目或授权值，不包含eval入口。
IN上使用实际探针函数检查缺失属性与false的区分，以及无属性赋值；实机加载正常。

前置远端备份：
`20261103T155517Z-maintainer-laptop-sc5000-before-readonly-hotcue-probe-20261103-23c9b8938eeb-sc5000-hotcue-diagnostic-ui-prestate.tar.gz`。

存盘UI现已恢复原SHA-256：
`d0d405f9f9ce96b95d14a263c795dc4c3662b36799ac641674d700f9d543d095`。
恢复时未再次重启，Engine仍为5952，NRestarts=0；当前内存中的探针会按180秒期限停下。
原始日志和完整状态快照暂存 `/tmp/sc5000-hotcue-diagnostic-20261103/`，没有写入用户曲库。

旧Opus报告的20分钟播放基线涵盖此次有意重启。t≈983秒的XRUN/CLOSED/OPEN及PID切换
必须标成维护中断；不能把它当自然卡音。其他时间的欠载报告仍需单独分析。

## 在执行的工作

- Sol设备分析：IN agent `e098b37e-f903-40f7-b5db-2e003edbd872`，独立目录
  `/root/build/sc5000-compat-20261103-rea-stems`。
- 远端Deemix准备API草案：IN agent `6ef79e59-8efc-4b32-ad28-bf0e17a41737`，
  `/root/build/sc5000-online-stems-20261103`，尚未部署。
- 用户指定的新Opus5.5开发agent：`f36a7737-18c5-4acf-8eb0-57a1fcf894bf`，
  Paseo workspace `wks_9afea3bdd0d9c240`，独立分支 `feat/stems-developer-20261103`。
  已交付现有协议/离线Jobs代码与真实运行值，先做部署准备和确定的问题修复，后续按冻结接口集成。

四个独立FLAC的离线原型已通过IN测试；它不代表原厂Stems控制已完成。

## 开发交接进展

Deemix准备API草案已完成受影响包race与5个程序ARMv7交叉编译。
包含鉴权后的远端取音、分析、分轨、SHA校验、取消和碟机导入调用，未部署。
14个源码/测试文件及交接记录已冻结提取到
`/tmp/sc5000-online-source-20261103`，交给新Opus在独立工作树合并，保留其已做的Jobs容量/淘汰修复。

合并前已发现的产品问题：草案会让普通acquire等待分轨并下载六个文件，
既拖慢普通播放，也可能使超出分析上限的长曲无法沿用原播放链。
Opus接续规格改为明确触发/后台准备，普通播放继续原Deemix优先路径；
同时补准备成果缓存的容量/余量约束，不把缓存淘汰应用到已导入用户文件。

设备侧新增已确认事实：现有顶部Header只清选中项并延迟排序，
没有执行原厂Stems集合选择或歌曲加载。GET_STEMS_STATUS的完成状态按
origin track identity/context路径与完成文件查找，也不检查四轨解码；
列表状态3、文件存在与播放Feature=false可以同时成立。
对名单消失和加载阻塞仍缺集合身份及load job时间线，尚未宣称修复。

## 后续并发基线变化

23:11另一个旧会话重新生成、部署BPM UI并将主分支推进到 `f209238`。
23:16只读核查显示 Engine PID8067、service MainPID7954、ExecMainStart23:11:10，
zh-Hant EmbeddedMain变为SHA-256
`bc3752f456955af230a513ffd3cd69807d011165b23621bb75b4b5325cfb6a8f`。
这发生在本次恢复诊断源文件之后；本次临时文件清理没有覆盖该文件或重启服务。
新Opus已获知要保留此新基线；前面的PID5952和恢复哈希记录仅对应当时的已验证窗口。
