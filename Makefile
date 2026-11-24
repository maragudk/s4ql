BUF := go run github.com/bufbuild/buf/cmd/buf@v1.75.0
CROSSCHECK := internal/sqlite/crosscheck
SIMLINT := internal/simlint
FUZZTIME ?= 30s

# benchmark passes -short, as test does, so that it needs no credentials.
.PHONY: benchmark
benchmark:
	go test -short -bench . ./...

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

# fuzz runs each fuzz test in the SQLite binding, its cross-checks, the differential test of the driver, the storage formats, the storage core, the SQL
# tokenizer, SQLite's verdicts on SQL, the SQL parser, the check of the shape of its tree, the pass
# after the parser, and the engine for FUZZTIME. Pass FUZZFLAGS=-asan to fuzz under the address
# sanitizer (Linux only). It fails if it cannot list the fuzz tests of a directory, if a directory
# has none, or if it cannot tell whether a directory's tests link C, so that no fuzz test is skipped
# in silence.
#
# The fuzz tests of a directory are in one of two groups, which LINKSC computes: c if the
# directory's test binary links C, and go if not. FUZZGROUP=c or FUZZGROUP=go runs one group only;
# test-asan runs the first under the address sanitizer and the second without it. FUZZSHARD=i/n
# runs the i-th of n shards of what FUZZGROUP selects, so that CI can fuzz in jobs that run at the
# same time. The fuzz tests go to the shards one by one, in the order of FUZZDIRS and of their test
# files: the k-th, from 0, to the shard k modulo n, plus one. So every fuzz test is in exactly one
# shard, and the shards differ by one fuzz test at most. A shard that gets no fuzz test fails, so n
# must not be more than the number of fuzz tests in the group. fuzz-shards checks the shards.
#
# To make the groups and the shards without building a test binary, which takes minutes for the
# packages that link SQLite, it reads the names of the fuzz tests from the test files that go list
# names. Before it fuzzes a directory, it lists the directory's fuzz tests with go test, with the
# build that the fuzzing then uses, and fails if go test names other fuzz tests than the files.
FUZZDIRS ?= internal/sqlite $(CROSSCHECK) $(CROSSCHECK)/drivercheck internal/format internal/core internal/sql/token internal/sql/oracle internal/sql/parsetest \
	internal/sql/parser internal/sql/shapetest internal/sql/stmt internal/engine
FUZZGROUP ?=
FUZZSHARD ?=
# FUZZPLAN prints a line for each directory of FUZZDIRS: its group, the directory, and the names of
# its fuzz tests.
FUZZPLAN = for dir in $(FUZZDIRS); do \
		files=$$(cd $$dir && go list -f '{{range .TestGoFiles}}{{$$.Dir}}/{{.}} {{end}}{{range .XTestGoFiles}}{{$$.Dir}}/{{.}} {{end}}' .) || \
			{ echo "fuzz: cannot list the fuzz tests of $$dir" >&2; exit 1; }; \
		tests=$$([ -z "$$files" ] || sed -n 's/^func \(Fuzz[A-Za-z0-9_]*\)(.*/\1/p' $$files); \
		[ -n "$$tests" ] || { echo "fuzz: no fuzz test in $$dir" >&2; exit 1; }; \
		deps=$$(cd $$dir && $(LINKSC) .) || { echo "fuzz: cannot list the packages of $$dir" >&2; exit 1; }; \
		group=$$(echo "$$deps" | awk $(LINKSCAWK) | cut -d ' ' -f 1); \
		[ "$$group" = c ] || [ "$$group" = go ] || \
			{ echo "fuzz: cannot tell whether the tests of $$dir link C, so its fuzz tests are in no group" >&2; exit 1; }; \
		echo $$group $$dir $$tests; \
	done
