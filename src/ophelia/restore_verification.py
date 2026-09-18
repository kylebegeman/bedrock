"""Backup and restore verification platform (Phase 12).

This module turns a recorded backup into evidence that it could actually be
restored, without ever touching production data.

Two surfaces are exposed:

- :func:`backup_verify_plan` is **read-only**. It selects a backup (the latest
  by default, or a named one), reports its freshness, the data contracts it is
  expected to cover, the *isolated* rehearsal target it would use, the typed
  verification checks it would run, and a confirmation token derived from the
  canonical apply input. It writes nothing.
- :func:`backup_verify_apply` recomputes and validates that token, refuses any
  rehearsal target that resolves into a production app directory / the repo /
  outside the rehearsals area, runs file/archive/metadata-level checks plus an
  optional bounded isolated rehearsal step, and writes a *receipt* under
  ``<runtime_root>/apps/<app>/restore-drills/``. It never overwrites production
  data and never deletes anything (cleanup is a separate future plan).

Secret *values* from a backup are never read or printed. Only names, paths, and
booleans are surfaced, and every payload is swept with :func:`deep_redact`.
"""

from __future__ import annotations

import tarfile
from datetime import datetime, timezone
from pathlib import Path
from typing import Dict, List, Mapping, Optional

from .config import DEFAULT_RUNTIME_ROOT, REPO_ROOT
from .manifest import Manifest
from .operation_schema import (
    artifact,
    error_envelope,
    issue as schema_issue,
    plan_envelope,
    receipt_envelope,
    report_envelope,
    token,
)
from .portability import (
    _backup_records,
    _backup_threshold_hours,
    _freshness_status,
    _read_json,
    _utc_now,
    resolve_app_manifest,
)
from .redaction import deep_redact, redact_command_string
from .receipt_sources import receipt_payload, receipt_records

VERIFY_PLAN_OPERATION = "backup.verify.plan"
VERIFY_APPLY_OPERATION = "backup.verify.apply"

# Where isolated rehearsals are allowed to write. Anything resolving outside
# this subtree (or into a production app dir / the repo) is refused.
REHEARSALS_DIRNAME = "rehearsals"


def is_restore_drill_receipt_operation(operation: str) -> bool:
    return (
        operation in {VERIFY_APPLY_OPERATION, "backup.rehearse.apply"}
        or "restore-drill" in operation
        or "restore_drill" in operation
    )


