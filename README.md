```
                                    ,        ,
                                   /(        )`
                                   \ \___   / |
                                   /- _  `-/  '
                                  (/\/ \ \   /\
                                  / /   | `   /
                                  O O   ) /   |
                                  `-^--'`<     '
                                 (_.)  _  )   /
                                  `.___/`    /
                                    `-----' /
                              <----.     '__\
                              <----|====O)))==)
                              <----'    `--'
```

# BeastieMon 🐡

**Lightweight FreeBSD system-monitoring daemon with a self-contained
web UI and a colourful CLI.**

- One static binary for the daemon (`beastied`), one for the CLI (`beastie`).
- Live graphs over Server-Sent Events (or WebSocket) — CPU, memory, disk I/O,
  network, filesystem usage, temperatures, load, top processes, plus optional
  ZFS pool/ARC and jail panels.
- **Prometheus `/metrics`** exporter, optional **SQLite history** (for ranges
  beyond the in-memory hour, tiered to hourly roll-ups past a week), and
  optional **threshold alerts** with webhooks — plus a sampler watchdog, a
  live rule-state API (`/api/alerts`), and an Alerts dashboard card.
- The CLI samples standalone or reads the running daemon over HTTP/Unix
  socket (`beastie --remote`), including a nagios-style `check` mode.
- Native FreeBSD packaging: `rc.d` script, `pkg(8)` manifest, dedicated
  `_beastie` system user, `newsyslog` log rotation.
- Optional built-in auth (HTTP Basic + bearer token), **off by default**.
  No TLS — bind to `localhost` and put nginx in front for anything
  untrusted.

> For the architecture deep-dive, see [DESIGN.md](DESIGN.md).

---

## Table of Contents

1. [Quick Start](#quick-start)
2. [Requirements](#requirements)
3. [Building from Source](#building-from-source)
4. [Building a FreeBSD Package](#building-a-freebsd-package)
5. [Installation](#installation)
6. [Configuration](#configuration)
7. [Service Management](#service-management)
8. [The `beastie` CLI](#the-beastie-cli)
9. [The Web Dashboard](#the-web-dashboard)
10. [HTTP API Reference](#http-api-reference)
11. [Exposing on the LAN (Reverse Proxy)](#exposing-on-the-lan-reverse-proxy)
12. [Troubleshooting](#troubleshooting)
13. [Uninstalling](#uninstalling)
14. [Development](#development)
15. [Licence](#licence)

---

## Quick Start

If you already have a built `.pkg`:

```sh
pkg install ./beastiemon-0.2.0.pkg
sysrc beastied_enable=YES
service beastied start
```

Open `http://127.0.0.1:8088/` and watch the graphs come alive.
For terminal output:

```sh
beastie          # one-shot snapshot — CPU, mem, disk, net, fs, temp, procs, load, uptime
beastie top      # continuous refresh (like top(1))
beastie proc     # just the top processes by CPU
```

---

## Requirements

- **FreeBSD 13 or later**, amd64 or arm64.
- **Go 1.23+** (`pkg install go`) — to build from source.
- **GNU Make** (`pkg install gmake`) — for the `Makefile`.
- Optional: `curl` or `fetch` for the `vendor-js` target.

Runtime requirements (after installation): none beyond the base system.
Both binaries are built with `CGO_ENABLED=0`, so they are statically
linked and have no shared-library dependencies — including optional SQLite
history, which uses a **pure-Go** driver (`modernc.org/sqlite`), so the
single-static-binary story holds. The optional ZFS and jail panels shell out
to base-system tools (`zpool`, `jls`, `ps`) only when you enable them.

---

## Building from Source

### One-shot build

```sh
git clone https://github.com/nexusriot/beastiemon
cd beastiemon
gmake all          # download deps, vendor uPlot, build both binaries
```

This produces `./beastied` and `./beastie` in the project root.

### Build targets

| Target               | What it does |
|----------------------|--------------|
| `gmake all`          | `deps + vendor-js + build` (default) |
| `gmake build`        | Cross-compile both binaries (defaults `GOOS=freebsd GOARCH=amd64`) |
| `gmake build-native` | Compile for the current host (handy on Linux dev boxes) |
| `gmake vendor-js`    | Download uPlot to `web/vendor/` and rewrite `index.html` so the binary is self-contained at runtime (no CDN) |
| `gmake stage`        | Lay out the install tree under `.stage/` |
| `gmake install`      | Copy from staging into `$DESTDIR/$PREFIX` (run as root) |
| `gmake pkg`          | Produce `.pkg/beastiemon-<VERSION>.pkg` |
| `gmake clean`        | Remove build artefacts |
| `gmake run`          | Build for host and run with the bundled sample config |
| `gmake fmt`          | Run `gofmt -w .` |
| `gmake lint`         | Run `go vet ./...` |
| `gmake test`         | Run `go test ./...` |

