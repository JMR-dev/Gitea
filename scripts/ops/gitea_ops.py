#!/usr/bin/env python3
"""Release engine for the Gitea VM: backups, releases with automatic rollback,
data restores, and the drills that prove them.

One implementation for both callers: an operator on a workstation (ADC) and
Cloud Build (the build service account). It shells out to gcloud for
everything it changes, and runs each VM-side step through Ansible
(scripts/ops/ansible/ops.yml) over IAP SSH, so the VM itself needs nothing
installed. The pinned ansible-core is installed on first use, before anything
changes (ensure_ansible).

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
  probe          read-only: every VM step that changes nothing, checked
                 against what it must say

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
import atexit
import datetime
import hashlib
import json
import os
import re
import shlex
import shutil
import subprocess
import sys
import tempfile
import time
import urllib.error
import urllib.request

HERE = os.path.dirname(os.path.abspath(__file__))
# The VM steps: one playbook, one task file per step, and the bash the steps
# that move data run (scripts/ops/ansible/ops.yml says how).
ANSIBLE_DIR = os.path.join(HERE, "ansible")
OPS_YML = os.path.join(ANSIBLE_DIR, "ops.yml")
LIB = os.path.join(ANSIBLE_DIR, "files", "gitea-ops-lib.sh")
# Every step ops.yml allows (its allowed_steps; test_cicd.py keeps the two,
# and the files in steps/, the same).
VM_STEPS = (
    "auto-update", "config-tarball", "dump", "inventory", "journal-errors", "marker", "marker-check",
    "migrate", "pause-timers", "pre-restore-list", "pre-restore-remove", "pull", "restart-if-not",
    "restarts", "restore-sync", "restore-test-dump", "restore-test-snapshot", "resume-timers",
    "running-digest", "selftest", "start", "stop",
)
# Every argument of a VM step, whole (ops.yml's arg_pattern): image references,
# digests, gs:// URLs, unit names, UTC times, tree names.
VM_ARG = re.compile(r"[A-Za-z0-9@:/._+=, -]*")

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
# How long the OS Login key each Job registers stays valid. Every Job pod makes
# a key of its own, and without an expiry they piled up on the account's OS
# Login profile until the VM refused them. A run that outlives it extends it
# first: a step that starts more than KEY_REFRESH seconds after the last
# registration registers the key again.
SSH_KEY_TTL = "1h"
KEY_REFRESH = 45 * 60


def cache_dir():
    """What the engine keeps for itself: the pinned ansible-core and a
    directory per run. In a Job, HOME is the pod's own /work."""
    return os.path.join(os.path.expanduser("~"), ".cache", "gitea-ops")


def ansible_env(pylib, work):
    """The environment ansible-playbook runs in: the pinned install first on
    the path, this file's ansible.cfg named explicitly (Ansible ignores one
    in a world-writable directory, and a ConfigMap mount is a symlink farm),
    and its state under the run's own directory."""
    env = dict(os.environ)
    env["PYTHONPATH"] = os.pathsep.join(p for p in (pylib, env.get("PYTHONPATH", "")) if p)
    env.update({
        "ANSIBLE_CONFIG": os.path.join(ANSIBLE_DIR, "ansible.cfg"),
        "ANSIBLE_HOME": os.path.join(work, "ansible-home"),
        "ANSIBLE_LOCAL_TEMP": os.path.join(work, "tmp"),
        "LC_ALL": "C.UTF-8",
        "PYTHONDONTWRITEBYTECODE": "1",
    })
    return env


