#!/usr/bin/env bash
# Run the end-to-end suite from a throwaway pod inside the EKS cluster.
#
# Why in-cluster: payments-svc is internal (its NetworkPolicy admits only pods labelled
# app.kubernetes.io/name=demo-api, which this runner carries), and TC-F talks to pod IPs.
# It also sidesteps laptops whose network filter (e.g. Cloudflare WARP/Gateway) blocks the
# freshly registered demo domain; the runner reaches the public URLs from the cluster.
#
# What it does: create the pod; copy the scripts, the demo users' passwords (Secrets Manager) and
# the secrets the checks need (stdin, mode 600, never in the pod spec); run verify.sh and the
# TC-* scenarios; confirm two facts from the laptop that the pod cannot see (payments-svc logged
# exactly one payment for TC-B's claim; TC-F's subscriber pod served the WebSocket); print one
# summary; delete the pod and every temp file. Exit non-zero on any FAIL.
#
#   scripts/e2e/run-in-cluster.sh            # everything
#   E2E_METRICS=off scripts/e2e/run-in-cluster.sh   # skip TC-E (waits for Prometheus, ~2 min)
set -uo pipefail
HERE=$(cd "$(dirname "$0")" && pwd); SCRIPTS=$(cd "$HERE/.." && pwd)
KCFG=$HOME/.kube/eks-gitops-demo; CTX=eks-gitops-demo; NS=demo
kc() { kubectl --kubeconfig "$KCFG" --context "$CTX" -n "$NS" "$@"; }
IMAGE=${E2E_IMAGE:-python:3.13-alpine@sha256:2d9aefe2fef018a7eb2c13064c89c71929800fd2e5dccdbf52ea5da5bb8d929a}
KC_URL=${KC:-https://auth.frankbluemind.com/realms/demo}
API_URL=${API:-https://app.frankbluemind.com}
POD=e2e-runner-$(date +%s)
TMP=$(mktemp -d); OUT=$TMP/out; mkdir -p "$OUT"
cleanup() {
  # delete only the exact pod this run created (positive check of name and label first)
  if [ "$(kc get pod "$POD" -o jsonpath='{.metadata.labels.e2e}' 2>/dev/null)" = runner ]; then
    kc delete pod "$POD" --wait=false >/dev/null 2>&1
  fi
  rm -rf "$TMP"
}
trap cleanup EXIT
die() { echo "e2e: $*" >&2; exit 2; }
# Expired or missing credentials are not a test result: exit 3 (no verdict), never FAIL.
creds_ok() { aws sts get-caller-identity >/dev/null 2>&1 && kc auth can-i get pods >/dev/null 2>&1; }
inconclusive() { echo "e2e: INCONCLUSIVE: $* (not a test result; run 'aws login' and re-run)" >&2; exit 3; }
pods_of() { kc get pods -l "app.kubernetes.io/name=$1,app.kubernetes.io/instance=$1" -o jsonpath='{.items[*].metadata.name}' 2>/dev/null; }

[ -f "$KCFG" ] || die "missing $KCFG (make eks-kubeconfig)"
creds_ok || inconclusive "AWS or Kubernetes credentials expired or missing before the run; no test was run"
kc get deploy demo-api >/dev/null 2>&1 || die "no demo-api deployment in $CTX/$NS"
# Pods at the start: the laptop-side log checks only mean something if these are still the pods
# afterwards (a rollout takes payments-svc's in-memory store and the old pods' logs with it).
PAY_PODS_START=$(pods_of payments-svc); API_PODS_START=$(pods_of demo-api)

echo "== inputs (secrets stay in variables and mode-600 files)"
# The passwords live only in Secrets Manager (demo/keycloak/users: username -> password); verify.sh
# and lib.sh read them in the realm-file shape (users[].username, users[].credentials[0].value).
aws secretsmanager get-secret-value --region ap-southeast-2 --secret-id demo/keycloak/users --query SecretString --output text \
  | jq '{users: [to_entries[] | {username: .key, credentials: [{value: .value}]}]}' > "$TMP/realm.json" \
  || die "could not read the demo users' passwords (Secrets Manager demo/keycloak/users)"
[ "$(jq '.users | length' "$TMP/realm.json")" -ge 5 ] || die "realm users missing"
PAY_SECRET=$(kc get secret demo-api-payments -o jsonpath='{.data.clientSecret}' | base64 -d)
[ -n "$PAY_SECRET" ] || die "no demo-api-svc client secret (secret demo-api-payments)"
GRAFANA_JSON=""
if [ "${E2E_METRICS:-on}" != off ]; then
  GRAFANA_JSON=$(aws secretsmanager get-secret-value --region ap-southeast-2 --secret-id demo/grafana/admin --query SecretString --output text 2>/dev/null) \
    || echo "   (no Grafana admin secret: TC-E will SKIP)"
fi
{ printf 'export PAYMENTS_CLIENT_SECRET=%q\n' "$PAY_SECRET"
  [ -n "$GRAFANA_JSON" ] && printf 'export GRAFANA_USER=%q\nexport GRAFANA_PASSWORD=%q\n' \
    "$(jq -r .username <<<"$GRAFANA_JSON")" "$(jq -r .password <<<"$GRAFANA_JSON")"
} > "$TMP/secrets.env"; chmod 600 "$TMP/secrets.env" "$TMP/realm.json"

# Two different Ready demo-api replicas for TC-F (names and IPs from the API server).
# (No mapfile: macOS ships bash 3.2.)
READY=$(kc get pods -l app.kubernetes.io/name=demo-api,app.kubernetes.io/instance=demo-api -o json \
  | jq -r '.items[] | select(.status.phase=="Running") | select(any(.status.conditions[]; .type=="Ready" and .status=="True")) | "\(.metadata.name) \(.status.podIP)"')
NREADY=$(printf '%s\n' "$READY" | grep -c .)
read -r A_NAME A_IP <<<"$(printf '%s\n' "$READY" | sed -n 1p)"
read -r B_NAME B_IP <<<"$(printf '%s\n' "$READY" | sed -n 2p)"
echo "   demo-api replicas ready: $NREADY"

echo "== runner pod $POD ($IMAGE)"
# e2e=runner is also what the gitops repo's NetworkPolicies admit to demo-api (TC-F dials pod IPs) and to
# Grafana (TC-E); app.kubernetes.io/name=demo-api is what payments-svc's admits. Renaming either breaks those.
kc run "$POD" --image="$IMAGE" --restart=Never --labels="app.kubernetes.io/name=demo-api,e2e=runner" \
  --command -- sleep 1800 >/dev/null || die "could not create the runner pod"
kc wait --for=condition=Ready "pod/$POD" --timeout=180s >/dev/null || die "runner pod not Ready"
kc exec "$POD" -- sh -c 'apk add --no-cache bash curl jq >/dev/null && mkdir -p /e2e' || die "could not install bash/curl/jq"
tar -C "$SCRIPTS" -cf - verify.sh pkce-login.py ws-subscribe.py e2e | kc exec -i "$POD" -- tar -C /e2e -xf - || die "copy failed"
kc exec -i "$POD" -- sh -c 'umask 077; cat > /e2e/secrets.env' < "$TMP/secrets.env"
kc exec -i "$POD" -- sh -c 'umask 077; cat > /e2e/realm.json' < "$TMP/realm.json"
ENVS="set -a; . /e2e/secrets.env; set +a; cd /e2e; export KC='$KC_URL' API='$API_URL' REALM_FILE=/e2e/realm.json SCRIPTS=/e2e PAYMENTS_SVC_URL=http://payments-svc.demo.svc:8083"
run() { # name command -> runs in the pod, output to $OUT/name.log
  echo "== $1"; kc exec "$POD" -- bash -c "$ENVS; $2" 2>&1 | tee "$OUT/$1.log"
}
START=$(date -u +%Y-%m-%dT%H:%M:%SZ)
run verify "TOKEN_MODE=pkce PAYMENTS=on PAYMENTS_SVC_URL=http://payments-svc.demo.svc:8083 ./verify.sh"
run TC-B "./e2e/TC-B-approve-pay-replay.sh"
if [ "$NREADY" -ge 2 ]; then
  run TC-F "./e2e/TC-F-cross-pod-subscription.sh $A_NAME $A_IP $B_NAME $B_IP"
else
  echo "SKIP  TC-F cross-pod subscription" | tee "$OUT/TC-F.log"; echo "      needs 2 Ready demo-api replicas" | tee -a "$OUT/TC-F.log"
fi
if [ "${E2E_METRICS:-on}" != off ]; then run TC-E "./e2e/TC-E-bounded-operation-label.sh"
else echo "SKIP  TC-E bounded operation label (E2E_METRICS=off)" | tee "$OUT/TC-E.log"; fi

echo "== laptop-side checks (logs the runner pod cannot read)"
creds_ok || inconclusive "credentials expired during the run; scenario results above may be cut off"
post_ok()   { echo "PASS  $1" | tee -a "$OUT/post.log"; }
post_bad()  { printf 'FAIL  %s\n      %s\n' "$1" "$2" | tee -a "$OUT/post.log"; }
post_skip() { printf 'SKIP  %s\n      %s\n' "$1" "$2" | tee -a "$OUT/post.log"; }
CLAIM=$(sed -n 's/^CLAIM_ID=//p' "$OUT/TC-B.log" | tail -1)
PAY_PODS_NOW=$(pods_of payments-svc)
if [ -z "$CLAIM" ]; then
  post_bad "payments-svc recorded exactly one payment for TC-B's claim" "TC-B produced no claim id"
elif [ "$PAY_PODS_NOW" != "$PAY_PODS_START" ]; then
  # TC-B's GET /payments/<ref> already proved the payment exists; only the "exactly one" count is lost.
  post_skip "payments-svc recorded exactly one payment for TC-B's claim" \
    "payments-svc rolled during the run ($PAY_PODS_START -> $PAY_PODS_NOW): its in-memory store and logs went with the old pod"
else
  n=$(kc logs -l app.kubernetes.io/name=payments-svc --since-time="$START" --tail=-1 2>/dev/null \
      | jq -c --arg c "$CLAIM" 'select(.msg=="payment recorded" and .reference==$c)' 2>/dev/null | wc -l | tr -d ' ')
  [ "$n" = 1 ] && post_ok "payments-svc recorded exactly one payment for TC-B's claim $CLAIM" \
    || post_bad "payments-svc recorded exactly one payment for TC-B's claim" "found $n 'payment recorded' lines for $CLAIM (pod $PAY_PODS_NOW)"
fi
if [ "$NREADY" -ge 2 ] && grep -q '^PASS  subscriber on A' "$OUT/TC-F.log"; then
  case " $(pods_of demo-api) " in
    *" $A_NAME "*)
      kc logs "$A_NAME" --since-time="$START" --tail=-1 2>/dev/null | jq -e 'select(.status==101)' >/dev/null 2>&1 \
        && post_ok "pod A ($A_NAME) served the WebSocket (101) during TC-F" \
        || post_bad "pod A served the WebSocket during TC-F" "no status 101 in $A_NAME's access log since $START" ;;
    *) post_skip "pod A served the WebSocket during TC-F" "demo-api rolled during the run ($A_NAME is gone; pods at start: $API_PODS_START)" ;;
  esac
fi

# A scenario that crashed before its last line printed no FAIL: count that as a FAIL itself.
for t in verify TC-B TC-F TC-E; do
  [ -f "$OUT/$t.log" ] || { post_bad "$t ran" "no output"; continue; }
  grep -qE 'passed=[0-9]+ failed=[0-9]+' "$OUT/$t.log" || grep -q '^SKIP' "$OUT/$t.log" \
    || post_bad "$t ran to completion" "no summary line (crashed or cut off)"
done

echo; echo "== summary"
cat "$OUT"/*.log | grep -E '^(PASS|FAIL|SKIP)  ' > "$TMP/all"
P=$(grep -c '^PASS' "$TMP/all"); F=$(grep -c '^FAIL' "$TMP/all"); S=$(grep -c '^SKIP' "$TMP/all")
grep -E '^(FAIL|SKIP)' "$TMP/all" || true
echo "e2e passed=$P failed=$F skipped=$S"
[ "$F" -eq 0 ]
