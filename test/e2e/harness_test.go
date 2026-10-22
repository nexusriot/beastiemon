//go:build e2e

// Package e2e drives the real beastied and beastie binaries the way an
// operator does: a config file, a process, HTTP requests, signals, exit codes.
// It is tagged so `go test ./...` stays a fast unit run; `make e2e` executes
// it inside a container, where the daemon can bind ports and write files
// without touching the developer's machine.
package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

var (
	beastiedBin string
	beastieBin  string
)

// TestMain finds the binaries under test. The container image builds them and
// points the two environment variables here; a developer running
// `make e2e-local` gets them built into a temp directory instead.
func TestMain(m *testing.M) {
	code, err := run(m)
	if err != nil {
		fmt.Fprintln(os.Stderr, "e2e setup:", err)
		os.Exit(1)
	}
	os.Exit(code)
}

func run(m *testing.M) (int, error) {
	beastiedBin = os.Getenv("BEASTIED_BIN")
	beastieBin = os.Getenv("BEASTIE_BIN")
	if beastiedBin == "" || beastieBin == "" {
		dir, err := os.MkdirTemp("", "beastiemon-e2e-bin")
		if err != nil {
			return 0, err
		}
		defer os.RemoveAll(dir)
		for _, c := range []struct {
			name string
			dst  *string
		}{
			{"beastied", &beastiedBin},
			{"beastie", &beastieBin},
		} {
			out := filepath.Join(dir, c.name)
			cmd := exec.Command("go", "build", "-o", out, "../../cmd/"+c.name)
			cmd.Env = append(os.Environ(), "CGO_ENABLED=0")
			if b, err := cmd.CombinedOutput(); err != nil {
				return 0, fmt.Errorf("building %s: %v\n%s", c.name, err, b)
			}
			*c.dst = out
		}
	}
	for _, p := range []string{beastiedBin, beastieBin} {
		if _, err := os.Stat(p); err != nil {
			return 0, err
		}
	}
	return m.Run(), nil
}

// daemon is one running beastied, with its config, address and log.
type daemon struct {
	t       *testing.T
	cmd     *exec.Cmd
	cfgPath string
	addr    string // "host:port", or an absolute path for a Unix socket
	baseURL string
	client  *http.Client
	logs    *syncBuffer
	done    chan struct{}
	stopped bool
}

// syncBuffer collects the daemon's stderr while the test reads it.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.String()
}

// freePort asks the kernel for a port, then gives it back. The daemon takes
// its address from a config file, so there is no way to let it pick one.
func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserving a port: %v", err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

// startDaemon writes cfg (a TOML body with %ADDR% replaced by a fresh
// 127.0.0.1 address) and runs beastied against it until the test ends.
func startDaemon(t *testing.T, cfg string) *daemon {
	t.Helper()
	addr := fmt.Sprintf("127.0.0.1:%d", freePort(t))
	return startDaemonAt(t, addr, strings.ReplaceAll(cfg, "%ADDR%", addr))
}

// startDaemonAt is startDaemon for a caller that needs a specific listen
// address — a wildcard bind, or a Unix socket path.
func startDaemonAt(t *testing.T, addr, cfg string) *daemon {
	t.Helper()
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "beastiemon.conf")
	if err := os.WriteFile(cfgPath, []byte(cfg), 0o644); err != nil {
		t.Fatalf("writing config: %v", err)
	}

	d := &daemon{
		t:       t,
		cfgPath: cfgPath,
		addr:    addr,
		logs:    &syncBuffer{},
		done:    make(chan struct{}),
	}
	d.client, d.baseURL = clientFor(addr)

	d.cmd = exec.Command(beastiedBin, "-config", cfgPath)
	d.cmd.Stdout = d.logs
	d.cmd.Stderr = d.logs
	if err := d.cmd.Start(); err != nil {
		t.Fatalf("starting beastied: %v", err)
	}
	go func() {
		d.cmd.Wait()
		close(d.done)
	}()
	t.Cleanup(d.stop)

	d.waitReady()
	return d
}

// clientFor builds the HTTP client for a TCP address or a Unix socket path.
func clientFor(addr string) (*http.Client, string) {
	if strings.HasPrefix(addr, "/") {
		sock := addr
		return &http.Client{
			Timeout: 10 * time.Second,
			Transport: &http.Transport{
				DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
					return (&net.Dialer{}).DialContext(ctx, "unix", sock)
				},
			},
		}, "http://beastied"
	}
	// A wildcard bind answers on loopback; that is where the test talks to it.
	if host, port, err := net.SplitHostPort(addr); err == nil && (host == "" || host == "0.0.0.0") {
		addr = net.JoinHostPort("127.0.0.1", port)
	}
	return &http.Client{Timeout: 10 * time.Second}, "http://" + addr
}

