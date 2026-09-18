"""Minimal systemd notification support without a runtime dependency."""

from __future__ import annotations

import os
import socket


def notify_systemd(message: str) -> bool:
    """Send one bounded state update when systemd supplied ``NOTIFY_SOCKET``."""

    target = os.environ.get("NOTIFY_SOCKET")
    if not target:
        return False
    if not isinstance(message, str) or not message or "\x00" in message:
        raise ValueError("systemd notification must be non-empty text without NUL bytes.")
    encoded = message.encode("utf-8")
    if len(encoded) > 4096:
        raise ValueError("systemd notification exceeds 4096 bytes.")
    address = "\x00" + target[1:] if target.startswith("@") else target
    channel = socket.socket(
        socket.AF_UNIX,
        socket.SOCK_DGRAM | getattr(socket, "SOCK_CLOEXEC", 0),
    )
    try:
        channel.connect(address)
        channel.sendall(encoded)
    finally:
        channel.close()
    return True


def watchdog_ping_seconds(environ) -> float | None:
    """Seconds between watchdog pings, or ``None`` when systemd wants none.

    systemd sets ``WATCHDOG_USEC`` to the configured ``WatchdogSec`` and expects
    a ping comfortably inside it; half the interval is the convention, so one
    lost or delayed ping is not fatal. ``WATCHDOG_PID`` scopes the contract to a
    single process when systemd sets it.
    """

    if not environ.get("NOTIFY_SOCKET"):
        return None
    raw = environ.get("WATCHDOG_USEC")
    if not raw:
        return None
    watchdog_pid = environ.get("WATCHDOG_PID")
    if watchdog_pid and watchdog_pid.strip() != str(os.getpid()):
        return None
    try:
        microseconds = int(raw)
    except (TypeError, ValueError):
        return None
    if microseconds <= 0:
        return None
    return max(1.0, microseconds / 2_000_000)
