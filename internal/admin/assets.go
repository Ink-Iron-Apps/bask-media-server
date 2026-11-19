package admin

import (
	"bytes"
	"embed"
	"net/http"
	"strconv"
	"time"
)

// web holds the interface itself: one page, one stylesheet, one script.
//
// Compiled in rather than read from disk, which is what keeps a release a
// single file that can be copied to a machine and run. Rule 7 of the resource
// budget is the other half of this: no Node at serve time, no bundler, no
// framework — the page is small enough to be written in the browser's own
// languages and that is cheaper than the toolchain would be.
//
//go:embed web/index.html web/app.css web/app.js
var web embed.FS

// buildTime stamps the assets so a browser revalidates after an upgrade
// without being asked to fetch them again on every page view.
var buildTime = time.Now()

func (s *Server) page(w http.ResponseWriter, r *http.Request) {
	body, err := web.ReadFile("web/index.html")
	if err != nil {
		s.fail(w, "read the admin page", err)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	// The page is a shell; everything on it comes from the API, which is
	// no-store. The shell itself must not be cached across an upgrade.
	w.Header().Set("Cache-Control", "no-cache")
	// Nothing here loads anything from anywhere else, so say so: a strict
	// policy on a page that administers a server costs one header and removes
	// a whole class of injection from the picture.
	w.Header().Set("Content-Security-Policy",
		"default-src 'none'; script-src 'self'; style-src 'self'; img-src 'self' data:; "+
			"connect-src 'self'; form-action 'none'; base-uri 'none'; frame-ancestors 'none'")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	_, _ = w.Write(body)
}

func (s *Server) asset(name, contentType string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		body, err := web.ReadFile("web/" + name)
		if err != nil {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", contentType)
		w.Header().Set("X-Content-Type-Options", "nosniff")
		// ServeContent rather than a write, for conditional requests: a
		// reloaded page answers 304 and sends nothing.
		http.ServeContent(w, r, name, buildTime, bytes.NewReader(body))
	}
}
