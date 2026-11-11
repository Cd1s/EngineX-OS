/* EngineX Mod Hub（enginexdj.com/#mods）：官方 registry + GitHub `mod` Issue 社区 + 本机预览。
   纯前端：浏览器里解包 / 打包 / 校验 .tar.gz，评论走 GitHub Issue。所有外部数据一律转义后输出。 */
(function () {
  const GH = "https://github.com/Cd1s/EngineX-OS";
  const API = "https://api.github.com/repos/Cd1s/EngineX-OS";
  const REGISTRY = "mods/registry.json";
  const LS_LOCAL = "enginex-mods-local", LS_REV = "enginex-mods-reviews", LS_CACHE = "enginex-mods-gh:";
  const CACHE_MS = 10 * 60 * 1000; // 未登录 GitHub API 每小时 60 次，列表与评论缓存 10 分钟
  const MAX_PKG = 50 * 1024 * 1024, MAX_KEEP = 1536 * 1024; // 上传上限；超过 1.5 MB 的包不存 localStorage
  const ID_RE = /^[a-zA-Z0-9][a-zA-Z0-9._-]{0,63}$/; // 与 pkg/mods.idPattern 相同
  const CATS = ["all", "official", "network", "lighting", "audio", "midi", "ui", "community"];

  const L = (s, t, e) => ({ s, t, e });
  const ALL16 = L("全部 16 款（一代 armhf / 二代 armhf / AZ04 arm64）", "全部 16 款（一代 armhf / 二代 armhf / AZ04 arm64）", "All 16 models (gen-1 armhf / gen-2 armhf / AZ04 arm64)");
  const pkg = (id, v, arch) => ({ label: arch || "", url: `mods/packages/${id}-${v}${arch ? "-" + arch : ""}.tar.gz` });
  // 离线 / registry.json 加载失败时的兜底；registry 里同 id 的字段会覆盖这里
  const OFFICIAL = [
    { id: "tailscale-remote", name: "Tailscale Remote", version: "1.104.5", author: "EngineX OS", category: "network", issues: [130],
      description: L("Tailscale / Headscale 异地组网：在外地或跨网段安全打开碟机 `:8080` Web 控制台、SSH 与远程曲库。安装后在 Web 控制台 **Mods → Tailscale Remote** 一键开启并扫码登录。",
        "Tailscale / Headscale 異地組網：在外地或跨網段安全開啟碟機 `:8080` Web 主控台、SSH 與遠端曲庫。安裝後在 Web 主控台 **Mods → Tailscale Remote** 一鍵開啟並掃碼登入。",
        "Tailscale / Headscale mesh networking: reach the deck's `:8080` web console, SSH and remote libraries from anywhere. After installing, enable it in the web console under **Mods → Tailscale Remote** and scan to log in."),
      models: L("全部 16 款：一代与二代 armhf 用 32 位包，PRIME 4 G2 / SYSTEM ONE 用 64 位 arm64 包", "全部 16 款：一代與二代 armhf 用 32 位元包，PRIME 4 G2 / SYSTEM ONE 用 64 位元 arm64 包", "All 16 models: 32-bit armhf package for gen-1 and armhf gen-2, 64-bit arm64 package for PRIME 4 G2 / SYSTEM ONE"),
      packages: [pkg("tailscale-remote", "1.104.5")] },
    { id: "dmx-studio", name: "DMX Studio", version: "1.1.0", author: "EngineX OS", category: "lighting", issues: [134],
      description: L("免电脑的独立多域 DMX512 控灯台：支持 FTDI / uDMX USB 接口与 Art-Net，摇头灯 EFX 轨迹引擎、烟雾机控制与 2D 灯位监视器，随 BPM 自动走灯。",
        "免電腦的獨立多域 DMX512 控燈台：支援 FTDI / uDMX USB 介面與 Art-Net，搖頭燈 EFX 軌跡引擎、煙霧機控制與 2D 燈位監視器，隨 BPM 自動走燈。",
        "Computer-free multi-universe DMX512 console: FTDI / uDMX USB interfaces and Art-Net, a moving-head EFX path engine, fog machine control and a 2D fixture monitor that follows the BPM."),
      models: ALL16, packages: [pkg("dmx-studio", "1.1.0")] },
    { id: "jog-logo-studio", name: "Jog Logo Studio", version: "1.1.0", author: "EngineX OS", category: "ui", issues: [137],
      description: L("转盘中心屏自定义 Logo：上传图片后一键裁切、预览并同步到碟机转盘屏。", "轉盤中心屏自訂 Logo：上傳圖片後一鍵裁切、預覽並同步到碟機轉盤屏。", "Custom jog-wheel display logo: upload an image, crop and preview it, then sync it to the jog screen in one click."),
      models: L("带转盘中心屏的机型（SC6000 / SC6000M 等）", "帶轉盤中心屏的機型（SC6000 / SC6000M 等）", "Models with a jog-wheel display (SC6000 / SC6000M and others)"), packages: [pkg("jog-logo-studio", "1.1.0")] },
    { id: "sampler-pad-studio", name: "Sampler Pad Studio", version: "1.1.0", author: "EngineX OS", category: "audio", issues: [137, 120],
      description: L("8 格采样器打击垫可视化管理：拖拽分配采样、Loop 切片与音量预览。", "8 格取樣器打擊墊視覺化管理：拖曳分配取樣、Loop 切片與音量預覽。", "Visual manager for the 8 sampler pads: drag samples onto pads, slice loops and preview levels."),
      models: ALL16, packages: [pkg("sampler-pad-studio", "1.1.0")] },
    { id: "minimal-club-mode", name: "Minimal Club Mode", version: "1.1.0", author: "EngineX OS", category: "ui", issues: [136],
      description: L("俱乐部演出纯净模式：隐藏演出时用不到的菜单与提示，只留波形、曲库和关键控制。", "俱樂部演出純淨模式：隱藏演出時用不到的選單與提示，只留波形、曲庫與關鍵控制。", "Club performance mode: hides menus and prompts you do not need on stage, leaving waveforms, the library and key controls."),
      models: ALL16, packages: [pkg("minimal-club-mode", "1.1.0")] },
    { id: "midi-controller-mapper", name: "Generic MIDI Controller Mapper", version: "1.1.0", author: "EngineX OS", category: "midi", issues: [137],
      description: L("外部 USB MIDI 控制器映射器（首发支持 Pioneer CDJ-400）：低优先级 (nice 19) ALSA Sequencer helper 与 60 Hz QML 桥接，将外部控制器直接映射到四轨模式下的内部 Deck 3/4，含连续速度转盘积分器 (64=停止)、带 ▲▼ 指示的变速推子软接管及热拔插自动停用保护。",
        "外部 USB MIDI 控制器映射器（首發支援 Pioneer CDJ-400）：低優先級 (nice 19) ALSA Sequencer helper 與 60 Hz QML 橋接，將外部控制器直接映射至四軌模式下的內部 Deck 3/4，含連續速度轉盤積分器 (64=停止)、帶 ▲▼ 指示的變速推桿軟接管及熱拔插自動停用保護。",
        "External USB MIDI controller mapper (Pioneer CDJ-400 first): low-priority (nice 19) ALSA Sequencer helper and 60 Hz QML bridge mapping external controllers directly to internal Deck 3/4 in 4-deck mode, with continuous velocity jog time-integration (64=stop), tempo soft takeover with ▲▼ indicators, and hot-unplug auto-disable."),
      models: ALL16, packages: [pkg("midi-controller-mapper", "1.1.0")] },
  ].map((m) => ({ ...m, official: true }));

  const UI = {
    s: { title: "Mod 社区", lead: "开源 Mod 的下载、上传与评价。所有包都公开源码，碟机安装前会再校验一次。", upload: "+ 上传开源 Mod",
      archT: "两层架构", arch1T: "内置于系统", arch1: "原厂能力（含原厂 Engine Lighting 解锁）、流媒体、Stems、DSP 防护直接装在 EngineX OS 里，不需要额外安装。",
      arch2T: "开源 Mod 按需安装", arch2: "Tailscale 远程访问、专业多域 DMX 控灯台等大型插件做成 Mod，需要时再装；未启用时 0 内存占用。",
      cats: { all: "全部", official: "官方", network: "网络与远程", lighting: "舞台灯光", audio: "音频与采样", midi: "MIDI 控制器", ui: "界面与转盘", community: "社区上传", other: "其它" },
      search: "搜索 Mod…", sort: { hot: "最热", rating: "最高评分", new: "最新上传" }, none: "没有匹配的 Mod", loading: "正在加载社区 Mod…", ghFail: "GitHub 暂时无法访问，只显示官方与本机 Mod",
      official: "官方", community: "社区", local: "本机预览", by: "作者", ver: "版本", models: "支持机型", sha: "SHA-256", files: "包内文件", hw: "实机验证", dl: "下载", dlPkg: "下载 .tar.gz", noPkg: "作者还没上传包，去 Issue 查看",
      install: "安装方式", inst1: "碟机与电脑在同一网络时，打开 `http://<碟机 IP>:8080` → **Mods**，把 `.tar.gz` 拖进去上传，启用后立即生效。", inst2: "或者把 `.tar.gz` 解压到 U 盘根目录的 `/mods/` 下（得到 `/mods/<id>/mod.json`），插上碟机即用。",
      issue: "GitHub 讨论", reviews: "评论与评分", noRev: "还没有评论，来写第一条。", ghRev: "来自 GitHub", you: "本机", name: "昵称", stars: "评分", text: "使用反馈（机型、版本、效果）",
      post: "发表评论", sync: "同步到 GitHub Issue", synced: "评论已复制到剪贴板，在打开的 Issue 页粘贴即可。", needText: "请选择星级并填写反馈。", rating: "评分", nRev: "条评价", close: "关闭",
      upT: "上传 / 发布开源 Mod", upA: "上传现有 .tar.gz", upB: "在线打包", drop: "把 `.tar.gz` 拖到这里，或点击选择文件", dropHint: "浏览器内解包校验，不会上传到任何服务器。",
      fId: "Mod ID", fName: "名称", fVer: "版本", fAuthor: "作者", fCat: "分类", fDesc: "描述", fFiles: "QML / 脚本 / 资源文件", fTarget: "替换的原厂 qrc 路径", fScript: "启动脚本", fAsset: "资源",
      pack: "打包并校验", ok: "校验通过", bad: "校验未通过", warn: "提醒", size: "大小",
      keep: "加入本机预览 & 下载 .tar.gz", submit: "提交到 GitHub 开源 Mod 仓库", submitHint: "会打开预填好的 Issue；请把刚下载的 `.tar.gz` 拖进 Issue 作为附件，社区才能下载。",
      safety: "浏览器只检查包结构、`mod.json` 与路径；碟机安装时 `pkg/mods` 还会完整校验一次（含 QML 语法）。Mod 在碟机上运行，只安装你信任的作者发布的包。", del: "移除本机预览" },
    t: { title: "Mod 社群", lead: "開源 Mod 的下載、上傳與評價。所有套件都公開原始碼，碟機安裝前會再校驗一次。", upload: "+ 上傳開源 Mod",
      archT: "兩層架構", arch1T: "內建於系統", arch1: "原廠能力（含原廠 Engine Lighting 解鎖）、串流、Stems、DSP 防護直接裝在 EngineX OS 裡，不需要額外安裝。",
      arch2T: "開源 Mod 按需安裝", arch2: "Tailscale 遠端存取、專業多域 DMX 控燈台等大型外掛做成 Mod，需要時再裝；未啟用時 0 記憶體占用。",
      cats: { all: "全部", official: "官方", network: "網路與遠端", lighting: "舞台燈光", audio: "音訊與取樣", midi: "MIDI 控制器", ui: "介面與轉盤", community: "社群上傳", other: "其他" },
      search: "搜尋 Mod…", sort: { hot: "最熱門", rating: "最高評分", new: "最新上傳" }, none: "沒有符合的 Mod", loading: "正在載入社群 Mod…", ghFail: "GitHub 暫時無法存取，只顯示官方與本機 Mod",
      official: "官方", community: "社群", local: "本機預覽", by: "作者", ver: "版本", models: "支援機型", sha: "SHA-256", files: "套件內檔案", hw: "實機驗證", dl: "下載", dlPkg: "下載 .tar.gz", noPkg: "作者尚未上傳套件，請到 Issue 查看",
      install: "安裝方式", inst1: "碟機與電腦在同一網路時，開啟 `http://<碟機 IP>:8080` → **Mods**，把 `.tar.gz` 拖進去上傳，啟用後立即生效。", inst2: "或者把 `.tar.gz` 解壓到隨身碟根目錄的 `/mods/` 下（得到 `/mods/<id>/mod.json`），插上碟機即用。",
      issue: "GitHub 討論", reviews: "評論與評分", noRev: "還沒有評論，來寫第一則。", ghRev: "來自 GitHub", you: "本機", name: "暱稱", stars: "評分", text: "使用回饋（機型、版本、效果）",
      post: "發表評論", sync: "同步到 GitHub Issue", synced: "評論已複製到剪貼簿，在開啟的 Issue 頁貼上即可。", needText: "請選擇星級並填寫回饋。", rating: "評分", nRev: "則評價", close: "關閉",
      upT: "上傳 / 發布開源 Mod", upA: "上傳現有 .tar.gz", upB: "線上打包", drop: "把 `.tar.gz` 拖到這裡，或點擊選擇檔案", dropHint: "在瀏覽器內解包校驗，不會上傳到任何伺服器。",
      fId: "Mod ID", fName: "名稱", fVer: "版本", fAuthor: "作者", fCat: "分類", fDesc: "描述", fFiles: "QML / 腳本 / 資源檔案", fTarget: "替換的原廠 qrc 路徑", fScript: "啟動腳本", fAsset: "資源",
      pack: "打包並校驗", ok: "校驗通過", bad: "校驗未通過", warn: "提醒", size: "大小",
      keep: "加入本機預覽 & 下載 .tar.gz", submit: "提交到 GitHub 開源 Mod 倉庫", submitHint: "會開啟預填好的 Issue；請把剛下載的 `.tar.gz` 拖進 Issue 作為附件，社群才能下載。",
      safety: "瀏覽器只檢查套件結構、`mod.json` 與路徑；碟機安裝時 `pkg/mods` 還會完整校驗一次（含 QML 語法）。Mod 在碟機上執行，只安裝你信任的作者發布的套件。", del: "移除本機預覽" },
    e: { title: "Mod Hub", lead: "Download, publish and review open-source mods. Every package ships its source and is validated again on the deck before install.", upload: "+ Upload Mod",
      archT: "Two layers", arch1T: "Built into the system", arch1: "Stock features (including the unlocked stock Engine Lighting), streaming, Stems and DSP guards ship inside EngineX OS with nothing extra to install.",
      arch2T: "Open-source mods on demand", arch2: "Large add-ons such as Tailscale remote access and the multi-universe DMX console are mods you install when you need them; disabled mods use 0 memory.",
      cats: { all: "All", official: "Official", network: "Network · Tailscale", lighting: "DMX & Lighting", audio: "Audio & Sampler", midi: "MIDI Controllers", ui: "UI & Jog", community: "Community", other: "Other" },
      search: "Search mods…", sort: { hot: "Most popular", rating: "Top rated", new: "Newest" }, none: "No mods match", loading: "Loading community mods…", ghFail: "GitHub is unreachable right now; showing official and local mods only",
      official: "Official", community: "Community", local: "Local preview", by: "Author", ver: "Version", models: "Models", sha: "SHA-256", files: "Files", hw: "Hardware validation", dl: "Download", dlPkg: "Download .tar.gz", noPkg: "No package attached yet; see the Issue",
      install: "How to install", inst1: "With the deck on your network, open `http://<deck IP>:8080` → **Mods** and drop the `.tar.gz` in; it takes effect once enabled.", inst2: "Or extract the `.tar.gz` into `/mods/` on a USB stick (giving `/mods/<id>/mod.json`) and plug it into the deck.",
      issue: "GitHub discussion", reviews: "Comments & Reviews", noRev: "No reviews yet. Be the first.", ghRev: "From GitHub", you: "This browser", name: "Name", stars: "Rating", text: "Your feedback (model, version, results)",
      post: "Post review", sync: "Sync to GitHub Issue", synced: "Review copied to the clipboard; paste it on the Issue page that just opened.", needText: "Pick a rating and write some feedback.", rating: "Rating", nRev: "reviews", close: "Close",
      upT: "Upload / publish a mod", upA: "Upload a .tar.gz", upB: "Package online", drop: "Drop a `.tar.gz` here or click to choose", dropHint: "Unpacked and checked in your browser; nothing is uploaded to a server.",
      fId: "Mod ID", fName: "Name", fVer: "Version", fAuthor: "Author", fCat: "Category", fDesc: "Description", fFiles: "QML / script / asset files", fTarget: "Stock qrc path it replaces", fScript: "Start script", fAsset: "Asset",
      pack: "Package & validate", ok: "Validation passed", bad: "Validation failed", warn: "Notes", size: "Size",
      keep: "Add to local preview & download .tar.gz", submit: "Submit to the GitHub mod repo", submitHint: "Opens a pre-filled Issue; drag the `.tar.gz` you just downloaded into it as an attachment so others can download it.",
      safety: "The browser checks the package layout, `mod.json` and paths; the deck's `pkg/mods` validates again on install (including QML syntax). Mods run on your deck: install only packages from authors you trust.", del: "Remove local preview" },
  };

  const $ = (s, r = document) => r.querySelector(s);
  const esc = (x) => String(x ?? "").replace(/[&<>"']/g, (c) => ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" }[c]));
  // 只展开 `代码` 与 **粗体**，先转义：描述可能来自社区 Issue
  const md = (x) => esc(x).replace(/`([^`]+)`/g, "<code>$1</code>").replace(/\*\*([^*]+)\*\*/g, "<b>$1</b>");
  const safeUrl = (u, base) => { try { const x = new URL(u, base || location.href); return /^https?:$/.test(x.protocol) || x.protocol === "file:" ? x.href : ""; } catch { return ""; } };
  const kb = (n) => (n < 1024 ? n + " B" : n < 1048576 ? (n / 1024).toFixed(1) + " KB" : (n / 1048576).toFixed(1) + " MB");
  const load = (k, d) => { try { return JSON.parse(localStorage.getItem(k)) ?? d; } catch { return d; } };
  const save = (k, v) => { try { localStorage.setItem(k, JSON.stringify(v)); return true; } catch { return false; } };

  /* ───────────── tar.gz：解包、打包、校验（纯函数，Node 22 也能跑） ───────────── */
  const dec = new TextDecoder(), enc = new TextEncoder();
  const pipe = async (buf, t) => new Uint8Array(await new Response(new Blob([buf]).stream().pipeThrough(t)).arrayBuffer());
  const gunzip = (buf) => pipe(buf, new DecompressionStream("gzip"));
  const gzip = (buf) => pipe(buf, new CompressionStream("gzip"));
  const sha256 = async (buf) => [...new Uint8Array(await crypto.subtle.digest("SHA-256", buf))].map((b) => b.toString(16).padStart(2, "0")).join("");

  function untar(u8) {
    const out = [];
    let off = 0, longName = null;
    const cstr = (a) => dec.decode(a).replace(/\0[\s\S]*$/, "");
    while (off + 512 <= u8.length) {
      const h = u8.subarray(off, off + 512);
      if (h.every((b) => b === 0)) break;
      let sum = 0;
      for (let i = 0; i < 512; i++) sum += i >= 148 && i < 156 ? 32 : h[i];
      if (sum !== parseInt(cstr(h.subarray(148, 156)).trim(), 8)) throw new Error("tar header checksum mismatch @" + off);
      const size = parseInt(cstr(h.subarray(124, 136)).trim() || "0", 8);
      const type = String.fromCharCode(h[156] || 48);
      let name = cstr(h.subarray(0, 100));
      const prefix = cstr(h.subarray(257, 263)).startsWith("ustar") ? cstr(h.subarray(345, 500)) : "";
      if (prefix) name = prefix + "/" + name;
      if (off + 512 + size > u8.length) throw new Error("truncated tar entry " + name);
      const data = u8.slice(off + 512, off + 512 + size);
      off += 512 + Math.ceil(size / 512) * 512;
      if (type === "L") { longName = cstr(data); continue; }
      if (type === "x") { const m = dec.decode(data).match(/(?:^|\n)\d+ path=([^\n]*)\n/); if (m) longName = m[1]; continue; }
      if (type === "g") continue;
      if (longName) { name = longName; longName = null; }
      out.push({ name: name.replace(/^\.\//, ""), type, size, data });
    }
    return out;
  }

  function tar(files) {
    const blocks = [];
    for (const f of files) {
      const h = new Uint8Array(512), nb = enc.encode(f.name);
      // shortcut: 不写 GNU 长文件名，路径超过 100 字节直接报错；Mod 内目录很浅，真遇到再补 'L' 头
      if (nb.length > 100) throw new Error("path too long (>100 bytes): " + f.name);
      const put = (s, o) => h.set(enc.encode(s), o);
      h.set(nb, 0);
      put(f.name.endsWith(".sh") ? "0000755\0" : "0000644\0", 100); put("0000000\0", 108); put("0000000\0", 116);
      put(f.data.length.toString(8).padStart(11, "0") + "\0", 124);
      put(Math.floor(Date.now() / 1000).toString(8).padStart(11, "0") + "\0", 136);
      put("        ", 148); put("0", 156); put("ustar\0", 257); put("00", 263);
      put(h.reduce((a, b) => a + b, 0).toString(8).padStart(6, "0") + "\0 ", 148);
      blocks.push(h, f.data, new Uint8Array((512 - (f.data.length % 512)) % 512));
    }
    blocks.push(new Uint8Array(1024));
    const out = new Uint8Array(blocks.reduce((a, b) => a + b.length, 0));
    let o = 0;
    for (const b of blocks) { out.set(b, o); o += b.length; }
    return out;
  }

  // 与 pkg/mods.isSafeRelPath 一致：非空、非绝对、没有 .. 段
  const safeRel = (p) => typeof p === "string" && p !== "" && !/^[\\/]/.test(p) && !/^[a-zA-Z]:/.test(p) && !p.replace(/\\/g, "/").split("/").includes("..");

  // 返回 { ok, errors, warnings, manifest, root, files }
  function validate(entries) {
    const errors = [], warnings = [];
    for (const e of entries) {
      if (!safeRel(e.name)) errors.push("unsafe path in archive: " + e.name);
      if (e.type === "1" || e.type === "2") errors.push("links are not allowed: " + e.name);
    }
    const files = entries.filter((e) => e.type === "0" || e.type === "\0" || e.type === "7");
    const mj = files.filter((e) => /^([^/]+\/)?mod\.json$/.test(e.name)).sort((a, b) => a.name.length - b.name.length)[0];
    if (!mj) return { ok: false, errors: [...errors, "mod.json not found (expected mod.json or <id>/mod.json)"], warnings, files };
    const root = mj.name.slice(0, -"mod.json".length);
    let m;
    try { m = JSON.parse(dec.decode(mj.data)); } catch (err) { errors.push("mod.json is not valid JSON: " + err.message); }
    if (m && (typeof m !== "object" || Array.isArray(m))) { errors.push("mod.json must be an object"); m = null; }
    if (m) {
      const has = (p) => files.some((f) => f.name === root + p.replace(/^\.\//, ""));
      if (!ID_RE.test(m.id || "")) errors.push(`invalid id "${m.id ?? ""}": must match ${ID_RE.source}`);
      for (const k of ["name", "version"]) if (typeof m[k] !== "string" || !m[k].trim()) errors.push(`"${k}" is required`);
      if (!m.author) warnings.push('"author" is empty');
      if (!m.description) warnings.push('"description" is empty');
      const qml = m.qmlFiles && typeof m.qmlFiles === "object" && !Array.isArray(m.qmlFiles) ? m.qmlFiles : {};
      if (m.qmlFiles !== undefined && qml !== m.qmlFiles) errors.push('"qmlFiles" must be an object { "src.qml": "qrc/target.qml" }');
      if (!Object.keys(qml).length && !m.script) errors.push('mod must define "qmlFiles" or "script"');
      for (const [src, dst] of Object.entries(qml)) {
        if (!safeRel(src)) errors.push(`unsafe QML source "${src}"`);
        else if (!has(src)) errors.push(`QML source missing from package: ${src}`);
        if (!safeRel(dst)) errors.push(`unsafe QML target "${dst}"`);
        if (!/\.qml$/i.test(src)) warnings.push(`QML source does not end in .qml: ${src}`);
      }
      if (m.script) {
        if (!safeRel(m.script)) errors.push(`unsafe script path "${m.script}"`);
        else if (!has(m.script)) errors.push(`script missing from package: ${m.script}`);
      }
      if (root && m.id && root !== m.id + "/") warnings.push(`top folder "${root}" differs from id "${m.id}"; the deck uses /mods/<folder>/`);
    }
    return { ok: !errors.length, errors, warnings, manifest: m, root, files };
  }

  async function inspect(gzBuf) {
    const sha = await sha256(gzBuf);
    let entries;
    try { entries = untar(await gunzip(gzBuf)); } catch (err) { return { ok: false, errors: ["not a valid .tar.gz: " + err.message], warnings: [], files: [], sha, size: gzBuf.byteLength }; }
    return { ...validate(entries), sha, size: gzBuf.byteLength };
  }

  async function build(manifest, userFiles) {
    const dir = manifest.id + "/";
    const files = [{ name: dir + "mod.json", data: enc.encode(JSON.stringify(manifest, null, 2) + "\n") }, ...userFiles.map((f) => ({ name: dir + f.name, data: f.data }))];
    const gz = await gzip(tar(files));
    return { gz, report: await inspect(gz) };
  }

  if (typeof document === "undefined") { module.exports = { untar, tar, gzip, gunzip, sha256, validate, inspect, build, safeRel }; return; }

  /* ───────────── 数据：registry + GitHub + localStorage ───────────── */
  const starOf = (body) => { const m = String(body).match(/(?:★|rating|评分|評分)[^\n]{0,40}?\b([1-5])\s*\/\s*5\b/i); return m ? +m[1] : 0; };
  let lang = "s", cat = "all", sort = "hot", q = "", mods = OFFICIAL.map((m) => norm(m, "official")), ghState = "loading";
  const comments = {}; // issue → [{user, avatar, url, at, body, stars}]
  let started = false, loaded = false, pendingSub = "", upload = null;

  function catOf(c, official) {
    const s = String(c || "").toLowerCase();
    if (/midi/.test(s)) return "midi";
    if (/net|remote|tailscale|vpn/.test(s)) return "network";
    if (/light|dmx/.test(s)) return "lighting";
    if (/audio|sampl|fx|sound/.test(s)) return "audio";
    if (/ui|jog|interface|theme|skin|display/.test(s)) return "ui";
    return official ? "ui" : "other";
  }
  function norm(m, src, base) {
    const pk = (m.packages || m.assets || (m.downloadUrl || m.url || m.download ? [{ url: m.downloadUrl || m.url || m.download, sha256: m.sha256, size: m.size }] : []))
      .map((p) => ({ label: p.label || p.arch || "", url: p.url ? safeUrl(p.url, /^(https?:|mods\/)/.test(p.url) ? undefined : base) : "", sha256: p.sha256 || "", size: p.size || 0, b64: p.b64 }))
      .filter((p) => p.url || p.b64);
    const issues = [...new Set([...(m.issues || []), m.issue, m.issueNumber].map(Number).filter(Boolean))];
    // registry 的 initialComments：字段名兼容 body|text、author|user、date|createdAt
    const seed = (m.initialComments || []).map((c) => ({ user: c.author || c.user || "", avatar: c.avatar || "", url: c.url ? safeUrl(c.url) : "", at: c.date || c.createdAt || "", body: c.body || c.text || "", stars: +c.stars || starOf(c.body || c.text || "") }));
    return { ...m, src, official: src === "official" || !!m.official, cat: catOf(m.category, src === "official" || m.official), packages: pk, issues,
      files: m.files || [...Object.keys(m.qmlFiles || {}), ...(m.script ? [m.script] : [])], downloads: +m.downloads || 0, updated: m.updated || m.date || m.updatedAt || "", seed, key: src + ":" + m.id };
  }
  const T = (x) => (x == null ? "" : typeof x === "string" ? x : x[lang] || x[{ s: "zh-CN", t: "zh-TW", e: "en" }[lang]] || x.e || x.en || x.s || Object.values(x)[0] || "");

  async function ghGet(path) {
    const k = LS_CACHE + path, c = load(k, null);
    if (c && Date.now() - c.at < CACHE_MS) return c.data;
    const r = await fetch(API + path, { headers: { Accept: "application/vnd.github+json" } });
    if (!r.ok) { if (c) return c.data; throw new Error("GitHub " + r.status); }
    const data = await r.json();
    save(k, { at: Date.now(), data });
    return data;
  }

  function fromIssue(i) {
    const body = i.body || "";
    let man = {};
    const j = body.match(/```json\s*([\s\S]*?)```/);
    if (j) try { man = JSON.parse(j[1]); } catch { /* 正文里的 JSON 写坏了就只用标题 */ }
    const t = (i.title || "").match(/^\s*\[mod\]\s*(.+?)\s+v?(\d[\w.+-]*)\s*$/i);
    const url = (body.match(/https:\/\/[^\s)"'<>]+?\.(?:tar\.gz|tgz)\b/i) || [])[0];
    const sha = (body.match(/\b[a-f0-9]{64}\b/i) || [])[0];
    const id = ID_RE.test(man.id || "") ? man.id : "issue-" + i.number;
    return norm({ ...man, id, name: man.name || (t ? t[1] : i.title), version: man.version || (t ? t[2] : "—"), author: man.author || i.user?.login || "",
      authorUrl: i.user?.html_url, description: man.description || body.replace(/```[\s\S]*?```/g, "").replace(/[#>*`|_-]+/g, " ").replace(/\s+/g, " ").trim().slice(0, 280),
      issue: i.number, updated: i.created_at, downloads: (i.comments || 0) + (i.reactions?.total_count || 0), packages: url ? [{ url, sha256: sha || "" }] : [] }, "community");
  }

  async function start() {
    started = true;
    try {
      const r = await fetch(REGISTRY, { cache: "no-cache" });
      if (r.ok) {
        const j = await r.json(), list = Array.isArray(j) ? j : j.mods || [];
        const base = new URL(REGISTRY, location.href).href;
        for (const raw of list) {
          if (!raw || !ID_RE.test(raw.id || "")) continue;
          const fb = OFFICIAL.find((o) => o.id === raw.id) || {};
          const m = norm({ ...fb, ...raw, description: raw.description || fb.description, updated: raw.updated || raw.updatedAt || j.updatedAt, packages: raw.packages || raw.assets || (raw.downloadUrl || raw.url ? null : fb.packages), issues: raw.issues || (raw.issue || raw.issueNumber ? [] : fb.issues) }, raw.official === false ? "community" : "official", base);
          const i = mods.findIndex((x) => x.key === m.key || (x.src === "official" && x.id === m.id));
          i >= 0 ? (mods[i] = m) : mods.push(m);
        }
      }
    } catch { /* file:// 或离线：用内置兜底 */ }
    mods.push(...load(LS_LOCAL, []).map((m) => norm(m, "local")));
    paint();
    const officialIssues = new Set(mods.flatMap((m) => m.issues));
    try {
      const list = await ghGet("/issues?labels=mod&state=all&per_page=30");
      for (const i of list) {
        if (i.pull_request || officialIssues.has(i.number)) continue;
        const m = fromIssue(i);
        if (!mods.some((x) => x.src !== "local" && x.id === m.id)) mods.push(m);
      }
      ghState = "ok";
    } catch { ghState = "fail"; }
    paint();
    loaded = true;
    if (pendingSub) { const k = pendingSub; pendingSub = ""; openDetail(k); } // #mods/<id> deep link opened before the registry arrived
    // 先拉官方 Mod 关联 Issue 的评论，评分排序才有数据
    await Promise.all([...officialIssues].map((n) => fetchComments(n).catch(() => {})));
    paint();
  }

  async function fetchComments(n) {
    if (comments[n]) return comments[n];
    const list = await ghGet(`/issues/${n}/comments?per_page=100`);
    return (comments[n] = list.map((c) => ({ user: c.user?.login || "", avatar: c.user?.avatar_url || "", url: c.html_url, at: c.created_at, body: c.body || "", stars: starOf(c.body || "") })));
  }
  const ghComments = (m) => { const live = m.issues.flatMap((n) => comments[n] || []), urls = new Set(live.map((c) => c.url)); return [...m.seed.filter((c) => !c.url || !urls.has(c.url)), ...live]; };
  function score(m) {
    const all = [...(load(LS_REV, {})[m.key] || []).map((r) => r.stars), ...ghComments(m).map((c) => c.stars)].filter(Boolean);
    if (!all.length && m.stars > 0 && m.stars <= 5) return { avg: +m.stars, n: 0, seed: true }; // registry 给的初始评分
    return { avg: all.length ? all.reduce((a, b) => a + b, 0) / all.length : 0, n: all.length };
  }

  /* ───────────── 渲染 ───────────── */
  const stars = (v, cls = "") => `<span class="mx-stars ${cls}" aria-label="${v.toFixed(1)} / 5">${[1, 2, 3, 4, 5].map((i) => `<i class="${v >= i - 0.25 ? "on" : ""}">★</i>`).join("")}</span>`;
  const badge = (m, u) => `<span class="tag ${m.official ? "gen1" : m.src === "local" ? "mx-loc" : "gen2"}">${m.official ? u.official : m.src === "local" ? u.local : u.community}</span>`;

  function visible() {
    const s = q.trim().toLowerCase();
    return mods
      .filter((m) => cat === "all" || (cat === "official" ? m.official : cat === "community" ? !m.official : m.cat === cat))
      .filter((m) => !s || [m.id, T(m.name), m.author, T(m.summary), T(m.description), m.category, ...(m.tags || [])].join(" ").toLowerCase().includes(s))
      .sort((a, b) => sort === "new" ? String(b.updated).localeCompare(String(a.updated)) || b.official - a.official
        : sort === "rating" ? score(b).avg - score(a).avg || score(b).n - score(a).n
        : b.official - a.official || (b.downloads + ghComments(b).length) - (a.downloads + ghComments(a).length));
  }

  function shell() {
    const u = UI[lang], root = $("#mods");
    root.innerHTML = `<div class="mods">
      <div class="mx-head"><div><h1>${u.title}</h1><p>${u.lead}</p></div><button class="btn pri" data-act="upload">${u.upload}</button></div>
      <section class="mx-arch" aria-label="${u.archT}"><div><b>${u.arch1T}</b><span>${md(u.arch1)}</span></div><div><b>${u.arch2T}</b><span>${md(u.arch2)}</span></div></section>
      <div class="mx-bar">
        <div class="chips" role="tablist">${CATS.map((c) => `<button data-cat="${c}" class="${cat === c ? "on" : ""}">${u.cats[c]}</button>`).join("")}</div>
        <input class="mx-q" type="search" placeholder="${u.search}" aria-label="${u.search}" value="${esc(q)}">
        <select class="mx-sort" aria-label="Sort">${Object.entries(u.sort).map(([k, v]) => `<option value="${k}" ${sort === k ? "selected" : ""}>${v}</option>`).join("")}</select>
      </div>
      <p class="mx-state" aria-live="polite"></p>
      <div class="mx-grid"></div>
      <dialog class="mx-dlg" id="mx-detail"></dialog>
      <dialog class="mx-dlg" id="mx-upload"></dialog>
    </div>`;
  }

  function paint() {
    const u = UI[lang], grid = $("#mods .mx-grid");
    if (!grid) return;
    const list = visible();
    $("#mods .mx-state").textContent = ghState === "loading" ? u.loading : ghState === "fail" ? u.ghFail : "";
    grid.innerHTML = list.length ? list.map((m) => {
      const sc = score(m);
      return `<button class="card mx-card" data-open="${esc(m.key)}">
        <span class="mx-top">${badge(m, u)}<span class="mx-cat">${u.cats[m.cat]}</span></span>
        <b>${esc(T(m.name))} <small>v${esc(m.version)}</small></b>
        <span>${md(T(m.summary || m.description))}</span>
        <span class="mx-meta">${sc.avg ? stars(sc.avg) + ` <small>${sc.avg.toFixed(1)}${sc.n ? " · " + sc.n : ""}</small>` : ""}${m.downloads ? `<small>⬇ ${m.downloads}</small>` : ""}<small>${esc(m.author)}</small></span>
      </button>`;
    }).join("") : `<p class="mx-empty">${u.none}</p>`;
    // registry / GitHub 晚到时，已打开的详情（如 #mods/<id> 深链）换成新数据；对象没变就不重渲染，免得清掉正在写的评论
    const dlg = $("#mx-detail"), cur = dlg?.open && mods.find((x) => x.key === dlg.dataset.key || (x.id === dlg._m?.id && x.src === dlg._m?.src));
    if (cur && cur !== dlg._m) detail(cur);
  }

  async function openDetail(key) {
    const m = mods.find((x) => x.key === key || x.id === key);
    const dlg = $("#mx-detail");
    if (!m && !loaded) pendingSub = key;
    if (!m || !dlg) return;
    detail(m);
    if (!dlg.open) dlg.showModal();
    if (m.issues.length) { await Promise.all(m.issues.map((n) => fetchComments(n).catch(() => {}))); const cur = mods.find((x) => x.key === dlg.dataset.key); if (dlg.open && cur) detail(cur); }
  }

  function detail(m) {
    const u = UI[lang], dlg = $("#mx-detail"), sc = score(m);
    const mine = load(LS_REV, {})[m.key] || [];
    const gh = ghComments(m);
    dlg.dataset.key = m.key;
    dlg._m = m;
    const pkgs = m.packages.length ? m.packages.map((p, i) => `<div class="mx-pkg">
        ${p.b64 ? `<button class="btn pri" data-localdl="${i}">${u.dlPkg}</button>` : `<a class="btn pri" href="${esc(p.url)}" download>${u.dlPkg}${p.label ? " · " + esc(p.label) : ""}</a>`}
        ${p.sha256 ? `<code class="mx-sha" title="${u.sha}">${esc(p.sha256)}</code>` : ""}${p.size ? `<small>${kb(p.size)}</small>` : ""}</div>`).join("")
      : `<p class="mx-dim">${u.noPkg}</p>`;
    const rev = [...mine.map((r) => ({ ...r, local: true })), ...gh.map((c) => ({ name: c.user, avatar: c.avatar, url: c.url, at: c.at, text: c.body, stars: c.stars }))];
    dlg.innerHTML = `<form method="dialog" class="mx-x"><button class="icon-btn" aria-label="${u.close}">✕</button></form>
      <header class="mx-dh"><span class="mx-top">${badge(m, u)}<span class="mx-cat">${u.cats[m.cat]}</span></span>
        <h2>${esc(T(m.name))} <small>v${esc(m.version)}</small></h2>
        ${sc.avg ? `<p>${stars(sc.avg, "lg")} <b>${sc.avg.toFixed(1)}</b>${sc.n ? ` <small>· ${sc.n} ${u.nRev}</small>` : ""}</p>` : ""}</header>
      <p>${md(T(m.description))}</p>
      <dl class="mx-dl">
        <dt>ID</dt><dd><code>${esc(m.id)}</code></dd>
        <dt>${u.by}</dt><dd>${m.authorUrl ? `<a href="${esc(safeUrl(m.authorUrl))}" target="_blank" rel="noopener">${esc(m.author)}</a>` : esc(m.author)}</dd>
        <dt>${u.ver}</dt><dd>${esc(m.version)}</dd>
        ${m.models ? `<dt>${u.models}</dt><dd>${md(T(m.models))}</dd>` : ""}
        ${m.hardwareValidation ? `<dt>${u.hw}</dt><dd>${md(typeof m.hardwareValidation === "object" && !m.hardwareValidation.s && !m.hardwareValidation.e ? JSON.stringify(m.hardwareValidation) : T(m.hardwareValidation))}</dd>` : ""}
        ${Array.isArray(m.tags) && m.tags.length ? `<dt>Tags</dt><dd>${m.tags.map((x) => `<span class="tag">${esc(x)}</span>`).join(" ")}</dd>` : ""}
        ${m.issues.length ? `<dt>${u.issue}</dt><dd>${m.issues.map((n) => `<a href="${GH}/issues/${n}" target="_blank" rel="noopener">#${n}</a>`).join(" · ")}</dd>` : ""}
        ${m.files.length ? `<dt>${u.files}</dt><dd>${m.files.map((f) => `<code>${esc(f)}</code>`).join(" ")}</dd>` : ""}
      </dl>
      <h3>${u.dl}</h3>${pkgs}
      <h3>${u.install}</h3><ol class="mx-ol"><li>${md(u.inst1)}</li><li>${md(u.inst2).replace("&lt;id&gt;", esc(m.id))}</li></ol>
      ${m.src === "local" ? `<p><button class="btn" data-act="dellocal">${u.del}</button></p>` : ""}
      <h3>${u.reviews}</h3>
      <div class="mx-revs">${rev.length ? rev.map((r) => `<article class="mx-rev">
        ${r.avatar ? `<img src="${esc(safeUrl(r.avatar))}&s=64" alt="" width="32" height="32" loading="lazy">` : `<span class="mx-av">${esc((r.name || "?").slice(0, 1).toUpperCase())}</span>`}
        <div><p class="mx-rh"><b>${r.url ? `<a href="${esc(safeUrl(r.url))}" target="_blank" rel="noopener">${esc(r.name)}</a>` : esc(r.name)}</b>
          <small>${r.local ? u.you : u.ghRev} · ${esc(new Date(r.at).toLocaleDateString())}</small>${r.stars ? stars(r.stars) : ""}</p>
        <p class="mx-rt">${esc(String(r.text).slice(0, 1500))}</p></div></article>`).join("") : `<p class="mx-dim">${u.noRev}</p>`}</div>
      <form class="mx-form" data-form="review">
        <div class="mx-row"><label>${u.name}<input name="name" maxlength="40" required value="${esc(localStorage.getItem("enginex-mods-nick") || "")}"></label>
          <fieldset class="mx-rate"><legend>${u.stars}</legend>${[5, 4, 3, 2, 1].map((i) => `<input type="radio" name="stars" value="${i}" id="mx-s${i}"><label for="mx-s${i}" title="${i}/5">★</label>`).join("")}</fieldset></div>
        <label>${u.text}<textarea name="text" rows="3" maxlength="2000" required></textarea></label>
        <p class="mx-msg" aria-live="polite"></p>
        <div class="mx-acts"><button class="btn pri" type="submit">${u.post}</button>${m.issues.length || m.issue ? `<button class="btn" type="button" data-act="sync">${u.sync}</button>` : ""}</div>
      </form>`;
  }

  function uploadView() {
    const u = UI[lang], dlg = $("#mx-upload"), mode = upload?.mode || "a";
    const r = upload?.report;
    const fileRows = (upload?.files || []).map((f, i) => `<li><code>${esc(f.name)}</code> <small>${kb(f.data.length)}</small>
      ${/\.qml$/i.test(f.name) ? `<label class="mx-inl">${u.fTarget}<input data-target="${i}" value="${esc(f.target || f.name)}" placeholder="Screens/Deck/Foo.qml"></label>`
        : /\.sh$/i.test(f.name) ? `<label class="mx-inl"><input type="radio" name="script" value="${i}" ${upload.script === i ? "checked" : ""}> ${u.fScript}</label>` : `<small>${u.fAsset}</small>`}</li>`).join("");
    const f = upload?.form || {};
    dlg.innerHTML = `<form method="dialog" class="mx-x"><button class="icon-btn" aria-label="${u.close}">✕</button></form>
      <h2>${u.upT}</h2>
      <div class="chips">${["a", "b"].map((k) => `<button data-mode="${k}" class="${mode === k ? "on" : ""}">${k === "a" ? u.upA : u.upB}</button>`).join("")}</div>
      ${mode === "a" ? `<label class="mx-drop"><input type="file" accept=".tar.gz,.tgz,application/gzip" data-pick="tgz" hidden><b>${md(u.drop)}</b><small>${u.dropHint}</small></label>`
      : `<form class="mx-form" data-form="pack">
          <div class="mx-row"><label>${u.fId}<input name="id" required pattern="[a-zA-Z0-9][a-zA-Z0-9._\\-]{0,63}" value="${esc(f.id || "")}" placeholder="my-mod"></label>
            <label>${u.fName}<input name="name" required value="${esc(f.name || "")}"></label><label>${u.fVer}<input name="version" required value="${esc(f.version || "1.0.0")}"></label></div>
          <div class="mx-row"><label>${u.fAuthor}<input name="author" value="${esc(f.author || "")}"></label>
            <label>${u.fCat}<select name="category">${["network", "lighting", "audio", "ui", "other"].map((c) => `<option value="${c}" ${f.category === c ? "selected" : ""}>${u.cats[c]}</option>`).join("")}</select></label></div>
          <label>${u.fDesc}<textarea name="description" rows="2">${esc(f.description || "")}</textarea></label>
          <label>${u.fFiles}<input type="file" multiple data-pick="src" accept=".qml,.sh,.js,.json,.png,.jpg,.svg,.txt,.md"></label>
          <ul class="mx-files">${fileRows}</ul>
          <div class="mx-acts"><button class="btn pri" type="submit">${u.pack}</button></div>
        </form>`}
      <p class="mx-dim mx-safe">${md(u.safety)}</p>
      ${r ? `<section class="mx-res ${r.ok ? "ok" : "bad"}" aria-live="polite"><b>${r.ok ? "✓ " + u.ok : "✕ " + u.bad}</b>
        ${r.errors.length ? `<ul>${r.errors.map((e) => `<li>${esc(e)}</li>`).join("")}</ul>` : ""}
        ${r.warnings.length ? `<p class="mx-dim">${u.warn}</p><ul>${r.warnings.map((e) => `<li>${esc(e)}</li>`).join("")}</ul>` : ""}
        <dl class="mx-dl"><dt>${u.sha}</dt><dd><code class="mx-sha">${esc(r.sha)}</code></dd><dt>${u.size}</dt><dd>${kb(r.size)}</dd>
          ${r.manifest ? `<dt>mod.json</dt><dd><pre>${esc(JSON.stringify(r.manifest, null, 2))}</pre></dd>` : ""}
          <dt>${u.files}</dt><dd>${r.files.map((x) => `<code>${esc(x.name)}</code> <small>${kb(x.size)}</small>`).join("<br>")}</dd></dl>
        ${r.ok ? `<div class="mx-acts"><button class="btn pri" data-act="keep">${u.keep}</button><button class="btn" data-act="submit">${u.submit}</button></div><p class="mx-dim">${md(u.submitHint)}</p>` : ""}
      </section>` : ""}`;
  }

  const fileName = () => `${upload.report.manifest.id}-${upload.report.manifest.version}.tar.gz`;
  function download(bytes, name) {
    const a = document.createElement("a");
    a.href = URL.createObjectURL(new Blob([bytes], { type: "application/gzip" }));
    a.download = name;
    a.click();
    setTimeout(() => URL.revokeObjectURL(a.href), 1000);
  }
  const b64 = (u8) => { let s = ""; for (let i = 0; i < u8.length; i += 32768) s += String.fromCharCode(...u8.subarray(i, i + 32768)); return btoa(s); };
  const unb64 = (s) => Uint8Array.from(atob(s), (c) => c.charCodeAt(0));

  function issueBody(r) {
    const m = r.manifest;
    let man = JSON.stringify(m, null, 2);
    if (man.length > 4000) man = man.slice(0, 4000) + "\n…";
    return [`### ${T(m.name)} v${m.version}`, "", m.description || "", "",
      "| | |", "|---|---|", `| ID | \`${m.id}\` |`, `| Author | ${m.author || ""} |`, `| Category | ${m.category || ""} |`, `| Package | \`${fileName()}\` (${kb(r.size)}) |`, `| SHA-256 | \`${r.sha}\` |`, "",
      "### mod.json", "```json", man, "```", "", "### Files", ...r.files.slice(0, 40).map((f) => `- \`${f.name}\` (${kb(f.size)})`), "",
      "### Install", "1. Web console `http://<deck IP>:8080` → **Mods** → drop the `.tar.gz`.", `2. Or extract it into \`/mods/\` on a USB stick (\`/mods/${m.id}/mod.json\`).`, "",
      `> ⬇ Attach \`${fileName()}\` to this Issue (drag & drop) so others can download it.`, "", "_Validated in the browser on enginexdj.com/#mods_"].join("\n");
  }

  async function onPick(input) {
    const u8 = [];
    for (const file of input.files) {
      if (file.size > MAX_PKG) { alert(file.name + " > " + kb(MAX_PKG)); return; }
      u8.push({ name: file.name.replace(/[^\w.\-]/g, "_"), data: new Uint8Array(await file.arrayBuffer()) });
    }
    if (input.dataset.pick === "tgz") {
      if (!u8[0]) return;
      upload.gz = u8[0].data;
      upload.report = await inspect(u8[0].data);
    } else {
      upload.files = u8;
      upload.script = u8.findIndex((f) => /\.sh$/i.test(f.name));
      upload.report = null;
    }
    uploadView();
  }

  async function onPack(form) {
    const fd = Object.fromEntries(new FormData(form));
    upload.form = fd;
    form.querySelectorAll("[data-target]").forEach((i) => (upload.files[+i.dataset.target].target = i.value.trim()));
    const sel = form.querySelector('[name="script"]:checked');
    upload.script = sel ? +sel.value : -1;
    const files = upload.files || [];
    const qmlFiles = Object.fromEntries(files.filter((f) => /\.qml$/i.test(f.name)).map((f) => [f.name, f.target || f.name]));
    const man = { id: fd.id.trim(), name: fd.name.trim(), version: fd.version.trim(), author: fd.author.trim(), category: fd.category, description: fd.description.trim() };
    if (Object.keys(qmlFiles).length) man.qmlFiles = qmlFiles;
    if (upload.script >= 0 && files[upload.script]) man.script = files[upload.script].name;
    try { const b = await build(man, files); upload.gz = b.gz; upload.report = b.report; }
    catch (err) { upload.report = { ok: false, errors: [err.message], warnings: [], files: [], sha: "", size: 0 }; }
    uploadView();
  }

  /* ───────────── 事件（全部委托在 #mods 上，重渲染不丢） ───────────── */
  function bind(root) {
    root.addEventListener("click", async (e) => {
      const t = e.target;
      const c = t.closest("[data-cat]"); if (c) { cat = c.dataset.cat; root.querySelectorAll("[data-cat]").forEach((b) => b.classList.toggle("on", b === c)); paint(); return; }
      const o = t.closest("[data-open]"); if (o) { history.replaceState(null, "", "#mods/" + encodeURIComponent(mods.find((m) => m.key === o.dataset.open)?.id || "")); openDetail(o.dataset.open); return; }
      const md_ = t.closest("[data-mode]"); if (md_) { upload.mode = md_.dataset.mode; upload.report = null; uploadView(); return; }
      if (t.matches("dialog")) { const r = t.getBoundingClientRect(); if (e.clientX < r.left || e.clientX > r.right || e.clientY < r.top || e.clientY > r.bottom) t.close(); return; } // 只有点遮罩才关闭
      const dl = t.closest("[data-localdl]");
      if (dl) { const m = mods.find((x) => x.key === $("#mx-detail").dataset.key); const p = m.packages[+dl.dataset.localdl]; download(unb64(p.b64), `${m.id}-${m.version}.tar.gz`); return; }
      const act = t.closest("[data-act]")?.dataset.act;
      if (act === "upload") { upload = { mode: "a" }; uploadView(); $("#mx-upload").showModal(); }
      else if (act === "keep") {
        const r = upload.report, man = r.manifest;
        download(upload.gz, fileName());
        const list = load(LS_LOCAL, []).filter((x) => !(x.id === man.id && x.version === man.version));
        list.unshift({ ...man, updated: new Date().toISOString(), files: r.files.map((f) => f.name), packages: [{ sha256: r.sha, size: r.size, b64: r.size <= MAX_KEEP ? b64(upload.gz) : undefined, url: "" }].filter((p) => p.b64) });
        save(LS_LOCAL, list);
        mods = mods.filter((x) => x.src !== "local").concat(list.map((x) => norm(x, "local")));
        $("#mx-upload").close(); cat = "all"; shell(); paint();
      } else if (act === "submit") {
        const m = upload.report.manifest;
        open(`${GH}/issues/new?labels=mod&title=${encodeURIComponent(`[Mod] ${T(m.name)} v${m.version}`)}&body=${encodeURIComponent(issueBody(upload.report))}`, "_blank", "noopener");
      } else if (act === "dellocal") {
        const key = $("#mx-detail").dataset.key;
        save(LS_LOCAL, load(LS_LOCAL, []).filter((x) => "local:" + x.id !== key));
        mods = mods.filter((x) => x.key !== key);
        $("#mx-detail").close(); paint();
      } else if (act === "sync") {
        const m = mods.find((x) => x.key === $("#mx-detail").dataset.key), f = t.closest("form"), u = UI[lang];
        const st = +(f.querySelector('[name="stars"]:checked')?.value || 0), text = f.text.value.trim();
        if (!st || !text) { f.querySelector(".mx-msg").textContent = u.needText; return; }
        const body = `**Rating:** ${"★".repeat(st)}${"☆".repeat(5 - st)} (${st}/5) · \`${m.id}\` v${m.version}\n\n${text}\n\n— ${f.name.value.trim()} via enginexdj.com/#mods`;
        try { await navigator.clipboard.writeText(body); f.querySelector(".mx-msg").textContent = u.synced; } catch { prompt(u.synced, body); }
        open(`${GH}/issues/${m.issues[0] || m.issue}#new_comment_field`, "_blank", "noopener");
      }
    });
    root.addEventListener("submit", (e) => {
      const f = e.target.closest("[data-form]");
      if (!f) return;
      e.preventDefault();
      if (f.dataset.form === "pack") { onPack(f); return; }
      const st = +(f.querySelector('[name="stars"]:checked')?.value || 0), text = f.text.value.trim(), name = f.name.value.trim();
      if (!st || !text || !name) { f.querySelector(".mx-msg").textContent = UI[lang].needText; return; }
      const key = $("#mx-detail").dataset.key, all = load(LS_REV, {});
      (all[key] = all[key] || []).unshift({ name: name.slice(0, 40), stars: st, text: text.slice(0, 2000), at: new Date().toISOString() });
      save(LS_REV, all);
      localStorage.setItem("enginex-mods-nick", name.slice(0, 40));
      detail(mods.find((x) => x.key === key)); paint();
    });
    root.addEventListener("change", (e) => {
      if (e.target.matches("[data-pick]")) onPick(e.target);
      else if (e.target.matches(".mx-sort")) { sort = e.target.value; paint(); }
    });
    root.addEventListener("input", (e) => { if (e.target.matches(".mx-q")) { q = e.target.value; paint(); } });
    root.addEventListener("close", (e) => { if (e.target.id === "mx-detail" && location.hash.startsWith("#mods/")) history.replaceState(null, "", "#mods"); }, true);
    // 拖放 .tar.gz
    root.addEventListener("dragover", (e) => { const d = e.target.closest(".mx-drop"); if (d) { e.preventDefault(); d.classList.add("over"); } });
    root.addEventListener("dragleave", (e) => e.target.closest(".mx-drop")?.classList.remove("over"));
    root.addEventListener("drop", (e) => {
      const d = e.target.closest(".mx-drop");
      if (!d) return;
      e.preventDefault();
      const input = d.querySelector("input");
      input.files = e.dataTransfer.files;
      onPick(input);
    });
  }

  /* app.js 在切到 #mods / #mods/<id> 或切换语言时调用 */
  window.EXMods = {
    render(l, sub) {
      const root = $("#mods");
      if (!root) return;
      if (!root.dataset.bound) { bind(root); root.dataset.bound = "1"; }
      if (l !== lang || !root.firstChild) { lang = l; shell(); }
      paint();
      if (!started) start();
      if (sub) openDetail(sub);
      else { pendingSub = ""; $("#mx-detail")?.close(); }
    },
  };
})();