Override `VERSION`, `PREFIX`, `GOOS`, `GOARCH`, or `DESTDIR` on the
command line:

```sh
gmake VERSION=0.3.0 GOARCH=arm64 pkg
gmake PREFIX=/opt/beastiemon DESTDIR=/tmp/root install
```

### Quick dev loop

```sh
gmake run
# beastied 0.2.0 listening on 127.0.0.1:8088
```

The binary stays in the foreground; `Ctrl-C` to stop. Web assets are
loaded from the same Go binary via `//go:embed` — no separate file
serving needed.

---

## Building a FreeBSD Package

On a FreeBSD host (or anywhere with `pkg-create`):

```sh
gmake VERSION=0.2.0 pkg
# .pkg/beastiemon-0.2.0.pkg
```

Inspect before installing:

```sh
pkg info -F .pkg/beastiemon-0.2.0.pkg
```

---

## Installation

### From the built package

```sh
pkg install ./.pkg/beastiemon-0.2.0.pkg
```

The package install does the following automatically:

- creates the `_beastie` system user/group (uid/gid 874)
- adds `_beastie` to the `operator` group (needed for `devstat(3)` disk stats)
- installs `beastied` and `beastie` to `/usr/local/bin/`
- installs the rc.d script to `/usr/local/etc/rc.d/beastied`
- installs `beastiemon.conf.sample`; creates `beastiemon.conf` on first install only
- prints next-step hints

### Enable and start

```sh
sysrc beastied_enable=YES
service beastied start
service beastied status
```

---

## Configuration

The config file lives at **`/usr/local/etc/beastiemon.conf`** (TOML).

### Full reference

```toml
[server]
# Bind address. Default localhost-only.
# To expose on the LAN, change to "0.0.0.0:8088" and put nginx in front (see below).
# An absolute path (e.g. "/var/run/beastied.sock") binds a Unix-domain socket
# instead of TCP — local-only access with no port to firewall.
listen = "127.0.0.1:8088"

[collect]
# Sample interval. Any Go duration: "500ms", "1s", "5s".
interval = "1s"

# Seconds of 1-second-resolution history to keep in RAM.
# 3600 = 1 hour at 1s sampling ≈ 7 MB RSS.
ring_size = 3600

# Filesystems to include in the FS usage panel.
# Comment out or use [] to include every mount.
fs_include = ["/", "/var", "/usr", "/tmp"]

# Network interfaces to skip (loopback is rarely interesting).
net_exclude = ["lo0"]

# Number of processes shown in the "Top Processes" panel, ranked by CPU%.
top_procs = 5

# FreeBSD-only extra panels, off by default. Each shells out (zpool / jls / ps)
# every tick, so enable only where the host uses them.
zfs   = false
jails = false

[auth]
# Optional HTTP authentication. Disabled by default (all fields empty).
# Basic auth (username + password) makes the browser dashboard prompt for
# credentials; the bearer token is for API/CLI clients. /healthz is always
# open so liveness probes keep working. See "Exposing on the LAN" below.
username = ""        # set username + password to require HTTP Basic auth
password = ""
token    = ""        # set to require "Authorization: Bearer <token>" (or ?token=)

[store]
# Optional on-disk history (pure-Go SQLite), parallel to the in-memory ring.
# When set, /api/series can answer ranges longer than ring_size and history
# survives restarts. Disabled while path is empty. The directory must be
# writable by _beastie:
#   mkdir -p /var/db/beastiemon && chown _beastie:_beastie /var/db/beastiemon
path       = ""       # e.g. "/var/db/beastiemon/history.db"
retention  = "720h"   # prune samples older than this (default 30 days)
resolution = "1m"     # keep at most one sample per interval (default 1m)

# Tiered downsampling: rows older than coarse_after are re-aggregated into
# one row per coarse_resolution (min/max envelopes merge exactly), so a month
# of retention costs ~720 hourly rows instead of ~43k minute rows past the
# first week. Set coarse_after = "0s" to keep fine resolution throughout.
coarse_after      = "168h"  # default 7 days
coarse_resolution = "1h"    # default 1 hour

[alerts]
# Optional threshold alerts. Each [[alerts.rule]] watches one metric field;
# when it stays beyond the threshold for at least `for`, a JSON webhook POST
# fires (and again, with "state":"resolved", on recovery). Rule states and
# recent events are also served at /api/alerts and shown on the dashboard;
# with [store] enabled, events persist across restarts.
webhook = ""          # default endpoint; a rule may set its own
format  = ""          # default payload format: "" / "raw" (alert JSON), "slack", "discord"

# Sampler watchdog: fire a synthetic "watchdog" alert (to the section-level
# webhook above) when no sample has been collected for this long — the one
# failure threshold rules can't see, since they only run when samples arrive.
# Resolves on the next sample. "0s" (default) disables it.
stale_after = "0s"    # e.g. "30s"

# Array metrics (fs, temp, disk, net) use `field` as a selector: an exact
# name/mount, or "max" (default) for the worst-case value.
# [[alerts.rule]]
# name       = "cpu-sustained"
# metric     = "cpu"    # cpu|mem|swap|load|fs|temp|disk|net
# field      = "total"  # metric-specific: total, used_pct, load1, max, ...
# op         = ">"      # > | >= | < | <=
# threshold  = 90
# for        = "30s"    # sustain duration before firing (default 0 = immediate)
# repeat     = "5m"     # re-notify every 5m while still firing (default 0 = notify once)
# hysteresis = 5        # recovery margin: only resolve at <=85, damping flapping (default 0)
# webhook    = ""       # per-rule endpoint override
# format     = ""       # per-rule payload-format override
```

