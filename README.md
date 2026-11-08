# reticulum-server-notifications

`rnsalert` runs shell commands on a schedule, puts each reading into one of five
states, and sends an [LXMF](https://github.com/markqvist/LXMF) message over
[Reticulum](https://reticulum.network/) when an alert changes state. The message lands in
Sideband, MeshChat or any other LXMF app. You don't need an internet push service, an account
or an open port: rnsalert dials out to one Reticulum TCP node.

```
CRITICAL LOW ── LOW WARNING ──  SAFE  ── HIGH WARNING ── CRITICAL HIGH
           critical_low   low_warning   high_warning   critical_high
```

It's written in pure Go on [`reticulum-go`](https://github.com/thatSFguy/reticulum-go), with no cgo
and no other services, and builds to one static binary.

## How it works

- **Config:** one file. Global settings come first, then one `[alert NAME]` section per alert. Each
  alert sets a **name**, a **message template**, an **LXMF destination**, a **command** that prints
  the value, **how often** to check, and the thresholds for the **five states**.
- **Checks:** the command runs under `/bin/sh -c` with a timeout. The first field of its first
  line is the value (a trailing `%` is fine). A non-zero exit, a timeout or a non-number
  puts the alert in a sixth state, `ERROR`, so a broken sensor gets reported too.
- **Staggered scheduling:** each alert's checks belong to wall-clock windows of `every`
  (`every = 5m` gives windows at :00, :05, :10…). A check runs at the start of its window plus an
  offset below `stagger`, so twenty 1-minute alerts don't all fork at second :00:
  - `stagger_mode = random` picks a fresh random offset every window (the default).
  - `stagger_mode = spread` gives each alert a fixed offset, spaced evenly by config order.

  After a restart the first check runs within `stagger`, without waiting for the next
  window. Windows missed while the machine slept are skipped rather than run back to back.
- **Messages go out on change**, including the recovery to `SAFE`. Optional settings:
  `repeat` re-sends while the alert is not SAFE, `confirm` needs N readings in a row before a change
  counts, `hysteresis` stops a value that wobbles around a limit from flapping, and `notify` picks
  which states send at all.
- **Delivery that survives restarts:** every message is queued on disk per recipient and retried
  with backoff. After a few failed direct tries it goes to an LXMF propagation node (if one is
  configured), where it waits for a phone that's offline. Alert states are saved, so a restart
  doesn't send everything again.

## Install

```sh
go build -o rnsalert .
sudo install -m 0755 rnsalert /usr/local/bin/
sudo useradd --system --no-create-home --shell /usr/sbin/nologin rnsalert
sudo install -m 0640 -g rnsalert rnsalert.conf.example /etc/rnsalert.conf   # then edit it
sudo install -m 0644 rnsalert.service /etc/systemd/system/
sudo systemctl daemon-reload && sudo systemctl enable --now rnsalert
journalctl -u rnsalert   # shows rnsalert's own LXMF address
```

The config holds shell commands, so rnsalert refuses to load it if group or other can
write to it. The checks run as the unprivileged `rnsalert` user inside the unit's sandbox.

The first start creates a Reticulum identity in `/var/lib/rnsalert/identity`. Messages come
from its LXMF address (it's in the journal); give it a nickname in your app.

## Commands

```
rnsalert [-config /etc/rnsalert.conf] [-state /var/lib/rnsalert] run | test [NAME...] | check-config
```

- `run` starts the service.
- `test` runs every check (or only the named ones) once and prints the state, title and
  message it would send. It sends nothing and doesn't need the node, so use it while writing
  commands. It exits 1 if any check errors.
- `check-config` validates the file, including dry-running every template.

## Config

See [`rnsalert.conf.example`](rnsalert.conf.example) for every key. A short version:

```ini
node    = rns.example.org:4242            # Reticulum TCP node to dial
to      = <0123456789abcdef0123456789abcdef>   # default LXMF recipient(s), comma-separated
stagger = 20s                             # checks start within 20s of their window

[alert Disk /]
command       = df --output=pcent / | tail -n 1
every         = 5m
unit          = %
high_warning  = 85
critical_high = 95
hysteresis    = 2
repeat        = 6h
message       = {{.Host}}: disk {{.Value}}{{.Unit}} ({{.State}})

[alert Greenhouse temp]
command       = cat /sys/bus/w1/devices/28-*/temperature | awk '{print $1/1000}'
every         = 2m
to            = <fedcba9876543210fedcba9876543210>
unit          = °C
critical_low  = 2
low_warning   = 5
high_warning  = 32
critical_high = 38
message.critical_low = Frost risk! Greenhouse at {{.Value}}{{.Unit}}.
```

**States.** Set any of the four thresholds, but at least one. They're inclusive and must
increase from `critical_low` to `critical_high`. A value at or below `critical_low` is
CRITICAL LOW, at or below `low_warning` it's LOW WARNING, at or above `high_warning` it's HIGH
WARNING, at or above `critical_high` it's CRITICAL HIGH, and anything in between is SAFE. An alert
with only high thresholds treats every low value as SAFE.

**Templates** use Go `text/template` (`\n` gives a newline). The fields are `{{.Name}}`,
`{{.State}}`, `{{.Prev}}`, `{{.Value}}`, `{{.Unit}}`, `{{.Threshold}}` (the limit that defines
this state), `{{.Output}}`, `{{.Error}}`, `{{.Host}}`, `{{.Time}}` and `{{.Repeat}}`. Override
per state with `message.STATE` / `title.STATE`. The STATE keys are `critical_low`, `low_warning`,
`safe`, `high_warning`, `critical_high` and `error`.

Windows line up with the Unix epoch, which is UTC. So `every = 1h` checks just after each hour, and
`every = 24h` checks just after midnight UTC.

## Development

```sh
go test ./...          # includes a local end-to-end LXMF delivery test
go test -short ./...   # unit tests only
```

## License

[MIT](LICENSE)
