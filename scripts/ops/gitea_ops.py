#!/usr/bin/env python3
"""Release engine for the Gitea VM: backups, releases with automatic rollback,
data restores, and the drills that prove them.

One implementation for both callers: an operator on a workstation (ADC) and
Cloud Build (the build service account). It shells out to gcloud for
everything it changes, and runs VM-side steps by shipping vm-steps.sh over IAP
SSH, so the VM itself needs nothing installed.

Subcommands:
  backup         gitea dump + config tarball + exports + data-disk snapshot
  release        promote new images to :prod with backup, status checks and
                 automatic rollback (and data restore when the DB may have
                 been migrated)
  rollback       put back the images and, optionally, the data recorded in a
                 release manifest
  verify         the status checks on their own
  drill-restore  prove a snapshot and a dump each restore to a working Gitea,
                 in an isolated container, without touching prod

Anything that changes something needs --yes; --dry-run prints what would run.
Every backup and release writes a JSON manifest to
gs://<backup-bucket>/config/<stamp>/manifest.json, once, when it finishes.
A release also leaves progress-NN-<step>.json there after each step; if the
engine dies, the newest one stands in for the manifest.

Log lines starting GITEA_RELEASE_ROLLBACK, GITEA_RELEASE_ROLLBACK_FAILED and
GITEA_RESTORE_DISK_LEFT_ATTACHED are alerting hooks; keep them stable.

Usage: see `gitea_ops.py <subcommand> --help`.
"""
import argparse
import base64
import datetime
import json
import os
import re
import shlex
import subprocess
import sys
import time
import urllib.error
import urllib.request

HERE = os.path.dirname(os.path.abspath(__file__))
VM_STEPS = os.path.join(HERE, "vm-steps.sh")

# Secret ids whose *version numbers* go into the manifest -- never the values.
SECRETS = [
    "gitea-secret-key",
    "gitea-internal-token",
    "gitea-oauth2-jwt-secret",
    "gitea-lfs-jwt-secret",
    "gitea-runner-registration-token",
    "gitea-acme-email",
    "pulumi-config-passphrase",
]

IMAGES = ("gitea", "caddy")
# Printed on the VM right before a step runs; see Ops.vm.
VM_MARK = "__GITEA_VM_STEP_OUTPUT__"
# How long the OS Login key each Job registers stays valid. Every Job pod makes
# a key of its own, and without an expiry they piled up on the account's OS
# Login profile until the VM refused them. `gcloud compute ssh` re-registers
# an expired key, so a Job that outlives it is unaffected.
SSH_KEY_TTL = "1h"


def stale_ssh_keys(listing, now_usec):
    """Fingerprints to remove from `gcloud compute os-login ssh-keys list
    --format=json` output: keys that never expire (left by Jobs before
    SSH_KEY_TTL) or already have. A live expiring key is kept."""
    return [k["key"] for k in listing
            if int((k.get("value") or {}).get("expirationTimeUsec") or 0) < now_usec]
VERSION_LABEL = "org.opencontainers.image.version"
ALPINE_LABEL = "dev.jasonmross.alpine.minor"


class Abort(Exception):
    """A planned stop: something is not in a state to proceed."""


class DrillFailure(Exception):
    """An injected failure, so a drill exercises the rollback path."""


def log(msg):
    print(f"==> {msg}", file=sys.stderr, flush=True)


def utcnow():
    return datetime.datetime.now(datetime.timezone.utc)


