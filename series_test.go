package main

import (
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestSeriesRoundTrip(t *testing.T) {
	root := t.TempDir()
	base := time.Date(2026, 10, 31, 23, 0, 0, 0, time.UTC) // spans a month boundary
	for i := range 240 {
		v := float64(i)
		if i == 100 {
			v = math.NaN()
		}
		if err := appendPoint(root, "Disk /", base.Add(time.Duration(i)*time.Minute), v); err != nil {
			t.Fatal(err)
		}
	}
	if files, _ := filepath.Glob(filepath.Join(root, "*", "*.dat")); len(files) != 2 {
		t.Fatalf("want 2 month files, got %v", files)
	}
	r := newSeriesReader(root, "Disk /")
	pts := r.read(base, base.Add(4*time.Hour))
	if len(pts) != 240 || pts[0].V != 0 || pts[239].V != 239 || !math.IsNaN(pts[100].V) {
		t.Fatalf("got %d points", len(pts))
	}
	// A narrower window, then new data arriving: only the tail is read again.
	if got := r.read(base.Add(time.Hour), base.Add(2*time.Hour)); len(got) != 61 || got[0].V != 60 {
		t.Fatalf("window: %d points, first %v", len(got), got[0])
	}
	appendPoint(root, "Disk /", base.Add(240*time.Minute), 240)
	// A half-written record is ignored until complete.
	f, _ := os.OpenFile(monthFile(filepath.Join(root, seriesID("Disk /")), base.Add(5*time.Hour)), os.O_WRONLY|os.O_APPEND, 0)
	f.Write([]byte{1, 2, 3})
	f.Close()
	if got := r.read(base, base.Add(5*time.Hour)); len(got) != 241 || got[240].V != 240 {
		t.Fatalf("after append: %d points", len(got))
	}
	if other := newSeriesReader(root, "Disk /home").read(base, base.Add(5*time.Hour)); len(other) != 0 {
		t.Fatal("series leaked between alerts")
	}
}

func TestSeriesID(t *testing.T) {
	a, b := seriesID("Disk /"), seriesID("Disk -")
	if a == b || !strings.HasPrefix(a, "disk-") || strings.ContainsAny(a, "/ ") {
		t.Errorf("ids %q %q", a, b)
	}
	if seriesID("Disk /") != a {
		t.Error("not stable")
	}
}

func TestPrune(t *testing.T) {
	root := t.TempDir()
	now := time.Date(2026, 11, 8, 0, 0, 0, 0, time.UTC)
	for _, m := range []time.Time{now.AddDate(0, -3, 0), now.AddDate(0, -1, 0), now} {
		appendPoint(root, "x", m, 1)
	}
	pruneSeries(root, 40*24*time.Hour, now)
	files, _ := filepath.Glob(filepath.Join(root, "*", "*.dat"))
	if len(files) != 2 {
		t.Fatalf("kept %v", files)
	}
	pruneSeries(root, 0, now.AddDate(10, 0, 0)) // 0 = keep forever
	if again, _ := filepath.Glob(filepath.Join(root, "*", "*.dat")); len(again) != 2 {
		t.Fatal("retain 0 deleted data")
	}
}

func TestSummarizeAndETA(t *testing.T) {
	a := alert(t, "unit = %\n")
	now := time.Date(2026, 11, 8, 0, 0, 0, 0, time.UTC)
	var pts []point
	for d := 30; d >= 0; d-- { // climbing 1/day from 40 to 70
		pts = append(pts, point{now.AddDate(0, 0, -d).UnixMilli(), float64(70 - d)})
	}
	pts = append(pts, point{now.UnixMilli(), math.NaN()})
	s := summarize(pts)
	if s.N != 31 || s.Errors != 1 || s.Min != 40 || s.Max != 70 || math.Abs(s.SlopePerDay-1) > 1e-9 || s.R2 < 0.999 {
		t.Fatalf("stats %+v", s)
	}
	if e := eta(a, s, now, 30*24*time.Hour); !strings.Contains(e, "HIGH WARNING (80%) in ~10d") {
		t.Errorf("eta %q", e)
	}
	// A cycle has no trend to project.
	pts = nil
	for h := range 48 {
		pts = append(pts, point{now.Add(time.Duration(h) * time.Hour).UnixMilli(), 50 + 20*math.Sin(float64(h)/24*2*math.Pi)})
	}
	if e := eta(a, summarize(pts), now, 48*time.Hour); e != "" {
		t.Errorf("cycle eta %q", e)
	}
}

func TestChartAndSparkline(t *testing.T) {
	a := alert(t, "")
	now := time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC)
	var pts []point
	for i := range 60 {
		pts = append(pts, point{now.Add(time.Duration(i-60) * time.Minute).UnixMilli(), float64(i)})
	}
	from := now.Add(-time.Hour)
	if sp := sparkline(pts, from, now, 10); []rune(sp)[0] != '▁' || []rune(sp)[9] != '█' {
		t.Errorf("sparkline %q", sp)
	}
	lines := chart(a, pts, from, now, 60, 10, newPalette(false))
	if len(lines) != 10 {
		t.Fatalf("%d lines", len(lines))
	}
	for _, l := range lines {
		if visLen(l) > 60 {
			t.Errorf("line too wide (%d): %q", visLen(l), l)
		}
	}
	if !strings.Contains(strings.Join(lines, "\n"), "┄") {
		t.Error("no threshold line drawn")
	}
	if got := chart(a, nil, from, now, 60, 10, newPalette(false)); len(got) != 1 || !strings.Contains(got[0], "no data") {
		t.Errorf("empty: %q", got)
	}
}

func TestFit(t *testing.T) {
	s := "\x1b[1mhello\x1b[0m world"
	if visLen(s) != 11 {
		t.Fatalf("visLen %d", visLen(s))
	}
	if got := fit(s, 3); visLen(got) != 3 || !strings.HasPrefix(got, "\x1b[1mhel") {
		t.Errorf("fit %q", got)
	}
	if keys := splitKeys([]byte("j\x1b[Aé\r")); len(keys) != 4 || keys[1] != "\x1b[A" || keys[2] != "é" {
		t.Errorf("keys %q", keys)
	}
}

func TestParseDurationDays(t *testing.T) {
	if d, err := parseDuration("400d", false); err != nil || d != 400*24*time.Hour {
		t.Errorf("400d: %v %v", d, err)
	}
	if _, err := parseDuration("xd", false); err == nil {
		t.Error("xd accepted")
	}
}