# FUZZSELECT reads the lines of FUZZPLAN and prints a line for each fuzz test that the shell
# variables group and shard select, as FUZZGROUP and FUZZSHARD do: its group, its directory, and its
# name. It fails on a group or a shard of another form, and on a shard after the last.
FUZZSELECT = awk -v group="$$group" -v shard="$$shard" ' \
	BEGIN { \
		if (group != "" && group != "c" && group != "go") { print "fuzz: FUZZGROUP is c, go, or empty, not " group > "/dev/stderr"; bad = 1; exit 2 } \
		if (shard != "" && shard !~ /^[1-9][0-9]*\/[1-9][0-9]*$$/) { print "fuzz: FUZZSHARD is i/n, such as 1/4, not " shard > "/dev/stderr"; bad = 1; exit 2 } \
	} \
	group == "" || $$1 == group { for (i = 3; i <= NF; i++) { n++; test[n] = $$1 " " $$2 " " $$i } } \
	END { \
		if (bad) exit 2; \
		if (shard == "") { for (i = 1; i <= n; i++) print test[i]; exit } \
		split(shard, s, "/"); k = s[1] + 0; m = s[2] + 0; \
		if (k > m) { print "fuzz: FUZZSHARD " shard " names a shard after the last" > "/dev/stderr"; exit 2 } \
		for (i = 1; i <= n; i++) if ((i - 1) % m == k - 1) print test[i]; \
	}'
.PHONY: fuzz
fuzz:
	plan=$$($(FUZZPLAN)) || exit 1; \
	group='$(FUZZGROUP)' shard='$(FUZZSHARD)'; \
	run=$$(echo "$$plan" | $(FUZZSELECT)) || exit 1; \
	[ -n "$$run" ] || { echo "fuzz: no fuzz test in FUZZGROUP '$(FUZZGROUP)' and FUZZSHARD '$(FUZZSHARD)'" >&2; exit 1; }; \
	echo "$$run" | sed 's/^/fuzz: /'; \
	echo "$$plan" | while read -r group dir tests; do \
		mine=$$(echo "$$run" | awk -v dir="$$dir" '$$2 == dir { print $$3 }'); \
		[ -n "$$mine" ] || continue; \
		list=$$(cd $$dir && go test $(FUZZFLAGS) -run '^$$' -fuzz '^$$' -list '^Fuzz' . </dev/null) || \
			{ echo "fuzz: cannot list the fuzz tests of $$dir" >&2; exit 1; }; \
		[ "$$(echo "$$list" | grep '^Fuzz' | sort)" = "$$(printf '%s\n' $$tests | sort)" ] || \
			{ echo "fuzz: go test lists other fuzz tests in $$dir than its test files declare:" $$(echo "$$list" | grep '^Fuzz') >&2; exit 1; }; \
		for test in $$mine; do \
			(cd $$dir && go test $(FUZZFLAGS) -run '^$$' -fuzz "^$$test\$$" -fuzztime $(FUZZTIME) . </dev/null) || exit 1; \
		done; \
	done

# fuzz-shards checks that the shards of fuzz leave no fuzz test out: that every directory with a fuzz
# test, outside testdata, is in FUZZDIRS, and that the FUZZSHARDS shards of FUZZGROUP, as
# FUZZSHARD=1/n to n/n select them, together hold every fuzz test of the group exactly once, and
# each at least one. It prints the fuzz tests of each shard. It reads the test files only, as fuzz
# does before it builds, so it takes seconds.
FUZZSHARDS ?=
.PHONY: fuzz-shards
fuzz-shards:
	n='$(FUZZSHARDS)'; \
	echo "$$n" | grep -Eq '^[1-9][0-9]*$$' || { echo "fuzz-shards: FUZZSHARDS is a number of shards, such as 4, not '$$n'" >&2; exit 1; }; \
	declared=$$(git grep -l '^func Fuzz' -- '*_test.go' ':!*/testdata/*' | xargs -n 1 dirname | sort -u) && [ -n "$$declared" ] || \
		{ echo "fuzz-shards: cannot find the directories with fuzz tests" >&2; exit 1; }; \
	listed=$$(printf '%s\n' $(FUZZDIRS) | sort -u); \
	[ "$$declared" = "$$listed" ] || \
		{ echo "fuzz-shards: the directories with fuzz tests are" $$declared "and FUZZDIRS holds" $$listed >&2; exit 1; }; \
	plan=$$($(FUZZPLAN)) || exit 1; \
	group='$(FUZZGROUP)' shard=; \
	all=$$(echo "$$plan" | $(FUZZSELECT)) && [ -n "$$all" ] || \
		{ echo "fuzz-shards: no fuzz test in FUZZGROUP '$(FUZZGROUP)'" >&2; exit 1; }; \
	held=; k=1; \
	while [ $$k -le $$n ]; do \
		shard=$$k/$$n; \
		one=$$(echo "$$plan" | $(FUZZSELECT)) && [ -n "$$one" ] || { echo "fuzz-shards: the shard $$shard holds no fuzz test" >&2; exit 1; }; \
		echo "$$one" | sed "s|^|fuzz-shards: $$shard: |"; \
		held=$$(printf '%s\n%s' "$$held" "$$one"); \
		k=$$((k + 1)); \
	done; \
	[ "$$(echo "$$held" | sed '/^$$/d' | sort)" = "$$(echo "$$all" | sort)" ] || \
		{ echo "fuzz-shards: the $$n shards do not hold every fuzz test of FUZZGROUP '$(FUZZGROUP)' exactly once" >&2; exit 1; }; \
	echo "fuzz-shards: the $$n shards hold each of the" $$(echo "$$all" | wc -l) "fuzz tests of FUZZGROUP '$(FUZZGROUP)' exactly once"

