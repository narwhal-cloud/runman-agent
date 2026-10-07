"""Run the real --update-only path in a temporary sandbox with mocked host tools.

Linux CI only. No root, containers, real systemd or network access required.
"""
import json
import fcntl
import os
from pathlib import Path
import sqlite3
import subprocess
import tempfile
import unittest


class UpgradeTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory(prefix="runman-upgrade-test-")
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name)
        self.agent = self.root / "agent"
        self.data = self.root / "data"
        self.mock = self.root / "bin"
        for directory in (self.agent, self.data, self.mock):
            directory.mkdir()
        self.calls = self.root / "calls"
        self.db_path = self.agent / "agent.db"
        self.db = sqlite3.connect(self.db_path)
        self.addCleanup(self.db.close)
        self.db.execute("PRAGMA journal_mode=WAL")
        self.db.execute("CREATE TABLE preserved (id TEXT)")
        self.db.execute("INSERT INTO preserved VALUES ('container-and-ssh-rule')")
        self.db.commit()  # Keep connection open so committed data remains in WAL.
        self.config = {
            "virt_type": "incus", "db": str(self.db_path), "token": "keep-token",
            "web_pass_hash": "keep-password-hash", "ipv6_mode": "subnet",
            "ipv6_subnet": "2001:db8::/64", "incus_ipv6_only": True,
            "incus_image_mirror": "https://private.example.test",
            "custom_unknown_field": "keep-me",
        }
        self.write_config()
        (self.agent / "narwhal-agent").write_text("old-agent")
        self.new_binary = b'\x7fELF\x02\x01' + bytes(12) + (62).to_bytes(2, 'little') + b'new-agent'
        (self.root / "new-agent").write_bytes(self.new_binary)
        source = (Path(__file__).resolve().parents[1] / "install.sh").read_text()
        source = source.replace("/opt/narwhal-agent", str(self.agent))
        source = source.replace("/var/lib/narwhal-agent", str(self.data))
        source = source.replace("/run/lock/narwhal-agent-install.lock", str(self.root / "install.lock"))
        self.script = self.root / "install.sh"
        self.script.write_text(source)
        self.env = dict(os.environ, PATH=str(self.mock) + ":" + os.environ["PATH"],
                        TEST_ROOT=str(self.root), TEST_CALLS=str(self.calls), TEST_ACTIVE="1")
        for key in list(self.env):
            if key.startswith(("INCUS_", "IPV6_", "PODMAN_", "RUNMAN_", "NARWHAL_")):
                del self.env[key]
        self.tool("id", "echo 0")
        self.tool("sleep", "exit 0")
        self.tool("uname", "echo x86_64")
        self.tool("flock", 'python3 -c "import sys, fcntl; fcntl.flock(int(sys.argv[2]), fcntl.LOCK_EX | fcntl.LOCK_NB)" "$@"')
        self.tool("systemctl", r'''
printf 'systemctl %s\n' "$*" >> "$TEST_CALLS"
case "$1" in
  show) printf '%s --config %s\n' "$TEST_ROOT/agent/narwhal-agent" "$TEST_ROOT/agent/config.json" ;;
  cat) echo '[Service]' ;;
  is-active) test "$TEST_ACTIVE" = 1 ;;
  restart) test "${TEST_RESTART_FAIL:-0}" != 1 ;;
  *) exit 91 ;;
esac
''')
        self.tool("curl", r'''
printf 'curl %s\n' "$*" >> "$TEST_CALLS"
test "${TEST_DOWNLOAD_FAIL:-0}" != 1 || exit 22
test "$1" = -fsSL && test "$2" = -o || exit 92
cp "$TEST_ROOT/new-agent" "$3"
''')
        for tool in ("podman", "incus", "sysctl", "apt-get", "ip", "modprobe"):
            self.tool(tool, 'echo "FORBIDDEN $0 $*" >> "$TEST_CALLS"; exit 93')

    def tool(self, name, body):
        path = self.mock / name
        path.write_text("#!/bin/bash\nset -e\n" + body + "\n")
        path.chmod(0o755)

    def write_config(self):
        (self.agent / "config.json").write_text(json.dumps(self.config))

    def run_installer(self, *args, success=True):
        result = subprocess.run(["bash", str(self.script), "en", "--update-only",
                                 "--non-interactive", *args], cwd=self.root, env=self.env,
                                text=True, capture_output=True, timeout=30)
        output = result.stdout + result.stderr
        if success:
            self.assertEqual(result.returncode, 0, output)
        else:
            self.assertNotEqual(result.returncode, 0, output)
        calls = self.calls.read_text() if self.calls.exists() else ""
        self.assertNotIn("FORBIDDEN", calls)
        return output, calls

    def test_preserves_config_and_live_wal_database(self):
        _, calls = self.run_installer()
        self.assertIn("systemctl restart narwhal-agent", calls)
        self.assertIn("narwhal-cloud/runman-agent/releases/download/continuous", calls)
        self.assertEqual((self.agent / "narwhal-agent").read_bytes(), self.new_binary)
        config = json.loads((self.agent / "config.json").read_text())
        for key, value in self.config.items():
            self.assertEqual(config[key], value, key)
        backup, = (self.data / "backups").glob("upgrade-*")
        self.assertEqual(backup.stat().st_mode & 0o777, 0o700)
        self.assertEqual((backup / "narwhal-agent").read_text(), "old-agent")
        self.assertEqual(json.loads((backup / "config.json").read_text()), self.config)
        with sqlite3.connect(backup / "agent.db") as copied:
            self.assertEqual(copied.execute("SELECT id FROM preserved").fetchone()[0], "container-and-ssh-rule")

    def test_stopped_agent_stays_stopped(self):
        self.env["TEST_ACTIVE"] = "0"
        _, calls = self.run_installer()
        self.assertNotIn("systemctl restart", calls)

    def test_menu_agent_only_upgrade(self):
        result = subprocess.run(["bash", str(self.script), "en", "--menu"],
                                cwd=self.root, env=self.env, input="14\n",
                                text=True, capture_output=True, timeout=30)
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
        self.assertIn("Agent-only update complete", result.stdout)
        self.assertNotIn("FORBIDDEN", self.calls.read_text())

    def test_download_failure_keeps_original_binary(self):
        self.env["TEST_DOWNLOAD_FAIL"] = "1"
        _, calls = self.run_installer(success=False)
        self.assertEqual((self.agent / "narwhal-agent").read_text(), "old-agent")
        self.assertNotIn("systemctl restart", calls)
        self.assertEqual(json.loads((self.agent / "config.json").read_text()), self.config)

    def test_wrong_download_does_not_replace_binary_or_config(self):
        (self.root / "new-agent").write_text('<html>download error</html>')
        output, calls = self.run_installer(success=False)
        self.assertIn("not an ELF64 binary", output)
        self.assertEqual((self.agent / "narwhal-agent").read_text(), "old-agent")
        self.assertEqual(json.loads((self.agent / "config.json").read_text()), self.config)
        self.assertNotIn("systemctl restart", calls)

    def test_restart_failure_is_not_reported_as_success(self):
        self.env["TEST_RESTART_FAIL"] = "1"
        output, _ = self.run_installer(success=False)
        self.assertIn("Agent restart failed. Backup:", output)

    def test_missing_config_does_not_install_fresh(self):
        (self.agent / "config.json").unlink()
        output, calls = self.run_installer(success=False)
        self.assertIn("Incomplete/nonstandard installation", output)
        self.assertNotIn("curl", calls)

    def test_missing_database_fails_before_changes(self):
        self.config["db"] = str(self.root / "absent.db")
        self.write_config()
        _, calls = self.run_installer(success=False)
        self.assertNotIn("curl", calls)

    def test_rejects_backend_switch(self):
        output, _ = self.run_installer("--virt", "podman", success=False)
        self.assertIn("cannot switch", output)

    def test_invalid_json_fails_before_download(self):
        (self.agent / "config.json").write_text("{broken")
        _, calls = self.run_installer(success=False)
        self.assertNotIn("curl", calls)

    def test_database_backup_failure_prevents_replacement(self):
        self.db.close()
        self.db_path.write_bytes(b"not a sqlite database")
        _, calls = self.run_installer(success=False)
        self.assertNotIn("curl", calls)
        self.assertEqual((self.agent / "narwhal-agent").read_text(), "old-agent")

    def test_concurrent_update_is_rejected(self):
        with (self.root / "install.lock").open("w") as lock:
            fcntl.flock(lock, fcntl.LOCK_EX | fcntl.LOCK_NB)
            output, calls = self.run_installer(success=False)
        self.assertIn("already running", output)
        self.assertNotIn("curl", calls)

    def test_agent_only_ignores_stray_local_debug_binary(self):
        (self.root / "runman-agent-linux-amd64").write_text("stale-debug-binary")
        self.run_installer()
        self.assertEqual((self.agent / "narwhal-agent").read_bytes(), self.new_binary)

    def test_rejects_network_change(self):
        output, _ = self.run_installer("--nat4", success=False)
        self.assertIn("preserves existing networking", output)

    def test_rejects_token_change(self):
        _, calls = self.run_installer("--token", "replace-token", success=False)
        self.assertNotIn("curl", calls)
        self.assertEqual(json.loads((self.agent / "config.json").read_text())["token"], "keep-token")

    def test_rejects_uninstall_action(self):
        output, calls = self.run_installer("--uninstall", success=False)
        self.assertIn("cannot be combined", output)
        self.assertEqual(calls, "")

    def test_nonstandard_service_fails_closed(self):
        self.tool("systemctl", "echo /some/other/agent")
        output, _ = self.run_installer(success=False)
        self.assertIn("Nonstandard service", output)

    def test_no_installation_fails_closed(self):
        (self.agent / "narwhal-agent").unlink()
        (self.agent / "config.json").unlink()
        self.env["TEST_ACTIVE"] = "0"
        output, _ = self.run_installer(success=False)
        self.assertIn("will not perform a fresh installation", output)

    def test_mirror_images_are_selected_by_host_architecture(self):
        source = (Path(__file__).resolve().parents[1] / "install.sh").read_text()
        self.assertIn('--arg arch "$ARCH"', source)
        self.assertIn('(.value.architecture? // .value.arch? // "") == $arch', source)
        self.assertIn('MIRROR_IMPORT_REASON="no_matching_arch"', source)
        self.assertNotIn('[ "$ARCH" = "amd64" ]', source)
        self.assertIn('curl 退出码 $curl_status', source)

    def test_rfw_retries_xdp_attach_failures_in_skb_mode(self):
        source = (Path(__file__).resolve().parents[1] / "install.sh").read_text()
        self.assertIn('rfw_api_ready()', source)
        self.assertIn('enable_rfw_skb_mode()', source)
        self.assertIn('--xdp-mode skb', source)
        self.assertIn('XDP.*(attach|附加)', source)


