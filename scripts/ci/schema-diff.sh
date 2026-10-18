#!/usr/bin/env bash
# Compares the GraphQL schema of this branch with the base branch's and fails
# on a breaking change (a removed field, a changed type, a new required
# argument, ...) unless the pull request carries the label
# breaking-change-approved. Dangerous changes (a new union member or enum
# value that old clients may not handle) and safe ones are listed in the job
# summary but never fail the check.
#
# Env: BASE_SHA (required), APPROVED ("true" when the label is present),
#      GITHUB_STEP_SUMMARY (set by Actions; a temp file locally).
set -euo pipefail

: "${BASE_SHA:?BASE_SHA is required}"
APPROVED="${APPROVED:-false}"
SUMMARY="${GITHUB_STEP_SUMMARY:-/dev/null}"
SCHEMA_DIR="internal/adapter/graphql/schema"
INSPECTOR="@graphql-inspector/cli@7.0.0"

work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT

# The schema may span several files; the diff takes one document per side.
schema_at() { # $1 = git revision
  git ls-tree -r --name-only "$1" -- "$SCHEMA_DIR" | grep '\.graphqls$' | sort |
    while IFS= read -r f; do git show "$1:$f"; echo; done
}
schema_at "$BASE_SHA" >"$work/base.graphql"
schema_at HEAD >"$work/head.graphql"
if [ ! -s "$work/base.graphql" ] || [ ! -s "$work/head.graphql" ]; then
  echo "::error::could not read the schema at the base or the head revision"
  exit 1
fi

set +e
npx --yes "$INSPECTOR" diff "$work/base.graphql" "$work/head.graphql" >"$work/out.txt" 2>&1
rc=$?
set -e

breaking="$(grep -c '✖' "$work/out.txt" || true)"
dangerous="$(grep -c '⚠' "$work/out.txt" || true)"
safe="$(grep -c '✔' "$work/out.txt" || true)"
{
  echo "### GraphQL schema compatibility"
  echo
  echo "| breaking | dangerous | safe |"
  echo "|---|---|---|"
  echo "| ${breaking} | ${dangerous} | ${safe} |"
  echo
  echo '```'
  grep -E '✖|⚠|✔' "$work/out.txt" | sed -E 's/^\[log\] +//' || echo "no schema changes"
  echo '```'
} >>"$SUMMARY"
cat "$work/out.txt"

if [ "$rc" -eq 0 ]; then
  echo "schema check: no breaking changes"
  exit 0
fi
if [ "$breaking" -eq 0 ]; then
  echo "::error::graphql-inspector failed without reporting a breaking change (exit ${rc}); see the log above"
  exit 1
fi
if [ "$APPROVED" = "true" ]; then
  echo "::warning::${breaking} breaking schema change(s) approved by the breaking-change-approved label"
  echo "**${breaking} breaking change(s) approved** (label breaking-change-approved)." >>"$SUMMARY"
  exit 0
fi
echo "::error::${breaking} breaking schema change(s). Make the change additive (add a field, deprecate the old one), or label the PR breaking-change-approved after agreeing it with the clients."
exit 1
