# CI/CD for Gitea itself

This repo's images, VM and `infra/` stack deploy from a GCP project of their
own, `gitea-ci-781388`, not from the app project `gitea-496920`. CI for the
repos Gitea *hosts* is unaffected. It stays on Gitea Actions and the GKE
runners.

**Status:** live since 2026-10-23: drills D1–D6 passed and `autoDeploy = true`,
so merges to main deploy (below).

## How it fits together

```
Gitea webhook --HMAC--> gitea-webhook fn (public) --> Pub/Sub ci-preview | ci-deploy | ci-gated
Cloud Scheduler (Sun 04:00 UTC) ---------------------------> Pub/Sub ci-rebuild
   each topic --> its Cloud Build trigger (only ci-gated waits for approval)
                    launcher: clone the commit from Gitea, check a deploy is on main,
                              gcloud builds submit cloudbuild/infra.yaml, then image.yaml
cloud-builds topic --push, OIDC--> build-status fn (private) --> Gitea commit status
rollback / failure log lines --> log-match alerts --> e-mail to the operator
```

| Piece | Where |
|---|---|
| Project, state bucket, passphrase and webhook secrets | `scripts/cicd/bootstrap.py` (once, as you) |
| Everything else in the CI project | `cicd/` (Pulumi Go), applied with `make ci-up`, never from CI |
| Webhook relay and build-status functions | `cicd/relay/` (own Go module) |
| Infra pipeline | `cloudbuild/infra.yaml` → `scripts/cicd/infra_deploy.py` |
| Image pipeline and release | `cloudbuild/image.yaml` → the release-engine Job |
| Release gate | `scripts/cicd/release_gate.py` |

**A release never runs in Cloud Build.** The image pipeline launches the same
Kubernetes Job a person's `make release` does (`scripts/ops/launch_job.py`), on
the runner cluster's ops pool, as `gitea-release@`. A build that dies can't
take the rollback down with it, and every release runs as the one identity
whose permissions have been proven for real.

### Identities (CI project)

| Account | Runs | Holds |
|---|---|---|
| `cb-launcher` | the deploy, gated and rebuild triggers' own build | submit builds as `cb-infra` and `cb-image`, the source bucket |
| `cb-infra` | `cloudbuild/infra.yaml` for main | the app project roles Pulumi needs (`cicd/pkg/identity`) |
| `cb-image` | `cloudbuild/image.yaml` for main | push to AR `gitea`, see the cluster, start Jobs in `gitea-ops` (via `gitea:opsLaunchers`), read and add (never replace) drill passes |
| `cb-launcher-preview` | the preview trigger's own build | submit builds as the two below **only**, the source bucket |
| `cb-infra-preview` | a PR's `cloudbuild/infra.yaml` | `roles/dns.reader` + `roles/browser` on the app project, read the app stack's state, its passphrase, read drill passes |
| `cb-image-preview` | a PR's `cloudbuild/image.yaml` | logs and the source bucket; nothing in the app project |
| `ci-relay`, `ci-status` | the two functions | their topics, the webhook secret and the status token |
| `ci-status-push` | the push subscription | invoke `build-status`, nothing else |

**A PR preview runs code nobody has reviewed yet**, so it runs as accounts
that can't change prod:
- `pulumi preview` executes the branch's program, and the pipeline file comes
  from the branch too. A branch that edits its pipeline file to run `pulumi up`
  gets a failed build, not a deploy.
- The preview trigger's launcher can act only as the two `-preview` accounts.
  So nothing in the branch, or in the launcher's own script, can start a build
  as `cb-infra` or `cb-image`.
- `autoDeploy = false` doesn't pause previews, and doesn't need to.

**Read-only isn't secret-free.** A preview has to decrypt the stack's config to
diff it, so `cb-infra-preview` holds the app stack's passphrase and can read the
state bucket:
- It can read the stack's secrets. Today that is `gitea:fail2banIgnore` (the
  operator's home IP, which also appears as the VM's `runner-egress-ip`
  metadata in state). The Kubernetes provider no longer stores a credential:
  its kubeconfig runs the `gke-gcloud-auth-plugin` exec plugin, which mints a
  token from the *caller's* own credentials at connect time
  (`infra/pkg/runner/runner.go`), so a reader of the state has no deployer token
  to replay.
