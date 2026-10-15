# Gitea Actions runners on GKE

Gitea Actions jobs run on a small GKE cluster in the same project, not on
`gitea-vm`. The VM has no memory to spare (see *Memory on a 2 GB instance* in
[runbook.md](runbook.md)), and a runner on the host it deploys is exactly the
loop [migrate-to-gitea-scm.md](migrate-to-gitea-scm.md) warns about.

Everything here is behind one flag, `gitea:runnersEnabled`, which is off by
default.

There are two runners, and a job's `runs-on` picks one:

| `runs-on` | Runner | How the job runs |
|---|---|---|
| `ubuntu-latest`, `ubuntu-24.04`, `ubuntu-22.04` | **pod runner** (`gitea-runner-pods`) | As its own pod in the `gitea-jobs` namespace. Jobs run in parallel, up to `gitea:runnerJobCapacity`, and the node pool grows to fit them. |
| `docker` | **DinD runner** (`gitea-runner`) | Inside that runner's rootless Docker daemon, one node per replica, `gitea:runnerCapacity` jobs at a time. |

**A job that needs a Docker daemon must use `runs-on: docker`.** Job pods have
none. On the pod runner, Docker actions, `docker://` steps, and a job
container's `options`, `volumes` or `credentials` fail the job, and so does
`docker build` in a `run:` step.

## Shape

| Piece | Choice | Where |
|---|---|---|
| Cluster | GKE **Standard**, zonal (`gitea:zone`), named `gitea-runners`, used for nothing else | `infra/pkg/gke` |
| Nodes | One autoscaled pool, `e2-standard-2` **spot** by default, private (no external IPs), COS | `infra/pkg/gke` |
| Node identity | `gitea-gke-nodes@`, holding only `roles/container.defaultNodeServiceAccount` | `infra/pkg/gke` |
| Egress | Cloud NAT on a **reserved static IP** (`runnerEgressIp` stack output) | `infra/pkg/network` |
| Control plane | **DNS endpoint only**, authorised by IAM; IP endpoints are disabled | `infra/pkg/gke` |
| Pod runner | StatefulSet of `docker.io/gitea/runner:<tag>` (no Docker daemon), one unprivileged pod that creates a pod per job | `infra/pkg/runner` |
| Job pods | Namespace `gitea-jobs`, Pod Security `baseline` enforced, own NetworkPolicy | `infra/pkg/runner` |
| DinD runner | StatefulSet of `docker.io/gitea/runner:<tag>-dind-rootless`, one pod per node | `infra/pkg/runner` |
| Registration | Preset token in Secret Manager, shared by Gitea and the runners | `scripts/bootstrap.sh` |
| WAF | Runner protocol and artifact routes bypass Coraza | `vm/config/Caddyfile.tmpl` |

Why Standard and not Autopilot: `docker` jobs need a Docker daemon (`docker
build`, Docker actions), which means a privileged Docker-in-Docker pod, and
Autopilot refuses privileged pods. The runner's Kubernetes backend, which runs
each job as its own pod, needs no privileges. That is why `ubuntu-*` jobs use
it, but it cannot run Docker actions or `docker build`.

Why zonal: GKE's free tier credits the management fee of **one** zonal or
Autopilot cluster per billing account. If this billing account already has a
cluster, this one adds about $73/month.

### Cost

Roughly **$25–35/month** idle, provided the free-tier credit covers the cluster
fee. Idle means one node, carrying the DinD runner and the pod runner:

| | approx. / month |
|---|---|
| spot `e2-standard-2` node | $15 |
| static NAT IP (in use) | $3.65 |
| Cloud NAT gateway + data processing | $1 + $0.045/GB |
| boot disk 30 GB + PVCs 10 + 20 + 10 GB, pd-balanced | $7 |

Busy periods add **burst nodes**. Each one costs a spot `e2-standard-2` plus its
boot disk, about $0.03/hour together, only while it exists. The autoscaler
removes a node about 10 minutes after its last job pod finishes. Each extra
DinD replica adds an always-on node and two PVCs.

## Threat model

Read this before pointing a repository you do not fully trust at these runners.

