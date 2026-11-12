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

.PHONY: fmt
fmt:
	goimports -w -local `head -n 1 go.mod | sed 's/^module //'` .

# fuzz runs each fuzz test in the SQLite binding, its cross-checks, and the storage formats for
# FUZZTIME. Pass FUZZFLAGS=-asan to fuzz under the address sanitizer (Linux only).
.PHONY: fuzz
fuzz:
	for dir in internal/sqlite $(CROSSCHECK) internal/format; do \
		for test in $$(cd $$dir && go test -list '^Fuzz' . | grep '^Fuzz'); do \
			(cd $$dir && go test $(FUZZFLAGS) -run '^$$' -fuzz "^$$test\$$" -fuzztime $(FUZZTIME) .) || exit 1; \
		done; \
	done

# generate the Go code for the schema of the commit object in proto/ into internal/s4qlpb, with the
# Buf pinned above and the protoc-gen-go pinned as a tool in go.mod.
.PHONY: generate
generate:
	$(BUF) generate

# lint also runs the lint for code that runs under the simulation, from internal/simlint. It is a
# module of its own, so that its dependencies stay out of the library's, and is built there and run
# here.
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
