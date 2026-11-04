package providers

// Direct client for music.163.com, used when no NeteaseCloudMusicApi service is
// configured (BaseURL is empty). "weapi" is the request wrapping of NetEase's own
// web player; it carries the user's own login and grants nothing beyond it.

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"math/big"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

const (
	neteaseWebURL = "https://music.163.com"
	weapiNonce    = "0CoJUm6Qyw8W8jud"
	weapiIV       = "0102030405060708"
	weapiModulus  = "00e0b509f6259df8642dbc35662901477df22677ec152b5ff68ace615bb7b725152b3ab17a876aea8a5aa76d2e417629ec4ee341f56135fccf695280104e0312ecbda92557c93870114af6c9d05c4f7f0c3685b7a46bee255932575cce10b424d813cfe4875d3e82047b97ddef52741d546b8e289dc6935b3ece0462db0a22b8e7"
	weapiKeyChars = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"
)

func weapiAES(plain []byte, key string) (string, error) {
	block, err := aes.NewCipher([]byte(key))
	if err != nil {
		return "", err
	}
	pad := aes.BlockSize - len(plain)%aes.BlockSize
	data := append(bytes.Clone(plain), bytes.Repeat([]byte{byte(pad)}, pad)...)
	cipher.NewCBCEncrypter(block, []byte(weapiIV)).CryptBlocks(data, data)
	return base64.StdEncoding.EncodeToString(data), nil
}

// weapiEncrypt returns the two form fields of a weapi request.
func weapiEncrypt(payload []byte) (params, encSecKey string, err error) {
	raw := make([]byte, 16)
	if _, err = rand.Read(raw); err != nil {
		return "", "", err
	}
	key := make([]byte, 16)
	for i, b := range raw {
		key[i] = weapiKeyChars[int(b)%len(weapiKeyChars)]
	}
	if params, err = weapiAES(payload, weapiNonce); err != nil {
		return "", "", err
	}
	if params, err = weapiAES([]byte(params), string(key)); err != nil {
		return "", "", err
	}
	// Textbook RSA over the reversed key, as the web player does it.
	reversed := make([]byte, len(key))
	for i, b := range key {
		reversed[len(key)-1-i] = b
	}
	mod, _ := new(big.Int).SetString(weapiModulus, 16)
	enc := new(big.Int).Exp(new(big.Int).SetBytes(reversed), big.NewInt(0x10001), mod).Bytes()
	padded := make([]byte, 128)
	copy(padded[128-len(enc):], enc)
	return params, hex.EncodeToString(padded), nil
}

func (n *Netease) webBase() string {
	if n.WebURL != "" {
		return strings.TrimRight(n.WebURL, "/")
	}
	return neteaseWebURL
}

// weapi posts one request and returns the JSON object and session cookies.
func (n *Netease) weapi(ctx context.Context, path string, fields map[string]string) (map[string]any, []string, error) {
	n.mu.Lock()
	cookie := n.Cookie
	n.mu.Unlock()
	if csrf := cookieValue(cookie, "__csrf"); csrf != "" {
		fields["csrf_token"] = csrf
	}
	payload, err := json.Marshal(fields)
	if err != nil {
		return nil, nil, err
	}
	params, key, err := weapiEncrypt(payload)
	if err != nil {
		return nil, nil, err
	}
	form := url.Values{"params": {params}, "encSecKey": {key}}
	req, err := http.NewRequestWithContext(ctx, "POST", n.webBase()+"/weapi"+path, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Referer", "https://music.163.com/")
	req.Header.Set("User-Agent", "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0 Safari/537.36")
	req.Header.Set("Cookie", strings.TrimRight("os=pc; appver=8.9.75; "+cookie, "; "))
	resp, err := client(n.HTTP).Do(req) // never http.DefaultClient: it has no timeout
	if err != nil {
		return nil, nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if err != nil {
		return nil, nil, err
	}
	if resp.StatusCode != 200 {
		return nil, nil, errors.New("netease: HTTP " + strconv.Itoa(resp.StatusCode))
	}
	var out map[string]any
	if err = json.Unmarshal(body, &out); err != nil {
		return nil, nil, errors.New("netease: invalid response")
	}
	var cookies []string
	for _, c := range resp.Cookies() {
		if c.Value != "" {
			cookies = append(cookies, c.Name+"="+c.Value)
		}
	}
	return out, cookies, nil
}

func cookieValue(cookie, name string) string {
	for _, part := range strings.Split(cookie, ";") {
		if k, v, ok := strings.Cut(strings.TrimSpace(part), "="); ok && k == name {
			return v
		}
	}
	return ""
}

// web answers the API paths of the compatible service from music.163.com.
func (n *Netease) web(ctx context.Context, path string, q url.Values) (map[string]any, error) {
	var api string
	fields := map[string]string{}
	switch path {
	case "/login/qr/create":
		// The app scans this address; only the unikey comes from the server.
		return map[string]any{"code": float64(200), "data": map[string]any{"qrurl": "https://music.163.com/login?codekey=" + url.QueryEscape(q.Get("key"))}}, nil
	case "/login/qr/key":
		api, fields["type"] = "/login/qrcode/unikey", "1"
	case "/login/qr/check":
		api, fields["key"], fields["type"] = "/login/qrcode/client/login", q.Get("key"), "1"
	case "/login/status":
		api = "/w/nuser/account/get"
	case "/user/playlist":
		api = "/user/playlist"
		for _, k := range []string{"uid", "limit", "offset"} {
			fields[k] = q.Get(k)
		}
		fields["includeVideo"] = "true"
	case "/playlist/detail":
		api, fields["id"], fields["n"], fields["s"] = "/v6/playlist/detail", q.Get("id"), "100000", "8"
	case "/song/detail":
		api = "/v3/song/detail"
		ids := strings.Split(q.Get("ids"), ",")
		items := make([]string, len(ids))
		for i, id := range ids {
			items[i] = `{"id":` + id + `}`
		}
		fields["ids"], fields["c"] = "["+strings.Join(ids, ",")+"]", "["+strings.Join(items, ",")+"]"
	case "/album/sublist":
		api, fields["limit"], fields["offset"], fields["total"] = "/album/sublist", q.Get("limit"), q.Get("offset"), "true"
	case "/album":
		api = "/v1/album/" + q.Get("id")
	case "/dj/sublist":
		api, fields["limit"], fields["offset"], fields["total"] = "/djradio/get/subed", q.Get("limit"), q.Get("offset"), "true"
	case "/dj/program":
		api, fields["radioId"], fields["limit"], fields["offset"], fields["asc"] = "/dj/program/byradio", q.Get("id"), q.Get("limit"), q.Get("offset"), "false"
	case "/song/url/v1":
		api, fields["ids"], fields["level"], fields["encodeType"] = "/song/enhance/player/url/v1", "["+q.Get("id")+"]", q.Get("level"), "flac"
	case "/cloudsearch":
		api, fields["s"], fields["type"], fields["limit"], fields["offset"] = "/cloudsearch/pc", q.Get("keywords"), q.Get("type"), q.Get("limit"), "0"
	default:
		return nil, errors.New("netease: unsupported request")
	}
	out, cookies, err := n.weapi(ctx, api, fields)
	if err != nil {
		return nil, err
	}
	switch path {
	case "/login/qr/key":
		out["data"] = map[string]any{"unikey": out["unikey"]}
	case "/login/qr/check":
		out["cookie"] = strings.Join(cookies, "; ")
	case "/login/status":
		out["data"] = map[string]any{"profile": out["profile"]}
	}
	return out, nil
}
