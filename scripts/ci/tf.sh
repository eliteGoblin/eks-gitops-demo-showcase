#!/usr/bin/env bash
# Terraform steps shared by the IaC workflows and `make tf-plan`.
#
#   tf.sh check <root>                 fmt, init (no lock; no backend if the state holds secrets: cloudflare), validate, tflint, Trivy config (report-only)
#   tf.sh plan  <root>                 read-only plan (no lock) + redacted Markdown summary
#   tf.sh apply-roots <roots> [plan]   for each root, in order: plan with -detailed-exitcode, apply if it changed
#
# Every check, plan and apply fails the job on error: each step is an explicit
# if/else (a `cmd && summary` list would let a failure slip past `set -e`).
# Summaries never include attribute values: a plan can carry sensitive data, so
# only resource addresses and actions are printed (to stdout and, in CI, to
# $GITHUB_STEP_SUMMARY and <root>.summary.md for the PR comment).
set -euo pipefail

# eks reads bootstrap's state, argocd reads eks's; cloudflare reads no state but
# uses secret grants made in eks.
ROOTS_IN_ORDER="bootstrap eks argocd cloudflare"
# Roots whose state holds secret values. The pull-request role may not read
# their state (infra/bootstrap denies it), so their checks init without the backend.
SECRET_STATE_ROOTS="cloudflare"
REPO_ROOT=$(cd "$(dirname "$0")/../.." && pwd)
# A saved plan holds the state's values in clear (for cloudflare: the tunnel
# token), so it never outlives the script, whichever way the script ends.
trap 'rm -f "$REPO_ROOT"/infra/*/tfplan.bin' EXIT
TRIVY_IMAGE=${TRIVY_IMAGE:-aquasec/trivy:0.76.0}
LOCK_TIMEOUT=${TF_LOCK_TIMEOUT:-10m} # wait this long for a laptop or CI apply that holds the S3 lockfile

root_dir() { echo "$REPO_ROOT/infra/$1"; }

summary() { # append Markdown to the step summary when running in CI, and echo it
  if [ -n "${GITHUB_STEP_SUMMARY:-}" ]; then printf '%s\n' "$*" >> "$GITHUB_STEP_SUMMARY"; fi
  printf '%s\n' "$*"
}

step() { # $1 label, rest: the command; records ok/FAILED in the summary and stops the job on failure
  local label=$1; shift
  if "$@"; then summary "- $label: ok"; else summary "- $label: **FAILED**"; exit 1; fi
}

tf_init() { # $1 root, $2 lock (true|false), $3 backend (true|false, default true)
  terraform -chdir="$(root_dir "$1")" init -input=false -no-color -lock="$2" -backend="${3:-true}" >/dev/null
}

# The Cloudflare provider authenticates only on its first API call, so without
# a token a plan with nothing to refresh would still "succeed". Fail instead.
require_credentials() { # $1 root
  if [ "$1" = cloudflare ] && [ -z "${CLOUDFLARE_API_TOKEN:-}" ]; then
    summary "### \`infra/$1\`: **FAILED**: CLOUDFLARE_API_TOKEN is not set"; exit 1
  fi
}

# Redacted summary of a saved plan: counts plus "action address" lines, no values.
plan_summary() { # $1 root, $2 plan file (relative to the root dir)
  local dir; dir=$(root_dir "$1")
  local json; json=$(terraform -chdir="$dir" show -json "$2")
  local adds changes destroys
  adds=$(jq '[.resource_changes[]? | select(.change.actions | index("create"))] | length' <<<"$json")
  changes=$(jq '[.resource_changes[]? | select(.change.actions | index("update"))] | length' <<<"$json")
  destroys=$(jq '[.resource_changes[]? | select(.change.actions | index("delete"))] | length' <<<"$json")
  if [ "$adds$changes$destroys" = "000" ]; then
    echo "**No changes.** Infrastructure matches the configuration."
    return
  fi
  echo "**Plan: $adds to add, $changes to change, $destroys to destroy.**"
  echo
  echo '```'
  jq -r '.resource_changes[]? | select(.change.actions != ["no-op"])
         | (.change.actions | join("/")) as $a
         | (if $a == "create" then "+" elif $a == "delete" then "-" elif $a == "update" then "~"
            elif ($a | test("delete")) then "-/+" else $a end) + " " + .address' <<<"$json"
  echo '```'
  local outs; outs=$(jq -r '.output_changes // {} | to_entries[] | select(.value.actions != ["no-op"]) | "- output `\(.key)` \(.value.actions | join("/"))"' <<<"$json")
  [ -z "$outs" ] || { echo; echo "$outs"; }
}

