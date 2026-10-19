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
	python3 -m py_compile scripts/ops/gitea_ops.py
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

# The release engine (scripts/ops/gitea_ops.py). Every target that changes
# something passes --yes; add OPS_ARGS=--dry-run to see the plan instead, or
# any other global flag (e.g. OPS_ARGS="--state-bucket <bucket>").
OPS     = python3 scripts/ops/gitea_ops.py --project $(PROJECT) --region $(REGION) --zone $(ZONE) $(OPS_ARGS)
GITEA  ?= $(shell git rev-parse --short=7 HEAD)
CADDY  ?= $(GITEA)

.PHONY: release
release: ## Put built images live: backup, checks, automatic rollback (GITEA=<tag> CADDY=<tag>|none RELEASE_ARGS=...)
	$(OPS) --yes release --gitea $(GITEA) $(if $(filter-out none,$(CADDY)),--caddy $(CADDY)) $(RELEASE_ARGS)

.PHONY: ops-backup
ops-backup: ## Full backup: gitea dump, config tarball, exports, data-disk snapshot
	$(OPS) --pulumi-dir infra --yes backup $(BACKUP_ARGS)

.PHONY: ops-verify
ops-verify: ## Status checks against prod (MANIFEST=gs://.../manifest.json to compare)
	$(OPS) verify $(if $(MANIFEST),--manifest $(MANIFEST))

.PHONY: ops-rollback
ops-rollback: ## Undo a release from its manifest (MANIFEST=...; RESTORE_DATA=1 restores /var/lib/gitea; TOOL_IMAGE= an image with rsync)
	$(OPS) --yes rollback --manifest $(MANIFEST) $(if $(RESTORE_DATA),--restore-data) $(if $(TOOL_IMAGE),--tool-image $(TOOL_IMAGE))

.PHONY: ops-drill-restore
ops-drill-restore: ## Prove the latest snapshot and dump restore, in isolation (TOOL_IMAGE=... before 28)
	$(OPS) --yes drill-restore $(if $(TOOL_IMAGE),--tool-image $(TOOL_IMAGE))

.PHONY: rollout
rollout: ## Pull the latest :prod images onto the VM right now
	gcloud compute ssh $(VM) --zone=$(ZONE) --tunnel-through-iap --project=$(PROJECT) \
		--command 'sudo systemctl start podman-auto-update.service && sudo podman ps'

.PHONY: sync
sync: ## Re-render VM config from the bucket and restart what changed
	gcloud compute ssh $(VM) --zone=$(ZONE) --tunnel-through-iap --project=$(PROJECT) \
		--command 'sudo systemctl start gitea-config-sync.service && sudo journalctl -u gitea-config-sync -n 40 --no-pager'

.PHONY: ssh
ssh: ## Shell on the VM through IAP
	gcloud compute ssh $(VM) --zone=$(ZONE) --tunnel-through-iap --project=$(PROJECT)

.PHONY: logs
logs: ## Tail Gitea and Caddy logs
	gcloud compute ssh $(VM) --zone=$(ZONE) --tunnel-through-iap --project=$(PROJECT) \
		--command 'sudo journalctl -u gitea -u caddy -f'

.PHONY: status
status: ## Health summary from the VM
	gcloud compute ssh $(VM) --zone=$(ZONE) --tunnel-through-iap --project=$(PROJECT) \
		--command 'sudo systemctl status --no-pager gitea caddy nftables fail2ban; sudo podman ps; sudo systemctl list-timers --no-pager'

.PHONY: backup
backup: ## Take an on-demand Gitea dump to the backup bucket
	gcloud compute ssh $(VM) --zone=$(ZONE) --tunnel-through-iap --project=$(PROJECT) \
		--command 'sudo systemctl start gitea-backup.service && sudo journalctl -u gitea-backup -n 20 --no-pager'

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
