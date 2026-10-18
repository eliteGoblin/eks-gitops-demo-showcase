#!/usr/bin/env bash
# TC-B: riley.provider submits a fresh claim, case.officer approves it -> PAID with a pay_ ref;
# a replayed approval is refused; a re-read shows the same payment. Prints CLAIM_ID/REF so
# run-in-cluster.sh can count payments-svc's "payment recorded" lines for it (must be exactly 1).
set -u
. "$(dirname "$0")/lib.sh"
AMOUNT=${AMOUNT:-7.31}
RILEY=$(token riley.provider); CASE=$(token case.officer)
need_tokens riley.provider="$RILEY" case.officer="$CASE"
r=$(gql "$RILEY" "mutation { submitClaim(input:{planId:\"PL1\",category:DAILY_LIVING,amount:\"$AMOUNT\",serviceDate:\"2026-05-01\"}) { __typename ... on Claim { id status amount } ... on BudgetExceededError { remaining requested } } }")
check "riley submits a claim of $AMOUNT -> SUBMITTED" "$r" ".data.submitClaim.__typename==\"Claim\" and .data.submitClaim.status==\"SUBMITTED\" and .data.submitClaim.amount==\"$AMOUNT\""
ID=$(printf '%s' "$r" | jq -r '.data.submitClaim.id // empty')
[ -n "$ID" ] || { summary "TC-B"; exit 1; }
r=$(gql "$CASE" "mutation { approveClaim(id:\"$ID\", reason:\"e2e TC-B approve\") { __typename ... on Claim { id status payment { status ref } } ... on ValidationError { message } } }")
check "case.officer approves -> PAID" "$r" '.data.approveClaim.status=="PAID" and .data.approveClaim.payment.status=="PAID"'
REF=$(printf '%s' "$r" | jq -r '.data.approveClaim.payment.ref // empty')
expect "payment ref is the processor's id" "ref='$REF'" is_payment_ref "$REF"
if in_cluster; then # the payment exists in payments-svc for this claim and amount (GET, not log grepping)
  ST=$(svc_token)
  if is_jwt_of_type "$ST" Bearer; then
    CENTS=$(jq -n --arg a "$AMOUNT" '($a|tonumber)*100|round')
    p=$(curl -s -H "Authorization: Bearer $ST" "$PAYMENTS_SVC_URL/payments/$REF")
    check "payments-svc GET /payments/$REF -> claim $ID, $AMOUNT to PR1" "$p" \
      ".id==\"$REF\" and .reference==\"$ID\" and .amountCents==$CENTS and .payee==\"PR1\""
  else
    bad "payments-svc GET /payments/<ref>" "no client-credentials token"
  fi
else
  skip "payments-svc GET /payments/<ref>" "in-cluster only (PAYMENTS_SVC_URL, PAYMENTS_CLIENT_SECRET)"
fi
r=$(gql "$CASE" "mutation { approveClaim(id:\"$ID\", reason:\"e2e TC-B replay\") { __typename ... on ValidationError { message } } }")
check "replayed approval refused (no longer SUBMITTED)" "$r" '.data.approveClaim.__typename=="ValidationError" and (.data.approveClaim.message|test("SUBMITTED"))'
r=$(find_claim "$CASE" "$ID")
check "re-read: still PAID with the same ref" "$r" ".status==\"PAID\" and .payment.ref==\"$REF\""
echo "CLAIM_ID=$ID"
echo "REF=$REF"
summary "TC-B"
