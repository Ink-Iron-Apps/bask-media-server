package transcode

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"time"
)

// Hardware encoding.
//
// A software encode of one 4K stream will occupy every core a household
// server has, and the machine that holds a film library is usually the
// machine with a graphics chip sitting idle beside it. This is the part that
// uses it.
//
// Two rules shape everything here. Hardware encoding is an optimisation, so
// a failure of it must never be a failure to watch something: every path
// falls back to the software encoder and says so once. And support is
// claimed only after it has been proved on this machine — `-hwaccels` lists
// what ffmpeg was compiled with, which on a stock build includes accelerators
// for hardware that is not present, drivers that are not installed and
// devices this process may not open.

// Accel is one way of encoding with hardware.
type Accel struct {
	// Name is what an operator writes in the flag and reads in the log.
	Name string

	// Encoder is the ffmpeg encoder, used in place of libx264.
	Encoder string

	// Before are arguments that have to appear ahead of the input, which is
	// where ffmpeg wants a device opened.
	Before []string

	// Quality is the encoder's own speed and rate control, which has a
	// different spelling for every one of them.
	Quality []string

	// Scale builds the filter chain for a target height. Hardware scaling
	// happens on the device for the accelerators that can, because pulling
	// frames back to system memory to resize them gives away most of what
	// the hardware was for.
	Scale func(height int) string
}

// softwareScale is the filter the CPU encoder uses, and the fallback for any
// accelerator that does not scale on the device.
func softwareScale(height int) string { return fmt.Sprintf("scale=-2:%d", height) }

// accelerators are every option, in the order they are tried.
//
// Ordered by how much work they take off the processor on the hardware they
// run on, not by preference: an operator who wants a particular one names it
// and detection is skipped.
func accelerators() []Accel {
	switch runtime.GOOS {
	case "darwin":
		return []Accel{{
			Name:    "videotoolbox",
			Encoder: "h264_videotoolbox",
			Quality: []string{"-realtime", "1"},
			Scale:   softwareScale,
		}}
	case "windows":
		return []Accel{
			{
				Name:    "nvenc",
				Encoder: "h264_nvenc",
				Quality: []string{"-preset", "p4", "-tune", "ll", "-rc", "vbr"},
				Scale:   softwareScale,
			},
			{
				Name:    "qsv",
				Encoder: "h264_qsv",
				Quality: []string{"-preset", "veryfast"},
				Scale:   softwareScale,
			},
			{
				Name:    "amf",
				Encoder: "h264_amf",
				Quality: []string{"-quality", "speed"},
				Scale:   softwareScale,
			},
		}
	default:
		return []Accel{
			{
				Name:    "nvenc",
				Encoder: "h264_nvenc",
				Quality: []string{"-preset", "p4", "-tune", "ll", "-rc", "vbr"},
				Scale:   softwareScale,
			},
			{
				Name:    "qsv",
				Encoder: "h264_qsv",
				Quality: []string{"-preset", "veryfast"},
				Scale:   softwareScale,
			},
			{
				// VAAPI is the one that scales on the device, and the one
				// whose filter chain has to be written in a particular
				// order: the frames are uploaded in a format the hardware
				// takes, then resized there.
				Name:    "vaapi",
				Encoder: "h264_vaapi",
				Before:  []string{"-vaapi_device", vaapiDevice()},
				Quality: []string{"-compression_level", "1"},
				Scale: func(height int) string {
					return fmt.Sprintf("format=nv12,hwupload,scale_vaapi=-2:%d", height)
				},
			},
		}
	}
}

// vaapiDevice is the render node, which is the same path on every Linux
// machine that has one. An operator with two cards and an opinion about
// which to use says so in the flag instead.
func vaapiDevice() string {
	if named := strings.TrimSpace(os.Getenv("BMS_VAAPI_DEVICE")); named != "" {
		return named
	}
	return "/dev/dri/renderD128"
}