# generate the Go code for the schema of the commit object in proto/ into internal/s4qlpb, with the
# Buf pinned above and the protoc-gen-go pinned as a tool in go.mod.
.PHONY: generate
generate:
	$(BUF) generate

# lint also runs lint-proto, the lints of internal/simlint, for code that runs under the simulation
# and for tests that skip themselves, both lints over the browser's build of BROWSERPKGS, whose files
# a native run does not see, and both lints over the native build of SIMULATORPKGS with the tag
# s4ql_browser. The lints of internal/simlint are a module of their own, so that their dependencies
# stay out of the library's, and are built there and run here. The lint for skips also runs over
# the other two modules. golangci-lint also runs over the tests that sim-speed and sim-hang build
# with tags of their own, so that they cannot drift unseen.
.PHONY: lint
lint:
	golangci-lint run
	GOOS=js GOARCH=wasm golangci-lint run --build-tags s4ql_browser $(BROWSERPKGS)
	golangci-lint run --build-tags s4ql_browser $(SIMULATORPKGS)
	golangci-lint run --build-tags s4ql_speed,s4ql_hang ./internal/sim/
	cd $(CROSSCHECK) && golangci-lint run
	cd $(SIMLINT) && golangci-lint run
	dir=$$(mktemp -d) && (cd $(SIMLINT) && go build -o $$dir/simlint ./cmd/simlint) && $$dir/simlint ./... && \
		GOFLAGS=-tags=s4ql_browser $$dir/simlint $(SIMULATORPKGS) && \
		GOOS=js GOARCH=wasm GOFLAGS=-tags=s4ql_browser $$dir/simlint $(BROWSERPKGS) && \
		(cd $(CROSSCHECK) && $$dir/simlint -skiplint ./...) && (cd $(SIMLINT) && $$dir/simlint -skiplint ./...); \
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

# sim-hang shows that a sync.Mutex held while its holder waits hangs a run of the simulation's
# scheduler until the timeout of go test: TestScheduler_mutexHeldAcrossWait in internal/sim, which
# is built only with the tag s4ql_hang, because it cannot pass. It prints the output of the run, and
# succeeds only if the run timed out with a goroutine waiting for a sync.Mutex.
.PHONY: sim-hang
sim-hang:
	out=$$(go test -tags s4ql_hang -count 1 -run '^TestScheduler_mutexHeldAcrossWait$$' -timeout 5s ./internal/sim/ 2>&1); \
		echo "$$out"; \
		echo "$$out" | grep -q '^panic: test timed out' && echo "$$out" | grep -q '^sync\.(\*Mutex)\.Lock' || \
			{ echo "sim-hang: the run did not hang on a sync.Mutex until the timeout" >&2; exit 1; }

# sim-speed logs how fast the simulation's scheduler is on this machine: TestSimulationSpeed in
# internal/sim, which is built only with the tag s4ql_speed, because it asserts nothing that the
# other tests do not.
.PHONY: sim-speed
sim-speed:
	go test -tags s4ql_speed -count 1 -run '^TestSimulationSpeed$$' -v ./internal/sim/

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

