// Package fingerprint reduces a stretch of a file's audio to a small array of
// numbers that can be compared against another file's.
//
// This is what makes Skip Intro work on an ordinary library. Chapters are the
// cheap detector and almost nothing has them; what every episode of a season
// does have is the same opening audio, so the intro can be measured rather
// than guessed at — find the stretch an episode shares with its neighbours,
// and that is the intro.
//
// The fingerprint is a per-frame 32-bit hash in the shape Haitsma and
// Kalker's robust audio hashing describes: energy in 33 log-spaced bands, one
// bit per adjacent band pair, each bit the sign of how that pair's difference
// changed since the previous frame. Differences of differences are what make
// it survive re-encoding, a volume change and a different audio codec, which
// matters because two episodes of a show are routinely not encoded identically.
//
// Costs, because they are the reason for every choice here:
//
//   - Audio is decoded to mono 8 kHz, which is all a 300–3000 Hz band split
//     needs, and makes the decode a fraction of real time.
//   - A frame is 2048 samples with a 1024-sample hop, so ten minutes is 4,688
//     hashes — 18 KiB, against the ~100 MiB those ten minutes would be as PCM.
//   - Nothing holds the audio. The decoder is read in 32 KiB chunks and each
//     frame is folded into a hash and dropped.
package fingerprint

import (
	"bufio"
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"math"
	"math/bits"
	"os/exec"
	"time"
)

const (
	// SampleRate is what the audio is decoded to. The bands this hash uses
	// stop at 3 kHz, so anything above 8 kHz is paid for and discarded.
	SampleRate = 8000

	// FrameSize and Hop set the time resolution: a 256 ms window every 128 ms.
	// That is the accuracy a marker boundary is reported to, which is well
	// inside what anyone notices in a skip.
	FrameSize = 2048
	Hop       = 1024

	// Bands is one more than the number of bits in a hash: each bit compares
	// one adjacent pair.
	Bands = 33

	bandLowHz  = 300.0
	bandHighHz = 3000.0

	// maxFrames bounds one fingerprint however long the segment turns out to
	// be — a mislabelled duration or a file that never ends must not become an
	// unbounded allocation.
	maxFrames = 20000
)

// FrameDuration is how much time one hash covers.
var FrameDuration = time.Duration(Hop) * time.Second / SampleRate

// Print is one segment of one item's audio, fingerprinted.
type Print struct {
	ItemID int64

	// Offset is where this segment starts in the file, so a frame index can be
	// turned back into a position a client can seek to.
	Offset time.Duration

	Hashes []uint32
}

// At returns the position in the file of a frame index.
func (p Print) At(frame int) time.Duration {
	return p.Offset + time.Duration(frame)*FrameDuration
}

// Duration is how much audio this fingerprint covers.
func (p Print) Duration() time.Duration { return time.Duration(len(p.Hashes)) * FrameDuration }

// Printer fingerprints files by decoding them with ffmpeg.
//
// ffmpeg rather than a Go decoder: the formats in a real library are whatever
// handbrake, makemkv and twenty years of rippers produced, and the one program
// that reads all of them is already installed next to the ffprobe this server
// uses to measure durations.
type Printer struct {
	Path string

	// Timeout bounds one decode. A file on a failing disk can hang forever,
	// and a detection pass that hangs looks exactly like one that crashed.
	Timeout time.Duration
}

// New finds ffmpeg, preferring an explicit path and falling back to PATH.
func New(path string) (*Printer, error) {
	if path == "" {
		found, err := exec.LookPath("ffmpeg")
		if err != nil {
			return nil, fmt.Errorf("ffmpeg not found: %w", err)
		}
		path = found
	}
	return &Printer{Path: path, Timeout: 10 * time.Minute}, nil
}

// Head fingerprints the first window of a file, which is where an intro is.
func (p *Printer) Head(ctx context.Context, path string, window time.Duration) (Print, error) {
	return p.segment(ctx, path, 0, window, []string{"-ss", "0", "-t", seconds(window)})
}

// Tail fingerprints the last window of a file, which is where credits are.
//
// Takes the duration because the offset has to be reported in file time: a
// client needs a position to seek to, not "somewhere in the last ten minutes".
func (p *Printer) Tail(ctx context.Context, path string, duration, window time.Duration) (Print, error) {
	offset := duration - window
	if duration <= 0 {
		// Unknown duration: ask ffmpeg for the tail anyway, and report the
		// offset as zero rather than as a negative number. A marker from this
		// would be misplaced, so detection skips such items — see detect.
		return p.segment(ctx, path, 0, window, []string{"-sseof", "-" + seconds(window)})
	}
	if offset < 0 {
		offset = 0
	}
	return p.segment(ctx, path, offset, window, []string{"-sseof", "-" + seconds(window)})
}

