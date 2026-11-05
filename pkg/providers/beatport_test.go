package providers

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"encoding/binary"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func bearer(r *http.Request) string {
	return strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
}

// The website login gives a session, the session an authorization code, the code a
// token. A token about to expire is renewed and saved before the next call.
func TestBeatportLoginRefreshAndBrowse(t *testing.T) {
	refreshes := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/auth/login/":
			var v map[string]string
			json.NewDecoder(r.Body).Decode(&v)
			if v["username"] != "dj" || v["password"] != "pw" {
				w.WriteHeader(400)
				return
			}
			http.SetCookie(w, &http.Cookie{Name: "sessionid", Value: "s1"})
		case "/auth/o/authorize/":
			if c, err := r.Cookie("sessionid"); err != nil || c.Value != "s1" || r.URL.Query().Get("client_id") != BeatportClientID {
				w.WriteHeader(403)
				return
			}
			http.Redirect(w, r, "https://www.beatport.com/?code=c1", http.StatusFound)
		case "/auth/o/token/":
			r.ParseForm()
			switch {
			case r.Form.Get("client_id") != BeatportClientID:
				w.WriteHeader(400)
			case r.Form.Get("grant_type") == "authorization_code" && r.Form.Get("code") == "c1":
				io.WriteString(w, `{"access_token":"a1","refresh_token":"r1","expires_in":60}`)
			case r.Form.Get("grant_type") == "refresh_token" && r.Form.Get("refresh_token") == "r1":
				refreshes++
				io.WriteString(w, `{"access_token":"a2","refresh_token":"r2","expires_in":36000}`)
			default:
				w.WriteHeader(400)
			}
		default:
			if bearer(r) != "a2" {
				w.WriteHeader(401)
				return
			}
			q := r.URL.Query()
			switch r.URL.Path {
			case "/my/playlists/":
				io.WriteString(w, `{"next":null,"results":[{"id":11,"name":"Peak","track_count":2}]}`)
			case "/catalog/charts/":
				if q.Get("order_by") != "-publish_date" || q.Get("per_page") != "30" {
					t.Errorf("charts query %v", q)
				}
				io.WriteString(w, `{"next":"more","results":[{"id":7,"name":"Top 10","track_count":10}]}`)
			case "/my/playlists/11/tracks/":
				if q.Get("page") == "1" {
					io.WriteString(w, `{"next":"p2","results":[{"id":1,"position":1,"track":{"id":501,"name":"Losing It","mix_name":"Original Mix","bpm":125,"isrc":"US123","length_ms":379000,"artists":[{"name":"Fisher"}],"key":{"camelot_number":8,"camelot_letter":"A"},"release":{"name":"Losing It","image":{"uri":"https://img/full.jpg","dynamic_uri":"https://img/{w}x{h}/x.jpg"}}}}]}`)
					return
				}
				io.WriteString(w, `{"next":null,"results":[{"id":2,"track":{"id":502,"name":"Track (Extended Mix)","mix_name":"Extended Mix","artists":[{"name":"A"},{"name":"B"}],"release":{"image":{"uri":"https://img/only.jpg"}}}}]}`)
			case "/my/playlists/99/tracks/":
				w.WriteHeader(404)
			case "/catalog/playlists/99/tracks/", "/catalog/charts/7/tracks/":
				io.WriteString(w, `{"next":null,"results":[{"id":503,"name":"Editor Pick"}]}`)
			default:
				t.Errorf("unexpected %s", r.URL)
				w.WriteHeader(404)
			}
		}
	}))
	defer srv.Close()
	path := filepath.Join(t.TempDir(), "beatport-owner.json")
	b := &Beatport{BaseURL: srv.URL, Path: path}
	ctx := context.Background()
	if err := b.Login(ctx, "dj", "pw"); err != nil {
		t.Fatal(err)
	}
	if st, err := os.Stat(path); err != nil || st.Mode().Perm() != 0o600 {
		t.Fatalf("credential file: %v %v", st, err)
	}
	lists, err := b.Playlists(ctx)
	if err != nil || len(lists) != 2 || lists[0] != (Playlist{"11", "Peak", 2}) || lists[1] != (Playlist{"chart:7", "【榜单】Top 10", 10}) {
		t.Fatalf("playlists %+v %v", lists, err)
	}
	if refreshes != 1 {
		t.Fatalf("refreshes = %d", refreshes)
	}
	saved, _ := os.ReadFile(path)
	if !strings.Contains(string(saved), `"a2"`) || !strings.Contains(string(saved), `"r2"`) {
		t.Fatalf("renewed login not saved: %s", saved)
	}
	// Another instance reads the saved login without logging in again.
	tracks, err := (&Beatport{BaseURL: srv.URL, Path: path}).Tracks(ctx, "11")
	if err != nil || len(tracks) != 2 {
		t.Fatalf("tracks %+v %v", tracks, err)
	}
	want := Track{ID: "501", Title: "Losing It (Original Mix)", Artist: "Fisher", Artists: []string{"Fisher"}, Album: "Losing It", ISRC: "US123", DurationMS: 379000, Provider: "beatport", BPM: 125, Key: "8A", CoverURL: "https://img/500x500/x.jpg", ThumbURL: "https://img/120x120/x.jpg"}
	got := tracks[0]
	if got.Title != want.Title || got.Artist != want.Artist || got.Album != want.Album || got.ISRC != want.ISRC || got.DurationMS != want.DurationMS || got.Provider != want.Provider || got.BPM != want.BPM || got.Key != want.Key || got.CoverURL != want.CoverURL || got.ThumbURL != want.ThumbURL {
		t.Fatalf("track\n got %+v\nwant %+v", got, want)
	}
	if tracks[1].Title != "Track (Extended Mix)" || tracks[1].Artist != "A, B" || tracks[1].CoverURL != "https://img/only.jpg" || tracks[1].Key != "" {
		t.Fatalf("second track %+v", tracks[1])
	}
	for _, id := range []string{"99", "chart:7"} {
		if tracks, err = b.Tracks(ctx, id); err != nil || len(tracks) != 1 || tracks[0].ID != "503" {
			t.Fatalf("%s: %+v %v", id, tracks, err)
		}
	}
	if _, err = b.Tracks(ctx, "../x"); err == nil {
		t.Fatal("invalid ID accepted")
	}
}

