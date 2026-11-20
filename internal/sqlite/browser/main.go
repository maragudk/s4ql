// Command browser runs a Go program built with GOOS=js GOARCH=wasm and the tag s4ql_browser, such as
// a test binary, in a Worker of a page in headless Chromium, on the official SQLite WebAssembly build
// in this directory. It is made for the -exec flag of go test:
//
//	go build -o /tmp/s4ql-browser ./internal/sqlite/browser
//	GOOS=js GOARCH=wasm go test -tags s4ql_browser -exec /tmp/s4ql-browser ./internal/core
//
// It takes the path of the program and the program's arguments, serves the page, the program, and
// the JavaScript on a port of 127.0.0.1, starts Chromium on the page, writes what the program writes
// to its standard output and standard error to its own, and exits with the program's exit code, or
// with 3 if the page fails. The program's working directory is the root of the in-memory file system
// of fs.js, which is empty but for /tmp. It gets the environment variables of the runner whose names
// start with S4QL_, and GODEBUG and GOTRACEBACK, and no others.
//
// Chromium is the program in the environment variable S4QL_CHROMIUM, or else the first of chromium,
// chromium-browser, google-chrome, google-chrome-stable, and chrome on the PATH, or else Google Chrome
// or Chromium in /Applications on macOS. S4QL_CHROMIUM_FLAGS adds flags, separated by spaces, such as
// --no-sandbox where Chromium's sandbox cannot start.
//
// A Chromium that stops, or has not started the program a minute after it was started, before the
// program started, is started again, with a new profile, up to three times in all; no code of the
// program has run by then. Each time, the runner writes to its standard error which steps of the
// start it saw, and the end of Chromium's output.
//
// With -test.timeout among the arguments, as go test passes it, the runner stops Chromium 30 seconds
// after that timeout, in case the program cannot stop itself, which it cannot while SQLite holds the
// only thread. That is before go test stops the runner, a minute after the timeout. The runner then
// writes which steps it saw, when the program last wrote, and, if the program ran with -test.v, the
// tests that had started and not ended, with the end of Chromium's output. It does the same, and
// stops Chromium, when it gets SIGINT, SIGTERM, or SIGQUIT. With S4QL_CHROMIUM_STEPS set, it writes
// what it saw to its standard error when the program has exited, too.
package main

import (
	"bytes"
	"context"
	"embed"
	"encoding/json/v2"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

//go:embed index.html worker.js fs.js vfs.js bridge.js sqlite3.js sqlite3.wasm
var assets embed.FS

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM, syscall.SIGQUIT)
	r := runner{stdout: os.Stdout, stderr: os.Stderr, startup: time.Minute, grace: 30 * time.Second, attempts: 3}
	code, err := r.run(ctx, os.Args[1:])
	stop()
	if err != nil {
		fmt.Fprintln(os.Stderr, "browser:", err)
		os.Exit(3)
	}
	os.Exit(code)
}

// runner of a program in Chromium. A Chromium that stops, or has not started the program within
// startup, before the program started, is started again until it has been started attempts times.
// Chromium is stopped grace after the -test.timeout of the program.
type runner struct {
	stdout, stderr io.Writer
	startup, grace time.Duration
	attempts       int
}

// errNotStarted is the error of an attempt in which Chromium did not start the program in time.
var errNotStarted = errors.New("chromium did not start the program")