# test runs the tests with -short, so that it needs no credentials. Here -short does two things: the
# tests against a real bucket skip, which otherwise fail without their settings (see
# internal/store/s3test), and the tests of the SQL front end and of the engine compare with SQLite on
# a tenth of the corpus and of the generated texts, as under the race detector. test-s3 runs the
# first and test-sql the second without it. CI's jobs test, test-core, and test-engine run go test
# ./... between them, without -short, with the credentials, and its job sql runs test-sql.
.PHONY: test
test: test-changes
	go test -short -coverprofile cover.out -shuffle on ./...
	go test -tags s4ql_browser -shuffle on $(SIMULATORPKGS)
	cd $(CROSSCHECK) && go test -shuffle on ./...
	cd $(SIMLINT) && go test -shuffle on ./...

# test-changes runs the tests of the rule by which CI's job changes decides which jobs a change
# needs, in .github/scripts. SH is the shell that runs the script, such as SH=dash, the sh of CI.
.PHONY: test-changes
test-changes:
	sh .github/scripts/changes_test.sh

# test-s3 runs the tests of S3PKGS, the packages that hold the tests against a real bucket, under the
# race detector and without -short, as CI's job test does. Those tests fail without the settings of
# the bucket, which come from .env.test and the credentials from .env or the environment: see
# internal/store/s3test.
S3PKGS := . ./internal/store/s3store ./internal/store/s3test
.PHONY: test-s3
test-s3:
	go test -race -count 1 -shuffle on $(S3PKGS)

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
#
# BROWSERSHARD=i/n, with n BROWSERSHARDS, runs the i-th shard of the tests of the default
# BROWSERPKGS, so that CI can run them in jobs at the same time; BROWSERFLAGS must then hold no -run
# or -skip. Every test is in exactly one shard, and each shard is one go test. The tests of
# internal/core take most of the time, so they are in four shards: 1, the planted bugs of a commit,
# the subtests of BROWSERPLANTED that match BROWSERCOMMIT; 2, the other planted bugs; 3, the tests of
# BROWSERSIM; and 4, the rest, which skips those of 1 to 3. Shard 5 runs the packages of
# BROWSERSHARD5, side by side, and shard 6 every other package of BROWSERPKGS, so that a new package
# lands there. The shards are balanced by the times that CI measured on 2026-11-20; see
# docs/diary/2026-11-20-faster-browser-ci.md.
BROWSERSHARDS := 6
BROWSERSHARD ?=
BROWSERCORE := ./internal/core
BROWSERPLANTED := TestSimulation_plantedBugs
BROWSERCOMMIT := ^should_find_a_commit
BROWSERSIM := TestSimulation|TestFreeRunning
BROWSERSHARD5 := ./internal/store/simstore ./internal/sim ./internal/sqlite ./internal/sqlite/faultvfs \
	./internal/simulator/engine
