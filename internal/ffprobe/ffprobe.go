// Package ffprobe measures media files by asking ffprobe.
//
// A subprocess rather than a library binding: BMS never decodes video itself,
// so linking libav would buy nothing but a C toolchain and a cross-compilation
// problem. ffprobe is also what every other server in this space uses, so a
// machine that can run one can run BMS.
package ffprobe

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strconv"
	"time"
)

// Prober runs a known ffprobe binary.
type Prober struct {
	Path string

	// Timeout bounds one probe. A file on a sleeping drive or a dying disk can
	// hang forever, and a scan that hangs is indistinguishable from a scan
	// that crashed.
	Timeout time.Duration
}

// New finds ffprobe, preferring an explicit path and falling back to PATH.
func New(path string) (*Prober, error) {
	if path == "" {
		found, err := exec.LookPath("ffprobe")
		if err != nil {
			return nil, fmt.Errorf("ffprobe not found: %w", err)
		}
		path = found
	}
	return &Prober{Path: path, Timeout: 30 * time.Second}, nil
}

type probeOutput struct {
	Format struct {
		Duration string `json:"duration"`
	} `json:"format"`
}

// Duration reports how long a file plays for.
//
// Output is capped by reading into a bounded buffer: ffprobe asked for one
// field should answer in a few hundred bytes, and a buffer that grows to
// whatever a subprocess writes is a memory leak with extra steps.
func (p *Prober) Duration(ctx context.Context, path string) (time.Duration, error) {
	timeout := p.Timeout
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, p.Path,
		"-v", "error",
		"-show_entries", "format=duration",
		"-of", "json",
		path)

	var out bytes.Buffer
	out.Grow(512)
	cmd.Stdout = &out
	cmd.Stderr = nil

	if err := cmd.Run(); err != nil {
		return 0, fmt.Errorf("probe %s: %w", path, err)
	}

	var parsed probeOutput
	if err := json.Unmarshal(out.Bytes(), &parsed); err != nil {
		return 0, fmt.Errorf("probe %s: %w", path, err)
	}
	if parsed.Format.Duration == "" {
		return 0, fmt.Errorf("probe %s: no duration reported", path)
	}
	seconds, err := strconv.ParseFloat(parsed.Format.Duration, 64)
	if err != nil {
		return 0, fmt.Errorf("probe %s: %w", path, err)
	}
	return time.Duration(seconds * float64(time.Second)), nil
}