func (r runner) run(ctx context.Context, args []string) (int, error) {
	if len(args) == 0 {
		return 0, errors.New("usage: browser PROGRAM.wasm [ARGUMENT...]")
	}
	program, programArgs := args[0], args[1:]

	chromium, err := findChromium()
	if err != nil {
		return 0, err
	}
	wasmExec, err := wasmExecJS()
	if err != nil {
		return 0, err
	}
	config, err := json.Marshal(struct {
		Args []string          `json:"args"`
		Env  map[string]string `json:"env"`
	}{Args: programArgs, Env: programEnv(os.Environ())})
	if err != nil {
		return 0, err
	}

	// The deadline counts from the start of the runner, as the timeout of go test does.
	var deadline <-chan time.Time
	if timeout, ok := testTimeout(programArgs); ok {
		deadline = time.After(timeout + r.grace)
	}
	for attempt := 1; ; attempt++ {
		code, err := r.attempt(ctx, chromium, program, wasmExec, config, deadline)
		if !errors.Is(err, errNotStarted) {
			return code, err
		}
		if attempt == r.attempts || ctx.Err() != nil {
			return 0, fmt.Errorf("attempt %d of %d: %w", attempt, r.attempts, err)
		}
		_, _ = fmt.Fprintf(r.stderr, "browser: attempt %d of %d: %v\nbrowser: starting Chromium again\n", attempt, r.attempts, err)
	}
}

// attempt to run the program in a new Chromium, on a new port and with a new profile.
func (r runner) attempt(ctx context.Context, chromium, program string, wasmExec, config []byte, deadline <-chan time.Time) (int, error) {
	p := &progress{start: time.Now()}
	exited := make(chan int, 1)
	var exitOnce sync.Once
	exit := func(code int) { exitOnce.Do(func() { exited <- code }) }
	started := make(chan struct{})
	var startOnce sync.Once

	var output sync.Mutex
	mux := http.NewServeMux()
	mux.HandleFunc("GET /config", func(w http.ResponseWriter, req *http.Request) {
		p.mark("the Worker fetched its configuration")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(config)
	})
	mux.HandleFunc("GET /program.wasm", func(w http.ResponseWriter, req *http.Request) {
		p.mark("the Worker fetched the program")
		w.Header().Set("Content-Type", "application/wasm")
		http.ServeFile(w, req, program)
	})
	mux.HandleFunc("GET /wasm_exec.js", func(w http.ResponseWriter, req *http.Request) {
		w.Header().Set("Content-Type", "text/javascript")
		_, _ = w.Write(wasmExec)
	})
	mux.HandleFunc("POST /started", func(w http.ResponseWriter, req *http.Request) {
		startOnce.Do(func() {
			p.mark("the program started")
			close(started)
		})
	})
	mux.HandleFunc("POST /output", func(w http.ResponseWriter, req *http.Request) {
		out := r.stdout
		if req.URL.Query().Get("fd") == "2" {
			out = r.stderr
		}
		b, _ := io.ReadAll(req.Body)
		p.output(b)
		output.Lock()
		_, _ = out.Write(b)
		output.Unlock()
	})
	mux.HandleFunc("POST /exit", func(w http.ResponseWriter, req *http.Request) {
		if msg := req.URL.Query().Get("error"); msg != "" {
			_, _ = fmt.Fprintln(r.stderr, "browser: the page failed:", msg)
		}
		code, err := strconv.Atoi(req.URL.Query().Get("code"))
		if err != nil {
			code = 3
		}
		exit(code)
	})
	files := http.FileServerFS(assets)
	mux.HandleFunc("GET /", func(w http.ResponseWriter, req *http.Request) {
		switch req.URL.Path {
		case "/":
			p.mark("the page loaded")
		case "/worker.js":
			p.mark("the Worker loaded")
		}
		files.ServeHTTP(w, req)
	})

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	server := &http.Server{
		// The headers make the page cross-origin isolated, so that the VFS can wait with Atomics.wait
		// on a SharedArrayBuffer and need not spin.
		Handler: http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			w.Header().Set("Cross-Origin-Opener-Policy", "same-origin")
			w.Header().Set("Cross-Origin-Embedder-Policy", "require-corp")
			w.Header().Set("Cache-Control", "no-store")
			mux.ServeHTTP(w, req)
		}),
		ReadHeaderTimeout: time.Minute,
	}
	go func() { _ = server.Serve(listener) }()
	defer func() { _ = server.Close() }()

	profile, err := os.MkdirTemp("", "s4ql-browser-")
	if err != nil {
		return 0, err
	}
	defer func() { _ = os.RemoveAll(profile) }()

	chromiumCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	flags := []string{
		"--headless", "--no-first-run", "--no-default-browser-check", "--disable-extensions",
		"--disable-background-timer-throttling", "--disable-renderer-backgrounding",
		"--disable-backgrounding-occluded-windows", "--user-data-dir=" + profile,
	}
	flags = append(flags, strings.Fields(os.Getenv("S4QL_CHROMIUM_FLAGS"))...)
	flags = append(flags, "http://"+listener.Addr().String()+"/")
	cmd := exec.CommandContext(chromiumCtx, chromium, flags...)
	var log logBuffer
	cmd.Stdout = &log
	cmd.Stderr = &log
	if err := cmd.Start(); err != nil {
		return 0, err
	}
	p.mark("chromium started")
	// stopped is closed when Chromium has stopped, with the error of its stop in waitErr.
	stopped := make(chan struct{})
	var waitErr error
	go func() {
		waitErr = cmd.Wait()
		close(stopped)
	}()
	// stop Chromium, and describe what the runner saw, for an error.
	stop := func() string {
		cancel()
		<-stopped
		return p.report(time.Now()) + "\nthe end of Chromium's output:\n" + log.String()
	}

	startup := time.NewTimer(r.startup)
	defer startup.Stop()
	for {
		select {
		case code := <-exited:
			cancel()
			<-stopped
			if os.Getenv("S4QL_CHROMIUM_STEPS") != "" {
				_, _ = fmt.Fprintln(r.stderr, "browser:", p.report(time.Now()))
			}
			return code, nil
		case <-started:
			started = nil
			startup.Stop()
		case <-startup.C:
			return 0, fmt.Errorf("%w within %v:\n%s", errNotStarted, r.startup, stop())
		case <-stopped:
			err := fmt.Errorf("chromium stopped before the program exited: %v\n%s", waitErr, stop())
			if started != nil {
				err = fmt.Errorf("%w: %w", errNotStarted, err)
			}
			return 0, err
		case <-deadline:
			return 0, fmt.Errorf("the program did not exit within %v after its -test.timeout:\n%s", r.grace, stop())
		case <-ctx.Done():
			return 0, fmt.Errorf("stopped by a signal:\n%s", stop())
		}
	}
}

