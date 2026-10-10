#!/usr/bin/env python3
"""Run selected operational proofs on an explicitly disposable Linux machine."""
import argparse
from datetime import datetime, timezone
import hashlib
import json
import os
from pathlib import Path
import re
import signal
import shutil
import subprocess
import sys
import tempfile
import time

HERE = Path(__file__).resolve().parent
ROOT = HERE.parent
PROOFS = {
    "m1": {"title": "Journal recovery after a killed daemon", "needs": "fresh Linux with systemd and SSH", "timeout": 300},
    "m2": {"title": "Host setup, reboot, and upgrade rollback", "needs": "fresh Ubuntu/Debian; package-network access", "timeout": 1800},
    "m3": {"title": "Deploy, certificates, rollback, and failed checks", "needs": "m2; public wildcard DNS and reachable ports 80/443", "dns": True, "timeout": 1200},
    "m4": {"title": "Secrets, cron, one-offs, and private restore fixtures", "needs": "m3; --staging with sanitized sources and matching archives", "dns": True, "private": True, "timeout": 1800},
    "m5": {"title": "Backups, restore drills, email, and monitoring", "needs": "m4 apps; LANE_DW_CHAPTERS", "dns": True, "timeout": 1800},
    "m6": {"title": "Git push, isolation, DNS, and webhooks", "needs": "m5 apps; LANE_BEGAMIN_REPO; public wildcard DNS", "dns": True, "timeout": 1800},
    "m7": {"title": "Multi-workload releases and data recovery", "needs": "m2; --setup storage (or existing storage); public wildcard DNS", "dns": True, "timeout": 1800},
    "cloudflare": {"title": "Real Cloudflare record lifecycle", "needs": "m6; explicit --real-cloudflare; token already installed on disposable target", "dns": True, "timeout": 1800},
}


def stamp() -> str:
    return datetime.now(timezone.utc).isoformat()


def local(*args: str) -> str:
    return subprocess.check_output(args, cwd=ROOT, text=True).strip()


def remote(config: str, script: str, timeout: int = 60) -> subprocess.CompletedProcess:
    # Static shell program; external values are passed through the environment.
    return subprocess.run(["bash", "-c", '. "$1/common.sh"; lane_run "$2"', "lane", str(HERE), script],
                          cwd=ROOT, env={**os.environ, "LANE_CONFIG": config}, text=True, capture_output=True, timeout=timeout)


def configuration(config: str) -> dict:
    result = subprocess.run(["bash", "-c", '. "$1/common.sh"; python3 - <<\'PY\'\nimport json,os\nkeys=["LANE_PROVIDER","LANE_ID","HOST","PORT","LANE_ARCH","LANE_DOMAIN","LANE_ZONE","LANE_SITE_URL","LANE_BEGAMIN_REPO","LANE_DW_CHAPTERS","LANE_VM_IMAGE","LANE_OS_IMAGE_SHA256"]\nprint(json.dumps({k:os.environ.get(k, "") for k in keys}))\nPY', "lane", str(HERE)],
                            env={**os.environ, "LANE_CONFIG": config, "BASH_ENV": ""}, text=True, capture_output=True, timeout=60)
    if result.returncode:
        raise RuntimeError(result.stderr.strip() or "Disposable target guard failed")
    return json.loads(result.stdout)


def prerequisites(args, settings: dict) -> list[str]:
    problems = []
    for proof in args.proofs:
        spec = PROOFS[proof]
        if spec.get("dns"):
            if not settings.get("LANE_DOMAIN") or not settings.get("LANE_ZONE"):
                problems.append(f"{proof}: LANE_DOMAIN and LANE_ZONE are required for public DNS/certificate checks")
            if settings.get("LANE_PROVIDER") == "qemu-docker":
                problems.append(f"{proof}: the loopback QEMU guest is for m1/m2; public DNS/ports need a dedicated externally reachable SSH VM")
        if proof == "m4":
            stage = Path(args.staging) if args.staging else None
            if stage is None:
                problems.append("m4: pass --staging containing sanitized src/{begamin,dragon-writer} and archives")
            else:
                for name in ("src/begamin/Dockerfile", "src/dragon-writer/Dockerfile", "archives/begamin-data.tar.gz"):
                    if not (stage / name).is_file():
                        problems.append(f"m4: staging is missing {name}")
                for pattern in ("dragon_writer_*.dump", "uploads_*.tar.gz"):
                    if len(list((stage / "archives").glob(pattern))) != 1:
                        problems.append(f"m4: staging requires exactly one archives/{pattern}")
                if any(p.name == ".env" or p.name.startswith(".env.") for p in (stage / "src").rglob("*")):
                    problems.append("m4: staging sources contain an .env file; sanitize fixture sources first")
        if proof in ("m4", "m5") and not settings.get("LANE_DW_CHAPTERS", "").isdigit():
            problems.append(f"{proof}: LANE_DW_CHAPTERS must name the expected fixture record count")
        if proof in ("m4", "m6") and not settings.get("LANE_SITE_URL"):
            problems.append(f"{proof}: LANE_SITE_URL is required")
        if proof == "m6" and not Path(settings.get("LANE_BEGAMIN_REPO", "/nonexistent")).is_dir():
            problems.append("m6: LANE_BEGAMIN_REPO must point at the explicitly provided fixture repository")
        if proof == "cloudflare" and not args.real_cloudflare:
            problems.append("cloudflare: pass --real-cloudflare to authorize real DNS fixture mutations")
    return problems


