#!/bin/sh
# mutation-report.sh writes the report of a round of the workflow mutation.yml into the file in $1,
# in Markdown, for the summary of the run and its issue, and prints two lines for $GITHUB_OUTPUT:
# issue=true when the round leaves work for a builder, which is an unexplained survivor, a stale
# entry of the list of equivalent mutants, a mutant without a result, a shard that failed, or a
# merge that failed; and complete=true when the round is of the default selection, whole, and left
# no work, so that it records itself.
#
# It reads the merged results in merged/, written by the runner's -merge, the timing of each shard
# in shards/*/timing, the jobs of the run from GitHub's API, and these variables:
#
#	SHA            the commit of the round
#	SINCE          the input since: round, all, or a commit
#	BASE           the commit that the round diffs against, empty for all
#	PKGS, RUN      the inputs pkgs and run
#	SAMPLE, SEED   the inputs sample and seed
#	MUTANTS        the number of mutants of the selection
#	SHARDS         the number of shards that ran them
#	SHARD_RESULT   the result of the job shard: success, failure, cancelled, or skipped
#	MERGE_OUTCOME  the outcome of the step that merged the shards
#	GH_REPO, GH_TOKEN, RUN_ID, RUN_URL  the repository, a token that reads its runs, and the run

set -u

out=$1
work=false
default=false
if [ "$SINCE" = round ] && [ -z "$RUN" ] && [ "$PKGS" = all ] && [ "$SAMPLE" = 1 ]; then
	default=true
fi

# seconds of a time of GitHub's API, such as 2026-11-26T13:22:04Z.
seconds() {
	date -u -d "$1" +%s
}

{
	echo "## Mutation testing at ${SHA}"
	echo
	selection="every declaration of \`${PKGS}\`"
	if [ -n "$BASE" ]; then
		selection="the declarations of \`${PKGS}\` that changed since ${BASE}"
		if [ "$SINCE" = round ]; then
			selection="$selection, the last recorded round"
		fi
	fi
	if [ -n "$RUN" ]; then
		selection="$selection, whose key or \`file:line\` matches \`${RUN}\`"
	fi
	if [ "$SAMPLE" != 1 ]; then
		selection="$selection, in a sample of ${SAMPLE} by the seed ${SEED}"
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

	# The cost of the round: the runner-minutes of the jobs plan and shard, which this job does not
	# count itself, and how much of the shards' time went to their setup and their planning.
	jobs=$(gh api "repos/$GH_REPO/actions/runs/$RUN_ID/jobs?per_page=100" --jq '.jobs[] | select(.status == "completed" and .started_at != null and .completed_at != null) | [.name, .started_at, .completed_at, ([.steps[]? | select(.name == "Run the mutants of the shard") | .started_at][0] // "-")] | @tsv') || jobs=
	total=0
	setup=0
	n=0
	tab=$(printf '\t')
	while IFS="$tab" read -r name start end step; do
		[ -n "$name" ] || continue
		s=$(seconds "$start")
		total=$((total + $(seconds "$end") - s))
		if [ "$step" != - ]; then
			setup=$((setup + $(seconds "$step") - s))
			n=$((n + 1))
		fi
	done <<EOF
$jobs
EOF
	plan=$(sed -n 's/^plan=//p' shards/*/timing 2>/dev/null | awk '{s += $1} END {print s + 0}')
	unexplained=$(sed -n 's/^unexplained=//p' merged/counts 2>/dev/null)
	echo
	echo "### The cost of the round"
	echo
	if [ "$total" -gt 0 ]; then
		echo "The jobs plan and shard used $(awk -v t="$total" 'BEGIN {printf "%.0f", t / 60}') runner-minutes, $(awk -v m="$MUTANTS" -v t="$total" 'BEGIN {printf "%.1f", m / (t / 60)}') mutants a runner-minute, and found ${unexplained:-no count of} unexplained survivors."
		if [ "$n" -gt 0 ]; then
			echo "Each of the ${n} shards spent on average $(awk -v s="$setup" -v n="$n" 'BEGIN {printf "%.1f", s / n / 60}') minutes on its checkout and Go before its run, and $(awk -v s="$plan" -v n="$n" 'BEGIN {printf "%.1f", s / n / 60}') minutes on the planning of its stages; a round with fewer shards repeats that less."
		fi
	else
		echo "The runner-minutes of the run could not be read from GitHub's API."
	fi

	echo
	echo "### The round"
	echo
	if [ "$default" = true ] && [ "$work" = false ]; then
		echo "The round counts from ${BASE} and left no work, so on \`main\` it records itself: the next round counts from ${SHA}."
	elif [ "$default" = true ]; then
		echo "The round counts from ${BASE}, the last recorded round, and until a new one is recorded, every round reports again what it lists here, with what changed since. Work through it: kill each unexplained survivor with a test, remove the code that nothing needs, or list it in \`internal/sql/mutation/equivalent.txt\` with its reason, and remove the stale entries, as the runner's README says. Then write ${SHA} into \`internal/sql/mutation/round.txt\` in the same pull request, and close the issue of the round."
	else
		echo "This run ran a selection of its own, or a sample, so it does not complete a round: the last recorded round stays as it is."
	fi
} >"$out"

complete=false
if [ "$default" = true ] && [ "$work" = false ]; then
	complete=true
fi
echo "issue=$work"
echo "complete=$complete"
