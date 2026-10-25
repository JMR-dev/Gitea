# Runbook

## Post-deploy verification

Work top to bottom the first time. Several of these controls fail *silently*, so
the drills matter more than the status output.

### Infrastructure

```bash
cd infra && pulumi preview            # clean, no diff
dig +short A gitea.jasonmross.dev     # the static IP
gcloud compute instances describe gitea-vm --zone <zone> \
  --format='value(disks[].deviceName, shieldedInstanceConfig)'
```

### Host

```bash
make ssh
mount | grep /var/lib/gitea                          # PD mounted, xfs
systemctl list-dependencies gitea.service | grep mount   # the ordering dep exists
ls -Zd /var/lib/gitea                                # container_file_t
systemctl status gitea caddy nftables fail2ban
podman ps                                            # both healthy
```

**The nftables reload check** — this is what catches an accidental global flush:

```bash
sudo nft list ruleset | grep -E '^table (inet gitea_filter|inet netavark|ip netavark)'
sudo nft -f /etc/sysconfig/nftables.conf    # exactly what the playbook's handler does
sudo nft list table inet netavark | grep -E 'dport (443|2222) dnat'   # both survive
curl -fsS https://gitea.jasonmross.dev/api/healthz
```

Never `systemctl reload` (or `restart`/`stop`) nftables on this box. On EL the
unit's reload is `flush ruleset` plus an include, so it wipes netavark's DNAT
rules and fail2ban's bans. Every container not restarted afterwards loses its
published ports, with nothing in `podman ps` to show it. If it happens anyway,
`sudo systemctl restart gitea caddy` puts the port forwards back.

**Subnet agreement** — a mismatch here is what silently breaks fail2ban:

```bash
sudo podman network inspect gitea --format '{{range .Subnets}}{{.Subnet}}{{end}}'
sudo grep REVERSE_PROXY_TRUSTED_PROXIES /etc/gitea/app.ini
```

### TLS / DNS-01

```bash
sudo journalctl -u caddy | grep -i 'acme\|challenge'
```

Look for the **dns-01** challenge. If Caddy fell back to http-01, the
googleclouddns plugin or its credentials are not working — check
`/etc/gitea/caddy-network` and see *Caddy cannot get a certificate* below.

```bash
curl -vI https://gitea.jasonmross.dev            # valid Let's Encrypt cert
```

DNS-01 means renewal does not need inbound port 80 at all. That is testable:
temporarily remove the `gitea-allow-web` port 80 rule and force a renewal.

The ACME account and certificates live in the `caddy-data` podman volume, under
`/var/lib/containers` on the **boot** disk, not the data disk. Replacing the
instance therefore re-registers and re-issues on first start. That is fine
occasionally, but Let's Encrypt allows 5 duplicate certificates per week, so
several replacements in a few days can lock issuance out until the window
rolls over.

### fail2ban — drill it, do not trust the status output

Two jails read Gitea's log, through the two modes of `vm/fail2ban/filter.d/gitea.conf`:

- `gitea` (5 in 10 min, 1 h ban) counts refused credentials: a web sign-in, or an SSH
  key or password.
- `gitea-ssh-probe` (50 in 3 h, 1 day ban, doubling for repeat offenders) counts SSH
  connections that end without offering one: `ssh-keyscan`, port scanners, bots that
  enumerate auth methods.

A single `ssh-keyscan` makes five probe connections and bans nobody. Your own addresses
belong in the secret `gitea:fail2banIgnore` (space-separated), never in the jail file.
This repo is mirrored publicly.