class Ops:
    def __init__(self, args):
        self.a = args
        self.stamp = utcnow().strftime("%Y%m%dT%H%M%SZ")
        self.ar = f"{args.region}-docker.pkg.dev/{args.project}/{args.repo}"
        self.manifest = {
            "stamp": self.stamp,
            "command": args.cmd,
            "project": args.project,
            "build": os.environ.get("BUILD_ID", ""),
            "commit": os.environ.get("COMMIT_SHA") or git_head(),
        }

    # -- plumbing -----------------------------------------------------------

    def run(self, cmd, *, mutate=False, capture=True, check=True, stdin=None):
        """Run a command. Mutating commands are skipped under --dry-run."""
        if mutate and self.a.dry_run:
            log("DRY-RUN: " + shlex.join(cmd))
            return ""
        print("+ " + shlex.join(cmd), file=sys.stderr, flush=True)
        r = subprocess.run(cmd, input=stdin, capture_output=capture, text=True)
        if check and r.returncode != 0:
            if capture:
                sys.stderr.write(r.stderr)
            raise subprocess.CalledProcessError(r.returncode, cmd, r.stdout, r.stderr)
        return (r.stdout or "").strip() if capture else ""

    def gcloud(self, *args, **kw):
        return self.run(["gcloud", *args, f"--project={self.a.project}"], **kw)

    def ensure_ssh_key(self):
        """Create gcloud's SSH key up front, quietly. Otherwise the first
        `gcloud compute ssh` in a fresh HOME (every Job pod has one) runs
        ssh-keygen itself, and its "Generating public/private..." banner lands
        on stdout -- where it was once read as a container digest."""
        key = os.path.expanduser("~/.ssh/google_compute_engine")
        if not os.path.exists(key):
            os.makedirs(os.path.dirname(key), mode=0o700, exist_ok=True)
            self.run(["ssh-keygen", "-q", "-t", "rsa", "-b", "3072", "-N", "", "-C", "gitea-ops", "-f", key])
            self.prune_ssh_keys()

    def prune_ssh_keys(self):
        """Clear the stale OS Login keys earlier Jobs left on this account, once
        per Job, before its first SSH (stale_ssh_keys says which). Only one
        engine Job runs at a time (launch_job.py refuses a second), so none of
        them is in use. Account housekeeping that SSH depends on, so it runs
        under --dry-run too. Best-effort: whatever fails, SSH still tries."""
        try:
            keys = json.loads(self.run(["gcloud", "compute", "os-login", "ssh-keys", "list",
                                        "--format=json"]) or "[]")
            stale = stale_ssh_keys(keys, time.time() * 1e6)
            for fp in stale:
                self.run(["gcloud", "compute", "os-login", "ssh-keys", "remove", f"--key={fp}"], check=False)
            log(f"ssh: removed {len(stale)} stale OS Login key(s) of {len(keys)}")
        except (subprocess.CalledProcessError, ValueError, KeyError, TypeError) as e:
            log(f"ssh: could not prune OS Login keys ({e}); trying SSH anyway")

    def vm(self, step, *args, mutate=True):
        """Run one vm-steps.sh step on the VM as root; returns its stdout.

        Only what the step prints after VM_MARK counts: anything gcloud, ssh
        or the login shell writes to stdout before it is not the step's."""
        with open(VM_STEPS, "rb") as f:
            payload = base64.b64encode(f.read()).decode()
        remote = (
            "t=$(mktemp) && echo " + payload + " | base64 -d > \"$t\" && echo " + VM_MARK + " && "
            "sudo bash \"$t\" " + shlex.join([step, *args]) + "; rc=$?; rm -f \"$t\"; exit $rc"
        )
        if mutate and self.a.dry_run:
            log(f"DRY-RUN: vm-steps {shlex.join([step, *args])}")
            return ""
        log(f"vm: {shlex.join([step, *args])}")
        self.ensure_ssh_key()
        r = subprocess.run(
            ["gcloud", "compute", "ssh", self.a.vm, f"--zone={self.a.zone}",
             f"--project={self.a.project}", "--tunnel-through-iap", "--quiet",
             f"--ssh-key-expire-after={SSH_KEY_TTL}",
             "--ssh-flag=-oServerAliveInterval=30", f"--command={remote}"],
            capture_output=True, text=True,
        )
        # The step's own progress goes to its stderr; pass it through.
        sys.stderr.write(r.stderr)
        if r.returncode != 0:
            raise subprocess.CalledProcessError(r.returncode, ["vm-steps", step, *args], r.stdout, r.stderr)
        out = r.stdout
        if VM_MARK not in out:
            raise subprocess.CalledProcessError(1, ["vm-steps", step, *args], out, "no output marker from the VM")
        return out.rsplit(VM_MARK, 1)[1].strip()

    def require_yes(self):
        if not (self.a.yes or self.a.dry_run):
            raise Abort("this changes things: pass --yes (or --dry-run to see the plan)")

    def save_manifest(self, *, quiet=False, name="manifest.json"):
        """Every write is a new object: the engine may create objects in the
        backup bucket but not overwrite them (an overwrite needs
        storage.objects.delete). So each run writes manifest.json once, with
        its outcome, and each checkpoint gets a name of its own."""
        url = f"gs://{self.a.backup_bucket}/config/{self.stamp}/{name}"
        body = json.dumps(self.manifest, indent=2, sort_keys=True)
        if not quiet:
            print(body)
        self.run(["gcloud", "storage", "cp", "-", url, f"--project={self.a.project}", "--quiet"],
                 mutate=True, stdin=body)
        if not quiet:
            log(f"manifest: {url}")

    def checkpoint(self, step):
        """Record progress in GCS, as progress-NN-<step>.json next to the
        manifest. If this process dies, the newest one says how far it got --
        and holds the previous digests and the restore point a manual
        `rollback --manifest` needs."""
        progress = self.manifest.setdefault("progress", [])
        progress.append({"step": step, "at": utcnow().strftime("%Y-%m-%dT%H:%M:%SZ")})
        log(f"checkpoint: {step}")
        self.save_manifest(quiet=True, name=f"progress-{len(progress):02d}-{step}.json")

    # -- Artifact Registry --------------------------------------------------

    def digest(self, image, tag):
        return self.gcloud("artifacts", "docker", "images", "describe",
                           f"{self.ar}/{image}:{tag}", "--format=value(image_summary.digest)")

    def tag(self, image, digest, tag):
        """Point image:tag at digest, through the Artifact Registry API.

        Not `gcloud artifacts docker tags add`: that moves an existing tag by
        deleting and re-creating it, which needs artifactregistry.tags.delete
        (not in roles/artifactregistry.writer) and leaves a moment with no
        :prod at all. tags.patch moves it in one step with tags.update."""
        pkg = (f"projects/{self.a.project}/locations/{self.a.region}"
               f"/repositories/{self.a.repo}/packages/{image}")
        body = {"name": f"{pkg}/tags/{tag}", "version": f"{pkg}/versions/{digest}"}
        if self.a.dry_run:
            log(f"DRY-RUN: tag {self.ar}/{image}@{digest} as :{tag}")
            return
        print(f"+ tag {self.ar}/{image}@{digest} as :{tag}", file=sys.stderr, flush=True)
        token = self.run(["gcloud", "auth", "print-access-token"])

        def call(method, url):
            req = urllib.request.Request(
                url, method=method, data=json.dumps(body).encode(),
                headers={"Authorization": f"Bearer {token}", "Content-Type": "application/json"})
            with urllib.request.urlopen(req, timeout=30) as r:
                return json.load(r)

        api = "https://artifactregistry.googleapis.com/v1"
        try:
            try:
                got = call("PATCH", f"{api}/{body['name']}?updateMask=version")
            except urllib.error.HTTPError as e:
                if e.code != 404:
                    raise
                got = call("POST", f"{api}/{pkg}/tags?tagId={tag}")
        except urllib.error.HTTPError as e:
            raise Abort(f"tag {image}:{tag}: HTTP {e.code}: {e.read().decode(errors='replace')[:300]}")
        if got.get("version") != body["version"]:
            raise Abort(f"tag {image}:{tag} points at {got.get('version')}, not {digest}")

    def labels(self, image, ref):
        """Image config labels, read from the registry API (no pull, no crane)."""
        token = self.run(["gcloud", "auth", "print-access-token"])
        host = f"{self.a.region}-docker.pkg.dev"
        base = f"https://{host}/v2/{self.a.project}/{self.a.repo}/{image}"
        accept = ", ".join([
            "application/vnd.oci.image.index.v1+json",
            "application/vnd.docker.distribution.manifest.list.v2+json",
            "application/vnd.oci.image.manifest.v1+json",
            "application/vnd.docker.distribution.manifest.v2+json",
        ])

        def get(url, acc=accept):
            req = urllib.request.Request(url, headers={"Authorization": f"Bearer {token}", "Accept": acc})
            with urllib.request.urlopen(req, timeout=30) as r:
                return json.load(r)

        m = get(f"{base}/manifests/{ref}")
        if "manifests" in m:  # an index: take linux/amd64
            plat = [x for x in m["manifests"]
                    if x.get("platform", {}).get("os") == "linux"
                    and x.get("platform", {}).get("architecture") == "amd64"]
            if not plat:
                raise Abort(f"{image}:{ref} has no linux/amd64 manifest")
            m = get(f"{base}/manifests/{plat[0]['digest']}")
        cfg = get(f"{base}/blobs/{m['config']['digest']}", acc="*/*")
        return cfg.get("config", {}).get("Labels") or {}

    # -- backups ------------------------------------------------------------

    def snapshot(self, purpose, *, keep=False, disk=None):
        """keep=True exempts it from prune_snapshots: the restore point of a
        major upgrade must outlive the 14-day window."""
        disk = disk or self.a.data_disk
        name = f"{disk}-{purpose}-{self.stamp.lower()}"[:63]
        labels = f"gitea-release=1,purpose={purpose}" + (",keep=true" if keep else "")
        self.gcloud("compute", "snapshots", "create", name,
                    "--source-disk=" + disk, f"--source-disk-zone={self.a.zone}",
                    f"--storage-location={self.a.region}", f"--labels={labels}", mutate=True)
        if not self.a.dry_run:
            status = ""
            for _ in range(60):
                status = self.gcloud("compute", "snapshots", "describe", name, "--format=value(status)")
                if status == "READY":
                    break
                time.sleep(10)
            else:
                raise Abort(f"snapshot {name} is still {status} after 10 minutes")
        self.manifest.setdefault("snapshots", []).append(name)
        return name

    def prune_snapshots(self, days=14):
        cutoff = (utcnow() - datetime.timedelta(days=days)).strftime("%Y-%m-%dT%H:%M:%SZ")
        names = self.gcloud(
            "compute", "snapshots", "list",
            f"--filter=labels.gitea-release=1 AND -labels.keep=true AND creationTimestamp<'{cutoff}'",
            "--format=value(name)")
        for n in filter(None, names.splitlines()):
            self.gcloud("compute", "snapshots", "delete", n, "--quiet", mutate=True)

    def record_state(self):
        """Non-secret facts a rollback needs: digests, versions, host keys, secret versions."""
        state = {}
        for img in IMAGES:
            d = self.digest(img, "prod")
            state[img] = {"digest": d, "labels": self.labels(img, d)}
        self.manifest["prod"] = state
        self.manifest["ssh_host_keys"] = self.ssh_host_keys()
        versions = {}
        for s in SECRETS:
            try:
                versions[s] = self.gcloud("secrets", "versions", "list", s, "--filter=state:ENABLED",
                                          "--limit=1", "--format=value(name)")
            except subprocess.CalledProcessError:
                versions[s] = "unreadable"
        self.manifest["secret_versions"] = versions

    def backup(self, *, snapshot="online", record=True):
        """snapshot: "online" (crash-consistent), "none", or "consistent" (stops Gitea)."""
        prefix = f"gs://{self.a.backup_bucket}"
        if record:
            self.record_state()

        dump = f"{prefix}/dumps/gitea-{self.stamp}.zip"
        self.vm("dump", dump)
        if not self.a.dry_run:
            size = int(self.gcloud("storage", "objects", "describe", dump, "--format=value(size)"))
            if size < self.a.min_dump_bytes:
                raise Abort(f"dump {dump} is only {size} bytes")
            self.manifest["dump"] = {"url": dump, "bytes": size}

        tarball = f"{prefix}/config/{self.stamp}/vm-config.tar.gz"
        self.vm("config-tarball", tarball)
        self.manifest["config_tarball"] = tarball

        exports = {
            "instance.json": ["gcloud", "compute", "instances", "describe", self.a.vm,
                              f"--zone={self.a.zone}", f"--project={self.a.project}", "--format=json"],
            "iam-policy.json": ["gcloud", "projects", "get-iam-policy", self.a.project, "--format=json"],
        }
        if self.a.pulumi_dir:
            exports["pulumi-stack.json"] = ["pulumi", "stack", "export", "--stack", self.a.stack,
                                            "--cwd", self.a.pulumi_dir]
        for name, cmd in exports.items():
            try:
                body = self.run(cmd)
            except (subprocess.CalledProcessError, FileNotFoundError) as e:
                log(f"WARNING: export {name} skipped: {e}")
                continue
            self.run(["gcloud", "storage", "cp", "-", f"{prefix}/config/{self.stamp}/{name}",
                      f"--project={self.a.project}"], mutate=True, stdin=body)

        if snapshot == "consistent":
            self.vm("stop", "gitea.service")
            try:
                self.snapshot("backup")
            finally:
                self.vm("start", "gitea.service")
        elif snapshot == "online":
            self.snapshot("backup")
        self.prune_snapshots()

    # -- checks -------------------------------------------------------------

    def http(self, path, *, timeout=15):
        url = f"https://{self.a.domain}{path}"
        req = urllib.request.Request(url, headers={"User-Agent": "gitea-ops"})
        try:
            with urllib.request.urlopen(req, timeout=timeout) as r:
                return r.status, r.headers.get("Content-Type", ""), r.read()
        except urllib.error.HTTPError as e:
            return e.code, "", b""
        except (urllib.error.URLError, TimeoutError, ConnectionError) as e:
            return 0, "", str(e).encode()

    def ssh_host_keys(self):
        scan = self.run(["ssh-keyscan", "-p", "2222", self.a.domain], check=False)
        fps = self.run(["ssh-keygen", "-lf", "-"], stdin=scan + "\n", check=False)
        return sorted(f"{p[1]} {p[-1]}" for p in (l.split() for l in fps.splitlines()) if len(p) >= 4)

    def verify(self, *, expect=None, soak=0, since=None):
        """Status checks. expect = {"version": "28.2.0", "gitea": digest, "caddy": digest}."""
        expect = expect or {}
        failures = []

        def check(ok, what):
            log(("ok   " if ok else "FAIL ") + what)
            if not ok:
                failures.append(what)

        code, _, _ = self.http("/api/healthz")
        check(code == 200, f"/api/healthz -> {code}")
        code, _, body = self.http("/api/v1/version")
        version = ""
        if code == 200:
            try:
                version = json.loads(body).get("version", "")
            except ValueError:
                version = "<unparseable>"
        if expect.get("version"):
            check(version == expect["version"], f"version {version!r} == {expect['version']!r}")
        for img in IMAGES:
            if expect.get(img):
                running = self.vm("running-digest", img, mutate=False)
                check(running == expect[img], f"{img} runs {running[:19]} == {expect[img][:19]}")
        if self.manifest.get("ssh_host_keys"):
            check(self.ssh_host_keys() == self.manifest["ssh_host_keys"], "SSH host keys unchanged")
        code, ctype, _ = self.http(f"/{self.a.probe_repo}.git/info/refs?service=git-upload-pack")
        check(code == 200 and "git-upload-pack" in ctype, f"anonymous git over HTTPS -> {code}")

        if soak and not failures:
            restarts0 = self.vm("restarts", "gitea.service", mutate=False)
            deadline = time.time() + soak
            log(f"soak: {soak}s")
            while time.time() < deadline:
                time.sleep(30)
                code, _, _ = self.http("/api/healthz")
                if code != 200:
                    check(False, f"soak: /api/healthz -> {code}")
                    break
            restarts1 = self.vm("restarts", "gitea.service", mutate=False)
            check(restarts0 == restarts1, f"gitea restarts {restarts0} -> {restarts1}")
            if since:
                errs = self.vm("journal-errors", "gitea.service", since, mutate=False)
                check(errs in ("", "0"), f"fatal/panic lines in journal since {since}: {errs or 0}")
        return failures

    # -- restores -----------------------------------------------------------

    def with_restore_disk(self, snapshot, step, *args):
        """Attach a disk made from snapshot as device gitea-restore, run step,
        and ALWAYS detach and delete it: fstab mounts /var/lib/gitea by UUID,
        and the copy has the same UUID, so it must never survive a reboot."""
        disk = f"gitea-data-restore-{self.stamp.lower()}"[:63]
        self.gcloud("compute", "disks", "create", disk, f"--source-snapshot={snapshot}",
                    f"--zone={self.a.zone}", "--type=pd-balanced", mutate=True)
        attached = False
        try:
            self.gcloud("compute", "instances", "attach-disk", self.a.vm, f"--disk={disk}",
                        "--device-name=gitea-restore", f"--zone={self.a.zone}", mutate=True)
            attached = True
            return self.vm(step, *args)
        finally:
            try:
                if attached:
                    self.gcloud("compute", "instances", "detach-disk", self.a.vm,
                                f"--disk={disk}", f"--zone={self.a.zone}", mutate=True)
                self.gcloud("compute", "disks", "delete", disk, f"--zone={self.a.zone}", "--quiet",
                            mutate=True)
            except subprocess.CalledProcessError:
                log(f"GITEA_RESTORE_DISK_LEFT_ATTACHED disk={disk}: detach/delete it before any reboot")
                self.manifest["restore_disk_left_attached"] = disk
                raise

    def restore_data(self, snapshot, tool_image):
        """Replace /var/lib/gitea with the contents of snapshot. Gitea must be stopped."""
        self.with_restore_disk(snapshot, "restore-sync", tool_image)


