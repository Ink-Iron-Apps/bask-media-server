// Package artwork keeps posters and backdrops on disk.
//
// Images are files here, never objects. One is downloaded once, resized on
// request, and served from disk; a decoded image is never held past the
// request that needed it, and the cache has a stated size cap rather than
// growing until somebody notices. Both rules come straight from
// docs/RESOURCE-BUDGET.md, because a poster wall is the one place a media
// server is tempted to hold a hundred bitmaps in memory.
package artwork

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"image"
	"image/jpeg"
	_ "image/png" // posters are occasionally PNG
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"time"

	"golang.org/x/image/draw"
)

// ErrNotCached means the image has never been fetched.
var ErrNotCached = errors.New("not cached")

// ErrNotAnImage means what arrived was not a picture this cache can render.
//
// Its own error because it is the one failure a person can do something
// about: a link that leads to a login page or an error page answers with a
// perfectly good 200 and a body full of HTML.
var ErrNotAnImage = errors.New("not a picture this server can read")

// UserAgent is how this program names itself when fetching a picture.
//
// A name and a purpose and nothing else. It never carries anything about
// whose library this is, and it exists at all because an unnamed client is
// refused by a good part of the web.
const UserAgent = "BaskMediaServer/1.0 (+artwork)"

// sniffBytes is how much of a file is needed to tell what it is. The longest
// signature checked below is eight bytes; sixteen leaves room without
// holding a buffer worth thinking about.
const sniffBytes = 16

// IsImage reports whether these first bytes begin a picture this cache can
// decode.
//
// JPEG and PNG, which are what the decoders registered in this package read.
// Deliberately not WebP: nothing here can decode one, and accepting a file
// that stores happily and then renders as nothing would be a poster that
// looks saved and is not.
//
// By the bytes rather than by a Content-Type header or a file extension,
// because both of those are what somebody else said and neither is what the
// resizer will be handed.
func IsImage(data []byte) bool {
	switch {
	case len(data) >= 3 && data[0] == 0xFF && data[1] == 0xD8 && data[2] == 0xFF:
		return true // JPEG
	case len(data) >= 8 && string(data[:8]) == "\x89PNG\r\n\x1a\n":
		return true // PNG
	}
	return false
}

// maxImageBytes caps what will be stored as one image, whether it came over
// the network or off this machine. A poster is tens of kilobytes and a scan
// of an album cover is a few megabytes; past this it is not a cover.
const maxImageBytes = 32 << 20

// Cache stores original images and their resized renditions under one root.
type Cache struct {
	Root string

	// MaxBytes is the cap across originals and renditions together. Eviction
	// is oldest-accessed-first once a store exceeds it, which is the whole
	// reason this is a cache rather than a directory that fills a disk.
	MaxBytes int64

	// MaxPixels refuses an image larger than this once decoded, which is what
	// stops a 20,000-pixel-wide file from becoming a gigabyte of memory during
	// a resize.
	MaxPixels int

	client *http.Client
	resize chan struct{}
}

// New builds a cache. concurrency bounds simultaneous decodes, which is the
// memory-hungry half of serving artwork: each one holds a decoded bitmap.
func New(root string, maxBytes int64, concurrency int) (*Cache, error) {
	if root == "" {
		return nil, errors.New("artwork cache needs a root")
	}
	if maxBytes <= 0 {
		maxBytes = 512 << 20 // half a gigabyte of posters is a very large library
	}
	if concurrency <= 0 {
		concurrency = 2
	}
	if err := os.MkdirAll(filepath.Join(root, "originals"), 0o755); err != nil {
		return nil, fmt.Errorf("artwork cache: %w", err)
	}
	if err := os.MkdirAll(filepath.Join(root, "renditions"), 0o755); err != nil {
		return nil, fmt.Errorf("artwork cache: %w", err)
	}
	return &Cache{
		Root:      root,
		MaxBytes:  maxBytes,
		MaxPixels: 40_000_000,
		client: &http.Client{
			Timeout: 30 * time.Second,
			Transport: &http.Transport{
				MaxIdleConns:        4,
				MaxIdleConnsPerHost: 4,
				IdleConnTimeout:     30 * time.Second,
			},
		},
		resize: make(chan struct{}, concurrency),
	}, nil
}

