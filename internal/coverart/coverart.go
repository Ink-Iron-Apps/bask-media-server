// Package coverart pulls the picture out of a music file.
//
// Most libraries carry their covers twice: a folder.jpg beside the tracks and
// the same image inside every file. Some carry it only inside — anything
// ripped from a streaming service, and most of what a phone sends over — so a
// server that reads only folder images shows a wall of blank squares for
// albums that have had a cover all along.
//
// Parsed here rather than extracted with ffmpeg, for two reasons. A scan runs
// over every file in a library, and a subprocess per album is a cost worth
// avoiding; and a cover is wanted on a machine with no ffmpeg at all, where
// the alternative is no covers. The three containers below are what a
// household's music is actually in.
//
// Every read is bounded: see docs/RESOURCE-BUDGET.md. A cover is a few
// hundred kilobytes, this package refuses anything over [MaxPicture], and no
// file is read further than the metadata at the front of it.
package coverart

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// ErrNoPicture means the file carries no cover, which is the common case and
// not a fault.
var ErrNoPicture = errors.New("no picture in this file")

// MaxPicture is the largest cover this will hand back. Generous for a cover
// and small enough that a mislabelled field cannot turn into an allocation
// the size of the file.
const MaxPicture = 8 << 20

// maxMetadata is how far into a file the metadata is looked for. ID3 tags and
// FLAC metadata blocks sit at the front; an MP4's moov box can sit at the
// back, which is why that reader seeks rather than scanning.
const maxMetadata = 24 << 20

// Picture is an image found inside a media file.
type Picture struct {
	Data []byte

	// MIME is what the file says the picture is. Trusted only as a label:
	// what is actually done with the bytes is decided by decoding them.
	MIME string

	// Kind is the picture's role as the container numbers it, where it says.
	// 3 is a front cover, which is the one worth having.
	Kind int
}

// Read returns the cover inside a music file.
//
// Dispatched on the extension rather than by sniffing, because the extension
// is what the scanner already used to decide the file was music at all.
func Read(path string) (Picture, error) {
	file, err := os.Open(path)
	if err != nil {
		return Picture{}, err
	}
	defer file.Close()

	switch strings.ToLower(filepath.Ext(path)) {
	case ".mp3", ".aiff", ".aif", ".wav":
		// ID3 is routinely bolted onto all three.
		return fromID3(file)
	case ".flac":
		return fromFLAC(file)
	case ".m4a", ".m4b", ".mp4", ".alac", ".aac":
		return fromMP4(file)
	default:
		return Picture{}, ErrNoPicture
	}
}

/* ---------- ID3v2, as found in an MP3 ---------- */

func fromID3(r io.ReadSeeker) (Picture, error) {
	var header [10]byte
	if _, err := io.ReadFull(r, header[:]); err != nil {
		return Picture{}, ErrNoPicture
	}
	if !bytes.HasPrefix(header[:], []byte("ID3")) {
		return Picture{}, ErrNoPicture
	}
	major := header[3]
	size := syncSafe(header[6:10])
	if size <= 0 || size > maxMetadata {
		return Picture{}, ErrNoPicture
	}
	// The tag is read whole because its frames have to be walked in order,
	// and the tag's own declared size is the bound.
	tag := make([]byte, size)
	if _, err := io.ReadFull(r, tag); err != nil {
		return Picture{}, ErrNoPicture
	}
	if header[5]&0x40 != 0 {
		// An extended header, whose size is the first four bytes of the tag.
		if len(tag) < 4 {
			return Picture{}, ErrNoPicture
		}
		skip := syncSafe(tag[:4])
		if skip <= 0 || skip > len(tag) {
			return Picture{}, ErrNoPicture
		}
		tag = tag[skip:]
	}

	best := Picture{Kind: -1}
	for at := 0; at+10 <= len(tag); {
		id := string(tag[at : at+4])
		if id == "\x00\x00\x00\x00" {
			break // padding: the rest of the tag is empty
		}
		var frameSize int
		if major >= 4 {
			frameSize = syncSafe(tag[at+4 : at+8])
		} else {
			frameSize = int(binary.BigEndian.Uint32(tag[at+4 : at+8]))
		}
		if frameSize <= 0 || at+10+frameSize > len(tag) {
			break
		}
		if id == "APIC" {
			if picture, err := parseAPIC(tag[at+10 : at+10+frameSize]); err == nil {
				if better(best, picture) {
					best = picture
				}
			}
		}
		at += 10 + frameSize
	}
	if best.Data == nil {
		return Picture{}, ErrNoPicture
	}
	return best, nil
}

