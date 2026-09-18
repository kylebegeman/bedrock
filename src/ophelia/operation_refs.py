from __future__ import annotations

import json
from pathlib import Path
from typing import Any, Callable, Dict, List, Optional, Set, Tuple

from .config import DEFAULT_RUNTIME_ROOT
from .operation_schema import issue
from .receipt_index import receipt_timeline
from .receipt_sources import receipt_payload
from .redaction import deep_redact


def resolve_receipt_ref(
    reference: str,
    *,
    runtime_root: Path = DEFAULT_RUNTIME_ROOT,
    app: Optional[str] = None,
    environment: Optional[str] = None,
    operation: Optional[str] = None,
    status: Optional[str] = None,
    operation_filter: Optional[Callable[[str], bool]] = None,
) -> Dict[str, Any]:
    """Resolve a receipt reference to one deterministic receipt record.

    Supported references:
    - exact receipt id
    - unambiguous receipt-id prefix
    - path to a receipt JSON file
    - ``latest``
    - ``latest:<app>`` or ``latest:<operation>``
    """
    timeline = receipt_timeline(
        runtime_root,
        environment=environment,
        status=status,
    )
    records = timeline.get("receipts") if isinstance(timeline.get("receipts"), list) else []
    if operation_filter is not None:
        records = [
            record
            for record in records
            if isinstance(record, dict)
            and operation_filter(str(record.get("operation") or ""))
        ]
    resolution = _resolve_ref(
        reference,
        target="receipt",
        records_loader=lambda: [record for record in records if isinstance(record, dict)],
        id_key="receipt_id",
        app=app,
        environment=environment,
        operation=operation,
        status=status,
    )
    if resolution.get("ok") and operation_filter is not None:
        record = resolution.get("record") if isinstance(resolution.get("record"), dict) else {}
        resolved_operation = str(record.get("operation") or "")
        if not operation_filter(resolved_operation):
            resolution["ok"] = False
            resolution["payload"] = None
            resolution["blockers"] = [
                *(
                    resolution.get("blockers", [])
                    if isinstance(resolution.get("blockers"), list)
                    else []
                ),
                issue(
                    "operation_ref_operation_mismatch",
                    f"Reference operation '{resolved_operation or 'unknown'}' is not allowed for this command.",
                ),
            ]
    timeline_warnings = (
        timeline.get("warnings") if isinstance(timeline.get("warnings"), list) else []
    )
    resolution["warnings"] = _dedupe_issues(
        [
            *(
                resolution.get("warnings")
                if isinstance(resolution.get("warnings"), list)
                else []
            ),
            *(item for item in timeline_warnings if isinstance(item, dict)),
        ]
    )
    return resolution


def resolve_workflow_ref(
    reference: str,
    *,
    runtime_root: Path = DEFAULT_RUNTIME_ROOT,
    app: Optional[str] = None,
    environment: Optional[str] = None,
    operation: Optional[str] = None,
) -> Dict[str, Any]:
    """Resolve a stored workflow graph reference.

    Mirrors :func:`resolve_receipt_ref` for workflow graph artifacts under
    ``<runtime_root>/workflows``.
    """
    return _resolve_ref(
        reference,
        target="workflow",
        records_loader=lambda: _workflow_records(runtime_root),
        id_key="workflow_id",
        app=app,
        environment=environment,
        operation=operation,
        status=None,
    )


def public_resolution(resolution: Dict[str, Any]) -> Dict[str, Any]:
    """Return a redacted, JSON-safe summary of a resolver result."""
    record = resolution.get("record") if isinstance(resolution.get("record"), dict) else {}
    return deep_redact(
        {
            "requested": resolution.get("requested"),
            "target": resolution.get("target"),
            "strategy": resolution.get("strategy"),
            "resolved_id": resolution.get("resolved_id"),
            "path": resolution.get("path"),
            "record": {
                "id": record.get("receipt_id") or record.get("workflow_id"),
                "operation": record.get("operation"),
                "app": record.get("app"),
                "environment": record.get("environment"),
                "status": record.get("status"),
                "path": record.get("path"),
            },
            "candidates": resolution.get("candidates", []),
        },
        propagate=True,
    )


