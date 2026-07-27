##############################################################################
# multiprox — Kubernetes operator Makefile
##############################################################################

OPERATOR_DIR := operator
IMG          ?= ghcr.io/feynman/multiprox-operator:latest
NODE_IMG     ?= ghcr.io/feynman/multiprox-node:latest
NAMESPACE    ?= multiprox-system
KUBECTL      := kubectl
HELM         := helm

.PHONY: help \
        operator-build operator-push \
        node-build node-push \
        install uninstall \
        deploy delete \
        helm-install helm-uninstall \
        status logs \
        generate manifests fmt vet

# ── Build ─────────────────────────────────────────────────────────────────────

operator-build:
	cd $(OPERATOR_DIR) && \
	  docker build -t $(IMG) .

operator-push:
	docker push $(IMG)

# Build the PVE node image (from docker/Dockerfile)
node-build:
	docker build -t $(NODE_IMG) ../docker/

node-push:
	docker push $(NODE_IMG)

# ── Code generation (requires controller-gen) ─────────────────────────────────

generate:
	cd $(OPERATOR_DIR) && \
	  go generate ./...

manifests:
	cd $(OPERATOR_DIR) && \
	  controller-gen rbac:roleName=multiprox-operator-role crd webhook \
	    paths="./..." \
	    output:crd:artifacts:config=../config/crd \
	    output:rbac:artifacts:config=../config/rbac

fmt:
	cd $(OPERATOR_DIR) && go fmt ./...

vet:
	cd $(OPERATOR_DIR) && go vet ./...

# ── Unit tests ────────────────────────────────────────────────────────────────
# Pure-Go, no cluster and no API server required. Runs in under a second, so
# there is no reason not to run it before every commit.
#
# Covers the logic most likely to break silently: output parsing
# (ParseInventory, ParseCephStatus), change detection (MergeNodeStatus), the
# Ceph plan and its clamping rules, and the resource builders — in particular
# that OSD PVCs are volumeMode: Block and attached as raw volumeDevices.

.PHONY: test test-verbose test-cover test-race check

test:
	cd $(OPERATOR_DIR) && go test ./... -count=1

test-verbose:
	cd $(OPERATOR_DIR) && go test ./... -count=1 -v

test-race:
	cd $(OPERATOR_DIR) && go test ./... -count=1 -race

test-cover:
	cd $(OPERATOR_DIR) && go test ./... -count=1 -coverprofile=coverage.out
	cd $(OPERATOR_DIR) && go tool cover -func=coverage.out | tail -30
	@echo ""
	@echo "HTML report: cd $(OPERATOR_DIR) && go tool cover -html=coverage.out"

# Everything that does not need a cluster — suitable for CI on every push.
check: fmt vet test

# ── Manifests: raw kubectl ────────────────────────────────────────────────────

install:
	@echo "==> Creating namespace..."
	$(KUBECTL) apply -f config/namespace.yaml
	@echo "==> Applying CRDs..."
	$(KUBECTL) apply -f config/crd/
	@echo "==> Applying RBAC..."
	$(KUBECTL) apply -f config/rbac/
	@echo "==> Done. Run 'make deploy' to start the operator."

uninstall:
	$(KUBECTL) delete -f config/rbac/ --ignore-not-found
	$(KUBECTL) delete -f config/crd/  --ignore-not-found

deploy:
	$(KUBECTL) apply -f config/namespace.yaml
	$(KUBECTL) apply -f config/manager/deployment.yaml

delete:
	$(KUBECTL) delete -f config/manager/deployment.yaml --ignore-not-found

# ── Helm ──────────────────────────────────────────────────────────────────────

helm-install:
	$(HELM) upgrade --install multiprox-operator helm/multiprox-operator \
	  --namespace $(NAMESPACE) \
	  --create-namespace \
	  --set image.repository=$(shell echo $(IMG) | cut -d: -f1) \
	  --set image.tag=$(shell echo $(IMG) | cut -d: -f2) \
	  --wait

helm-uninstall:
	$(HELM) uninstall multiprox-operator --namespace $(NAMESPACE)

# ── Status / debugging ────────────────────────────────────────────────────────