# -- subcommands ------------------------------------------------------------

def cmd_backup(ops, a):
    ops.require_yes()
    ops.backup(snapshot=a.snapshot)
    ops.manifest["outcome"] = "ok"
    ops.save_manifest()


def cmd_verify(ops, a):
    """Without --manifest: health, version and git checks. With one: also the
    digests that release promoted and the SSH host keys it recorded."""
    expect = {"version": a.expect_version} if a.expect_version else {}
    if a.manifest:
        src = json.loads(ops.run(["gcloud", "storage", "cat", a.manifest]))
        ops.manifest["ssh_host_keys"] = src.get("ssh_host_keys", [])
        expect = {"version": src.get("versions", {}).get("new"), **src.get("new", {})}
    failures = ops.verify(expect=expect, soak=a.soak)
    if failures:
        raise Abort(f"{len(failures)} check(s) failed")


class Lock:
    """A GCS object created with if-generation-match=0: only one writer wins.
    Without a state bucket (before the CI project exists) it is a no-op."""

    def __init__(self, ops):
        self.ops = ops
        self.url = f"gs://{ops.a.state_bucket}/lock" if ops.a.state_bucket else None

    def __enter__(self):
        if not self.url:
            log("no --state-bucket: running without a release lock")
            return self
        body = json.dumps({"stamp": self.ops.stamp, "build": self.ops.manifest["build"]})
        try:
            self.ops.run(["gcloud", "storage", "cp", "-", self.url, "--if-generation-match=0"],
                         mutate=True, stdin=body)
        except subprocess.CalledProcessError:
            holder = self.ops.run(["gcloud", "storage", "cat", self.url], check=False)
            raise Abort(f"release lock {self.url} is held: {holder}. "
                        "If that run is really dead: gcloud storage rm " + self.url)
        return self

    def __exit__(self, *exc):
        if self.url:
            self.ops.run(["gcloud", "storage", "rm", self.url], mutate=True, check=False)


