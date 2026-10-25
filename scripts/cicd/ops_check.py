#!/usr/bin/env python3
"""Checks of the release engine's VM steps (scripts/ops/ansible) that need the
pinned ansible-core and podman, so not part of `make ci-check`:

  - every step file passes a syntax check, and follows the two rules in
    ops.yml (expand_argument_vars: false on every command; no Go templates);
  - ops.yml really runs a step and hands its result to gitea_ops.py, through
    Ops.vm itself, against this machine (connection local, no sudo): the lib
    arrives byte for byte, podman's JSON parses, and a refused step, a refused
    argument and a failing step each come back as CalledProcessError;
  - gitea-ops-lib.sh's pre-restore-list and pre-restore-remove, and selftest,
    in an AlmaLinux 10 container (the VM's OS);
  - the whole path a release takes, against an AlmaLinux 10 container booted
    with systemd, standing in for the VM: Ops.vm's own inventory over SSH
    (sshd on stdin, as IAP's tunnel is), an unprivileged login and sudo,
    pipelining, the result handed back to the controller, one master
    connection. Units, timers and the dead man's switch are real (and the bash
    engine's switch and this one's disarm each other); podman and gcloud are
    stand-ins that answer like the VM's;
  - the lib is the bash engine's vm-steps.sh (as of c6ac72c) with steps
    deleted and selftest added, nothing changed.

Nothing here reaches a cloud project. Run: make ops-check
"""
import argparse
import hashlib
import io
import json
import os
import re
import shlex
import shutil
import subprocess
import sys
import tempfile
import unittest
from contextlib import redirect_stderr

ROOT = os.path.normpath(os.path.join(os.path.dirname(os.path.abspath(__file__)), "..", ".."))
sys.path.insert(0, os.path.join(ROOT, "scripts", "ops"))
import gitea_ops as O  # noqa: E402

STEPS = os.path.join(O.ANSIBLE_DIR, "steps")
ALMA = os.environ.get("ALMA_IMAGE", "quay.io/almalinuxorg/almalinux:10")
ALMA_INIT = os.environ.get("ALMA_INIT_IMAGE", "docker.io/almalinux/10-init:latest")
# The attested bash engine: the lib must be its vm-steps.sh, minus steps.
BASH_ENGINE = "c6ac72c"
PYLIB = None


def setUpModule():
    global PYLIB
    with redirect_stderr(io.StringIO()):
        PYLIB = O.ensure_ansible()


def read(path):
    with open(path) as f:
        return f.read()


def lib_text():
    return read(O.LIB)


def playbook(*args, cwd):
    return subprocess.run([sys.executable, os.path.join(PYLIB, "bin", "ansible-playbook"), *args],
                          capture_output=True, text=True, cwd=cwd, env=O.ansible_env(PYLIB, cwd))