// parseAPIC reads one attached-picture frame: a text encoding, a MIME type, a
// picture type, a description, and then the image.
func parseAPIC(frame []byte) (Picture, error) {
	if len(frame) < 4 {
		return Picture{}, ErrNoPicture
	}
	encoding := frame[0]
	rest := frame[1:]

	mime, rest, ok := cut(rest, 0)
	if !ok {
		return Picture{}, ErrNoPicture
	}
	if len(rest) < 2 {
		return Picture{}, ErrNoPicture
	}
	kind := int(rest[0])
	rest = rest[1:]

	// The description is terminated the way the frame's encoding says: one
	// zero byte for the Latin-1 and UTF-8 encodings, two for the UTF-16 ones.
	if encoding == 1 || encoding == 2 {
		_, rest, ok = cutWide(rest)
	} else {
		_, rest, ok = cut(rest, 0)
	}
	if !ok || len(rest) == 0 || len(rest) > MaxPicture {
		return Picture{}, ErrNoPicture
	}
	return Picture{Data: rest, MIME: normalizeMIME(string(mime)), Kind: kind}, nil
}

// syncSafe reads ID3's seven-bits-per-byte length, which exists so a length
// can never contain a byte that looks like the start of an audio frame.
func syncSafe(b []byte) int {
	if len(b) < 4 {
		return 0
	}
	n := 0
	for _, part := range b[:4] {
		if part&0x80 != 0 {
			// Not sync-safe after all, which some taggers write in v2.4
			// frames: fall back to reading it as a plain integer.
			return int(binary.BigEndian.Uint32(b[:4]))
		}
		n = n<<7 | int(part)
	}
	return n
}

/* ---------- FLAC ---------- */

func fromFLAC(r io.Reader) (Picture, error) {
	var magic [4]byte
	if _, err := io.ReadFull(r, magic[:]); err != nil || string(magic[:]) != "fLaC" {
		return Picture{}, ErrNoPicture
	}

	best := Picture{Kind: -1}
	for {
		var head [4]byte
		if _, err := io.ReadFull(r, head[:]); err != nil {
			break
		}
		last := head[0]&0x80 != 0
		kind := head[0] & 0x7F
		size := int(head[1])<<16 | int(head[2])<<8 | int(head[3])
		if size < 0 || size > maxMetadata {
			break
		}
		if kind != 6 { // 6 is PICTURE; everything else is skipped past
			if _, err := io.CopyN(io.Discard, r, int64(size)); err != nil {
				break
			}
			if last {
				break
			}
			continue
		}

		block := make([]byte, size)
		if _, err := io.ReadFull(r, block); err != nil {
			break
		}
		if picture, err := parseFLACPicture(block); err == nil && better(best, picture) {
			best = picture
		}
		if last {
			break
		}
	}
	if best.Data == nil {
		return Picture{}, ErrNoPicture
	}
	return best, nil
}

// parseFLACPicture reads a METADATA_BLOCK_PICTURE, which is APIC's fields in
// a fixed order with explicit lengths.
func parseFLACPicture(block []byte) (Picture, error) {
	read := func(n int) ([]byte, bool) {
		if len(block) < n {
			return nil, false
		}
		out := block[:n]
		block = block[n:]
		return out, true
	}
	readLen := func() (int, bool) {
		raw, ok := read(4)
		if !ok {
			return 0, false
		}
		n := int(binary.BigEndian.Uint32(raw))
		if n < 0 || n > MaxPicture {
			return 0, false
		}
		return n, true
	}

	kindRaw, ok := read(4)
	if !ok {
		return Picture{}, ErrNoPicture
	}
	kind := int(binary.BigEndian.Uint32(kindRaw))

	mimeLen, ok := readLen()
	if !ok {
		return Picture{}, ErrNoPicture
	}
	mime, ok := read(mimeLen)
	if !ok {
		return Picture{}, ErrNoPicture
	}
	descLen, ok := readLen()
	if !ok {
		return Picture{}, ErrNoPicture
	}
	if _, ok := read(descLen); !ok {
		return Picture{}, ErrNoPicture
	}
	// Width, height, depth and the colour count, which say nothing this
	// server needs: the image itself is the authority on its own size.
	if _, ok := read(16); !ok {
		return Picture{}, ErrNoPicture
	}
	dataLen, ok := readLen()
	if !ok {
		return Picture{}, ErrNoPicture
	}
	data, ok := read(dataLen)
	if !ok || len(data) == 0 {
		return Picture{}, ErrNoPicture
	}
	return Picture{Data: data, MIME: normalizeMIME(string(mime)), Kind: kind}, nil
}

/* ---------- MP4, as found in an M4A ---------- */

