#!/usr/bin/env python3
"""Refuse to run release code automatically until a drill has passed with it.

Each release and each infra deploy runs the rollback code of the commit being
deployed. An edit to that code is live at the next release, and with
automation on, that release can be the unattended weekly rebuild. So the code
that can roll prod back is identified by a hash of its files, and a drill
that passes records that hash in the CI project's deploy-state bucket:

  gs://<bucket>/drills/<pipeline>/<hash>/<drill>-<build>.json

One object per passing run, never overwritten: the image pipeline's identity
can add objects to that bucket but not replace them.

An automatic run (`check`) goes ahead only when every drill its pipeline
needs has passed with that exact hash. A build a person approved (the gated
trigger, or one submitted by hand with _APPROVED=true) goes ahead anyway,
with a warning: the approval is the person taking that on. The engine itself
(`make release`) never consults this.

  image  D3 fail-verify (image rollback) and D4 fail-after-migrate (data restore)
  infra  D5 fail-verify (re-apply the last good commit)

`superseded image --sha S` answers whether a newer commit on main also
changes the image, in which case that commit's build releases it and this
one must not: builds that finish out of order would otherwise put an older
image back. It asks Gitea's API anonymously (the repo is public) and answers
"no" when it cannot tell, so a Gitea outage never blocks a release.

Usage:
  release_gate.py hash       <pipeline>
  release_gate.py check      <pipeline> --bucket B [--approved]
  release_gate.py attest     <pipeline> --bucket B --drill NAME --build B --sha S
  release_gate.py superseded image --sha S [--gitea-url U --repo owner/name]
"""
import argparse
import datetime
import hashlib
import json
import os
import subprocess
import sys
import urllib.request

ROOT = os.path.normpath(os.path.join(os.path.dirname(os.path.abspath(__file__)), "..", ".."))

# The release engine: gitea_ops.py, its Job launcher, and every file of its
# VM steps (scripts/ops/ansible). Each one is code a rollback runs, so each is
# named here, literally -- test_cicd.py checks none is missing.
ENGINE = [
    "scripts/ops/ansible/ansible.cfg",
    "scripts/ops/ansible/files/gitea-ops-lib.sh",
    "scripts/ops/ansible/ops.yml",
    "scripts/ops/ansible/requirements.in",
    "scripts/ops/ansible/requirements.txt",
    "scripts/ops/ansible/steps/auto-update.yml",
    "scripts/ops/ansible/steps/config-tarball.yml",
    "scripts/ops/ansible/steps/dump.yml",
    "scripts/ops/ansible/steps/inventory.yml",
    "scripts/ops/ansible/steps/journal-errors.yml",
    "scripts/ops/ansible/steps/marker-check.yml",
    "scripts/ops/ansible/steps/marker.yml",
    "scripts/ops/ansible/steps/migrate.yml",
    "scripts/ops/ansible/steps/pause-timers.yml",
    "scripts/ops/ansible/steps/pre-restore-list.yml",
    "scripts/ops/ansible/steps/pre-restore-remove.yml",
    "scripts/ops/ansible/steps/pull.yml",
    "scripts/ops/ansible/steps/restart-if-not.yml",
    "scripts/ops/ansible/steps/restarts.yml",
    "scripts/ops/ansible/steps/restore-sync.yml",
    "scripts/ops/ansible/steps/restore-test-dump.yml",
    "scripts/ops/ansible/steps/restore-test-snapshot.yml",
    "scripts/ops/ansible/steps/resume-timers.yml",
    "scripts/ops/ansible/steps/running-digest.yml",
    "scripts/ops/ansible/steps/selftest.yml",
    "scripts/ops/ansible/steps/start.yml",
    "scripts/ops/ansible/steps/stop.yml",
    "scripts/ops/gitea_ops.py",
    "scripts/ops/launch_job.py",
]
# Every file whose code decides what a release or a rollback does. Keep in
# step with cicd/relay's ReleasePaths, which flags a push touching any of them.
FILES = {
    "image": [
        "cloudbuild/image.yaml",
        "scripts/cicd/release_gate.py",
        *ENGINE,
    ],
    "infra": [
        "cloudbuild/infra.yaml",
        "scripts/cicd/infra_deploy.py",
        "scripts/cicd/release_gate.py",
        *ENGINE,
    ],
}
DRILLS = {"image": ["fail-verify", "fail-after-migrate"], "infra": ["fail-verify"]}


def code_hash(pipeline):
    h = hashlib.sha256()
    for rel in FILES[pipeline]:
        with open(os.path.join(ROOT, rel), "rb") as f:
            data = f.read()
        # Name and length too, so moving bytes between files changes the hash.
        h.update(f"{rel}\0{len(data)}\0".encode())
        h.update(data)
    return h.hexdigest()[:16]


