#!/usr/bin/env python3
"""Tests for the decisions in infra_deploy.py and release_gate.py.

Run: python3 -m unittest discover -s scripts/cicd   (make ci-check runs it)
"""
import argparse
import contextlib
import datetime
import hashlib
import io
import os
import shutil
import subprocess
import sys
import tempfile
import unittest
from contextlib import redirect_stderr, redirect_stdout
from unittest import mock

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
sys.path.insert(0, os.path.join(os.path.dirname(os.path.abspath(__file__)), "..", "ops"))
import gitea_ops as O  # noqa: E402
import infra_deploy as D  # noqa: E402
import release_gate as G  # noqa: E402

U = "urn:pulumi:prod::gitea::"


def step(op, rtype, name, parent=""):
    return {"op": op, "urn": f"{U}{parent + '$' if parent else ''}{rtype}::{name}"}


class Guard(unittest.TestCase):
    def test_vm_replace_needs_approval(self):
        p = {"steps": [step("replace", "gcp:compute/instance:Instance", "gitea-vm"),
                       step("update", "gcp:storage/bucketObject:BucketObject", "vm/x")]}
        self.assertEqual(D.destructive_steps(p), [("replace", "gcp:compute/instance:Instance", "gitea-vm")])

    def test_replacement_halves_and_deletes(self):
        p = {"steps": [step("create-replacement", "gcp:compute/disk:Disk", "gitea-data"),
                       step("delete-replaced", "gcp:compute/disk:Disk", "gitea-data"),
                       step("delete", "gcp:storage/bucket:Bucket", "gitea-backups"),
                       step("replace", "gcp:dns/recordSet:RecordSet", "gitea-a", parent="gcp:dns/managedZone:ManagedZone")]}
        self.assertEqual(len(D.destructive_steps(p)), 4)

    def test_ordinary_changes_go(self):
        p = {"steps": [step("update", "gcp:compute/instance:Instance", "gitea-vm"),
                       step("replace", "gcp:storage/bucketObject:BucketObject", "vm/fail2ban/x"),
                       step("create", "gcp:storage/bucket:Bucket", "new"),
                       step("delete", "kubernetes:core/v1:ConfigMap", "old"),
                       step("same", "gcp:compute/disk:Disk", "gitea-data")]}
        self.assertEqual(D.destructive_steps(p), [])

    def test_malformed_preview(self):
        self.assertEqual(D.destructive_steps({}), [])
        self.assertEqual(D.destructive_steps({"steps": [{"op": "delete", "urn": "nonsense"}]}), [])


class Converged(unittest.TestCase):
    def test_provider_token_is_not_a_change(self):
        p = {"steps": [{"op": "update", "urn": f"{U}pulumi:providers:kubernetes::gitea-runners"},
                       step("same", "gcp:compute/instance:Instance", "gitea-vm")]}
        self.assertEqual(D.leftover_changes(p), [])

    def test_drift_is(self):
        p = {"steps": [step("update", "gcp:compute/instance:Instance", "gitea-vm")]}
        self.assertEqual(D.leftover_changes(p), ["update gitea-vm"])