```bash
# 1. Fail a web login, then read the log line. It must show YOUR ip,
#    not a 10.89.x address. If it shows Caddy, REVERSE_PROXY_TRUSTED_PROXIES is wrong.
sudo journalctl CONTAINER_NAME=gitea | grep -i 'failed authentication'

# 2. Both jails are live, the ignore list holds what you expect, and each
#    filter mode matches the real log (counts, not zero).
sudo fail2ban-client status gitea
sudo fail2ban-client status gitea-ssh-probe
sudo fail2ban-client get gitea ignoreip
sudo fail2ban-regex -m CONTAINER_NAME=gitea systemd-journal 'gitea[mode=auth]'  | grep ^Lines
sudo fail2ban-regex -m CONTAINER_NAME=gitea systemd-journal 'gitea[mode=probe]' | grep ^Lines

# 3. The action really runs. Ban documentation addresses (TEST-NET-1, and IPv6
#    2001:db8::/32), see each one land in its set, then unban.
sudo fail2ban-client set gitea banip 192.0.2.1
sudo fail2ban-client set gitea banip 2001:db8::1
sudo nft list table inet f2b-prerouting     # both elements, and a drop rule per set
sudo fail2ban-client set gitea unbanip 192.0.2.1
sudo fail2ban-client set gitea unbanip 2001:db8::1
sudo grep -iE 'error|script' /var/log/fail2ban.log | tail   # nothing new
```

A ban that appears in `fail2ban-client status` but not in the nft set means the
action is failing. Check `/var/log/fail2ban.log` for "Script error". Whitespace
before a `;` anywhere in `nft-prerouting.conf` does exactly that: fail2ban reads
` ;` as a comment and cuts the line. A ban that is in the set but still lets traffic
through means the prerouting action is not in effect: the default INPUT-hook actions
never see DNAT'd container traffic.

### WAF — git must still work, and blocks must still happen

Two drills. The first is the one that catches a broken product; run it after any
change to the Caddyfile matcher.

```bash
# 1. git still works through the proxy (the bypass is intact)
git clone https://gitea.jasonmross.dev/<you>/<repo>.git /tmp/wafdrill
cd /tmp/wafdrill && dd if=/dev/urandom of=blob.bin bs=1M count=20
git add -A && git commit -qm 'waf drill' && git push
```

A `403` on push means the `@gittransport` matcher no longer covers the git
routes. See [waf.md](waf.md).

```bash
# 2. the WAF is actually inspecting the web branch
curl -s -o /dev/null -w '%{http_code}\n' \
  'https://gitea.jasonmross.dev/?file=../../../../etc/passwd'
```

Expect `403` when `gitea:wafMode` is `On`, and `200` in `DetectionOnly` — in
detection mode, confirm it was *recorded* instead:

```bash
make ssh
sudo journalctl CONTAINER_NAME=caddy --since '5 min ago' | grep 949110
```

If neither blocks nor records, the WAF module is not in the request path — check
`order coraza_waf first` survived the last Caddyfile edit.

### Actions runners, if `gitea:runnersEnabled` is on

```bash
make kube && make runners                       # gitea-runner-0 Running, registered
make ssh
sudo ls -l /etc/gitea/runner-token              # -r-------- 1000 1000
sudo fail2ban-client get gitea ignoreip         # includes `pulumi stack output runnerEgressIp`
```

Then run the smoke workflow in [runners.md](runners.md) — it checks `docker
build`, the cache, artifacts, and that the metadata server is unreachable from a
job. The last one is the check that matters most and fails most quietly.

### fail2ban's WAF jail follows the mode

```bash
sudo fail2ban-client status                 # caddy-coraza listed only when wafMode=On
grep -A2 '^\[caddy-coraza\]' /etc/fail2ban/jail.d/gitea.local
```

`enabled = false` in `DetectionOnly` is correct, not a bug: the WAF is not
refusing anything, so there is no verdict to escalate into a ban.

### Auto-update and rollback

```bash
sudo podman auto-update --dry-run    # lists both units; UPDATED = false
```

If that errors on authentication, `/etc/containers/ar-auth.json` is stale or
missing — `sudo systemctl start gitea-ar-auth.service` and check the timer.

**Rollback drill.** Push a deliberately broken `:prod` (a bad `CMD` is enough),
run `sudo systemctl start podman-auto-update.service`, and confirm:

```bash
sudo journalctl -u podman-auto-update | grep -i rollback
sudo podman inspect gitea --format '{{.ImageName}}'
```

