#!/usr/bin/env python3
"""Own one disposable QEMU guest; never adopt or delete another container."""
import argparse
import json
import os
from pathlib import Path
import secrets
import shlex
import subprocess
import sys
import time

HERE = Path(__file__).resolve().parent
STATE = Path(os.environ.get("LANE_VM_STATE", str(Path(os.environ.get("TMPDIR", "/tmp")) / f"bedrock-lane-{os.getuid()}")))
IMAGE = "bedrock-lane-qemu:ubuntu-24.04"
LABEL = "io.bedrock.lane.id"


def command(*args: str, capture: bool = False, timeout: int = 900) -> str:
    result = subprocess.run(args, check=True, text=True, capture_output=capture, timeout=timeout)
    return result.stdout.strip() if capture else ""


def saved() -> dict:
    path = STATE / "machine.json"
    if not path.is_file():
        raise RuntimeError("No managed lane VM. Run lane/vm.sh up.")
    data = json.loads(path.read_text())
    if not isinstance(data.get("id"), str) or len(data["id"]) != 32 or any(c not in "0123456789abcdef" for c in data["id"]):
        raise RuntimeError("Invalid saved VM identity; refusing to act.")
    if data.get("container") != "bedrock-lane-" + data["id"]:
        raise RuntimeError("Saved container is not a managed lane name; refusing to act.")
    info = json.loads(command("docker", "inspect", data["container"], capture=True))[0]
    if info.get("Config", {}).get("Labels", {}).get(LABEL) != data["id"]:
        raise RuntimeError("Container identity does not match; refusing to act.")
    return data


def destroy() -> None:
    data = saved()
    if data.get("ready"):
        # An owned VM can later acquire a production marker or foreign apps.
        # Once it has booted, deletion must pass the same remote refusal guard.
        subprocess.run(["bash", "-c", '. "$1/common.sh"', "lane", str(HERE.parent)],
                       env={**os.environ, "LANE_CONFIG": str(STATE / "lane.env")}, check=True, timeout=60)
    command("docker", "rm", "-f", data["container"])
    # These are exactly the generated files owned by this provisioner.
    for name in ("machine.json", "lane.env", "ssh-key", "ssh-key.pub", "host-key", "host-key.pub", "known_hosts", "user-data", "meta-data"):
        (STATE / name).unlink(missing_ok=True)
    print("Disposable VM removed.")


def wait_ready(data: dict) -> None:
    port = command("docker", "port", data["container"], "2222/tcp", capture=True).rsplit(":", 1)[1]
    # Docker may allocate a different loopback port after a container restart.
    host_public = (STATE / "host-key.pub").read_text().strip()
    (STATE / "known_hosts").write_text(f"[127.0.0.1]:{port} {host_public}\n")
    image_id = command("docker", "inspect", "--format", "{{.Image}}", data["container"], capture=True)
    image_sha = command("docker", "exec", data["container"], "/bin/cat", "/opt/ubuntu.sha256", capture=True).split()[0]
    data["provider_image"], data["guest_image_sha256"] = image_id, image_sha
    values = {"LANE_PROVIDER": "qemu-docker", "LANE_DISPOSABLE": "true", "LANE_ID": data["id"],
              "LANE_VM_IMAGE": image_id, "LANE_OS_IMAGE_SHA256": image_sha, "HOST": "127.0.0.1", "PORT": port,
              "KEY": str(STATE / "ssh-key"), "KNOWN_HOSTS": str(STATE / "known_hosts")}
    (STATE / "lane.env").write_text("\n".join(f"{key}={shlex.quote(value)}" for key, value in values.items()) + "\n")
    (STATE / "lane.env").chmod(0o600)
    print("Waiting for the disposable Ubuntu guest (software virtualization can take several minutes)...", flush=True)
    deadline = time.monotonic() + 900
    while time.monotonic() < deadline:
        try:
            result = subprocess.run(["ssh", "-i", str(STATE / "ssh-key"), "-p", port, "-o", "BatchMode=yes", "-o", "IdentitiesOnly=yes",
                                 "-o", "IdentityAgent=none", "-o", f"UserKnownHostsFile={STATE / 'known_hosts'}", "-o", "StrictHostKeyChecking=yes",
                                 "-o", "ConnectTimeout=5", "root@127.0.0.1", "cloud-init status --wait >/dev/null && test -f /etc/bedrock-lane-id"],
                                    stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL, timeout=60)
        except subprocess.TimeoutExpired:
            time.sleep(5)
            continue
        if result.returncode == 0:
            data["ready"] = True
            (STATE / "machine.json").write_text(json.dumps(data) + "\n")
            print(f"Ready. Configuration: {STATE / 'lane.env'}")
            return
        time.sleep(5)
    raise RuntimeError(f"VM did not become ready in 15 minutes. Inspect: docker logs {data['container']}. lane/vm.sh down removes only this VM.")


