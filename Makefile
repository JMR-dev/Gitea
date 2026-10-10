# Convenience wrappers. Everything here is also runnable by hand; nothing in the
# deployment depends on make.

PROJECT ?= $(shell cd infra && pulumi config get gcp:project 2>/dev/null)
REGION  ?= $(shell cd infra && pulumi config get gcp:region 2>/dev/null || echo us-east1)
# -b, matching the default in infra/pkg/config: us-east1 has no -a zone.
ZONE    ?= $(shell cd infra && pulumi config get gitea:zone 2>/dev/null || echo $(REGION)-b)
VM      ?= gitea-vm

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
	bash -n vm/bootstrap.sh scripts/bootstrap.sh
	@command -v shellcheck >/dev/null && shellcheck -S warning vm/bootstrap.sh scripts/bootstrap.sh || echo "shellcheck not installed -- skipped"

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
# SHORT_SHA: Cloud Build fills it in only for triggered builds. For `builds
#   submit` it is empty, and image.yaml's `--tag <image>:$SHORT_SHA` becomes an
#   invalid reference that fails the build.
.PHONY: build
build: ## Build and roll out the container images via Cloud Build
	gcloud builds submit --config cloudbuild/image.yaml --project $(PROJECT) \
		--region=$(REGION) \
		--service-account=projects/$(PROJECT)/serviceAccounts/cb-image@$(PROJECT).iam.gserviceaccount.com \
		--substitutions=_REGION=$(REGION),_ZONE=$(ZONE),SHORT_SHA=$(shell git rev-parse --short=7 HEAD)

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
