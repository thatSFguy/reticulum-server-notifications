package main

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"io"
	"math"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

// The history store: every reading of every alert, kept for graphs.
//
// It is deliberately tiny and dependency-free: one directory per alert under
// <state>/series, one append-only file per UTC month, and 16-byte records
// (int64 unix milliseconds, float64 value, little-endian). A failed check is stored as
// NaN, so graphs show a gap. A reading a minute is ~8 MB per alert per year.
//
// Readers (the dashboard) open the files read-only while the service appends; they only
// ever look at whole records, so a half-written tail is simply not seen yet. Retention
// drops whole months.

const recSize = 16

type point struct {
	T int64 // unix ms
	V float64
}

func (p point) time() time.Time { return time.UnixMilli(p.T) }

// seriesID names an alert's directory: a readable slug plus a hash, so any alert name
// (slashes, spaces, emoji) maps to one safe, stable directory.
func seriesID(name string) string {
	slug := strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			return r
		case r >= 'A' && r <= 'Z':
			return r + 'a' - 'A'
		}
		return '-'
	}, name)
	slug = strings.Trim(slug, "-")
	if len(slug) > 32 {
		slug = slug[:32]
	}
	h := sha256.Sum256([]byte(name))
	return slug + "-" + hex.EncodeToString(h[:4])
}

func monthFile(dir string, t time.Time) string {
	return filepath.Join(dir, t.UTC().Format("2006-01")+".dat")
}

// appendPoint records one reading.
func appendPoint(root, name string, t time.Time, v float64) error {
	dir := filepath.Join(root, seriesID(name))
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return err
	}
	f, err := os.OpenFile(monthFile(dir, t), os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o640)
	if err != nil {
		return err
	}
	var b [recSize]byte
	binary.LittleEndian.PutUint64(b[0:], uint64(t.UnixMilli()))
	binary.LittleEndian.PutUint64(b[8:], math.Float64bits(v))
	_, err = f.Write(b[:])
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	return err
}

// pruneSeries removes month files that ended more than retain ago.
func pruneSeries(root string, retain time.Duration, now time.Time) {
	if retain <= 0 {
		return
	}
	cutoff := now.Add(-retain).UTC()
	files, _ := filepath.Glob(filepath.Join(root, "*", "*.dat"))
	for _, f := range files {
		m, err := time.Parse("2006-01", strings.TrimSuffix(filepath.Base(f), ".dat"))
		if err == nil && m.AddDate(0, 1, 0).Before(cutoff) {
			os.Remove(f)
		}
	}
}

// seriesReader reads one alert's history and caches it, so a dashboard refreshing
// every few seconds only reads the records appended since last time.
type seriesReader struct {
	dir   string
	files map[string]*cachedFile
}

type cachedFile struct {
	off int64
	pts []point
}

func newSeriesReader(root, name string) *seriesReader {
	return &seriesReader{dir: filepath.Join(root, seriesID(name)), files: map[string]*cachedFile{}}
}

// read returns the points in [from, to], oldest first.
func (r *seriesReader) read(from, to time.Time) []point {
	var want []string
	for m := time.Date(from.UTC().Year(), from.UTC().Month(), 1, 0, 0, 0, 0, time.UTC); !m.After(to); m = m.AddDate(0, 1, 0) {
		want = append(want, monthFile(r.dir, m))
	}
	for k := range r.files {
		if !slices.Contains(want, k) {
			delete(r.files, k) // out of range now: free it
		}
	}
	f0, t0 := from.UnixMilli(), to.UnixMilli()
	var out []point
	for _, path := range want {
		c := r.files[path]
		if c == nil {
			c = &cachedFile{}
			r.files[path] = c
		}
		c.refresh(path)
		lo, _ := slices.BinarySearchFunc(c.pts, f0, func(p point, t int64) int { return cmpInt(p.T, t) })
		for _, p := range c.pts[lo:] {
			if p.T > t0 {
				break
			}
			out = append(out, p)
		}
	}
	return out
}

func (c *cachedFile) refresh(path string) {
	f, err := os.Open(path)
	if err != nil {
		return
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return
	}
	if fi.Size() < c.off { // replaced or truncated: start over
		c.off, c.pts = 0, nil
	}
	n := (fi.Size() - c.off) / recSize * recSize
	if n == 0 {
		return
	}
	buf := make([]byte, n)
	if _, err := f.ReadAt(buf, c.off); err != nil && err != io.EOF {
		return
	}
	for i := 0; i+recSize <= len(buf); i += recSize {
		p := point{
			T: int64(binary.LittleEndian.Uint64(buf[i:])),
			V: math.Float64frombits(binary.LittleEndian.Uint64(buf[i+8:])),
		}
		// Keep it sorted even if the clock stepped back.
		if k := len(c.pts); k > 0 && p.T < c.pts[k-1].T {
			j, _ := slices.BinarySearchFunc(c.pts, p.T, func(q point, t int64) int { return cmpInt(q.T, t) })
			c.pts = slices.Insert(c.pts, j, p)
			continue
		}
		c.pts = append(c.pts, p)
	}
	c.off += n
}

func cmpInt(a, b int64) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
}
