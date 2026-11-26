//go:build s4ql_browser

// Command sitecheck checks the simulator's page of the project site in headless Chromium, as GitHub
// Pages serves it: without the headers of cross-origin isolation. It serves the site from -root,
// which must hold the engine that make site builds into site/simulator/engine, and opens check.html,
// which loads the page in a frame and drives it as a visitor would:
//
//	go run -tags s4ql_browser ./internal/simulator/sitecheck -root site
//
// The check passes if the engine becomes ready and the page waits at zero until Play; Play advances
// fake time by the real time, turbo by five times it, and the log and the commit objects grow; Pause
// holds fake time; when the browser suspends the audio context while sound is on, the stick figure
// on the frame of the log stops dancing, and the page asks to resume it when its state changes, at
// the next sound, key, and click;
// the client calls a from the start, and b gets no call; each action on an instance has its effect
// in the state, and an action switched off again takes it away; the client keeps
// calling a while a is crashed and after its restart, and its switch moves its calls to b and back; a
// crash of the instance that serves lets the other take over; fake time goes
// past 10 minutes; and the page writes nothing into its address. The box for SQL, while paused,
// selects from barcodes, creates a table, inserts, selects with order by and limit, shows an error of
// the engine and the refusal of a write to barcodes, moves fake time only until each answer, and lets
// a write wait while the connection to S3 hangs. Then S3: the page stores nothing of it, and a reload
// starts on an empty S3 and the box as at the first visit; Reset empties the database; and the page
// stops at the cap of commit objects, and Reset starts over from there. The page remembers Turbo and
// Sound in localStorage across reloads, and Reset keeps them; Sound remembered on waits, without an
// audio context, until the first click or key on the page; and storage that throws, or holds what
// the page did not write, gives the toggles of a first visit. It also checks the words of run.js,
// and that a fault at start-up ends in the page's failure box: the page without run.js, and the page
// without its Play button, as a page.js of one deploy would find the HTML of another. It writes what
// it saw, and exits with 0 if the check passed, 1 if it failed, and 2 if it could not run.
//
// -version is the version that the site's page was built with, as the Pages workflow builds it with
// make site SITEVERSION: see the package comment of internal/simulator/serve. The server records the
// requests of the page, and the check fails unless the page asked for every file of the simulator,
// and every request for a script, the Worker, WebAssembly, or build.json carries the query
// ?v=<version>, or no query without -version.
//
//	go run -tags s4ql_browser ./internal/simulator/sitecheck -root /tmp/site -version 3f6fbfd1
//
// The page picks a random seed for each visit, and every step of the check holds for any seed. To go
// on in fake time without waiting real minutes, the check calls the page's hook s4qlPage.skip, which
// advances the session at once. Nothing in the check waits for a time: it waits for what the page
// says through data attributes and its state, or for animation frames. -timeout only stops a check
// that hangs, and is about three times what a check takes at the most in CI. On a timeout, it prints
// the stacks of its goroutines, the requests of the page so far, and the output of Chromium.
//
// Chromium is found as the runner in internal/sqlite/browser finds it: S4QL_CHROMIUM, or the PATH, or
// /Applications on macOS. S4QL_CHROMIUM_FLAGS adds flags, such as --no-sandbox. As that runner does,
// it starts Chromium again, with a new profile, up to six times in all, when Chromium has not loaded
// the check's page a minute after its start, or 30 seconds after a later start, or stops before it
// did. On a fresh runner of GitHub Actions, a Chromium sometimes never loads its page, at times
// several in a row over the first minutes, while a later one does. -timeout counts from the load of
// the check's page.
package main

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/json/v2"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"runtime"
	"runtime/pprof"
	"strings"
	"sync"
	"time"
)

// starts of Chromium: the time that the first has to load the check's page, the time that each later
// one has, and how many there are at most.
type starts struct {
	first, later time.Duration
	attempts     int
}

//go:embed check.html
var checkHTML []byte