// A token the API refuses is renewed once, and the call repeated.
func TestBeatportRenewsRefusedToken(t *testing.T) {
	refreshes := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/auth/o/token/":
			r.ParseForm()
			if r.Form.Get("refresh_token") != "r-old" {
				w.WriteHeader(400)
				return
			}
			refreshes++
			io.WriteString(w, `{"access_token":"a-new","expires_in":36000}`)
		case "/catalog/search/":
			if bearer(r) != "a-new" {
				w.WriteHeader(401)
				return
			}
			if r.URL.Query().Get("q") != "fisher" || r.URL.Query().Get("is_available_for_streaming") != "true" {
				t.Errorf("search query %v", r.URL.Query())
			}
			io.WriteString(w, `{"tracks":[{"id":501,"name":"Losing It"}],"releases":[]}`)
		}
	}))
	defer srv.Close()
	path := filepath.Join(t.TempDir(), "beatport-owner.json")
	b := &Beatport{BaseURL: srv.URL, Path: path}
	if err := b.SetToken(BeatportToken{Access: "a-old", Refresh: "r-old"}); err != nil {
		t.Fatal(err)
	}
	found, err := b.Search(context.Background(), "fisher")
	if err != nil || len(found) != 1 || found[0].ID != "501" || refreshes != 1 {
		t.Fatalf("%+v %v refreshes=%d", found, err, refreshes)
	}
	// The refresh token is kept when the reply has none.
	if tok := b.Token(); tok.Access != "a-new" || tok.Refresh != "r-old" || tok.Expiry.IsZero() {
		t.Fatalf("token %+v", tok)
	}
}

