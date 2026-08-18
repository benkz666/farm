#!/usr/bin/env python3
"""Capture and compare the context required for an apples-to-apples benchmark."""

from __future__ import annotations

import argparse
import datetime as dt
import hashlib
import json
import os
import platform
import subprocess
import sys
from pathlib import Path
from typing import Any


SCHEMA = "farm-benchmark-context/v1"
CACHE_STATES = ("cold", "hot", "not-applicable")
COMPARABLE_PATHS = (
    "environment",
    "service_topology",
    "resource_profile",
    "redis_topology",
    "cache_state.connections",
    "cache_state.actors",
    "cache_state.local_read_cache",
    "cache_state.redis_cache",
    "fixture.sha256",
    "fixture.size",
    "load_tool.sha256",
    "load_generator.host",
    "load_generator.platform",
    "load_generator.cpu_count",
    "settings",
)


def sha256_file(path: Path) -> str:
    digest = hashlib.sha256()
    with path.open("rb") as source:
        for chunk in iter(lambda: source.read(1024 * 1024), b""):
            digest.update(chunk)
    return digest.hexdigest()


def git_output(root: Path, *args: str, text: bool = True) -> str | bytes:
    return subprocess.check_output(
        ["git", "-C", str(root), *args],
        text=text,
        stderr=subprocess.DEVNULL,
    ).strip()


def source_context(root: Path) -> dict[str, Any]:
    try:
        commit = str(git_output(root, "rev-parse", "HEAD"))
        status = str(git_output(root, "status", "--porcelain=v1", "--untracked-files=all"))
        tracked_diff = bytes(git_output(root, "diff", "--binary", "HEAD", text=False))
        untracked = str(git_output(root, "ls-files", "--others", "--exclude-standard")).splitlines()
    except (OSError, subprocess.CalledProcessError):
        return {"commit": "unknown", "dirty": None, "fingerprint": "unknown"}

    digest = hashlib.sha256()
    digest.update(commit.encode())
    digest.update(tracked_diff)
    for relative in sorted(filter(None, untracked)):
        path = root / relative
        digest.update(relative.encode())
        if path.is_file():
            digest.update(sha256_file(path).encode())
    return {
        "commit": commit,
        "dirty": bool(status),
        "fingerprint": digest.hexdigest(),
        "status": status.splitlines(),
    }


def json_value(raw: str, label: str) -> Any:
    candidate = Path(raw)
    try:
        text = candidate.read_text(encoding="utf-8") if candidate.is_file() else raw
        return json.loads(text)
    except (OSError, json.JSONDecodeError) as error:
        raise SystemExit(f"{label} 必须是 JSON 或 JSON 文件路径: {error}") from error


def file_context(raw: str) -> dict[str, Any]:
    if not raw:
        return {"path": "", "sha256": "not-applicable", "size": 0}
    path = Path(raw).expanduser().resolve()
    if not path.is_file():
        raise SystemExit(f"文件不存在: {path}")
    return {"path": str(path), "sha256": sha256_file(path), "size": path.stat().st_size}


def capture(args: argparse.Namespace) -> int:
    root = Path(args.repo_root).resolve()
    context = {
        "schema": SCHEMA,
        "captured_at": dt.datetime.now(dt.timezone.utc).isoformat(),
        "environment": args.environment,
        "deployment_id": args.deployment_id,
        "service_topology": json_value(args.service_topology, "service-topology"),
        "resource_profile": json_value(args.resource_profile, "resource-profile"),
        "redis_topology": args.redis_topology,
        "cache_state": {
            "connections": args.connections,
            "actors": args.actors,
            "local_read_cache": args.local_read_cache,
            "redis_cache": args.redis_cache,
        },
        "fixture": file_context(args.fixture),
        "load_tool": file_context(args.load_tool),
        "settings": json_value(args.settings, "settings"),
        "source": source_context(root),
        "load_generator": {
            "host": platform.node(),
            "platform": platform.platform(),
            "cpu_count": os.cpu_count(),
        },
        "notes": args.notes,
    }
    output = Path(args.output).resolve()
    output.parent.mkdir(parents=True, exist_ok=True)
    output.write_text(json.dumps(context, ensure_ascii=False, indent=2, sort_keys=True) + "\n", encoding="utf-8")
    print(output)
    return 0


