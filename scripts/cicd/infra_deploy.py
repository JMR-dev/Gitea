#!/usr/bin/env python3
"""Deploy the app stack (infra/) from Cloud Build, with a safety net.

`preview` builds the program and runs `pulumi preview`. `up` does, in order:

  1. release gate: the code that can roll this deploy back must have passed
     drill D5 (scripts/cicd/release_gate.py), unless a person approved
  2. lock: one infra deploy at a time (gs://<deploy-state>/infra.lock)
  3. superseded check: a commit older than the last one deployed is skipped,
     so builds that finish out of order never put old infrastructure back
  4. guard: a replacement or deletion of the VM, a disk, a bucket, the
     cluster, an address or a DNS record needs approval; anything else goes
  5. backup: the release engine's backup Job (dump, config tarball, online
     snapshot -- a consistent one when an approved change replaces something)
     plus a `pulumi stack export`. From step 3 to the end, a placeholder pod
     holds the ops pool's node, so this Job and step 7's both start on a warm
     node with the engine image already pulled
  6. pulumi up, then the VM config (vm_sync): the tree being applied decides
     how. With vm/bootstrap.sh the VM pulls its own config, so the sync is
     `systemctl start gitea-config-sync` over SSH; with vm/ansible/site.yml
     the playbook is pushed over the same IAP SSH. A replaced VM is waited
     for first: SSH, then boot, then (after its config) HTTPS
  7. verify: the release engine's status checks, then a preview that must show
     nothing left to change
  8. on success, this commit's tree becomes last-good; on a failure in 6-7, the
     last-good tree is re-applied (pulumi up, sync, verify) and the build fails

A rollback is a re-apply, not an undo: a replaced VM is replaced again, and
data is never restored here (scripts/ops/gitea_ops.py rollback does that, by
hand). Log lines starting GITEA_INFRA_ROLLBACK and GITEA_INFRA_ROLLBACK_FAILED
feed the alerting in cicd/pkg/alerting.

`vm-check` and `vm-apply` run only the playbook (`--check --diff`, or for
real) against --vm from --tree: the pre-cutover proof against prod, and the
rehearsal VM. A tree with vm/ansible also has its checks run by `preview` and
`up` (ansible_checks).

Everything comes from the environment Cloud Build maps its substitutions into;
see cloudbuild/infra.yaml.
"""
import argparse
import datetime
import hashlib
import json
import os
import subprocess
import sys
import tarfile
import tempfile
import time
import urllib.request

ROOT = os.path.normpath(os.path.join(os.path.dirname(os.path.abspath(__file__)), "..", ".."))
HERE = os.path.dirname(os.path.abspath(__file__))

# The infra stack's Kubernetes provider authenticates to the runner cluster with
# the gke-gcloud-auth-plugin exec plugin (infra/pkg/runner/runner.go), so both
# `pulumi preview` and `pulumi up` must find it on PATH. The deploy step's
# gcloud image has it on PATH already; elsewhere it can sit under the Cloud
# SDK's bin dir without that dir being on PATH, so prepend whichever exists.
# Done here, not in cloudbuild/infra.yaml's step env, because a PATH set there
# would replace the image's PATH wholesale.
def ensure_auth_plugin_on_path():
    for d in ("/usr/lib/google-cloud-sdk/bin", "/usr/lib64/google-cloud-sdk/bin"):
        if os.path.isdir(d) and d not in os.environ.get("PATH", "").split(os.pathsep):
            os.environ["PATH"] = d + os.pathsep + os.environ.get("PATH", "")


# The deploy step runs on Google's gcloud image, which Cloud Build's workers
# already hold, so it pulls in about a second against ~100 s for pulumi/pulumi
# from Docker Hub. It has python3, git, ssh and the auth plugin, but neither Go
# nor Pulumi: --fetch-tools downloads those two, pinned, and checks each against
# the SHA-256 its publisher lists (go.dev/dl; Pulumi's checksums.txt, the same
# on get.pulumi.com and GitHub). A few seconds. Go only bootstraps the build: if
# infra/go.mod asks for a newer toolchain, Go fetches and verifies that itself.
# Pulumi stays on the version the stack was last deployed with; bump it here.
TOOLS = {  # name: (url, sha256, directory to put on PATH)
    "go": ("https://go.dev/dl/go1.26.9.linux-amd64.tar.gz",
           "42d158b4d8f7b61ac0a830567c940a86098fb7aac52e467a5ebec03ef5cc2f8d", "go/bin"),
    "pulumi": ("https://get.pulumi.com/releases/sdk/pulumi-v3.266.0-linux-x64.tar.gz",
               "05c42a7bf80b9aa0299773f4edd750eb8e1dcd8fbd52d99c3578f38a7de21262", "pulumi"),
}


def fetch_tools(dest):
    """Download, check and unpack TOOLS under dest, and put them first on PATH."""
    for name, (url, sha, bindir) in TOOLS.items():
        tgz = os.path.join(dest, f"{name}.tgz")
        for attempt in range(3):
            log(f"fetching {url}")
            try:
                h = hashlib.sha256()
                with urllib.request.urlopen(url, timeout=120) as r, open(tgz, "wb") as f:
                    for chunk in iter(lambda: r.read(1 << 20), b""):
                        h.update(chunk)
                        f.write(chunk)
                break
            except OSError as e:
                if attempt == 2:
                    raise Failed(f"could not download {url}: {e}")
                log(f"download failed ({e}); retrying")
                time.sleep(10)
        if h.hexdigest() != sha:
            raise Failed(f"{url} has sha256 {h.hexdigest()}, not the pinned {sha}")
        run(["tar", "-xzf", tgz, "-C", dest])
        os.remove(tgz)
        os.environ["PATH"] = os.path.join(dest, bindir) + os.pathsep + os.environ.get("PATH", "")

