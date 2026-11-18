//go:build s4ql_browser

package main

import (
	"encoding/json/v2"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"maragu.dev/is"
)

func TestRun(t *testing.T) {
	t.Run("should refuse to run without -out or -addr", func(t *testing.T) {
		err := run(options{})
		is.True(t, err != nil)
		is.Equal(t, "give -out, -addr, or both", err.Error())
	})

	t.Run("should refuse -root without -addr", func(t *testing.T) {
		err := run(options{out: t.TempDir(), root: t.TempDir()})
		is.True(t, err != nil)
		is.Equal(t, "-root needs -addr", err.Error())
	})
}

func TestHandler(t *testing.T) {
	root := t.TempDir()
	is.NotError(t, os.WriteFile(filepath.Join(root, "engine.wasm"), []byte("\x00asm"), 0o644))

	t.Run("should serve with the headers of cross-origin isolation if isolated", func(t *testing.T) {
		w := httptest.NewRecorder()
		handler(root, true).ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/engine.wasm", nil))
		is.Equal(t, http.StatusOK, w.Code)
		is.Equal(t, "same-origin", w.Header().Get("Cross-Origin-Opener-Policy"))
		is.Equal(t, "require-corp", w.Header().Get("Cross-Origin-Embedder-Policy"))
		is.Equal(t, "no-store", w.Header().Get("Cache-Control"))
		is.Equal(t, "application/wasm", w.Header().Get("Content-Type"))
	})

	t.Run("should serve without the headers of cross-origin isolation if not isolated", func(t *testing.T) {
		w := httptest.NewRecorder()
		handler(root, false).ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/engine.wasm", nil))
		is.Equal(t, http.StatusOK, w.Code)
		is.Equal(t, "", w.Header().Get("Cross-Origin-Opener-Policy"))
		is.Equal(t, "", w.Header().Get("Cross-Origin-Embedder-Policy"))
		is.Equal(t, "application/wasm", w.Header().Get("Content-Type"))
	})
}

func TestWriteBuild(t *testing.T) {
	t.Run("should write the commit into build.json", func(t *testing.T) {
		dir := t.TempDir()
		is.NotError(t, writeBuild(dir, "3f6fbfd1"))

		b, err := os.ReadFile(filepath.Join(dir, "build.json"))
		is.NotError(t, err)
		var build struct {
			Commit string `json:"commit"`
		}
		is.NotError(t, json.Unmarshal(b, &build, json.RejectUnknownMembers(true)))
		is.Equal(t, "3f6fbfd1", build.Commit)
	})
}
