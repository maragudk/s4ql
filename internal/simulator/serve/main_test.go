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

	t.Run("should refuse -version without -page, and -page without -version", func(t *testing.T) {
		err := run(options{out: t.TempDir(), version: "abc"})
		is.True(t, err != nil)
		is.Equal(t, "-page and -version go together", err.Error())

		err = run(options{out: t.TempDir(), page: "index.html"})
		is.True(t, err != nil)
		is.Equal(t, "-page and -version go together", err.Error())
	})
}

func TestStamp(t *testing.T) {
	t.Run("should load page.js and run.js at the version, and nothing else", func(t *testing.T) {
		page := `<link rel="modulepreload" href="run.js">
<link rel="modulepreload" href="page.js">
<link rel="icon" href="../favicon.svg">
<a href="../">Back</a>
<p>page.js and run.js</p>
<script type="module" src="page.js"></script>
`
		stamped, err := stamp([]byte(page), "3f6fbfd1")
		is.NotError(t, err)
		is.Equal(t, `<link rel="modulepreload" href="run.js?v=3f6fbfd1">
<link rel="modulepreload" href="page.js?v=3f6fbfd1">
<link rel="icon" href="../favicon.svg">
<a href="../">Back</a>
<p>page.js and run.js</p>
<script type="module" src="page.js?v=3f6fbfd1"></script>
`, string(stamped))
	})

	t.Run("should refuse a page without the script page.js", func(t *testing.T) {
		_, err := stamp([]byte(`<script type="module" src="other.js"></script>`), "3f6fbfd1")
		is.True(t, err != nil)
		is.Equal(t, `the page has no src="page.js"`, err.Error())
	})

	t.Run("should refuse a page that is stamped already", func(t *testing.T) {
		_, err := stamp([]byte(`<script type="module" src="page.js?v=3f6fbfd1"></script>`), "3f6fbfd1")
		is.True(t, err != nil)
	})

	t.Run("should refuse a version that does not go into a URL as it is", func(t *testing.T) {
		for _, version := range []string{"", "a b", "a&b", `a"b`, "a/b", "ä"} {
			_, err := stamp([]byte(`<script type="module" src="page.js"></script>`), version)
			is.True(t, err != nil, version)
		}
	})
}

func TestStampFile(t *testing.T) {
	t.Run("should write the stamped page in place", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "index.html")
		is.NotError(t, os.WriteFile(path, []byte(`<script type="module" src="page.js"></script>`), 0o644))

		is.NotError(t, stampFile(path, "3f6fbfd1"))

		b, err := os.ReadFile(path)
		is.NotError(t, err)
		is.Equal(t, `<script type="module" src="page.js?v=3f6fbfd1"></script>`, string(b))
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
