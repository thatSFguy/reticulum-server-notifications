package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"time"
	"unicode"

	"github.com/thatSFguy/reticulum-go/lxmf"
	"github.com/thatSFguy/reticulum-go/rns"
)

// Delivery: dial the node, announce our LXMF address, and deliver queued messages.
//
// Every message becomes one file per recipient under <state>/queue, so a restart or a
// phone that's off the mesh loses nothing. Delivery is retried with backoff; after a few
// failed direct tries it goes to the propagation node (if configured) to wait for the
// phone. Items still undelivered after maxAge move to <state>/failed.

const (
	pollEvery     = 10 * time.Second
	announceEvery = 30 * time.Minute
	// Re-announce before a send if our last announce is older than this: a phone is only
	// on the mesh while its app is connected, so it may have missed the periodic ones, and
	// without our key it can't verify the message.
	announceFresh = 10 * time.Minute
	directTries   = 3
	maxBackoff    = 30 * time.Minute
	maxAge        = 48 * time.Hour
	maxTitle      = 80
	maxBody       = 2000
)

type item struct {
	To       string    `json:"to"` // hex address
	Title    string    `json:"title"`
	Body     string    `json:"body"`
	Created  time.Time `json:"created"`
	Next     time.Time `json:"next"`
	Attempts int       `json:"attempts"`
}

// backoff is the wait after the n-th failed attempt: 30s, 1m, 2m … capped at 30m.
func backoff(n int) time.Duration {
	d := 30 * time.Second
	for i := 1; i < n && d < maxBackoff; i++ {
		d *= 2
	}
	return min(d, maxBackoff)
}

type outbox struct {
	cfg      *Config
	queueDir string
	failDir  string
	knownF   string
	kick     chan struct{}
	t        *rns.Transport
	d        *lxmf.Delivery
	announce func()
	lastAnn  atomic.Int64 // unix seconds of our last announce
}

func newOutbox(cfg *Config, state string) (*outbox, error) {
	o := &outbox{
		cfg:      cfg,
		queueDir: filepath.Join(state, "queue"),
		failDir:  filepath.Join(state, "failed"),
		knownF:   filepath.Join(state, "known.json"),
		kick:     make(chan struct{}, 1),
	}
	for _, d := range []string{o.queueDir, o.failDir} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			return nil, err
		}
	}
	return o, nil
}

// enqueue queues one message to each recipient and wakes the sender.
func (o *outbox) enqueue(to [][]byte, title, body string) error {
	now := time.Now()
	title, body = clean(title, maxTitle), clean(body, maxBody)
	var errs []error
	for _, r := range to {
		it := item{To: hex.EncodeToString(r), Title: title, Body: body, Created: now}
		name := fmt.Sprintf("%d-%s-%s.json", now.UnixNano(), it.To[:8], randHex(4))
		errs = append(errs, writeJSON(filepath.Join(o.queueDir, name), it))
	}
	select {
	case o.kick <- struct{}{}:
	default:
	}
	return errors.Join(errs...)
}

