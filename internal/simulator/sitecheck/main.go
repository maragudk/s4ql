//go:build s4ql_browser

// Command sitecheck checks the simulator's page of the project site in headless Chromium, as GitHub
// Pages serves it: without the headers of cross-origin isolation. It serves the site from -root,
// which must hold the engine that make site builds into site/simulator/engine, and opens check.html,
// which loads the page in frames and drives it as a visitor would:
//
//	go run -tags s4ql_browser ./internal/simulator/sitecheck -root site
//
// The check passes if the page loads, the engine becomes ready, the default run has events, a crash
// added at a time changes the run from that time on and not before it, the link of the page gives the
// same run with the same hash, and a link of another format gives the default run. It writes what it
// saw, and exits with 0 if the check passed, 1 if it failed, and 2 if it could not run.
//
// Nothing in the check waits for a time: it waits for what the page says through data attributes.
// -timeout only stops a check that hangs, and is far above what a run takes on a slow machine.
//
// Chromium is found as the runner in internal/sqlite/browser finds it: S4QL_CHROMIUM, or the PATH, or
// /Applications on macOS. S4QL_CHROMIUM_FLAGS adds flags, such as --no-sandbox.
package main

import (
	"context"
	_ "embed"
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
	"strings"
	"time"
)

//go:embed check.html
var checkHTML []byte

func main() {
	root := flag.String("root", "site", "the directory of the site, with the engine built into it")
	timeout := flag.Duration("timeout", 10*time.Minute, "the time after which a check that hangs is stopped")
	flag.Parse()
	ok, err := run(*root, *timeout)
	if err != nil {
		fmt.Fprintln(os.Stderr, "sitecheck:", err)
		os.Exit(2)
	}
	if !ok {
		fmt.Println("sitecheck: FAIL")
		os.Exit(1)
	}
	fmt.Println("sitecheck: ok")
}

// result that check.html posts.
type result struct {
	OK    bool     `json:"ok"`
	Lines []string `json:"lines"`
}

func run(root string, timeout time.Duration) (bool, error) {
	if _, err := os.Stat(filepath.Join(root, "simulator", "engine", "engine.wasm")); err != nil {
		return false, fmt.Errorf("the site in %v has no engine; run make site first: %w", root, err)
	}
	chromium, err := findChromium()
	if err != nil {
		return false, err
	}

	results := make(chan result, 1)
	mux := http.NewServeMux()
	mux.HandleFunc("GET /__check/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write(checkHTML)
	})
	mux.HandleFunc("POST /__check/result", func(w http.ResponseWriter, r *http.Request) {
		var res result
		b, err := io.ReadAll(r.Body)
		if err == nil {
			err = json.Unmarshal(b, &res)
		}
		if err != nil {
			res = result{Lines: []string{"the result could not be read: " + err.Error()}}
		}
		select {
		case results <- res:
		default:
		}
	})
	files := http.FileServer(http.Dir(root))
	mux.Handle("GET /", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		files.ServeHTTP(w, r)
	}))

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return false, err
	}
	server := &http.Server{Handler: mux, ReadHeaderTimeout: time.Minute}
	go func() { _ = server.Serve(listener) }()
	defer func() { _ = server.Close() }()

	profile, err := os.MkdirTemp("", "s4ql-sitecheck-")
	if err != nil {
		return false, err
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
	flags = append(flags, "http://"+listener.Addr().String()+"/__check/")
	cmd := exec.CommandContext(ctx, chromium, flags...)
	var log strings.Builder
	cmd.Stdout = &log
	cmd.Stderr = &log
	if err := cmd.Start(); err != nil {
		return false, err
	}
	// stopped is closed when Chromium has stopped, with the error of its stop in waitErr.
	stopped := make(chan struct{})
	var waitErr error
	go func() {
		waitErr = cmd.Wait()
		close(stopped)
	}()

	select {
	case res := <-results:
		cancel()
		<-stopped
		for _, l := range res.Lines {
			fmt.Println(l)
		}
		return res.OK, nil
	case <-stopped:
		return false, fmt.Errorf("chromium stopped before the check ended: %v\n%s", waitErr, tail(log.String()))
	case <-time.After(timeout):
		cancel()
		<-stopped
		return false, fmt.Errorf("the check did not end within %v", timeout)
	}
}

// tail of the output of Chromium, its last 64 KiB.
func tail(s string) string {
	if len(s) > 64<<10 {
		return s[len(s)-64<<10:]
	}
	return s
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
