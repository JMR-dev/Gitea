# vm/ansible — the VM's configuration

The playbook that configures the Gitea VM, pushed to it over IAP SSH: every
infra deploy runs it after `pulumi up` (`scripts/cicd/infra_deploy.py`), and
`make vm-check` / `make vm-apply` run it by hand. The files it installs stay
where they were: `vm/config`, `vm/quadlets`, `vm/systemd`, `vm/nftables`,
`vm/fail2ban`, `vm/dnf`.

## Changing a templated file

The files in `vm/config`, `vm/quadlets`, `vm/nftables` and
`vm/fail2ban/jail.d` are rendered with `${VAR}` placeholders (Jinja with `${ }`
as its variable delimiters). Each one has a rendered copy per fixture in
`tests/golden/`, and `tests/check` — run by `make check`, every PR preview and
every deploy — fails if what renders differs from them.

1. Edit the file.
2. A new `${VAR}`:
   - define it in `roles/app/vars/main.yml` (config, quadlets) or
     `roles/firewall/vars/main.yml` (nftables, the jail);
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

`tests/make-goldens` wrote the first goldens the way `vm/bootstrap.sh` did, the
byte-for-byte proof for the cutover from the bash. It only knows the templates
the bash had; don't use it for changes.