class Static(unittest.TestCase):
    def files(self):
        out = [O.OPS_YML]
        for n in sorted(os.listdir(STEPS)):
            out.append(os.path.join(STEPS, n))
        return out

    def test_every_command_keeps_its_arguments_as_given(self):
        for path in self.files():
            text = read(path)
            commands = len(re.findall(r"^\s*-? ?ansible\.builtin\.command:", text, re.M))
            kept = len(re.findall(r"^\s+expand_argument_vars: false$", text, re.M))
            self.assertEqual(commands, kept, f"{path}: every command needs expand_argument_vars: false")
            self.assertNotRegex(text, r"^\s*-? ?(ansible\.builtin\.)?(shell|raw|script):", path)

    def test_no_go_templates_and_only_builtin_modules(self):
        for path in self.files() + [O.LIB]:
            self.assertNotRegex(read(path), r"\{\{-?\s*\.", path)
        for path in self.files():
            for mod in re.findall(r"^\s*-? ?([a-z_]+\.[a-z_]+\.[a-z_]+):", read(path), re.M):
                self.assertTrue(mod.startswith("ansible.builtin."), f"{path}: {mod}")

    def test_every_step_leaves_a_result(self):
        for n in os.listdir(STEPS):
            self.assertIn("step_result", read(os.path.join(STEPS, n)), n)

    def test_syntax(self):
        work = tempfile.mkdtemp()
        self.addCleanup(shutil.rmtree, work)
        # include_tasks is dynamic, so a syntax check of ops.yml never opens a
        # step file; this play imports every one of them statically.
        imports = os.path.join(work, "all-steps.yml")
        with open(imports, "w") as f:
            f.write("- hosts: all\n  gather_facts: false\n  tasks:\n")
            for n in sorted(os.listdir(STEPS)):
                f.write(f"    - ansible.builtin.import_tasks: {os.path.join(STEPS, n)}\n")
        inv = os.path.join(work, "inventory.json")
        with open(inv, "w") as f:
            json.dump({"all": {"hosts": {"gitea": {}}}}, f)
        for pb in (O.OPS_YML, imports):
            r = playbook("--syntax-check", "-i", inv, pb, "-e", "step=selftest", "-e", "result_file=/dev/null",
                         cwd=work)
            self.assertEqual(r.returncode, 0, r.stdout + r.stderr)


class LocalOps(O.Ops):
    """Ops whose VM is this machine: connection local, no become."""

    def connect(self):
        if not self.work:
            self.work = tempfile.mkdtemp(prefix="ops-check-")
            os.mkdir(os.path.join(self.work, "calls"))
            self.inventory = os.path.join(self.work, "inventory.json")
            with open(self.inventory, "w") as f:
                json.dump({"all": {"hosts": {"gitea": {
                    "ansible_connection": "local", "ansible_become": False,
                    "ansible_python_interpreter": sys.executable}}}}, f)


class Harness(unittest.TestCase):
    """ops.yml and Ops.vm end to end, against this machine."""

    def setUp(self):
        self.ops = LocalOps(argparse.Namespace(region="r", project="p", repo="gitea", cmd="probe",
                                               dry_run=False, vm="local"))
        self.ops.pylib = PYLIB
        self.addCleanup(lambda: self.ops.work and shutil.rmtree(self.ops.work, ignore_errors=True))

    def vm(self, *args):
        with redirect_stderr(io.StringIO()) as err:
            try:
                return self.ops.vm(*args, mutate=False), err.getvalue()
            except subprocess.CalledProcessError as e:
                return e, err.getvalue()

    def test_the_lib_arrives_byte_for_byte(self):
        out, _ = self.vm("selftest")
        self.assertEqual(out, hashlib.sha256(lib_text().encode()).hexdigest())

    def test_native_steps_parse_what_they_read(self):
        self.assertEqual(self.vm("marker-check", "none")[0], "absent")
        self.assertRegex(self.vm("restarts", "dbus.service")[0], r"^\d+$")
        self.assertRegex(self.vm("journal-errors", "dbus.service", "2026-10-25 12:00:00 UTC")[0], r"^\d+$")

    def test_running_digest_and_restart_if_not_read_podman_json(self):
        if not shutil.which("podman"):
            self.skipTest("no podman")
        name = f"gitea-ops-check-{os.getpid()}"
        subprocess.run(["podman", "run", "-d", "--rm", "--name", name, ALMA, "sleep", "300"],
                       check=True, capture_output=True)
        self.addCleanup(subprocess.run, ["podman", "rm", "-f", "-t", "0", name], capture_output=True)
        want = subprocess.run(["podman", "inspect", name, "--format", "{{.ImageDigest}}"],
                              check=True, capture_output=True, text=True).stdout.strip()
        self.assertEqual(self.vm("running-digest", name)[0], want)
        out, err = self.vm("restart-if-not", want, "no-such-unit.service", name)
        self.assertEqual(out, "")
        self.assertIn(f"{name} already runs {want}", err)

    def test_failures_come_back_as_called_process_error(self):
        e, err = self.vm("pre-restore-remove", "../etc")
        self.assertIsInstance(e, subprocess.CalledProcessError)
        self.assertEqual(e.returncode, 1)
        self.assertIn("not a pre-restore tree: ../etc", err)
        e, _ = self.vm("running-digest", "no-such-container-anywhere")
        self.assertIsInstance(e, subprocess.CalledProcessError)

    def run_raw(self, call):
        """ops.yml with a call gitea_ops.py would refuse to make."""
        self.ops.connect()
        path = os.path.join(self.ops.work, "raw.json")
        call["result_file"] = os.path.join(self.ops.work, "raw.result.json")
        with open(path, "w") as f:
            json.dump(call, f)
        r = playbook("-i", self.ops.inventory, O.OPS_YML, "-e", "@" + path, cwd=self.ops.work)
        return r.returncode, O.read_result(call["result_file"])

    def test_the_playbook_refuses_on_its_own(self):
        for call in ({"step": "no-such-step", "step_args": []},
                     {"step": "../ops", "step_args": []},
                     {"step": "marker-check", "step_args": ["x; rm -rf /"]},
                     {"step": "marker-check", "step_args": ["x\n"]},
                     {"step": "marker-check", "step_args": "none"},
                     {"step": "marker-check", "step_args": [1]}):
            rc, res = self.run_raw(call)
            self.assertNotEqual(rc, 0, call)
            self.assertTrue(res and res["failed"] and res["rc"] != 0, (call, res))
            self.assertIn("refused", res["msg"], call)
        rc, res = self.run_raw({"step": "marker-check", "step_args": ["none"]})
        self.assertEqual((rc, res["failed"], res["stdout"]), (0, False, "absent"))


