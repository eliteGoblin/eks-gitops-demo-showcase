GO ?= go
KEYCLOAK_PORT ?= 8080
DISCOVERY := http://localhost:$(KEYCLOAK_PORT)/realms/demo/.well-known/openid-configuration
IMAGE ?= demo-api:dev
# Which cmd/ to build into the image: api (demo-api) or payments (payments-svc).
IMAGE_CMD ?= api
KEYCLOAK_IMAGE := quay.io/keycloak/keycloak:26.0
POSTGRES_IMAGE := postgres:17-alpine
PG_PORT ?= 5432
# Local development database (deploy/docker-compose.yml); dev-only credentials.
LOCAL_DATABASE_URL ?= postgres://demo:demo@localhost:$(PG_PORT)/demo?sslmode=disable
KIND_CLUSTER := eks-gitops-demo
KIND_NS := demo
KIND_ARCH := $(shell docker version --format '{{.Server.Arch}}' 2>/dev/null)
# Override when proxy.golang.org is unreachable: make image GOPROXY=https://goproxy.cn,direct
GOPROXY ?= https://proxy.golang.org,direct
# Optional PEM for networks that intercept TLS (e.g. a corporate gateway); used only during the build.
EXTRA_CA ?=
COVER_PKGS := ./internal/domain/... ./internal/app/...
COVER_MIN := 80
comma := ,

.PHONY: persisted-queries browser-check up down db-up migrate gen api api-mem test test-db cover lint vuln verify build image kind-up kind-deploy kind-down kind-verify secrets-seed eks-up eks-down eks-kubeconfig payments load tf-plan secrets-sync

## up: start Postgres and Keycloak (realm "demo" imported from deploy/keycloak/realm.json) and wait for both
up:
	KEYCLOAK_PORT=$(KEYCLOAK_PORT) PG_PORT=$(PG_PORT) docker compose -f deploy/docker-compose.yml up -d --wait postgres
	KEYCLOAK_PORT=$(KEYCLOAK_PORT) PG_PORT=$(PG_PORT) docker compose -f deploy/docker-compose.yml up -d
	@echo "waiting for Keycloak at $(DISCOVERY) ..."
	@for i in $$(seq 1 90); do \
	  if curl -sf $(DISCOVERY) | grep -q '"issuer"'; then echo "Keycloak ready"; exit 0; fi; sleep 2; done; \
	  echo "Keycloak did not become ready in time; see: docker compose -f deploy/docker-compose.yml logs"; exit 1

## down: stop Keycloak and Postgres and delete the database volume (the realm and data are recreated on the next up)
down:
	docker compose -f deploy/docker-compose.yml down -v

## db-up: start only the local Postgres
db-up:
	PG_PORT=$(PG_PORT) docker compose -f deploy/docker-compose.yml up -d --wait postgres

## migrate: apply the database migrations plus the demo seed to the local Postgres
migrate:
	STORE=postgres DATABASE_URL='$(LOCAL_DATABASE_URL)' ENV=dev $(GO) run ./cmd/api migrate --seed

## gen: regenerate gqlgen code from internal/adapter/graphql/schema/*.graphqls
gen:
	$(GO) tool gqlgen generate

## persisted-queries: rebuild persisted/queries.json (the allowlist) from persisted/documents/*.graphql
persisted-queries:
	$(GO) run ./cmd/persistedgen

