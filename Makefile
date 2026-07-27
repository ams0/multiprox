##############################################################################
# multiprox — root Makefile
#
# Delegates to docker/ and kubernetes/ sub-projects.
# Run 'make help' for the full target list.
##############################################################################

.PHONY: help \
        docker-build docker-up docker-down docker-clean \
        docker-cluster-init docker-cluster-join docker-cluster-full \
        docker-status docker-shell docker-logs \
        k8s-install k8s-uninstall k8s-deploy k8s-delete \
        k8s-operator-build k8s-status k8s-logs \
        k8s-helm-install k8s-helm-uninstall

DOCKER_MAKE := $(MAKE) -C docker
K8S_MAKE    := $(MAKE) -C kubernetes

# ── Docker cluster ────────────────────────────────────────────────────────────

docker-build:
	$(DOCKER_MAKE) build

docker-up:
	$(DOCKER_MAKE) up

docker-down:
	$(DOCKER_MAKE) down

docker-clean:
	$(DOCKER_MAKE) clean

docker-cluster-init:
	$(DOCKER_MAKE) cluster-init

docker-cluster-join:
	$(DOCKER_MAKE) cluster-join

docker-cluster-full:
	$(DOCKER_MAKE) cluster-full

docker-status:
	$(DOCKER_MAKE) status

docker-shell:
	$(DOCKER_MAKE) shell N=$(N)

docker-logs:
	$(DOCKER_MAKE) logs N=$(N)

# ── Kubernetes operator ───────────────────────────────────────────────────────

k8s-operator-build:
	$(K8S_MAKE) operator-build

k8s-install:
	$(K8S_MAKE) install

k8s-uninstall:
	$(K8S_MAKE) uninstall

k8s-deploy:
	$(K8S_MAKE) deploy

k8s-delete:
	$(K8S_MAKE) delete

k8s-helm-install:
	$(K8S_MAKE) helm-install

k8s-helm-uninstall:
	$(K8S_MAKE) helm-uninstall

k8s-status:
	$(K8S_MAKE) status

k8s-logs:
	$(K8S_MAKE) logs

# ── Help ──────────────────────────────────────────────────────────────────────

help:
	@echo ""
	@echo "multiprox — Proxmox cluster scaffold"
	@echo ""
	@echo "Docker targets (prefix: docker-)"
	@echo "  docker-build          Build PVE node image"
	@echo "  docker-up             Start 3-node cluster"
	@echo "  docker-cluster-full   Init + join cluster"
	@echo "  docker-status         pvecm status"
	@echo "  docker-clean          Destroy containers + volumes"
	@echo ""
	@echo "Kubernetes targets (prefix: k8s-)"
	@echo "  k8s-install           Apply CRDs + RBAC"
	@echo "  k8s-deploy            Deploy the operator"
	@echo "  k8s-helm-install      Install via Helm"
	@echo "  k8s-status            Show ProxmoxCluster resources"
	@echo "  k8s-logs              Tail operator logs"
	@echo "  k8s-uninstall         Remove CRDs + RBAC"
	@echo "  k8s-delete            Delete operator deployment"
	@echo ""