After a change, restart — or send `SIGHUP` to reload most settings in place
(auth, alerts, and the `[collect]` sampler options; `listen`, `ring_size`,
and the store path still need a restart):

```sh
service beastied restart      # full restart
# or, hot reload:
service beastied reload        # sends SIGHUP
```

### Alerting

A rule fires when its metric stays past `threshold` for `for`, and resolves
when it recovers. Two per-rule knobs tame noisy alerts:

- **`repeat`** re-sends the firing webhook on that cadence while the rule stays
  tripped (a reminder). The default `0` notifies once per episode.
- **`hysteresis`** widens the recovery gap: a firing `> 90` rule with
  `hysteresis = 5` only resolves once the value falls to `≤ 85`, so a metric
  hovering right at the threshold doesn't flap between firing and resolved.

Set `format = "slack"` or `"discord"` (section-wide, or per rule) to POST the
message shape those services' incoming webhooks expect — point a rule's
`webhook` straight at a Slack/Discord webhook URL, no translating proxy needed.
The default (`raw`) posts the alert as JSON:

```json
{"rule":"cpu-sustained","metric":"cpu","field":"total","op":">","threshold":90,
 "value":95.2,"state":"firing","ts":"2026-07-16T10:00:00Z"}
```

Alert activity is also visible without a webhook: `GET /api/alerts` reports
each rule's live state (`ok` / `pending` / `firing`, with the current value)
plus recent events, and the dashboard shows both in an **Alerts** card. Events
are kept in memory (last 200) — or in SQLite when `[store]` is enabled, where
they survive restarts and age out with `retention`.

**Sampler watchdog.** `stale_after` covers the failure mode rules can't:
the sampler itself wedging. When no sample has arrived for that long, a
synthetic `watchdog` alert fires to the section-level `webhook` (value =
seconds since the last sample) and resolves on the next sample. It appears in
`/api/alerts` and the dashboard like any rule.

### Defaults if no config file exists

The daemon ships with sensible defaults; missing config file is fine.
Defaults match the table above.

### rc.conf knobs

```sh
# Required
sysrc beastied_enable=YES

# Optional overrides
sysrc beastied_config=/usr/local/etc/beastiemon.conf
sysrc beastied_runas=_beastie       # daemon drops to this user
sysrc beastied_logfile=/var/log/beastied.log
sysrc beastied_flags=""             # extra args for beastied
```

> **Why `beastied_runas` and not `beastied_user`?**
> `rc.subr` treats `${name}_user` as a magic variable — it `su(1)`s the
> entire command line (including `daemon(8)`) to that user, which then
> can't write the PID file. The non-magic name keeps `daemon(8)` running
> as root long enough to create the PID file, then drops privileges via
> its own `-u` flag. (See DESIGN.md §13 for the full story.)

---

## Service Management

```sh
service beastied start
service beastied stop
service beastied restart
service beastied status
service beastied reload    # SIGHUP: hot-reload config (see Configuration)
```

Log:

```sh
tail -f /var/log/beastied.log
```

The daemon logs only startup, the listen address, config reloads, and fatal
errors — no per-request logging. Use a reverse proxy if you want access logs.

The package installs a `newsyslog(8)` rule at
`/usr/local/etc/newsyslog.conf.d/beastied.conf` that rotates
`/var/log/beastied.log` (keeps 7, compresses). Rotation signals the
`daemon(8)` supervisor, which reopens the log so nothing is lost.

