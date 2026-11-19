// Package photos reads what a picture file says about itself.
//
// Three facts matter for a photo library, and all three are in the file: how
// big it is, which way up it goes, and when it was taken. The first comes
// from the format's own header, the other two from EXIF, and both are read
// without decoding a single pixel — a camera roll is tens of thousands of
// files, and a scan that decoded each one would be a scan nobody finishes.
//
// EXIF is parsed here rather than pulled in as a dependency because the part
// of it that matters is three tags, and the format is twenty-five years old
// and unchanging. See docs/RESOURCE-BUDGET.md for why every read is bounded.
package photos

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"image"
	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"
	"io"
	"os"
	"time"

	_ "golang.org/x/image/bmp"
	_ "golang.org/x/image/tiff"
	_ "golang.org/x/image/webp"
)

// ErrNoPixels means the format is one this program cannot decode, which today
// is HEIC and AVIF: a phone's own format, listed in the library and served as
// it is, with no dimensions and no thumbnail of our making.
var ErrNoPixels = errors.New("this format cannot be decoded here")

// Info is what one picture file says about itself.
type Info struct {
	Width  int
	Height int

	// Orientation is the EXIF value, 1 through 8, or 0 when the file does not
	// say. Anything above 1 means the pixels are stored one way and meant to
	// be shown another, which is why a phone photograph arrives sideways in
	// anything that ignores it.
	Orientation int

	// TakenAt is EXIF DateTimeOriginal where the file has one, and the file's
	// mtime where it does not. Never zero for a file that exists, because an
	// album has to be orderable by something.
	TakenAt time.Time
}

// Rotated reports whether showing this picture correctly swaps its width and
// height, which is the case for the four quarter-turn orientations.
func (i Info) Rotated() bool {
	return i.Orientation >= 5 && i.Orientation <= 8
}

// Display is the size this picture is meant to be seen at, with a quarter
// turn applied where the file asks for one.
func (i Info) Display() (width, height int) {
	if i.Rotated() {
		return i.Height, i.Width
	}
	return i.Width, i.Height
}

// maxHeader is how much of a file is read looking for its metadata. EXIF
// lives in the first segment of a JPEG and a TIFF's first directory is near
// the front, so a megabyte is generous; the point of the cap is that a
// corrupt file cannot make this read a whole disk.
const maxHeader = 1 << 20

// Read measures a picture without decoding it.
//
// A format that cannot be decoded still returns an Info — with a TakenAt from
// the filesystem — and ErrNoPixels, because "this is a photograph taken on
// Tuesday that we cannot measure" is a usable answer and an error is not.
func Read(path string) (Info, error) {
	file, err := os.Open(path)
	if err != nil {
		return Info{}, err
	}
	defer file.Close()

	info := Info{}
	if stat, err := file.Stat(); err == nil {
		info.TakenAt = stat.ModTime().UTC()
	}

	header, err := io.ReadAll(io.LimitReader(file, maxHeader))
	if err != nil {
		return info, fmt.Errorf("read %s: %w", path, err)
	}

	if config, _, err := image.DecodeConfig(bytes.NewReader(header)); err == nil {
		info.Width, info.Height = config.Width, config.Height
	} else {
		// A format with no decoder registered, or a file whose header is
		// longer than the cap. Either way the dimensions are unknown rather
		// than wrong.
		if orientation, taken, ok := exif(header); ok {
			info.Orientation = orientation
			if !taken.IsZero() {
				info.TakenAt = taken
			}
		}
		return info, ErrNoPixels
	}

	if orientation, taken, ok := exif(header); ok {
		info.Orientation = orientation
		if !taken.IsZero() {
			info.TakenAt = taken
		}
	}
	return info, nil
}

// exif pulls the orientation and the capture time out of a file's header.
//
// Handles the two places EXIF lives: a JPEG's APP1 segment, and the start of
// a TIFF, which is also what a raw file and a HEIC's metadata look like.
func exif(header []byte) (orientation int, taken time.Time, ok bool) {
	if block, found := jpegExif(header); found {
		return readTIFF(block)
	}
	if len(header) > 8 && (bytes.HasPrefix(header, []byte("II*\x00")) || bytes.HasPrefix(header, []byte("MM\x00*"))) {
		return readTIFF(header)
	}
	return 0, time.Time{}, false
}

// jpegExif walks a JPEG's markers to its EXIF segment.
//
// Walked rather than searched for, because the bytes "Exif\x00\x00" can
// appear inside the compressed picture data and a search would find that one
// in a file with no EXIF at all.
func jpegExif(data []byte) ([]byte, bool) {
	if len(data) < 4 || data[0] != 0xFF || data[1] != 0xD8 {
		return nil, false
	}
	for at := 2; at+4 <= len(data); {
		if data[at] != 0xFF {
			return nil, false
		}
		marker := data[at+1]
		switch {
		case marker == 0xD8 || marker == 0x01 || (marker >= 0xD0 && marker <= 0xD7):
			at += 2
			continue
		case marker == 0xDA || marker == 0xD9:
			// Start of the picture itself: there is no metadata past here.
			return nil, false
		}
		length := int(binary.BigEndian.Uint16(data[at+2 : at+4]))
		if length < 2 || at+2+length > len(data) {
			return nil, false
		}
		segment := data[at+4 : at+2+length]
		if marker == 0xE1 && len(segment) > 6 && bytes.HasPrefix(segment, []byte("Exif\x00\x00")) {
			return segment[6:], true
		}
		at += 2 + length
	}
	return nil, false
}