def cmd_release(ops, a):
    ops.require_yes()
    if a.drill and os.environ.get("GITEA_DRILL_OK") != "1":
        raise Abort("--drill needs GITEA_DRILL_OK=1 (set only by `make ci-drill`)")
    m = ops.manifest
    with Lock(ops):
        ops.record_state()
        prev = {img: m["prod"][img]["digest"] for img in IMAGES}
        new = {"gitea": ops.digest("gitea", a.gitea)}
        if a.caddy:
            new["caddy"] = ops.digest("caddy", a.caddy)
        new_labels = ops.labels("gitea", new["gitea"])
        prev_labels = m["prod"]["gitea"]["labels"]
        prev_ver, new_ver = prev_labels.get(VERSION_LABEL, ""), new_labels.get(VERSION_LABEL, "")
        if not new_ver:
            raise Abort(f"gitea:{a.gitea} has no {VERSION_LABEL} label")

        # The two changes only an approved PR may bring: a Gitea major, an Alpine minor.
        if prev_ver.split(".")[0] != new_ver.split(".")[0] and not a.allow_major:
            raise Abort(f"Gitea {prev_ver} -> {new_ver} crosses a major version: "
                        "that needs the approval-gated path (--allow-major)")
        if prev_labels.get(ALPINE_LABEL) != new_labels.get(ALPINE_LABEL) and not a.allow_alpine_minor:
            raise Abort(f"Alpine {prev_labels.get(ALPINE_LABEL)} -> {new_labels.get(ALPINE_LABEL)}: "
                        "that needs the approval-gated path (--allow-alpine-minor)")

        # Path B whenever the Gitea version changes: the schema may migrate,
        # and then only a data restore can go back.
        path = "B" if (prev_ver != new_ver or a.drill == "fail-after-migrate") else "A"
        m.update({"path": path, "previous": prev, "new": new,
                  "versions": {"previous": prev_ver, "new": new_ver}})
        log(f"release: gitea {prev_ver} -> {new_ver}, path {path}")
        if all(prev.get(k) == v for k, v in new.items()) and not a.drill:
            log("nothing to release: :prod already has these digests")
            m["outcome"] = "noop"
            ops.save_manifest()
            return

        ops.checkpoint("planned")
        ops.backup(snapshot="online" if path == "A" else "none", record=False)
        for img in IMAGES:
            ops.tag(img, prev[img], "previous")
        ops.checkpoint("backed-up")

        since = utcnow().strftime("%Y-%m-%d %H:%M:%S UTC")
        snap = None
        timers_paused = False
        gitea_ref = f"{ops.ar}/gitea@{new['gitea']}"
        try:
            if path == "A":
                for img, d in new.items():
                    ops.tag(img, d, "prod")
                ops.checkpoint("promoted")
                ops.vm("auto-update")
            else:
                ops.vm("pull", gitea_ref)
                ops.vm("pause-timers")
                timers_paused = True
                ops.checkpoint("timers-paused")
                ops.vm("stop", "gitea.service")
                ops.checkpoint("gitea-stopped")
                # Across a major or an Alpine minor this is the last snapshot
                # of the old world: keep it, and take the boot disk too
                # (Caddy's certificates and every rendered config live there).
                gated = a.allow_major or a.allow_alpine_minor
                snap = ops.snapshot("pre-release", keep=gated)
                m["restore_point"] = snap
                if gated:
                    ops.snapshot("pre-release", keep=True, disk=a.vm)
                ops.checkpoint("snapshotted")
                ops.vm("migrate", gitea_ref)
                ops.checkpoint("migrated")
                for img, d in new.items():
                    ops.tag(img, d, "prod")
                ops.checkpoint("promoted")
                ops.vm("pull", f"{ops.ar}/gitea:prod")
                ops.vm("start", "gitea.service")
                ops.checkpoint("started")
                if a.drill == "fail-after-migrate":
                    ops.vm("marker", ops.stamp)
                if "caddy" in new:
                    ops.vm("auto-update")

            ops.checkpoint("verifying")
            failures = ops.verify(expect={"version": new_ver, **new}, soak=a.soak, since=since)
            if failures:
                raise Abort("status checks failed: " + "; ".join(failures))
            if a.drill in ("fail-verify", "fail-after-migrate"):
                raise DrillFailure(f"drill {a.drill}: injected failure after a passing verify")
        except Exception as e:  # anything at all once prod is being changed
            log(f"GITEA_RELEASE_ROLLBACK path={path} reason={e!r}")
            m["failure"] = str(e)
            # Bookkeeping must never stand between a failure and the rollback.
            try:
                ops.checkpoint("rolling-back")
            except subprocess.CalledProcessError:
                log("could not record the rolling-back checkpoint; rolling back anyway")
            m["outcome"] = rollback(ops, path, prev, prev_ver, snap, tool_image=gitea_ref)
            if a.drill == "fail-after-migrate":
                marker = ops.vm("marker-check", ops.stamp, mutate=False)
                m["drill"] = {"name": a.drill, "marker_after_restore": marker,
                              "passed": marker == "absent" and m["outcome"] == "rolled-back"}
                log(f"DRILL {a.drill}: {'PASS' if m['drill']['passed'] else 'FAIL'} (marker {marker})")
            try:
                ops.save_manifest()
            except subprocess.CalledProcessError:
                log(f"could not write the manifest; outcome={m['outcome']}")
            sys.exit(1 if m["outcome"] == "rolled-back" else 2)
        finally:
            if timers_paused and not m.get("restore_disk_left_attached"):
                ops.vm("resume-timers")

        for img, d in new.items():
            ops.tag(img, d, "last-good")
        m["outcome"] = "ok"
        ops.save_manifest()


