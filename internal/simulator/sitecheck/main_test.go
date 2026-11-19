//go:build s4ql_browser

package main

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"maragu.dev/is"
)

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
