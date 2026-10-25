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
        self.assertEqual(args["ProxyCommand"], "gcloud compute start-iap-tunnel %h 22 --listen-on-stdin "
                                               "--zone=z --project=p --verbosity=warning")
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


if __name__ == "__main__":
    unittest.main()
