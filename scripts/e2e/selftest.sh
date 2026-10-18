#!/usr/bin/env bash
# Self-test of the end-to-end assertions (no cluster needed; runs in CI).
#
# The rule: an assertion that can pass on bad evidence is worse than no assertion.
# Every predicate in asserts.sh is run against the broken inputs an independent
# audit showed slipping through the old checks (unreachable host, missing token,
# error envelope, empty object, state accumulated by earlier runs, any non-empty
# ref) and must FAIL on each, then against good evidence and must PASS.
set -u
HERE=$(cd "$(dirname "$0")" && pwd)
. "$HERE/asserts.sh"
pass=0; fail=0
must_fail() { if "${@:2}"; then fail=$((fail+1)); printf 'FAIL  %s (a predicate accepted bad evidence)\n' "$1"; else pass=$((pass+1)); printf 'PASS  rejects: %s\n' "$1"; fi; }
must_pass() { if "${@:2}"; then pass=$((pass+1)); printf 'PASS  accepts: %s\n' "$1"; else fail=$((fail+1)); printf 'FAIL  %s (a predicate rejected good evidence)\n' "$1"; fi; }
b64url() { printf '%s' "$1" | base64 | tr -d '=\n' | tr '/+' '_-'; }
jwt() { printf '%s.%s.sig' "$(b64url '{"alg":"RS256","typ":"JWT"}')" "$(b64url "$1")"; }

echo "== reused refresh token rejected (Keycloak must answer 400 invalid_grant)"
UNREACHABLE=$(python3 "$HERE/../pkce-login.py" refresh-status "https://auth.invalid/realms/demo" spa "not-a-token" 2>/dev/null)
must_fail "unreachable host (no answer at all; exit $?)" is_refresh_rejected "$UNREACHABLE"
must_fail "an unrelated 401" is_refresh_rejected '{"http_status":401,"body":{"error":"unauthorized_client"}}'
must_fail "a 200 with fresh tokens (the reuse worked)" is_refresh_rejected '{"http_status":200,"body":{"access_token":"x"}}'
must_fail "a 400 for another reason" is_refresh_rejected '{"http_status":400,"body":{"error":"invalid_request"}}'
must_pass "400 invalid_grant" is_refresh_rejected '{"http_status":400,"body":{"error":"invalid_grant","error_description":"Stale token"}}'

echo "== ID token must be an ID token before its 401 means anything"
must_fail "missing token (jq printed null)" is_jwt_of_type "null" ID
must_fail "empty token" is_jwt_of_type "" ID
must_fail "not a JWT" is_jwt_of_type "garbage" ID
must_fail "an access token" is_jwt_of_type "$(jwt '{"typ":"Bearer","aud":"demo-api"}')" ID
must_pass "a JWT with typ ID" is_jwt_of_type "$(jwt '{"typ":"ID","aud":"spa"}')" ID

echo "== a participant the caller may not see answers data.participant = null, without errors"
must_fail "UNAUTHENTICATED envelope" is_hidden_participant '{"errors":[{"extensions":{"code":"UNAUTHENTICATED"}}],"data":null}'
must_fail "empty object {}" is_hidden_participant '{}'
must_fail "no body" is_hidden_participant ''
must_fail "data without the field" is_hidden_participant '{"data":{}}'
must_fail "null plus an error" is_hidden_participant '{"data":{"participant":null},"errors":[{"message":"x"}]}'
must_fail "the participant itself" is_hidden_participant '{"data":{"participant":{"id":"P2"}}}'
must_pass "data.participant null" is_hidden_participant '{"data":{"participant":null}}'

echo "== reserved budget must grow by exactly the approved amount (state accumulates across runs)"
must_fail "already above 50 before anything was approved" money_delta_is "350.00" "350.00" "50.00"
must_fail "missing before" money_delta_is "" "400.00" "50.00"
must_fail "missing after" money_delta_is "350.00" "" "50.00"
must_fail "not money" money_delta_is "abc" "400.00" "50.00"
must_fail "grew by a different amount" money_delta_is "350.00" "420.00" "50.00"
must_pass "grew by exactly 50.00" money_delta_is "350.00" "400.00" "50.00"
must_pass "grew by exactly 50.00 from zero" money_delta_is "0" "50" "50.00"

echo "== a payment ref is the processor's id"
must_fail "any non-empty string" is_payment_ref "x"
must_fail "null" is_payment_ref "null"
must_fail "empty" is_payment_ref ""
must_fail "prefix only" is_payment_ref "pay_"
must_fail "wrong case" is_payment_ref "PAY_ABC123"
must_pass "pay_ + base32" is_payment_ref "pay_FZAT5IQFWAB7TYNTZJ3BMHF4BC"

echo "== HTTP status and header evidence comes from a real response"
H=$(mktemp); trap 'rm -f "$H"' EXIT
must_fail "no response captured" is_http_status /nonexistent 201
: > "$H"; must_fail "empty header dump" is_http_status "$H" 201
printf 'HTTP/1.1 200 OK\r\nContent-Type: application/json\r\n\r\n' > "$H"
must_fail "200 when 201 is expected" is_http_status "$H" 201
must_fail "header absent" has_header "$H" Idempotent-Replayed true
printf 'HTTP/1.1 100 Continue\r\n\r\nHTTP/1.1 201 Created\r\nIdempotent-Replayed: false\r\n\r\n' > "$H"
must_pass "final status 201 after a 100 Continue" is_http_status "$H" 201
must_fail "header with another value" has_header "$H" Idempotent-Replayed true
printf 'HTTP/1.1 200 OK\r\nidempotent-replayed: true\r\n\r\n' > "$H"
must_pass "header present (case-insensitive name)" has_header "$H" Idempotent-Replayed true

echo
echo "selftest passed=$pass failed=$fail"
[ "$fail" -eq 0 ]
