from __future__ import annotations

import base64
import hashlib
import os
import http.client
import io
import json
import runpy
import signal
import socket
import subprocess
import tempfile
import tarfile
import threading
import time
import unittest
from datetime import datetime, timedelta, timezone
from pathlib import Path
from typing import Dict, List, Optional, Sequence, Set
from unittest import mock

from ophelia.cron import CronExpressionError, cron_matches, validate_cron_expression
from ophelia.daemon.config import DaemonConfig, DaemonConfigError, load_daemon_config
from ophelia.daemon.api import create_unix_server
from ophelia.daemon.agent import OutboundHostAgent
from ophelia.daemon.agent_store import AgentStore
from ophelia.daemon.service import OpheliaDaemon
from ophelia.daemon.store import DaemonStore
from ophelia.daemon.install import (
    _activate_service,
    _install_release,
    _source_digest,
    daemon_install_plan,
)
from ophelia.daemon.enrollment import (
    EnrollmentError,
    _enable_agent_config,
    enrollment_plan,
)
from ophelia.daemon.systemd import notify_systemd, watchdog_ping_seconds
from ophelia.daemon.decisions import verify_lumen_decision
from ophelia.daemon.upgrades import (
    AgentUpgradeError,
    _extract_source_archive,
    confirm_running_upgrade,
    stage_agent_upgrade,
)
from ophelia.domain import Actor
from ophelia.domain._contracts import canonical_json
from ophelia.execution import (
    IdempotencyConflict,
    OperationConflict,
    SQLiteOperationJournal,
)
from ophelia.execution.subprocesses import ProcessResult
from ophelia.manifest_v2_execution import load_manifest_v2_plan
from ophelia.version import package_version


PINNED_IMAGE = "ghcr.io/example/daemon@sha256:" + "d" * 64


class FakeRunner:
    def __init__(self) -> None:
        self.commands: List[List[str]] = []
        self.running: Dict[str, Set[str]] = {}

    def run(self, argv: Sequence[str], **_: object) -> ProcessResult:
        command = list(argv)
        self.commands.append(command)
        project = _option(command, "-p")
        stdout = ""
        exit_code = 0
        reason = "success"
        if command[:3] == ["docker", "image", "inspect"]:
            stdout = "node\n" if "--format" in command else PINNED_IMAGE + "\n"
        elif command[:2] == ["docker", "inspect"]:
            exit_code = 1
            reason = "nonzero_exit"
        elif "up" in command:
            arguments = command[command.index("up") + 1 :]
            services = [item for item in arguments if not item.startswith("-") and "=" not in item]
            self.running.setdefault(project, set()).update(services)
        elif "stop" in command:
            self.running.setdefault(project, set()).difference_update(command[command.index("stop") + 1 :])
        elif "down" in command:
            self.running.pop(project, None)
        elif "ps" in command and "--services" in command:
            stdout = "\n".join(sorted(self.running.get(project, set()))) + "\n"
        return ProcessResult(tuple(command), exit_code, reason, stdout, "", False, False, 1)


class FakeAgentTransport:
    def __init__(self, responses: List[dict]) -> None:
        self.responses = responses
        self.requests: List[dict] = []

    def exchange(self, payload: dict) -> dict:
        self.requests.append(payload)
        response = dict(self.responses.pop(0))
        response.setdefault("exchange_id", payload["exchange_id"])
        return response


class FakeUpgradeRunner:
    def __init__(self) -> None:
        self.commands: List[List[str]] = []

    def run(self, argv: Sequence[str], **_: object) -> ProcessResult:
        command = list(argv)
        self.commands.append(command)
        if command[:3] == ["python3", "-m", "venv"]:
            release = Path(command[3])
            executable = release / "bin" / "opheliad"
            executable.parent.mkdir(parents=True)
            executable.write_text("#!/bin/sh\nexit 0\n")
            executable.chmod(0o700)
        return ProcessResult(tuple(command), 0, "success", "", "", False, False, 1)


class CronTests(unittest.TestCase):
    def test_validates_ranges_steps_and_utc_matching(self) -> None:
        value = datetime(2026, 7, 19, 3, 17, tzinfo=timezone.utc)
        self.assertTrue(cron_matches("17 3 * * 0", value))
        self.assertTrue(cron_matches("*/17 3 1-31/2 * 0,7", value))
        self.assertFalse(cron_matches("18 3 * * *", value))
        with self.assertRaises(CronExpressionError):
            validate_cron_expression("61 * * * *")