def rollback(ops, path, prev, prev_ver, snap, *, tool_image):
    """Put prod back on prev (and, for path B, the pre-release data). Returns
    "rolled-back", or "rollback-failed" after logging the alert line."""
    try:
        if path == "B":
            ops.vm("stop", "gitea.service")
            for img in IMAGES:
                ops.tag(img, prev[img], "prod")
            if snap:
                ops.restore_data(snap, tool_image)
            ops.vm("pull", f"{ops.ar}/gitea:prod", f"{ops.ar}/caddy:prod")
            ops.vm("start", "gitea.service")
            ops.vm("restart-if-not", prev["caddy"], "caddy.service", "caddy")
        else:
            for img in IMAGES:
                ops.tag(img, prev[img], "prod")
            ops.vm("pull", f"{ops.ar}/gitea:prod", f"{ops.ar}/caddy:prod")
            ops.vm("restart-if-not", prev["gitea"], "gitea.service", "gitea")
            ops.vm("restart-if-not", prev["caddy"], "caddy.service", "caddy")
        failures = ops.verify(expect={"version": prev_ver, **prev}, soak=ops.a.rollback_soak)
        if not failures:
            return "rolled-back"
        reason = "; ".join(failures)
    except Exception as e:  # report, never mask: the alert line must go out
        reason = repr(e)
    log(f"GITEA_RELEASE_ROLLBACK_FAILED reason={reason} -- manual recovery: docs/runbook.md")
    return "rollback-failed"