- **`docker` jobs can act as the runner.** They run inside the DinD runner
  pod's own dockerd, and get its socket, which is what makes `docker build`
  work. Through that socket a job can read anything the daemon can see,
  including the runner's `/data/.runner` credentials and the mounted
  registration token. A malicious workflow could register a rogue runner, or
  impersonate this one and pick up other repositories' jobs and secrets. That
  is acceptable here only because this is a single-tenant instance with
  `DISABLE_REGISTRATION = true`: every workflow author is already trusted with
  every secret.
- **`ubuntu-*` jobs are better isolated.** Each runs in its own pod in
  `gitea-jobs`, which shares nothing with the pod runner but the cache port.
  It gets no Docker socket, no Kubernetes API token, and no view of the
  runner's credentials. `gitea-jobs` enforces the `baseline` Pod Security
  level, so no job pod can be privileged or use host namespaces or hostPath,
  whatever creates it.
- **The pod runner holds an API token, scoped to `gitea-jobs`.** It can create,
  exec into and delete pods there, and create and delete Secrets. It cannot
  read Secrets back, and it cannot touch `gitea-runner`, so it cannot exec into
  the DinD runner. Only the pod runner may reach the API server: job pods are
  blocked by the RFC1918 exclusion below.
- **Jobs cannot reach Google Cloud.** This part is enforced, not assumed:
  - **Jobs get no useful Google identity.** Each runner's Kubernetes service
    account holds exactly one permission, read access to the registration
    token secret. Job pods run as `gitea-jobs`'s `default` account, which holds
    none.
  - **The metadata server is blocked.** `docker` job containers' traffic
    leaves from the DinD runner pod's network namespace, so the GKE metadata
    server would issue them that KSA's token. The NetworkPolicies on both
    namespaces block `169.254.0.0/16` for that reason. They are
    **load-bearing**: do not "simplify" them away.
  - **Escaping to the node gains little.** For `docker` jobs, dockerd runs as
    uid 1000 (rootless), so a job that breaks out of its own container lands as
    an unprivileged user. A job pod is an ordinary unprivileged container, so
    escaping one takes a kernel or runtime exploit, and lands as root. Root on
    the node would yield the node SA, which holds logging and monitoring writes
    and nothing else, plus whatever the other pods on that node hold. That can
    include the runners' credentials, since several jobs and runners now share
    nodes. That is the same trust boundary as the first point.
- **The cluster is not a path to the VM's private side.** The NetworkPolicies
  also block all RFC1918 ranges. The one exception is the pod runner, which
  may reach the control plane on 443. Jobs reach Gitea the way the rest of the
  internet does: through its public address, Caddy, and the WAF (except the
  bypassed runner routes below).

## The WAF bypass

`vm/config/Caddyfile.tmpl` routes these prefixes to Gitea **without** Coraza:

```
/api/actions/*                                         runner protocol (Connect RPC)
/api/actions_pipeline/*                                artifacts v3, job summaries
/twirp/github.actions.results.api.v1.ArtifactService/* artifacts v4
```

This is not optional. The runner speaks Connect with binary protobuf bodies
(`Content-Type: application/proto`), and artifact uploads are PUT/PATCH with
`application/octet-stream`. CRS rejects both outright in `On` mode: the
content type is not in `tx.allowed_request_content_type`, and the methods are
not in `tx.allowed_methods`. Log uploads would trip SQLi/XSS rules on build
output even if they got that far. Every bypassed route authenticates with a
runner or task token. The reasoning is the same as for the git bypass in
[waf.md](waf.md).

The matcher uses prefixes, not a list of RPC names, so a Gitea upgrade that
adds an RPC does not silently start returning 403. `/api/v1/` is a different
prefix and stays behind the WAF. Caddy cleans the path before matching, so
`/api/actions/../v1/...` is inspected like any other `/api/v1` request.

## fail2ban and the shared egress IP

Every runner and every job shares the Cloud NAT address. With the `gitea` jail
at `maxretry = 5`, a single job retrying a bad token would ban all of CI for
an hour. It would also take the runners offline, since they poll through the
same IP. So the VM adds that address to `ignoreip`, using instance metadata
`runner-egress-ip`. The trade-off is that hostile traffic from a job is never
banned, which is acceptable under the threat model above.

## Turning it on

1. Re-run `scripts/bootstrap.sh <project-id>`. It is idempotent, and on an
   existing project it adds three things:
   - the `container.googleapis.com` API
   - `roles/container.admin` for `cb-infra@`
   - the `gitea-runner-registration-token` secret, as 64 random hex characters
