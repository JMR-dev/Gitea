# Convenience wrappers. Everything here is also runnable by hand; nothing in the
# deployment depends on make.

PROJECT ?= $(shell cd infra && pulumi config get gcp:project 2>/dev/null)
REGION  ?= $(shell cd infra && pulumi config get gcp:region 2>/dev/null || echo us-east1)
# -b, matching the default in infra/pkg/config: us-east1 has no -a zone.
ZONE    ?= $(shell cd infra && pulumi config get gitea:zone 2>/dev/null || echo $(REGION)-b)
VM      ?= gitea-vm
# The GKE runner cluster (docs/runners.md). kubectl targets name the context
# explicitly, so they never act on whatever cluster happens to be current.
RUNNER_CLUSTER ?= gitea-runners
KUBE_CONTEXT   ?= gke_$(PROJECT)_$(ZONE)_$(RUNNER_CLUSTER)

.PHONY: help
help:
	@grep -E '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) \
		| awk 'BEGIN {FS = ":.*?## "}; {printf "  \033[36m%-16s\033[0m %s\n", $$1, $$2}'

.PHONY: bootstrap
bootstrap: ## One-time project setup (run before the first `make up`)
	scripts/bootstrap.sh $(PROJECT) $(REGION)

.PHONY: fmt
fmt: ## Format Go sources
	cd infra && gofmt -w .

# -o gitea-infra: Pulumi.yaml points the go runtime at this prebuilt binary, so
#   Pulumi runs it rather than compiling. Without it preview/up fail outright.
.PHONY: check
check: ## Build and vet the Pulumi program, and syntax-check the shell scripts
	cd infra && go build -o gitea-infra . && go vet ./...
	bash -n vm/bootstrap.sh scripts/bootstrap.sh scripts/ops/vm-steps.sh
	python3 -m py_compile scripts/ops/gitea_ops.py scripts/ops/launch_job.py
	@command -v shellcheck >/dev/null && shellcheck -S warning vm/bootstrap.sh scripts/bootstrap.sh scripts/ops/vm-steps.sh || echo "shellcheck not installed -- skipped"

.PHONY: preview
preview: check ## Show what `pulumi up` would change
	cd infra && pulumi preview

.PHONY: up
up: check ## Apply the infrastructure
	cd infra && pulumi up

# --region: the triggers are 2nd-gen and therefore regional. Without this flag
#   `builds submit` runs in the global region -- a different worker pool from
#   every automated build.
# --service-account: the build's last step reaches the VM over an IAP tunnel,
#   needing iap.tunnelResourceAccessor + compute.osAdminLogin. Those are granted
#   to cb-image@, not to whatever default Cloud Build account this project
#   happens to have -- and on newer projects the legacy default does not exist.
#   Without this the images push fine and the rollout step fails.
# --gcs-source-staging-dir: running as cb-image@, the build must be able to read
#   the uploaded source. Pulumi grants that on this bucket only; the default
#   <project>_cloudbuild bucket is unreadable to it and the build fails at
#   "could not resolve source".
# SHORT_SHA: Cloud Build fills it in only for triggered builds. For `builds
#   submit` it is empty, and image.yaml's `--tag <image>:$SHORT_SHA` becomes an
#   invalid reference that fails the build.
#
# This builds and pushes :$SHORT_SHA and the version tags, NOT :prod. Putting
# an image live is `make release` -- see scripts/ops/gitea_ops.py.
PUSH ?= true
.PHONY: build
build: ## Build and push the images via Cloud Build (does not touch :prod)
	gcloud builds submit --config cloudbuild/image.yaml --project $(PROJECT) \
		--region=$(REGION) \
		--service-account=projects/$(PROJECT)/serviceAccounts/cb-image@$(PROJECT).iam.gserviceaccount.com \
		--gcs-source-staging-dir=gs://$(PROJECT)-gitea-build-source/source \
		--substitutions=_REGION=$(REGION),_PUSH=$(PUSH),SHORT_SHA=$(shell git rev-parse --short=7 HEAD)