- Project-wide it has only `roles/dns.reader` and `roles/browser`: the DNS zone,
  and project and IAM metadata (role and member names). It can read no bucket
  object, no secret value, no VM metadata and no Kubernetes Secret -- in
  particular not the backups bucket, which holds the Gitea database and the
  server secrets. (`roles/viewer` would have granted bucket-object read through
  the `projectViewer:` convenience member, hence the narrower roles.)

That is why the relay still previews only same-repo branches whose **author and
pusher** are both in `gitea-cicd:allowedAuthors`. Keep that list to people you'd
trust with the home IP.

## What deploys, and when

The policy lives in `Classify` in `cicd/relay/webhook.go`, with table tests.

| Event | Runs | Approval |
|---|---|---|
| PR opened, synchronized or reopened (same repo, allowed author **and** pusher) | `pulumi preview`; image build without push | no |
| push to main touching `infra/`, `vm/`, `cloudbuild/infra.yaml` or `scripts/cicd/infra_deploy.py` | infra deploy | no |
| push to main touching `image/` or `cloudbuild/image.yaml` | image build and release | no |
| push to main touching `scripts/cicd/release_gate.py` (the gate both pipelines run) | infra deploy **and** image build and release | no |
| push to main touching `image/gitea.version`, `image/alpine.version` or `vm/bootstrap.sh` | as above | **yes** (`make approve`) |
| push whose commit list Gitea truncated (more than 15 commits) | everything | **yes** |
| anything else (docs, `cicd/`, `scripts/ops/`, `scripts/cicd/bootstrap.py` and tests) | nothing; status "nothing to deploy" | n/a |
| Sunday 04:00 UTC | image rebuild from main and release | no |

- The version files are what make a Gitea major or Alpine minor change.
- A `scripts/cicd/` change that drives a deploy (`infra_deploy.py`,
  `release_gate.py`) deploys through that file's pipeline on merge. Because it
  is release code, re-run its drill from the branch first (so the merge's
  deploy passes the gate); the merge's pending `cloudbuild/drill-*` status
  marks it due. `bootstrap.py` and the tests drive no deploy.
- The VM is configured by the playbook in `vm/ansible`, which the infra deploy
  runs over IAP SSH after `pulumi up`; a `vm/` change deploys through the infra
  pipeline. Until 2026-10-26 `vm/bootstrap.sh` was the VM's startup script, so
  editing it replaced the VM; the relay still gates that path.
- A change that replaces the VM (machine type, boot image, anything Pulumi
  cannot update in place) is about 20–30 min down and a new Let's Encrypt
  certificate: the deploy deletes the old VM, creates the new one, waits for
  SSH and boot, runs the playbook, then waits for HTTPS before verify.
- Even without approval, the infra pipeline refuses to replace or delete the
  VM, a disk, a bucket, the cluster, an address or a DNS record.
- The engine refuses a major or Alpine-minor change without `--allow-*`, which
  only an approved build passes.

**Approving** a build covers the whole build:

```bash
make approve                 # lists pending builds
make approve BUILD=<id>      # gcloud beta builds approve (there is no GA command)
```

### Order and overlap

- Infra runs before image, and an image is never released after a failed
  infra deploy.
- Infra deploys take a lock, `gs://gitea-ci-781388-deploy-state/infra.lock`,
  and wait up to 45 minutes for one already running.
- Releases wait for a running engine Job.
- **A commit older than one already deployed is skipped**, so builds that
  finish out of order never put old code back:
  - for infra, by comparing the commit with last-good;
  - for the image, by asking Gitea, right before the release, whether a newer
    commit on main also changes the image.
  - An approved build is never skipped this way, because it may carry the
    version bump the newer one can't release.

## Safety net

**Infra deploy (`infra_deploy.py up`):**
1. Release gate.
2. Lock.
3. Superseded check.
4. Destructive-change guard.
5. Backup: the engine's backup Job (dump, config tarball, online snapshot) plus
   a `pulumi stack export` under `config/<stamp>-infra/`.
6. `pulumi up`, then the VM's config: the playbook (`vm/ansible`) for a tree
   that has it, the old `gitea-config-sync` for one with `vm/bootstrap.sh` --
   the tree being applied decides, so a rollback across the change works. A
   replaced VM is waited for (SSH, boot, then HTTPS after its config).
