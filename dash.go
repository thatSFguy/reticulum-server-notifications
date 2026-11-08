package main

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"
	"unicode/utf8"

	"golang.org/x/term"
)

// The terminal dashboard ("rnsalert dash"): every alert with its current value, state
// and a sparkline, and a full chart of one alert with stats and a trend projection.
// It reads the history files directly (read-only), so it works over plain SSH and needs
// nothing running but the service. Drawn with ANSI escapes; no curses library.

var ranges = []struct {
	name string
	d    time.Duration
}{
	{"1h", time.Hour}, {"6h", 6 * time.Hour}, {"24h", 24 * time.Hour},
	{"7d", 7 * 24 * time.Hour}, {"30d", 30 * 24 * time.Hour}, {"1y", 365 * 24 * time.Hour},
}

const refreshEvery = 5 * time.Second

type dash struct {
	cfg       *Config
	root      string // series directory
	stateFile string
	readers   map[string]*seriesReader
	trackers  map[string]*tracker
	pal       palette

	sel, rng int
	detail   bool
	w, h     int
}

func newDash(cfg *Config, state string, color bool) *dash {
	d := &dash{
		cfg: cfg, root: filepath.Join(state, "series"), stateFile: filepath.Join(state, "alerts.json"),
		readers: map[string]*seriesReader{}, pal: newPalette(color), rng: 2,
	}
	for _, a := range cfg.Alerts {
		d.readers[a.Name] = newSeriesReader(d.root, a.Name)
	}
	return d
}

func (d *dash) loadStates() {
	d.trackers = map[string]*tracker{}
	if data, err := os.ReadFile(d.stateFile); err == nil {
		json.Unmarshal(data, &d.trackers)
	}
}

// view is one alert's data for the current range.
type view struct {
	a     *Alert
	pts   []point
	st    stats
	state State
	last  float64 // latest finite reading, NaN if none
}

func (d *dash) view(a *Alert, now time.Time) view {
	from := now.Add(-ranges[d.rng].d)
	v := view{a: a, pts: d.readers[a.Name].read(from, now), last: math.NaN()}
	v.st = summarize(v.pts)
	if v.st.N > 0 {
		v.last = v.st.Last.V
	}
	if t := d.trackers[a.Name]; t != nil && t.State != Unknown {
		v.state = t.State
	} else if v.st.N > 0 {
		v.state = a.stateFor(v.last, Unknown)
	}
	return v
}

func runDash(cfg *Config, state string) error {
	in, out := int(os.Stdin.Fd()), int(os.Stdout.Fd())
	if !term.IsTerminal(in) || !term.IsTerminal(out) {
		return fmt.Errorf("dash needs a terminal (use \"rnsalert graph NAME\" for plain output)")
	}
	old, err := term.MakeRaw(in)
	if err != nil {
		return err
	}
	// Alternate screen, hidden cursor; put everything back however we leave.
	os.Stdout.WriteString("\x1b[?1049h\x1b[?25l")
	defer func() {
		os.Stdout.WriteString("\x1b[0m\x1b[?25h\x1b[?1049l")
		term.Restore(in, old)
	}()

	d := newDash(cfg, state, os.Getenv("NO_COLOR") == "")
	keys := make(chan string, 16)
	go func() {
		buf := make([]byte, 64)
		for {
			n, err := os.Stdin.Read(buf)
			if err != nil {
				close(keys)
				return
			}
			for _, k := range splitKeys(buf[:n]) {
				keys <- k
			}
		}
	}()
	winch := make(chan os.Signal, 1)
	signal.Notify(winch, syscall.SIGWINCH)
	defer signal.Stop(winch)
	tick := time.NewTicker(refreshEvery)
	defer tick.Stop()

	for {
		d.w, d.h, _ = term.GetSize(out)
		d.loadStates()
		os.Stdout.WriteString(d.render(time.Now()))
		select {
		case k, ok := <-keys:
			if !ok || !d.key(k) {
				return nil
			}
		case <-winch:
			os.Stdout.WriteString("\x1b[2J")
		case <-tick.C:
		}
	}
}

// splitKeys breaks a read into keys, keeping escape sequences (arrows) whole.
func splitKeys(b []byte) []string {
	var out []string
	for len(b) > 0 {
		if b[0] == 0x1b && len(b) >= 3 && (b[1] == '[' || b[1] == 'O') {
			out = append(out, string(b[:3]))
			b = b[3:]
			continue
		}
		_, n := utf8.DecodeRune(b)
		out = append(out, string(b[:n]))
		b = b[n:]
	}
	return out
}

