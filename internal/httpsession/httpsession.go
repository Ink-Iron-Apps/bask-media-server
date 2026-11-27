// Package httpsession is the browser session both interfaces share.
//
// There used to be two: `bms_web` scoped to /web and `bms_admin` scoped to
// /admin, each with its own sign-in. An operator who watches on the same
// server they administer therefore typed a token twice, and again a fortnight
// later, because the cookie's life was fixed at sign-in and never extended.
// One cookie at the root, renewed while it is being used, is the whole fix.
//
// Sharing the cookie does not share the authority: the admin interface still
// refuses a session whose account is not an administrator, so a household
// member's viewing session reaches exactly what it did before.
package httpsession

import (
	"net/http"
	"strings"
	"time"
)

// Name is the cookie both interfaces read and write.
const Name = "bms_session"

// legacy names the cookies this replaced, with the paths they were set at. A
// browser holding one is signed in; it is read, honoured, and upgraded in
// place rather than being asked for the token again.
var legacy = []struct{ name, path string }{
	{"bms_web", "/web"},
	{"bms_admin", "/admin"},
}

// Read returns the session secret a request carries, and whether it came from
// one of the old cookies. An empty secret means no session.
func Read(r *http.Request) (secret string, fromLegacy bool) {
	if cookie, err := r.Cookie(Name); err == nil && cookie.Value != "" {
		return cookie.Value, false
	}
	for _, old := range legacy {
		if cookie, err := r.Cookie(old.name); err == nil && cookie.Value != "" {
			return cookie.Value, true
		}
	}
	return "", false
}

// Set writes the session cookie for ttl.
//
// Path is the root because one session covers both interfaces. SameSite is
// strict and the cookie is HttpOnly, so a cross-site page cannot send it and
// no script can read it; each interface additionally requires its own header
// on anything that changes something.
func Set(w http.ResponseWriter, r *http.Request, secret string, ttl time.Duration) {
	http.SetCookie(w, &http.Cookie{
		Name:     Name,
		Value:    secret,
		Path:     "/",
		HttpOnly: true,
		Secure:   IsTLS(r),
		SameSite: http.SameSiteStrictMode,
		MaxAge:   int(ttl.Seconds()),
	})
}

// Clear expires the session cookie, and the old ones with it: a sign-out that
// left a legacy cookie behind would sign the browser straight back in.
func Clear(w http.ResponseWriter, r *http.Request) {
	expire(w, r, Name, "/")
	ClearLegacy(w, r)
}

// ClearLegacy expires the two cookies this replaced, at the paths they were
// set at. Called after a legacy cookie has been upgraded, so the browser stops
// sending both.
func ClearLegacy(w http.ResponseWriter, r *http.Request) {
	for _, old := range legacy {
		expire(w, r, old.name, old.path)
	}
}

func expire(w http.ResponseWriter, r *http.Request, name, path string) {
	http.SetCookie(w, &http.Cookie{
		Name:     name,
		Value:    "",
		Path:     path,
		HttpOnly: true,
		Secure:   IsTLS(r),
		SameSite: http.SameSiteStrictMode,
		MaxAge:   -1,
	})
}

// IsTLS reports whether the browser reached this server over HTTPS, directly
// or through a proxy that said so. The cookie is marked Secure only then: on
// the plain-HTTP tailnet names these servers are usually reached by, a Secure
// cookie would be set and never sent back.
func IsTLS(r *http.Request) bool {
	if r.TLS != nil {
		return true
	}
	return strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https")
}
