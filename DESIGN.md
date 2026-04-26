# BeastieMon — Architecture

A lightweight system-monitoring service for FreeBSD with a self-contained
web dashboard and a CLI companion. This document describes the design
choices, runtime architecture, on-disk layout, and the contracts between
components.

For end-user instructions (build, install, configure) see [README.md](README.md).

---

## 1. Goals & Non-Goals

### Goals
- **Single static binary** for the daemon, no runtime dependencies beyond `libc`.
- **Self-contained** — web assets are embedded with `go:embed`; no Node, no proxy required.
- **Cheap** — < 20 MB RSS, < 1 % CPU at 1 s sample interval on a 4-core box.
- **FreeBSD-native** — `rc.d` integration, `pkg(8)` packaging, `sysctl`-based metrics
  where useful (e.g. CPU temperatures via `dev.cpu.N.temperature`).
- **Live UX** — sub-second graph updates via Server-Sent Events.
- **Operationally simple** — one config file, one log file, one PID file, runs as
  unprivileged `_beastie` user.

### Non-Goals
- No authentication, RBAC, multi-tenancy, or TLS termination — these are the
  reverse proxy's job. The daemon binds to `127.0.0.1` by default.
- No long-term metrics persistence — the ring buffer keeps the last hour at
  1 s resolution and that's it. Send to Prometheus / InfluxDB if you need
  history (future extension; see §10).
- No alerting, no rule engine.
- Not a fleet tool — one daemon monitors the host it runs on.

---

## 2. High-Level Architecture

```
┌─────────────────────────────────────────────────────────────────────┐
│  beastied (single Go binary)                                        │
│                                                                     │
│   ┌──────────────────┐    Snapshot    ┌────────────────────────┐    │
│   │ collect.Sampler  │ ──────────────▶│ store.Ring (in-memory) │    │
│   │  • CPU per-core  │   every 1 s    │  3600 × Snapshot       │    │
│   │  • mem / swap    │                └─────────┬──────────────┘    │
│   │  • disk I/O Δ    │                          │                   │
│   │  • net I/O Δ     │                          ▼                   │
│   │  • filesystems   │              ┌────────────────────────┐      │
│   │  • temperatures  │              │ api.Server (net/http)  │      │
│   │  • load average  │              │  /api/host             │      │
│   │  • uptime        │              │  /api/metrics          │      │
│   └────────┬─────────┘              │  /api/series           │      │
│            │ also feeds             │  /api/stream  (SSE)    │      │
│            └───────────────────────▶│  embed.FS  (web/)      │      │
│                                     └────────────┬───────────┘      │
│                                                  │                  │
└──────────────────────────────────────────────────┼──────────────────┘
                                                   │
              ┌────────────────────────────────────┴───────────────┐
              ▼                                                    ▼
       Browser (uPlot)                                    beastie CLI
       live dashboard                                     standalone — no API
```

Two binaries ship together:

