#!/usr/bin/env bash
# Fails if any secrets/<env>/<service>.enc.yaml is not sops-encrypted: the
# trailing `sops:` metadata block must be present, and every value before it
# must be an ENC[...] token (map values and list items alike). Runs on pull
# requests, so a plaintext secret is refused before it can reach history
# (secrets-sync only runs on main). No sops binary needed: the file format is
# checked, not decrypted.
set -euo pipefail
dir=${1:-secrets}
shopt -s nullglob
status=0
for f in "$dir"/*/*.enc.yaml; do
  if awk -v f="$f" '
      function plaintext(why) { print f ": plaintext " why " at line " NR; bad = 1 }
      /^sops:/ { insops = 1 }
      insops { next }
      {
        line = $0
        sub(/^[[:space:]]+/, "", line)
        if (line == "" || line ~ /^#/) next              # blank or comment
        if (sub(/^-[[:space:]]*/, "", line) && line == "") next  # bare "-" opening a list of maps
        if (line ~ /^ENC\[/) next                        # encrypted scalar or list item
        if (match(line, /^[^:[:space:]][^:]*:([[:space:]]|$)/)) {
          v = substr(line, RLENGTH + 1); sub(/^[[:space:]]+/, "", v)
          if (v != "" && v !~ /^ENC\[/) plaintext("value")
          next                                           # "key:" alone opens a nested map or list
        }
        plaintext("line")                                # anything else: list item, block-scalar text
      }
      END { if (!insops) { print f ": no sops metadata block"; bad = 1 }; exit bad }' "$f"; then
    echo "$f: encrypted"
  else
    status=1
  fi
done
exit $status
