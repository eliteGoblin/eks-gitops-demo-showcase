#!/usr/bin/env bash
# TC-E: the GraphQL `operation` metric label is bounded. Requests carry a random marker as
# operationName, alias and unknown field; once the metrics have reached Amazon Managed
# Prometheus (positive control: this run's own requests are counted), no series or label
# value may contain the marker. Queries go through Grafana's datasource proxy (uid amp).
# Needs GRAFANA_URL (default in-cluster), GRAFANA_USER, GRAFANA_PASSWORD.
set -u
. "$(dirname "$0")/lib.sh"
G=${GRAFANA_URL:-http://grafana.observability.svc}
if [ -z "${GRAFANA_USER:-}" ] || [ -z "${GRAFANA_PASSWORD:-}" ]; then
  skip "operation label stays bounded" "needs GRAFANA_USER and GRAFANA_PASSWORD"; summary "TC-E"; exit 0
fi
NETRC=$(mktemp); trap 'rm -f "$NETRC"' EXIT
( umask 077; printf 'machine %s login %s password %s\n' "$(printf '%s' "$G" | sed -E 's#^https?://##; s#[:/].*##')" "$GRAFANA_USER" "$GRAFANA_PASSWORD" > "$NETRC" )
prom() { curl -s --netrc-file "$NETRC" -G "$G/api/datasources/proxy/uid/amp/api/v1/$1" "${@:2}"; }
MARK=E2E_MARK_$(head -c 6 /dev/urandom | od -An -tx1 | tr -d ' \n')
START=$(date -u +%s)
# Positive control: the summed counter must grow by this run's 5 requests. Not increase():
# on a fresh pod the series is new, and increase() takes a new series' first sample as its
# baseline (reports 0 even though the requests were counted).
CONTROL='sum(graphql_operations_total{job="demo-api",operation="query.__typename+mySupportRequests"})'
control() { prom query --data-urlencode "query=$CONTROL" | jq -r '.data.result[0].value[1] // "0"'; }
BASE=$(control)
Z=$(token zhaolin.sun); need_tokens zhaolin.sun="$Z"
for _ in 1 2 3 4 5; do gql "$Z" "query $MARK { __typename mySupportRequests { id } }" "$MARK" >/dev/null; done # control
for _ in 1 2; do gql "$Z" 'query Foo { me { subject } }' "${MARK}_X" >/dev/null; done                       # unknown operationName
for _ in 1 2; do gql "$Z" "query $MARK { ${MARK}_alias: me { subject } }" "$MARK" >/dev/null; done          # alias
gql "$Z" "{ ${MARK}_field }" >/dev/null                                                                    # unknown root field
grown() { jq -ne --arg b "$BASE" --arg n "${1:-0}" '(($n|tonumber) - ($b|tonumber)) >= 5' >/dev/null 2>&1; }
now=""
for _ in $(seq 30); do # metrics reach AMP after the collector's batch + remote write
  now=$(control); grown "$now" && break
  sleep 5
done
if grown "$now"; then
  ok "positive control: this run's 5 requests reached Prometheus (counter $BASE -> $now)"
  series=$(prom series --data-urlencode 'match[]={job="demo-api"}' --data-urlencode "start=$START")
  n=$(printf '%s' "$series" | jq -r '.data | length')
  if [ "${n:-0}" -gt 0 ]; then
    printf '%s' "$series" | grep -q "$MARK" \
      && bad "no demo-api series carries the marker" "marker $MARK found in a label value" \
      || ok "no demo-api series carries the marker ($n series checked since the run started)"
  else
    bad "no demo-api series carries the marker" "series query returned nothing: $(printf '%s' "$series" | head -c 200)"
  fi
  vals=$(prom label/operation/values --data-urlencode 'match[]=graphql_operations_total{job="demo-api"}')
  check "operation label values are bounded (none client-supplied)" "$vals" \
    "(.data|length) > 0 and (.data|length) < 100 and ([.data[] | select(test(\"$MARK\"))] | length) == 0"
else
  bad "positive control: this run's 5 requests reached Prometheus" "counter $BASE -> ${now:-?} after 150 s (collector or AMP path broken?)"
fi
summary "TC-E"
