package web

import (
	"bytes"
	"embed"
	"net/http"
	"strconv"
	"time"
)

// web holds the viewing interface: one page, one stylesheet, one script.
//
//go:embed web/index.html web/app.css web/app.js web/vendor/hls.light.min.js
var assets embed.FS

var buildTime = time.Now()

func (s *Server) page(w http.ResponseWriter, r *http.Request) {
	body, err := assets.ReadFile("web/index.html")
	if err != nil {
		s.fail(w, "read the page", err)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	// media-src is what the admin page does not need: the player loads video
	// from this server and nowhere else.
	w.Header().Set("Content-Security-Policy",
		"default-src 'none'; script-src 'self'; style-src 'self'; img-src 'self' data:; "+
			"media-src 'self' blob:; connect-src 'self'; worker-src 'self' blob:; "+
			"form-action 'none'; base-uri 'none'; frame-ancestors 'none'")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	_, _ = w.Write(body)
}

func (s *Server) asset(name, contentType string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		body, err := assets.ReadFile("web/" + name)
		if err != nil {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", contentType)
		w.Header().Set("X-Content-Type-Options", "nosniff")
		http.ServeContent(w, r, name, buildTime, bytes.NewReader(body))
	}
}