def _resolve_ref(
    reference: str,
    *,
    target: str,
    records_loader: Any,
    id_key: str,
    app: Optional[str],
    environment: Optional[str],
    operation: Optional[str],
    status: Optional[str],
) -> Dict[str, Any]:
    requested = str(reference or "").strip()
    base = _base_resolution(requested, target)
    if not requested:
        base["blockers"].append(issue("operation_ref_missing", f"{target.capitalize()} reference is required."))
        return base

    path_resolution = _resolve_path_reference(requested, target, id_key, app, environment, operation, status)
    if path_resolution is not None:
        return path_resolution

    records = records_loader()
    if requested == "latest" or requested.startswith("latest:"):
        alias_app, alias_operation, scope_blockers = _latest_scope(requested, records)
        if scope_blockers:
            base["blockers"].extend(scope_blockers)
            return base
        scoped = _filter_records(
            records,
            app=_combine_scope("app", app, alias_app, base["blockers"]),
            environment=environment,
            operation=_combine_scope("operation", operation, alias_operation, base["blockers"]),
            status=status,
        )
        if base["blockers"]:
            return base
        if not scoped:
            base["strategy"] = "latest"
            base["blockers"].append(issue("operation_ref_not_found", f"No {target} matched reference '{requested}'."))
            return base
        return _resolved(base, scoped[0], id_key, strategy="latest")

    scoped_records = _filter_records(records, app=app, environment=environment, operation=operation, status=status)
    exact = [record for record in scoped_records if str(record.get(id_key) or "") == requested]
    if len(exact) == 1:
        return _resolved(base, exact[0], id_key, strategy="exact")

    # A receipt carries the identifiers earlier releases used for the same
    # evidence, such as a legacy `verify_id`. An operator holding one of those
    # must still resolve the receipt, and an exact alias is stronger evidence
    # than a prefix guess, so it is tried before prefix matching.
    alias_matches = [record for record in scoped_records if requested in _record_aliases(record)]
    if len(alias_matches) == 1:
        return _resolved(base, alias_matches[0], id_key, strategy="alias")
    if len(alias_matches) > 1:
        base["strategy"] = "alias"
        base["candidates"] = [_candidate(record, id_key) for record in alias_matches[:10]]
        candidate_ids = ", ".join(str(item.get("id")) for item in base["candidates"])
        base["blockers"].append(
            issue(
                "operation_ref_ambiguous",
                f"{target.capitalize()} reference '{requested}' matched multiple candidates: {candidate_ids}.",
            )
        )
        return base

    prefix = [record for record in scoped_records if str(record.get(id_key) or "").startswith(requested)]
    if len(prefix) == 1:
        return _resolved(base, prefix[0], id_key, strategy="prefix")
    if len(prefix) > 1:
        base["strategy"] = "prefix"
        base["candidates"] = [_candidate(record, id_key) for record in prefix[:10]]
        candidate_ids = ", ".join(str(item.get("id")) for item in base["candidates"])
        base["blockers"].append(
            issue(
                "operation_ref_ambiguous",
                f"{target.capitalize()} reference '{requested}' matched multiple candidates: {candidate_ids}.",
            )
        )
        return base

    base["blockers"].append(issue("operation_ref_not_found", f"No {target} matched reference '{requested}'."))
    return base


def _record_aliases(record: Any) -> Set[str]:
    """Identifiers a record answers to besides its own id.

    Receipt discovery records the ids earlier releases wrote for the same
    evidence. Resolution reads them so an operator is not required to know
    which release produced a receipt.
    """

    values = record.get("aliases") if hasattr(record, "get") else None
    if not isinstance(values, (list, tuple, set)):
        return set()
    return {value for value in values if isinstance(value, str) and value}


def _base_resolution(requested: str, target: str) -> Dict[str, Any]:
    return {
        "ok": False,
        "requested": requested,
        "target": target,
        "strategy": None,
        "resolved_id": None,
        "path": None,
        "record": None,
        "payload": None,
        "candidates": [],
        "blockers": [],
        "warnings": [],
    }


