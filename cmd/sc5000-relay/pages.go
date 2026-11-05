package main

import (
	"html/template"
	"log"
	"net/http"
)

type pageData struct {
	Title, Message, Error   string
	Token, Provider, UserID string
	Done                    bool
	// Spotify
	ClientID, HTTPSRedirect, AuthHTTPS, AuthLoopback string
	Auto                                             bool
	// panel
	Backend string
}

func renderPage(w http.ResponseWriter, code int, name string, d pageData) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("X-Frame-Options", "DENY")
	w.WriteHeader(code)
	if err := pages.ExecuteTemplate(w, name, d); err != nil {
		log.Printf("template %s: %v", name, err)
	}
}

var pages = template.Must(template.New("").Parse(`
{{define "head"}}<!doctype html><html lang="zh-CN"><head><meta charset="utf-8">
<meta name="viewport" content="width=device-width,initial-scale=1,viewport-fit=cover">
<meta name="color-scheme" content="dark"><title>{{.Title}}</title>
<style>
:root{--bg:#0d0f14;--card:#171a22;--line:#262a35;--fg:#e8eaf0;--mute:#8b91a1;--acc:#1ed760;--dz:#a238ff;--err:#ff5d6c}
*{box-sizing:border-box}body{margin:0;background:var(--bg);color:var(--fg);font:16px/1.55 -apple-system,BlinkMacSystemFont,"PingFang SC","Noto Sans CJK SC",sans-serif;padding:max(20px,env(safe-area-inset-top)) 16px 40px}
.wrap{max-width:520px;margin:0 auto}.wide{max-width:860px}
h1{font-size:22px;margin:8px 0 4px}h2{font-size:17px;margin:0 0 10px}.sub{color:var(--mute);font-size:14px;margin:0 0 18px}
.card{background:var(--card);border:1px solid var(--line);border-radius:16px;padding:18px;margin:14px 0}
label{display:block;font-weight:600;margin:4px 0 6px}.hint{color:var(--mute);font-size:13px;margin:6px 0 0}
input,textarea{width:100%;background:#0f1218;border:1px solid var(--line);border-radius:12px;color:var(--fg);padding:13px 14px;font-size:16px;font-family:inherit}
textarea{min-height:84px;resize:vertical;word-break:break-all}input:focus,textarea:focus{outline:none;border-color:#4b5263}
.btn{display:block;width:100%;text-align:center;border:0;border-radius:999px;padding:15px 18px;font-size:17px;font-weight:700;cursor:pointer;text-decoration:none;margin-top:14px}
.green{background:var(--acc);color:#04130a}.purple{background:var(--dz);color:#fff}.ghost{background:transparent;color:var(--fg);border:1px solid var(--line)}
.small{display:inline-block;width:auto;padding:9px 14px;font-size:14px;margin:8px 8px 0 0}
.err{background:#2a1418;border:1px solid #5a2630;color:#ffc2c8;border-radius:12px;padding:12px 14px;margin:12px 0}
.ok{background:#10251a;border:1px solid #1f5a37;color:#b9f5cf;border-radius:12px;padding:12px 14px;margin:12px 0}
.big{font-size:64px;text-align:center;margin:30px 0 0}.center{text-align:center}
code,.mono{font-family:ui-monospace,SFMono-Regular,Menlo,monospace;font-size:13px;word-break:break-all}
.copy{background:#0f1218;border:1px dashed var(--line);border-radius:10px;padding:10px;margin-top:8px;user-select:all}
details summary{cursor:pointer;color:var(--mute)}.or{color:var(--mute);text-align:center;margin:10px 0;font-size:13px}
table{width:100%;border-collapse:collapse;font-size:13px}td,th{border-bottom:1px solid var(--line);padding:6px 4px;text-align:left;vertical-align:top}
.kv{display:grid;grid-template-columns:auto 1fr;gap:6px 14px;font-size:14px}.kv b{color:var(--mute);font-weight:500}
.badge{display:inline-block;border-radius:999px;padding:1px 8px;font-size:12px;background:#262a35}.warn{background:#4a3410;color:#ffd58a}.good{background:#103a22;color:#9ff0bf}
</style></head><body>{{end}}

{{define "errorPage"}}{{template "head" .}}<div class="wrap"><div class="big">⚠️</div><h1 class="center">{{.Title}}</h1><p class="sub center">{{.Message}}</p></div></body></html>{{end}}

{{define "donePage"}}{{template "head" .}}<div class="wrap"><div class="big">✅</div>
<h1 class="center">已完成连接！</h1><p class="sub center">{{if eq .Provider "deezer"}}碟机已自动登录 Deezer 与 Deemix。<br>{{end}}碟机屏幕上的二维码将自动关闭并加载歌单。<br>现在可以关闭此页面。</p></div></body></html>{{end}}

{{define "soundcloudPage"}}{{template "head" .}}<div class="wrap">
<h1>连接 SoundCloud</h1><p class="sub">SC5000 碟机扫码登录 · 手机用 5G/4G 也可以</p>
{{if .Done}}<div class="ok">此二维码已完成连接。重新提交会覆盖之前的设置。</div>{{end}}
{{if .Error}}<div class="err">{{.Error}}</div>{{end}}
{{if .AuthLoopback}}<div class="card"><h2>方式一：登录账号（可读私密歌单和私人收藏）</h2>
<a class="btn green" href="{{.AuthLoopback}}">打开 SoundCloud 账号登录授权</a>
<p class="hint">同意后会自动回到这里完成连接。如果跳到打不开的 127.0.0.1 页面，复制地址栏的完整地址粘贴到下面。</p>
<form method="post" id="scLogin">
<input id="callback" name="callback" autocomplete="off" autocapitalize="off" spellcheck="false" placeholder="http://127.0.0.1:8765/callback?code=...">
<button class="btn ghost" type="button" onclick="navigator.clipboard.readText().then(function(t){document.getElementById('callback').value=t.trim();document.getElementById('scLogin').submit()}).catch(function(){document.getElementById('callback').focus()})">一键读取剪贴板并完成登录</button>
<button class="btn ghost" type="submit">粘贴后点这里完成登录</button>
</form></div>{{end}}
<form method="post" class="card"><h2>方式二：只填公开主页（免登录）</h2>
<label for="user">主页链接、分享链接或用户名</label>
<input id="user" name="user" inputmode="url" autocomplete="off" autocapitalize="off" spellcheck="false" placeholder="https://soundcloud.com/你的主页">
<p class="hint">例如 https://soundcloud.com/你的主页。填好就能看到点赞的曲目和歌单。</p>
<label for="clientId">Client ID（已在后台配置时可留空）</label>
<input id="clientId" name="clientId" autocomplete="off" autocapitalize="off" spellcheck="false">
<label for="clientSecret">Client Secret（已在后台配置时可留空）</label>
<input id="clientSecret" name="clientSecret" type="password" autocomplete="off" autocapitalize="off" spellcheck="false">
<label for="token">OAuth Token（可选）</label>
<input id="token" name="token" type="password" autocomplete="off" autocapitalize="off" spellcheck="false">
<p class="hint">碟机只播放 API 标记为可完整播放的内容。</p>
<button class="btn purple" type="submit">完成连接</button>
</form></div></body></html>{{end}}

{{define "beatportPage"}}{{template "head" .}}<div class="wrap">
<h1>连接 Beatport</h1><p class="sub">SC5000 碟机扫码登录 · 手机用 5G/4G 也可以</p>
{{if .Done}}<div class="ok">此二维码已完成连接。重新提交会覆盖之前的登录。</div>{{end}}
{{if .Error}}<div class="err">{{.Error}}</div>{{end}}
<form class="card" method="post" action="/m/beatport?t={{.Token}}">
<label for="bp-user">Beatport 账号</label>
<input id="bp-user" name="user" autocomplete="username" autocapitalize="off" spellcheck="false" placeholder="邮箱或用户名">
<label for="bp-pass" style="margin-top:12px">密码</label>
<input id="bp-pass" name="password" type="password" autocomplete="current-password">
<p class="hint">中继只用密码换取登录令牌，碟机只保存令牌。Essential 播放 128k 流，Advanced 下载 128k AAC，Professional 下载 FLAC 或 256k AAC。</p>
<details style="margin-top:12px"><summary>或者粘贴 Token</summary>
<textarea name="token" autocomplete="off" autocapitalize="off" spellcheck="false" placeholder='{"access_token":"…","refresh_token":"…"} 或 Access Token'></textarea></details>
<button class="btn green">连接</button>
</form></div></body></html>{{end}}

{{define "deezerPage"}}{{template "head" .}}<div class="wrap">
<h1>连接 Deezer</h1><p class="sub">SC5000 碟机扫码登录 · 手机用 5G/4G 也可以</p>
{{if .Done}}<div class="ok">此二维码已完成连接。重新提交会覆盖之前的设置。</div>{{end}}
{{if .Error}}<div class="err">{{.Error}}</div>{{end}}
<div class="card">
<h2>方式一：在线登录 Deezer 账号（自动获取 ARL）</h2>
<p class="hint">在下方云端浏览器中登录 Deezer。登录成功后会自动连接碟机，并销毁登录窗口。五分钟无操作会关闭窗口。</p>
<button id="dz-start" class="btn purple" type="button">启动云端登录窗口</button>
<p id="dz-status" class="hint" role="status" aria-live="polite"></p>
<div id="dz-window" hidden>
<img id="dz-frame" width="390" height="680" draggable="false" alt="Deezer 云端登录画面，点击选择输入框" style="display:block;width:100%;max-width:390px;height:auto;margin:12px auto;touch-action:none;user-select:none">
<p class="hint">点击画面选择输入框；在画面上滑动，或用下方按钮滚动。</p>
<button class="btn ghost small" type="button" data-dz-wheel="-420">向上滚动</button>
<button class="btn ghost small" type="button" data-dz-wheel="420">向下滚动</button>
<label for="dz-text">输入文字（邮箱/密码/验证码）</label>
<input id="dz-text" type="password" autocomplete="off" autocapitalize="off" spellcheck="false" placeholder="先点击画面中的目标输入框">
<button id="dz-fill" class="btn ghost" type="button">填入当前焦点框</button>
<button class="btn ghost small" type="button" data-dz-key="Enter">按回车 (Enter)</button>
<button class="btn ghost small" type="button" data-dz-key="Tab">按 Tab</button>
<button class="btn ghost small" type="button" data-dz-key="Backspace">退格</button>
<button id="dz-stop" class="btn ghost" type="button">关闭并清除登录窗口</button>
</div></div>
<script>
(()=>{
 const token={{.Token}}, base='/m/deezer', query='?t='+encodeURIComponent(token);
 const start=document.getElementById('dz-start'), win=document.getElementById('dz-window'), img=document.getElementById('dz-frame'), status=document.getElementById('dz-status'), input=document.getElementById('dz-text');
 let active=false, timer, blobURL, chain=Promise.resolve();
 function queue(fn){const p=chain.then(fn);chain=p.catch(()=>{});return p;}
 function clearImage(){if(blobURL){URL.revokeObjectURL(blobURL);blobURL=null;}img.removeAttribute('src');}
 function stopped(message){active=false;clearTimeout(timer);win.hidden=true;start.disabled=false;input.value='';clearImage();status.textContent=message;}
 function done(){stopped('已完成连接！碟机已自动登录 Deezer 与 Deemix');location.replace(base+query);}
 async function response(r){const j=await r.json();if(!r.ok||!j.ok)throw new Error(j.error||'登录窗口暂不可用');if(j.status==='done'){done();return false;}return true;}
 async function frame(){
  if(!active)return;
  const r=await fetch(base+'/frame'+query+'&poll=1',{cache:'no-store',signal:AbortSignal.timeout(30000)});
  if(!(r.headers.get('Content-Type')||'').startsWith('image/jpeg')){await response(r);return;}
  if(!r.ok)throw new Error('无法获取登录画面');
  const blob=await r.blob();if(!active)return;
  const next=URL.createObjectURL(blob), old=blobURL;blobURL=next;img.src=next;if(old)URL.revokeObjectURL(old);
  status.textContent='请在画面中登录 Deezer，登录后将自动完成连接。';
 }
 function schedule(){clearTimeout(timer);if(active)timer=setTimeout(()=>queue(async()=>{try{await frame();schedule();}catch(e){stopped(e.message);}}),1000);}
 function act(action){
  clearTimeout(timer);
  return queue(async()=>{
   if(!active&&action.type!=='start')return;
   try{
    const r=await fetch(base+'/act'+query,{method:'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify(action),signal:AbortSignal.timeout(30000)});
    if(!await response(r))return;
    if(action.type==='stop'){stopped('登录窗口已关闭，浏览器数据已清除。');return;}
    active=true;win.hidden=false;start.disabled=true;
    await frame();schedule();
   }catch(e){stopped(e.message);}
  });
 }
 start.onclick=()=>{start.disabled=true;status.textContent='正在启动隔离登录窗口…';act({type:'start'});};
 document.getElementById('dz-stop').onclick=()=>act({type:'stop'});
 document.getElementById('dz-fill').onclick=()=>{const text=input.value;input.value='';if(text)act({type:'text',text});};
 document.querySelectorAll('[data-dz-key]').forEach(b=>b.onclick=()=>act({type:'key',key:b.dataset.dzKey}));
 document.querySelectorAll('[data-dz-wheel]').forEach(b=>b.onclick=()=>act({type:'wheel',dy:Number(b.dataset.dzWheel)}));
 let pointer;
 img.onpointerdown=e=>{if(!active)return;e.preventDefault();pointer={x:e.clientX,y:e.clientY,id:e.pointerId};img.setPointerCapture(e.pointerId);};
 img.onpointercancel=()=>{pointer=null;};
 img.onpointerup=e=>{
  if(!pointer||pointer.id!==e.pointerId)return;e.preventDefault();const p=pointer;pointer=null;
  const dy=p.y-e.clientY;
  if(Math.abs(dy)>15){act({type:'wheel',dy:Math.max(-1500,Math.min(1500,dy*2))});return;}
  const r=img.getBoundingClientRect(),x=(e.clientX-r.left)*390/r.width,y=(e.clientY-r.top)*680/r.height;
  if(x>=0&&x<390&&y>=0&&y<680)act({type:'click',x,y});
 };
 img.addEventListener('wheel',e=>{e.preventDefault();if(e.deltaY)act({type:'wheel',dy:Math.max(-1500,Math.min(1500,e.deltaY))});},{passive:false});
 // Pausing polling while hidden does not refresh the server idle deadline.
 document.addEventListener('visibilitychange',()=>{if(document.hidden)clearTimeout(timer);else schedule();});
 window.addEventListener('pagehide',()=>{clearTimeout(timer);input.value='';clearImage();if(active)fetch(base+'/act'+query,{method:'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify({type:'stop'}),keepalive:true}).catch(()=>{});});
})();
</script>
<form method="post" class="card">
<h2>方式二：直接粘贴分享链接 / 用户 ID / ARL</h2>
<label for="user">Deezer 链接或用户 ID</label>
<input id="user" name="user" inputmode="url" autocomplete="off" autocapitalize="off" spellcheck="false" placeholder="https://link.deezer.com/s/… 或 6850245583">
<p class="hint">支持 Deezer App「分享个人资料」的 link.deezer.com 短链、deezer.com/profile/…、deezer.com/playlist/… 或纯数字 ID。{{if .UserID}}当前服务端 User ID：<code>{{.UserID}}</code>{{end}}</p>
<div class="or">— 可以任填其一，也可以同时填写 —</div>
<label for="arl">Deezer ARL Cookie（可选）</label>
<textarea id="arl" name="arl" autocomplete="off" autocapitalize="off" spellcheck="false" placeholder="192 位十六进制 arl"></textarea>
<p class="hint">填入后自动登录 Deezer 歌单，并同时登录 Deemix 无损下载。在电脑浏览器登录 deezer.com 后，从开发者工具 → Cookie 里复制 <code>arl</code>。</p>
<button class="btn purple" type="submit">完成连接</button>
</form></div></body></html>{{end}}

{{define "spotifyPage"}}{{template "head" .}}<div class="wrap">
<h1>连接 Spotify</h1><p class="sub">SC5000 碟机扫码登录 · 手机用 5G/4G 也可以</p>
{{if .Done}}<div class="ok">此二维码已完成连接，碟机会自动加载歌单。</div>{{end}}
{{if .Error}}<div class="err">{{.Error}}</div>{{end}}
{{if .Auto}}
<div class="card"><a class="btn green" href="{{.AuthHTTPS}}">打开 Spotify 授权</a>
<p class="hint">登录并同意后会自动跳回这里完成连接。</p></div>
<details class="card"><summary>跳转失败？改用粘贴方式</summary>
<a class="btn ghost" href="{{.AuthLoopback}}">打开 Spotify 登录授权（127.0.0.1 回调）</a>
{{template "pasteForm" .}}</details>
{{else}}
<div class="card"><a class="btn green" href="{{.AuthLoopback}}">1. 打开 Spotify 登录授权</a>
<p class="hint">同意授权后，浏览器会跳到一个打不开的 <code>http://127.0.0.1:8080/…</code> 页面——这是正常的，复制地址栏里的完整链接即可。</p></div>
<div class="card"><label>2. 粘贴授权后地址栏的 127.0.0.1 链接（或 code）</label>{{template "pasteForm" .}}</div>
<div class="card"><h2>想要扫码后全自动跳转？</h2>
<p class="hint">在 developer.spotify.com 为该 App 添加 Redirect URI：</p>
<div class="copy mono">{{.HTTPSRedirect}}</div>
<p class="hint">添加后点下面按钮试一次，成功后以后扫码都会全自动完成：</p>
<a class="btn ghost" href="{{.AuthHTTPS}}">已添加，试试全自动跳转</a></div>
{{end}}
<details class="card"><summary>更换 Client ID（可选）</summary>
<form method="get"><input type="hidden" name="t" value="{{.Token}}">
<p class="hint">当前 Client ID：<code>{{.ClientID}}</code></p>
<input name="client_id" placeholder="32 位 Client ID" autocomplete="off" autocapitalize="off" spellcheck="false" pattern="[0-9a-fA-F]{32}">
<button class="btn ghost" type="submit">使用此 Client ID</button></form></details>
</div></body></html>{{end}}

{{define "pasteForm"}}<form method="post" id="pf">
<textarea name="code" id="code" autocomplete="off" autocapitalize="off" spellcheck="false" placeholder="http://127.0.0.1:8080/oauth/spotify/callback?code=…"></textarea>
<button class="btn green" type="button" id="clip">一键读取剪贴板并完成登录</button>
<button class="btn ghost" type="submit">提交</button></form>
<script>
document.getElementById('clip').onclick=async function(){
 try{const t=await navigator.clipboard.readText();if(!t){alert('剪贴板是空的');return}
 document.getElementById('code').value=t.trim();document.getElementById('pf').submit()}
 catch(e){alert('无法读取剪贴板，请长按输入框手动粘贴后点「提交」')}};
</script>{{end}}

{{define "loginPage"}}{{template "head" .}}<div class="wrap">
<h1>SC5000 Deemix</h1><p class="sub">扫码登录与凭证中继 · 请输入访问密码</p>
{{if .Error}}<div class="err">{{.Error}}</div>{{end}}
<form method="post" action="login" class="card"><label for="pw">访问密码</label>
<input id="pw" type="password" name="password" autocomplete="current-password" autofocus>
<button class="btn purple" type="submit">登录</button></form></div></body></html>{{end}}

{{define "panelPage"}}{{template "head" .}}<div class="wrap wide">
<div style="display:flex;justify-content:space-between;align-items:center"><h1>SC5000 Deemix 控制台</h1>
<form method="post" action="logout"><button class="btn ghost small" type="submit">退出</button></form></div>
<div id="msg"></div>
<div class="card"><h2>SC5000 后端</h2><div class="kv">
<b>后端地址</b><span class="mono" id="backend">{{.Backend}}</span>
<b>访问密码</b><span id="pwstate">…</span>
<b>Spotify 回调</b><span class="mono" id="redir"></span></div>
<p class="hint">此网页用于登录和传递凭证。音频由 SC5000 本机直接下载并保存。</p></div>

<div class="card"><h2>Deezer ARL Cookie</h2><p class="hint" id="arlstate">…</p>
<textarea id="arl" placeholder="粘贴新的 arl（保存时自动校验并提取 User ID）" autocomplete="off" spellcheck="false"></textarea>
<button class="btn purple small" onclick="save({deezerArl:val('arl')})">校验并保存 ARL</button>
<button class="btn ghost small" onclick="if(confirm('确定清除服务端保存的 ARL？'))save({clearArl:'1'})">清除 ARL</button></div>

<div class="card"><h2>Deezer User ID</h2>
<input id="uid" placeholder="数字 ID 或 Deezer 个人资料/歌单/短链" autocomplete="off" spellcheck="false">
<button class="btn purple small" onclick="save({deezerUserId:val('uid')})">保存</button></div>

<div class="card"><h2>Spotify Client ID</h2>
<input id="cid" placeholder="32 位十六进制" autocomplete="off" spellcheck="false">
<button class="btn green small" onclick="save({spotifyClientId:val('cid')})">保存</button></div>

<div class="card"><h2>BPM 增强（可选）</h2><p class="hint" id="bpmstate">…</p>
<input id="scid" placeholder="SoundCloud Client ID（留空不改）" autocomplete="off" spellcheck="false" style="margin-bottom:8px">
<input id="scsec" type="password" placeholder="SoundCloud Client Secret（留空不改）" autocomplete="off" style="margin-bottom:8px">
<input id="sctok" type="password" placeholder="SoundCloud OAuth Token（留空不改）" autocomplete="off" style="margin-bottom:8px">
<input id="gsb" type="password" placeholder="GetSongBPM API Key（留空不改）" autocomplete="off">
<button class="btn green small" onclick="save({soundcloudClientId:val('scid'),soundcloudClientSecret:val('scsec'),soundcloudToken:val('sctok'),getSongBpmKey:val('gsb')}).then(()=>{el('scid').value='';el('scsec').value='';el('sctok').value='';el('gsb').value=''})">保存</button>
<button class="btn ghost small" onclick="if(confirm('确定清除这些密钥？'))save({clearBpm:'1'})">清除</button>
<p class="hint"><a href="https://getsongbpm.com" target="_blank" rel="noreferrer">BPM data by GetSongBPM</a></p></div>

<div class="card"><h2>修改访问密码</h2>
<input id="cur" type="password" placeholder="当前密码" autocomplete="current-password" style="margin-bottom:8px">
<input id="np" type="password" placeholder="新密码（至少 8 位）" autocomplete="new-password">
<button class="btn ghost small" onclick="save({currentPassword:val('cur'),newPassword:val('np')}).then(()=>{el('cur').value='';el('np').value=''})">修改密码</button></div>


</div>
<script>
const el=id=>document.getElementById(id),val=id=>el(id).value.trim();
function esc(s){return String(s??'').replace(/[&<>"']/g,c=>({'&':'&amp;','<':'&lt;','>':'&gt;','"':'&quot;',"'":'&#39;'}[c]))}
function show(ok,lines){el('msg').innerHTML=lines.length?'<div class="'+(ok?'ok':'err')+'">'+lines.map(esc).join('<br>')+'</div>':''}
function render(c){
 el('pwstate').innerHTML=c.passwordIsDefault?'<span class="badge warn">仍为默认初始密码，建议修改</span>':'<span class="badge good">已自定义</span>'+(c.passwordChangedAt?' '+esc(new Date(c.passwordChangedAt).toLocaleString()):'');
 el('redir').textContent=c.spotifyRedirect;
 el('arlstate').innerHTML=c.deezerArlSet?'已保存：<code>'+esc(c.deezerArlMasked)+'</code>'+(c.deezerArlUser?' · 用户 '+esc(c.deezerArlUser):''):'<span class="badge warn">未设置 ARL</span>（请先保存 ARL，供 SC5000 同步登录）';
 el('bpmstate').textContent='SoundCloud Client ID：'+(c.soundcloudClientIdSet?'已设置':'未设置')+' · Secret：'+(c.soundcloudSecretSet?'已设置':'未设置')+' · Token：'+(c.soundcloudTokenSet?'已设置':'未设置')+' · GetSongBPM：'+(c.getSongBpmKeySet?'已设置':'未设置');
 el('uid').value=c.deezerUserId||'';el('cid').value=c.spotifyClientId||'';}
async function api(path,opt){const r=await fetch(path,Object.assign({credentials:'same-origin',headers:{'Content-Type':'application/json'}},opt||{}));
 if(r.status==401){location.reload();throw new Error('unauthorized')}return r.json()}
async function load(){const j=await api('api/config');render(j.config)}
async function save(body){const j=await api('api/config',{method:'POST',body:JSON.stringify(body)});
 if(j.config)render(j.config);show(j.ok,j.ok?(j.messages||[]):[j.error]);if(j.ok)el('arl').value=''}
load();
</script></body></html>{{end}}
`))
