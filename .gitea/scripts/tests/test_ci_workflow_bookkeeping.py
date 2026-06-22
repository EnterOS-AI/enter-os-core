from pathlib import Path

import yaml


ROOT = Path(__file__).resolve().parents[2]


def load_workflow(name: str) -> dict:
    with (ROOT / "workflows" / name).open() as f:
        return yaml.safe_load(f)


def _all_required(workflow: dict) -> dict:
    return workflow["jobs"]["all-required"]


def test_all_required_uses_dedicated_meta_runner_lane():
    workflow = load_workflow("ci.yml")
    all_required = _all_required(workflow)

    # Stays on the dedicated `ci-meta` lane (the sentinel does no docker
    # work, so it must NOT occupy the general docker-host pool).
    assert all_required["runs-on"] == "ci-meta"


def test_all_required_is_needs_aggregator_not_a_polling_gate():
    """fix/ci-scheduler-fanout (2026-06-01): the sentinel was converted
    from a status-polling loop (which squatted a ci-meta executor slot for
    up to 40 min per PR) into a plain `needs:` aggregator that frees the
    slot immediately. Pin the new shape so a regression to the poller is
    caught.
    """
    workflow = load_workflow("ci.yml")
    all_required = _all_required(workflow)
    rendered = str(all_required)

    # The job MUST aggregate via `needs:` (the slot-freeing design).
    assert "needs" in all_required, "all-required must be a needs: aggregator"

    # It MUST NOT reintroduce the polling loop / per-SHA status fetch that
    # was the throughput sink.
    assert "detect-changes.py" not in rendered, (
        "all-required must not run the detect-changes poller path"
    )
    assert "commits/" not in rendered and "statuses" not in rendered, (
        "all-required must not poll commit statuses (the slot-squat path)"
    )


def test_all_required_does_not_use_if_always():
    """Plain `needs:` works on Gitea 1.22.6 / act_runner v0.6.1; `needs:` +
    `if: always()` is BROKEN (feedback_gitea_needs_works_only_ifalways_broken)
    and would let a non-success need pass the gate. The sentinel must use
    plain `needs:` WITHOUT a job-level `if: always()`.
    """
    workflow = load_workflow("ci.yml")
    all_required = _all_required(workflow)

    job_if = all_required.get("if")
    assert not (isinstance(job_if, str) and "always()" in job_if), (
        "all-required must not combine needs: with if: always()"
    )


def test_all_required_needs_matches_ci_required_drift_f1_set():
    """The sentinel `needs:` list MUST equal ci-required-drift.py's
    `ci_job_names()` set: every job MINUS the sentinel itself MINUS jobs
    whose `if:` gates on github.event_name/github.ref (event-gated jobs
    skip on PRs and a `needs:` on a skipped job would never let the
    sentinel run). If they diverge, ci-required-drift F1 fires.
    """
    workflow = load_workflow("ci.yml")
    jobs = workflow["jobs"]
    sentinel = "all-required"

    expected = set()
    for key, body in jobs.items():
        if key == sentinel:
            continue
        gate = body.get("if") if isinstance(body, dict) else None
        if isinstance(gate, str) and (
            "github.event_name" in gate or "github.ref" in gate
        ):
            # event-gated → legitimately skips on some triggers; excluded
            # from both `needs:` and the F1 set.
            continue
        expected.add(key)

    needs = jobs[sentinel].get("needs", [])
    if isinstance(needs, str):
        needs = [needs]
    actual = set(needs)

    assert actual == expected, (
        f"all-required needs: {sorted(actual)} != ci_job_names() "
        f"{sorted(expected)} — ci-required-drift F1 would fire"
    )


def test_all_required_needs_reference_real_jobs():
    """F1b guard: every entry in `needs:` must name an existing job."""
    workflow = load_workflow("ci.yml")
    jobs = workflow["jobs"]
    needs = jobs["all-required"].get("needs", [])
    if isinstance(needs, str):
        needs = [needs]
    job_keys = set(jobs)
    for dep in needs:
        assert dep in job_keys, f"all-required needs unknown job {dep!r}"


def test_all_required_step_uses_extracted_script():
    """Anti-inline regression: the all-required step must invoke the
    extracted .gitea/scripts/all-required-check.sh script, not contain the
    inline check() logic. This guarantees the fail-closed contract is tested
    outside CI (test_all_required_check.sh) and cannot be silently re-inlined.
    """
    workflow = load_workflow("ci.yml")
    all_required = _all_required(workflow)
    steps = all_required.get("steps", [])
    assert steps, "all-required job has no steps"

    run_block = ""
    for step in steps:
        if step.get("name") == "Verify all aggregated CI jobs succeeded":
            run_block = step.get("run", "")
            break

    assert run_block, "all-required verify step not found"
    assert "all-required-check.sh" in run_block, (
        "all-required step must invoke .gitea/scripts/all-required-check.sh"
    )
    assert "check()" not in run_block, (
        "all-required step still contains inline check() function — re-inline regression"
    )