def redact(text: str) -> str:
    text = re.sub(r"AGE-SECRET-KEY-1[A-Z0-9]+", "[redacted private key]", text)
    text = re.sub(r"(?im)(password|token|key|secret)\s*[=:]\s*[^\s]+", r"\1=[redacted]", text)
    # Legacy integrations print generated passwords on their own line.
    return re.sub(r"(?m)^\s*[0-9a-f]{48}\s*$", "[redacted generated password]", text)


def write_report(directory: Path, report: dict) -> None:
    completed = {item["proof"] for item in report["results"]}
    for proof in report["selected_proofs"]:
        if proof not in completed:
            report["results"].append({"proof": proof, "status": "not run"})
    report["finished_at"] = stamp()
    (directory / "report.json").write_text(json.dumps(report, indent=2) + "\n")
    lines = ["# Bedrock operational proof", "", f"Status: **{report['status']}**", f"Commit: `{report['commit']}`",
             f"Working tree: {'modified' if report['dirty'] else 'clean'}", f"Started: {report['started_at']}", f"Finished: {report['finished_at']}", ""]
    if "target" in report:
        lines += [f"Target architecture: `{report['target'].get('LANE_ARCH', 'unknown')}`", f"Provider: `{report['target'].get('LANE_PROVIDER') or 'dedicated SSH'}`", f"Guest image SHA256: `{report['target'].get('LANE_OS_IMAGE_SHA256') or 'owner-provisioned'}`", "", "```text", report.get("os", ""), "```", ""]
    if report.get("installed_binary"):
        lines += ["Installed binary:", "", "```text", report["installed_binary"], "```", ""]
    for item in report["results"]:
        lines += [f"- {item['proof']}: **{item['status']}**" + (f" ({item['seconds']:.1f}s)" if "seconds" in item else "")]
    if report.get("problems"):
        lines += ["", "## Missing prerequisites or failures", ""] + ["- " + p for p in report["problems"]]
    lines += ["", "Only the selected completed proofs are proven. A blocked or failed report is not release approval.", "", "Diagnostics contain operational state and receipts, not credential files or database contents."]
    (directory / "report.md").write_text("\n".join(lines) + "\n")


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("proofs", nargs="*", choices=list(PROOFS))
    parser.add_argument("--list", action="store_true", help="show proof scope and prerequisites without connecting")
    parser.add_argument("--check", action="store_true", help="verify selected prerequisites without running proofs")
    parser.add_argument("--config", default=os.environ.get("LANE_CONFIG", str(Path(os.environ.get("TMPDIR", "/tmp")) / f"bedrock-lane-{os.getuid()}" / "lane.env")))
    parser.add_argument("--artifacts", help="new output directory, default a temporary directory")
    parser.add_argument("--staging")
    parser.add_argument("--setup", action="append", choices=("storage", "email"), default=[])
    parser.add_argument("--real-cloudflare", action="store_true")
    args = parser.parse_args()
    if args.list:
        for name, spec in PROOFS.items():
            print(f"{name}: {spec['title']}\n  Needs: {spec['needs']}")
        return 0
    if not args.proofs:
        parser.error("select at least one proof or --list")
    directory = Path(args.artifacts).resolve() if args.artifacts else Path(tempfile.mkdtemp(prefix="bedrock-proof-"))
    if args.artifacts:
        directory.mkdir(parents=True, exist_ok=False, mode=0o700)
    directory.chmod(0o700)
    tracked = local("git", "status", "--porcelain")
    report = {"started_at": stamp(), "commit": local("git", "rev-parse", "HEAD"), "dirty": bool(tracked),
              "source_diff_sha256": hashlib.sha256(local("git", "diff", "HEAD").encode()).hexdigest(),
              "status": "blocked", "selected_proofs": args.proofs, "results": [], "problems": []}
    print(f"Proof artifacts: {directory}", flush=True)
    try:
        for tool in ("go", "ssh", "scp"):
            if shutil.which(tool) is None:
                raise RuntimeError(f"Missing local prerequisite: {tool}")
        settings = configuration(str(Path(args.config).resolve()))
        report["target"] = {k: settings[k] for k in ("LANE_PROVIDER", "LANE_ID", "LANE_ARCH", "LANE_VM_IMAGE", "LANE_OS_IMAGE_SHA256")}
        report["problems"] = prerequisites(args, settings)
        facts = remote(args.config, 'test "$(id -u)" = 0 && test "$(uname -s)" = Linux && test "$(cat /proc/1/comm)" = systemd && command -v python3 >/dev/null && . /etc/os-release && printf "%s\\n" "$PRETTY_NAME" && uname -r && uname -m')
        report["os"] = facts.stdout.strip()
        if facts.returncode:
            report["problems"].append("Target must run Linux with systemd as PID 1, root SSH, and python3: " + facts.stderr.strip())
        if "m2" in args.proofs:
            supported = remote(args.config, '. /etc/os-release; case "$ID:$VERSION_ID" in ubuntu:22.04|ubuntu:24.04|debian:12|debian:13) exit 0 ;; *) printf "unsupported host setup OS: %s %s\\n" "$ID" "$VERSION_ID" >&2; exit 1 ;; esac')
            if supported.returncode:
                report["problems"].append(supported.stderr.strip() or "m2 requires a supported Ubuntu/Debian version")
        # Fail early on implicit historical fixture dependencies.
        preceding = set()
        for proof in args.proofs:
            if proof in ("m3", "m7") and "m2" not in preceding:
                ready = remote(args.config, "command -v docker >/dev/null && systemctl is-active bedrock >/dev/null && docker info >/dev/null")
                if ready.returncode:
                    report["problems"].append(f"{proof}: host is not prepared; select m2 first")
            if proof in ("m5", "m6") and "m4" not in preceding:
                ready = remote(args.config, "bedrock ps --json")
                names = {row["app"] for row in json.loads(ready.stdout)} if ready.returncode == 0 else set()
                if not {"hello", "site", "begamin", "dragon-writer"}.issubset(names):
                    report["problems"].append(f"{proof}: m4 fixture apps are missing; select m3 m4 first")
            if proof == "m7" and "storage" not in args.setup and "m5" not in preceding:
                ready = remote(args.config, "bedrock integration list --json")
                values = json.loads(ready.stdout).get("integrations", []) if ready.returncode == 0 else []
                if not any(i["name"] == "storage" and i.get("set") for i in values):
                    report["problems"].append("m7: storage integration is missing; pass --setup storage")
            preceding.add(proof)
        if report["problems"]:
            for problem in report["problems"]:
                print("Missing prerequisite: " + problem, file=sys.stderr)
            return 2
        if args.check:
            report["status"] = "ready"
            report["results"] = [{"proof": p, "status": "not run (preflight only)"} for p in args.proofs]
            print("Prerequisites ready; no proof or fixture setup was run.")
            return 0
        env = {**os.environ, "LANE_CONFIG": str(Path(args.config).resolve())}
        fixtures_ready = not args.setup
        if args.setup and "m2" not in args.proofs:
            subprocess.run([str(HERE / "setup-fixtures.sh"), *args.setup], cwd=ROOT, env=env, check=True, timeout=300)
            fixtures_ready = True
        report["status"] = "passed"
        for proof in args.proofs:
            started = time.monotonic()
            cmd = [str(HERE / f"prove-{proof}.sh")]
            if proof == "m4":
                cmd.append(str(Path(args.staging).resolve()))
            print(f"Running {proof}: {PROOFS[proof]['title']}", flush=True)
            try:
                process = subprocess.Popen(cmd, cwd=ROOT, env=env, text=True, stdout=subprocess.PIPE, stderr=subprocess.STDOUT, start_new_session=True)
                output, _ = process.communicate(timeout=PROOFS[proof]["timeout"])
                transcript = redact(output)
                code = process.returncode
            except subprocess.TimeoutExpired:
                os.killpg(process.pid, signal.SIGKILL)
                output, _ = process.communicate()
                transcript = redact(output) + "\nProof timed out.\n"
                code = 124
            (directory / f"{proof}.log").write_text(transcript)
            print(transcript, end="", flush=True)
            report["results"].append({"proof": proof, "status": "passed" if code == 0 else "failed", "exit_code": code, "seconds": time.monotonic() - started})
            if code == 0 and proof == "m2" and not fixtures_ready:
                subprocess.run([str(HERE / "setup-fixtures.sh"), *args.setup], cwd=ROOT, env=env, check=True, timeout=300)
                fixtures_ready = True
            if code:
                report["status"] = "failed"
                report["problems"].append(f"{proof} exited {code}; see {proof}.log and diagnostics.txt")
                break
        binary = remote(args.config, 'sha256sum /usr/local/bin/bedrock; bedrock version', timeout=30)
        report["installed_binary"] = binary.stdout.strip()
        diagnostics = remote(args.config, 'systemctl show bedrock -p ActiveState -p SubState -p NRestarts; bedrock history --json --limit 10', timeout=45)
        (directory / "diagnostics.txt").write_text(redact(diagnostics.stdout + diagnostics.stderr))
        return 0 if report["status"] == "passed" else 1
    except (OSError, RuntimeError, ValueError, subprocess.SubprocessError, KeyError) as error:
        if report["results"]:
            report["status"] = "failed"
        report["problems"].append(str(error))
        print(f"Proof unavailable: {error}", file=sys.stderr)
        return 2
    finally:
        write_report(directory, report)
        print(f"Report: {directory / 'report.md'}", flush=True)


if __name__ == "__main__":
    raise SystemExit(main())
