package main

import (
	"bufio"
	"encoding/hex"
	"fmt"
	"os"
	"strconv"
	"strings"
	"text/template"
	"time"
)

// Config is the whole config file: global settings, then one [alert NAME] section per alert.
//
// The format is "key = value" lines with # comments, so it needs no third-party parser.
// See rnsalert.conf.example for every key.
type Config struct {
	Node        string   // host:port of the Reticulum TCP node to dial out to
	To          [][]byte // default LXMF recipients
	Propagation []byte   // optional LXMF propagation node for offline recipients
	Name        string   // optional display name to announce

	Stagger     time.Duration // default: checks start within this long after their window begins
	StaggerMode string        // "random" (fresh offset each window) or "spread" (fixed, evenly spaced)
	Timeout     time.Duration // default command timeout

	Alerts []*Alert
}

// Alert is one [alert NAME] section.
type Alert struct {
	Name    string
	Command string        // run with /bin/sh -c; prints the value
	Every   time.Duration // how often to check
	Stagger time.Duration
	Timeout time.Duration
	To      [][]byte
	Unit    string

	// Thresholds, indexed by thCritLow … thCritHigh. Any may be unset.
	Th         [4]threshold
	Hysteresis float64
	Confirm    int           // consecutive readings needed before a state change counts
	Repeat     time.Duration // re-send while not SAFE; 0 = only on change
	Notify     map[State]bool

	Title, Message     *template.Template
	TitleBy, MessageBy map[State]*template.Template
}

type threshold struct {
	V   float64
	Set bool
}

const (
	thCritLow = iota
	thLowWarn
	thHighWarn
	thCritHigh
)

const (
	defaultTitle   = `{{.Name}}: {{.State}}`
	defaultMessage = `{{if .Error}}check failed: {{.Error}}{{else}}{{.Name}} is {{.Value}}{{.Unit}}{{if .Threshold}} (limit {{.Threshold}}{{.Unit}}){{end}}{{end}}{{if .Prev}}, was {{.Prev}}{{end}}`
)

var thresholdKeys = map[string]int{
	"critical_low": thCritLow, "low_warning": thLowWarn,
	"high_warning": thHighWarn, "critical_high": thCritHigh,
}

