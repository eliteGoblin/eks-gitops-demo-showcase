#!/usr/bin/env bash
# TC-F: a subscription held on pod A (its pod IP, bypassing the Service) receives a change made
# through pod B (its pod IP), and the reverse. Proves the cross-replica fan-out (Postgres
# LISTEN/NOTIFY), not just "a subscription works" (which can land on one pod via the Service).
# Usage: TC-F-cross-pod-subscription.sh <podA-name> <podA-ip> <podB-name> <podB-ip>
set -u
. "$(dirname "$0")/lib.sh"
A_NAME=${1:-}; A=${2:-}; B_NAME=${3:-}; B=${4:-}; PORT=${PORT:-8082}
if [ -z "$A" ] || [ -z "$B" ] || [ "$A_NAME" = "$B_NAME" ] || [ "$A" = "$B" ]; then
  bad "two different demo-api pods" "A=$A_NAME/$A B=$B_NAME/$B (need two Ready replicas)"; summary "TC-F"; exit 1
fi
ok "two different demo-api pods: A=$A_NAME ($A), B=$B_NAME ($B)"
Z=$(token zhaolin.sun); RILEY=$(token riley.provider); CASE=$(token case.officer)
need_tokens zhaolin.sun="$Z" riley.provider="$RILEY" case.officer="$CASE"
SUBQ='subscription { claimStatusChanged(planId:"PL1") { id status } }'
OUT=$(mktemp); trap 'rm -f "$OUT"' EXIT
sub() { # pod-ip: subscribe in the background ($SP), wait until registered
  python3 "$SCRIPTS/ws-subscribe.py" "ws://$1:$PORT/graphql" "$Z" "$SUBQ" 30 > "$OUT" 2>&1 & SP=$!
  for _ in $(seq 50); do grep -q '^ready' "$OUT" && { sleep 1; return 0; }; kill -0 "$SP" 2>/dev/null || return 1; sleep 0.2; done
  return 1
}
echo "$(ts) [1] subscribe on A, submit on B"
if sub "$A"; then
  r=$(gqlat "http://$B:$PORT" "$RILEY" 'mutation { submitClaim(input:{planId:"PL1",category:DAILY_LIVING,amount:"1.11",serviceDate:"2026-05-01"}) { __typename ... on Claim { id status } } }')
  ID=$(printf '%s' "$r" | jq -r '.data.submitClaim.id // empty')
  wait "$SP"
  check "subscriber on A received the claim submitted on B" "$(tail -1 "$OUT")" \
    ".data.claimStatusChanged.id==\"${ID:-none}\" and .data.claimStatusChanged.status==\"SUBMITTED\""
else
  bad "subscribe on A" "$(cat "$OUT")"; ID=""
fi
echo "$(ts) [2] subscribe on B, approve on A"
if [ -n "$ID" ] && sub "$B"; then
  gqlat "http://$A:$PORT" "$CASE" "mutation { approveClaim(id:\"$ID\", reason:\"e2e TC-F cross-pod\") { __typename } }" >/dev/null
  wait "$SP"
  check "subscriber on B received the approval made on A" "$(tail -1 "$OUT")" \
    ".data.claimStatusChanged.id==\"$ID\" and (.data.claimStatusChanged.status==\"APPROVED\" or .data.claimStatusChanged.status==\"PAID\")"
else
  bad "subscribe on B" "$( [ -n "$ID" ] && cat "$OUT" || echo 'no claim from step 1')"
fi
echo "CLAIM_ID=${ID:-}"
summary "TC-F"
