package scan

import (
	"path/filepath"
	"strings"
)

// ImageExtensions are the picture formats a scan will pick up, mapped to what
// the format is called.
//
// Includes the two a Go program cannot decode — HEIC and AVIF, which is what
// a modern phone writes — because a library that silently omitted half a
// camera roll would be the wrong answer. They are listed, they are served as
// they are, and what cannot be made into a thumbnail says so.
var ImageExtensions = map[string]string{
	".jpg": "jpeg", ".jpeg": "jpeg", ".png": "png", ".gif": "gif",
	".webp": "webp", ".bmp": "bmp", ".tif": "tiff", ".tiff": "tiff",
	".heic": "heic", ".heif": "heic", ".avif": "avif",
}

// IsImage reports whether a path looks like a photograph.
func IsImage(path string) bool {
	_, found := ImageExtensions[strings.ToLower(filepath.Ext(path))]
	return found
}

// ImageFormat is what a path's extension says the picture is.
func ImageFormat(path string) string {
	return ImageExtensions[strings.ToLower(filepath.Ext(path))]
}

// ArtworkNames are the filenames that are a folder's cover rather than a
// photograph in their own right. Used for music, where a folder image is the
// album's cover — and deliberately not used for a photo library, where
// cover.jpg is somebody's picture of a cover.
var ArtworkNames = map[string]bool{
	"cover": true, "folder": true, "front": true, "album": true,
	"albumart": true, "albumartsmall": true, "poster": true, "thumb": true,
	"artist": true, "banner": true, "fanart": true, "backdrop": true,
	"default": true,
}

// IsArtwork reports whether a picture in a music folder is that folder's
// cover rather than a file to list.
func IsArtwork(path string) bool {
	if !IsImage(path) {
		return false
	}
	base := strings.ToLower(strings.TrimSuffix(filepath.Base(path), filepath.Ext(path)))
	if ArtworkNames[base] {
		return true
	}
	// "cover (front)", "folder-1", "AlbumArt_{GUID}_Large" — the shapes
	// Windows Media Player and the taggers write.
	for name := range ArtworkNames {
		if strings.HasPrefix(base, name+" ") || strings.HasPrefix(base, name+"-") ||
			strings.HasPrefix(base, name+"_") || strings.HasPrefix(base, name+"(") {
			return true
		}
	}
	return false
}

// Photo is where a photograph belongs: the chain of albums above it, outermost
// first, and what to call it.
type Photo struct {
	// Albums is the folder path from the library root, as names. A file
	// directly in the root gets the root folder's own name, so that every
	// photograph is in an album and a client has somewhere to show it.
	Albums []string
	Title  string
}

// ParsePhoto reads a photograph's place in the library out of its path.
//
// The folder tree is the metadata here, and all of it: people file photographs
// in folders that mean something — a year, a holiday, a person — and any
// cleverness beyond keeping that structure would be a guess over the top of
// an answer somebody already gave.
func ParsePhoto(root, path string) (Photo, bool) {
	if !IsImage(path) {
		return Photo{}, false
	}
	rel, err := filepath.Rel(root, path)
	if err != nil || strings.HasPrefix(rel, "..") {
		return Photo{}, false
	}
	parts := strings.Split(filepath.ToSlash(rel), "/")
	base := parts[len(parts)-1]

	var albums []string
	for _, dir := range parts[:len(parts)-1] {
		if name := cleanTitle(dir); name != "" {
			albums = append(albums, name)
		}
	}
	if len(albums) == 0 {
		// A loose file at the top of the library. The root's own name is the
		// one album name that is certainly meaningful to whoever chose it.
		if name := cleanTitle(filepath.Base(root)); name != "" {
			albums = []string{name}
		} else {
			albums = []string{"Photos"}
		}
	}

	title := strings.TrimSuffix(base, filepath.Ext(base))
	if title == "" {
		return Photo{}, false
	}
	return Photo{Albums: albums, Title: title}, true
}
