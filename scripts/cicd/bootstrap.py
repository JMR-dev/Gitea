#!/usr/bin/env python3
"""Create the pieces of the CI project that Pulumi cannot.

Gitea's own CI/CD (cicd/) lives in a GCP project of its own, gitea-ci-<n>,
separate from the app project it deploys to. A service account cannot create
a project outside an organization, and Pulumi needs somewhere to keep its
state and a passphrase before it can run at all, so this script makes, as
the operator:

  * the project (label purpose=gitea-ci, so a re-run finds it again) and its
    billing link;
  * the APIs Pulumi itself needs to start; cicd/pkg/project enables the rest;
  * gs://<project>-pulumi-state (versioned) and the pulumi-config-passphrase
    secret, generated here and never printed;
  * gitea-webhook-secret, generated here: the relay checks every delivery
    against it, and `hook` hands the same value to Gitea;
  * gitea-ci-status-token, empty: a write:repository token the operator adds,
    since creating one needs a Gitea password, not a token.

Idempotent: everything that exists is left alone, and a generated secret is
never regenerated. Nothing changes without --yes; --dry-run prints the plan.

Usage:
  bootstrap.py [--dry-run | --yes]         # project, billing, state, secrets
  GITEA_TOKEN=... bootstrap.py --yes hook  # after `make ci-up`: the repo webhook
"""
import argparse
import json
import os
import secrets
import subprocess
import sys
import urllib.error
import urllib.request

LABEL = "purpose=gitea-ci"
BASE_APIS = [
    "cloudresourcemanager.googleapis.com",
    "serviceusage.googleapis.com",
    "iam.googleapis.com",
    "storage.googleapis.com",
    "secretmanager.googleapis.com",
]
# Generated here, never printed; the value only ever travels on a pipe.
GENERATED = ["pulumi-config-passphrase", "gitea-webhook-secret"]
# Created empty; the operator adds the version.
EMPTY = ["gitea-ci-status-token"]


def log(msg):
    print(f"==> {msg}", file=sys.stderr, flush=True)


class Run:
    def __init__(self, a):
        self.a = a

    def gcloud(self, *args, mutate=False, stdin=None, check=True):
        cmd = ["gcloud", *args, "--quiet"]
        if mutate and not self.a.yes:
            print("DRY-RUN: " + " ".join(cmd) + (" < (generated, not shown)" if stdin else ""))
            return ""
        r = subprocess.run(cmd, input=stdin, capture_output=True, text=True)
        if check and r.returncode != 0:
            sys.exit(f"gcloud {' '.join(args[:3])} failed: {r.stderr.strip()}")
        return r.stdout.strip() if r.returncode == 0 else None


def stack_file(a):
    return os.path.join(a.cicd_dir, f"Pulumi.{a.stack}.yaml")


def recorded_project(a):
    """gcp:project from the cicd stack's config, which is where it has to be anyway."""
    try:
        with open(stack_file(a)) as f:
            for line in f:
                k, _, v = line.strip().partition(":")
                if k == "gcp" and v.startswith("project:"):
                    return v.removeprefix("project:").strip().strip('"') or None
    except FileNotFoundError:
        pass
    return None


def find_project(run, a):
    pid = a.project or recorded_project(a)
    if pid:
        return pid
    # The fallback only: projects.list is backed by a search index that can
    # lag a new project by minutes, so it cannot be how a re-run finds one.
    out = run.gcloud("projects", "list", f"--filter=labels.{LABEL} AND lifecycleState=ACTIVE",
                     "--format=value(projectId)")
    ids = [p for p in out.split() if p]
    if len(ids) > 1:
        sys.exit(f"more than one project labelled {LABEL}: {ids}; pass --project")
    return ids[0] if ids else None


