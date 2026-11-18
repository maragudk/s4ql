//go:build s4ql_browser

// Command serve builds the simulator's engine for a browser and puts every file that a page needs
// into one directory, and serves that directory with the demo page of internal/simulator/engine:
//
//	go run -tags s4ql_browser ./internal/simulator/serve -out /tmp/simulator
//	go run -tags s4ql_browser ./internal/simulator/serve -addr 127.0.0.1:8080
//
// With -out it writes the directory and exits. With -addr it writes the directory, to a temporary one
// if -out is not given, and serves it on that address, with the headers that make the page
// cross-origin isolated, until it is stopped. Run it from inside the module. It builds with the Go
// on the PATH, whose wasm_exec.js it takes.
//
// For the project's site, -demo=false leaves out the demo page, and -commit writes build.json,
// {"commit":"..."}, with the commit that the engine is built from. -root serves another directory
// than that of -out, such as the whole site, and builds nothing unless -out is given too.
// -isolated=false leaves out the two headers, as GitHub Pages serves the site:
//
//	go run -tags s4ql_browser ./internal/simulator/serve -out site/simulator/engine -demo=false -commit "$(git rev-parse HEAD)"
//	go run -tags s4ql_browser ./internal/simulator/serve -root site -isolated=false -addr 127.0.0.1:8081
package main

import (
	"encoding/json/v2"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

func main() {
	var o options
	flag.StringVar(&o.out, "out", "", "the directory to write the files to")
	flag.StringVar(&o.addr, "addr", "", "the address to serve the files on")
	flag.StringVar(&o.root, "root", "", "the directory to serve, if not that of -out")
	flag.BoolVar(&o.demo, "demo", true, "whether to write the demo page")
	flag.StringVar(&o.commit, "commit", "", "the commit to write into build.json, if any")
	flag.BoolVar(&o.isolated, "isolated", true, "whether to serve with the headers that make a page cross-origin isolated")
	flag.Parse()
	if err := run(o); err != nil {
		fmt.Fprintln(os.Stderr, "serve:", err)
		os.Exit(1)
	}
}

// options of a run, from the flags.
type options struct {
	out, addr, root, commit string
	demo, isolated          bool
}

func run(o options) error {
	if o.out == "" && o.addr == "" {
		return errors.New("give -out, -addr, or both")
	}
	if o.root != "" && o.addr == "" {
		return errors.New("-root needs -addr")
	}
	if o.out == "" && o.root == "" {
		dir, err := os.MkdirTemp("", "s4ql-simulator-")
		if err != nil {
			return err
		}
		defer func() { _ = os.RemoveAll(dir) }()
		o.out = dir
	}
	if o.out != "" {
		if err := bundle(o.out, o.demo); err != nil {
			return err
		}
		if o.commit != "" {
			if err := writeBuild(o.out, o.commit); err != nil {
				return err
			}
		}
	}
	if o.addr == "" {
		return nil
	}
	root := o.root
	if root == "" {
		root = o.out
	}
	fmt.Printf("serving %v on http://%v/\n", root, o.addr)
	server := &http.Server{
		Addr:              o.addr,
		Handler:           handler(root, o.isolated),
		ReadHeaderTimeout: time.Minute,
	}
	return server.ListenAndServe()
}

// handler of the files in root, without caching, and with the headers that make a page cross-origin
// isolated if isolated is true.
func handler(root string, isolated bool) http.Handler {
	files := http.FileServer(http.Dir(root))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if isolated {
			w.Header().Set("Cross-Origin-Opener-Policy", "same-origin")
			w.Header().Set("Cross-Origin-Embedder-Policy", "require-corp")
		}
		w.Header().Set("Cache-Control", "no-store")
		files.ServeHTTP(w, r)
	})
}

// writeBuild into dir the file build.json, with the commit that the engine is built from.
func writeBuild(dir, commit string) error {
	b, err := json.Marshal(struct {
		Commit string `json:"commit"`
	}{Commit: commit})
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, "build.json"), append(b, '\n'), 0o644)
}

// bundle the files of the simulator into dir: the program, the JavaScript of the engine and of the
// SQLite binding, the SQLite WebAssembly build, Go's wasm_exec.js, and the demo page if demo is true.
func bundle(dir string, demo bool) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	engine, err := packageDir("./internal/simulator/engine")
	if err != nil {
		return err
	}
	browser, err := packageDir("./internal/sqlite/browser")
	if err != nil {
		return err
	}
	goroot, err := output("go", "env", "GOROOT")
	if err != nil {
		return err
	}
	build := exec.Command("go", "test", "-c", "-tags", "s4ql_browser", "-o", filepath.Join(dir, "engine.wasm"), "./internal/simulator/engine")
	build.Env = append(os.Environ(), "GOOS=js", "GOARCH=wasm")
	build.Stdout, build.Stderr = os.Stdout, os.Stderr
	if err := build.Run(); err != nil {
		return fmt.Errorf("build the engine: %w", err)
	}
	copies := map[string]string{
		"worker.js":    filepath.Join(engine, "worker.js"),
		"simulator.js": filepath.Join(engine, "simulator.js"),
		"sqlite3.js":   filepath.Join(browser, "sqlite3.js"),
		"sqlite3.wasm": filepath.Join(browser, "sqlite3.wasm"),
		"vfs.js":       filepath.Join(browser, "vfs.js"),
		"bridge.js":    filepath.Join(browser, "bridge.js"),
		"fs.js":        filepath.Join(browser, "fs.js"),
		"wasm_exec.js": filepath.Join(goroot, "lib", "wasm", "wasm_exec.js"),
	}
	if demo {
		copies["index.html"] = filepath.Join(engine, "index.html")
	}
	for name, from := range copies {
		b, err := os.ReadFile(from)
		if err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(dir, name), b, 0o644); err != nil {
			return err
		}
	}
	return nil
}

// packageDir of the package at the path, from go list.
func packageDir(path string) (string, error) {
	return output("go", "list", "-tags", "s4ql_browser", "-f", "{{.Dir}}", path)
}

func output(name string, args ...string) (string, error) {
	b, err := exec.Command(name, args...).Output()
	if err != nil {
		return "", fmt.Errorf("%v %v: %w", name, strings.Join(args, " "), err)
	}
	return strings.TrimSpace(string(b)), nil
}
