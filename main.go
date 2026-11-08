// rnsalert runs shell commands on a schedule, sorts each reading into one of five
// states (CRITICAL LOW, LOW WARNING, SAFE, HIGH WARNING, CRITICAL HIGH), and sends an
// LXMF message over Reticulum when an alert changes state.
//
//	rnsalert run            the service: check on schedule, deliver messages
//	rnsalert test [NAME…]   run the checks once and print what would be sent (sends nothing)
//	rnsalert check-config   exit 0 if the config is valid
//	rnsalert dash           terminal dashboard of current values and history
//	rnsalert graph NAME     print one alert's chart and trend (-range 7d, -height 16)
//
// It opens no port: it dials out to one Reticulum TCP node.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"slices"
	"syscall"
	"time"
)

func main() {
	confPath := flag.String("config", "/etc/rnsalert.conf", "config file")
	state := flag.String("state", "/var/lib/rnsalert", "state directory (identity, queue, alert states)")
	flag.Usage = func() {
		fmt.Fprintln(os.Stderr, "usage: rnsalert [flags] run | test [NAME...] | check-config | dash | graph [-range 24h] [-height 16] NAME")
		flag.PrintDefaults()
	}
	flag.Parse()
	if h, err := os.Hostname(); err == nil {
		hostname = h
	}

	cfg, err := loadConfig(*confPath)
	if err != nil {
		log.Fatal(err)
	}
	switch flag.Arg(0) {
	case "run":
		if cfg.Node == "" {
			log.Fatalf("%s: node is required", *confPath)
		}
		if os.Getenv("JOURNAL_STREAM") != "" {
			log.SetFlags(0) // journald adds the time
		}
		ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
		defer stop()
		if err := runService(ctx, cfg, *state); err != nil {
			log.Fatal(err)
		}
	case "test":
		if !test(cfg, flag.Args()[1:]) {
			os.Exit(1)
		}
	case "dash":
		if err := runDash(cfg, *state); err != nil {
			log.Fatal(err)
		}
	case "graph":
		fs := flag.NewFlagSet("graph", flag.ExitOnError)
		rng := fs.String("range", "24h", "how far back (e.g. 1h, 24h, 7d, 30d, 365d)")
		height := fs.Int("height", 16, "chart height in lines")
		fs.Parse(flag.Args()[1:])
		span, err := parseDuration(*rng, false)
		if err != nil {
			log.Fatalf("-range: %v", err)
		}
		if fs.NArg() != 1 {
			log.Fatal("usage: rnsalert graph [-range 24h] [-height 16] NAME")
		}
		if err := runGraph(cfg, *state, fs.Arg(0), span, max(4, *height)); err != nil {
			log.Fatal(err)
		}
	case "check-config":
		if cfg.Node == "" {
			log.Fatalf("%s: node is required", *confPath)
		}
		fmt.Printf("%s: %d alert(s) OK\n", *confPath, len(cfg.Alerts))
	default:
		flag.Usage()
		os.Exit(2)
	}
}

// test runs each (or each named) alert once, as if it had no history, and prints the
// state and the message that state would send.
func test(cfg *Config, names []string) bool {
	ok, found := true, 0
	for _, a := range cfg.Alerts {
		if len(names) > 0 && !slices.Contains(names, a.Name) {
			continue
		}
		found++
		r := runCommand(context.Background(), a)
		s := Error
		if r.Err == nil {
			s = a.stateFor(r.Value, Unknown)
		} else {
			ok = false
		}
		title, body := a.render(a.event(r, s, Unknown, time.Now()))
		fmt.Printf("[%s] every %s → %s\n  title:   %s\n  message: %s\n\n", a.Name, a.Every, s, title, body)
	}
	if found < len(names) {
		fmt.Fprintln(os.Stderr, "some named alerts are not in the config")
		return false
	}
	return ok
}
