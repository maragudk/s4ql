//go:build s4ql_browser

package main

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"maragu.dev/is"
)

// TestMain makes the test binary a fake Chromium when S4QL_CHROMIUM_FAKE names what it does: it
// then takes the check's URL as its last argument and speaks to sitecheck as check.html would.
func TestMain(m *testing.M) {
	if mode := os.Getenv("S4QL_CHROMIUM_FAKE"); mode != "" {
		fakeChromium(mode, os.Args[len(os.Args)-1])
	}
	os.Exit(m.Run())
}

// fakeChromium in the given mode:
//   - hang: never ask for anything;
//   - pass: load the check's page, ask for every file of the simulator, post a result that passed,
//     and wait to be stopped, as Chromium does;
//   - crash-once: exit with code 1 at once if the file in S4QL_CHROMIUM_FAKE_MARK is not there, and
//     create it, and else do as pass.
func fakeChromium(mode, page string) {
	get := func(u string) {
		res, err := http.Get(u)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(2)
		}
		_, _ = io.Copy(io.Discard, res.Body)
		_ = res.Body.Close()
	}
	if mode == "crash-once" {
		mark := os.Getenv("S4QL_CHROMIUM_FAKE_MARK")
		if _, err := os.Stat(mark); err != nil {
			_ = os.WriteFile(mark, nil, 0o600)
			fmt.Fprintln(os.Stderr, "the fake crashed")
			os.Exit(1)
		}
		mode = "pass"
	}
	switch mode {
	case "hang":
		time.Sleep(time.Hour)
	case "pass":
		u, err := url.Parse(page)
		if err != nil {
			os.Exit(2)
		}
		get(page)
		for _, f := range simulatorFiles {
			get(u.Scheme + "://" + u.Host + f)
		}
		res, err := http.Post(u.Scheme+"://"+u.Host+"/__check/result", "application/json", strings.NewReader(`{"ok":true,"lines":["ok   the fake passed"]}`))
		if err != nil {
			os.Exit(2)
		}
		_ = res.Body.Close()
		time.Sleep(time.Hour)
	}
	os.Exit(2)
}

// fakeSite makes the test binary the Chromium of sitecheck, in the given mode of [fakeChromium], and
// returns the root of a site with an engine.
func fakeSite(t *testing.T, mode string) string {
	t.Helper()
	exe, err := os.Executable()
	is.NotError(t, err)
	t.Setenv("S4QL_CHROMIUM", exe)
	t.Setenv("S4QL_CHROMIUM_FLAGS", "")
	t.Setenv("S4QL_CHROMIUM_FAKE", mode)
	t.Setenv("S4QL_CHROMIUM_FAKE_MARK", filepath.Join(t.TempDir(), "mark"))
	root := t.TempDir()
	is.NotError(t, os.MkdirAll(filepath.Join(root, "simulator", "engine"), 0o755))
	is.NotError(t, os.WriteFile(filepath.Join(root, "simulator", "engine", "engine.wasm"), nil, 0o644))
	return root
}

// syncBuilder is a strings.Builder that several goroutines may write to.
type syncBuilder struct {
	mu sync.Mutex
	b  strings.Builder
}

func (s *syncBuilder) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuilder) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

func TestRun(t *testing.T) {
	t.Run("should pass the check that the page passed", func(t *testing.T) {
		root := fakeSite(t, "pass")
		var stderr syncBuilder

		ok, err := run(root, "", time.Hour, starts{first: time.Minute, later: time.Minute, attempts: 1}, &stderr)
		is.NotError(t, err)
		is.True(t, ok)
		is.Equal(t, "", stderr.String())
	})

	t.Run("should start Chromium again when it did not load the check's page in time, and fail after the last attempt", func(t *testing.T) {
		root := fakeSite(t, "hang")
		var stderr syncBuilder

		_, err := run(root, "", time.Hour, starts{first: 10 * time.Millisecond, later: 20 * time.Millisecond, attempts: 2}, &stderr)
		is.True(t, err != nil)
		is.True(t, strings.Contains(err.Error(), "attempt 2 of 2: chromium did not load the check's page within 20ms"), err)
		is.True(t, strings.Contains(stderr.String(), "sitecheck: attempt 1 of 2: chromium did not load the check's page within 10ms"), stderr.String())
		is.True(t, strings.Contains(stderr.String(), "sitecheck: starting Chromium again"), stderr.String())
	})

	t.Run("should start Chromium again when it stopped before it loaded the check's page", func(t *testing.T) {
		root := fakeSite(t, "crash-once")
		var stderr syncBuilder

		ok, err := run(root, "", time.Hour, starts{first: time.Minute, later: time.Minute, attempts: 2}, &stderr)
		is.NotError(t, err)
		is.True(t, ok)
		is.True(t, strings.Contains(stderr.String(), "sitecheck: attempt 1 of 2: chromium stopped before it loaded the check's page"), stderr.String())
		is.True(t, strings.Contains(stderr.String(), "the fake crashed"), stderr.String())
	})
}

