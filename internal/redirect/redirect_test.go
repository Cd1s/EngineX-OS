package redirect

import (
	"net/http"
	"testing"
)

// No redirect carries a Referer; one is followed only within the first URL's origin.
func TestSameOrigin(t *testing.T) {
	first, _ := http.NewRequest("GET", "http://radio.example:8000/admin/metadata?song=x", nil)
	for _, c := range []struct {
		to string
		ok bool
	}{
		{"http://radio.example:8000/admin/metadata2", true},
		{"http://RADIO.example:8000/other", true},
		{"http://radio.example:8001/admin/metadata", false},
		{"https://radio.example:8000/admin/metadata", false},
		{"http://sub.radio.example:8000/admin/metadata", false},
		{"http://other.example:8000/admin/metadata", false},
	} {
		next, _ := http.NewRequest("GET", c.to, nil)
		next.Header.Set("Referer", first.URL.String())
		err := SameOrigin(next, []*http.Request{first})
		if (err == nil) != c.ok {
			t.Errorf("redirect to %s: err %v, want followed=%v", c.to, err, c.ok)
		}
		if next.Header.Get("Referer") != "" {
			t.Errorf("redirect to %s kept its Referer", c.to)
		}
	}
	plain, _ := http.NewRequest("GET", "http://radio.example/a", nil)
	if err := SameOrigin(mustRequest(t, "http://radio.example:80/b"), []*http.Request{plain}); err != nil {
		t.Errorf("the default port spelled out is the same origin: %v", err)
	}
	via := make([]*http.Request, 10)
	for i := range via {
		via[i] = first
	}
	if err := SameOrigin(mustRequest(t, "http://radio.example:8000/again"), via); err == nil {
		t.Error("an 11th redirect was followed")
	}
}

func mustRequest(t *testing.T, raw string) *http.Request {
	t.Helper()
	r, err := http.NewRequest("GET", raw, nil)
	if err != nil {
		t.Fatal(err)
	}
	return r
}
