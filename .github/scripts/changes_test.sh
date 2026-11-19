#!/bin/sh
# Tests of changes.sh: a table of lists of changed paths and the decision for each, and runs of the
# decision for an event against commits in a temporary repository, which check the diff itself, such
# as that a renamed file is listed under both of its names. Run it with make test-changes. It prints
# each case that fails and exits with 1 if any did.

set -u

# SH is the shell that runs changes.sh, such as SH=dash, the sh of the runners of CI.
SH=${SH:-sh}
script=$(cd "$(dirname "$0")" && pwd)/changes.sh
failed=0
count=0

# check that what changes.sh printed, in got, is want, for the case named in $1.
check() {
	count=$((count + 1))
	if [ "$got" != "$want" ]; then
		failed=$((failed + 1))
		printf 'FAIL %s\n  want: %s\n  got:  %s\n' "$1" "$(echo $want)" "$(echo $got)"
	fi
}

# paths runs the decision for a list of paths, given as the lines of $2, and checks it against the
# code and site of $3 and $4.
paths() {
	got=$(printf '%s' "$2" | $SH "$script" paths 2>/dev/null)
	want=$(printf 'code=%s\nsite=%s' "$3" "$4")
	check "paths: $1"
}

nl='
'

paths 'only documents' "docs/todo.md${nl}docs/conversations/2026-11-19-ci.txt${nl}README.md${nl}AGENTS.md${nl}CLAUDE.md${nl}" false false
paths 'every listed file at the top' "PRODUCT.md${nl}README.md${nl}AGENTS.md${nl}CLAUDE.md${nl}LICENSE${nl}" false false
paths 'the design tool' ".impeccable/config.json${nl}.impeccable/surfaces/site-index-html.md${nl}" false false
paths 'only the site' "site/index.html${nl}site/simulator/page.js${nl}" false true
paths 'the site and documents' "site/index.html${nl}docs/roadmap.md${nl}" false true
paths 'one Go file' "internal/core/core.go${nl}" true true
paths 'a document and a Go file' "docs/todo.md${nl}conn.go${nl}" true true
paths 'a Go file under docs' "docs/research/example.go${nl}" true true
paths 'a go.mod under site' "site/tool/go.mod${nl}" true true
paths 'a workflow file' ".github/workflows/ci.yml${nl}" true true
paths 'this script' ".github/scripts/changes.sh${nl}" true true
paths 'the Makefile' "Makefile${nl}docs/todo.md${nl}" true true
paths '.env.test' ".env.test${nl}" true true
paths 'go.mod' "go.mod${nl}" true true
paths 'a Markdown file inside a Go package' "internal/sql/stmt/README.md${nl}" true true
paths 'a Markdown file inside a Go package that no test reads' "internal/core/README.md${nl}" true true
paths 'a README in another directory' "proto/README.md${nl}" true true
paths 'a file named like a listed one, deeper' "internal/LICENSE${nl}internal/docs/x.md${nl}" true true
paths 'a file named like a listed directory' "docs${nl}" true true
paths 'a listed name with a suffix' "README.md.orig${nl}" true true
paths 'a path with a space' "docs/research/a note.md${nl}site/a file.html${nl}" false true
paths 'a path with a space outside the list' "internal/a dir/a.go${nl}" true true
paths 'a path that git quotes' "\"docs/a\\tb.md\"${nl}" true true
paths 'a list without a final line feed' "docs/todo.md${nl}site/index.html" false true
paths 'an empty list' "" true true
paths 'a list of empty lines' "${nl}${nl}" true true