func loadConfig(path string) (*Config, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	// The file holds shell commands that the service runs; refuse one others could edit.
	if fi, err := f.Stat(); err == nil && fi.Mode().Perm()&0o022 != 0 {
		return nil, fmt.Errorf("%s is group- or world-writable (it holds commands); chmod go-w it", path)
	}
	c := &Config{Stagger: 15 * time.Second, StaggerMode: "random", Timeout: 30 * time.Second}
	var cur *Alert
	var raw []map[string]string // per-alert keys, applied once globals are known
	var lines []map[string]int
	seen := map[string]bool{}

	sc := bufio.NewScanner(f)
	for n := 1; sc.Scan(); n++ {
		line := strings.TrimSpace(stripComment(sc.Text()))
		if line == "" {
			continue
		}
		where := fmt.Sprintf("%s:%d", path, n)
		if strings.HasPrefix(line, "[") {
			name, ok := sectionName(line)
			if !ok {
				return nil, fmt.Errorf("%s: want [alert NAME]", where)
			}
			if seen[name] {
				return nil, fmt.Errorf("%s: duplicate alert %q", where, name)
			}
			seen[name] = true
			cur = &Alert{Name: name}
			c.Alerts = append(c.Alerts, cur)
			raw = append(raw, map[string]string{})
			lines = append(lines, map[string]int{"": n})
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			return nil, fmt.Errorf("%s: want key = value", where)
		}
		k, v = strings.TrimSpace(k), strings.TrimSpace(v)
		if cur != nil {
			if _, dup := raw[len(raw)-1][k]; dup {
				return nil, fmt.Errorf("%s: %s set twice", where, k)
			}
			raw[len(raw)-1][k] = v
			lines[len(lines)-1][k] = n
			continue
		}
		if err := c.setGlobal(k, v); err != nil {
			return nil, fmt.Errorf("%s: %w", where, err)
		}
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	for i, a := range c.Alerts {
		if err := c.buildAlert(a, raw[i], lines[i]); err != nil {
			return nil, fmt.Errorf("%s: %w", path, err)
		}
	}
	if len(c.Alerts) == 0 {
		return nil, fmt.Errorf("%s: no [alert NAME] sections", path)
	}
	return c, nil
}

// stripComment drops a # comment, except inside a command or template where "#" is
// often legitimate: there, only " #" (space before) starts a comment, and "\#" is a literal.
func stripComment(s string) string {
	if strings.HasPrefix(strings.TrimSpace(s), "#") {
		return ""
	}
	for i := 0; i < len(s); i++ {
		if s[i] == '#' && i > 0 && (s[i-1] == ' ' || s[i-1] == '\t') {
			return s[:i]
		}
	}
	return strings.ReplaceAll(s, `\#`, "#")
}

func sectionName(line string) (string, bool) {
	if !strings.HasSuffix(line, "]") {
		return "", false
	}
	inner := strings.TrimSpace(line[1 : len(line)-1])
	rest, ok := strings.CutPrefix(inner, "alert")
	if !ok || (rest != "" && rest[0] != ' ' && rest[0] != '\t') {
		return "", false
	}
	name := strings.Trim(strings.TrimSpace(rest), `"`)
	return name, name != ""
}

func (c *Config) setGlobal(k, v string) error {
	var err error
	switch k {
	case "node":
		c.Node = v
	case "to":
		c.To, err = parseHashes(v)
	case "propagation":
		if v != "" {
			c.Propagation, err = parseHash(v)
		}
	case "name":
		c.Name = v
	case "stagger":
		c.Stagger, err = parseDuration(v, true)
	case "stagger_mode":
		if v != "random" && v != "spread" {
			return fmt.Errorf("stagger_mode: want random or spread")
		}
		c.StaggerMode = v
	case "timeout":
		c.Timeout, err = parseDuration(v, false)
	default:
		return fmt.Errorf("unknown global key %q (alert keys go under an [alert NAME] section)", k)
	}
	if err != nil {
		return fmt.Errorf("%s: %w", k, err)
	}
	return nil
}

func (c *Config) buildAlert(a *Alert, kv map[string]string, line map[string]int) error {
	a.Stagger, a.Timeout, a.To, a.Confirm = c.Stagger, c.Timeout, c.To, 1
	a.Notify = map[State]bool{}
	for _, s := range allStates {
		a.Notify[s] = true
	}
	a.TitleBy, a.MessageBy = map[State]*template.Template{}, map[State]*template.Template{}
	title, message := defaultTitle, defaultMessage

	fail := func(k string, err error) error {
		return fmt.Errorf("%d: [alert %s] %s: %w", line[k], a.Name, k, err)
	}
	for k, v := range kv {
		var err error
		switch {
		case k == "command":
			a.Command = v
		case k == "every":
			a.Every, err = parseDuration(v, false)
		case k == "stagger":
			a.Stagger, err = parseDuration(v, true)
		case k == "timeout":
			a.Timeout, err = parseDuration(v, false)
		case k == "to":
			a.To, err = parseHashes(v)
		case k == "unit":
			a.Unit = v
		case k == "hysteresis":
			a.Hysteresis, err = parseNumber(v)
			if err == nil && a.Hysteresis < 0 {
				err = fmt.Errorf("must not be negative")
			}
		case k == "confirm":
			a.Confirm, err = strconv.Atoi(v)
			if err == nil && a.Confirm < 1 {
				err = fmt.Errorf("must be 1 or more")
			}
		case k == "repeat":
			a.Repeat, err = parseDuration(v, true)
		case k == "notify":
			a.Notify, err = parseStates(v)
		case k == "title":
			title = v
		case k == "message":
			message = v
		case strings.HasPrefix(k, "title.") || strings.HasPrefix(k, "message."):
			kind, sk, _ := strings.Cut(k, ".")
			s, ok := stateByKey[sk]
			if !ok {
				err = fmt.Errorf("unknown state %q", sk)
				break
			}
			var t *template.Template
			if t, err = parseTemplate(k, v); err == nil {
				if kind == "title" {
					a.TitleBy[s] = t
				} else {
					a.MessageBy[s] = t
				}
			}
		default:
			i, ok := thresholdKeys[k]
			if !ok {
				err = fmt.Errorf("unknown key")
				break
			}
			if v == "" {
				break
			}
			a.Th[i].V, err = parseNumber(v)
			a.Th[i].Set = err == nil
		}
		if err != nil {
			return fail(k, err)
		}
	}
	var err error
	if a.Title, err = parseTemplate("title", title); err != nil {
		return fail("title", err)
	}
	if a.Message, err = parseTemplate("message", message); err != nil {
		return fail("message", err)
	}
	switch {
	case a.Command == "":
		return fail("", fmt.Errorf("command is required"))
	case a.Every == 0:
		return fail("", fmt.Errorf("every is required"))
	case len(a.To) == 0:
		return fail("", fmt.Errorf("no recipients: set to here or globally"))
	case a.Timeout > a.Every:
		return fail("timeout", fmt.Errorf("longer than every (%s)", a.Every))
	}
	if a.Stagger >= a.Every {
		a.Stagger = a.Every / 2 // still leaves room for the check itself
	}
	any := false
	prev, prevKey := 0.0, ""
	for i, k := range []string{"critical_low", "low_warning", "high_warning", "critical_high"} {
		if !a.Th[i].Set {
			continue
		}
		if any && a.Th[i].V <= prev {
			return fail(k, fmt.Errorf("must be above %s (%g)", prevKey, prev))
		}
		any, prev, prevKey = true, a.Th[i].V, k
	}
	if !any {
		return fail("", fmt.Errorf("set at least one of critical_low, low_warning, high_warning, critical_high"))
	}
	return nil
}

// parseTemplate parses a text/template and dry-runs it, so a typo'd field fails at load,
// not at 3am when the alert finally fires. "\n" in the value is a newline.
func parseTemplate(name, s string) (*template.Template, error) {
	s = strings.ReplaceAll(s, `\n`, "\n")
	t, err := template.New(name).Parse(s)
	if err != nil {
		return nil, err
	}
	if err := t.Execute(discard{}, sampleEvent); err != nil {
		return nil, err
	}
	return t, nil
}

type discard struct{}

func (discard) Write(p []byte) (int, error) { return len(p), nil }

func parseDuration(s string, zeroOK bool) (time.Duration, error) {
	d, err := time.ParseDuration(s)
	if err != nil {
		return 0, err
	}
	if d < 0 || (d == 0 && !zeroOK) {
		return 0, fmt.Errorf("must be positive")
	}
	return d, nil
}

func parseStates(v string) (map[State]bool, error) {
	m := map[State]bool{}
	for _, k := range strings.Split(v, ",") {
		k = strings.TrimSpace(k)
		if k == "" || k == "none" {
			continue
		}
		s, ok := stateByKey[k]
		if !ok {
			return nil, fmt.Errorf("unknown state %q", k)
		}
		m[s] = true
	}
	return m, nil
}

func parseHashes(v string) ([][]byte, error) {
	var out [][]byte
	for _, h := range strings.Split(v, ",") {
		if h = strings.TrimSpace(h); h == "" {
			continue
		}
		b, err := parseHash(h)
		if err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, nil
}

// parseHash accepts an LXMF address as Sideband shows it: 32 hex digits, optionally in <>.
func parseHash(s string) ([]byte, error) {
	s = strings.Trim(s, "<> ")
	b, err := hex.DecodeString(s)
	if err != nil || len(b) != 16 {
		return nil, fmt.Errorf("%q is not a 32-hex-digit LXMF address", s)
	}
	return b, nil
}
