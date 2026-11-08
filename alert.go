package main

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// State is where an alert's last reading put it. The five value states run from
// CriticalLow to CriticalHigh; Error means the command failed or printed no number;
// Unknown means not checked yet.
type State int

const (
	Unknown State = iota
	CriticalLow
	LowWarning
	Safe
	HighWarning
	CriticalHigh
	Error
)

var allStates = []State{CriticalLow, LowWarning, Safe, HighWarning, CriticalHigh, Error}

var stateNames = map[State]string{
	CriticalLow: "CRITICAL LOW", LowWarning: "LOW WARNING", Safe: "SAFE",
	HighWarning: "HIGH WARNING", CriticalHigh: "CRITICAL HIGH", Error: "ERROR",
}

var stateKeys = map[State]string{
	CriticalLow: "critical_low", LowWarning: "low_warning", Safe: "safe",
	HighWarning: "high_warning", CriticalHigh: "critical_high", Error: "error",
}

var stateByKey = func() map[string]State {
	m := map[string]State{}
	for s, k := range stateKeys {
		m[k] = s
	}
	return m
}()

func (s State) String() string { return stateNames[s] }

// Saved by key, so reordering the constants never misreads an old state file.
func (s State) MarshalJSON() ([]byte, error) { return json.Marshal(stateKeys[s]) }

func (s *State) UnmarshalJSON(b []byte) error {
	var k string
	if err := json.Unmarshal(b, &k); err != nil {
		return err
	}
	*s = stateByKey[k] // unknown key → Unknown
	return nil
}

// level places the value states on a line: -2 … +2, SAFE at 0.
func (s State) level() int {
	switch s {
	case CriticalLow:
		return -2
	case LowWarning:
		return -1
	case HighWarning:
		return 1
	case CriticalHigh:
		return 2
	}
	return 0
}

func (s State) isValue() bool { return s >= CriticalLow && s <= CriticalHigh }

// classify maps a value to a state. Thresholds are inclusive: with high_warning = 80,
// a reading of 80 is HIGH WARNING. lowAdj/highAdj shift the thresholds toward SAFE
// (used for hysteresis).
func classify(v float64, th [4]threshold, lowAdj, highAdj float64) State {
	switch {
	case th[thCritHigh].Set && v >= th[thCritHigh].V-highAdj:
		return CriticalHigh
	case th[thHighWarn].Set && v >= th[thHighWarn].V-highAdj:
		return HighWarning
	case th[thCritLow].Set && v <= th[thCritLow].V+lowAdj:
		return CriticalLow
	case th[thLowWarn].Set && v <= th[thLowWarn].V+lowAdj:
		return LowWarning
	}
	return Safe
}

// stateFor classifies v given the current state. Getting worse uses the plain
// thresholds; getting better on the same side needs the value to clear the threshold
// by Hysteresis, so a reading wobbling around a limit doesn't flap.
func (a *Alert) stateFor(v float64, cur State) State {
	raw := classify(v, a.Th, 0, 0)
	if a.Hysteresis == 0 || !cur.isValue() {
		return raw
	}
	c, r := cur.level(), raw.level()
	switch {
	case c > 0 && r < c:
		adj := classify(v, a.Th, 0, a.Hysteresis)
		if adj.level() > c {
			return cur
		}
		if adj.level() > 0 || raw.level() == 0 {
			return adj
		}
		return raw // crossed all the way to the low side
	case c < 0 && r > c:
		adj := classify(v, a.Th, a.Hysteresis, 0)
		if adj.level() < c {
			return cur
		}
		if adj.level() < 0 || raw.level() == 0 {
			return adj
		}
		return raw
	}
	return raw
}

// tracker is an alert's remembered state, persisted across restarts.
type tracker struct {
	State    State     `json:"state"`
	Since    time.Time `json:"since"`
	Pending  State     `json:"pending,omitempty"`
	PendingN int       `json:"pending_n,omitempty"`
	LastSent time.Time `json:"last_sent,omitempty"`
}

