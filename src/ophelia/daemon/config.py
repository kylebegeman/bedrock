"""Strict typed configuration for ``opheliad``."""

from __future__ import annotations

import ast
import os
import re
import stat
from dataclasses import dataclass
from pathlib import Path
from typing import Any, Mapping, Optional, Tuple
from urllib.parse import urlparse

from ..execution.legacy_adapter import local_host_id


PROTOCOL_VERSION = 1
_HOST_ID = re.compile(r"^host_[A-Za-z0-9][A-Za-z0-9._-]{0,126}$")
DEFAULT_CONFIG_PATH = Path("/etc/ophelia/agent.toml")
DEFAULT_RUNTIME_ROOT = Path("/var/lib/ophelia")
DEFAULT_IDENTITY_ROOT = Path("/var/lib/ophelia-identity")
DEFAULT_SOCKET_PATH = Path("/run/ophelia/opheliad.sock")
DEFAULT_INSTALL_ROOT = Path("/opt/ophelia")


class DaemonConfigError(ValueError):
    """Daemon configuration is missing, malformed, or unsafe."""


@dataclass(frozen=True)
class DaemonConfig:
    host_id: str
    runtime_root: Path
    socket_path: Path
    allowed_uids: Tuple[int, ...]
    allowed_manifest_roots: Tuple[Path, ...]
    install_root: Path = DEFAULT_INSTALL_ROOT
    max_request_bytes: int = 1024 * 1024
    max_workers: int = 4
    operation_poll_seconds: float = 0.5
    reconciliation_seconds: float = 15.0
    # How long reconciliation may go without completing a pass before the
    # watchdog loop stops pinging and lets systemd restart a wedged daemon. A
    # deploy can make one pass take minutes, so this is far above the usual
    # 15 second cadence while still bounding a true hang.
    watchdog_stall_seconds: float = 900.0
    integrity_check_seconds: float = 300.0
    scheduler_seconds: float = 15.0
    require_edge_runtime: bool = True
    local_planning_enabled: bool = True
    local_apply_enabled: bool = True
    agent_enabled: bool = False
    control_plane_url: Optional[str] = None
    control_plane_ca_path: Optional[Path] = None
    host_certificate_path: Optional[Path] = None
    host_private_key_path: Optional[Path] = None
    decision_public_key_path: Optional[Path] = None
    agent_poll_seconds: float = 5.0
    agent_exchange_bytes: int = 8 * 1024 * 1024
    agent_event_batch: int = 250
    agent_command_batch: int = 100
    recovery_backup_roots: Tuple[Path, ...] = ()
    recovery_age_recipient: Optional[str] = None
    recovery_max_bytes: int = 64 * 1024 * 1024 * 1024
    recovery_freshness_seconds: int = 24 * 60 * 60
    observation_seconds: float = 15.0
    observation_retention: int = 1000
    disk_warning_percent: float = 85.0
    disk_critical_percent: float = 95.0

    def redacted_dict(self) -> dict[str, Any]:
        return {
            "host_id": self.host_id,
            "runtime_root": str(self.runtime_root),
            "socket_path": str(self.socket_path),
            "install_root": str(self.install_root),
            "allowed_uids": list(self.allowed_uids),
            "allowed_manifest_roots": [str(path) for path in self.allowed_manifest_roots],
            "max_request_bytes": self.max_request_bytes,
            "max_workers": self.max_workers,
            "operation_poll_seconds": self.operation_poll_seconds,
            "reconciliation_seconds": self.reconciliation_seconds,
            "watchdog_stall_seconds": self.watchdog_stall_seconds,
            "integrity_check_seconds": self.integrity_check_seconds,
            "scheduler_seconds": self.scheduler_seconds,
            "require_edge_runtime": self.require_edge_runtime,
            "local_planning_enabled": self.local_planning_enabled,
            "local_apply_enabled": self.local_apply_enabled,
            "agent_enabled": self.agent_enabled,
            "control_plane_url": self.control_plane_url,
            "control_plane_ca_path": _path_or_none(self.control_plane_ca_path),
            "host_certificate_path": _path_or_none(self.host_certificate_path),
            "host_private_key_path": _path_or_none(self.host_private_key_path),
            "decision_public_key_path": _path_or_none(self.decision_public_key_path),
            "agent_poll_seconds": self.agent_poll_seconds,
            "agent_exchange_bytes": self.agent_exchange_bytes,
            "agent_event_batch": self.agent_event_batch,
            "agent_command_batch": self.agent_command_batch,
            "recovery_backup_roots": [str(path) for path in self.recovery_backup_roots],
            "recovery_age_recipient": self.recovery_age_recipient,
            "recovery_max_bytes": self.recovery_max_bytes,
            "recovery_freshness_seconds": self.recovery_freshness_seconds,
            "observation_seconds": self.observation_seconds,
            "observation_retention": self.observation_retention,
            "disk_warning_percent": self.disk_warning_percent,
            "disk_critical_percent": self.disk_critical_percent,
        }


