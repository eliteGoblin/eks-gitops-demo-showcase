#!/usr/bin/env bash
# Make keycloak.localtest.me resolve to the in-cluster Keycloak Service, so pods use the same issuer
# URL as the browser (split-horizon DNS, like a real public issuer name). Idempotent.
set -euo pipefail
NS=${NS:-demo}
RULE="rewrite name exact keycloak.localtest.me keycloak.${NS}.svc.cluster.local"

corefile=$(kubectl -n kube-system get configmap coredns -o jsonpath='{.data.Corefile}')
if printf '%s' "$corefile" | grep -qF "$RULE"; then
  echo "CoreDNS rewrite already present"; exit 0
fi
patched=$(printf '%s\n' "$corefile" | awk -v rule="    $RULE" '/^ *kubernetes cluster.local/ && !done { print rule; done=1 } { print }')
kubectl -n kube-system create configmap coredns --from-literal=Corefile="$patched" --dry-run=client -o yaml | kubectl apply -f -
kubectl -n kube-system rollout restart deployment coredns
kubectl -n kube-system rollout status deployment coredns --timeout=90s
