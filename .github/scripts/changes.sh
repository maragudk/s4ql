#!/bin/sh
# changes.sh decides what a change touches, for the job changes of CI, which runs the other jobs only
# where the change needs them. It prints two lines for $GITHUB_OUTPUT:
#
#	code=true or code=false: whether the change touches code. With code=true every job runs. With
#	code=false the jobs that build, test, lint, fuzz, simulate, cross-check, and generate Go code do
#	not run.
#	site=true or site=false: whether the check of the site runs: with code, or when a path under site/
#	changes.
#
# The rule is an allowlist, because a wrong rule turns checks off without anyone seeing it. A change
# is not code only if every changed path is one of these, which no test, lint, or build of the Go
# code reads:
#
#	docs/**, site/**, .impeccable/**, PRODUCT.md, README.md, AGENTS.md, CLAUDE.md, LICENSE
#
# Any other path is code: everything under .github/ and internal/, the Makefile, go.mod, .env.test,
# and every file that nobody thought of. A Markdown file inside a Go package is code, as tests read
# some of them, such as internal/sql/stmt/README.md. A Go file, a go.mod, a go.sum, or a go.work is
# code wherever it is, even under docs/ or site/, since the Go tools would see it there. While a
# listed directory holds any of those at all, the mode event makes every change code, since the other
# files of such a package, such as those it embeds, cannot be told from documents. The check of
# the site, make site-check, reads site/ and builds the engine from the Go code. Add a path to the
# list only after checking that no job reads it.
#
# When the decision cannot be made, the change is code, so that everything runs: an empty list of
# changed paths, an event other than a push or a pull request (a schedule, a manual run), a forced
# push, a push whose previous commit is unknown or all zeros (a new branch), a commit that the
# checkout lacks (a shallow clone), and a diff that fails. git diff lists a renamed file under both
# of its names, so that a Go file moved into docs/ still counts as code.
#
# Usage:
#
#	sh changes.sh paths < list    decide for the changed paths in list, one a line
#	sh changes.sh event           decide for the event of the run, from git in the current directory
#
# The mode event reads EVENT, github.event_name; for a pull request BASE, the commit of its base, and
# for a push BEFORE, the commit before the push, and FORCED, whether it was forced; and HEAD, the
# commit that the run checks. A pull request is compared with its base, since HEAD is the commit
# that merges it into the base; a push is compared with the commit before it, which takes in all the
# commits of the push. The tests are in changes_test.sh; run make test-changes.

set -u

# everything prints the decision that runs every job, with the reason on standard error, and exits.
everything() {
	echo "changes: everything runs: $1" >&2
	echo code=true
	echo site=true
	exit 0
}

# listed exits with 0 if the path in $1 is not code.
listed() {
	case $1 in
	*.go | go.mod | */go.mod | go.sum | */go.sum | go.work | */go.work) return 1 ;;
	esac
	case $1 in
	docs/* | site/* | .impeccable/* | PRODUCT.md | README.md | AGENTS.md | CLAUDE.md | LICENSE) return 0 ;;
	esac
	return 1
}

# decide for the paths on standard input, one a line.
decide() {
	code=false
	site=false
	n=0
	while IFS= read -r path || [ -n "$path" ]; do
		[ -n "$path" ] || continue
		n=$((n + 1))
		listed "$path" || { code=true && echo "changes: code: $path" >&2; }
		case $path in
		site/*) site=true ;;
		esac
	done
	[ "$n" != 0 ] || everything 'no path changed'
	[ "$code" = false ] || site=true
	echo "changes: $n paths changed" >&2
	echo "code=$code"
	echo "site=$site"
}

# commit exits with 0 if $1 names a commit that the checkout has.
commit() {
	case $1 in
	'' | *[!0-9a-f]*) return 1 ;;
	esac
	case $1 in
	*[!0]*) ;;
	*) return 1 ;;
	esac
	git cat-file -e "$1^{commit}" 2>/dev/null
}

event() {
	case ${EVENT:-} in
	pull_request)
		base=${BASE:-}
		;;
	push)
		[ "${FORCED:-}" = false ] || everything "a push that may be forced: forced is '${FORCED:-}'"
		base=${BEFORE:-}
		;;
	*)
		everything "the event '${EVENT:-}' has no base to compare with"
		;;
	esac
	commit "$base" || everything "the base '$base' is not a commit in the checkout"
	commit "${HEAD:-}" || everything "the head '${HEAD:-}' is not a commit in the checkout"
	# A Go package in a listed directory would have files that are not Go files, such as those it
	# embeds, which the list cannot tell from documents. The names are split at NUL, so that git
	# does not quote them, and a name with a line feed in it can only add a line that matches.
	git ls-tree -r --name-only "$HEAD" -- docs site .impeccable >/dev/null ||
		everything 'git ls-tree failed'
	gocode=$(git ls-tree -r -z --name-only "$HEAD" -- docs site .impeccable | tr '\0' '\n' |
		grep -E '(\.go|(^|/)go\.(mod|work))$' | head -n 1)
	[ -z "$gocode" ] || everything "a listed directory holds Go code: $gocode"
	echo "changes: comparing $base with $HEAD" >&2
	# core.quotePath=false prints names with spaces and other letters as they are; a name that git
	# must still quote, such as one with a tab, starts with a quote and is not listed.
	files=$(git -c core.quotePath=false diff --no-renames --name-only "$base" "$HEAD" --) ||
		everything 'git diff failed'
	printf '%s\n' "$files" | decide
}

case ${1:-} in
paths) decide ;;
event) event ;;
*)
	echo 'usage: changes.sh paths < list, or changes.sh event' >&2
	exit 2
	;;
esac
