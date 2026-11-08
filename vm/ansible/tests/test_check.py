"""Tests for tests/check's own static checks: fixed_macs(), app_handlers() and
neighbor_flush(), each fed made-up goldens, site.yml handlers or role app
tasks, good and bad, and held to what it reports. A check that passes on the
real tree proves nothing unless it is also seen to fail on a broken one.

tests/check runs these (python3 -m unittest discover -s vm/ansible/tests -p
test_check.py) before the checks themselves, so PR previews, deploys and
`make ansible-check` all do. They read and write only temporary files.
"""
import importlib.machinery
import importlib.util
import os
import tempfile
import unittest

import yaml

HERE = os.path.dirname(os.path.abspath(__file__))


def load_check():
    """tests/check, which has no .py name, as a module."""
    loader = importlib.machinery.SourceFileLoader("tests_check", os.path.join(HERE, "check"))
    module = importlib.util.module_from_spec(importlib.util.spec_from_loader("tests_check", loader))
    loader.exec_module(module)
    return module


CHECK = load_check()
ADDRESSES, CONFIG = CHECK.ADDRESSES_CHANGED, CHECK.CONFIG_CHANGED
GITEA_MAC, CADDY_MAC = "02:00:00:00:00:fa", "02:00:00:00:00:fb"


class Case(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self.tmp.cleanup)

    def assertReports(self, problems, text):
        self.assertTrue(any(text in p for p in problems), f"no problem saying {text!r} in {problems}")


class FixedMacs(Case):
    """fixed_macs(): Gitea's and Caddy's Network= lines in every fixture."""

    def goldens(self, gitea, caddy):
        """A goldens directory with one fixture whose quadlets have these
        Network= values; fixed_macs()' problems with it."""
        fixture = os.path.join(self.tmp.name, "golden", "one")
        os.makedirs(fixture)
        for quadlet, network in ((CHECK.GITEA_QUADLET, gitea), (CHECK.CADDY_QUADLET, caddy)):
            with open(os.path.join(fixture, quadlet + ".golden"), "w") as fh:
                fh.write(f"# a comment\n[Container]\nContainerName=x\nNetwork={network}\nLogDriver=journald\n")
        return CHECK.fixed_macs(os.path.join(self.tmp.name, "golden"))

    def test_both_on_the_bridge_with_their_own_macs(self):
        self.assertEqual(self.goldens(f"gitea.network:mac={GITEA_MAC}", f"gitea.network:mac={CADDY_MAC}"), [])

    def test_caddy_on_the_host_network_needs_none(self):
        self.assertEqual(self.goldens(f"gitea.network:mac={GITEA_MAC}", "host"), [])

    def test_the_same_mac_twice(self):
        self.assertReports(self.goldens(f"gitea.network:mac={GITEA_MAC}", f"gitea.network:mac={GITEA_MAC}"),
                           f"Gitea and Caddy have the same MAC, {GITEA_MAC}")

    def test_a_universally_administered_mac(self):
        self.assertReports(self.goldens("gitea.network:mac=00:16:3e:00:00:fa", f"gitea.network:mac={CADDY_MAC}"),
                           "mac=00:16:3e:00:00:fa is not locally administered unicast")

    def test_a_multicast_mac(self):
        self.assertReports(self.goldens(f"gitea.network:mac={GITEA_MAC}", "gitea.network:mac=03:00:00:00:00:fb"),
                           "mac=03:00:00:00:00:fb is not locally administered unicast")

    def test_capitals(self):
        self.assertReports(self.goldens("gitea.network:mac=02:00:00:00:00:FA", f"gitea.network:mac={CADDY_MAC}"),
                           "mac=02:00:00:00:00:FA is not a MAC in lowercase")

    def test_not_a_mac(self):
        self.assertReports(self.goldens("gitea.network:mac=02:00:00:00:fa", f"gitea.network:mac={CADDY_MAC}"),
                           "mac=02:00:00:00:fa is not a MAC in lowercase")

    def test_no_mac_option(self):
        problems = self.goldens("gitea.network", "gitea.network:interface_name=eth0")
        self.assertReports(problems, "gitea.container: Network=gitea.network has 0 mac= options, not one")
        self.assertReports(problems, "caddy.container: Network=gitea.network:interface_name=eth0 has 0 mac= options")

    def test_two_mac_options(self):
        self.assertReports(self.goldens(f"gitea.network:mac={GITEA_MAC},mac=02:00:00:00:00:fc",
                                        f"gitea.network:mac={CADDY_MAC}"),
                           "has 2 mac= options, not one")

    def test_another_network(self):
        self.assertReports(self.goldens(f"other.network:mac={GITEA_MAC}", f"gitea.network:mac={CADDY_MAC}"),
                           "gitea.container: Network=other.network:mac=02:00:00:00:00:fa, not gitea.network")

    def test_gitea_on_the_host_network(self):
        problems = self.goldens("host", f"gitea.network:mac={CADDY_MAC}")
        self.assertReports(problems, "gitea.container: Network=host, not gitea.network")
        self.assertFalse(any("or host" in p for p in problems), problems)

    def test_caddy_with_a_mac_on_the_host_network(self):
        self.assertReports(self.goldens(f"gitea.network:mac={GITEA_MAC}", f"host:mac={CADDY_MAC}"),
                           "caddy.container: Network=host:mac=02:00:00:00:00:fb, not gitea.network or host")


