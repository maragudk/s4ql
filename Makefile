BUF := go run github.com/bufbuild/buf/cmd/buf@v1.75.0
CROSSCHECK := internal/sqlite/crosscheck
SIMLINT := internal/simlint
FUZZTIME ?= 30s

.PHONY: benchmark
benchmark:
	go test -bench . ./...

.PHONY: cover
cover:
	go tool cover -html cover.out

# fmt formats all Go files except generated ones, because make generate writes those and the CI job
# proto checks that they stay as it wrote them.
.PHONY: fmt
fmt:
	grep -rLE '^// Code generated .* DO NOT EDIT\.$$' --include='*.go' . | \
		xargs goimports -w -local `head -n 1 go.mod | sed 's/^module //'`

# corpus regenerates the corpus of SQL statements in internal/sql/corpus from the archives of SQLite
# that internal/sql/corpus/source.json pins, which it downloads from sqlite.org.
.PHONY: corpus
corpus:
	go run ./internal/sql/corpus/update

# corpus-verify downloads the archives of SQLite that internal/sql/corpus/source.json pins again, and
# checks that the committed corpus is what make corpus writes from them.
.PHONY: corpus-verify
corpus-verify:
	go run ./internal/sql/corpus/update -check

# fuzz runs each fuzz test in the SQLite binding, its cross-checks, the differential test of the driver, the storage formats, the SQL
# tokenizer, SQLite's verdicts on SQL, the SQL parser, the check of the shape of its tree, the pass
# after the parser, and the engine for FUZZTIME. Pass FUZZFLAGS=-asan to fuzz under the address
# sanitizer (Linux only). It fails if it cannot list the fuzz tests of a directory, or if a directory
# has none, so that no directory is skipped in silence.
FUZZDIRS ?= internal/sqlite $(CROSSCHECK) $(CROSSCHECK)/drivercheck internal/format internal/sql/token internal/sql/oracle internal/sql/parsetest \
	internal/sql/parser internal/sql/shapetest internal/sql/stmt internal/engine
.PHONY: fuzz
fuzz:
	for dir in $(FUZZDIRS); do \
		list=$$(cd $$dir && go test -list '^Fuzz' .) || { echo "fuzz: cannot list the fuzz tests of $$dir" >&2; exit 1; }; \
		tests=$$(echo "$$list" | grep '^Fuzz'); \
		[ -n "$$tests" ] || { echo "fuzz: no fuzz test in $$dir" >&2; exit 1; }; \
		for test in $$tests; do \
			(cd $$dir && go test $(FUZZFLAGS) -run '^$$' -fuzz "^$$test\$$" -fuzztime $(FUZZTIME) .) || exit 1; \
		done; \
	done

# generate the Go code for the schema of the commit object in proto/ into internal/s4qlpb, with the
# Buf pinned above and the protoc-gen-go pinned as a tool in go.mod.
.PHONY: generate
generate:
	$(BUF) generate

# lint also runs lint-proto, the lint for code that runs under the simulation, from
# internal/simlint, both lints over the browser's build of BROWSERPKGS, whose files a native run does
# not see, and both lints over the native build of SIMULATORPKGS with the tag s4ql_browser.
# The simulation's lint is a module of its own, so that its dependencies stay out of the library's,
# and is built there and run here.
.PHONY: lint
lint:
	golangci-lint run
	GOOS=js GOARCH=wasm golangci-lint run --build-tags s4ql_browser $(BROWSERPKGS)
	golangci-lint run --build-tags s4ql_browser $(SIMULATORPKGS)
	cd $(CROSSCHECK) && golangci-lint run
	cd $(SIMLINT) && golangci-lint run
	dir=$$(mktemp -d) && (cd $(SIMLINT) && go build -o $$dir/simlint ./cmd/simlint) && $$dir/simlint ./... && \
		GOFLAGS=-tags=s4ql_browser $$dir/simlint $(SIMULATORPKGS) && \
		GOOS=js GOARCH=wasm GOFLAGS=-tags=s4ql_browser $$dir/simlint $(BROWSERPKGS); \
		status=$$?; rm -rf $$dir; exit $$status
	$(MAKE) lint-proto

# lint-proto checks the schema in proto/ with Buf's lint rules and its formatter.
.PHONY: lint-proto
lint-proto:
	$(BUF) lint
	$(BUF) format --diff --exit-code

