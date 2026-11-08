package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func writeConf(t *testing.T, s string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "rnsalert.conf")
	if err := os.WriteFile(p, []byte(s), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

const addr = "0123456789abcdef0123456789abcdef"

func TestExampleConfigLoads(t *testing.T) {
	os.Chmod("rnsalert.conf.example", 0o644)
	c, err := loadConfig("rnsalert.conf.example")
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Alerts) != 4 || c.Alerts[0].Name != "Disk /" || c.Alerts[3].Unit != "°C" {
		t.Fatalf("alerts: %+v", c.Alerts)
	}
	g := c.Alerts[3]
	if !g.Th[thCritLow].Set || g.Th[thCritLow].V != 2 || g.MessageBy[CriticalLow] == nil {
		t.Errorf("greenhouse: %+v", g)
	}
	if c.Alerts[1].Confirm != 3 || c.Alerts[0].Repeat != 6*time.Hour {
		t.Errorf("confirm/repeat not parsed")
	}
}

func TestConfigErrors(t *testing.T) {
	base := "node = n:1\nto = " + addr + "\n"
	for name, body := range map[string]string{
		"no alerts":     base,
		"no command":    base + "[alert a]\nevery = 1m\nhigh_warning = 1\n",
		"no every":      base + "[alert a]\ncommand = echo 1\nhigh_warning = 1\n",
		"no threshold":  base + "[alert a]\ncommand = echo 1\nevery = 1m\n",
		"order":         base + "[alert a]\ncommand = echo 1\nevery = 1m\nhigh_warning = 9\ncritical_high = 5\n",
		"unknown key":   base + "[alert a]\ncommand = echo 1\nevery = 1m\nhigh_warning = 1\nbogus = 1\n",
		"bad template":  base + "[alert a]\ncommand = echo 1\nevery = 1m\nhigh_warning = 1\nmessage = {{.Nope}}\n",
		"bad state":     base + "[alert a]\ncommand = echo 1\nevery = 1m\nhigh_warning = 1\nnotify = sorta\n",
		"no recipients": "node = n:1\n[alert a]\ncommand = echo 1\nevery = 1m\nhigh_warning = 1\n",
		"duplicate":     base + "[alert a]\ncommand = echo 1\nevery = 1m\nhigh_warning = 1\n[alert a]\n",
		"bad address":   "to = 1234\n",
		"bad section":   base + "[a]\n",
	} {
		if _, err := loadConfig(writeConf(t, body)); err == nil {
			t.Errorf("%s: no error", name)
		}
	}
}

func TestConfigRefusesWritable(t *testing.T) {
	p := writeConf(t, "node = n:1\nto = "+addr+"\n[alert a]\ncommand = echo 1\nevery = 1m\nhigh_warning = 1\n")
	os.Chmod(p, 0o666)
	if _, err := loadConfig(p); err == nil || !strings.Contains(err.Error(), "writable") {
		t.Fatalf("got %v", err)
	}
}

func TestComments(t *testing.T) {
	for in, want := range map[string]string{
		"# all comment":                  "",
		"command = echo 1 # trailing":    "command = echo 1 ",
		"command = echo ${#x}":           "command = echo ${#x}",
		`message = issue \#4 is back`:    "message = issue #4 is back",
		"command = awk '/x/ {print $2}'": "command = awk '/x/ {print $2}'",
	} {
		if got := stripComment(in); got != want {
			t.Errorf("stripComment(%q) = %q, want %q", in, got, want)
		}
	}
}

func alert(t *testing.T, extra string) *Alert {
	t.Helper()
	c, err := loadConfig(writeConf(t, "to = "+addr+"\n[alert t]\ncommand = true\nevery = 1m\n"+
		"critical_low = 0\nlow_warning = 10\nhigh_warning = 80\ncritical_high = 90\n"+extra))
	if err != nil {
		t.Fatal(err)
	}
	return c.Alerts[0]
}

