package httpapi

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/scottjab/folio/internal/identity"
)

// authedHandler is a handler that runs with an authenticated user in context.
type authedHandler func(http.ResponseWriter, *http.Request)

// withIdentity resolves the tailnet peer and attaches the user to the request.
func (a *API) withIdentity(next authedHandler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u, err := a.Identity.Identify(r.Context(), a.PeerAddr(r))
		if err != nil {
			a.fail(w, r, statusFor(err), err)
			return
		}
		ctx := contextWithUser(r.Context(), u)
		next(w, r.WithContext(ctx))
	})
}

// withCSRF blocks cross-site state-changing requests.
//
// This is load-bearing, not defence in depth. folio authenticates by network
// position, so a browser sitting on the tailnet will happily attach the user's
// identity to a request initiated by any page anywhere. Without this check,
// https://evil.example.com could contain a form that POSTs to
// https://folio.your-tailnet.ts.net/api/vaults/me/notes and it would be
// accepted as you.
//
// The check is the modern, token-free one: prefer the browser's own
// Sec-Fetch-Site declaration, and fall back to comparing Origin against Host for
// clients that do not send it. Safe methods are exempt, since they change
// nothing.
func (a *API) withCSRF(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if isSafeMethod(r.Method) {
			next.ServeHTTP(w, r)
			return
		}
		if err := checkCrossOrigin(r); err != nil {
			a.fail(w, r, http.StatusForbidden, err)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// isSafeMethod reports whether the method is read-only by definition.
func isSafeMethod(m string) bool {
	switch m {
	case http.MethodGet, http.MethodHead, http.MethodOptions, http.MethodTrace:
		return true
	}
	return false
}

var errCrossSite = errors.New("cross-site request blocked")

// checkCrossOrigin implements the Sec-Fetch-Site / Origin check.
func checkCrossOrigin(r *http.Request) error {
	switch r.Header.Get("Sec-Fetch-Site") {
	case "same-origin", "none":
		// "none" is a user-initiated navigation, such as typing the URL.
		return nil
	case "same-site", "cross-site":
		return fmt.Errorf("%w: Sec-Fetch-Site is %q", errCrossSite, r.Header.Get("Sec-Fetch-Site"))
	}

	// No Sec-Fetch-Site: an older browser, or a non-browser client such as curl
	// or the MCP bridge. Fall back to Origin.
	origin := r.Header.Get("Origin")
	if origin == "" {
		// No Origin at all means no browser was tricked into sending this, so
		// there is no CSRF to speak of. A real attacker cannot suppress Origin
		// from a victim's browser.
		return nil
	}
	u, err := url.Parse(origin)
	if err != nil {
		return fmt.Errorf("%w: unparseable Origin %q", errCrossSite, origin)
	}
	if !strings.EqualFold(u.Host, r.Host) {
		return fmt.Errorf("%w: Origin %q does not match host %q", errCrossSite, u.Host, r.Host)
	}
	return nil
}

// withSecurityHeaders sets the headers that apply to every response.
func (a *API) withSecurityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		// The app is entirely self-contained: one bundled script, one bundled
		// stylesheet, no CDN. Saying so means a content-injection bug in a note
		// cannot phone home or pull in a script.
		h.Set("Content-Security-Policy", strings.Join([]string{
			"default-src 'self'",
			"script-src 'self'",
			"style-src 'self' 'unsafe-inline'",
			"img-src 'self' data: blob:",
			"font-src 'self' data:",
			"connect-src 'self'",
			// The service worker and the manifest. Both would fall back to
			// default-src anyway; naming them means a later loosening of
			// default-src cannot quietly widen what folio will install.
			"worker-src 'self'",
			"manifest-src 'self'",
			"frame-ancestors 'none'",
			"base-uri 'none'",
			"form-action 'none'",
		}, "; "))
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("Cross-Origin-Opener-Policy", "same-origin")
		h.Set("Cross-Origin-Resource-Policy", "same-origin")
		next.ServeHTTP(w, r)
	})
}

// contextWithUser attaches the authenticated user to a request context.
func contextWithUser(ctx context.Context, u identity.User) context.Context {
	return context.WithValue(ctx, userKey, u)
}