// run connects to the node and delivers until ctx ends.
func (o *outbox) run(ctx context.Context, identityPath string) error {
	id, err := loadIdentity(identityPath)
	if err != nil {
		return err
	}
	logger := quietLogger{}
	o.t = rns.NewTransport(logger)
	o.t.SetInitiatorIdentity(id)
	tc, err := rns.DialReconnectingTCP(o.cfg.Node, 15*time.Second, logger)
	if err != nil {
		return err
	}
	defer tc.Close()
	o.t.AddInterface(tc)

	build := func(c byte) (*rns.Packet, error) {
		var name []byte // nil: announce no display name
		if o.cfg.Name != "" {
			name = []byte(o.cfg.Name)
		}
		ad, err := rns.EncodeLXMFAppData(name, nil)
		if err != nil {
			return nil, err
		}
		return rns.BuildAnnounceWithContext(id, lxmf.FullName(), ad, nil, c)
	}
	o.d, err = lxmf.NewDelivery(o.t, id, build)
	if err != nil {
		return err
	}
	o.d.OnError = func(err error) { log.Printf("lxmf: %v", err) }
	o.d.OnMessage = func(m *lxmf.Message) {
		log.Printf("message from %x (%d bytes) ignored", m.SourceHash, len(m.Content))
	}

	peers := o.peers()
	o.t.PinDestinations(peers)
	o.restoreKnown()

	done := make(chan struct{})
	go func() { o.t.Run(ctx); close(done) }()
	go o.t.RunLinkSweeper(ctx)
	o.announce = func() {
		p, err := build(rns.ContextNone)
		if err == nil {
			err = o.t.Broadcast(p)
		}
		if err != nil {
			log.Printf("announce: %v", err)
			return
		}
		o.lastAnn.Store(time.Now().Unix())
	}
	go o.t.AnnouncePeriodically(ctx, announceEvery, func() (*rns.Packet, error) {
		o.lastAnn.Store(time.Now().Unix())
		return build(rns.ContextNone)
	})
	log.Printf("rnsalert: LXMF address <%x> via %s", o.d.Hash(), o.cfg.Node)
	for _, p := range peers {
		if o.t.Recall(p) == nil {
			o.t.RequestPath(p)
		}
	}

	tick := time.NewTicker(pollEvery)
	defer tick.Stop()
	lastSave := time.Now()
	for {
		o.deliver(ctx)
		if time.Since(lastSave) > 10*time.Minute {
			o.saveKnown()
			lastSave = time.Now()
		}
		select {
		case <-ctx.Done():
			o.saveKnown()
			<-done
			return nil
		case <-tick.C:
		case <-o.kick:
		}
	}
}

// peers is every recipient of every alert, plus the propagation node.
func (o *outbox) peers() [][]byte {
	var out [][]byte
	add := func(b []byte) {
		if !slices.ContainsFunc(out, func(x []byte) bool { return string(x) == string(b) }) {
			out = append(out, b)
		}
	}
	for _, b := range o.cfg.To {
		add(b)
	}
	for _, a := range o.cfg.Alerts {
		for _, b := range a.To {
			add(b)
		}
	}
	if o.cfg.Propagation != nil {
		add(o.cfg.Propagation)
	}
	return out
}

// quietLogger drops the library's line for every announce heard (every node on the mesh
// announces, which would flood the journal). Everything else is kept.
type quietLogger struct{}

func (quietLogger) Printf(format string, args ...any) {
	if strings.HasPrefix(format, "announce verified") {
		return
	}
	log.Printf(format, args...)
}

func loadIdentity(path string) (*rns.Identity, error) {
	if _, err := os.Stat(path); err == nil {
		return rns.IdentityFromFile(path)
	}
	id, err := rns.NewIdentity()
	if err != nil {
		return nil, err
	}
	return id, id.Save(path)
}

// deliver tries every queue item that is due, oldest first, one at a time.
func (o *outbox) deliver(ctx context.Context) {
	files, _ := filepath.Glob(filepath.Join(o.queueDir, "*.json"))
	now := time.Now()
	for _, f := range files {
		if ctx.Err() != nil {
			return
		}
		var it item
		if data, err := os.ReadFile(f); err != nil || json.Unmarshal(data, &it) != nil {
			os.Rename(f, filepath.Join(o.failDir, filepath.Base(f)))
			continue
		}
		if now.Before(it.Next) {
			continue
		}
		to, err := parseHash(it.To)
		if err != nil {
			os.Rename(f, filepath.Join(o.failDir, filepath.Base(f)))
			continue
		}
		how, err := o.send(to, it)
		if err == nil {
			log.Printf("delivered %q to %s (%s)", it.Title, it.To, how)
			os.Remove(f)
			continue
		}
		it.Attempts++
		it.Next = time.Now().Add(backoff(it.Attempts))
		if errors.Is(err, lxmf.ErrRecipientUnknown) && it.Attempts < directTries {
			it.Next = time.Time{} // a path answer usually comes within seconds: retry next poll
		}
		if time.Since(it.Created) > maxAge {
			log.Printf("giving up on %q to %s after %d attempts: %v", it.Title, it.To, it.Attempts, err)
			writeJSON(filepath.Join(o.failDir, filepath.Base(f)), it)
			os.Remove(f)
			continue
		}
		log.Printf("not delivered %q to %s (attempt %d, retry in %s): %v", it.Title, it.To, it.Attempts, backoff(it.Attempts), err)
		writeJSON(f, it)
	}
}