# mutation runs the mutation testing of the SQL tokenizer, syntax tree, parser, the pass after the
# parser, and the engine, with the results and the report in MUTATIONDIR. It is not part of test,
# since a full run takes long; see internal/sql/mutation/README.md. A run with the same MUTATIONDIR goes on where the last one stopped,
# and refuses it after a change of the code; remove the directory then.
MUTATIONDIR ?= $(or $(TMPDIR),/tmp)/s4ql-mutation
.PHONY: mutation
mutation:
	go run ./internal/sql/mutation -out $(MUTATIONDIR) $(MUTATIONFLAGS)

# sqlite-update vendors the SQLite amalgamation of VERSION, such as make sqlite-update VERSION=3.54.1.
.PHONY: sqlite-update
sqlite-update:
	go run ./internal/sqlite/update -version $(VERSION)

# sqlite-browser-update vendors the official SQLite WebAssembly build of VERSION into
# internal/sqlite/browser, such as make sqlite-browser-update VERSION=3.54.1. It is the version of the
# amalgamation, which the browser's tests require.
.PHONY: sqlite-browser-update
sqlite-browser-update:
	go run ./internal/sqlite/browser/update -version $(VERSION)

# sqlite-browser-verify downloads the vendored SQLite WebAssembly build again and checks it against
# the vendored files.
.PHONY: sqlite-browser-verify
sqlite-browser-verify:
	go run ./internal/sqlite/browser/update -verify

# sqlite-verify downloads the vendored SQLite amalgamation again and checks it against the vendored files.
.PHONY: sqlite-verify
sqlite-verify:
	go run ./internal/sqlite/update -verify

.PHONY: test
test:
	go test -coverprofile cover.out -shuffle on ./...
	go test -tags s4ql_browser -shuffle on $(SIMULATORPKGS)
	cd $(CROSSCHECK) && go test -shuffle on ./...
	cd $(SIMLINT) && go test -shuffle on ./...

# test-s3 runs S3TESTS, every test that needs a real bucket, in S3PKGS under the race detector. It
# fails if S4QL_S3_BUCKET is empty, and if one of S3TESTS did not run and pass, so that a run cannot
# pass by skipping them. How to run it by hand is in the documentation of internal/store/s3test.
S3TESTS := TestOpen_realBucket TestClient_conformance TestClient_realBucket TestClient_core TestDeleteOld_realBucket
S3PKGS := . ./internal/store/s3store ./internal/store/s3test
.PHONY: test-s3
test-s3:
	@[ -n "$$S4QL_S3_BUCKET" ] || { echo "test-s3: S4QL_S3_BUCKET is empty, so the tests against a real bucket would skip; see internal/store/s3test" >&2; exit 1; }
	out=$$(mktemp) && \
		go test -race -count 1 -shuffle on -v -run "^($$(echo $(S3TESTS) | tr ' ' '|'))\$$" $(S3PKGS) > $$out 2>&1; \
		status=$$?; cat $$out; \
		for test in $(S3TESTS); do \
			grep -q "^--- PASS: $$test " $$out || { echo "test-s3: $$test did not run and pass" >&2; status=1; }; \
		done; \
		rm -f $$out; exit $$status

# test-sql runs the tests of the SQL front end and of the engine without the race detector and the
# address sanitizer, under which they compare with SQLite on a tenth of the corpus and of the generated
# texts only, and leave out the checks that need all of them, such as the use of every rule of the
# grammar and the number of texts on which SQLite does not say.
.PHONY: test-sql
test-sql:
	go test -count 1 -shuffle on ./internal/sql/... ./internal/engine/...

# BROWSERPKGS are the packages that build for GOOS=js GOARCH=wasm with the tag s4ql_browser, on the
# official SQLite WebAssembly build in internal/sqlite/browser; see its README.
BROWSERPKGS ?= ./internal/sqlite ./internal/sqlite/faultvfs ./internal/core ./internal/sim ./internal/store/simstore \
	./internal/simulator ./internal/simulator/engine