# Replacing or deleting one of these is an outage or a data loss: approval.
PROTECTED_TYPES = {
    "gcp:compute/instance:Instance",
    "gcp:compute/disk:Disk",
    "gcp:storage/bucket:Bucket",
    "gcp:container/cluster:Cluster",
    "gcp:compute/address:Address",
    "gcp:dns/recordSet:RecordSet",
}
DESTRUCTIVE_OPS = {"replace", "create-replacement", "delete-replaced", "delete", "discard"}
LOCK_WAIT = 45 * 60
LOCK_STALE = 4 * 3600
GO_CACHE_PREFIX = "infra-go-cache-"
SSH_KEY_TTL = "1h"  # as scripts/ops/gitea_ops.py: the sync's OS Login key
ANSIBLE_DIR = os.path.join("vm", "ansible")
PROD_VM = "gitea-vm"  # vm-apply refuses it without --approved
SSH_WAIT = 10 * 60    # a just-created VM: IAP, sshd and the OS Login agent come up
BOOT_WAIT = 15 * 60   # then systemd finishes booting
HTTPS_WAIT = 10 * 60  # then, after its config, Caddy gets a certificate


def tree_kind(src):
    """How a tree's VM config reaches the VM: "legacy" while it has
    vm/bootstrap.sh (the VM's startup script, which pulls the config itself),
    "ansible" once it has vm/ansible/site.yml instead, None for neither. A
    rollback can re-apply a tree of either kind, so this is asked of the tree
    being applied, never of ROOT."""
    if os.path.isfile(os.path.join(src, "vm", "bootstrap.sh")):
        return "legacy"
    if os.path.isfile(os.path.join(src, ANSIBLE_DIR, "site.yml")):
        return "ansible"
    return None


def nftables_restarts(src):
    """Lines in the tree's playbook that restart, reload or stop nftables. On
    EL the unit's ExecReload and ExecStop flush the whole ruleset -- netavark's
    DNAT and fail2ban's bans with it -- so the firewall is only ever loaded
    with `nft -f` (vm/bootstrap.sh setup_nftables). Returns "path:line" strings."""
    bad, base = [], os.path.join(src, ANSIBLE_DIR)
    for d, _, files in os.walk(base):
        for f in files:
            if not f.endswith((".yml", ".yaml", ".j2", ".sh")) and "." in f:
                continue
            p = os.path.join(d, f)
            try:
                with open(p, encoding="utf-8") as fh:
                    lines = fh.read().splitlines()
            except (OSError, UnicodeDecodeError):
                continue
            for i, line in enumerate(lines):
                s = line.strip()
                if "systemctl" in s and "nftables" in s and any(w in s.split() for w in ("restart", "reload", "stop", "try-restart", "reload-or-restart")):
                    bad.append(f"{os.path.relpath(p, src)}:{i + 1}")
                if s.split("#")[0].strip() in ("name: nftables", "name: nftables.service"):
                    near = " ".join(lines[max(0, i - 4): i + 5])
                    if any(f"state: {st}" in near for st in ("restarted", "reloaded", "stopped")):
                        bad.append(f"{os.path.relpath(p, src)}:{i + 1}")
    return bad


def log(msg):
    print(f"==> {msg}", flush=True)


class Failed(Exception):
    pass


class DrillFailure(Exception):
    pass


def run(cmd, *, cwd=ROOT, check=True, capture=False, stdin=None, env=None):
    log("$ " + " ".join(cmd))
    r = subprocess.run(cmd, cwd=cwd, input=stdin, text=True, env=env,
                       stdout=subprocess.PIPE if capture else None,
                       stderr=subprocess.PIPE if capture else None)
    if check and r.returncode != 0:
        raise Failed(f"{cmd[0]} {' '.join(cmd[1:3])} exited {r.returncode}"
                     + (f": {r.stderr.strip()[-800:]}" if capture and r.stderr else ""))
    return r


def destructive_steps(preview):
    """The (op, type, name) of every step in `pulumi preview --json` output that
    replaces or deletes a protected resource."""
    out = []
    for step in preview.get("steps") or []:
        op, urn = step.get("op", ""), step.get("urn", "")
        parts = urn.split("::")
        if len(parts) < 4:
            continue
        rtype, name = parts[2].split("$")[-1], parts[3]
        if op in DESTRUCTIVE_OPS and rtype in PROTECTED_TYPES:
            out.append((op, rtype, name))
    return out


def leftover_changes(preview):
    """Steps that would still change something. A provider's own update -- the
    kubernetes provider gets a fresh token every run -- is not a change."""
    out = []
    for step in preview.get("steps") or []:
        op, urn = step.get("op", ""), step.get("urn", "")
        if op in ("same", "read", "refresh"):
            continue
        if "::pulumi:providers:" in urn:
            continue
        out.append(f"{op} {urn.split('::')[-1]}")
    return out