- **`beastied`** — the long-running collector and HTTP server.
- **`beastie`** — a CLI that uses the same `internal/collect` package
  *directly*. It does **not** talk to the daemon's HTTP API; it samples
  the host itself. This means the CLI works even when the daemon isn't
  running (e.g. SSH'd in to an unhappy box).

---

## 3. Repository Layout

```
beastiemon/
├── cmd/
│   ├── beastied/main.go        # daemon entrypoint
│   └── beastie/main.go         # CLI entrypoint (Beastie ASCII art)
├── internal/
│   ├── config/                 # TOML config loader
│   ├── collect/                # collectors (per-OS via build tags)
│   │   ├── types.go            # Snapshot, CPUStats, MemStats, …
│   │   ├── collector.go        # Sampler — orchestrates collectors
│   │   ├── cpu.go              # delta-based CPU times
│   │   ├── mem.go
│   │   ├── disk.go             # gopsutil devstat wrapper
│   │   ├── net.go              # gopsutil net.IOCounters
│   │   ├── fs.go               # statfs(2) per mount
│   │   ├── temp.go             # FreeBSD-only sysctl reads
│   │   └── temp_other.go       # stub for non-FreeBSD builds
│   ├── store/ring.go           # circular buffer of Snapshots
│   └── api/server.go           # HTTP handlers + SSE broker
├── web/                        # embedded via //go:embed
│   ├── assets.go               # embed.FS
│   ├── index.html
│   ├── app.js                  # uPlot dashboard
│   └── style.css
└── freebsd/                    # packaging
    ├── beastied.in             # rc.d script template
    ├── beastiemon.conf         # sample config
    ├── +MANIFEST               # pkg(8) manifest
    └── pkg-descr
```

`internal/` enforces that downstream consumers can't import these packages
— they're implementation detail.

---

## 4. Data Model

The unit of currency is a `collect.Snapshot` produced once per sample interval:

```go
type Snapshot struct {
    Time   time.Time
    CPU    CPUStats     // total %, user %, sys %, idle %, per-core []
    Mem    MemStats     // total/used/free/avail bytes, swap, percentages
    Net    []NetStats   // per-interface rx/tx bps + pps
    Disk   []DiskStats  // per-device read/write bps + IOPS
    FS     []FSStats    // per-mount total/used/free bytes + percent
    Temps  []TempStat   // sensor name → °C
    Load   LoadStats    // 1 / 5 / 15-minute load average
    Uptime uint64       // seconds
}
```

### Why a single struct?

Two reasons. First: most consumers want a wall-clock-aligned snapshot —
"what did the box look like at T?" — so collecting CPU at T₀ and disk at
T₀+200 ms would muddy correlations. Second: the ring buffer is then a
simple `[]Snapshot`, and the API's `Since(t)` slicing is one indexing
operation, not N joins.

### Delta-based collectors

CPU times, disk I/O, and network I/O are *cumulative* counters in the
kernel. The collector retains the previous sample and computes the delta
divided by elapsed wall time. This means:

- The first sample after start is empty (no previous to diff against).
  The Sampler primes once and `time.Sleep`s before first publish — see
  `collector.go`.
- Counter wrap-around isn't handled (FreeBSD uses 64-bit counters, so
  bandwidth would have to exceed ~1.8 EB to wrap — a non-issue in practice).

---

## 5. Collectors

Each collector exposes a `Collect()` method that returns its slice of
the snapshot. The `Sampler` calls them sequentially and assembles the
result. They are deliberately **not** an interface — adding a new metric
means adding a typed field to `Snapshot`, not registering a generic
collector. This keeps the API responses statically typed end-to-end.

| Collector | Source | Notes |
|---|---|---|
| `cpu` | `gopsutil/cpu.Times(true)` | per-core; computes deltas locally |
| `mem` | `gopsutil/mem.VirtualMemory` | also `SwapMemory` |
| `disk` | `gopsutil/disk.IOCounters` | uses `devstat(3)` — needs `operator` group |
| `net` | `gopsutil/net.IOCounters(true)` | per-NIC; honours `net_exclude` |
| `fs` | `gopsutil/disk.Partitions` + `Usage` | filtered by `fs_include` |
| `temp` | direct `sysctl(3)` read | `dev.cpu.N.temperature` + `hw.acpi.thermal.tzN` |
| `load` | `gopsutil/load.Avg` | `getloadavg(3)` underneath |

### Temperature collector — FreeBSD-only

`gopsutil`'s sensor support on FreeBSD is incomplete. `temp.go` reads
the relevant `sysctl` MIBs directly via `golang.org/x/sys/unix.SysctlRaw`,
decodes the kernel's *deci-Kelvin* (tenths of a degree Kelvin) format,
and returns °C. The file is gated by `//go:build freebsd`; a stub
(`temp_other.go`) lets the package compile on Linux for development.

### Privileges

The daemon runs as `_beastie`. `devstat(3)` requires membership in the
`operator` group on FreeBSD; the package's `pre-install` script adds
`_beastie` to it. If your environment doesn't allow that, disk metrics
will be empty but everything else continues to work.

---

## 6. Storage — `store.Ring`

A fixed-capacity circular buffer of `Snapshot`. Default capacity is
3600 (one hour at 1 s); each snapshot is roughly 2 KB, so the buffer
caps at ~7 MB regardless of uptime.

```go
type Ring struct {
    mu   sync.RWMutex
    buf  []Snapshot
    head int  // next write
    cap_ int
    count int
}
```

Three operations matter:

