"""Independent single-host authority, recovery loop, and scheduler."""

from __future__ import annotations

import json
import os
import platform
import re
import shutil
import stat
import threading
import time
import uuid
from datetime import datetime, timezone
from pathlib import Path
from typing import Any, Dict, Optional

from ..cron import cron_matches
from ..domain import Actor, ApprovedPlanRef
from ..execution import OperationConflict, SQLiteOperationJournal
from ..manifest_v2_execution import (
    apply_manifest_v2_plan,
    describe_manifest_v2_plan,
    load_manifest_v2_plan,
    local_approval_key,
    manifest_v2_executor,
    plan_manifest_v2,
    submit_manifest_v2_plan,
)
from ..manifest_v2 import load_manifest_v2
from ..manifest_v2_sources import manifest_v2_request_digest
from ..runtime_secrets import RuntimeSecretResolver
from ..version import package_version
from ..validation import CanonicalValidationError, parse_environment, parse_identifier
from .config import DaemonConfig, PROTOCOL_VERSION
from .agent import OutboundHostAgent
from .agent_transport import HTTPSAgentTransport
from .identity import identity_status
from .observations import collect_host_observation
from .recovery import create_host_backup as create_encrypted_host_backup
from .store import DaemonStore
from .systemd import notify_systemd, watchdog_ping_seconds
from .workloads import WorkloadExecutionError, WorkloadRunManager


