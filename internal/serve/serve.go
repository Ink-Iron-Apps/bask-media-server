// Package serve sends files to clients.
//
// One copy of this rather than one per surface: the Plex-compatible API and
// the web interface both hand out the same media files and the same cached
// images, and range requests, conditional requests and If-Range are exactly
// the thing not to implement twice. http.ServeContent does all of it, and
// rule 1 of the resource budget — nothing reads a whole file — is kept here
// by never holding more than its copy buffer.
package serve

import (
	"net/http"
	"os"

	"github.com/inknironapps/bask-media-server/internal/artwork"
)

// File sends a media file, with seeking.
//
// The error is the caller's to turn into a status: a missing file means the
// row outlived what was on disk, which is a 404 and a note for the next scan,
// not a server fault.
func File(w http.ResponseWriter, r *http.Request, path string) error {
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()

	info, err := file.Stat()
	if err != nil {
		return err
	}
	w.Header().Set("Accept-Ranges", "bytes")
	http.ServeContent(w, r, path, info.ModTime(), file)
	return nil
}

// Image sends a cached poster or backdrop at the size asked for, rendering
// that size once if it has not been asked for before.
func Image(w http.ResponseWriter, r *http.Request, cache *artwork.Cache, key string, width, height int) error {
	file, err := cache.Open(r.Context(), key, width, height)
	if err != nil {
		return err
	}
	defer file.Close()

	info, err := file.Stat()
	if err != nil {
		return err
	}
	// Immutable by construction: a rendition's name contains the hash of the
	// source image and the size, so a client can keep it as long as it likes.
	w.Header().Set("Cache-Control", "public, max-age=2592000, immutable")
	http.ServeContent(w, r, info.Name(), info.ModTime(), file)
	return nil
}