// Key is the stable name an image is stored under.
func Key(url string) string {
	sum := sha256.Sum256([]byte(url))
	return hex.EncodeToString(sum[:16])
}

// FileKey is the name a file on disk is cached under, including its size and
// mtime so that replacing a cover with a different image is a different key
// rather than a stale thumbnail that never updates.
func FileKey(path string, size int64, modified time.Time) string {
	return Key(fmt.Sprintf("file:%s|%d|%d", path, size, modified.UnixMilli()))
}

func (c *Cache) originalPath(key string) string {
	return filepath.Join(c.Root, "originals", key+".img")
}

func (c *Cache) renditionPath(key string, width, height int) string {
	return filepath.Join(c.Root, "renditions", fmt.Sprintf("%s_%dx%d.jpg", key, width, height))
}

// Fetch downloads an image unless it is already stored, and returns its key.
//
// Streamed straight to disk: an image is never assembled in memory, and the
// copy is bounded so a mislabelled URL cannot write a film into the cache.
func (c *Cache) Fetch(ctx context.Context, url string) (string, error) {
	if url == "" {
		return "", errors.New("no image url")
	}
	key := Key(url)
	path := c.originalPath(key)
	if _, err := os.Stat(path); err == nil {
		return key, nil
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", fmt.Errorf("artwork request: %w", err)
	}
	// Named and asking for a picture. Go's default user agent is refused
	// outright by a good deal of the web — anything behind a general-purpose
	// bot filter — and a poster site is exactly the sort of place that does
	// it. The name is this program and its purpose and nothing about whose
	// library it is.
	req.Header.Set("User-Agent", UserAgent)
	req.Header.Set("Accept", "image/avif,image/webp,image/jpeg,image/png,image/*;q=0.8,*/*;q=0.5")
	resp, err := c.client.Do(req)
	if err != nil {
		return "", fmt.Errorf("fetch artwork: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("fetch artwork: status %d", resp.StatusCode)
	}

	// Written to a temporary name and renamed, so an interrupted download
	// cannot leave a truncated image that looks cached forever.
	tmp, err := os.CreateTemp(filepath.Dir(path), "part-")
	if err != nil {
		return "", fmt.Errorf("artwork cache: %w", err)
	}
	defer os.Remove(tmp.Name())

	// Sniffed before anything is written, because a 200 is not a promise of
	// a picture: a site that wants a login, or a link that has gone to an
	// error page, answers with HTML and a cheerful status. Storing that
	// would be a poster that is a file on disk and a blank square forever.
	lead := make([]byte, sniffBytes)
	read, err := io.ReadFull(io.LimitReader(resp.Body, sniffBytes), lead)
	if err != nil && !errors.Is(err, io.ErrUnexpectedEOF) && !errors.Is(err, io.EOF) {
		tmp.Close()
		return "", fmt.Errorf("fetch artwork: %w", err)
	}
	lead = lead[:read]
	if !IsImage(lead) {
		tmp.Close()
		return "", ErrNotAnImage
	}

	body := io.MultiReader(bytes.NewReader(lead), resp.Body)
	if _, err := io.Copy(tmp, io.LimitReader(body, maxImageBytes)); err != nil {
		tmp.Close()
		return "", fmt.Errorf("store artwork: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return "", fmt.Errorf("store artwork: %w", err)
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		return "", fmt.Errorf("store artwork: %w", err)
	}
	return key, nil
}

// Store keeps an image that came out of another file — a cover embedded in a
// music file — and returns its key.
//
// Keyed by the bytes themselves, so an album whose every track carries the
// same cover stores it once however many tracks there are.
//
// Deliberately not checked against [IsImage]. These bytes came out of a file
// already in the library, and a tagger that wrote a format this program
// cannot decode should cost that album its cover and nothing else — the
// scan carries on and the square is blank, which is what it would be
// anyway. A caller taking bytes from somewhere less trustworthy checks them
// first: see the admin interface's upload, and [Cache.Fetch], which is
// reading from a URL somebody typed.
func (c *Cache) Store(data []byte) (string, error) {
	if len(data) == 0 {
		return "", errors.New("no image data")
	}
	sum := sha256.Sum256(data)
	key := hex.EncodeToString(sum[:16])
	path := c.originalPath(key)
	if _, err := os.Stat(path); err == nil {
		return key, nil
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), "part-")
	if err != nil {
		return "", fmt.Errorf("artwork cache: %w", err)
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return "", fmt.Errorf("store artwork: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return "", fmt.Errorf("store artwork: %w", err)
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		return "", fmt.Errorf("store artwork: %w", err)
	}
	return key, nil
}

// Adopt copies an image that is already on this machine into the cache — a
// folder.jpg beside an album — and returns its key.
//
// Copied rather than referenced because a cover is small, the folder it is in
// may be renamed or unplugged, and everything downstream of a key expects the
// cache to still have the image. Photographs are the opposite case and use
// [Cache.OpenFrom], which never copies.
func (c *Cache) Adopt(path string) (string, error) {
	stat, err := os.Stat(path)
	if err != nil {
		return "", err
	}
	if stat.Size() > maxImageBytes {
		return "", fmt.Errorf("adopt artwork: %s is %d bytes", path, stat.Size())
	}
	key := FileKey(path, stat.Size(), stat.ModTime())
	stored := c.originalPath(key)
	if _, err := os.Stat(stored); err == nil {
		return key, nil
	}

	in, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer in.Close()

	tmp, err := os.CreateTemp(filepath.Dir(stored), "part-")
	if err != nil {
		return "", fmt.Errorf("artwork cache: %w", err)
	}
	defer os.Remove(tmp.Name())
	if _, err := io.Copy(tmp, io.LimitReader(in, maxImageBytes)); err != nil {
		tmp.Close()
		return "", fmt.Errorf("store artwork: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return "", fmt.Errorf("store artwork: %w", err)
	}
	if err := os.Rename(tmp.Name(), stored); err != nil {
		return "", fmt.Errorf("store artwork: %w", err)
	}
	return key, nil
}

// OpenFrom returns a rendition of a file that stays where it is.
//
// This is how a photo library is served. A copy of every photograph would
// double the space the library takes, so the original is read from the
// library and only the thumbnail is cached — which is also what makes the
// cache's own cap meaningful, since the thumbnails are the part this server
// created.
//
// orientation is the EXIF value, applied here, because a phone's photographs
// are stored sideways and a thumbnail that ignores that is a sideways
// thumbnail.
func (c *Cache) OpenFrom(ctx context.Context, source string, width, height, orientation int) (*os.File, error) {
	stat, err := os.Stat(source)
	if err != nil {
		return nil, err
	}
	if width <= 0 && height <= 0 && orientation <= 1 {
		// Nothing to do to it: the file itself is the answer.
		return os.Open(source)
	}

	key := FileKey(source, stat.Size(), stat.ModTime())
	if orientation > 1 {
		key = fmt.Sprintf("%s-o%d", key, orientation)
	}
	rendition := c.renditionPath(key, width, height)
	if f, err := os.Open(rendition); err == nil {
		_ = os.Chtimes(rendition, time.Now(), time.Now())
		return f, nil
	}

	select {
	case c.resize <- struct{}{}:
		defer func() { <-c.resize }()
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	if f, err := os.Open(rendition); err == nil {
		return f, nil
	}
	if err := c.render(source, rendition, width, height, orientation); err != nil {
		return nil, err
	}
	return os.Open(rendition)
}

// Open returns a file holding the image at the requested size, generating the
// rendition once if it does not exist.
//
// A width or height of zero means "as stored", which is how a client asking
// for the original is served without a decode at all — the common case for a
// phone that scales in its own layer.
func (c *Cache) Open(ctx context.Context, key string, width, height int) (*os.File, error) {
	original := c.originalPath(key)
	if _, err := os.Stat(original); err != nil {
		return nil, ErrNotCached
	}
	if width <= 0 && height <= 0 {
		return os.Open(original)
	}

	rendition := c.renditionPath(key, width, height)
	if f, err := os.Open(rendition); err == nil {
		// Touch so eviction sees it as recently used. Failure is harmless.
		_ = os.Chtimes(rendition, time.Now(), time.Now())
		return f, nil
	}

	select {
	case c.resize <- struct{}{}:
		defer func() { <-c.resize }()
	case <-ctx.Done():
		return nil, ctx.Err()
	}

	// Another request may have generated it while this one waited for the
	// semaphore, which on a poster wall is the normal case rather than a race
	// worth a lock.
	if f, err := os.Open(rendition); err == nil {
		return f, nil
	}
	if err := c.render(original, rendition, width, height, 0); err != nil {
		return nil, err
	}
	return os.Open(rendition)
}

func (c *Cache) render(originalPath, renditionPath string, width, height, orientation int) error {
	in, err := os.Open(originalPath)
	if err != nil {
		return err
	}
	defer in.Close()

	config, _, err := image.DecodeConfig(in)
	if err != nil {
		return fmt.Errorf("decode artwork header: %w", err)
	}
	if config.Width*config.Height > c.MaxPixels {
		return fmt.Errorf("artwork is %dx%d, past the decode limit", config.Width, config.Height)
	}
	if _, err := in.Seek(0, io.SeekStart); err != nil {
		return err
	}

	src, _, err := image.Decode(in)
	if err != nil {
		return fmt.Errorf("decode artwork: %w", err)
	}

	bounds := src.Bounds()
	// The size asked for is the size as seen, so a quarter-turn is taken off
	// the picture's own dimensions before working out what to scale to.
	seenW, seenH := bounds.Dx(), bounds.Dy()
	if quarterTurn(orientation) {
		seenW, seenH = seenH, seenW
	}
	targetW, targetH := fit(seenW, seenH, width, height)
	scaledW, scaledH := targetW, targetH
	if quarterTurn(orientation) {
		scaledW, scaledH = targetH, targetW
	}

	dst := image.NewRGBA(image.Rect(0, 0, scaledW, scaledH))
	// CatmullRom costs more than bilinear and is the difference between a
	// poster that looks resized and one that looks right. It runs at most
	// `concurrency` times at once, which is what keeps that affordable.
	draw.CatmullRom.Scale(dst, dst.Bounds(), src, bounds, draw.Over, nil)
	// Turned after scaling rather than before: the transform allocates an
	// image of its own, and doing it to the thumbnail costs kilobytes where
	// doing it to a twelve-megapixel original costs fifty megabytes.
	dst = orient(dst, orientation)

	tmp, err := os.CreateTemp(filepath.Dir(renditionPath), "part-")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if err := jpeg.Encode(tmp, dst, &jpeg.Options{Quality: 85}); err != nil {
		tmp.Close()
		return fmt.Errorf("encode artwork: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), renditionPath)
}

// quarterTurn reports whether an orientation turns the picture on its side,
// which is what swaps its width and its height.
func quarterTurn(orientation int) bool {
	return orientation >= 5 && orientation <= 8
}

// orient applies an EXIF orientation, which is the file saying "the pixels
// are stored this way and meant to be seen that way".
//
// The eight values are every combination of a quarter turn and a mirror. All
// eight are handled because all eight are written by something: 6 by almost
// every phone held upright, and the mirrored ones by front cameras.
func orient(src *image.RGBA, orientation int) *image.RGBA {
	if orientation <= 1 || orientation > 8 {
		return src
	}
	bounds := src.Bounds()
	w, h := bounds.Dx(), bounds.Dy()
	outW, outH := w, h
	if quarterTurn(orientation) {
		outW, outH = h, w
	}
	dst := image.NewRGBA(image.Rect(0, 0, outW, outH))

	for y := range h {
		for x := range w {
			var nx, ny int
			switch orientation {
			case 2: // mirrored
				nx, ny = w-1-x, y
			case 3: // half turn
				nx, ny = w-1-x, h-1-y
			case 4: // mirrored, half turn
				nx, ny = x, h-1-y
			case 5: // mirrored, quarter turn
				nx, ny = y, x
			case 6: // quarter turn clockwise
				nx, ny = h-1-y, x
			case 7: // mirrored, three quarter turn
				nx, ny = h-1-y, w-1-x
			case 8: // three quarter turn clockwise
				nx, ny = y, w-1-x
			}
			dst.SetRGBA(nx, ny, src.RGBAAt(bounds.Min.X+x, bounds.Min.Y+y))
		}
	}
	return dst
}

// fit scales an image to the requested box, keeping aspect when only one
// dimension is given and never enlarging past the original.
func fit(srcW, srcH, wantW, wantH int) (int, int) {
	if srcW <= 0 || srcH <= 0 {
		return 1, 1
	}
	switch {
	case wantW > 0 && wantH > 0:
		// Fit inside the box rather than stretching to it.
		scale := min(float64(wantW)/float64(srcW), float64(wantH)/float64(srcH))
		if scale > 1 {
			scale = 1
		}
		return max(1, int(float64(srcW)*scale)), max(1, int(float64(srcH)*scale))
	case wantW > 0:
		if wantW > srcW {
			wantW = srcW
		}
		return wantW, max(1, srcH*wantW/srcW)
	default:
		if wantH > srcH {
			wantH = srcH
		}
		return max(1, srcW*wantH/srcH), wantH
	}
}

// Sweep evicts files until the store is under its cap, renditions before
// originals and least-recently-used first within each.
//
// Called after an enrichment pass rather than on every request: eviction
// wants a whole-directory view, and doing that per request would be the
// expensive kind of tidy.
//
// The order is the point, and it is not a tidiness preference. A rendition
// costs a decode to make again and nothing else, so losing one is invisible.
// An original is the only copy: an item stores a cache key and not the
// address the picture came from, so an evicted original is a poster that
// cannot be recovered without somebody choosing it again. That matters most
// for exactly the pictures somebody chose by hand — those items are locked,
// so no later pass will refetch them, and a flat least-recently-used sweep
// would quietly delete the one poster in the library that was a decision.
//
// Originals are still evicted if clearing every rendition is not enough,
// because a cache that refuses to honour its own cap is not a cache. By then
// the cap is smaller than the library's posters and the honest fix is a
// larger cap.
func (c *Cache) Sweep() (freed int64, err error) {
	type entry struct {
		path string
		size int64
		used time.Time
	}
	var (
		renditions []entry
		originals  []entry
		total      int64
	)
	for _, dir := range []string{"originals", "renditions"} {
		root := filepath.Join(c.Root, dir)
		into := &originals
		if dir == "renditions" {
			into = &renditions
		}
		walkErr := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
			if err != nil || d.IsDir() {
				return nil
			}
			info, err := d.Info()
			if err != nil {
				return nil
			}
			*into = append(*into, entry{path: path, size: info.Size(), used: info.ModTime()})
			total += info.Size()
			return nil
		})
		if walkErr != nil {
			return 0, walkErr
		}
	}
	if total <= c.MaxBytes {
		return 0, nil
	}

	for _, group := range [][]entry{renditions, originals} {
		sort.Slice(group, func(i, j int) bool { return group[i].used.Before(group[j].used) })
		for _, f := range group {
			if total-freed <= c.MaxBytes {
				return freed, nil
			}
			if err := os.Remove(f.path); err != nil {
				continue
			}
			freed += f.size
		}
	}
	return freed, nil
}

// Usage reports what the cache is holding, for a status page.
//
// A walk rather than a counter: the counter would have to be kept correct
// across eviction, a restart and a file someone deleted by hand, and the walk
// costs a stat per cached image on a page nobody opens in a loop.
func (c *Cache) Usage() (bytes int64, files int, err error) {
	for _, dir := range []string{"originals", "renditions"} {
		root := filepath.Join(c.Root, dir)
		walkErr := filepath.WalkDir(root, func(_ string, d os.DirEntry, err error) error {
			if err != nil || d.IsDir() {
				return nil
			}
			info, err := d.Info()
			if err != nil {
				return nil
			}
			bytes += info.Size()
			files++
			return nil
		})
		if walkErr != nil {
			return bytes, files, walkErr
		}
	}
	return bytes, files, nil
}