7. The engine's `verify` Job.
8. A preview that must show nothing left to change.
9. On success, the tree becomes **last-good**. On a failure in steps 6–8, the
   last-good tree is re-applied and `GITEA_INFRA_ROLLBACK` is logged.

This rollback is a re-apply, not an undo:
- A replaced VM is replaced again.
- Data is never restored automatically: `make ops-rollback` does that, by hand.

**Image release:** the engine's own path A or path B, with automatic rollback
(see [runbook.md](runbook.md)).

**Alerts** go to the operator by e-mail (`cicd/pkg/alerting`):
- an automatic rollback;
- a failed rollback (critical);
- any failed pipeline build.

## The release gate

Each release and infra deploy runs the rollback code of the commit being
deployed. An edit to that code is live at the next release, and once
automation is on, that can be the unattended weekly rebuild. So the gate makes
re-running the drills a check rather than a rule:

- The release-path files of each pipeline are hashed: the engine, its
  launcher, `vm-steps.sh`, the gate and the pipeline file.
- A passing drill records the hash under `gs://gitea-ci-781388-deploy-state/drills/`.
- An automatic release or infra deploy refuses code no drill has passed with.
- An approved build goes ahead with a warning, and `make release` never asks.
- A push that changes release code gets one pending status per drill it needs
  (`cloudbuild/drill-<pipeline>-<name>`). Each passing drill turns its own green.

## Drills: the greenlight gate

Run each drill from a clean checkout of the commit to attest
(`make ci-drill PIPELINE=... NAME=...`). Each one runs the real pipeline as
CI's identities, with a failure injected after a passing check. It passes only
if the rollback put prod back. Record the build IDs in the log at the bottom of
this page.

| | Drill | Proves | Cost |
|---|---|---|---|
| D1 | `make ops-backup` | dump non-trivial, snapshot READY, manifest, stack and IAM exports present | none |
| D2 | `make ops-drill-restore` | the latest snapshot and dump each boot an isolated Gitea with the same users and repos | none |
| D3 | `make ci-drill PIPELINE=image NAME=fail-verify` | image rollback: `:prod` back on `:previous`, old version healthy | a restart |
| D4 | `make ci-drill PIPELINE=image NAME=fail-after-migrate` | data restore: the marker written after the snapshot is gone, old digest back | **10–15 min down; run it in a window** |
| D5 | `make ci-infra-up` once (it creates last-good), then a harmless change on a branch (a bucket label) and `make ci-drill PIPELINE=infra NAME=fail-verify` | the last-good tree is re-applied; the label is gone, and the preview at last-good is clean | a config sync |
| D6 | D3–D5 above | each sends the rollback e-mail, and each failed check shows on Gitea | none |

D2 last ran by hand before the 28 upgrade (2026-10-20, PASS). D3, D4 and D5
have never run. The 28 release itself succeeded, so its automatic rollback
never ran either.

**Re-run D3–D5 whenever the release code changes.** The gate enforces this for
automatic runs, and the pending `cloudbuild/drill-*` statuses show what's due.
D4 needs a window, because it really stops Gitea and restores its disk. **Run
D4 before the next Gitea major or Alpine minor bump** regardless: that's the
path whose only way back is a data restore. Each D4 leaves the replaced tree
on the data disk as `/var/lib/gitea/.pre-restore-<stamp>`, a full copy of the
data; the engine's backup removes it once it is a week old
(`docs/runbook.md`, "Restore the data disk from a snapshot").

### Turning automation on, and off

Only after D1–D6 pass:

```bash
cd cicd && pulumi config set autoDeploy true && make ci-up
```

That enables `ci-deploy`, `ci-gated` and `ci-rebuild`, and un-pauses the weekly
schedule. Then fire the rebuild once by hand (`make ci-rebuild`). Expect path A,
a brief restart, and a success status on main.

`autoDeploy = false` is the kill switch. So is deleting the Gitea webhook.

## First-time setup