class GoCache(unittest.TestCase):
    def test_name_is_stable_and_keyed_on_deps_and_toolchain(self):
        n = D.go_cache_name(b"module x\n", b"a v1 h1:x\n", "go1.26.9")
        self.assertEqual(n, D.go_cache_name(b"module x\n", b"a v1 h1:x\n", "go1.26.9"))
        self.assertTrue(n.startswith(D.GO_CACHE_PREFIX) and n.endswith(".tar"))
        for other in (D.go_cache_name(b"module x\n", b"a v2 h1:y\n", "go1.26.9"),
                      D.go_cache_name(b"module x\ntoolchain go1.27.0\n", b"a v1 h1:x\n", "go1.26.9"),
                      D.go_cache_name(b"module x\n", b"a v1 h1:x\n", "go1.27.0")):
            self.assertNotEqual(n, other)

    def test_name_changes_with_the_pinned_tools(self):
        n = D.go_cache_name(b"module x\n", b"a v1 h1:x\n", "go1.26.9")
        with mock.patch.dict(D.TOOLS, {"pulumi": ("https://x/pulumi-v9.tar.gz", "0" * 64, "pulumi")}):
            self.assertNotEqual(D.go_cache_name(b"module x\n", b"a v1 h1:x\n", "go1.26.9"), n)

    def test_newest_object_ignores_the_total_line(self):
        ls = ("  10  2026-10-20T01:00:00Z  gs://b/infra-go-cache-aaaa.tar\n"
              "  20  2026-10-24T01:00:00Z  gs://b/infra-go-cache-bbbb.tar\n"
              "  30  2026-10-22T01:00:00Z  gs://b/infra-go-cache-cccc.tar\n"
              "TOTAL: 3 objects, 60 bytes (60B)\n")
        self.assertEqual(D.newest_object(ls), "gs://b/infra-go-cache-bbbb.tar")
        self.assertIsNone(D.newest_object(""))
        self.assertIsNone(D.newest_object("TOTAL: 0 objects, 0 bytes (0B)\n"))

    def saves(self, mode, hit):
        d = D.Deploy(argparse.Namespace(mode=mode, state_bucket="ci-deploy-state"))
        d.go_cache, d.go_cache_hit = "infra-go-cache-abcd.tar", hit
        ok = subprocess.CompletedProcess([], 0)
        with mock.patch.object(D, "run", return_value=ok) as run, redirect_stdout(io.StringIO()):
            d.save_go_cache()
            d.save_go_cache()  # a second prepare in the same run
        return [c.args[0][:3] for c in run.call_args_list]

    def test_save_only_from_a_deploy_without_an_exact_hit_and_only_once(self):
        self.assertEqual(self.saves("preview", hit=False), [])
        self.assertEqual(self.saves("up", hit=True), [])
        self.assertEqual(self.saves("up", hit=False),
                         [["tar", "-cf", mock.ANY], ["gcloud", "storage", "cp"]])


class Tools(unittest.TestCase):
    URL = "https://x/tool.tar.gz"

    def fetch(self, sha):
        """fetch_tools with one tool whose download is its own URL's bytes:
        (the error or None, PATH afterwards, the commands it ran)."""
        dest = tempfile.mkdtemp()
        self.addCleanup(shutil.rmtree, dest)
        body = lambda url, timeout: contextlib.nullcontext(io.BytesIO(url.encode()))
        with mock.patch.dict(D.TOOLS, {"tool": (self.URL, sha, "tool/bin")}, clear=True), \
                mock.patch.dict(os.environ, {"PATH": "/usr/bin"}), \
                mock.patch.object(D.urllib.request, "urlopen", side_effect=body), \
                mock.patch.object(D, "run") as run, redirect_stdout(io.StringIO()):
            try:
                D.fetch_tools(dest)
                err = None
            except D.Failed as e:
                err = e
            return err, os.environ["PATH"], [c.args[0][:2] for c in run.call_args_list], dest

    def test_a_checked_download_goes_first_on_path(self):
        err, path, ran, dest = self.fetch(hashlib.sha256(self.URL.encode()).hexdigest())
        self.assertIsNone(err)
        self.assertEqual(path, os.path.join(dest, "tool/bin") + os.pathsep + "/usr/bin")
        self.assertEqual(ran, [["tar", "-xzf"]])

    def test_a_wrong_checksum_is_never_unpacked(self):
        err, path, ran, _ = self.fetch("0" * 64)
        self.assertIn("not the pinned", str(err))
        self.assertEqual((path, ran), ("/usr/bin", []))

    def test_the_pins_are_https_and_sha256(self):
        for url, sha, _ in D.TOOLS.values():
            self.assertTrue(url.startswith("https://"), url)
            self.assertRegex(sha, r"^[0-9a-f]{64}$")


class SshKeys(unittest.TestCase):
    NOW = 2_000_000_000 * 1_000_000  # usec
    LISTING = [
        {"key": "never", "value": {"key": "ssh-rsa A", "fingerprint": "never"}},
        {"key": "expired", "value": {"expirationTimeUsec": str(NOW - 1)}},
        {"key": "live", "value": {"expirationTimeUsec": str(NOW + 3_600_000_000)}},
        {"key": "bare"},
    ]

    def test_stale_are_the_never_expiring_and_the_expired(self):
        self.assertEqual(O.stale_ssh_keys(self.LISTING, self.NOW), ["never", "expired", "bare"])
        self.assertEqual(O.stale_ssh_keys([], self.NOW), [])

    def test_infra_deploy_prunes_once_and_spares_a_live_key(self):
        import json
        d = D.Deploy(argparse.Namespace(mode="up", state_bucket="ci-deploy-state"))
        listing = subprocess.CompletedProcess([], 0, stdout=json.dumps(self.LISTING))
        with mock.patch.object(D, "run", return_value=listing) as run, \
                mock.patch.object(D.time, "time", return_value=self.NOW / 1e6), redirect_stdout(io.StringIO()):
            d.prune_ssh_keys()
            d.prune_ssh_keys()  # a rollback's second sync
        removed = [c.args[0][-1] for c in run.call_args_list if "remove" in c.args[0]]
        self.assertEqual(removed, ["--key=never", "--key=expired", "--key=bare"])


