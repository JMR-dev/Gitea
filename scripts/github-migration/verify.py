#!/usr/bin/env python3
"""Compare migrated repositories between GitHub and Gitea. Read-only on both.

Checks, per repository: every branch and tag points at the same commit; issue,
pull request and release counts match; archived flag and visibility match.

Usage: GITEA_TOKEN=... verify.py repos.json [name ...]
GitHub is read through the local `gh` CLI.
"""
import json
import os
import subprocess
import sys
import urllib.error
import urllib.request

GITEA = os.environ.get("GITEA_URL", "https://gitea.jasonmross.dev").rstrip("/")
OWNER = os.environ.get("OWNER", "JMR-dev")
GITEA_TOKEN = os.environ["GITEA_TOKEN"]


def gh_api(path):
    out = subprocess.run(["gh", "api", "--paginate", path], check=True, capture_output=True, text=True).stdout
    # --paginate concatenates JSON arrays as `][`; join them into one.
    return json.loads(out.replace("]\n[", ",").replace("][", ",")) if out.strip() else []


def gh_counts(name):
    q = ('query($o:String!,$n:String!){repository(owner:$o,name:$n){'
         'issues{totalCount} pullRequests{totalCount} releases{totalCount}}}')
    out = subprocess.run(["gh", "api", "graphql", "-f", f"query={q}", "-f", f"o={OWNER}", "-f", f"n={name}"],
                         check=True, capture_output=True, text=True).stdout
    r = json.loads(out)["data"]["repository"]
    return {"issues": r["issues"]["totalCount"], "pulls": r["pullRequests"]["totalCount"],
            "releases": r["releases"]["totalCount"]}


def gitea_get(path):
    req = urllib.request.Request(f"{GITEA}/api/v1{path}", headers={"Authorization": f"token {GITEA_TOKEN}"})
    with urllib.request.urlopen(req, timeout=60) as r:
        return json.loads(r.read()), r.headers.get("X-Total-Count")


def gitea_all(path):
    items, page = [], 1
    sep = "&" if "?" in path else "?"
    while True:
        batch, _ = gitea_get(f"{path}{sep}limit=50&page={page}")
        batch = batch or []  # an empty repository returns null, not []
        items += batch
        if len(batch) < 50:
            return items
        page += 1


def gitea_count(path):
    _, total = gitea_get(path + ("&" if "?" in path else "?") + "limit=1")
    return int(total or 0)


def verify(repo):
    name = repo["name"]
    problems = []
    try:
        g, _ = gitea_get(f"/repos/{OWNER}/{name}")
    except urllib.error.HTTPError as e:
        return {"name": name, "ok": False, "problems": [f"not in gitea ({e.code})"]}

    if g["archived"] != repo["isArchived"]:
        problems.append(f"archived gitea={g['archived']} github={repo['isArchived']}")
    if g["private"] != (repo["visibility"] != "PUBLIC"):
        problems.append(f"private gitea={g['private']} github={repo['visibility']}")

    gh_branches = {b["name"]: b["commit"]["sha"] for b in gh_api(f"repos/{OWNER}/{name}/branches")}
    gt_branches = {b["name"]: b["commit"]["id"] for b in gitea_all(f"/repos/{OWNER}/{name}/branches")}
    gh_tags = {t["name"]: t["commit"]["sha"] for t in gh_api(f"repos/{OWNER}/{name}/tags")}
    gt_tags = {t["name"]: t["commit"]["sha"] for t in gitea_all(f"/repos/{OWNER}/{name}/tags")}
    for kind, a, b in (("branch", gh_branches, gt_branches), ("tag", gh_tags, gt_tags)):
        for ref in sorted(set(a) | set(b)):
            if a.get(ref) != b.get(ref):
                problems.append(f"{kind} {ref}: github={(a.get(ref) or 'missing')[:10]} gitea={(b.get(ref) or 'missing')[:10]}")

    gh = gh_counts(name)
    gt = {"issues": gitea_count(f"/repos/{OWNER}/{name}/issues?state=all&type=issues"),
          "pulls": gitea_count(f"/repos/{OWNER}/{name}/issues?state=all&type=pulls"),
          "releases": gitea_count(f"/repos/{OWNER}/{name}/releases")}
    for k in gh:
        if gh[k] != gt[k]:
            problems.append(f"{k}: github={gh[k]} gitea={gt[k]}")

    return {"name": name, "ok": not problems, "branches": len(gt_branches), "tags": len(gt_tags),
            **{f"gitea_{k}": v for k, v in gt.items()}, "problems": problems}


def main():
    repos_file, *names = sys.argv[1:]
    repos = [r for r in json.load(open(repos_file)) if not r["isFork"]]
    if names:
        repos = [r for r in repos if r["name"] in set(names)]
    for repo in repos:
        try:
            res = verify(repo)
        except Exception as e:
            res = {"name": repo["name"], "ok": False, "problems": [f"verify error: {e!r}"]}
        print(json.dumps(res), flush=True)


if __name__ == "__main__":
    main()
