#!/bin/sh
# fuzz-failures.sh prints each new file under a testdata/fuzz directory of the checkout in the
# current directory, with its path. go test writes such a file when a fuzz test fails: the input
# that made it fail, in the form that go test reads from there. Committed at the path that this
# prints, the file runs as a case of its fuzz test in every go test after.

set -u

files=$(git ls-files --others --exclude-standard -- '*testdata/fuzz/*') || exit 1
if [ -z "$files" ]; then
	echo "fuzz-failures: no new input under testdata/fuzz"
	exit 0
fi
echo "$files" | while IFS= read -r f; do
	echo "fuzz-failures: $f:"
	cat "$f"
	echo
done