class VmConfig(unittest.TestCase):
    """How a tree's VM config is applied: the legacy pull, or the playbook."""

    def tree(self, *files, content=""):
        root = tempfile.mkdtemp()
        self.addCleanup(shutil.rmtree, root)
        for f in files:
            p = os.path.join(root, f)
            os.makedirs(os.path.dirname(p), exist_ok=True)
            with open(p, "w") as fh:
                fh.write(content)
        return root

    def deploy(self, **kw):
        a = dict(mode="up", state_bucket="", vm="gitea-vm", zone="z", app_project="p", approved=False,
                 tree="", gitea_url="https://gitea.example")
        a.update(kw)
        return D.Deploy(argparse.Namespace(**a))

    def test_the_tree_decides_and_bootstrap_wins(self):
        self.assertEqual(D.tree_kind(self.tree("vm/bootstrap.sh")), "legacy")
        self.assertEqual(D.tree_kind(self.tree("vm/ansible/site.yml")), "ansible")
        self.assertEqual(D.tree_kind(self.tree("vm/bootstrap.sh", "vm/ansible/site.yml")), "legacy")
        self.assertIsNone(D.tree_kind(self.tree("vm/quadlets/x")))

    def test_nftables_is_only_ever_loaded_never_restarted(self):
        bad_unit = "- systemd_service:\n    name: nftables\n    state: restarted\n"
        bad_shell = "- command: systemctl reload nftables\n"
        good = "- systemd_service:\n    name: nftables\n    state: started\n    enabled: true\n- command: nft -f x\n"
        self.assertEqual(len(D.nftables_restarts(self.tree("vm/ansible/roles/f/tasks/main.yml", content=bad_unit))), 1)
        self.assertEqual(len(D.nftables_restarts(self.tree("vm/ansible/handlers.yml", content=bad_shell))), 1)
        self.assertEqual(D.nftables_restarts(self.tree("vm/ansible/roles/f/tasks/main.yml", content=good)), [])

    def test_sync_follows_the_tree(self):
        d = self.deploy()
        with mock.patch.object(d, "prime_ssh"), mock.patch.object(d, "wait_booted"), \
                mock.patch.object(d, "ssh") as ssh, mock.patch.object(d, "playbook") as pb:
            d.vm_sync(self.tree("vm/bootstrap.sh"))
            self.assertIn("systemctl start gitea-config-sync.service", ssh.call_args_list[-1].args[0])
            self.assertIn("google-startup-scripts.service", ssh.call_args_list[0].args[0])
            pb.assert_not_called()
            ssh.reset_mock()
            d.vm_sync(self.tree("vm/ansible/site.yml"))
            pb.assert_called_once()
            ssh.assert_not_called()
            with self.assertRaises(D.Failed):
                d.vm_sync(self.tree("vm/x"))

    def applied(self, ids):
        d = self.deploy()
        with mock.patch.object(d, "prepare", return_value="/infra"), mock.patch.object(d, "pulumi"), \
                mock.patch.object(d, "instance_id", side_effect=ids), mock.patch.object(d, "vm_sync") as sync, \
                mock.patch.object(d, "wait_https") as https, mock.patch.object(d, "ops"), \
                mock.patch.object(d, "preview_json", return_value={"steps": []}), redirect_stdout(io.StringIO()):
            d.apply("/src", "deploy")
        return sync.call_args.args, https.called

    def test_a_new_instance_id_is_a_replacement_and_waits_for_https(self):
        self.assertEqual(self.applied(["1", "2"]), (("/src", True), True))
        self.assertEqual(self.applied(["1", "1"]), (("/src", False), False))

    def test_priming_retries_only_when_told_to_wait(self):
        d = self.deploy()
        no = subprocess.CompletedProcess([], 255, stdout="", stderr="4003: failed to connect")
        yes = subprocess.CompletedProcess([], 0, stdout="sa_123\n", stderr="")
        clock = iter(range(0, 10_000, 15))
        with mock.patch.object(d, "prune_ssh_keys"), mock.patch.object(d, "ssh", side_effect=[no, no, yes]), \
                mock.patch.object(D.time, "time", side_effect=lambda: next(clock)), \
                mock.patch.object(D.time, "sleep"), redirect_stdout(io.StringIO()):
            self.assertEqual(d.prime_ssh(wait=True), "sa_123")
        with mock.patch.object(d, "prune_ssh_keys"), mock.patch.object(d, "ssh", return_value=no), \
                self.assertRaises(D.Failed):
            d.prime_ssh(wait=False)

    def test_controller_installs_hash_locked_outside_the_tree_once(self):
        src = self.tree("vm/ansible/requirements.txt", content="ansible-core==1 --hash=sha256:00\n")
        d = self.deploy()
        def pip(cmd, **kw):
            os.makedirs(os.path.join(cmd[cmd.index("--target") + 1], "bin"), exist_ok=True)
            return subprocess.CompletedProcess(cmd, 0)
        with mock.patch.object(D, "run", side_effect=pip) as run:
            env = d.ansible_env(src)
            d.ansible_env(src)
        self.assertEqual(run.call_count, 1)
        cmd = run.call_args.args[0]
        self.assertIn("--require-hashes", cmd)
        self.assertTrue(cmd[cmd.index("--target") + 1].startswith(d.tmp))
        self.assertEqual(env["ANSIBLE_CONFIG"], os.path.join(src, "vm", "ansible", "ansible.cfg"))

    def test_vm_apply_to_prod_needs_approval(self):
        src = self.tree("vm/ansible/site.yml")
        d = self.deploy(mode="vm-apply", tree=src)
        with mock.patch.object(d, "ansible_checks"), mock.patch.object(d, "playbook") as pb, \
                self.assertRaises(D.Failed):
            d.vm(check=False)
        pb.assert_not_called()
        d = self.deploy(mode="vm-apply", tree=src, vm="gitea-rehearsal")
        with mock.patch.object(d, "ansible_checks"), mock.patch.object(d, "playbook") as pb:
            self.assertEqual(d.vm(check=False), 0)
        pb.assert_called_once_with(src, check=False)


