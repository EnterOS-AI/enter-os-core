#!/usr/bin/env python3
"""Lint that every Gitea workflow job running on the self-hosted `publish`
pool declares an explicit `timeout-minutes`, so a hung Docker build/push
cannot wedge the org-wide publish runner indefinitely.

Issue #2913 — used by .gitea/workflows/lint-publish-timeout.yml.
"""
from __future__ import annotations

import glob
import os
import sys
from pathlib import Path

try:
    import yaml
except ImportError as exc:  # pragma: no cover - runner installs PyYAML
    print(f"::error::PyYAML is required: {exc}", file=sys.stderr)
    sys.exit(2)


def _iter_jobs(workflow: dict) -> list[tuple[str, dict]]:
    """Yield (job_name, job_definition) pairs from a loaded workflow."""
    jobs = workflow.get("jobs") if isinstance(workflow, dict) else None
    if not isinstance(jobs, dict):
        return []
    out: list[tuple[str, dict]] = []
    for name, body in jobs.items():
        if isinstance(body, dict):
            out.append((name, body))
    return out


def _runs_on_publish(job: dict) -> bool:
    """Return True if the job's runner is the self-hosted `publish` pool."""
    runs_on = job.get("runs-on")
    if isinstance(runs_on, str):
        return runs_on.strip() == "publish"
    if isinstance(runs_on, list):
        return any(isinstance(label, str) and label.strip() == "publish" for label in runs_on)
    return False


def main() -> int:
    workflows_dir = Path(".gitea/workflows")
    if not workflows_dir.is_dir():
        print("::error::.gitea/workflows directory not found", file=sys.stderr)
        return 2

    violations: list[str] = []
    for path in sorted(workflows_dir.glob("*.yml")):
        try:
            with path.open("r", encoding="utf-8") as f:
                workflow = yaml.safe_load(f)
        except yaml.YAMLError as exc:
            violations.append(f"{path}: invalid YAML: {exc}")
            continue

        if not isinstance(workflow, dict):
            continue

        for job_name, job in _iter_jobs(workflow):
            if not _runs_on_publish(job):
                continue
            if "timeout-minutes" not in job:
                violations.append(
                    f"{path}: job '{job_name}' runs-on: publish but missing timeout-minutes"
                )

    if violations:
        print("::error::Publish-runner timeout-minutes lint failed")
        for v in violations:
            print(f"::error::{v}")
        return 1

    print("All publish-runner jobs declare timeout-minutes")
    return 0


if __name__ == "__main__":
    sys.exit(main())