// key handles one key press; false means quit.
func (d *dash) key(k string) bool {
	n := len(d.cfg.Alerts)
	switch k {
	case "q", "Q", "\x03", "\x04":
		return false
	case "\x1b[A", "\x1bOA", "k":
		d.sel = (d.sel - 1 + n) % n
	case "\x1b[B", "\x1bOB", "j", "\t":
		d.sel = (d.sel + 1) % n
	case "\r", "\n", "\x1b[C", "\x1bOC", "l":
		d.detail = true
	case "\x1b", "\x7f", "\b", "\x1b[D", "\x1bOD", "h":
		d.detail = false
	case "+", "=":
		d.rng = min(d.rng+1, len(ranges)-1)
	case "-", "_":
		d.rng = max(d.rng-1, 0)
	default:
		if len(k) == 1 && k[0] >= '1' && int(k[0]-'1') < len(ranges) {
			d.rng = int(k[0] - '1')
		}
	}
	return true
}

func (d *dash) render(now time.Time) string {
	var lines []string
	lines = append(lines, d.header(now), "")
	if d.detail {
		lines = append(lines, d.detailView(now)...)
	} else {
		lines = append(lines, d.listView(now)...)
	}
	help := "↑↓ select   enter graph   1-6 or +/- range   q quit"
	if d.detail {
		help = "↑↓ next alert   esc back   1-6 or +/- range   q quit"
	}
	for len(lines) < d.h-1 {
		lines = append(lines, "")
	}
	lines = append(lines[:max(0, min(len(lines), d.h-1))], d.pal.dim+help+d.pal.reset)

	var b strings.Builder
	b.WriteString("\x1b[H")
	for i, l := range lines {
		b.WriteString(fit(l, d.w))
		b.WriteString("\x1b[K")
		if i < len(lines)-1 {
			b.WriteString("\r\n")
		}
	}
	b.WriteString("\x1b[J")
	return b.String()
}

func (d *dash) header(now time.Time) string {
	left := fmt.Sprintf("%srnsalert%s %s  %s", d.pal.bold, d.pal.reset, hostname, now.Format("2006-01-02 15:04:05"))
	var r strings.Builder
	for i, rg := range ranges {
		if i == d.rng {
			fmt.Fprintf(&r, " %s %s %s", d.pal.inv, rg.name, d.pal.reset)
		} else {
			fmt.Fprintf(&r, "  %s ", rg.name)
		}
	}
	gap := d.w - visLen(left) - visLen(r.String())
	return left + strings.Repeat(" ", max(1, gap)) + r.String()
}

func (d *dash) listView(now time.Time) []string {
	nameW := 4
	for _, a := range d.cfg.Alerts {
		nameW = max(nameW, min(28, utf8.RuneCountInString(a.Name)))
	}
	const valW, stW, chgW, rateW = 12, 14, 11, 12
	sparkW := max(8, d.w-2-nameW-valW-stW-chgW-rateW-5)
	rg := ranges[d.rng]
	hdr := fmt.Sprintf("  %-*s %*s %-*s %-*s %*s %*s", nameW, "ALERT", valW, "NOW", stW, "  STATE", sparkW, rg.name+" HISTORY", chgW, "CHANGE", rateW, "PER DAY")
	out := []string{d.pal.dim + hdr + d.pal.reset}
	from := now.Add(-rg.d)
	for i, a := range d.cfg.Alerts {
		v := d.view(a, now)
		cursor, rs, re := "  ", "", ""
		if i == d.sel {
			cursor, rs, re = "▶ ", d.pal.bold, d.pal.reset
		}
		val := "—"
		if !math.IsNaN(v.last) {
			val = short(v.last) + a.Unit
		}
		chg, rate := "", ""
		if v.st.N >= 2 {
			chg = signed(v.st.Last.V-v.st.First.V) + a.Unit
			if v.st.Fit != nil && v.st.R2 >= minR2 {
				rate = signed(v.st.SlopePerDay) + a.Unit
			}
		}
		sc := d.pal.state[v.state]
		line := fmt.Sprintf("%s%s%-*s%s %*s %s%-*s%s %s %*s %*s",
			cursor, rs, nameW, clip(a.Name, nameW), re,
			valW, clip(val, valW),
			sc, stW, "  "+v.state.String(), d.pal.reset,
			sparkline(v.pts, from, now, sparkW),
			chgW, chg, rateW, rate)
		out = append(out, line)
	}
	return out
}

