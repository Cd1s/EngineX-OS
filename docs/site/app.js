/* EngineX OS 文档站渲染器：数据全在 content.js，这里不放任何文案以外的业务内容。 */
(function () {
  const D = window.DOCS;
  const UI = {
    s: { lang: "s", search: "搜索文档…", heroT: "EngineX OS 使用文档", heroP: "16 款 Denon DJ / Numark / Rane 独立设备的安装、功能与排障指南。", model: "机型", board: "板号", type: "类型", file: "下载", all: "全部", gen1: "一代 · .img", gen2: "二代 · 数据包", none: "没有匹配的内容", top: "本章目录", hits: "条匹配", img: ".img 刷机包", bundle: "/data 数据包" },
    t: { lang: "t", search: "搜尋文件…", heroT: "EngineX OS 使用文件", heroP: "16 款 Denon DJ / Numark / Rane 獨立設備的安裝、功能與排障指南。", model: "機型", board: "板號", type: "類型", file: "下載", all: "全部", gen1: "一代 · .img", gen2: "二代 · 資料包", none: "沒有符合的內容", top: "本章目錄", hits: "筆符合", img: ".img 刷機包", bundle: "/data 資料包" },
    e: { lang: "e", search: "Search the docs…", heroT: "EngineX OS Documentation", heroP: "Install, use and troubleshoot all 16 Denon DJ / Numark / Rane standalone units.", model: "Model", board: "Board", type: "Type", file: "Download", all: "All", gen1: "Gen 1 · .img", gen2: "Gen 2 · bundle", none: "Nothing matches", top: "On this page", hits: "matches", img: ".img image", bundle: "/data bundle" },
  };
  const SHOT_DIR = { s: "zh-CN", t: "zh-TW", e: "en" };
  const $ = (s, r = document) => r.querySelector(s);
  const $$ = (s, r = document) => [...r.querySelectorAll(s)];
  const esc = (x) => x.replace(/[&<>"]/g, (c) => ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;" }[c]));

  let lang = pickLang();
  let query = "";
  let gen = "all";

  function pickLang() {
    const q = new URLSearchParams(location.search).get("lang");
    const saved = localStorage.getItem("enginex-docs-lang");
    const v = q || saved || navigator.language || "";
    if (["s", "t", "e"].includes(v)) return v;
    if (/zh.*(TW|HK|MO|Hant)/i.test(v)) return "t";
    if (/^zh/i.test(v)) return "s";
    return "e";
  }
  // 内容是自家 content.js，可信：只展开 **粗体** 与 `代码`，其余（含 <a>）原样输出。
  const md = (x) => x.replace(/`([^`]+)`/g, (_, c) => "<code>" + c.replace(/</g, "&lt;").replace(/>/g, "&gt;") + "</code>").replace(/\*\*([^*]+)\*\*/g, "<b>$1</b>").replace(/\*([^*]+)\*/g, "<i>$1</i>");
  const T = (o) => md(typeof o === "string" ? o : o[lang]);
  const hid = (sec, b) => sec.id + "-" + b.id;

  function block(sec, b) {
    const u = UI[lang];
    switch (b.k) {
      case "h": return `<h3 id="${hid(sec, b)}" class="blk">${T(b.x)}</h3>`;
      case "p": return `<p class="blk">${T(b.x)}</p>`;
      case "ul": case "ol": return `<${b.k} class="blk">${b.x.map((i) => `<li>${T(i)}</li>`).join("")}</${b.k}>`;
      case "note": return `<div class="note blk ${b.tone || ""}">${T(b.x)}</div>`;
      case "shot": {
        const src = b.src || `assets/screenshots/${SHOT_DIR[lang]}/${b.name.replace(/^zh:/, "")}`;
        return `<figure class="shot blk"><img loading="lazy" src="${esc(src)}" alt="${esc(T(b.cap).replace(/<[^>]+>/g, ""))}"><figcaption>${T(b.cap)}</figcaption></figure>`;
      }
      case "table": return `<div class="tbl blk"><table><thead><tr>${b.head.map((h) => `<th>${T(h)}</th>`).join("")}</tr></thead><tbody>${b.rows.map((r) => `<tr>${r.map((c) => `<td>${T(c)}</td>`).join("")}</tr>`).join("")}</tbody></table></div>`;
      case "models": {
        const chips = ["all", "gen1", "gen2"].map((g) => `<button data-gen="${g}" class="${gen === g ? "on" : ""}">${u[g]}</button>`).join("");
        const rows = D.models.filter((m) => gen === "all" || m.gen === gen).map((m) =>
          `<tr><td><b>${esc(m.name)}</b></td><td><code>${m.board}</code></td><td><span class="tag ${m.gen}">${m.gen === "gen1" ? u.img : u.bundle}</span></td><td><a class="dl" href="${m.url}">${esc(m.file)}</a></td></tr>`).join("");
        return `<div class="blk"><div class="chips">${chips}</div><div class="tbl"><table><thead><tr><th>${u.model}</th><th>${u.board}</th><th>${u.type}</th><th>${u.file}</th></tr></thead><tbody>${rows}</tbody></table></div></div>`;
      }
      case "installers": return `<div class="insts blk">${D.installers.map((i) => `<a href="${i.url}"><b>${esc(i.name)}</b><small>${esc(i.file)}</small></a>`).join("")}</div>`;
      case "faq": return b.items.map((i) => `<details class="qa blk"><summary>${T(i.q)}</summary><div class="a">${T(i.a)}</div></details>`).join("");
    }
    return "";
  }

  function render() {
    const u = UI[lang];
    document.documentElement.lang = { s: "zh-CN", t: "zh-TW", e: "en" }[lang];
    document.title = u.heroT;
    $("#q").placeholder = u.search;
    $$(".langs button").forEach((b) => b.classList.toggle("on", b.dataset.l === lang));
    $("#nav").innerHTML = D.sections.map((s) => `<a href="#${s.id}" data-s="${s.id}">${s.icon} ${T(s.title)}</a>`).join("");
    $("#main").innerHTML = `<div class="hero"><h1><b>EngineX OS</b> ${u.heroT.replace("EngineX OS ", "")} <small style="color:var(--dim);font-size:14px">v${D.version}</small></h1><p>${u.heroP}</p></div>` +
      D.sections.map((s) => `<section class="sec" id="${s.id}"><h2>${s.icon} ${T(s.title)}</h2>${s.blocks.map((b) => block(s, b)).join("")}</section>`).join("") +
      `<div class="empty hide" id="empty">${u.none}</div>`;
    applySearch();
    buildToc();
    observe();
  }

  /* ---- search: hide non-matching blocks, mark matches ---- */
  function applySearch() {
    const q = query.trim().toLowerCase();
    let hits = 0;
    $$("section.sec").forEach((sec) => {
      let any = false;
      $$(".blk", sec).forEach((el) => {
        const ok = !q || el.textContent.toLowerCase().includes(q);
        el.classList.toggle("hide", !ok);
        if (ok) { any = true; if (q) hits++; if (q && el.tagName === "DETAILS") el.open = true; }
      });
      sec.classList.toggle("hide", !any);
    });
    $("#empty").classList.toggle("hide", !q || hits > 0);
    if (q) mark(q);
  }
  function mark(q) {
    const re = new RegExp(q.replace(/[.*+?^${}()|[\]\\]/g, "\\$&"), "gi");
    $$(".blk:not(.hide)").forEach((root) => {
      const w = document.createTreeWalker(root, NodeFilter.SHOW_TEXT);
      const nodes = [];
      while (w.nextNode()) if (re.test(w.currentNode.nodeValue)) nodes.push(w.currentNode), (re.lastIndex = 0);
      nodes.forEach((n) => {
        const span = document.createElement("span");
        span.innerHTML = esc(n.nodeValue).replace(new RegExp(esc(q).replace(/[.*+?^${}()|[\]\\]/g, "\\$&"), "gi"), (m) => `<mark>${m}</mark>`);
        n.replaceWith(span);
      });
    });
  }

  /* ---- right TOC + nav highlight ---- */
  let io;
  function buildToc() {
    const first = D.sections[0].id;
    setToc(currentSec() || first);
  }
  function currentSec() { return $$("section.sec:not(.hide)").find((s) => s.getBoundingClientRect().bottom > 120)?.id; }
  function setToc(id) {
    const s = D.sections.find((x) => x.id === id) || D.sections[0];
    $("#toc").innerHTML = `<h4>${UI[lang].top}</h4>` + s.blocks.filter((b) => b.k === "h").map((b) => `<a href="#${hid(s, b)}">${T(b.x)}</a>`).join("");
    $$("#nav a").forEach((a) => a.classList.toggle("on", a.dataset.s === s.id));
  }
  function observe() {
    if (io) io.disconnect();
    io = new IntersectionObserver((es) => { if (es.some((e) => e.isIntersecting)) setToc(currentSec()); }, { rootMargin: "-80px 0px -60% 0px" });
    $$("section.sec").forEach((s) => io.observe(s));
  }

  /* ---- hash routing: #install  #streaming/#streaming-login ... ---- */
  function route() {
    const h = decodeURIComponent(location.hash.slice(1));
    if (!h) return;
    const el = document.getElementById(h) || document.getElementById(D.sections.map((s) => s.id).find((id) => h === id));
    if (el && !el.closest(".hide")) el.scrollIntoView();
  }

  /* ---- events ---- */
  $(".langs").addEventListener("click", (e) => {
    const l = e.target.dataset.l; if (!l) return;
    lang = l; localStorage.setItem("enginex-docs-lang", l); render(); route();
  });
  $("#q").addEventListener("input", (e) => { query = e.target.value; applySearch(); });
  document.addEventListener("keydown", (e) => {
    const typing = /INPUT|TEXTAREA/.test(document.activeElement.tagName);
    if ((e.metaKey || e.ctrlKey) && e.key.toLowerCase() === "k") { e.preventDefault(); $("#q").focus(); $("#q").select(); }
    else if (e.key === "/" && !typing) { e.preventDefault(); $("#q").focus(); }
    else if (e.key === "Escape") { $("#lb").classList.remove("on"); if (typing) { $("#q").value = ""; query = ""; applySearch(); $("#q").blur(); } }
  });
  $("#main").addEventListener("click", (e) => {
    const g = e.target.closest("[data-gen]");
    if (g) { gen = g.dataset.gen; render(); return; }
    if (e.target.matches(".shot img")) { $("#lb img").src = e.target.src; $("#lb").classList.add("on"); }
  });
  $("#main").addEventListener("error", (e) => { if (e.target.matches(".shot img")) e.target.closest(".shot").classList.add("broken"); }, true);
  $("#lb").addEventListener("click", () => $("#lb").classList.remove("on"));
  $(".burger").addEventListener("click", () => $("#nav").classList.toggle("open"));
  $("#nav").addEventListener("click", () => $("#nav").classList.remove("open"));
  addEventListener("hashchange", route);

  render();
  route();
})();
