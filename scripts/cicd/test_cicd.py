#!/usr/bin/env python3
"""Tests for the decisions in infra_deploy.py and release_gate.py.

Run: python3 -m unittest discover -s scripts/cicd   (make ci-check runs it)
"""
import argparse
import contextlib
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