class PreRestore(unittest.TestCase):
    NOW = datetime.datetime(2026, 10, 31, 4, 0, tzinfo=datetime.timezone.utc)

    def test_only_week_old_trees_named_exactly_so(self):
        names = [".pre-restore-20261023T230921Z",   # 7d 4h old
                 ".pre-restore-20261024T230540Z",   # 6d 5h old
                 ".pre-restore-20261024T040000Z",   # exactly 7 days: kept
                 ".pre-restore-20261313T000000Z",   # no such date
                 ".pre-restore-20261001T000000Z/../data", ".pre-restore-", "data", ""]
        self.assertEqual(O.stale_pre_restore(names, self.NOW), [".pre-restore-20261023T230921Z"])
        self.assertEqual(O.stale_pre_restore([], self.NOW), [])

    def prune(self, listing, fail=False):
        ops = O.Ops(argparse.Namespace(region="r", project="p", repo="gitea", cmd="backup", dry_run=False))
        calls = []

        def vm(step, *args, mutate=True):
            calls.append((step, *args))
            if fail:
                raise subprocess.CalledProcessError(255, ["vm-steps", step])
            return listing if step == "pre-restore-list" else ""
        with mock.patch.object(ops, "vm", side_effect=vm), mock.patch.object(O, "utcnow", return_value=self.NOW), \
                redirect_stderr(io.StringIO()):
            ops.prune_pre_restore()
        return calls, ops.manifest.get("pre_restore_removed")

    def test_removes_the_old_ones_and_records_them(self):
        calls, removed = self.prune(".pre-restore-20261023T230921Z\n.pre-restore-20261024T230540Z\n")
        self.assertEqual(calls, [("pre-restore-list",), ("pre-restore-remove", ".pre-restore-20261023T230921Z")])
        self.assertEqual(removed, [".pre-restore-20261023T230921Z"])

    def test_nothing_old_means_no_remove(self):
        self.assertEqual(self.prune(".pre-restore-20261030T000000Z\n"), ([("pre-restore-list",)], None))
        self.assertEqual(self.prune(""), ([("pre-restore-list",)], None))

    def test_a_vm_error_never_fails_the_backup(self):
        calls, removed = self.prune("", fail=True)
        self.assertEqual((calls, removed), ([("pre-restore-list",)], None))