class DefaultRouteIfaceTests(unittest.TestCase):
    """The uplink must be parsed by field name, never by a fixed column.

    Kernels using RFC 5549 nexthop objects (AWS EC2, some cloud images) render
    the IPv6 default route as::

        default nhid 3525900573 via fe80::4ee:... dev ens5 proto ra metric 100

    Reading a fixed column (awk '{print $5}') then yields the gateway address
    instead of the interface name, so every later ``ip ... dev <gateway>`` call
    fails silently and detection reports "No global IPv6 address found".  On a
    host whose only IPv6 is a single /128 that produced ipv6_mode=none and a
    container network with ipv6_enabled=false.
    """
    # ip(8) output formats seen in the wild, all of which must yield "ens5".
    ROUTES = {
        "nhid": ("default nhid 3525900573 via fe80::4ee:e8ff:fe3e:2051 dev ens5 "
                 "proto ra metric 100 expires 1792sec pref medium"),
        "plain-via": "default via fe80::4ee:e8ff:fe3e:2051 dev ens5 proto ra metric 100",
        "no-gateway": "default dev ens5 proto ra metric 100",
        "metric-first": "default metric 100 via fe80::4ee:e8ff:fe3e:2051 dev ens5 proto ra",
    }
    ADDR = ("2: ens5: <BROADCAST,MULTICAST,UP,LOWER_UP> mtu 9001 state UP qlen 1000\n"
            "    inet6 2406:da18:1e08:2900:21f3:dda1:e810:46b8/128 scope global "
            "dynamic noprefixroute \n"
            "       valid_lft 386sec preferred_lft 76sec\n"
            "    inet6 fe80::4ee:e8ff:fe3e:2051/64 scope link \n"
            "       valid_lft forever preferred_lft forever\n")

    def setUp(self):
        self.temp = tempfile.TemporaryDirectory(prefix="runman-route-test-")
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name)
        self.mock = self.root / "bin"
        self.mock.mkdir()
        # Only ens5 answers; a wrong interface name yields no address at all.
        (self.mock / "ip").write_text(
            '#!/bin/bash\n'
            'case "$*" in\n'
            '  "-6 route show default") echo "$TEST_ROUTE" ;;\n'
            '  "-6 addr show dev ens5") cat "$TEST_ADDR_FILE" ;;\n'
            '  *) exit 1 ;;\n'
            'esac\n')
        (self.mock / "ip").chmod(0o755)
        (self.mock / "curl").write_text("#!/bin/bash\nexit 0\n")
        (self.mock / "curl").chmod(0o755)
        (self.root / "addr.txt").write_text(self.ADDR)
        self.script = self.root / "install.sh"
        self.script.write_text((Path(__file__).resolve().parents[1] / "install.sh").read_text())

    def detect(self, route):
        env = dict(os.environ, PATH=str(self.mock) + ":" + os.environ["PATH"],
                   TEST_ROUTE=route, TEST_ADDR_FILE=str(self.root / "addr.txt"))
        for key in list(env):
            if key.startswith(("INCUS_", "IPV6_", "PODMAN_", "RUNMAN_", "NARWHAL_")):
                del env[key]
        return subprocess.run(["bash", str(self.script), "--detect-ipv6"], cwd=self.root,
                              env=env, text=True, capture_output=True, timeout=30)

    def test_uplink_is_found_for_every_route_format(self):
        for label, route in self.ROUTES.items():
            with self.subTest(format=label):
                result = self.detect(route)
                self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
                self.assertIn("Default IPv6 interface: ens5", result.stderr)
                self.assertNotIn("No default IPv6 route found", result.stderr)

    def test_slash128_host_selects_snat(self):
        result = self.detect(self.ROUTES["nhid"])
        self.assertIn("SNAT mode", result.stderr)
        self.assertEqual(
            result.stdout.strip().splitlines()[-1],
            "ens5|2406:da18:1e08:2900:21f3:dda1:e810:46b8|128||0")

    def test_source_never_parses_route_columns_by_position(self):
        source = (Path(__file__).resolve().parents[1] / "install.sh").read_text()
        self.assertNotRegex(source, r"route show default.*print \$5")


if __name__ == "__main__":
    unittest.main()