def record_project(a, pid):
    path = stack_file(a)
    if os.path.exists(path):
        with open(path) as f:
            text = f.read()
        if f"gcp:project: {pid}" in text:
            return
        sys.exit(f"{path} exists without gcp:project: {pid}; add it by hand")
    with open(path, "w") as f:
        f.write(f"config:\n  gcp:project: {pid}\n")
    log(f"recorded the project in {os.path.relpath(path)}")


def ensure_project(run, a):
    pid = find_project(run, a)
    if pid:
        if not run.gcloud("projects", "describe", pid, "--format=value(projectId)", check=False):
            sys.exit(f"project {pid} (from --project or {stack_file(a)}) is not visible to you")
        log(f"project {pid} exists")
        return pid
    pid = f"gitea-ci-{secrets.randbelow(900000) + 100000}"
    log(f"creating project {pid}")
    run.gcloud("projects", "create", pid, "--name=Gitea CI", f"--labels={LABEL}", mutate=True)
    if a.yes:
        record_project(a, pid)
    return pid


def ensure_billing(run, a, pid):
    cur = run.gcloud("billing", "projects", "describe", pid, "--format=value(billingAccountName)",
                     check=False)
    if cur and cur.endswith(a.billing_account):
        log(f"billing already linked to {a.billing_account}")
        return
    log(f"linking billing account {a.billing_account}")
    out = run.gcloud("billing", "projects", "link", pid, f"--billing-account={a.billing_account}",
                     mutate=True, check=False)
    if out is None:
        sys.exit("billing link failed. A billing account can only be linked to a limited number of "
                 "projects; unlink an unused one, or ask Google to raise the quota, then re-run.")


def ensure_apis(run, pid):
    log(f"enabling {len(BASE_APIS)} base APIs")
    run.gcloud("services", "enable", *BASE_APIS, f"--project={pid}", mutate=True)


def ensure_state_bucket(run, a, pid):
    bucket = f"gs://{pid}-pulumi-state"
    if run.gcloud("storage", "buckets", "describe", bucket, "--format=value(name)", check=False):
        log(f"{bucket} exists")
    else:
        log(f"creating {bucket}")
        run.gcloud("storage", "buckets", "create", bucket, f"--project={pid}", f"--location={a.region}",
                   "--uniform-bucket-level-access", "--public-access-prevention", mutate=True)
    # Versioned: a bad `pulumi up` or a fat-fingered `stack rm` is recoverable.
    run.gcloud("storage", "buckets", "update", bucket, "--versioning", mutate=True)


def ensure_secrets(run, pid):
    for name in GENERATED + EMPTY:
        exists = run.gcloud("secrets", "describe", name, f"--project={pid}", "--format=value(name)", check=False)
        if not exists:
            log(f"creating secret {name}")
            run.gcloud("secrets", "create", name, f"--project={pid}", "--replication-policy=automatic",
                       "--labels=managed-by=bootstrap-py", mutate=True)
        if name in EMPTY:
            continue
        versions = run.gcloud("secrets", "versions", "list", name, f"--project={pid}",
                              "--filter=state=ENABLED", "--format=value(name)", check=False) if exists else ""
        if versions:
            log(f"secret {name} already has a value; not regenerated")
            continue
        log(f"generating a value for {name}")
        run.gcloud("secrets", "versions", "add", name, f"--project={pid}", "--data-file=-",
                   mutate=True, stdin=secrets.token_urlsafe(48))


def cmd_init(run, a):
    pid = ensure_project(run, a)
    ensure_billing(run, a, pid)
    ensure_apis(run, pid)
    ensure_state_bucket(run, a, pid)
    ensure_secrets(run, pid)
    log("done" if a.yes else "dry run done; re-run with --yes to apply")
    print(f"""
next:
  1. cicd/Pulumi.prod.yaml holds gcp:project: {pid}. Then:
       make ci-init           # pulumi login + stack init, passphrase from Secret Manager
       make ci-preview
  2. A Gitea token with write:repository (Settings -> Applications), straight
     from your password manager into the secret, never onto the screen:
       gopass show -o <path> | gcloud secrets versions add gitea-ci-status-token \\
         --project={pid} --data-file=-
  3. After `make ci-up`: GITEA_TOKEN=... scripts/cicd/bootstrap.py --yes hook
""")