def cmd_rollback(ops, a):
    """Manual rollback from a release manifest (or explicit digests)."""
    ops.require_yes()
    if a.manifest:
        src = json.loads(ops.run(["gcloud", "storage", "cat", a.manifest]))
        prev = src["previous"]
        prev_ver = src["versions"]["previous"]
        snap = src.get("restore_point") or (src.get("snapshots") or [None])[-1]
    else:
        prev = {"gitea": a.gitea_digest, "caddy": a.caddy_digest}
        prev_ver = a.expect_version
        snap = a.snapshot
    if a.restore_data and not snap:
        raise Abort("--restore-data needs a snapshot (none in the manifest; pass --snapshot)")
    ops.record_state()
    ops.manifest["rollback_of"] = a.manifest or "explicit"
    ops.vm("pause-timers")
    try:
        outcome = rollback(ops, "B" if a.restore_data else "A", prev, prev_ver,
                           snap if a.restore_data else None,
                           tool_image=a.tool_image or f"{ops.ar}/gitea@{ops.manifest['prod']['gitea']['digest']}")
    finally:
        if not ops.manifest.get("restore_disk_left_attached"):
            ops.vm("resume-timers")
    ops.manifest["outcome"] = outcome
    ops.save_manifest()
    if outcome != "rolled-back":
        sys.exit(2)