def go_cache_name(go_mod, go_sum, go_version):
    """The build-cache object for one dependency set and toolchain: infra's
    go.mod and go.sum, the Go the build starts from, and the pinned TOOLS.
    Those fix the toolchain the build runs with too (go.mod can make Go
    download a newer one) and the Pulumi plugins the object also holds (the
    provider SDKs in go.sum name them). The build runs with -trimpath, so the
    compile cache records no paths and still hits after being restored under a
    different temp dir; without it every cache key named the random dir, and
    nothing ever hit."""
    h = hashlib.sha256(b"plugins-2\0" + go_version.encode())
    for part in (json.dumps(TOOLS, sort_keys=True).encode(), go_mod, go_sum):
        h.update(b"\0" + part)
    return f"{GO_CACHE_PREFIX}{h.hexdigest()[:16]}.tar"


def newest_object(ls_long):
    """The newest gs:// URL in `gcloud storage ls -l` output, or None."""
    best = None
    for line in ls_long.splitlines():
        parts = line.split()
        if len(parts) == 3 and parts[2].startswith("gs://") and (best is None or parts[1] > best[0]):
            best = (parts[1], parts[2])
    return best[1] if best else None


class Deploy:
    def __init__(self, a):
        self.a = a
        self.lockurl = f"gs://{a.state_bucket}/infra.lock" if a.state_bucket else None
        self.locked = False
        self.stamp = datetime.datetime.now(datetime.timezone.utc).strftime("%Y%m%dT%H%M%SZ")
        self.applied = False  # pulumi up has started: from here a failure rolls back
        # Outside the source tree, which is what gets archived as last-good.
        self.tmp = tempfile.mkdtemp(prefix="infra-deploy-")
        self.go_cache = None         # this go.sum's cache object, once restore has run
        self.go_cache_hit = False    # restored exactly that object
        self.go_cache_saved = False
        self.ops_warm = None         # (kube, pod path) while the warm-up pod exists
        self.ssh_pruned = False

    # --- Pulumi ---------------------------------------------------------------

    def pulumi(self, *args, cwd, capture=False, check=True):
        return run(["pulumi", *args, "--stack", self.a.stack, "--non-interactive"], cwd=cwd,
                   capture=capture, check=check)

    def prepare(self, src):
        infra = os.path.join(src, "infra")
        # Go's module and compile caches and Pulumi's plugins persist across
        # deploys in the build-cache bucket: compiling against pulumi-gcp from
        # cold is the slow part of an infra build, and the gcp and kubernetes
        # plugins are ~400 MB to download. Restored once per run (a second
        # prepare, or a rollback's, builds against the same directory) and saved
        # at most once, by up() after its first preview has fetched the plugins.
        # Best-effort: a missing, broken or unreadable cache just means a cold
        # build, never a failed deploy. -trimpath is what lets the compile cache
        # hit at all, since the directory differs every run (go_cache_name).
        self.use_cache_dirs()
        self.restore_go_cache(infra)
        run(["go", "build", "-trimpath", "-o", "gitea-infra", "."], cwd=infra)
        run(["pulumi", "login", f"gs://{self.a.app_project}-pulumi-state"], cwd=infra)
        return infra

    # --- Go build cache (best-effort; a cache error never fails the deploy) ---

    def cache_bucket(self):
        """The CI project's build-cache bucket, derived from the deploy-state
        bucket (both are <ci-project>-<name>). None without a state bucket."""
        sb, suffix = self.a.state_bucket, "-deploy-state"
        return sb[: -len(suffix)] + "-build-cache" if sb and sb.endswith(suffix) else None

    def go_cache_dir(self):
        return os.path.join(self.tmp, "gocache")

    def use_cache_dirs(self):
        """Point Go and Pulumi at the cache dir, for this whole process: Pulumi's
        Go language host reads the module cache too, to find which plugins the
        program needs, and installs them under PULUMI_HOME/plugins."""
        base = self.go_cache_dir()
        for var, sub in (("GOCACHE", "build"), ("GOMODCACHE", "mod"), ("PULUMI_HOME", "pulumi")):
            os.environ[var] = os.path.join(base, sub)
            os.makedirs(os.environ[var], exist_ok=True)

    def restore_go_cache(self, infra):
        if self.go_cache is not None:  # once per run
            return
        self.go_cache = ""
        bucket = self.cache_bucket()
        if not bucket:
            return
        try:
            # GOTOOLCHAIN=local: the image's Go, without downloading the one
            # go.mod may ask for (that download belongs in the cache dir).
            gov = run(["go", "env", "GOVERSION"], cwd=infra, check=False, capture=True,
                      env=dict(os.environ, GOTOOLCHAIN="local")).stdout.strip()
            with open(os.path.join(infra, "go.mod"), "rb") as fm, open(os.path.join(infra, "go.sum"), "rb") as fs:
                self.go_cache = go_cache_name(fm.read(), fs.read(), gov)
            exact = f"gs://{bucket}/{self.go_cache}"
            if run(["gcloud", "storage", "ls", exact], check=False, capture=True).returncode == 0:
                src, self.go_cache_hit = exact, True
            else:
                # None for this go.sum yet (a dependency bump): start from the
                # newest one, which still holds every module that did not change.
                ls = run(["gcloud", "storage", "ls", "-l", f"gs://{bucket}/{GO_CACHE_PREFIX}*.tar"],
                         check=False, capture=True)
                src = newest_object(ls.stdout) if ls.returncode == 0 else None
            if not src:
                log("go cache: nothing to restore (cold build)")
                return
            tar = os.path.join(self.tmp, "go-cache-in.tar")
            if run(["gcloud", "storage", "cp", src, tar], check=False, capture=True).returncode != 0 \
                    or run(["tar", "-xf", tar, "-C", self.go_cache_dir()], check=False).returncode != 0:
                self.go_cache_hit = False
                log("go cache: restore failed (cold build)")
                return
            os.remove(tar)
            log(f"go cache: restored ({'exact' if self.go_cache_hit else 'partial'}) from {src}")
        except Exception as e:  # caching must never fail the deploy
            self.go_cache_hit = False
            log(f"go cache: restore skipped ({e!r})")

    def save_go_cache(self):
        # Only a deploy writes it (a preview's account can only read), at most
        # once a run, and only when this go.sum had no object of its own: after
        # an exact hit there is nothing new worth the upload. Not compressed --
        # gzip of the cache took minutes, and the bucket is in the build's region.
        if self.a.mode != "up" or self.go_cache_saved or self.go_cache_hit or not self.go_cache:
            return
        self.go_cache_saved = True
        bucket = self.cache_bucket()
        if not bucket:
            return
        url, tar = f"gs://{bucket}/{self.go_cache}", os.path.join(self.tmp, "go-cache-out.tar")
        plugins = os.path.join("pulumi", "plugins")
        parts = ["build", "mod"] + ([plugins] if os.path.isdir(os.path.join(self.go_cache_dir(), plugins)) else [])
        try:
            if run(["tar", "-cf", tar, "-C", self.go_cache_dir(), *parts], check=False).returncode == 0 \
                    and run(["gcloud", "storage", "cp", tar, url], check=False).returncode == 0:
                log(f"go cache: saved to {url}")
            if os.path.exists(tar):
                os.remove(tar)
        except Exception as e:  # caching must never fail the deploy
            log(f"go cache: save skipped ({e!r})")

    # --- ops pool warm-up (best-effort, like the cache) -------------------------

    def warm_ops(self):
        """Hold a placeholder pod on the ops pool for the rest of the deploy.
        Scaling that pool up from zero (a node, then the engine image) took ~3
        minutes and happened twice a deploy: before the backup Job, and again
        before the verify Job because the node scaled back down during `pulumi
        up`. The placeholder runs the engine's own image on the engine Job's
        placement, so both Jobs start on a warm node with the image pulled. It
        is deleted at the end and expires by itself if the deploy dies first. A
        failure here only means the Jobs wait for the pool, as before."""
        try:
            if os.path.join(ROOT, "scripts", "ops") not in sys.path:
                sys.path.insert(0, os.path.join(ROOT, "scripts", "ops"))
            import launch_job as lj  # the engine Job's client, image and placement
            kube = lj.Kube(self.a.app_project, self.a.zone, "gitea-runners")
            pods = f"/api/v1/namespaces/{lj.NAMESPACE}/pods"
            made = kube.request("POST", pods, {
                "apiVersion": "v1", "kind": "Pod",
                "metadata": {
                    "generateName": "gitea-ops-warm-", "namespace": lj.NAMESPACE,
                    "labels": {"app": "gitea-ops-warm"},
                    # Keeps the node up while it runs, which is the point.
                    "annotations": {"cluster-autoscaler.kubernetes.io/safe-to-evict": "false"},
                },
                "spec": {
                    "automountServiceAccountToken": False,
                    "restartPolicy": "Never",
                    "activeDeadlineSeconds": lj.DEADLINE_SECONDS,
                    "terminationGracePeriodSeconds": 0,
                    # Exactly where launch_job.manifests puts the engine Job.
                    "nodeSelector": {lj.POOL_KEY: lj.POOL, "iam.gke.io/gke-metadata-server-enabled": "true"},
                    "tolerations": [{"key": lj.POOL_KEY, "operator": "Equal", "value": lj.POOL,
                                     "effect": "NoSchedule"}],
                    "containers": [{
                        "name": "warm", "image": lj.IMAGE, "command": ["sleep", "infinity"],
                        "resources": {"requests": {"cpu": "10m", "memory": "16Mi"}, "limits": {"memory": "64Mi"}},
                    }],
                },
            })
            name = made["metadata"]["name"]
            self.ops_warm = (kube, f"{pods}/{name}")
            log(f"ops pool: warming it with pod {lj.NAMESPACE}/{name}")
        except (Exception, SystemExit) as e:  # launch_job exits on API errors
            log(f"ops pool: warm-up skipped ({e!r})")

    def unwarm_ops(self):
        if not self.ops_warm:
            return
        kube, path = self.ops_warm
        self.ops_warm = None
        try:
            kube.request("DELETE", path, ok404=True)
            log("ops pool: warm-up pod deleted")
        except (Exception, SystemExit) as e:
            log(f"ops pool: warm-up pod not deleted ({e!r}); it expires by itself")

    def preview_json(self, infra):
        r = self.pulumi("preview", "--json", cwd=infra, capture=True)
        try:
            return json.loads(r.stdout)
        except ValueError as e:
            raise Failed(f"pulumi preview --json is not JSON: {e}")

    # --- deploy-state bucket --------------------------------------------------

    def read_state(self, name):
        r = run(["gcloud", "storage", "cat", f"gs://{self.a.state_bucket}/{name}"], check=False, capture=True)
        return r.stdout if r.returncode == 0 else None

    def lock(self):
        body = json.dumps({"build": self.a.build_id, "sha": self.a.sha, "at": time.time()})
        deadline = time.time() + LOCK_WAIT
        while True:
            r = run(["gcloud", "storage", "cp", "-", self.lockurl, "--if-generation-match=0"],
                    check=False, capture=True, stdin=body)
            if r.returncode == 0:
                self.locked = True
                log(f"holding {self.lockurl}")
                return
            held = self.read_state("infra.lock")
            try:
                info = json.loads(held or "{}")
            except ValueError:
                info = {}
            age = time.time() - float(info.get("at", time.time()))
            if age > LOCK_STALE:
                raise Failed(f"{self.lockurl} is {age / 3600:.1f}h old (build {info.get('build')}). If that "
                             "build is gone, check prod, then remove the lock by hand.")
            if time.time() > deadline:
                raise Failed(f"waited {LOCK_WAIT // 60} min for {self.lockurl} (build {info.get('build')})")
            log(f"another infra deploy holds the lock (build {info.get('build')}); waiting")
            time.sleep(30)

    def unlock(self):
        if self.locked:
            run(["gcloud", "storage", "rm", self.lockurl], check=False)
            self.locked = False

    def superseded(self):
        """True if a newer commit than this one has already been deployed."""
        raw = self.read_state("infra-last-good.json")
        if not raw:
            log("no last-good yet: this deploy has nothing to roll back to")
            return False
        last = json.loads(raw)["sha"]
        if last == self.a.sha:
            return False
        hist = os.path.join(self.tmp, "history")
        r = run(["git", "clone", "--quiet", "--filter=blob:none", "--no-checkout",
                 f"{self.a.gitea_url}/{self.a.repo}.git", hist], check=False, capture=True)
        if r.returncode != 0:
            log("WARNING: could not clone the repo to compare with last-good; going ahead")
            return False
        anc = lambda x, y: run(["git", "merge-base", "--is-ancestor", x, y], cwd=hist, check=False).returncode == 0
        if anc(last, self.a.sha):
            return False
        if anc(self.a.sha, last):
            return True
        if self.a.approved:
            log(f"WARNING: {self.a.sha} and last-good {last} have diverged; approved, going ahead")
            return False
        raise Failed(f"{self.a.sha} is neither ahead of nor behind last-good {last} (a force-push?); "
                     "deploy it by hand or approve")

    def save_last_good(self):
        tgz = os.path.join(self.tmp, "last-good.tgz")
        skip = {".git", "gitea-infra", "gitea-cicd", "__pycache__"}
        with tarfile.open(tgz, "w:gz") as t:
            t.add(ROOT, arcname="src", filter=lambda ti: None if os.path.basename(ti.name) in skip else ti)
        run(["gcloud", "storage", "cp", tgz, f"gs://{self.a.state_bucket}/infra-last-good.tgz"])
        run(["gcloud", "storage", "cp", "-", f"gs://{self.a.state_bucket}/infra-last-good.json"],
            stdin=json.dumps({"sha": self.a.sha, "build": self.a.build_id, "at": self.stamp}))
        log(f"last-good is now {self.a.sha}")

    # --- the VM and the release engine ----------------------------------------

    def ops(self, *engine_args):
        """A gitea_ops.py command as the release-engine Job, after any running one."""
        launcher = [sys.executable, os.path.join(ROOT, "scripts/ops/launch_job.py"),
                    "--project", self.a.app_project, "--zone", self.a.zone]
        deadline = time.time() + 2 * 3600
        while True:
            st = run(launcher + ["status"], check=False, capture=True).stdout.strip()
            if not st.startswith(("active", "pending")):
                break
            if time.time() > deadline:
                raise Failed("a release-engine Job has been running for 2h: " + st)
            log("waiting for the running release-engine Job: " + st)
            time.sleep(30)
        r = run(launcher + ["run", "--", "--region", self.a.region, "--zone", self.a.zone, *engine_args], check=False)
        if r.returncode != 0:
            raise Failed(f"gitea_ops.py {engine_args[-1] if engine_args else ''} failed (exit {r.returncode})")

    def ssh(self, command, *, check=True, capture=False):
        return run(["gcloud", "compute", "ssh", self.a.vm, f"--zone={self.a.zone}", f"--project={self.a.app_project}",
                    "--tunnel-through-iap", "--quiet", f"--ssh-key-expire-after={SSH_KEY_TTL}",
                    "--command", command], check=check, capture=capture)

    def instance_id(self):
        """The VM's numeric id, or "" if there is none. A different id after
        `pulumi up` means the VM was replaced."""
        r = run(["gcloud", "compute", "instances", "describe", self.a.vm, f"--zone={self.a.zone}",
                 f"--project={self.a.app_project}", "--format=value(id)"], check=False, capture=True)
        return r.stdout.strip() if r.returncode == 0 else ""

    def prime_ssh(self, wait):
        """One SSH that registers this run's OS Login key (good for SSH_KEY_TTL),
        records the VM's host key in gcloud's known-hosts file under
        compute.<instance id>, and returns the OS Login user -- what Ansible's
        own SSH then uses. Done before every playbook run, since a run can
        outlive a key. A VM that was just created refuses for a while (IAP
        4003, connection refused, publickey until the guest agent is up), so
        with `wait` this retries for SSH_WAIT."""
        self.prune_ssh_keys()
        deadline = time.time() + (SSH_WAIT if wait else 0)
        while True:
            r = self.ssh("id -un", check=False, capture=True)
            user = r.stdout.strip().splitlines()[-1] if r.returncode == 0 and r.stdout.strip() else ""
            if user:
                return user
            if time.time() >= deadline:
                raise Failed(f"cannot SSH to {self.a.vm}: {(r.stderr or '').strip()[-400:]}")
            log(f"{self.a.vm} not reachable over SSH yet; retrying")
            time.sleep(15)

    def wait_booted(self):
        """Until systemd has finished booting. degraded counts: one failed unit
        must not stop the config that may fix it."""
        r = self.ssh(f"timeout {BOOT_WAIT} systemctl is-system-running --wait; true", check=False, capture=True)
        state = (r.stdout or "").strip().splitlines()[-1:] or [""]
        if state[0] not in ("running", "degraded"):
            raise Failed(f"{self.a.vm} did not finish booting: {state[0] or r.stderr.strip()[-300:]}")
        log(f"{self.a.vm} is up ({state[0]})")

    def vm_sync(self, src, replaced=False):
        """Apply the VM config of the tree at src (see tree_kind)."""
        kind = tree_kind(src)
        if kind is None:
            raise Failed(f"{src} has neither vm/bootstrap.sh nor {ANSIBLE_DIR}/site.yml: no way to configure the VM")
        self.prime_ssh(wait=replaced)
        self.wait_booted()
        if kind == "legacy":
            # The startup script (vm/bootstrap.sh in full mode) installs
            # gitea-config-sync.service; on a VM that was just created it is
            # still running, and starting the sync before it finishes either
            # fails (no such unit) or races it. Once it has finished, the unit
            # is there or never will be (it skips the units when the secrets
            # are unreadable).
            self.ssh(f"timeout {BOOT_WAIT} sh -c 'while [ \"$(systemctl show -p ActiveState --value "
                     "google-startup-scripts.service)\" = activating ]; do sleep 10; done' && "
                     "{ [ -f /etc/systemd/system/gitea-config-sync.service ] || "
                     "{ echo 'the startup script finished without installing gitea-config-sync.service' >&2; "
                     "exit 1; }; }")
            self.ssh("sudo systemctl start gitea-config-sync.service && "
                     "sudo journalctl -u gitea-config-sync -n 40 --no-pager")
            return
        self.playbook(src, check=False)

    # --- Ansible (the VM config of a tree with vm/ansible) ----------------------

    def ansible_env(self, src):
        """ansible-core, installed from the tree's own hash-locked requirements
        into a directory keyed on that file -- outside the tree, which is what
        save_last_good archives -- and the environment that runs it. A
        rollback's tree may pin a different version, hence per tree. pip, not
        a venv: the deploy image's Python has no ensurepip; --target also keeps
        Debian's externally-managed marker out of it."""
        req = os.path.join(src, ANSIBLE_DIR, "requirements.txt")
        with open(req, "rb") as f:
            target = os.path.join(self.tmp, "ansible-" + hashlib.sha256(f.read()).hexdigest()[:16])
        if not os.path.isdir(os.path.join(target, "bin")):
            run([sys.executable, "-m", "pip", "install", "--quiet", "--disable-pip-version-check",
                 "--root-user-action=ignore", "--no-cache-dir", "--require-hashes", "--only-binary=:all:", "--no-deps",
                 "--target", target, "-r", req])
        env = dict(os.environ)
        env["PYTHONPATH"] = os.pathsep.join(p for p in (target, env.get("PYTHONPATH", "")) if p)
        env["PATH"] = os.path.join(target, "bin") + os.pathsep + env.get("PATH", "")
        env.update({
            # Ansible ignores an ansible.cfg in a world-writable directory,
            # and Cloud Build's /workspace can be one: name it explicitly.
            "ANSIBLE_CONFIG": os.path.join(src, ANSIBLE_DIR, "ansible.cfg"),
            "ANSIBLE_HOME": os.path.join(self.tmp, "ansible-home"),
            "ANSIBLE_LOCAL_TEMP": os.path.join(self.tmp, "ansible-tmp"),
            "LC_ALL": "C.UTF-8",
            "PYTHONDONTWRITEBYTECODE": "1",
        })
        return env

    def playbook(self, src, *, check):
        """site.yml against --vm, over the IAP SSH that prime_ssh just set up.
        An apply is idempotent, so one failure is retried once (usually a
        package mirror) before the deploy gives up and rolls back."""
        user = self.prime_ssh(wait=False)
        env = self.ansible_env(src)
        env.update({"GITEA_VM": self.a.vm, "GITEA_ZONE": self.a.zone, "GITEA_PROJECT": self.a.app_project,
                    "GITEA_SSH_USER": user, "GITEA_INSTANCE_ID": self.instance_id()})
        base = os.path.join(src, ANSIBLE_DIR)
        cmd = ["ansible-playbook", "-i", os.path.join(base, "inventory.yml"), os.path.join(base, "site.yml"), "--diff"]
        if check:
            cmd.append("--check")
        for attempt in (1, 2):
            if run(cmd, cwd=base, env=env, check=False).returncode == 0:
                return
            if check or attempt == 2:
                raise Failed(f"ansible-playbook {'--check ' if check else ''}against {self.a.vm} failed")
            log("ansible-playbook failed; retrying once")
            self.prime_ssh(wait=False)

    def ansible_checks(self, src):
        """What a tree's playbook must pass before it goes anywhere near the VM:
        the nftables rule above, a syntax check, and the tree's own tests
        (vm/ansible/tests/check: the golden renders). Run by preview and by up
        before the backup, so a broken playbook fails a PR, not a deploy."""
        if not os.path.isdir(os.path.join(src, ANSIBLE_DIR)):
            return
        bad = nftables_restarts(src)
        if bad:
            raise Failed("the playbook restarts, reloads or stops nftables (flushes every ruleset; "
                         "load it with `nft -f`): " + ", ".join(bad))
        env = self.ansible_env(src)
        # Syntax only: the inventory's values are never used.
        env.update({"GITEA_VM": "syntax-check", "GITEA_ZONE": "z", "GITEA_PROJECT": "p",
                    "GITEA_SSH_USER": "u", "GITEA_INSTANCE_ID": "0"})
        base = os.path.join(src, ANSIBLE_DIR)
        run(["ansible-playbook", "--syntax-check", "-i", os.path.join(base, "inventory.yml"),
             os.path.join(base, "site.yml")], cwd=base, env=env)
        tests = os.path.join(base, "tests", "check")
        if os.path.isfile(tests):
            run([tests], cwd=base, env=env)

    def wait_https(self):
        """After a replaced VM's config: until Gitea answers through Caddy with
        a certificate that verifies -- DNS-01 issuance takes a minute or two --
        so the release engine's verify does not fail on a cert still coming."""
        url = self.a.gitea_url.rstrip("/") + "/api/healthz"
        deadline, last = time.time() + HTTPS_WAIT, None
        while True:
            try:
                with urllib.request.urlopen(url, timeout=15) as r:
                    if r.status == 200:
                        log(f"{url} answers with a valid certificate")
                        return
            except OSError as e:  # includes TLS and HTTP errors
                last = e
            if time.time() >= deadline:
                raise Failed(f"{url} not healthy {HTTPS_WAIT // 60} min after the VM was replaced: {last!r}")
            time.sleep(15)

    def prune_ssh_keys(self):
        """Every build has a fresh HOME, so the sync's SSH registers a new OS
        Login key each run; before SSH_KEY_TTL they never expired and piled up
        on cb-infra's profile until the VM would refuse them. Clear the stale
        ones once a run, by the engine's rule (gitea_ops.stale_ssh_keys). Only
        one infra deploy runs at a time (the lock). Best-effort, like the cache."""
        if self.ssh_pruned:
            return
        self.ssh_pruned = True
        try:
            if os.path.join(ROOT, "scripts", "ops") not in sys.path:
                sys.path.insert(0, os.path.join(ROOT, "scripts", "ops"))
            import gitea_ops
            r = run(["gcloud", "compute", "os-login", "ssh-keys", "list", "--format=json"], check=False, capture=True)
            keys = json.loads(r.stdout or "[]") if r.returncode == 0 else []
            stale = gitea_ops.stale_ssh_keys(keys, time.time() * 1e6)
            for fp in stale:
                run(["gcloud", "compute", "os-login", "ssh-keys", "remove", f"--key={fp}"], check=False, capture=True)
            log(f"ssh: removed {len(stale)} stale OS Login key(s) of {len(keys)}")
        except Exception as e:  # SSH still gets to try
            log(f"ssh: could not prune OS Login keys ({e!r}); trying SSH anyway")

    def apply(self, src, label):
        infra = self.prepare(src)
        before = self.instance_id()
        self.applied = True
        self.pulumi("up", "--yes", "--diff", cwd=infra)
        # `pulumi up` returns once a new VM is RUNNING, long before it boots.
        after = self.instance_id()
        replaced = after != before
        if replaced:
            log(f"{self.a.vm} was replaced (instance {before or 'none'} -> {after or 'none'})")
        self.vm_sync(src, replaced)
        if replaced:
            self.wait_https()
        self.ops("verify")
        left = leftover_changes(self.preview_json(infra))
        if left:
            raise Failed(f"{label}: still not converged after up: " + ", ".join(left[:10]))

    def rollback(self, reason):
        log(f"GITEA_INFRA_ROLLBACK sha={self.a.sha} reason={reason}")
        try:
            tgz, dst = os.path.join(self.tmp, "rollback.tgz"), os.path.join(self.tmp, "lastgood")
            r = run(["gcloud", "storage", "cp", f"gs://{self.a.state_bucket}/infra-last-good.tgz", tgz],
                    check=False, capture=True)
            if r.returncode != 0:
                raise Failed("there is no last-good tree to re-apply")
            with tarfile.open(tgz) as t:
                try:
                    t.extractall(dst, filter="data")
                except TypeError:  # Python before 3.11.4 has no filter; the tarball is our own
                    t.extractall(dst)
            last = json.loads(self.read_state("infra-last-good.json") or "{}").get("sha", "?")
            log(f"re-applying last-good {last}")
            self.apply(os.path.join(dst, "src"), "rollback")
            log(f"rolled back to {last}")
            return True
        except Exception as e:  # report, never mask: the alert line must go out
            log(f"GITEA_INFRA_ROLLBACK_FAILED sha={self.a.sha} reason={e!r} -- manual recovery: docs/runbook.md")
            return False

    # --- modes ------------------------------------------------------------------

    def preview(self):
        self.ansible_checks(ROOT)
        infra = self.prepare(ROOT)
        self.pulumi("preview", "--diff", cwd=infra)
        gate = run([sys.executable, os.path.join(HERE, "release_gate.py"), "check", "infra",
                    "--bucket", self.a.state_bucket], check=False) if self.a.state_bucket else None
        if gate and gate.returncode != 0:
            log("note: merging this as it stands would be refused by the release gate until D5 passes")
        return 0

    def up(self):
        if self.a.drill and os.environ.get("GITEA_DRILL_OK") != "1":
            raise Failed("_DRILL needs GITEA_DRILL_OK=1 (set only by `make ci-drill`)")
        if not self.a.drill:
            gate = [sys.executable, os.path.join(HERE, "release_gate.py"), "check", "infra",
                    "--bucket", self.a.state_bucket] + (["--approved"] if self.a.approved else [])
            if run(gate, check=False).returncode != 0:
                return 1
        self.lock()
        try:
            if self.superseded():
                log(f"{self.a.sha} is older than the last deployed commit; nothing to do")
                return 0
            # Before the build, so the pool scales up while it compiles.
            self.warm_ops()
            infra = self.prepare(ROOT)
            plan = self.preview_json(infra)
            self.save_go_cache()  # after a preview, which has fetched the plugins
            risky = destructive_steps(plan)
            if risky:
                listing = "; ".join(f"{op} {t} {n}" for op, t, n in risky)
                if not self.a.approved:
                    raise Failed(f"needs approval, not deployed: {listing}")
                log(f"approved destructive changes: {listing}")
            self.ansible_checks(ROOT)
            # A replacement is the one change whose rollback can need the data
            # disk as it was: snapshot it with Gitea stopped, not online.
            self.ops("--yes", "backup", *(["--snapshot", "consistent"] if risky else []))
            exp = self.pulumi("stack", "export", cwd=infra, capture=True)
            run(["gcloud", "storage", "cp", "-",
                 f"gs://{self.a.app_project}-gitea-backups/config/{self.stamp}-infra/stack.json"], stdin=exp.stdout)
            try:
                self.apply(ROOT, "deploy")
                if self.a.drill == "fail-verify":
                    raise DrillFailure("drill fail-verify: injected failure after a passing verify")
            except Exception as e:  # anything at all once prod is being changed
                if not self.applied:
                    raise
                ok = self.rollback(repr(e))
                if self.a.drill == "fail-verify" and isinstance(e, DrillFailure) and ok:
                    log("DRILL infra fail-verify: PASS")
                    run([sys.executable, os.path.join(HERE, "release_gate.py"), "attest", "infra",
                         "--bucket", self.a.state_bucket, "--drill", "fail-verify",
                         "--build", self.a.build_id, "--sha", self.a.sha])
                    return 0
                if self.a.drill:
                    log(f"DRILL infra {self.a.drill}: FAIL")
                return 1 if ok else 2
            self.save_last_good()
            return 0
        finally:
            self.unwarm_ops()
            self.unlock()

    def vm(self, check):
        """vm-check / vm-apply: only the playbook, from --tree, against --vm.
        An apply to prod outside a deploy needs --approved, and holds the infra
        lock (when there is a state bucket) so it never overlaps a deploy."""
        src = os.path.abspath(self.a.tree or ROOT)
        if tree_kind(src) != "ansible":
            if not check and tree_kind(src) == "legacy":
                # The rollback rehearsal: the old tree's config sync, waited for.
                self.vm_sync(src, replaced=True)
                return 0
            raise Failed(f"{src} has no {ANSIBLE_DIR}/site.yml")
        self.ansible_checks(src)
        if check:
            self.playbook(src, check=True)
            return 0
        if self.a.vm == PROD_VM and not self.a.approved:
            raise Failed(f"vm-apply to {PROD_VM} needs --approved (a deploy applies it by itself)")
        if self.lockurl:
            self.lock()
        try:
            self.playbook(src, check=False)
        finally:
            self.unlock()
        return 0


