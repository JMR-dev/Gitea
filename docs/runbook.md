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
sudo systemctl reload nftables
sudo podman ps        # container networking must still work
curl -fsS https://gitea.jasonmross.dev/api/healthz
```

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

### fail2ban — drill it, do not trust the status output

```bash
# 1. Fail a web login, then read the log line. It must show YOUR ip,
#    not a 10.89.x address. If it shows Caddy, REVERSE_PROXY_TRUSTED_PROXIES is wrong.
sudo journalctl CONTAINER_NAME=gitea | grep -i 'failed authentication'

# 2. The jail is live.
sudo fail2ban-client status gitea

# 3. Ban a throwaway address you control, then verify from that host that
#    both 443 and 2222 are genuinely unreachable.
sudo fail2ban-client set gitea banip <ip>
sudo nft list set inet f2b-prerouting f2b-gitea-v4
sudo fail2ban-client set gitea unbanip <ip>
```

A ban that appears in `fail2ban-client status` but still lets traffic through
means the prerouting action is not in effect — the default INPUT-hook actions
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

`bootstrap.sh` provisions a 2 GB swap file with `vm.swappiness = 10` as ballast,
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
- adding a Gitea Actions runner to this VM (don't — see
  [migrate-to-gitea-scm.md](migrate-to-gitea-scm.md))

### Resilience

```bash
make backup                                   # object lands in the backups bucket
gcloud compute instances reset gitea-vm --zone <zone>
# after it comes back: data intact, cert valid, nftables and fail2ban up
```

---

## Common operations

### Roll back to a previous image

Every build pushes `:$SHORT_SHA` alongside `:prod`. Find the digest in the build
log, then:

```bash
make ssh
sudo podman tag <region>-docker.pkg.dev/<project>/gitea/gitea:<sha> \
                <region>-docker.pkg.dev/<project>/gitea/gitea:prod
sudo systemctl restart gitea
```

For a durable rollback, re-point `:prod` in Artifact Registry instead — a host-
local retag is undone by the next `podman auto-update`.

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
server for Application Default Credentials. `vm/bootstrap.sh` probes this at
first boot and caches the answer:

```bash
cat /etc/gitea/caddy-network     # "bridge" or "host"
```

To re-probe, delete that file and run `sudo systemctl start gitea-config-sync`.
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

`gitea-config-sync.service` only restarts services when a rendered file actually
changed. To see what it did:

```bash
sudo journalctl -u gitea-config-sync -n 100 --no-pager
```

If the render was skipped, the message will say the Gitea secrets were
unavailable — check `gcloud secrets versions list gitea-internal-token`.

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