**Precondition.** The app stack must already carry the exec-plugin kubeconfig
(`infra/pkg/runner/runner.go`, branch `fix-runner-kubeconfig-exec`), merged and
applied with a `pulumi up`, *before* `make ci-up`. Until then the stack state
holds a deployer access token, and `cb-infra-preview` — which reads the state
and its passphrase — could replay it as a deployer. (That branch also needs the
`gke-gcloud-auth-plugin` on PATH wherever `make preview`/`make up` runs: install
`google-cloud-cli-gke-gcloud-auth-plugin` on the workstation **before merging**,
or local deploys break.) Confirm the applied state is token-free before creating
the preview accounts: after the `up`, `make preview` shows **no changes** (the
exec kubeconfig ends the per-run token drift that previously showed one update),
and the state's `pulumi:providers:kubernetes gitea-runners` no longer has a
secret `inputs.kubeconfig`.

1. `scripts/cicd/bootstrap.py --yes`: project, billing link, state bucket,
   secrets. Done 2026-10-20 (`gitea-ci-781388`).
2. Set the operator's address, as a secret:
   ```bash
   cd cicd && pulumi config set --secret gitea-cicd:operatorEmail <you>
   ```
3. Put a token into the status-token secret, straight from the password
   manager. The functions won't deploy without a version.
   - **Make it a bot user's token, not yours.** A Gitea 28 token's
     `write:repository` scope covers every repo its user can write, and never
     expires.
   - So create a bot user (say `ci-status`), add it to `JMR-dev/Gitea` only,
     as a collaborator with write access, and give its token
     `write:repository` alone. The bot's write access can't be turned into a
     preview of unreviewed code: the relay previews a PR only when both its
     author and its pusher are in `allowedAuthors`, and the bot is neither.
   ```bash
   gopass show -o <path> | gcloud secrets versions add gitea-ci-status-token \
     --project=gitea-ci-781388 --data-file=-
   ```
4. `make ci-init`, then `make ci-preview`, then `make ci-up`.
5. Let CI launch releases:
   ```bash
   cd infra && pulumi config set gitea:opsLaunchers "$(cd ../cicd && pulumi stack output imageServiceAccount)"
   ```
   Then a `pulumi up` on the app stack (it adds a Role and a RoleBinding in
   `gitea-ops`).
6. `GITEA_TOKEN=... scripts/cicd/bootstrap.py --yes hook`: the repo webhook.
   Send a test delivery from the hook's settings. A ping is answered 204.
7. Open a PR (every PR previews both pipelines): the preview's statuses
   appear on it. Then check:
   - both preview builds ran as `cb-infra-preview` and `cb-image-preview`
     (`gcloud builds list --project=gitea-ci-781388 --format='value(id,serviceAccount)' --limit=3`);
   - the infra preview got through the runner cluster's objects (the
     client-side fallback, under Known limits);
   - a delivery with a bad signature gets 401 (`curl -X POST -H 'X-Gitea-Event: push' -d '{}' <webhookUrl>`);
   - `build-status` without a token gets 403.
8. The drills, then automation (above).

## Break-glass

Everything still works by hand, as before:
- `make release`, `make ops-rollback`;
- `cd infra && pulumi up`, `make vm-apply` (`make up` runs both);
- `make build` (in the app project).

`make ci-infra-up` runs the infra pipeline from your checkout as CI's identity,
as an approved build.

## Undoing the CI work

Until the old in-project CI is removed (PR 3), all of this is additive:
- `autoDeploy = false`, or deleting the webhook, returns you to local
  `make` and `pulumi up`.
- `cd cicd && pulumi destroy` removes exactly the grants it added. Unset
  `gitea:opsLaunchers` to remove the Role.
- The CI project can be deleted (30-day soft delete) without touching prod.

## Known limits

- Cloud Build has no Gitea connector, so the triggers carry an inline launcher.
  The source-less Pub/Sub trigger shape is allowed by gcloud's flag rules but
  is proven only at the first `make ci-up`.