def ensure_ansible():
    """Install the pinned ansible-core (scripts/ops/ansible/requirements.txt,
    every wheel hash-locked) into a directory keyed on that file, once per
    machine or Job pod, and return the directory. pip --target rather than a
    venv: the engine image's Python has pip but no venv module. main() runs
    this before anything else, so a run that could not reach the VM stops
    before it changes anything rather than halfway."""
    req = os.path.join(ANSIBLE_DIR, "requirements.txt")
    with open(req, "rb") as f:
        target = os.path.join(cache_dir(), "py-" + hashlib.sha256(f.read()).hexdigest()[:16])
    playbook = os.path.join(target, "bin", "ansible-playbook")
    if not os.path.isfile(playbook):
        # Into a directory of its own, then renamed: a half-finished install
        # (a killed pod, a full disk) is never mistaken for a whole one.
        tmp = f"{target}.tmp-{os.getpid()}"
        shutil.rmtree(tmp, ignore_errors=True)
        log(f"ansible: installing {req} into {target}")
        r = subprocess.run([sys.executable, "-m", "pip", "install", "--quiet", "--require-hashes",
                            "--only-binary=:all:", "--no-deps", "--root-user-action=ignore",
                            "--disable-pip-version-check", "--no-cache-dir", "--target", tmp, "-r", req],
                           capture_output=True, text=True)
        if r.returncode != 0:
            shutil.rmtree(tmp, ignore_errors=True)
            raise Abort(f"could not install ansible-core from {req}: {(r.stderr or r.stdout).strip()[-800:]}")
        try:
            os.rename(tmp, target)
        except OSError:  # another run on this machine finished first
            shutil.rmtree(tmp, ignore_errors=True)
    r = subprocess.run([sys.executable, playbook, "--version"], capture_output=True, text=True,
                       env=ansible_env(target, target))
    if r.returncode != 0:
        raise Abort(f"the ansible-core in {target} does not run: {(r.stderr or r.stdout).strip()[-800:]}")
    log(f"ansible: {r.stdout.splitlines()[0] if r.stdout else '?'} from {target}")
    return target


def read_result(path):
    """The {rc, stdout, stderr, failed, msg} ops.yml wrote for one step, or
    None when there is none, or none in that shape."""
    try:
        with open(path) as f:
            res = json.load(f)
    except (OSError, ValueError):
        return None
    if not (isinstance(res, dict) and isinstance(res.get("rc"), int) and isinstance(res.get("failed"), bool)
            and all(isinstance(res.get(k), str) for k in ("stdout", "stderr"))):
        return None
    return res


def stale_ssh_keys(listing, now_usec):
    """Fingerprints to remove from `gcloud compute os-login ssh-keys list
    --format=json` output: keys that never expire (left by Jobs before
    SSH_KEY_TTL) or already have. A live expiring key is kept."""
    return [k["key"] for k in listing
            if int((k.get("value") or {}).get("expirationTimeUsec") or 0) < now_usec]
VERSION_LABEL = "org.opencontainers.image.version"
ALPINE_LABEL = "dev.jasonmross.alpine.minor"
# How long a tree a data restore moved aside (/var/lib/gitea/.pre-restore-
# <stamp>, gitea-ops-lib.sh restore-sync) stays on the data disk. Each is a full
# copy of Gitea's data, and every D4 drill leaves one, so a backup removes the
# older ones once its own snapshot exists. That snapshot, and each daily one
# taken while the tree was there, keeps it for 14 days more.
PRE_RESTORE_DAYS = 7