# SIMULATORPKGS are the packages of the simulator, which build only with the tag s4ql_browser, and
# natively too; see internal/simulator/README.md.
SIMULATORPKGS := ./internal/simulator/...
BROWSER := GOOS=js GOARCH=wasm go
BROWSERTIMEOUT ?= 30m
# BROWSERSKIP are the tests that cannot pass in a browser for a reason in the route, and that
# test-browser skips. Two canaries in internal/sim/cgo_test.go run eight goroutines that read in a
# loop that never blocks, beside a writer. Go's js/wasm port has one thread and no preemption, so
# the first such goroutine runs alone for good, and the test hangs; measured in Chromium, where it
# never ended. A canary about calls into C and real parallelism has nothing to measure there.
BROWSERSKIP ?= TestSynctestWithCgo/among_parallel_readers

# check-browser builds and vets BROWSERPKGS for the browser, and compiles their tests, so that the
# browser's binding cannot drift from the cgo binding's API unnoticed. It needs no browser.
.PHONY: check-browser
check-browser:
	$(BROWSER) build -tags s4ql_browser $(BROWSERPKGS)
	$(BROWSER) vet -tags s4ql_browser $(BROWSERPKGS)
	dir=$$(mktemp -d) && for pkg in $(BROWSERPKGS); do \
		$(BROWSER) test -tags s4ql_browser -c -o $$dir/ $$pkg || { rm -rf $$dir; exit 1; }; \
	done; rm -rf $$dir

# test-browser runs the tests of BROWSERPKGS in headless Chromium, through the runner in
# internal/sqlite/browser as the -exec program of go test. BROWSERFLAGS go to go test, such as
# BROWSERFLAGS='-run TestSimulation -v'.
.PHONY: test-browser
test-browser:
	dir=$$(mktemp -d) && go build -o $$dir/browser ./internal/sqlite/browser && \
		$(BROWSER) test -tags s4ql_browser -exec $$dir/browser -timeout $(BROWSERTIMEOUT) -skip '$(BROWSERSKIP)' $(BROWSERFLAGS) $(BROWSERPKGS); \
		status=$$?; rm -rf $$dir; exit $$status

# simulator builds the simulator's engine for a browser, and writes it with every file that a page
# needs into SIMULATORDIR. simulator-demo also serves that directory, with the demo page of
# internal/simulator/engine, on SIMULATORADDR.
SIMULATORDIR ?= $(or $(TMPDIR),/tmp)/s4ql-simulator
SIMULATORADDR ?= 127.0.0.1:8080
.PHONY: simulator
simulator:
	go run -tags s4ql_browser ./internal/simulator/serve -out $(SIMULATORDIR)

.PHONY: simulator-demo
simulator-demo:
	go run -tags s4ql_browser ./internal/simulator/serve -out $(SIMULATORDIR) -addr $(SIMULATORADDR)

# site builds the simulator's engine into the project site, in SITEENGINE, which .gitignore covers,
# with build.json for the page, which names SITECOMMIT as the commit it is built from. The Pages
# workflow runs it before it uploads site/. site-serve builds it and serves site/ on SITEADDR, without
# the headers of cross-origin isolation, as GitHub Pages serves it. site-check builds it and checks
# the simulator's page in headless Chromium: see internal/simulator/sitecheck.
SITEENGINE := site/simulator/engine
SITECOMMIT ?= $(shell git rev-parse HEAD)
SITEADDR ?= 127.0.0.1:8081
.PHONY: site
site:
	rm -rf $(SITEENGINE)
	go run -tags s4ql_browser ./internal/simulator/serve -out $(SITEENGINE) -demo=false -commit $(SITECOMMIT)

.PHONY: site-serve
site-serve: site
	go run -tags s4ql_browser ./internal/simulator/serve -root site -isolated=false -addr $(SITEADDR)

.PHONY: site-check
site-check: site
	go run -tags s4ql_browser ./internal/simulator/sitecheck -root site

# test-asan runs all tests, and then each fuzz test for FUZZTIME, under the C address sanitizer. Go
# supports -asan on Linux only, so elsewhere it runs in a Linux container with Docker, with Docker
# volumes for the module and build caches.
.PHONY: test-asan
test-asan:
ifeq ($(shell go env GOOS),linux)
	go test -asan -shuffle on ./...
	cd $(CROSSCHECK) && go test -asan -shuffle on ./...
	$(MAKE) fuzz FUZZFLAGS=-asan
else
	docker run --rm -v "$(CURDIR)":/src -v s4ql-gomod:/go/pkg/mod -v s4ql-gocache:/root/.cache/go-build -w /src \
		golang:$(shell sed -n 's/^go //p' go.mod) make test-asan FUZZTIME=$(FUZZTIME)
endif
