package main

import (
	"context"
	"log"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/thatSFguy/reticulum-go/lxmf"
	"github.com/thatSFguy/reticulum-go/rns"
)

// A "phone" listens on a local TCP port (standing in for the node), the service dials
// it, and an alert whose command reads CRITICAL HIGH must arrive as an LXMF message.
// Local only; no real network.
func TestServiceAlertsPhone(t *testing.T) {
	if testing.Short() {
		t.Skip("end-to-end")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	logger := log.New(os.Stderr, "", 0)

	phoneID, _ := rns.NewIdentity()
	pt := rns.NewTransport(logger)
	srv, err := rns.ListenTCP("127.0.0.1:0", logger)
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()
	pt.AddInterface(srv)
	build := func(c byte) (*rns.Packet, error) {
		ad, err := rns.EncodeLXMFAppData(nil, nil)
		if err != nil {
			return nil, err
		}
		return rns.BuildAnnounceWithContext(phoneID, lxmf.FullName(), ad, nil, c)
	}
	phone, err := lxmf.NewDelivery(pt, phoneID, build)
	if err != nil {
		t.Fatal(err)
	}
	got := make(chan *lxmf.Message, 4)
	phone.OnMessage = func(m *lxmf.Message) { got <- m }
	go pt.Run(ctx)
	go pt.RunLinkSweeper(ctx)

	a := &Alert{
		Name: "Boiler", Command: "echo 97.5", Every: time.Minute, Timeout: 5 * time.Second,
		Stagger: time.Second, To: [][]byte{phone.Hash()}, Unit: "°C", Confirm: 1,
		Notify: map[State]bool{CriticalHigh: true},
	}
	a.Th[thHighWarn] = threshold{V: 80, Set: true}
	a.Th[thCritHigh] = threshold{V: 95, Set: true}
	a.Title, _ = parseTemplate("title", defaultTitle)
	a.Message, _ = parseTemplate("message", defaultMessage)
	cfg := &Config{Node: srv.Addr().String(), StaggerMode: "random", Alerts: []*Alert{a}}

	dctx, dstop := context.WithCancel(ctx)
	errc := make(chan error, 1)
	state := t.TempDir()
	go func() { errc <- runService(dctx, cfg, state) }()
	go pt.AnnouncePeriodically(ctx, 2*time.Second, func() (*rns.Packet, error) { return build(rns.ContextNone) })

	select {
	case m := <-got:
		if string(m.Title) != "Boiler: CRITICAL HIGH" || string(m.Content) != "Boiler is 97.5°C (limit 95°C)" {
			t.Errorf("got %q / %q", m.Title, m.Content)
		}
	case <-ctx.Done():
		t.Fatal("no message within 60s")
	}
	dstop()
	if err := <-errc; err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(state, "alerts.json")); err != nil {
		t.Errorf("alert state not saved: %v", err)
	}
	pts := newSeriesReader(filepath.Join(state, "series"), "Boiler").read(time.Now().Add(-time.Hour), time.Now())
	if len(pts) == 0 || pts[0].V != 97.5 {
		t.Errorf("history not recorded: %v", pts)
	}
}
