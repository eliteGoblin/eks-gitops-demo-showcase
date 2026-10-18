#!/usr/bin/env bash
# End-to-end checks against a running Keycloak (:8080) and API (:8082).
# Two ways to get tokens:
#   TOKEN_MODE=password (default, local only): the DEV-ONLY "verify-cli" password-grant client.
#   TOKEN_MODE=pkce: the real browser flow (Authorization Code + PKCE through Keycloak's login
#     page, scripted by pkce-login.py) with the public "spa" client. Use it against environments
#     that have no password-grant client, e.g. the public EKS demo:
#     TOKEN_MODE=pkce KC=https://auth.frankbluemind.com/realms/demo API=https://app.frankbluemind.com \
#       REALM_FILE=<users file> PAYMENTS=on scripts/verify.sh
#     where <users file> holds the passwords from Secrets Manager demo/keycloak/users in the realm
#     shape (scripts/e2e/run-in-cluster.sh writes one; the gitops realm.yaml has only placeholders).
# PAYMENTS=on expects approvals to be paid through payments-svc (PAYMENTS_ENABLED on the API).
# Live updates (subscriptions over WebSocket) are checked with ws-subscribe.py.
# In-cluster only (payments-svc is internal and its NetworkPolicy admits only demo-api pods):
#   PAYMENTS_SVC_URL=http://payments-svc.demo.svc:8083 PAYMENTS_CLIENT_SECRET=<demo-api-svc secret>
#   turns on the payments-svc checks (the payment exists; Idempotency-Key replays). Without them
#   those checks are reported as SKIP, never as PASS. scripts/e2e/run-in-cluster.sh sets both.
#
# Every check prints PASS, FAIL or SKIP; the predicates in e2e/asserts.sh only accept positive
# evidence (scripts/e2e/selftest.sh proves they reject the inputs that used to slip through).
set -u
HERE=$(cd "$(dirname "$0")" && pwd)
. "$HERE/e2e/asserts.sh"
REALM_FILE=${REALM_FILE:-$HERE/../deploy/keycloak/realm.json}
KC=${KC:-http://localhost:8080/realms/demo}
API=${API:-http://localhost:8082}
TOKEN_MODE=${TOKEN_MODE:-password}
REDIRECT=${REDIRECT:-$API/}
PAYMENTS=${PAYMENTS:-off}
PAYMENTS_SVC_URL=${PAYMENTS_SVC_URL:-}
PAYMENTS_CLIENT_ID=${PAYMENTS_CLIENT_ID:-demo-api-svc}
PAYMENTS_CLIENT_SECRET=${PAYMENTS_CLIENT_SECRET:-}
WS_URL="$(printf '%s' "$API" | sed 's#^http#ws#')/graphql"
TMPD=$(mktemp -d); SUB_OUT=$TMPD/sub; trap 'rm -rf "$TMPD"' EXIT
pass=0; fail=0; skipped=0

ok()   { pass=$((pass+1)); printf 'PASS  %s\n' "$1"; }
bad()  { fail=$((fail+1)); printf 'FAIL  %s\n      %s\n' "$1" "$2"; }
skip() { skipped=$((skipped+1)); printf 'SKIP  %s\n      %s\n' "$1" "$2"; }
check() { # name, actual, jq-expression that must be true
  local name=$1 actual=$2 expr=$3
  if printf '%s' "$actual" | jq -e "$expr" >/dev/null 2>&1; then ok "$name"; else bad "$name" "$(printf '%s' "$actual" | head -c 400)"; fi
}
expect() { # name, detail-on-failure, predicate command...
  local name=$1 detail=$2; shift 2
  if "$@"; then ok "$name"; else bad "$name" "$detail"; fi
}
in_cluster() { [ -n "$PAYMENTS_SVC_URL" ] && [ -n "$PAYMENTS_CLIENT_SECRET" ]; }
svc_token() { # client-credentials token of the service that pays (demo-api-svc)
  curl -s -X POST "$KC/protocol/openid-connect/token" -d grant_type=client_credentials \
    -d "client_id=$PAYMENTS_CLIENT_ID" --data-urlencode "client_secret=$PAYMENTS_CLIENT_SECRET" | jq -r '.access_token // empty'
}
reserved() { # token plan category -> the category's reserved amount
  gql "$1" "{ plan(id:\"$2\") { budgets { category reserved } } }" | jq -r --arg c "$3" '.data.plan.budgets[]? | select(.category==$c) | .reserved'
}
# Demo passwords come from REALM_FILE: the dev realm locally, a file built from Secrets Manager for EKS.
realm() { case "$REALM_FILE" in *.yaml|*.yml) yq -o json '.' "$REALM_FILE" ;; *) cat "$REALM_FILE" ;; esac; }
pw() { realm | jq -r --arg u "$1" '.users[] | select(.username==$u) | .credentials[0].value'; }
tokens() { # user -> token response JSON
  if [ "$TOKEN_MODE" = pkce ]; then
    pw "$1" | python3 "$HERE/pkce-login.py" login "$KC" spa "$REDIRECT" "$1"
  else
    curl -sf -X POST "$KC/protocol/openid-connect/token" -d grant_type=password -d client_id=verify-cli -d "username=$1" --data-urlencode "password=$(pw "$1")" -d scope=openid
  fi
}
token() { tokens "$1" | jq -r .access_token; }
payload() { printf '%s' "$1" | cut -d. -f2 | tr '_-' '/+' | { read -r s; printf '%s' "$s"; printf '%*s' $(( (4 - ${#s} % 4) % 4 )) '' | tr ' ' '='; } | base64 -d 2>/dev/null; }
gql() { # token, query -> body
  curl -s -X POST "$API/graphql" -H 'Content-Type: application/json' -H "Authorization: Bearer $1" --data "$(jq -cn --arg q "$2" '{query:$q}')"
}
status() { curl -s -o /dev/null -w '%{http_code}' -X POST "$API/graphql" -H 'Content-Type: application/json' ${@+"$@"} --data '{"query":"{ me { subject } }"}'; }
ws() { python3 "$HERE/ws-subscribe.py" "$WS_URL" "$@"; } # token, query, timeout -> "ready", then the first update
subscribe() { # token, query: subscribe in the background ($SUB_PID, output in $SUB_OUT) and wait until subscribed
  ws "$1" "$2" 30 > "$SUB_OUT" 2>&1 & SUB_PID=$!
  for _ in $(seq 50); do
    if grep -q '^ready' "$SUB_OUT"; then sleep 1; return 0; fi # the server registers it right after
    if ! kill -0 "$SUB_PID" 2>/dev/null; then return 1; fi
    sleep 0.2
  done
  return 1
}

echo "== tokens ($TOKEN_MODE)"
for u in zhaolin.sun sam.nominee riley.provider case.officer demo.guest; do
  t=$(token "$u"); v=${u%%.*}; declare "TOK_$v=$t"
  [ -n "$t" ] && [ "$t" != null ] && ok "token for $u" || bad "token for $u" "empty token"
done
check "zhaolin token aud contains demo-api" "$(payload "$TOK_zhaolin")" '(.aud | if type=="array" then . else [.] end) | index("demo-api") != null'
check "zhaolin token typ is Bearer and has participant_id=P1" "$(payload "$TOK_zhaolin")" '.typ=="Bearer" and .participant_id=="P1"'

echo "== authentication"
[ "$(status)" = 401 ] && ok "no token -> 401" || bad "no token -> 401" "got $(status)"
ZT=$(tokens zhaolin.sun); IDT=$(printf '%s' "$ZT" | jq -r '.id_token // empty')
# A 401 only means something if what was sent really is an ID token (a missing token is 401 too).
if is_jwt_of_type "$IDT" ID; then
  [ "$(status -H "Authorization: Bearer $IDT")" = 401 ] && ok "ID token (typ=ID, aud=client) -> 401" || bad "ID token -> 401" "got $(status -H "Authorization: Bearer $IDT")"
else
  bad "ID token (typ=ID, aud=client) -> 401" "no ID token to send: '${IDT:0:20}'"
fi
if [ "$TOKEN_MODE" = pkce ]; then # refresh-token rotation: each refresh returns a new token; the old one dies
  # Keycloak spots a stale refresh token by its issue time (1 s resolution), so space the calls
  # like a real client (which refreshes minutes apart).
  RT1=$(printf '%s' "$ZT" | jq -r .refresh_token)
  sleep 2; R2=$(python3 "$HERE/pkce-login.py" refresh "$KC" spa "$RT1" 2>/dev/null); RT2=$(printf '%s' "$R2" | jq -r .refresh_token)
  sleep 2; R3=$(python3 "$HERE/pkce-login.py" refresh "$KC" spa "$RT2" 2>/dev/null); RT3=$(printf '%s' "$R3" | jq -r .refresh_token)
  [ -n "$RT2" ] && [ "$RT2" != null ] && [ "$RT2" != "$RT1" ] && [ -n "$RT3" ] && [ "$RT3" != null ] \
    && ok "refresh twice: each refresh returns a new refresh token" || bad "refresh rotation" "rt2=${RT2:0:12} rt3=${RT3:0:12}"
  [ "$(status -H "Authorization: Bearer $(printf '%s' "$R3" | jq -r .access_token)")" = 200 ] && ok "refreshed access token accepted" || bad "refreshed access token" "rejected"
  # Rejected means Keycloak answered 400 invalid_grant; "the call failed" (e.g. host unreachable) is not evidence.
  RS=$(python3 "$HERE/pkce-login.py" refresh-status "$KC" spa "$RT1" 2>/dev/null)
  expect "reused (first) refresh token rejected: 400 invalid_grant" "got: ${RS:-no answer from $KC}" is_refresh_rejected "$RS"
fi
[ "$(status -H 'Authorization: Bearer garbage.token.value')" = 401 ] && ok "garbage token -> 401" || bad "garbage token -> 401" "got $(status -H 'Authorization: Bearer garbage.token.value')"

echo "== zhaolin.sun (participant P1)"
r=$(gql "$TOK_zhaolin" '{ me { roles participant { id } } }')
check "zhaolin me: roles [PARTICIPANT], participant P1" "$r" '.data.me.roles==["PARTICIPANT"] and .data.me.participant.id=="P1"'
r=$(gql "$TOK_zhaolin" '{ participant(id:"P1") { dateOfBirth plans { id budgets { category } } } }')
check "zhaolin participant(P1) has dateOfBirth and 2 budgets" "$r" '.data.participant.dateOfBirth=="1990-05-04" and (.data.participant.plans[0].budgets|length)==2'
r=$(gql "$TOK_zhaolin" '{ participant(id:"P2") { id } }')
expect "zhaolin participant(P2) is null" "$(printf '%s' "$r" | head -c 400)" is_hidden_participant "$r"
r=$(gql "$TOK_zhaolin" 'mutation { submitClaim(input:{planId:"PL1",category:DAILY_LIVING,amount:"10.00",serviceDate:"2026-05-01"}) { __typename } }')
check "zhaolin submitClaim -> FORBIDDEN" "$r" '.errors[0].extensions.code=="FORBIDDEN"'

echo "== riley.provider (provider PR1, booked PL1/DAILY_LIVING)"
r=$(gql "$TOK_riley" '{ participant(id:"P1") { givenName schemeNumber dateOfBirth plans { id budgets { category remaining } } } }')
check "riley participant(P1): dateOfBirth null, name visible" "$r" '.data.participant.givenName=="Zhaolin" and .data.participant.dateOfBirth==null'
check "riley participant(P1): budgets only DAILY_LIVING" "$r" '[.data.participant.plans[0].budgets[].category]==["DAILY_LIVING"]'
r=$(gql "$TOK_riley" '{ participant(id:"P2") { id } }')
expect "riley participant(P2) is null" "$(printf '%s' "$r" | head -c 400)" is_hidden_participant "$r"
r=$(gql "$TOK_riley" '{ claims { totalCount edges { node { id plan { id participant { id dateOfBirth } } } } } }')
check "riley nested claims->plan->participant: only PL1, dateOfBirth null" "$r" '(.data.claims.totalCount>=2) and ([.data.claims.edges[].node.plan.id]|unique==["PL1"]) and ([.data.claims.edges[].node.plan.participant.dateOfBirth]|unique==[null])'
r=$(gql "$TOK_riley" 'mutation { submitClaim(input:{planId:"PL1",category:DAILY_LIVING,amount:"999999.00",serviceDate:"2026-05-01"}) { __typename ... on BudgetExceededError { remaining requested } } }')
check "riley submitClaim over budget -> BudgetExceededError" "$r" '.data.submitClaim.__typename=="BudgetExceededError"'
r=$(gql "$TOK_riley" 'mutation { submitClaim(input:{planId:"PL1",category:THERAPY,amount:"10.00",serviceDate:"2026-05-01"}) { __typename } }')
check "riley submitClaim on unbooked category -> FORBIDDEN" "$r" '.errors[0].extensions.code=="FORBIDDEN"'
subscribe "$TOK_zhaolin" 'subscription { claimStatusChanged(planId:"PL1") { id status } }' \
  && ok "zhaolin subscribes to claims on PL1 (WebSocket)" || bad "zhaolin subscribes to claims on PL1" "$(cat "$SUB_OUT")"
r=$(gql "$TOK_riley" 'mutation { submitClaim(input:{planId:"PL1",category:DAILY_LIVING,amount:"120.50",serviceDate:"2026-05-01"}) { __typename ... on Claim { id status amount } } }')
check "riley submitClaim valid -> Claim SUBMITTED" "$r" '.data.submitClaim.__typename=="Claim" and .data.submitClaim.status=="SUBMITTED" and .data.submitClaim.amount=="120.50"'
NEW_CLAIM=$(printf '%s' "$r" | jq -r '.data.submitClaim.id // empty')
wait "$SUB_PID"
check "zhaolin's subscription receives riley's new claim" "$(tail -1 "$SUB_OUT")" ".data.claimStatusChanged.id==\"$NEW_CLAIM\" and .data.claimStatusChanged.status==\"SUBMITTED\""
r=$(gql "$TOK_riley" "mutation { approveClaim(id:\"$NEW_CLAIM\", reason:\"x\") { __typename } }")
check "riley approveClaim -> FORBIDDEN" "$r" '.errors[0].extensions.code=="FORBIDDEN"'

echo "== case.officer (agency_staff)"
r=$(gql "$TOK_case" "mutation { approveClaim(id:\"${NEW_CLAIM:-C2}\", reason:\"verified invoice\") { __typename ... on Claim { id status payment { status ref } } } }")
if [ "$PAYMENTS" = on ]; then
  check "case.officer approveClaim -> Claim PAID, payment PAID" "$r" '.data.approveClaim.status=="PAID" and .data.approveClaim.payment.status=="PAID"'
  REF=$(printf '%s' "$r" | jq -r '.data.approveClaim.payment.ref // empty')
  expect "payment ref is the processor's id (pay_…)" "ref='$REF'" is_payment_ref "$REF"
  if in_cluster; then # the ref must resolve in payments-svc to this claim and amount
    SVC_TOKEN=$(svc_token)
    if is_jwt_of_type "$SVC_TOKEN" Bearer; then
      p=$(curl -s -H "Authorization: Bearer $SVC_TOKEN" "$PAYMENTS_SVC_URL/payments/$REF")
      check "payments-svc has payment $REF for claim ${NEW_CLAIM:-?} (120.50 to PR1)" "$p" \
        ".id==\"$REF\" and .reference==\"$NEW_CLAIM\" and .amountCents==12050 and .payee==\"PR1\" and .currency==\"AUD\""
    else
      bad "payments-svc has the payment for the claim" "no client-credentials token for $PAYMENTS_CLIENT_ID"
    fi
  else
    skip "payments-svc has the payment for the claim" "in-cluster only (PAYMENTS_SVC_URL, PAYMENTS_CLIENT_SECRET)"
  fi
  r=$(gql "$TOK_case" "mutation { approveClaim(id:\"${NEW_CLAIM:-C2}\", reason:\"replayed\") { __typename ... on ValidationError { message } } }")
  check "replayed approveClaim refused (claim no longer SUBMITTED)" "$r" '.data.approveClaim.__typename=="ValidationError" and (.data.approveClaim.message|test("SUBMITTED"))'
else
  check "case.officer approveClaim -> Claim APPROVED, no payment (payments off)" "$r" '.data.approveClaim.__typename=="Claim" and .data.approveClaim.status=="APPROVED" and .data.approveClaim.payment==null'
fi
r=$(gql "$TOK_case" '{ participant(id:"P2") { id dateOfBirth } }')
check "case.officer participant(P2) visible with dateOfBirth" "$r" '.data.participant.id=="P2" and .data.participant.dateOfBirth!=null'

echo "== sam.nominee (nominee for P1)"
r=$(gql "$TOK_sam" '{ participant(id:"P1") { id dateOfBirth plans { id budgets { category } } } }')
check "sam participant(P1) visible with dateOfBirth and both budgets" "$r" '.data.participant.id=="P1" and .data.participant.dateOfBirth=="1990-05-04" and (.data.participant.plans[0].budgets|length)==2'
r=$(gql "$TOK_sam" '{ participant(id:"P2") { id } }')
expect "sam participant(P2) is null" "$(printf '%s' "$r" | head -c 400)" is_hidden_participant "$r"

echo "== demo.guest (participant P3)"
r=$(gql "$TOK_demo" '{ me { participant { id } } participant(id:"P1") { id } }')
check "demo.guest sees own participant P3, not P1" "$r" '.data.me.participant.id=="P3" and .data.participant==null'

echo "== support requests (zhaolin.sun raises, case.officer decides, nobody decides their own)"
RAISE='mutation { raiseSupportRequest(input:{planId:"%s",category:THERAPY,description:"Weekly physiotherapy",estimatedAmount:"%s",preferredProviderId:"PR2"}) { __typename ... on SupportRequest { id status requestedBy } ... on ValidationError { field } } }'
RESERVED_BEFORE=$(reserved "$TOK_zhaolin" PL1 THERAPY) # reservations accumulate across runs: compare, don't threshold
r=$(gql "$TOK_zhaolin" "$(printf "$RAISE" PL1 50.00)")
check "zhaolin raiseSupportRequest on own plan -> PENDING" "$r" '.data.raiseSupportRequest.__typename=="SupportRequest" and .data.raiseSupportRequest.status=="PENDING" and .data.raiseSupportRequest.requestedBy=="zhaolin.sun"'
SR=$(printf '%s' "$r" | jq -r '.data.raiseSupportRequest.id // empty')
r=$(gql "$TOK_demo" "$(printf "$RAISE" PL1 1.00)")
check "demo.guest raiseSupportRequest on someone else's plan -> FORBIDDEN" "$r" '.errors[0].extensions.code=="FORBIDDEN"'
DECIDE='mutation { decideSupportRequest(input:{id:"%s",decision:%s,reason:"%s"}) { __typename ... on SupportRequest { status bookedProvider { id } } ... on SelfApprovalNotAllowedError { message } ... on BudgetExceededError { remaining } } }'
r=$(gql "$TOK_zhaolin" "$(printf "$DECIDE" "$SR" APPROVE "self service")")
check "zhaolin decideSupportRequest -> FORBIDDEN (staff only)" "$r" '.errors[0].extensions.code=="FORBIDDEN"'
r=$(gql "$TOK_case" '{ pendingSupportRequests { id } }')
check "case.officer sees it in the pending queue" "$r" "[.data.pendingSupportRequests[].id] | index(\"$SR\") != null"
r=$(gql "$TOK_case" "$(printf "$DECIDE" "$SR" APPROVE "assessed by OT")")
check "case.officer approves -> APPROVED, booked preferred provider PR2" "$r" '.data.decideSupportRequest.status=="APPROVED" and .data.decideSupportRequest.bookedProvider.id=="PR2"'
r=$(gql "$TOK_zhaolin" '{ mySupportRequests { id status } }')
check "zhaolin mySupportRequests shows the decision" "$r" "[.data.mySupportRequests[] | select(.id==\"$SR\") | .status] == [\"APPROVED\"]"
r=$(gql "$TOK_case" "$(printf "$RAISE" PL3 10.00)")
OWN=$(printf '%s' "$r" | jq -r '.data.raiseSupportRequest.id // empty')
r=$(gql "$TOK_case" "$(printf "$DECIDE" "$OWN" APPROVE "approving my own")")
check "case.officer deciding a request they raised -> SelfApprovalNotAllowedError" "$r" '.data.decideSupportRequest.__typename=="SelfApprovalNotAllowedError"'
RESERVED_AFTER=$(reserved "$TOK_zhaolin" PL1 THERAPY)
expect "approval reserved exactly 50.00 more THERAPY budget" "reserved before=${RESERVED_BEFORE:-?} after=${RESERVED_AFTER:-?}" \
  money_delta_is "$RESERVED_BEFORE" "$RESERVED_AFTER" "50.00"

echo "== live updates: authentication and authorization"
r=$(ws garbage.token.value 'subscription { claimStatusChanged(planId:"PL1") { id } }' 10)
[ "$r" = "closed 4401 Unauthorized" ] && ok "WebSocket with an invalid token -> close 4401" || bad "WebSocket with an invalid token -> close 4401" "$r"
r=$(ws "$TOK_demo" 'subscription { claimStatusChanged(planId:"PL1") { id } }' 10 | tail -1)
check "demo.guest subscribing to P1's plan -> FORBIDDEN" "$r" '.errors[0].extensions.code=="FORBIDDEN"'
r=$(ws "$TOK_zhaolin" '{ me { subject } }' 10 | tail -1)
check "a query over the WebSocket -> BAD_USER_INPUT (POST only)" "$r" '.errors[0].extensions.code=="BAD_USER_INPUT"'

echo "== misc"
r=$(gql "$TOK_zhaolin" 'mutation { submitClaim(input:{planId:"PL1",category:DAILY_LIVING,amount:"abc",serviceDate:"2026-05-01"}) { __typename } }')
check "invalid Money -> BAD_USER_INPUT" "$r" '.errors[0].extensions.code=="BAD_USER_INPUT"'
r=$(gql "$TOK_zhaolin" '{ __schema { queryType { name } } }')
check "introspection on in dev" "$r" '.data.__schema.queryType.name=="Query"'
[ "$(curl -s -o /dev/null -w '%{http_code}' "$API/readyz")" = 200 ] && ok "readyz 200 (JWKS fetched)" || bad "readyz" "not ready"
curl -s "$API/playground" | grep -q 'graphiql' && ok "playground served in dev" || bad "playground" "not served"
[ "$(curl -s -o /dev/null -w '%{http_code}' -X POST "$API/graphql")" = 401 ] && curl -s -X POST "$API/graphql" | jq -e '.errors[0].extensions.code=="UNAUTHENTICATED"' >/dev/null && ok "401 body is the error envelope" || bad "401 envelope" "unexpected body"

echo "== payments-svc idempotency (in-cluster only: same key + body replays, same key + other body conflicts)"
if in_cluster; then
  SVC_TOKEN=${SVC_TOKEN:-$(svc_token)}
  KEY="e2e-$(date +%s)-$RANDOM"
  pay() { # header-file amount-cents -> body
    curl -s -D "$1" -X POST "$PAYMENTS_SVC_URL/payments" -H "Authorization: Bearer $SVC_TOKEN" \
      -H 'Content-Type: application/json' -H "Idempotency-Key: $KEY" \
      --data "{\"reference\":\"e2e-idempotency-$KEY\",\"payee\":\"e2e\",\"amountCents\":$2,\"currency\":\"AUD\"}"
  }
  b1=$(pay "$TMPD/h1" 1); id1=$(printf '%s' "$b1" | jq -r '.id // empty')
  { is_http_status "$TMPD/h1" 201 && is_payment_ref "$id1"; } \
    && ok "first POST /payments -> 201 Created, $id1" || bad "first POST /payments -> 201" "$(head -1 "$TMPD/h1" 2>/dev/null) $b1"
  b2=$(pay "$TMPD/h2" 1); id2=$(printf '%s' "$b2" | jq -r '.id // empty')
  { is_http_status "$TMPD/h2" 200 && has_header "$TMPD/h2" Idempotent-Replayed true && [ -n "$id1" ] && [ "$id2" = "$id1" ]; } \
    && ok "same key + same body -> 200 Idempotent-Replayed, same payment" || bad "same key + same body replays" "$(head -1 "$TMPD/h2" 2>/dev/null) id1=$id1 id2=$id2"
  b3=$(pay "$TMPD/h3" 2)
  { is_http_status "$TMPD/h3" 409 && printf '%s' "$b3" | jq -e '.error.code=="IDEMPOTENCY_KEY_REUSED"' >/dev/null 2>&1; } \
    && ok "same key + other body -> 409 IDEMPOTENCY_KEY_REUSED" || bad "same key + other body -> 409" "$(head -1 "$TMPD/h3" 2>/dev/null) $b3"
else
  skip "payments-svc Idempotency-Key replay and conflict" "in-cluster only (PAYMENTS_SVC_URL, PAYMENTS_CLIENT_SECRET)"
fi

echo
echo "passed=$pass failed=$fail skipped=$skipped"
[ "$fail" -eq 0 ]
