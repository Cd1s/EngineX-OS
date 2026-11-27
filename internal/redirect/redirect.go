// Package redirect is the redirect policy of an HTTP client whose requests carry a
// credential in a header (Authorization, a token): a redirect is followed only within
// the origin of the first URL (the same scheme, host and effective port) and never
// carries a Referer. Go's own policy follows a redirect to any host and keeps the
// Authorization header on a subdomain or another port of the same host (#335 round
// 11). It has no dependencies, so the small programs that send a credential (the
// Stems probe, the broadcaster) use it as well as the providers.
package redirect

import (
	"errors"
	"net/http"
	"net/url"
	"strings"
)

// ErrOffOrigin: a request that carries a credential was redirected to another origin;
// it is not followed.
var ErrOffOrigin = errors.New("redirect to another origin not followed: the request carries a credential")

// SameOrigin is that policy, for http.Client.CheckRedirect: at most 10 redirects, as
// Go's default.
func SameOrigin(req *http.Request, via []*http.Request) error {
	req.Header.Del("Referer")
	if len(via) >= 10 {
		return errors.New("stopped after 10 redirects")
	}
	if !sameOrigin(req.URL, via[0].URL) {
		return ErrOffOrigin
	}
	return nil
}

// NoReferer is the policy of a client whose requests carry no credential header but
// whose URL may be signed (a media URL a service handed out): a redirect is followed
// anywhere, as a CDN needs, and never carries a Referer, which Go would build from the
// signed URL. At most 10 redirects, as Go's default.
func NoReferer(req *http.Request, via []*http.Request) error {
	req.Header.Del("Referer")
	if len(via) >= 10 {
		return errors.New("stopped after 10 redirects")
	}
	return nil
}

// sameOrigin reports whether u is on base's origin: the same scheme and host, and the
// same port once a missing one is read as the scheme's own (80 for http, 443 for
// https). A URL without a scheme or host is on no origin.
func sameOrigin(u, base *url.URL) bool {
	if u.Scheme == "" || u.Hostname() == "" || effectivePort(u) == "" {
		return false
	}
	return strings.EqualFold(u.Scheme, base.Scheme) && strings.EqualFold(u.Hostname(), base.Hostname()) && effectivePort(u) == effectivePort(base)
}

func effectivePort(u *url.URL) string {
	if port := u.Port(); port != "" {
		return port
	}
	switch strings.ToLower(u.Scheme) {
	case "http":
		return "80"
	case "https":
		return "443"
	}
	return ""
}