def parse_inventory(text):
    """The TSV vm-steps.sh `inventory` prints (plus "boot ok" from a restore test)."""
    inv = {"integrity": None, "users": {}, "repos": {}, "boot": False, "dbsource": None}
    for line in text.splitlines():
        f = line.split("\t")
        if f[0] == "integrity":
            inv["integrity"] = f[1] if len(f) > 1 else ""
        elif f[0] == "user" and len(f) == 4:
            inv["users"][f[1]] = {"created": int(f[3] or 0)}
        elif f[0] == "repo" and len(f) == 5:
            inv["repos"][f[1]] = {"created": int(f[2] or 0), "updated": int(f[3] or 0), "refs": f[4]}
        elif f[0] == "boot":
            inv["boot"] = f[1:] == ["ok"]
        elif f[0] == "dbsource" and len(f) > 1:
            inv["dbsource"] = f[1]
    return inv


def judge_restore(prod, restored, taken_at, grace=900):
    """Problems with a restored copy, judged against prod NOW. Only what the
    backup could not have seen is excused: anything created, or repos pushed
    to, within `grace` seconds before taken_at or later. Everything else must
    be there, and repos unchanged since the backup must have identical refs --
    so a copy with 2 of 40 repos fails, however non-zero its counts."""
    cutoff = taken_at - grace
    problems = []
    if restored["integrity"] != "ok":
        problems.append(f"sqlite integrity_check: {restored['integrity']!r}")
    if not restored["boot"]:
        problems.append("the restored copy did not boot healthy")
    for kind in ("users", "repos"):
        expected = {k for k, v in prod[kind].items() if v["created"] < cutoff}
        if not expected and prod[kind]:
            problems.append(f"no prod {kind} predate the backup -- nothing to compare against")
        missing = sorted(expected - restored[kind].keys())
        if missing:
            problems.append(f"{len(missing)}/{len(expected)} {kind} missing: {', '.join(missing[:10])}")
    for name, r in restored["repos"].items():
        if r["refs"] == "missing":
            problems.append(f"{name}: in the database but no git data")
            continue
        p = prod["repos"].get(name)
        if p and p["created"] < cutoff and p["updated"] < cutoff and p["refs"] != r["refs"]:
            problems.append(f"{name}: refs differ from prod although it is unchanged since the backup")
    return problems


def parse_time(ts):
    """gcloud timestamps -> unix seconds (handles Z, +0000 and +00:00)."""
    ts = ts.strip().replace("Z", "+00:00")
    if re.search(r"[+-]\d{4}$", ts):
        ts = ts[:-2] + ":" + ts[-2:]
    return int(datetime.datetime.fromisoformat(ts).timestamp())


def cmd_drill_restore(ops, a):
    """D2: a snapshot and a dump each boot a working, isolated Gitea."""
    ops.require_yes()
    tool = a.tool_image
    if not tool:
        prod = ops.digest("gitea", "prod")
        if not ops.labels("gitea", prod).get(ALPINE_LABEL):
            raise Abort("gitea:prod predates the Alpine image (no rsync/unzip/sqlite3 in it); "
                        "pass --tool-image with an image built from this repo")
        tool = f"{ops.ar}/gitea@{prod}"
    snap = a.snapshot or ops.gcloud(
        "compute", "snapshots", "list", f"--filter=sourceDisk~/{ops.a.data_disk}$ AND status=READY",
        "--sort-by=~creationTimestamp", "--limit=1", "--format=value(name)")
    dump = a.dump
    if not dump:
        listing = ops.run(["gcloud", "storage", "ls", f"gs://{ops.a.backup_bucket}/dumps/"], check=False)
        dumps = sorted(l for l in listing.splitlines() if l.endswith(".zip"))
        dump = dumps[-1] if dumps else None
    if not dump:
        raise Abort("no dump in the backup bucket: run `gitea_ops.py backup` first")
    ops.vm("pull", tool)
    snap_at = parse_time(ops.gcloud("compute", "snapshots", "describe", snap,
                                    "--format=value(creationTimestamp)"))
    # The dump's database is written first and its upload ends last; the
    # grace window in judge_restore covers the minutes in between.
    dump_at = parse_time(json.loads(ops.run(["gcloud", "storage", "objects", "describe", dump,
                                             "--format=json"]))["creation_time"])
    prod = parse_inventory(ops.vm("inventory", tool, mutate=False))
    # The copy is attached to the prod VM: no reboot (fstab mounts by UUID, and
    # the copy shares it) and no auto-update while it is there.
    ops.vm("pause-timers")
    # Each half is judged on its own: one failing must not hide the other.
    outputs = {}
    try:
        for label, step, args in (("snapshot", "restore-test-snapshot", (tool,)),
                                  ("dump", "restore-test-dump", (dump, tool))):
            try:
                outputs[label] = (ops.with_restore_disk(snap, step, *args), None)
            except subprocess.CalledProcessError as e:
                if ops.manifest.get("restore_disk_left_attached"):
                    raise
                outputs[label] = ("", f"{step} failed (exit {e.returncode})")
    finally:
        if not ops.manifest.get("restore_disk_left_attached"):
            ops.vm("resume-timers")
    results = {}
    for label, taken_at, src in (("snapshot", snap_at, snap), ("dump", dump_at, dump)):
        text, error = outputs[label]
        inv = parse_inventory(text)
        problems = ([error] if error else []) + judge_restore(prod, inv, taken_at)
        results[label] = {"source": src, "taken_at": taken_at, "users": len(inv["users"]),
                          "repos": len(inv["repos"]), "problems": problems}
        if inv["dbsource"]:
            results[label]["dbsource"] = inv["dbsource"]
            if inv["dbsource"] == "bundled":
                log(f"NOTE {label}: gitea-db.sql would not import with stock sqlite3; "
                    "restored from the dump's bundled gitea.db (copied live, crash-consistent at best)")
        for pr in problems:
            log(f"FAIL {label}: {pr}")
    ok = not any(r["problems"] for r in results.values())
    ops.manifest["drill"] = {"name": "restore", "passed": ok,
                             "prod": {"users": len(prod["users"]), "repos": len(prod["repos"])},
                             **results}
    ops.manifest["outcome"] = "ok" if ok else "failed"
    log(f"DRILL restore: {'PASS' if ok else 'FAIL'} -- prod {len(prod['users'])} users/"
        f"{len(prod['repos'])} repos; snapshot {results['snapshot']['users']}/{results['snapshot']['repos']}; "
        f"dump {results['dump']['users']}/{results['dump']['repos']}")
    ops.save_manifest()
    if not ok:
        sys.exit(1)