// findChromium to run, as the package comment says.
func findChromium() (string, error) {
	if p := os.Getenv("S4QL_CHROMIUM"); p != "" {
		return p, nil
	}
	for _, name := range []string{"chromium", "chromium-browser", "google-chrome", "google-chrome-stable", "chrome"} {
		if p, err := exec.LookPath(name); err == nil {
			return p, nil
		}
	}
	if runtime.GOOS == "darwin" {
		for _, p := range []string{
			"/Applications/Google Chrome.app/Contents/MacOS/Google Chrome",
			"/Applications/Chromium.app/Contents/MacOS/Chromium",
		} {
			if _, err := os.Stat(p); err == nil {
				return p, nil
			}
		}
	}
	return "", errors.New("no Chromium found; set S4QL_CHROMIUM to its path")
}

// wasmExecJS of the Go that runs this, which must be the one that built the program.
func wasmExecJS() ([]byte, error) {
	out, err := exec.Command("go", "env", "GOROOT").Output()
	if err != nil {
		return nil, fmt.Errorf("go env GOROOT: %w", err)
	}
	return os.ReadFile(filepath.Join(strings.TrimSpace(string(out)), "lib", "wasm", "wasm_exec.js"))
}

// programEnv of the variables in env, given as NAME=VALUE, that the program gets.
func programEnv(env []string) map[string]string {
	out := map[string]string{}
	for _, kv := range env {
		name, value, ok := strings.Cut(kv, "=")
		if !ok {
			continue
		}
		if strings.HasPrefix(name, "S4QL_CHROMIUM") {
			continue
		}
		if strings.HasPrefix(name, "S4QL_") || name == "GODEBUG" || name == "GOTRACEBACK" {
			out[name] = value
		}
	}
	return out
}