If it does **not** roll back, `Notify=healthy` is not taking effect on this
podman version. Fall back to `HealthCmd` plus `HealthOnFailure=stop` in
`vm/quadlets/gitea.container` and record it here.

### The weekly rebuild — fire it once by hand

**Do this immediately after the first `pulumi up`.** It is the only check here
that cannot wait for its schedule, because a failure is completely silent: the
job fires at 04:00 on a Sunday, gets a 400, and layer 3 of the update story
quietly stops feeding layer 2. Nothing alerts.

```bash
gcloud scheduler jobs run gitea-weekly-rebuild --location <region>
sleep 15
gcloud builds list --region <region> --limit 5   # a build must have started
                                                # (2nd-gen triggers are regional;
                                                #  the default is the global region)
gcloud scheduler jobs describe gitea-weekly-rebuild --location <region> \
    --format='value(status)'
```

The job POSTs an **empty** body to the regional
`.../locations/{region}/triggers/{id}:run` endpoint. That is deliberate:
`RunBuildTriggerRequest.source` is a 1st-generation `RepoSource` that cannot name
a 2nd-gen repository, and omitting it tells Cloud Build to use the trigger's own
configured repository and branch — the REST equivalent of
`gcloud builds triggers run TRIGGER --region=…` with no `--branch`.

If it returns 400 or 404, check `scheduleRebuild` in
`infra/pkg/build/build.go`: a 404 usually means the URL used the global
`.../projects/{p}/triggers/{id}:run` path instead of the regional one.

Until it passes, run `make build` manually to pick up base-image security fixes.

### Memory on a 2 GB instance

`e2-small` is 2 shared vCPU / 2 GB RAM. Measured on this exact image set:

| | idle | peak during a 60 MB / 400-object push |
|---|---|---|
| gitea | 105 MB | 375 MB |
| caddy + Coraza + full CRS | 50 MB | 51 MB |

The WAF is not the memory story — CRS costs about 50 MB and does not grow under
load. Git subprocesses are: `index-pack` and `gc` scale with what is being
pushed, and that number climbs with repo size.

The playbook (role `base`) provisions a 2 GB swap file with `vm.swappiness = 10` as ballast,
because GCE images ship with none and an OOM kill mid-push is the failure this
prevents. Check it:

```bash
free -m
swapon --show
```

**Swap should be near-idle.** If `free -m` shows sustained swap use, that is the
signal to move to `e2-medium` (`pulumi config set gitea:machineType e2-medium`),
not to enlarge the swap file. Things that will push you over:

- repositories in the multi-GB range, or many concurrent clones
- switching from SQLite to a PostgreSQL container on the same host
- adding a Gitea Actions runner to this VM (don't — runners live on GKE, see
  [runners.md](runners.md); and [migrate-to-gitea-scm.md](migrate-to-gitea-scm.md)
  for why a runner must not live on the host it deploys)

### CPU: check for crawlers before resizing

On 2026-10-15, Gitea sat at a full core while memory and swap were fine. AI
crawlers (ClaudeBot, GPTBot) were walking every `src/`, `commits/`, `raw/` and
`blame/` page of every public repo, a few requests a second at 1.5 s of git
work each. Two things now keep that load off:

- **`/robots.txt`**, answered by Caddy (`vm/config/Caddyfile.tmpl`). It
  disallows the expensive repository routes. Well-behaved crawlers stop within
  a crawl cycle.
- **`gitea:requireSigninView expensive`**. Anonymous visitors are redirected to
  the login page for code, history, source archives, issues, pull requests and
  wikis. Repo home pages, releases, profiles, clones and release attachments
  stay public. This catches crawlers that ignore robots.txt. Set it back to
  `false` (then `pulumi up` and `make sync`) to reopen those pages.

If Gitea is slow again, see who is asking before reaching for `e2-medium`:

```bash
sudo podman stats --no-stream                          # is it gitea, and CPU or memory?
sudo journalctl -u caddy --since -30min -o cat \
  | jq -r 'select(.request) | .request.headers["User-Agent"][0]' \
  | sort | uniq -c | sort -rn | head                   # top user agents
```

