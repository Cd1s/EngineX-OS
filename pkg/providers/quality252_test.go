package providers_test

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"

	"github.com/Cd1s/EngineX-OS/pkg/deemixclient"
	"github.com/Cd1s/EngineX-OS/pkg/providers"
)

func TestQuality252NeteaseStrict(t *testing.T) {
	for _, tc := range []struct{ name, data, want string }{
		{"full", `{"url":"https://cdn/full","type":"flac"}`, "https://cdn/full"},
		{"lossy-response", `{"url":"https://cdn/lossy","type":"mp3"}`, ""},
		{"preview", `{"url":"https://cdn/preview","type":"flac","freeTrialInfo":{"end":30}}`, ""},
		{"unavailable", `{"url":null}`, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var levels []string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				level := r.FormValue("level")
				levels = append(levels, level)
				data := tc.data
				if level != "lossless" {
					data = `{"url":"https://cdn/lossy","type":"mp3"}`
				}
				io.WriteString(w, `{"code":200,"data":[`+data+`]}`)
			}))
			defer srv.Close()
			n := &providers.Netease{BaseURL: srv.URL}
			got, err := n.AudioURL(context.Background(), "7", true, false)
			if got != tc.want || (tc.want == "" && !errors.Is(err, providers.ErrNoFullAudio)) || (tc.want != "" && err != nil) || !reflect.DeepEqual(levels, []string{"lossless"}) {
				t.Fatalf("url=%q err=%v levels=%v; want %q and lossless only", got, err, levels, tc.want)
			}
		})
	}
}

// The same existing policy controls Deezer and NetEase: an explicit MP3
// preference allows MP3 even when AllowLossy itself is false.
func TestQuality252LossyPolicy(t *testing.T) {
	for _, tc := range []struct {
		name             string
		allow, mp3, want bool
	}{
		{"strict", false, false, false}, {"fallback", true, false, true}, {"mp3", false, true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := &deemixclient.Client{AllowLossy: tc.allow, PreferMP3: func() bool { return tc.mp3 }}
			if got := c.LossyAllowed(); got != tc.want {
				t.Fatalf("LossyAllowed=%v, want %v", got, tc.want)
			}
		})
	}
}
