# Keycloak realm (DEV / kind only)

`realm.json` is the **development** realm for docker-compose and the local kind cluster. It deliberately contains:

- fixed, shareable demo passwords (rotate after demos);
- the `verify-cli` password-grant client, used only by `scripts/verify.sh`;
- `localhost` redirect URIs.

None of these belong in a deployed environment. The EKS realm is managed as code (keycloak-config-cli, `gitops/config/keycloak/realm.yaml`) without them.
