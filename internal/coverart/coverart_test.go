package coverart_test

import (
	"bytes"
	"encoding/binary"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/inknironapps/bask-media-server/internal/coverart"
)

// The three containers below are built by hand, byte for byte, because that
// is the only way to test a parser of them: a committed fixture would say
// nothing about which field is which, and these tests are also the
// documentation of the three formats.

func write(t *testing.T, name string, data []byte) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// syncSafe is ID3's seven-bits-per-byte length.
func syncSafe(n int) []byte {
	return []byte{
		byte(n >> 21 & 0x7F), byte(n >> 14 & 0x7F), byte(n >> 7 & 0x7F), byte(n & 0x7F),
	}
}

// apic is one attached-picture frame: encoding, MIME, picture type,
// description, image.
func apic(mime string, kind byte, description string, data []byte) []byte {
	var frame bytes.Buffer
	frame.WriteByte(0) // Latin-1
	frame.WriteString(mime)
	frame.WriteByte(0)
	frame.WriteByte(kind)
	frame.WriteString(description)
	frame.WriteByte(0)
	frame.Write(data)

	var out bytes.Buffer
	out.WriteString("APIC")
	out.Write(syncSafe(frame.Len()))
	out.Write([]byte{0, 0}) // flags
	out.Write(frame.Bytes())
	return out.Bytes()
}

func mp3With(frames ...[]byte) []byte {
	var body bytes.Buffer
	for _, frame := range frames {
		body.Write(frame)
	}
	var out bytes.Buffer
	out.WriteString("ID3")
	out.Write([]byte{4, 0, 0}) // version 2.4, no flags
	out.Write(syncSafe(body.Len()))
	out.Write(body.Bytes())
	out.WriteString("\xff\xfbaudio frames would follow")
	return out.Bytes()
}

func TestTheCoverInsideAnMP3(t *testing.T) {
	path := write(t, "01 - track.mp3", mp3With(apic("image/jpeg", 3, "front", []byte("JPEGBYTES"))))

	picture, err := coverart.Read(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(picture.Data) != "JPEGBYTES" {
		t.Errorf("got %q", picture.Data)
	}
	if picture.MIME != "image/jpeg" {
		t.Errorf("mime = %q", picture.MIME)
	}
}

// A file can carry a dozen pictures — the disc, the back, a photograph of the
// band — and the album's cover is the front one.
func TestTheFrontCoverWinsOverTheOthers(t *testing.T) {
	path := write(t, "track.mp3", mp3With(
		apic("image/png", 4, "back", []byte("BACK")),
		apic("image/jpeg", 3, "front", []byte("FRONT")),
		apic("image/png", 6, "media", []byte("DISC")),
	))

	picture, err := coverart.Read(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(picture.Data) != "FRONT" {
		t.Errorf("got %q, want the front cover", picture.Data)
	}
}

// Most files have no picture in them, which is a fact and not a fault: the
// scanner tries the folder image first and only asks the file when there was
// none.
func TestAFileWithNoPictureSaysSo(t *testing.T) {
	path := write(t, "bare.mp3", mp3With())

	if _, err := coverart.Read(path); !errors.Is(err, coverart.ErrNoPicture) {
		t.Fatalf("err = %v, want ErrNoPicture", err)
	}
}

func flacWith(blocks ...[]byte) []byte {
	var out bytes.Buffer
	out.WriteString("fLaC")
	for i, block := range blocks {
		kind := byte(6) // PICTURE
		if i == 0 {
			kind = 0 // STREAMINFO, which every FLAC starts with
		}
		if i == len(blocks)-1 {
			kind |= 0x80 // the last metadata block
		}
		out.WriteByte(kind)
		out.Write([]byte{byte(len(block) >> 16), byte(len(block) >> 8), byte(len(block))})
		out.Write(block)
	}
	return out.Bytes()
}

func flacPicture(mime string, kind uint32, data []byte) []byte {
	var out bytes.Buffer
	write32 := func(v uint32) { binary.Write(&out, binary.BigEndian, v) }
	write32(kind)
	write32(uint32(len(mime)))
	out.WriteString(mime)
	write32(0)  // no description
	write32(16) // width
	write32(16) // height
	write32(24) // depth
	write32(0)  // colours
	write32(uint32(len(data)))
	out.Write(data)
	return out.Bytes()
}

func TestTheCoverInsideAFLAC(t *testing.T) {
	path := write(t, "02 - track.flac", flacWith(
		bytes.Repeat([]byte{0}, 34), // a stand-in for STREAMINFO
		flacPicture("image/png", 3, []byte("PNGBYTES")),
	))

	picture, err := coverart.Read(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(picture.Data) != "PNGBYTES" || picture.MIME != "image/png" {
		t.Errorf("got %q (%s)", picture.Data, picture.MIME)
	}
}

// box writes an MP4 box: a length, a name, and its contents.
func box(name string, contents ...[]byte) []byte {
	var body bytes.Buffer
	for _, part := range contents {
		body.Write(part)
	}
	var out bytes.Buffer
	binary.Write(&out, binary.BigEndian, uint32(body.Len()+8))
	out.WriteString(name)
	out.Write(body.Bytes())
	return out.Bytes()
}

func TestTheCoverInsideAnM4A(t *testing.T) {
	data := box("data", []byte{0, 0, 0, 13, 0, 0, 0, 0}, []byte("M4ABYTES"))
	file := append(
		box("ftyp", []byte("M4A ")),
		box("moov", box("udta", box("meta", []byte{0, 0, 0, 0}, box("ilst", box("covr", data)))))...,
	)
	path := write(t, "03 - track.m4a", file)

	picture, err := coverart.Read(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(picture.Data) != "M4ABYTES" {
		t.Errorf("got %q", picture.Data)
	}
	// The low byte of the data box's flags says which format it is, and 13 is
	// a JPEG — the one thing an MP4 cover says about itself.
	if picture.MIME != "image/jpeg" {
		t.Errorf("mime = %q, want image/jpeg", picture.MIME)
	}
}

// A truncated file is the one a download stopped halfway through, and the
// answer has to be "no picture" rather than a panic or a slice of whatever
// follows.
func TestATruncatedFileIsRefusedRatherThanRead(t *testing.T) {
	full := mp3With(apic("image/jpeg", 3, "front", bytes.Repeat([]byte("x"), 400)))
	for _, cut := range []int{12, 20, 40, 120} {
		if cut >= len(full) {
			continue
		}
		path := write(t, "cut.mp3", full[:cut])
		if _, err := coverart.Read(path); err == nil {
			t.Errorf("a file cut at %d bytes returned a picture", cut)
		}
	}
}

// Nothing here reads a whole file: a cover is a few hundred kilobytes, and a
// field claiming to be larger is a corrupt file rather than an allocation to
// make.
func TestAnImpossiblePictureLengthIsRefused(t *testing.T) {
	var picture bytes.Buffer
	write32 := func(v uint32) { binary.Write(&picture, binary.BigEndian, v) }
	write32(3)
	write32(10)
	picture.WriteString("image/jpeg")
	write32(0)
	for range 4 {
		write32(0)
	}
	write32(1 << 30) // a gigabyte, claimed in a block of a few bytes
	path := write(t, "liar.flac", flacWith(bytes.Repeat([]byte{0}, 34), picture.Bytes()))

	if _, err := coverart.Read(path); !errors.Is(err, coverart.ErrNoPicture) {
		t.Fatalf("err = %v, want ErrNoPicture", err)
	}
}
