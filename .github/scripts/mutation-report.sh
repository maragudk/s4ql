#!/bin/sh
# mutation-report.sh writes the report of a round of the workflow mutation.yml into the file in $1,
# in Markdown, for the summary of the run and its issue, and prints issue=true or issue=false for
# $GITHUB_OUTPUT: whether the round leaves work for a builder, which is an unexplained survivor, a
# stale entry of the list of equivalent mutants, a mutant without a result, a shard that failed,
# or a merge that failed.
#
# It reads the merged results in merged/, written by the runner's -merge, and these variables:
#
#	SHA            the commit of the round
#	SINCE          the input since: round, all, or a commit
#	BASE           the commit that the round diffs against, empty for all
#	PKGS, RUN      the inputs pkgs and run
#	MUTANTS        the number of mutants of the selection
#	SHARDS         the number of shards that ran them
#	SHARD_RESULT   the result of the job shard: success, failure, cancelled, or skipped
#	MERGE_OUTCOME  the outcome of the step that merged the shards
#	RUN_URL        the address of the run

set -u

out=$1
work=false

{
	echo "## Mutation testing at ${SHA}"
	echo
	selection="every declaration of \`${PKGS}\`"
	if [ -n "$BASE" ]; then
		selection="the declarations of \`${PKGS}\` that changed since ${BASE}"
		if [ "$SINCE" = round ]; then
			selection="$selection, the last round in \`internal/sql/mutation/round.txt\`"
		fi
	fi
	if [ -n "$RUN" ]; then
		selection="$selection, whose key or \`file:line\` matches \`${RUN}\`"
	fi
	echo "The run ${RUN_URL} mutated ${selection}: ${MUTANTS} mutants, in ${SHARDS} shards."
	echo

	if [ "$MUTANTS" = 0 ]; then
		echo "No declaration changed, so there is nothing to mutate."
	else
		if [ "$SHARD_RESULT" != success ]; then
			work=true
			echo "**The shards did not all succeed** (${SHARD_RESULT}). The log of each failed shard says why. The mutants that it did not run have no result below; start the workflow again with the same inputs to run them."
			echo
		fi
		if [ "$MERGE_OUTCOME" != success ] || [ ! -f merged/summary.md ]; then
			work=true
			echo "**The merge of the shards failed**; its log says why."
		else
			cat merged/summary.md
			for name in unexplained stale pending; do
				if ! grep -qx "$name=0" merged/counts; then
					work=true
				fi
			done
		fi
	fi
	echo
	echo "### The round"
	echo
	if [ "$SINCE" = round ] && [ -z "$RUN" ] && [ "$PKGS" = all ]; then
		echo "Work through what is above: kill each unexplained survivor with a test, remove the code that nothing needs, or list it in \`internal/sql/mutation/equivalent.txt\` with its reason, and remove the stale entries, as the runner's README says. Then write ${SHA} into \`internal/sql/mutation/round.txt\` in the same pull request, and close the issue of the round. The next round mutates what changed since."
	else
		echo "This run ran a selection of its own, so it does not complete a round: \`internal/sql/mutation/round.txt\` stays as it is."
	fi
} >"$out"

echo "issue=$work"
