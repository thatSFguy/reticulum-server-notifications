package main

import (
	"fmt"
	"math"
	"strings"
	"time"
)

// Rendering for the dashboard: sparklines, braille line charts, stats and trends.
// Pure functions of the data, so they're easy to test and reuse for "rnsalert graph".

// ANSI colours, keyed by state. Empty strings when colour is off.
type palette struct {
	reset, dim, bold, inv string
	state                 map[State]string
}

func newPalette(on bool) palette {
	if !on {
		return palette{state: map[State]string{}}
	}
	return palette{
		reset: "\x1b[0m", dim: "\x1b[2m", bold: "\x1b[1m", inv: "\x1b[7m",
		state: map[State]string{
			CriticalLow: "\x1b[1;31m", CriticalHigh: "\x1b[1;31m",
			LowWarning: "\x1b[33m", HighWarning: "\x1b[33m",
			Safe: "\x1b[32m", Error: "\x1b[35m",
		},
	}
}

// stats summarizes the finite points of a series.
type stats struct {
	N             int
	Min, Max, Avg float64
	First, Last   point
	SlopePerDay   float64 // least-squares fit
	Fit           func(t int64) float64
	R2            float64 // how well the line fits: 1 = straight line, ~0 = no trend
	Errors        int     // NaN readings (failed checks)
}

func summarize(pts []point) stats {
	var s stats
	var sumT, sumV, sumTT, sumTV, sumVV float64
	var t0 int64
	for _, p := range pts {
		if math.IsNaN(p.V) {
			s.Errors++
			continue
		}
		if s.N == 0 {
			s.Min, s.Max, s.First, t0 = p.V, p.V, p, p.T
		}
		s.N++
		s.Min, s.Max, s.Last = math.Min(s.Min, p.V), math.Max(s.Max, p.V), p
		t := float64(p.T-t0) / 1000 // seconds since first, for precision
		sumT += t
		sumV += p.V
		sumTT += t * t
		sumTV += t * p.V
		sumVV += p.V * p.V
	}
	if s.N == 0 {
		return s
	}
	n := float64(s.N)
	s.Avg = sumV / n
	if d := n*sumTT - sumT*sumT; s.N >= 2 && d > 0 {
		slope := (n*sumTV - sumT*sumV) / d // per second
		icpt := (sumV - slope*sumT) / n
		s.SlopePerDay = slope * 86400
		s.Fit = func(t int64) float64 { return icpt + slope*float64(t-t0)/1000 }
		if dv := n*sumVV - sumV*sumV; dv > 0 {
			r := (n*sumTV - sumT*sumV) / math.Sqrt(d*dv)
			s.R2 = r * r
		}
	}
	return s
}

// minR2 is how straight the history must be before eta projects it: a steady climb
// (disk filling) fits well; a daily cycle (temperature) doesn't, and projecting it
// would be nonsense.
const minR2 = 0.6

// eta projects the trend to the next threshold in the direction it's moving. It returns
// "" when there is no clear trend, no threshold that way, or it's too far off to mean much.
func eta(a *Alert, s stats, now time.Time, span time.Duration) string {
	if s.Fit == nil || s.SlopePerDay == 0 || s.R2 < minR2 {
		return ""
	}
	cur := s.Fit(now.UnixMilli())
	order := []int{thHighWarn, thCritHigh}
	if s.SlopePerDay < 0 {
		order = []int{thLowWarn, thCritLow}
	}
	for _, i := range order {
		th := a.Th[i]
		if !th.Set || (s.SlopePerDay > 0 && cur >= th.V) || (s.SlopePerDay < 0 && cur <= th.V) {
			continue
		}
		days := (th.V - cur) / s.SlopePerDay
		d := time.Duration(days * 24 * float64(time.Hour))
		if d <= 0 || d > 10*span || d > 5*365*24*time.Hour {
			return ""
		}
		name := map[int]State{thHighWarn: HighWarning, thCritHigh: CriticalHigh, thLowWarn: LowWarning, thCritLow: CriticalLow}[i]
		return fmt.Sprintf("at this rate %s (%s%s) in ~%s", name, formatNum(th.V), a.Unit, humanDur(d))
	}
	return ""
}

func humanDur(d time.Duration) string {
	switch {
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()+0.5))
	case d < 48*time.Hour:
		return fmt.Sprintf("%.0fh", d.Hours())
	case d < 120*24*time.Hour:
		return fmt.Sprintf("%.0fd", d.Hours()/24)
	}
	return fmt.Sprintf("%.1fy", d.Hours()/24/365)
}

