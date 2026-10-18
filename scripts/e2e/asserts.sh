# Assertion predicates shared by scripts/verify.sh and the scenario scripts.
#
# Each predicate returns 0 only on positive evidence of the claim. Missing,
# empty or malformed input (an unreachable host, a null token, an error
# envelope, an empty object) must return non-zero, so a broken run can never
# be recorded as a pass. scripts/e2e/selftest.sh proves that for every
# predicate against the inputs an independent audit showed slipping through.

# jwt_payload TOKEN -> the decoded JSON payload of a JWT (non-zero if TOKEN is not a JWT).
jwt_payload() {
  local t=${1:-} s
  case "$t" in *.*.*) ;; *) return 1 ;; esac
  s=$(printf '%s' "$t" | cut -d. -f2 | tr '_-' '/+')
  s="$s$(printf '%*s' $(( (4 - ${#s} % 4) % 4 )) '' | tr ' ' '=')"
  printf '%s' "$s" | base64 -d 2>/dev/null | jq -ce 'type=="object"' >/dev/null 2>&1 || return 1
  printf '%s' "$s" | base64 -d 2>/dev/null
}

# is_jwt_of_type TOKEN TYP: a well-formed JWT whose typ claim is TYP (Bearer, ID, Refresh).
is_jwt_of_type() {
  jwt_payload "${1:-}" | jq -e --arg t "$2" '.typ==$t' >/dev/null 2>&1
}

# is_hidden_participant BODY: a well-formed GraphQL answer {"data":{"participant":null}}
# with no errors. An error envelope (e.g. UNAUTHENTICATED), {} or no body is not evidence.
is_hidden_participant() {
  printf '%s' "${1:-}" | jq -e '
    type=="object" and (has("errors")|not)
    and (.data|type)=="object" and (.data|has("participant")) and .data.participant==null' >/dev/null 2>&1
}

# is_refresh_rejected STATUS_JSON: output of `pkce-login.py refresh-status`; Keycloak
# answered 400 invalid_grant. No output (host unreachable) or any other answer is not.
is_refresh_rejected() {
  printf '%s' "${1:-}" | jq -e '.http_status==400 and .body.error=="invalid_grant"' >/dev/null 2>&1
}

# is_payment_ref REF: the processor's payment id format.
is_payment_ref() {
  [[ "${1:-}" =~ ^pay_[A-Z0-9]+$ ]]
}

# money_delta_is BEFORE AFTER EXPECTED: AFTER - BEFORE == EXPECTED, in cents (decimal strings).
# Empty or non-numeric input is not evidence.
money_delta_is() {
  jq -ne --arg b "${1:-}" --arg a "${2:-}" --arg e "${3:-}" '
    ($b|test("^-?[0-9]+(\\.[0-9]{1,2})?$")) and ($a|test("^-?[0-9]+(\\.[0-9]{1,2})?$"))
    and ((($a|tonumber)*100|round) - (($b|tonumber)*100|round)) == (($e|tonumber)*100|round)' >/dev/null 2>&1
}

# is_http_status FILE CODE: a curl `-D FILE` header dump whose final status line is CODE.
is_http_status() {
  [ -s "${1:-}" ] && [ "$(grep -E '^HTTP/' "$1" | tail -1 | awk '{print $2}')" = "$2" ]
}

# has_header FILE NAME VALUE: the header dump carries NAME: VALUE (case-insensitive name).
has_header() {
  [ -s "${1:-}" ] && grep -iqE "^$2:[[:space:]]*$3[[:space:]]*$" <(tr -d '\r' < "$1")
}