class LibOnAlma(unittest.TestCase):
    """gitea-ops-lib.sh as the VM's root runs it (bash -c, AlmaLinux 10)."""

    def setUp(self):
        if not shutil.which("podman"):
            self.skipTest("no podman")

    def lib(self, step, *args, setup=""):
        script = (setup + "\n" if setup else "") + 'bash -c "$LIB" gitea-ops-lib "$@"; rc=$?; ' \
            'find /var/lib/gitea -mindepth 1 -maxdepth 2 -printf "tree %P\\n" | sort; exit $rc'
        r = subprocess.run(["podman", "run", "--rm", "--network", "none", "-e", "LIB=" + lib_text(), ALMA,
                            "bash", "-c", script, "check", step, *args], capture_output=True, text=True)
        return r.returncode, r.stdout, r.stderr

    TREES = ("mkdir -p /var/lib/gitea/data /var/lib/gitea/.pre-restore-20261001T000000Z/data "
             "/var/lib/gitea/.pre-restore-20261002T000000Z /var/lib/gitea/elsewhere/keep && "
             "touch /var/lib/gitea/.pre-restore-20261003T000000Z && "
             "ln -s /var/lib/gitea/elsewhere /var/lib/gitea/.pre-restore-20261004T000000Z")

    def test_selftest_is_the_text_it_was_given(self):
        # Through the environment, not argv: the hash covers the exact text,
        # trailing newline included.
        rc, out, _ = self.lib("selftest")
        self.assertEqual(rc, 0)
        self.assertEqual(out.split()[0], hashlib.sha256(lib_text().encode()).hexdigest())

    def test_pre_restore_list_names_only_real_trees(self):
        rc, out, err = self.lib("pre-restore-list", setup=self.TREES)
        self.assertEqual(rc, 0, err)
        listed = [l for l in out.splitlines() if not l.startswith("tree ")]
        self.assertEqual(listed, [".pre-restore-20261001T000000Z", ".pre-restore-20261002T000000Z"])

    def test_pre_restore_remove_deletes_exactly_those(self):
        rc, out, err = self.lib("pre-restore-remove", ".pre-restore-20261001T000000Z",
                                ".pre-restore-20261004T000000Z", ".pre-restore-20261009T000000Z",
                                setup=self.TREES)
        self.assertEqual(rc, 0, err)
        trees = {l[5:] for l in out.splitlines() if l.startswith("tree ")}
        self.assertNotIn(".pre-restore-20261001T000000Z", trees)
        # A symlink is not a tree: neither it nor what it points at goes.
        self.assertTrue({".pre-restore-20261002T000000Z", ".pre-restore-20261003T000000Z",
                         ".pre-restore-20261004T000000Z", "elsewhere/keep", "data"} <= trees, trees)
        self.assertIn("is not a directory", err)

    def test_pre_restore_remove_refuses_anything_else(self):
        for bad in ("../etc", ".pre-restore-2026", "data", ".pre-restore-20261001T000000Z/../data"):
            rc, out, err = self.lib("pre-restore-remove", bad, setup=self.TREES)
            self.assertEqual(rc, 1, bad)
            self.assertIn(f"not a pre-restore tree: {bad}", err)
            self.assertIn("tree data", out)

