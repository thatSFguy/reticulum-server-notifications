package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"math"
	"math/rand/v2"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// Scheduling. Each alert's checks belong to wall-clock windows of length Every
// (every = 5m → windows start at :00, :05, :10 …). A check runs at the start of its
// window plus an offset below Stagger, so a server with many 1-minute alerts doesn't
// fork them all at second :00.
//
//	random  a fresh random offset each window (default)
//	spread  a fixed offset per alert, spaced evenly by config order: alert i of n
//	        runs at i/n × stagger into each window
//
// Windows are aligned to the Unix epoch, i.e. to UTC: every = 1h runs on the hour;
// every = 24h runs just after midnight UTC.

// offsetFunc returns the offset into a window for alert i of n.
func offsetFunc(mode string, stagger time.Duration, i, n int) func() time.Duration {
	if stagger <= 0 {
		return func() time.Duration { return 0 }
	}
	if mode == "spread" {
		off := stagger * time.Duration(i) / time.Duration(max(n, 1))
		return func() time.Duration { return off }
	}
	return func() time.Duration { return rand.N(stagger) }
}

// nextRun returns when to check next, given the window the last check belonged to
// (zero before the first). It never returns a time in the past, and skips windows
// missed while asleep or behind rather than running them back to back.
func nextRun(now, lastWindow time.Time, every time.Duration, offset func() time.Duration) (at, window time.Time) {
	if lastWindow.IsZero() {
		// The first check after start: within the stagger from now, not up to a whole
		// window later (with every = 1h that would be an hour of silence).
		return now.Add(offset()), now.Truncate(every)
	}
	window = lastWindow.Add(every)
	if cw := now.Truncate(every); cw.After(window) {
		window = cw
	}
	at = window.Add(offset())
	if at.Before(now) {
		if window.Equal(now.Truncate(every)) {
			return now, window // in the right window, just late: run now
		}
		window = window.Add(every)
		at = window.Add(offset())
	}
	return at, window
}

// Running a check.

const maxOutput = 64 << 10

// runCommand runs a's command under /bin/sh with a timeout and parses its output.
// The whole process group is killed on timeout, so a stuck pipeline can't linger.
func runCommand(ctx context.Context, a *Alert) reading {
	ctx, cancel := context.WithTimeout(ctx, a.Timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "/bin/sh", "-c", a.Command)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	cmd.WaitDelay = 2 * time.Second
	cmd.Env = append(os.Environ(), "RNSALERT_NAME="+a.Name)
	var out, errb capped
	out.max, errb.max = maxOutput, 1024
	cmd.Stdout, cmd.Stderr = &out, &errb

	err := cmd.Run()
	output := strings.TrimSpace(out.String())
	if ctx.Err() == context.DeadlineExceeded {
		return reading{Output: output, Err: fmt.Errorf("timed out after %s", a.Timeout)}
	}
	if err != nil {
		msg := strings.TrimSpace(errb.String())
		if i := strings.IndexByte(msg, '\n'); i >= 0 {
			msg = msg[:i]
		}
		if msg != "" {
			err = fmt.Errorf("%w: %s", err, msg)
		}
		return reading{Output: output, Err: err}
	}
	v, err := parseValue(output)
	return reading{Value: v, Output: output, Err: err}
}

// parseValue takes the first field of the first non-empty line, allowing a trailing %.
func parseValue(out string) (float64, error) {
	for _, line := range strings.Split(out, "\n") {
		f := strings.Fields(line)
		if len(f) == 0 {
			continue
		}
		return parseNumber(strings.TrimSuffix(f[0], "%"))
	}
	return 0, errors.New("command printed nothing")
}

func parseNumber(s string) (float64, error) {
	v, err := strconv.ParseFloat(s, 64)
	if err != nil || math.IsNaN(v) || math.IsInf(v, 0) {
		if len(s) > 40 {
			s = s[:40] + "…"
		}
		return 0, fmt.Errorf("%q is not a number", s)
	}
	return v, nil
}

// capped keeps the first max bytes written and quietly drops the rest.
type capped struct {
	bytes.Buffer
	max int
}

func (c *capped) Write(p []byte) (int, error) {
	if room := c.max - c.Len(); room > 0 {
		c.Buffer.Write(p[:min(len(p), room)])
	}
	return len(p), nil
}