func (d *dash) detailView(now time.Time) []string {
	a := d.cfg.Alerts[d.sel]
	v := d.view(a, now)
	rg := ranges[d.rng]
	cur := "—"
	if !math.IsNaN(v.last) {
		cur = short(v.last) + a.Unit
	}
	title := fmt.Sprintf("%s%s%s  %s  %s%s%s   %scheck every %s%s",
		d.pal.bold, a.Name, d.pal.reset, cur, d.pal.state[v.state], v.state, d.pal.reset, d.pal.dim, a.Every, d.pal.reset)
	out := []string{title, ""}
	chartH := max(4, d.h-len(out)-7)
	out = append(out, chart(a, v.pts, now.Add(-rg.d), now, d.w, chartH, d.pal)...)
	out = append(out, "")
	out = append(out, statLines(a, v.st, now, rg.d, d.pal)...)
	return out
}

// statLines is the summary under a chart.
func statLines(a *Alert, s stats, now time.Time, span time.Duration, pal palette) []string {
	if s.N == 0 {
		if s.Errors > 0 {
			return []string{fmt.Sprintf("%s%d failed checks, no readings%s", pal.state[Error], s.Errors, pal.reset)}
		}
		return nil
	}
	u := a.Unit
	l1 := fmt.Sprintf("min %s%s   avg %s%s   max %s%s   %d readings", short(s.Min), u, short(s.Avg), u, short(s.Max), u, s.N)
	if s.Errors > 0 {
		l1 += fmt.Sprintf("   %s%d failed%s", pal.state[Error], s.Errors, pal.reset)
	}
	out := []string{l1}
	if s.N >= 2 {
		chg := s.Last.V - s.First.V
		l2 := "change " + signed(chg) + u
		if s.First.V != 0 {
			l2 += fmt.Sprintf(" (%s%%)", signed(chg/math.Abs(s.First.V)*100))
		}
		l2 += " since " + s.First.time().Format("Jan 2 15:04")
		if s.Fit != nil {
			l2 += "   trend " + signed(s.SlopePerDay) + u + "/day"
			if s.R2 < minR2 {
				l2 += " (no clear trend)"
			}
		}
		out = append(out, l2)
	}
	if e := eta(a, s, now, span); e != "" {
		out = append(out, pal.state[HighWarning]+e+pal.reset)
	}
	return out
}

func signed(v float64) string {
	if v > 0 {
		return "+" + short(v)
	}
	return short(v)
}

// runGraph prints one alert's chart and stats to stdout, for scripts and quick looks.
func runGraph(cfg *Config, state, name string, span time.Duration, height int) error {
	var a *Alert
	for _, x := range cfg.Alerts {
		if x.Name == name {
			a = x
		}
	}
	if a == nil {
		return fmt.Errorf("no alert named %q", name)
	}
	out := int(os.Stdout.Fd())
	w := 100
	if tw, _, err := term.GetSize(out); err == nil && tw > 20 {
		w = tw
	}
	pal := newPalette(term.IsTerminal(out) && os.Getenv("NO_COLOR") == "")
	now := time.Now()
	pts := newSeriesReader(filepath.Join(state, "series"), name).read(now.Add(-span), now)
	s := summarize(pts)
	cur := "no readings"
	if s.N > 0 {
		cur = short(s.Last.V) + a.Unit + "  " + a.stateFor(s.Last.V, Unknown).String()
	}
	fmt.Printf("%s%s%s  %s  (last %s)\n\n", pal.bold, a.Name, pal.reset, cur, humanDur(span))
	for _, l := range chart(a, pts, now.Add(-span), now, w, height, pal) {
		fmt.Println(l)
	}
	fmt.Println()
	for _, l := range statLines(a, s, now, span, pal) {
		fmt.Println(l)
	}
	return nil
}

// visLen is the on-screen width of s, ignoring ANSI escapes (one cell per rune).
func visLen(s string) int {
	n, esc := 0, false
	for _, r := range s {
		switch {
		case esc:
			if r >= '@' && r <= '~' && r != '[' {
				esc = false
			}
		case r == 0x1b:
			esc = true
		default:
			n++
		}
	}
	return n
}

// fit cuts s to w visible cells, keeping escapes intact.
func fit(s string, w int) string {
	if visLen(s) <= w {
		return s
	}
	var b strings.Builder
	n, esc := 0, false
	for _, r := range s {
		switch {
		case esc:
			b.WriteRune(r)
			if r >= '@' && r <= '~' && r != '[' {
				esc = false
			}
		case r == 0x1b:
			esc = true
			b.WriteRune(r)
		case n < w:
			b.WriteRune(r)
			n++
		}
	}
	return b.String() + "\x1b[0m"
}

func clip(s string, w int) string {
	if utf8.RuneCountInString(s) <= w {
		return s
	}
	r := []rune(s)
	return string(r[:w-1]) + "…"
}
