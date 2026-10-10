#!/usr/bin/env python3
"""Re-point local clones' remotes from GitHub (JMR-dev) to Gitea.

Only remotes whose URL is a JMR-dev GitHub repository that exists on Gitea are
changed. Third-party remotes (upstreams, other owners) and JMR-dev repos that
were not migrated (forks) are left untouched and reported. GitHub renames are
followed via the API redirect. Every change is appended to a TSV so it can be
reverted with `git remote set-url <remote> <old-url>`.

Usage: GITEA_TOKEN=... repoint.py <dirs-file> <workspace-root> <changes.tsv> [--apply]
Without --apply it only prints the plan.
"""
import json
import os
import re
import subprocess
import sys
import urllib.error
import urllib.request

OWNER = "JMR-dev"
GITEA = "https://gitea.jasonmross.dev"
GITEA_SSH = "ssh://git@gitea.jasonmross.dev:2222"
GH_URL = re.compile(r"^(?:git@github\.com:|ssh://git@github\.com/|https://github\.com/)([^/]+)/(.+?)(?:\.git)?/?$")


def git(cwd, *args):
    return subprocess.run(["git", *args], cwd=cwd, capture_output=True, text=True)


def gitea_has(name):
    req = urllib.request.Request(f"{GITEA}/api/v1/repos/{OWNER}/{name}",
                                 headers={"Authorization": f"token {os.environ['GITEA_TOKEN']}"})
    try:
        with urllib.request.urlopen(req, timeout=30) as r:
            return json.load(r)["name"]
    except urllib.error.HTTPError as e:
        if e.code == 404:
            return None
        raise


def canonical(name):
    """Follow a GitHub rename: the API redirects old names to the new repo."""
    r = subprocess.run(["gh", "api", f"repos/{OWNER}/{name}", "--jq", ".name"], capture_output=True, text=True)
    return r.stdout.strip() or name


def main():
    dirs_file, root, changes_file, *flags = sys.argv[1:]
    apply = "--apply" in flags
    out = open(changes_file, "a") if apply else None
    for rel in open(dirs_file).read().split():
        d = os.path.normpath(os.path.join(root, rel))
        remotes = git(d, "remote").stdout.split()
        for remote in remotes:
            url = git(d, "remote", "get-url", remote).stdout.strip()
            m = GH_URL.match(url)
            if not m:
                print(f"SKIP   {rel:38} {remote:9} not a GitHub URL: {url}")
                continue
            owner, name = m.groups()
            if owner != OWNER:
                print(f"SKIP   {rel:38} {remote:9} third-party ({owner}/{name})")
                continue
            target = gitea_has(canonical(name))
            if not target:
                print(f"SKIP   {rel:38} {remote:9} {owner}/{name} is not on Gitea (fork, not migrated)")
                continue
            new = f"{GITEA_SSH}/{OWNER}/{target}.git"
            if url == new:
                print(f"OK     {rel:38} {remote:9} already {new}")
                continue
            print(f"{'CHANGE' if apply else 'PLAN  '} {rel:38} {remote:9} {url} -> {new}")
            if apply:
                r = git(d, "remote", "set-url", remote, new)
                if r.returncode:
                    print(f"  !! set-url failed: {r.stderr.strip()}")
                    continue
                out.write(f"{d}\t{remote}\t{url}\t{new}\n")
                out.flush()


if __name__ == "__main__":
    main()