// Detect finds an accelerator that works on this machine, or nil.
//
// Works, not "is compiled in": each candidate encodes a frame of a generated
// picture, because that is the only check that catches a driver that is not
// installed, a card that is not there, and a render node this process may not
// open — all three of which are the normal state of a stock ffmpeg on a
// server, and all three of which look like support until something tries.
//
// Called once at startup. A few hundred milliseconds there is worth more than
// the first viewer of the day discovering it.
func Detect(ctx context.Context, ffmpeg string, want string) *Accel {
	want = strings.ToLower(strings.TrimSpace(want))
	if want == "off" || want == "none" || want == "cpu" {
		return nil
	}

	for _, candidate := range accelerators() {
		if want != "" && want != "auto" && want != candidate.Name {
			continue
		}
		if works(ctx, ffmpeg, candidate) {
			found := candidate
			return &found
		}
	}
	return nil
}

// works encodes one frame and reports whether ffmpeg managed it.
func works(ctx context.Context, ffmpeg string, accel Accel) bool {
	// Its own deadline: a broken driver can hang rather than fail, and a
	// server that will not start because of an optimisation is worse than
	// one that quietly encodes on the processor.
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	args := []string{"-nostdin", "-hide_banner", "-loglevel", "error"}
	args = append(args, accel.Before...)
	args = append(args,
		"-f", "lavfi", "-i", "testsrc2=size=320x240:rate=1:duration=1",
		"-vf", accel.Scale(240),
		"-c:v", accel.Encoder)
	args = append(args, accel.Quality...)
	args = append(args, "-frames:v", "1", "-f", "null", "-")

	return exec.CommandContext(ctx, ffmpeg, args...).Run() == nil
}

// video appends the encoder's half of a command line, hardware or not.
//
// One function so the software and hardware paths cannot drift: every
// difference between them is a field on Accel, and a reader comparing the two
// is comparing data rather than two copies of an argument list.
func (s *Session) video(args []string, seconds float64, height int, bitRate int64) []string {
	accel := s.accel
	plan := s.req.Plan
	// A bitmap subtitle is composited in a filtergraph that this server only
	// knows how to write for the processor, so a session burning one encodes
	// there: see the note above [Session.overlay].
	if plan.Burning() && plan.Burn.Picture {
		accel = nil
	}

	encoder, quality := "libx264", []string{"-preset", "veryfast", "-crf", "21"}
	scale := softwareScale
	if accel != nil {
		encoder, quality, scale = accel.Encoder, accel.Quality, accel.Scale
	}

	args = append(args, "-c:v", encoder)
	args = append(args, quality...)
	if accel == nil {
		// Only the software encoder is told about the pixel format and the
		// profile: a hardware encoder produces what its device produces, and
		// naming a format it cannot make is how a working card starts
		// refusing files.
		args = append(args, "-pix_fmt", "yuv420p", "-profile:v", "high")
	}
	args = append(args,
		"-force_key_frames", fmt.Sprintf("expr:gte(t,n_forced*%g)", seconds),
		"-sc_threshold", "0")

	if bitRate > 0 {
		args = append(args,
			"-maxrate", fmt.Sprintf("%d", bitRate),
			"-bufsize", fmt.Sprintf("%d", bitRate*2))
	}
	// The filter chain. A text subtitle is rendered first, on the processor,
	// because libass draws text and not device frames — which also puts it
	// in the right place for VAAPI, whose own chain starts by uploading.
	//
	// A bitmap subtitle has already been dealt with by the filtergraph in
	// [Session.args], so nothing here scales it again.
	burning := plan.Burning() && plan.Burn.Picture
	chain := []string{}
	if subtitle := s.subtitleFilter(); subtitle != "" {
		chain = append(chain, subtitle)
	}
	if height > 0 && !burning {
		chain = append(chain, scale(height))
	}
	if len(chain) > 0 {
		args = append(args, "-vf", strings.Join(chain, ","))
	}
	return args
}
