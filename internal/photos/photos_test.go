package photos_test

import (
	"bytes"
	"encoding/binary"
	"image"
	"image/color"
	"image/jpeg"
	"os"
	"path/filepath"
	"testing"

	"github.com/inknironapps/bask-media-server/internal/photos"
)

// jpegWithEXIF builds a real JPEG and splices an EXIF segment into it, which
// is what a camera writes: an APP1 segment holding a TIFF directory, right
// after the start-of-image marker.
//
// Built rather than committed as a fixture so the test says what it is testing
// — these are the exact bytes a phone's "rotate me" instruction is — and so
// the repository carries no binaries.
func jpegWithEXIF(t *testing.T, width, height int, orientation int, taken string) []byte {
	t.Helper()

	picture := image.NewRGBA(image.Rect(0, 0, width, height))
	for y := range height {
		for x := range width {
			picture.Set(x, y, color.RGBA{R: uint8(x), G: uint8(y), B: 90, A: 255})
		}
	}
	var encoded bytes.Buffer
	if err := jpeg.Encode(&encoded, picture, nil); err != nil {
		t.Fatal(err)
	}
	body := encoded.Bytes()

	exif := tiffDirectory(orientation, taken)
	segment := append([]byte("Exif\x00\x00"), exif...)
	header := make([]byte, 4)
	header[0], header[1] = 0xFF, 0xE1
	binary.BigEndian.PutUint16(header[2:], uint16(len(segment)+2))

	out := append([]byte{}, body[:2]...) // the start-of-image marker
	out = append(out, header...)
	out = append(out, segment...)
	return append(out, body[2:]...)
}

// tiffDirectory writes a little-endian TIFF with one directory: an
// orientation, and a pointer to a sub-directory holding the capture time.
func tiffDirectory(orientation int, taken string) []byte {
	var b bytes.Buffer
	b.WriteString("II")
	write16 := func(v uint16) { binary.Write(&b, binary.LittleEndian, v) }
	write32 := func(v uint32) { binary.Write(&b, binary.LittleEndian, v) }
	write16(42)
	write32(8) // the first directory begins straight after the header

	// IFD0: orientation, and the offset of the EXIF sub-directory.
	write16(2)
	write16(0x0112) // orientation
	write16(3)      // short
	write32(1)
	write32(uint32(orientation)) // in the entry itself, being four bytes or fewer
	write16(0x8769)              // the EXIF sub-directory
	write16(4)                   // long
	write32(1)
	write32(uint32(8 + 2 + 24 + 4)) // where it is written, below
	write32(0)                      // no next directory

	// The sub-directory: one entry, whose value is longer than four bytes and
	// so sits after it.
	stamp := []byte(taken + "\x00")
	write16(1)
	write16(0x9003) // DateTimeOriginal
	write16(2)      // ASCII
	write32(uint32(len(stamp)))
	write32(uint32(8 + 2 + 24 + 4 + 2 + 12 + 4))
	write32(0)
	b.Write(stamp)
	return b.Bytes()
}

func writeFile(t *testing.T, name string, data []byte) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// The three facts a photo library is built from, out of one file.
func TestReadFindsSizeTimeAndRotation(t *testing.T) {
	path := writeFile(t, "IMG_0042.jpg", jpegWithEXIF(t, 60, 40, 6, "2016:07:04 18:22:01"))

	info, err := photos.Read(path)
	if err != nil {
		t.Fatal(err)
	}

	if info.Width != 60 || info.Height != 40 {
		t.Errorf("got %dx%d, want 60x40", info.Width, info.Height)
	}
	if info.Orientation != 6 {
		t.Errorf("orientation = %d, want 6", info.Orientation)
	}
	if got := info.TakenAt.Format("2006-01-02 15:04:05"); got != "2016-07-04 18:22:01" {
		t.Errorf("taken at %s, want the EXIF time", got)
	}
}

// Orientation 6 is a phone held upright: the pixels are landscape and the
// picture is portrait. Anything that lays out a page from the stored size
// alone gets every phone photograph the wrong way round.
func TestAQuarterTurnSwapsTheDisplayedSize(t *testing.T) {
	path := writeFile(t, "upright.jpg", jpegWithEXIF(t, 60, 40, 6, "2016:07:04 18:22:01"))

	info, err := photos.Read(path)
	if err != nil {
		t.Fatal(err)
	}
	if !info.Rotated() {
		t.Fatal("orientation 6 is a quarter turn")
	}
	if w, h := info.Display(); w != 40 || h != 60 {
		t.Errorf("displayed size = %dx%d, want 40x60", w, h)
	}
}

// A picture with no EXIF at all is the common case for anything that has been
// through an editor, and it still has to be a row in the library.
func TestAPictureWithoutEXIFFallsBackToItsFileTime(t *testing.T) {
	var encoded bytes.Buffer
	if err := jpeg.Encode(&encoded, image.NewRGBA(image.Rect(0, 0, 8, 8)), nil); err != nil {
		t.Fatal(err)
	}
	path := writeFile(t, "plain.jpg", encoded.Bytes())

	info, err := photos.Read(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Width != 8 || info.Height != 8 {
		t.Errorf("got %dx%d, want 8x8", info.Width, info.Height)
	}
	if info.Orientation != 0 {
		t.Errorf("orientation = %d, want none claimed", info.Orientation)
	}
	if info.TakenAt.IsZero() {
		t.Error("a photograph with no EXIF still has to be orderable")
	}
}

// A phone's own format. There is no Go decoder for it, and the answer has to
// be "this is a photograph we cannot measure" rather than an error that would
// keep it out of the library.
func TestAFormatWeCannotDecodeIsStillAPhotograph(t *testing.T) {
	path := writeFile(t, "IMG_1.heic", []byte("\x00\x00\x00\x18ftypheic"))

	info, err := photos.Read(path)
	if err == nil {
		t.Fatal("want ErrNoPixels for a HEIC")
	}
	if info.TakenAt.IsZero() {
		t.Error("the file's own time is what is left to order it by")
	}
	if info.Width != 0 {
		t.Errorf("width = %d, want nothing claimed", info.Width)
	}
}

// The marker bytes "Exif\x00\x00" turn up inside compressed picture data, and
// a parser that searched for them rather than walking the markers would find
// that one and read noise as a date.
func TestEXIFIsNotSearchedForInThePictureData(t *testing.T) {
	var encoded bytes.Buffer
	if err := jpeg.Encode(&encoded, image.NewRGBA(image.Rect(0, 0, 8, 8)), nil); err != nil {
		t.Fatal(err)
	}
	// A JPEG whose scan data happens to contain the string, which is what a
	// search would trip over.
	body := append(encoded.Bytes(), []byte("Exif\x00\x00IIxx")...)
	path := writeFile(t, "noisy.jpg", body)

	info, err := photos.Read(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Orientation != 0 {
		t.Errorf("orientation = %d, want none: there is no EXIF segment here", info.Orientation)
	}
}
