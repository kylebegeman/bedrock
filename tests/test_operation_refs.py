from __future__ import annotations

import json
import sys
import tempfile
import unittest
from pathlib import Path


sys.path.insert(0, str(Path(__file__).resolve().parents[1] / "src"))

from ophelia.operation_refs import resolve_receipt_ref, resolve_workflow_ref
from ophelia.workflows import plan_workflow


class OperationReferenceTests(unittest.TestCase):
    def test_receipt_latest_prefix_and_path_resolution(self) -> None:
        with tempfile.TemporaryDirectory() as temp_dir:
            runtime_root = Path(temp_dir)
            first = _write_receipt(
                runtime_root,
                "demo-service",
                "app.export.create.demo-service.production.aaa111",
                "app.export.create",
                "2026-06-20T10:00:00Z",
            )
            second = _write_receipt(
                runtime_root,
                "demo-service",
                "app.traffic.apply.demo-service.production.bbb222",
                "app.traffic.apply",
                "2026-06-21T10:00:00Z",
            )

            latest = resolve_receipt_ref("latest", runtime_root=runtime_root, app="demo-service")
            self.assertTrue(latest["ok"])
            self.assertEqual("app.traffic.apply.demo-service.production.bbb222", latest["resolved_id"])

            latest_operation = resolve_receipt_ref("latest:app.export.create", runtime_root=runtime_root)
            self.assertTrue(latest_operation["ok"])
            self.assertEqual("app.export.create.demo-service.production.aaa111", latest_operation["resolved_id"])

            prefix = resolve_receipt_ref("app.traffic.apply", runtime_root=runtime_root)
            self.assertTrue(prefix["ok"])
            self.assertEqual(str(second), prefix["path"])

            path = resolve_receipt_ref(str(first), runtime_root=runtime_root)
            self.assertTrue(path["ok"])
            self.assertEqual("path", path["strategy"])
            self.assertEqual("app.export.create.demo-service.production.aaa111", path["resolved_id"])

    def test_receipt_prefix_ambiguity_reports_candidates(self) -> None:
        with tempfile.TemporaryDirectory() as temp_dir:
            runtime_root = Path(temp_dir)
            _write_receipt(runtime_root, "demo-service", "shared-prefix-alpha", "deploy.apply", "2026-06-20T10:00:00Z")
            _write_receipt(runtime_root, "demo-service", "shared-prefix-beta", "deploy.apply", "2026-06-20T11:00:00Z")

            resolution = resolve_receipt_ref("shared-prefix", runtime_root=runtime_root)

            self.assertFalse(resolution["ok"])
            codes = {blocker["code"] for blocker in resolution["blockers"]}
            self.assertIn("operation_ref_ambiguous", codes)
            self.assertEqual(2, len(resolution["candidates"]))

    def test_receipt_resolves_a_legacy_alias_before_guessing_a_prefix(self) -> None:
        """An operator holding an identifier an earlier release wrote still resolves.

        Discovery records those identifiers as aliases. An exact alias is
        stronger evidence than a prefix guess, so it wins.
        """

        with tempfile.TemporaryDirectory() as temp_dir:
            runtime_root = Path(temp_dir)
            _write_receipt(
                runtime_root,
                "demo-service",
                "verify-modern-id",
                "backup.verify.apply",
                "2026-06-20T10:00:00Z",
                verify_id="verify-legacy-id",
            )
            _write_receipt(
                runtime_root,
                "demo-service",
                "verify-legacy-id-but-different-receipt",
                "backup.verify.apply",
                "2026-06-20T09:00:00Z",
            )

            resolution = resolve_receipt_ref("verify-legacy-id", runtime_root=runtime_root)

            self.assertTrue(resolution["ok"], resolution.get("blockers"))
            self.assertEqual("alias", resolution["strategy"])
            self.assertEqual("verify-modern-id", resolution["resolved_id"])

    def test_receipt_alias_ambiguity_reports_candidates(self) -> None:
        with tempfile.TemporaryDirectory() as temp_dir:
            runtime_root = Path(temp_dir)
            for receipt_id in ("verify-one", "verify-two"):
                _write_receipt(
                    runtime_root,
                    "demo-service",
                    receipt_id,
                    "backup.verify.apply",
                    "2026-06-20T10:00:00Z",
                    verify_id="verify-shared-alias",
                )

            resolution = resolve_receipt_ref(
                "verify-shared-alias", runtime_root=runtime_root
            )

            self.assertFalse(resolution["ok"])
            codes = {blocker["code"] for blocker in resolution["blockers"]}
            self.assertIn("operation_ref_ambiguous", codes)
            self.assertEqual(2, len(resolution["candidates"]))

    def test_workflow_latest_and_prefix_resolution(self) -> None:
        with tempfile.TemporaryDirectory() as temp_dir:
            runtime_root = Path(temp_dir)
            first = plan_workflow("move-app", app="first-app", runtime_root=runtime_root)
            second = plan_workflow("move-app", app="second-app", runtime_root=runtime_root)

            latest_app = resolve_workflow_ref("latest:second-app", runtime_root=runtime_root)
            self.assertTrue(latest_app["ok"])
            self.assertEqual(second["workflow_id"], latest_app["resolved_id"])

            prefix = resolve_workflow_ref(str(first["workflow_id"])[:24], runtime_root=runtime_root)
            self.assertTrue(prefix["ok"])
            self.assertEqual(first["workflow_id"], prefix["resolved_id"])


def _write_receipt(
    runtime_root: Path,
    app: str,
    receipt_id: str,
    operation: str,
    started_at: str,
    *,
    verify_id: str | None = None,
) -> Path:
    path = runtime_root / "apps" / app / "receipts" / f"{receipt_id}.json"
    path.parent.mkdir(parents=True, exist_ok=True)
    payload = {
        "schema_version": 1,
        "kind": "ophelia.receipt",
        "operation": operation,
        "operation_id": receipt_id,
        "status": "succeeded",
        "app": app,
        "environment": "production",
        "started_at": started_at,
        "completed_at": started_at,
        "inputs_redacted": True,
    }
    if verify_id is not None:
        payload["verify_id"] = verify_id
    path.write_text(json.dumps(payload) + "\n")
    return path


if __name__ == "__main__":
    unittest.main()