---

## The `beastie` CLI

The CLI is **standalone** — it samples metrics directly via the same
collectors the daemon uses, so it works whether or not `beastied` is
running. When the daemon *is* running, add `--remote` to read its latest
snapshot instead of sampling locally (see [Remote mode](#remote-mode)):
output is instant, because the daemon's rate deltas are already warm.

### Sample output

```
$ beastie
    ,        ,
   /(        )`
   \ \___   / |
   /- _  `-/  '
  (/\/ \ \   /\
  / /   | `   /
  O O   ) /   |
  `-^--'`<     '
 (_.)  _  )   /
  `.___/`    /
    `-----' /
<----.     '__\
<----|====O)))==)
<----'    `--'
    BeastieMon v0.2.0  — FreeBSD system monitor

Host: monitor.local  OS: freebsd 14.0-RELEASE

CPU     ████████░░░░░░░░░░░░ 42.3%  user:35.1%  sys:7.2%  idle:57.7%
        cores: cpu0:48% cpu1:39% cpu2:41% cpu3:40%
MEM     ████████████░░░░░░░░ 61.5%  used:4.9GB  free:3.1GB  total:8.0GB
SWAP    ░░░░░░░░░░░░░░░░░░░░ 0.0%  used:0B  total:2.0GB
NET     em0       ↓ 1.2MB/s    ↑ 0.4MB/s    rx:850pps tx:420pps
DISK    ada0      R: 12.4MB/s  W: 5.2MB/s   riops:124 wiops:48
FS      /            ████░░░░░░░░░░░░ 28.4%  used:18.2GB free:45.9GB total:64.0GB
TEMP    cpu0      52.3°C
LOAD    0.82  0.75  0.71
UPTIME  5d 03:42:15
PROC    PID       CPU%   MEM%        RSS  COMMAND
        845        0.3    0.2       14MB  beastied
        612        0.1    0.1        8MB  sshd
```

The `SWAP` line only appears when swap is configured; `PROC` is always
printed last. `beastie proc` shows just the process table.

### Subcommands

| Command           | Output |
|-------------------|--------|
| `beastie`         | Full snapshot (default — equivalent to `status`) |
| `beastie status`  | Same as above, explicit |
| `beastie cpu`     | CPU only, with per-core breakdown |
| `beastie mem`     | Memory and swap |
| `beastie net`     | Network interfaces |
| `beastie disk`    | Disk I/O |
| `beastie fs`      | Filesystem usage |
| `beastie temp`    | Temperature sensors |
| `beastie proc`    | Top-N processes by CPU |
| `beastie load`    | Load average |
| `beastie top`     | Continuous refresh — like `top(1)`, Ctrl-C to quit |
| `beastie check`   | Nagios-style threshold check (see below) |
| `beastie version` | Print version and exit |
| `beastie help`    | Usage |

### Monitoring check mode

`beastie check` turns a single metric into a nagios/Icinga-compatible plugin:
it prints one status line with perfdata and exits `0` (OK), `1` (WARNING),
`2` (CRITICAL), or `3` (UNKNOWN). Higher is "worse" for every metric.

```sh
$ beastie check --warn 80 --crit 90 cpu
OK: cpu.total% = 42.30 | value=42.30;80;90       # exit 0