_KEYS = {
    "host_id",
    "runtime_root",
    "socket_path",
    "install_root",
    "allowed_uids",
    "allowed_manifest_roots",
    "max_request_bytes",
    "max_workers",
    "operation_poll_seconds",
    "reconciliation_seconds",
    "watchdog_stall_seconds",
    "integrity_check_seconds",
    "scheduler_seconds",
    "require_edge_runtime",
    "local_planning_enabled",
    "local_apply_enabled",
    "agent_enabled",
    "control_plane_url",
    "control_plane_ca_path",
    "host_certificate_path",
    "host_private_key_path",
    "decision_public_key_path",
    "agent_poll_seconds",
    "agent_exchange_bytes",
    "agent_event_batch",
    "agent_command_batch",
    "recovery_backup_roots",
    "recovery_age_recipient",
    "recovery_max_bytes",
    "recovery_freshness_seconds",
    "observation_seconds",
    "observation_retention",
    "disk_warning_percent",
    "disk_critical_percent",
}


def load_daemon_config(
    path: Optional[Path] = None,
    *,
    runtime_root: Optional[Path] = None,
    socket_path: Optional[Path] = None,
) -> DaemonConfig:
    """Load strict TOML, or construct secure defaults when no file is present."""

    config_path = Path(path or DEFAULT_CONFIG_PATH).expanduser()
    raw: Mapping[str, Any] = {}
    if config_path.exists() or config_path.is_symlink():
        _validate_config_file(config_path)
        try:
            text = config_path.read_text(encoding="utf-8")
            try:
                import tomllib
            except ImportError:  # Python 3.9/3.10 compatibility
                document = _parse_flat_toml(text)
            else:
                document = tomllib.loads(text)
        except (OSError, UnicodeError, ValueError) as exc:
            raise DaemonConfigError("Daemon configuration is not valid TOML.") from exc
        if not isinstance(document, dict):
            raise DaemonConfigError("Daemon configuration root must be a table.")
        raw = document
    unknown = sorted(set(raw) - _KEYS)
    if unknown:
        raise DaemonConfigError("Unknown daemon configuration keys: %s" % ", ".join(unknown))

    effective_runtime = _directory_path(
        runtime_root or raw.get("runtime_root", DEFAULT_RUNTIME_ROOT),
        "runtime_root",
    )
    effective_socket = _absolute_path(
        socket_path or raw.get("socket_path", DEFAULT_SOCKET_PATH),
        "socket_path",
    )
    manifest_roots = tuple(
        _directory_path(value, "allowed_manifest_roots")
        for value in _list(raw.get("allowed_manifest_roots", []), "allowed_manifest_roots")
    )
    recovery_roots = tuple(
        _directory_path(value, "recovery_backup_roots")
        for value in _list(raw.get("recovery_backup_roots", []), "recovery_backup_roots")
    )
    allowed_uids = tuple(
        sorted(
            set(
                _bounded_int(value, "allowed_uids", minimum=0, maximum=2**31 - 1)
                for value in _list(raw.get("allowed_uids", [os.geteuid()]), "allowed_uids")
            )
        )
    )
    if not allowed_uids:
        raise DaemonConfigError("allowed_uids must contain at least one UID.")
    host_id = raw.get("host_id", local_host_id())
    if not isinstance(host_id, str) or _HOST_ID.fullmatch(host_id) is None:
        raise DaemonConfigError("host_id must be a bounded Ophelia host identifier.")
    agent_enabled = _boolean(raw.get("agent_enabled", False), "agent_enabled")
    control_plane_url = _optional_https_url(raw.get("control_plane_url"))
    ca_path = _optional_credential_path(raw.get("control_plane_ca_path"), "control_plane_ca_path")
    certificate_path = _optional_credential_path(
        raw.get("host_certificate_path"), "host_certificate_path"
    )
    private_key_path = _optional_credential_path(
        raw.get("host_private_key_path"),
        "host_private_key_path",
        private=True,
    )
    decision_key_path = _optional_credential_path(
        raw.get("decision_public_key_path"), "decision_public_key_path"
    )
    if agent_enabled and any(
        value is None
        for value in (
            control_plane_url,
            ca_path,
            certificate_path,
            private_key_path,
            decision_key_path,
        )
    ):
        raise DaemonConfigError(
            "Enabled outbound agent requires its URL, CA, host certificate, private key, and decision key."
        )
    if agent_enabled:
        assert ca_path is not None
        assert certificate_path is not None
        assert private_key_path is not None
        assert decision_key_path is not None
        identity_root = certificate_path.parent
        if private_key_path.parent != identity_root:
            raise DaemonConfigError(
                "Host certificate and private key must share one identity directory."
            )
        trust_roots = (ca_path.parent, decision_key_path.parent)
        if any(
            trust_root == identity_root
            or trust_root in identity_root.parents
            or identity_root in trust_root.parents
            for trust_root in trust_roots
        ):
            raise DaemonConfigError(
                "Control-plane trust keys must be separate from writable host identity."
            )
        _validate_credential_directory(identity_root, "host identity")
        _validate_credential_directory(ca_path.parent, "control-plane trust")
        _validate_credential_directory(decision_key_path.parent, "decision trust")
    config = DaemonConfig(
        host_id=host_id,
        runtime_root=effective_runtime,
        socket_path=effective_socket,
        allowed_uids=allowed_uids,
        allowed_manifest_roots=manifest_roots,
        install_root=_directory_path(
            raw.get("install_root", DEFAULT_INSTALL_ROOT), "install_root"
        ),
        max_request_bytes=_bounded_int(
            raw.get("max_request_bytes", 1024 * 1024),
            "max_request_bytes",
            minimum=1024,
            maximum=16 * 1024 * 1024,
        ),
        max_workers=_bounded_int(
            raw.get("max_workers", 4), "max_workers", minimum=1, maximum=64
        ),
        operation_poll_seconds=_bounded_number(
            raw.get("operation_poll_seconds", 0.5), "operation_poll_seconds", 0.05, 60
        ),
        reconciliation_seconds=_bounded_number(
            raw.get("reconciliation_seconds", 15), "reconciliation_seconds", 1, 3600
        ),
        integrity_check_seconds=_bounded_number(
            raw.get("integrity_check_seconds", 300),
            "integrity_check_seconds",
            10,
            86400,
        ),
        scheduler_seconds=_bounded_number(
            raw.get("scheduler_seconds", 15), "scheduler_seconds", 1, 60
        ),
        require_edge_runtime=_boolean(
            raw.get("require_edge_runtime", True), "require_edge_runtime"
        ),
        local_planning_enabled=_boolean(
            raw.get("local_planning_enabled", True), "local_planning_enabled"
        ),
        local_apply_enabled=_boolean(
            raw.get("local_apply_enabled", True), "local_apply_enabled"
        ),
        agent_enabled=agent_enabled,
        control_plane_url=control_plane_url,
        control_plane_ca_path=ca_path,
        host_certificate_path=certificate_path,
        host_private_key_path=private_key_path,
        decision_public_key_path=decision_key_path,
        agent_poll_seconds=_bounded_number(
            raw.get("agent_poll_seconds", 5), "agent_poll_seconds", 0.25, 300
        ),
        agent_exchange_bytes=_bounded_int(
            raw.get("agent_exchange_bytes", 8 * 1024 * 1024),
            "agent_exchange_bytes",
            minimum=64 * 1024,
            maximum=64 * 1024 * 1024,
        ),
        agent_event_batch=_bounded_int(
            raw.get("agent_event_batch", 250),
            "agent_event_batch",
            minimum=1,
            maximum=1000,
        ),
        agent_command_batch=_bounded_int(
            raw.get("agent_command_batch", 100),
            "agent_command_batch",
            minimum=1,
            maximum=1000,
        ),
        recovery_backup_roots=recovery_roots,
        recovery_age_recipient=_optional_age_recipient(
            raw.get("recovery_age_recipient")
        ),
        recovery_max_bytes=_bounded_int(
            raw.get("recovery_max_bytes", 64 * 1024 * 1024 * 1024),
            "recovery_max_bytes",
            minimum=1024 * 1024,
            maximum=1024 * 1024 * 1024 * 1024,
        ),
        recovery_freshness_seconds=_bounded_int(
            raw.get("recovery_freshness_seconds", 24 * 60 * 60),
            "recovery_freshness_seconds",
            minimum=60,
            maximum=365 * 24 * 60 * 60,
        ),
        observation_seconds=_bounded_number(
            raw.get("observation_seconds", 15), "observation_seconds", 1, 3600
        ),
        observation_retention=_bounded_int(
            raw.get("observation_retention", 1000),
            "observation_retention",
            minimum=1,
            maximum=100000,
        ),
        disk_warning_percent=_bounded_number(
            raw.get("disk_warning_percent", 85), "disk_warning_percent", 1, 99
        ),
        disk_critical_percent=_bounded_number(
            raw.get("disk_critical_percent", 95), "disk_critical_percent", 2, 100
        ),
    )
    if config.disk_warning_percent >= config.disk_critical_percent:
        raise DaemonConfigError(
            "disk_warning_percent must be lower than disk_critical_percent."
        )
    return config