### Resilience

```bash
make backup                                   # object lands in the backups bucket
gcloud compute instances reset gitea-vm --zone <zone>
# after it comes back: data intact, cert valid, nftables and fail2ban up
```

---

## Common operations

### Local Pulumi needs the GKE auth plugin

The app stack's runner cluster is reached through a `gke-gcloud-auth-plugin`
exec stanza in its kubeconfig (`infra/pkg/runner/runner.go`), so `make preview`
and `make up` fail to build the Kubernetes provider unless that plugin is on
PATH. Install it once:

```bash
sudo dnf install google-cloud-cli-gke-gcloud-auth-plugin
command -v gke-gcloud-auth-plugin   # confirm it is on PATH
```

In Cloud Build the pinned gcloud image already ships it, and
`scripts/cicd/infra_deploy.py` puts the SDK bin dir on PATH. The plugin mints a
token from the caller's own credentials at connect time; no token is stored in
the stack state.

### Releases: how an image goes live

A build (`make build`, or the pipeline) pushes `:$SHORT_SHA` and a version tag
and **never `:prod`**. Moving `:prod` is a release, and only
`scripts/ops/gitea_ops.py release` does it (`make release GITEA=<sha>`):

1. Records state: the `:prod` digests and labels, SSH host keys, and secret
   version *numbers*. Refuses a Gitea **major** or an Alpine **minor** change
   unless given `--allow-major` / `--allow-alpine-minor`; those changes arrive
   only as PRs to `image/gitea.version` / `image/alpine.version`.
2. Backs up: a `gitea dump`, a config tarball, the instance and IAM exports
   (below), and tags the current images `:previous`.
3. Picks a path:
   - **A**, same Gitea version (an Alpine or Caddy rebuild): an online snapshot,
     move `:prod`, `podman-auto-update` (podman's own health-gated rollback
     still applies).
   - **B**, Gitea version changed: pre-pull, pause the auto-update and reboot
     timers, stop Gitea, take a **consistent** snapshot, run `gitea migrate` as
     a one-shot container, then move `:prod` and start the unit. The unit start
     is never the migration: under `TimeoutStartSec=300` a long migration would
     be killed and re-run on a half-migrated database.
4. Status checks: health and version endpoints, the digest each container is
   *actually* running (catches a silent podman rollback), the SSH host keys,
   an anonymous clone over HTTPS, then a 5-minute soak (health every 30 s, no
   unit restarts, no `[F]`/`panic` in the journal).
5. **On any failure it rolls back by itself**: `:prod` back to `:previous`, and
   on path B also the data, from the snapshot it took in step 3. It logs
   `GITEA_RELEASE_ROLLBACK` and exits 1; if the rollback's own checks fail, it
   logs `GITEA_RELEASE_ROLLBACK_FAILED` and exits 2. That is the case for the
   manual procedure below.

Every run writes `gs://<project>-gitea-backups/config/<stamp>/manifest.json`
once, when it finishes. A release also writes `progress-NN-<step>.json` there
after every step that changes something: the engine's identity can create
objects in the backup bucket but never overwrite or delete them, so each
checkpoint is a new object. `--dry-run` prints the plan without changing
anything.

**Where it runs.** The rollback is code in the same process as the release.
So the engine runs as a Kubernetes Job (`gitea-ops/gitea-ops`) on the runner
cluster's **ops pool**, not on the machine that starts it. That pool is one
on-demand node, scaled from zero, tainted so no CI pod lands on it, under its
own `gitea-release@` identity (`infra/pkg/ops`). `make release` and the
`ops-*` targets create the Job (`scripts/ops/launch_job.py`) and follow its
log. Closing the terminal stops only the log: `make ops-logs` re-attaches,
and `make ops-status` says what is running. Only one Job exists at a time.
It is never retried, and it is killed after 2 hours. It is deliberately not a
Gitea Actions job: the release stops Gitea, and an Actions job reports to
Gitea. `RUN_ON=local` runs the engine here instead, as break-glass for when
the cluster itself is broken. Keep the machine awake, and use tmux.

