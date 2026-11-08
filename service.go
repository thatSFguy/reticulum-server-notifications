package main

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"math"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// service runs one goroutine per alert and hands their messages to the outbox.
type service struct {
	cfg       *Config
	out       *outbox
	stateFile string
	series    string

	mu       sync.Mutex
	trackers map[string]*tracker
}

func runService(ctx context.Context, cfg *Config, state string) error {
	// 0750: members of the service's group may read alert states and history (the
	// dashboard); the identity and the message queue stay 0600/0700.
	if err := os.MkdirAll(state, 0o750); err != nil {
		return err
	}
	out, err := newOutbox(cfg, state)
	if err != nil {
		return err
	}
	s := &service{cfg: cfg, out: out, stateFile: filepath.Join(state, "alerts.json"), series: filepath.Join(state, "series")}
	s.load()
	go func() {
		for {
			pruneSeries(s.series, cfg.Retain, time.Now())
			select {
			case <-ctx.Done():
				return
			case <-time.After(24 * time.Hour):
			}
		}
	}()

	var wg sync.WaitGroup
	for i, a := range cfg.Alerts {
		wg.Add(1)
		go func() {
			defer wg.Done()
			s.watch(ctx, a, offsetFunc(cfg.StaggerMode, a.Stagger, i, len(cfg.Alerts)))
		}()
	}
	log.Printf("rnsalert: watching %d alert(s)", len(cfg.Alerts))
	err = out.run(ctx, filepath.Join(state, "identity"))
	wg.Wait()
	return err
}

// watch checks a on its schedule until ctx ends.
func (s *service) watch(ctx context.Context, a *Alert, offset func() time.Duration) {
	var window time.Time
	for {
		var at time.Time
		at, window = nextRun(time.Now(), window, a.Every, offset)
		select {
		case <-ctx.Done():
			return
		case <-time.After(time.Until(at)):
		}
		s.check(ctx, a)
	}
}

func (s *service) check(ctx context.Context, a *Alert) {
	r := runCommand(ctx, a)
	if ctx.Err() != nil {
		return // shutting down: a killed command isn't a reading
	}
	now := time.Now()
	v := r.Value
	if r.Err != nil {
		v = math.NaN() // a gap in the graph
	}
	if err := appendPoint(s.series, a.Name, now, v); err != nil {
		log.Printf("%s: history: %v", a.Name, err)
	}
	s.mu.Lock()
	t := s.trackers[a.Name]
	if t == nil {
		t = &tracker{}
		s.trackers[a.Name] = t
	}
	before := *t
	ev := a.step(t, r, now)
	changed := *t != before
	s.mu.Unlock()

	if r.Err != nil {
		log.Printf("%s: %v", a.Name, r.Err)
	}
	if ev != nil {
		title, body := a.render(ev)
		log.Printf("%s: %s → %s (%s)", a.Name, ev.Prev, ev.State, ev.Value)
		if err := s.out.enqueue(a.To, title, body); err != nil {
			log.Printf("%s: queue: %v", a.Name, err)
		}
	}
	if changed {
		s.save()
	}
}

// State file: each alert's last state, so a restart doesn't re-announce everything.
// Alerts removed from the config are dropped from it.
func (s *service) load() {
	s.trackers = map[string]*tracker{}
	data, err := os.ReadFile(s.stateFile)
	if errors.Is(err, os.ErrNotExist) {
		return
	}
	var saved map[string]*tracker
	if err != nil || json.Unmarshal(data, &saved) != nil {
		log.Printf("state: unreadable %s, starting fresh", s.stateFile)
		return
	}
	for _, a := range s.cfg.Alerts {
		if t := saved[a.Name]; t != nil {
			s.trackers[a.Name] = t
		}
	}
}

func (s *service) save() {
	s.mu.Lock()
	data := make(map[string]tracker, len(s.trackers))
	for k, t := range s.trackers {
		data[k] = *t
	}
	err := writeJSONMode(s.stateFile, data, 0o640)
	s.mu.Unlock()
	if err != nil {
		log.Printf("state: %v", err)
	}
}