BROWSERSHARD6 = $(filter-out $(BROWSERCORE) $(BROWSERSHARD5),$(BROWSERPKGS))
# browserskip is BROWSERSKIP with the pattern given after it as an alternative, which a -skip takes
# at every level of a test's name. An empty alternative would skip every test.
browserskip = $(if $(BROWSERSKIP),$(1)|$(BROWSERSKIP),$(1))
.PHONY: test-browser
test-browser:
	dir=$$(mktemp -d) || exit 1; go build -o $$dir/browser ./internal/sqlite/browser || { rm -rf $$dir; exit 1; }; \
	t() { $(BROWSER) test -tags s4ql_browser -exec $$dir/browser -timeout $(BROWSERTIMEOUT) $(BROWSERFLAGS) "$$@"; }; \
	case '$(BROWSERSHARD)' in \
	'') t -skip '$(BROWSERSKIP)' $(BROWSERPKGS) ;; \
	1/$(BROWSERSHARDS)) t -run '^$(BROWSERPLANTED)$$/$(BROWSERCOMMIT)' -skip '$(BROWSERSKIP)' $(BROWSERCORE) ;; \
	2/$(BROWSERSHARDS)) t -run '^$(BROWSERPLANTED)$$' -skip '$(call browserskip,^$(BROWSERPLANTED)$$/$(BROWSERCOMMIT))' $(BROWSERCORE) ;; \
	3/$(BROWSERSHARDS)) t -run '^($(BROWSERSIM))$$' -skip '$(BROWSERSKIP)' $(BROWSERCORE) ;; \
	4/$(BROWSERSHARDS)) t -skip '$(call browserskip,^($(BROWSERPLANTED)|$(BROWSERSIM))$$)' $(BROWSERCORE) ;; \
	5/$(BROWSERSHARDS)) t -skip '$(BROWSERSKIP)' $(BROWSERSHARD5) ;; \
	6/$(BROWSERSHARDS)) [ -n '$(BROWSERSHARD6)' ] || { echo "test-browser: shard 6 has no package of BROWSERPKGS left" >&2; false; } && \
		t -skip '$(BROWSERSKIP)' $(BROWSERSHARD6) ;; \
	*) echo "test-browser: BROWSERSHARD is i/$(BROWSERSHARDS), such as 1/$(BROWSERSHARDS), or empty, not $(BROWSERSHARD)" >&2; false ;; \
	esac; \
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

# site builds the simulator's engine into the project site in SITEDIR, in simulator/engine/, which
# .gitignore covers, with build.json for the page, which names SITECOMMIT as the commit it is built
# from. With SITEVERSION, it also writes that version into the simulator's page, in place, so that
# the page loads every file of the simulator at that version, and a cache cannot pair files of two
# deploys: see internal/simulator/serve. The Pages workflow runs it on its checkout, with the commit
# as SITEVERSION, before it uploads site/. That changes a file in git, so a local run is for a copy
# of the site only. Without SITEVERSION the page stays as it is in git, and loads its files without
# a version. site-serve builds it and serves SITEDIR on SITEADDR, without the headers of cross-origin
# isolation, as GitHub Pages serves it. site-check builds it without a version and checks the
# simulator's page in headless Chromium twice: in SITEDIR, and in a copy whose page it stamps as the
# Pages workflow does, with SITEVERSION, or SITECOMMIT if that is empty, as the version. See
# internal/simulator/sitecheck.
SITEDIR ?= site
SITECOMMIT ?= $(shell git rev-parse HEAD)
SITEVERSION ?=
SITEADDR ?= 127.0.0.1:8081
SITECHECK := go run -tags s4ql_browser ./internal/simulator/sitecheck
.PHONY: site
site:
	rm -rf $(SITEDIR)/simulator/engine
	go run -tags s4ql_browser ./internal/simulator/serve -out $(SITEDIR)/simulator/engine -demo=false -commit $(SITECOMMIT) \
		$(if $(SITEVERSION),-page $(SITEDIR)/simulator/index.html -version $(SITEVERSION))

.PHONY: site-serve
site-serve: site
	go run -tags s4ql_browser ./internal/simulator/serve -root $(SITEDIR) -isolated=false -addr $(SITEADDR)

.PHONY: site-check
site-check:
	$(MAKE) site SITEVERSION=
	$(SITECHECK) -root $(SITEDIR)
	dir=$$(mktemp -d) && cp -R $(SITEDIR) $$dir/site && \
		go run -tags s4ql_browser ./internal/simulator/serve -page $$dir/site/simulator/index.html -version $(or $(SITEVERSION),$(SITECOMMIT)) && \
		$(SITECHECK) -root $$dir/site -version $(or $(SITEVERSION),$(SITECOMMIT)); \
		status=$$?; rm -rf $$dir; exit $$status

# site-icons draws the favicons of the site and its web app manifest from the logo into SITEDIR, which
# commits them: see internal/siteicons.
.PHONY: site-icons
site-icons:
	go run ./internal/siteicons -out $(SITEDIR)

