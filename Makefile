GO_IMAGE ?= golang:1.25@sha256:cbff9d1a9041b316010f2da6b701b6c0d597718cb90928c85eb597334a0d23d4
NODE_IMAGE ?= node:22.14.0-alpine@sha256:9bef0ef1e268f60627da9ba7d7605e8831d5b56ad07487d24d1aa386336d1944
OPERATOR_IMAGE ?= ember-operator:dev
MOCK_ENGINE_IMAGE ?= ember-mock-engine:dev
FAKE_GPU_IMAGE ?= ember-fake-gpu:dev
PREFETCH_IMAGE ?= ember-prefetch:dev
GATEWAY_IMAGE ?= ember-gateway:dev
CONTROL_API_IMAGE ?= ember-control-api:dev
KIND_CLUSTER ?= ember
KIND_NODE_IMAGE ?= kindest/node:v1.32.0
KIND ?= kind
GCP_PROJECT ?= $(shell gcloud config get-value project 2>/dev/null)
GKE_CLUSTER ?= ember-gpu
GKE_LOCATION ?= us-central1-a
GKE_GPU_NODE_POOL ?= l4-spot
GCP_REGION ?= us-central1
GKE_IMAGE_REPOSITORY ?= ember
GKE_IMAGE_TAG ?= $(shell git rev-parse --short=12 HEAD)
GPU_TTL_HOURS ?= 3
CLUSTER_TTL_HOURS ?= 6
GO_DOCKER = docker run --rm --user "$$(id -u):$$(id -g)" -e GOMODCACHE=/workspace/.cache/gomod -e GOCACHE=/workspace/.cache/gocache -v "$(CURDIR)":/workspace -w /workspace $(GO_IMAGE)
NODE_DOCKER = docker run --rm --user "$$(id -u):$$(id -g)" -e npm_config_cache=/tmp/npm-cache -v "$(CURDIR)":/workspace -w /workspace/web $(NODE_IMAGE)
COST_GUARD_ENV = PROJECT_ID="$(GCP_PROJECT)" CLUSTER_NAME="$(GKE_CLUSTER)" CLUSTER_LOCATION="$(GKE_LOCATION)" GPU_NODE_POOL="$(GKE_GPU_NODE_POOL)" GPU_TTL_HOURS="$(GPU_TTL_HOURS)" CLUSTER_TTL_HOURS="$(CLUSTER_TTL_HOURS)"

.PHONY: fmt tidy test vet build web-install web-test web-build scripts-check verify images manifests kind-create kind-load cluster-auth kind-auth keda-install deploy sample kind-smoke control-api-smoke undeploy gke-cluster-create gke-cluster-status gke-build-images gke-deploy gke-real-smoke gcp-cost-guard-setup gcp-cost-guard-arm gcp-cost-guard-status gcp-cost-guard-disarm gcp-cost-guard-destroy

fmt:
	$(GO_DOCKER) sh -ec 'gofmt -w $$(find . -name "*.go" -not -path "./.cache/*")'

tidy:
	$(GO_DOCKER) go mod tidy

test:
	$(GO_DOCKER) go test ./...

vet:
	$(GO_DOCKER) go vet ./...

build:
	$(GO_DOCKER) sh -ec 'mkdir -p .cache/bin && go build -o .cache/bin/ember-operator ./operator/cmd/operator && go build -o .cache/bin/ember-mock-engine ./cmd/mock-engine && go build -o .cache/bin/ember-fake-gpu ./cmd/fake-gpu-plugin && go build -o .cache/bin/ember-prefetch ./cmd/prefetch && go build -o .cache/bin/ember-gateway ./gateway/cmd/gateway && go build -o .cache/bin/ember-control-api ./controlapi/cmd/control-api && go build -o .cache/bin/ember-auth-tool ./cmd/auth-tool'

web-install:
	$(NODE_DOCKER) npm ci --ignore-scripts --no-audit --no-fund

web-test: web-install
	$(NODE_DOCKER) npm run test

web-build: web-install
	$(NODE_DOCKER) npm run build