# A temporary repository with a history to diff, and runs of the decision for an event in it. The
# git configuration of the machine, such as hooks or a template, stays out of it.
GIT_CONFIG_GLOBAL=/dev/null
GIT_CONFIG_NOSYSTEM=1
export GIT_CONFIG_GLOBAL GIT_CONFIG_NOSYSTEM
repo=$(mktemp -d)
trap 'rm -rf "$repo"' EXIT
g() { git -C "$repo" -c user.name=test -c user.email=test@example.com -c commit.gpgsign=false -c core.hooksPath=/dev/null "$@"; }
commit() { g add -A && g commit -q -m "$1" && g rev-parse HEAD; }
die() {
	echo 'changes_test: cannot make the temporary repository' >&2
	exit 1
}

g init -q --template= || die
mkdir -p "$repo/docs" "$repo/internal/a" "$repo/site"
echo a >"$repo/docs/a.md"
echo 'package a' >"$repo/internal/a/a.go"
echo b >"$repo/internal/a/b.go"
printf 'line %s\n' 1 2 3 4 5 6 7 8 >"$repo/internal/a/notes.txt"
echo s >"$repo/site/index.html"
c0=$(commit 'the start') || die

echo b >>"$repo/docs/a.md"
c1=$(commit 'a document') || die

echo s >>"$repo/site/index.html"
c2=$(commit 'the site') || die

# A file of a Go package that is not a Go file, renamed into docs, which git diff sees as a rename
# unless it is told not to. It comes before any Go file is in docs, which would make it code anyway.
g mv internal/a/notes.txt docs/notes.txt
r1=$(commit 'a text file of a Go package renamed into docs') || die

g mv internal/a/a.go docs/a.go
c3=$(commit 'a Go file renamed into docs') || die

g mv docs/a.md internal/a/a.md
c4=$(commit 'a document renamed into a Go package') || die

# The Go file in docs goes too, so that the commits after this one have no Go file under docs.
g rm -q internal/a/b.go docs/a.go
c5=$(commit 'two Go files deleted') || die

echo n >"$repo/docs/a note.md"
c6=$(commit 'a document with a space in its name') || die

g commit -q --allow-empty -m 'nothing' || die
c7=$(g rev-parse HEAD) || die

# A name that git quotes unless core.quotePath is false.
echo u >"$repo/docs/über.md"
c8=$(commit 'a document with a letter outside ASCII in its name') || die

zeros=0000000000000000000000000000000000000000

# event runs the decision for the event with the variables in $2, and checks it as paths does.
event() {
	got=$(cd "$repo" && env $2 $SH "$script" event 2>/dev/null)
	want=$(printf 'code=%s\nsite=%s' "$3" "$4")
	check "event: $1"
}

event 'a push of a document' "EVENT=push BEFORE=$c0 HEAD=$c1 FORCED=false" false false
event 'a push of the site' "EVENT=push BEFORE=$c1 HEAD=$c2 FORCED=false" false true
event 'a push of two commits, documents and the site' "EVENT=push BEFORE=$c0 HEAD=$c2 FORCED=false" false true
event 'a push of a text file of a Go package renamed into docs' "EVENT=push BEFORE=$c2 HEAD=$r1 FORCED=false" true true
event 'a push of a Go file renamed into docs' "EVENT=push BEFORE=$r1 HEAD=$c3 FORCED=false" true true
event 'a push of a document renamed into a Go package' "EVENT=push BEFORE=$c3 HEAD=$c4 FORCED=false" true true
event 'a push of a deleted Go file' "EVENT=push BEFORE=$c4 HEAD=$c5 FORCED=false" true true
event 'a push of a document with a space' "EVENT=push BEFORE=$c5 HEAD=$c6 FORCED=false" false false
event 'a push of many commits, the last a document' "EVENT=push BEFORE=$c2 HEAD=$c6 FORCED=false" true true
event 'a push of an empty commit' "EVENT=push BEFORE=$c6 HEAD=$c7 FORCED=false" true true
event 'a push of a document with a letter outside ASCII' "EVENT=push BEFORE=$c7 HEAD=$c8 FORCED=false" false false
event 'a forced push' "EVENT=push BEFORE=$c0 HEAD=$c1 FORCED=true" true true
event 'a push of a new branch' "EVENT=push BEFORE=$zeros HEAD=$c1 FORCED=false" true true
event 'a push without a previous commit' "EVENT=push BEFORE= HEAD=$c1 FORCED=false" true true
event 'a push whose previous commit is not in the checkout' "EVENT=push BEFORE=1234567890123456789012345678901234567890 HEAD=$c1 FORCED=false" true true
event 'a push whose previous commit is not a commit name' "EVENT=push BEFORE=--output=x HEAD=$c1 FORCED=false" true true
event 'a push whose commit is not in the checkout' "EVENT=push BEFORE=$c0 HEAD=1234567890123456789012345678901234567890 FORCED=false" true true
event 'a pull request of a document' "EVENT=pull_request BASE=$c0 HEAD=$c1" false false
event 'a pull request of a Go file' "EVENT=pull_request BASE=$c4 HEAD=$c5" true true
event 'a pull request without a base' "EVENT=pull_request BASE= HEAD=$c1" true true
event 'a schedule' "EVENT=schedule BEFORE=$c0 BASE=$c0 HEAD=$c1" true true
event 'a manual run' "EVENT=workflow_dispatch BEFORE=$c0 BASE=$c0 HEAD=$c1" true true
event 'no event' "EVENT= BEFORE=$c0 BASE=$c0 HEAD=$c1" true true