def _path_or_none(value: Optional[Path]) -> Optional[str]:
    return None if value is None else str(value)


def _optional_https_url(value: object) -> Optional[str]:
    if value is None:
        return None
    if not isinstance(value, str) or len(value) > 2048:
        raise DaemonConfigError("control_plane_url must be a bounded HTTPS URL.")
    parsed = urlparse(value)
    try:
        parsed.port
    except ValueError as exc:
        raise DaemonConfigError("control_plane_url contains an invalid port.") from exc
    if (
        parsed.scheme != "https"
        or not parsed.hostname
        or parsed.username is not None
        or parsed.password is not None
        or parsed.query
        or parsed.fragment
        or any(ord(character) <= 32 or ord(character) == 127 for character in value)
    ):
        raise DaemonConfigError(
            "control_plane_url must be HTTPS without credentials, query, or fragment."
        )
    return value.rstrip("/")


def _optional_age_recipient(value: object) -> Optional[str]:
    if value is None:
        return None
    if not isinstance(value, str) or not value or len(value) > 4096 or "\x00" in value:
        raise DaemonConfigError(
            "recovery_age_recipient must be a bounded public age or SSH recipient."
        )
    native = value.startswith(("age1", "age-plugin-")) and not any(
        character.isspace() for character in value
    )
    ssh = re.fullmatch(
        r"ssh-(?:ed25519|rsa) [A-Za-z0-9+/]+={0,3}", value
    ) is not None
    if not native and not ssh:
        raise DaemonConfigError(
            "recovery_age_recipient must be a bounded public age or SSH recipient."
        )
    return value