func (p *Printer) segment(ctx context.Context, path string, offset, window time.Duration, seek []string) (Print, error) {
	timeout := p.Timeout
	if timeout <= 0 {
		timeout = 10 * time.Minute
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	args := append([]string{"-v", "error", "-nostdin"}, seek...)
	args = append(args,
		"-i", path,
		"-vn", "-sn", "-dn",
		"-ac", "1",
		"-ar", fmt.Sprint(SampleRate),
		"-f", "s16le",
		"-")

	cmd := exec.CommandContext(ctx, p.Path, args...)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return Print{}, fmt.Errorf("fingerprint %s: %w", path, err)
	}
	cmd.Stderr = nil
	if err := cmd.Start(); err != nil {
		return Print{}, fmt.Errorf("fingerprint %s: %w", path, err)
	}

	hashes, hashErr := hashStream(bufio.NewReaderSize(stdout, 32<<10), framesIn(window))

	// Drain before waiting: a decoder blocked writing into a pipe nobody is
	// reading never exits, and this is the subprocess that must die with its
	// request rather than outlive the pass.
	io.Copy(io.Discard, stdout)
	waitErr := cmd.Wait()
	if hashErr != nil {
		return Print{}, fmt.Errorf("fingerprint %s: %w", path, hashErr)
	}
	if waitErr != nil {
		return Print{}, fmt.Errorf("fingerprint %s: %w", path, waitErr)
	}
	return Print{Offset: offset, Hashes: hashes}, nil
}

func framesIn(window time.Duration) int {
	if window <= 0 {
		return maxFrames
	}
	frames := int(window/FrameDuration) + 2
	return min(frames, maxFrames)
}

func seconds(d time.Duration) string { return fmt.Sprintf("%.0f", d.Seconds()) }

// hashStream folds a stream of signed 16-bit mono samples into frame hashes.
//
// One frame buffer, one previous-energy array, one output slice: memory is
// proportional to the fingerprint, not to the audio.
func hashStream(r io.Reader, limit int) ([]uint32, error) {
	var (
		window = make([]float64, FrameSize)
		filled int
		raw    = make([]byte, 8<<10)
		out    []uint32
		table  = bandTable()
		hann   = hannWindow()
		reBuf  = make([]float64, FrameSize)
		imBuf  = make([]float64, FrameSize)
		// Two band arrays, swapped rather than allocated per frame: ten
		// minutes is five thousand frames, and an allocation each is churn
		// for nothing.
		energy         = make([]float64, Bands)
		previousEnergy = make([]float64, Bands)
		havePrevious   bool
	)
	if limit > 0 {
		out = make([]uint32, 0, limit)
	}

	flushFrame := func() {
		copy(reBuf, window)
		for i := range reBuf {
			reBuf[i] *= hann[i]
			imBuf[i] = 0
		}
		fft(reBuf, imBuf)

		var total float64
		for b := range Bands {
			lo, hi := table[b], table[b+1]
			var sum float64
			for bin := lo; bin < hi && bin < FrameSize/2; bin++ {
				sum += reBuf[bin]*reBuf[bin] + imBuf[bin]*imBuf[bin]
			}
			energy[b] = sum
			total += sum
		}

		// A floor relative to the frame's own energy, then a log.
		//
		// Both halves matter. The log makes a gain applied to the whole
		// signal add the same constant to every band, so it cancels out of
		// the band differences below and an episode mastered louder than
		// another still matches it. The floor is what keeps a band with
		// nothing in it from being compared on its noise: an empty band sits
		// at the quantisation floor, where the difference between two
		// encodings of the same audio is arbitrary, and the sign of an
		// arbitrary difference is a bit that flips for no reason. Clamped,
		// every empty band reads the same and contributes a stable bit — the
		// measured difference is seven bits of thirty-two on a volume change
		// without this, which is enough to lose a real match.
		floor := total*1e-6 + 1e-12
		for b := range energy {
			energy[b] = math.Log(math.Max(energy[b], floor))
		}

		if havePrevious {
			var h uint32
			for b := 0; b+1 < Bands; b++ {
				now := energy[b] - energy[b+1]
				then := previousEnergy[b] - previousEnergy[b+1]
				if now > then {
					h |= 1 << uint(b)
				}
			}
			out = append(out, h)
		}
		energy, previousEnergy = previousEnergy, energy
		havePrevious = true

		// Slide by the hop, keeping the overlap.
		copy(window, window[Hop:])
		filled = FrameSize - Hop
	}

	for len(out) < limit {
		n, err := r.Read(raw)
		for i := 0; i+1 < n; i += 2 {
			sample := float64(int16(binary.LittleEndian.Uint16(raw[i:i+2]))) / 32768
			window[filled] = sample
			filled++
			if filled == FrameSize {
				flushFrame()
				if len(out) >= limit {
					break
				}
			}
		}
		if err != nil {
			if err == io.EOF {
				return out, nil
			}
			return out, err
		}
		if n == 0 {
			return out, nil
		}
	}
	return out, nil
}