**If the engine itself dies mid-release** (its node, an OOM, the 2-hour
deadline):

1. `make ops-status` and `make ops-logs` show the last step. So does the
   newest `progress-NN-<step>.json` in the run's `config/<stamp>/` folder, which
   stands in for the `manifest.json` the run never got to write.
2. Pausing the timers armed a dead man's switch on the VM. Three hours later it
   re-enables the auto-update and reboot timers and logs `DEADMAN` at crit
   (`journalctl -t gitea-ops`). It does **not** start Gitea, because that
   depends on how far a migration got. It leaves the timers paused if a restore
   disk is still attached.
3. Finish by hand from it: `make ops-rollback MANIFEST=<that progress url>`, adding
   `RESTORE_DATA=1` if the progress got past `snapshotted`. Or, if it got to
   `started` and the checks look good, just re-enable the timers.

A path B rollback loses whatever was written during the soak, and a later push
can then make the GitHub push mirror force-push those commits away. Pause the
mirror before a manual data rollback. Those writes are not gone at once: the
replaced tree stays in `/var/lib/gitea/.pre-restore-<stamp>` for 7 days (see
below).

### Roll back to a previous image

From a release's manifest:

```bash
make ops-rollback MANIFEST=gs://<project>-gitea-backups/config/<stamp>/manifest.json
# add RESTORE_DATA=1 if that release changed the Gitea version (path B)
```

By hand, re-point `:prod` in Artifact Registry and let the VM pull it:

```bash
gcloud artifacts docker tags add <region>-docker.pkg.dev/<project>/gitea/gitea:previous \
                                 <region>-docker.pkg.dev/<project>/gitea/gitea:prod
make rollout
```

