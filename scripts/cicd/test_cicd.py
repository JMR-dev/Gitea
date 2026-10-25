#!/usr/bin/env python3
"""Tests for the decisions in infra_deploy.py and release_gate.py.

Run: python3 -m unittest discover -s scripts/cicd   (make ci-check runs it)
"""
import argparse
import datetime
import io
import os
import subprocess
import sys
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
