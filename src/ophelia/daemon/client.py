"""Small versioned client for the local ``opheliad`` Unix socket."""

from __future__ import annotations

import http.client
import json
import socket
import time
import uuid
from pathlib import Path
from typing import Any, Dict, Optional
from urllib.parse import urlencode

from .config import DEFAULT_SOCKET_PATH, PROTOCOL_VERSION


class DaemonClientError(RuntimeError):
    def __init__(self, status: int, code: str, message: str) -> None:
        self.status = status
        self.code = code
        super().__init__(message)


class _UnixConnection(http.client.HTTPConnection):
    def __init__(self, socket_path: Path, timeout: float) -> None:
        super().__init__("localhost", timeout=timeout)
        self.socket_path = Path(socket_path)

    def connect(self) -> None:
        self.sock = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
        self.sock.settimeout(self.timeout)
        self.sock.connect(str(self.socket_path))


class DaemonClient:
    def __init__(
        self,
        socket_path: Path = DEFAULT_SOCKET_PATH,
        *,
        timeout_seconds: float = 30.0,
        reconnect_seconds: float = 8.0,
    ) -> None:
        self.socket_path = Path(socket_path).expanduser()
        self.timeout_seconds = timeout_seconds
        # systemd restarts opheliad a few seconds after a stop, and the socket
        # is absent until it is listening again. A read that lands in that
        # window waits for it rather than reporting the host as unavailable,
        # which would fail a caller's whole script over a transient restart.
        self.reconnect_seconds = reconnect_seconds

    def get(self, path: str, *, query: Optional[Dict[str, object]] = None) -> Dict[str, Any]:
        if query:
            path += "?" + urlencode(query)
        return self._request("GET", path, None, idempotency_key=None)

    def post(
        self,
        path: str,
        body: Dict[str, Any],
        *,
        idempotency_key: str,
    ) -> Dict[str, Any]:
        return self._request("POST", path, body, idempotency_key=idempotency_key)

    def _request(
        self,
        method: str,
        path: str,
        body: Optional[Dict[str, Any]],
        *,
        idempotency_key: Optional[str],
    ) -> Dict[str, Any]:
        request_id = "request_ship-" + uuid.uuid4().hex
        headers = {
            "X-Request-ID": request_id,
            "X-Ophelia-Protocol-Version": str(PROTOCOL_VERSION),
        }
        encoded = None
        if body is not None:
            encoded = json.dumps(body, sort_keys=True, separators=(",", ":")).encode("utf-8")
            headers["Content-Type"] = "application/json"
            headers["Content-Length"] = str(len(encoded))
        if idempotency_key is not None:
            headers["Idempotency-Key"] = idempotency_key
        # Only a read is replayed. A write carries an idempotency key for the
        # daemon's own deduplication, but a connection that died mid-request
        # gives no proof the write did not land, so it is never resent here.
        deadline = time.monotonic() + (self.reconnect_seconds if method == "GET" else 0.0)
        while True:
            connection = _UnixConnection(self.socket_path, self.timeout_seconds)
            try:
                connection.request(method, path, body=encoded, headers=headers)
                response = connection.getresponse()
                payload = json.loads(response.read().decode("utf-8"))
                break
            except OSError as exc:
                # A missing or refused socket is a daemon that is down or
                # restarting; anything else already reached it.
                if time.monotonic() >= deadline:
                    raise DaemonClientError(
                        0,
                        "daemon_unavailable",
                        "Unable to communicate with opheliad at %s." % self.socket_path,
                    ) from exc
            except (http.client.HTTPException, UnicodeError, ValueError) as exc:
                raise DaemonClientError(
                    0,
                    "daemon_unavailable",
                    "Unable to communicate with opheliad at %s." % self.socket_path,
                ) from exc
            finally:
                connection.close()
            time.sleep(0.25)
        if not isinstance(payload, dict) or payload.get("request_id") != request_id:
            raise DaemonClientError(0, "protocol_error", "Daemon response envelope is invalid.")
        if response.status >= 400 or not payload.get("ok"):
            error = payload.get("data", {}).get("error", {})
            raise DaemonClientError(
                response.status,
                str(error.get("code", "daemon_error")),
                str(error.get("message", "Daemon request failed.")),
            )
        data = payload.get("data")
        if not isinstance(data, dict):
            raise DaemonClientError(0, "protocol_error", "Daemon response data is invalid.")
        return data