def _resolve_path_reference(
    requested: str,
    target: str,
    id_key: str,
    app: Optional[str],
    environment: Optional[str],
    operation: Optional[str],
    status: Optional[str],
) -> Optional[Dict[str, Any]]:
    if target == "receipt" and requested.startswith("sqlite:"):
        base = _base_resolution(requested, target)
        payload = receipt_payload(requested)
        if not isinstance(payload, dict):
            base["strategy"] = "path"
            base["path"] = requested
            base["blockers"].append(
                issue("operation_ref_unreadable", f"Could not read {target} locator: {requested}.")
            )
            return base
        record = _record_from_payload(Path(requested.rpartition("/")[2]), payload, id_key)
        record["path"] = requested
        record["source"] = "journal"
        scope_blockers = _scope_blockers(
            record,
            app=app,
            environment=environment,
            operation=operation,
            status=status,
        )
        if scope_blockers:
            base["strategy"] = "path"
            base["path"] = requested
            base["record"] = record
            base["blockers"].extend(scope_blockers)
            return base
        base.update(
            {
                "ok": True,
                "strategy": "path",
                "resolved_id": record.get(id_key),
                "path": requested,
                "record": record,
                "payload": payload,
            }
        )
        return base

    candidate_path = Path(requested).expanduser()
    if not candidate_path.exists() or not candidate_path.is_file():
        return None
    base = _base_resolution(requested, target)
    payload = (
        receipt_payload(candidate_path)
        if target == "receipt"
        else _read_json(candidate_path)
    )
    if not isinstance(payload, dict):
        base["strategy"] = "path"
        base["path"] = str(candidate_path)
        base["blockers"].append(issue("operation_ref_unreadable", f"Could not read {target} JSON: {candidate_path}."))
        return base
    record = _record_from_payload(candidate_path, payload, id_key)
    scope_blockers = _scope_blockers(record, app=app, environment=environment, operation=operation, status=status)
    if scope_blockers:
        base["strategy"] = "path"
        base["path"] = str(candidate_path)
        base["record"] = record
        base["blockers"].extend(scope_blockers)
        return base
    base.update(
        {
            "ok": True,
            "strategy": "path",
            "resolved_id": record.get(id_key),
            "path": str(candidate_path),
            "record": record,
            "payload": payload,
        }
    )
    return base


def _workflow_records(runtime_root: Path) -> List[Dict[str, Any]]:
    root = Path(runtime_root) / "workflows"
    records: List[Dict[str, Any]] = []
    if not root.exists():
        return records
    for path in sorted(root.glob("*.json")):
        payload = _read_json(path)
        if not isinstance(payload, dict):
            continue
        record = _record_from_payload(path, payload, "workflow_id")
        if record.get("operation") is None:
            record["operation"] = "workflow.plan"
        record["name"] = payload.get("name")
        records.append(record)
    records.sort(key=lambda item: (str(item.get("workflow_id") or ""), str(item.get("path") or "")), reverse=True)
    return records


def _record_from_payload(path: Path, payload: Dict[str, Any], id_key: str) -> Dict[str, Any]:
    terminal = payload.get("ophelia_receipt")
    record_payload = terminal if id_key == "receipt_id" and isinstance(terminal, dict) else payload
    resolved_id = _payload_id(path, record_payload, id_key)
    return {
        id_key: resolved_id,
        "receipt_id": resolved_id if id_key == "receipt_id" else record_payload.get("receipt_id"),
        "workflow_id": resolved_id if id_key == "workflow_id" else record_payload.get("workflow_id"),
        "operation": payload.get("operation") or record_payload.get("operation"),
        "status": payload.get("status") or record_payload.get("status") or record_payload.get("outcome"),
        "app": payload.get("app") or record_payload.get("app"),
        "environment": payload.get("environment") or record_payload.get("environment"),
        "started_at": (
            payload.get("started_at")
            or record_payload.get("started_at")
            or payload.get("created_at")
            or payload.get("applied_at")
        ),
        "completed_at": (
            payload.get("completed_at")
            or record_payload.get("completed_at")
            or payload.get("created_at")
            or payload.get("applied_at")
        ),
        "path": str(path),
    }


def _payload_id(path: Path, payload: Dict[str, Any], id_key: str) -> str:
    preferred = payload.get(id_key)
    if isinstance(preferred, str) and preferred:
        return preferred
    for key in (
        "receipt_id",
        "verify_id",
        "restore_drill_id",
        "operation_id",
        "workflow_id",
        "backup_id",
        "rollback_id",
    ):
        value = payload.get(key)
        if isinstance(value, str) and value:
            return value
    return path.stem