// short formats a value for axes and tables: at most ~6 significant characters.
func short(v float64) string {
	if math.IsNaN(v) {
		return "—"
	}
	a := math.Abs(v)
	switch {
	case a == 0:
		return "0"
	case a >= 1e9:
		return trimZeros(fmt.Sprintf("%.2f", v/1e9)) + "G"
	case a >= 1e6:
		return trimZeros(fmt.Sprintf("%.2f", v/1e6)) + "M"
	case a >= 1e4:
		return trimZeros(fmt.Sprintf("%.1f", v/1e3)) + "k"
	case a >= 100:
		return fmt.Sprintf("%.0f", v)
	case a >= 1:
		return trimZeros(fmt.Sprintf("%.2f", v))
	}
	return trimZeros(fmt.Sprintf("%.3g", v))
}

func trimZeros(s string) string {
	if strings.Contains(s, ".") {
		s = strings.TrimRight(strings.TrimRight(s, "0"), ".")
	}
	return s
}

// bucket splits [from, to] into n columns and returns each column's min, max and last
// finite value (NaN where the column holds no reading).
func bucket(pts []point, from, to time.Time, n int) (lo, hi, last []float64) {
	lo, hi, last = make([]float64, n), make([]float64, n), make([]float64, n)
	for i := range n {
		lo[i], hi[i], last[i] = math.NaN(), math.NaN(), math.NaN()
	}
	f, span := from.UnixMilli(), to.UnixMilli()-from.UnixMilli()
	if span <= 0 || n == 0 {
		return
	}
	for _, p := range pts {
		if math.IsNaN(p.V) {
			continue
		}
		i := int((p.T - f) * int64(n) / span)
		if i < 0 || i >= n {
			if i == n {
				i = n - 1
			} else {
				continue
			}
		}
		if math.IsNaN(lo[i]) || p.V < lo[i] {
			lo[i] = p.V
		}
		if math.IsNaN(hi[i]) || p.V > hi[i] {
			hi[i] = p.V
		}
		last[i] = p.V
	}
	return
}

// sparkline draws pts as one row of block characters, width cells wide.
func sparkline(pts []point, from, to time.Time, width int) string {
	const blocks = "▁▂▃▄▅▆▇█"
	_, hi, _ := bucket(pts, from, to, width)
	min, max := math.Inf(1), math.Inf(-1)
	for _, v := range hi {
		if !math.IsNaN(v) {
			min, max = math.Min(min, v), math.Max(max, v)
		}
	}
	var b strings.Builder
	runes := []rune(blocks)
	for _, v := range hi {
		switch {
		case math.IsNaN(v):
			b.WriteByte(' ')
		case max == min:
			b.WriteRune(runes[3])
		default:
			b.WriteRune(runes[int((v-min)/(max-min)*float64(len(runes)-1)+0.5)])
		}
	}
	return b.String()
}