func TestClassify(t *testing.T) {
	a := alert(t, "")
	for v, want := range map[float64]State{
		-5: CriticalLow, 0: CriticalLow, 0.1: LowWarning, 10: LowWarning, 10.5: Safe,
		50: Safe, 79.9: Safe, 80: HighWarning, 89: HighWarning, 90: CriticalHigh, 1e9: CriticalHigh,
	} {
		if got := a.stateFor(v, Unknown); got != want {
			t.Errorf("%g: got %s, want %s", v, got, want)
		}
	}
	// Only a high side configured: low values are SAFE.
	one := alert(t, "")
	one.Th[thCritLow].Set, one.Th[thLowWarn].Set = false, false
	if s := one.stateFor(-100, Unknown); s != Safe {
		t.Errorf("no low thresholds: got %s", s)
	}
}

func TestHysteresis(t *testing.T) {
	a := alert(t, "hysteresis = 2\n")
	cases := []struct {
		v        float64
		cur, out State
	}{
		{79, HighWarning, HighWarning}, // inside the band: stays
		{77.9, HighWarning, Safe},      // cleared it
		{89, CriticalHigh, CriticalHigh},
		{87.5, CriticalHigh, HighWarning},
		{50, CriticalHigh, Safe},
		{85, HighWarning, HighWarning},
		{91, HighWarning, CriticalHigh}, // getting worse ignores hysteresis
		{11, LowWarning, LowWarning},
		{12.5, LowWarning, Safe},
		{1, CriticalLow, CriticalLow},
		{2.5, CriticalLow, LowWarning},
		{-1, HighWarning, CriticalLow}, // straight across
		{79, Safe, Safe},
	}
	for _, c := range cases {
		if got := a.stateFor(c.v, c.cur); got != c.out {
			t.Errorf("%g from %s: got %s, want %s", c.v, c.cur, got, c.out)
		}
	}
}

func TestStep(t *testing.T) {
	a := alert(t, "confirm = 2\nrepeat = 1h\n")
	var tr tracker
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	val := func(v float64) reading { return reading{Value: v} }
	step := func(r reading) *Event { now = now.Add(time.Minute); return a.step(&tr, r, now) }

	if ev := step(val(50)); ev != nil { // first, unconfirmed
		t.Fatalf("got %+v", ev)
	}
	if ev := step(val(50)); ev != nil || tr.State != Safe { // first SAFE is silent
		t.Fatalf("got %+v, state %s", ev, tr.State)
	}
	if ev := step(val(85)); ev != nil { // one high reading isn't enough
		t.Fatalf("unconfirmed: %+v", ev)
	}
	step(val(50))                       // and the streak resets
	if ev := step(val(85)); ev != nil { // so this is the first again
		t.Fatalf("streak not reset: %+v", ev)
	}
	ev := step(val(86))
	if ev == nil || ev.State != "HIGH WARNING" || ev.Prev != "SAFE" || ev.Value != "86" || ev.Threshold != "80" {
		t.Fatalf("got %+v", ev)
	}
	if ev := step(val(87)); ev != nil {
		t.Fatalf("no change, no repeat yet: %+v", ev)
	}
	now = now.Add(time.Hour)
	if ev := step(val(87)); ev == nil || !ev.Repeat {
		t.Fatalf("want repeat, got %+v", ev)
	}
	step(reading{Err: context.DeadlineExceeded})
	ev = step(reading{Err: context.DeadlineExceeded})
	if ev == nil || ev.State != "ERROR" || ev.Error == "" {
		t.Fatalf("got %+v", ev)
	}
	step(val(20))
	if ev := step(val(20)); ev == nil || ev.State != "SAFE" || ev.Prev != "ERROR" {
		t.Fatalf("recovery: %+v", ev)
	}
}

func TestNotifyFilter(t *testing.T) {
	a := alert(t, "notify = critical_high, critical_low\n")
	tr := tracker{State: Safe}
	if ev := a.step(&tr, reading{Value: 85}, time.Now()); ev != nil || tr.State != HighWarning {
		t.Fatalf("warning should be tracked but silent: %+v", ev)
	}
	if ev := a.step(&tr, reading{Value: 95}, time.Now()); ev == nil {
		t.Fatal("critical should send")
	}
}

