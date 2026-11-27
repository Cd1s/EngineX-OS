package providers

import (
	"context"
	"errors"
	"net/http"
	"strings"
)

// ErrRedirectOffOrigin: a request that carries a credential was redirected to another
// origin; it is not followed.
var ErrRedirectOffOrigin = errors.New("redirect to another origin not followed: the request carries a credential")

type credentialKey struct{}

// WithCredential marks ctx: a request made with it carries a credential in its URL or
// headers (a signed NAS cover, for one), so CheckRedirect keeps its redirects on the
// origin of its first URL.
func WithCredential(ctx context.Context) context.Context {
	return context.WithValue(ctx, credentialKey{}, true)
}

// HasCredential reports whether ctx is marked by WithCredential.
func HasCredential(ctx context.Context) bool {
	return ctx.Value(credentialKey{}) != nil
}

// CheckRedirect is the redirect policy of a client whose requests may carry
// credentials, in front of next (nil: none).
//   - No redirect carries a Referer: Go builds it from the previous URL, query
//     included (a signed URL's credentials), before it asks CheckRedirect.
//   - A request that carries a credential (carriesCredential: an Authorization,
//     Cookie or token header, a credential in its first URL, a context marked by
//     WithCredential), or every request when always is set, is followed only within
//     the origin of its first URL: the same scheme, host and effective port. Go's own
//     rule would follow it anywhere and keep an Authorization header on a subdomain or
//     another port, and any other header (Plex's X-Plex-Token) everywhere.
//   - A redirect that sends the request's body again (307 and 308) is followed only
//     within that origin too: a body is a login or a token request (a password, a
//     refresh token, a client secret), and nothing says which part of it is secret.
//   - At most 10 redirects, as Go's default.
func CheckRedirect(always bool, next func(*http.Request, []*http.Request) error) func(*http.Request, []*http.Request) error {
	return func(req *http.Request, via []*http.Request) error {
		req.Header.Del("Referer")
		if len(via) >= 10 {
			return errors.New("stopped after 10 redirects")
		}
		resent := req.Body != nil && req.Body != http.NoBody // Go set it: this hop repeats the body
		if (always || resent || HasCredential(req.Context()) || carriesCredential(via[0])) && !sameOrigin(req.URL, via[0].URL) {
			return ErrRedirectOffOrigin
		}
		if next != nil {
			return next(req, via)
		}
		return nil
	}
}

// carriesCredential reports whether first, a request's first hop, carries a credential:
// a context marked by WithCredential, user info in its URL, an Authorization, Cookie or
// token header, or a query parameter named as a credential (credentialName). Go copies
// such headers to a redirect's target (Authorization and Cookie within a domain, others
// anywhere); a query credential could only travel in the Referer, which is never sent.
func carriesCredential(first *http.Request) bool {
	if HasCredential(first.Context()) || first.URL.User != nil {
		return true
	}
	for name := range first.Header {
		if n := strings.ToLower(name); n == "authorization" || n == "proxy-authorization" || n == "cookie" || credentialName(n) {
			return true
		}
	}
	for name := range first.URL.Query() {
		if credentialName(strings.ToLower(name)) {
			return true
		}
	}
	return false
}

// credentialName: a lowercase header or query name that says it holds a credential
// (a token, secret, password or key: X-Plex-Token, Media-User-Token, access_token,
// api_key, X-Api-Key). A CDN's URL signature (Signature, Key-Pair-Id) is bound to its
// URL and is not one.
func credentialName(n string) bool {
	return strings.Contains(n, "token") || strings.Contains(n, "secret") || strings.Contains(n, "password") ||
		strings.HasSuffix(n, "key") || n == "auth"
}

// LockRedirects is a copy of c (nil: a client with the providers' default timeout)
// whose every request follows redirects only within its own origin, with no Referer
// (CheckRedirect with always set, in front of c's own policy). c is not changed.
func LockRedirects(c *http.Client) *http.Client {
	lc := *client(c)
	lc.CheckRedirect = CheckRedirect(true, lc.CheckRedirect)
	return &lc
}