def _latest_scope(reference: str, records: List[Dict[str, Any]]) -> Tuple[Optional[str], Optional[str], List[Dict[str, str]]]:
    if reference == "latest":
        return None, None, []
    _prefix, _sep, suffix = reference.partition(":")
    suffix = suffix.strip()
    if not suffix:
        return None, None, [issue("operation_ref_invalid", "`latest:` requires an app or operation suffix.")]
    if suffix.startswith("app:"):
        return suffix.split(":", 1)[1], None, []
    if suffix.startswith("operation:"):
        return None, suffix.split(":", 1)[1], []
    operations = {str(record.get("operation")) for record in records if record.get("operation")}
    if suffix in operations or "." in suffix:
        return None, suffix, []
    return suffix, None, []


def _combine_scope(
    label: str,
    explicit: Optional[str],
    alias: Optional[str],
    blockers: List[Dict[str, str]],
) -> Optional[str]:
    if explicit and alias and explicit != alias:
        blockers.append(
            issue(
                "operation_ref_scope_conflict",
                f"{label} filter '{explicit}' conflicts with alias scope '{alias}'.",
            )
        )
        return explicit
    return explicit or alias


def _filter_records(
    records: List[Dict[str, Any]],
    *,
    app: Optional[str],
    environment: Optional[str],
    operation: Optional[str],
    status: Optional[str],
) -> List[Dict[str, Any]]:
    filtered = [
        record
        for record in records
        if not _scope_blockers(record, app=app, environment=environment, operation=operation, status=status)
    ]
    filtered.sort(
        key=lambda item: (
            str(item.get("started_at") or item.get("completed_at") or ""),
            str(item.get("receipt_id") or item.get("workflow_id") or ""),
        ),
        reverse=True,
    )
    return filtered


def _scope_blockers(
    record: Dict[str, Any],
    *,
    app: Optional[str],
    environment: Optional[str],
    operation: Optional[str],
    status: Optional[str],
) -> List[Dict[str, str]]:
    blockers: List[Dict[str, str]] = []
    if app is not None and record.get("app") != app:
        blockers.append(issue("operation_ref_app_mismatch", f"Reference app does not match '{app}'."))
    if environment is not None and record.get("environment") != environment:
        blockers.append(issue("operation_ref_environment_mismatch", f"Reference environment does not match '{environment}'."))
    if operation is not None and record.get("operation") != operation:
        blockers.append(issue("operation_ref_operation_mismatch", f"Reference operation does not match '{operation}'."))
    if status is not None and record.get("status") != status:
        blockers.append(issue("operation_ref_status_mismatch", f"Reference status does not match '{status}'."))
    return blockers


def _resolved(base: Dict[str, Any], record: Dict[str, Any], id_key: str, *, strategy: str) -> Dict[str, Any]:
    payload = receipt_payload(record) if id_key == "receipt_id" else None
    if id_key == "receipt_id" and not isinstance(payload, dict):
        base.update(
            {
                "strategy": strategy,
                "resolved_id": record.get(id_key),
                "path": record.get("path"),
                "record": record,
            }
        )
        base["blockers"].append(
            issue(
                "operation_ref_unreadable",
                f"Receipt '{record.get(id_key)}' was discovered but its payload is unreadable.",
                str(record.get("path") or ""),
            )
        )
        return base
    base.update(
        {
            "ok": True,
            "strategy": strategy,
            "resolved_id": record.get(id_key),
            "path": record.get("path"),
            "record": record,
            "payload": payload,
        }
    )
    return base


def _candidate(record: Dict[str, Any], id_key: str) -> Dict[str, Any]:
    return {
        "id": record.get(id_key),
        "operation": record.get("operation"),
        "app": record.get("app"),
        "environment": record.get("environment"),
        "status": record.get("status"),
        "path": record.get("path"),
    }


def _read_json(path: Path) -> Optional[Dict[str, Any]]:
    try:
        payload = json.loads(path.read_text())
    except (OSError, UnicodeDecodeError, json.JSONDecodeError):
        return None
    return payload if isinstance(payload, dict) else None


def _dedupe_issues(items: List[Dict[str, Any]]) -> List[Dict[str, Any]]:
    result: List[Dict[str, Any]] = []
    seen = set()
    for item in items:
        identity = (
            str(item.get("code") or ""),
            str(item.get("message") or ""),
            str(item.get("path") or ""),
        )
        if identity in seen:
            continue
        seen.add(identity)
        result.append(item)
    return result
