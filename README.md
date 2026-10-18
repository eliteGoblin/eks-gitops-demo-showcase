# eks-gitops-demo

Go GraphQL API on AWS EKS behind a Cloudflare Zero Trust edge, with Keycloak single sign-on, GitOps by Argo CD and Terraform applied by CI; live at [app.frankbluemind.com](https://app.frankbluemind.com/).

![Solution architecture: users reach the system only through Cloudflare; inside AWS, EKS runs cloudflared, demo-api, payments-svc, Keycloak, Grafana and the platform helpers next to RDS, Secrets Manager, ECR and the observability services; GitHub Actions releases to ECR and Argo CD watches the gitops repo](docs/showcase/solution-architecture.svg)

## Try it (2 minutes)

| Open | Expect |
|---|---|
| [app.frankbluemind.com](https://app.frankbluemind.com/) | token walkthrough: PKCE login (guest account) → decoded token → first query |
| [app.frankbluemind.com/playground](https://app.frankbluemind.com/playground) | GraphiQL with login bar (same account); introspection on |
| [auth.frankbluemind.com/realms/demo/.well-known/openid-configuration](https://auth.frankbluemind.com/realms/demo/.well-known/openid-configuration) | OIDC discovery, 200; `/admin` and master realm → 404 at the edge |
| [grafana.frankbluemind.com/public-dashboards/…](https://grafana.frankbluemind.com/public-dashboards/12ba0f7298d948c195379317cc6e798b) | "demo-api: RED + scaling", public, no login, last 30 min |
| [argocd.frankbluemind.com](https://argocd.frankbluemind.com/) · [grafana.frankbluemind.com](https://grafana.frankbluemind.com/) | 302 → Cloudflare Access → Keycloak → signed in |
| `curl -s https://app.frankbluemind.com/readyz` | `{"status":"ready"}`, no login; `POST /graphql` without a token → 401 |

Guest login: read-only, provided separately.

**[Solution overview with diagrams](https://elitegoblin.github.io/eks-gitops-demo-docs/)**: flows · component table · observability and code diagrams · design records

## Go server

- Clean Architecture: `domain` ← `app` ← adapters ← `cmd`; consumer-owned ports; one composition root per service · [code](internal/)
- Middleware chain: request id → tracing → recover → access log → security headers → CORS → body limit + 10 s timeout → rate limit → token verify · [httpmw](internal/platform/httpmw/httpmw.go)
- Context + shutdown: per-request deadline; cancellation → `CANCELLED`; SIGTERM → `/readyz` 503 → 3 s → 20 s drain (chart preStop + grace period) · [server kit](internal/platform/platform.go)
- Logs + tests: JSON logs with request, trace and span ids; table-driven tests, `-race`, one contract suite on memory + Postgres, coverage gate, `go vet` + golangci-lint · [pr.yaml](.github/workflows/pr.yaml)

## GraphQL

- gqlgen, schema-first; thin resolvers, use cases do the work; breaking-change gate in CI · [schema](internal/adapter/graphql/schema/schema.graphqls)
- DataLoaders per response: one query per field level, no N+1 (visible in traces) · [loaders](internal/adapter/graphql/loaders.go)
- `@auth` directive → app-layer RoleGate, refusals audited; typed union errors (`ValidationError`, `BudgetExceededError`, `SelfApprovalNotAllowedError`) · [gate](internal/app/guard.go)
- APQ live; trusted-document allowlist mode built and tested, off in the demo; depth 8 · complexity 300 · 32 KiB documents; WebSocket subscriptions fanned out via Postgres NOTIFY/LISTEN; idempotency keys on claims and payments · [persisted](persisted/)

## EKS

- EKS 1.37, access entries; managed arm64 node group (AL2023, t4g, 2–4); add-ons VPC CNI · CoreDNS · kube-proxy · Pod Identity agent; Pod Identity, no static keys · [infra/eks](infra/eks/)
- Argo CD app-of-apps: root → addons → platform → apps, sync waves, 14 ApplicationSets, Helm as renderer, charts from ECR (pull token refreshed every 6 h by External Secrets) · [gitops/bootstrap](gitops/bootstrap/) · [apps/dev](gitops/apps/dev/)
- Platform: External Secrets + Reloader · ADOT collector · Fluent Bit → CloudWatch · metrics-server + kube-state-metrics · [addons](gitops/addons/)
- Workloads: demo-api (HPA 2–6, PDB, `/readyz` + `/healthz`, rolling updates, preStop drain) · payments-svc · Keycloak · Grafana · cloudflared ×2; NetworkPolicies (VPC CNI network policy agent): cloudflared-only ingress · [chart](deploy/helm/demo-api/) · [policies](gitops/cloudflared/networkpolicies.yaml)

## Auth

- OIDC discovery + JWKS (RS256); checks: issuer · audience · expiry · `typ` Bearer (ID tokens rejected) · `azp` allowlist · [authn](internal/platform/authn/)
- Authorization Code + PKCE (S256) for browsers, public client `spa`; client credentials service-to-service, audience-scoped (`demo-api-svc` → payments-svc, role `payments:write`) · [realm](gitops/config/keycloak/realm.yaml)
- Refresh-token rotation · brute-force protection · 5-minute access tokens
- Roles → RoleGate → relationship policy → field masking; separation of duties; SSO across app · Argo CD · Grafana (groups → roles); Access with Keycloak as IdP · [policy](internal/app/policy.go) · [Argo CD RBAC](infra/argocd/argocd.tf)

## Pipelines

- Release: PR → vet, race tests, coverage, codegen drift, lint, Helm lint + kubeconform, gitleaks, gosec, govulncheck, Trivy, tf plan → merge → arm64 image + OCI chart → ECR, digest-pinned, SBOM, keyless cosign → gitops bump → Argo CD sync → rolling update · [release.yaml](.github/workflows/release.yaml)
- Infra: Terraform roots `bootstrap` / `eks` / `argocd` / `cloudflare`, applied by CI via GitHub OIDC, no stored keys; CI holds no cluster credentials; rollback = revert the bump · [tf-apply.yaml](.github/workflows/tf-apply.yaml)
- Secrets: SOPS in Git → CI → Secrets Manager → External Secrets → Reloader; RDS master password rotated by Secrets Manager; demo passwords only in Secrets Manager · [secrets/](secrets/) · [rotation demo](scripts/rotate-secret-demo.sh)

## Cloudflare

- Tunnel = only ingress: cloudflared ×2 outbound, remotely managed rules, no inbound ports, no load balancer; Access (Keycloak IdP) on `argocd.` + `grafana.`, bypass for the public dashboard · [tunnel.tf](infra/cloudflare/tunnel.tf) · [access.tf](infra/cloudflare/access.tf)
- Zone: proxied DNS · Universal SSL · TLS 1.2+ · HTTPS only; `auth.` allowlist `/realms/demo` + `/resources`, dot segments → 404; Browser Integrity Check on; 301s from the old domain · [zone.tf](infra/cloudflare/zone.tf) · [redirects.tf](infra/cloudflare/redirects.tf)
- `CF-Connecting-IP` trusted only because NetworkPolicy admits cloudflared alone; per-IP rate limit before auth · [cloudflared/](gitops/cloudflared/)

## Verified

- 68/68 in-cluster e2e checks: PKCE for 5 users · token rejections · field masking · payment idempotency · cross-pod subscriptions · bounded labels; a self-test proves each check rejects bad evidence (added after an independent audit found a false pass) · [scripts/e2e/](scripts/e2e/)
- Zero-downtime secret rotation (0 failed requests) · Cloudflare cutover 73,836 probe requests, 0 errors · browser SSO into Argo CD + Grafana
- Design-first · AI-assisted implementation under the author's direction and review · the Try-it table re-checked live at every publish · [docs/](docs/)

> **About this repository:** published snapshot · CI/CD and Argo CD run from private working copies · [`gitops/`](gitops/) = what Argo CD watches