// EXIF tags, which are the whole of this file's vocabulary.
const (
	tagOrientation      = 0x0112
	tagDateTime         = 0x0132
	tagExifIFD          = 0x8769
	tagDateTimeOriginal = 0x9003
)

// readTIFF reads IFD0 and, where it points to one, the EXIF sub-directory.
func readTIFF(block []byte) (orientation int, taken time.Time, ok bool) {
	if len(block) < 8 {
		return 0, time.Time{}, false
	}
	var order binary.ByteOrder
	switch {
	case block[0] == 'I' && block[1] == 'I':
		order = binary.LittleEndian
	case block[0] == 'M' && block[1] == 'M':
		order = binary.BigEndian
	default:
		return 0, time.Time{}, false
	}
	if order.Uint16(block[2:4]) != 42 {
		return 0, time.Time{}, false
	}

	first := int(order.Uint32(block[4:8]))
	values, subIFD := readIFD(block, order, first)
	if sub, found := subIFD[tagExifIFD]; found {
		// Only one level down, and only the one directory: EXIF can chain,
		// and following chains out of a file is how a parser becomes a loop.
		more, _ := readIFD(block, order, sub)
		for tag, value := range more {
			if _, already := values[tag]; !already {
				values[tag] = value
			}
		}
	}

	if raw, found := values[tagOrientation]; found {
		if n, err := asInt(raw); err == nil && n >= 1 && n <= 8 {
			orientation = n
		}
	}
	for _, tag := range []uint16{tagDateTimeOriginal, tagDateTime} {
		if raw, found := values[tag]; found {
			if when, err := parseEXIFTime(string(raw)); err == nil {
				taken = when
				break
			}
		}
	}
	return orientation, taken, orientation != 0 || !taken.IsZero()
}

// readIFD returns one directory's values, keyed by tag, plus the offsets of
// any sub-directories it points at.
func readIFD(block []byte, order binary.ByteOrder, at int) (map[uint16][]byte, map[uint16]int) {
	values := map[uint16][]byte{}
	offsets := map[uint16]int{}
	if at < 0 || at+2 > len(block) {
		return values, offsets
	}
	count := int(order.Uint16(block[at : at+2]))
	// A directory with thousands of entries is a corrupt file, and reading it
	// would be reading whatever follows as tags.
	if count > 512 {
		count = 512
	}
	for i := range count {
		entry := at + 2 + i*12
		if entry+12 > len(block) {
			break
		}
		tag := order.Uint16(block[entry : entry+2])
		kind := order.Uint16(block[entry+2 : entry+4])
		items := int(order.Uint32(block[entry+4 : entry+8]))
		size := items * typeSize(kind)
		if size < 0 || size > len(block) {
			continue
		}

		var raw []byte
		if size <= 4 {
			raw = block[entry+8 : entry+8+max(size, 0)]
		} else {
			offset := int(order.Uint32(block[entry+8 : entry+12]))
			if offset < 0 || offset+size > len(block) {
				continue
			}
			raw = block[offset : offset+size]
		}
		if tag == tagExifIFD && size == 4 {
			offsets[tag] = int(order.Uint32(block[entry+8 : entry+12]))
			continue
		}
		switch kind {
		case 1, 2, 7: // byte, ASCII, undefined
			values[tag] = bytes.TrimRight(raw, "\x00")
		// Numbers are stored back big-endian whatever the file's own order
		// was, so asInt has one thing to read rather than two.
		case 3: // short
			if len(raw) >= 2 {
				out := make([]byte, 2)
				binary.BigEndian.PutUint16(out, order.Uint16(raw[:2]))
				values[tag] = out
			}
		case 4: // long
			if len(raw) >= 4 {
				out := make([]byte, 4)
				binary.BigEndian.PutUint32(out, order.Uint32(raw[:4]))
				values[tag] = out
			}
		}
	}
	return values, offsets
}

func typeSize(kind uint16) int {
	switch kind {
	case 1, 2, 6, 7:
		return 1
	case 3, 8:
		return 2
	case 4, 9, 11:
		return 4
	case 5, 10, 12:
		return 8
	default:
		return 0
	}
}

// asInt reads a short or a long stored big-endian by readIFD.
func asInt(raw []byte) (int, error) {
	switch len(raw) {
	case 2:
		return int(binary.BigEndian.Uint16(raw)), nil
	case 4:
		return int(binary.BigEndian.Uint32(raw)), nil
	default:
		return 0, errors.New("not a number")
	}
}

// parseEXIFTime reads EXIF's own date format, which is local time with no
// zone: "2016:07:04 18:22:01". Kept as a wall-clock time in UTC, because
// guessing a zone would move a photograph to a different day.
func parseEXIFTime(value string) (time.Time, error) {
	value = string(bytes.TrimRight([]byte(value), "\x00 "))
	for _, layout := range []string{"2006:01:02 15:04:05", "2006-01-02 15:04:05", "2006:01:02"} {
		if when, err := time.ParseInLocation(layout, value, time.UTC); err == nil {
			if when.Year() > 1900 {
				return when, nil
			}
		}
	}
	return time.Time{}, fmt.Errorf("not an EXIF time: %q", value)
}