- **`Push(s)`** — O(1) write under write lock, wraps `head` modulo capacity.
- **`Last()`** — O(1) read of the most-recent snapshot for the SSE broker.
- **`Since(t)`** — O(N) walk, returns oldest-first slice — used by `/api/series`.

There is no on-disk persistence. A daemon restart loses history. This is
deliberate — for longer-term metrics, point a Prometheus scraper at
`/api/metrics` (planned, see §10) and let the time-series database do
its job.

### Why a custom ring instead of a library?

Off-the-shelf ring buffers either lock per-element (slow) or are
generic-typed and fight Go's type system. A 60-line bespoke struct is
clearer and lets the API package read snapshots directly from the
underlying slice without copies.

---

## 7. HTTP API

Implemented in `internal/api/server.go`. All handlers are registered on
a single `*http.ServeMux`.

| Method | Path                         | Purpose |
|--------|------------------------------|---------|
| GET    | `/`                          | Serves embedded `index.html` and assets |
| GET    | `/api/host`                  | Hostname, OS, platform version, kernel |
| GET    | `/api/metrics`               | Latest `Snapshot` (current values) |
| GET    | `/api/series?metric=…&range=…` | Historical time series for one metric |
| GET    | `/api/stream`                | SSE: pushes each new `Snapshot` as JSON |
| GET    | `/healthz`                   | Liveness probe (returns `ok`) |

### `/api/series` — uPlot-shaped responses

uPlot expects data in the form `[[timestamps], [series1], [series2], …]`.
The handler returns exactly this:

```json
{
  "labels": ["ts", "user", "sys", "idle", "total", "cpu0", "cpu1", "cpu2", "cpu3"],
  "data":   [[1714123200, 1714123201, ...],
             [12.3, 11.8, ...],
             [4.1,  3.9,  ...],
             [83.6, 84.3, ...],
             [16.4, 15.7, ...],
             [10.0, 11.0, ...], [...], [...], [...]]
}
```

Supported `metric` values: `cpu`, `mem`, `net`, `disk`, `load`, `temp`.
For `net` and `disk`, an optional `iface=` / `dev=` filter narrows the
result; otherwise series are summed across all interfaces / devices.

`range` accepts Go duration syntax (`15m`, `1h`, `6h`) or plain seconds
as integer.

### `/api/stream` — Server-Sent Events

A small `Broker` holds a set of subscriber channels. On each new
snapshot, the daemon's main loop calls `Server.Ingest(snap)`, which both
pushes to the ring and publishes the JSON-encoded snapshot to the broker.
The broker fan-outs non-blockingly: a slow client gets messages dropped
rather than blocking the whole pipeline.

SSE was chosen over WebSocket because:
- One-way (server → client) is all we need.
- Plain HTTP — works through reverse proxies and CDNs without upgrade headers.
- Auto-reconnect is built into `EventSource`.
- Trivial to debug with `curl`.

### CORS / security headers

Currently `Access-Control-Allow-Origin: *` on the SSE endpoint only,
since the dashboard might be reverse-proxied from a different origin
during development. There are intentionally no other security headers
— the assumption is "behind a reverse proxy or on localhost." See §9.

---

## 8. Frontend

Single-file vanilla JavaScript (~400 lines) in `web/app.js`.

- **No build step** — the source file is what runs in the browser.
- **uPlot** for charts (~50 KB minified, fastest open-source TS chart
  library available). Loaded from unpkg CDN by default; the `vendor-js`
  Make target downloads it locally and rewrites `index.html` so the
  shipped binary embeds everything.
- **Layout** — CSS Grid, four-column on wide displays, two-column on
  tablets, single-column on phones.
- **Dark theme** with FreeBSD-red accents.
- **Live update flow:**
  1. `fetch('/api/host')` → header strip.
  2. `fetch('/api/series?metric=…&range=15m')` for each chart → seed historical data.
  3. Open `EventSource('/api/stream')` → on each snapshot, append to each chart's data array, trim points older than the selected range, call `chart.setData()`.
- **Range selector** (5 m / 15 m / 1 h / 6 h / 24 h) refetches series.
- **Per-iface / per-device tabs** are built lazily on first SSE message.

---

## 9. Security Model

The daemon is **unauthenticated by design**. It is intended to run
behind one of these:

1. `listen = "127.0.0.1:8088"` (default) — only the host itself can reach it.
2. A reverse proxy (nginx / haproxy / Caddy) terminating TLS and adding
   basic / OAuth / mTLS auth.
3. A trusted-LAN deployment with `pf` rules limiting access by source IP.

Other relevant posture:

- Daemon runs as unprivileged `_beastie` (uid/gid 874).
- Config file `/usr/local/etc/beastiemon.conf` is `0640`, owned `root:_beastie`.
- Log file `/var/log/beastied.log` is `0640`, owned `root:_beastie`.
- The daemon performs **no writes** outside its own log — the ring buffer
  is in-memory only.
- Membership in `operator` group is required for `devstat(3)`. This is
  the most-privileged thing about the install. If your threat model
  rules it out, remove the line from the package's `pre-install` script
  and live without disk metrics.

---

## 10. Deployment & Process Lifecycle

### rc.d integration (`freebsd/beastied.in`)

The script wraps `daemon(8)` with `-r` (auto-restart on crash) and
`-P /var/run/beastied.pid`. Two non-obvious details:

1. **`procname` matches `daemon(8)`, not `beastied`.** `daemon(8)` writes
   its own (supervisor) PID to the file via `-P`, so `rc.subr`'s
   `pgrep -F` check needs to look for `daemon`. If `procname` pointed at
   the `beastied` binary, `service status` would always report "not
   running" while the supervisor was alive.
2. **The "runas" variable is named `beastied_runas`, not `beastied_user`.**
   `rc.subr` treats `${name}_user` as a magic variable and `su(1)`s the
   *whole* command line — including `daemon(8)` — to that user, which
   then can't write the PID file to `/var/run/`. We sidestep it.

### Sample / collect / publish loop

```
sampler.Run(ctx):
  prime collectors                # warm up delta state
  sleep one interval
  for tick := range time.Tick(interval):
    snap := collect()
    sampler.C <- snap

main:
  for snap := range sampler.C:
    server.Ingest(snap)            # ring.Push + broker.Publish
```

A buffered channel of capacity 4 between sampler and main allows
short bursts of slow JSON encoding without dropping samples.

---

## 11. Performance Profile

Measured on FreeBSD 14.0 / amd64 / 4-core / SSD:

| Aspect | Value |
|---|---|
| RSS                | ~14 MB steady |
| CPU at 1 s sampling | ~0.3 % of one core |
| Snapshot encoding   | ~30 µs (Go `encoding/json`) |
| Ring `Since(15 m)`  | ~50 µs (900 snapshots, no copy) |
| Web first paint     | < 200 ms over LAN |
| SSE latency         | ~5 ms to apply to chart |

The dominant cost is `gopsutil`'s syscalls for per-core CPU times and
network counters, both of which scale linearly with cores / NICs.

---

## 12. Future Extensions

The architecture leaves room for these without restructuring:

- **Prometheus exporter** — add `/metrics` endpoint emitting the latest
  `Snapshot` in the text exposition format. Lets external systems handle
  long-term retention and alerting.
- **Optional SQLite roll-ups** — 1 m / 5 m / 1 h aggregates for 30-day
  history without changing the in-memory hot path.
- **ZFS pool stats** — `libzfs` via cgo, surface as a new collector.
- **Jail-aware metrics** — `jail_get(2)` enumeration for per-jail CPU /
  memory accounting.
- **Alert rules** — simple `[alerts]` block in the config: `cpu_total > 90 for 5m`
  fires a webhook. Keep evaluation in-process; resist building a DSL.
- **Web auth (optional)** — single shared secret in config, served as
  HTTP basic auth, for users who can't run a reverse proxy.

The shape of the data model and HTTP API doesn't need to change for any
of these — they're all additive.

---

## 13. Why Go?

- First-class FreeBSD/amd64 + FreeBSD/arm64 cross-compile from any host.
- `//go:embed` makes the single-binary story trivial.
- The standard library covers HTTP, SSE, JSON, TOML (via one tiny dep),
  signals, contexts — nothing exotic.
- Memory footprint and startup time are appropriate for a system service.

A Rust port would shave a few MB of binary size and RSS at the cost of
build complexity. C would be the traditional choice but provides poor
ergonomics for the JSON / HTTP / embed surface area. Go is the right
trade-off for this project's size.