$ beastie check --warn 70 --crit 85 mem
WARNING: mem.used% = 78.40 | value=78.40;70;85   # exit 1
```

Metrics: `cpu` (total%), `mem` (used%), `swap` (used%), `load` (load1),
`fs` (worst mount used%), `temp` (hottest sensor °C), `net` (total rx+tx
bytes/s across interfaces), `disk` (total read+write bytes/s across devices).
Thresholds are optional; omit both for an informational `OK` with the current
value. Flags precede the metric. A failed collection — local sampling timed
out, or the `--remote` daemon is unreachable — reports `UNKNOWN` (exit 3)
rather than a false `OK: … = 0.00`. Combine with `--remote` for instant
checks against the running daemon.

### Remote mode

`--remote` makes every command (including `top` and `check`) read the running
daemon's `/api/metrics` instead of sampling locally:

```sh
beastie --remote auto status            # "auto" = server.listen from the config
beastie --remote monitor.local:8088 top # over the network
beastie --remote /var/run/beastied.sock mem   # daemon bound to a Unix socket
beastie --remote https://monitor.example.org check --warn 80 --crit 90 cpu
```

Local sampling needs a full `interval` warm-up per invocation; remote mode
returns in milliseconds and sees exactly what the daemon sees (including
FreeBSD extras like ZFS/jails when enabled there). Credentials come from the
config's `[auth]` section: the bearer `token` if set, else basic
`username`/`password`.

### Flags

```
-config <path>   Use a non-default config file (default: /usr/local/etc/beastiemon.conf)
--json           Emit JSON instead of coloured text (NDJSON for `top`)
--no-color       Disable ANSI colour and the banner (also auto-off when not a TTY)
--remote <addr>  Read from a running beastied: "auto", host:port, URL, or socket path
```

Flags must come before the command (`beastie --json cpu`, not `beastie cpu --json`).
Colour and the banner are emitted only when stdout is a terminal; piping to a
file or `less`, or setting the `NO_COLOR` environment variable, yields clean
plain text automatically.

`top_procs` from the config controls how many processes `beastie proc`
and the full status output display.

### JSON output

`--json` swaps the coloured text for machine-readable JSON — handy for
scripts, `jq`, and ad-hoc monitoring without the daemon. The banner and
host line are suppressed so stdout is pure JSON. Each subcommand emits
just its slice of the snapshot, using the same field names as
[`/api/metrics`](#get-apimetrics):

```sh
beastie --json              # full snapshot object (same shape as /api/metrics)
beastie --json cpu          # {"total":5.5,"user":4.6,"sys":0.9,"idle":94.5,"per_core":[...]}
beastie --json mem | jq .used_pct
beastie --json proc         # array of top-N processes
beastie --json top          # NDJSON: one snapshot object per interval, forever
```

---

## The Web Dashboard

Open `http://127.0.0.1:8088/` (or whatever you bound to). The page is a
single-file vanilla-JS app using [uPlot](https://github.com/leeoniya/uPlot)
for charts.

**Cards on the dashboard:**

- **Header** — hostname, OS, kernel, uptime, live indicator, time-range picker,
  and a light/dark theme toggle (remembers your choice; defaults to the OS
  preference).
- **CPU** — area chart of user / sys / idle plus a total line. On wide ranges
  backed by `[store]`, a shaded min/max band around the total line shows the
  intra-bucket spikes that roll-up averaging would otherwise smooth away.
  (Per-core values are collected and exposed via the CLI and `/api/series`,
  but the web card plots the aggregate only.)
- **Load** — 1 / 5 / 15-minute lines, with current values.
- **Memory** — used / free / swap stacked area, in bytes.
- **Network** — RX / TX, sums all NICs by default; per-iface tabs appear
  if you have more than one.
- **Disk I/O** — read / write, with per-device tabs.
- **Temperatures** — bar gauges, colour-coded (green / orange / red).
- **Filesystems** — usage progress bars per mount.
- **Top Processes** — live-updating table of `top_procs` processes by
  CPU%, with PID, name, CPU%, MEM%, RSS.
- **Alerts** — rule table (name, condition, ok/pending/firing badge, current
  value — the sampler watchdog included) plus the most recent events. Hidden
  until `[alerts]` rules or `stale_after` are configured; refreshes every 10 s.
- **ZFS** — per-pool usage bars plus an ARC size / hit-rate summary. Hidden
  unless `[collect] zfs = true` and pools exist.
- **Jails** — table of running jails (JID, name, hostname, process count).
  Hidden unless `[collect] jails = true` and jails are running.

The range selector (5 m / 15 m / 1 h / 6 h / 24 h) re-fetches historical
data; live updates flow over SSE and are appended to the existing series
in-place.

> **Note:** without `[store]`, ranges longer than the configured `ring_size`
> return only what the in-memory buffer holds. Enable `[store]` to serve long
> ranges (and history across restarts) from SQLite, or scrape `/metrics` into
> Prometheus for real long-term retention.

---

## HTTP API Reference

All endpoints return JSON unless noted. Examples assume default bind.
If `[auth]` is configured, every endpoint except `/healthz` requires a
credential (HTTP Basic or `Authorization: Bearer <token>` / `?token=`)
and returns `401 Unauthorized` otherwise.

### `GET /api/host`

```json
{
  "hostname": "monitor.local",
  "os": "freebsd",
  "platform": "freebsd",
  "platformVersion": "14.0-RELEASE",
  "kernelVersion": "14.0-RELEASE",
  "procs": 153
}
```

This is gopsutil's `host.InfoStat` marshalled verbatim, so the real
response carries a few more fields (`uptime`, `bootTime`, `kernelArch`,
`hostId`, …); the ones above are what the dashboard header consumes.

### `GET /api/metrics`

Most-recent `Snapshot` in full (CPU, mem, net[], disk[], fs[], temps[],
procs[], load, uptime; plus `zfs[]`/`arc`/`jails[]` when those collectors are
enabled). Returns `503 Service Unavailable` if the daemon hasn't taken its
first sample yet (~1 s after start).

### `GET /api/series?metric=<name>&range=<dur>`

Returns uPlot-shaped data:

```json
{
  "labels": ["ts", "user", "sys", "idle", "total", "cpu0", "cpu1"],
  "data":   [[t...], [u...], [s...], [i...], [tot...], [c0...], [c1...]]
}
```

Supported metrics: `cpu`, `mem`, `load`, `net`, `disk`, `temp`, `fs`, `proc`,
`zfs`, `arc`, `jail`. Optional filters: `iface=em0` (`net`), `dev=ada0`
(`disk`), `mount=/` (`fs`), `pid=845` (`proc`), `pool=zroot` (`zfs`), `jid=3`
(`jail`). `fs` returns one used-% series per mount and `zfs` one per pool;
`proc` returns one CPU-% series per process in the latest sample and `jail`
one process-count series per jail; `arc` returns `size`/`target` (bytes) and
`hit_rate` (%) columns.
`range` accepts Go durations (`5m`, `1h`, `24h`) or seconds as a plain int.
When `[store]` is configured, ranges beyond `ring_size` are served from the
SQLite history — rolled up to one averaged sample per `resolution` (one per
`coarse_resolution` past `coarse_after`) — and transparently merged with the
live ring.

Add **`band=1`** to also return the per-bucket min/max envelope: each data
series `X` gains paired `X_min` and `X_max` columns, so a client can draw a
spike band around the average even on wide ranges where roll-ups would
otherwise hide short peaks. Full-resolution ring samples report `min = max =
value`; only the rolled-up store portion spreads the band. The dashboard uses
this for the CPU chart.

### `GET /api/alerts`

Current alert-rule states plus recent events (`?limit=`, default 50, max 500):

```json
{
  "enabled": true,
  "rules": [
    {"name":"cpu-sustained","metric":"cpu","field":"total","op":">",
     "threshold":90,"state":"firing","value":95.2,
     "since":"2026-08-24T10:00:00Z","last_fire":"2026-08-24T10:00:30Z"}
  ],
  "events": [
    {"rule":"cpu-sustained","metric":"cpu","field":"total","op":">",
     "threshold":90,"value":95.2,"state":"firing","ts":"2026-08-24T10:00:30Z"}
  ]
}
```

`state` is `ok`, `pending` (condition true, `for` not yet elapsed), or
`firing`. The `stale_after` watchdog appears as a rule named `watchdog`.
Events come from SQLite when `[store]` is enabled (surviving restarts), else
from the engine's in-memory history; `enabled` is `false` when no alerts are
configured. The dashboard's Alerts card is a client of this endpoint.

### `GET /api/stream`  (Server-Sent Events)

```
$ curl -N http://127.0.0.1:8088/api/stream
data: {"ts":"2026-06-04T15:01:23Z","cpu":{...},"mem":{...},...}

data: {"ts":"2026-06-04T15:01:24Z","cpu":{...},"mem":{...},...}
```

Each event is one JSON `Snapshot` per sample interval.

### `GET /api/ws`  (WebSocket)

The same live stream as `/api/stream`, over WebSocket for clients that prefer
it. Each message is one raw-JSON `Snapshot` text frame (no `data:` framing).
Server-to-client only; the dashboard still defaults to SSE.

```sh
websocat ws://127.0.0.1:8088/api/ws
```

### `GET /metrics`  (Prometheus)

Latest snapshot in Prometheus text exposition format, so BeastieMon can be a
scrape target — `beastie_cpu_percent`, `beastie_mem_used_percent`,
`beastie_fs_used_percent{mount="/"}`, `beastie_load{period="1"}`, and so on
(including ZFS/ARC/jail series when enabled). `503` until the first sample.
When `[auth]` is on, scrapers pass the bearer token like any other endpoint.

```yaml
# prometheus.yml
scrape_configs:
  - job_name: beastiemon
    static_configs: [{ targets: ["monitor.local:8088"] }]
    # authorization: { credentials: "<token>" }   # if [auth].token is set
```

### `GET /healthz`

Returns the literal string `ok` with `200 OK`. For load balancers /
container orchestrators.

---

## Exposing on the LAN (Reverse Proxy)

⚠️ **Auth is off by default**, and even when enabled the daemon speaks
plain HTTP — it has **no TLS**. If anyone untrusted can route to the
port, they see all your metrics (and any credentials travel in the
clear). The recommended path is still to keep `listen = "127.0.0.1:8088"`
and front it with nginx (or Caddy / haproxy) for TLS.

### Optional built-in auth

For a trusted LAN where you'd rather not run a proxy, the daemon has a
built-in `[auth]` section (see [Configuration](#configuration)):

```sh
# Browser dashboard — HTTP Basic (the browser prompts for credentials):
sysrc -f /usr/local/etc/beastiemon.conf  # edit by hand; sysrc doesn't do TOML
# [auth]
# username = "admin"
# password = "change-me"

# API/CLI clients — bearer token:
# [auth]
# token = "long-random-string"
curl -H "Authorization: Bearer long-random-string" http://host:8088/api/metrics
curl -N "http://host:8088/api/stream?token=long-random-string"   # SSE can't set headers
```

`/healthz` stays reachable without credentials. Because there's no TLS,
treat built-in auth as access control on a trusted network, **not** as a
substitute for the reverse proxy when crossing untrusted links.

### One-shot config change to bind on all interfaces

If you really want direct LAN exposure (e.g. trusted home network):

```sh
sed -i '' 's|127.0.0.1:8088|0.0.0.0:8088|' /usr/local/etc/beastiemon.conf
service beastied restart
```

Then allow the port through `pf` if you have a firewall:

```
# /etc/pf.conf
pass in on em0 proto tcp to port 8088
```

```sh
pfctl -f /etc/pf.conf
```

### Recommended: nginx in front

```nginx
# /usr/local/etc/nginx/nginx.conf
server {
    listen 443 ssl http2;
    server_name monitor.example.org;

    ssl_certificate     /usr/local/etc/letsencrypt/live/monitor/fullchain.pem;
    ssl_certificate_key /usr/local/etc/letsencrypt/live/monitor/privkey.pem;

    auth_basic           "BeastieMon";
    auth_basic_user_file /usr/local/etc/nginx/htpasswd;

    location / {
        proxy_pass         http://127.0.0.1:8088;
        proxy_http_version 1.1;
        proxy_buffering    off;       # SSE needs streaming
        proxy_read_timeout 1h;        # SSE long-lived connections
        proxy_set_header   Host       $host;
        proxy_set_header   X-Real-IP  $remote_addr;
    }
}
```

Create the htpasswd file:

```sh
pkg install apache24-utils   # provides htpasswd(1)
htpasswd -c /usr/local/etc/nginx/htpasswd admin
chown root:www /usr/local/etc/nginx/htpasswd
chmod 0640 /usr/local/etc/nginx/htpasswd
```

Keep `beastied` itself bound to `127.0.0.1`.

---

## Troubleshooting

### `service beastied start` says "process already running"

A previous `daemon(8)` supervisor is still alive but the PID file
points elsewhere. Find it and kill it:

```sh
ps -ax | grep '[d]aemon.*beastied'
kill <PID>
rm -f /var/run/beastied.pid
service beastied start
```

### Web page loads but charts are empty for the first second

The daemon needs at least two samples to compute CPU / disk / network /
process deltas — wait one or two `interval` ticks after a restart. The
header strip and filesystem panel populate immediately; the time-series
charts populate after the first delta is ready.

### Disk metrics are blank

`devstat(3)` requires `operator` group membership. The package install
handles this automatically; if you're running from source:

```sh
pw groupmod operator -m _beastie
service beastied restart
```

### Temperatures don't appear

Load the relevant kernel module:

```sh
kldload coretemp        # Intel
# or
kldload amdtemp         # AMD
sysctl dev.cpu.0.temperature   # confirm it's readable
```

To make it persist:

```sh
echo 'coretemp_load="YES"' >> /boot/loader.conf
```

### "ppidfile … Permission denied" at startup

You're running an older version of the rc.d script that uses
`beastied_user` instead of `beastied_runas`. Reinstall the package or
update `/usr/local/etc/rc.d/beastied` from the current source. See
DESIGN.md §13 for the `rc.subr` magic-variable explanation.

### Browser shows red "live" dot

SSE connection dropped. The page auto-reconnects every 5 s. If it
persists, check `service beastied status` and `/var/log/beastied.log`.

### "no data yet" / 503 on `/api/metrics` or `/api/series`

Daemon just started — first sample takes one interval. Wait a second
and retry.

### Top Processes panel shows nothing

Same delta-warmup story as CPU. After one full interval the first
processes will rank. If still empty, check that the daemon can list
processes (it should — no special privilege needed).

---

## Uninstalling

```sh
service beastied stop
sysrc -x beastied_enable
pkg delete beastiemon
```

The package leaves the `_beastie` user behind on purpose (uninstalling
a user that owns files on disk is dangerous). Remove manually if
desired:

```sh
pw userdel _beastie
pw groupdel _beastie
rm -f /usr/local/etc/beastiemon.conf /var/log/beastied.log
```

---

## Development

### Layout

```
cmd/beastied/      daemon entrypoint
cmd/beastie/       CLI entrypoint (standalone sampling, or --remote via the API)
internal/config/   TOML config: defaults, loader, duration shim, value clamping
internal/collect/  metric collectors (cpu, mem, disk, net, fs, temp, proc; +zfs, jail)
internal/store/    in-memory ring + SQLite history (roll-ups, coarse tier, alert events)
internal/alert/    threshold rule engine: webhooks, states/events, sampler watchdog
internal/api/      HTTP handlers, SSE/WebSocket, Prometheus /metrics, /api/alerts, auth gate
web/               embedded HTML/JS/CSS (uPlot dashboard)
freebsd/           rc.d, pkg manifest, sample conf, newsyslog rule
```

See [DESIGN.md](DESIGN.md) for the architectural rationale.

### Run on Linux for development

The temperature collector is FreeBSD-only (gated by `//go:build freebsd`)
but everything else is portable. On Linux:

```sh
gmake build-native
./beastied -config freebsd/beastiemon.conf
```

Disk, network, filesystem, CPU, memory, and process metrics use
`gopsutil` and work fine on Linux — as do the store, alerts, and every
HTTP surface; the temperature, ZFS, and jail panels stay empty (their
collectors are FreeBSD-only stubs).

### Code style

- `gofmt` clean (`gmake fmt`).
- `go vet` clean (`gmake lint`).
- No external test framework — standard `testing` package.
- No third-party HTTP router, no logging framework, no DI container.

### Testing

```sh
gmake test        # go test ./...
```

Everything runs on any OS with the standard `testing` package — no live
daemon, no FreeBSD host, no external framework. The suites:

- [`internal/api/server_test.go`](internal/api/server_test.go) — the auth
  gate, table-driven: disabled auth, HTTP Basic (challenge / wrong /
  correct), bearer token (header and `?token=`), the always-open
  `/healthz`, and `Server.Close` unblocking a live SSE stream. Uses
  `httptest` and an in-memory `fstest.MapFS`.
- [`internal/api/features_test.go`](internal/api/features_test.go) — the
  Prometheus `/metrics` exposition, `/api/series` shaping (`fs`, `proc`,
  `zfs`, `arc`, `jail`, and `band=1`), `/api/alerts`, and the WebSocket
  stream.
- [`internal/alert/alert_test.go`](internal/alert/alert_test.go) — the
  threshold engine: sustained firing/resolve, immediate fire on `for = 0`,
  the `fs` "max" selector, the webhook POST payload, re-notify/hysteresis,
  rule states (`ok`/`pending`/`firing`), the capped event history + sink,
  and the `stale_after` sampler watchdog.
- [`internal/store/sqlite_test.go`](internal/store/sqlite_test.go) — the
  SQLite history: persist-and-query, downsampling to `resolution`, retention
  pruning, async writes, coarse-tier re-aggregation (merge, idempotence,
  recent rows untouched), and alert-event persistence.
- [`internal/collect/bsdextra_parse_test.go`](internal/collect/bsdextra_parse_test.go)
  — the pure `zpool` / kstat / `jls` / `ps` parsers (split out precisely so
  they test on any OS, not just FreeBSD) — plus the counter-reset clamp in
  [`delta_test.go`](internal/collect/delta_test.go).
- [`internal/config/config_test.go`](internal/config/config_test.go) — loader
  defaults, clamping of nonsensical values (`interval = "0s"`,
  `ring_size = 0`), and the coarse-tier / watchdog knobs.
- [`cmd/beastie/main_test.go`](cmd/beastie/main_test.go) — the colour/TTY
  guard, `check`-mode threshold evaluation (incl. `net`/`disk`), the
  interval-scaled collection timeout, and remote mode (target parsing plus a
  live `httptest` fetch with bearer auth).

### Contributing

Issues and PRs welcome. Keep changes focused — one feature or fix per PR.

---

## Licence

MIT — see [`LICENSE`](LICENSE).

Beastie the FreeBSD daemon mascot is a trademark of The FreeBSD
Foundation. The ASCII rendering here is in the public domain.

---

```
                          .-.
                         / \\\\\
                        |\___/|
                        )     (
                       =\\     /=
                         )===(
                        /     \\
                        |     |
                       /       \\
                       \\       /
                  jgs   \\__  __/
                          ((
                           ))
                          (( beastie likes you )) 🐡
```
