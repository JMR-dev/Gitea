# vm/ansible — the VM's configuration

The playbook that configures the Gitea VM, pushed to it over IAP SSH: every
infra deploy runs it after `pulumi up` (`scripts/cicd/infra_deploy.py`), and
`make vm-check` / `make vm-apply` run it by hand. The files it installs stay
where they were: `vm/config`, `vm/quadlets`, `vm/systemd`, `vm/nftables`,
`vm/fail2ban`, `vm/dnf`, `vm/logrotate`.

The last two roles ship the VM's logs and metrics to watchtower
(`docs/runbook.md`, "Telemetry to watchtower"): `tunnel` installs the
WireGuard peer config, fetched and checked on the VM by
`roles/tunnel/files/gitea-watchtower-peer` so it never reaches the machine
running Ansible, and `logship` runs Grafana Alloy (`vm/config/config.alloy.tmpl`,
`vm/quadlets/alloy.*`). Their handlers are their own and never restart Gitea
or Caddy; nothing of theirs fails a run, it warns.

## Changing a templated file

The files in `vm/config`, `vm/quadlets`, `vm/nftables` and
`vm/fail2ban/jail.d` are rendered with `${VAR}` placeholders (Jinja with `${ }`
as its variable delimiters). Each one has a rendered copy per fixture in
`tests/golden/`, and `tests/check` — run by `make check`, every PR preview and
every deploy — fails if what renders differs from them.

1. Edit the file.
2. A new `${VAR}`:
   - define it in `roles/app/vars/main.yml` (config, quadlets),
     `roles/firewall/vars/main.yml` (nftables, the jail) or
     `roles/logship/vars/main.yml` (Alloy's config and quadlets);
   - if its value comes from the VM, read it in `roles/preflight` (an instance
     metadata key, set in `infra/pkg/compute`) and give it a value in every
     `tests/golden/*/fixture.yml`;
   - a secret is left as a placeholder (`NAME: "${NAME}"` in the vars file) and
     filled in on the VM by `roles/app/files/gitea-render-secrets` — add it to
     that script's envsubst list and to `SECRETS` in `tests/check`.

   `tests/check` names whichever of these is missing before it renders.
3. A new templated file: add it to a role and to `tests/render.yml`.
4. `make goldens` re-renders the goldens; review `git diff vm/ansible/tests/golden`
   — it is exactly what the change does to the VM's files — and commit it with
   the change.

Once the goldens match, `tests/check` also runs the telemetry checks:

- everywhere: `scripts/check-peer-config.sh` over the peer script,
  `tests/tunnel-state.yml` over role tunnel's states, and
  `scripts/check-metadata-guard.sh --static`, which checks that the ruleset's
  metadata guard names the uid `alloy.container` runs as;
- in a PR preview, `make check` and by hand, failing hard: `alloy fmt --test`
  and `alloy validate` on every rendered `config.alloy`, and its scrub rules
  over `scripts/testdata/scrub.log` and `scrub.caddy.log`
  (`scripts/check-alloy-scrub.sh`, copied from watchtower). These download
  Alloy and jq pinned by sha256 (`TOOLS` in `tests/check`), so a deploy and
  `make vm-check`/`vm-apply` skip them: a deploy never waits on github.com.
  `tests/check` tells the two apart by its parent's command line, the mode
  `infra_deploy.py` was run with (`run_mode()`); `GITEA_CHECK_MODE=strict` or
  `deploy` decides instead.

`make check` also loads the ruleset in a throwaway namespace and probes the
metadata server as Alloy's uid and others (`make check-metadata-guard`). A new
scrub rule gets fixture lines in `scripts/testdata`: `keep:`/`same:`/`drop:`
tagged, made up, and anything that must never ship in `scrub.forbidden`.

`tests/make-goldens` wrote the first goldens the way `vm/bootstrap.sh` did, the
byte-for-byte proof for the cutover from the bash. It only knows the templates
the bash had; don't use it for changes.