// Each subscription gets the best file it allows; lossless starts at FLAC.
func TestBeatportDownloadLadder(t *testing.T) {
	var asked []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query().Get("quality")
		asked = append(asked, q)
		if r.URL.Path != "/catalog/tracks/42/download/" || q == "lossless" {
			w.WriteHeader(403)
			return
		}
		io.WriteString(w, `{"location":"https://cdn/x.`+map[string]string{"high": "256k", "medium": "128k"}[q]+`.aac.mp4","stream_quality":".`+map[string]string{"high": "256k", "medium": "128k"}[q]+`.aac.mp4"}`)
	}))
	defer srv.Close()
	b := &Beatport{BaseURL: srv.URL}
	b.SetToken(BeatportToken{Access: "a"})
	location, ext, err := b.Download(context.Background(), "42", true)
	if err != nil || ext != "mp4" || location != "https://cdn/x.256k.aac.mp4" || strings.Join(asked, ",") != "lossless,high" {
		t.Fatalf("%q %q %v asked=%v", location, ext, err, asked)
	}
	asked = nil
	if _, _, err = b.Download(context.Background(), "42", false); err != nil || strings.Join(asked, ",") != "high" {
		t.Fatalf("320k: %v asked=%v", err, asked)
	}
}

func TestParseBeatportToken(t *testing.T) {
	tok, err := ParseBeatportToken(` {"access_token":"a","refresh_token":"r","expires_in":3600} `)
	if err != nil || tok.Access != "a" || tok.Refresh != "r" || time.Until(tok.Expiry) < 59*time.Minute {
		t.Fatalf("%+v %v", tok, err)
	}
	if tok, err = ParseBeatportToken("Bearer abc.def"); err != nil || tok.Access != "abc.def" || !tok.Expiry.IsZero() {
		t.Fatalf("%+v %v", tok, err)
	}
	for _, bad := range []string{"", "two words", `{"refresh_token":"r"}`} {
		if _, err = ParseBeatportToken(bad); err == nil {
			t.Fatalf("%q accepted", bad)
		}
	}
}

// adtsFrame is one AAC LC frame, 44.1 kHz stereo, with a recognisable payload.
func adtsFrame(i int) []byte {
	payload := bytes.Repeat([]byte{byte(i + 1)}, 20+i)
	n := 7 + len(payload)
	return append([]byte{0xff, 0xf1, 1<<6 | 4<<2, 2<<6 | byte(n>>11), byte(n >> 3), byte(n&7)<<5 | 0x1f, 0xfc}, payload...)
}

// tsPackets carries one PES packet on pid, stuffing the last packet.
func tsPackets(pid int, pes []byte) []byte {
	var out []byte
	for first := true; len(pes) > 0; first = false {
		p := make([]byte, 188)
		p[0], p[1], p[2], p[3] = 0x47, byte(pid>>8), byte(pid), 0x10
		if first {
			p[1] |= 0x40
		}
		n := min(184, len(pes))
		if n < 184 {
			p[3] = 0x30
			af := 183 - n
			p[4] = byte(af)
			for i := 5; i < 5+af; i++ {
				p[i] = 0xff
			}
			copy(p[5+af:], pes[:n])
		} else {
			copy(p[4:], pes[:n])
		}
		pes = pes[n:]
		out = append(out, p...)
	}
	return out
}

func encryptSegment(data, key, iv []byte) []byte {
	pad := aes.BlockSize - len(data)%aes.BlockSize
	data = append(bytes.Clone(data), bytes.Repeat([]byte{byte(pad)}, pad)...)
	block, _ := aes.NewCipher(key)
	cipher.NewCBCEncrypter(block, iv).CryptBlocks(data, data)
	return data
}