// bandTable is the FFT bin each band starts at, log-spaced between 300 Hz and
// 3 kHz: the range speech and music both live in, and the range a telephone
// codec would keep, which is a good proxy for what survives transcoding.
func bandTable() [Bands + 1]int {
	var table [Bands + 1]int
	ratio := math.Pow(bandHighHz/bandLowHz, 1/float64(Bands))
	binHz := float64(SampleRate) / float64(FrameSize)
	for b := range Bands + 1 {
		hz := bandLowHz * math.Pow(ratio, float64(b))
		table[b] = int(hz / binHz)
	}
	return table
}

func hannWindow() []float64 {
	w := make([]float64, FrameSize)
	for i := range w {
		w[i] = 0.5 - 0.5*math.Cos(2*math.Pi*float64(i)/float64(FrameSize-1))
	}
	return w
}

// fft is an in-place iterative radix-2 transform.
//
// Written out rather than taken as a dependency: it is thirty lines, it is the
// only maths this project needs, and a dependency here would be a dependency
// in every build of a server whose whole point is that it is one binary.
func fft(re, im []float64) {
	n := len(re)
	for i, j := 1, 0; i < n; i++ {
		bit := n >> 1
		for ; j&bit != 0; bit >>= 1 {
			j ^= bit
		}
		j ^= bit
		if i < j {
			re[i], re[j] = re[j], re[i]
			im[i], im[j] = im[j], im[i]
		}
	}
	for length := 2; length <= n; length <<= 1 {
		angle := -2 * math.Pi / float64(length)
		wRe, wIm := math.Cos(angle), math.Sin(angle)
		for i := 0; i < n; i += length {
			curRe, curIm := 1.0, 0.0
			for j := range length / 2 {
				evenRe, evenIm := re[i+j], im[i+j]
				oddRe, oddIm := re[i+j+length/2], im[i+j+length/2]
				mulRe := oddRe*curRe - oddIm*curIm
				mulIm := oddRe*curIm + oddIm*curRe
				re[i+j], im[i+j] = evenRe+mulRe, evenIm+mulIm
				re[i+j+length/2], im[i+j+length/2] = evenRe-mulRe, evenIm-mulIm
				curRe, curIm = curRe*wRe-curIm*wIm, curRe*wIm+curIm*wRe
			}
		}
	}
}

// Informative reports whether a frame hash says anything about what was
// playing.
//
// A hash is the sign of how each band pair's difference changed, so audio that
// is not changing — silence, a held tone, a room tone under dialogue — comes
// out all zeros or all ones. Such frames match each other perfectly and
// therefore match everything: measured on real files, a detector that counts
// them finds that every episode of a show shares its every silent passage, and
// happily reports an intro that covers the whole episode. They are excluded
// from matching entirely, which costs nothing, because a frame carrying no
// information cannot be evidence of anything either way.
func Informative(h uint32) bool {
	ones := bits.OnesCount32(h)
	return ones >= minInformativeBits && ones <= 32-minInformativeBits
}

// minInformativeBits is how lopsided a hash may be before it is treated as
// carrying nothing. Six of thirty-two: a frame of real audio sits near half,
// and the pathological cases sit at zero or thirty-two.
const minInformativeBits = 6

// Distance is how many of 32 bits two frame hashes disagree on. Same audio
// through two encoders lands in the low single digits; unrelated audio sits
// near sixteen, which is what a coin toss per bit looks like.
func Distance(a, b uint32) int { return bits.OnesCount32(a ^ b) }

// Encode packs a fingerprint for storage, little-endian so a database file
// moves between machines.
func Encode(hashes []uint32) []byte {
	out := make([]byte, 4*len(hashes))
	for i, h := range hashes {
		binary.LittleEndian.PutUint32(out[4*i:], h)
	}
	return out
}

// Decode reads a stored fingerprint.
func Decode(raw []byte) []uint32 {
	out := make([]uint32, len(raw)/4)
	for i := range out {
		out[i] = binary.LittleEndian.Uint32(raw[4*i:])
	}
	return out
}
