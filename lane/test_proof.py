#!/usr/bin/env python3
"""Proof reports distinguish missing infrastructure from a successful run."""
import importlib.util
import json
from pathlib import Path
import subprocess
import tempfile
from types import SimpleNamespace
import unittest

HERE = Path(__file__).resolve().parent
SPEC = importlib.util.spec_from_file_location("lane_proof", HERE / "proof.py")
PROOF = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(PROOF)


class ProofReports(unittest.TestCase):
    def test_missing_target_configuration_writes_a_blocked_unrun_report(self):
        with tempfile.TemporaryDirectory(prefix="bedrock-proof-test-") as directory:
            artifacts = Path(directory) / "artifacts"
            result = subprocess.run([str(HERE / "prove.sh"), "--config", str(Path(directory) / "missing.env"),
                                     "--artifacts", str(artifacts), "m1"], text=True, capture_output=True)
            self.assertEqual(result.returncode, 2, result.stdout + result.stderr)
            report = json.loads((artifacts / "report.json").read_text())
            self.assertEqual(report["status"], "blocked")
            self.assertEqual(report["results"], [{"proof": "m1", "status": "not run"}])
            self.assertEqual(len(report["commit"]), 40)
            self.assertTrue(report["problems"])
            self.assertEqual(artifacts.stat().st_mode & 0o777, 0o700)

    def test_credentials_are_masked_in_legacy_transcripts(self):
        values = "token=throwaway-token\npassword: throwaway-password\nAGE-SECRET-KEY-1ABCDEF\n  " + "a" * 48 + "\n"
        text = PROOF.redact(values)
        for secret in ("throwaway-token", "throwaway-password", "AGE-SECRET-KEY-1ABCDEF", "a" * 48):
            self.assertNotIn(secret, text)
        self.assertIn("redacted", text)

    def test_real_cloudflare_requires_explicit_mutation_opt_in(self):
        args = SimpleNamespace(proofs=["cloudflare"], real_cloudflare=False)
        settings = {"LANE_DOMAIN": "lane.example.com", "LANE_ZONE": "example.com"}
        self.assertTrue(any("--real-cloudflare" in item for item in PROOF.prerequisites(args, settings)))

    def test_private_restore_sources_with_env_files_are_refused(self):
        with tempfile.TemporaryDirectory(prefix="bedrock-stage-test-") as directory:
            stage = Path(directory)
            for filename in ("src/begamin/Dockerfile", "src/dragon-writer/Dockerfile", "archives/begamin-data.tar.gz",
                             "archives/dragon_writer_fixture.dump", "archives/uploads_fixture.tar.gz", "src/begamin/.env"):
                path = stage / filename
                path.parent.mkdir(parents=True, exist_ok=True)
                path.touch()
            args = SimpleNamespace(proofs=["m4"], staging=str(stage))
            settings = {"LANE_DOMAIN": "lane.example.com", "LANE_ZONE": "example.com", "LANE_SITE_URL": "https://example.com", "LANE_DW_CHAPTERS": "1"}
            self.assertTrue(any(".env" in item for item in PROOF.prerequisites(args, settings)))


if __name__ == "__main__":
    unittest.main()
