package webui

// The pages a phone opens from the deck's QR code when no relay is set up: the
// deck alone, on the same Wi-Fi as the phone. They are in English unless the QR
// code carries the deck's language or the switch at the top picks another.

import (
	"encoding/json"
	"html"
	"net/http"
	"net/url"
	"strings"
)

var phoneLangs = [...]struct{ id, label string }{{"en", "EN"}, {"zh-Hans", "简体中文"}, {"zh-Hant", "繁體中文"}}

func phoneLang(r *http.Request) int {
	for i, l := range phoneLangs {
		if r.URL.Query().Get("lang") == l.id {
			return i
		}
	}
	return 0
}

// phoneText is every phrase in English, Simplified and Traditional Chinese. Some
// hold markup; none holds user input.
var phoneText = map[string][3]string{
	"expired":       {"This QR code has expired", "二维码已失效", "QR Code 已失效"},
	"expiredHint":   {"Open the QR code on the deck again.", "请在碟机屏幕上重新打开二维码。", "請在碟機螢幕上重新開啟 QR Code。"},
	"done":          {"Connected", "已连接", "已連接"},
	"doneHint":      {"Go back to the deck. The QR code closes by itself and your playlists appear.", "回到碟机即可：二维码会自动关闭，歌单随后出现。", "回到碟機即可：QR Code 會自動關閉，歌單隨後出現。"},
	"retry":         {"Try again", "重试", "重試"},
	"connect":       {"Connect", "连接", "連接"},
	"howTo":         {"Show me how", "查看教程", "查看教學"},
	"privacy":       {"Everything you enter stays on this deck.", "填写的内容只保存在这台碟机上。", "填寫的內容只儲存在這台碟機上。"},
	"open":          {"Connect %s", "连接 %s", "連接 %s"},
	"badForm":       {"Please fill in one of the fields", "请至少填写一项", "請至少填寫一項"},
	"deezerFLAC":    {"Enter your Deezer ARL to recognise your account and playlists automatically and enable lossless FLAC downloads.", "填入您的 Deezer ARL 即可自动识别账号歌单并启用 FLAC 无损下载。", "填入您的 Deezer ARL 即可自動識別帳號歌單並啟用 FLAC 無損下載。"},
	"deezerARL":     {"Log in with your ARL (recommended)", "用 ARL 登录（推荐）", "用 ARL 登入（推薦）"},
	"deezerARLPh":   {"Paste your ARL (192 characters)", "粘贴 ARL（192 个字符）", "貼上 ARL（192 個字元）"},
	"deezerStep1":   {"On a computer, open deezer.com and log in.", "在电脑上打开 deezer.com 并登录。", "在電腦上開啟 deezer.com 並登入。"},
	"deezerOpen":    {"Open deezer.com", "打开 deezer.com", "開啟 deezer.com"},
	"deezerStep2":   {"Press F12 (Mac: Option+Command+I). Go to <b>Application</b> (Firefox and Safari: <b>Storage</b>) → <b>Cookies</b> → <b>https://www.deezer.com</b>.", "按 F12（Mac：Option+Command+I），打开 <b>Application</b>（Firefox、Safari 叫 <b>存储/Storage</b>）→ <b>Cookies</b> → <b>https://www.deezer.com</b>。", "按 F12（Mac：Option+Command+I），開啟 <b>Application</b>（Firefox、Safari 叫 <b>儲存空間/Storage</b>）→ <b>Cookies</b> → <b>https://www.deezer.com</b>。"},
	"deezerStep3":   {"Copy the <b>Value</b> of the row named <b>arl</b> and paste it above. Send it to this phone with any chat or notes app.", "复制名为 <b>arl</b> 那一行的 <b>Value</b>，粘贴到上面。可以用聊天或备忘录 App 发到这台手机。", "複製名為 <b>arl</b> 那一列的 <b>Value</b>，貼到上面。可以用聊天或備忘錄 App 傳到這支手機。"},
	"deezerNote":    {"Phone browsers cannot show this cookie, so this step needs a computer. FLAC needs a paid Deezer plan; free accounts get MP3.", "手机浏览器看不到这个 Cookie，所以这一步要用电脑。FLAC 需要 Deezer 付费会员，免费账号只有 MP3。", "手機瀏覽器看不到這個 Cookie，所以這一步要用電腦。FLAC 需要 Deezer 付費方案，免費帳號只有 MP3。"},
	"deezerUser":    {"Or link your profile (public playlists, no downloads)", "或者只绑定资料页（公开歌单，不能下载）", "或者只綁定個人頁面（公開歌單，不能下載）"},
	"deezerUserPh":  {"Profile link, share link or user ID", "资料页链接、分享链接或用户 ID", "個人頁面連結、分享連結或使用者 ID"},
	"deezerUserHow": {"In the Deezer app open <b>Favourites</b>, tap your name, then <b>Share</b> → <b>Copy link</b>. Only public playlists show up.", "在 Deezer App 打开<b>收藏</b>，点你的名字，再点<b>分享</b> → <b>复制链接</b>。只显示公开歌单。", "在 Deezer App 開啟<b>收藏</b>，點你的名字，再點<b>分享</b> → <b>複製連結</b>。只顯示公開歌單。"},
	"deezerBadARL":  {"That ARL is invalid or expired", "这个 ARL 无效或已过期", "這個 ARL 無效或已過期"},
	"deezerBadUser": {"No Deezer profile found", "没有找到这个 Deezer 资料页", "找不到這個 Deezer 個人頁面"},
	"deezerNoLists": {"Could not read that profile's public playlists", "无法读取这个资料页的公开歌单", "無法讀取這個個人頁面的公開歌單"},
	"scIntro":       {"Enter your SoundCloud username or profile URL. Your likes and playlists appear on the deck a few seconds later. No password, no Artist Pro and no API app needed.", "填写 SoundCloud 用户名或主页链接，几秒后点赞和歌单就会出现在碟机上。不用密码，不用 Artist Pro，也不用申请 API。", "填寫 SoundCloud 使用者名稱或主頁連結，幾秒後按讚和歌單就會出現在碟機上。不用密碼，不用 Artist Pro，也不用申請 API。"},
	"scUserPh":      {"Enter your SoundCloud username or profile URL", "SoundCloud 用户名或主页链接", "SoundCloud 使用者名稱或主頁連結"},
	"scHow":         {"In the SoundCloud app open your profile, tap <b>⋯</b> → <b>Share</b> → <b>Copy link</b>. Or use the name after soundcloud.com/ in your profile address.", "在 SoundCloud App 打开你的主页，点 <b>⋯</b> → <b>分享</b> → <b>复制链接</b>。也可以直接填主页地址里 soundcloud.com/ 后面的名字。", "在 SoundCloud App 開啟你的主頁，點 <b>⋯</b> → <b>分享</b> → <b>複製連結</b>。也可以直接填主頁網址裡 soundcloud.com/ 後面的名字。"},
	"scAdvanced":    {"Advanced (private playlists only)", "高级（只有私密歌单才需要）", "進階（只有私人歌單才需要）"},
	"scTokenHow":    {"On a computer, log in at soundcloud.com, press F12 → <b>Application</b> → <b>Cookies</b> → <b>https://soundcloud.com</b> and copy the value of <b>oauth_token</b> (it looks like 2-123456-…).", "在电脑上登录 soundcloud.com，按 F12 → <b>Application</b> → <b>Cookies</b> → <b>https://soundcloud.com</b>，复制 <b>oauth_token</b> 的值（形如 2-123456-…）。", "在電腦上登入 soundcloud.com，按 F12 → <b>Application</b> → <b>Cookies</b> → <b>https://soundcloud.com</b>，複製 <b>oauth_token</b> 的值（形如 2-123456-…）。"},
	"scTokenPh":     {"oauth_token (optional)", "oauth_token（可选）", "oauth_token（選填）"},
	"scAppHow":      {"Own API app (optional): SoundCloud only lets Artist Pro accounts register apps at soundcloud.com/you/apps. If you have one, enter its Client ID and Secret.", "自有 API 应用（可选）：SoundCloud 只允许 Artist Pro 账号在 soundcloud.com/you/apps 注册应用。有的话填它的 Client ID 和 Secret。", "自有 API 應用（選填）：SoundCloud 只允許 Artist Pro 帳號在 soundcloud.com/you/apps 註冊應用。有的話填它的 Client ID 和 Secret。"},
	"scNote":        {"Only tracks SoundCloud lets you play in full are listed; Go+ previews are skipped.", "只列出 SoundCloud 允许完整播放的曲目，Go+ 试听片段会跳过。", "只列出 SoundCloud 允許完整播放的曲目，Go+ 試聽片段會略過。"},
	"scFailed":      {"Could not connect SoundCloud", "无法连接 SoundCloud", "無法連接 SoundCloud"},
	"scFailedHint":  {"Check the username or link. A pasted oauth_token may have expired.", "请检查用户名或链接；粘贴的 oauth_token 可能已过期。", "請檢查使用者名稱或連結；貼上的 oauth_token 可能已過期。"},
	"bpIntro":       {"Log in with your Beatport account. The deck keeps only the login token, never the password. Essential plays 128k streams, Advanced downloads 128k AAC, Professional downloads FLAC or 256k AAC.", "用 Beatport 账号登录。碟机只保存登录令牌，不保存密码。Essential 播放 128k 流，Advanced 下载 128k AAC，Professional 下载 FLAC 或 256k AAC。", "用 Beatport 帳號登入。碟機只儲存登入權杖，不儲存密碼。Essential 播放 128k 串流，Advanced 下載 128k AAC，Professional 下載 FLAC 或 256k AAC。"},
	"bpUser":        {"Email or username", "邮箱或用户名", "電子郵件或使用者名稱"},
	"bpPassword":    {"Password", "密码", "密碼"},
	"bpAdvanced":    {"Advanced: paste a token", "高级：粘贴 Token", "進階：貼上 Token"},
	"bpTokenPh":     {"Token JSON or access token", "Token JSON 或 Access Token", "Token JSON 或 Access Token"},
	"bpFailed":      {"Could not log in to Beatport", "无法登录 Beatport", "無法登入 Beatport"},
	"bpFailedHint":  {"Check the email and password, or the token.", "请检查账号密码或 Token。", "請檢查帳號密碼或 Token。"},
	"spStep1":       {"Step 1: log in to Spotify and tap <b>Agree</b>.", "第 1 步：登录 Spotify，点<b>同意</b>。", "第 1 步：登入 Spotify，點<b>同意</b>。"},
	"spOpen":        {"Open Spotify login", "打开 Spotify 登录", "開啟 Spotify 登入"},
	"spStep2":       {"Step 2: Spotify then opens a page starting with <b>127.0.0.1</b> that cannot load. That is expected. Copy the whole address from the address bar, come back to this tab, and paste it here.", "第 2 步：之后会打开一个以 <b>127.0.0.1</b> 开头、显示无法访问的页面，这是正常的。复制地址栏里的完整地址，回到这个页面粘贴到下面。", "第 2 步：之後會開啟一個以 <b>127.0.0.1</b> 開頭、顯示無法連線的頁面，這是正常的。複製網址列裡的完整網址，回到這個頁面貼到下面。"},
	"spPaste":       {"Paste & Connect", "粘贴并连接", "貼上並連接"},
	"spPastePh":     {"http://127.0.0.1:8080/oauth/spotify/callback?code=…", "http://127.0.0.1:8080/oauth/spotify/callback?code=…", "http://127.0.0.1:8080/oauth/spotify/callback?code=…"},
	"spHelp":        {"Email code or a \"new device\" page on the way? Finish them; this page waits 30 minutes. On iPhone, long-press the address bar and choose Copy. Only the part with code= matters.", "中途要邮箱验证码或「新设备」确认？照做即可，这个页面会等 30 分钟。iPhone 上长按地址栏选「拷贝」。只要包含 code= 的那部分。", "中途要電子郵件驗證碼或「新裝置」確認？照做即可，這個頁面會等 30 分鐘。iPhone 上長按網址列選「拷貝」。只要包含 code= 的那部分。"},
	"spLinksFailed": {"Could not read these Spotify playlist links (public playlists/albums only)", "读取不了这些 Spotify 歌单链接（只支持公开歌单/专辑）", "讀取不了這些 Spotify 歌單連結（只支援公開歌單/專輯）"},
	"spLinks":       {"No account? Paste public playlist or album links instead:", "没有账号？可以直接粘贴公开歌单/专辑链接：", "沒有帳號？可以直接貼上公開歌單/專輯連結："},
	"spBadPaste":    {"No code= found in what you pasted", "粘贴的内容里没有 code=", "貼上的內容裡沒有 code="},
	"spOtherLogin":  {"That address belongs to another login. Open Spotify login again from this page.", "这个地址属于另一次登录，请在本页重新打开 Spotify 登录。", "這個網址屬於另一次登入，請在本頁重新開啟 Spotify 登入。"},
	"amIntro":       {"Paste the link of any Apple Music playlist or album: no account needed. In the Music app tap <b>···</b> → <b>Share</b> → <b>Copy Link</b>.", "粘贴任意 Apple Music 歌单或专辑的链接，不需要账号。在“音乐”App 里点 <b>···</b> → <b>分享</b> → <b>拷贝链接</b>。", "貼上任意 Apple Music 歌單或專輯的連結，不需要帳號。在「音樂」App 裡點 <b>···</b> → <b>分享</b> → <b>拷貝連結</b>。"},
	"amLinks":       {"Playlist or album links, one per line", "歌单或专辑链接，每行一个", "歌單或專輯連結，每行一個"},
	"amAudio":       {"Apple Music audio is protected, so tracks play from Deezer (lossless FLAC) or NetEase, matched by ISRC or by title and artist. Connect Deezer as well for FLAC.", "Apple Music 的音频受保护，所以曲目按 ISRC 或歌名加歌手匹配，从 Deezer（无损 FLAC）或网易云播放。想要 FLAC 请同时连接 Deezer。", "Apple Music 的音訊受保護，所以曲目依 ISRC 或歌名加歌手比對，從 Deezer（無損 FLAC）或網易雲播放。想要 FLAC 請同時連接 Deezer。"},
	"amLibrary":     {"Your library (optional)", "你的资料库（可选）", "你的資料庫（可選）"},
	"amLibraryHow":  {"On a computer, open music.apple.com in Chrome or Safari and sign in. Open the developer tools → Application (Storage) → Cookies → music.apple.com. Copy the whole value of <b>media-user-token</b> and paste it here.", "在电脑上用 Chrome 或 Safari 打开 music.apple.com 并登录。打开开发者工具 → 应用（存储）→ Cookie → music.apple.com，复制 <b>media-user-token</b> 的完整值，粘贴到这里。", "在電腦上用 Chrome 或 Safari 開啟 music.apple.com 並登入。開啟開發者工具 → 應用程式（儲存空間）→ Cookie → music.apple.com，複製 <b>media-user-token</b> 的完整值，貼到這裡。"},
	"amStorefront":  {"Store region (automatic from the links)", "商店地区（默认取链接里的地区）", "商店地區（預設取連結裡的地區）"},
	"amAdvanced":    {"Your own Apple developer token", "自己的 Apple 开发者令牌", "自己的 Apple 開發者權杖"},
	"amDevHow":      {"Optional. Without one the deck uses the token of Apple's web player. With your own MusicKit token, this page also offers Sign in with Apple Music.", "可选。不填时碟机使用 Apple 网页播放器的令牌。填入自己的 MusicKit 令牌后，本页还会出现“用 Apple Music 登录”。", "可選。不填時碟機使用 Apple 網頁播放器的權杖。填入自己的 MusicKit 權杖後，本頁還會出現「用 Apple Music 登入」。"},
	"amSignIn":      {"Sign in with Apple Music", "用 Apple Music 登录", "用 Apple Music 登入"},
	"amFallback":    {"Apple did not allow sign-in here. On a computer or phone, open music.apple.com, sign in, then run this bookmarklet (it copies your token) and paste the token below:", "Apple 不允许在这里登录。请在 music.apple.com 登录后运行下面的书签脚本（会复制你的令牌），再把令牌粘贴到下方：", "Apple 不允許在這裡登入。請在 music.apple.com 登入後執行下面的書籤腳本（會複製你的權杖），再把權杖貼到下方："},
	"amFailed":      {"Apple Music did not accept this", "Apple Music 没有接受", "Apple Music 沒有接受"},
	"amFailedHint":  {"Check that each link opens in Apple Music and that the token was copied whole.", "请确认每个链接都能在 Apple Music 里打开，令牌也复制完整。", "請確認每個連結都能在 Apple Music 裡開啟，權杖也複製完整。"},
	"spRefused":     {"Spotify refused this login. A code works only once: open Spotify login again.", "Spotify 拒绝了这次登录。code 只能用一次，请重新打开 Spotify 登录。", "Spotify 拒絕了這次登入。code 只能用一次，請重新開啟 Spotify 登入。"},
}

