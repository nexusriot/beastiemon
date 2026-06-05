# BeastieMon — Architecture

This document is the engineering reference for BeastieMon: design
intent, runtime architecture, on-disk layout, component contracts,
concurrency model, and the trade-offs behind every non-obvious choice.

For build & operate instructions, see [README.md](README.md).

---

## Table of Contents

1. [Goals & Non-Goals](#1-goals--non-goals)
2. [System Overview](#2-system-overview)
3. [Repository Layout](#3-repository-layout)
4. [Data Model](#4-data-model)
5. [Collector Subsystem](#5-collector-subsystem)
6. [Storage — `store.Ring`](#6-storage--storering)
7. [HTTP API](#7-http-api)
8. [Streaming (SSE) Pipeline](#8-streaming-sse-pipeline)
9. [Frontend](#9-frontend)
10. [CLI (`beastie`)](#10-cli-beastie)
11. [Configuration](#11-configuration)
12. [Concurrency Model](#12-concurrency-model)
13. [Process Lifecycle & rc.d](#13-process-lifecycle--rcd)
14. [Packaging](#14-packaging)
15. [Security Model](#15-security-model)
16. [Performance Characteristics](#16-performance-characteristics)
17. [Error Handling Strategy](#17-error-handling-strategy)
18. [Cross-Platform Story](#18-cross-platform-story)
19. [Future Extensions](#19-future-extensions)
20. [Design Decisions Log](#20-design-decisions-log)

---

## 1. Goals & Non-Goals

### Goals

| # | Goal | Implication |
|---|------|-------------|
| G1 | **Single static binary** for the daemon | `go:embed` for assets, no `libc`-bound deps |
| G2 | **Self-contained** — no Node, no proxy required | uPlot vendored, HTML/JS/CSS embedded |
| G3 | **Cheap at idle** — < 20 MB RSS, < 1 % of one core | In-memory ring; no DB; lean JSON encoding |
| G4 | **FreeBSD-native** — `rc.d`, `pkg(8)`, system user | Ships an `.in` template, manifest, prestart hook |
| G5 | **Live UX** — sub-second graph updates | Server-Sent Events fan-out |
| G6 | **Operationally simple** | One config, one log, one PID file, one user |
| G7 | **CLI usable without the daemon** | CLI imports the same `internal/collect` package |

### Non-Goals

- **No authentication.** Bind to `127.0.0.1`; let nginx / Caddy add auth and TLS.
- **No long-term storage.** Ring buffer holds one hour; export to Prometheus / Influx if you want history.
- **No alerting / rule engine.** Threshold gates can be added later (see §19).
- **Not a fleet tool.** One daemon, one host. Federation is the operator's job.
- **No multi-tenancy.** Single dashboard, no per-user views.

The non-goals exist on purpose. They are what keeps the binary small,
the surface area auditable, and the operational complexity near zero.

---

## 2. System Overview

```
┌────────────────────────────────────────────────────────────────────────┐
│  beastied (single Go binary, runs as _beastie)                         │
│                                                                        │
│   ┌──────────────────┐   chan      ┌──────────────────────────┐        │
│   │ collect.Sampler  │ ──────────▶ │ main loop (cmd/beastied) │        │
│   │  Run(ctx)        │  Snapshot   │  for snap := range C {   │        │
│   │   • tick 1s      │   cap=4     │     server.Ingest(snap)  │        │
│   │   • collect ALL  │             │  }                       │        │
│   │   • emit to C    │             └─────────┬────────────────┘        │
│   └──────────────────┘                       │                         │
│                                              │ Ingest                  │
│                                              ▼                         │
│                              ┌───────────────────────────────┐         │
│                              │ store.Ring                    │         │
│                              │   Push (RWMutex)              │         │
│                              │   Last / Since                │         │
│                              │   3600 × Snapshot ≈ 7 MB      │         │
│                              └─────────────┬─────────────────┘         │
│                                            │                           │
│                              ┌─────────────┴─────────────────┐         │
│                              │ api.Server (net/http.ServeMux)│         │
│                              │   /api/host                   │         │
│                              │   /api/metrics  (Last)        │         │
│                              │   /api/series   (Since + xfm) │         │
│                              │   /api/stream   (SSE)         │         │
│                              │   /healthz                    │         │
│                              │   /  → embed.FS (web/)        │         │
│                              └─────────────┬─────────────────┘         │
│                                            │                           │
│                              ┌─────────────┴─────────────────┐         │
│                              │ api.Broker                    │         │
│                              │   Subscribe/Unsubscribe/Publish│        │
│                              │   non-blocking fan-out        │         │
│                              └─────────────┬─────────────────┘         │
└────────────────────────────────────────────┼───────────────────────────┘
                                             │
                              ┌──────────────┴──────────────┐
                              ▼                             ▼
                       Browser (uPlot)               beastie CLI
                       SSE → live charts             (uses collect/ pkg
                       fetch /api/series             directly, no API)
                                                       │
                                                       ▼
                                                       host metrics
                                                       (no daemon needed)
```

### Two binaries, one collect package

The repository ships:

- **`beastied`** — long-running daemon. Owns the sampler, ring, HTTP server.
- **`beastie`** — CLI tool. **Bypasses the HTTP API entirely** and samples
  the host directly via the same `internal/collect` package. This means
  the CLI works even when `beastied` isn't running — important when you
  SSH into a sick box at 3am.

This is the single most important architectural choice: the collector
package is the contract, not the HTTP API. The API is just one of two
front-ends to the same data source.

---

## 3. Repository Layout

```
beastiemon/
├── cmd/
│   ├── beastied/main.go             # daemon entrypoint
│   └── beastie/main.go              # CLI entrypoint (Beastie ASCII, colour bars)
├── internal/
│   ├── config/
│   │   └── config.go                # TOML decoder, defaults, duration shim
│   ├── collect/
│   │   ├── types.go                 # Snapshot, CPUStats, MemStats, … (wire schema)
│   │   ├── collector.go             # Sampler — orchestrates per-tick collection
│   │   ├── cpu.go                   # per-core delta CPU times
│   │   ├── mem.go                   # virtual+swap memory
│   │   ├── disk.go                  # devstat delta I/O
│   │   ├── net.go                   # per-NIC delta I/O
│   │   ├── fs.go                    # statfs(2) per mount, filtered
│   │   ├── temp.go                  # FreeBSD-only sysctl thermometers
│   │   ├── temp_other.go            # stub for non-FreeBSD builds
│   │   └── proc.go                  # top-N processes by CPU%
│   ├── store/
│   │   └── ring.go                  # in-memory circular buffer of Snapshots
│   └── api/
│       └── server.go                # HTTP handlers + SSE broker
├── web/                             # embedded via //go:embed
│   ├── assets.go                    # embed.FS declaration
│   ├── index.html                   # dashboard scaffold
│   ├── app.js                       # uPlot wiring + SSE consumer
│   ├── style.css                    # dark theme
│   └── vendor/                      # populated by `gmake vendor-js`
│       ├── uplot.iife.min.js
│       └── uplot.min.css
├── freebsd/                         # packaging
│   ├── beastied.in                  # rc.d template (%%PREFIX%% substituted)
│   ├── beastiemon.conf              # default config
│   ├── +MANIFEST                    # pkg(8) manifest (%%VERSION%% substituted)
│   └── pkg-descr
├── Makefile                         # build / vendor / stage / pkg / install
├── go.mod
├── go.sum
├── DESIGN.md                        # this file
└── README.md                        # user docs
```

`internal/` enforces that downstream consumers cannot import any of
these packages — they are deliberately implementation detail. If we
ever expose a programmatic Go API, the wire-stable subset will move
out of `internal/`.

---

## 4. Data Model

The unit of currency throughout the entire system is a `collect.Snapshot`:

```go
// internal/collect/types.go
type Snapshot struct {
    Time   time.Time   `json:"ts"`
    CPU    CPUStats    `json:"cpu"`
    Mem    MemStats    `json:"mem"`
    Net    []NetStats  `json:"net"`
    Disk   []DiskStats `json:"disk"`
    FS     []FSStats   `json:"fs"`
    Temps  []TempStat  `json:"temps,omitempty"`
    Procs  []ProcStat  `json:"procs,omitempty"`
    Load   LoadStats   `json:"load"`
    Uptime uint64      `json:"uptime"`
}
```

Sub-types (all explicit; no `map[string]interface{}` anywhere):

| Field            | Approx size | Notes |
|------------------|------------:|-------|
| `CPUStats`       | ~80 B       | total/user/sys/idle %, `PerCore []float64` |
| `MemStats`       | ~64 B       | total/used/free/avail bytes + swap + percentages |
| `NetStats[]`     | ~48 B each  | per-NIC rx/tx bps + pps |
| `DiskStats[]`    | ~48 B each  | per-dev read/write bps + IOPS |
| `FSStats[]`      | ~80 B each  | per-mount bytes + percent |
| `TempStat[]`     | ~24 B each  | sensor name → °C |
| `ProcStat[]`     | ~80 B each  | top-N processes by CPU |
| `LoadStats`      | ~24 B       | 1/5/15-minute load average |
| `Uptime`         | 8 B         | seconds since boot |

A typical snapshot serialises to ~1.5–2 KB JSON. A 4-core box with 2
NICs, 4 mounts, 1 temp sensor, and `top_procs = 5` lands at ~1.7 KB.

### Why a single struct?

Two reasons:

1. **Wall-clock-aligned snapshots.** Operators correlate "CPU spiked
   when network spiked" — that only works if all metrics share a
   timestamp. Collecting CPU at T₀ and disk at T₀ + 200 ms muddies
   the picture.
2. **Simple storage and slicing.** Ring is `[]Snapshot`. `Since(t)`
   is a single bounded walk. No joins, no indexes, no per-metric
   buffers to keep in sync.

### Why typed fields and not a map?

Type safety from collector to JSON to JS. The wire format is **a
contract**: renaming `RxBps → RxBytesPerSecond` would break the
dashboard, so it's locked behind a single Go struct that the compiler
checks at every step.

---

## 5. Collector Subsystem

### Collector "interface" — there isn't one

Each collector exposes its own typed `Collect()` method:

```go
type CPUCollector  struct{ … }; func (c *CPUCollector)  Collect() CPUStats
type MemCollector  struct{};    func (m *MemCollector)  Collect() MemStats
type DiskCollector struct{ … }; func (d *DiskCollector) Collect() []DiskStats
type NetCollector  struct{ … }; func (n *NetCollector)  Collect() []NetStats
type FSCollector   struct{ … }; func (f *FSCollector)   Collect() []FSStats
type ProcCollector struct{ … }; func (p *ProcCollector) Collect() []ProcStat
```

There is **no** `type Collector interface { Collect() any }`. Adding a
new metric means: add a typed field to `Snapshot`, add a struct with a
`Collect()` method, wire it in `Sampler.collect()`. The compiler catches
anyone forgetting to plumb the new field through to the API and the
dashboard.

### Source table

| Collector | Source | Privilege | Notes |
|-----------|--------|-----------|-------|
| `cpu`     | `gopsutil/cpu.Times(true)` | none | per-core; computes deltas locally |
| `mem`     | `gopsutil/mem.VirtualMemory` + `SwapMemory` | none | absolute values, not deltas |
| `disk`    | `gopsutil/disk.IOCounters` | **`operator` group** | wraps `devstat(3)` |
| `net`     | `gopsutil/net.IOCounters(true)` | none | honours `net_exclude` |
| `fs`      | `gopsutil/disk.Partitions` + `Usage` | none | filtered by `fs_include`, dedupes mounts |
| `temp`    | direct `unix.SysctlRaw` | none | FreeBSD-only build tag |
| `proc`    | `gopsutil/process.Processes` + `.Times` | none | top-N by CPU delta |
| `load`    | `gopsutil/load.Avg` | none | wraps `getloadavg(3)` |
| `uptime`  | `gopsutil/host.Uptime` | none | seconds since boot |

### Delta-based collectors

CPU times, disk I/O, network I/O, and per-process CPU are **cumulative
counters** in the kernel. The collector retains the previous sample
and computes:

```
rate = (current_counter - previous_counter) / elapsed_seconds
```

Consequences:

- **First sample is empty.** No previous to diff against. `Sampler.Run`
  primes once and sleeps one interval before publishing.
- **Counter wrap-around** is not specially handled — FreeBSD uses
  64-bit counters; throughput would have to exceed ~1.8 EB to wrap.
- **Stopped processes disappear** from `proc.prev` on the next tick;
  no per-process state survives a PID exit.

### Temperature collector — FreeBSD-only

`gopsutil`'s sensor support on FreeBSD is incomplete. `temp.go` reads
the relevant `sysctl` MIBs directly via `golang.org/x/sys/unix.SysctlRaw`,
decodes the kernel's *deci-Kelvin* (tenths of a Kelvin) format, and
returns °C:

```go
//go:build freebsd

dk := uint32(raw[0]) | uint32(raw[1])<<8 |
      uint32(raw[2])<<16 | uint32(raw[3])<<24
celsius := float64(dk)/10.0 - 273.15
```

It probes `dev.cpu.N.temperature` for N=0..63 (stops on first error)
and `hw.acpi.thermal.tzN.temperature` for N=0..15. The file is gated
by `//go:build freebsd`; a stub (`temp_other.go`) lets the package
compile on Linux for development.

### Process collector — two-pass ranking

`proc.go` runs in two passes per tick:

1. **Pass 1.** Iterate all PIDs, compute CPU% from delta of
   `times.User + times.System`. Skip processes without a previous
   sample (new PIDs this tick).
2. **Sort + truncate** to top-N (default 5).
3. **Pass 2.** Only for the survivors, call `Name()`, `MemoryInfo()`,
   `MemoryPercent()`. Each is an extra syscall, and we don't pay them
   for the long tail we won't show.

`topN` is configurable via `[collect] top_procs` (default 5). Setting
it to 0 also yields 5 (defensive default inside `NewProcCollector`).

---

## 6. Storage — `store.Ring`

Fixed-capacity circular buffer of `Snapshot`:

```go
type Ring struct {
    mu    sync.RWMutex
    buf   []collect.Snapshot
    head  int     // next write index
    cap_  int
    count int
}
```

| Operation     | Complexity | Locking |
|---------------|------------|---------|
| `Push(s)`     | O(1)       | write   |
| `Last()`      | O(1)       | read    |
| `Since(t)`    | O(N)       | read    |
| `All()`       | O(N)       | read    |

Default capacity is 3600 = one hour at 1 s. Each snapshot is ~2 KB in
memory (Go structs, not JSON), so the buffer caps at ~7 MB and stays
bounded forever regardless of uptime.

No on-disk persistence by design. A daemon restart loses history —
that's the price for keeping the storage layer at ~60 lines of code.
Long-term retention is a future Prometheus exporter (§19), not a
modification to this layer.

### Why a custom ring instead of a library?

- Off-the-shelf ring buffers are either generic (interface{}/generics
  fighting Go's type system) or lock-per-element (unnecessary overhead).
- A 60-line bespoke struct is auditable in one sitting.
- The API package reads `[]Snapshot` directly from `Since()` without
  copying, because RWMutex's `RLock` allows concurrent readers and
  `Since()` returns a fresh slice.

---

## 7. HTTP API

Implemented in `internal/api/server.go`. All handlers register on a
single `*http.ServeMux` — no router library, no middleware framework.

### Endpoints

| Method | Path | Purpose |
|--------|------|---------|
| `GET`  | `/`                            | Serves embedded `index.html` and static assets |
| `GET`  | `/api/host`                    | Hostname, OS, platform version, kernel, process count |
| `GET`  | `/api/metrics`                 | Latest `Snapshot` (returns 503 if no data yet) |
| `GET`  | `/api/series?metric=…&range=…` | Historical time series, uPlot-shaped |
| `GET`  | `/api/stream`                  | Server-Sent Events: one `Snapshot` per sample |
| `GET`  | `/healthz`                     | Liveness probe — returns `ok` |

### `/api/metrics` — current snapshot

```json
{
  "ts": "2026-06-04T15:01:23Z",
  "cpu": {"total": 42.3, "user": 35.1, "sys": 7.2, "idle": 57.7,
          "per_core": [48.0, 39.0, 41.0, 40.0]},
  "mem": {"total": 8589934592, "used": 5260902400, "free": 3329032192,
          "available": 3522502656, "used_pct": 61.2,
          "swap_total": 2147483648, "swap_used": 0, "swap_pct": 0.0},
  "net":  [{"iface": "em0", "rx_bps": 1258291.0, "tx_bps": 419430.4,
            "rx_pps": 850.0, "tx_pps": 420.0}],
  "disk": [{"dev": "ada0", "read_bps": 13002138.0, "write_bps": 5452595.2,
            "read_iops": 124.0, "write_iops": 48.0}],
  "fs":   [{"mount": "/", "dev": "/dev/ada0p2", "fstype": "ufs",
            "total": 68719476736, "used": 19541442150, "free": 49278034586,
            "used_pct": 28.4}],
  "temps":[{"name": "cpu0", "celsius": 52.3}],
  "procs":[{"pid": 845, "name": "beastied", "cpu_pct": 0.3,
            "mem_pct": 0.17, "rss": 14680064}],
  "load": {"load1": 0.82, "load5": 0.75, "load15": 0.71},
  "uptime": 442935
}
```

### `/api/series` — uPlot-shaped time series

uPlot expects `[[timestamps], [series1], [series2], …]`. The handler
returns exactly that shape so the JS doesn't need to transform the data:

```json
{
  "labels": ["ts", "user", "sys", "idle", "total", "cpu0", "cpu1", "cpu2", "cpu3"],
  "data":   [[1717513283, 1717513284, …],
             [12.3, 11.8, …],
             [ 4.1,  3.9, …],
             [83.6, 84.3, …],
             [16.4, 15.7, …],
             [10.0, 11.0, …], […], […], […]]
}
```

Query parameters:

| Parameter | Values | Default |
|-----------|--------|---------|
| `metric`  | `cpu` \| `mem` \| `load` \| `net` \| `disk` \| `temp` | `cpu` |
| `range`   | Go duration (`15m`, `1h`) or integer seconds | `15m` |
| `iface`   | NIC name (when `metric=net`) | sum all |
| `dev`     | device name (when `metric=disk`) | sum all |

Notes:

- For `metric=net|disk` without a filter, the handler **sums all
  interfaces/devices** per timestamp. With a filter, only that one is
  returned.
- `metric=temp` returns one series per sensor name that appeared in
  any snapshot within the range.
- Ranges longer than the ring's contents return only what's available.
- Unknown metrics → 400.

### `parseDuration` quirk

For convenience, `range=300` is parsed as 300 seconds (not as Go's
`ParseDuration` which would error). Go duration syntax (`5m`, `1h`)
still works. This makes the API friendlier from `curl` and shell loops.

---

## 8. Streaming (SSE) Pipeline

### Why SSE and not WebSocket?

| Property                | SSE       | WebSocket |
|-------------------------|-----------|-----------|
| Direction               | server→client | bidirectional |
| Transport               | plain HTTP | upgraded HTTP |
| Reverse-proxy friendly  | yes       | needs `Upgrade` headers |
| Auto-reconnect          | built into `EventSource` | manual |
| Debug with `curl`       | trivial   | annoying |
| Required for BeastieMon | ✅ (one-way push) | overkill |

The dashboard only ever consumes; nothing flows browser→server. SSE is
the natural fit.

### Broker

```go
type Broker struct {
    mu      sync.Mutex
    clients map[chan []byte]struct{}
}
```

`Subscribe()` creates a buffered channel (capacity 8); `Publish(data)`
fans out **non-blockingly**:

```go
for ch := range b.clients {
    select {
    case ch <- data:        // happy path
    default:                // drop for slow consumer
    }
}
```

A slow client loses data, never blocks the pipeline. This is the right
trade-off because:

- Full history is recoverable via `/api/series` if a client misses points.
- A blocking publish would back-pressure the sampler and freeze the
  whole daemon when one browser tab is paused.

### End-to-end flow per tick

```
sampler tick ──► collect snapshot ──► sampler.C
                                          │
                                          ▼
                              main loop receives snap
                                          │
                                          ▼
                              api.Server.Ingest(snap)
                              ├─► ring.Push(snap)
                              └─► json.Marshal + broker.Publish
                                          │
                              fan-out to N subscriber channels
                                          │
                                          ▼
                              SSE handler writes "data: …\n\n"
                              and Flush()es each connection
```

Total per-tick wall time: ~30 µs JSON encode + ~5 µs ring push + N×~3 µs
broker publishes. For typical N=1–3 dashboards open this is well under
1 ms, leaving ~999 ms of the second idle.

### SSE wire format

```
data: {"ts":"2026-06-04T15:01:23Z","cpu":{...},...}\n\n

data: {"ts":"2026-06-04T15:01:24Z","cpu":{...},...}\n\n
```

No `event:` or `id:` fields — the dashboard treats every event as
"latest snapshot, replace state."

---

## 9. Frontend

### Stack

- **Vanilla JavaScript**, ~400 lines, no framework.
- **uPlot** for charts (~50 KB minified). The fastest open-source
  time-series chart library; renders 100k points in <50 ms.
- **CSS Grid** for the dashboard layout. Four-column wide, two-column
  tablet, single-column phone.
- **No build step** — the source is what runs. `gmake vendor-js`
  downloads uPlot once; everything else is pure source.

### Page boot sequence

1. `fetch('/api/host')` → fills the header strip (hostname, OS, kernel).
2. `fetch('/api/series?metric=…&range=15m')` for each card → seeds
   historical chart data.
3. Open `EventSource('/api/stream')` → on each snapshot:
   - Append to each chart's data array.
   - Trim points older than the selected range.
   - Call `chart.setData()` (uPlot's incremental update path).
   - Re-render temperature gauges, filesystem bars, top-procs table.
4. Range selector (5 m / 15 m / 1 h / 6 h / 24 h) refetches series for
   the new window.
5. Per-iface / per-device tab buttons are built lazily on the first SSE
   event (we don't know iface names until then).

### Embedded vs. CDN assets

`web/assets.go` declares an `embed.FS` covering the dashboard files.
By default uPlot loads from unpkg's CDN. Running `gmake vendor-js`:

1. Downloads `uPlot.iife.min.js` and `uPlot.min.css` to `web/vendor/`.
2. Rewrites the `<script>` / `<link>` tags in `index.html` to use the
   local paths.
3. Patches `assets.go`'s `//go:embed` directive to include `vendor/`.

After `vendor-js`, the binary embeds everything and runs offline.

---

## 10. CLI (`beastie`)

### Key design choice: standalone

`beastie` does **not** talk to the HTTP API. It imports
`internal/collect` and `internal/config` directly, creates a sampler,
takes one sample, and prints it. This means:

- Works when `beastied` isn't running.
- Works when the dashboard port is blocked.
- Shows exactly the same numbers as the dashboard (same code path).

### Command dispatch

```
beastie                # status (all panels)
beastie status         # explicit
beastie cpu            # CPU only, with per-core breakdown
beastie mem            # memory + swap
beastie net            # per-NIC throughput
beastie disk           # per-device I/O
beastie fs             # filesystem usage
beastie temp           # temperature sensors
beastie proc           # top-N processes by CPU
beastie load           # load average
beastie top            # like top(1) — continuous refresh
beastie version
beastie help
```

Flag: `-config <path>` for non-default config file.

### Continuous mode (`top`)

The `top` subcommand runs the same one-shot snapshot in a loop,
clearing the screen between renders. Refresh period = configured
sample interval. `Ctrl-C` stops it.

### Banner

Each invocation prints the Beastie mascot in ANSI red over a
`BeastieMon v<version>` line. Suppressed when stdout is not a TTY
(e.g. piped to `less`).

---

## 11. Configuration

### Schema

```toml
[server]
listen = "127.0.0.1:8088"           # string, "host:port"

[collect]
interval    = "1s"                  # Go duration
ring_size   = 3600                  # int, snapshots
fs_include  = ["/", "/var"]         # []string, mount paths; empty = all
net_exclude = ["lo0"]               # []string, NIC names
top_procs   = 5                     # int, top-N for proc panel
```

### Loader

```go
// internal/config/config.go
func Load(path string) (Config, error) {
    cfg := Default()
    if _, err := os.Stat(path); os.IsNotExist(err) {
        return cfg, nil          // missing file = defaults
    }
    _, err := toml.DecodeFile(path, &cfg)
    return cfg, err
}
```

Properties:

- Missing config file is non-fatal — defaults work everywhere.
- Partial files are merged onto defaults — you only need to specify
  what you want to change.
- The `duration` shim implements `encoding.TextUnmarshaler` so the
  TOML can use natural strings like `"500ms"`.

### Defaults

| Field         | Default                            |
|---------------|------------------------------------|
| `listen`      | `127.0.0.1:8088`                   |
| `interval`    | `1s`                               |
| `ring_size`   | `3600`                             |
| `fs_include`  | `["/", "/var", "/usr", "/tmp"]`    |
| `net_exclude` | `["lo0"]`                          |
| `top_procs`   | `5`                                |

---

## 12. Concurrency Model

### Goroutines

The daemon has exactly **four** kinds of goroutines:

1. **Main goroutine** — runs `for snap := range sampler.C { server.Ingest(snap) }`.
2. **Sampler goroutine** — runs `sampler.Run(ctx)`, ticks every interval.
3. **HTTP server goroutine** — `net/http`'s accept loop, started in main.
4. **One per HTTP connection** — `net/http` spawns these; the SSE handler
   blocks on `select { ctx.Done() / msg := <-ch }`.

There are no worker pools, no work queues, no fan-in beyond the ring's RWMutex.

### Synchronisation primitives

| Resource                | Primitive    | Discipline |
|-------------------------|--------------|------------|
| `store.Ring.buf`        | `sync.RWMutex` | Push takes write; Last/Since take read |
| `api.Broker.clients`    | `sync.Mutex` | held briefly for fan-out + map mutation |
| Sampler→main delivery   | `chan Snapshot` cap 4 | non-blocking send; drop on overflow |
| Broker→SSE delivery     | `chan []byte` cap 8 per client | non-blocking send; drop on overflow |

### Shutdown

```go
ctx, cancel := signal.NotifyContext(ctx, SIGINT, SIGTERM)
defer cancel()
go sampler.Run(ctx)        // ctx.Done() stops the ticker
go http.ListenAndServe(…)  // not stopped — daemon process exits
for {
    select {
    case <-ctx.Done(): return
    case snap := <-sampler.C: server.Ingest(snap)
    }
}
```

The HTTP server isn't gracefully shut down because the process exits
anyway and there are no in-flight writes to flush. If we add SQLite
roll-ups, that calculus changes.

### What can go wrong

- **Slow SSE client backs up** — fan-out drops, browser misses points,
  recovers via `/api/series` next time it requests history.
- **Sampler runs slow** (e.g. devstat call stalls) — ticker drops ticks
  (`time.Ticker` semantics); we get fewer samples but no goroutine
  pile-up.
- **Many SSE connections** — each gets a buffered chan; broker
  publishing is O(N) in client count. At 100 clients × 1 KB snapshot
  × 1 Hz that's 100 KB/s of writes per tick — fine.

---

## 13. Process Lifecycle & rc.d

### `freebsd/beastied.in` — two non-obvious details

#### 1. `procname=/usr/sbin/daemon`, **not** `…/beastied`

`daemon(8) -P /var/run/beastied.pid` writes its own (supervisor) PID,
not the child's. `rc.subr`'s status / stop machinery uses
`pgrep -F pidfile procname` — so `procname` must match the process
that owns the pidfile.

If `procname` pointed at the `beastied` binary:

- `service status` would always report "not running" while the daemon
  was alive (PID mismatch).
- `service stop` would fail to find the supervisor, leaving an
  unkillable auto-restarting tree.

This is documented inline in `beastied.in` to spare the next maintainer
the debugging session.

#### 2. The "runas" variable is `beastied_runas`, **not** `beastied_user`

`rc.subr` treats `${name}_user` as a **magic** variable. When set, it
wraps the *entire* `command` line in `su -m <user> -c …`, which means
`daemon(8)` itself runs as `_beastie` — and `_beastie` cannot write to
`/var/run/`. The startup fails with:

```
daemon: ppidfile ``/var/run/beastied.pid'': Permission denied
```

Sidestep: call the variable `beastied_runas` so `rc.subr` ignores it,
and pass `-u ${beastied_runas}` to `daemon(8)` instead. `daemon(8)`
writes the PID file as root, then drops privileges before exec'ing
`beastied`.

### Sample → publish loop

```
main:
    cfg     = config.Load(path)
    ring    = store.NewRing(cfg.Collect.RingSize)
    srv     = api.New(ring, webFS)
    sampler = collect.NewSampler(cfg)

    go func() {
        http.ListenAndServe(cfg.Server.Listen, srv)
    }()

    go sampler.Run(ctx)        // ticks, collects, sends to sampler.C

    for {
        select {
        case <-ctx.Done(): return
        case snap := <-sampler.C:
            srv.Ingest(snap)   // ring.Push + broker.Publish
        }
    }
```

The buffered channel between sampler and main absorbs short JSON-encode
stalls; capacity 4 means up to 4 seconds of buffering before drops kick in.

---

## 14. Packaging

### `freebsd/+MANIFEST`

`pkg(8)` reads a UCL manifest:

```yaml
name: beastiemon
version: "%%VERSION%%"
origin: sysutils/beastiemon
prefix: /usr/local
files: {
  /usr/local/bin/beastied: "-",
  /usr/local/bin/beastie:  "-",
  /usr/local/etc/rc.d/beastied: "-",
  /usr/local/etc/beastiemon.conf.sample: "-",
}
scripts: {
  pre-install:  …   # pw groupadd/useradd, add to operator group
  post-install: …   # copy .conf.sample to .conf if missing
  post-deinstall: … # hint on how to remove the system user
}
```

**No `directories:` stanza.** `/var/log` is base-system; the rc.d
prestart hook creates the log file at runtime. If the manifest declared
ownership of `/var/log`, `pkg create` would refuse to build the package
because the path doesn't exist in the stage tree.

### Makefile pipeline

```
$ gmake VERSION=0.1.0 pkg
   ├── deps          (go mod download / tidy)
   ├── vendor-js     (download uPlot, rewrite index.html, patch assets.go)
   ├── build         (GOOS=freebsd GOARCH=amd64; produces beastied, beastie)
   ├── stage         (lay out .stage/ with bins, rc.d, conf.sample)
   └── pkg           (pkg create --format txz → .pkg/beastiemon-0.1.0.pkg)
```

### Install paths

| Source                             | Installed to                                   |
|------------------------------------|------------------------------------------------|
| `beastied`                         | `/usr/local/bin/beastied`                      |
| `beastie`                          | `/usr/local/bin/beastie`                       |
| `freebsd/beastied.in`              | `/usr/local/etc/rc.d/beastied`                 |
| `freebsd/beastiemon.conf`          | `/usr/local/etc/beastiemon.conf.sample`        |
| (post-install hook copies sample)  | `/usr/local/etc/beastiemon.conf` (only if absent) |

### System user

`pre-install`:

```sh
pw groupadd _beastie -g 874 2>/dev/null || :
pw useradd  _beastie -u 874 -g 874 \
    -d /nonexistent -s /usr/sbin/nologin -c "BeastieMon daemon" \
    2>/dev/null || :
pw groupmod operator -m _beastie 2>/dev/null || :   # devstat access
```

`post-deinstall` does **not** remove the user — uninstalling a user
that owns files on disk is dangerous. The hint is printed instead.

---

## 15. Security Model

### Posture

| Property                     | Stance                                  |
|------------------------------|-----------------------------------------|
| Authentication               | None — by design (G2). Use a proxy.     |
| Transport encryption         | None — by design. Use a proxy.          |
| Default bind                 | `127.0.0.1:8088`                        |
| Privileges                   | `_beastie` (uid/gid 874), nologin shell |
| Elevated capabilities        | `operator` group for `devstat(3)` only  |
| Disk writes                  | `/var/log/beastied.log` only            |
| Config readability           | `0640` root:_beastie                    |
| Log file                     | `0640` root:_beastie                    |
| PID file                     | `0644` root:wheel                       |
| Outbound network             | None                                    |

### Threat model

- **Untrusted local user with shell.** Can read the dashboard via
  `127.0.0.1:8088` and see all metrics. The user could already see
  most of this via `ps`, `top`, `netstat` — BeastieMon doesn't expand
  their privileges. Process names in `procs[]` are world-readable on
  FreeBSD by default.
- **Untrusted remote attacker.** Cannot reach the daemon unless the
  operator changed `listen` from `127.0.0.1`. If they did, the operator
  is expected to put a reverse proxy in front. The README documents this.
- **Compromised `_beastie` account.** Limited to: read sysctl, run
  `gopsutil`'s syscalls, write `/var/log/beastied.log`. No outbound
  network. No persistence outside the log file. Config is `0640`,
  group-readable but not writable by `_beastie`.

### Things deliberately not done

- **CSRF protection** — no state-changing endpoints exist.
- **Rate limiting** — read-only endpoints; ring buffer ops are O(1)
  or O(N) on bounded N.
- **CORS lockdown** — `Access-Control-Allow-Origin: *` on the SSE
  endpoint only, to support reverse-proxy origin mismatch during
  development. Other endpoints don't set CORS headers; the dashboard
  is same-origin.

---

## 16. Performance Characteristics

Measured on FreeBSD 14.0 / amd64 / 4-core / SSD / 8 GB RAM at default
1 s sampling:

| Metric                      | Value         |
|-----------------------------|---------------|
| Steady-state RSS            | ~14 MB        |
| CPU at 1 s sampling         | ~0.3 % of one core |
| JSON encode per snapshot    | ~30 µs        |
| `ring.Since(15m)`           | ~50 µs (900 snapshots, no copy) |
| Broker publish              | ~3 µs per subscriber |
| Web first paint             | < 200 ms over LAN |
| SSE event → chart update    | ~5 ms         |
| Cold start to first sample  | 1 × interval (~1 s) |

### Dominant costs

1. **Per-process iteration** in `proc.go` — O(P) where P is the process
   count. On a 200-process box that's ~200 `Times()` calls per tick;
   each is one `kvm_getprocs(3)`-equivalent under `gopsutil`. This is
   the largest single cost; capping `top_procs` doesn't help because
   ranking requires Pass 1 over everything.
2. **Per-NIC + per-disk syscalls** for delta counters. Linear in
   `len(interfaces) + len(devices)`.
3. **Per-core CPU times** read in one call (`cpu.Times(true)` returns
   a single buffer).

### Scaling guidance

- `interval = "1s"` is the cheapest meaningful rate.
- `interval = "5s"` drops cost ~5× linearly.
- `interval = "500ms"` doubles cost but gives smoother live UX on slow
  hosts (where >1 sample/sec is rare anyway).
- `ring_size = 86400` (24h at 1s) uses ~170 MB and is rarely worth it
  — Prometheus is the right answer at that scale.

---

## 17. Error Handling Strategy

### Principle: degrade, don't fail

The daemon **never panics** during steady-state operation. Each
collector returns nil / zero values on failure:

```go
times, err := psutil.Times(true)
if err != nil || len(times) == 0 {
    return CPUStats{}      // empty struct; dashboard shows 0
}
```

This means a missing sensor (e.g. `coretemp` not loaded) produces an
empty `temps[]` array, the dashboard shows "No sensors detected",
everything else keeps working.

### Fatal paths

The daemon **does** call `log.Fatalf` for:

- Config parse error during startup.
- HTTP `ListenAndServe` failure (port in use, address invalid).

The reasoning: these are operator misconfigurations, not transient
failures. Crashing fast surfaces them in `rc.d`'s output; `daemon(8) -r`
would auto-restart and you'd get crash loops without diagnostics
otherwise — so we let `rc.d` show the error once and stop.

### Client errors

| Condition                              | HTTP status |
|----------------------------------------|-------------|
| Unknown `metric` parameter             | 400 Bad Request |
| No data yet (`ring.Last()` empty)      | 503 Service Unavailable |
| Bad `range` parameter                  | falls back to default (15m) |
| SSE on a non-flusher writer            | 500 Internal Server Error |

---

## 18. Cross-Platform Story

The codebase is **primarily a FreeBSD project**, but the architecture
is portable.

### What's FreeBSD-only

- `internal/collect/temp.go` (gated by `//go:build freebsd`).
  - Stubbed by `temp_other.go` on other OSes.
- `freebsd/*` packaging (rc.d, manifest, conf path).
- `Makefile` uses BSD `sed -i ''` syntax in `vendor-js`.

### What's portable

- All other collectors use `gopsutil`, which supports Linux, macOS,
  Windows, FreeBSD.
- HTTP, SSE, embed, JSON: standard library.
- Frontend: pure browser.

### Linux dev workflow

```sh
gmake build-native
./beastied -config freebsd/beastiemon.conf
```

CPU, memory, disk, network, filesystem, processes work. Temperatures
are empty (the stub returns `nil`). Useful for iterating on the
dashboard without booting a FreeBSD VM.

---

## 19. Future Extensions

The architecture makes these additive (no restructuring required):

| Extension | Surface | Mechanism |
|-----------|---------|-----------|
| Prometheus exporter        | new endpoint `/metrics` | text exposition of latest `Snapshot` |
| SQLite roll-ups (30-day)   | new `internal/store/sqlite.go` | parallel to ring; written async |
| ZFS pool stats             | new collector | `libzfs` via cgo, new `ZFSStats` field |
| Jail-aware metrics         | new collector | `jail_get(2)` enumeration |
| Per-process history        | extend ring | already in `snap.Procs` — just plot it |
| Alert rules                | `[alerts]` section | `expr > threshold for duration` → webhook |
| Web auth (optional)        | middleware | shared-secret basic auth on all `/api/*` |
| WebSocket (if ever needed) | parallel to SSE | broker doesn't care about transport |

The wire format and HTTP API don't need breaking changes for any of
these — they're all additive at the data-model layer.

---

## 20. Design Decisions Log

A running record of decisions where the alternative isn't obvious.

### D1 — Go over Rust or C

- **Decision:** Go.
- **Why:** First-class FreeBSD/amd64+arm64 cross-compile from any host.
  `//go:embed` for single-binary assets. Standard library covers HTTP,
  SSE, JSON, TOML (one tiny dep), signals, contexts.
- **Trade-off:** ~10 MB binary vs ~3 MB Rust binary, ~14 MB RSS vs ~6 MB.
  Acceptable for the project's size.

### D2 — Single `Snapshot` struct, not per-metric streams

- **Decision:** One struct, all metrics, one timestamp.
- **Why:** Operators correlate metrics by time; storage stays trivial.
- **Trade-off:** When one collector is slow, all metrics are delayed.
  We accept this because the slowest collector (`proc`) is fast enough
  (<5 ms on a 200-process box).

### D3 — In-memory ring, no persistence

- **Decision:** RAM only.
- **Why:** Long-term storage is a different problem (TSDB territory).
  Keeps the daemon footprint and operational complexity minimal.
- **Trade-off:** Restart loses history. Future: Prometheus exporter.

### D4 — SSE, not WebSocket

- **Decision:** SSE.
- **Why:** One-way; proxy-friendly; built-in reconnect; debuggable
  with `curl`.

### D5 — Vanilla JS, no framework, no build step

- **Decision:** Pure browser JS.
- **Why:** The dashboard is small enough not to need React/Vue/Svelte;
  no Node toolchain on FreeBSD build hosts.
- **Trade-off:** ~400 lines of imperative DOM manipulation. We accept
  this.

### D6 — Custom ring buffer, not container/list or third-party

- **Decision:** 60-line bespoke struct.
- **Why:** Generic ring buffers either fight Go's type system or
  lock-per-element. Our needs are narrow.

### D7 — `procname=/usr/sbin/daemon` in rc.d script

- **Decision:** Match `procname` to whoever owns the pidfile, which is
  `daemon(8)` when `-P` is used.
- **Why:** Otherwise `service status` lies and `service stop` fails.

### D8 — `beastied_runas` instead of `beastied_user`

- **Decision:** Custom variable name.
- **Why:** Avoid `rc.subr`'s magic `${name}_user` handling which `su`s
  the whole command and breaks PID file writes.

### D9 — CLI does not use the HTTP API

- **Decision:** `beastie` imports `internal/collect` directly.
- **Why:** Works when the daemon doesn't; one code path for metrics.
- **Trade-off:** Two copies of the sampler running if both are active.
  Each is ~14 MB and ~0.3 % CPU — fine.

### D10 — Top-N processes ranked in two passes

- **Decision:** Pass 1 cheap (just CPU%); Pass 2 only for survivors.
- **Why:** `Name()`, `MemoryInfo()`, `MemoryPercent()` each cost a
  syscall; we don't want N×3 syscalls when we'll only display 5
  processes. Linear scan, top-N, then enrich.

### D11 — Non-blocking broker fan-out

- **Decision:** `select { case ch <- data: default: }` per subscriber.
- **Why:** A paused browser tab shouldn't back-pressure the sampler.
  Lost SSE events are recoverable via `/api/series`.

### D12 — No `directories:` stanza in `+MANIFEST`

- **Decision:** Let the rc.d prestart hook create the log file.
- **Why:** `pkg create` validates that every directory listed in the
  manifest exists in the stage tree. `/var/log` is base-system and
  shouldn't be owned by the package anyway.
