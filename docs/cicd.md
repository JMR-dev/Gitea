# CI/CD for Gitea itself

This repo's images, VM and `infra/` stack deploy from a GCP project of their
own, `gitea-ci-781388`, not from the app project `gitea-496920`. CI for the
repos Gitea *hosts* is unaffected. It stays on Gitea Actions and the GKE
runners.

**Status:** built, `autoDeploy = false`. Nothing deploys on merge until drills
D1–D6 below have passed and `autoDeploy` is turned on.

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
| push to main touching `infra/`, `vm/`, `cloudbuild/infra.yaml` | infra deploy | no |
| push to main touching `image/`, `cloudbuild/image.yaml` | image build and release | no |
| push to main touching `image/gitea.version`, `image/alpine.version` or `vm/bootstrap.sh` | as above | **yes** (`make approve`) |
| push whose commit list Gitea truncated (more than 15 commits) | everything | **yes** |
| anything else (docs, `cicd/`, scripts) | nothing; status "nothing to deploy" | n/a |
| Sunday 04:00 UTC | image rebuild from main and release | no |

- The version files are what make a Gitea major or Alpine minor change.
- `vm/bootstrap.sh` is the VM's startup script, so any edit replaces the VM
  (about 15 min down and a new Let's Encrypt certificate).
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
6. `pulumi up` and the config sync.
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
path whose only way back is a data restore.

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
and its passphrase — could replay it as a deployer. Confirm the applied state is
token-free before creating the preview accounts.

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
- `cd infra && pulumi up`, `make sync`;
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
- A PR preview of runner (Kubernetes) changes can't do the server-side dry
  run: the exec-plugin kubeconfig connects as `cb-infra-preview`, which holds no
  `container` role, so the cluster refuses it. The Kubernetes provider falls
  back to a client-side diff instead (pulumi-kubernetes v4, `Create` and
  `Update` on a refused preview), so server-defaulted fields don't show. A local
  `make preview` (run as you) still does the full dry run. That the fallback
  covers every object is proven only at the first PR preview.
- A wrong password or token on Gitea's REST API logs only a 401, so fail2ban
  can't see API guessing.

## Drill log

| Date | Drill | Commit | Build | Result |
|---|---|---|---|---|
| 2026-10-20 | D2 (by hand, pre-28) | 4a30834 | Job `gitea-ops` | PASS: 1 user / 96 repos from snapshot and dump |