// testTimeout given by -test.timeout in the arguments of a test binary, if it is there and positive.
func testTimeout(args []string) (time.Duration, bool) {
	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	timeout := fs.Duration("test.timeout", 0, "")
	for i, a := range args {
		if a == "-test.timeout" || a == "--test.timeout" {
			if i+1 < len(args) {
				_ = fs.Parse(args[i : i+2])
			}
		} else if strings.HasPrefix(a, "-test.timeout=") || strings.HasPrefix(a, "--test.timeout=") {
			_ = fs.Parse([]string{a})
		}
	}
	return *timeout, *timeout > 0
}

// progress of a run, as the runner sees it: the steps of the start, when the program wrote, and the
// tests that had started and not ended, from the lines that go test -v writes. Its zero value is
// ready to use, with the start at the zero time.
type progress struct {
	mu          sync.Mutex
	start       time.Time
	steps       []string
	first, last time.Time
	line        []byte
	tests       []string
}

// mark a step of the start as done now.
func (p *progress) mark(step string) {
	p.markAt(step, time.Now())
}

// markAt marks a step as done at the given time.
func (p *progress) markAt(step string, at time.Time) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.steps = append(p.steps, step+" after "+since(p.start, at))
}

// output of the program, written now.
func (p *progress) output(b []byte) {
	p.outputAt(b, time.Now())
}

// outputAt takes output of the program, written at the given time.
func (p *progress) outputAt(b []byte, at time.Time) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(b) == 0 {
		return
	}
	if p.first.IsZero() {
		p.first = at
	}
	p.last = at
	p.line = append(p.line, b...)
	for {
		i := bytes.IndexByte(p.line, '\n')
		if i < 0 {
			break
		}
		p.take(string(p.line[:i]))
		p.line = p.line[i+1:]
	}
}

// take a whole line of output.
func (p *progress) take(line string) {
	if name, ok := strings.CutPrefix(line, "=== RUN   "); ok {
		p.tests = append(p.tests, strings.TrimSpace(name))
		return
	}
	line = strings.TrimLeft(line, " ")
	for _, prefix := range []string{"--- PASS: ", "--- FAIL: ", "--- SKIP: "} {
		if rest, ok := strings.CutPrefix(line, prefix); ok {
			name, _, _ := strings.Cut(rest, " ")
			if i := slices.Index(p.tests, name); i >= 0 {
				p.tests = slices.Delete(p.tests, i, i+1)
			}
			return
		}
	}
}

// running tests: those that had started and not ended, in the order they started.
func (p *progress) running() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return slices.Clone(p.tests)
}

// report of the progress, at the given time.
func (p *progress) report(now time.Time) string {
	p.mu.Lock()
	defer p.mu.Unlock()
	var b strings.Builder
	b.WriteString("what the runner saw: ")
	if len(p.steps) == 0 {
		b.WriteString("no step of the start")
	}
	b.WriteString(strings.Join(p.steps, ", "))
	if p.first.IsZero() {
		b.WriteString("; no output from the program")
	} else {
		fmt.Fprintf(&b, "; the first output after %v, the last output after %v, %v ago",
			since(p.start, p.first), since(p.start, p.last), since(p.last, now))
	}
	if len(p.tests) == 0 {
		b.WriteString("\nno test had started and not ended, or the program ran without -test.v")
	} else {
		b.WriteString("\nthe tests that had started and not ended: " + strings.Join(p.tests, ", "))
	}
	return b.String()
}

// since from to at, rounded for a person to read.
func since(from, at time.Time) string {
	d := at.Sub(from)
	if d < time.Second {
		return d.Round(time.Millisecond).String()
	}
	return d.Round(100 * time.Millisecond).String()
}

// logBuffer keeps the last 64 KiB written to it, for the output of Chromium.
type logBuffer struct {
	mu sync.Mutex
	b  []byte
}

func (l *logBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.b = append(l.b, p...)
	if len(l.b) > 64<<10 {
		l.b = l.b[len(l.b)-64<<10:]
	}
	return len(p), nil
}

func (l *logBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return string(l.b)
}