def main():
    e = os.environ.get
    p = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    p.add_argument("mode", choices=["preview", "up", "vm-check", "vm-apply"])
    p.add_argument("--tree", default=e("_TREE", ""), help="vm-check/vm-apply: the tree whose playbook runs "
                   "(default: this checkout)")
    p.add_argument("--sha", default=e("_SHA") or e("COMMIT_SHA", ""))
    p.add_argument("--build-id", default=e("BUILD_ID", "local"))
    p.add_argument("--app-project", default=e("_APP_PROJECT", "gitea-496920"))
    p.add_argument("--region", default=e("_REGION", "us-east1"))
    p.add_argument("--zone", default=e("_ZONE", "us-east1-b"))
    p.add_argument("--vm", default=e("_VM", "gitea-vm"))
    p.add_argument("--stack", default=e("_STACK", "prod"))
    p.add_argument("--state-bucket", default=e("_DEPLOY_STATE_BUCKET", ""))
    p.add_argument("--gitea-url", default=e("_GITEA_URL", "https://gitea.jasonmross.dev"))
    p.add_argument("--repo", default=e("_GITEA_REPO", "JMR-dev/Gitea"))
    p.add_argument("--approved", action="store_true", default=e("_APPROVED") == "true")
    p.add_argument("--drill", default=e("_DRILL", ""), choices=["", "fail-verify"])
    p.add_argument("--fetch-tools", action="store_true",
                   help="download the pinned Go and Pulumi (TOOLS) first; cloudbuild/infra.yaml sets it")
    a = p.parse_args()
    if a.mode == "up" and not (a.sha and a.state_bucket):
        p.error("up needs --sha and --state-bucket (_SHA, _DEPLOY_STATE_BUCKET)")
    ensure_auth_plugin_on_path()
    try:
        if a.fetch_tools:
            fetch_tools(tempfile.mkdtemp(prefix="infra-tools-"))
        d = Deploy(a)
        if a.mode in ("vm-check", "vm-apply"):
            sys.exit(d.vm(check=a.mode == "vm-check"))
        sys.exit(d.preview() if a.mode == "preview" else d.up())
    except Failed as err:
        log(f"FAILED: {err}")
        sys.exit(1)


if __name__ == "__main__":
    main()
