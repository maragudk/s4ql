//go:build s4ql_browser

// Command serve builds the simulator's engine for a browser and puts every file that a page needs
// into one directory, and serves that directory with the demo page of internal/simulator/engine:
//
//	go run -tags s4ql_browser ./internal/simulator/serve -out /tmp/simulator
//	go run -tags s4ql_browser ./internal/simulator/serve -addr 127.0.0.1:8080
//
// With -out it writes the directory and exits. With -addr it writes the directory, to a temporary one
// if -out is not given, and serves it on that address, with the headers that make the page
// cross-origin isolated, until it is stopped. Run it from inside the module. It runs the Go on the PATH, whose wasm_exec.js it takes.
package main

import (
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
	out := flag.String("out", "", "the directory to write the files to")
	addr := flag.String("addr", "", "the address to serve the files on")
	flag.Parse()
	if err := run(*out, *addr); err != nil {
		fmt.Fprintln(os.Stderr, "serve:", err)
		os.Exit(1)
	}
}

func run(out, addr string) error {
	if out == "" && addr == "" {
		return errors.New("give -out, -addr, or both")
	}
	if out == "" {
		dir, err := os.MkdirTemp("", "s4ql-simulator-")
		if err != nil {
			return err
		}
		defer func() { _ = os.RemoveAll(dir) }()
		out = dir
	}
	if err := bundle(out); err != nil {
		return err
	}
	if addr == "" {
		return nil
	}
	fmt.Printf("serving %v on http://%v/\n", out, addr)
	files := http.FileServer(http.Dir(out))
	server := &http.Server{
		Addr: addr,
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Cross-Origin-Opener-Policy", "same-origin")
			w.Header().Set("Cross-Origin-Embedder-Policy", "require-corp")
			w.Header().Set("Cache-Control", "no-store")
			files.ServeHTTP(w, r)
		}),
		ReadHeaderTimeout: time.Minute,
	}
	return server.ListenAndServe()
}

// bundle the files of the simulator into dir: the program, the JavaScript of the engine and of the
// SQLite binding, the SQLite WebAssembly build, Go's wasm_exec.js, and the demo page.
func bundle(dir string) error {
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
		"index.html":   filepath.Join(engine, "index.html"),
		"sqlite3.js":   filepath.Join(browser, "sqlite3.js"),
		"sqlite3.wasm": filepath.Join(browser, "sqlite3.wasm"),
		"vfs.js":       filepath.Join(browser, "vfs.js"),
		"bridge.js":    filepath.Join(browser, "bridge.js"),
		"fs.js":        filepath.Join(browser, "fs.js"),
		"wasm_exec.js": filepath.Join(goroot, "lib", "wasm", "wasm_exec.js"),
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