## browser-check: load / and /playground in Chrome, log in with PKCE, fail on any CSP violation (API and Keycloak must be running; DEMO_USER/DEMO_PASS set)
browser-check:
	cd scripts/browser && PUPPETEER_SKIP_DOWNLOAD=1 npm install --no-audit --no-fund --silent && BASE=$(or $(BASE),http://localhost:8082) node check.js

build:
	$(GO) build ./...

## api: run the GraphQL API + pages on http://localhost:8082 on the local Postgres, with development features on
api: migrate
	STORE=postgres DATABASE_URL='$(LOCAL_DATABASE_URL)' ENV=dev PLAYGROUND_ENABLED=true INTROSPECTION_ENABLED=true $(GO) run ./cmd/api

## api-mem: the same on the in-memory store (no database needed; data resets on restart)
api-mem:
	ENV=dev PLAYGROUND_ENABLED=true INTROSPECTION_ENABLED=true $(GO) run ./cmd/api

## payments: run payments-svc on http://localhost:8083 against the local Keycloak
payments:
	ENV=dev OIDC_ISSUER=http://localhost:$(KEYCLOAK_PORT)/realms/demo $(GO) run ./cmd/payments

## test: unit and contract tests on the memory store (Postgres contract tests are skipped)
test:
	$(GO) vet ./... && $(GO) test -race ./...

## test-db: the same plus the Postgres contract tests against the local database (make db-up first)
test-db:
	TEST_DATABASE_URL='$(LOCAL_DATABASE_URL)' $(GO) test -race ./...

## cover: fail unless domain + app coverage is at least $(COVER_MIN)%
cover:
	$(GO) test -coverprofile=coverage.out $(COVER_PKGS)
	@total=$$($(GO) tool cover -func=coverage.out | awk '/^total:/ {sub("%","",$$3); print $$3}'); \
	  echo "domain+app coverage: $$total%"; \
	  awk -v t="$$total" -v m=$(COVER_MIN) 'BEGIN { exit (t+0 >= m) ? 0 : 1 }' || { echo "below $(COVER_MIN)%"; exit 1; }

lint:
	golangci-lint run ./...

vuln:
	govulncheck ./...

## verify: end-to-end checks against running Keycloak + API (uses the dev-only verify-cli client)
verify:
	./scripts/verify.sh

.PHONY: e2e e2e-selftest
## e2e-selftest: prove the end-to-end assertions reject bad evidence (no cluster needed; runs in CI)
e2e-selftest:
	./scripts/e2e/selftest.sh

## e2e: verify.sh + the TC-* scenarios from a throwaway pod in the EKS cluster (scripts/e2e/README.md)
e2e:
	./scripts/e2e/run-in-cluster.sh

## image: build a service image ($(IMAGE) from cmd/$(IMAGE_CMD)) for the local Docker platform
image:
	DOCKER_BUILDKIT=1 docker build -t $(IMAGE) --build-arg CMD=$(IMAGE_CMD) --build-arg GOPROXY=$(GOPROXY) \
	  $(if $(EXTRA_CA),--secret id=extra_ca$(comma)src=$(EXTRA_CA)) .

K6_IMAGE := grafana/k6:1.3.0
LOAD_URL ?= http://localhost:8082
LOAD_TOKEN_URL ?= http://localhost:$(KEYCLOAK_PORT)/realms/demo/protocol/openid-connect/token
LOAD_CLIENT_SECRET ?= dev-only-load-generator-secret
PEAK_VUS ?= 20
## load: ~3-minute k6 ramp against $(LOAD_URL) (local k6, else the k6 container)
load:
	@if command -v k6 >/dev/null; then \
	  k6 run -e BASE_URL=$(LOAD_URL) -e TOKEN_URL=$(LOAD_TOKEN_URL) -e CLIENT_SECRET=$(LOAD_CLIENT_SECRET) -e PEAK_VUS=$(PEAK_VUS) loadtest/graphql.js; \
	else \
	  docker run --rm -v "$(CURDIR)/loadtest:/scripts:ro" $(K6_IMAGE) run \
	    -e BASE_URL=$(subst localhost,host.docker.internal,$(LOAD_URL)) \
	    -e TOKEN_URL=$(subst localhost,host.docker.internal,$(LOAD_TOKEN_URL)) \
	    -e CLIENT_SECRET=$(LOAD_CLIENT_SECRET) -e PEAK_VUS=$(PEAK_VUS) /scripts/graphql.js; \
	fi

## kind-up: create the local kind cluster (host ports 8180 Keycloak, 8182 API)
kind-up:
	kind get clusters | grep -qx $(KIND_CLUSTER) || kind create cluster --config deploy/kind/cluster.yaml
	kubectl config use-context kind-$(KIND_CLUSTER)

## kind-deploy: load images, deploy Keycloak and the Helm chart into namespace $(KIND_NS)
kind-deploy: image kind-up
	for img in $(KEYCLOAK_IMAGE) $(POSTGRES_IMAGE); do docker image inspect $$img >/dev/null 2>&1 || docker pull $$img || exit 1; done
	# Single-platform archives: `kind load docker-image` fails on Docker Desktop's multi-platform image store.
	tmp=$$(mktemp -d) && trap 'rm -rf "$$tmp"' EXIT && for img in $(IMAGE) $(KEYCLOAK_IMAGE) $(POSTGRES_IMAGE); do \
	  docker save --platform linux/$(KIND_ARCH) $$img -o "$$tmp/image.tar" && \
	  kind load image-archive --name $(KIND_CLUSTER) "$$tmp/image.tar" || exit 1; done
	kubectl create namespace $(KIND_NS) --dry-run=client -o yaml | kubectl apply -f -
	kubectl -n $(KIND_NS) create configmap keycloak-realm --from-file=realm.json=deploy/keycloak/realm.json \
	  --dry-run=client -o yaml | kubectl apply -f -
	kubectl -n $(KIND_NS) apply -f deploy/kind/postgres.yaml
	kubectl -n $(KIND_NS) apply -f deploy/kind/keycloak.yaml
	NS=$(KIND_NS) ./deploy/kind/setup-dns.sh
	kubectl -n $(KIND_NS) rollout status deployment postgres --timeout=120s
	kubectl -n $(KIND_NS) rollout status deployment keycloak --timeout=180s
	helm upgrade --install demo-api deploy/helm/demo-api -n $(KIND_NS) -f deploy/kind/values-kind.yaml --wait --timeout 180s
	kubectl -n $(KIND_NS) rollout restart deployment demo-api
	kubectl -n $(KIND_NS) rollout status deployment demo-api --timeout=180s

## kind-verify: run the end-to-end checks against the kind deployment
kind-verify:
	KC=http://keycloak.localtest.me:8180/realms/demo API=http://localhost:8182 ./scripts/verify.sh

## kind-down: delete the kind cluster
kind-down:
	kind delete cluster --name $(KIND_CLUSTER)

## secrets-seed: generate the long-lived secrets once (Secrets Manager + deploy keys); idempotent
secrets-seed:
	./scripts/secrets-seed.sh

## eks-up: build the cluster layer (VPC, EKS, RDS, IAM), then the GitOps bridge (Argo CD + root app)
eks-up:
	terraform -chdir=infra/eks init -input=false
	terraform -chdir=infra/eks apply -input=false -auto-approve
	./scripts/secrets-seed.sh # values for the containers eks just created (Grafana); existing values are kept
	$(MAKE) eks-kubeconfig
	terraform -chdir=infra/argocd init -input=false
	terraform -chdir=infra/argocd apply -input=false -auto-approve
	./scripts/argocd-repo-secret.sh

eks-kubeconfig:
	aws eks update-kubeconfig --region ap-southeast-2 --name eks-gitops-demo --alias eks-gitops-demo

## eks-down: stop Argo CD, then destroy the argocd and eks layers (bootstrap and cloudflare stay)
eks-down:
	./scripts/eks-down.sh

ROOT ?= eks
## tf-plan: the pull-request checks and a read-only plan for one Terraform root (ROOT=bootstrap|eks|argocd|cloudflare), no lock; cloudflare's plan needs CLOUDFLARE_API_TOKEN in the environment
tf-plan:
	scripts/ci/tf.sh check $(ROOT) && scripts/ci/tf.sh plan $(ROOT)

## secrets-sync: decrypt secrets/**/*.enc.yaml with sops and write Secrets Manager (what secrets-sync.yaml does on main)
secrets-sync:
	scripts/ci/secrets-sync.sh secrets