def path_value(data: dict[str, Any], dotted: str) -> Any:
    value: Any = data
    for part in dotted.split("."):
        if not isinstance(value, dict) or part not in value:
            return "<missing>"
        value = value[part]
    return value


def compare(args: argparse.Namespace) -> int:
    baseline = json.loads(Path(args.baseline).read_text(encoding="utf-8"))
    candidate = json.loads(Path(args.candidate).read_text(encoding="utf-8"))
    if baseline.get("schema") != SCHEMA or candidate.get("schema") != SCHEMA:
        raise SystemExit(f"只支持 {SCHEMA}")

    mismatches = []
    for dotted in COMPARABLE_PATHS:
        before, after = path_value(baseline, dotted), path_value(candidate, dotted)
        if before != after:
            mismatches.append({"field": dotted, "baseline": before, "candidate": after})

    result = {
        "comparable": not mismatches,
        "mismatches": mismatches,
        # Deployment/source identity must differ when comparing old and new
        # code. Record both for traceability, but do not reject the comparison.
        "baseline_deployment_id": path_value(baseline, "deployment_id"),
        "candidate_deployment_id": path_value(candidate, "deployment_id"),
        "source_changed": path_value(baseline, "source.fingerprint") != path_value(candidate, "source.fingerprint"),
        "baseline_commit": path_value(baseline, "source.commit"),
        "candidate_commit": path_value(candidate, "source.commit"),
    }
    print(json.dumps(result, ensure_ascii=False, indent=2, sort_keys=True))
    return 0 if not mismatches else 2


def parser() -> argparse.ArgumentParser:
    root = Path(__file__).resolve().parents[2]
    result = argparse.ArgumentParser(description=__doc__)
    commands = result.add_subparsers(dest="command", required=True)

    capture_parser = commands.add_parser("capture", help="记录一次压测的不可缺失上下文")
    capture_parser.add_argument("--output", required=True)
    capture_parser.add_argument("--repo-root", default=str(root))
    capture_parser.add_argument("--environment", required=True, help="例如 k3s-benkz")
    capture_parser.add_argument("--deployment-id", required=True, help="镜像 digest 或本次部署唯一标识")
    capture_parser.add_argument("--service-topology", required=True, help='例如 {"gateway":3,"farm":1,"social":1}')
    capture_parser.add_argument("--resource-profile", required=True, help="各服务 CPU/内存 JSON 或文件")
    capture_parser.add_argument("--redis-topology", choices=("single", "split"), required=True)
    capture_parser.add_argument("--connections", choices=CACHE_STATES, required=True)
    capture_parser.add_argument("--actors", choices=CACHE_STATES, required=True)
    capture_parser.add_argument("--local-read-cache", choices=CACHE_STATES, required=True)
    capture_parser.add_argument("--redis-cache", choices=CACHE_STATES, required=True)
    capture_parser.add_argument("--fixture", default="")
    capture_parser.add_argument("--load-tool", default="")
    capture_parser.add_argument("--settings", required=True, help="QPS、时长、并发等 JSON 或文件")
    capture_parser.add_argument("--notes", default="")
    capture_parser.set_defaults(run=capture)

    compare_parser = commands.add_parser("compare", help="拒绝比较上下文不一致的两次结果")
    compare_parser.add_argument("baseline")
    compare_parser.add_argument("candidate")
    compare_parser.set_defaults(run=compare)
    return result


def main() -> int:
    args = parser().parse_args()
    return int(args.run(args))


if __name__ == "__main__":
    sys.exit(main())