2. Set the flag and push:
   ```bash
   (cd infra && pulumi config set gitea:runnersEnabled true)
   make preview        # local: the infra trigger only fires on main, so a branch gets no preview
   git commit infra/Pulumi.prod.yaml -m 'Enable GKE runners' && git push
   ```
   The first apply takes 15–25 minutes: cluster, node pool, then the
   StatefulSet. If the StatefulSet wait times out (IAM or CSI propagation on a
   brand-new cluster), run `pulumi up` again. Everything already created stays.
3. The infra pipeline's config-sync step restarts Gitea with the token.
   Meanwhile the runner retries registration every 5 seconds. If the runner logs
   keep saying the token is invalid after the pipeline finishes, the VM most
   likely read the secret before its IAM grant had propagated. Run `make sync`.
4. Check:
   ```bash
   make kube && make runners    # both runner pods Running, logs show each registered and declared
   ```
   `make kube` needs `gke-gcloud-auth-plugin`: `gcloud components install
   gke-gcloud-auth-plugin`, or `google-cloud-cli-gke-gcloud-auth-plugin` from
   the same package repository as a distro-packaged gcloud.
   Site Administration → Actions → Runners should list two runners as Idle:
   `gitea-runner-pods-0`, with labels `ubuntu-latest`, `ubuntu-24.04` and
   `ubuntu-22.04`, and `gitea-runner-0`, with label `docker`.
5. Run the smoke workflow below once.

### Smoke workflow

Put this in a scratch repository as `.gitea/workflows/smoke.yaml`:

```yaml
on: [push]
jobs:
  fan-out:                      # four job pods at once
    runs-on: ubuntu-latest      # required: runner 5.x fails jobs without runs-on
    strategy:
      matrix: { n: [1, 2, 3, 4] }
    steps:
      - uses: actions/checkout@v4
      - uses: actions/cache@v4
        with: { path: cached, key: smoke-${{ matrix.n }}-${{ github.run_id }}, restore-keys: smoke-${{ matrix.n }}- }
      - run: mkdir -p cached && date >> cached/stamp && cat cached/stamp
      - uses: actions/upload-artifact@v4
        with: { name: smoke-${{ matrix.n }}, path: cached }
      - name: metadata server must be unreachable
        run: |
          ! curl -sf -m 5 -H 'Metadata-Flavor: Google' \
            http://169.254.169.254/computeMetadata/v1/instance/service-accounts/default/email
      - name: API server must be unreachable
        run: "! curl -sk -m 5 https://kubernetes.default.svc/version"
      - name: egress IP
        run: curl -s https://api.ipify.org   # must equal `pulumi stack output runnerEgressIp`
      - run: sleep 120                     # hold the pods long enough to watch them

  docker:
    runs-on: docker
    steps:
      - uses: actions/checkout@v4
      - name: docker build
        run: |
          printf 'FROM alpine:3\nRUN echo built\n' > Dockerfile
          docker build -t smoke . && docker run --rm smoke echo ran
          docker info | grep -A1 'Registry Mirrors'    # expect mirror.gcr.io
      - name: metadata server must be unreachable
        run: |
          ! curl -sf -m 5 -H 'Metadata-Flavor: Google' \
            http://169.254.169.254/computeMetadata/v1/instance/service-accounts/default/email
```

While it runs, `make runners` should show the four `fan-out` pods in
`gitea-jobs` **at the same time**. Usually they land on more than one node, and
the node count goes up. Push twice: the second run's `cached/stamp` files
should have two lines. `make logs` should show **no** Coraza audit entries for
`/api/actions`, `/api/actions_pipeline` or `/twirp/`.

## Day-to-day

| Task | How |
|---|---|
| More parallel `ubuntu-*` jobs | `gitea:runnerJobCapacity`, and `gitea:runnerMaxNodes` (max 10) so they fit |
| Bigger or smaller job pods | `gitea:runnerJobCpu` / `gitea:runnerJobMemory` (requests; the memory limit is twice the request) |
| More parallel `docker` jobs | `gitea:runnerCapacity 2` (same node), or `gitea:runnerReplicas` (a node each, ≤ `runnerMaxNodes`) |
| On-demand instead of spot | `gitea:runnerSpot false`. The pool is replaced create-first, so the runners reschedule. |
| Bigger nodes | `gitea:runnerMachineType e2-standard-4` |
| Upgrade the runner | bump `gitea:runnerImageTag`; it must stay a `-dind-rootless` tag |
| Shell into a runner | `kubectl --context=gke_<p>_<zone>_gitea-runners -n gitea-runner exec -it gitea-runner-pods-0 -- sh` (or `gitea-runner-0`) |
| Watch job pods | `kubectl --context=… -n gitea-jobs get pods -o wide -w` |

