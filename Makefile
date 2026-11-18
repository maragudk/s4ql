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

# fuzz runs each fuzz test in the SQLite binding, its cross-checks, the storage formats, the SQL
# tokenizer, SQLite's verdicts on SQL, the SQL parser, the check of the shape of its tree, and the
# pass after the parser for FUZZTIME. Pass FUZZFLAGS=-asan to fuzz under the address sanitizer (Linux
# only).
.PHONY: fuzz
fuzz:
	for dir in internal/sqlite $(CROSSCHECK) internal/format internal/sql/token internal/sql/oracle internal/sql/parsetest internal/sql/parser internal/sql/shapetest internal/sql/stmt; do \
		for test in $$(cd $$dir && go test -list '^Fuzz' . | grep '^Fuzz'); do \
			(cd $$dir && go test $(FUZZFLAGS) -run '^$$' -fuzz "^$$test\$$" -fuzztime $(FUZZTIME) .) || exit 1; \
		done; \
	done

# generate the Go code for the schema of the commit object in proto/ into internal/s4qlpb, with the
# Buf pinned above and the protoc-gen-go pinned as a tool in go.mod.
.PHONY: generate
generate:
	$(BUF) generate

# lint also runs lint-proto, and the lint for code that runs under the simulation, from
# internal/simlint. That lint is a module of its own, so that its dependencies stay out of the
# library's, and is built there and run here.
.PHONY: lint
lint:
	golangci-lint run
	cd $(CROSSCHECK) && golangci-lint run
	cd $(SIMLINT) && golangci-lint run
	dir=$$(mktemp -d) && (cd $(SIMLINT) && go build -o $$dir/simlint ./cmd/simlint) && $$dir/simlint ./...; \
		status=$$?; rm -rf $$dir; exit $$status
	$(MAKE) lint-proto

# lint-proto checks the schema in proto/ with Buf's lint rules and its formatter.
.PHONY: lint-proto
lint-proto:
	$(BUF) lint
	$(BUF) format --diff --exit-code

# mutation runs the mutation testing of the SQL tokenizer, syntax tree, parser, and the pass after the
# parser, with the results and the report in MUTATIONDIR. It is not part of test, since a full run takes long; see
# internal/sql/mutation/README.md. A run with the same MUTATIONDIR goes on where the last one stopped,
# and refuses it after a change of the code; remove the directory then.
MUTATIONDIR ?= $(or $(TMPDIR),/tmp)/s4ql-mutation
.PHONY: mutation
mutation:
	go run ./internal/sql/mutation -out $(MUTATIONDIR) $(MUTATIONFLAGS)

# sqlite-update vendors the SQLite amalgamation of VERSION, such as make sqlite-update VERSION=3.54.1.
.PHONY: sqlite-update
sqlite-update:
	go run ./internal/sqlite/update -version $(VERSION)

# sqlite-verify downloads the vendored SQLite amalgamation again and checks it against the vendored files.
.PHONY: sqlite-verify
sqlite-verify:
	go run ./internal/sqlite/update -verify

.PHONY: test
test:
	go test -coverprofile cover.out -shuffle on ./...
	cd $(CROSSCHECK) && go test -shuffle on ./...
	cd $(SIMLINT) && go test -shuffle on ./...

# BROWSERPKGS are the packages that build for GOOS=js GOARCH=wasm with the tag s4ql_browser, on the
# official SQLite WebAssembly build in internal/sqlite/browser; see its README.
BROWSERPKGS ?= ./internal/sqlite ./internal/sqlite/faultvfs ./internal/core ./internal/sim ./internal/store/simstore
BROWSER := GOOS=js GOARCH=wasm go
BROWSERTIMEOUT ?= 30m

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
		$(BROWSER) test -tags s4ql_browser -exec $$dir/browser -timeout $(BROWSERTIMEOUT) $(BROWSERFLAGS) $(BROWSERPKGS); \
		status=$$?; rm -rf $$dir; exit $$status

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