class OpheliaDaemon:
    def __init__(self, config: DaemonConfig, *, runner=None) -> None:
        self.config = config
        self.config.runtime_root.mkdir(mode=0o700, parents=True, exist_ok=True)
        os.chmod(self.config.runtime_root, 0o700)
        self.journal = SQLiteOperationJournal.beneath_runtime_root(config.runtime_root)
        self.store = DaemonStore(self.journal)
        self.secret_resolver = RuntimeSecretResolver(config.runtime_root)
        self.executor = manifest_v2_executor(
            runtime_root=config.runtime_root,
            journal=self.journal,
            runner=runner,
            secret_resolver=self.secret_resolver,
            require_edge_runtime=config.require_edge_runtime,
        )
        self.workloads = WorkloadRunManager(
            runtime_root=config.runtime_root,
            host_id=config.host_id,
            store=self.store,
            runner=runner,
        )
        self.owner_id = "opheliad-%d-%s" % (os.getpid(), uuid.uuid4().hex[:12])
        self._stop = threading.Event()
        self.restart_requested = threading.Event()
        self._threads: list[threading.Thread] = []
        self._last_scheduled_minute: Optional[str] = None
        self._last_integrity_check = 0.0
        # Monotonic stamp of the last completed reconciliation pass. The
        # watchdog loop reads it to tell a slow pass from a wedged daemon.
        self._reconciler_progress = time.monotonic()
        self._health_lock = threading.Lock()
        self._loop_errors: Dict[str, Dict[str, str]] = {}
        self.agent: Optional[OutboundHostAgent] = None
        if config.agent_enabled:
            self.agent = OutboundHostAgent(
                self, HTTPSAgentTransport(config), register=False
            )

    def capabilities(self) -> Dict[str, Any]:
        return {
            "schema_version": 1,
            "kind": "ophelia.capabilities",
            "protocol_versions": [PROTOCOL_VERSION],
            "manifest_versions": [1, 2],
            "workload_kinds": [
                "web",
                "worker",
                "cron",
                "task",
                "migration",
                "internal",
                "static",
            ],
            "operations": [
                "manifest.plan",
                "deploy.apply",
                "operation.cancel",
                "workload.run",
                "workload.cancel",
                "host.drain",
                "host.maintenance",
                "host.certificate.rotate",
                "host.upgrade",
                "host.backup.create",
                "events.read",
                "events.acknowledge",
            ],
            "features": {
                "journaled_recovery": True,
                "revision_isolation": True,
                "cron_scheduler": True,
                "task_execution": True,
                "outbound_agent": self.config.agent_enabled,
                "self_upgrade": True,
                "encrypted_host_backup": bool(
                    self.config.recovery_backup_roots
                    and self.config.recovery_age_recipient
                    and shutil.which("age") is not None
                ),
                "continuous_host_observations": True,
                "runtime_secret_references": True,
            },
            "runtime": {
                "os": platform.system().lower(),
                "architecture": platform.machine().lower(),
                "docker_available": shutil.which("docker") is not None,
                "systemd_available": shutil.which("systemctl") is not None,
            },
            "limits": {
                "max_request_bytes": self.config.max_request_bytes,
                "agent_exchange_bytes": self.config.agent_exchange_bytes,
                "max_workers": self.config.max_workers,
                "event_batch": 1000,
            },
            "agent_version": package_version(),
            "identity": identity_status(self.config),
        }

    def health(self) -> Dict[str, Any]:
        observation = self.latest_observation()
        with self._health_lock:
            errors = dict(self._loop_errors)
        critical = (
            observation is not None
            and observation["observation"].get("status") == "critical"
        )
        result = {
            "status": "degraded" if errors or critical else "ready",
            "host_id": self.config.host_id,
            "owner_id": self.owner_id,
            "threads": {
                thread.name: thread.is_alive() for thread in self._threads
            },
            "loop_errors": errors,
            "observation": observation,
        }
        if self.agent is not None:
            try:
                result["agent"] = self.agent.store.state(self.config.host_id)
            except KeyError:
                result["agent"] = None
        result["restart_requested"] = self.restart_requested.is_set()
        return result

    def request_restart(self) -> None:
        """Request a clean process exit so systemd can reload staged identity or code."""

        self.restart_requested.set()

    def start(self) -> None:
        self.store.register_host(
            host_id=self.config.host_id,
            capabilities=self.capabilities(),
            agent_version=package_version(),
            protocol_version=PROTOCOL_VERSION,
        )
        if self.agent is not None:
            self.agent.store.register(self.config.host_id)
        try:
            self._record_observation()
            self._clear_error("observations")
        except Exception as exc:
            self._record_error("observations", exc)
        if self._threads:
            return
        workload_workers = max(1, self.config.max_workers // 2)
        operation_workers = max(1, self.config.max_workers - workload_workers)
        for index in range(operation_workers):
            self._spawn("operation-%d" % index, self._operation_loop)
        for index in range(workload_workers):
            self._spawn("workload-%d" % index, self._workload_loop)
        self._spawn("scheduler", self._scheduler_loop)
        self._spawn("reconciler", self._reconciliation_loop)
        self._spawn("watchdog", self._watchdog_loop)
        self._spawn("observations", self._observation_loop)
        if self.agent is not None:
            self._spawn("agent", self._agent_loop)

    def stop(self, *, timeout_seconds: float = 10.0) -> None:
        self._stop.set()
        deadline = time.monotonic() + timeout_seconds
        for thread in self._threads:
            thread.join(max(0.0, deadline - time.monotonic()))
        self._threads.clear()

    def plan(
        self,
        manifest_path: Path,
        *,
        actor: Actor,
        idempotency_key: str,
    ) -> Dict[str, Any]:
        if not self.config.local_planning_enabled:
            raise PermissionError("Local manifest planning is disabled by host policy.")
        exact = self._allowed_manifest_path(manifest_path)
        return self._plan_exact(exact, actor=actor, idempotency_key=idempotency_key)

    def plan_agent_manifest(
        self,
        manifest_path: Path,
        *,
        actor: Actor,
        idempotency_key: str,
    ) -> Dict[str, Any]:
        exact = Path(manifest_path).resolve(strict=True)
        inbox = (self.config.runtime_root / "inbox").resolve(strict=True)
        try:
            exact.relative_to(inbox)
        except ValueError as exc:
            raise PermissionError("Agent manifest is outside the trusted inbox.") from exc
        if exact.is_symlink() or not exact.is_file():
            raise PermissionError("Agent manifest must be a trusted regular file.")
        return self._plan_exact(exact, actor=actor, idempotency_key=idempotency_key)

    def _plan_exact(
        self,
        exact: Path,
        *,
        actor: Actor,
        idempotency_key: str,
    ) -> Dict[str, Any]:
        manifest = load_manifest_v2(exact)
        request_digest = manifest_v2_request_digest(manifest, exact)
        key = local_approval_key(self.config.runtime_root, create=True)
        existing = self.store.plan_for_request(
            actor_id=actor.actor_id,
            idempotency_key=idempotency_key,
            request_digest=request_digest,
        )
        if existing is not None:
            return describe_manifest_v2_plan(
                self.config.runtime_root, existing, approval_key=key
            )
        report = plan_manifest_v2(
            exact,
            runtime_root=self.config.runtime_root,
            approval_key=key,
            host_id=self.config.host_id,
            idempotency_key=idempotency_key,
            require_edge_runtime=self.config.require_edge_runtime,
            secret_resolver=self.secret_resolver,
        )
        selected = self.store.record_plan_request(
            actor_id=actor.actor_id,
            idempotency_key=idempotency_key,
            request_digest=request_digest,
            plan_id=report["plan_id"],
        )
        if selected != report["plan_id"]:
            return describe_manifest_v2_plan(
                self.config.runtime_root, selected, approval_key=key
            )
        return report

    def submit_approved_operation(
        self,
        plan_id: str,
        *,
        actor: Actor,
        approval: ApprovedPlanRef,
    ) -> Dict[str, Any]:
        host = self.store.host(self.config.host_id)
        if host["maintenance_mode"] or host["drained"]:
            raise OperationConflict("Host policy is not accepting new operations.")
        loaded = load_manifest_v2_plan(self.config.runtime_root, plan_id)
        return submit_manifest_v2_plan(
            loaded,
            actor=actor,
            approval=approval,
            runtime_root=self.config.runtime_root,
            require_edge_runtime=self.config.require_edge_runtime,
            owner_id=self.owner_id,
            secret_resolver=self.secret_resolver,
            execute=False,
        )

    def submit_operation(
        self,
        plan_id: str,
        confirmation: str,
        *,
        actor: Actor,
    ) -> Dict[str, Any]:
        if not self.config.local_apply_enabled:
            raise PermissionError("Local manifest apply is disabled by host policy.")
        host = self.store.host(self.config.host_id)
        if host["maintenance_mode"] or host["drained"]:
            raise OperationConflict("Host policy is not accepting new operations.")
        return apply_manifest_v2_plan(
            plan_id,
            runtime_root=self.config.runtime_root,
            approval_key=local_approval_key(self.config.runtime_root, create=False),
            confirmation=confirmation,
            require_edge_runtime=self.config.require_edge_runtime,
            secret_resolver=self.secret_resolver,
            owner_id=self.owner_id,
            execute=False,
            actor=actor,
        )

    def operation(self, operation_id: str) -> Dict[str, Any]:
        operation = self.journal.get(operation_id)
        receipt = self.journal.receipt(operation_id)
        return {
            "operation": operation.to_dict(),
            "events": [event.to_dict() for event in self.journal.events(operation_id)],
            "receipt": None if receipt is None else receipt.to_dict(),
        }

    def cancel_operation(self, operation_id: str, *, actor: Actor) -> Dict[str, Any]:
        return self.executor.cancel(operation_id, actor).to_dict()

    def accept_task(
        self,
        *,
        actor: Actor,
        app: str,
        environment: str,
        workload_name: str,
        idempotency_key: str,
    ) -> Dict[str, Any]:
        host = self.store.host(self.config.host_id)
        if host["maintenance_mode"] or host["drained"]:
            raise OperationConflict("Host policy is not accepting new workload runs.")
        return self.workloads.accept(
            actor=actor,
            app=app,
            environment=environment,
            workload_name=workload_name,
            trigger_kind="task",
            idempotency_key=idempotency_key,
        ).to_dict()

    def apps(self) -> Dict[str, Any]:
        items = []
        root = self.config.runtime_root / "apps"
        if root.is_dir() and not root.is_symlink():
            for active_path in sorted(root.glob("*/environments/*/traffic/active.json")):
                try:
                    if active_path.is_symlink() or not active_path.is_file():
                        continue
                    value = json.loads(active_path.read_text(encoding="utf-8"))
                    if isinstance(value, dict):
                        items.append(value)
                except (OSError, ValueError):
                    continue
        return {"apps": items}

    def create_host_backup(
        self, *, backup_id: str, destination_root: Path
    ) -> Dict[str, Any]:
        return create_encrypted_host_backup(
            self.config,
            self.journal,
            backup_id=backup_id,
            destination_root=destination_root,
            trusted_remote=True,
        )

    def latest_observation(self) -> Optional[Dict[str, Any]]:
        try:
            return self.store.latest_observation(self.config.host_id)
        except Exception as exc:
            self._record_error("observations", exc)
            return None

    def app(self, app: str, environment: str) -> Dict[str, Any]:
        try:
            parsed_app = str(parse_identifier(app, field="app"))
            parsed_environment = parse_environment(environment, field="environment")
        except CanonicalValidationError as exc:
            raise KeyError((app, environment)) from exc
        if parsed_environment is None:
            raise KeyError((app, environment))
        scope = (
            self.config.runtime_root
            / "apps"
            / parsed_app
            / "environments"
            / parsed_environment.value
        )
        active_path = scope / "traffic" / "active.json"
        if active_path.is_symlink() or not active_path.is_file():
            raise KeyError((app, environment))
        active = json.loads(active_path.read_text(encoding="utf-8"))
        if (
            not isinstance(active, dict)
            or not isinstance(active.get("revision_id"), str)
            or re.fullmatch(r"rev_[A-Za-z0-9_-]{1,251}", active["revision_id"]) is None
        ):
            raise ValueError("Active revision evidence is malformed.")
        revision_path = scope / "revisions" / active["revision_id"] / "revision.json"
        revision = json.loads(revision_path.read_text(encoding="utf-8"))
        runs = [
            item.to_dict()
            for item in self.store.list_runs(limit=100)
            if item.app == app and item.environment == environment
        ]
        return {"active": active, "revision": revision, "workload_runs": runs}

    def _operation_loop(self) -> None:
        owner = self.owner_id + "-" + threading.current_thread().name
        while not self._stop.is_set():
            try:
                recoverable = self.journal.list_recoverable(limit=1)
                if recoverable:
                    self.executor.run(
                        recoverable[0].operation_id,
                        owner_id=owner,
                    )
                    self._clear_error("operations")
                    continue
                self._clear_error("operations")
            except Exception as exc:
                # The durable operation remains recoverable. A later health
                # surface reports degraded state without losing the work.
                self._record_error("operations", exc)
            self._stop.wait(self.config.operation_poll_seconds)

    def _workload_loop(self) -> None:
        owner = self.owner_id + "-" + threading.current_thread().name
        while not self._stop.is_set():
            try:
                run = self.store.acquire_run(owner, lease_seconds=300)
                if run is not None:
                    self.workloads.execute(run, owner_id=owner)
                    self._clear_error("workloads")
                    continue
                self._clear_error("workloads")
            except Exception as exc:
                self._record_error("workloads", exc)
            self._stop.wait(self.config.operation_poll_seconds)

    def _scheduler_loop(self) -> None:
        actor = Actor(
            actor_id="actor_opheliad-scheduler",
            source="opheliad-scheduler",
            authenticated_by="host-daemon",
        )
        while not self._stop.is_set():
            try:
                host = self.store.host(self.config.host_id)
                now = datetime.now(timezone.utc).replace(second=0, microsecond=0)
                minute = now.isoformat().replace("+00:00", "Z")
                if (
                    minute != self._last_scheduled_minute
                    and not host["maintenance_mode"]
                    and not host["drained"]
                ):
                    self._schedule_minute(actor, now, minute)
                    self._last_scheduled_minute = minute
                self._clear_error("scheduler")
            except Exception as exc:
                self._record_error("scheduler", exc)
            self._stop.wait(self.config.scheduler_seconds)

    def _schedule_minute(self, actor: Actor, now: datetime, minute: str) -> None:
        root = self.config.runtime_root / "apps"
        if not root.is_dir() or root.is_symlink():
            return
        for path in sorted(root.glob("*/environments/*/cron/active.json")):
            try:
                value = json.loads(path.read_text(encoding="utf-8"))
                schedules = value.get("schedules", []) if isinstance(value, dict) else []
                app = path.parents[3].name
                environment = path.parents[1].name
                for schedule in schedules:
                    if isinstance(schedule, dict) and cron_matches(schedule["schedule"], now):
                        self.workloads.accept(
                            actor=actor,
                            app=app,
                            environment=environment,
                            workload_name=schedule["name"],
                            trigger_kind="cron",
                            idempotency_key="cron:%s:%s:%s"
                            % (schedule["revision_digest"], schedule["name"], minute),
                            expected_revision_digest=schedule["revision_digest"],
                        )
            except (OSError, ValueError, KeyError, WorkloadExecutionError, OperationConflict):
                continue

    def _reconciliation_loop(self) -> None:
        while not self._stop.is_set():
            try:
                self.store.heartbeat(self.config.host_id)
                now = time.monotonic()
                if now - self._last_integrity_check >= self.config.integrity_check_seconds:
                    self.journal.integrity_check()
                    self._last_integrity_check = now
                # Status is informational. The watchdog ping belongs to its own
                # loop so a slow store or a raised error cannot starve it.
                notify_systemd("STATUS=Ophelia host state reconciled")
                self._clear_error("reconciler")
            except Exception as exc:
                self._record_error("reconciler", exc)
            # A pass that failed still proves the loop is running, so progress
            # is recorded either way. Only a pass that never returns stops it.
            self._reconciler_progress = time.monotonic()
            self._stop.wait(self.config.reconciliation_seconds)

    def _watchdog_loop(self) -> None:
        interval = watchdog_ping_seconds(os.environ)
        if interval is None:
            return
        stall_limit = self.config.watchdog_stall_seconds
        while not self._stop.is_set():
            stalled = time.monotonic() - self._reconciler_progress
            # Ping while the daemon is demonstrably alive. Withholding the ping
            # once reconciliation has stopped returning is what still lets
            # systemd restart a wedged daemon, which is the point of a watchdog.
            if stalled <= stall_limit:
                try:
                    notify_systemd("WATCHDOG=1")
                    self._clear_error("watchdog")
                except Exception as exc:
                    self._record_error("watchdog", exc)
            self._stop.wait(interval)

    def _agent_loop(self) -> None:
        if self.agent is None:
            return
        delay = self.config.agent_poll_seconds
        while not self._stop.is_set():
            try:
                delay = self.agent.run_once()
                self._clear_error("agent")
            except Exception as exc:
                self._record_error("agent", exc)
                delay = min(300.0, max(self.config.agent_poll_seconds, delay * 2))
            self._stop.wait(delay)

    def _observation_loop(self) -> None:
        while not self._stop.wait(self.config.observation_seconds):
            try:
                self._record_observation()
                self._clear_error("observations")
            except Exception as exc:
                self._record_error("observations", exc)

    def _record_observation(self) -> Dict[str, Any]:
        observation = collect_host_observation(
            self.config,
            self.journal,
            active_apps=len(self.apps()["apps"]),
        )
        return self.store.record_observation(
            self.config.host_id,
            observation,
            retention=self.config.observation_retention,
        )

    def _allowed_manifest_path(self, value: Path) -> Path:
        requested = Path(value).expanduser().absolute()
        if not self.config.allowed_manifest_roots:
            raise PermissionError("No local manifest roots are allowed by host policy.")
        for root in self.config.allowed_manifest_roots:
            try:
                lexical_root = Path(root).expanduser().absolute()
                relative = requested.relative_to(lexical_root)
                current = lexical_root
                for component in relative.parts:
                    current = current / component
                    if stat.S_ISLNK(current.lstat().st_mode):
                        raise PermissionError(
                            "Manifest path may not traverse symbolic links."
                        )
                candidate = requested.resolve(strict=True)
                candidate.relative_to(lexical_root.resolve(strict=True))
                if not candidate.is_file():
                    raise PermissionError("Manifest path must be a trusted regular file.")
                return candidate
            except PermissionError:
                raise
            except (OSError, ValueError):
                continue
        raise PermissionError("Manifest path is outside the configured allowlist.")

    def _spawn(self, name: str, target) -> None:
        thread = threading.Thread(
            name="opheliad-" + name,
            target=target,
            daemon=True,
        )
        thread.start()
        self._threads.append(thread)

    def _record_error(self, loop: str, error: Exception) -> None:
        with self._health_lock:
            self._loop_errors[loop] = {
                "error_type": type(error).__name__,
                "observed_at": datetime.now(timezone.utc).isoformat().replace(
                    "+00:00", "Z"
                ),
            }

    def _clear_error(self, loop: str) -> None:
        with self._health_lock:
            self._loop_errors.pop(loop, None)