func tr(lang int, key string) string {
	v, ok := phoneText[key]
	if !ok {
		panic("webui: no phone text " + key) // caught by the tests
	}
	return v[lang]
}

// phonePage writes a page with the language switch. r supplies the address the
// switch keeps, including the QR code's token.
func phonePage(w http.ResponseWriter, r *http.Request, body string) {
	lang := phoneLang(r)
	var sw strings.Builder
	for i, l := range phoneLangs {
		q := r.URL.Query()
		q.Set("lang", l.id)
		if i > 0 {
			sw.WriteString(" | ")
		}
		if i == lang {
			sw.WriteString("<b>" + l.label + "</b>")
		} else {
			sw.WriteString(`<a href="` + html.EscapeString(r.URL.Path+"?"+q.Encode()) + `">` + l.label + `</a>`)
		}
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Write([]byte(`<!doctype html><html lang="` + phoneLangs[lang].id + `"><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1"><title>EngineX OS</title><style>` +
		`body{background:#111;color:#eee;font:17px/1.5 system-ui,sans-serif;padding:16px 20px 40px;max-width:560px;margin:auto}` +
		`nav{text-align:right;font-size:14px;color:#888}nav a{color:#9cf}` +
		`input,textarea,select,button,a.b{display:block;width:100%;box-sizing:border-box;font:inherit;padding:14px;margin:10px 0;border-radius:10px;border:1px solid #444;background:#1c1c1c;color:#eee;text-align:center;text-decoration:none}` +
		`button,a.b{background:#2a6;border:0;color:#fff;font-weight:600}a.b.alt{background:#333}` +
		`.note{background:#1d2a3a;border-left:4px solid #4af;padding:10px 12px;border-radius:6px}` +
		`details{background:#1a1a1a;border-radius:10px;padding:8px 12px;margin:12px 0}summary{cursor:pointer;color:#9cf}` +
		`ol{padding-left:22px}li{margin:6px 0}small{color:#999}h3{margin-top:28px}</style>` +
		`<nav>` + sw.String() + `</nav>` + body))
}

// phoneAction is where a page posts back, keeping the token and the language.
func phoneAction(r *http.Request, flow *loginFlow) string {
	q := url.Values{"t": {flow.token}, "lang": {phoneLangs[phoneLang(r)].id}}
	return "/m/" + flow.Provider + "?" + q.Encode()
}

func phoneMessage(w http.ResponseWriter, r *http.Request, flow *loginFlow, title, hint string) {
	lang := phoneLang(r)
	body := "<h2>" + html.EscapeString(title) + "</h2>"
	if hint != "" {
		body += "<p>" + html.EscapeString(hint) + "</p>"
	}
	if flow != nil && flow.token != "" {
		body += `<a class="b" href="` + html.EscapeString(phoneAction(r, flow)) + `">` + tr(lang, "retry") + `</a>`
	}
	phonePage(w, r, body)
}

func serviceName(provider string) string {
	return map[string]string{"deezer": "Deezer", "soundcloud": "SoundCloud", "beatport": "Beatport", "spotify": "Spotify", "applemusic": "Apple Music"}[provider]
}

func phoneHeading(lang int, provider string) string {
	return `<h2><img src="/icons/` + provider + `" width="32" height="32" alt="" style="vertical-align:middle;border-radius:8px;margin-right:10px">` + strings.Replace(tr(lang, "open"), "%s", serviceName(provider), 1) + "</h2>"
}

func deezerPhonePage(lang int, action string) string {
	a := html.EscapeString(action)
	return phoneHeading(lang, "deezer") +
		`<p class="note">` + tr(lang, "deezerFLAC") + `</p>` +
		`<h3>` + tr(lang, "deezerARL") + `</h3><form method="post" action="` + a + `"><input name="arl" placeholder="` + tr(lang, "deezerARLPh") + `" autocomplete="off" autocapitalize="off" spellcheck="false"><button>` + tr(lang, "connect") + `</button></form>` +
		`<details open><summary>` + tr(lang, "howTo") + `</summary><ol><li>` + tr(lang, "deezerStep1") + `<a class="b alt" target="_blank" rel="noreferrer" href="https://www.deezer.com/login">` + tr(lang, "deezerOpen") + `</a></li><li>` + tr(lang, "deezerStep2") + `</li><li>` + tr(lang, "deezerStep3") + `</li></ol><small>` + tr(lang, "deezerNote") + `</small></details>` +
		`<h3>` + tr(lang, "deezerUser") + `</h3><form method="post" action="` + a + `"><input name="user" placeholder="` + tr(lang, "deezerUserPh") + `" autocomplete="off" autocapitalize="off"><button>` + tr(lang, "connect") + `</button></form>` +
		`<details><summary>` + tr(lang, "howTo") + `</summary><p>` + tr(lang, "deezerUserHow") + `</p></details>` +
		`<small>` + tr(lang, "privacy") + `</small>`
}

func soundCloudPhonePage(lang int, action string) string {
	a := html.EscapeString(action)
	return phoneHeading(lang, "soundcloud") + `<p>` + tr(lang, "scIntro") + `</p>` +
		`<form method="post" action="` + a + `"><input name="user" placeholder="` + tr(lang, "scUserPh") + `" autocomplete="off" autocapitalize="off" spellcheck="false"><button>` + tr(lang, "connect") + `</button>` +
		`<details><summary>` + tr(lang, "howTo") + `</summary><p>` + tr(lang, "scHow") + `</p></details>` +
		`<details><summary>` + tr(lang, "scAdvanced") + `</summary><p>` + tr(lang, "scTokenHow") + `</p><input name="token" type="password" placeholder="` + tr(lang, "scTokenPh") + `" autocomplete="off">` +
		`<p>` + tr(lang, "scAppHow") + `</p><input name="clientId" placeholder="Client ID" autocomplete="off"><input name="clientSecret" type="password" placeholder="Client Secret" autocomplete="off"></details></form>` +
		`<small>` + tr(lang, "scNote") + " " + tr(lang, "privacy") + `</small>`
}

func beatportPhonePage(lang int, action string) string {
	a := html.EscapeString(action)
	return phoneHeading(lang, "beatport") + `<p>` + tr(lang, "bpIntro") + `</p>` +
		`<form method="post" action="` + a + `"><input name="user" placeholder="` + tr(lang, "bpUser") + `" autocomplete="username" autocapitalize="off"><input name="password" type="password" placeholder="` + tr(lang, "bpPassword") + `" autocomplete="current-password"><button>` + tr(lang, "connect") + `</button>` +
		`<details><summary>` + tr(lang, "bpAdvanced") + `</summary><input name="token" placeholder="` + tr(lang, "bpTokenPh") + `" autocomplete="off" autocapitalize="off"></details></form>`
}

// amBookmarklet runs on music.apple.com and copies the signed-in media-user-token.
const amBookmarklet = `javascript:void(function(){var t;try{t=MusicKit.getInstance().musicUserToken}catch(e){}if(!t){var m=document.cookie.match(/media-user-token=([^;]+)/);t=m&&m[1]}t?(navigator.clipboard&&navigator.clipboard.writeText(t),prompt("media-user-token",t)):alert("Sign in first")}())`

// appleMusicPhonePage offers links (no account), the library token and, with
// the owner's own developer token, MusicKit's sign-in, which fills the token in.
func appleMusicPhonePage(lang int, action, devToken string) string {
	a := html.EscapeString(action)
	stores := `<option value="">—</option>`
	for _, s := range []string{"us", "cn", "hk", "tw", "jp", "gb", "de", "fr", "au", "ca", "sg", "kr"} {
		stores += `<option>` + s + `</option>`
	}
	signIn := ""
	if devToken != "" {
		js, _ := json.Marshal(devToken) // escapes <, > and &, so it cannot end the script
		signIn = `<button type="button" id="amk">` + tr(lang, "amSignIn") + `</button>` +
			`<div id="amb" hidden><p>` + tr(lang, "amFallback") + `</p><input readonly value="` + html.EscapeString(amBookmarklet) + `" onfocus="this.select()"></div>` +
			`<script src="https://js-cdn.music.apple.com/musickit/v3/musickit.js" async></script><script>` +
			`document.getElementById("amk").onclick=async()=>{try{const m=await MusicKit.configure({developerToken:` + string(js) + `,app:{name:"EngineX OS",build:"5.3.0.2"}});` +
			`document.getElementById("amt").value=await m.authorize();document.getElementById("amf").submit()}catch(e){document.getElementById("amb").hidden=false}}</script>`
	}
	return phoneHeading(lang, "applemusic") + `<p>` + tr(lang, "amIntro") + `</p>` +
		`<form id="amf" method="post" action="` + a + `"><textarea name="links" rows="4" placeholder="https://music.apple.com/…/playlist/…" autocomplete="off" autocapitalize="off" spellcheck="false"></textarea>` +
		`<button>` + tr(lang, "connect") + `</button>` +
		`<h3>` + tr(lang, "amLibrary") + `</h3>` + signIn +
		`<input id="amt" name="userToken" type="password" placeholder="media-user-token" autocomplete="off">` +
		`<details><summary>` + tr(lang, "howTo") + `</summary><p>` + tr(lang, "amLibraryHow") + `</p></details>` +
		`<label>` + tr(lang, "amStorefront") + `<select name="storefront">` + stores + `</select></label>` +
		`<details><summary>` + tr(lang, "amAdvanced") + `</summary><p>` + tr(lang, "amDevHow") + `</p><input name="devToken" type="password" placeholder="eyJ…" autocomplete="off"></details>` +
		`<button>` + tr(lang, "connect") + `</button></form>` +
		`<p class="note">` + tr(lang, "amAudio") + `</p><small>` + tr(lang, "privacy") + `</small>`
}

func spotifyPhonePage(lang int, action, authorize string) string {
	return phoneHeading(lang, "spotify") +
		`<p>` + tr(lang, "spStep1") + `</p><a class="b" target="_blank" rel="noreferrer" href="` + html.EscapeString(authorize) + `">` + tr(lang, "spOpen") + `</a>` +
		`<p>` + tr(lang, "spStep2") + `</p><form method="post" action="` + html.EscapeString(action) + `"><input name="callback" placeholder="` + tr(lang, "spPastePh") + `" required autocomplete="off" autocapitalize="off" spellcheck="false"><button>` + tr(lang, "spPaste") + `</button></form>` +
		`<p>` + tr(lang, "spLinks") + `</p><form method="post" action="` + html.EscapeString(action) + `"><textarea name="links" rows="3" placeholder="https://open.spotify.com/playlist/…" autocomplete="off" autocapitalize="off" spellcheck="false"></textarea><button>` + tr(lang, "connect") + `</button></form>` +
		`<details><summary>` + tr(lang, "howTo") + `</summary><p>` + tr(lang, "spHelp") + `</p></details>`
}
