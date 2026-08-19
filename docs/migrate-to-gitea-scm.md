# Moving CI off GitHub and onto this Gitea instance

Today the repository lives on GitHub and Cloud Build watches it through a
`cloudbuildv2` connection. The intent is to move the repository into the Gitea
instance this repo deploys. Doing that well is mostly about not building a loop
that can strand itself.

## The bootstrapping problem

**The runner that deploys the VM must not live on the VM it deploys.** If a
Gitea Actions runner on `gitea-vm` executes the infra pipeline, then any change
that restarts Gitea — a config sync, an image rollout, a reboot — kills the job
mid-flight. Worse, a bad change locks you out of the tool needed to fix it.

Two workable shapes:

1. **Keep Cloud Build as the executor.** Gitea fires a webhook; Cloud Build runs
   the build. Cloud Build has no first-class Gitea trigger, so this means a small
   authenticated endpoint (Cloud Run or a Cloud Function) that validates the
   webhook signature and calls `cloudbuild.projects.triggers.run`. The deploy
   path stays entirely outside the VM. **This is the recommended option.**
2. **A Gitea Actions runner on separate infrastructure** — a second small VM or a
   Cloud Run job. Self-contained, but you now operate a runner, and the infra
   pipeline still needs GCP credentials.

Either way, keep the GitHub trigger working until the replacement has
successfully deployed at least once.

## Order of operations

1. `[actions] ENABLED = true` is already set in `vm/config/app.ini.tmpl`, so no
   Gitea-side config change is needed to start.
2. Mirror the repository into Gitea and verify history, LFS objects, and tags.
3. Stand up the chosen executor and prove it can run `cloudbuild/image.yaml`
   end to end, including the IAP rollout step.
4. Point `gitea:githubOwner` at nothing / remove the GitHub trigger. The Pulumi
   program already tolerates GitHub being unconfigured — `build.New` logs a
   warning and skips the triggers — so the stack still applies cleanly.
5. Keep the GitHub repo as an archived mirror for a while. It is the cheapest
   possible disaster recovery for the repo that contains the deployment.

## Do not forget

- The `cb-infra@` service account and the Pulumi state bucket are created by
  `scripts/bootstrap.sh`, not by Pulumi. They survive this migration untouched.
- Backups (`gitea-backup.timer`) become considerably more important once the
  repository that describes the infrastructure lives *on* the infrastructure.
  Verify a restore before you cut over, not after.