status:
	@echo "=== ProxmoxCluster resources ==="
	$(KUBECTL) get proxmoxclusters --all-namespaces -o wide 2>/dev/null || \
	  echo "(CRD not installed yet)"
	@echo ""
	@echo "=== ProxmoxNode projection (read-only, operator-written) ==="
	$(KUBECTL) get proxmoxnodes --all-namespaces 2>/dev/null || \
	  echo "(CRD not installed yet)"
	@echo ""
	@echo "=== Operator pods ==="
	$(KUBECTL) get pods -n $(NAMESPACE) -l app.kubernetes.io/name=multiprox-operator

logs:
	$(KUBECTL) logs -n $(NAMESPACE) \
	  -l app.kubernetes.io/name=multiprox-operator \
	  --tail=100 -f

# ── E2E tests ─────────────────────────────────────────────────────────────────
# Runs against a dedicated kind cluster named multiprox-e2e. Every command
# targets the context kind-multiprox-e2e explicitly, so other clusters on the
# host are never touched.
#
# The PVE node image used here is a STUB (test/e2e/stub-node). Proxmox packages
# are amd64-only and kind's local-path StorageClass cannot provide raw block
# volumes, so these tests validate the OPERATOR, not Proxmox or Ceph themselves.

E2E_CLUSTER := multiprox-e2e
E2E_CTX     := kind-multiprox-e2e
E2E_DIR     := test/e2e

.PHONY: e2e e2e-setup e2e-build e2e-run e2e-teardown

# Full cycle: create cluster, build+load images, install, run tests.
e2e: e2e-setup e2e-build e2e-install e2e-run

e2e-setup:
	@if kind get clusters 2>/dev/null | grep -qx "$(E2E_CLUSTER)"; then \
	  echo "==> kind cluster $(E2E_CLUSTER) already exists, reusing"; \
	else \
	  echo "==> creating kind cluster $(E2E_CLUSTER)"; \
	  kind create cluster --config $(E2E_DIR)/kind-config.yaml --wait 120s; \
	fi

e2e-build:
	@echo "==> building operator image"
	cd operator && docker build -t multiprox-operator:e2e .
	@echo "==> building stub PVE node image"
	cd $(E2E_DIR)/stub-node && docker build -t multiprox-node:e2e-stub .
	@echo "==> loading images into $(E2E_CLUSTER)"
	kind load docker-image multiprox-operator:e2e --name $(E2E_CLUSTER)
	kind load docker-image multiprox-node:e2e-stub --name $(E2E_CLUSTER)

e2e-install:
	kubectl --context $(E2E_CTX) apply -f config/namespace.yaml
	kubectl --context $(E2E_CTX) apply -f config/crd/
	kubectl --context $(E2E_CTX) apply -f config/rbac/
	kubectl --context $(E2E_CTX) apply -f config/manager/deployment.yaml
	kubectl --context $(E2E_CTX) -n $(NAMESPACE) set image \
	  deployment/multiprox-operator manager=multiprox-operator:e2e
	kubectl --context $(E2E_CTX) -n $(NAMESPACE) patch deployment multiprox-operator \
	  --type=json -p='[{"op":"replace","path":"/spec/template/spec/containers/0/imagePullPolicy","value":"Never"}]'
	kubectl --context $(E2E_CTX) -n $(NAMESPACE) rollout status \
	  deployment/multiprox-operator --timeout=180s

e2e-run:
	$(E2E_DIR)/run-e2e.sh

# Deletes ONLY the e2e cluster, by name.
e2e-teardown:
	kind delete cluster --name $(E2E_CLUSTER)

# ── Help ──────────────────────────────────────────────────────────────────────

help:
	@echo ""
	@echo "  operator-build      Build the operator Docker image"
	@echo "  node-build          Build the PVE node Docker image"
	@echo "  install             Apply CRD + RBAC with kubectl"
	@echo "  deploy              Deploy the operator Deployment"
	@echo "  helm-install        Install everything via Helm"
	@echo "  helm-uninstall      Remove Helm release"
	@echo "  status              Show ProxmoxCluster resources + operator pods"
	@echo "  logs                Tail operator logs"
	@echo "  manifests           Re-generate CRD + RBAC (needs controller-gen)"
	@echo ""
	@echo "  test                Unit tests (no cluster needed, <1s)"
	@echo "  test-cover          Unit tests with a coverage report"
	@echo "  check               fmt + vet + test (CI-friendly, no cluster)"
	@echo ""
	@echo "  e2e                 Full e2e: create kind cluster, build, install, test"
	@echo "  e2e-run             Re-run tests against an existing e2e cluster"
	@echo "  e2e-teardown        Delete the multiprox-e2e kind cluster only"
	@echo ""
