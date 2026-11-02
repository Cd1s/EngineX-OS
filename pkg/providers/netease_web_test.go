package providers

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestNeteaseDirectQRLogin(t *testing.T) {
	polls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.FormValue("params") == "" || len(r.FormValue("encSecKey")) != 256 {
			t.Errorf("%s: not a weapi request", r.URL.Path)
		}
		switch r.URL.Path {
		case "/weapi/login/qrcode/unikey":
			json.NewEncoder(w).Encode(map[string]any{"code": 200, "unikey": "K1"})
		case "/weapi/login/qrcode/client/login":
			polls++
			if polls == 1 {
				json.NewEncoder(w).Encode(map[string]any{"code": 802})
				return
			}
			http.SetCookie(w, &http.Cookie{Name: "MUSIC_U", Value: "secret"})
			json.NewEncoder(w).Encode(map[string]any{"code": 803})
		case "/weapi/w/nuser/account/get":
			if !strings.Contains(r.Header.Get("Cookie"), "MUSIC_U=secret") {
				t.Error("login cookie not sent")
			}
			json.NewEncoder(w).Encode(map[string]any{"code": 200, "profile": map[string]any{"userId": 42}})
		default:
			t.Errorf("unexpected %s", r.URL.Path)
		}
	}))
	defer srv.Close()
	n := &Netease{WebURL: srv.URL, CookiePath: t.TempDir() + "/owner.json"}
	q, err := n.BeginQR(context.Background())
	if err != nil || q.Key != "K1" || q.URL != "https://music.163.com/login?codekey=K1" {
		t.Fatalf("%+v %v", q, err)
	}
	if code, err := n.PollQR(context.Background(), q.Key); err != nil || code != 802 {
		t.Fatalf("%d %v", code, err)
	}
	if code, err := n.PollQR(context.Background(), q.Key); err != nil || code != 803 || n.UserID != 42 {
		t.Fatalf("%d %v uid=%d", code, err, n.UserID)
	}
}

func TestWeapiEncryptionMatchesTheFormat(t *testing.T) {
	cipherText, err := weapiAES([]byte("hello"), weapiNonce)
	if err != nil {
		t.Fatal(err)
	}
	data, err := base64.StdEncoding.DecodeString(cipherText)
	if err != nil {
		t.Fatal(err)
	}
	block, _ := aes.NewCipher([]byte(weapiNonce))
	cipher.NewCBCDecrypter(block, []byte(weapiIV)).CryptBlocks(data, data)
	if string(data[:5]) != "hello" || data[len(data)-1] != 11 {
		t.Fatalf("%q", data)
	}
	params, key, err := weapiEncrypt([]byte(`{}`))
	if err != nil || params == "" || len(key) != 256 {
		t.Fatalf("%q %q %v", params, key, err)
	}
}