class DaemonConfigTests(unittest.TestCase):
    def test_defaults_are_typed_and_explicit_overrides_work(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            config = load_daemon_config(
                root / "missing.toml",
                runtime_root=root / "runtime",
                socket_path=root / "opheliad.sock",
            )
        self.assertEqual((root / "runtime").resolve(), config.runtime_root)
        self.assertEqual((os.geteuid(),), config.allowed_uids)

    def test_unknown_keys_and_writable_config_are_rejected(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory) / "agent.toml"
            path.write_text("mystery = true\n")
            with self.assertRaisesRegex(DaemonConfigError, "Unknown"):
                load_daemon_config(path)
            path.write_text("agent_enabled = false\n")
            path.chmod(0o666)
            with self.assertRaisesRegex(DaemonConfigError, "writable"):
                load_daemon_config(path)

    def test_recovery_and_observation_boundaries_are_strict(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            path = root / "agent.toml"
            path.write_text(
                '\n'.join(
                    [
                        'host_id = "host_observer-1"',
                        'recovery_backup_roots = ["%s"]' % root,
                        'recovery_age_recipient = "age1publicfixture"',
                        'disk_warning_percent = 80',
                        'disk_critical_percent = 90',
                    ]
                )
                + '\n'
            )
            config = load_daemon_config(
                path,
                runtime_root=root / "runtime",
                socket_path=root / "daemon.sock",
            )
            self.assertEqual((root.resolve(),), config.recovery_backup_roots)
            path.write_text('host_id = "host_../escape"\n')
            with self.assertRaisesRegex(DaemonConfigError, "host_id"):
                load_daemon_config(path)
            path.write_text(
                "disk_warning_percent = 95\ndisk_critical_percent = 90\n"
            )
            with self.assertRaisesRegex(DaemonConfigError, "lower"):
                load_daemon_config(path)

    def test_enabled_agent_requires_trusted_complete_tls_identity(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            trust = root / "trust"
            identity = root / "identity"
            trust.mkdir(mode=0o700)
            identity.mkdir(mode=0o700)
            credentials = {}
            for name in ("ca.pem", "host.crt", "host.key", "decision.pem"):
                path = (
                    identity / name
                    if name in {"host.crt", "host.key"}
                    else trust / name
                )
                path.write_text("fixture")
                path.chmod(0o600 if name == "host.key" else 0o644)
                credentials[name] = path
            config_path = root / "agent.toml"
            config_path.write_text(
                "\n".join(
                    [
                        "agent_enabled = true",
                        'control_plane_url = "https://control.example.com"',
                        'control_plane_ca_path = "%s"' % credentials["ca.pem"],
                        'host_certificate_path = "%s"' % credentials["host.crt"],
                        'host_private_key_path = "%s"' % credentials["host.key"],
                        'decision_public_key_path = "%s"' % credentials["decision.pem"],
                    ]
                )
                + "\n"
            )
            config = load_daemon_config(
                config_path,
                runtime_root=root / "runtime",
                socket_path=root / "daemon.sock",
            )
            self.assertTrue(config.agent_enabled)
            credentials["host.key"].chmod(0o644)
            with self.assertRaisesRegex(DaemonConfigError, "0600"):
                load_daemon_config(
                    config_path,
                    runtime_root=root / "runtime",
                    socket_path=root / "daemon.sock",
                )

    def test_control_plane_url_rejects_invalid_ports_and_whitespace(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            path = root / "agent.toml"
            for url in ("https://control.example.com:invalid", "https://bad host"):
                path.write_text('control_plane_url = %s\n' % json.dumps(url))
                with self.assertRaisesRegex(DaemonConfigError, "control_plane_url"):
                    load_daemon_config(
                        path,
                        runtime_root=root / "runtime",
                        socket_path=root / "daemon.sock",
                    )


class DaemonInstallTests(unittest.TestCase):
    def test_enrollment_config_pins_the_planned_logical_host_identity(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            config = Path(directory) / "agent.toml"
            config.write_text("agent_enabled = false\n")
            _enable_agent_config(
                config,
                {
                    "host_id": "host_recovered-1",
                    "control_plane_url": "https://control.example.com",
                    "targets": {
                        "control_plane_ca": "/etc/ophelia/trust/ca.pem",
                        "host_certificate": "/var/lib/ophelia-identity/host.crt",
                        "host_key": "/var/lib/ophelia-identity/host.key",
                        "decision_public_key": "/etc/ophelia/trust/decision.pem",
                    },
                },
            )
            self.assertIn('host_id = "host_recovered-1"', config.read_text())

    def test_plan_distinguishes_complete_and_incomplete_versioned_releases(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            source = root / "source"
            source.mkdir()
            (source / "pyproject.toml").write_text("[project]\nname='fixture'\n")
            install_root = root / "install"
            config = root / "agent.toml"
            unit = root / "opheliad.service"
            with mock.patch(
                "ophelia.daemon.install.shutil.which", return_value="/usr/bin/tool"
            ):
                initial = daemon_install_plan(
                    source_root=source,
                    install_root=install_root,
                    config_path=config,
                    unit_path=unit,
                )
                release = Path(initial["release_root"])
                release.mkdir(parents=True)
                incomplete = daemon_install_plan(
                    source_root=source,
                    install_root=install_root,
                    config_path=config,
                    unit_path=unit,
                )
                executable = release / "bin" / "opheliad"
                executable.parent.mkdir()
                executable.write_text("#!/bin/sh\nexit 0\n")
                executable.chmod(0o755)
                (release / "ophelia-install.json").write_text(
                    json.dumps(
                        {
                            "schema_version": 1,
                            "kind": "ophelia.daemon-install",
                            "version": package_version(),
                            "source_digest": incomplete["observations"]["source_digest"],
                        }
                    )
                )
                complete = daemon_install_plan(
                    source_root=source,
                    install_root=install_root,
                    config_path=config,
                    unit_path=unit,
                )
                deferred = daemon_install_plan(
                    source_root=source,
                    install_root=install_root,
                    config_path=config,
                    unit_path=unit,
                    activate=False,
                )

        self.assertTrue(incomplete["observations"]["release_present"])
        self.assertFalse(incomplete["observations"]["release_valid"])
        self.assertTrue(complete["observations"]["release_valid"])
        self.assertFalse(deferred["activate"])
        self.assertIn(
            "enable-service-with-start-deferred-for-recovery", deferred["steps"]
        )

    def test_installer_creates_virtualenv_at_final_release_path(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            release = root / "releases" / "0.6.3-fixture"
            source = root / "source"
            source.mkdir()
            runner = FakeUpgradeRunner()

            _install_release(
                runner,
                release,
                source,
                source_digest="sha256:" + "a" * 64,
            )

        self.assertEqual(
            ["python3", "-m", "venv", str(release)],
            runner.commands[0],
        )
        staged_source = Path(runner.commands[1][-1])
        self.assertNotEqual(source, staged_source)
        self.assertFalse(staged_source.exists())
        self.assertFalse(any(".install-" in part for command in runner.commands for part in command))

    def test_source_digest_ignores_build_and_package_metadata(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            source = Path(directory)
            (source / "pyproject.toml").write_text("[project]\nname='fixture'\n")
            expected = _source_digest(source)
            for path in (
                source / "build/lib/fixture.py",
                source / "src/fixture.egg-info/PKG-INFO",
                source / "src/fixture/__pycache__/module.pyc",
            ):
                path.parent.mkdir(parents=True, exist_ok=True)
                path.write_text("generated")

            self.assertEqual(expected, _source_digest(source))
            (source / "src/fixture/module.py").parent.mkdir(parents=True, exist_ok=True)
            (source / "src/fixture/module.py").write_text("value = 1\n")
            self.assertNotEqual(expected, _source_digest(source))

    def test_installer_restarts_an_already_active_daemon_after_promotion(self) -> None:
        runner = FakeUpgradeRunner()

        _activate_service(runner)

        self.assertEqual(
            [
                ["systemctl", "enable", "opheliad.service"],
                ["systemctl", "restart", "opheliad.service"],
                ["systemctl", "is-active", "--quiet", "opheliad.service"],
            ],
            runner.commands,
        )

    def test_enrollment_plan_binds_token_digest_without_exposing_token(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            token = root / "enrollment.token"
            token.write_text("secret-one-time-token")
            token.chmod(0o600)
            config = root / "agent.toml"
            config.write_text("agent_enabled = false\n")
            with mock.patch(
                "ophelia.daemon.enrollment.shutil.which", return_value="/usr/bin/tool"
            ):
                plan = enrollment_plan(
                    host_id="host_fixture-1",
                    control_plane_url="https://control.example.com",
                    token_file=token,
                    trust_root=root / "trust",
                    identity_root=root / "identity",
                    config_path=config,
                )

        self.assertTrue(plan["can_apply"])
        self.assertNotIn("secret-one-time-token", json.dumps(plan))
        self.assertTrue(plan["observations"]["token_digest"].startswith("sha256:"))

    def test_enrollment_plan_rejects_an_invalid_control_plane_port(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            token = root / "enrollment.token"
            token.write_text("secret-one-time-token")
            token.chmod(0o600)
            config = root / "agent.toml"
            config.write_text("agent_enabled = false\n")

            with self.assertRaisesRegex(EnrollmentError, "invalid port"):
                enrollment_plan(
                    host_id="host_fixture-1",
                    control_plane_url="https://control.example.com:invalid",
                    token_file=token,
                    trust_root=root / "trust",
                    identity_root=root / "identity",
                    config_path=config,
                )

    def test_enrollment_plan_blocks_a_journal_bound_to_another_host(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            runtime = root / "runtime"
            token = root / "enrollment.token"
            token.write_text("secret-one-time-token")
            token.chmod(0o600)
            config = root / "agent.toml"
            config.write_text(
                'runtime_root = %s\nagent_enabled = false\n' % json.dumps(str(runtime))
            )
            journal = SQLiteOperationJournal.beneath_runtime_root(runtime)
            DaemonStore(journal).register_host(
                host_id="host_existing",
                capabilities={},
                agent_version="0.6.17",
                protocol_version=1,
            )
            with mock.patch(
                "ophelia.daemon.enrollment.shutil.which", return_value="/usr/bin/tool"
            ):
                plan = enrollment_plan(
                    host_id="host_replacement",
                    control_plane_url="https://control.example.com",
                    token_file=token,
                    trust_root=root / "trust",
                    identity_root=root / "identity",
                    config_path=config,
                )

        self.assertFalse(plan["can_apply"])
        self.assertIn("operation_journal_host_identity_mismatch", plan["blockers"])
        self.assertEqual(
            ["host_existing"],
            plan["observations"]["operation_journal"]["host_ids"],
        )


class AgentUpgradeTests(unittest.TestCase):
    def test_staged_upgrade_promotes_and_confirms_exact_release(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            runtime = root / "runtime"
            install = root / "install"
            previous = install / "releases" / "previous"
            previous.mkdir(parents=True)
            (install / "current").symlink_to(previous)
            inbox = runtime / "inbox" / "command_upgrade-1"
            inbox.mkdir(parents=True)
            archive = inbox / "ophelia-source.tar.gz"
            _source_archive(archive, version=package_version())
            digest = "sha256:" + hashlib.sha256(archive.read_bytes()).hexdigest()
            config = DaemonConfig(
                host_id="host_fixture-1",
                runtime_root=runtime,
                socket_path=root / "opheliad.sock",
                allowed_uids=(os.geteuid(),),
                allowed_manifest_roots=(root,),
                install_root=install,
                require_edge_runtime=False,
            )

            staged = stage_agent_upgrade(
                config,
                command_id="command_upgrade-1",
                envelope_digest="sha256:" + "a" * 64,
                payload={
                    "version": package_version(),
                    "archive_path": str(archive),
                    "source_digest": digest,
                },
                runner=FakeUpgradeRunner(),
            )
            receipt = confirm_running_upgrade(config)

            self.assertEqual(
                Path(staged["target_release"]).resolve(),
                (install / "current").resolve(),
            )
            self.assertEqual("succeeded", receipt["status"])
            self.assertFalse((install / "upgrade-pending.json").exists())

    def test_source_archive_rejects_traversal_before_writing(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            archive = root / "bad.tar.gz"
            with tarfile.open(archive, "w:gz") as bundle:
                member = tarfile.TarInfo("../escape")
                member.size = 4
                bundle.addfile(member, io.BytesIO(b"nope"))
            target = root / "target"
            target.mkdir()

            with self.assertRaisesRegex(AgentUpgradeError, "unsafe"):
                _extract_source_archive(archive, target, maximum_bytes=1024)
            self.assertFalse((root / "escape").exists())


class SystemdNotificationTests(unittest.TestCase):
    def test_notify_uses_supplied_unix_datagram_socket(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory) / "notify.sock"
            receiver = socket.socket(socket.AF_UNIX, socket.SOCK_DGRAM)
            receiver.bind(str(path))
            receiver.settimeout(2)
            try:
                with mock.patch.dict(os.environ, {"NOTIFY_SOCKET": str(path)}):
                    self.assertTrue(notify_systemd("READY=1"))
                self.assertEqual(b"READY=1", receiver.recv(4096))
            finally:
                receiver.close()


class WatchdogPingIntervalTests(unittest.TestCase):
    def test_interval_is_half_the_configured_watchdog_window(self) -> None:
        self.assertEqual(
            30.0,
            watchdog_ping_seconds(
                {"NOTIFY_SOCKET": "/run/notify", "WATCHDOG_USEC": "60000000"}
            ),
        )

    def test_no_interval_without_systemd_or_a_watchdog(self) -> None:
        self.assertIsNone(watchdog_ping_seconds({}))
        self.assertIsNone(watchdog_ping_seconds({"NOTIFY_SOCKET": "/run/notify"}))
        self.assertIsNone(
            watchdog_ping_seconds({"WATCHDOG_USEC": "60000000"}),
        )

    def test_a_watchdog_scoped_to_another_process_is_ignored(self) -> None:
        self.assertIsNone(
            watchdog_ping_seconds(
                {
                    "NOTIFY_SOCKET": "/run/notify",
                    "WATCHDOG_USEC": "60000000",
                    "WATCHDOG_PID": str(os.getpid() + 1),
                }
            )
        )

    def test_an_unreadable_or_zero_window_asks_for_no_pings(self) -> None:
        for value in ("", "abc", "0", "-1"):
            self.assertIsNone(
                watchdog_ping_seconds(
                    {"NOTIFY_SOCKET": "/run/notify", "WATCHDOG_USEC": value}
                ),
                value,
            )


class WatchdogLoopTests(unittest.TestCase):
    """The watchdog must survive a reconciliation pass that blocks for minutes.

    A deploy drives long synchronous work through this daemon, and before this
    the watchdog ping was sent at the end of the reconciliation pass, so a pass
    that outran WatchdogSec had systemd SIGABRT the process mid-deploy.
    """

    def _daemon(self, directory: str, **overrides) -> OpheliaDaemon:
        root = Path(directory)
        config = DaemonConfig(
            host_id="host_fixture-1",
            runtime_root=root / "runtime",
            socket_path=root / "opheliad.sock",
            allowed_uids=(os.geteuid(),),
            allowed_manifest_roots=(),
            require_edge_runtime=False,
            watchdog_stall_seconds=60.0,
            **overrides,
        )
        return OpheliaDaemon(config, runner=FakeRunner())

    def test_pings_continue_while_a_reconciliation_pass_is_still_running(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            service = self._daemon(directory)
            path = Path(directory) / "notify.sock"
            receiver = socket.socket(socket.AF_UNIX, socket.SOCK_DGRAM)
            receiver.bind(str(path))
            receiver.settimeout(5)
            try:
                environment = {
                    "NOTIFY_SOCKET": str(path),
                    "WATCHDOG_USEC": "2000000",
                }
                with mock.patch.dict(os.environ, environment):
                    # No pass has completed for 30 seconds, well past the one
                    # minute systemd would allow, but inside the stall limit.
                    service._reconciler_progress = time.monotonic() - 30.0
                    thread = threading.Thread(target=service._watchdog_loop, daemon=True)
                    thread.start()
                    try:
                        self.assertEqual(b"WATCHDOG=1", receiver.recv(4096))
                        self.assertEqual(b"WATCHDOG=1", receiver.recv(4096))
                    finally:
                        service._stop.set()
                        thread.join(timeout=5)
            finally:
                receiver.close()

    def test_pings_stop_once_reconciliation_has_stalled_past_the_limit(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            service = self._daemon(directory)
            path = Path(directory) / "notify.sock"
            receiver = socket.socket(socket.AF_UNIX, socket.SOCK_DGRAM)
            receiver.bind(str(path))
            receiver.settimeout(1)
            try:
                environment = {
                    "NOTIFY_SOCKET": str(path),
                    "WATCHDOG_USEC": "2000000",
                }
                with mock.patch.dict(os.environ, environment):
                    # A daemon that has not completed a pass for far longer than
                    # the stall limit is wedged, and systemd must be allowed to
                    # restart it.
                    service._reconciler_progress = time.monotonic() - 600.0
                    thread = threading.Thread(target=service._watchdog_loop, daemon=True)
                    thread.start()
                    try:
                        with self.assertRaises(socket.timeout):
                            receiver.recv(4096)
                    finally:
                        service._stop.set()
                        thread.join(timeout=5)
            finally:
                receiver.close()

    def test_a_failing_reconciliation_pass_still_records_progress(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            service = self._daemon(directory, reconciliation_seconds=0.01)
            service._reconciler_progress = time.monotonic() - 300.0
            with mock.patch.object(
                service.store, "heartbeat", side_effect=RuntimeError("store is busy")
            ):
                thread = threading.Thread(
                    target=service._reconciliation_loop, daemon=True
                )
                thread.start()
                try:
                    deadline = time.monotonic() + 5
                    while time.monotonic() < deadline:
                        if time.monotonic() - service._reconciler_progress < 5:
                            break
                        time.sleep(0.01)
                finally:
                    service._stop.set()
                    thread.join(timeout=5)
            # The loop is alive even though every pass raised, so the watchdog
            # must keep pinging; only a pass that never returns stops it.
            self.assertLess(time.monotonic() - service._reconciler_progress, 5)
            self.assertIn("reconciler", service.health()["loop_errors"])


class DaemonClientReconnectTests(unittest.TestCase):
    """A read must survive the few seconds systemd takes to restart opheliad.

    A production deploy preflight failed outright on 2026-09-17 because its
    closing `ship daemon status` landed inside a watchdog restart window.
    """

    def test_a_read_waits_for_a_socket_that_appears_during_a_restart(self) -> None:
        from ophelia.daemon.client import DaemonClient

        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            config = DaemonConfig(
                host_id="host_fixture-1",
                runtime_root=root / "runtime",
                socket_path=root / "opheliad.sock",
                allowed_uids=(os.geteuid(),),
                allowed_manifest_roots=(),
                require_edge_runtime=False,
            )
            daemon = OpheliaDaemon(config, runner=FakeRunner())
            daemon.store.register_host(
                host_id=config.host_id,
                capabilities=daemon.capabilities(),
                agent_version="0.6.0",
                protocol_version=1,
            )
            client = DaemonClient(config.socket_path, reconnect_seconds=8.0)
            listening = threading.Event()
            holder: Dict[str, object] = {}

            def serve() -> None:
                # The socket does not exist yet, exactly as while systemd is
                # restarting the daemon.
                time.sleep(0.75)
                server = create_unix_server(daemon)
                holder["server"] = server
                listening.set()
                server.serve_forever(poll_interval=0.05)

            thread = threading.Thread(target=serve, daemon=True)
            thread.start()
            try:
                data = client.get("/v1/health")
                self.assertTrue(listening.is_set())
                self.assertIn(data["status"], {"ready", "degraded"})
            finally:
                server = holder.get("server")
                if server is not None:
                    server.shutdown()
                    server.server_close()
                thread.join(timeout=5)

    def test_a_read_still_fails_once_the_budget_is_spent(self) -> None:
        from ophelia.daemon.client import DaemonClient, DaemonClientError

        with tempfile.TemporaryDirectory() as directory:
            client = DaemonClient(
                Path(directory) / "missing.sock", reconnect_seconds=0.5
            )
            started = time.monotonic()
            with self.assertRaises(DaemonClientError) as caught:
                client.get("/v1/health")
            self.assertEqual("daemon_unavailable", caught.exception.code)
            self.assertGreaterEqual(time.monotonic() - started, 0.5)

    def test_a_write_is_never_replayed(self) -> None:
        from ophelia.daemon.client import DaemonClient, DaemonClientError

        with tempfile.TemporaryDirectory() as directory:
            client = DaemonClient(
                Path(directory) / "missing.sock", reconnect_seconds=30.0
            )
            started = time.monotonic()
            with self.assertRaises(DaemonClientError):
                client.post("/v1/apps", {}, idempotency_key="key-1")
            self.assertLess(time.monotonic() - started, 5.0)


class SystemdLauncherTests(unittest.TestCase):
    def test_forwarded_shutdown_is_clean_and_does_not_trigger_upgrade_rollback(self) -> None:
        launcher = runpy.run_path(
            str(
                Path(__file__).resolve().parents[1]
                / "src/ophelia/resources/systemd/opheliad-launcher.py"
            )
        )
        expected_stop = launcher["_expected_service_stop"]

        self.assertTrue(expected_stop(-signal.SIGTERM, signal.SIGTERM))
        self.assertTrue(expected_stop(0, signal.SIGTERM))
        self.assertTrue(expected_stop(-signal.SIGINT, signal.SIGINT))
        self.assertFalse(expected_stop(1, signal.SIGTERM))
        self.assertFalse(expected_stop(-signal.SIGTERM, None))


class DaemonStoreTests(unittest.TestCase):
    def test_workload_runs_are_idempotent_fenced_and_replayable(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            journal = SQLiteOperationJournal.beneath_runtime_root(Path(directory))
            now = [1_774_111_200.0]
            store = DaemonStore(journal, clock=lambda: now[0])
            store.register_host(
                host_id="host_fixture-1",
                capabilities={"manifest_versions": [2]},
                agent_version="0.6.0",
                protocol_version=1,
            )
            with self.assertRaisesRegex(OperationConflict, "different host"):
                store.register_host(
                    host_id="host_rebound-1",
                    capabilities={},
                    agent_version="0.6.0",
                    protocol_version=1,
                )
            actor = Actor("actor_fixture-1", "test", "test")
            arguments = dict(
                actor=actor,
                host_id="host_fixture-1",
                app="demo",
                environment="staging",
                revision_id="rev_fixture-1",
                revision_digest="sha256:" + "1" * 64,
                workload_name="cleanup",
                workload_kind="task",
                trigger_kind="task",
                idempotency_key="task-1",
                concurrency_policy="forbid",
            )
            first = store.accept_run(**arguments)
            retry = store.accept_run(**arguments)
            self.assertEqual(first.run_id, retry.run_id)
            with self.assertRaises(IdempotencyConflict):
                store.accept_run(**{**arguments, "app": "different"})

            claimed = store.acquire_run("worker-1")
            self.assertEqual(first.run_id, claimed.run_id)
            self.assertTrue(
                store.renew_run_lease(
                    claimed.run_id,
                    owner_id="worker-1",
                    fencing_token=claimed.fencing_token,
                )
            )
            now[0] += 301
            replacement = store.acquire_run("worker-2")
            self.assertEqual(claimed.fencing_token + 1, replacement.fencing_token)
            self.assertFalse(
                store.renew_run_lease(
                    claimed.run_id,
                    owner_id="worker-1",
                    fencing_token=claimed.fencing_token,
                )
            )
            terminal = store.complete_run(
                replacement.run_id,
                owner_id="worker-2",
                fencing_token=replacement.fencing_token,
                state="succeeded",
                result_digest="sha256:" + "2" * 64,
                exit_code=0,
                exit_reason="success",
            )
            events = journal.host_events_after(0)
            acknowledged = journal.acknowledge_host_events("lumen", events[-1]["cursor"])

            self.assertEqual("succeeded", terminal.state)
            self.assertEqual(4, len(events))
            self.assertEqual(events[-1]["cursor"], acknowledged)
            journal.integrity_check()

    def test_agent_commands_are_strictly_ordered_replayable_and_acknowledged(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            journal = SQLiteOperationJournal.beneath_runtime_root(Path(directory))
            store = AgentStore(journal, clock=lambda: 1_774_111_200.0)
            store.register("host_fixture-1")
            command = store.accept(
                command_id="command_fixture-1",
                sequence=1,
                operation="host.drain",
                actor_id="actor_fixture-1",
                idempotency_key="command-1",
                envelope_digest="sha256:" + "a" * 64,
                payload={"enabled": True},
            )
            retry = store.accept(
                command_id="command_fixture-1",
                sequence=1,
                operation="host.drain",
                actor_id="actor_fixture-1",
                idempotency_key="command-1",
                envelope_digest="sha256:" + "a" * 64,
                payload={"enabled": True},
            )
            self.assertEqual(command, retry)
            with self.assertRaises(IdempotencyConflict):
                store.accept(
                    command_id="command_fixture-1",
                    sequence=1,
                    operation="host.drain",
                    actor_id="actor_fixture-1",
                    idempotency_key="command-1",
                    envelope_digest="sha256:" + "a" * 64,
                    payload={"enabled": False},
                )
            store.mark_running(command.command_id)
            terminal = store.complete(
                command.command_id,
                succeeded=True,
                result={"status": "succeeded"},
            )
            self.assertEqual("succeeded", terminal.state)
            self.assertEqual(1, len(store.pending_results("host_fixture-1")))
            self.assertEqual(1, store.acknowledge_results("host_fixture-1", 1))
            self.assertEqual((), store.pending_results("host_fixture-1"))
            journal.integrity_check()

    def test_agent_result_delivery_stops_at_first_nonterminal_sequence(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            journal = SQLiteOperationJournal.beneath_runtime_root(Path(directory))
            store = AgentStore(journal, clock=lambda: 1_774_111_200.0)
            store.register("host_fixture-1")
            first = store.accept(
                command_id="command_fixture-1",
                sequence=1,
                operation="host.drain",
                actor_id="actor_fixture-1",
                idempotency_key="command-1",
                envelope_digest="sha256:" + "a" * 64,
                payload={"enabled": True},
            )
            second = store.accept(
                command_id="command_fixture-2",
                sequence=2,
                operation="host.maintenance",
                actor_id="actor_fixture-1",
                idempotency_key="command-2",
                envelope_digest="sha256:" + "b" * 64,
                payload={"enabled": True},
            )
            store.complete(
                second.command_id,
                succeeded=True,
                result={"status": "succeeded"},
            )
            self.assertEqual((), store.pending_results("host_fixture-1"))
            store.complete(
                first.command_id,
                succeeded=True,
                result={"status": "succeeded"},
            )
            self.assertEqual(
                [1, 2],
                [item["sequence"] for item in store.pending_results("host_fixture-1")],
            )

    def test_agent_accepts_the_next_sequence_after_an_authoritative_core_skip(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            journal = SQLiteOperationJournal.beneath_runtime_root(Path(directory))
            store = AgentStore(journal, clock=lambda: 1_774_111_200.0)
            store.register("host_fixture-1")
            self.assertEqual(2, store.acknowledge_results("host_fixture-1", 2))
            command = store.accept(
                command_id="command_fixture-3",
                sequence=3,
                operation="host.drain",
                actor_id="actor_fixture-1",
                idempotency_key="command-3",
                envelope_digest="sha256:" + "c" * 64,
                payload={"enabled": True},
            )
            self.assertEqual(3, command.sequence)
            with self.assertRaises(OperationConflict):
                store.accept(
                    command_id="command_fixture-5",
                    sequence=5,
                    operation="host.maintenance",
                    actor_id="actor_fixture-1",
                    idempotency_key="command-5",
                    envelope_digest="sha256:" + "e" * 64,
                    payload={"enabled": True},
                )
            journal.integrity_check()

    def test_agent_connection_preserves_last_successful_exchange(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            now = [1_774_111_200.0]
            store = AgentStore(
                SQLiteOperationJournal.beneath_runtime_root(Path(directory)),
                clock=lambda: now[0],
            )
            store.register("host_fixture-1")
            connected = store.record_connection("host_fixture-1", connected=True)
            now[0] += 30
            disconnected = store.record_connection(
                "host_fixture-1", connected=False, error_type="TimeoutError"
            )
            self.assertEqual(connected["last_exchange_at"], disconnected["last_exchange_at"])
            now[0] += 30
            reconnected = store.record_connection("host_fixture-1", connected=True)
            self.assertNotEqual(connected["connected_at"], reconnected["connected_at"])


class OutboundAgentTests(unittest.TestCase):
    def test_host_backup_command_uses_the_configured_control_plane_scope(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            config = DaemonConfig(
                host_id="host_fixture-1",
                runtime_root=root / "runtime",
                socket_path=root / "opheliad.sock",
                allowed_uids=(os.geteuid(),),
                allowed_manifest_roots=(),
                require_edge_runtime=False,
            )
            service = OpheliaDaemon(config, runner=FakeRunner())
            service.store.register_host(
                host_id=config.host_id,
                capabilities=service.capabilities(),
                agent_version="0.6.0",
                protocol_version=1,
            )
            command = _agent_command(
                sequence=1,
                operation="host.backup.create",
                payload={
                    "backup_id": "backup_control-plane",
                    "destination_root": "/srv/backups",
                },
            )
            agent = OutboundHostAgent(
                service,
                FakeAgentTransport([_agent_response(commands=[command])]),
                command_verifier=lambda _: "sha256:" + "a" * 64,
            )
            with mock.patch.object(
                service,
                "create_host_backup",
                return_value={"status": "succeeded"},
            ) as create:
                agent.run_once()

            create.assert_called_once_with(
                backup_id="backup_control-plane",
                destination_root=Path("/srv/backups"),
            )
            self.assertEqual(
                "succeeded",
                agent.store.pending_results(config.host_id)[0]["result"]["status"],
            )

    def test_signed_command_is_verified_before_execution(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            private_key = root / "decision-private.pem"
            public_key = root / "decision-public.pem"
            subprocess.run(
                [
                    "openssl",
                    "genpkey",
                    "-algorithm",
                    "RSA",
                    "-pkeyopt",
                    "rsa_keygen_bits:2048",
                    "-out",
                    str(private_key),
                ],
                check=True,
                capture_output=True,
            )
            subprocess.run(
                [
                    "openssl",
                    "pkey",
                    "-in",
                    str(private_key),
                    "-pubout",
                    "-out",
                    str(public_key),
                ],
                check=True,
                capture_output=True,
            )
            config = DaemonConfig(
                host_id="host_fixture-1",
                runtime_root=root / "runtime",
                socket_path=root / "opheliad.sock",
                allowed_uids=(os.geteuid(),),
                allowed_manifest_roots=(root,),
                decision_public_key_path=public_key,
                require_edge_runtime=False,
            )
            service = OpheliaDaemon(config, runner=FakeRunner())
            service.store.register_host(
                host_id=config.host_id,
                capabilities=service.capabilities(),
                agent_version="0.6.0",
                protocol_version=1,
            )
            command = _agent_command(
                sequence=1,
                operation="host.drain",
                payload={"enabled": True},
            )
            command["signature"] = _sign_document(command, private_key, root)
            agent = OutboundHostAgent(
                service,
                FakeAgentTransport([_agent_response(commands=[command])]),
            )

            agent.run_once()

            self.assertTrue(service.store.host(config.host_id)["drained"])

    def test_exchange_executes_control_command_and_replays_result_until_ack(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            config = DaemonConfig(
                host_id="host_fixture-1",
                runtime_root=root / "runtime",
                socket_path=root / "opheliad.sock",
                allowed_uids=(os.geteuid(),),
                allowed_manifest_roots=(root,),
                require_edge_runtime=False,
            )
            service = OpheliaDaemon(config, runner=FakeRunner())
            service.store.register_host(
                host_id=config.host_id,
                capabilities=service.capabilities(),
                agent_version="0.6.0",
                protocol_version=1,
            )
            command = _agent_command(
                sequence=1,
                operation="host.drain",
                payload={"enabled": True},
            )
            transport = FakeAgentTransport(
                [
                    _agent_response(commands=[command]),
                    _agent_response(acknowledged_command_sequence=1),
                ]
            )
            agent = OutboundHostAgent(
                service,
                transport,
                command_verifier=lambda _: "sha256:" + "a" * 64,
            )

            agent.run_once()
            self.assertTrue(service.store.host(config.host_id)["drained"])
            self.assertEqual(1, len(agent.store.pending_results(config.host_id)))
            agent.run_once()

            self.assertEqual(1, len(transport.requests[1]["command_results"]))
            self.assertEqual(1, agent.store.state(config.host_id)["acknowledged_command_sequence"])

    def test_invalid_command_batch_does_not_commit_result_acknowledgement(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            config = DaemonConfig(
                host_id="host_fixture-1",
                runtime_root=root / "runtime",
                socket_path=root / "opheliad.sock",
                allowed_uids=(os.geteuid(),),
                allowed_manifest_roots=(root,),
                require_edge_runtime=False,
            )
            service = OpheliaDaemon(config, runner=FakeRunner())
            service.store.register_host(
                host_id=config.host_id,
                capabilities=service.capabilities(),
                agent_version="0.6.0",
                protocol_version=1,
            )
            command = _agent_command(
                sequence=1,
                operation="host.drain",
                payload={"enabled": True},
            )
            invalid_response = _agent_response(
                acknowledged_command_sequence=1,
                commands=[{"schema_version": 1}],
            )
            agent = OutboundHostAgent(
                service,
                FakeAgentTransport(
                    [_agent_response(commands=[command]), invalid_response]
                ),
                command_verifier=lambda _: "sha256:" + "a" * 64,
            )

            agent.run_once()
            with self.assertRaisesRegex(ValueError, "command fields"):
                agent.run_once()

            self.assertEqual(
                0,
                agent.store.state(config.host_id)["acknowledged_command_sequence"],
            )
            self.assertEqual(1, len(agent.store.pending_results(config.host_id)))

    def test_invalid_command_payload_is_durably_rejected_without_storing_it(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            config = DaemonConfig(
                host_id="host_fixture-1",
                runtime_root=root / "runtime",
                socket_path=root / "opheliad.sock",
                allowed_uids=(os.geteuid(),),
                allowed_manifest_roots=(root,),
                require_edge_runtime=False,
            )
            service = OpheliaDaemon(config, runner=FakeRunner())
            service.store.register_host(
                host_id=config.host_id,
                capabilities=service.capabilities(),
                agent_version="0.6.0",
                protocol_version=1,
            )
            secret_like_value = "must-not-enter-the-journal"
            transport = FakeAgentTransport(
                [
                    _agent_response(
                        commands=[
                            _agent_command(
                                sequence=1,
                                operation="unsupported.operation",
                                payload={"credential": secret_like_value},
                            )
                        ]
                    )
                ]
            )
            agent = OutboundHostAgent(
                service,
                transport,
                command_verifier=lambda _: "sha256:" + "a" * 64,
            )

            agent.run_once()

            result = agent.store.pending_results(config.host_id)[0]
            self.assertEqual("failed", result["state"])
            connection = service.journal._connect()
            try:
                stored = connection.execute(
                    "SELECT payload_json FROM agent_commands WHERE sequence = 1"
                ).fetchone()[0]
            finally:
                connection.close()
            self.assertNotIn(secret_like_value, stored)
            self.assertIn("payload_digest", stored)

    def test_manifest_bundle_is_materialized_and_never_persisted_in_command_payload(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            config = DaemonConfig(
                host_id="host_fixture-1",
                runtime_root=root / "runtime",
                socket_path=root / "opheliad.sock",
                allowed_uids=(os.geteuid(),),
                allowed_manifest_roots=(root,),
                require_edge_runtime=False,
            )
            service = OpheliaDaemon(config, runner=FakeRunner())
            service.store.register_host(
                host_id=config.host_id,
                capabilities=service.capabilities(),
                agent_version="0.6.0",
                protocol_version=1,
            )
            manifest = f"""
version: 2
app: agent-demo
environment: staging
artifacts: {{app: {{image: "{PINNED_IMAGE}"}}}}
workloads:
  worker: {{kind: worker, artifact: app}}
routes: []
update: {{strategy: recreate}}
"""
            transport = FakeAgentTransport(
                [
                    _agent_response(
                        commands=[
                            _agent_command(
                                sequence=1,
                                operation="manifest.plan",
                                payload={"manifest": manifest, "files": []},
                            )
                        ]
                    ),
                    _agent_response(acknowledged_command_sequence=1),
                ]
            )
            agent = OutboundHostAgent(
                service,
                transport,
                command_verifier=lambda _: "sha256:" + "a" * 64,
            )
            agent.run_once()

            result = agent.store.pending_results(config.host_id)[0]["result"]["result"]
            inbox = config.runtime_root / "inbox" / "command_fixture-1"
            self.assertTrue(inbox.is_dir())
            self.assertNotIn("confirmation_token", result)
            self.assertEqual("sha256:", result["request_digest"][:7])
            self.assertEqual("sha256:", result["observed_state_digest"][:7])
            self.assertEqual("sha256:", result["policy_digest"][:7])
            connection = service.journal._connect()
            try:
                payload_json = connection.execute(
                    "SELECT payload_json FROM agent_commands WHERE sequence = 1"
                ).fetchone()[0]
            finally:
                connection.close()
            self.assertNotIn(manifest.strip(), payload_json)
            self.assertIn("bundle_digest", payload_json)

            agent.run_once()

            self.assertFalse(inbox.exists())
            self.assertEqual((), agent.store.acknowledged_bundle_ids(1))
            connection = service.journal._connect()
            try:
                pruned_at = connection.execute(
                    "SELECT bundle_pruned_at FROM agent_commands WHERE sequence = 1"
                ).fetchone()[0]
            finally:
                connection.close()
            self.assertIsNotNone(pruned_at)

    def test_signed_lumen_decision_binds_plan_without_persisting_nonce(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            runtime = root / "runtime"
            manifest = root / "app.ophelia.yml"
            manifest.write_text(
                f"""
version: 2
app: decision-demo
environment: staging
artifacts: {{app: {{image: "{PINNED_IMAGE}"}}}}
workloads:
  worker: {{kind: worker, artifact: app}}
routes: []
update: {{strategy: recreate}}
"""
            )
            service = OpheliaDaemon(
                DaemonConfig(
                    host_id="host_fixture-1",
                    runtime_root=runtime,
                    socket_path=root / "opheliad.sock",
                    allowed_uids=(os.geteuid(),),
                    allowed_manifest_roots=(root,),
                    require_edge_runtime=False,
                ),
                runner=FakeRunner(),
            )
            service.store.register_host(
                host_id=service.config.host_id,
                capabilities=service.capabilities(),
                agent_version="0.6.0",
                protocol_version=1,
            )
            actor = Actor("actor_lumen-1", "test", "mutual-tls-host-agent")
            report = service.plan(manifest, actor=actor, idempotency_key="decision-plan")
            loaded = load_manifest_v2_plan(runtime, report["plan_id"])
            private_key = root / "decision-private.pem"
            public_key = root / "decision-public.pem"
            subprocess.run(
                ["openssl", "genpkey", "-algorithm", "RSA", "-pkeyopt", "rsa_keygen_bits:2048", "-out", str(private_key)],
                check=True,
                capture_output=True,
            )
            subprocess.run(
                ["openssl", "pkey", "-in", str(private_key), "-pubout", "-out", str(public_key)],
                check=True,
                capture_output=True,
            )
            plan = loaded["plan"]
            now = datetime.now(timezone.utc)
            signed = {
                "schema_version": 1,
                "kind": "lumen.ophelia-decision",
                "plan_id": plan.plan_id,
                "plan_digest": plan.plan_digest(),
                "request_digest": plan.request_digest,
                "manifest_digest": plan.manifest_digest,
                "artifact_digests": list(plan.artifact_digests),
                "observed_state_digest": plan.observed_state_digest,
                "policy_digest": plan.policy_digest,
                "host_id": plan.host_id,
                "app": plan.app,
                "environment": plan.environment,
                "revision_id": plan.revision_id,
                "revision_digest": plan.revision_digest,
                "actor_id": actor.actor_id,
                "decision_id": "decision_lumen-1",
                "issuer": "lumen-control-plane",
                "audience": plan.host_id,
                "approved_at": now.isoformat().replace("+00:00", "Z"),
                "expires_at": (now + timedelta(minutes=10)).isoformat().replace("+00:00", "Z"),
                "nonce": "one-time-nonce",
            }
            claim_path = root / "claim.json"
            signature_path = root / "claim.sig"
            claim_path.write_text(canonical_json(signed))
            subprocess.run(
                ["openssl", "dgst", "-sha256", "-sign", str(private_key), "-out", str(signature_path), str(claim_path)],
                check=True,
                capture_output=True,
            )
            signed["signature"] = base64.b64encode(signature_path.read_bytes()).decode()

            approval = verify_lumen_decision(
                signed,
                plan=plan,
                actor=actor,
                public_key_path=public_key,
                runtime_root=runtime,
                now=now,
            )

            self.assertEqual("lumen_decision", approval.authorization_kind.value)
            self.assertNotIn("one-time-nonce", json.dumps(approval.to_dict()))


class DaemonServiceTests(unittest.TestCase):
    def test_plan_submit_recover_and_task_use_one_host_authority(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            runtime = root / "runtime"
            manifest = root / "app.ophelia.yml"
            manifest.write_text(
                f"""
version: 2
app: daemon-demo
environment: staging
artifacts: {{app: {{image: "{PINNED_IMAGE}"}}}}
workloads:
  worker: {{kind: worker, artifact: app}}
  maintenance: {{kind: task, artifact: app, command: ["./maintenance"]}}
routes: []
update: {{strategy: recreate}}
"""
            )
            config = DaemonConfig(
                host_id="host_fixture-1",
                runtime_root=runtime,
                socket_path=root / "opheliad.sock",
                allowed_uids=(os.geteuid(),),
                allowed_manifest_roots=(root,),
                require_edge_runtime=False,
            )
            runner = FakeRunner()
            daemon = OpheliaDaemon(config, runner=runner)
            daemon.store.register_host(
                host_id=config.host_id,
                capabilities=daemon.capabilities(),
                agent_version="0.6.0",
                protocol_version=1,
            )
            actor = Actor("actor_unix-501", "test", "unix-peer-credentials")
            linked_manifest = root / "linked.ophelia.yml"
            linked_manifest.symlink_to(manifest)
            with self.assertRaises(PermissionError):
                daemon.plan(linked_manifest, actor=actor, idempotency_key="linked")
            plan = daemon.plan(manifest, actor=actor, idempotency_key="deploy-1")
            retry = daemon.plan(manifest, actor=actor, idempotency_key="deploy-1")
            self.assertEqual(plan, retry)
            manifest.write_text(manifest.read_text().replace("./maintenance", "./repair"))
            with self.assertRaises(IdempotencyConflict):
                daemon.plan(manifest, actor=actor, idempotency_key="deploy-1")
            manifest.write_text(manifest.read_text().replace("./repair", "./maintenance"))
            accepted = daemon.submit_operation(
                plan["plan_id"], plan["confirmation_token"], actor=actor
            )
            operation_id = accepted["operation"]["operation_id"]
            receipt = daemon.executor.run(operation_id, owner_id="test-worker")

            task = daemon.accept_task(
                actor=actor,
                app="daemon-demo",
                environment="staging",
                workload_name="maintenance",
                idempotency_key="task-1",
            )
            claimed = daemon.store.acquire_run("task-worker")
            terminal = daemon.workloads.execute(claimed, owner_id="task-worker")

            self.assertEqual("succeeded", receipt.outcome.value)
            self.assertEqual(task["run_id"], terminal.run_id)
            self.assertEqual("succeeded", terminal.state)
            self.assertTrue(any("run" in command and "maintenance" in command for command in runner.commands))

    def test_unix_api_derives_peer_identity_and_enforces_protocol_envelopes(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            config = DaemonConfig(
                host_id="host_fixture-1",
                runtime_root=root / "runtime",
                socket_path=root / "opheliad.sock",
                allowed_uids=(os.geteuid(),),
                allowed_manifest_roots=(root,),
                require_edge_runtime=False,
            )
            daemon = OpheliaDaemon(config, runner=FakeRunner())
            daemon.store.register_host(
                host_id=config.host_id,
                capabilities=daemon.capabilities(),
                agent_version="0.6.0",
                protocol_version=1,
            )
            daemon._record_observation()
            server = create_unix_server(daemon)
            thread = threading.Thread(target=server.serve_forever, daemon=True)
            thread.start()
            try:
                connection = UnixHTTPConnection(config.socket_path)
                connection.request(
                    "GET",
                    "/v1/capabilities",
                    headers={
                        "X-Request-ID": "request-api-1",
                        "X-Ophelia-Protocol-Version": "1",
                    },
                )
                response = connection.getresponse()
                payload = json.loads(response.read())
                connection.close()

                self.assertEqual(200, response.status)
                self.assertTrue(payload["ok"])
                self.assertEqual(1, payload["protocol_version"])

                connection = UnixHTTPConnection(config.socket_path)
                connection.request(
                    "GET",
                    "/v1/observations/latest",
                    headers={
                        "X-Request-ID": "request-observation-1",
                        "X-Ophelia-Protocol-Version": "1",
                    },
                )
                response = connection.getresponse()
                payload = json.loads(response.read())
                connection.close()
                self.assertEqual(200, response.status)
                self.assertEqual(
                    config.host_id,
                    payload["data"]["observation"]["host_id"],
                )

                connection = UnixHTTPConnection(config.socket_path)
                connection.request(
                    "POST",
                    "/v1/host/drain",
                    body=json.dumps({"enabled": True}),
                    headers={
                        "Content-Type": "application/json",
                        "X-Request-ID": "request-api-2",
                        "X-Ophelia-Protocol-Version": "1",
                        "Idempotency-Key": "drain-1",
                    },
                )
                response = connection.getresponse()
                payload = json.loads(response.read())
                connection.close()

                self.assertEqual(200, response.status)
                self.assertTrue(payload["data"]["drained"])

                manifest = root / "app.ophelia.yml"
                manifest.write_text(
                    f"""
version: 2
app: api-demo
environment: staging
artifacts: {{app: {{image: "{PINNED_IMAGE}"}}}}
workloads:
  worker: {{kind: worker, artifact: app}}
routes: []
update: {{strategy: recreate}}
"""
                )
                headers = {
                    "Content-Type": "application/json",
                    "X-Request-ID": "request-api-plan-1",
                    "X-Ophelia-Protocol-Version": "1",
                    "Idempotency-Key": "plan-api-1",
                }
                connection = UnixHTTPConnection(config.socket_path)
                connection.request(
                    "POST",
                    "/v1/plans",
                    body=json.dumps({"manifest_path": str(manifest)}),
                    headers=headers,
                )
                response = connection.getresponse()
                response.read()
                connection.close()
                self.assertEqual(201, response.status)

                manifest.write_text(manifest.read_text().replace("worker:", "processor:"))
                headers["X-Request-ID"] = "request-api-plan-2"
                connection = UnixHTTPConnection(config.socket_path)
                connection.request(
                    "POST",
                    "/v1/plans",
                    body=json.dumps({"manifest_path": str(manifest)}),
                    headers=headers,
                )
                response = connection.getresponse()
                payload = json.loads(response.read())
                connection.close()
                self.assertEqual(409, response.status)
                self.assertEqual("idempotency_conflict", payload["data"]["error"]["code"])
            finally:
                server.shutdown()
                server.server_close()
                thread.join(5)


class UnixHTTPConnection(http.client.HTTPConnection):
    def __init__(self, socket_path: Path) -> None:
        super().__init__("localhost")
        self.socket_path = socket_path

    def connect(self) -> None:
        self.sock = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
        self.sock.connect(str(self.socket_path))


def _option(command: Sequence[str], name: str) -> str:
    try:
        return command[command.index(name) + 1]
    except (ValueError, IndexError):
        return ""


def _agent_command(*, sequence: int, operation: str, payload: dict) -> dict:
    now = datetime.now(timezone.utc)
    scope = {
        "host.drain": "ophelia:host:drain",
        "host.maintenance": "ophelia:host:maintenance",
        "workload.run": "ophelia:workload:run",
        "operation.cancel": "ophelia:operation:cancel",
        "manifest.plan": "ophelia:plan:create",
        "deploy.apply": "ophelia:deploy:apply",
        "host.certificate.rotate": "ophelia:host:identity:rotate",
        "host.upgrade": "ophelia:host:upgrade",
        "host.backup.create": "ophelia:backup:create",
    }.get(operation, "ophelia:" + operation)
    return {
        "schema_version": 1,
        "kind": "lumen.ophelia-command",
        "command_id": "command_fixture-%d" % sequence,
        "sequence": sequence,
        "operation": operation,
        "actor_id": "actor_lumen-1",
        "audience": "host_fixture-1",
        "scopes": [scope],
        "idempotency_key": "agent-command-%d" % sequence,
        "issued_at": now.isoformat().replace("+00:00", "Z"),
        "expires_at": (now + timedelta(minutes=10)).isoformat().replace(
            "+00:00", "Z"
        ),
        "payload": payload,
        "signature": "fixture-signature",
    }


def _agent_response(
    *,
    commands: Optional[List[dict]] = None,
    acknowledged_event_cursor: int = 0,
    acknowledged_command_sequence: int = 0,
) -> dict:
    return {
        "schema_version": 1,
        "kind": "ophelia.agent-exchange-response",
        "protocol_version": 1,
        "host_id": "host_fixture-1",
        "acknowledged_event_cursor": acknowledged_event_cursor,
        "acknowledged_command_sequence": acknowledged_command_sequence,
        "commands": commands or [],
        "poll_after_seconds": 1,
    }


def _sign_document(document: dict, private_key: Path, root: Path) -> str:
    signed = dict(document)
    signed.pop("signature", None)
    claim = root / ("signed-" + os.urandom(4).hex() + ".json")
    signature = claim.with_suffix(".sig")
    claim.write_text(canonical_json(signed))
    subprocess.run(
        [
            "openssl",
            "dgst",
            "-sha256",
            "-sign",
            str(private_key),
            "-out",
            str(signature),
            str(claim),
        ],
        check=True,
        capture_output=True,
    )
    return base64.b64encode(signature.read_bytes()).decode("ascii")


def _source_archive(path: Path, *, version: str) -> None:
    pyproject = (
        "[build-system]\nrequires = ['setuptools']\n"
        "[project]\nname = 'ophelia'\nversion = %s\n" % json.dumps(version)
    ).encode("utf-8")
    with tarfile.open(path, "w:gz") as bundle:
        member = tarfile.TarInfo("ophelia/pyproject.toml")
        member.size = len(pyproject)
        bundle.addfile(member, io.BytesIO(pyproject))


if __name__ == "__main__":
    unittest.main()