def up() -> None:
    if (STATE / "machine.json").exists():
        data = saved()
        print(f"Existing managed VM: {data['container']}", flush=True)
        wait_ready(data)
        return
    command("docker", "info", "--format", "{{.OSType}}", capture=True)
    STATE.mkdir(parents=True, exist_ok=True, mode=0o700)
    STATE.chmod(0o700)
    identity = secrets.token_hex(16)
    name = "bedrock-lane-" + identity
    for key in ("ssh-key", "host-key"):
        key_path = STATE / key
        if key_path.exists():
            raise RuntimeError(f"Unowned key exists: {key_path}. Use a fresh LANE_VM_STATE directory.")
        command("ssh-keygen", "-q", "-t", "ed25519", "-N", "", "-C", "bedrock disposable lane", "-f", str(key_path))
    authorized = (STATE / "ssh-key.pub").read_text().strip()
    host_private = (STATE / "host-key").read_text().rstrip()
    host_public = (STATE / "host-key.pub").read_text().strip()
    private_block = "\n".join("    " + line for line in host_private.splitlines())
    user_data = f"""#cloud-config
hostname: bedrock-lane
disable_root: false
ssh_pwauth: false
ssh_authorized_keys:
  - {authorized}
ssh_keys:
  ed25519_private: |
{private_block}
  ed25519_public: {host_public}
write_files:
  - path: /etc/bedrock-lane-id
    owner: root:root
    permissions: '0600'
    content: '{identity}'
runcmd:
  - [sh, -c, "systemctl enable ssh; systemctl restart ssh"]
"""
    (STATE / "user-data").write_text(user_data)
    (STATE / "user-data").chmod(0o600)
    (STATE / "meta-data").write_text(f"instance-id: {identity}\nlocal-hostname: bedrock-lane\n")
    provider_image = os.environ.get("LANE_VM_PROVIDER_IMAGE")
    if provider_image:
        label = command("docker", "image", "inspect", "--format", '{{index .Config.Labels "io.bedrock.lane.provider"}}', provider_image, capture=True)
        if label != "qemu-ubuntu-24.04":
            raise RuntimeError("LANE_VM_PROVIDER_IMAGE is not a previously built lane provider image.")
    else:
        command("docker", "build", "--tag", IMAGE, str(HERE), timeout=1800)
        provider_image = IMAGE
    command("docker", "create", "--name", name, "--label", f"{LABEL}={identity}", "--publish", "127.0.0.1::2222", provider_image)
    # Record ownership immediately so a failed boot can be inspected/removed.
    image_id = command("docker", "inspect", "--format", "{{.Image}}", name, capture=True)
    image_sha = command("docker", "run", "--rm", "--entrypoint", "/bin/cat", image_id, "/opt/ubuntu.sha256", capture=True).split()[0]
    data = {"id": identity, "container": name, "provider_image": image_id, "guest_image_sha256": image_sha, "ready": False}
    (STATE / "machine.json").write_text(json.dumps(data) + "\n")
    command("docker", "cp", str(STATE / "user-data"), f"{name}:/work/user-data")
    command("docker", "cp", str(STATE / "meta-data"), f"{name}:/work/meta-data")
    command("docker", "start", name)
    port = command("docker", "port", name, "2222/tcp", capture=True).rsplit(":", 1)[1]
    (STATE / "known_hosts").write_text(f"[127.0.0.1]:{port} {host_public}\n")
    values = {"LANE_PROVIDER": "qemu-docker", "LANE_DISPOSABLE": "true", "LANE_ID": identity,
              "LANE_VM_IMAGE": image_id, "LANE_OS_IMAGE_SHA256": image_sha, "HOST": "127.0.0.1", "PORT": port, "KEY": str(STATE / "ssh-key"), "KNOWN_HOSTS": str(STATE / "known_hosts")}
    (STATE / "lane.env").write_text("\n".join(f"{key}={shlex.quote(value)}" for key, value in values.items()) + "\n")
    (STATE / "lane.env").chmod(0o600)
    wait_ready(data)


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("action", choices=("up", "status", "down", "reset"))
    args = parser.parse_args()
    try:
        if args.action == "up":
            up()
        elif args.action == "down":
            destroy()
        elif args.action == "reset":
            destroy()
            up()
        else:
            data = saved()
            print(command("docker", "inspect", "--format", "{{.State.Status}}", data["container"], capture=True))
            print(f"Configuration: {STATE / 'lane.env'}")
        return 0
    except (RuntimeError, OSError, subprocess.SubprocessError, ValueError, KeyError) as error:
        print(f"Lane VM: {error}", file=sys.stderr)
        return 2


if __name__ == "__main__":
    raise SystemExit(main())
