package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"maragu.dev/is"
)

// TestMain makes the test binary a fake Chromium when S4QL_CHROMIUM_FAKE names what it does: it
// then takes the page's URL as its last argument and speaks to the runner as the page would.
func TestMain(m *testing.M) {
	if mode := os.Getenv("S4QL_CHROMIUM_FAKE"); mode != "" {
		fakeChromium(mode, os.Args[len(os.Args)-1])
	}
	os.Exit(m.Run())
}

// fakeChromium in the given mode:
//   - hang: never speak to the runner;
//   - exit: fetch the program and start it, and it writes "hello" and exits with code 7, and the fake
//     exits right after, so that the runner sees the exit and Chromium's stop at about the same time;
//   - crash-once: exit with code 1 at once if the file in S4QL_CHROMIUM_FAKE_MARK is not there, and
//     create it, and else do as exit;
//   - fetch-and-hang: fetch the program, and never start it;
//   - run-and-hang: fetch the program and start it, and it writes that TestA and TestB started and
//     TestA ended, and then hangs.
func fakeChromium(mode, url string) {
	post := func(path, body string) {
		res, err := http.Post(url+path, "text/plain", strings.NewReader(body))
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(2)
		}
		_ = res.Body.Close()
	}
	fetch := func(url string) {
		res, err := http.Get(url)
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
		mode = "exit"
	}
	switch mode {
	case "hang":
		time.Sleep(time.Hour)
	case "exit":
		res, err := http.Get(url + "config")
		if err != nil {
			os.Exit(2)
		}
		_, _ = io.Copy(io.Discard, res.Body)
		_ = res.Body.Close()
		fetch(url + "program.wasm")
		post("started", "")
		post("output?fd=1", "hello\n")
		post("exit?code=7", "")
		os.Exit(0)
	case "fetch-and-hang":
		fetch(url + "program.wasm")
		time.Sleep(time.Hour)
	case "run-and-hang":
		fetch(url + "program.wasm")
		post("started", "")
		post("output?fd=1", "=== RUN   TestA\n=== RUN   TestB\n--- PASS: TestA (0.00s)\n")
		time.Sleep(time.Hour)
	}
	os.Exit(2)
}

// signalOn is a writer that closes seen once what was written to it holds text.
type signalOn struct {
	mu   sync.Mutex
	text string
	b    strings.Builder
	seen chan struct{}
}

func (s *signalOn) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.b.Write(p)
	if s.seen != nil && strings.Contains(s.b.String(), s.text) {
		close(s.seen)
		s.seen = nil
	}
	return len(p), nil
}

// fake makes the test binary the Chromium of the runner, in the given mode of [fakeChromium], and
// returns the path of a program to pass to the runner, which the fake never fetches.
func fake(t *testing.T, mode string) string {
	t.Helper()
	exe, err := os.Executable()
	is.NotError(t, err)
	t.Setenv("S4QL_CHROMIUM", exe)
	t.Setenv("S4QL_CHROMIUM_FLAGS", "")
	t.Setenv("S4QL_CHROMIUM_STEPS", "")
	t.Setenv("S4QL_CHROMIUM_FAKE", mode)
	t.Setenv("S4QL_CHROMIUM_FAKE_MARK", filepath.Join(t.TempDir(), "mark"))
	program := filepath.Join(t.TempDir(), "program.wasm")
	is.NotError(t, os.WriteFile(program, nil, 0o600))
	return program
}

// syncBuilder is a strings.Builder that the runner's goroutines may write to at once.
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

