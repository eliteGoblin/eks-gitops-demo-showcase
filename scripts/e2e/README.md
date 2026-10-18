# End-to-end tests

Two layers, one rule: **a check may only pass on positive evidence.** An unreachable host, a
missing token, an error envelope, `{}`, state left by an earlier run, or "the call failed" is
never a pass. Every check prints `PASS`, `FAIL` or `SKIP`; `SKIP` is listed in the summary and is
never counted as a pass.

| What | Where it runs | Command |
|---|---|---|
| Assertion self-test: each predicate in `asserts.sh` must reject the bad inputs an independent audit found slipping through, and accept good evidence | anywhere, no cluster (CI: `pr` → lint) | `make e2e-selftest` |
| `scripts/verify.sh` + the scenarios below, against the live EKS demo | a throwaway pod in the cluster | `make e2e` |
| `scripts/verify.sh` against a local stack (compose or kind) | the laptop | `make verify` / `make kind-verify` |

## Scenarios (`make e2e`)

| Script | Proves |
|---|---|
| `../verify.sh` (in-cluster mode) | authn/authz, field masking, payments (`PAID`, `pay_…` ref, the ref resolves in payments-svc to the claim and amount), replayed approval refused, payments-svc `Idempotency-Key` replay (201 → 200 `Idempotent-Replayed`, same id) and conflict (409), support requests (reserved budget grows by exactly the approved amount), live updates |
| `TC-B-approve-pay-replay.sh` | a fresh claim is paid once; the laptop then counts payments-svc's `payment recorded` lines for that claim (must be exactly 1) |
| `TC-F-cross-pod-subscription.sh` | a subscriber on pod A (pod IP, not the Service) receives a change made through pod B, and the reverse; the two pods are asserted different; the laptop confirms pod A served the WebSocket (101) |
| `TC-E-bounded-operation-label.sh` | the GraphQL `operation` metric label cannot be set by clients: requests carry a random marker as operationName, alias and field; once this run's own requests are counted in Amazon Managed Prometheus (positive control), no series or label value may contain the marker |

`run-in-cluster.sh` creates the runner pod (labelled `app.kubernetes.io/name=demo-api`, so
payments-svc's NetworkPolicy admits it; it is not a Service endpoint and not in the PDB), copies the
scripts, the demo users' passwords (Secrets Manager `demo/keycloak/users`) and the secrets the checks need (piped over
stdin into mode-600 files, never in the pod spec), runs everything, adds the two laptop-side log
checks, prints one summary, and deletes the pod and every temp file. It uses only
`~/.kube/eks-gitops-demo`. `E2E_METRICS=off` skips TC-E (it waits up to ~2.5 min for Prometheus).

Why in-cluster: payments-svc is internal; TC-F needs pod IPs; and laptops behind a filtering proxy
(e.g. Cloudflare WARP/Gateway blocking a newly registered domain) cannot reach the public demo URLs.

## Exit codes and what is not a test result

| Exit | Meaning |
|---|---|
| 0 | every check PASS (SKIPs listed) |
| 1 | at least one FAIL |
| 2 | the runner could not start (missing kubeconfig, no demo-api deployment, pod not Ready) |
| 3 | **INCONCLUSIVE**: AWS or Kubernetes credentials expired or were missing (before or during the run). No verdict; run `aws login` and re-run |

A rollout during the run is also not a failure of the system under test: the laptop-side log
checks compare the pods at the start with the pods at the end, and report `SKIP` (with the
before/after pod names) when payments-svc or TC-F's pod A changed, because the in-memory payment
store and the old pods' logs went with them. The payment's existence is proved independently by
`GET /payments/<ref>` (TC-B and verify.sh); only the "exactly one" count needs the logs.

## Notes

- Counters in Prometheus: compare the summed value before and after, not `increase()`. On a fresh
  pod the series is new and `increase()` treats its first sample as the baseline (reports 0).
- The scenarios create demo data (claims, support requests, a 1-cent mock payment per run). That is
  normal application use of the dev environment.
- Requires: kubectl, jq, aws CLI (the demo users' passwords, and the Grafana admin secret for TC-E).
