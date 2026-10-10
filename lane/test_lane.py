#!/usr/bin/env python3
"""Exercise fail-closed target guards without connecting to any server."""
import json
import os
from pathlib import Path
import shlex
import subprocess
import tempfile
import unittest

HERE = Path(__file__).resolve().parent
IDENTITY = "1234567890abcdef1234567890abcdef"


class TargetSafety(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory(prefix="bedrock-lane-guard-")
        self.addCleanup(self.temp.cleanup)
        self.work = Path(self.temp.name)
        self.bin = self.work / "bin"
        self.bin.mkdir()
        self.marker = self.work / "bedrock-lane-id"
        self.marker.write_text(IDENTITY)
        self.marker.chmod(0o600)
        (self.work / "registry.json").write_text('{"apps":[]}')
        for name in ("key", "known_hosts"):
            (self.work / name).write_text("test fixture\n")
        self.script("id", "#!/bin/sh\necho 0\n")
        self.script("bedrock", '#!/bin/sh\ncat "$LANE_TEST_ROOT/registry.json"\n')
        self.script("stat", "#!/usr/bin/env python3\nimport os,sys\ns=os.stat(sys.argv[3]); print(int(os.environ.get('LANE_TEST_OWNER', '0')) if sys.argv[2]=='%u' else oct(s.st_mode & 0o777)[2:])\n")
        self.script("ssh", '''#!/usr/bin/env python3
import os,sys,subprocess
if os.environ.get("LANE_TEST_UNREACHABLE"): raise SystemExit(255)
if sys.argv[-2:] == ["uname", "-m"]: print("x86_64"); raise SystemExit(0)
script=sys.stdin.read().replace("/etc/bedrock-production", os.environ["LANE_TEST_ROOT"] + "/bedrock-production").replace("/etc/bedrock-lane-id", os.environ["LANE_TEST_ROOT"] + "/bedrock-lane-id")
result=subprocess.run(["sh", "-s", "--", os.environ["LANE_TEST_ID"]], input=script, text=True)
raise SystemExit(result.returncode)
''')
        self.config = self.work / "lane.env"
        self.write_config()

    def script(self, name: str, source: str):
        path = self.bin / name
        path.write_text(source)
        path.chmod(0o755)

    def write_config(self, disposable="true", identity=IDENTITY, host="127.0.0.1"):
        values = {"LANE_DISPOSABLE": disposable, "LANE_ID": identity, "HOST": host,
                  "KEY": str(self.work / "key"), "KNOWN_HOSTS": str(self.work / "known_hosts")}
        self.config.write_text("\n".join(k + "=" + shlex.quote(v) for k, v in values.items()) + "\n")

    def run_guard(self, **extra):
        env = {**os.environ, "PATH": str(self.bin) + os.pathsep + os.environ["PATH"], "LANE_CONFIG": str(self.config),
               "LANE_TEST_ROOT": str(self.work), "LANE_TEST_ID": IDENTITY, **extra}
        return subprocess.run(["bash", "-c", '. "$1/common.sh"; echo PROOF_CAN_START', "test", str(HERE)],
                              env=env, capture_output=True, text=True)

    def refused(self, **extra):
        result = self.run_guard(**extra)
        self.assertNotEqual(result.returncode, 0, result.stdout)
        self.assertNotIn("PROOF_CAN_START", result.stdout)
        return result

    def test_exact_disposable_identity_with_empty_registry_is_accepted(self):
        result = self.run_guard()
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertIn("PROOF_CAN_START", result.stdout)

    def test_own_fixture_apps_are_accepted(self):
        (self.work / "registry.json").write_text('{"apps":[{"name":"hello"},{"name":"notes"}]}')
        self.assertEqual(self.run_guard().returncode, 0)

    def test_empty_registry_null_matches_the_cli_contract(self):
        (self.work / "registry.json").write_text('{"apps":null}')
        self.assertEqual(self.run_guard().returncode, 0)

    def test_unmanaged_foreign_container_is_refused(self):
        (self.work / "registry.json").write_text('{"apps":null,"unmanaged":["production-database"]}')
        self.refused()

    def test_missing_apps_field_is_refused(self):
        (self.work / "registry.json").write_text('{}')
        self.refused()

    def test_production_marker_cannot_be_overridden(self):
        (self.work / "bedrock-production").touch()
        self.refused(BEDROCK_LANE_DESTROY="127.0.0.1")

    def test_foreign_app_cannot_be_overridden(self):
        (self.work / "registry.json").write_text('{"apps":[{"name":"customer-production"}]}')
        self.refused(BEDROCK_LANE_DESTROY="127.0.0.1")

    def test_unreachable_target_is_not_treated_as_empty(self):
        self.refused(LANE_TEST_UNREACHABLE="true")

    def test_unreadable_registry_is_not_treated_as_empty(self):
        (self.work / "registry.json").write_text('not json')
        self.refused()

    def test_missing_identity_is_refused(self):
        self.marker.unlink()
        self.refused()

    def test_mismatched_identity_is_refused(self):
        self.marker.write_text("a" * 32)
        self.refused()

    def test_identity_owned_by_another_user_is_refused(self):
        self.refused(LANE_TEST_OWNER="501")

    def test_group_writable_identity_is_refused(self):
        self.marker.chmod(0o660)
        self.refused()

    def test_symlink_identity_is_refused(self):
        target = self.work / "identity-target"
        self.marker.rename(target)
        self.marker.symlink_to(target)
        self.refused()

    def test_explicit_disposable_opt_in_is_required(self):
        self.write_config(disposable="false")
        self.refused()

    def test_host_shell_syntax_is_refused_before_ssh(self):
        self.write_config(host="example.com; touch bad")
        self.refused()

    def test_invalid_identity_is_refused_before_ssh(self):
        self.write_config(identity="invalid")
        self.refused()


if __name__ == "__main__":
    unittest.main()