def prefix(bucket, pipeline, digest):
    return f"gs://{bucket}/drills/{pipeline}/{digest}"


def gcloud(*args, stdin=None):
    return subprocess.run(["gcloud", *args, "--quiet"], input=stdin, capture_output=True, text=True)


def cmd_hash(a):
    print(code_hash(a.pipeline))


def cmd_check(a):
    digest = code_hash(a.pipeline)
    r = gcloud("storage", "ls", prefix(a.bucket, a.pipeline, digest) + "/")
    names = [os.path.basename(line) for line in r.stdout.split() if line.endswith(".json")]
    missing = [d for d in DRILLS[a.pipeline] if not any(n.startswith(d + "-") for n in names)]
    if not missing:
        print(f"release gate: {a.pipeline} code {digest} has passed {', '.join(DRILLS[a.pipeline])}")
        return 0
    if r.returncode != 0 and "matched no objects" not in r.stderr and "One or more URLs matched no" not in r.stderr:
        # Cannot tell: refuse rather than guess, unless a person approved.
        print(f"release gate: could not read {a.bucket}: {r.stderr.strip()}", file=sys.stderr)
    if a.approved:
        print(f"release gate: WARNING {a.pipeline} code {digest} has not passed {', '.join(missing)}; "
              "going ahead because this build was approved", file=sys.stderr)
        return 0
    print(f"release gate: REFUSED. The {a.pipeline} release code at this commit ({digest}) has not "
          f"passed drill(s) {', '.join(missing)}. Run `make ci-drill PIPELINE={a.pipeline} "
          f"NAME=<drill>` for each, then re-run this build -- or approve a gated build.", file=sys.stderr)
    return 1


def cmd_attest(a):
    if a.drill not in DRILLS[a.pipeline]:
        sys.exit(f"{a.drill} is not a {a.pipeline} drill")
    digest = code_hash(a.pipeline)
    body = json.dumps({
        "pipeline": a.pipeline, "drill": a.drill, "hash": digest, "files": FILES[a.pipeline],
        "build": a.build, "sha": a.sha,
        "passed_at": datetime.datetime.now(datetime.timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ"),
    }, indent=2)
    url = f"{prefix(a.bucket, a.pipeline, digest)}/{a.drill}-{a.build}.json"
    r = gcloud("storage", "cp", "-", url, stdin=body)
    if r.returncode != 0:
        sys.exit(f"could not record the drill at {url}: {r.stderr.strip()}")
    print(f"release gate: recorded {a.pipeline} {a.drill} PASS for code {digest} at {url}")
    return 0


# What makes a newer commit's build release the image (cicd/relay Classify).
IMAGE_PATHS = ("image/", "cloudbuild/image.yaml")


def newer_touches(compare, paths):
    """Whether any commit in a Gitea compare response changes one of paths."""
    for c in compare.get("commits") or []:
        for f in c.get("files") or []:
            name = f.get("filename", "")
            if any(name == p or (p.endswith("/") and name.startswith(p)) for p in paths):
                return c.get("sha", "?")
    return None


def cmd_superseded(a):
    url = f"{a.gitea_url.rstrip('/')}/api/v1/repos/{a.repo}/compare/{a.sha}...main"
    try:
        with urllib.request.urlopen(url, timeout=30) as r:
            compare = json.load(r)
    except Exception as e:  # cannot tell: never block a release on it
        print("no")
        print(f"superseded: could not ask Gitea ({e}); assuming not", file=sys.stderr)
        return 0
    newer = newer_touches(compare, IMAGE_PATHS)
    print("yes" if newer else "no")
    if newer:
        print(f"superseded: {newer[:7]} on main also changes the image; its build releases it", file=sys.stderr)
    return 0


def main():
    p = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    sub = p.add_subparsers(dest="cmd", required=True)
    sp = sub.add_parser("superseded")
    sp.add_argument("pipeline", choices=["image"])
    sp.add_argument("--sha", required=True)
    sp.add_argument("--gitea-url", default="https://gitea.jasonmross.dev")
    sp.add_argument("--repo", default="JMR-dev/Gitea")
    for name in ("hash", "check", "attest"):
        s = sub.add_parser(name)
        s.add_argument("pipeline", choices=sorted(FILES))
        if name != "hash":
            s.add_argument("--bucket", required=True, help="the CI project's deploy-state bucket")
        if name == "check":
            s.add_argument("--approved", action="store_true")
        if name == "attest":
            s.add_argument("--drill", required=True)
            s.add_argument("--build", required=True)
            s.add_argument("--sha", required=True)
    a = p.parse_args()
    sys.exit({"hash": cmd_hash, "check": cmd_check, "attest": cmd_attest,
              "superseded": cmd_superseded}[a.cmd](a) or 0)


if __name__ == "__main__":
    main()