def backup_verify_plan(
    app: str,
    environment: Optional[str] = None,
    runtime_root: Path = DEFAULT_RUNTIME_ROOT,
    backup_id: Optional[str] = None,
    manifest_path: Optional[Path] = None,
) -> Dict[str, object]:
    """Read-only plan for verifying a backup. Writes nothing."""
    resolution = resolve_app_manifest(app, environment, manifest_path=manifest_path)
    manifest = resolution.manifest
    warnings: List[Dict[str, str]] = [schema_issue("manifest_warning", message) for message in resolution.warnings]
    blockers: List[Dict[str, str]] = [schema_issue("manifest_unresolved", message) for message in resolution.blockers]
    resolved_environment = environment or (manifest.environment if manifest else None) or "unknown"

    backups_root = runtime_root / "backups" / "apps" / app
    backups = _backup_records(backups_root)
    selected = _select_backup(backups, backup_id)

    if backup_id is not None and selected is None:
        blockers.append(
            schema_issue("backup_not_found", f"No backup `{backup_id}` found for {app}.", str(backups_root))
        )
    elif selected is None:
        blockers.append(schema_issue("backup_missing", f"No backup exists for {app}.", str(backups_root)))

    critical = bool(manifest and manifest.pack.portability == "critical")
    data_contracts = _expected_data_contracts(manifest)
    freshness = _freshness_status(backups[-1] if backups else None, _backup_threshold_hours(manifest))

    selected_id = str(selected["backup_id"]) if selected else None
    selected_path = str(selected["path"]) if selected else None
    rehearsal_target = (
        _rehearsal_target(runtime_root, app, selected_id) if selected_id else None
    )
    backup_digest = _backup_digest(Path(selected_path)) if selected_path else None

    checks = [
        {"name": "backup_selected", "ok": selected is not None, "message": selected_id or "none"},
        {
            "name": "backup_freshness",
            "ok": freshness.get("status") in {"fresh", "unknown"},
            "message": str(freshness.get("status", "unknown")),
        },
        {
            "name": "rehearsal_target_isolated",
            "ok": bool(rehearsal_target) and _is_safe_rehearsal_target(Path(rehearsal_target), runtime_root, app),
            "message": rehearsal_target or "n/a",
        },
    ]

    verification_commands = _planned_verification_commands(selected_path, manifest, rehearsal_target)

    exact_apply_input: Dict[str, object] = {
        "app": app,
        "environment": resolved_environment,
        "backup_id": selected_id,
        "backup_digest": backup_digest,
        "rehearsal_target": rehearsal_target,
    }
    confirmation_token = token(VERIFY_APPLY_OPERATION, exact_apply_input) if selected is not None else None

    plan = plan_envelope(
        VERIFY_PLAN_OPERATION,
        app,
        resolved_environment,
        f"Backup verify plan for {app}: "
        + (f"backup {selected_id}." if selected_id else "no backup available."),
        blockers=blockers,
        warnings=warnings,
        checks=checks,
        artifacts=[
            artifact(selected_path, "backup", f"Selected backup {selected_id}", present=True)
        ]
        if selected_path
        else [],
        confirmation_required=True,
        confirmation_token=confirmation_token,
        exact_apply_input=exact_apply_input if selected is not None else None,
        risk="medium",
        read_only=True,
        selected_backup={
            "backup_id": selected_id,
            "path": selected_path,
            "created_at": selected.get("created_at") if selected else None,
            "coverage": deep_redact(selected.get("coverage", {}), propagate=True) if selected else {},
            "database": deep_redact(selected.get("database", {}), propagate=True) if selected else {},
            "digest": backup_digest,
        },
        freshness=freshness,
        expected_data_contracts=data_contracts,
        rehearsal_target=rehearsal_target,
        resources_required=_resources_required(selected_path),
        verification_commands=verification_commands,
        cleanup_note=(
            "Cleanup of rehearsal artifacts is a separate future plan; this command "
            "never deletes anything implicitly."
        ),
        pack_portability=manifest.pack.portability if manifest else None,
        missing_checksum_is_blocking=critical,
        secrets_redacted=True,
        apply_command=(
            f"ship backup verify apply {app} --environment {resolved_environment} "
            + (f"--backup-id {selected_id} " if selected_id else "")
            + f"--confirm {confirmation_token}"
            if confirmation_token
            else None
        ),
    )
    # ``confirmation_token`` is a non-secret SHA digest of the canonical apply
    # input; it must survive redaction or the apply gate cannot match it.
    return deep_redact(plan, safe_keys=("secrets_redacted", "confirmation_token"), propagate=True)