def _optional_credential_path(
    value: object,
    field: str,
    *,
    private: bool = False,
) -> Optional[Path]:
    if value is None:
        return None
    requested = _absolute_path(value, field)
    if requested.is_symlink() or not requested.is_file():
        raise DaemonConfigError("%s must be a trusted regular file." % field)
    path = requested.resolve(strict=True)
    metadata = path.stat()
    if metadata.st_uid not in {0, os.geteuid()}:
        raise DaemonConfigError("%s has an untrusted owner." % field)
    if private and stat.S_IMODE(metadata.st_mode) & 0o077:
        raise DaemonConfigError("%s must use mode 0600 or stricter." % field)
    if not private and stat.S_IMODE(metadata.st_mode) & 0o022:
        raise DaemonConfigError("%s may not be group- or world-writable." % field)
    return path


def _validate_config_file(path: Path) -> None:
    if path.is_symlink() or not path.is_file():
        raise DaemonConfigError("Daemon configuration must be a real regular file.")
    metadata = path.stat()
    if metadata.st_uid not in {0, os.geteuid()}:
        raise DaemonConfigError("Daemon configuration must be owned by root or the Ophelia UID.")
    if stat.S_IMODE(metadata.st_mode) & 0o022:
        raise DaemonConfigError("Daemon configuration may not be group- or world-writable.")


