#!/usr/bin/env python3
"""Copy GitHub repositories into Gitea as full, one-time migrations.

Non-destructive by construction:
  * GitHub is only ever read (clone + REST API through Gitea's downloader).
  * A repository that already exists in Gitea is never deleted or overwritten;
    it is skipped, so the script is safe to re-run after a partial failure.
  * The only Gitea-side change to an existing repository is setting the archived
    flag on a repository that is archived on GitHub.

Usage:
  GITEA_TOKEN=... [GITHUB_TOKEN=...] migrate.py repos.json results.jsonl [name ...]

repos.json is `gh repo list OWNER --json name,visibility,isFork,isArchived,
diskUsage,description,defaultBranchRef`. Forks are always skipped. With names,
only those repositories are processed, in the order given; otherwise every
non-fork, smallest first.
"""
import json
import os
import sys
import time
import urllib.error
import urllib.request

GITEA = os.environ.get("GITEA_URL", "https://gitea.jasonmross.dev").rstrip("/")
OWNER = os.environ.get("OWNER", "JMR-dev")
GITEA_TOKEN = os.environ["GITEA_TOKEN"]
GITHUB_TOKEN = os.environ.get("GITHUB_TOKEN", "")


def gitea(method, path, body=None, timeout=60):
    req = urllib.request.Request(
        f"{GITEA}/api/v1{path}",
        method=method,
        data=None if body is None else json.dumps(body).encode(),
        headers={"Authorization": f"token {GITEA_TOKEN}", "Content-Type": "application/json"},
    )
    try:
        with urllib.request.urlopen(req, timeout=timeout) as r:
            raw = r.read()
            return r.status, (json.loads(raw) if raw else None)
    except urllib.error.HTTPError as e:
        raw = e.read()
        try:
            return e.code, json.loads(raw)
        except ValueError:
            return e.code, {"message": raw.decode(errors="replace")[:500]}


def migrate_one(repo):
    name = repo["name"]
    status, existing = gitea("GET", f"/repos/{OWNER}/{name}")
    if status == 200:
        result = {"result": "exists-skipped"}
        if repo["isArchived"] and not existing.get("archived"):
            s, _ = gitea("PATCH", f"/repos/{OWNER}/{name}", {"archived": True})
            result["archived_set"] = s == 200
        return result
    if status != 404:
        return {"result": "error", "stage": "lookup", "status": status, "detail": existing}

    body = {
        "clone_addr": f"https://github.com/{OWNER}/{name}.git",
        "service": "github",
        "repo_owner": OWNER,
        "repo_name": name,
        "private": repo["visibility"] != "PUBLIC",
        "description": (repo.get("description") or "")[:2048],
        "mirror": False,
        "wiki": True,
        "issues": True,
        "labels": True,
        "milestones": True,
        "pull_requests": True,
        "releases": True,
        "lfs": True,
    }
    if GITHUB_TOKEN:
        body["auth_token"] = GITHUB_TOKEN

    started = time.time()
    # The API migrates synchronously; a repository with hundreds of PRs takes
    # many minutes, mostly in Gitea's rate-limit-aware GitHub downloader.
    status, resp = gitea("POST", "/repos/migrate", body, timeout=4 * 3600)
    elapsed = round(time.time() - started, 1)
    if status != 201:
        return {"result": "error", "stage": "migrate", "status": status, "seconds": elapsed,
                "detail": (resp or {}).get("message", resp)}

    result = {"result": "migrated", "seconds": elapsed, "private": resp.get("private")}
    if repo["isArchived"]:
        s, _ = gitea("PATCH", f"/repos/{OWNER}/{name}", {"archived": True})
        result["archived_set"] = s == 200
    return result


def main():
    repos_file, results_file, *names = sys.argv[1:]
    repos = [r for r in json.load(open(repos_file)) if not r["isFork"]]
    if names:
        by_name = {r["name"]: r for r in repos}
        missing = [n for n in names if n not in by_name]
        if missing:
            sys.exit(f"not in {repos_file} (or a fork): {', '.join(missing)}")
        repos = [by_name[n] for n in names]
    else:
        repos.sort(key=lambda r: r["diskUsage"])

    print(f"{len(repos)} repositories; github token: {'yes' if GITHUB_TOKEN else 'NO'}", flush=True)
    with open(results_file, "a") as out:
        for i, repo in enumerate(repos, 1):
            print(f"[{i}/{len(repos)}] {repo['name']} ({repo['diskUsage']} KB) ...", flush=True)
            try:
                result = migrate_one(repo)
            except Exception as e:  # keep going; one bad repository must not stop the batch
                result = {"result": "error", "stage": "exception", "detail": repr(e)}
            result = {"name": repo["name"], "ts": time.strftime("%H:%M:%S"), **result}
            out.write(json.dumps(result) + "\n")
            out.flush()
            print(f"    -> {result['result']} {json.dumps({k: v for k, v in result.items() if k not in ('name', 'result', 'ts')})}", flush=True)


if __name__ == "__main__":
    main()