cmd_check() {
  local root=$1 dir backend=true; dir=$(root_dir "$root")
  case " $SECRET_STATE_ROOTS " in *" $root "*) backend=false ;; esac
  summary "### \`infra/$root\`: checks"
  step fmt      terraform -chdir="$dir" fmt -check -recursive -no-color
  step init     tf_init "$root" false "$backend"
  step validate terraform -chdir="$dir" validate -no-color
  step tflint   tflint --chdir="$dir" --config="$REPO_ROOT/.tflint.hcl" --format compact
  # Trivy config is report-only, like the other security scans in pr.yaml.
  local report="$REPO_ROOT/trivy-config-$root.json"
  if command -v trivy >/dev/null; then
    trivy config --quiet --severity HIGH,CRITICAL --exit-code 0 --format json --output "$report" "$dir"
  else
    docker run --rm -v "$REPO_ROOT:/w" -w /w "$TRIVY_IMAGE" config --quiet --severity HIGH,CRITICAL --exit-code 0 \
      --format json --output "trivy-config-$root.json" "infra/$root"
  fi
  summary "$("$REPO_ROOT/scripts/ci/summarize-trivy.sh" "Trivy config (report-only)" "$report")"
}

cmd_plan() { # read-only: no lock, no state writes; the plan file stays local
  local root=$1 dir; dir=$(root_dir "$root")
  require_credentials "$root"
  if ! tf_init "$root" false; then summary "### \`infra/$root\`: init **FAILED**"; exit 1; fi
  if ! terraform -chdir="$dir" plan -input=false -no-color -lock=false -refresh=true -out=tfplan.bin >/dev/null; then
    summary "### \`infra/$root\`: plan **FAILED** (see the job log)"; exit 1
  fi
  { echo "### \`infra/$root\`: plan"; echo; plan_summary "$root" tfplan.bin; } | tee "$REPO_ROOT/$root.summary.md"
  [ -z "${GITHUB_STEP_SUMMARY:-}" ] || cat "$REPO_ROOT/$root.summary.md" >> "$GITHUB_STEP_SUMMARY"
  rm -f "$dir/tfplan.bin"
}

cmd_apply_roots() { # $1 comma-separated roots, $2 "plan" for plan-only
  local wanted mode=${2:-apply} root dir rc
  wanted=",$(tr -d ' ' <<<"$1"),"
  for root in $(tr ',' ' ' <<<"$1"); do # a typo must fail, not skip a root
    case " $ROOTS_IN_ORDER " in *" $root "*) ;; *) summary "Unknown root \`$root\` (roots: $ROOTS_IN_ORDER)"; exit 1 ;; esac
  done
  for root in $ROOTS_IN_ORDER; do
    case "$wanted" in *",$root,"*) ;; *) continue ;; esac
    dir=$(root_dir "$root")
    require_credentials "$root"
    if ! tf_init "$root" true; then summary "### \`infra/$root\`: init **FAILED**"; exit 1; fi
    set +e
    terraform -chdir="$dir" plan -input=false -no-color -detailed-exitcode -lock-timeout="$LOCK_TIMEOUT" -out=tfplan.bin >/dev/null
    rc=$?
    set -e
    case $rc in
      0) summary "### \`infra/$root\`"; summary "No changes." ;;
      2) summary "### \`infra/$root\`"; summary "$(plan_summary "$root" tfplan.bin)"
         if [ "$mode" = plan ]; then
           summary "_Plan only: not applied._"
         elif terraform -chdir="$dir" apply -input=false -no-color -auto-approve -lock-timeout="$LOCK_TIMEOUT" tfplan.bin >/dev/null; then
           summary "**Applied.**"
         else
           summary "**Apply FAILED** (see the job log); later roots were not run."
           rm -f "$dir/tfplan.bin"; exit 1
         fi ;;
      *) summary "### \`infra/$root\`: plan **FAILED** (see the job log)"; exit 1 ;;
    esac
    rm -f "$dir/tfplan.bin"
  done
}

case "${1:-}" in
  check)       cmd_check "$2" ;;
  plan)        cmd_plan "$2" ;;
  apply-roots) cmd_apply_roots "$2" "${3:-apply}" ;;
  *) sed -n '2,7p' "$0"; exit 2 ;;
esac
