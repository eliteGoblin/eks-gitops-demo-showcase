#!/usr/bin/env bash
# Live demo: rotate the partner API key through the whole pipeline and watch
# each hop. Never prints the key itself, only its fingerprint (first 8 hex
# characters of its SHA-256), which is what demo-api logs and exports.
#
#   scripts/rotate-secret-demo.sh expect   fingerprint of the key in Git (sops -d)
#   scripts/rotate-secret-demo.sh rotate   write a new random key into the sops file
#                                          (then: commit, PR, merge; secrets-sync runs)
#   scripts/rotate-secret-demo.sh watch    print each hop as it happens (Ctrl-C to stop)
#
# Hops: merge → secrets-sync → Secrets Manager (new version) → external-secrets
# (demo-api-partner-file every 30s, demo-api-partner-env every 3m) → file: the
# running pods log "secret loaded … reload=true" (no restart) → env: Reloader
# rolls demo-api and the new pods log source=env with the new fingerprint.
# Needs: sops + AWS credentials (expect/rotate); kubectl + aws (watch).
set -euo pipefail

FILE=secrets/dev/demo-api.enc.yaml
SECRET_ID=demo/dev/demo-api/partner-api
NS=demo
export KUBECONFIG=${KUBECONFIG:-$HOME/.kube/eks-gitops-demo}

fingerprint() { # stdin → first 8 hex characters of its SHA-256
  if command -v sha256sum >/dev/null; then sha256sum; else shasum -a 256; fi | cut -c1-8
}

expect() {
  sops -d --extract '["partner-api"]["api_key"]' "$FILE" | fingerprint
}

rotate() {
  local key
  key=$(openssl rand -hex 24)
  sops set "$FILE" '["partner-api"]["api_key"]' "\"$key\""
  echo "new key written to $FILE (fingerprint $(printf '%s' "$key" | fingerprint))"
  echo "next: git switch -c rotate/partner-key && git commit -am 'chore(secrets): rotate the partner API key' && gh pr create --fill"
}

k8s_secret_fp() { # Secret name → resourceVersion and fingerprint of its api_key
  local rv
  rv=$(kubectl -n "$NS" get secret "$1" -o jsonpath='{.metadata.resourceVersion}' 2>/dev/null) || { echo "missing"; return; }
  echo "rv=$rv fp=$(kubectl -n "$NS" get secret "$1" -o jsonpath='{.data.api_key}' | base64 -d | fingerprint)"
}

watch_hops() {
  local last="" now line
  while true; do
    now=$(date -u +%H:%M:%SZ)
    line="sm=$(aws secretsmanager describe-secret --secret-id "$SECRET_ID" \
      --query 'VersionIdsToStages | keys(@) | length(@)' --output text) versions"
    line="$line | file-secret $(k8s_secret_fp demo-api-partner-file)"
    line="$line | env-secret $(k8s_secret_fp demo-api-partner-env)"
    line="$line | pods $(kubectl -n "$NS" get pods -l app.kubernetes.io/name=demo-api \
      -o jsonpath='{range .items[*]}{.metadata.name}{" "}{end}')"
    if [ "$line" != "$last" ]; then echo "$now $line"; last=$line; fi
    kubectl -n "$NS" logs -l app.kubernetes.io/name=demo-api --since=6s --prefix 2>/dev/null \
      | grep '"msg":"secret loaded"' | sed "s/^/$now   /" || true
    sleep 5
  done
}

case "${1:-}" in
  expect) expect ;;
  rotate) rotate ;;
  watch) watch_hops ;;
  *) sed -n '2,15p' "$0"; exit 2 ;;
esac