class Gate(unittest.TestCase):
    def test_hash_is_stable_and_covers_the_engine(self):
        self.assertEqual(G.code_hash("image"), G.code_hash("image"))
        self.assertIn("scripts/ops/gitea_ops.py", G.FILES["image"])
        self.assertIn("scripts/ops/gitea_ops.py", G.FILES["infra"])
        self.assertIn("scripts/cicd/release_gate.py", G.FILES["image"])
        for files in G.FILES.values():
            for f in files:
                self.assertTrue(os.path.exists(os.path.join(G.ROOT, f)), f)

    def test_hash_changes_with_content(self):
        real = open

        def fake_open(path, mode="r", *a, **k):
            if path.endswith("vm-steps.sh") and "b" in mode:
                with real(path, mode, *a, **k) as f:
                    return io.BytesIO(f.read() + b"\n# edited")
            return real(path, mode, *a, **k)
        before = G.code_hash("image")
        with mock.patch("builtins.open", fake_open):
            self.assertNotEqual(G.code_hash("image"), before)

    def check(self, listing, approved=False, rc=0):
        a = argparse.Namespace(pipeline="image", bucket="b", approved=approved)
        out = subprocess.CompletedProcess([], rc, stdout=listing, stderr="")
        with mock.patch.object(G, "gcloud", return_value=out), redirect_stdout(io.StringIO()), \
                redirect_stderr(io.StringIO()) as err:
            return G.cmd_check(a), err.getvalue()

    def test_refuses_until_both_image_drills_pass(self):
        h = G.code_hash("image")
        p = f"gs://b/drills/image/{h}/"
        self.assertEqual(self.check("", rc=1)[0], 1)
        self.assertEqual(self.check(f"{p}fail-verify-b1.json\n")[0], 1)
        self.assertEqual(self.check(f"{p}fail-verify-b1.json\n{p}fail-after-migrate-b2.json\n")[0], 0)

    def test_approval_overrides_with_a_warning(self):
        rc, err = self.check("", approved=True, rc=1)
        self.assertEqual(rc, 0)
        self.assertIn("WARNING", err)

    def test_a_drill_name_prefix_is_not_a_pass(self):
        h = G.code_hash("image")
        p = f"gs://b/drills/image/{h}/"
        # "fail-verify" must not be satisfied by some other drill's object.
        self.assertEqual(self.check(f"{p}fail-verifyx.json\n{p}fail-after-migrate-b2.json\n")[0], 1)


class Superseded(unittest.TestCase):
    def test_newer_image_change_supersedes(self):
        c = {"commits": [{"sha": "aaa", "files": [{"filename": "docs/x.md"}]},
                         {"sha": "bbb", "files": [{"filename": "image/gitea/Dockerfile"}]}]}
        self.assertEqual(G.newer_touches(c, G.IMAGE_PATHS), "bbb")

    def test_unrelated_or_lookalike_does_not(self):
        c = {"commits": [{"sha": "aaa", "files": [{"filename": "images/logo.png"}, {"filename": "infra/main.go"},
                                                  {"filename": "cloudbuild/image.yaml.bak"}]}]}
        self.assertIsNone(G.newer_touches(c, G.IMAGE_PATHS))
        self.assertIsNone(G.newer_touches({}, G.IMAGE_PATHS))

    def test_pipeline_file_counts(self):
        c = {"commits": [{"sha": "ccc", "files": [{"filename": "cloudbuild/image.yaml"}]}]}
        self.assertEqual(G.newer_touches(c, G.IMAGE_PATHS), "ccc")


class RelayAgrees(unittest.TestCase):
    """cicd/relay flags the same files this gate hashes, for the same pipelines."""

    def test_release_paths_match(self):
        import re
        src = open(os.path.join(G.ROOT, "cicd/relay/webhook.go")).read()
        block = src[src.index("var ReleasePaths"):]
        block = block[:block.index("\n}\n")]
        relay = {}
        for path, pls in re.findall(r'"([^"]+)":\s*\{([^}]*)\}', block):
            relay[path] = {"image" if "Image" in x else "infra" for x in pls.split(",") if x.strip()}
        gate = {}
        for pl, files in G.FILES.items():
            for f in files:
                gate.setdefault(f, set()).add(pl)
        self.assertEqual(relay, gate)


if __name__ == "__main__":
    unittest.main()