scripts-check:
	bash -n scripts/*.sh

verify: fmt tidy test vet build web-test web-build scripts-check manifests

images:
	docker build -f Dockerfile.operator -t $(OPERATOR_IMAGE) .
	docker build -f Dockerfile.mock-engine -t $(MOCK_ENGINE_IMAGE) .
	docker build -f Dockerfile.fake-gpu -t $(FAKE_GPU_IMAGE) .
	docker build -f Dockerfile.prefetch -t $(PREFETCH_IMAGE) .
	docker build -f Dockerfile.gateway -t $(GATEWAY_IMAGE) .
	docker build -f Dockerfile.control-api -t $(CONTROL_API_IMAGE) .

manifests:
	kubectl kustomize operator/config/crd >/dev/null
	kubectl kustomize operator/config/rbac >/dev/null
	kubectl kustomize operator/config/policy >/dev/null
	kubectl kustomize operator/config/manager >/dev/null
	kubectl kustomize gateway/config >/dev/null
	kubectl kustomize controlapi/config >/dev/null
	kubectl kustomize monitoring/config >/dev/null
	kubectl kustomize operator/config/samples >/dev/null
	kubectl kustomize deploy/kind >/dev/null
	kubectl kustomize deploy/gke >/dev/null

kind-create:
	$(KIND) create cluster --name $(KIND_CLUSTER) --image $(KIND_NODE_IMAGE) --config deploy/kind/kind-config.yaml

kind-load:
	$(KIND) load docker-image --name $(KIND_CLUSTER) $(OPERATOR_IMAGE) $(MOCK_ENGINE_IMAGE) $(FAKE_GPU_IMAGE) $(PREFETCH_IMAGE) $(GATEWAY_IMAGE) $(CONTROL_API_IMAGE)

cluster-auth:
	kubectl get namespace ember-system >/dev/null
	@if ! kubectl -n ember-system get secret ember-jwt-keys >/dev/null 2>&1; then \
		$(GO_DOCKER) go run ./cmd/auth-tool keygen --namespace ember-system --name ember-jwt-keys | kubectl apply -f -; \
	fi
	@if ! kubectl -n ember-system get secret ember-postgres >/dev/null 2>&1; then \
		$(GO_DOCKER) go run ./cmd/auth-tool postgres-secret \
			--namespace ember-system \
			--name ember-postgres | kubectl apply -f -; \
	fi

kind-auth:
	kubectl apply -f deploy/kind/namespace.yaml
	$(MAKE) cluster-auth

keda-install:
	./scripts/install-keda.sh

deploy: kind-auth keda-install
	kubectl apply -k deploy/kind

sample:
	kubectl apply -k operator/config/samples

kind-smoke:
	./scripts/kind-smoke.sh

control-api-smoke:
	./scripts/control-api-smoke.sh

undeploy:
	kubectl delete -k deploy/kind --ignore-not-found

gke-cluster-create:
	$(COST_GUARD_ENV) ./scripts/gke-cluster.sh create

gke-cluster-status:
	$(COST_GUARD_ENV) ./scripts/gke-cluster.sh status

gke-build-images:
	PROJECT_ID="$(GCP_PROJECT)" REGION="$(GCP_REGION)" REPOSITORY="$(GKE_IMAGE_REPOSITORY)" IMAGE_TAG="$(GKE_IMAGE_TAG)" ./scripts/gke-build-images.sh

gke-deploy:
	PROJECT_ID="$(GCP_PROJECT)" CLUSTER_NAME="$(GKE_CLUSTER)" CLUSTER_LOCATION="$(GKE_LOCATION)" REGION="$(GCP_REGION)" REPOSITORY="$(GKE_IMAGE_REPOSITORY)" IMAGE_TAG="$(GKE_IMAGE_TAG)" ./scripts/gke-deploy.sh

gke-real-smoke:
	PROJECT_ID="$(GCP_PROJECT)" CLUSTER_NAME="$(GKE_CLUSTER)" CLUSTER_LOCATION="$(GKE_LOCATION)" GPU_NODE_POOL="$(GKE_GPU_NODE_POOL)" ./scripts/gke-real-smoke.sh

gcp-cost-guard-setup:
	$(COST_GUARD_ENV) ./scripts/gcp-cost-guard.sh setup

gcp-cost-guard-arm:
	$(COST_GUARD_ENV) ./scripts/gcp-cost-guard.sh arm

gcp-cost-guard-status:
	$(COST_GUARD_ENV) ./scripts/gcp-cost-guard.sh status

gcp-cost-guard-disarm:
	$(COST_GUARD_ENV) ./scripts/gcp-cost-guard.sh disarm

gcp-cost-guard-destroy:
	$(COST_GUARD_ENV) ./scripts/gcp-cost-guard.sh destroy-all
