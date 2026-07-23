#!/bin/bash
# Renders a tmt run directory as a GitHub Actions job summary. The junit
# rollup only counts tests, and a tier is one shell test — "1 passed" says
# nothing. The tier scripts already narrate every assertion (=== / ok: /
# FAIL:), so surface that instead: one collapsible section per executed
# test with its verdict, failures expanded by default.
#
# Usage: gha-summary.sh /var/tmp/orthogonals-tmt-vm >> "$GITHUB_STEP_SUMMARY"
set -euo pipefail

run=$1
found=0
for results in "$run"/plans/*/execute/results.yaml; do
	[ -e "$results" ] || continue
	found=1
	plan=${results%/execute/results.yaml}
	plan=${plan#"$run"/plans/}
	echo "### tmt plan \`/plans/$plan\`"
	echo
	# results.yaml is tmt-generated: each result's own keys sit at two-space
	# indent (nested `name:`s sit deeper), and `name` dumps before `result`,
	# so this pairs every test with the verdict that follows it.
	while read -r key value; do
		case $key in
		name:) name=$value ;;
		result:)
			icon="✅"
			open=""
			if [ "$value" != pass ]; then
				icon="❌"
				open=" open"
			fi
			echo "<details$open><summary>$icon <code>$name</code> — $value</summary>"
			echo
			echo '```'
			# The test's own narration; its data dir is <name>-<serial>.
			grep -hE '^(=== |ok: |FAIL|--- SKIP)' \
				"${results%results.yaml}"data/guest/*/"${name#/}"-*/output.txt 2>/dev/null ||
				echo "no step output captured"
			echo '```'
			echo "</details>"
			;;
		esac
	done < <(grep -E '^  (name|result): ' "$results")
	echo
done
[ "$found" = 1 ] || echo "No tmt results — the run died before the execute step."
