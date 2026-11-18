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
// With -test.timeout among the arguments, as go test passes it, the runner stops Chromium a minute
// after that timeout, in case the program cannot stop itself, which it cannot while SQLite holds the
// only thread.
package main

import (
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
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"
)

//go:embed index.html worker.js fs.js vfs.js bridge.js sqlite3.js sqlite3.wasm
var assets embed.FS

func main() {
	code, err := run(os.Args[1:])
	if err != nil {
		fmt.Fprintln(os.Stderr, "browser:", err)
		os.Exit(3)
	}
	os.Exit(code)
}

func run(args []string) (int, error) {
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

	exited := make(chan int, 1)
	var once sync.Once
	exit := func(code int) { once.Do(func() { exited <- code }) }
	var output sync.Mutex
	mux := http.NewServeMux()
	mux.HandleFunc("GET /config", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(config)
	})
	mux.HandleFunc("GET /program.wasm", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/wasm")
		http.ServeFile(w, r, program)
	})
	mux.HandleFunc("GET /wasm_exec.js", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/javascript")
		_, _ = w.Write(wasmExec)
	})
	mux.HandleFunc("POST /output", func(w http.ResponseWriter, r *http.Request) {
		out := os.Stdout
		if r.URL.Query().Get("fd") == "2" {
			out = os.Stderr
		}
		output.Lock()
		_, _ = io.Copy(out, r.Body)
		output.Unlock()
	})
	mux.HandleFunc("POST /exit", func(w http.ResponseWriter, r *http.Request) {
		if msg := r.URL.Query().Get("error"); msg != "" {
			fmt.Fprintln(os.Stderr, "browser: the page failed:", msg)
		}
		code, err := strconv.Atoi(r.URL.Query().Get("code"))
		if err != nil {
			code = 3
		}
		exit(code)
	})
	mux.Handle("GET /", http.FileServerFS(assets))

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	server := &http.Server{
		// The headers make the page cross-origin isolated, so that the VFS can wait with Atomics.wait
		// on a SharedArrayBuffer and need not spin.
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Cross-Origin-Opener-Policy", "same-origin")
			w.Header().Set("Cross-Origin-Embedder-Policy", "require-corp")
			w.Header().Set("Cache-Control", "no-store")
			mux.ServeHTTP(w, r)
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

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	flags := []string{
		"--headless", "--no-first-run", "--no-default-browser-check", "--disable-extensions",
		"--disable-background-timer-throttling", "--disable-renderer-backgrounding",
		"--disable-backgrounding-occluded-windows", "--user-data-dir=" + profile,
	}
	flags = append(flags, strings.Fields(os.Getenv("S4QL_CHROMIUM_FLAGS"))...)
	flags = append(flags, "http://"+listener.Addr().String()+"/")
	cmd := exec.CommandContext(ctx, chromium, flags...)
	var log logBuffer
	cmd.Stdout = &log
	cmd.Stderr = &log
	if err := cmd.Start(); err != nil {
		return 0, err
	}
	stopped := make(chan error, 1)
	go func() { stopped <- cmd.Wait() }()

	var deadline <-chan time.Time
	if timeout, ok := testTimeout(programArgs); ok {
		deadline = time.After(timeout + time.Minute)
	}
	select {
	case code := <-exited:
		cancel()
		<-stopped
		return code, nil
	case err := <-stopped:
		return 0, fmt.Errorf("chromium stopped before the program exited: %v\n%s", err, log.String())
	case <-deadline:
		cancel()
		<-stopped
		return 0, errors.New("the program did not exit within a minute after its -test.timeout")
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