def git_head():
    try:
        return subprocess.run(["git", "-C", HERE, "rev-parse", "HEAD"], capture_output=True,
                              text=True, check=True).stdout.strip()
    except (subprocess.CalledProcessError, FileNotFoundError):
        return ""


def main():
    p = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    p.add_argument("--project", default=os.environ.get("APP_PROJECT", "gitea-496920"))
    p.add_argument("--region", default="us-east1")
    p.add_argument("--zone", default="us-east1-b")
    p.add_argument("--vm", default="gitea-vm")
    p.add_argument("--data-disk", default="gitea-data")
    p.add_argument("--repo", default="gitea", help="Artifact Registry repository id")
    p.add_argument("--domain", default="gitea.jasonmross.dev")
    p.add_argument("--probe-repo", default="JMR-dev/Gitea", help="public repo for the anonymous clone check")
    p.add_argument("--backup-bucket", default=None, help="default: <project>-gitea-backups")
    p.add_argument("--state-bucket", default=os.environ.get("DEPLOY_STATE_BUCKET"),
                   help="bucket holding the release lock (the CI project's deploy-state bucket)")
    p.add_argument("--pulumi-dir", default=None, help="include `pulumi stack export` from this dir")
    p.add_argument("--stack", default="prod")
    p.add_argument("--min-dump-bytes", type=int, default=50 * 1024 * 1024)
    p.add_argument("--yes", action="store_true", help="actually make changes")
    p.add_argument("--dry-run", action="store_true", help="print mutating commands instead of running them")
    sub = p.add_subparsers(dest="cmd", required=True)

    b = sub.add_parser("backup", help="dump, config tarball, exports, snapshot")
    b.add_argument("--snapshot", choices=["online", "consistent", "none"], default="online")
    b.set_defaults(fn=cmd_backup)

    r = sub.add_parser("release", help="promote images with backup, checks and automatic rollback")
    r.add_argument("--gitea", required=True, help="tag of the gitea image to release (e.g. the short SHA)")
    r.add_argument("--caddy", help="tag of the caddy image to release, if any")
    r.add_argument("--allow-major", action="store_true")
    r.add_argument("--allow-alpine-minor", action="store_true")
    r.add_argument("--soak", type=int, default=300)
    r.add_argument("--rollback-soak", type=int, default=60)
    r.add_argument("--drill", choices=["fail-verify", "fail-after-migrate"])
    r.set_defaults(fn=cmd_release)

    rb = sub.add_parser("rollback", help="restore images (and optionally data) from a manifest")
    rb.add_argument("--manifest", help="gs://.../manifest.json of the release to undo "
                    "(or its newest progress-*.json, if the engine died)")
    rb.add_argument("--gitea-digest")
    rb.add_argument("--caddy-digest")
    rb.add_argument("--expect-version")
    rb.add_argument("--snapshot")
    rb.add_argument("--restore-data", action="store_true", help="also restore /var/lib/gitea from the snapshot")
    rb.add_argument("--tool-image", help="image with rsync for the restore (default: current gitea:prod)")
    rb.add_argument("--rollback-soak", type=int, default=60)
    rb.set_defaults(fn=cmd_rollback)

    v = sub.add_parser("verify", help="run the status checks")
    v.add_argument("--expect-version")
    v.add_argument("--manifest", help="gs://.../manifest.json (or a progress-*.json) of a release to check against")
    v.add_argument("--soak", type=int, default=0)
    v.set_defaults(fn=cmd_verify)

    d = sub.add_parser("drill-restore", help="D2: prove a snapshot and a dump restore, in isolation")
    d.add_argument("--snapshot")
    d.add_argument("--dump")
    d.add_argument("--tool-image")
    d.set_defaults(fn=cmd_drill_restore)

    a = p.parse_args()
    a.backup_bucket = a.backup_bucket or f"{a.project}-gitea-backups"
    if a.cmd == "rollback" and not a.manifest and not (a.gitea_digest and a.caddy_digest and a.expect_version):
        p.error("rollback needs --manifest, or --gitea-digest, --caddy-digest and --expect-version")
    ops = Ops(a)
    try:
        a.fn(ops, a)
    except Abort as e:
        log(f"ABORT: {e}")
        sys.exit(3)


if __name__ == "__main__":
    main()