func TestRunner_run(t *testing.T) {
	t.Run("should write what the program writes and exit with its code", func(t *testing.T) {
		program := fake(t, "exit")
		var stdout, stderr syncBuilder
		r := runner{stdout: &stdout, stderr: &stderr, startup: time.Minute, grace: time.Minute, attempts: 1}

		code, err := r.run(t.Context(), []string{program, "-test.v"})
		is.NotError(t, err)
		is.Equal(t, 7, code)
		is.Equal(t, "hello\n", stdout.String())
		is.Equal(t, "", stderr.String())
	})

	t.Run("should write what it saw when the program exited, with S4QL_CHROMIUM_STEPS", func(t *testing.T) {
		program := fake(t, "exit")
		t.Setenv("S4QL_CHROMIUM_STEPS", "1")
		var stdout, stderr syncBuilder
		r := runner{stdout: &stdout, stderr: &stderr, startup: time.Minute, grace: time.Minute, attempts: 1}

		code, err := r.run(t.Context(), []string{program})
		is.NotError(t, err)
		is.Equal(t, 7, code)
		is.True(t, strings.Contains(stderr.String(), "browser: what the runner saw: chromium started after"), stderr.String())
		is.True(t, strings.Contains(stderr.String(), "the Worker fetched its configuration after"), stderr.String())
		is.True(t, strings.Contains(stderr.String(), "the program started after"), stderr.String())
	})

	t.Run("should start Chromium again when it did not start the program in time, and fail after the last attempt", func(t *testing.T) {
		program := fake(t, "hang")
		var stdout, stderr syncBuilder
		r := runner{stdout: &stdout, stderr: &stderr, startup: 10 * time.Millisecond, restartup: 20 * time.Millisecond, grace: time.Minute, attempts: 2}

		_, err := r.run(t.Context(), []string{program})
		is.True(t, errors.Is(err, errNotStarted), err)
		is.True(t, strings.Contains(err.Error(), "attempt 2 of 2: chromium did not start the program: chromium did not fetch it within 20ms"), err)
		is.True(t, strings.Contains(stderr.String(), "attempt 1 of 2: chromium did not start the program: chromium did not fetch it within 10ms"), stderr.String())
		is.True(t, strings.Contains(stderr.String(), "what the runner saw: chromium started after"), stderr.String())
	})

	t.Run("should start Chromium again when the Worker fetched the program and did not start it in time", func(t *testing.T) {
		program := fake(t, "fetch-and-hang")
		var stdout, stderr syncBuilder
		r := runner{stdout: &stdout, stderr: &stderr, startup: time.Hour, restartup: time.Hour, compile: 10 * time.Millisecond, grace: time.Minute, attempts: 2}

		_, err := r.run(t.Context(), []string{program})
		is.True(t, errors.Is(err, errNotStarted), err)
		is.True(t, strings.Contains(err.Error(), "attempt 2 of 2: chromium did not start the program: the Worker fetched it, and did not start it within 10ms after"), err)
		is.True(t, strings.Contains(stderr.String(), "the Worker fetched the program after"), stderr.String())
	})

	t.Run("should start Chromium again when it stopped before it started the program", func(t *testing.T) {
		program := fake(t, "crash-once")
		var stdout, stderr syncBuilder
		r := runner{stdout: &stdout, stderr: &stderr, startup: time.Minute, restartup: time.Minute, grace: time.Minute, attempts: 2}

		code, err := r.run(t.Context(), []string{program})
		is.NotError(t, err)
		is.Equal(t, 7, code)
		is.True(t, strings.Contains(stderr.String(), "attempt 1 of 2: chromium did not start the program: chromium stopped before the program exited"), stderr.String())
		is.True(t, strings.Contains(stderr.String(), "the fake crashed"), stderr.String())
	})

	t.Run("should stop Chromium after the -test.timeout and the grace, and say what it saw", func(t *testing.T) {
		program := fake(t, "hang")
		var stdout, stderr syncBuilder
		r := runner{stdout: &stdout, stderr: &stderr, startup: time.Hour, grace: 0, attempts: 1}

		_, err := r.run(t.Context(), []string{program, "-test.timeout=1ms"})
		is.True(t, err != nil)
		is.True(t, strings.Contains(err.Error(), "the program did not exit within 0s after its -test.timeout"), err)
		is.True(t, strings.Contains(err.Error(), "no output from the program"), err)
	})

	t.Run("should stop Chromium on a signal, and name the tests that had started and not ended", func(t *testing.T) {
		program := fake(t, "run-and-hang")
		stdout := &signalOn{text: "--- PASS: TestA", seen: make(chan struct{})}
		seen := stdout.seen
		var stderr syncBuilder
		r := runner{stdout: stdout, stderr: &stderr, startup: time.Hour, grace: time.Hour, attempts: 1}
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		go func() {
			<-seen
			cancel()
		}()

		_, err := r.run(ctx, []string{program, "-test.v"})
		is.True(t, err != nil)
		is.True(t, strings.Contains(err.Error(), "stopped by a signal"), err)
		is.True(t, strings.Contains(err.Error(), "the tests that had started and not ended: TestB"), err)
	})
}