def write_yaml(directory, name, content):
    path = os.path.join(directory, name)
    with open(path, "w") as fh:
        yaml.safe_dump(content, fh, sort_keys=False)
    return path


def handler(name, unit, state, listen):
    return {"name": name, "ansible.builtin.systemd_service": {"name": unit, "state": state}, "listen": listen}


class AppHandlers(Case):
    """app_handlers(): site.yml's stop and restarts, in order, on their topics."""

    def handlers(self, **change):
        """site.yml's three handlers (a string listen:, a list, and a string),
        with an unrelated one around them; change maps one of stop, gitea,
        caddy to a replacement (None drops it)."""
        named = {
            "stop": handler("Stop caddy", "caddy.service", "stopped", ADDRESSES),
            "gitea": handler("Restart gitea", "gitea.service", "restarted", [CONFIG]),
            "caddy": handler("Restart caddy", "caddy.service", "restarted", CONFIG),
        }
        named.update(change)
        return named

    def problems(self, order=("stop", "gitea", "caddy"), **change):
        named = self.handlers(**change)
        handlers = [{"name": "Reload systemd", "ansible.builtin.systemd_service": {"daemon_reload": True},
                     "listen": [CONFIG]}]
        handlers += [named[k] for k in order if named[k] is not None]
        handlers.append({"name": "Load nftables", "ansible.builtin.command": "nft -f /etc/sysconfig/nftables.conf"})
        return CHECK.app_handlers(write_yaml(self.tmp.name, "site.yml", [{"hosts": "gitea", "handlers": handlers}]))

    def test_in_order_on_their_topics(self):
        self.assertEqual(self.problems(), [])

    def test_listen_as_a_list_or_a_string(self):
        self.assertEqual(self.problems(stop=handler("Stop caddy", "caddy.service", "stopped", [ADDRESSES, "x"]),
                                       gitea=handler("Restart gitea", "gitea.service", "restarted", CONFIG)), [])

    def test_each_one_missing(self):
        for which, says in (("stop", "stops caddy.service"), ("gitea", "restarts gitea.service"),
                            ("caddy", "restarts caddy.service")):
            with self.subTest(which):
                self.assertReports(self.problems(**{which: None}), f"site.yml: no handler {says}")

    def test_caddy_restarts_before_gitea(self):
        self.assertReports(self.problems(order=("stop", "caddy", "gitea")), "the handlers must run in this order")

    def test_caddy_stops_after_gitea_restarts(self):
        self.assertReports(self.problems(order=("gitea", "stop", "caddy")), "the handlers must run in this order")

    def test_caddy_restarts_before_it_stops(self):
        self.assertReports(self.problems(order=("caddy", "stop", "gitea")), "the handlers must run in this order")

    def test_caddy_stops_on_the_wrong_topic(self):
        self.assertReports(self.problems(stop=handler("Stop caddy", "caddy.service", "stopped", CONFIG)),
                           "'Stop caddy' doesn't listen on container addresses changed")

    def test_a_restart_on_the_wrong_topic(self):
        self.assertReports(self.problems(gitea=handler("Restart gitea", "gitea.service", "restarted", ADDRESSES)),
                           "'Restart gitea' doesn't listen on gitea config changed")

    def test_no_listen_at_all(self):
        self.assertReports(self.problems(caddy={"name": "Restart caddy", "ansible.builtin.systemd_service":
                                                {"name": "caddy.service", "state": "restarted"}}),
                           "'Restart caddy' doesn't listen on gitea config changed")


