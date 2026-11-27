package providers

import (
	"context"
	"errors"
	"net/http"
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
//   - A request whose context is marked (WithCredential), or every request when
//     always is set, is followed only within the origin of its first URL: the same
//     scheme, host and effective port. Go's own rule would follow it anywhere and keep
//     an Authorization header on a subdomain or another port, and any other header
//     (Plex's X-Plex-Token) everywhere.
//   - At most 10 redirects, as Go's default.
func CheckRedirect(always bool, next func(*http.Request, []*http.Request) error) func(*http.Request, []*http.Request) error {
	return func(req *http.Request, via []*http.Request) error {
		req.Header.Del("Referer")
		if len(via) >= 10 {
			return errors.New("stopped after 10 redirects")
		}
		if (always || HasCredential(req.Context())) && !sameOrigin(req.URL, via[0].URL) {
			return ErrRedirectOffOrigin
		}
		if next != nil {
			return next(req, via)
		}
		return nil
	}
}

// LockRedirects is a copy of c (nil: a client with the providers' default timeout)
// whose every request follows redirects only within its own origin, with no Referer
// (CheckRedirect with always set, in front of c's own policy). c is not changed.
func LockRedirects(c *http.Client) *http.Client {
	lc := *client(c)
	lc.CheckRedirect = CheckRedirect(true, lc.CheckRedirect)
	return &lc
}
