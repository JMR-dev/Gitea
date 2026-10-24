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
     snapshot) plus a `pulumi stack export`
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


class Deploy:
    def __init__(self, a):
        self.a = a
        self.lockurl = f"gs://{a.state_bucket}/infra.lock" if a.state_bucket else None
        self.locked = False
        self.stamp = datetime.datetime.now(datetime.timezone.utc).strftime("%Y%m%dT%H%M%SZ")
        self.applied = False  # pulumi up has started: from here a failure rolls back
        # Outside the source tree, which is what gets archived as last-good.
        self.tmp = tempfile.mkdtemp(prefix="infra-deploy-")

    # --- Pulumi ---------------------------------------------------------------

    def pulumi(self, *args, cwd, capture=False, check=True):
        return run(["pulumi", *args, "--stack", self.a.stack, "--non-interactive"], cwd=cwd,
                   capture=capture, check=check)

    def prepare(self, src):
        infra = os.path.join(src, "infra")
        # Persist Go's module and compile caches across deploys: compiling
        # against pulumi-gcp from cold is the slow part of an infra build. This
        # is best-effort -- a missing, broken, or unreachable cache (a preview's
        # account cannot read it) just means a cold build, never a failed
        # deploy -- so only bother on a real `up`.
        build_env = None
        if self.a.mode == "up":
            build_env = self.go_cache_env()
            self.restore_go_cache()
        run(["go", "build", "-o", "gitea-infra", "."], cwd=infra, env=build_env)
        if build_env is not None:
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

    def restore_go_cache(self):
        bucket = self.cache_bucket()
        if not bucket:
            return
        url, tar = f"gs://{bucket}/infra-go-cache.tar.gz", os.path.join(self.tmp, "go-cache-in.tgz")
        try:
            if run(["gcloud", "storage", "cp", url, tar], check=False, capture=True).returncode != 0:
                log("go cache: nothing to restore (cold build)")
                return
            run(["tar", "-xzf", tar, "-C", self.go_cache_dir()], check=False)
            log(f"go cache: restored from {url}")
        except Exception as e:  # caching must never fail the deploy
            log(f"go cache: restore skipped ({e!r})")

    def save_go_cache(self):
        bucket = self.cache_bucket()
        if not bucket:
            return
        url, tar = f"gs://{bucket}/infra-go-cache.tar.gz", os.path.join(self.tmp, "go-cache-out.tgz")
        try:
            run(["tar", "-czf", tar, "-C", self.go_cache_dir(), "build", "mod"], check=False)
            run(["gcloud", "storage", "cp", tar, url], check=False)
            log(f"go cache: saved to {url}")
        except Exception as e:  # caching must never fail the deploy
            log(f"go cache: save skipped ({e!r})")

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
