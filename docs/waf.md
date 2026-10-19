# The WAF (Coraza + OWASP CRS)

Caddy is built with [Coraza](https://coraza.io) compiled in, running the OWASP
Core Rule Set. The CRS ships inside the binary via the `coraza-coreruleset` Go
package — there are no rule files on the VM to sync or version.

Two things about this deployment are unusual and deliberate. Both were verified
by experiment, not assumed.

## 1. Git and LFS bypass the WAF entirely

`vm/config/Caddyfile.tmpl` routes these paths to Gitea **without** the WAF:

```
/{owner}/{repo}/info/refs
/{owner}/{repo}/git-upload-pack
/{owner}/{repo}/git-receive-pack
/{owner}/{repo}/HEAD
/{owner}/{repo}/objects/...
/{owner}/{repo}/info/lfs/...
```

(The matcher uses `[^/]+` for the repo segment, so the `.git` suffix variants are
covered by the same pattern.)

This is not a hedge. With the bypass removed, a plain `git push` fails:

```
fatal: unable to access '.../wafrepo.git/': The requested URL returned error: 403
```

Packfiles are binary and reliably trip CRS's SQLi and XSS rules, and
`coraza.conf-recommended`'s `SecRequestBodyLimit` would refuse large pushes on
its own. Running CRS over git transport does not harden anything — Gitea still
authenticates every one of these requests — it only breaks the product.

If you change the matcher, re-run the git drill in `docs/runbook.md`.

### Gitea Actions routes bypass it too

`/api/actions/*`, `/api/actions_pipeline/*` and
`/twirp/github.actions.results.api.v1.ArtifactService/*` go to Gitea under a
second matcher, `@actions`. The runner's Connect RPC bodies are binary protobuf
(`application/proto`) and artifact uploads are PUT/PATCH octet streams. CRS's
default allowed content types and methods refuse both, so in `On` mode every
runner would go offline. Same argument as git: every one of these routes
authenticates with a runner or task token. Details in [runners.md](runners.md).

## 2. `wafMode` drives both the WAF and the fail2ban jail

One stack config value, `gitea:wafMode`:

| Value | `SecRuleEngine` | `caddy-coraza` jail | Use |
|---|---|---|---|
| `DetectionOnly` | logs, does not block | **disabled** | the default; tuning |
| `On` | blocks with 403 | **enabled** | after tuning |
| `Off` | inactive | disabled | debugging only |

The jail is tied to the mode on purpose. Banning on detections that were never
blocks would turn a tuning false positive into an nftables ban at the prerouting
hook — strictly worse than the 403 that `DetectionOnly` exists to avoid.

**A WAF ban is not limited to HTTP.** `nft-prerouting` drops by source address at
the prerouting hook and ignores fail2ban's `port` setting, so an address banned
for a WAF verdict also loses git over SSH on 2222. That is intentional — an
attacker should lose every door at once — but it means a WAF false positive in
`On` mode cuts a user off from git entirely, not just from the web UI. One more
reason to tune in `DetectionOnly` first.

## Rollout

**Do not start in `On`.** Gitea legitimately carries code, markdown, and SQL in
POST bodies — issue comments, PR descriptions, the wiki, the web file editor.
CRS will flag some of it.

1. Deploy with the default `DetectionOnly` and use the instance normally for a
   week or two. Exercise the parts that carry content: open issues with code
   blocks, edit a file in the web UI, use the API.
2. Review what **would have been blocked**. The signal is rule 949110, the CRS
   anomaly-score threshold — it fires in both modes, as `Warning` in
   `DetectionOnly` and `Access denied` in `On`, and both carry the same
   `Inbound Anomaly Score Exceeded` text:
   ```bash
   make ssh
   # score, client, and URI for every would-be block, worst first
   sudo journalctl CONTAINER_NAME=caddy --since '7 days ago' \
     | grep 'Inbound Anomaly Score Exceeded' \
     | sed -nE 's/.*\[client \\"([^\\]+)\\".*Total Score: ([0-9]+).*\[uri \\"([^\\]+)\\".*/\2\t\1\t\3/p' \
     | sort -rn | uniq
   ```
   Anything you recognise as your own legitimate traffic needs an exclusion.
   To see which individual rules contributed to a score:
   ```bash
   sudo journalctl CONTAINER_NAME=caddy --since '7 days ago' \
     | grep 'http.handlers.waf' \
     | grep -oP '\[id \\"\K[0-9]+' | sort | uniq -c | sort -rn
   ```
   Every warn line ends with `[unique_id "..."]`, which matches
   `.transaction.id` in the JSON audit record — that is how you get the full
   request for one event:
   ```bash
   sudo journalctl CONTAINER_NAME=caddy | grep '"transaction"' \
     | jq --arg id '<unique_id>' 'select(.transaction.id==$id)'
   ```
   Once `wafMode` is `On`, confirmed blocks are simply:
   ```bash
   sudo journalctl CONTAINER_NAME=caddy | grep '"transaction"' \
     | jq -c 'select(.transaction.is_interrupted) | {ip:.transaction.client_ip, uri:.transaction.request.uri}'
   ```
   **Gitea 28 replaced Server-Sent Events with a WebSocket** for live updates
   (notification counts, stopwatches). Caddy proxies the `Upgrade` handshake
   on its own, but the handshake is an ordinary GET through the WAF. Look for
   hits on it specifically before switching to `On`. A blocked handshake
   doesn't break anything visibly; the UI just quietly falls back to polling.
3. Add exclusions to the `directives` block in `vm/config/Caddyfile.tmpl`, before
   the `Include @owasp_crs/*.conf` line for rule-set config, or after it for
   `SecRuleRemoveById` / `SecRuleUpdateTargetById`. Document each one — which
   rule, which endpoint, and why.
4. `pulumi config set gitea:wafMode On` and push. The jail enables itself.

### Exclusion examples

```
# Rule 942100 (libinjection SQLi) on the issue comment body: users paste SQL
# into issues, that is the point of an issue tracker.
SecRule REQUEST_URI "@rx ^/[^/]+/[^/]+/issues/" \
    "id:1000001,phase:1,pass,nolog,ctl:ruleRemoveById=942100"

# The web file editor posts arbitrary file content.
SecRule REQUEST_URI "@rx ^/[^/]+/[^/]+/_(edit|new)/" \
    "id:1000002,phase:1,pass,nolog,ctl:ruleRemoveTargetById=949110;ARGS:content"
```

Use ids in the 1,000,000+ range — CRS reserves everything below.

## Reading the logs

Coraza emits two shapes, both to journald via the `caddy` container:

**Individual rule matches** — one per rule, noisy, informational:

```
"logger":"http.handlers.waf","msg":"[client \"203.0.113.9\"] Coraza: Warning.
 Host header is a numeric IP address ... [id \"920350\"]"
```

**The blocking decision** — emitted once when the accumulated anomaly score
crosses the threshold and the request is actually refused:

```
"logger":"http.handlers.waf","msg":"[client \"203.0.113.9\"] Coraza: Access denied
 (phase 2). Inbound Anomaly Score Exceeded (Total Score: 23) ... [id \"949110\"]"
```

`vm/fail2ban/filter.d/caddy-coraza.conf` matches **only** the second. Banning on
individual rule hits would ban people for pasting a code snippet.

In `DetectionOnly` nothing is refused, so no `Access denied` line is ever
emitted — which is precisely why the jail is inert in that mode. The would-be
block still appears, as a `Warning` carrying the same
`Inbound Anomaly Score Exceeded` text and `[id "949110"]`, which is what the
review command above reads.

The JSON audit record is the companion: it holds the full request, and
`transaction.is_interrupted` is the unambiguous "this was actually refused"
flag. It does **not** contain the matched rule ids — `transaction.messages` comes
back empty in practice — so rule-level tuning reads the warn lines, not the
audit JSON.

## Tuning knobs already set

| Directive | Value | Why |
|---|---|---|
| `SecResponseBodyAccess` | `Off` | Inspecting responses on a git host costs CPU and catches nothing worth catching. |
| `SecRequestBodyLimitAction` | `ProcessPartial` | Truncate and inspect rather than reject: a large but legitimate attachment should not 413 because the WAF gave up. |
| `SecAuditEngine` | `RelevantOnly` | Auditing every request would pour full request volume into journald and then Cloud Logging. |
| `SecAuditLogRelevantStatus` | `^(?:5[0-9]{2}|403)$` | The CRS default audits every `401`, and unauthenticated API and web probes generate those constantly on a public host. Narrowed to real refusals and server errors. |

Note that `RelevantOnly` still audits any transaction that trips a rule,
whatever its status — that is deliberate, since it is what keeps `DetectionOnly`
useful. So the audit log is not silent between blocks; it is bounded by how many
rules your traffic trips, which is exactly what the tuning pass reduces.

The Caddy-level `request_body max_size 512MB` governs the **git** branch; the
much smaller `SecRequestBodyLimit` governs the **WAF** branch. They apply to
different routes and are not a mismatch to be "fixed".

## Versions

`image/caddy.version` and `image/coraza.version` are **coupled**: coraza-caddy
pins a minimum Caddy version, and a mismatch fails the build at `go get` with
`requires github.com/caddyserver/caddy/v2@vX, but vY is requested`. Bump both
together. The weekly scheduled rebuild picks up CRS and dependency fixes without
a version change.
