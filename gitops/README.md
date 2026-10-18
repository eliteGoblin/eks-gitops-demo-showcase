# eks-gitops-demo-gitops

Desired state of the `eks-gitops-demo` cluster. Argo CD pulls this repo; nothing here is applied by hand.

| Path | What | Read by |
|---|---|---|
| `bootstrap/` | Children of the root Application (app of apps): `addons` (wave 0), `platform` (wave 1), `apps` (wave 2) | root Application (created by Terraform, `infra/argocd` in the app repo) |
| `addons/` | One ApplicationSet per cluster add-on (external-secrets, metrics-server, Reloader, and the observability agents). Values come from the Argo CD cluster Secret's annotations, which Terraform writes (the GitOps bridge). | ApplicationSet controller |
| `platform/` | ClusterSecretStore (AWS Secrets Manager), ECR chart-repo credential (refreshed every 6 h), Argo CD OIDC client secret | Argo CD |
| `apps/dev/` | Keycloak, its realm config, demo-secrets, demo-api, load-generator (manual sync), cloudflared, keycloak-cloudflare-access | Argo CD |
| `cloudflared/` | Cloudflare Tunnel connector (2 replicas, PDB, token from Secrets Manager) and the NetworkPolicies that admit it (and the e2e runner) to the four origins, the only ingress since the ALB was removed | Argo CD (kustomize) |
| `charts/keycloak/` | Keycloak in production mode, database on RDS | Argo CD (Helm render) |
| `charts/demo-secrets/` | demo-api's `DATABASE_URL` for the demo namespace (RDS master secret, application database `demoapi`) via external-secrets | Argo CD (Helm render) |
| `loadtest/` | The k6 "fake load" Job (copied from the app repo's `loadtest/`, public URL patched in). Sync `load-generator` in Argo CD to run it; watch `kubectl -n demo get hpa -w`. | Argo CD (kustomize) |
| `config/keycloak/` | Realm as code (`realm.yaml`) applied by a keycloak-config-cli PostSync Job | Argo CD (kustomize) |
| `config/keycloak-cloudflare-access/` | The Keycloak client `cloudflare-access` (Cloudflare Access's identity provider), imported by its own PostSync Job so the realm import never depends on a Cloudflare secret | Argo CD (kustomize) |
| `values/dev/` | Helm values per environment | Argo CD (multi-source `$values`) |

**Promotion:** the app repo's release workflow bumps `targetRevision` in `apps/dev/demo-api.yaml` with a commit (dev only; a production environment would get a pull request with a required review). **Rollback:** revert that commit.

**Secrets:** none in this repo. Everything comes from AWS Secrets Manager through external-secrets, including the demo users' passwords: `config/keycloak/realm.yaml` holds only `$(env:PW_<USERNAME>)` placeholders, filled in by the realm Job from `demo/keycloak/users` (`config/keycloak/user-passwords.yaml`).