// send tries direct delivery, then the propagation node once direct tries run out.
func (o *outbox) send(to []byte, it item) (string, error) {
	title, body := []byte(it.Title), []byte(it.Body)
	if time.Since(time.Unix(o.lastAnn.Load(), 0)) > announceFresh {
		o.announce()
		time.Sleep(2 * time.Second) // let it reach the phone ahead of the message
	}
	if o.t.Recall(to) != nil {
		err := o.d.Send(to, title, body, nil)
		if err == nil || it.Attempts+1 < directTries || o.cfg.Propagation == nil {
			return "direct", err
		}
	} else {
		o.t.RequestPath(to)
		if it.Attempts+1 < directTries || o.cfg.Propagation == nil {
			return "", lxmf.ErrRecipientUnknown
		}
	}
	if o.t.Recall(o.cfg.Propagation) == nil {
		o.t.RequestPath(o.cfg.Propagation)
		return "", lxmf.ErrPropagationNodeUnknown
	}
	if o.t.Recall(to) == nil {
		return "", lxmf.ErrRecipientUnknown // can't encrypt for it, even via the node
	}
	_, err := o.d.SendPropagated(o.cfg.Propagation, to, title, body, nil)
	return "via propagation node", err
}

// The announce cache is in memory only; keep our peers' entries so a restart can
// deliver at once instead of waiting for a path response.
func (o *outbox) saveKnown() {
	peers := o.peers()
	var keep []*rns.KnownIdentity
	for _, k := range o.t.KnownSnapshot() {
		if slices.ContainsFunc(peers, func(b []byte) bool { return string(b) == string(k.DestHash) }) {
			keep = append(keep, k)
		}
	}
	if err := writeJSON(o.knownF, keep); err != nil {
		log.Printf("known: %v", err)
	}
}

func (o *outbox) restoreKnown() {
	data, err := os.ReadFile(o.knownF)
	if errors.Is(err, os.ErrNotExist) {
		return
	}
	var ks []*rns.KnownIdentity
	if err != nil || json.Unmarshal(data, &ks) != nil {
		log.Printf("known: unreadable %s, starting fresh", o.knownF)
		return
	}
	for _, k := range ks {
		o.t.Restore(k)
	}
}

// writeJSON replaces path atomically (0600: it's ours alone).
func writeJSON(path string, v any) error { return writeJSONMode(path, v, 0o600) }

func writeJSONMode(path string, v any, mode os.FileMode) error {
	data, err := json.Marshal(v)
	if err != nil {
		return err
	}
	tmp := filepath.Join(filepath.Dir(path), "."+filepath.Base(path)+".tmp")
	if err := os.WriteFile(tmp, data, mode); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// clean keeps printable text and newlines, trimmed to max bytes on a rune boundary.
func clean(s string, max int) string {
	s = strings.Map(func(r rune) rune {
		if r == '\n' || r == '\t' || unicode.IsPrint(r) {
			return r
		}
		return -1
	}, s)
	s = strings.TrimSpace(s)
	if len(s) > max {
		cut := max - len("…")
		for cut > 0 && s[cut]&0xC0 == 0x80 {
			cut--
		}
		s = s[:cut] + "…"
	}
	return s
}

func randHex(n int) string {
	b := make([]byte, n)
	rand.Read(b)
	return hex.EncodeToString(b)
}
