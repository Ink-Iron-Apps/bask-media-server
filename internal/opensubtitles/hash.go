package opensubtitles

import (
	"encoding/binary"
	"fmt"
	"io"
	"os"
)

// The hash OpenSubtitles matches on.
//
// Not a cryptographic hash and not a hash of the file: it is the file's size
// plus the 64-bit sum of the first and last 64 kibibytes, read as little
// endian 64-bit words. That is the whole algorithm, it has been unchanged for
// twenty years, and it is what makes an exact match possible — two bytes into
// a film is a different hash, so a subtitle that claims this hash was timed
// against this file rather than against another release of the same title.
//
// Cheap, too: 128 KiB read from a file of any size, which is why it can be
// computed while somebody waits.

// hashChunk is how much is read from each end, fixed by the algorithm.
const hashChunk = 64 << 10

// Hash is the OpenSubtitles hash of a file, as the lower-case hex string the
// service expects, with its size.
func Hash(path string) (hash string, size int64, err error) {
	file, err := os.Open(path)
	if err != nil {
		return "", 0, err
	}
	defer file.Close()

	info, err := file.Stat()
	if err != nil {
		return "", 0, err
	}
	size = info.Size()
	if size < hashChunk {
		// Too small for the algorithm, which reads 64 KiB from each end: a
		// file this size is not a film, and there is nothing to match.
		return "", size, fmt.Errorf("file is too small to hash: %d bytes", size)
	}

	sum := uint64(size)
	head := make([]byte, hashChunk)
	if _, err := io.ReadFull(file, head); err != nil {
		return "", size, err
	}
	sum += words(head)

	if _, err := file.Seek(size-hashChunk, io.SeekStart); err != nil {
		return "", size, err
	}
	tail := make([]byte, hashChunk)
	if _, err := io.ReadFull(file, tail); err != nil {
		return "", size, err
	}
	sum += words(tail)

	return fmt.Sprintf("%016x", sum), size, nil
}

// words adds a block as little-endian 64-bit integers, wrapping, which is
// what the algorithm specifies.
func words(block []byte) uint64 {
	var sum uint64
	for i := 0; i+8 <= len(block); i += 8 {
		sum += binary.LittleEndian.Uint64(block[i : i+8])
	}
	return sum
}