// Essential's stream: AES-128 segments of MPEG-TS and packed ADTS (one frame split
// across two segments, IVs from the playlist and from sequence numbers) become one
// MP4 file holding exactly the AAC frames.
func TestBeatportStreamBecomesMP4(t *testing.T) {
	var frames, raw []byte
	for i := 0; i < 10; i++ {
		f := adtsFrame(i)
		frames = append(frames, f...)
		raw = append(raw, f[7:]...)
	}
	cut := 0
	for i := 0; i < 5; i++ {
		cut += len(adtsFrame(i))
	}
	pat := tsPackets(0, []byte{0, 0, 0xb0, 0x0d, 0, 1})
	pes := append([]byte{0, 0, 1, 0xc0, 0, 0, 0x80, 0x80, 5, 0x21, 0, 1, 0, 1}, frames[:cut]...)
	seg1 := append(pat, tsPackets(0x101, pes)...)
	id3 := append([]byte("ID3\x04\x00\x00\x00\x00\x00\x05"), "PRIVx"...)
	split := len(frames) - 30
	seg2 := append(id3, frames[cut:split]...)
	seg3 := frames[split:]

	key := []byte("0123456789abcdef")
	iv1 := bytes.Repeat([]byte{0xab}, 16)
	seqIV := func(n uint64) []byte {
		iv := make([]byte, 16)
		binary.BigEndian.PutUint64(iv[8:], n)
		return iv
	}
	files := map[string][]byte{
		"/hls/key.bin": key,
		"/hls/s1.ts":   encryptSegment(seg1, key, iv1),
		"/hls/s2.aac":  encryptSegment(seg2, key, seqIV(6)),
		"/hls/s3.aac":  encryptSegment(seg3, key, seqIV(7)),
	}
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/catalog/tracks/42/stream/":
			io.WriteString(w, `{"stream_url":"`+srv.URL+`/hls/index.m3u8","sample_start_ms":0}`)
		case "/hls/index.m3u8":
			io.WriteString(w, "#EXTM3U\n#EXT-X-MEDIA-SEQUENCE:5\n#EXT-X-KEY:METHOD=AES-128,URI=\"key.bin\",IV=0xabababababababababababababababab\n#EXTINF:10,\ns1.ts\n#EXT-X-KEY:METHOD=AES-128,URI=\"key.bin\"\n#EXTINF:10,\ns2.aac\n#EXTINF:1,\ns3.aac\n#EXT-X-ENDLIST\n")
		default:
			if b, ok := files[r.URL.Path]; ok {
				w.Write(b)
				return
			}
			w.WriteHeader(404)
		}
	}))
	defer srv.Close()
	b := &Beatport{BaseURL: srv.URL}
	b.SetToken(BeatportToken{Access: "a"})
	f, err := os.Create(filepath.Join(t.TempDir(), "out.mp4"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err = b.SaveStream(context.Background(), "42", f); err != nil {
		t.Fatal(err)
	}
	out, _ := os.ReadFile(f.Name())
	if string(out[4:8]) != "ftyp" || string(out[32:36]) != "mdat" {
		t.Fatalf("layout %q", out[:40])
	}
	mdat := int(binary.BigEndian.Uint32(out[28:]))
	if !bytes.Equal(out[36:28+mdat], raw) || string(out[28+mdat+4:28+mdat+8]) != "moov" {
		t.Fatal("mdat does not hold exactly the AAC frames")
	}
	stsz := bytes.Index(out, []byte("stsz"))
	if stsz < 0 || binary.BigEndian.Uint32(out[stsz+12:]) != 10 {
		t.Fatal("stsz does not count 10 frames")
	}
	if !bytes.Contains(out, []byte{0x05, 0x02, 0x12, 0x10}) { // AAC LC, 44.1 kHz, stereo
		t.Fatal("missing AudioSpecificConfig")
	}
}