**Scaling follows the job queue.** Each `ubuntu-*` job pod requests
`runnerJobCpu` and `runnerJobMemory`. When a job pod fits on no node, it waits
Pending and the autoscaler adds a node, up to `runnerMaxNodes`. A node is
removed about 10 minutes after its last job pod finishes. A running job pod
blocks its node's removal, so scale-down never kills a job. A job that needs a
new node waits for it to boot, and then for the node to pull the job image.
That takes a few minutes, compared with seconds on a warm node.

At most `runnerJobCapacity` jobs run at once. Size it to what `runnerMaxNodes`
nodes can actually hold (`kubectl describe nodes` shows allocatable and what
GKE's own pods take). Jobs beyond that capacity wait in Gitea's queue. Jobs
within it that find no room become Pending pods. Whether the runner waits for
such a pod indefinitely or eventually fails the job is not yet verified. Until
it is, keep the capacity within what the nodes can hold.

The pool's floor is `max(1, runnerReplicas)`. Pod anti-affinity gives each DinD
runner a node of its own, and the floor is at least 1 because the runners must
keep polling Gitea. The pod runner prefers the DinD runner's node, which the
pool keeps anyway.

**Spot preemption** gives a pod about 15 seconds, so a job running at the time
fails. Re-run it. **The pod runner is the runner for every `ubuntu-*` job**, so
if its own node is preempted, every running `ubuntu-*` job fails. A rollout of
the pod runner (a config change, a runner upgrade) waits up to 55 minutes for
running jobs to finish. `pulumi up` waits with it. On on-demand nodes, a drain (node upgrade or scale-down) lets
running jobs finish for up to 55 minutes. Node upgrades normally happen in the
Sunday 05:00–09:00 UTC window, alongside the VM's reboot window. GKE can still
push urgent security patches and auto-repairs outside it, and those interrupt
running jobs without warning, just as preemption does.

**Runner 5.x is tested upstream against current Gitea, not 1.27.** If
registration or job fetching misbehaves after a runner upgrade, roll
`gitea:runnerImageTag` back to the previous tag before debugging anything else.

## Rotating the registration token

Registered runners authenticate with their own `.runner` credentials, so
rotation never affects them; it only governs new registrations.

```bash
openssl rand -hex 32 | tr -d '\n' \
  | gcloud secrets versions add gitea-runner-registration-token --project <p> --data-file=-
make sync     # renders the new token file and restarts Gitea
```

Two Gitea behaviours make this a one-way street:

- **Never re-add a value the secret has held before.** When Gitea starts with a
  new preset token, it deactivates every older instance-level token, and it
  never reactivates one. Rotating A→B→A leaves A dead: Gitea only logs a
  warning and every new registration fails.
- **"Reset registration token" in the admin UI also kills the preset token**,
  permanently, across restarts. Use it only to revoke, then add a fresh secret
  version as above.

To revoke without replacing, click Reset in the admin UI. Removing the secret
does not revoke anything, because Gitea keeps the token active in its database.

## Turning it off

Two applies, in this order:

```bash
cd infra
pulumi config set gitea:runnerReplicas 0 && pulumi config set gitea:runnerJobCapacity 0 \
  && pulumi up                                            # 1. deletes the pods AND their PVC disks
pulumi config set gitea:runnersEnabled false && pulumi up # 2. deletes the cluster, NAT, subnet, IP
```

Step 1 matters because deleting a cluster does **not** delete PVC-backed disks.
Skipping it leaves orphaned PDs that keep billing. To find them:

```bash
gcloud compute disks list --filter='name~^pvc- AND -users:*'
```

The Kubernetes objects carry `DeletedWith(cluster)`, so step 2 does not try to
delete them one by one through an API whose token expired long ago.

Then remove stale runners from Site Administration → Actions → Runners. The
token file on the VM is left in place on purpose (see *Rotating* above). It is
harmless.

## Troubleshooting

**The first apply fails creating the Namespace or the SecretProviderClass.** A
brand-new cluster can report RUNNING a little before its API server, or the
Secret Manager add-on's CRD, is ready. Run `pulumi up` again; nothing is lost.

**`pulumi refresh` or `destroy` fails with 401 against the cluster.** The
Kubernetes provider's token in state has expired. Add `--run-program`, which
re-runs the program and fetches a fresh one. As a last resort for a cluster
that is already gone, set `PULUMI_K8S_DELETE_UNREACHABLE=true` for one run.

**Pod crashloops; the log mentions user namespaces.** Rootless dockerd needs
`/proc/sys/user/max_user_namespaces` > 0 on the node. COS_CONTAINERD on GKE
1.36 has it; check if a node image change ever breaks this:

```bash
kubectl --context=… debug node/<node> -it --image=busybox -- cat /proc/sys/user/max_user_namespaces
```

If it is 0, the rootless image cannot work on that node image. The fallback is
the rootful `docker:dind` native-sidecar pattern from the upstream
`examples/kubernetes/statefulset-dind.yaml`. A job escape then means root on
the node: the node SA is minimal and the metadata block still applies, but
update the threat model above to match.

**`docker build` in a job fails to reach the daemon.** Check whether the runner
log says "jobs get the daemon socket directly". The runner probes whether the
inner dockerd can see its temp directory, and falls back to mounting the
daemon socket directly if it cannot. If neither path works, it is the same
fallback as above.

**`actions/cache` never restores.** Jobs reach their runner's cache server at
the runner pod's IP on port 8088. For job pods, that is real pod-to-pod
traffic, allowed by the `gitea-jobs` policy (egress) and the
`gitea-runner-pods` policy (ingress). If it still fails, check which address
the runner advertises. If it is wrong, set `cache.host` explicitly in
`runnerYAML` (`infra/pkg/runner`) from a downward-API `POD_IP` environment
variable; the config expands `${NAME}`.

**The runner cannot reach Gitea at all** (`cannot ping the Gitea instance
server: deadline_exceeded`). Check DNS first:

```bash
kubectl --context=… -n gitea-runner exec gitea-runner-0 -- nslookup gitea.jasonmross.dev
```

A timeout here means the NetworkPolicy is not letting queries through. GKE
enables NodeLocal DNSCache on new clusters, so the kube-dns Service IP is
answered by the `node-local-dns` pod on the node. The DNS egress rule must
allow `k8s-app=node-local-dns` as well as `k8s-app=kube-dns`. If DNS works,
note that Cloud NAT does hairpin to the VM's external address (verified on
the first deploy). Look at the VPC firewall and fail2ban on the VM next.