def backup_verify_apply(
    app: str,
    environment: Optional[str] = None,
    runtime_root: Path = DEFAULT_RUNTIME_ROOT,
    backup_id: Optional[str] = None,
    confirm: Optional[str] = None,
    manifest_path: Optional[Path] = None,
) -> Dict[str, object]:
    """Token-gated, rehearsal-isolated backup verification. Never mutates production."""
    started_at = _utc_now()
    plan = backup_verify_plan(app, environment, runtime_root, backup_id, manifest_path)
    resolved_environment = str(plan.get("environment") or environment or "unknown")
    plan_blockers = plan.get("blockers") if isinstance(plan.get("blockers"), list) else []
    expected = plan.get("confirmation_token")

    # Token validation first: reject mismatch/empty before doing any work.
    if not isinstance(confirm, str) or not confirm:
        return error_envelope(
            "Backup verify apply requires a confirmation token from backup verify plan.",
            "confirmation_token_missing",
            warnings=plan.get("warnings", []),
            operation=VERIFY_APPLY_OPERATION,
            app=app,
            environment=resolved_environment,
        )
    if not isinstance(expected, str) or confirm != expected:
        return error_envelope(
            "Backup verify apply confirmation token does not match the current plan.",
            "confirmation_token_mismatch",
            warnings=plan.get("warnings", []),
            operation=VERIFY_APPLY_OPERATION,
            app=app,
            environment=resolved_environment,
        )
    if plan_blockers:
        return error_envelope(
            "Backup verify apply is blocked by the current plan.",
            "plan_blocked",
            blockers=list(plan_blockers),
            warnings=plan.get("warnings", []),
            operation=VERIFY_APPLY_OPERATION,
            app=app,
            environment=resolved_environment,
        )

    selected = plan.get("selected_backup") if isinstance(plan.get("selected_backup"), dict) else {}
    selected_id = selected.get("backup_id")
    backup_path = Path(str(selected.get("path"))) if selected.get("path") else None
    rehearsal_target_raw = plan.get("rehearsal_target")
    rehearsal_target = Path(str(rehearsal_target_raw)) if rehearsal_target_raw else None

    # Safety gate: refuse any target that resolves into production / repo / outside rehearsals.
    if rehearsal_target is None or not _is_safe_rehearsal_target(rehearsal_target, runtime_root, app):
        return error_envelope(
            "Refusing to run verification: the rehearsal target is not an isolated rehearsals path.",
            "unsafe_rehearsal_target",
            blockers=[
                schema_issue(
                    "unsafe_rehearsal_target",
                    "Rehearsal target must resolve under "
                    f"{runtime_root / 'rehearsals'} and never into a production app dir or the repo.",
                    str(rehearsal_target) if rehearsal_target else "none",
                )
            ],
            warnings=plan.get("warnings", []),
            operation=VERIFY_APPLY_OPERATION,
            app=app,
            environment=resolved_environment,
        )

    manifest = _resolve_manifest_for_apply(app, environment, manifest_path)
    critical = bool(manifest and manifest.pack.portability == "critical")

    verify_blockers: List[Dict[str, str]] = []
    verify_warnings: List[Dict[str, str]] = list(plan.get("warnings", []))
    verification_checks = _run_verification_checks(
        backup_path,
        app,
        resolved_environment,
        manifest,
        critical,
        rehearsal_target,
        verify_blockers,
        verify_warnings,
    )

    status = "succeeded" if all(bool(check.get("ok")) for check in verification_checks) and not verify_blockers else "failed"

    verify_id = _verify_id(app)
    receipt_dir = runtime_root / "apps" / app / "restore-drills"
    receipt_path = receipt_dir / f"{verify_id}.json"

    # The rehearsal target directory (isolated) is created so artifacts have a home,
    # but no production path is ever written and nothing is ever deleted.
    rehearsal_target.mkdir(parents=True, exist_ok=True)

    receipt = receipt_envelope(
        VERIFY_APPLY_OPERATION,
        app,
        resolved_environment,
        status,
        started_at,
        _utc_now(),
        artifacts=[
            artifact(str(receipt_path), "restore-verification-receipt", present=True),
            artifact(str(rehearsal_target), "rehearsal-target", "Isolated rehearsal area", present=True),
            artifact(str(backup_path), "backup", f"Verified backup {selected_id}", present=bool(backup_path and backup_path.exists())),
        ],
        checks=verification_checks,
        rollback={
            "available": False,
            "note": "No source runtime state was changed.",
        },
        plan_operation_id=plan.get("operation_id") if isinstance(plan.get("operation_id"), str) else None,
        blockers=verify_blockers,
        warnings=verify_warnings,
        verify_id=verify_id,
        backup_id=selected_id,
        backup_path=str(backup_path) if backup_path else None,
        rehearsal_target=str(rehearsal_target),
        pack_portability=manifest.pack.portability if manifest else None,
        missing_checksum_is_blocking=critical,
        production_data_modified=False,
        deleted_anything=False,
        secrets_redacted=True,
        inputs_redacted=True,
    )
    receipt = deep_redact(
        receipt,
        safe_keys=("secrets_redacted", "inputs_redacted", "production_data_modified", "deleted_anything"),
        propagate=True,
    )
    receipt_dir.mkdir(parents=True, exist_ok=True)
    receipt_path.write_text(_dumps(receipt))
    return receipt


def restore_drills_list(
    app: str,
    environment: Optional[str] = None,
    runtime_root: Path = DEFAULT_RUNTIME_ROOT,
) -> Dict[str, object]:
    """List restore-drill and backup-verification receipts for an app (read-only)."""
    drills = _verification_and_drill_records(runtime_root, app, environment)
    return report_envelope(
        "restore.drills.list",
        app,
        environment,
        f"Found {len(drills)} restore drill / verification receipt(s) for {app}.",
        checks=[{"name": "restore_drill_scan", "ok": True, "message": str(runtime_root)}],
        kind="ophelia.restore_drills",
        drills=drills,
        secrets_redacted=True,
    )


