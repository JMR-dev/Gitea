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
     snapshot) plus a `pulumi stack export`. From step 3 to the end, a
     placeholder pod holds the ops pool's node, so this Job and step 7's both
     start on a warm node with the engine image already pulled
  6. pulumi up, then the VM config sync
  7. verify: the release engine's status checks, then a preview that must show
     nothing left to change
  8. on success, this commit's tree becomes last-good; on a failure in 6-7, the
     last-good tree is re-applied (pulumi up, sync, verify) and the build fails

A rollback is a re-apply, not an undo: a replaced VM is replaced again, and
data is never restored here (scripts/ops/gitea_ops.py rollback does that, by
hand). Log lines starting GITEA_INFRA_ROLLBACK and GITEA_INFRA_ROLLBACK_FAILED
feed the alerting in cicd/pkg/alerting.

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

ROOT = os.path.normpath(os.path.join(os.path.dirname(os.path.abspath(__file__)), "..", ".."))
HERE = os.path.dirname(os.path.abspath(__file__))

# The infra stack's Kubernetes provider authenticates to the runner cluster with
# the gke-gcloud-auth-plugin exec plugin (infra/pkg/runner/runner.go), so both
# `pulumi preview` and `pulumi up` must find it on PATH. The pinned pulumi image
# ships it under the Cloud SDK's bin dir but does not put that dir on PATH, and
# it lives in a different place on a workstation; prepend whichever exists.
# Done here, not in cloudbuild/infra.yaml's step env, because a PATH set there
# would replace the image's PATH wholesale.
def ensure_auth_plugin_on_path():
    for d in ("/usr/lib/google-cloud-sdk/bin", "/usr/lib64/google-cloud-sdk/bin"):
        if os.path.isdir(d) and d not in os.environ.get("PATH", "").split(os.pathsep):
            os.environ["PATH"] = d + os.pathsep + os.environ.get("PATH", "")

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
    """The build-cache object for one dependency set and Go toolchain: infra's
    go.mod and go.sum, and the image's own Go. Those three fix the toolchain the
    build runs with too (go.mod can make Go download a newer one). The build
    runs with -trimpath, so the compile cache records no paths and still hits
    after being restored under a different temp dir; without it every cache key
    named the random dir, and nothing ever hit."""
    h = hashlib.sha256(b"trimpath-1\0" + go_version.encode())
    for part in (go_mod, go_sum):
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

    # --- Pulumi ---------------------------------------------------------------

    def pulumi(self, *args, cwd, capture=False, check=True):
        return run(["pulumi", *args, "--stack", self.a.stack, "--non-interactive"], cwd=cwd,
                   capture=capture, check=check)

    def prepare(self, src):
        infra = os.path.join(src, "infra")
        # Go's module and compile caches persist across deploys in the
        # build-cache bucket: compiling against pulumi-gcp from cold is the slow
        # part of an infra build. Restored once per run (a second prepare, or a
        # rollback's, builds against the same directory) and saved at most once.
        # Best-effort: a missing, broken or unreadable cache just means a cold
        # build, never a failed deploy. -trimpath is what lets the compile cache
        # hit at all, since the directory differs every run (go_cache_name).
        env = self.go_cache_env()
        self.restore_go_cache(infra)
        run(["go", "build", "-trimpath", "-o", "gitea-infra", "."], cwd=infra, env=env)
        self.save_go_cache()
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

    def go_cache_env(self):
        base = self.go_cache_dir()
        env = dict(os.environ)
        env["GOCACHE"], env["GOMODCACHE"] = os.path.join(base, "build"), os.path.join(base, "mod")
        for d in (env["GOCACHE"], env["GOMODCACHE"]):
            os.makedirs(d, exist_ok=True)
        return env

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
        try:
            if run(["tar", "-cf", tar, "-C", self.go_cache_dir(), "build", "mod"], check=False).returncode == 0 \
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

    def sync(self):
        run(["gcloud", "compute", "ssh", self.a.vm, f"--zone={self.a.zone}", f"--project={self.a.app_project}",
             "--tunnel-through-iap", "--quiet", "--command",
             "sudo systemctl start gitea-config-sync.service && sudo journalctl -u gitea-config-sync -n 40 --no-pager"])

    def apply(self, src, label):
        infra = self.prepare(src)
        self.applied = True
        self.pulumi("up", "--yes", "--diff", cwd=infra)
        self.sync()
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
            risky = destructive_steps(self.preview_json(infra))
            if risky:
                listing = "; ".join(f"{op} {t} {n}" for op, t, n in risky)
                if not self.a.approved:
                    raise Failed(f"needs approval, not deployed: {listing}")
                log(f"approved destructive changes: {listing}")
            self.ops("--yes", "backup")
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


def main():
    e = os.environ.get
    p = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    p.add_argument("mode", choices=["preview", "up"])
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
    a = p.parse_args()
    if a.mode == "up" and not (a.sha and a.state_bucket):
        p.error("up needs --sha and --state-bucket (_SHA, _DEPLOY_STATE_BUCKET)")
    ensure_auth_plugin_on_path()
    d = Deploy(a)
    try:
        sys.exit(d.preview() if a.mode == "preview" else d.up())
    except Failed as err:
        log(f"FAILED: {err}")
        sys.exit(1)


if __name__ == "__main__":
    main()
