#!/usr/bin/env python3
"""Tests for the decisions in infra_deploy.py, release_gate.py and the release
engine (scripts/ops: gitea_ops.py, launch_job.py). The engine's checks that
need ansible-core or podman are scripts/cicd/ops_check.py (make ops-check).

Run: python3 -m unittest discover -s scripts/cicd   (make ci-check runs it)
"""
import argparse
import contextlib
import datetime
import hashlib
import io
import json
import os
import re
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
import launch_job as L  # noqa: E402
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


class InfraLock(unittest.TestCase):
    """Deploy.lock: one infra deploy at a time. A cancelled build never reaches
    unlock(), so a lock whose holder Cloud Build says has finished is taken
    over; one whose holder can't be proven finished is waited for, and goes
    stale after LOCK_STALE as before.

    gcloud is faked by argv over one lock object: its live generation and
    body, plus every generation's body (the deploy-state bucket is versioned,
    so an old generation still reads). Cloud Build's answers are looked up by
    (build id, region). The clock moves only when the code sleeps."""

    BUCKET = "gitea-ci-1-deploy-state"
    URL = "gs://gitea-ci-1-deploy-state/infra.lock"
    ME = "6f1c2a8e-0b7d-4c55-9e3a-1d2f3a4b5c6d"
    HOLDER = "0abf2591-ff7f-40b5-a8a8-42f72506a7a6"
    WINNER = "37925220-7798-489a-8c1f-798f4c21488d"
    REGION = "us-east5"
    T0 = 1_794_000_000.0
    FINISHED = ("SUCCESS", "FAILURE", "INTERNAL_ERROR", "TIMEOUT", "CANCELLED", "EXPIRED")
    RUNNING = ("WORKING", "QUEUED", "PENDING", "STATUS_UNKNOWN")
    NOT_FOUND = (1, "", "ERROR: (gcloud.builds.describe) NOT_FOUND: Requested entity was not found.")
    DENIED = (1, "", "ERROR: (gcloud.builds.describe) PERMISSION_DENIED: The caller does not have permission "
                     "(cloudbuild.builds.get)")

    def setUp(self):
        self.reset()

    def reset(self):
        self.clock = [self.T0]
        self.live = None      # (generation, body) of the lock object, or None
        self.versions = {}    # generation: body, for every generation ever written
        self.gen = 1_700_000_000_000_000
        self.builds = {}      # (build id, region): (rc, stdout, stderr) of `gcloud builds describe`
        self.calls = []       # (argv, clock) of every command, in order
        self.slept = []
        self.events = []      # (time, fn): fn runs at the first command once the clock reaches time
        self.hooks = {}       # one-shot: "describe" (before), "rm" (before), "rm-ok" (after a removal)
        self.rm_fails = False
        self.log = ""

    # --- the fake ---------------------------------------------------------

    def stamp(self, t):
        """A finishTime as gcloud prints it."""
        return datetime.datetime.fromtimestamp(t, datetime.timezone.utc).strftime("%Y-%m-%dT%H:%M:%S") + ".451932Z"

    def build(self, build_id, status, finished=None, region=REGION):
        """What Cloud Build says of a build: its status and, if it finished at
        clock time `finished`, its finishTime."""
        out = status + (f"\t{self.stamp(finished)}" if finished is not None else "") + "\n"
        self.builds[(build_id, region)] = (0, out, "")

    def hold(self, build=HOLDER, at=None, region=REGION, raw=None):
        """Put a lock in place as another build wrote it; its generation."""
        body = raw if raw is not None else json.dumps(
            {"build": build, "sha": "old", "at": self.T0 if at is None else at, "region": region})
        self.gen += 1
        self.live = (self.gen, body)
        self.versions[self.gen] = body
        return self.gen

    def release(self):
        self.live = None

    def release_at(self, t):
        self.events.append((t, self.release))

    def hook(self, name):
        fn = self.hooks.pop(name, None)
        if fn:
            fn()

    @staticmethod
    def flag(cmd, name):
        for x in cmd:
            if x.startswith(name + "="):
                return x.split("=", 1)[1]
        return None

    def fake(self, cmd, **kw):
        self.calls.append((list(cmd), self.clock[0]))
        for ev in list(self.events):
            if self.clock[0] >= ev[0]:
                self.events.remove(ev)
                ev[1]()
        ok = lambda out="": subprocess.CompletedProcess(cmd, 0, stdout=out, stderr="")
        no = lambda err: subprocess.CompletedProcess(cmd, 1, stdout="", stderr=err)
        if cmd[:3] == ["gcloud", "storage", "cp"]:
            self.assertEqual(cmd, ["gcloud", "storage", "cp", "-", self.URL, "--if-generation-match=0"])
            if self.live:
                return no("ERROR: At least one of the pre-conditions you specified did not hold. 412")
            self.gen += 1
            self.live = (self.gen, kw["stdin"])
            self.versions[self.gen] = kw["stdin"]
            return ok()
        if cmd[:4] == ["gcloud", "storage", "objects", "describe"]:
            self.assertEqual(cmd, ["gcloud", "storage", "objects", "describe", self.URL, "--format=value(generation)"])
            self.hook("describe")
            return ok(f"{self.live[0]}\n") if self.live else no(f"ERROR: {self.URL} not found: 404.")
        if cmd[:3] == ["gcloud", "storage", "cat"]:
            url, _, g = cmd[3].partition("#")
            self.assertEqual((len(cmd), url), (4, self.URL))
            body = self.versions.get(int(g)) if g.isdigit() else None
            return ok(body) if body is not None else no(f"ERROR: {cmd[3]} not found: 404.")
        if cmd[:3] == ["gcloud", "storage", "rm"]:
            self.assertEqual(cmd[:4], ["gcloud", "storage", "rm", self.URL])
            self.hook("rm")
            want = self.flag(cmd, "--if-generation-match")
            if self.rm_fails or not self.live or (want is not None and want != str(self.live[0])):
                return no("ERROR: At least one of the pre-conditions you specified did not hold. 412")
            self.live = None
            self.hook("rm-ok")
            return ok()
        if cmd[:3] == ["gcloud", "builds", "describe"]:
            rc, out, err = self.builds.get((cmd[3], self.flag(cmd, "--region")), self.NOT_FOUND)
            return subprocess.CompletedProcess(cmd, rc, stdout=out, stderr=err)
        raise AssertionError(f"unexpected command: {cmd}")

    def lock(self, **kw):
        """Deploy.lock() against the fake: (the Deploy, the Failed or None).
        Its output is in self.log."""
        a = dict(mode="up", state_bucket=self.BUCKET, build_id=self.ME, sha="new", build_region=self.REGION)
        a.update(kw)
        d = D.Deploy(argparse.Namespace(**a))
        self.addCleanup(shutil.rmtree, d.tmp, True)

        def sleep(s):
            self.slept.append(s)
            self.clock[0] += s
        out, err = io.StringIO(), None
        with mock.patch.object(D, "run", side_effect=self.fake), \
                mock.patch.object(D.time, "time", side_effect=lambda: self.clock[0]), \
                mock.patch.object(D.time, "sleep", side_effect=sleep), redirect_stdout(out):
            try:
                d.lock()
            except D.Failed as e:
                err = e
        self.log = out.getvalue()
        return d, err

    def ran(self, *prefix):
        return [c for c, _ in self.calls if c[:len(prefix)] == list(prefix)]

    def rms(self):
        return self.ran("gcloud", "storage", "rm")

    def described(self):
        """The build ids `gcloud builds describe` was asked about."""
        return [c[3] for c in self.ran("gcloud", "builds", "describe")]

    def assert_ours(self, d):
        self.assertTrue(d.locked)
        self.assertEqual(json.loads(self.live[1])["build"], self.ME)

    # --- the cases --------------------------------------------------------

    def test_the_statuses(self):
        self.assertEqual(D.FINISHED, set(self.FINISHED))
        self.assertTrue(D.FINISHED.isdisjoint(self.RUNNING))
        self.assertEqual(D.LOCK_GRACE, 60)

    def test_utc_seconds(self):
        self.assertEqual(D.utc_seconds("2026-11-07T03:43:54.451932Z"),
                         datetime.datetime(2026, 11, 7, 3, 43, 54, tzinfo=datetime.timezone.utc).timestamp())
        self.assertEqual(D.utc_seconds(self.stamp(self.T0)), self.T0)
        for bad in ("", "yesterday", "2026-13-01T00:00:00Z", None, 17):
            self.assertIsNone(D.utc_seconds(bad), repr(bad))

    def test_a_free_lock_is_taken(self):
        self.build(self.ME, "WORKING")
        d, err = self.lock()
        self.assertIsNone(err)
        self.assert_ours(d)
        self.assertEqual(json.loads(self.live[1]), {"build": self.ME, "sha": "new", "at": self.T0, "region": self.REGION})
        self.assertEqual(len(self.ran("gcloud", "storage", "cp")), 1)
        self.assertEqual(self.rms(), [])
        # The self-check: this build's own status, from where it runs.
        desc, = self.ran("gcloud", "builds", "describe")
        self.assertEqual(desc[3], self.ME)
        self.assertIn("--project=gitea-ci-1", desc)
        self.assertIn("--region=us-east5", desc)
        self.assertIn(f"holding {self.URL} as build {self.ME} in us-east5 (WORKING): should this build die, "
                      "the next infra build takes the lock over", self.log)
        self.assertNotIn("WARNING", self.log)
        self.assertEqual(self.slept, [])

    def test_our_own_status_unreadable_still_locks_with_a_warning(self):
        for name, kw, cause in (("no cloudbuild.builds.get", {}, "cloudbuild.builds.get"),
                                ("no BUILD_REGION", {"build_region": ""}, "BUILD_REGION"),
                                ("a local run", {"build_id": "local"}, "local run")):
            with self.subTest(name):
                self.reset()
                self.builds[(self.ME, self.REGION)] = self.DENIED
                d, err = self.lock(**kw)
                self.assertIsNone(err)
                self.assertTrue(d.locked)
                self.assertIn("WARNING", self.log)
                self.assertIn(cause, self.log)
                self.assertEqual(self.described(), [self.ME] if not kw else [])

    def test_a_finished_holder_is_taken_over(self):
        for status in self.FINISHED:
            with self.subTest(status):
                self.reset()
                self.build(self.ME, "WORKING")
                self.build(self.HOLDER, status, finished=self.T0 - 300)
                g = self.hold()
                d, err = self.lock()
                self.assertIsNone(err)
                self.assert_ours(d)
                self.assertEqual(self.rms(), [["gcloud", "storage", "rm", self.URL, f"--if-generation-match={g}"]])
                storage = [c[2] for c, _ in self.calls if c[1] == "storage"]
                self.assertEqual(storage, ["cp", "objects", "cat", "rm", "cp"])
                self.assertEqual(self.ran("gcloud", "storage", "cat"), [["gcloud", "storage", "cat", f"{self.URL}#{g}"]])
                self.assertEqual(self.slept, [])
                line, = [x for x in self.log.splitlines() if "GITEA_INFRA_LOCK_TAKEOVER" in x]
                self.assertIn(f"build={self.HOLDER} sha=old: {status} at ", line)
                self.assertIn(f"still holding {self.URL}; taking it over", line)
                self.assertEqual("it stopped mid-run" in line, status != "SUCCESS")

    def test_a_running_holder_is_waited_for(self):
        for status in self.RUNNING:
            with self.subTest(status):
                self.reset()
                self.build(self.ME, "WORKING")
                self.build(self.HOLDER, status)
                self.hold()
                self.release_at(self.T0 + 600)
                d, err = self.lock()
                self.assertIsNone(err)
                self.assert_ours(d)
                self.assertEqual(self.rms(), [])
                self.assertGreaterEqual(self.clock[0], self.T0 + 600)
                self.assertIn(f"another infra deploy holds the lock (build {self.HOLDER}: {status}", self.log)
                self.assertNotIn("TAKEOVER", self.log)

    def test_a_holder_that_cannot_be_checked_is_never_taken(self):
        for name, answer in (("permission denied", self.DENIED), ("not found", self.NOT_FOUND),
                             ("empty", (0, "", "")), ("empty with a tab", (0, "\t\n", "")),
                             ("garbage", (0, "{]\x00 nonsense\n", "")),
                             ("a lookalike status", (0, f"SUCCESSFUL\t{self.stamp(self.T0 - 600)}\n", ""))):
            with self.subTest(name):
                self.reset()
                self.builds[(self.HOLDER, self.REGION)] = answer
                self.hold()
                d, err = self.lock()
                self.assertIsInstance(err, D.Failed)
                self.assertIn(f"waited 45 min for {self.URL} (build {self.HOLDER}: ", str(err))
                self.assertFalse(d.locked)
                self.assertEqual(self.rms(), [])
                if answer[0]:
                    self.assertIn("Cloud Build did not say: ", str(err))

    def test_old_local_and_garbled_locks_are_waited_for_without_asking(self):
        lock = lambda **kw: json.dumps({"sha": "old", "at": self.T0, **kw})
        for name, raw in (("from before takeover: no region", lock(build=self.HOLDER)),
                          ("a laptop's make vm-apply", lock(build="local", region=self.REGION)),
                          ("not JSON", "{not json"),
                          ("not an object", "[1, 2]"),
                          ("no build", lock(region=self.REGION)),
                          ("a shell-y build id", lock(build="$(id); --region=x", region=self.REGION)),
                          ("an upper-case id", lock(build=self.HOLDER.upper(), region=self.REGION)),
                          ("a shell-y region", lock(build=self.HOLDER, region="us-east5 --project=other")),
                          ("a region that is not one", lock(build=self.HOLDER, region="global"))):
            with self.subTest(name):
                self.reset()
                self.build(self.ME, "WORKING")
                # Were it ever asked, Cloud Build would say the holder is gone.
                self.build(self.HOLDER, "CANCELLED", finished=self.T0 - 600)
                self.hold(raw=raw)
                self.release_at(self.T0 + 120)
                d, err = self.lock()
                self.assertIsNone(err)
                self.assert_ours(d)
                self.assertEqual(self.rms(), [])
                self.assertEqual(self.described(), [self.ME], "only the self-check, once locked")
                self.assertIn("another infra deploy holds the lock (build ", self.log)

    def test_an_uncheckable_lock_still_goes_stale(self):
        for name, raw, answer in (
                ("no region", json.dumps({"build": self.HOLDER, "sha": "old", "at": self.T0 - 5 * 3600}), None),
                ("Cloud Build refuses", None, self.DENIED)):
            with self.subTest(name):
                self.reset()
                if answer:
                    self.builds[(self.HOLDER, self.REGION)] = answer
                self.hold(raw=raw, at=self.T0 - 5 * 3600)
                d, err = self.lock()
                self.assertEqual(str(err), f"{self.URL} is 5.0h old (build {self.HOLDER}). If that build is gone, "
                                           "check prod, then remove the lock by hand.")
                self.assertEqual(self.rms(), [])

    def test_a_finished_holder_older_than_the_stale_limit_is_taken_over(self):
        self.build(self.ME, "WORKING")
        self.build(self.HOLDER, "TIMEOUT", finished=self.T0 - 4 * 3600)
        g = self.hold(at=self.T0 - 7 * 3600)
        d, err = self.lock()
        self.assertIsNone(err)
        self.assert_ours(d)
        self.assertEqual(self.rms(), [["gcloud", "storage", "rm", self.URL, f"--if-generation-match={g}"]])
        self.assertIn("GITEA_INFRA_LOCK_TAKEOVER", self.log)

    def test_a_just_finished_holder_gets_its_grace(self):
        finish = self.T0 - 10
        self.build(self.ME, "WORKING")
        self.build(self.HOLDER, "CANCELLED", finished=finish)
        self.hold()
        d, err = self.lock()
        self.assertIsNone(err)
        self.assert_ours(d)
        (rm, at), = [(c, t) for c, t in self.calls if c[:3] == ["gcloud", "storage", "rm"]]
        self.assertGreaterEqual(at, finish + D.LOCK_GRACE)
        self.assertLess(at, finish + D.LOCK_GRACE + 30, "taken over on the first pass past the grace")

    def test_a_finished_holder_without_a_finish_time_is_never_taken(self):
        for out in ("CANCELLED\n", "CANCELLED\t\n", "CANCELLED\tyesterday\n"):
            with self.subTest(out):
                self.reset()
                self.builds[(self.HOLDER, self.REGION)] = (0, out, "")
                self.hold()
                d, err = self.lock()
                self.assertIn("waited 45 min", str(err))
                self.assertEqual(self.rms(), [])

    def test_a_build_that_replaces_the_dead_lock_first_wins(self):
        self.build(self.ME, "WORKING")
        self.build(self.HOLDER, "CANCELLED", finished=self.T0 - 5 * 3600)
        self.build(self.WINNER, "WORKING")
        dead = self.hold(at=self.T0 - 5 * 3600)
        winner = []

        def taken():  # the other build's takeover lands between our read and our rm
            self.release()
            winner.append(self.hold(build=self.WINNER, at=self.clock[0]))
            self.release_at(self.clock[0] + 300)
        self.hooks["rm"] = taken
        d, err = self.lock()
        self.assertIsNone(err, "no stale error for the 5 h old dead lock")
        self.assert_ours(d)
        self.assertEqual(self.rms(), [["gcloud", "storage", "rm", self.URL, f"--if-generation-match={dead}"]])
        self.assertNotIn(f"--if-generation-match={winner[0]}", sum(self.rms(), []))
        self.assertIn("but its lock could not be removed (another build may have taken it)", self.log)
        self.assertIn(f"another infra deploy holds the lock (build {self.WINNER}: WORKING", self.log)
        self.assertGreaterEqual(self.clock[0], self.T0 + 300)

    def test_a_build_that_creates_first_after_our_rm_wins(self):
        self.build(self.ME, "WORKING")
        self.build(self.HOLDER, "CANCELLED", finished=self.T0 - 600)
        self.build(self.WINNER, "WORKING")
        self.hold()

        def created():  # the other build's create lands between our rm and our create
            self.hold(build=self.WINNER, at=self.clock[0])
            self.release_at(self.clock[0] + 300)
        self.hooks["rm-ok"] = created
        d, err = self.lock()
        self.assertIsNone(err)
        self.assert_ours(d)
        self.assertEqual(len(self.rms()), 1)
        self.assertIn(f"another infra deploy holds the lock (build {self.WINNER}: WORKING", self.log)
        self.assertGreaterEqual(self.clock[0], self.T0 + 300)

    def test_a_failing_rm_never_spins(self):
        self.build(self.HOLDER, "CANCELLED", finished=self.T0 - 600)
        self.hold()
        self.rm_fails = True
        d, err = self.lock()
        self.assertIsInstance(err, D.Failed)
        self.assertIn(f"waited 45 min for {self.URL} (build {self.HOLDER}: CANCELLED at ", str(err))
        self.assertIn("could not be removed", str(err))
        self.assertFalse(d.locked)
        self.assertGreater(len(self.rms()), 1)
        self.assertLessEqual(len(self.rms()), D.LOCK_WAIT // 30 + 1)
        self.assertEqual(set(self.slept), {30})
        self.assertEqual(len(self.slept), len(self.rms()) - 1)

    def test_a_lock_gone_before_it_is_read_is_tried_again(self):
        self.build(self.ME, "WORKING")
        self.hold()
        self.hooks["describe"] = self.release  # the holder unlocks between our create and our read
        d, err = self.lock()
        self.assertIsNone(err)
        self.assert_ours(d)
        self.assertEqual(len(self.ran("gcloud", "storage", "cp")), 2)
        self.assertEqual(self.rms(), [])
        self.assertEqual(self.slept, [30], "one wait, then the create again")

    def test_the_build_region_reaches_the_lock(self):
        with open(os.path.join(D.ROOT, "cloudbuild", "infra.yaml")) as f:
            self.assertIn("      - BUILD_REGION=${LOCATION}\n", f.read())
        with mock.patch.dict(os.environ, {"BUILD_REGION": "us-east5"}), \
                mock.patch.object(sys, "argv", ["infra_deploy.py", "preview"]), \
                mock.patch.object(D, "ensure_auth_plugin_on_path"), \
                mock.patch.object(D, "Deploy") as deploy, self.assertRaises(SystemExit):
            D.main()
        self.assertEqual(deploy.call_args.args[0].build_region, "us-east5")

    def test_a_lock_held_by_this_build_is_ours(self):
        # A create that went through on the server but reported an error.
        self.build(self.ME, "WORKING")
        g = self.hold(build=self.ME)
        d, err = self.lock()
        self.assertIsNone(err)
        self.assert_ours(d)
        self.assertEqual(self.live[0], g)
        self.assertEqual(self.rms(), [])
        self.assertEqual(self.slept, [])

    def test_two_laptop_runs_never_share_the_lock(self):
        # Every laptop run is build "local": only a Cloud Build id is ever "ours".
        self.hold(build="local")
        self.release_at(self.T0 + 120)
        d, err = self.lock(build_id="local")
        self.assertIsNone(err)
        self.assertTrue(d.locked)
        self.assertGreaterEqual(self.clock[0], self.T0 + 120)
        self.assertEqual(self.rms(), [])


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
                raise subprocess.CalledProcessError(255, ["vm-step", step])
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


class EngineVm(unittest.TestCase):
    """Ops.vm: one VM step through ansible-playbook and ops.yml."""

    def ops(self, dry_run=False):
        o = O.Ops(argparse.Namespace(region="r", project="p", repo="gitea", cmd="release", dry_run=dry_run,
                                     vm="gitea-vm", zone="z"))
        o.pylib = "/py"
        work = tempfile.mkdtemp()
        self.addCleanup(shutil.rmtree, work)

        def connect():
            if not o.work:
                o.work = work
                os.mkdir(os.path.join(work, "calls"))
                o.inventory = os.path.join(work, "inventory.json")
        o.connect = connect
        return o

    def vm(self, o, *args, result=None, rc=0, mutate=True):
        """o.vm(*args) with ansible-playbook faked: it writes `result` (if
        any) where the call says and exits rc. (outcome, runs, stderr)."""
        runs = []

        def fake_run(cmd, **kw):
            with open(cmd[cmd.index("-e") + 1][1:]) as f:
                call = json.load(f)
            runs.append((cmd, kw, call))
            if result is not None:
                with open(call["result_file"], "w") as f:
                    f.write(result if isinstance(result, str) else json.dumps(result))
            return subprocess.CompletedProcess(cmd, rc, stdout="PLAY RECAP ...", stderr="")
        with mock.patch.object(O.subprocess, "run", side_effect=fake_run), redirect_stderr(io.StringIO()) as err:
            try:
                out = o.vm(*args, mutate=mutate)
            except subprocess.CalledProcessError as e:
                out = e
        return out, runs, err.getvalue()

    OK = {"rc": 0, "stdout": "sha256:abc\n", "stderr": "[vm-steps] progress", "failed": False, "msg": ""}

    def test_runs_the_playbook_with_the_call_and_returns_stdout(self):
        o = self.ops()
        out, runs, err = self.vm(o, "running-digest", "gitea", result=self.OK, mutate=False)
        self.assertEqual(out, "sha256:abc")
        self.assertIn("[vm-steps] progress", err)
        (cmd, kw, call), = runs
        self.assertEqual(cmd[:6], [sys.executable, "/py/bin/ansible-playbook", "-i", o.inventory, O.OPS_YML, "-e"])
        self.assertTrue(cmd[6].startswith("@" + os.path.join(o.work, "calls", "01-running-digest")))
        self.assertEqual(call["step"], "running-digest")
        self.assertEqual(call["step_args"], ["gitea"])
        self.assertTrue(call["result_file"].startswith(o.work))
        env = kw["env"]
        self.assertEqual(env["ANSIBLE_CONFIG"], os.path.join(O.HERE, "ansible", "ansible.cfg"))
        self.assertEqual(env["PYTHONPATH"].split(os.pathsep)[0], "/py")
        self.assertTrue(env["ANSIBLE_LOCAL_TEMP"].startswith(o.work) and env["ANSIBLE_HOME"].startswith(o.work))
        self.assertEqual(env["LC_ALL"], "C.UTF-8")
        # A second step is a second call file.
        _, runs, _ = self.vm(o, "restarts", "gitea.service", result=self.OK, mutate=False)
        self.assertIn("02-restarts", runs[0][0][6])

    def test_a_failed_step_raises_its_own_exit_code(self):
        res = {"rc": 7, "stdout": "", "stderr": "[vm-steps] ERROR: gitea.service is running", "failed": True,
               "msg": "non-zero return code"}
        e, _, err = self.vm(self.ops(), "migrate", "img", result=res, rc=2)
        self.assertIsInstance(e, subprocess.CalledProcessError)
        self.assertEqual((e.returncode, e.cmd), (7, ["vm-step", "migrate", "img"]))
        self.assertIn("gitea.service is running", err)
        # A failure with no output of its own (an assert) shows its message.
        res = {"rc": 1, "stdout": "", "stderr": "", "failed": True, "msg": "refused: step x"}
        e, _, err = self.vm(self.ops(), "stop", "gitea.service", result=res, rc=2)
        self.assertEqual(e.returncode, 1)
        self.assertIn("refused: step x", err)

    def test_no_result_or_a_bad_one_raises(self):
        for result, rc in ((None, 4), (None, 0), ("{not json", 0), ({"rc": "0", "stdout": "", "stderr": "",
                                                                     "failed": False}, 0),
                           ({**self.OK, "failed": "no"}, 0), (self.OK, 2)):
            e, runs, _ = self.vm(self.ops(), "restarts", "gitea.service", result=result, rc=rc, mutate=False)
            self.assertIsInstance(e, subprocess.CalledProcessError, (result, rc))
            self.assertEqual(e.cmd, ["vm-step", "restarts", "gitea.service"])
            self.assertNotEqual(e.returncode, 0)
            self.assertEqual(len(runs), 1)

    def test_dry_run_skips_mutating_steps_only(self):
        o = self.ops(dry_run=True)
        out, runs, err = self.vm(o, "stop", "gitea.service", result=self.OK)
        self.assertEqual((out, runs), ("", []))
        self.assertIn("DRY-RUN: vm-steps stop gitea.service", err)
        out, runs, _ = self.vm(o, "restarts", "gitea.service", result=self.OK, mutate=False)
        self.assertEqual((out, len(runs)), ("sha256:abc", 1))

    REAL_ARGS = ["us-east1-docker.pkg.dev/gitea-496920/gitea/gitea@sha256:" + "0123456789abcdef" * 4,
                 "us-east1-docker.pkg.dev/gitea-496920/gitea/gitea:prod",
                 "gs://gitea-496920-gitea-backups/dumps/gitea-20261025T060000Z.zip",
                 "gs://gitea-496920-gitea-backups/config/20261025T060000Z/vm-config.tar.gz",
                 "2026-10-25 06:00:00 UTC", ".pre-restore-20261024T230540Z", "gitea.service", "caddy",
                 "sha256:" + "f" * 64, "3h", "20261025T060000Z", "none", ""]

    def test_the_argument_allowlist(self):
        for x in self.REAL_ARGS:
            self.assertTrue(O.VM_ARG.fullmatch(x), x)
        for x in ("a;b", "$(id)", "${HOME}", "a\nb", "a\n", "a'b", 'a"b', "`id`", "a|b", "a&b", "{{ x }}",
                  "a\\b", "*", "~root", "a\tb", "<x", "a>b", "!x", "#x", "x?"):
            self.assertIsNone(O.VM_ARG.fullmatch(x), repr(x))

    def test_refused_steps_and_arguments_never_run(self):
        for args in (("rm", "-rf"), ("../ops",), ("marker", "x;y"), ("pull", "img\n"), ("stop", 1)):
            e, runs, _ = self.vm(self.ops(), *args, result=self.OK)
            self.assertIsInstance(e, subprocess.CalledProcessError, args)
            self.assertEqual(runs, [], args)

    def test_steps_playbook_and_files_agree(self):
        with open(O.OPS_YML) as f:
            ops_yml = f.read()
        block = ops_yml[ops_yml.index("    allowed_steps:\n"):]
        block = block[:block.index("\n    # ")]
        allowed = re.findall(r"^      - ([a-z-]+)$", block, re.M)
        files = sorted(n[:-4] for n in os.listdir(os.path.join(O.ANSIBLE_DIR, "steps")))
        self.assertEqual(sorted(O.VM_STEPS), allowed)
        self.assertEqual(sorted(O.VM_STEPS), files)
        self.assertIn(f"arg_pattern: '{O.VM_ARG.pattern}'", ops_yml)


class EngineConnection(unittest.TestCase):
    """The pinned ansible-core, the OS Login key and the inventory."""

    def setUp(self):
        self.home = tempfile.mkdtemp()
        self.addCleanup(shutil.rmtree, self.home)
        p = mock.patch.dict(os.environ, {"HOME": self.home})
        p.start()
        self.addCleanup(p.stop)

    def test_installs_the_hash_locked_requirements_once(self):
        runs = []

        def fake_run(cmd, **kw):
            runs.append(cmd)
            if "pip" in cmd:
                os.makedirs(os.path.join(cmd[cmd.index("--target") + 1], "bin"))
                open(os.path.join(cmd[cmd.index("--target") + 1], "bin", "ansible-playbook"), "w").close()
            return subprocess.CompletedProcess(cmd, 0, stdout="ansible-playbook [core 2.21.5]\n", stderr="")
        with mock.patch.object(O.subprocess, "run", side_effect=fake_run), redirect_stderr(io.StringIO()):
            target = O.ensure_ansible()
            self.assertEqual(O.ensure_ansible(), target)
        with open(os.path.join(O.ANSIBLE_DIR, "requirements.txt"), "rb") as f:
            sha = hashlib.sha256(f.read()).hexdigest()[:16]
        self.assertEqual(target, os.path.join(self.home, ".cache", "gitea-ops", "py-" + sha))
        pips = [c for c in runs if "pip" in c]
        self.assertEqual(len(pips), 1)
        for flag in ("--require-hashes", "--only-binary=:all:", "--no-deps"):
            self.assertIn(flag, pips[0])
        self.assertEqual(pips[0][pips[0].index("-r") + 1], os.path.join(O.ANSIBLE_DIR, "requirements.txt"))
        self.assertTrue(os.path.isfile(os.path.join(target, "bin", "ansible-playbook")))
        self.assertEqual([c[1:] for c in runs if "pip" not in c], [[os.path.join(target, "bin", "ansible-playbook"),
                                                                      "--version"]] * 2)

    def test_a_failed_install_aborts_and_leaves_nothing(self):
        def fake_run(cmd, **kw):
            os.makedirs(cmd[cmd.index("--target") + 1], exist_ok=True)
            return subprocess.CompletedProcess(cmd, 1, stdout="", stderr="no route to pypi.org")
        with mock.patch.object(O.subprocess, "run", side_effect=fake_run), redirect_stderr(io.StringIO()), \
                self.assertRaises(O.Abort) as ctx:
            O.ensure_ansible()
        self.assertIn("no route to pypi.org", str(ctx.exception))
        self.assertEqual(os.listdir(os.path.join(self.home, ".cache", "gitea-ops")), [])

    def test_main_installs_before_anything_else_and_exits_3(self):
        ran = []
        with mock.patch.object(O, "ensure_ansible", side_effect=O.Abort("pip failed")), \
                mock.patch.object(O, "cmd_release", side_effect=lambda *a: ran.append(a)), \
                mock.patch.object(sys, "argv", ["gitea_ops.py", "--yes", "release", "--gitea", "abc"]), \
                redirect_stderr(io.StringIO()), self.assertRaises(SystemExit) as ctx:
            O.main()
        self.assertEqual((ctx.exception.code, ran), (3, []))

    def connect(self, now, profile, verbs_ok=("add", "update")):
        o = O.Ops(argparse.Namespace(region="r", project="p", repo="gitea", cmd="release", dry_run=False,
                                     vm="gitea-vm", zone="z"))
        gcloud = []

        def fake_run(cmd, **kw):
            gcloud.append(cmd[:5])
            ok = cmd[4] in verbs_ok if cmd[:4] == ["gcloud", "compute", "os-login", "ssh-keys"] else True
            return subprocess.CompletedProcess(cmd, 0 if ok else 1, stdout="", stderr="" if ok else "not found")
        with mock.patch.object(O.subprocess, "run", side_effect=fake_run), \
                mock.patch.object(o, "run", return_value=json.dumps(profile)) as run, \
                mock.patch.object(o, "ensure_ssh_key"), mock.patch.object(O.atexit, "register"), \
                mock.patch.object(O.time, "time", side_effect=lambda: now[0]), redirect_stderr(io.StringIO()):
            o.connect()
            first = list(gcloud)
            now[0] += 10 * 60
            o.connect()
            self.assertEqual(gcloud, first, "no new registration 10 minutes later")
            now[0] += 36 * 60
            o.connect()
        self.addCleanup(shutil.rmtree, o.work, True)
        self.assertEqual(run.call_count, 1, "the OS Login user is asked for once")
        return o, first, gcloud[len(first):]

    def test_inventory_and_key_registration(self):
        profile = {"posixAccounts": [{"username": "sa_other", "accountId": "x"},
                                     {"username": "sa_123", "primary": True, "accountId": "p"}]}
        o, first, later = self.connect([1_000_000.0], profile, verbs_ok=("add", "update"))
        verb = lambda calls: [c[4] for c in calls]
        self.assertEqual(verb(first), ["update"])
        self.assertEqual(verb(later), ["update"], "the key is extended 46 minutes later")
        self.assertTrue(o.work.startswith(os.path.join(self.home, ".cache", "gitea-ops")))
        with open(o.inventory) as f:
            host = json.load(f)["all"]["hosts"]["gitea"]
        self.assertEqual(host["ansible_host"], "gitea-vm")
        self.assertEqual(host["ansible_user"], "sa_123")
        self.assertEqual(host["ansible_python_interpreter"], "/usr/bin/python3")
        self.assertIn("ControlPersist=60m", host["ansible_ssh_args"])
        args = dict(a.split("=", 1) for a in __import__("shlex").split(host["ansible_ssh_common_args"]) if a != "-o")
        # gcloud must not see ansible-core's PYTHONPATH (write_inventory).
        self.assertEqual(args["ProxyCommand"], "env -u PYTHONPATH gcloud compute start-iap-tunnel %h 22 "
                                               "--listen-on-stdin --zone=z --project=p --verbosity=warning")
        self.assertEqual(args["ControlPath"], os.path.join(o.cp_dir, "%h"))
        self.assertEqual(args["UserKnownHostsFile"], os.path.join(o.work, "known_hosts"))
        self.assertEqual(args["StrictHostKeyChecking"], "accept-new")
        # A socket path, plus the suffix ssh adds while binding it, must fit,
        # whatever HOME is (this one is long).
        self.assertLessEqual(len(os.path.join(o.cp_dir, "gitea-vm")), 80)
        self.addCleanup(shutil.rmtree, o.cp_dir, True)
        o.ssh_user = None
        o.disconnect()
        self.assertFalse(os.path.exists(o.cp_dir) or o.work)

    def test_a_new_key_is_added(self):
        o, first, _ = self.connect([1_000_000.0], {"posixAccounts": [{"username": "sa_9"}]}, verbs_ok=("add",))
        self.assertEqual([c[4] for c in first], ["update", "add"])
        with open(o.inventory) as f:
            self.assertEqual(json.load(f)["all"]["hosts"]["gitea"]["ansible_user"], "sa_9")

    def test_unreachable_is_a_called_process_error(self):
        o = O.Ops(argparse.Namespace(region="r", project="p", repo="gitea", cmd="release", dry_run=False,
                                     vm="gitea-vm", zone="z"))
        o.pylib = "/py"
        with mock.patch.object(o, "connect", side_effect=OSError("no gcloud")), redirect_stderr(io.StringIO()), \
                self.assertRaises(subprocess.CalledProcessError) as ctx:
            o.vm("restarts", "gitea.service", mutate=False)
        self.assertEqual(ctx.exception.returncode, 255)


class EngineManifests(unittest.TestCase):
    """A release's manifests keep what any engine's `rollback --manifest`
    reads, so the bash engine (c6ac72c) can still undo this one's runs."""

    def test_path_b_progress_and_manifest_hold_the_rollback_inputs(self):
        a = argparse.Namespace(region="r", project="p", repo="gitea", cmd="release", dry_run=False, yes=True,
                               vm="gitea-vm", zone="z", data_disk="gitea-data", backup_bucket="b",
                               state_bucket=None, gitea="abc1234", caddy=None, allow_major=False,
                               allow_alpine_minor=False, soak=0, rollback_soak=0, drill="fail-after-migrate")
        ops = O.Ops(a)
        labels = {O.VERSION_LABEL: "28.2.0", O.ALPINE_LABEL: "3.22"}
        saved, steps = {}, []

        def record_state():
            ops.manifest["prod"] = {"gitea": {"digest": "sha256:oldg", "labels": labels},
                                    "caddy": {"digest": "sha256:oldc", "labels": {}}}

        def snapshot(purpose, keep=False, disk=None):
            name = f"gitea-data-{purpose}"
            ops.manifest.setdefault("snapshots", []).append(name)
            return name

        def save(quiet=False, name="manifest.json"):
            saved[name] = json.loads(json.dumps(ops.manifest))

        def vm(step, *args, mutate=True):
            steps.append((step, *args))
            return "absent" if step == "marker-check" else ""
        with mock.patch.object(ops, "record_state", side_effect=record_state), \
                mock.patch.object(ops, "digest", return_value="sha256:newg"), \
                mock.patch.object(ops, "labels", return_value=labels), mock.patch.object(ops, "backup"), \
                mock.patch.object(ops, "tag"), mock.patch.object(ops, "snapshot", side_effect=snapshot), \
                mock.patch.object(ops, "verify", return_value=[]), mock.patch.object(ops, "restore_data"), \
                mock.patch.object(ops, "save_manifest", side_effect=save), \
                mock.patch.object(ops, "vm", side_effect=vm), \
                mock.patch.dict(os.environ, {"GITEA_DRILL_OK": "1"}), redirect_stderr(io.StringIO()) as err, \
                redirect_stdout(io.StringIO()), self.assertRaises(SystemExit) as ctx:
            O.cmd_release(ops, a)
        self.assertEqual(ctx.exception.code, 1)
        self.assertIn("DRILL fail-after-migrate: PASS (marker absent)", err.getvalue())
        self.assertIn("GITEA_RELEASE_ROLLBACK path=B reason=DrillFailure(", err.getvalue())
        snapped = [n for n in saved if "-snapshotted" in n]
        self.assertEqual(len(snapped), 1)
        for name in (snapped[0], "manifest.json"):
            m = saved[name]
            self.assertEqual(m["previous"], {"gitea": "sha256:oldg", "caddy": "sha256:oldc"}, name)
            self.assertEqual(m["versions"]["previous"], "28.2.0", name)
            self.assertEqual(m["restore_point"], "gitea-data-pre-release", name)
            self.assertEqual(m["snapshots"], ["gitea-data-pre-release"], name)
        self.assertEqual(saved["manifest.json"]["outcome"], "rolled-back")
        ar = "r-docker.pkg.dev/p/gitea"
        self.assertEqual(steps, [
            ("pull", f"{ar}/gitea@sha256:newg"), ("pause-timers",), ("stop", "gitea.service"),
            ("migrate", f"{ar}/gitea@sha256:newg"), ("pull", f"{ar}/gitea:prod"), ("start", "gitea.service"),
            ("marker", ops.stamp),
            # the rollback
            ("stop", "gitea.service"), ("pull", f"{ar}/gitea:prod", f"{ar}/caddy:prod"), ("start", "gitea.service"),
            ("restart-if-not", "sha256:oldc", "caddy.service", "caddy"), ("marker-check", ops.stamp),
            ("resume-timers",)])


class EngineProbe(unittest.TestCase):
    def probe(self, running):
        ops = O.Ops(argparse.Namespace(region="r", project="p", repo="gitea", cmd="probe", dry_run=False))
        with open(O.LIB, "rb") as f:
            lib = hashlib.sha256(f.read()).hexdigest()
        out = {"selftest": lib, "restarts": "0", "journal-errors": "0", "pre-restore-list": "",
               "marker-check": "absent", "inventory": "integrity\tok\nuser\tjmr\t0\t1\nrepo\tjmr/x\t1\t2\tabc\n"}
        calls = []

        def vm(step, *args, mutate=True):
            calls.append((step, mutate))
            return running[args[0]] if step == "running-digest" else out[step]
        with mock.patch.object(ops, "vm", side_effect=vm), \
                mock.patch.object(ops, "digest", side_effect=lambda img, tag: f"sha256:{img}"), \
                redirect_stderr(io.StringIO()), redirect_stdout(io.StringIO()) as stdout:
            try:
                O.cmd_probe(ops, None)
                code = 0
            except SystemExit as e:
                code = e.code
        self.assertTrue(all(not m for _, m in calls), "every probe step is read-only")
        return code, json.loads(stdout.getvalue()), {s for s, _ in calls}

    def test_passes_when_the_vm_runs_prod(self):
        code, summary, steps = self.probe({"gitea": "sha256:gitea", "caddy": "sha256:caddy"})
        self.assertEqual((code, summary["failures"], summary["users"], summary["repos"]), (0, [], 1, 1))
        self.assertEqual(steps, {"selftest", "restarts", "running-digest", "journal-errors", "pre-restore-list",
                                 "marker-check", "inventory"})

    def test_fails_on_a_digest_that_is_not_prod(self):
        code, summary, _ = self.probe({"gitea": "sha256:gitea", "caddy": "sha256:old"})
        self.assertEqual(code, 1)
        self.assertEqual(len(summary["failures"]), 1)


class LaunchJob(unittest.TestCase):
    def test_the_job_ships_exactly_the_engine(self):
        with mock.patch.object(L, "gcloud", return_value="me@example.com"), \
                mock.patch.object(L, "git", side_effect=lambda *a: "" if a[0] == "status" else "abc"):
            cm, job, dirty = L.manifests(["--yes", "backup"], "p")
        self.assertFalse(dirty)
        volume, = [v for v in job["spec"]["template"]["spec"]["volumes"] if v["name"] == "ops"]
        items = {i["key"]: i["path"] for i in volume["configMap"]["items"]}
        self.assertEqual(set(items), set(cm["data"]))
        self.assertEqual({f"scripts/ops/{p}" for p in items.values()}, set(G.ENGINE))
        for key, path in items.items():
            self.assertRegex(key, r"^[-._a-zA-Z0-9]+$")
            self.assertNotIn("__", path, "the key encoding must be reversible")
            with open(os.path.join(L.HERE, path)) as f:
                self.assertEqual(cm["data"][key], f.read(), path)
        self.assertEqual(items["ansible__steps__start.yml"], "ansible/steps/start.yml")
        cmd = job["spec"]["template"]["spec"]["containers"][0]["command"]
        self.assertEqual(cmd[:3], ["python3", "-u", "/ops/gitea_ops.py"])

    def test_a_change_anywhere_in_scripts_ops_is_dirty(self):
        seen = []

        def git(*a):
            seen.append(a)
            return " M ansible/steps/start.yml" if a[0] == "status" else "abc"
        with mock.patch.object(L, "gcloud", return_value="me"), mock.patch.object(L, "git", side_effect=git):
            cm, job, dirty = L.manifests(["probe"], "p")
        self.assertTrue(dirty)
        self.assertIn(("status", "--porcelain", "--", "."), seen)
        self.assertEqual(cm["metadata"]["annotations"]["gitea-ops/commit"], "abc-dirty")


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

        def edited(name):
            def fake_open(path, mode="r", *a, **k):
                if path.endswith(name) and "b" in mode:
                    with real(path, mode, *a, **k) as f:
                        return io.BytesIO(f.read() + b"\n# edited")
                return real(path, mode, *a, **k)
            return fake_open
        for pipeline in ("image", "infra"):
            before = G.code_hash(pipeline)
            for name in ("gitea-ops-lib.sh", "steps/start.yml", "ansible.cfg", "requirements.txt"):
                with mock.patch("builtins.open", edited(name)):
                    self.assertNotEqual(G.code_hash(pipeline), before, (pipeline, name))

    def test_every_engine_file_is_release_code_of_both_pipelines(self):
        on_disk = {f"scripts/ops/{n}" for n in os.listdir(os.path.join(G.ROOT, "scripts/ops")) if n.endswith(".py")}
        for root, dirs, names in os.walk(os.path.join(G.ROOT, "scripts/ops/ansible")):
            dirs[:] = [d for d in dirs if d != "__pycache__"]
            on_disk |= {os.path.relpath(os.path.join(root, n), G.ROOT) for n in names}
        self.assertEqual(set(G.ENGINE), on_disk)
        self.assertEqual(len(G.ENGINE), len(set(G.ENGINE)))
        for files in G.FILES.values():
            self.assertTrue(set(G.ENGINE) <= set(files))
        self.assertFalse(os.path.exists(os.path.join(G.ROOT, "scripts/ops/vm-steps.sh")))

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

    # Gitea's compare lists each commit's parents as {created, sha, url}, and a
    # merge's files as its diff against its own first parent.
    API = "https://gitea.jasonmross.dev/api/v1/repos/JMR-dev/Gitea/git/commits/"
    INFRA = ("infra/main.go", "infra/pkg/iam/iam.go")
    CADDY = "image/caddy/Dockerfile"

    def commit(self, sha, parents, *files):
        return {"sha": sha, "files": [{"filename": f, "status": "modified"} for f in files],
                "parents": [{"created": "2026-11-07T00:00:00Z", "sha": p, "url": self.API + p} for p in parents]}

    def test_merge_from_main_does_not_supersede(self):
        # Branch B merged main (A, the commit asked about) before main merged
        # B: B2's diff against B1 is A's own image change, nothing newer.
        c = {"commits": [self.commit("MB", ["A", "B2"], *self.INFRA),
                         self.commit("B2", ["B1", "A"], self.CADDY),
                         self.commit("B1", ["B0"], *self.INFRA)]}
        self.assertIsNone(G.newer_touches(c, G.IMAGE_PATHS))

    def test_newer_image_merge_on_main_supersedes(self):
        # Listed before MC, C1 and B2 would answer first if every commit counted.
        c = {"commits": [self.commit("C1", ["MB"], self.CADDY),
                         self.commit("MB", ["A", "B2"], *self.INFRA),
                         self.commit("B2", ["B1", "A"], self.CADDY),
                         self.commit("MC", ["MB", "C1"], self.CADDY),
                         self.commit("B1", ["B0"], *self.INFRA)]}
        self.assertEqual(G.newer_touches(c, G.IMAGE_PATHS), "MC")
        self.assertEqual([x["sha"] for x in G.main_line(c["commits"])], ["MC", "MB"])

    def test_the_caddy_pin_was_not_superseded(self):
        # compare 2ce57a7...f93e5b7 as Gitea answered it: #48's merge, and its
        # branch's merge of main, which brought in #47's own Caddy change.
        pin = "2ce57a7281102c50d1c6a1ca6c46cbbdb8c5d59b"
        merge = "f93e5b769c1f385a4072c51b9476ee18ab649154"
        catch_up = "72f431042694fb10ca862c64a6f0a0c0c8f05b85"
        branch = "84ee2e523ef03e83db0ba5687721f9bcf02b28fa"
        dns = ("infra/pkg/dns/dns_test.go", "infra/pkg/iam/iam_test.go", "Makefile", "docs/runbook.md",
               "infra/main.go", "infra/pkg/dns/dns.go", "infra/pkg/iam/iam.go")
        c = {"total_commits": 3, "commits": [
            self.commit(merge, [pin, catch_up], *dns),
            self.commit(catch_up, [branch, pin], "docs/runbook.md", self.CADDY, "vm/ansible/README.md",
                        "vm/ansible/tests/check", "vm/config/Caddyfile.tmpl", "vm/quadlets/caddy.container"),
            self.commit(branch, ["b5c3a9da87bf6b5d28c44384efe2ed942b02bccd"], *dns)]}
        self.assertIsNone(G.newer_touches(c, G.IMAGE_PATHS))
        self.assertEqual([x["sha"] for x in G.main_line(c["commits"])], [merge])

    def test_without_one_head_or_parents_all_commits_count(self):
        # When main's line can't be told, every listed commit counts, as before.
        image = self.commit("I", ["A"], self.CADDY)
        for name, commits in (
                ("two heads", [self.commit("H", ["A"], *self.INFRA), image]),
                ("no parents", [self.commit("H", ["I"], *self.INFRA), {"sha": "I", "files": image["files"]}]),
                ("empty parents", [self.commit("H", ["I"], *self.INFRA), dict(image, parents=[])]),
                ("a parent without a sha", [self.commit("H", ["I"], *self.INFRA),
                                            dict(image, parents=[{"url": self.API}])])):
            with self.subTest(name):
                self.assertIsNone(G.main_line(commits))
                self.assertEqual(G.newer_touches({"commits": commits}, G.IMAGE_PATHS), "I")

    def test_a_parent_cycle_ends_the_walk(self):
        commits = [self.commit("H", ["X"], *self.INFRA), self.commit("X", ["Y"], self.CADDY),
                   self.commit("Y", ["X"], *self.INFRA)]
        self.assertEqual([x["sha"] for x in G.main_line(commits)], ["H", "X", "Y"])
        self.assertEqual(G.newer_touches({"commits": commits}, G.IMAGE_PATHS), "X")


class RelayAgrees(unittest.TestCase):
    """cicd/relay flags the same files this gate hashes, for the same pipelines."""

    def test_release_paths_match(self):
        import re
        with open(os.path.join(G.ROOT, "cicd/relay/webhook.go")) as f:
            src = f.read()
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


class ChecksWorkflowAgrees(unittest.TestCase):
    """The PR checks (.gitea/workflows/checks.yml) run the stages of `make check`
    that a PR should, and `make check` still runs them, so a stage can't drop
    out of one without the other noticing (docs/cicd.md, "PR checks")."""

    # What the workflow runs, and what only `make check` runs: ansible-check
    # is cloudbuild/preview's, check-metadata-guard needs namespaces and nft,
    # workflow-lint needs podman.
    IN_PR = {"infra-check", "ci-check", "scripts-check"}
    LOCAL_ONLY = {"ansible-check", "check-metadata-guard", "workflow-lint"}

    @classmethod
    def setUpClass(cls):
        with open(os.path.join(G.ROOT, ".gitea/workflows/checks.yml")) as f:
            cls.workflow = f.read()
        with open(os.path.join(G.ROOT, "Makefile")) as f:
            makefile = f.read()
        # Each rule, "target: prerequisites ## help", as target -> prerequisites.
        # Not .PHONY (a leading dot) and not a ":=" variable.
        cls.rules = {name: rest.split("##")[0].split()
                     for name, rest in re.findall(r"^([A-Za-z_-]+):(?!=)(.*)$", makefile, re.M)}
        # The workflow's lines, without comments or the "- " of a list item.
        cls.lines = [re.sub(r"^-\s+", "", s) for s in (line.strip() for line in cls.workflow.splitlines())
                     if s and not s.startswith("#")]

    def make_targets(self):
        """The targets of every `make` the workflow runs, flags and VAR=x left out."""
        targets = []
        for line in self.lines:
            m = re.match(r"(?:run:\s*)?make\s+(.*)$", line)
            if m:
                targets += [w for w in m.group(1).split() if not w.startswith("-") and "=" not in w]
        return targets

    def test_the_workflow_runs_exactly_the_pr_stages(self):
        self.assertEqual(sorted(self.make_targets()), sorted(self.IN_PR))

    def test_make_check_runs_them_and_the_local_stages(self):
        check = self.rules["check"]
        for target in self.IN_PR:
            self.assertIn(target, check)
        self.assertEqual(set(check) - self.IN_PR, self.LOCAL_ONLY)

    def test_every_stage_is_a_make_target(self):
        for target in self.IN_PR | self.LOCAL_ONLY | set(self.make_targets()):
            self.assertIn(target, self.rules)

    def test_every_action_is_pinned_to_a_commit(self):
        uses = [line[len("uses:"):].strip() for line in self.lines if line.startswith("uses:")]
        self.assertTrue(uses)
        for u in uses:
            self.assertRegex(u, r"^[\w.-]+/[\w./-]+@[0-9a-f]{40}\s+#\s*v\d[\w.]*$")

    def test_the_scripts_job_installs_its_tools(self):
        # ripgrep for the bats fakes; without shellcheck, scripts-check fails.
        installs = [line for line in self.lines if "apt" in line and re.search(r"\binstall\s", line)]
        self.assertEqual(len(installs), 1, installs)
        self.assertLessEqual({"shellcheck", "bats", "ripgrep"}, set(installs[0].split()))

    def test_no_path_filters(self):
        # A PR a filter skips never gets the status, and a required status that
        # never comes blocks the merge.
        self.assertNotRegex(self.workflow, r"(?m)^\s*paths(-ignore)?\s*:")


if __name__ == "__main__":
    unittest.main()