def restore_drills_show(
    drill_id: str,
    runtime_root: Path = DEFAULT_RUNTIME_ROOT,
    *,
    resolved_payload: Optional[Dict[str, object]] = None,
    receipt_locator: Optional[str] = None,
) -> Dict[str, object]:
    """Show one restore-drill / verification receipt by id (read-only)."""
    if isinstance(resolved_payload, dict):
        return _restore_drill_show_report(
            resolved_payload,
            _restore_drill_display_id(resolved_payload, drill_id),
            requested_ref=drill_id,
            receipt_locator=receipt_locator,
        )
    drill_path = Path(str(drill_id)).expanduser()
    if drill_path.exists() and drill_path.is_file():
        payload = receipt_payload(drill_path)
        if payload:
            resolved_id = _restore_drill_display_id(
                payload,
                str(
                    payload.get("receipt_id")
                    or payload.get("operation_id")
                    or drill_path.stem
                ),
            )
            return _restore_drill_show_report(
                payload,
                resolved_id,
                requested_ref=drill_id,
                receipt_locator=str(drill_path),
            )
        return report_envelope(
            "restore.drills.show",
            None,
            None,
            f"Restore drill / verification receipt path is unreadable: {drill_id}.",
            blockers=[schema_issue("restore_drill_unreadable", f"Could not read restore drill JSON: {drill_path}")],
            kind="ophelia.restore_drill",
            drill_id=drill_id,
        )
    for record in receipt_records(runtime_root):
        record_aliases = record.get("aliases")
        aliases = (
            {value for value in record_aliases if isinstance(value, str) and value}
            if isinstance(record_aliases, (list, tuple, set))
            else set()
        )
        # A receipt also answers to the identifiers earlier releases wrote for
        # it, so a legacy verification id still resolves.
        if str(record.get("receipt_id") or "") != drill_id and drill_id not in aliases:
            continue
        payload = receipt_payload(record)
        if isinstance(payload, dict):
            return _restore_drill_show_report(
                payload,
                _restore_drill_display_id(payload, drill_id),
                requested_ref=drill_id,
                receipt_locator=str(record.get("path") or ""),
                fallback_app=(
                    record.get("app")
                    if isinstance(record.get("app"), str)
                    else None
                ),
            )
    return report_envelope(
        "restore.drills.show",
        None,
        None,
        f"Restore drill / verification receipt not found: {drill_id}.",
        blockers=[schema_issue("restore_drill_not_found", f"Restore drill not found: {drill_id}")],
        kind="ophelia.restore_drill",
        drill_id=drill_id,
    )


def _restore_drill_display_id(payload: Mapping[str, object], fallback: str) -> str:
    """The identifier that names a drill or verification receipt.

    A verification receipt is named by its ``verify_id`` and a drill by its
    ``restore_drill_id``. Resolution may arrive by a different route, such as a
    kernel receipt id or a ``latest:`` alias, so the caller's id is the
    fallback. Every lookup path reports the same name for the same receipt.
    """

    nested = payload.get("ophelia_receipt")
    terminal = nested if isinstance(nested, dict) else payload
    for key in ("verify_id", "restore_drill_id"):
        for candidate in (payload, terminal):
            value = candidate.get(key) if hasattr(candidate, "get") else None
            if isinstance(value, str) and value:
                return value
    return fallback


def _restore_drill_show_report(
    payload: Dict[str, object],
    drill_id: str,
    *,
    requested_ref: str,
    receipt_locator: Optional[str],
    fallback_app: Optional[str] = None,
) -> Dict[str, object]:
    nested = payload.get("ophelia_receipt")
    terminal = nested if isinstance(nested, dict) else payload
    operation = str(payload.get("operation") or terminal.get("operation") or "")
    if not is_restore_drill_receipt_operation(operation):
        return report_envelope(
            "restore.drills.show",
            None,
            None,
            f"Receipt {drill_id} is not restore-drill or backup-verification evidence.",
            blockers=[
                schema_issue(
                    "restore_drill_operation_invalid",
                    f"Receipt operation '{operation or 'unknown'}' is not valid restore-drill evidence.",
                )
            ],
            kind="ophelia.restore_drill",
            drill_id=drill_id,
            requested_ref=requested_ref,
            receipt_locator=receipt_locator,
        )
    artifacts = (
        [artifact(receipt_locator, "restore-drill", present=True)]
        if receipt_locator
        else []
    )
    app_value = payload.get("app") or terminal.get("app")
    environment_value = payload.get("environment") or terminal.get("environment")
    return report_envelope(
        "restore.drills.show",
        app_value if isinstance(app_value, str) else fallback_app,
        environment_value if isinstance(environment_value, str) else None,
        f"Restore drill / verification receipt {drill_id}.",
        artifacts=artifacts,
        kind="ophelia.restore_drill",
        drill=deep_redact(payload, propagate=True),
        drill_id=drill_id,
        requested_ref=requested_ref,
        receipt_locator=receipt_locator,
        secrets_redacted=True,
    )


