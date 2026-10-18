# Shared helpers for the scenario scripts (TC-*.sh). Sourced, not run.
# Needs KC, API and REALM_FILE (JSON: users[].username, users[].credentials[0].value).
# Tokens and passwords are only ever held in variables; nothing secret is printed or written.
E2E_DIR=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
SCRIPTS=${SCRIPTS:-$E2E_DIR/..}
. "$E2E_DIR/asserts.sh"
KC=${KC:-https://auth.frankbluemind.com/realms/demo}
API=${API:-https://app.frankbluemind.com}
REALM_FILE=${REALM_FILE:?REALM_FILE (JSON with the realm users) is required}
pass=0; fail=0; skipped=0

ok()   { pass=$((pass+1)); printf 'PASS  %s\n' "$1"; }
bad()  { fail=$((fail+1)); printf 'FAIL  %s\n      %s\n' "$1" "$2"; }
skip() { skipped=$((skipped+1)); printf 'SKIP  %s\n      %s\n' "$1" "$2"; }
check() { # name actual jq-expression
  if printf '%s' "$2" | jq -e "$3" >/dev/null 2>&1; then ok "$1"; else bad "$1" "$(printf '%s' "$2" | head -c 400)"; fi
}
expect() { # name detail predicate...
  local name=$1 detail=$2; shift 2
  if "$@"; then ok "$name"; else bad "$name" "$detail"; fi
}
summary() { echo; echo "$1 passed=$pass failed=$fail skipped=$skipped"; [ "$fail" -eq 0 ]; }

pw()    { jq -r --arg u "$1" '.users[] | select(.username==$u) | .credentials[0].value' "$REALM_FILE"; }
token() { pw "$1" | python3 "$SCRIPTS/pkce-login.py" login "$KC" spa "$API/" "$1" | jq -r '.access_token // empty'; }
need_tokens() { # name=token... : fail fast unless each is a real access token
  local kv
  for kv in "$@"; do is_jwt_of_type "${kv#*=}" Bearer || { bad "access token for ${kv%%=*}" "login failed"; summary "${0##*/}"; exit 1; }; done
}
gqlat() { # base-url token query [operationName] -> body
  local body
  if [ -n "${4:-}" ]; then body=$(jq -cn --arg q "$3" --arg o "$4" '{query:$q, operationName:$o}'); else body=$(jq -cn --arg q "$3" '{query:$q}'); fi
  curl -s -X POST "$1/graphql" -H 'Content-Type: application/json' -H "Authorization: Bearer $2" --data "$body"
}
gql() { gqlat "$API" "$@"; }
# In-cluster only: payments-svc (internal; NetworkPolicy admits demo-api-labelled pods).
in_cluster() { [ -n "${PAYMENTS_SVC_URL:-}" ] && [ -n "${PAYMENTS_CLIENT_SECRET:-}" ]; }
svc_token() { # client-credentials token of the service that pays (demo-api-svc)
  curl -s -X POST "$KC/protocol/openid-connect/token" -d grant_type=client_credentials \
    -d "client_id=${PAYMENTS_CLIENT_ID:-demo-api-svc}" --data-urlencode "client_secret=$PAYMENTS_CLIENT_SECRET" | jq -r '.access_token // empty'
}
ts() { date -u +%Y-%m-%dT%H:%M:%SZ; }
find_claim() { # token claim-id -> the claim node (pages through every claim the caller can see)
  local after="" page q
  while :; do
    if [ -n "$after" ]; then q="{ claims(first:20, after:\"$after\") { pageInfo { hasNextPage endCursor } edges { node { id status amount plan { id } payment { status ref } } } } }"
    else q='{ claims(first:20) { pageInfo { hasNextPage endCursor } edges { node { id status amount plan { id } payment { status ref } } } } }'; fi
    page=$(gql "$1" "$q")
    printf '%s' "$page" | jq -c --arg id "$2" '.data.claims.edges[]?.node | select(.id==$id)'
    [ "$(printf '%s' "$page" | jq -r '.data.claims.pageInfo.hasNextPage // false')" = true ] || break
    after=$(printf '%s' "$page" | jq -r '.data.claims.pageInfo.endCursor')
  done
}
