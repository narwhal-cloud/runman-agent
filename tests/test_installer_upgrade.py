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
        self.assertIn("podcctv/runman-agent/releases/download/continuous", calls)
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


if __name__ == "__main__":
    unittest.main()
