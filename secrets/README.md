# secrets/: human-authored secrets as code

Values here are encrypted with [sops](https://github.com/getsops/sops) under the
KMS key `alias/sops-demo` (`.sops.yaml`). Keys stay readable, so a pull request
shows *which* secret changed; the values never do. Generated secrets (the RDS
password, the Argo CD OIDC client secret, deploy keys) are not in Git at all:
AWS creates and rotates them in Secrets Manager directly.

| File | Becomes |
|---|---|
| `secrets/<env>/<service>.enc.yaml`, top-level key `<key>` | Secrets Manager secret `demo/<env>/<service>/<key>` |

A map value is stored as a JSON SecretString (External Secrets selects fields
with `property`); a scalar is stored as-is. Example: `secrets/dev/demo-api.enc.yaml`
→ `demo/dev/demo-api/partner-api` with fields `api_key`, `base_url`.

## Flow

```
edit:  sops secrets/dev/demo-api.enc.yaml        (needs kms:Encrypt/Decrypt on the dev key)
PR:    reviewers see the changed keys, ciphertext values
merge: secrets-sync.yaml → role ci-secrets-sync → sops -d → put-secret-value (only if changed)
run:   External Secrets refreshes the Kubernetes Secret → Reloader restarts the pods
```

`make secrets-sync` runs the same script from a laptop. Re-runs are idempotent:
unchanged values create no new secret versions.

## Access model

One KMS key per environment. Whoever can decrypt a key can read every secret
of that environment, so the dev key is open to the developers (through IAM) and
a prod key would be limited to the release approvers and the sync role.
`ci-secrets-sync` can decrypt only this key and write only `demo/dev/*` and
`demo/prod/*`; the platform secrets under `demo/keycloak/*` and `demo/argocd/*`
are out of its reach.

Note: sops encrypts YAML comments as well, so keep explanations in this README.