// chart draws a braille line chart of pts, width×height cells including a y-axis on
// the left and a time axis below. Threshold lines are drawn in their state's colour
// where the line doesn't cover them.
func chart(a *Alert, pts []point, from, to time.Time, width, height int, pal palette) []string {
	const axisW = 8
	cw, ch := width-axisW, height-1 // plot area in cells
	if cw < 4 || ch < 2 {
		return nil
	}
	pw, ph := cw*2, ch*4 // in braille dots
	lo, hi, last := bucket(pts, from, to, pw)

	// Y range: the data, plus any threshold near it, padded a little.
	ymin, ymax := math.Inf(1), math.Inf(-1)
	for i := range lo {
		if !math.IsNaN(lo[i]) {
			ymin, ymax = math.Min(ymin, lo[i]), math.Max(ymax, hi[i])
		}
	}
	if math.IsInf(ymin, 0) {
		return []string{strings.Repeat(" ", axisW) + pal.dim + "no data in this range" + pal.reset}
	}
	span := ymax - ymin
	if span == 0 {
		span = math.Max(math.Abs(ymax)*0.1, 1)
	}
	for _, th := range a.Th {
		if th.Set && th.V >= ymin-span && th.V <= ymax+span {
			ymin, ymax = math.Min(ymin, th.V), math.Max(ymax, th.V)
		}
	}
	pad := (ymax - ymin) * 0.05
	if pad == 0 {
		pad = math.Max(math.Abs(ymax)*0.05, 0.5)
	}
	ymin, ymax = ymin-pad, ymax+pad
	toDot := func(v float64) int { // 0 = top
		y := int((ymax - v) / (ymax - ymin) * float64(ph-1))
		return max(0, min(ph-1, y))
	}

	dots := make([][]bool, ph)
	for i := range dots {
		dots[i] = make([]bool, pw)
	}
	// Join the line across empty columns when they're just the spacing between readings
	// (a short range has several columns per check); longer than ~2.5 checks is a gap.
	colDur := float64(to.Sub(from)) / float64(pw)
	maxGap := max(1, int(2.5*float64(a.Every)/colDur))
	prev, prevX := math.NaN(), -1
	for x := range pw {
		if math.IsNaN(lo[x]) {
			continue
		}
		top, bot := toDot(hi[x]), toDot(lo[x])
		if !math.IsNaN(prev) && x-prevX <= maxGap {
			// Straight line from the previous reading to this column.
			py, cy := toDot(prev), toDot((lo[x]+hi[x])/2)
			for gx := prevX + 1; gx < x; gx++ {
				y := py + (cy-py)*(gx-prevX)/(x-prevX)
				dots[y][gx] = true
			}
			if x-prevX == 1 {
				top, bot = min(top, py), max(bot, py)
			}
		}
		for y := top; y <= bot; y++ {
			dots[y][x] = true
		}
		prev, prevX = last[x], x
	}

	// Which cell row each threshold sits on.
	thRow := map[int]State{}
	for i, s := range []State{CriticalLow, LowWarning, HighWarning, CriticalHigh} {
		if th := a.Th[i]; th.Set && th.V >= ymin && th.V <= ymax {
			thRow[toDot(th.V)/4] = s
		}
	}

	// Braille: dot (dx,dy) in a 2×4 cell maps to these bits.
	bits := [4][2]rune{{0x01, 0x08}, {0x02, 0x10}, {0x04, 0x20}, {0x40, 0x80}}
	var out []string
	for r := range ch {
		var b strings.Builder
		// Y-axis label on the top, middle and bottom rows, and on threshold rows.
		label := ""
		v := ymax - (float64(r)*4+1.5)/float64(ph-1)*(ymax-ymin)
		if r == 0 || r == ch-1 || r == ch/2 {
			label = short(v)
		}
		s, isTh := thRow[r]
		if isTh {
			label = short(a.Th[map[State]int{CriticalLow: thCritLow, LowWarning: thLowWarn, HighWarning: thHighWarn, CriticalHigh: thCritHigh}[s]].V)
		}
		lc := pal.dim
		if isTh {
			lc = pal.state[s]
		}
		fmt.Fprintf(&b, "%s%*s%s %s┤%s", lc, axisW-2, label, pal.reset, pal.dim, pal.reset)
		for c := range cw {
			var cell rune
			for dy := range 4 {
				for dx := range 2 {
					if dots[r*4+dy][c*2+dx] {
						cell |= bits[dy][dx]
					}
				}
			}
			switch {
			case cell != 0:
				b.WriteRune(0x2800 + cell)
			case isTh:
				b.WriteString(pal.state[s] + "┄" + pal.reset)
			default:
				b.WriteByte(' ')
			}
		}
		out = append(out, b.String())
	}
	out = append(out, timeAxis(from, to, axisW, cw, pal))
	return out
}

// timeAxis labels the start, middle and end of the range.
func timeAxis(from, to time.Time, indent, width int, pal palette) string {
	layout := "15:04"
	switch d := to.Sub(from); {
	case d > 300*24*time.Hour:
		layout = "Jan 2006"
	case d > 2*24*time.Hour:
		layout = "Jan 2"
	case d > 20*time.Hour:
		layout = "Mon 15:04"
	}
	l, m, r := from.Format(layout), from.Add(to.Sub(from)/2).Format(layout), to.Format(layout)
	line := []rune(strings.Repeat(" ", width))
	put := func(at int, s string) {
		rs := []rune(s)
		at = max(0, min(at, width-len(rs)))
		copy(line[at:], rs)
	}
	put(0, l)
	put(width/2-len(m)/2, m)
	put(width-len(r), r)
	return pal.dim + strings.Repeat(" ", indent) + string(line) + pal.reset
}