# --------------------------------------------------------------------------- #
# Internal helpers
# --------------------------------------------------------------------------- #


def _select_backup(
    backups: List[Dict[str, object]], backup_id: Optional[str]
) -> Optional[Dict[str, object]]:
    if not backups:
        return None
    if backup_id is None:
        return backups[-1]
    for record in backups:
        if str(record.get("backup_id")) == backup_id:
            return record
    return None


def _rehearsal_target(runtime_root: Path, app: str, backup_id: str) -> str:
    return str(runtime_root / REHEARSALS_DIRNAME / app / backup_id)


def _is_safe_rehearsal_target(target: Path, runtime_root: Path, app: str) -> bool:
    """True only when ``target`` resolves under ``<runtime_root>/rehearsals``.

    Refuses anything that resolves into a production app directory
    (``<runtime_root>/apps/...``), the Ophelia repo, or outside the rehearsals
    area entirely. Uses fully resolved paths so symlinks/``..`` cannot escape.
    """
    try:
        resolved = target.resolve()
    except (OSError, RuntimeError):
        return False
    rehearsals_root = (runtime_root / REHEARSALS_DIRNAME).resolve()
    apps_root = (runtime_root / "apps").resolve()
    repo_root = REPO_ROOT.resolve()

    if not _is_within(resolved, rehearsals_root):
        return False
    if _is_within(resolved, apps_root):
        return False
    if _is_within(resolved, repo_root):
        return False
    return True


def _is_within(path: Path, parent: Path) -> bool:
    try:
        path.relative_to(parent)
    except ValueError:
        return False
    return True


def _expected_data_contracts(manifest: Optional[Manifest]) -> Dict[str, object]:
    if manifest is None:
        return {}
    data = manifest.to_lock_dict().get("data", {})
    return data if isinstance(data, dict) else {}


def _resources_required(backup_path: Optional[str]) -> Dict[str, object]:
    size_bytes = None
    if backup_path:
        path = Path(backup_path)
        if path.exists():
            size_bytes = sum(item.stat().st_size for item in path.rglob("*") if item.is_file())
    return {
        "disk_bytes_estimate": size_bytes,
        "tools": ["tarfile (listing only, never extracted over production)"],
        "network": False,
    }


def _planned_verification_commands(
    backup_path: Optional[str],
    manifest: Optional[Manifest],
    rehearsal_target: Optional[str],
) -> List[Dict[str, object]]:
    commands: List[Dict[str, object]] = [
        {"name": "backup_exists", "type": "filesystem", "executed_by_apply": True, "description": "Confirm the backup file/dir exists."},
        {"name": "checksum_match", "type": "checksum", "executed_by_apply": True, "description": "Verify checksums.sha256 if present."},
        {"name": "archive_listing", "type": "tarfile", "executed_by_apply": True, "description": "List archive members without extracting over production."},
        {"name": "metadata_match", "type": "metadata", "executed_by_apply": True, "description": "Confirm backup manifest app matches."},
    ]
    restore_command = _restore_rehearsal_command(manifest, rehearsal_target)
    commands.append(
        {
            "name": "restore_rehearsal",
            "type": "restore-rehearsal",
            "executed_by_apply": False,
            "description": "Restore command is described and isolated; not executed by default.",
            "command": restore_command,
        }
    )
    verify_hook = _data_verify_hook(manifest)
    commands.append(
        {
            "name": "app_verify_hook",
            "type": "app-hook",
            "executed_by_apply": False,
            "description": "App-defined verify hook is recorded; not executed by default.",
            "command": verify_hook,
        }
    )
    return commands


def _restore_rehearsal_command(manifest: Optional[Manifest], rehearsal_target: Optional[str]) -> Optional[str]:
    if manifest is None or manifest.data.postgres is None:
        return None
    import_config = manifest.data.postgres.import_config or {}
    base = import_config.get("command") if isinstance(import_config, dict) else None
    if not base:
        return None
    return redact_command_string(f"{base} --isolated-target {rehearsal_target or 'REHEARSAL_TARGET'}")


