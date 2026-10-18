#!/usr/bin/env bash
# Offline test of the demo users' passwords in scripts/secrets-seed.sh (no AWS, no
# GitHub): stub aws and gh on PATH, run the real script, and check that
#   - it seeds demo/keycloak/users once, from a file (no password on any argv or in
#     its output), with exactly the five users, five distinct values, each
#     Word-Word-NNNN-symbol;
#   - an existing value is kept unless ROTATE_DEMO_PASSWORDS=1;
#   - a broken random source (od failing, or printing garbage) fails closed:
#     non-zero exit and no upload.
set -uo pipefail
HERE=$(cd "$(dirname "$0")" && pwd); SEED=$HERE/../secrets-seed.sh
W=$(mktemp -d); trap 'rm -rf "$W"' EXIT
pass=0; fail=0
ok()  { pass=$((pass + 1)); echo "PASS  $*"; }
bad() { fail=$((fail + 1)); echo "FAIL  $*"; }
USERS="case.officer demo.guest riley.provider sam.nominee zhaolin.sun"
RE='^[A-Z][a-z]{3,6}-[A-Z][a-z]{3,6}-[0-9]{4}-[!@#*?=_.]$'

mkstubs() { # dir od-mode(real|fail|garbage)
  mkdir -p "$1"
  cat > "$1/aws" <<'EOF'
#!/usr/bin/env bash
# Every secret has a value, except demo/keycloak/users unless STUB_USERS_HAS_VALUE=1.
# Each call's argv is logged; put-secret-value keeps a copy of its file:// payload.
printf '%s\n' "$*" >> "$STUB_LOG.argv"
case "$*" in
  *get-secret-value*demo/keycloak/users*) [ "${STUB_USERS_HAS_VALUE:-}" = 1 ] ;;
  *put-secret-value*) for a in "$@"; do case $a in file://*) cp "${a#file://}" "$STUB_LOG.uploaded" ;; esac; done ;;
  *) exit 0 ;;
esac
EOF
  printf '#!/usr/bin/env bash\ncase "$*" in "secret list"*) printf "GITOPS_DEPLOY_KEY\\t2026-10-16\\n" ;; esac\n' > "$1/gh"
  case $2 in
    fail) printf '#!/bin/sh\nexit 1\n' > "$1/od" ;;
    garbage) printf '#!/bin/sh\necho "  12x4"\n' > "$1/od" ;;
  esac
  chmod +x "$1"/*
}
seed() { # name od-mode [env...] -> exit code of the real script; logs in $W/<name>.*
  local name=$1 mode=$2; shift 2
  mkstubs "$W/$name-bin" "$mode"; : > "$W/$name.argv"
  env "$@" STUB_LOG="$W/$name" PATH="$W/$name-bin:$PATH" bash "$SEED" > "$W/$name.out" 2>&1
}
uploads() { grep -c 'put-secret-value' "$W/$1.argv"; }

seed good real; rc=$?
[ "$rc" -eq 0 ] && ok "seeds with a working random source" || bad "exit $rc: $(tail -2 "$W/good.out")"
[ "$(uploads good)" = 1 ] && ok "exactly one upload" || bad "$(uploads good) uploads"
grep 'put-secret-value' "$W/good.argv" | grep -q -- "--secret-id demo/keycloak/users --secret-string file://" \
  && ok "uploaded to demo/keycloak/users from a file" || bad "upload not from a file: $(grep put-secret-value "$W/good.argv")"
if [ -s "$W/good.uploaded" ]; then
  [ "$(jq -r 'keys | join(" ")' "$W/good.uploaded")" = "$USERS" ] && ok "the five users, no others" || bad "keys: $(jq -c keys "$W/good.uploaded")"
  [ "$(jq '[.[]] | unique | length' "$W/good.uploaded")" = 5 ] && ok "five distinct passwords" || bad "passwords repeat"
  jq -e --arg re "$RE" 'all(.[]; test($re))' "$W/good.uploaded" >/dev/null && ok "every password is Word-Word-NNNN-symbol" || bad "a password has the wrong format"
  jq -r '.[]' "$W/good.uploaded" > "$W/pw"
  [ "$(grep -c -F -f "$W/pw" "$W/good.uploaded")" -ge 1 ] && ok "positive control: the grep finds the passwords where they are" || bad "positive control"
  [ "$(cat "$W/good.argv" "$W/good.out" | grep -c -F -f "$W/pw")" = 0 ] && ok "no password on any argv or in the output" || bad "a password leaked to argv or output"
else bad "nothing uploaded"; fi

seed keep real STUB_USERS_HAS_VALUE=1; rc=$?
[ "$rc" -eq 0 ] && [ "$(uploads keep)" = 0 ] && ok "an existing value is kept" || bad "existing value: exit $rc, $(uploads keep) uploads"
seed rotate real STUB_USERS_HAS_VALUE=1 ROTATE_DEMO_PASSWORDS=1; rc=$?
[ "$rc" -eq 0 ] && [ "$(uploads rotate)" = 1 ] && ok "ROTATE_DEMO_PASSWORDS=1 replaces it" || bad "rotate: exit $rc, $(uploads rotate) uploads"

for mode in fail garbage; do
  seed "$mode" "$mode"; rc=$?
  [ "$rc" -ne 0 ] && ok "od $mode: exits non-zero" || bad "od $mode: exit 0"
  [ "$(uploads "$mode")" = 0 ] && ok "od $mode: nothing uploaded" || bad "od $mode: uploaded $(jq -c 'to_entries | map(.value | length)' "$W/$mode.uploaded" 2>/dev/null)"
done

echo "secrets-seed test passed=$pass failed=$fail"
[ "$fail" -eq 0 ]