def gitea(method, path, token, body=None):
    url = os.environ.get("GITEA_URL", "https://gitea.jasonmross.dev").rstrip("/")
    req = urllib.request.Request(f"{url}/api/v1{path}", method=method,
                                 data=None if body is None else json.dumps(body).encode(),
                                 headers={"Authorization": f"token {token}", "Content-Type": "application/json"})
    try:
        with urllib.request.urlopen(req, timeout=30) as r:
            raw = r.read()
            return r.status, json.loads(raw) if raw else None
    except urllib.error.HTTPError as e:
        return e.code, {"message": e.read().decode(errors="replace")[:300]}


def cmd_hook(run, a):
    """Create (or re-point) the one repo webhook the relay listens for."""
    token = os.environ.get("GITEA_TOKEN")
    if not token:
        sys.exit("GITEA_TOKEN (a token with write:repository on the repo) is required")
    pid = find_project(run, a)
    if not pid:
        sys.exit("no CI project yet; run bootstrap.py --yes first")
    r = subprocess.run(["pulumi", "stack", "output", "webhookUrl", "--stack", a.stack],
                       cwd=a.cicd_dir, capture_output=True, text=True)
    hook_url = r.stdout.strip()
    if r.returncode != 0 or not hook_url.startswith("https://"):
        sys.exit(f"no webhookUrl stack output in {a.cicd_dir}: {r.stderr.strip() or hook_url!r}")
    secret = run.gcloud("secrets", "versions", "access", "latest", "--secret=gitea-webhook-secret",
                        f"--project={pid}")
    config = {"url": hook_url, "content_type": "json", "secret": secret}
    body = {"type": "gitea", "config": config, "events": ["push", "pull_request"], "active": True,
            "branch_filter": "*"}
    status, hooks = gitea("GET", f"/repos/{a.repo}/hooks?limit=50", token)
    if status != 200:
        sys.exit(f"listing hooks: HTTP {status}: {hooks}")
    mine = [h for h in hooks if (h.get("config") or {}).get("url") == hook_url]
    if not a.yes:
        print(f"DRY-RUN: {'PATCH' if mine else 'POST'} /repos/{a.repo}/hooks -> {hook_url} "
              "(push, pull_request; secret from Secret Manager)")
        return
    if mine:
        status, out = gitea("PATCH", f"/repos/{a.repo}/hooks/{mine[0]['id']}", token,
                            {"config": config, "events": body["events"], "active": True})
    else:
        status, out = gitea("POST", f"/repos/{a.repo}/hooks", token, body)
    if status not in (200, 201):
        sys.exit(f"webhook: HTTP {status}: {out}")
    log(f"webhook {out['id']} -> {hook_url}. Send a test delivery from the repo's webhook settings; "
        "a ping is answered 204 and runs nothing.")


def main():
    p = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    p.add_argument("--project", help="the CI project id (default: the one labelled purpose=gitea-ci)")
    p.add_argument("--billing-account", default="015319-DC7AAF-9CB1CC")
    p.add_argument("--region", default="us-east1")
    p.add_argument("--repo", default="JMR-dev/Gitea")
    p.add_argument("--stack", default="prod")
    p.add_argument("--cicd-dir", default=os.path.join(os.path.dirname(os.path.abspath(__file__)), "..", "..", "cicd"))
    g = p.add_mutually_exclusive_group()
    g.add_argument("--yes", action="store_true", help="actually make changes")
    g.add_argument("--dry-run", action="store_true", help="print what would change (the default)")
    p.add_argument("cmd", nargs="?", default="init", choices=["init", "hook"])
    a = p.parse_args()
    run = Run(a)
    {"init": cmd_init, "hook": cmd_hook}[a.cmd](run, a)


if __name__ == "__main__":
    main()