def _data_verify_hook(manifest: Optional[Manifest]) -> Optional[str]:
    if manifest is None:
        return None
    commands: List[str] = []
    if manifest.data.postgres is not None:
        verify = manifest.data.postgres.verify or {}
        if isinstance(verify, dict):
            command = verify.get("command")
            if command:
                commands.append(str(command))
    for volume in manifest.data.volumes:
        verify = volume.verify or {}
        if isinstance(verify, dict):
            command = verify.get("command")
            if command:
                commands.append(str(command))
    if not commands:
        return None
    return " && ".join(redact_command_string(command) for command in commands)


def _backup_digest(backup_path: Path) -> Optional[str]:
    """Return the recorded checksum digest if a checksums.sha256 file is present.

    No secret value is read: a checksums manifest contains only file paths and
    hex digests.
    """
    checksums = backup_path / "checksums.sha256"
    if not checksums.exists() or not checksums.is_file():
        return None
    import hashlib

    try:
        return hashlib.sha256(checksums.read_bytes()).hexdigest()[:20]
    except OSError:
        return None


def _run_verification_checks(
    backup_path: Optional[Path],
    app: str,
    environment: str,
    manifest: Optional[Manifest],
    critical: bool,
    rehearsal_target: Path,
    blockers: List[Dict[str, str]],
    warnings: List[Dict[str, str]],
) -> List[Dict[str, object]]:
    checks: List[Dict[str, object]] = []

    # (1) backup file/dir exists.
    exists = bool(backup_path and backup_path.exists())
    checks.append({"name": "backup_exists", "ok": exists, "message": str(backup_path) if backup_path else "none"})
    if not exists:
        blockers.append(schema_issue("backup_missing", "Backup path does not exist.", str(backup_path) if backup_path else "none"))
        return checks

    # (2) checksum match if checksums.sha256 is present.
    checksum_check = _checksum_check(backup_path, critical, blockers, warnings)
    checks.append(checksum_check)

    # (3) archive listing succeeds (list only, never extract over production).
    checks.append(_archive_listing_check(backup_path))

    # (4) metadata matches app.
    checks.append(_metadata_match_check(backup_path, app, environment, warnings))

    # (5) restore-command rehearsal: described, not executed by default.
    restore_command = _restore_rehearsal_command(manifest, str(rehearsal_target))
    checks.append(
        {
            "name": "restore_rehearsal",
            "ok": True,
            "status": "skipped",
            "message": (
                f"Not executed (isolated rehearsal target {rehearsal_target}). "
                + (f"Would run: {restore_command}" if restore_command else "No restore command declared.")
            ),
        }
    )

    # (6) app-defined verify hook: recorded, not executed by default.
    verify_hook = _data_verify_hook(manifest)
    checks.append(
        {
            "name": "app_verify_hook",
            "ok": True,
            "status": "skipped",
            "message": (
                f"Recorded but not executed. Would run: {verify_hook}" if verify_hook else "No app verify hook declared."
            ),
        }
    )
    return checks


def _checksum_check(
    backup_path: Path,
    critical: bool,
    blockers: List[Dict[str, str]],
    warnings: List[Dict[str, str]],
) -> Dict[str, object]:
    import hashlib

    checksums = backup_path / "checksums.sha256"
    if not checksums.exists() or not checksums.is_file():
        message = "No checksums.sha256 present in backup."
        if critical:
            blockers.append(schema_issue("checksum_missing", message + " Required for critical packs.", str(checksums)))
            return {"name": "checksum_match", "ok": False, "message": message + " (critical: blocker)"}
        warnings.append(schema_issue("checksum_missing", message, str(checksums)))
        return {"name": "checksum_match", "ok": True, "status": "skipped", "message": message + " (warning)"}

    mismatches: List[str] = []
    checked = 0
    try:
        for line in checksums.read_text().splitlines():
            line = line.strip()
            if not line:
                continue
            parts = line.split(None, 1)
            if len(parts) != 2:
                continue
            digest, rel = parts
            rel = rel.lstrip("*").strip()
            target = backup_path / rel
            if not target.exists() or not target.is_file():
                mismatches.append(rel)
                continue
            actual = hashlib.sha256(target.read_bytes()).hexdigest()
            if actual != digest:
                mismatches.append(rel)
            checked += 1
    except OSError as exc:
        return {"name": "checksum_match", "ok": False, "message": f"Failed reading checksums: {exc}"}

    ok = not mismatches
    if not ok:
        blockers.append(
            schema_issue(
                "checksum_mismatch",
                f"{len(mismatches)} file(s) failed checksum verification.",
                str(checksums),
            )
        )
    return {
        "name": "checksum_match",
        "ok": ok,
        "message": f"{checked} file(s) verified, {len(mismatches)} mismatch(es).",
    }