func TestRender(t *testing.T) {
	a := alert(t, "unit = %\nmessage.critical_high = PANIC {{.Value}}{{.Unit}}\n")
	tr := tracker{State: Safe}
	title, body := a.render(a.step(&tr, reading{Value: 85}, time.Now()))
	if title != "t: HIGH WARNING" || body != "t is 85% (limit 80%), was SAFE" {
		t.Errorf("default: %q / %q", title, body)
	}
	_, body = a.render(a.step(&tr, reading{Value: 99.5}, time.Now()))
	if body != "PANIC 99.5%" {
		t.Errorf("per-state: %q", body)
	}
}

func TestRunCommand(t *testing.T) {
	a := &Alert{Name: "x", Timeout: 2 * time.Second}
	for cmd, want := range map[string]float64{
		"echo 42":                       42,
		"printf '\\n  17.5%% used\\n'":  17.5,
		"echo -3e2":                     -300,
		`echo "$RNSALERT_NAME" | wc -c`: 2,
	} {
		a.Command = cmd
		r := runCommand(context.Background(), a)
		if r.Err != nil || r.Value != want {
			t.Errorf("%s: got %v, %v", cmd, r.Value, r.Err)
		}
	}
	for _, cmd := range []string{"echo hello", "true", "echo NaN", "echo boom >&2; exit 3"} {
		a.Command = cmd
		if r := runCommand(context.Background(), a); r.Err == nil {
			t.Errorf("%s: no error", cmd)
		}
	}
	a.Command, a.Timeout = "sleep 30 | cat", 200*time.Millisecond
	start := time.Now()
	r := runCommand(context.Background(), a)
	if r.Err == nil || !strings.Contains(r.Err.Error(), "timed out") || time.Since(start) > 5*time.Second {
		t.Errorf("timeout: %v after %s", r.Err, time.Since(start))
	}
}

func TestNextRun(t *testing.T) {
	every := 5 * time.Minute
	fixed := func(d time.Duration) func() time.Duration { return func() time.Duration { return d } }
	t0 := time.Date(2026, 1, 1, 12, 3, 10, 0, time.UTC)

	// First run: soon, not at the next window.
	at, w := nextRun(t0, time.Time{}, every, fixed(7*time.Second))
	if !at.Equal(t0.Add(7*time.Second)) || !w.Equal(time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)) {
		t.Fatalf("first: %s window %s", at, w)
	}
	// Next: the following window plus offset.
	at, w = nextRun(at.Add(time.Second), w, every, fixed(7*time.Second))
	if want := time.Date(2026, 1, 1, 12, 5, 7, 0, time.UTC); !at.Equal(want) {
		t.Fatalf("next: %s, want %s", at, want)
	}
	// Overslept by several windows, past this window's offset: run now, once.
	late := time.Date(2026, 1, 1, 12, 21, 0, 0, time.UTC)
	at, w = nextRun(late, w, every, fixed(7*time.Second))
	if !at.Equal(late) || !w.Equal(time.Date(2026, 1, 1, 12, 20, 0, 0, time.UTC)) {
		t.Fatalf("late: %s window %s", at, w)
	}
	at, _ = nextRun(late.Add(time.Second), w, every, fixed(7*time.Second))
	if want := time.Date(2026, 1, 1, 12, 25, 7, 0, time.UTC); !at.Equal(want) {
		t.Fatalf("after late: %s", at)
	}
}

func TestOffsets(t *testing.T) {
	st := 20 * time.Second
	for i, want := range []time.Duration{0, 5 * time.Second, 10 * time.Second, 15 * time.Second} {
		if got := offsetFunc("spread", st, i, 4)(); got != want {
			t.Errorf("spread %d: %s, want %s", i, got, want)
		}
	}
	f := offsetFunc("random", st, 0, 4)
	seen := map[time.Duration]bool{}
	for range 200 {
		d := f()
		if d < 0 || d >= st {
			t.Fatalf("random offset %s out of [0, %s)", d, st)
		}
		seen[d] = true
	}
	if len(seen) < 100 {
		t.Errorf("random offsets look fixed: %d distinct", len(seen))
	}
	if offsetFunc("random", 0, 0, 1)() != 0 {
		t.Error("stagger 0 should give no offset")
	}
}

func TestStateJSON(t *testing.T) {
	for _, s := range allStates {
		b, _ := s.MarshalJSON()
		var got State
		if err := got.UnmarshalJSON(b); err != nil || got != s {
			t.Errorf("%s round-trips to %s (%v)", s, got, err)
		}
	}
}