// reading is one run of an alert's command.
type reading struct {
	Value  float64
	Output string
	Err    error
}

// step folds a reading into t. It returns the event to send, or nil.
func (a *Alert) step(t *tracker, r reading, now time.Time) *Event {
	cand := Error
	if r.Err == nil {
		cand = a.stateFor(r.Value, t.State)
	}
	if cand == t.State {
		t.PendingN = 0
		if a.Repeat > 0 && cand != Safe && a.Notify[cand] && now.Sub(t.LastSent) >= a.Repeat {
			t.LastSent = now
			ev := a.event(r, cand, cand, now)
			ev.Repeat = true
			return ev
		}
		return nil
	}
	if cand == t.Pending && t.PendingN > 0 {
		t.PendingN++
	} else {
		t.Pending, t.PendingN = cand, 1
	}
	if t.PendingN < a.Confirm {
		return nil
	}
	prev := t.State
	t.State, t.Since, t.Pending, t.PendingN = cand, now, Unknown, 0
	if prev == Unknown && cand == Safe {
		return nil // first reading after install: nothing to say
	}
	if !a.Notify[cand] {
		return nil
	}
	t.LastSent = now
	return a.event(r, cand, prev, now)
}

// Event is what message and title templates see.
type Event struct {
	Name      string    // alert name
	State     string    // e.g. "HIGH WARNING"
	Prev      string    // previous state, "" on the first reading
	Value     string    // the reading, as a plain number ("" on ERROR)
	Unit      string    // the alert's unit
	Threshold string    // the limit this state is defined by ("" for SAFE and ERROR)
	Output    string    // the command's output, trimmed
	Error     string    // why the check failed (ERROR only)
	Host      string    // this machine's hostname
	Time      time.Time // when the reading was taken
	Repeat    bool      // a reminder, not a change
}

var sampleEvent = Event{Name: "x", State: "SAFE", Prev: "SAFE", Value: "1", Time: time.Now()}

var hostname = "localhost"

func (a *Alert) event(r reading, s, prev State, now time.Time) *Event {
	ev := &Event{
		Name: a.Name, State: s.String(), Prev: prev.String(), Unit: a.Unit,
		Output: r.Output, Host: hostname, Time: now,
	}
	if r.Err != nil {
		ev.Error = r.Err.Error()
	} else {
		ev.Value = formatNum(r.Value)
	}
	idx := map[State]int{CriticalLow: thCritLow, LowWarning: thLowWarn, HighWarning: thHighWarn, CriticalHigh: thCritHigh}
	if i, ok := idx[s]; ok && a.Th[i].Set {
		ev.Threshold = formatNum(a.Th[i].V)
	}
	return ev
}

// render fills in the title and message for ev, using the per-state templates if set.
func (a *Alert) render(ev *Event) (title, body string) {
	s := stateByName(ev.State)
	tt, mt := a.Title, a.Message
	if t := a.TitleBy[s]; t != nil {
		tt = t
	}
	if t := a.MessageBy[s]; t != nil {
		mt = t
	}
	var tb, mb strings.Builder
	if err := tt.Execute(&tb, ev); err != nil {
		tb.Reset()
		fmt.Fprintf(&tb, "%s: %s", ev.Name, ev.State)
	}
	if err := mt.Execute(&mb, ev); err != nil {
		mb.Reset()
		fmt.Fprintf(&mb, "%s %s (message template failed: %v)", ev.Value, ev.Unit, err)
	}
	if ev.Repeat {
		tb.WriteString(" (still)")
	}
	return tb.String(), mb.String()
}

func stateByName(n string) State {
	for s, name := range stateNames {
		if name == n {
			return s
		}
	}
	return Unknown
}

func formatNum(v float64) string { return strconv.FormatFloat(v, 'f', -1, 64) }
