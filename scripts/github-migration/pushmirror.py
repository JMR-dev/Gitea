#!/usr/bin/env python3
"""Create Gitea -> GitHub push mirrors that sync on every commit.

Gitea's push mirror force-pushes and prunes, so it would delete or overwrite
anything that exists only on GitHub. Guard: a mirror is created only when every
GitHub branch and tag already exists on Gitea at the same commit. Then the first
sync cannot remove or rewrite anything on GitHub. Repositories with an existing
GitHub push mirror are skipped, so re-runs are safe.

Usage: GITEA_TOKEN=... MIRROR_PAT=... pushmirror.py repos.json results.jsonl [name ...]
Archived (on GitHub) repositories and forks are always skipped: GitHub rejects
pushes to archived repositories.
"""
import json
import os
import subprocess
import sys
import time
import urllib.error
import urllib.request

GITEA = "https://gitea.jasonmross.dev"
OWNER = "JMR-dev"
GITEA_TOKEN = os.environ["GITEA_TOKEN"]
MIRROR_PAT = os.environ["MIRROR_PAT"]
INTERVAL = os.environ.get("MIRROR_INTERVAL", "8h")  # backstop; sync_on_commit does the real work


def gitea(method, path, body=None):
    req = urllib.request.Request(f"{GITEA}/api/v1{path}", method=method,
                                 data=None if body is None else json.dumps(body).encode(),
                                 headers={"Authorization": f"token {GITEA_TOKEN}", "Content-Type": "application/json"})
    try:
        with urllib.request.urlopen(req, timeout=120) as r:
            raw = r.read()
            return r.status, (json.loads(raw) if raw else None)
    except urllib.error.HTTPError as e:
        raw = e.read()
        try:
            return e.code, json.loads(raw)
        except ValueError:
            return e.code, {"message": raw.decode(errors="replace")[:300]}


def gitea_all(path):
    items, page = [], 1
    while True:
        _, batch = gitea("GET", f"{path}?limit=50&page={page}")
        batch = batch or []
        items += batch
        if len(batch) < 50:
            return items
        page += 1


def gh_refs(name, kind):
    out = subprocess.run(["gh", "api", "--paginate", f"repos/{OWNER}/{name}/{kind}"],
                         check=True, capture_output=True, text=True).stdout
    items = json.loads(out.replace("]\n[", ",").replace("][", ",")) if out.strip() else []
    return {i["name"]: i["commit"]["sha"] for i in items}


def refs_problems(name):
    problems = []
    for kind, gitea_key in (("branches", "id"), ("tags", "sha")):
        gh = gh_refs(name, kind)
        gt = {i["name"]: i["commit"][gitea_key] for i in gitea_all(f"/repos/{OWNER}/{name}/{kind}")}
        for ref, sha in gh.items():
            if gt.get(ref) != sha:
                problems.append(f"{kind[:-1] if kind != 'branches' else 'branch'} {ref}: github={sha[:10]} gitea={(gt.get(ref) or 'missing')[:10]}")
    return problems


def mirror_one(name):
    status, mirrors = gitea("GET", f"/repos/{OWNER}/{name}/push_mirrors")
    if status != 200:
        return {"result": "error", "stage": "list", "status": status, "detail": mirrors}
    if any("github.com" in (m.get("remote_address") or "") for m in mirrors or []):
        return {"result": "exists-skipped"}

    problems = refs_problems(name)
    if problems:
        return {"result": "blocked", "problems": problems}

    status, resp = gitea("POST", f"/repos/{OWNER}/{name}/push_mirrors", {
        "remote_address": f"https://github.com/{OWNER}/{name}.git",
        "remote_username": OWNER,
        "remote_password": MIRROR_PAT,
        "interval": INTERVAL,
        "sync_on_commit": True,
    })
    if status not in (200, 201):
        return {"result": "error", "stage": "create", "status": status, "detail": (resp or {}).get("message", resp)}
    gitea("POST", f"/repos/{OWNER}/{name}/push_mirrors-sync")
    return {"result": "created", "remote_name": resp.get("remote_name")}


def main():
    repos_file, results_file, *names = sys.argv[1:]
    repos = [r for r in json.load(open(repos_file)) if not r["isFork"] and not r["isArchived"]]
    if names:
        repos = [r for r in repos if r["name"] in set(names)]
    print(f"{len(repos)} repositories", flush=True)
    with open(results_file, "a") as out:
        for i, r in enumerate(repos, 1):
            try:
                res = mirror_one(r["name"])
            except Exception as e:
                res = {"result": "error", "stage": "exception", "detail": repr(e)}
            res = {"name": r["name"], "ts": time.strftime("%H:%M:%S"), **res}
            out.write(json.dumps(res) + "\n")
            out.flush()
            print(f"[{i}/{len(repos)}] {r['name']}: {res['result']} {json.dumps({k: v for k, v in res.items() if k not in ('name', 'ts', 'result')})}", flush=True)


if __name__ == "__main__":
    main()