def _validate_credential_directory(path: Path, label: str) -> None:
    if path.is_symlink() or not path.is_dir():
        raise DaemonConfigError("%s directory is unavailable or unsafe." % label)
    if stat.S_IMODE(path.stat().st_mode) & 0o022:
        raise DaemonConfigError("%s directory may not be group- or world-writable." % label)


def _absolute_path(value: object, field: str) -> Path:
    if not isinstance(value, (str, Path)):
        raise DaemonConfigError("%s must be a path." % field)
    path = Path(value).expanduser()
    if not path.is_absolute() or ".." in path.parts:
        raise DaemonConfigError("%s must be a safe absolute path." % field)
    return path


def _directory_path(value: object, field: str) -> Path:
    return _absolute_path(value, field).resolve(strict=False)


def _list(value: object, field: str) -> list[Any]:
    if not isinstance(value, list):
        raise DaemonConfigError("%s must be a list." % field)
    return value


def _bounded_int(value: object, field: str, *, minimum: int, maximum: int) -> int:
    if isinstance(value, bool) or not isinstance(value, int) or not minimum <= value <= maximum:
        raise DaemonConfigError("%s must be an integer from %d through %d." % (field, minimum, maximum))
    return value


def _bounded_number(value: object, field: str, minimum: float, maximum: float) -> float:
    if isinstance(value, bool) or not isinstance(value, (int, float)):
        raise DaemonConfigError("%s must be numeric." % field)
    parsed = float(value)
    if not minimum <= parsed <= maximum:
        raise DaemonConfigError("%s must be from %s through %s." % (field, minimum, maximum))
    return parsed


def _boolean(value: object, field: str) -> bool:
    if not isinstance(value, bool):
        raise DaemonConfigError("%s must be boolean." % field)
    return value


def _parse_flat_toml(text: str) -> dict[str, Any]:
    """Parse Ophelia's deliberately flat Python-3.9 configuration subset."""

    result: dict[str, Any] = {}
    for line_number, raw_line in enumerate(text.splitlines(), start=1):
        line = _strip_comment(raw_line).strip()
        if not line:
            continue
        if line.startswith("["):
            raise ValueError("TOML tables are not supported in agent.toml.")
        if "=" not in line:
            raise ValueError("TOML assignment is missing on line %d." % line_number)
        raw_key, raw_value = line.split("=", 1)
        key = raw_key.strip()
        if re.fullmatch(r"[A-Za-z_][A-Za-z0-9_-]*", key) is None or key in result:
            raise ValueError("TOML key is invalid or duplicated on line %d." % line_number)
        value = raw_value.strip()
        if value == "true":
            parsed: Any = True
        elif value == "false":
            parsed = False
        else:
            try:
                parsed = ast.literal_eval(value)
            except (SyntaxError, ValueError) as exc:
                try:
                    parsed = float(value) if "." in value else int(value)
                except ValueError:
                    raise ValueError("TOML value is unsupported on line %d." % line_number) from exc
        if not isinstance(parsed, (str, int, float, bool, list)):
            raise ValueError("TOML value type is unsupported on line %d." % line_number)
        result[key] = parsed
    return result


def _strip_comment(line: str) -> str:
    quote: Optional[str] = None
    escaped = False
    for index, character in enumerate(line):
        if escaped:
            escaped = False
            continue
        if character == "\\" and quote == '"':
            escaped = True
            continue
        if character in {'"', "'"}:
            quote = None if quote == character else (character if quote is None else quote)
            continue
        if character == "#" and quote is None:
            return line[:index]
    if quote is not None:
        raise ValueError("Unterminated TOML string.")
    return line