# The release engine (scripts/ops/gitea_ops.py). By default every target runs
# it as a Kubernetes Job on the runner cluster's ops pool (infra/pkg/ops,
# scripts/ops/launch_job.py), NOT here: its automatic rollback lives in the same
# process as the release, so a laptop that sleeps mid-release would leave Gitea
# stopped. Closing this terminal only stops the log; `make ops-logs` re-attaches.
#
# RUN_ON=local runs it on this machine instead -- break-glass only, for when
# the cluster itself is the problem. Keep the machine awake and use tmux.
#
# Targets that change something pass --yes; OPS_ARGS=--dry-run shows the plan.
RUN_ON ?= gke
ifeq ($(RUN_ON),local)
OPS = python3 scripts/ops/gitea_ops.py --project $(PROJECT) --region $(REGION) --zone $(ZONE) $(OPS_ARGS)
else
OPS = python3 scripts/ops/launch_job.py --project $(PROJECT) --zone $(ZONE) run -- \
	--region $(REGION) --zone $(ZONE) $(OPS_ARGS)
endif
GITEA  ?= $(shell git rev-parse --short=7 HEAD)
CADDY  ?= $(GITEA)

.PHONY: release
release: ## Put built images live: backup, checks, automatic rollback (GITEA=<tag> CADDY=<tag>|none RELEASE_ARGS=...)
	$(OPS) --yes release --gitea $(GITEA) $(if $(filter-out none,$(CADDY)),--caddy $(CADDY)) $(RELEASE_ARGS)

.PHONY: ops-backup
ops-backup: ## Full backup: gitea dump, config tarball, exports, data-disk snapshot
	$(OPS) $(if $(filter local,$(RUN_ON)),--pulumi-dir infra) --yes backup $(BACKUP_ARGS)

.PHONY: ops-verify
ops-verify: ## Status checks against prod (MANIFEST=gs://.../manifest.json to compare)
	$(OPS) verify $(if $(MANIFEST),--manifest $(MANIFEST))

.PHONY: ops-rollback
ops-rollback: ## Undo a release from its manifest (MANIFEST=...; RESTORE_DATA=1 restores /var/lib/gitea; TOOL_IMAGE= an image with rsync)
	$(OPS) --yes rollback --manifest $(MANIFEST) $(if $(RESTORE_DATA),--restore-data) $(if $(TOOL_IMAGE),--tool-image $(TOOL_IMAGE))

.PHONY: ops-drill-restore
ops-drill-restore: ## Prove the latest snapshot and dump restore, in isolation (TOOL_IMAGE=... before 28)
	$(OPS) --yes drill-restore $(if $(TOOL_IMAGE),--tool-image $(TOOL_IMAGE))

.PHONY: ops-logs
ops-logs: ## Re-attach to the running (or last) release-engine Job's log
	python3 scripts/ops/launch_job.py --project $(PROJECT) --zone $(ZONE) logs

.PHONY: ops-status
ops-status: ## One line about the current release-engine Job
	python3 scripts/ops/launch_job.py --project $(PROJECT) --zone $(ZONE) status

# --dns-endpoint: the runner cluster has no IP endpoint at all; access is IAM
# only (roles/container.developer or above on the project).
.PHONY: kube
kube: ## Fetch kubectl credentials for the GKE runner cluster
	gcloud container clusters get-credentials $(RUNNER_CLUSTER) --zone=$(ZONE) \
		--dns-endpoint --project=$(PROJECT)

.PHONY: runners
runners: ## Runner and job pods, disks, and recent runner logs (run `make kube` first)
	kubectl --context=$(KUBE_CONTEXT) get nodes
	kubectl --context=$(KUBE_CONTEXT) -n gitea-runner get pods,pvc -o wide
	kubectl --context=$(KUBE_CONTEXT) -n gitea-jobs get pods -o wide
	kubectl --context=$(KUBE_CONTEXT) -n gitea-runner logs statefulset/gitea-runner --tail=30
	kubectl --context=$(KUBE_CONTEXT) -n gitea-runner logs statefulset/gitea-runner-pods --tail=30