Only same-version rollbacks are this simple. **Gitea refuses to start an older
version on a database a newer one has migrated** ("Your database ... is for a
newer Gitea"), so going back across a version change needs the data restore
too.

`gitea:rollback-1.27.2` and `caddy:rollback-2.11.4` pin the last Debian-based
images, so the cleanup policy never deletes them.

### Restore the data disk from a snapshot

`gitea_ops.py rollback --restore-data` does this; by hand it is:

1. `systemctl stop gitea podman-auto-update.timer gitea-reboot-window.timer`.
2. Create a disk from the snapshot and attach it with device name
   `gitea-restore`.
3. `mount -o nouuid` it. The copy has the **same XFS UUID** as the live disk,
   which XFS refuses to mount twice without `nouuid`.
4. Move `/var/lib/gitea/*` aside into `/var/lib/gitea/.pre-restore-<stamp>`,
   then `rsync -aH` the copy in (inside the gitea image, which carries
   `rsync`; the host has none), then `restorecon -RF /var/lib/gitea`.
5. Unmount, then **detach and delete the copy before anything reboots**. fstab
   mounts `/var/lib/gitea` by UUID and could pick the copy.
6. Start the timers and gitea.

This keeps the Pulumi-managed `gitea-data` disk, so there is no state drift.

Each `.pre-restore-<stamp>` is a full copy of the data, and every D4 drill
leaves one. The engine's backup removes those older than 7 days, right after
its own snapshot (`make ops-backup`, and the backup in every infra deploy and
path A release; a path B release, which takes no snapshot there, does not). Its
log and manifest (`pre_restore_removed`) list what went. A removed copy is
still in that snapshot and in the daily ones taken while it was on the disk,
for 14 days more. Delete one sooner by hand once you know what went wrong.

### Backups and the restore drill

```bash
make ops-backup          # dump + config tarball + exports + online snapshot; prunes week-old .pre-restore-*
make ops-drill-restore   # newest snapshot AND newest dump each boot a working Gitea
```

The drill runs the restored copy as a separate container with `--network none`
(no mirror pushes, webhooks or runner traffic), compares user and repo counts
with prod, and touches nothing live. The dump half rebuilds `gitea.db` from
`gitea-db.sql`, because the `data/gitea.db` inside a dump was copied while
Gitea was writing to it.

Until 2026-10-19 **no dump ever reached the bucket**: before uploading,
`gcloud storage cp` reads the destination object (`storage.objects.get`) and
lists the bucket (`storage.objects.list`), and the VM had only
`objectCreator`. It now has the custom `giteaBackupWriter` role (create, get,
list; still no overwrite or delete). If the bucket stops growing, check that
first:

```bash
gcloud storage ls -l gs://<project>-gitea-backups/dumps/ | tail -3
```

### Upgrading across a Gitea major

`image/gitea.version` holds only the major (`28`); patches and minors float.
To move to the next major:

1. A PR that changes `image/gitea.version`. Check the release notes'
   breaking changes against `vm/config/app.ini.tmpl` and the hosted repos'
   workflows.
2. `make ops-backup`, then `make ops-drill-restore TOOL_IMAGE=<new image>`.
   That boots the newest snapshot and dump **on the new version**, so it is
   also a migration rehearsal on a copy of the real data.
3. `make build`, then `make release GITEA=<sha> RELEASE_ARGS=--allow-major`.

1.27 → 28 (2026-10-19) was also Debian → Alpine, so it needed
`--allow-major --allow-alpine-minor`.

### Restore from a dump

```bash
gcloud storage cp gs://<project>-gitea-backups/dumps/gitea-<stamp>.zip .
```

A `gitea dump` archive contains the repositories, the SQLite database, custom
files, and config. Restore is documented upstream at
<https://docs.gitea.com/administration/backup-and-restore>; the short version is
to stop `gitea.service`, unpack over `/var/lib/gitea`, fix ownership to
`1000:1000`, and start it again. **Do this once as a drill before you trust it.**

### Caddy cannot get a certificate

The most likely cause is the ACME plugin failing to reach the GCE metadata
server for Application Default Credentials. The playbook (role `app`) probes
this once, on its first run, and caches the answer:

```bash
cat /etc/gitea/caddy-network     # "bridge" or "host"
```

To re-probe, delete that file and run `make vm-apply`.
If the bridge cannot reach `169.254.169.254`, the file will say `host` and Caddy
is switched to the host network, reaching Gitea over `127.0.0.1:3000` instead.
Both paths are supported; only the rendering differs.

Also verify the zone-scoped grant:

```bash
gcloud dns managed-zones get-iam-policy <zone-name>
```

### Changing the podman firewall driver

`/etc/containers/containers.conf.d/10-gitea.conf` sets
`firewall_driver = "nftables"`. Changing it on a live host leaves conflicting
rules behind — stop the containers and reboot rather than reloading.

### A config change did not take effect

The playbook restarts Gitea and Caddy only when a file it manages actually
changed. The deploy's build log has the run (`--diff`: every changed file, with
app.ini's and the Caddyfile's secrets masked). To see what a run would do now:

```bash
make vm-check      # read-only: --check --diff against the VM
make vm-apply      # apply it
```

If app.ini was not rendered, the run fails saying which Gitea secret was
unavailable -- check `gcloud secrets versions list gitea-internal-token`.

### Configuring a VM by hand (break-glass)

The VM has no startup script: the playbook (`vm/ansible`) configures it, and
what it sets up persists across reboots. A VM created outside a deploy (a
manual `pulumi up`, say) is inert until the playbook runs: `make up` runs
`pulumi up` and then `make vm-apply`. `make vm-apply` needs only gcloud with
IAP and OS Login on the project -- no Pulumi, no passphrase.

---

## Deferred hardening

- **Rootless podman** under a dedicated user. Needs `loginctl enable-linger`,
  `--user` timers, and subuid mapping; on a single-tenant VM the isolation gain
  is small, which is why it was not done up front.
- **PostgreSQL** instead of SQLite. Add a `postgres.container` quadlet plus its
  volume and password secret, then follow Gitea's documented dump/restore
  migration. Worth doing well before SQLite write contention shows up.
- **Wildcard certificate** for `*.gitea.jasonmross.dev` — trivial now that
  DNS-01 works.
- **Ops Agent** for metrics dashboards and log-based alerts.
