#!/usr/bin/env bash
# Syncs sops-encrypted files into AWS Secrets Manager (idempotent).
#
#   secrets/<env>/<service>.enc.yaml
#     <key>: <scalar or map>        →  secret demo/<env>/<service>/<key>
#
# A map becomes a JSON SecretString (External Secrets can pick fields with
# `property`); a scalar is stored as-is, byte for byte (no trailing newline).
# Unchanged secrets are left alone, so re-runs create no new versions. A key
# removed or renamed in the file leaves its old secret behind (delete it by
# hand). Plaintext exists only in this process.
set -euo pipefail

dir=${1:-secrets}
prefix=${SECRET_PREFIX:-demo}
created=0 updated=0 unchanged=0

note() { # names only, never values
  printf '%s %s\n' "$1" "$2"
  [ -z "${GITHUB_STEP_SUMMARY:-}" ] || printf -- '- %s `%s`\n' "$1" "$2" >> "$GITHUB_STEP_SUMMARY"
}

canon() { jq -cS . <<<"$1" 2>/dev/null || printf '%s' "$1"; } # key order in JSON does not count as a change

# The value travels on stdin as-is: a here-string would append a newline, and
# the command line would show it in `ps`.
put() { printf '%s' "$2" | aws secretsmanager put-secret-value --secret-id "$1" --secret-string file:///dev/stdin >/dev/null; }
create() {
  printf '%s' "$3" | aws secretsmanager create-secret --name "$1" --description "sops-managed: $2" \
    --tags Key=ManagedBy,Value=sops Key=Source,Value="${2%% *}" --secret-string file:///dev/stdin >/dev/null
}

shopt -s nullglob
files=("$dir"/*/*.enc.yaml)
if [ ${#files[@]} -eq 0 ]; then echo "no files under $dir/*/*.enc.yaml"; exit 0; fi
[ -z "${GITHUB_STEP_SUMMARY:-}" ] || printf '### secrets-sync\n\n' >> "$GITHUB_STEP_SUMMARY"

for f in "${files[@]}"; do
  env=$(basename "$(dirname "$f")")
  service=$(basename "$f" .enc.yaml)
  plain=$(sops --decrypt --output-type json "$f")
  while IFS= read -r key; do
    name="$prefix/$env/$service/$key"
    desired=$(jq -r --arg k "$key" '.[$k] | if type == "object" or type == "array" then tojson else tostring end' <<<"$plain")
    if current=$(aws secretsmanager get-secret-value --secret-id "$name" --query SecretString --output text 2>/dev/null </dev/null); then
      if [ "$(canon "$current")" = "$(canon "$desired")" ]; then unchanged=$((unchanged+1)); note unchanged "$name"; continue; fi
      put "$name" "$desired"; updated=$((updated+1)); note updated "$name"
    elif aws secretsmanager describe-secret --secret-id "$name" >/dev/null 2>&1 </dev/null; then
      # The container exists (for example created by Terraform) but has no value yet.
      put "$name" "$desired"; updated=$((updated+1)); note updated "$name"
    else
      create "$name" "$f ($key)" "$desired"; created=$((created+1)); note created "$name"
    fi
  done < <(jq -r 'keys[]' <<<"$plain")
done

echo "created=$created updated=$updated unchanged=$unchanged"
[ -z "${GITHUB_STEP_SUMMARY:-}" ] || printf '\ncreated **%s**, updated **%s**, unchanged **%s**\n' "$created" "$updated" "$unchanged" >> "$GITHUB_STEP_SUMMARY"
