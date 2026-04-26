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

**Lightweight FreeBSD system-monitoring daemon research project with a self-contained web UI
and CLI.**

- One static binary for the daemon (`beastied`), one for the CLI (`beastie`).
- Live graphs over Server-Sent Events — CPU, memory, disk I/O, network,
  filesystem usage, temperatures, load.
- Native FreeBSD packaging: `rc.d` script, `pkg(8)` manifest, dedicated
  `_beastie` system user.
- No authentication — bind to `localhost` and put nginx in front for
  anything beyond a single host.

> For the architecture deep-dive, see [DESIGN.md](DESIGN.md).

---

## Table of Contents

1. [Quick Start](#quick-start)
2. [Building from Source](#building-from-source)
3. [Building a FreeBSD Package](#building-a-freebsd-package)
4. [Configuration](#configuration)
5. [The `beastie` CLI](#the-beastie-cli)
6. [The Web Dashboard](#the-web-dashboard)
7. [HTTP API Reference](#http-api-reference)
8. [Operations](#operations)
9. [Troubleshooting](#troubleshooting)
10. [Uninstalling](#uninstalling)
11. [Development](#development)
12. [Licence](#licence)

---

## Quick Start

```sh
gmake VERSION=0.1.0 pkg          # produces sysmon-0.1.0.pkg
pkg install ./sysmon-0.1.0.pkg
sysrc sysmond_enable=YES
service sysmond start
# open http://localhost:8088/
```

If you already have a built `.pkg`:

```sh
pkg install ./beastiemon-0.1.0.pkg
sysrc beastied_enable=YES
service beastied start
```

Open `http://127.0.0.1:8088/` and watch the graphs come alive.
For terminal output:

```sh
beastie          # one-shot snapshot
beastie top      # continuous refresh
```

---

## Building from Source

### Requirements

- FreeBSD 13 or later (amd64 / arm64).
- Go 1.21+ (`pkg install go`).
- GNU Make (`pkg install gmake`).
- Optional: `curl` or `fetch` for downloading uPlot offline.

### One-shot build

```sh
git clone https://github.com/nexusriot/beastiemon
cd beastiemon
gmake all          # download deps, vendor uPlot, build both binaries
```

This produces `./beastied` and `./beastie` in the project root.

### Build targets

| Target            | What it does |
|-------------------|--------------|
| `gmake build`     | Cross-compile both binaries (defaults `GOOS=freebsd GOARCH=amd64`) |
| `gmake build-native` | Compile for the current host (handy on Linux dev boxes) |
| `gmake vendor-js` | Download uPlot to `web/vendor/` and rewrite `index.html` so the binary is self-contained (no CDN dependency at runtime) |
| `gmake stage`     | Lay out the install tree under `.stage/` |
| `gmake install`   | Copy from staging into `$DESTDIR/$PREFIX` (run as root) |
| `gmake pkg`       | Produce `.pkg/beastiemon-<VERSION>.pkg` |
| `gmake clean`     | Remove build artefacts |
| `gmake run`       | Build for host and run with the bundled sample config |
| `gmake fmt` / `gmake lint` / `gmake test` | Standard Go housekeeping |

Override `VERSION`, `PREFIX`, `GOOS`, `GOARCH`, or `DESTDIR` on the
command line:

```sh
gmake VERSION=0.2.0 GOARCH=arm64 pkg
```

### Run for development

```sh
gmake run
# beastied 0.1.0 listening on 127.0.0.1:8088
```

The binary stays in the foreground; `Ctrl-C` to stop. The web UI loads
its assets from the same Go binary via `//go:embed` — no separate file
serving needed.

---

## Building a FreeBSD Package

```sh
gmake VERSION=0.1.0 pkg
# .pkg/beastiemon-0.1.0.pkg
```

Inspect before installing:

```sh
pkg info -F .pkg/beastiemon-0.1.0.pkg
```

Install:

```sh
pkg install ./.pkg/beastiemon-0.1.0.pkg
```

The package install does the following automatically:

- creates the `_beastie` system user/group (uid/gid 874)
- adds `_beastie` to the `operator` group (needed for `devstat(3)` disk stats)
- installs `beastied` and `beastie` to `/usr/local/bin/`
- installs the rc.d script to `/usr/local/etc/rc.d/beastied`
- installs `beastiemon.conf.sample`; creates `beastiemon.conf` on first install only
- prints next-step hints

---

## Configuration

The config file lives at **`/usr/local/etc/beastiemon.conf`** (TOML).

```toml
[server]
# Bind address. Default localhost-only; change to 0.0.0.0:8088 for LAN
# (and put a reverse proxy with auth in front!).
listen = "127.0.0.1:8088"

[collect]
# Sample interval (any Go duration: "500ms", "1s", "5s").
interval = "1s"

# How many seconds of history to keep in RAM. 3600 = 1 hour at 1s = ~7 MB.
ring_size = 3600

# Filesystems to show on the dashboard. Empty / omitted = all mounts.
fs_include = ["/", "/var", "/usr", "/tmp"]

# Network interfaces to skip (loopback is rarely interesting).
net_exclude = ["lo0"]
```

After any change:

```sh
service beastied restart
```

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
> its own `-u` flag.

### Exposing on the LAN

```sh
sed -i '' 's|127.0.0.1:8088|0.0.0.0:8088|' /usr/local/etc/beastiemon.conf
service beastied restart
```

⚠️ **There is no auth.** If anyone untrusted can route to this port,
they see your metrics. Front it with nginx + basic auth:

```nginx
server {
    listen 443 ssl;
    server_name monitor.example.org;
    ssl_certificate     /usr/local/etc/letsencrypt/live/monitor/fullchain.pem;
    ssl_certificate_key /usr/local/etc/letsencrypt/live/monitor/privkey.pem;

    auth_basic           "BeastieMon";
    auth_basic_user_file /usr/local/etc/nginx/htpasswd;

    location / {
        proxy_pass         http://127.0.0.1:8088;
        proxy_buffering    off;       # SSE needs streaming
        proxy_read_timeout 1h;
    }
}
```

Keep `beastied` itself bound to `127.0.0.1`.

---

## The `beastie` CLI

The CLI is **standalone** — it samples metrics directly via the same
collectors the daemon uses, so it works whether or not `beastied` is
running.

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
    BeastieMon v0.1.0  — FreeBSD system monitor

Host: monitor.local  OS: freebsd 14.0-RELEASE

CPU     ████████░░░░░░░░░░░░ 42.3%  user:35.1%  sys:7.2%  idle:57.7%
        cores: cpu0:48% cpu1:39% cpu2:41% cpu3:40%
MEM     ████████████░░░░░░░░ 61.5%  used:4.9GB  free:3.1GB  total:8.0GB
NET     em0       ↓ 1.2MB/s    ↑ 0.4MB/s    rx:850pps tx:420pps
DISK    ada0      R: 12.4MB/s  W: 5.2MB/s   riops:124 wiops:48
FS      /            ████░░░░░░░░░░░░ 28.4%  used:18.2GB free:45.9GB total:64.0GB
TEMP    cpu0      52.3°C
LOAD    0.82  0.75  0.71
UPTIME  5d 03:42:15
```

### Subcommands

| Command         | Output |
|-----------------|--------|
| `beastie`       | Full snapshot (default — equivalent to `status`) |
| `beastie cpu`   | CPU only, with per-core breakdown |
| `beastie mem`   | Memory and swap |
| `beastie net`   | Network interfaces |
| `beastie disk`  | Disk I/O |
| `beastie fs`    | Filesystem usage |
| `beastie temp`  | Temperature sensors |
| `beastie load`  | Load average |
| `beastie top`   | Continuous refresh — like `top(1)`, Ctrl-C to quit |
| `beastie version` | Print version and exit |
| `beastie help`  | Usage |

### Flags

```
-config <path>   Use a non-default config file (default: /usr/local/etc/beastiemon.conf)
```

Colours auto-disable when stdout is not a TTY (e.g. piped to `less`).

---

## The Web Dashboard

Open `http://127.0.0.1:8088/` (or whatever you bound to). The page is a
single-file vanilla-JS app using [uPlot](https://github.com/leeoniya/uPlot)
for charts.

**What's on it:**

- Header — hostname, OS, kernel, uptime, live indicator, time-range picker.
- **CPU** — stacked area: user / sys / idle. Per-core appended below.
- **Load** — 1 / 5 / 15-minute lines, with current values.
- **Memory** — used / free / swap stacked area, in bytes.
- **Network** — RX / TX, sums all NICs by default; per-iface tabs appear
  if you have more than one.
- **Disk I/O** — read / write, with per-device tabs.
- **Temperatures** — bar gauges, colour-coded (green / orange / red).
- **Filesystems** — usage progress bars per mount.

The range selector (5 m / 15 m / 1 h / 6 h / 24 h) re-fetches historical
data; live updates flow over SSE and are appended to the existing series
in-place.

> **Note:** ranges longer than the configured `ring_size` will return
> only what the buffer holds. To see 24 hours, raise `ring_size` to
> `86400` (uses ~170 MB) or shorten `interval` correspondingly.

---

## HTTP API Reference

All endpoints return JSON unless noted. Examples assume default bind.

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

### `GET /api/metrics`

The most-recent `Snapshot` in full (CPU, mem, net[], disk[], fs[],
temps[], load, uptime).

### `GET /api/series?metric=<name>&range=<dur>`

Returns uPlot-shaped data:

```json
{
  "labels": ["ts", "user", "sys", "idle", "total", "cpu0", "cpu1"],
  "data":   [[t...], [u...], [s...], [i...], [tot...], [c0...], [c1...]]
}
```

Supported metrics: `cpu`, `mem`, `load`, `net`, `disk`, `temp`.
Optional filters: `iface=em0` (for `net`), `dev=ada0` (for `disk`).
`range` accepts Go durations (`5m`, `1h`, `24h`) or seconds as a plain int.

### `GET /api/stream`  (Server-Sent Events)

```
$ curl -N http://127.0.0.1:8088/api/stream
data: {"ts":"2026-04-26T15:01:23Z","cpu":{...},"mem":{...},...}

data: {"ts":"2026-04-26T15:01:24Z","cpu":{...},"mem":{...},...}
```

Each event is one JSON `Snapshot` per sample interval.

### `GET /healthz`

Returns the literal string `ok` with `200 OK`. For load balancers /
container orchestrators.

---

## Operations

### Service control

```sh
service beastied start
service beastied stop
service beastied restart
service beastied status
```

### Log

```sh
tail -f /var/log/beastied.log
```

The daemon logs only startup, listen address, and fatal errors. For
verbose request logging, put nginx in front and use `access_log`.

### Resource expectations

On a 4-core / 8 GB box at default 1 s sampling:

- ~14 MB RSS
- ~0.3 % of one CPU core
- ~7 MB for the in-memory ring buffer (one hour)

---

## Troubleshooting

### `service beastied start` says "process already running"

A previous `daemon(8)` supervisor is still alive but the PID file is
stale. Kill it manually:

```sh
ps -ax | grep '[d]aemon.*beastied'
kill <PID>
rm -f /var/run/beastied.pid
service beastied start
```

### Web page loads but charts are empty

The daemon needs at least two samples to compute CPU / disk / network
deltas — wait one or two `interval` ticks after a restart.

### Disk metrics are blank

`devstat(3)` requires `operator` group membership. The package install
handles this; if you're running from source:

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

You're running an old version of the rc.d script that uses
`beastied_user` instead of `beastied_runas`. Reinstall the package or
update `/usr/local/etc/rc.d/beastied` from the current source.

### Browser shows red "live" dot

SSE connection dropped. The page auto-reconnects every 5 s. If it
persists, check `service beastied status` and `/var/log/beastied.log`.

---

## Uninstalling

```sh
service beastied stop
sysrc -x beastied_enable
pkg delete beastiemon
```

The package leaves the `_beastie` user behind on purpose (uninstalling a
user that owns files on disk is dangerous). Remove manually if desired:

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
cmd/beastie/       CLI entrypoint
internal/collect/  metric collectors
internal/store/    in-memory ring buffer
internal/api/      HTTP + SSE
web/               embedded HTML/JS/CSS
freebsd/           rc.d, pkg manifest, sample conf
```

See [DESIGN.md](DESIGN.md) for the architectural rationale.

### Run on Linux for development

The temperature collector is FreeBSD-only (gated by `//go:build freebsd`)
but everything else is portable. On Linux:

```sh
gmake build-native
./beastied -config freebsd/beastiemon.conf
```

Disk and network metrics use `gopsutil` and work fine on Linux; only
the CPU temperature panel will be empty.

### Code style

- `gofmt` clean (`gmake fmt`).
- `go vet` clean (`gmake lint`).
- No external test framework — standard `testing` package.

### Contributing

Issues and PRs welcome. Keep changes focused — one feature or fix per PR.

---

## Licence

BSD 2-Clause — see `LICENSE`.

Beastie the FreeBSD daemon mascot is a trademark of The FreeBSD
Foundation. The ASCII rendering here is in the public domain (Felix Lee,
1991, classic comp.unix).

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