def _archive_listing_check(backup_path: Path) -> Dict[str, object]:
    archives = sorted(
        path
        for path in backup_path.rglob("*")
        if path.is_file() and (path.suffix == ".tar" or path.name.endswith((".tar.gz", ".tgz", ".tar.zst")))
    )
    if not archives:
        return {"name": "archive_listing", "ok": True, "status": "skipped", "message": "No archives to list."}
    failures: List[str] = []
    listed = 0
    for archive in archives:
        if archive.name.endswith(".tar.zst"):
            # zstd archives are not opened here to avoid an extraction path; treated as informational.
            continue
        try:
            with tarfile.open(archive) as handle:
                handle.getmembers()
            listed += 1
        except (tarfile.TarError, OSError):
            failures.append(str(archive.relative_to(backup_path)))
    return {
        "name": "archive_listing",
        "ok": not failures,
        "message": f"{listed} archive(s) listed, {len(failures)} unreadable.",
    }


def _metadata_match_check(
    backup_path: Path,
    app: str,
    environment: str,
    warnings: List[Dict[str, str]],
) -> Dict[str, object]:
    manifest = _read_json(backup_path / "backup-manifest.json")
    if not manifest:
        warnings.append(schema_issue("backup_manifest_missing", "Backup manifest not found.", str(backup_path / "backup-manifest.json")))
        return {"name": "metadata_match", "ok": False, "message": "backup-manifest.json missing."}
    recorded_app = manifest.get("app")
    app_ok = recorded_app == app
    if not app_ok:
        warnings.append(
            schema_issue("backup_app_mismatch", f"Backup manifest app `{recorded_app}` does not match `{app}`.", "app")
        )
    return {
        "name": "metadata_match",
        "ok": app_ok,
        "message": f"manifest app={recorded_app} (expected {app}); environment requested={environment}.",
    }


def _verify_id(app: str) -> str:
    import hashlib

    stamp = datetime.now(timezone.utc).strftime("%Y%m%dT%H%M%S%fZ")
    suffix = hashlib.sha256(f"{app}:{stamp}".encode("utf-8")).hexdigest()[:8]
    return f"verify-{stamp}-{suffix}"


def _resolve_manifest_for_apply(
    app: str, environment: Optional[str], manifest_path: Optional[Path]
) -> Optional[Manifest]:
    resolution = resolve_app_manifest(app, environment, manifest_path=manifest_path)
    return resolution.manifest


def _verification_and_drill_records(
    runtime_root: Path, app: str, environment: Optional[str]
) -> List[Dict[str, object]]:
    """Combine drill and verification records across every receipt backend."""
    records: List[Dict[str, object]] = []
    seen: set = set()
    for record in receipt_records(runtime_root, app=app):
        operation = str(record.get("operation") or "")
        if not is_restore_drill_receipt_operation(operation):
            continue
        if environment and record.get("environment") not in (None, environment):
            continue
        receipt_id = str(record.get("receipt_id") or "")
        if not receipt_id or receipt_id in seen:
            continue
        payload = receipt_payload(record) or {}
        nested = payload.get("ophelia_receipt")
        terminal = nested if isinstance(nested, dict) else payload
        seen.add(receipt_id)
        records.append(
            {
                "receipt_id": receipt_id,
                "operation": operation,
                "status": record.get("status"),
                "backup_id": payload.get("backup_id") or terminal.get("backup_id"),
                "path": str(record.get("path") or ""),
                "kind": (
                    "backup-verification"
                    if operation == VERIFY_APPLY_OPERATION
                    else "restore-drill"
                ),
                "source": record.get("source"),
            }
        )
    return records


def _dumps(payload: object) -> str:
    import json

    return json.dumps(payload, indent=2, sort_keys=True) + "\n"