func TestProgress(t *testing.T) {
	t.Run("should name the tests that started and did not end, from the output of go test -v", func(t *testing.T) {
		var p progress
		p.output(1, []byte("=== RUN   TestA\n--- PASS: TestA (0.00s)\n=== RUN   TestB\n=== PAUSE TestB\n=== RUN   TestC\n"))
		p.output(1, []byte("=== RUN   TestC/sub_one\n    --- PASS: TestC/sub_one (0.01s)\n=== RUN   TestC/sub_"))
		p.output(1, []byte("two\n=== CONT  TestB\n"))

		is.EqualSlice(t, []string{"TestB", "TestC", "TestC/sub_two"}, p.running())
	})

	t.Run("should take a test as ended when it failed or skipped", func(t *testing.T) {
		var p progress
		p.output(1, []byte("=== RUN   TestA\n=== RUN   TestB\n=== RUN   TestC\n--- FAIL: TestA (1.00s)\n--- SKIP: TestB (0.00s)\n"))

		is.EqualSlice(t, []string{"TestC"}, p.running())
	})

	t.Run("should keep the lines of standard output and standard error apart", func(t *testing.T) {
		var p progress
		p.output(1, []byte("=== RUN   TestA\n=== RUN   TestB\n"))
		p.output(2, []byte("progress: 50%"))
		p.output(1, []byte("--- PASS: TestA (0.01s)\n"))
		p.output(2, []byte("\n"))

		is.EqualSlice(t, []string{"TestB"}, p.running())
	})

	t.Run("should read the lines that go test -json makes the binary write", func(t *testing.T) {
		var p progress
		p.output(1, []byte("\x16=== RUN   TestA\n\x16=== RUN   TestA/sub\n    \x16--- PASS: TestA/sub (0.00s)\n"))

		is.EqualSlice(t, []string{"TestA"}, p.running())
	})

	t.Run("should take a line only once it has ended", func(t *testing.T) {
		var p progress
		p.output(1, []byte("=== RUN   TestA"))
		is.Equal(t, 0, len(p.running()))

		p.output(1, []byte("\n"))
		is.EqualSlice(t, []string{"TestA"}, p.running())
	})

	t.Run("should report the steps that happened, in order, with the time since the start", func(t *testing.T) {
		start := time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)
		p := progress{start: start}
		p.markAt("chromium started", start.Add(100*time.Millisecond))
		p.markAt("the page loaded", start.Add(1500*time.Millisecond))
		p.outputAt(1, []byte("=== RUN   TestA\n"), start.Add(2*time.Second))
		p.outputAt(1, []byte("=== RUN   TestB\n"), start.Add(3*time.Second))

		report := p.report(start.Add(63 * time.Second))
		is.True(t, strings.Contains(report, "chromium started after 100ms, the page loaded after 1.5s"), report)
		is.True(t, strings.Contains(report, "the first output after 2s, the last output after 3s, 1m0s ago"), report)
		is.True(t, strings.Contains(report, "the tests that had started and not ended: TestA, TestB"), report)
	})

	t.Run("should report that the program wrote nothing, and that no test was seen", func(t *testing.T) {
		start := time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)
		p := progress{start: start}
		p.markAt("chromium started", start.Add(time.Second))

		report := p.report(start.Add(2 * time.Second))
		is.True(t, strings.Contains(report, "no output"), report)
		is.True(t, strings.Contains(report, "no test had started and not ended, or the program ran without -test.v"), report)
	})
}

func TestTestTimeout(t *testing.T) {
	t.Run("should read -test.timeout in both forms", func(t *testing.T) {
		d, ok := testTimeout([]string{"-test.v", "-test.timeout=2m0s"})
		is.True(t, ok)
		is.Equal(t, 2*time.Minute, d)

		d, ok = testTimeout([]string{"-test.timeout", "30s", "-test.run", "X"})
		is.True(t, ok)
		is.Equal(t, 30*time.Second, d)
	})

	t.Run("should report no timeout without the flag or with zero", func(t *testing.T) {
		_, ok := testTimeout([]string{"-test.v"})
		is.True(t, !ok)

		_, ok = testTimeout([]string{"-test.timeout=0"})
		is.True(t, !ok)
	})
}