class NeighborFlush(Case):
    """neighbor_flush(): role app reads the bridge's name and flushes it after
    "Gitea and Caddy running", on every run but --check."""

    RUNNING = {"name": "Gitea and Caddy running",
               "ansible.builtin.systemd_service": {"name": "{{ item }}", "state": "started"},
               "loop": ["gitea.service", "caddy.service"]}

    def bridge(self, **change):
        task = {"name": "The gitea network's bridge",
                "ansible.builtin.command": {"argv": ["podman", "network", "inspect", "gitea"]},
                "register": "app_bridge", "changed_when": False, "when": "not ansible_check_mode"}
        task.update(change)
        return {k: v for k, v in task.items() if v is not None}

    def flush(self, reads="app_bridge", **change):
        task = {"name": "Flush the host's neighbor entries on it",
                "ansible.builtin.command": {"argv": ["ip", "neigh", "flush", "dev",
                                                     f"{{{{ ({reads}.stdout | from_json | first).network_interface }}}}"]},
                "changed_when": False, "when": "not ansible_check_mode"}
        task.update(change)
        return {k: v for k, v in task.items() if v is not None}

    def problems(self, tasks):
        tasks = [{"name": "Quadlets", "ansible.builtin.template": {"src": "x", "dest": "y"}}] + tasks
        return CHECK.neighbor_flush(write_yaml(self.tmp.name, "main.yml", tasks))

    def test_right_after_both_run(self):
        self.assertEqual(self.problems([self.RUNNING, self.bridge(), self.flush()]), [])

    def test_when_as_a_one_item_list(self):
        self.assertEqual(self.problems([self.RUNNING, self.bridge(when=["not ansible_check_mode"]),
                                        self.flush(when=["not ansible_check_mode"])]), [])

    def test_each_one_missing(self):
        for tasks, says in (([self.bridge(), self.flush()], "no task starts gitea.service and caddy.service"),
                            ([self.RUNNING, self.flush()], "no task reads the gitea network's bridge"),
                            ([self.RUNNING, self.bridge()], "no task flushes the host's neighbor entries")):
            with self.subTest(says):
                self.assertReports(self.problems(tasks), says)

    def test_the_flush_before_both_run(self):
        self.assertReports(self.problems([self.bridge(), self.flush(), self.RUNNING]),
                           "must come after 'Gitea and Caddy running'")

    def test_the_bridge_after_the_flush(self):
        self.assertReports(self.problems([self.RUNNING, self.flush(), self.bridge()]),
                           "\"The gitea network's bridge\" must come before")

    def test_the_flush_under_check(self):
        self.assertReports(self.problems([self.RUNNING, self.bridge(), self.flush(when=None)]),
                           "\"Flush the host's neighbor entries on it\" must run on every run but --check")

    def test_the_bridge_under_check(self):
        self.assertReports(self.problems([self.RUNNING, self.bridge(when=None), self.flush()]),
                           "\"The gitea network's bridge\" must run on every run but --check")

    def test_the_flush_only_sometimes(self):
        self.assertReports(self.problems([self.RUNNING, self.bridge(),
                                          self.flush(when=["not ansible_check_mode", "app_wrong | length > 0"])]),
                           "\"Flush the host's neighbor entries on it\" must run on every run but --check")

    def test_the_flush_reads_another_name(self):
        self.assertReports(self.problems([self.RUNNING, self.bridge(), self.flush(reads="app_other")]),
                           "doesn't read what \"The gitea network's bridge\" registers (app_bridge)")

    NAME = "{{ (app_bridge.stdout | from_json | first).network_interface }}"

    def argv_reported(self, args):
        """A flush with these argv, the rest right: neighbor_flush()'s problems."""
        flush = self.flush()
        flush["ansible.builtin.command"] = {"argv": args}
        return self.problems([self.RUNNING, self.bridge(), flush])

    def test_flush_all_with_the_name_elsewhere(self):
        self.assertReports(self.argv_reported(["ip", "neigh", "flush", "all", self.NAME]),
                           "must be exactly `ip neigh flush dev")

    def test_dev_lo_with_the_name_elsewhere(self):
        self.assertReports(self.argv_reported(["ip", "neigh", "flush", "dev", "lo", self.NAME]),
                           "must be exactly `ip neigh flush dev")

    def test_extra_arguments(self):
        self.assertReports(self.argv_reported(["ip", "neigh", "flush", "dev", self.NAME, "nud", "stale"]),
                           "must be exactly `ip neigh flush dev")

    def test_no_dev(self):
        self.assertReports(self.argv_reported(["ip", "neigh", "flush", self.NAME]),
                           "must be exactly `ip neigh flush dev")

    def test_more_than_the_name_after_dev(self):
        self.assertReports(self.argv_reported(["ip", "neigh", "flush", "dev", "lo" + self.NAME]),
                           "must be exactly `ip neigh flush dev")

    def test_the_bridge_registers_nothing(self):
        self.assertReports(self.problems([self.RUNNING, self.bridge(register=None), self.flush()]),
                           "registers (nothing)")


class TheRealTree(unittest.TestCase):
    """The checks on this tree's own site.yml and role app tasks (fixed_macs()
    needs the goldens tests/check has just compared, so it runs there)."""

    def test_site_yml(self):
        self.assertEqual(CHECK.app_handlers(), [])

    def test_role_app_tasks(self):
        self.assertEqual(CHECK.neighbor_flush(), [])


if __name__ == "__main__":
    unittest.main()