# A shallow clone of the last commit lacks the commit before it. In the whole repository, the same
# push is of a document only.
shallow=$(mktemp -d)
trap 'rm -rf "$repo" "$shallow"' EXIT
git clone -q --depth 1 "file://$repo" "$shallow/clone" 2>/dev/null
got=$(cd "$shallow/clone" && env EVENT=push BEFORE="$c7" HEAD="$c8" FORCED=false $SH "$script" event 2>/dev/null)
want=$(printf 'code=true\nsite=true')
check 'event: a shallow checkout that lacks the previous commit'

# A Go package under site, and a change to a file of it that is not a Go file, such as one that it
# embeds. While a listed directory holds a Go package, every change is code.
mkdir -p "$repo/site/tool"
echo 'module tool' >"$repo/site/tool/go.mod"
echo 'package main' >"$repo/site/tool/main.go"
echo t >"$repo/site/tool/page.html"
p1=$(commit 'a Go package under site') || die
echo t >>"$repo/site/tool/page.html"
p2=$(commit 'a file that the Go package under site embeds') || die
echo c >>"$repo/docs/a note.md"
p3=$(commit 'a document, while site holds a Go package') || die
mkdir -p "$repo/docs/pkg"
g rm -q -r site/tool
echo 'package pkg' >"$repo/docs/pkg/pkg.go"
p4=$(commit 'the Go package under site removed, and one under docs') || die
echo c >>"$repo/docs/a note.md"
p5=$(commit 'a document, while docs holds a Go package') || die

event 'a push of a Go package under site' "EVENT=push BEFORE=$c8 HEAD=$p1 FORCED=false" true true
event 'a push of a file of a Go package under site' "EVENT=push BEFORE=$p1 HEAD=$p2 FORCED=false" true true
event 'a push of a document, while site holds a Go package' "EVENT=push BEFORE=$p2 HEAD=$p3 FORCED=false" true true
event 'a push of a document, while docs holds a Go package' "EVENT=push BEFORE=$p4 HEAD=$p5 FORCED=false" true true

# A mode that the script does not know prints no decision and fails, so that the job changes fails,
# and every job runs.
got=$($SH "$script" other </dev/null 2>/dev/null)
status=$?
want=''
check 'an unknown mode prints nothing'
count=$((count + 1))
if [ "$status" = 0 ]; then
	failed=$((failed + 1))
	echo 'FAIL an unknown mode exits with 0'
fi

if [ "$failed" != 0 ]; then
	echo "changes_test: $failed of $count cases failed"
	exit 1
fi
echo "changes_test: all $count cases passed"
