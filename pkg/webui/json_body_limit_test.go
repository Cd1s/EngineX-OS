package webui

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// countingBody counts what a handler reads from a request body.
type countingBody struct {
	r io.Reader
	n int
}

func (c *countingBody) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += n
	return n, err
}

// BH2-A5-14: three handlers decoded a JSON body of any size. Each gets 2 MiB of one
// unterminated string, which no decoder can accept: the answer is the same as before,
// but only the first 64 KiB may be read.
func TestSmallJSONBodiesAreBounded(t *testing.T) {
	s, _ := newTestServer(t)
	cases := []struct {
		name, method, target string
		handler              func(http.ResponseWriter, *http.Request)
		want                 int
	}{
		{"hot cue", "POST", "/api/hotcues", s.handleHotCuesPost, 400},
		{"mount delete", "DELETE", "/api/mounts", s.handleMountsDelete, 400},
		{"settings", "POST", "/api/ui?lang=not-a-language", s.uiUpdate, 400},
	}
	for _, c := range cases {
		body := &countingBody{r: strings.NewReader(`{"junk":"` + strings.Repeat("a", 2<<20))}
		req := httptest.NewRequest(c.method, c.target, body)
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		c.handler(rec, req)
		if rec.Code != c.want {
			t.Errorf("%s: status %d, want %d (%s)", c.name, rec.Code, c.want, strings.TrimSpace(rec.Body.String()))
		}
		if body.n > 2*jsonBodyLimit {
			t.Errorf("%s: the handler read %d bytes of a 2 MiB body, limit %d", c.name, body.n, jsonBodyLimit)
		}
	}
}