**dockerd exits with `chmod /home/rootless/.local/share/docker: operation not
permitted`.** dockerd was given the PVC mount point itself as its data root.
That directory is owned by host root, which is unmapped inside rootlesskit's
user namespace. The data root must be a subdirectory that dockerd creates
itself: `data-root` in `daemon.json`, `dockerDataRoot` in `infra/pkg/runner`.

**`ubuntu-*` jobs stay "Waiting" or their pods stay Pending.** `kubectl -n
gitea-jobs describe pod <pod>` says why. `Insufficient cpu/memory` with the
pool already at `runnerMaxNodes` means more jobs are admitted than the nodes
hold: lower `runnerJobCapacity`, lower the job requests, or raise
`runnerMaxNodes`. A job pod rejected outright with `violates PodSecurity
"baseline"` means the runner built a pod spec baseline does not allow. That
should not happen; do not relax the namespace label without understanding what
asked for it.

**The pod runner cannot create job pods** (timeouts or `connection refused`
against `10.28.0.1:443` in its log). It must reach the API server, whose
private endpoint is a Private Service Connect address in the node subnet
(`kubectl get endpointslices -n default -l kubernetes.io/service-name=kubernetes`).
The `gitea-runner-pods` NetworkPolicy allows TCP 443 to the node subnet,
because Dataplane V2 evaluates policy after translating the Service address. If
the endpoint ever moves out of `10.20.0.0/24`, that rule must follow it.

**A job fails on the pod runner with "not supported".** It uses a Docker
action, a `docker://` step, or a job container's `options`, `volumes` or
`credentials`. Move it to `runs-on: docker`.