- Cloud Build's default pool caps this project at **10 build vCPUs per region**
  and **10 builds at once** project-wide. Google refused an increase on
  2026-10-24, and its docs say neither is raised on request (a private pool
  would be, but this project's private-pool CPUs are capped at 4). A launcher
  (2 vCPUs, us-east1) holds its slot while it waits on its pipeline, so on
  2026-10-24 four launchers starved their own 8-vCPU pipelines until the
  queued builds would have expired (1 h). Pipeline builds therefore never run
  in us-east1: each goes to the least-busy region in
  `gitea-cicd:pipelineRegions` (default us-east4, us-east5, us-central1,
  us-south1, us-west1, all probed for `E2_HIGHCPU_8` and for publishing to
  `cloud-builds`). Five launchers plus five pipelines is the 10-build cap.
  `_REGION` stays the app's region; commit-status links point at each build's
  own region. `make ci-drill` / `ci-infra-up` still submit to us-east1.
- A PR preview of runner (Kubernetes) changes can't do the server-side dry
  run: the exec-plugin kubeconfig connects as `cb-infra-preview`, which holds no
  `container` role, so the cluster refuses it. The Kubernetes provider falls
  back to a client-side diff instead (pulumi-kubernetes v4, `Create` and
  `Update` on a refused preview), so server-defaulted fields don't show. A local
  `make preview` (run as you) still does the full dry run. That the fallback
  covers every object is proven only at the first PR preview.
- A wrong password or token on Gitea's REST API logs only a 401, so fail2ban
  can't see API guessing.
- The `gitea-backups`, `gitea-config` and `gitea-496920-pulumi-state` buckets had
  GCS's default `projectViewer:` read bindings (`storage.legacyObjectReader` /
  `legacyBucketReader`), which would let anyone with project Viewer read their
  objects. Removed by hand on 2026-10-21 so a project-Viewer grant can't become
  a path to the backups (the Gitea database and server secrets); legitimate
  access is per-service-account. GCS may re-add them if a bucket is recreated,
  so re-check after any bucket replacement. To reverse:
  `gcloud storage buckets add-iam-policy-binding gs://<b> --member=projectViewer:gitea-496920 --role=roles/storage.legacyObjectReader`.
- Builds run on the public pool, which caps a build at 10 CPUs with no
  high-memory type (hence `E2_HIGHCPU_8` and the constrained Go compile in
  `cloudbuild/infra.yaml`). The infra step runs on Google's gcloud image,
  which the workers have cached (a 1 s pull, where `pulumi/pulumi` took
  ~100 s from Docker Hub), and downloads the pinned Go and Pulumi CLI into it,
  each checked against its publisher's SHA-256 (`TOOLS` in
  `scripts/cicd/infra_deploy.py`; bump Pulumi there). The infra build's Go
  compile against pulumi-gcp, and the gcp and kubernetes Pulumi plugins, are
  cached across runs in a GCS `<ci>-build-cache` bucket, one object per
  `infra/go.mod` + `go.sum` + Go version + `TOOLS`: `scripts/cicd/infra_deploy.py`
  restores it before `go build` (deploys and previews) and a deploy writes it,
  after its first preview has fetched the plugins, only when its go.sum has
  none yet, so an ordinary deploy uploads nothing. A
  dependency bump starts from the newest object, and that upload goes up in
  parallel pieces (cb-infra can read the bucket's settings, which gcloud
  checks first). The build uses `-trimpath`:
  without it Go keys every compiled package by its directory, which differs
  each run, and the cache never hits. Best-effort: a miss or error just builds
  cold. Infra deploys also hold the ops pool's node with a placeholder pod
  from the build on, so the backup and verify Jobs never wait for it to scale
  up from zero.
  The image build is still cold: both Dockerfiles are multi-stage and the cost
  is in the discarded builder stages (Caddy's xcaddy compile, gitea's
  GPG-verified download), which BuildKit inline cache does not reach -- caching
  those needs buildx `--cache-to mode=max`, a heavier change left to its own PR.
- SSH to the VM goes through OS Login, which caps each account's profile at
  32 KiB of keys. Every engine Job pod and every infra build has a fresh HOME,
  so each registered a new key that never expired; on 2026-10-24 the VM began
  refusing the backup Job's SSH (`Permission denied (publickey)`) and two infra
  deploys failed before changing anything. Keys now expire after an hour
  (`--ssh-key-expire-after=1h` in `scripts/ops/gitea_ops.py` and the config
  sync in `scripts/cicd/infra_deploy.py`), and each Job / infra run first
  removes its account's keys that never expire or already have.

## Drill log