def stale_pre_restore(names, now, days=PRE_RESTORE_DAYS):
    """The names in the pre-restore-list step's output whose stamp is older
    than `days`. A name not exactly .pre-restore-<stamp> is never one."""
    cutoff = now - datetime.timedelta(days=days)
    out = []
    for n in names:
        m = re.fullmatch(r"\.pre-restore-(\d{8}T\d{6}Z)", n)
        if not m:
            continue
        try:
            at = datetime.datetime.strptime(m[1], "%Y%m%dT%H%M%SZ").replace(tzinfo=datetime.timezone.utc)
        except ValueError:  # digits, but no date (month 13)
            continue
        if at < cutoff:
            out.append(n)
    return out


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
        self.pylib = None      # ensure_ansible()'s install; main() sets it
        self.work = None       # this run's directory, made by the first VM step
        self.cp_dir = None     # where its SSH master connection's socket is
        self.inventory = None
        self.ssh_user = None
        self.key_at = 0        # when this run last registered its OS Login key
        self.calls = 0

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

    def connect(self):
        """Before each VM step: this run's directory and inventory, and an OS
        Login key that outlives the step. The inventory reaches the VM the way
        `gcloud compute ssh --tunnel-through-iap` does (an IAP tunnel, as this
        account's OS Login user), with one SSH master connection kept for the
        whole run. Its known_hosts is the run's own, as gcloud's was the pod's
        own: the VM's host key is taken on first use, and a key that changes
        within the run is refused."""
        if not self.work:
            os.makedirs(cache_dir(), exist_ok=True)
            self.work = tempfile.mkdtemp(prefix=self.stamp + "-", dir=cache_dir())
            os.mkdir(os.path.join(self.work, "calls"))
            # The SSH control socket's path, plus the suffix ssh adds while
            # binding it, must fit in a socket address (107 bytes). Under an
            # unusually long HOME it goes to the system's temp directory.
            self.cp_dir = os.path.join(self.work, "cp")
            if len(os.path.join(self.cp_dir, self.a.vm)) > 80:
                self.cp_dir = tempfile.mkdtemp(prefix="gitea-ops-cp-")
            else:
                os.mkdir(self.cp_dir)
            atexit.register(self.disconnect)
        self.ensure_ssh_key()
        if time.time() - self.key_at > KEY_REFRESH:
            self.register_key()
        if not self.inventory:
            self.ssh_user = self.os_login_user()
            self.inventory = self.write_inventory()

    def register_key(self):
        """Register this run's key with OS Login for SSH_KEY_TTL, or extend it,
        as `gcloud compute ssh --ssh-key-expire-after` did on every call:
        update the expiry of a key the profile has, add one it has not."""
        pub = os.path.expanduser("~/.ssh/google_compute_engine.pub")
        err = ""
        for verb in ("update", "add"):
            r = subprocess.run(["gcloud", "compute", "os-login", "ssh-keys", verb, f"--key-file={pub}",
                                f"--ttl={SSH_KEY_TTL}", "--format=none"], capture_output=True, text=True)
            if r.returncode == 0:
                self.key_at = time.time()
                log(f"ssh: OS Login key {'extended' if verb == 'update' else 'added'} for {SSH_KEY_TTL}")
                return
            err = r.stderr.strip()
        raise subprocess.CalledProcessError(r.returncode, ["gcloud", "compute", "os-login", "ssh-keys", "add"],
                                            "", err)

    def os_login_user(self):
        """The POSIX user OS Login gives this account: the primary one."""
        prof = json.loads(self.run(["gcloud", "compute", "os-login", "describe-profile", "--format=json"]))
        accounts = prof.get("posixAccounts") or []
        primary = [x for x in accounts if x.get("primary")] or accounts
        if not primary or not primary[0].get("username"):
            raise ValueError("the OS Login profile has no POSIX account")
        return primary[0]["username"]

    def write_inventory(self):
        """The one-host inventory ops.yml runs against (JSON, which Ansible
        reads as YAML). All the SSH options are here, none in ansible.cfg:
        ssh takes the first value it is given for each, so two places would
        quietly disagree."""
        # env -u PYTHONPATH: ansible-core is on PYTHONPATH (ansible_env), and
        # gcloud, on its own bundled Python, would import that cryptography
        # over its own and crash before the tunnel opened (seen in Cloud
        # Build's gcloud image).
        proxy = shlex.join(["env", "-u", "PYTHONPATH", "gcloud", "compute", "start-iap-tunnel", "%h", "22",
                            "--listen-on-stdin", f"--zone={self.a.zone}", f"--project={self.a.project}",
                            "--verbosity=warning"])
        host = {
            "ansible_host": self.a.vm,
            "ansible_user": self.ssh_user,
            "ansible_ssh_private_key_file": os.path.expanduser("~/.ssh/google_compute_engine"),
            "ansible_python_interpreter": "/usr/bin/python3",
            # One master connection for the run; its tunnel is closed at exit.
            "ansible_ssh_args": "-o ControlMaster=auto -o ControlPersist=60m "
                                "-o ServerAliveInterval=30 -o ServerAliveCountMax=4",
            "ansible_ssh_common_args": shlex.join([
                "-o", f"ProxyCommand={proxy}",
                # One host and one user a run, so the host name is enough.
                "-o", f"ControlPath={os.path.join(self.cp_dir, '%h')}",
                "-o", f"UserKnownHostsFile={os.path.join(self.work, 'known_hosts')}",
                "-o", "StrictHostKeyChecking=accept-new",
                "-o", "IdentitiesOnly=yes",
            ]),
        }
        path = os.path.join(self.work, "inventory.json")
        with open(path, "w") as f:
            json.dump({"all": {"hosts": {"gitea": host}}}, f, indent=2)
        return path

    def disconnect(self):
        """At exit: close the SSH master connection (ControlPersist would hold
        its IAP tunnel open for an hour) and remove the run's directory.
        Best-effort: it is only tidying up."""
        if not self.work:
            return
        if self.ssh_user:
            try:
                subprocess.run(["ssh", "-o", f"ControlPath={os.path.join(self.cp_dir, '%h')}", "-O", "exit",
                                f"{self.ssh_user}@{self.a.vm}"], capture_output=True, timeout=30)
            except (OSError, subprocess.SubprocessError):
                pass
        shutil.rmtree(self.cp_dir, ignore_errors=True)
        shutil.rmtree(self.work, ignore_errors=True)
        self.work = self.inventory = None

    def vm(self, step, *args, mutate=True):
        """Run one VM step (scripts/ops/ansible/steps/<step>.yml) on the VM as
        root, through ops.yml; returns its stdout, stripped.

        Only the result file ops.yml writes counts, never ansible-playbook's
        own output. Any failure -- the step's, the SSH connection's, or no
        result at all -- raises CalledProcessError, as every caller expects."""
        cmd = ["vm-step", step, *args]
        if step not in VM_STEPS or not all(isinstance(x, str) and VM_ARG.fullmatch(x) for x in args):
            raise subprocess.CalledProcessError(2, cmd, "", f"refused: no VM step {cmd[1:]!r}")
        what = shlex.join([step, *args])
        if mutate and self.a.dry_run:
            log(f"DRY-RUN: vm-steps {what}")
            return ""
        log(f"vm: {what}")
        t0 = time.time()
        try:
            self.connect()
        except (subprocess.CalledProcessError, OSError, ValueError, KeyError, AttributeError,
                TypeError) as e:
            log(f"vm: {step}: cannot reach {self.a.vm}: {e}")
            raise subprocess.CalledProcessError(255, cmd, "", f"cannot reach {self.a.vm}: {e}")
        self.calls += 1
        base = os.path.join(self.work, "calls", f"{self.calls:02d}-{step}")
        call = {"step": step, "step_args": list(args), "result_file": base + ".result.json"}
        with open(base + ".json", "w") as f:
            json.dump(call, f)
        r = subprocess.run(
            [sys.executable, os.path.join(self.pylib, "bin", "ansible-playbook"),
             "-i", self.inventory, OPS_YML, "-e", "@" + base + ".json"],
            capture_output=True, text=True, cwd=self.work, env=ansible_env(self.pylib, self.work))
        res = read_result(call["result_file"])
        took = time.time() - t0
        if res is None:
            # It never got to run the step, or never got its result back
            # (unreachable, refused key): its own output says why.
            sys.stderr.write((r.stdout + r.stderr)[-4000:])
            log(f"vm: {step} failed after {took:.1f}s: no result (ansible-playbook exit {r.returncode})")
            raise subprocess.CalledProcessError(r.returncode or 1, cmd, r.stdout, r.stderr)
        # The step's own progress went to its stderr; pass it through. A step
        # that failed without any (an assert, a module) has a message instead.
        err = res["stderr"] or res.get("msg") or ""
        if err:
            sys.stderr.write(err.rstrip("\n") + "\n")
        if res["failed"] or res["rc"] != 0 or r.returncode != 0:
            rc = res["rc"] or r.returncode or 1
            log(f"vm: {step} failed (exit {rc}) after {took:.1f}s")
            raise subprocess.CalledProcessError(rc, cmd, res["stdout"], err)
        log(f"vm: {step} ok in {took:.1f}s")
        return res["stdout"].strip()

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
        if snapshot != "none":
            self.prune_pre_restore()

    def prune_pre_restore(self):
        """Remove the trees earlier data restores moved aside, once they are a
        week old (stale_pre_restore). Only from a backup that has just taken a
        snapshot, which still holds them. Housekeeping: the backup is already
        complete, so a failure here is a warning, not a failed backup."""
        try:
            names = self.vm("pre-restore-list", mutate=False).split()
            old = stale_pre_restore(names, utcnow())
            if old:
                self.vm("pre-restore-remove", *old)
                self.manifest["pre_restore_removed"] = old
            log(f"pre-restore: {len(old)} of {len(names)} moved-aside data tree(s) older than "
                f"{PRE_RESTORE_DAYS} days{': ' + ' '.join(old) if old else ''}")
        except subprocess.CalledProcessError as e:
            log(f"WARNING: could not prune .pre-restore-* trees ({e}); they stay on the data disk")

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
    """The TSV the `inventory` step prints (plus "boot ok" from a restore test)."""
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