# The VM stand-in: a login user with sudo (as OS Login gives), the units and
# timers the steps touch, and gcloud and podman stand-ins on sudo's
# secure_path. gcloud keeps what it is asked to upload; podman answers from
# canned JSON, with two digests as on the VM (a container's ImageDigest and
# its image's own Digest differ).
VM_SETUP = r"""
set -euo pipefail
dnf install -y -q openssh-server sudo tar gzip findutils >/dev/null
useradd -m -u 2000 opsuser
install -d -m 0700 -o opsuser -g opsuser /home/opsuser/.ssh
printf '%s\n' "$PUBKEY" > /home/opsuser/.ssh/authorized_keys
chown opsuser:opsuser /home/opsuser/.ssh/authorized_keys
echo 'opsuser ALL=(ALL) NOPASSWD: ALL' > /etc/sudoers.d/opsuser
ssh-keygen -A >/dev/null
useradd -u 1000 gitea
mkdir -p /var/lib/gitea /etc/containers /etc/gitea
chown 1000:1000 /var/lib/gitea
unit() { printf '%s\n' "$2" > "/etc/systemd/system/$1"; }
unit gitea.service $'[Service]\nExecStart=/usr/bin/sleep infinity'
unit caddy.service $'[Service]\nExecStart=/usr/bin/sleep infinity'
unit gitea-ar-auth.service $'[Service]\nType=oneshot\nExecStart=/usr/bin/touch /etc/containers/ar-auth.json'
unit podman-auto-update.service $'[Service]\nType=oneshot\nExecStart=/usr/bin/echo auto-update ran'
unit podman-auto-update.timer $'[Timer]\nOnCalendar=daily'
unit gitea-reboot-window.service $'[Service]\nType=oneshot\nExecStart=/usr/bin/true'
unit gitea-reboot-window.timer $'[Timer]\nOnCalendar=daily'
systemctl daemon-reload
systemctl start gitea.service caddy.service podman-auto-update.timer gitea-reboot-window.timer
cat > /usr/bin/gcloud <<'G'
#!/bin/bash
[[ "$1 $2 $3" == "storage cp -" ]] || { echo "stand-in gcloud: $*" >&2; exit 2; }
cat > "/tmp/upload-$(basename "$4")"
G
cat > /usr/bin/podman <<'P'
#!/bin/bash
case "$1" in
  inspect) [[ "$2" == gitea || "$2" == caddy ]] || { echo "[]"; echo "no such object: $2" >&2; exit 125; }
           printf '[{"Name":"%s","ImageDigest":"sha256:%064d","ImageName":"x"}]\n' "$2" 7 ;;
  image)   printf '[{"Id":"abc","Digest":"sha256:%064d"}]\n' 9 ;;
  pull)    echo "Trying to pull $4..." >&2; echo "abc123" ;;
  exec)    printf 'dump-of %s' "$*" ;;
  *)       echo "stand-in podman: $*" >&2; exit 2 ;;
esac
P
chmod 0755 /usr/bin/gcloud /usr/bin/podman
"""


