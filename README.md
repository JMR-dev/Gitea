# Gitea on GCE — podman quadlets, Pulumi, Cloud Build

A self-hosted [Gitea](https://gitea.com) instance on a single Google Compute
Engine VM, served at `gitea.jasonmross.dev`.

| Layer | Choice |
|---|---|
| Host | AlmaLinux 10 (`almalinux-cloud/almalinux-10`), Shielded VM |
| Size | `e2-small` (2 shared vCPU, 2 GB RAM) in `us-east1`, 20 GB boot + 30 GB pd-balanced data |
| Runtime | Podman **quadlets** — systemd `.container`/`.network`/`.volume` units, not compose |
| Images | Gitea: our own rootless image on **Alpine 3.24**, floating on the newest **Gitea 28.x** (GPG-verified release binary). Caddy: **Debian 13 (trixie)** |
| TLS | Caddy with ACME **DNS-01** via Google Cloud DNS (custom `xcaddy` build) |
| WAF | **Coraza** + OWASP CRS compiled into Caddy, feeding fail2ban → nftables |
| Database | SQLite in WAL mode on a dedicated persistent disk |
| Firewall | nftables + fail2ban on the host, VPC firewall outside it |
| IaC | Pulumi, Go, self-managed GCS state backend |
| CI/CD | Cloud Build in its own project (`cicd/`): Gitea webhook → relay → Pub/Sub triggers, with backups, checks and automatic rollback; off until its drills pass ([cicd.md](docs/cicd.md)) |
| Gitea Actions | Optional runners on a dedicated GKE Standard cluster: a fixed on-demand base node for the runners, and each `ubuntu-*` job its own pod on autoscaled burst nodes; `runs-on: docker` gets rootless Docker-in-Docker, and `runs-on: docker-arm64` native arm64 builds on Spot Axion nodes scaled from zero — [docs/runners.md](docs/runners.md) |

## Layout

```
infra/        Pulumi program (Go). One package per slice of infrastructure;
              pkg/gke and pkg/runner are the optional Actions runner cluster.
image/        Dockerfiles for the Gitea and Caddy images, plus pinned versions.
vm/           Everything that lands on the VM: quadlets, systemd units,
              nftables, fail2ban, config templates, and bootstrap.sh.
cicd/         CI/CD for this repo, in a GCP project of its own: Pulumi program
              (Go) and the webhook relay functions (cicd/relay).
cloudbuild/   The two build pipelines.
scripts/      One-time project bootstrap; ops/ is the release engine, cicd/ the
              CI project's bootstrap, deploy script and release gate.
docs/         Runbook, CI/CD, WAF tuning guide, Actions runners, migration-to-Gitea-SCM plan.
```

`vm/` is uploaded to a GCS bucket by Pulumi and pulled down by the instance, so
changing a quadlet is a normal pull request.

## First-time setup

Pulumi cannot create the bucket holding its own state, the identity that runs
it, or Gitea's signing secrets — so there is one manual step first.

```bash
# 1. Project bootstrap: APIs, state bucket, Pulumi runner SA, Gitea secrets.
scripts/bootstrap.sh <project-id> us-east1

# 2. Install the Cloud Build GitHub App on this repository, note the
#    installation id, and store a PAT (repo + read:user scope):
printf %s '<token>' | gcloud secrets versions add github-pat --data-file=- --project <project-id>

# 3. Confirm the Cloud DNS zone is authoritative. DNS-01 cannot work otherwise.
dig NS gitea.jasonmross.dev

# 4. The ACME contact address. Kept in Secret Manager, not stack config, so it
#    stays out of this public repo; the VM reads it when rendering the Caddyfile.
printf %s 'you@example.com' | gcloud secrets versions add gitea-acme-email --data-file=- --project <project-id>

# 5. Configure and apply. Pulumi's GCS backend and Google provider use
#    Application Default Credentials, not your gcloud login.
gcloud auth application-default login
# Use the passphrase bootstrap.sh generated. Letting `stack init` prompt for a
# new one encrypts the stack with a key Cloud Build's infra trigger never sees.
export PULUMI_CONFIG_PASSPHRASE=$(gcloud secrets versions access latest \
    --secret=pulumi-config-passphrase --project <project-id>)
cd infra
pulumi login gs://<project-id>-pulumi-state
pulumi stack init prod
pulumi config set gcp:project <project-id>
pulumi config set gitea:domain gitea.jasonmross.dev
pulumi config set gitea:dnsZone <cloud-dns-managed-zone-name>   # gcloud dns managed-zones list
pulumi config set gitea:githubOwner <owner>
pulumi config set gitea:githubAppInstallationId <id>
pulumi config set gitea:infraBuildServiceAccount cb-infra@<project-id>.iam.gserviceaccount.com
# WAF starts in DetectionOnly. Tune, then switch to On -- see docs/waf.md.
pulumi config set gitea:wafMode DetectionOnly
pulumi up

# 6. First image build. Until this runs, the :prod images do not exist.
cd .. && make build

# 7. Create the admin user.
make ssh
sudo podman exec -u 1000 gitea gitea admin user create \
    -c /etc/gitea/app.ini --admin --username <you> --email <you@example.com> --random-password

# 7. Optional: Gitea Actions runners on GKE. Read docs/runners.md first --
#    it covers the threat model, the cost, and the two-step teardown.
cd infra && pulumi config set gitea:runnersEnabled true && pulumi up
```

### Expected on the first run, not a bug

Between step 5 and step 6 the `:prod` images do not exist yet, so `gitea.service`
and `caddy.service` crash-loop. That is intentional: the units carry
`Restart=always` with `StartLimitIntervalSec=0`, so they recover on their own
within 30 seconds of the first successful push. Likewise, `app.ini` is not
rendered until Gitea's secrets are readable — `bootstrap.sh` skips rendering
rather than writing a config with empty signing keys.

## Day-to-day

```bash
make status     # services, containers, timers
make logs       # tail gitea + caddy
make build      # build + push :<sha> images (does NOT go live)
make release GITEA=<sha>   # go live: backup, checks, automatic rollback
make ops-backup # dump + config tarball + exports + data-disk snapshot
make ops-verify # the release status checks, against prod
make rollout    # pull the latest :prod images now
make sync       # re-render VM config after a vm/ change
make ssh        # shell via IAP
```

The targets read the project and zone from the Pulumi stack, which needs
Application Default Credentials (`gcloud auth application-default login`).
Without them `pulumi config get` fails quietly and gcloud runs with an empty
`--project=`; pass `PROJECT=<project-id>` to skip the lookup.

Only `make release` (`scripts/ops/gitea_ops.py`) moves `:prod`. It takes the
backups, migrates explicitly when the Gitea version changes, runs the status
checks, and rolls back, data included, if they fail. See *Releases* in
[docs/runbook.md](docs/runbook.md).

## How updates happen

Three independent layers, because no single one covers everything:

1. **OS packages** — `dnf5-automatic` applies updates nightly. It never reboots;
   `gitea-reboot-window.timer` does that weekly, and only when
   `needs-restarting -r` says a reboot is genuinely required.
2. **Container images** — `podman-auto-update.timer` polls the `:prod` tag daily.
   `Notify=healthy` on the quadlets means systemd withholds "started" until the
   healthcheck passes, which is what arms podman's automatic rollback.
3. **Image contents** — a Cloud Scheduler job re-runs the image build every
   Sunday. It rebuilds Gitea from a floating `alpine:3.24` on the newest
   Gitea 28.x, and Caddy from a floating `debian:13-slim`, so base-OS, Gitea
   and Go security fixes reach the running containers. Without this, `:prod`
   never changes and layer 2 has nothing to pull.

`image/gitea.version` holds only the Gitea **major** (`28`) and
`image/alpine.version` the Alpine **minor** (`3.24`). Patches float; changing
either file is a deliberate PR, and a release across one needs
`--allow-major` / `--allow-alpine-minor`. Alpine minors are pinned because
they are known to break things (3.14's musl `faccessat2` change broke
containers on older runtimes). The Caddy version stays a deliberate change to
`image/caddy.version`.

> **Fire the weekly rebuild once by hand after the first deploy.** A broken
> scheduler request fails silently at 04:00 on a Sunday and stops layer 3
> from feeding layer 2. See *The weekly rebuild* in
> [docs/runbook.md](docs/runbook.md).

## Things worth knowing before you change something

- **The `:prod` tag is load-bearing.** `AutoUpdate=registry` compares digests
  *for a tag*. Pinning a digest in the quadlet silently disables auto-updates.
  Only `gitea_ops.py release` moves it. A Gitea version change must never
  reach the VM through auto-update: its rollback would put the old image back
  on a database the new one already migrated, and the old one refuses to start.
- **The Gitea image needs `bash`.** Gitea's server-side git hooks are
  `#!/usr/bin/env bash`; without it every push is rejected. Its
  `rsync`/`unzip`/`sqlite3` are the restore toolbox, because the host has none.
- **`app.ini` is fully managed** and `INSTALL_LOCK=true`. Gitea settings changed
  in the web UI that map to `app.ini` will not survive a config sync. Edit
  `vm/config/app.ini.tmpl` instead.
- **The podman subnet is pinned** (`10.89.10.0/24`). It is what
  `REVERSE_PROXY_TRUSTED_PROXIES` names; an unpinned subnet would silently make
  fail2ban ban Caddy instead of the attacker.
- **Never put `flush ruleset` in the nftables config.** It would wipe netavark's
  rules and break all container networking on reload.
- **The DNS zone, the backup bucket, and the Gitea secrets are not Pulumi-owned**
  by design, so `pulumi destroy` cannot take them with it.
- **Git and LFS deliberately bypass the WAF.** Remove that bypass and `git push`
  returns 403 — verified, not theoretical. See [docs/waf.md](docs/waf.md).
- **So do the Gitea Actions runner routes** (`/api/actions/`,
  `/api/actions_pipeline/`, the v4 artifact `twirp` service). The runner speaks
  protobuf over Connect, which CRS rejects in `On` mode. See
  [docs/runners.md](docs/runners.md).
- **The runner NetworkPolicies are load-bearing.** `docker` jobs share the DinD
  runner pod's network namespace, so blocking `169.254.0.0/16` is the only
  thing standing between a workflow and that runner's Workload Identity token.
  The same block keeps job pods in `gitea-jobs` off the metadata server and the
  API server.
- **Workflows that need Docker must say `runs-on: docker`** (or `docker-arm64`
  to build for arm64). `ubuntu-*` jobs run
  as plain pods with no Docker daemon, so `docker build`, Docker actions and
  `docker://` steps fail there.
- **Turning runners off is two applies**: `gitea:runnerReplicas 0`,
  `gitea:runnerJobCapacity 0` and `gitea:runnerArmMaxNodes 0` first, so the PVC disks are deleted, then
  `gitea:runnersEnabled false`. Deleting the cluster alone orphans billable
  disks.
- **`image/caddy.version` and `image/coraza.version` are coupled.** coraza-caddy
  pins a minimum Caddy version; bump them together or the build fails.
- **`us-east1` has no `-a` zone** (it is b/c/d). The stack pins `us-east1-b`.
- **2 GB of RAM is the real constraint**, not disk or CPU. A 2 GB swap file is
  provisioned as ballast; sustained swap use means move to `e2-medium`. Measured
  numbers are in [docs/runbook.md](docs/runbook.md).

See [docs/runbook.md](docs/runbook.md) for verification drills, restores, and
rollbacks, and [docs/waf.md](docs/waf.md) for WAF tuning and the
`DetectionOnly` → `On` rollout.
