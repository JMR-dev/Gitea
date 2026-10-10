# GitHub → Gitea migration

These scripts moved every non-fork repository owned by `JMR-dev` from GitHub into this
Gitea instance and made Gitea the primary. GitHub is now a push mirror: Gitea pushes
every commit to it. They are kept here for re-runs and for rotating the mirror token.

All of them are standard-library Python. They read tokens from the environment, never
from arguments or files. `verify.py`, `pushmirror.py` and `repoint.py` also read GitHub
through the local `gh` CLI.

| Script | What it does | Writes to |
|---|---|---|
| `migrate.py` | Full one-time migration: code, issues, PRs, releases, labels, milestones, wiki, LFS. Archives on Gitea whatever is archived on GitHub. | Gitea only |
| `verify.py` | Compares every branch and tag sha, the issue/PR/release counts, and the archived flag and visibility. | nothing |
| `repoint.py` | Re-points local clones' `JMR-dev` GitHub remotes at Gitea, following renames. Dry run unless `--apply`. | local `.git/config` |
| `pushmirror.py` | Creates a sync-on-commit push mirror to GitHub for each active repository. | Gitea, then GitHub through the mirror |

The input is a snapshot of the repository list:

```bash
gh repo list JMR-dev --limit 1000 \
  --json name,visibility,isFork,isArchived,diskUsage,description,defaultBranchRef > repos.json
```

## Tokens

Every token lives in Secret Manager in the Gitea project and is passed in by environment:

```bash
export GITEA_TOKEN=$(gcloud secrets versions access latest --secret=gitea-migration-token --project=<project>)
export GITHUB_TOKEN=$(gcloud secrets versions access latest --secret=github-migration-pat --project=<project>)  # migrate.py
export MIRROR_PAT=$(gcloud secrets versions access latest --secret=github-mirror-pat --project=<project>)       # pushmirror.py
```

- `GITEA_TOKEN` needs `write:repository` and `read:issue`.
- `GITHUB_TOKEN` for migrating should be a **read-only** fine-grained PAT (Contents, Metadata,
  Issues, Pull requests). Read-only cannot see draft releases; copy those by hand.
- `MIRROR_PAT` needs Contents and Workflows **read & write**. Without Workflows, GitHub rejects
  any push that touches `.github/workflows/`.

## Why they are safe to re-run

- `migrate.py` skips any repository that already exists on Gitea. It never deletes one in
  order to retry.
- `pushmirror.py` skips repositories that already have a GitHub push mirror. It also refuses
  to create one unless every GitHub branch and tag already matches Gitea. A push mirror
  force-pushes and prunes, so this check is what guarantees the first sync cannot overwrite
  or delete anything on GitHub.
- Archived repositories never get a push mirror, because GitHub rejects pushes to them.

## Rotating the mirror PAT

Each mirror stores its own copy of the PAT, and an expired PAT fails silently: the only sign
is the error on the repository's *Settings → Mirror* page. To rotate:

1. Store the new token as a new version of `github-mirror-pat`.
2. Delete each repository's GitHub mirror with
   `DELETE /api/v1/repos/JMR-dev/<repo>/push_mirrors/<remote_name>`.
3. Re-run `pushmirror.py`.

The refs check runs again in step 3. Anything pushed to GitHub directly in the meantime
shows up as `blocked` rather than being overwritten.

## Behaviour worth knowing

- New commits and branches reach GitHub within seconds.
- A bare branch delete does not trigger a sync. It reaches GitHub at the next push that carries
  commits, or at the 8-hour interval.
- Branches created on GitHub, such as Dependabot's, are pruned by the next sync.
- GitHub Actions `on: push` workflows run for mirrored pushes.