// waitReady blocks until /healthz answers, so a test never races the listener.
func (d *daemon) waitReady() {
	d.t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		select {
		case <-d.done:
			d.t.Fatalf("beastied exited during startup:\n%s", d.logs.String())
		default:
		}
		resp, err := d.client.Get(d.baseURL + "/healthz")
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	d.t.Fatalf("beastied never became ready:\n%s", d.logs.String())
}

// waitSample blocks until the first snapshot has been taken, which is when
// /api/metrics stops answering 503.
func (d *daemon) waitSample() { d.waitSampleWith(nil) }

// waitSampleWith is waitSample for a daemon with [auth] on, where an
// unauthenticated poll would sit at 401 until the deadline.
func (d *daemon) waitSampleWith(headers map[string]string) {
	d.t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		req, err := http.NewRequest(http.MethodGet, d.baseURL+"/api/metrics", nil)
		if err != nil {
			d.t.Fatal(err)
		}
		for k, v := range headers {
			req.Header.Set(k, v)
		}
		if resp, err := d.client.Do(req); err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	d.t.Fatalf("no sample arrived:\n%s", d.logs.String())
}

func (d *daemon) stop() {
	if d.stopped {
		return
	}
	d.stopped = true
	if d.cmd.Process != nil {
		d.cmd.Process.Signal(syscall.SIGTERM)
		select {
		case <-d.done:
		case <-time.After(10 * time.Second):
			d.cmd.Process.Kill()
			<-d.done
		}
	}
}

func (d *daemon) hup() {
	d.t.Helper()
	if err := d.cmd.Process.Signal(syscall.SIGHUP); err != nil {
		d.t.Fatalf("SIGHUP: %v", err)
	}
}

func (d *daemon) log() string { return d.logs.String() }

// waitLog polls the daemon's log for want, so a test can assert on something
// the daemon writes asynchronously without sleeping for a fixed time.
func (d *daemon) waitLog(want string, within time.Duration) bool {
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		if strings.Contains(d.log(), want) {
			return true
		}
		time.Sleep(50 * time.Millisecond)
	}
	return false
}

// get fetches path and returns the status and body.
func (d *daemon) get(path string) (int, []byte) {
	d.t.Helper()
	return d.getWith(path, nil)
}

func (d *daemon) getWith(path string, headers map[string]string) (int, []byte) {
	d.t.Helper()
	req, err := http.NewRequest(http.MethodGet, d.baseURL+path, nil)
	if err != nil {
		d.t.Fatalf("building request for %s: %v", path, err)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := d.client.Do(req)
	if err != nil {
		d.t.Fatalf("GET %s: %v", path, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		d.t.Fatalf("reading %s: %v", path, err)
	}
	return resp.StatusCode, body
}

// getJSON fetches path, requires 200, and unmarshals into v.
func (d *daemon) getJSON(path string, v any) {
	d.t.Helper()
	code, body := d.get(path)
	if code != http.StatusOK {
		d.t.Fatalf("GET %s: status %d, body %s", path, code, truncate(body))
	}
	if err := json.Unmarshal(body, v); err != nil {
		d.t.Fatalf("GET %s: %v in %s", path, err, truncate(body))
	}
}

// cli runs the beastie binary against this daemon's config and returns its
// stdout, stderr and exit code.
func (d *daemon) cli(args ...string) (stdout, stderr string, code int) {
	d.t.Helper()
	return runCLI(d.t, append([]string{"-config", d.cfgPath}, args...)...)
}

func runCLI(t *testing.T, args ...string) (stdout, stderr string, code int) {
	t.Helper()
	cmd := exec.Command(beastieBin, args...)
	var out, errb bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errb
	// NO_COLOR keeps the banner and escapes out of the comparisons; stdout is
	// not a terminal here anyway, but being explicit documents the intent.
	cmd.Env = append(os.Environ(), "NO_COLOR=1")
	err := cmd.Run()
	if ee, ok := err.(*exec.ExitError); ok {
		code = ee.ExitCode()
	} else if err != nil {
		t.Fatalf("running beastie %v: %v", args, err)
	}
	return out.String(), errb.String(), code
}

// daemonTest runs beastied -t against a config body and returns its output.
func daemonTestConfig(t *testing.T, cfg string) (output string, code int) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "beastiemon.conf")
	if err := os.WriteFile(path, []byte(cfg), 0o644); err != nil {
		t.Fatalf("writing config: %v", err)
	}
	cmd := exec.Command(beastiedBin, "-t", "-config", path)
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	err := cmd.Run()
	if ee, ok := err.(*exec.ExitError); ok {
		code = ee.ExitCode()
	} else if err != nil {
		t.Fatalf("running beastied -t: %v", err)
	}
	return out.String(), code
}

func truncate(b []byte) string {
	const max = 400
	if len(b) > max {
		return string(b[:max]) + "…"
	}
	return string(b)
}

// baseConfig is a fast-sampling daemon with nothing optional turned on.
func baseConfig() string {
	return `
[server]
listen = "%ADDR%"

[collect]
interval = "200ms"
ring_size = 600
fs_include = ["/"]
net_exclude = ["lo"]
top_procs = 5
`
}