// fromMP4 walks the box tree to moov/udta/meta/ilst/covr.
//
// Seeks rather than reads: an MP4's metadata can be at either end of the
// file, and the boxes on the way to it include the audio itself.
func fromMP4(r io.ReadSeeker) (Picture, error) {
	path := []string{"moov", "udta", "meta", "ilst", "covr"}
	start, end := int64(0), int64(1)<<62
	if seeker, ok := r.(interface{ Stat() (os.FileInfo, error) }); ok {
		if stat, err := seeker.Stat(); err == nil {
			end = stat.Size()
		}
	}

	for depth, want := range path {
		found, size, err := findBox(r, start, end, want)
		if err != nil {
			return Picture{}, ErrNoPicture
		}
		start, end = found, found+size
		if want == "meta" {
			// A meta box carries four bytes of version and flags before its
			// children, unlike every other container box here.
			start += 4
		}
		if depth == len(path)-1 {
			return mp4Cover(r, start, end)
		}
	}
	return Picture{}, ErrNoPicture
}

// findBox scans one level of the tree for a box by name, returning where its
// contents start and how long they are.
func findBox(r io.ReadSeeker, from, to int64, want string) (start, size int64, err error) {
	at := from
	for at+8 <= to {
		if _, err := r.Seek(at, io.SeekStart); err != nil {
			return 0, 0, err
		}
		var head [8]byte
		if _, err := io.ReadFull(r, head[:]); err != nil {
			return 0, 0, err
		}
		boxSize := int64(binary.BigEndian.Uint32(head[:4]))
		name := string(head[4:8])
		header := int64(8)
		switch boxSize {
		case 0:
			boxSize = to - at // the last box runs to the end
		case 1:
			// A 64-bit size, for a box larger than four gigabytes.
			var wide [8]byte
			if _, err := io.ReadFull(r, wide[:]); err != nil {
				return 0, 0, err
			}
			boxSize = int64(binary.BigEndian.Uint64(wide[:]))
			header = 16
		}
		if boxSize < header || at+boxSize > to {
			return 0, 0, ErrNoPicture
		}
		if name == want {
			return at + header, boxSize - header, nil
		}
		at += boxSize
	}
	return 0, 0, ErrNoPicture
}

// mp4Cover reads the data boxes inside a covr, which is where the bytes are.
func mp4Cover(r io.ReadSeeker, from, to int64) (Picture, error) {
	start, size, err := findBox(r, from, to, "data")
	if err != nil {
		return Picture{}, ErrNoPicture
	}
	if size < 8 || size > MaxPicture+8 {
		return Picture{}, ErrNoPicture
	}
	if _, err := r.Seek(start, io.SeekStart); err != nil {
		return Picture{}, ErrNoPicture
	}
	var head [8]byte
	if _, err := io.ReadFull(r, head[:]); err != nil {
		return Picture{}, ErrNoPicture
	}
	// The low byte of the version-and-flags word says what the payload is:
	// 13 is JPEG and 14 is PNG. Anything else is left to the decoder.
	mime := ""
	switch head[3] {
	case 13:
		mime = "image/jpeg"
	case 14:
		mime = "image/png"
	}

	data := make([]byte, size-8)
	if _, err := io.ReadFull(r, data); err != nil {
		return Picture{}, ErrNoPicture
	}
	if len(data) == 0 {
		return Picture{}, ErrNoPicture
	}
	return Picture{Data: data, MIME: mime, Kind: 3}, nil
}

/* ---------- shared ---------- */

// better prefers a front cover over any other kind of picture, and any
// picture over none. A file can carry a dozen — a back cover, the label, a
// photograph of the band — and the album's poster should be the front.
func better(current, candidate Picture) bool {
	if candidate.Data == nil {
		return false
	}
	if current.Data == nil {
		return true
	}
	if candidate.Kind == 3 && current.Kind != 3 {
		return true
	}
	return false
}

// cut splits at the first occurrence of a byte, reporting whether it was
// there at all: a field with no terminator is a truncated file, not an empty
// field.
func cut(data []byte, sep byte) (before, after []byte, ok bool) {
	at := bytes.IndexByte(data, sep)
	if at < 0 {
		return nil, nil, false
	}
	return data[:at], data[at+1:], true
}

// cutWide does the same for a UTF-16 field, whose terminator is two zero
// bytes on an even boundary.
func cutWide(data []byte) (before, after []byte, ok bool) {
	for at := 0; at+1 < len(data); at += 2 {
		if data[at] == 0 && data[at+1] == 0 {
			return data[:at], data[at+2:], true
		}
	}
	return nil, nil, false
}

func normalizeMIME(mime string) string {
	mime = strings.TrimSpace(strings.ToLower(mime))
	switch mime {
	case "", "image":
		return ""
	case "jpeg", "jpg", "image/jpg":
		return "image/jpeg"
	case "png":
		return "image/png"
	default:
		return mime
	}
}

// Describe is what a log line says about a picture that was found.
func (p Picture) Describe() string {
	return fmt.Sprintf("%s, %d bytes", p.MIME, len(p.Data))
}