class RehearsalOps(O.Ops):
    """Ops with its real connect() and inventory, except for what only GCP
    has: the key is a local one, the user is opsuser, and the proxy is sshd
    in the container on stdin instead of an IAP tunnel."""
    key = container = None

    def ensure_ssh_key(self):
        pass

    def register_key(self):
        self.key_at = 1e12

    def os_login_user(self):
        return "opsuser"

    def write_inventory(self):
        path = super().write_inventory()
        with open(path) as f:
            inv = json.load(f)
        host = inv["all"]["hosts"]["gitea"]
        host["ansible_ssh_private_key_file"] = self.key
        args = shlex.split(host["ansible_ssh_common_args"])
        i = next(n for n, a in enumerate(args) if a.startswith("ProxyCommand="))
        assert "start-iap-tunnel %h 22 --listen-on-stdin" in args[i]
        args[i] = f"ProxyCommand=podman exec -i {self.container} /usr/sbin/sshd -i"
        host["ansible_ssh_common_args"] = shlex.join(args)
        with open(path, "w") as f:
            json.dump(inv, f)
        return path


class Rehearsal(unittest.TestCase):
    """Every step a release, a rollback and a probe run that does not need a
    real podman or GCP, over SSH and sudo, against systemd."""

    @classmethod
    def setUpClass(cls):
        if not shutil.which("podman"):
            raise unittest.SkipTest("no podman")
        cls.dir = tempfile.mkdtemp(prefix="ops-rehearsal-")
        cls.key = os.path.join(cls.dir, "key")
        subprocess.run(["ssh-keygen", "-q", "-t", "ed25519", "-N", "", "-C", "ops-check", "-f", cls.key], check=True)
        cls.ctr = f"gitea-ops-rehearsal-{os.getpid()}"
        subprocess.run(["podman", "run", "-d", "--name", cls.ctr, "--systemd=always", ALMA_INIT],
                       check=True, capture_output=True)
        for _ in range(30):
            st = subprocess.run(["podman", "exec", cls.ctr, "systemctl", "is-system-running"],
                                capture_output=True, text=True).stdout.strip()
            if st in ("running", "degraded"):
                break
            subprocess.run(["sleep", "1"])
        with open(cls.key + ".pub") as f:
            pub = f.read().strip()
        r = subprocess.run(["podman", "exec", "-i", "-e", "PUBKEY=" + pub, cls.ctr, "bash"], input=VM_SETUP,
                           capture_output=True, text=True)
        if r.returncode != 0:
            cls.tearDownClass()
            raise RuntimeError("could not set up the rehearsal container: " + r.stderr[-2000:])
        RehearsalOps.key, RehearsalOps.container = cls.key, cls.ctr
        cls.ops = RehearsalOps(argparse.Namespace(region="r", project="p", repo="gitea", cmd="probe",
                                                  dry_run=False, vm="rehearsal", zone="z"))
        cls.ops.pylib = PYLIB

    @classmethod
    def tearDownClass(cls):
        if getattr(cls, "ops", None):
            cls.ops.disconnect()
        subprocess.run(["podman", "rm", "-f", "-t", "0", cls.ctr], capture_output=True)
        shutil.rmtree(cls.dir, ignore_errors=True)

    def vm(self, *args):
        with redirect_stderr(io.StringIO()) as err:
            try:
                return self.ops.vm(*args, mutate=False), err.getvalue()
            except subprocess.CalledProcessError as e:
                return e, err.getvalue()

    def on_vm(self, *cmd):
        return subprocess.run(["podman", "exec", self.ctr, *cmd], capture_output=True, text=True).stdout.strip()

    def active(self, *units):
        return self.on_vm("systemctl", "is-active", *units).split()

    def test_1_selftest_and_output_byte_for_byte(self):
        out, _ = self.vm("selftest")
        self.assertEqual(out, hashlib.sha256(lib_text().encode()).hexdigest())
        out, _ = self.vm("running-digest", "gitea")
        self.assertEqual(out, "sha256:" + "0" * 63 + "7")
        out, err = self.vm("pull", "img/a@sha256:1", "img/b:prod")
        self.assertEqual(out, "\n".join(["sha256:" + "0" * 63 + "9"] * 2))
        self.assertIn("Trying to pull img/b:prod...", err)
        self.assertNotIn("\r", out + err)

    def test_2_stop_start_and_restarts(self):
        self.assertEqual(self.vm("stop", "gitea.service")[0], "")
        self.assertEqual(self.active("gitea.service"), ["inactive"])
        self.assertEqual(self.vm("start", "gitea.service")[0], "")
        self.assertEqual(self.active("gitea.service"), ["active"])
        self.assertRegex(self.vm("restarts", "gitea.service")[0], r"^\d+$")

    def test_3_restart_if_not(self):
        pid = lambda: self.on_vm("systemctl", "show", "caddy.service", "-p", "ExecMainPID", "--value")
        before = pid()
        out, err = self.vm("restart-if-not", "sha256:" + "0" * 63 + "7", "caddy.service", "caddy")
        self.assertEqual((out, pid()), ("", before))
        self.assertIn("caddy already runs sha256:", err)
        self.vm("restart-if-not", "sha256:" + "1" * 64, "caddy.service", "caddy")
        self.assertNotEqual(pid(), before)
        self.assertEqual(self.active("caddy.service"), ["active"])

    def test_4_timers_and_the_dead_mans_switch(self):
        timers = ("podman-auto-update.timer", "gitea-reboot-window.timer")
        self.vm("pause-timers", "5m")
        self.assertEqual(self.active(*timers, "gitea-ops-deadman.timer"), ["inactive", "inactive", "active"])
        self.vm("pause-timers")  # again: the old switch is replaced, not an error
        self.assertIn("3h", self.on_vm("systemctl", "show", "gitea-ops-deadman.timer", "-p", "TimersMonotonic"))
        self.vm("resume-timers")
        self.assertEqual(self.active(*timers, "gitea-ops-deadman.timer"), ["active", "active", "inactive"])

    def test_4b_the_switch_fires(self):
        # The safety net for an engine that died: the timers come back on
        # their own, and it says so at crit. (AccuracySec=1min: up to a
        # minute late.)
        timers = ("podman-auto-update.timer", "gitea-reboot-window.timer")
        self.vm("pause-timers", "2s")
        self.assertEqual(self.active(*timers), ["inactive", "inactive"])
        for _ in range(90):
            if self.active(*timers) == ["active", "active"]:
                break
            subprocess.run(["sleep", "1"])
        self.assertEqual(self.active(*timers), ["active", "active"])
        for _ in range(10):
            log = self.on_vm("journalctl", "-t", "gitea-ops", "-p", "crit", "--no-pager", "-o", "cat")
            if "DEADMAN" in log:
                break
            subprocess.run(["sleep", "1"])
        self.assertIn("DEADMAN: the release engine never came back; timers re-enabled; gitea.service is active", log)
        self.vm("resume-timers")

    def test_5_either_engine_disarms_the_others_switch(self):
        r = subprocess.run(["git", "-C", ROOT, "show", f"{BASH_ENGINE}:scripts/ops/vm-steps.sh"],
                           capture_output=True, text=True)
        if r.returncode != 0:
            self.skipTest("no git history")
        bash = lambda step: subprocess.run(["podman", "exec", "-i", self.ctr, "bash", "-s", step], input=r.stdout,
                                           capture_output=True, text=True)
        timers = ("podman-auto-update.timer", "gitea-reboot-window.timer", "gitea-ops-deadman.timer")
        self.vm("pause-timers")
        self.assertEqual(bash("resume-timers").returncode, 0)
        self.assertEqual(self.active(*timers), ["active", "active", "inactive"])
        self.assertEqual(bash("pause-timers").returncode, 0)
        self.assertEqual(self.active(*timers), ["inactive", "inactive", "active"])
        self.vm("resume-timers")
        self.assertEqual(self.active(*timers), ["active", "active", "inactive"])

    def test_6_markers(self):
        self.assertEqual(self.vm("marker-check", "m1")[0], "absent")
        self.vm("marker", "m1")
        self.assertEqual(self.on_vm("stat", "-c", "%u:%g %a", "/var/lib/gitea/.drill-m1"), "1000:1000 644")
        self.assertEqual(self.vm("marker-check", "m1")[0], "present")

    def test_7_data_steps_stream_to_the_bucket(self):
        self.vm("dump", "gs://b/dumps/gitea-x.zip")
        self.assertEqual(self.on_vm("cat", "/tmp/upload-gitea-x.zip"),
                         "dump-of exec -u 1000 gitea gitea dump -c /etc/gitea/app.ini -t /tmp -f -")
        self.vm("config-tarball", "gs://b/config/x/vm-config.tar.gz")
        # What is there (this stand-in has no Caddy, no fstab: skipped, as
        # --ignore-failed-read says).
        listing = self.on_vm("tar", "-tzf", "/tmp/upload-vm-config.tar.gz").split()
        self.assertIn("etc/gitea/", listing)
        self.assertIn("etc/systemd/system/gitea-reboot-window.timer", listing)
        self.assertRegex(self.vm("journal-errors", "gitea.service", "2026-01-01 00:00:00 UTC")[0], r"^\d+$")
        self.assertEqual(self.vm("pre-restore-list")[0], "")

    def test_8_failures_keep_their_exit_code_and_message(self):
        e, err = self.vm("migrate", "img@sha256:1")  # gitea.service is running
        self.assertIsInstance(e, subprocess.CalledProcessError)
        self.assertEqual(e.returncode, 1)
        self.assertIn("[vm-steps] ERROR: gitea.service is running; stop it first", err)
        e, _ = self.vm("running-digest", "nothing")
        self.assertIsInstance(e, subprocess.CalledProcessError)
        self.assertEqual(self.active("gitea.service"), ["active"])

    def test_9_one_master_connection_closed_at_exit(self):
        self.vm("restarts", "gitea.service")
        sock = os.path.join(self.ops.cp_dir, "rehearsal")
        check = ["ssh", "-o", f"ControlPath={os.path.join(self.ops.cp_dir, '%h')}", "-O", "check", "opsuser@rehearsal"]
        self.assertEqual(subprocess.run(check, capture_output=True).returncode, 0, "master is up")
        with open(os.path.join(self.ops.work, "known_hosts")) as f:
            self.assertEqual(len(f.read().splitlines()), 1)
        self.ops.disconnect()
        self.assertNotEqual(subprocess.run(check, capture_output=True).returncode, 0, "master is gone")
        self.assertFalse(os.path.exists(sock))
        self.assertIsNone(self.ops.work)
        # And the next step simply connects again.
        self.assertRegex(self.vm("restarts", "gitea.service")[0], r"^\d+$")


class Provenance(unittest.TestCase):
    def test_the_lib_is_the_bash_engine_with_steps_deleted(self):
        r = subprocess.run(["git", "-C", ROOT, "show", f"{BASH_ENGINE}:scripts/ops/vm-steps.sh"],
                           capture_output=True, text=True)
        if r.returncode != 0:
            self.skipTest(f"no git history ({r.stderr.strip()})")
        old, new = r.stdout.split("\n"), lib_text().split("\n")
        s = new.index("# selftest: the sha256 of this script's own text. ops.yml never copies it to")
        e = new.index("}", s) + 2
        self.assertTrue(new[e - 2] == "}" and new[e - 3].startswith("    printf '%s' \"${BASH_EXECUTION_STRING"))
        rest = iter(old)
        missing = [l for l in new[:s] + new[e:] if not any(l == o for o in rest)]
        self.assertEqual(missing, [], "lines of the lib that are not vm-steps.sh's, in its order")


if __name__ == "__main__":
    unittest.main()
