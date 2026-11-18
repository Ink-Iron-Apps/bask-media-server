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

	const maxImageBytes = 32 << 20
	if _, err := io.Copy(tmp, io.LimitReader(resp.Body, maxImageBytes)); err != nil {
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
	if err := c.render(original, rendition, width, height); err != nil {
		return nil, err
	}
	return os.Open(rendition)
}

func (c *Cache) render(originalPath, renditionPath string, width, height int) error {
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
	targetW, targetH := fit(bounds.Dx(), bounds.Dy(), width, height)
	dst := image.NewRGBA(image.Rect(0, 0, targetW, targetH))
	// CatmullRom costs more than bilinear and is the difference between a
	// poster that looks resized and one that looks right. It runs at most
	// `concurrency` times at once, which is what keeps that affordable.
	draw.CatmullRom.Scale(dst, dst.Bounds(), src, bounds, draw.Over, nil)

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

// Sweep evicts least-recently-used files until the store is under its cap.
//
// Called after an enrichment pass rather than on every request: eviction wants
// a whole-directory view, and doing that per request would be the expensive
// kind of tidy.
func (c *Cache) Sweep() (freed int64, err error) {
	type entry struct {
		path string
		size int64
		used time.Time
	}
	var (
		files []entry
		total int64
	)
	for _, dir := range []string{"originals", "renditions"} {
		root := filepath.Join(c.Root, dir)
		walkErr := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
			if err != nil || d.IsDir() {
				return nil
			}
			info, err := d.Info()
			if err != nil {
				return nil
			}
			files = append(files, entry{path: path, size: info.Size(), used: info.ModTime()})
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

	sort.Slice(files, func(i, j int) bool { return files[i].used.Before(files[j].used) })
	for _, f := range files {
		if total-freed <= c.MaxBytes {
			break
		}
		if err := os.Remove(f.path); err != nil {
			continue
		}
		freed += f.size
	}
	return freed, nil
}