# LINKSC, run in the directory of a module with package patterns after it, lists the packages and
# their dependencies for LINKSCAWK, which prints a line for each of those packages that has tests:
# "c" and its import path if its test binary links C, and "go" and its import path if not. A test
# binary links C if it links a package outside the standard library with cgo files, such as
# internal/sqlite, internal/sqlite/faultvfs, or github.com/mattn/go-sqlite3. The address sanitizer
# finds faults in C code only, so asan-test and fuzz use this to put each package on its side, and a
# new package falls on the right side by itself.
LINKSC = go list -test -deps -f '{{if and (not .Standard) .CgoFiles}}cgo {{.ImportPath}}{{"\n"}}{{end}}{{if eq .Name "main"}}{{.ImportPath}}{{range .Deps}} {{.}}{{end}}{{end}}'
LINKSCAWK = '$$1 == "cgo" { c[$$2] = 1 } $$1 ~ /\.test$$/ { n++; t[n] = $$0 } \
	END { for (i = 1; i <= n; i++) { k = split(t[i], f, " "); l = "go"; for (j = 2; j <= k; j++) if (f[j] in c) l = "c"; \
	print l, substr(f[1], 1, length(f[1]) - 5) } }'

# asan-test runs the tests of the packages of both modules whose test binaries link C under the C
# address sanitizer (Linux only). The other packages have nothing for the sanitizer to check, and
# make test and CI's jobs test, test-core, and test-engine run them. It fails if it cannot list the packages of a module, or if
# none links C. It passes -short, so that the tests against a real bucket skip, since it has no
# credentials, and the job test runs those. Under the sanitizer, the tests of the SQL front end and
# of the engine use a tenth of the corpus whether or not -short is given, so it changes nothing else.
# ASANTIMEOUT is the timeout of go test for each package, after which go test prints the stacks of
# all goroutines and fails.
#
# CI runs these tests in jobs that run at the same time, with three settings that are empty by
# default, so that a run without them runs every test. ASANPKGS runs only these packages, and
# ASANSKIPPKGS leaves these out, by import path; a name that is not a package that links C fails, so
# that a name gone stale cannot run nothing. ASANSHARD=i/n runs the i-th of n shards of the tests of
# ASANPKGS: the tests named in ASANSHARDED, which give their subtests to the shards with
# internal/testshard, run with S4QL_TEST_SHARD=i/n. The tests named in ASANLAST run whole in the last
# shard, and every other test runs whole in the first, both without S4QL_TEST_SHARD; ASANLAST takes
# from the first shard tests of a package whose other tests would keep it the longest by far. A name
# of ASANLAST that a package does not have runs nothing there and leaves nothing out of the first
# shard. A shard fails if a package of ASANPKGS has no test in ASANSHARDED, as go test then runs
# nothing, and internal/testshard fails a test whose shard holds none of its subtests. The n shards
# together run every test, and every subtest of a test in ASANSHARDED once: see internal/testshard.
ASANTIMEOUT ?= 10m
ASANPKGS ?=
ASANSKIPPKGS ?=
ASANSHARD ?=
ASANSHARDED ?= TestSimulation_plantedBugs|FuzzTypes|FuzzTypes_core|FuzzSQL_core
ASANLAST ?= TestEngine_paths|TestEngine_update|TestDifferential|TestEngine_namesOfTheCore|FuzzChanges_core|TestEngine_indexes
.PHONY: asan-test
asan-test:
	shard='$(ASANSHARD)'; \
	if [ -n "$$shard" ]; then \
		{ echo "$$shard" | grep -Eq '^[1-9][0-9]*/[1-9][0-9]*$$' && [ "$${shard%/*}" -le "$${shard#*/}" ]; } || \
			{ echo "asan-test: ASANSHARD is i/n with 1 <= i <= n, such as 2/4, not $$shard" >&2; exit 1; }; \
		[ -n '$(ASANPKGS)' ] || { echo "asan-test: ASANSHARD needs ASANPKGS, the packages to shard" >&2; exit 1; }; \
	fi; \
	plan=$$(for dir in . $(CROSSCHECK); do \
		list=$$(cd $$dir && $(LINKSC) ./...) || { echo "asan-test: cannot list the packages of $$dir" >&2; exit 1; }; \
		sides=$$(echo "$$list" | awk $(LINKSCAWK)) || exit 1; \
		others=$$(echo "$$sides" | sed -n 's/^go //p'); \
		echo "asan-test: in $$dir, without C, so not under the sanitizer:" $${others:-none} >&2; \
		pkgs=$$(echo "$$sides" | sed -n 's/^c //p'); \
		[ -n "$$pkgs" ] || { echo "asan-test: no package in $$dir links C" >&2; exit 1; }; \
		for pkg in $$pkgs; do echo $$dir $$pkg; done; \
	done) || exit 1; \
	for pkg in $(ASANPKGS) $(ASANSKIPPKGS); do \
		echo "$$plan" | awk -v pkg="$$pkg" '$$2 == pkg { found = 1 } END { exit !found }' || \
			{ echo "asan-test: $$pkg is not a package that links C" >&2; exit 1; }; \
	done; \
	run=$$(echo "$$plan" | awk -v only='$(ASANPKGS)' -v skip='$(ASANSKIPPKGS)' ' \
		BEGIN { n = split(only, o, " "); for (i = 1; i <= n; i++) keep[o[i]] = 1; m = split(skip, s, " "); for (i = 1; i <= m; i++) drop[s[i]] = 1 } \
		(n == 0 || $$2 in keep) && !($$2 in drop)'); \
	[ -n "$$run" ] || { echo "asan-test: ASANPKGS and ASANSKIPPKGS leave no package to run" >&2; exit 1; }; \
	test='go test -asan -short -shuffle on -timeout $(ASANTIMEOUT)'; \
	for dir in . $(CROSSCHECK); do \
		pkgs=$$(echo "$$run" | awk -v dir="$$dir" '$$1 == dir { print $$2 }'); \
		[ -n "$$pkgs" ] || continue; \
		echo "asan-test: in $$dir, under the sanitizer:" $$pkgs; \
		if [ -z "$$shard" ]; then \
			(cd $$dir && $$test $$pkgs) || exit 1; \
			continue; \
		fi; \
		if [ "$${shard%/*}" = 1 ]; then \
			echo "asan-test: shard $$shard: every test but $(ASANSHARDED) and $(ASANLAST), whole"; \
			(cd $$dir && $$test -skip '^($(ASANSHARDED)|$(ASANLAST))$$' $$pkgs) || exit 1; \
		fi; \
		if [ "$${shard%/*}" = "$${shard#*/}" ]; then \
			echo "asan-test: shard $$shard: $(ASANLAST), whole"; \
			(cd $$dir && $$test -run '^($(ASANLAST))$$' $$pkgs) || exit 1; \
		fi; \
		echo "asan-test: shard $$shard: $(ASANSHARDED), with S4QL_TEST_SHARD=$$shard"; \
		out=$$(mktemp) || exit 1; \
		{ (cd $$dir && S4QL_TEST_SHARD=$$shard $$test -run '^($(ASANSHARDED))$$' $$pkgs); echo $$? >"$$out.status"; } | tee "$$out"; \
		status=$$(cat "$$out.status"); none=$$(grep -c 'no tests to run' "$$out"); \
		rm -f "$$out" "$$out.status"; \
		[ "$$status" = 0 ] || exit 1; \
		[ "$$none" = 0 ] || { echo "asan-test: a package of $$dir has no test in ASANSHARDED, $(ASANSHARDED)" >&2; exit 1; }; \
	done

# test-asan runs what CI's jobs for the address sanitizer run together: asan-test, the fuzz tests
# that link C under the sanitizer, and the other fuzz tests without it, each for FUZZTIME. Go
# supports -asan on Linux only, so elsewhere it runs in a Linux container with Docker, with Docker
# volumes for the module and build caches.
.PHONY: test-asan
test-asan:
ifeq ($(shell go env GOOS),linux)
	$(MAKE) asan-test
	$(MAKE) fuzz FUZZGROUP=c FUZZSHARD= FUZZFLAGS=-asan
	$(MAKE) fuzz FUZZGROUP=go FUZZSHARD= FUZZFLAGS=
else
	docker run --rm -v "$(CURDIR)":/src -v s4ql-gomod:/go/pkg/mod -v s4ql-gocache:/root/.cache/go-build -w /src \
		golang:$(shell sed -n 's/^go //p' go.mod) make test-asan FUZZTIME=$(FUZZTIME)
endif