func TestCheckRequests(t *testing.T) {
	// at gives the requests for every file of the simulator with the query, and for the page, a
	// font, and the check's own page, which need none.
	at := func(query string) []url.URL {
		us := []url.URL{{Path: "/simulator/"}, {Path: "/fonts/commit-mono-400.woff2"}, {Path: "/__check/", RawQuery: "version=3f6fbfd1"}}
		for _, f := range simulatorFiles {
			us = append(us, url.URL{Path: f, RawQuery: query})
		}
		return us
	}

	t.Run("should pass if every file of the simulator is asked for at the version", func(t *testing.T) {
		ok, lines := checkRequests(at("v=3f6fbfd1"), "3f6fbfd1")
		is.True(t, ok, strings.Join(lines, "\n"))
	})

	t.Run("should pass if every file of the simulator is asked for without a query and without a version", func(t *testing.T) {
		ok, lines := checkRequests(at(""), "")
		is.True(t, ok, strings.Join(lines, "\n"))
	})

	t.Run("should fail if a file of the simulator is asked for without the version", func(t *testing.T) {
		for _, f := range simulatorFiles {
			requests := append(at("v=3f6fbfd1"), url.URL{Path: f})
			ok, lines := checkRequests(requests, "3f6fbfd1")
			is.True(t, !ok, f)
			is.True(t, strings.Contains(lines[0], "FAIL") && strings.Contains(lines[0], f), lines[0])
		}
	})

	t.Run("should fail if a file of the simulator is asked for at another version", func(t *testing.T) {
		requests := append(at("v=3f6fbfd1"), url.URL{Path: "/simulator/engine/engine.wasm", RawQuery: "v=0000000"})
		ok, _ := checkRequests(requests, "3f6fbfd1")
		is.True(t, !ok)
	})

	t.Run("should fail if a file of the simulator is asked for with a query without a version", func(t *testing.T) {
		ok, _ := checkRequests(at("v=3f6fbfd1"), "")
		is.True(t, !ok)
	})

	t.Run("should fail if a file outside the list is asked for without the version", func(t *testing.T) {
		requests := append(at("v=3f6fbfd1"), url.URL{Path: "/simulator/engine/sqlite3-opfs-async-proxy.js", RawQuery: "vfs=opfs"})
		ok, _ := checkRequests(requests, "3f6fbfd1")
		is.True(t, !ok)
	})

	t.Run("should fail if a file of the simulator is not asked for", func(t *testing.T) {
		for i, f := range simulatorFiles {
			requests := at("v=3f6fbfd1")
			requests = append(requests[:3+i:3+i], requests[3+i+1:]...)
			ok, lines := checkRequests(requests, "3f6fbfd1")
			is.True(t, !ok, f)
			is.True(t, strings.Contains(lines[1], "FAIL") && strings.Contains(lines[1], f), lines[1])
		}
	})
}

func TestBroken(t *testing.T) {
	root := t.TempDir()
	is.NotError(t, os.MkdirAll(filepath.Join(root, "simulator"), 0o755))
	is.NotError(t, os.WriteFile(filepath.Join(root, "simulator", "index.html"), []byte(`<button id="play"><span id="play-word">Play</span></button>`), 0o644))
	is.NotError(t, os.WriteFile(filepath.Join(root, "simulator", "run.js"), []byte(`export const x = 1;`), 0o644))
	mux := http.NewServeMux()
	mux.Handle("GET /__check/broken/{how}/{path...}", broken(root))

	get := func(path string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, httptest.NewRequest(http.MethodGet, path, nil))
		return w
	}

	t.Run("should serve the site without run.js for no-run", func(t *testing.T) {
		is.Equal(t, http.StatusNotFound, get("/__check/broken/no-run/simulator/run.js").Code)
		w := get("/__check/broken/no-run/simulator/")
		is.Equal(t, http.StatusOK, w.Code)
		is.True(t, strings.Contains(w.Body.String(), `id="play"`))
	})

	t.Run("should serve the page without the ID of its Play button for no-play", func(t *testing.T) {
		w := get("/__check/broken/no-play/simulator/")
		is.Equal(t, http.StatusOK, w.Code)
		is.Equal(t, `<button id="no-play"><span id="play-word">Play</span></button>`, w.Body.String())
		w = get("/__check/broken/no-play/simulator/run.js")
		is.Equal(t, http.StatusOK, w.Code)
		is.Equal(t, `export const x = 1;`, w.Body.String())
	})

	t.Run("should serve nothing for another fault", func(t *testing.T) {
		is.Equal(t, http.StatusNotFound, get("/__check/broken/other/simulator/").Code)
	})
}