func main() {
	root := flag.String("root", "site", "the directory of the site, with the engine built into it")
	version := flag.String("version", "", "the version that the site's page was built with, if any")
	timeout := flag.Duration("timeout", 3*time.Minute, "the time after which a check that hangs is stopped")
	flag.Parse()
	ok, err := run(*root, *version, *timeout, starts{first: time.Minute, later: 30 * time.Second, attempts: 6}, os.Stderr)
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

// run the check, and write about each start of Chromium that failed to stderr.
func run(root, version string, timeout time.Duration, s starts, stderr io.Writer) (bool, error) {
	if _, err := os.Stat(filepath.Join(root, "simulator", "engine", "engine.wasm")); err != nil {
		return false, fmt.Errorf("the site in %v has no engine; run make site first: %w", root, err)
	}
	chromium, err := findChromium()
	if err != nil {
		return false, err
	}

	results := make(chan result, 1)
	var (
		mu       sync.Mutex
		requests []url.URL     // of the site, outside /__check/, by the Chromium of this start
		loaded   chan struct{} // closed when the Chromium of this start asks for the check's page
	)
	mux := http.NewServeMux()
	mux.HandleFunc("GET /__check/", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		if loaded != nil {
			close(loaded)
			loaded = nil
		}
		mu.Unlock()
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
	mux.Handle("GET /__check/broken/{how}/{path...}", broken(root))
	files := http.FileServer(http.Dir(root))
	mux.Handle("GET /", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		requests = append(requests, *r.URL)
		mu.Unlock()
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

	page := "http://" + listener.Addr().String() + "/__check/?" + url.Values{"version": {version}}.Encode()
	var c *browser
	for attempt := 1; ; attempt++ {
		limit := s.first
		if attempt > 1 {
			limit = s.later
		}
		ch := make(chan struct{})
		mu.Lock()
		requests, loaded = nil, ch
		mu.Unlock()
		c, err = startBrowser(chromium, page)
		if err != nil {
			return false, err
		}
		defer c.stop()
		var why string
		select {
		case <-ch:
		case <-c.stopped:
			why = fmt.Sprintf("chromium stopped before it loaded the check's page: %v", c.waitErr)
			// It may have asked for the page just before it stopped.
			select {
			case <-ch:
				why = ""
			default:
			}
		case <-time.After(limit):
			why = fmt.Sprintf("chromium did not load the check's page within %v", limit)
		}
		if why == "" {
			break
		}
		c.stop()
		msg := fmt.Sprintf("attempt %d of %d: %v; chromium started after %v:\nthe end of Chromium's output:\n%v",
			attempt, s.attempts, why, c.startedAfter, tail(c.log.String()))
		if attempt >= s.attempts {
			return false, errors.New(msg)
		}
		_, _ = fmt.Fprintf(stderr, "sitecheck: %v\nsitecheck: starting Chromium again\n", msg)
	}

	select {
	case res := <-results:
		c.stop()
		mu.Lock()
		ok, lines := checkRequests(requests, version)
		mu.Unlock()
		for _, l := range append(res.Lines, lines...) {
			fmt.Println(l)
		}
		return res.OK && ok, nil
	case <-c.stopped:
		return false, fmt.Errorf("chromium stopped before the check ended: %v\n%s", c.waitErr, tail(c.log.String()))
	case <-time.After(timeout):
		// For a hang: the stacks of this program's goroutines, taken before Chromium is stopped, the
		// requests of the page so far, and the output of Chromium.
		var stacks strings.Builder
		_ = pprof.Lookup("goroutine").WriteTo(&stacks, 2)
		c.stop()
		mu.Lock()
		var asked strings.Builder
		for _, u := range requests {
			fmt.Fprintln(&asked, u.String())
		}
		mu.Unlock()
		return false, fmt.Errorf("the check did not end within %v\n\nthe goroutines of sitecheck:\n%v\nthe requests of the page, in order:\n%v\nthe output of Chromium:\n%v",
			timeout, stacks.String(), asked.String(), tail(c.log.String()))
	}
}

// browser is a Chromium that runs on a page, with a profile of its own.
type browser struct {
	cancel       context.CancelFunc
	profile      string
	log          strings.Builder
	startedAfter time.Duration
	stopped      chan struct{} // closed when Chromium has stopped, with the error of its stop in waitErr
	waitErr      error
	once         sync.Once
}

// startBrowser starts the Chromium at the path on the page, with a new profile.
func startBrowser(chromium, page string) (*browser, error) {
	start := time.Now()
	profile, err := os.MkdirTemp("", "s4ql-sitecheck-")
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(context.Background())
	b := &browser{cancel: cancel, profile: profile, stopped: make(chan struct{})}
	flags := []string{
		"--headless", "--no-first-run", "--no-default-browser-check", "--disable-extensions",
		"--disable-background-timer-throttling", "--disable-renderer-backgrounding",
		"--disable-backgrounding-occluded-windows", "--user-data-dir=" + profile,
		// The page plays its run in animation frames, which Chromium does not run for a frame out of
		// sight, so the window holds the frame of check.html whole.
		"--window-size=1600,1200",
		// The check turns the page's sound on with a click of a script, which is no gesture of a user,
		// and the page's dancer dances only while its audio context runs.
		"--autoplay-policy=no-user-gesture-required",
	}
	flags = append(flags, strings.Fields(os.Getenv("S4QL_CHROMIUM_FLAGS"))...)
	flags = append(flags, page)
	cmd := exec.CommandContext(ctx, chromium, flags...)
	w := &syncWriter{w: &b.log}
	cmd.Stdout = w
	cmd.Stderr = w
	// The processes that Chromium starts inherit its output, and may outlive it.
	cmd.WaitDelay = 5 * time.Second
	if err := cmd.Start(); err != nil {
		cancel()
		_ = os.RemoveAll(profile)
		return nil, err
	}
	b.startedAfter = time.Since(start).Round(time.Millisecond)
	go func() {
		b.waitErr = cmd.Wait()
		close(b.stopped)
	}()
	return b, nil
}

// stop Chromium, wait until it has stopped, and remove its profile. Calls after the first do nothing.
func (b *browser) stop() {
	b.once.Do(func() {
		b.cancel()
		<-b.stopped
		_ = os.RemoveAll(b.profile)
	})
}

// syncWriter writes to w under a lock, for the output of Chromium, which two pipes copy.
type syncWriter struct {
	mu sync.Mutex
	w  io.Writer
}

func (s *syncWriter) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.w.Write(p)
}

// simulatorFiles that the page loads: page.js, which its HTML names, run.js and sound.js, which
// page.js loads, and the files of the engine, which page.js, simulator.js, and the Worker load.
var simulatorFiles = []string{
	"/simulator/page.js",
	"/simulator/run.js",
	"/simulator/sound.js",
	"/simulator/engine/build.json",
	"/simulator/engine/simulator.js",
	"/simulator/engine/worker.js",
	"/simulator/engine/sqlite3.js",
	"/simulator/engine/vfs.js",
	"/simulator/engine/bridge.js",
	"/simulator/engine/fs.js",
	"/simulator/engine/wasm_exec.js",
	"/simulator/engine/sqlite3.wasm",
	"/simulator/engine/engine.wasm",
}

// checkRequests says whether the requests of the site load the simulator at the version: every
// request under /simulator/ for a script, WebAssembly, or JSON has the query v=<version>, or no
// query if the version is empty, and each of [simulatorFiles] was asked for. It gives the lines of
// what it found, in the form of those of check.html.
func checkRequests(requests []url.URL, version string) (bool, []string) {
	query := ""
	if version != "" {
		query = url.Values{"v": {version}}.Encode()
	}
	asked := map[string]bool{}
	var wrong []string
	for _, u := range requests {
		if !strings.HasPrefix(u.Path, "/simulator/") {
			continue
		}
		switch path.Ext(u.Path) {
		case ".js", ".wasm", ".json":
		default:
			continue
		}
		asked[u.Path] = true
		if u.RawQuery != query {
			wrong = append(wrong, u.String())
		}
	}
	var missing []string
	for _, f := range simulatorFiles {
		if !asked[f] {
			missing = append(missing, f)
		}
	}

	var lines []string
	if len(wrong) == 0 {
		lines = append(lines, fmt.Sprintf("ok   every request for a file of the simulator has the query %q", query))
	} else {
		lines = append(lines, fmt.Sprintf("FAIL requests for files of the simulator without the query %q: %v", query, strings.Join(wrong, ", ")))
	}
	if len(missing) == 0 {
		lines = append(lines, fmt.Sprintf("ok   the page asks for each of the %v files of the simulator", len(simulatorFiles)))
	} else {
		lines = append(lines, "FAIL the page does not ask for "+strings.Join(missing, ", "))
	}
	return len(wrong) == 0 && len(missing) == 0, lines
}

// broken serves the site under /__check/broken/<how>/, with a fault: with how "no-run", run.js is not
// found, and with "no-play", the page's Play button has lost its ID, which make the page fail at
// start-up; with "no-storage", the page's sessionStorage and localStorage throw, as when the browser
// blocks the site's data. With "audio", the page is whole, and counts the audio contexts that it
// makes in window.s4qlContexts, from before its own scripts run.
func broken(root string) http.Handler {
	files := http.FileServer(http.Dir(root))
	// heads are the scripts that a variant puts first in the page's head.
	heads := map[string]string{
		"no-storage": `<script>for (const name of ["sessionStorage", "localStorage"]) Object.defineProperty(window, name, { get() { throw new DOMException("The storage is blocked.", "SecurityError"); } });</script>`,
		"audio":      `<script>window.s4qlContexts = 0; window.AudioContext = class extends window.AudioContext { constructor(...a) { super(...a); window.s4qlContexts++; } };</script>`,
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		how, p := r.PathValue("how"), "/"+r.PathValue("path")
		w.Header().Set("Cache-Control", "no-store")
		_, head := heads[how]
		switch {
		case how != "no-run" && how != "no-play" && !head, how == "no-run" && p == "/simulator/run.js":
			http.NotFound(w, r)
			return
		case (how == "no-play" || head) && p == "/simulator/":
			b, err := os.ReadFile(filepath.Join(root, "simulator", "index.html"))
			if err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			if head {
				b = bytes.Replace(b, []byte(`<head>`), []byte(`<head>`+heads[how]), 1)
			} else {
				b = bytes.Replace(b, []byte(`id="play"`), []byte(`id="no-play"`), 1)
			}
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			_, _ = w.Write(b)
			return
		}
		r = r.Clone(r.Context())
		r.URL.Path, r.URL.RawPath = p, ""
		files.ServeHTTP(w, r)
	})
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