def cmd_probe(ops, a):
    """Read-only: every VM step that changes nothing, run the way a release
    runs them, each checked against what it must say. Proof the engine reaches
    the VM and reads it right, before it is trusted to change anything."""
    failures = []
    seen = {}

    def check(ok, what):
        log(("ok   " if ok else "FAIL ") + what)
        if not ok:
            failures.append(what)

    def step(*args):
        try:
            return ops.vm(*args, mutate=False)
        except subprocess.CalledProcessError as e:
            check(False, f"{shlex.join(args)}: exit {e.returncode}")
            return None

    with open(LIB, "rb") as f:
        want = hashlib.sha256(f.read()).hexdigest()
    got = step("selftest")
    if got is not None:
        check(got == want, f"gitea-ops-lib.sh arrived intact: sha256 {got[:16]} == {want[:16]}")
    restarts0 = step("restarts", "gitea.service")
    if restarts0 is not None:
        check(restarts0.isdigit(), f"gitea.service NRestarts {restarts0!r}")
    prod = {}
    for img in IMAGES:
        prod[img] = ops.digest(img, "prod")
        running = step("running-digest", img)
        if running is not None:
            check(running == prod[img], f"{img} runs {running[:19]} == :prod {prod[img][:19]}")
        seen[img] = running
    since = (utcnow() - datetime.timedelta(hours=1)).strftime("%Y-%m-%d %H:%M:%S UTC")
    errs = step("journal-errors", "gitea.service", since)
    if errs is not None:
        check(errs.isdigit(), f"journal-errors since {since}: {errs!r} fatal/panic line(s)")
    pre = step("pre-restore-list")
    if pre is not None:
        log(f"pre-restore-list: {len(pre.split())} tree(s){': ' + ' '.join(pre.split()) if pre else ''}")
    marker = step("marker-check", "none")
    if marker is not None:
        check(marker == "absent", f"marker-check none -> {marker!r}")
    text = step("inventory", f"{ops.ar}/gitea@{prod['gitea']}")
    inv = parse_inventory(text or "")
    if text is not None:
        check(inv["integrity"] == "ok", f"inventory: sqlite integrity_check {inv['integrity']!r}")
        check(bool(inv["users"]) and bool(inv["repos"]),
              f"inventory: {len(inv['users'])} users, {len(inv['repos'])} repos")
    restarts1 = step("restarts", "gitea.service")
    if restarts0 is not None and restarts1 is not None:
        check(restarts0 == restarts1, f"gitea restarts {restarts0} -> {restarts1}")
    print(json.dumps({"lib_sha256": got, "prod": prod, "running": seen, "restarts": restarts1,
                      "journal_errors": errs, "pre_restore": (pre or "").split(), "marker": marker,
                      "users": len(inv["users"]), "repos": len(inv["repos"]), "failures": failures},
                     indent=2, sort_keys=True))
    log(f"PROBE: {'PASS' if not failures else 'FAIL'} ({len(failures)} failure(s), {ops.calls} VM step(s))")
    if failures:
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

    pr = sub.add_parser("probe", help="read-only: every VM step that changes nothing, checked")
    pr.set_defaults(fn=cmd_probe)

    a = p.parse_args()
    a.backup_bucket = a.backup_bucket or f"{a.project}-gitea-backups"
    if a.cmd == "rollback" and not a.manifest and not (a.gitea_digest and a.caddy_digest and a.expect_version):
        p.error("rollback needs --manifest, or --gitea-digest, --caddy-digest and --expect-version")
    ops = Ops(a)
    try:
        # First: a run that cannot reach the VM stops before any lock or change.
        ops.pylib = ensure_ansible()
        a.fn(ops, a)
    except Abort as e:
        log(f"ABORT: {e}")
        sys.exit(3)


if __name__ == "__main__":
    main()