| Date | Drill | Commit | Build | Result |
|---|---|---|---|---|
| 2026-10-20 | D2 (by hand, pre-28) | 4a30834 | Job `gitea-ops` | PASS: 1 user / 96 repos from snapshot and dump |
| 2026-10-23 | First CI infra deploy (`make ci-infra-up`) | cbb9f7f | `145921a9` | SUCCESS: backup Job as cb-infra, no-op `pulumi up` (128 unchanged), verify; `infra-last-good` set |
| 2026-10-23 | D1 backup | cbb9f7f | `145921a9` | PASS: dump + snapshot READY + manifest/exports present (part of the deploy above) |
| 2026-10-23 | D5 infra fail-verify | cbb9f7f | `90ed3e4f` | PASS: injected failure after verify, re-applied last-good, rolled back clean |
| 2026-10-23 | D3 image fail-verify (Path A) | cbb9f7f | `3386449e` | PASS: built+promoted, rolled back to `:previous` |
| 2026-10-23 | D4 image fail-after-migrate (Path B) | cbb9f7f | `51c9bbbc` | PASS: consistent snapshot, data restored, drill marker absent |
| 2026-10-23 | D6 alerting | cbb9f7f | D3–D5 | PASS: three "rolled back automatically" e-mails delivered to the operator |
| 2026-10-23 | Weekly rebuild (real, go-live validation) | cbb9f7f | `cdd26dc5` | SUCCESS: automated Path A release, prod on gitea `sha256:4b25d9c0…`, `:last-good` moved |
| 2026-10-24 | D5 infra fail-verify (infra.yaml step env) | 6b03c7e | `6e1a58b6` | PASS: infra code `bea3e5b` attested |
| 2026-10-24 | D5 infra fail-verify (Go build cache) | 1a04ebf | `f2dcd208` | PASS: infra code `7f2ef3b` attested; 29.4 min |
| 2026-10-24 | D5 infra fail-verify (-trimpath cache, ops warm-up) | 6207e77 | `51208ae7` | PASS: infra code `d70ce51` attested; 17.9 min with a cold cache, no ops-pool waits, last-good re-apply compiled in 13 s |
| 2026-10-24 | D5 infra fail-verify (SSH key expiry) | 550d155 | `d53fd2fa` | PASS: infra code `4d6036d` attested; cleared 22 stale OS Login keys (gitea-release) and 8 (cb-infra), and SSH worked again; 10.5 min |
| 2026-10-24 | D3 image fail-verify (Path A) | 550d155 | `46984fca` | PASS: image code `515c803` attested; promoted, rolled back to `:previous` |
| 2026-10-24 | D4 image fail-after-migrate (Path B) | 550d155 | `0d7cd5a0` | PASS: image code `515c803`; restored from `gitea-data-pre-release-20261024t225431z`, drill marker absent; down ~22:54–23:09 UTC |
| 2026-10-25 | D5 infra fail-verify (gcloud image, fetched tools, plugin cache) | 246882f | `2b8fa2d0` | PASS: infra code `405d4d7` attested; image pull 1.2 s (was 96–114 s), new cache object +493 MB of plugins; 8.7 min |
| 2026-10-25 | D5 infra fail-verify (prune week-old `.pre-restore` trees) | 639575d | `85a32d57` | PASS: infra code `dd70fec` attested; the backup's prune found 0 of 2 trees older than 7 days; 8.5 min |
| 2026-10-25 | D3 image fail-verify (Path A) | 639575d | `667106d1` | PASS: image code `e8a8e86` attested; promoted, rolled back to `:previous`. A first submission from a git worktree (`450d7883`) failed at `resolve-gitea` before any release: `.gcloudignore`'s `.git/` skips only a directory, so the worktree's `.git` file was uploaded |
| 2026-10-25 | D4 image fail-after-migrate (Path B) | 639575d | `960c42dc` | PASS: image code `e8a8e86`; restored from `gitea-data-pre-release-20261025t153628z`, drill marker absent; down 15:39–15:41 and 15:47–15:49 UTC (10 s probes), about 3.5 min in all |
| 2026-10-25 | D5 infra fail-verify (vm_sync by tree kind, replaced-VM waits) | 1435746 | `6920bafd` | PASS: infra code `6b092ee` attested; legacy path (startup-script wait, `running`, config sync) in deploy and rollback; 9.1 min |
