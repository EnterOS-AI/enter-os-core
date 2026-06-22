import importlib.util
import sys
from pathlib import Path

import pytest


SCRIPT = Path(__file__).resolve().parents[1] / "prod-auto-deploy.py"
spec = importlib.util.spec_from_file_location("prod_auto_deploy", SCRIPT)
prod = importlib.util.module_from_spec(spec)
sys.modules[spec.name] = prod
spec.loader.exec_module(prod)


def test_truthy_flag_accepts_operator_disable_values():
    for value in ("1", "true", "TRUE", "yes", "on", "disabled", "disable"):
        assert prod.truthy_flag(value) is True

    for value in ("", "0", "false", "no", "off", None):
        assert prod.truthy_flag(value) is False


def test_build_plan_defaults_to_staging_sha_target_and_prod_cp():
    plan = prod.build_plan(
        {
            "GITHUB_SHA": "abcdef1234567890",
            "PROD_AUTO_DEPLOY_DISABLED": "",
        }
    )

    assert plan["enabled"] is True
    assert plan["sha"] == "abcdef1234567890"
    assert plan["target_tag"] == "staging-abcdef1"
    assert plan["cp_url"] == "https://api.moleculesai.app"
    assert plan["body"] == {
        "target_tag": "staging-abcdef1",
        "canary_slug": "hongming",
        "soak_seconds": 60,
        "batch_size": 3,
        # quarantine up to 1 individually-stuck tenant rather than blocking the
        # whole fleet deploy (default).
        "max_stragglers": 1,
        "dry_run": False,
        # cp#228 / task #308: fleet-wide intent must carry confirm:true.
        "confirm": True,
    }


def test_build_plan_always_sets_confirm_true_for_fleet_intent():
    """Regression guard: every plan body MUST carry confirm:true.

    CP /cp/admin/tenants/redeploy-fleet (cp#228) returns 400 on empty
    body / {confirm:false} / {only_slugs:[]} to prevent accidental
    fleet-wide mutation. This caller is fleet-wide intent (canary +
    fan-out, no slug scoping), so the plan MUST carry confirm:true.
    Pairs with cp#228's TestRedeployFleet_EmptyBodyReturns400 +
    TestRedeployFleet_ConfirmTrueProceeds.
    """
    plan = prod.build_plan({"GITHUB_SHA": "abcdef1234567890"})
    assert plan["body"]["confirm"] is True

    # Operator-overridable knobs do NOT drop the ack.
    plan = prod.build_plan(
        {
            "GITHUB_SHA": "abcdef1234567890",
            "PROD_AUTO_DEPLOY_SOAK_SECONDS": "0",
            "PROD_AUTO_DEPLOY_BATCH_SIZE": "10",
            "PROD_AUTO_DEPLOY_DRY_RUN": "true",
            "PROD_AUTO_DEPLOY_CANARY_SLUG": "",
        }
    )
    assert plan["body"]["confirm"] is True


def test_build_plan_rejects_non_prod_cp_without_explicit_override():
    try:
        prod.build_plan(
            {
                "GITHUB_SHA": "abcdef1234567890",
                "CP_URL": "https://staging-api.moleculesai.app",
            }
        )
    except ValueError as exc:
        assert "PROD_ALLOW_NON_PROD_CP_URL=true" in str(exc)
    else:
        raise AssertionError("expected non-prod CP URL rejection")


def test_build_plan_allows_non_prod_cp_only_with_override():
    plan = prod.build_plan(
        {
            "GITHUB_SHA": "abcdef1234567890",
            "CP_URL": "https://staging-api.moleculesai.app",
            "PROD_ALLOW_NON_PROD_CP_URL": "true",
        }
    )

    assert plan["cp_url"] == "https://staging-api.moleculesai.app"


def test_build_plan_disable_flag_short_circuits_before_credentials():
    plan = prod.build_plan(
        {
            "GITHUB_SHA": "abcdef1234567890",
            "PROD_AUTO_DEPLOY_DISABLED": "true",
        }
    )

    assert plan["enabled"] is False
    assert plan["disabled_reason"] == "PROD_AUTO_DEPLOY_DISABLED=true"


def test_latest_status_for_context_picks_newest_by_id_regardless_of_order():
    # The exhaustively-paginated /statuses list is ascending id order
    # (oldest-first), the opposite of the combined /status view. The selector
    # must collapse duplicate context rows to the NEWEST (max id) so a stale
    # earlier run never shadows the current result, whichever way they arrive.
    statuses = [
        {"id": 10, "context": "CI / all-required (push)", "status": "pending"},
        {"id": 11, "context": "CI / all-required (pull_request)", "status": "success"},
        {"id": 12, "context": "CI / all-required (push)", "status": "success"},
    ]

    latest = prod.latest_status_for_context(statuses, "CI / all-required (push)")

    assert latest == {"id": 12, "context": "CI / all-required (push)", "status": "success"}

    # Same rows shuffled (newest-first, as the combined view would deliver)
    # must still resolve to the same newest row.
    latest_rev = prod.latest_status_for_context(list(reversed(statuses)), "CI / all-required (push)")
    assert latest_rev == {"id": 12, "context": "CI / all-required (push)", "status": "success"}


def test_ci_context_state_handles_missing_and_gitea_status_key():
    assert prod.ci_context_state([], "CI / all-required (push)") == "missing"
    assert (
        prod.ci_context_state(
            [{"context": "CI / all-required (push)", "status": "success"}],
            "CI / all-required (push)",
        )
        == "success"
    )
    assert (
        prod.ci_context_state(
            [{"context": "CI / all-required (push)", "state": "failure"}],
            "CI / all-required (push)",
        )
        == "failure"
    )


def test_context_is_satisfied_accepts_only_success():
    assert prod.context_is_satisfied("success") is True
    for state in ("failure", "error", "cancelled", "canceled", "skipped", "pending", "missing"):
        assert prod.context_is_satisfied(state) is False


def test_context_is_terminal_failure_rejects_cancelled_and_skipped():
    for state in ("failure", "error", "cancelled", "canceled", "skipped"):
        assert prod.context_is_terminal_failure(state) is True
    for state in ("pending", "missing", "success"):
        assert prod.context_is_terminal_failure(state) is False


def test_default_required_contexts_delegate_path_gating_to_all_required():
    assert prod.required_contexts({}) == [
        "CI / all-required (push)",
        "Secret scan / Scan diff for credential-shaped strings (push)",
    ]


def test_slugs_from_redeploy_response_uses_controlplane_plan_rows():
    body = {
        "results": [
            {"slug": "hongming", "phase": "canary", "ssm_status": "DryRun"},
            {"slug": "tenant-a", "phase": "batch-1", "ssm_status": "DryRun"},
            {"slug": "", "phase": "batch-1", "ssm_status": "DryRun"},
            {"phase": "batch-1", "ssm_status": "DryRun"},
        ]
    }

    assert prod.slugs_from_redeploy_response(body) == ["hongming", "tenant-a"]


def test_plan_rollout_slugs_asks_controlplane_for_dry_run_plan():
    calls = []

    def fake_redeploy(_cp_url, _token, body):
        calls.append(body)
        return 200, {
            "ok": True,
            "results": [
                {"slug": "hongming", "phase": "canary", "ssm_status": "DryRun"},
                {"slug": "tenant-a", "phase": "batch-1", "ssm_status": "DryRun"},
            ],
        }

    slugs = prod.plan_rollout_slugs(
        "https://api.moleculesai.app",
        "secret",
        {
            "target_tag": "staging-abcdef1",
            "canary_slug": "hongming",
            "soak_seconds": 60,
            "batch_size": 3,
            "dry_run": False,
            "confirm": True,
        },
        redeploy=fake_redeploy,
    )

    assert slugs == ["hongming", "tenant-a"]
    assert calls == [
        {
            "target_tag": "staging-abcdef1",
            "canary_slug": "hongming",
            "soak_seconds": 60,
            "batch_size": 3,
            "dry_run": True,
            "confirm": True,
        }
    ]


def test_scoped_redeploy_body_removes_canary_and_local_soak():
    base = {
        "target_tag": "staging-abcdef1",
        "canary_slug": "hongming",
        "soak_seconds": 60,
        "batch_size": 3,
        "dry_run": False,
        "confirm": True,
    }

    scoped = prod.scoped_redeploy_body(base, ["tenant-a", "tenant-b"])

    assert scoped == {
        "target_tag": "staging-abcdef1",
        "soak_seconds": 0,
        "batch_size": 2,
        "dry_run": False,
        "confirm": True,
        "only_slugs": ["tenant-a", "tenant-b"],
    }


def test_plan_scoped_rollout_preserves_canary_then_batches():
    calls, sleeps = [], []

    def fake_list(_cp_url, _token, _body):
        return ["tenant-a", "hongming", "tenant-b", "tenant-c"]

    def fake_redeploy(_cp_url, _token, body):
        calls.append(body)
        return 200, {
            "ok": True,
            "results": [{"slug": slug, "healthz_ok": True} for slug in body["only_slugs"]],
        }

    aggregate = prod.execute_scoped_rollout(
        {
            "cp_url": "https://api.moleculesai.app",
            "body": {
                "target_tag": "staging-abcdef1",
                "canary_slug": "hongming",
                "soak_seconds": 60,
                "batch_size": 2,
                "dry_run": False,
                "confirm": True,
            },
        },
        token="secret",
        list_slugs=fake_list,
        redeploy=fake_redeploy,
        sleep=sleeps.append,
    )

    assert [call["only_slugs"] for call in calls] == [
        ["hongming"],
        ["tenant-a", "tenant-b"],
        ["tenant-c"],
    ]
    assert sleeps == [60]
    assert aggregate["ok"] is True
    assert [result["slug"] for result in aggregate["results"]] == [
        "hongming",
        "tenant-a",
        "tenant-b",
        "tenant-c",
    ]


def test_rollout_uses_elevated_http_timeout(monkeypatch):
    """RFC#2843 #41: the real rollout POST must use the elevated read budget so
    a slow/dead tenant can't time the client out before the CP returns results.
    """
    seen_timeouts = []

    def fake_cp_api_json(_method, _url, _token, body, timeout=None):
        seen_timeouts.append(timeout)
        # Return a verified result so coverage passes and the loop completes.
        return 200, {"ok": True, "results": [{"slug": s, "verified_on_target": True}
                                             for s in (body.get("only_slugs") or [])]}

    monkeypatch.setattr(prod, "cp_api_json", fake_cp_api_json)

    plan = {
        "cp_url": "https://api.moleculesai.app",
        "rollout_http_timeout": 600,
        "body": {"target_tag": "staging-abc", "batch_size": 3, "max_stragglers": 1},
    }
    prod.execute_scoped_rollout(
        plan,
        "token",
        list_slugs=lambda _u, _t, _b: ["acme-demo", "demo-tenant"],
    )
    # Every real rollout POST went through redeploy_scoped → cp_api_json with the
    # elevated budget, never the fast 120s default.
    assert seen_timeouts, "expected at least one rollout POST"
    assert all(t == 600 for t in seen_timeouts), seen_timeouts


def test_cp_api_json_socket_timeout_becomes_retryable_504(monkeypatch):
    """A bare socket read timeout must surface as a synthetic 504 (retryable),
    not crash the run with an unhandled exception (RFC#2843 #41).
    """
    def boom(_req, timeout=None):
        raise TimeoutError("read operation timed out")

    monkeypatch.setattr(prod.urllib.request, "urlopen", boom)
    status, body = prod.cp_api_json("POST", "https://api.moleculesai.app/x", "tok", {"a": 1}, timeout=5)
    assert status == 504
    assert "timed out" in body["error"]
    assert status in prod.REDEPLOY_RETRY_STATUSES


def test_build_plan_sets_rollout_timeout_default_and_floor():
    base_env = {"GITHUB_SHA": "deadbeef0000"}
    plan = prod.build_plan(dict(base_env))
    assert plan["rollout_http_timeout"] == prod.ROLLOUT_HTTP_TIMEOUT_DEFAULT_SECONDS
    # A configured value below the fast-call floor is rejected (keeps the rollout
    # budget from ever shrinking below the dry-run budget).
    import pytest as _pytest
    with _pytest.raises(ValueError):
        prod.build_plan({**base_env, "PROD_AUTO_DEPLOY_ROLLOUT_HTTP_TIMEOUT_SECONDS": "30"})


def test_redeploy_scoped_retries_transient_502_then_succeeds(monkeypatch, capfd):
    responses = [
        (502, {"error": "Bad Gateway"}),
        (503, {"error": "Service Unavailable"}),
        (200, {"ok": True, "results": [{"slug": "hongming"}]}),
    ]
    calls = []
    sleeps = []

    def fake_cp_api_json(_method, _url, _token, body, timeout=None):
        calls.append(body)
        return responses.pop(0)

    monkeypatch.setattr(prod, "cp_api_json", fake_cp_api_json)
    monkeypatch.setattr(prod.time, "sleep", sleeps.append)

    status, resp = prod.redeploy_scoped(
        "https://api.moleculesai.app", "token", {"only_slugs": ["hongming"]}
    )

    assert status == 200
    assert resp["ok"] is True
    assert len(calls) == 3
    assert sleeps == [5, 10]
    captured = capfd.readouterr().out
    assert "attempt 1/4" in captured
    assert "attempt 2/4" in captured
    assert "Bad Gateway" in captured
    assert "Service Unavailable" in captured
    assert "/cp/admin/tenants/redeploy-fleet" in captured


def test_redeploy_scoped_gives_up_after_max_retries(monkeypatch, capfd):
    responses = [
        (502, {"error": "Bad Gateway"}),
        (504, {"error": "Gateway Timeout"}),
        (503, {"error": "Service Unavailable"}),
        (503, {"error": "Service Unavailable"}),
    ]
    sleeps = []

    def fake_cp_api_json(_method, _url, _token, _body, timeout=None):
        return responses.pop(0)

    monkeypatch.setattr(prod, "cp_api_json", fake_cp_api_json)
    monkeypatch.setattr(prod.time, "sleep", sleeps.append)

    status, resp = prod.redeploy_scoped(
        "https://api.moleculesai.app", "token", {"only_slugs": ["hongming"]}
    )

    assert status == 503
    assert resp["error"] == "Service Unavailable"
    # No sleep after the final (4th) attempt.
    assert sleeps == [5, 10, 20]
    captured = capfd.readouterr().out
    assert "attempt 4/4" in captured
    assert "retries exhausted" in captured
    assert "/cp/admin/tenants/redeploy-fleet" in captured


def test_redeploy_scoped_does_not_retry_non_transient_errors(monkeypatch):
    calls = []

    def fake_cp_api_json(_method, _url, _token, body, timeout=None):
        calls.append(body)
        return 500, {"error": "Internal Server Error"}

    monkeypatch.setattr(prod, "cp_api_json", fake_cp_api_json)
    monkeypatch.setattr(prod.time, "sleep", lambda _s: pytest.fail("should not sleep on 500"))

    status, resp = prod.redeploy_scoped(
        "https://api.moleculesai.app", "token", {"only_slugs": ["hongming"]}
    )

    assert status == 500
    assert resp["error"] == "Internal Server Error"
    assert len(calls) == 1


def test_raise_for_redeploy_result_surfaces_error_body():
    with pytest.raises(RuntimeError) as exc_info:
        prod._raise_for_redeploy_result(
            502,
            {"ok": False, "error": "upstream SSM throttled"},
            ["hongming"],
        )
    assert "HTTP 502" in str(exc_info.value)
    assert "upstream SSM throttled" in str(exc_info.value)
    assert "hongming" in str(exc_info.value)


def test_scoped_rollout_halts_after_failed_canary():
    calls = []

    def fake_redeploy(_cp_url, _token, body):
        calls.append(body)
        return 200, {"ok": False, "results": [{"slug": body["only_slugs"][0], "error": "bad"}]}

    try:
        prod.execute_scoped_rollout(
            {
                "cp_url": "https://api.moleculesai.app",
                "body": {
                    "target_tag": "staging-abcdef1",
                    "canary_slug": "hongming",
                    "soak_seconds": 60,
                    "batch_size": 2,
                    "dry_run": False,
                    "confirm": True,
                },
            },
            token="secret",
            list_slugs=lambda _cp_url, _token, _body: ["hongming", "tenant-a"],
            redeploy=fake_redeploy,
            sleep=lambda _seconds: None,
        )
    except prod.RolloutFailed as exc:
        assert "redeploy scoped call failed" in str(exc)
        assert exc.response["ok"] is False
        assert exc.response["results"] == [{"slug": "hongming", "error": "bad"}]
    else:
        raise AssertionError("expected failed canary to halt rollout")

    assert [call["only_slugs"] for call in calls] == [["hongming"]]


def test_rollout_from_plan_file_writes_partial_response_on_failure(tmp_path):
    plan_path = tmp_path / "plan.json"
    response_path = tmp_path / "response.json"
    plan_path.write_text(
        """
        {
          "enabled": true,
          "cp_url": "https://api.moleculesai.app",
          "body": {"target_tag": "staging-abcdef1", "confirm": true}
        }
        """,
        encoding="utf-8",
    )

    original = prod.execute_scoped_rollout

    def fake_execute(_plan, _token):
        raise prod.RolloutFailed(
            "redeploy scoped call failed for hongming: HTTP 500, ok=false",
            {
                "ok": False,
                "error": "redeploy scoped call failed for hongming: HTTP 500, ok=false",
                "results": [{"slug": "hongming", "error": "bad"}],
            },
        )

    prod.execute_scoped_rollout = fake_execute
    try:
        try:
            prod.rollout_from_plan_file(
                str(plan_path),
                str(response_path),
                {"CP_ADMIN_API_TOKEN": "secret"},
            )
        except prod.RolloutFailed:
            pass
        else:
            raise AssertionError("expected rollout failure")
    finally:
        prod.execute_scoped_rollout = original

    assert response_path.read_text(encoding="utf-8").strip()
    assert '"ok": false' in response_path.read_text(encoding="utf-8")
    assert '"slug": "hongming"' in response_path.read_text(encoding="utf-8")


# ──────────────────────────────────────────────────────────────────────
# No-silent-skip coverage gate (internal#724)
# ──────────────────────────────────────────────────────────────────────


def test_rollout_stragglers_flags_tenant_not_on_target():
    # b SSM-succeeded but its container is on the old tag → straggler.
    stragglers = prod.rollout_stragglers(
        ["a", "b", "c"],
        [
            {"slug": "a", "verified_on_target": True},
            {"slug": "b", "verified_on_target": False, "running_image": "platform-tenant:staging-old"},
            {"slug": "c", "verified_on_target": True},
        ],
    )
    assert stragglers == ["b"]


def test_rollout_stragglers_flags_enumerated_tenant_with_no_result():
    # agents-team class: enumerated but no batch ever produced a row for it.
    stragglers = prod.rollout_stragglers(
        ["a", "agents-team"],
        [{"slug": "a", "verified_on_target": True}],
    )
    assert stragglers == ["agents-team"]


def test_rollout_stragglers_missing_key_is_backward_compatible():
    # Older CP without verified_on_target → treat as verified (no spurious fail).
    stragglers = prod.rollout_stragglers(
        ["a", "b"],
        [{"slug": "a", "healthz_ok": True}, {"slug": "b", "healthz_ok": True}],
    )
    assert stragglers == []


def test_rollout_stragglers_ignores_dry_run_rows():
    stragglers = prod.rollout_stragglers(
        ["a"], [{"slug": "a", "ssm_status": "DryRun"}]
    )
    # dry-run row is skipped, so "a" has no verifying row → straggler.
    assert stragglers == ["a"]


def test_scoped_rollout_fails_when_a_tenant_stays_on_old_tag():
    # Every per-tenant call returns ok=True, but agents-team is NOT
    # verified_on_target. The rollout must still fail loudly — this is
    # the exact "reported success, one tenant silently skipped" bug.
    def fake_redeploy(_cp_url, _token, body):
        rows = []
        for slug in body["only_slugs"]:
            rows.append({"slug": slug, "verified_on_target": slug != "agents-team"})
        return 200, {"ok": True, "results": rows}

    try:
        prod.execute_scoped_rollout(
            {
                "cp_url": "https://api.moleculesai.app",
                "body": {
                    "target_tag": "staging-new",
                    "batch_size": 5,
                    "dry_run": False,
                    "confirm": True,
                },
            },
            token="secret",
            list_slugs=lambda _u, _t, _b: ["acme-demo", "agents-team", "hongming"],
            redeploy=fake_redeploy,
            sleep=lambda _s: None,
        )
    except prod.RolloutFailed as exc:
        assert "incomplete rollout" in str(exc)
        assert exc.response["stragglers"] == ["agents-team"]
        assert exc.response["ok"] is False
    else:
        raise AssertionError("expected an incomplete rollout to fail loudly")


def test_scoped_rollout_passes_when_all_tenants_verified_on_target():
    def fake_redeploy(_cp_url, _token, body):
        return 200, {
            "ok": True,
            "results": [{"slug": s, "verified_on_target": True} for s in body["only_slugs"]],
        }

    aggregate = prod.execute_scoped_rollout(
        {
            "cp_url": "https://api.moleculesai.app",
            "body": {
                "target_tag": "staging-new",
                "batch_size": 5,
                "dry_run": False,
                "confirm": True,
            },
        },
        token="secret",
        list_slugs=lambda _u, _t, _b: ["acme-demo", "agents-team", "hongming"],
        redeploy=fake_redeploy,
        sleep=lambda _s: None,
    )
    assert aggregate["ok"] is True
    assert "stragglers" not in aggregate


def test_scoped_rollout_quarantines_straggler_within_tolerance():
    # acme-demo never verifies on target; max_stragglers=1 tolerates it — the
    # rollout still succeeds (ships to the healthy majority) and reports the
    # quarantined straggler instead of failing the whole deploy.
    def fake_redeploy(_cp_url, _token, body):
        return 200, {
            "ok": True,
            "results": [
                {"slug": s, "verified_on_target": (s != "acme-demo")}
                for s in body["only_slugs"]
            ],
        }

    aggregate = prod.execute_scoped_rollout(
        {
            "cp_url": "https://api.moleculesai.app",
            "body": {
                "target_tag": "staging-new",
                "batch_size": 5,
                "dry_run": False,
                "confirm": True,
                "max_stragglers": 1,
            },
        },
        token="secret",
        list_slugs=lambda _u, _t, _b: ["acme-demo", "agents-team", "hongming"],
        redeploy=fake_redeploy,
        sleep=lambda _s: None,
    )
    assert aggregate["ok"] is True
    assert aggregate["stragglers"] == ["acme-demo"]


def test_scoped_rollout_fails_when_stragglers_exceed_tolerance():
    # Two tenants never verify; with max_stragglers=1 that is systemic → fail.
    def fake_redeploy(_cp_url, _token, body):
        return 200, {
            "ok": True,
            "results": [
                {"slug": s, "verified_on_target": (s == "hongming")}
                for s in body["only_slugs"]
            ],
        }

    try:
        prod.execute_scoped_rollout(
            {
                "cp_url": "https://api.moleculesai.app",
                "body": {
                    "target_tag": "staging-new",
                    "batch_size": 5,
                    "dry_run": False,
                    "confirm": True,
                    "max_stragglers": 1,
                },
            },
            token="secret",
            list_slugs=lambda _u, _t, _b: ["acme-demo", "agents-team", "hongming"],
            redeploy=fake_redeploy,
            sleep=lambda _s: None,
        )
        raise AssertionError("expected RolloutFailed when stragglers exceed tolerance")
    except prod.RolloutFailed as exc:
        assert "max tolerated 1" in str(exc)


def test_scoped_rollout_dry_run_does_not_assert_coverage():
    # A dry run proves nothing landed; coverage must NOT be asserted or
    # every plan would fail.
    def fake_redeploy(_cp_url, _token, body):
        return 200, {
            "ok": True,
            "results": [{"slug": s, "ssm_status": "DryRun"} for s in body["only_slugs"]],
        }

    aggregate = prod.execute_scoped_rollout(
        {
            "cp_url": "https://api.moleculesai.app",
            "body": {
                "target_tag": "staging-new",
                "batch_size": 5,
                "dry_run": True,
                "confirm": True,
            },
        },
        token="secret",
        list_slugs=lambda _u, _t, _b: ["a", "b"],
        redeploy=fake_redeploy,
        sleep=lambda _s: None,
    )
    assert aggregate["ok"] is True


# --- Superseded-deploy guard (false-stale fix) -----------------------------
#
# Scenario this fixes: no `concurrency:` on the prod-deploy workflow means two
# close main pushes run BOTH deploy-production jobs. eb31bcf (Fix A) and 286338
# (Fix C) merge back-to-back; the 286338 job rolls the fleet to staging-2863380
# first; the OLDER eb31bcf job's strict verify then sees tenants on 2863380 and
# false-reds "stale" though the fleet is AHEAD. superseded_by detects that main's
# head is no longer eb31bcf and lets the older job succeed without weakening the
# behind-tenant signal for whichever job IS the latest.


def test_superseded_by_returns_newer_head_when_main_moved_ahead(monkeypatch):
    # eb31bcf job: main head is now 2863380 -> superseded, return the newer head.
    monkeypatch.setattr(prod, "current_branch_head", lambda _env: "2863380fullhash")
    newer = prod.superseded_by({"GITHUB_SHA": "eb31bcffullhash"})
    assert newer == "2863380fullhash"


def test_superseded_by_none_when_this_job_is_still_head(monkeypatch):
    # 2863380 job (the latest): head == our SHA -> NOT superseded -> strict verify
    # runs, so a genuinely-behind tenant still fails loudly.
    monkeypatch.setattr(prod, "current_branch_head", lambda _env: "2863380fullhash")
    assert prod.superseded_by({"GITHUB_SHA": "2863380fullhash"}) is None


def test_superseded_by_matches_on_short_vs_full_sha_prefix(monkeypatch):
    # GITHUB_SHA is full; Gitea may return a different-length id. Equal prefixes
    # must NOT count as superseded (avoid false-skipping the real latest job).
    monkeypatch.setattr(prod, "current_branch_head", lambda _env: "2863380")
    assert prod.superseded_by({"GITHUB_SHA": "2863380fullhash"}) is None
    monkeypatch.setattr(prod, "current_branch_head", lambda _env: "2863380FULLHASH")
    assert prod.superseded_by({"GITHUB_SHA": "2863380fullhash"}) is None


def test_superseded_by_fail_safe_returns_none_when_head_unreadable(monkeypatch):
    # Fail-safe: unreadable head (no token / API error) must NOT be treated as
    # superseded, so the strict verify still runs and never silently greens.
    monkeypatch.setattr(prod, "current_branch_head", lambda _env: None)
    assert prod.superseded_by({"GITHUB_SHA": "eb31bcffullhash"}) is None


def test_superseded_by_none_without_github_sha(monkeypatch):
    monkeypatch.setattr(prod, "current_branch_head", lambda _env: "2863380fullhash")
    assert prod.superseded_by({}) is None


def test_current_branch_head_parses_gitea_branch_commit_id(monkeypatch):
    captured = {}

    def fake_optional(url, _token):
        captured["url"] = url
        return 200, {"name": "main", "commit": {"id": "2863380fullhash"}}

    monkeypatch.setattr(prod, "_api_json_optional", fake_optional)
    head = prod.current_branch_head(
        {"GITEA_TOKEN": "secret", "GITHUB_REPOSITORY": "molecule-ai/molecule-core"}
    )
    assert head == "2863380fullhash"
    assert captured["url"].endswith("/repos/molecule-ai/molecule-core/branches/main")


def test_current_branch_head_uses_ref_name_branch(monkeypatch):
    captured = {}

    def fake_optional(url, _token):
        captured["url"] = url
        return 200, {"commit": {"sha": "deadbeef"}}

    monkeypatch.setattr(prod, "_api_json_optional", fake_optional)
    head = prod.current_branch_head(
        {"GITEA_TOKEN": "secret", "GITHUB_REF_NAME": "release"}
    )
    assert head == "deadbeef"
    assert captured["url"].endswith("/branches/release")


def test_current_branch_head_none_without_token():
    assert prod.current_branch_head({}) is None


def test_current_branch_head_none_on_non_200(monkeypatch):
    monkeypatch.setattr(prod, "_api_json_optional", lambda _u, _t: (500, None))
    assert prod.current_branch_head({"GITEA_TOKEN": "secret"}) is None


# --- #2213: superseded check must fire BEFORE production side effects ----------
#
# Real incident shape: two main pushes land ~2 min apart. The OLDER deploy job
# (GITHUB_SHA=7a72516, target staging-7a72516) started LATE — main head was
# already 7f25373. The #2194 guard only protected the *verify* step, so the
# older job still:
#   1. rolled the canary (hongming) BACKWARD to staging-7a72516 (the #2213 red,
#      seen as the newer job's verify reading hongming on the old SHA), then
#   2. promoted :latest backward to the older image,
# before finally skipping verify. The workflow now calls this same superseded
# check BEFORE the redeploy + promote steps and gates both off when it fires.
# These tests pin the contract that check-superseded relies on for the exact
# incident shape.


def test_superseded_by_fires_for_older_job_when_newer_already_head(monkeypatch):
    # Older job (7a72516) re-checks the head just before rollout and finds the
    # newer merge (7f25373) already owns main -> superseded -> skip side effects.
    monkeypatch.setattr(
        prod, "current_branch_head", lambda _env: "7f25373309eca54a36f08c371ff783c3a47c3f8d"
    )
    newer = prod.superseded_by(
        {"GITHUB_SHA": "7a72516f7e7ba1a710c4f393fef08be8d22e1866"}
    )
    assert newer == "7f25373309eca54a36f08c371ff783c3a47c3f8d"


def test_superseded_by_none_for_latest_job_so_it_still_rolls(monkeypatch):
    # The newer job (7f25373) IS the head -> NOT superseded -> it proceeds to
    # roll the fleet and verify, so a genuinely-behind tenant still fails loud.
    monkeypatch.setattr(
        prod, "current_branch_head", lambda _env: "7f25373309eca54a36f08c371ff783c3a47c3f8d"
    )
    assert (
        prod.superseded_by(
            {"GITHUB_SHA": "7f25373309eca54a36f08c371ff783c3a47c3f8d"}
        )
        is None
    )


# ---------------------------------------------------------------------------
# /statuses pagination — required-context SUCCESS on page 2+ must be FOUND,
# genuinely-absent context must STILL fail-closed (no fail-open).
# Regression for the single-page-status bug (#2440-family, pagination RCA):
# the combined /status view caps `statuses` at ~30, so on a high-churn commit
# the still-current required-context row is pushed past page 1 and the reader
# falsely reports it `missing`.
# ---------------------------------------------------------------------------
def _paged_statuses_stub(pages):
    """Return a fake _api_json_list that serves `pages` keyed by ?page=N."""
    def fake(url, _token):
        # url looks like .../statuses?page=N&limit=100
        page = 1
        for part in url.split("?", 1)[-1].split("&"):
            if part.startswith("page="):
                page = int(part.split("=", 1)[1])
        return pages.get(page, [])
    return fake


def test_fetch_all_statuses_finds_required_success_on_page_two(monkeypatch):
    # Page 1 is a full 100 rows of unrelated/older churn; the required-context
    # SUCCESS only appears on page 2. A single-page reader would miss it.
    page1 = [
        {"id": i, "context": f"noise-{i} (push)", "status": "pending"}
        for i in range(100)
    ]
    page2 = [
        {"id": 200, "context": "CI / all-required (push)", "status": "success"},
        {"id": 201, "context": "Secret scan / Scan diff for credential-shaped strings (push)",
         "status": "success"},
    ]
    monkeypatch.setattr(prod, "_api_json_list", _paged_statuses_stub({1: page1, 2: page2}))

    rows = prod.fetch_all_statuses("git.moleculesai.app", "molecule-ai/molecule-core", "a" * 40, "tok")
    # Must have walked to page 2 and accumulated every row.
    assert len(rows) == 102
    assert prod.ci_context_state(rows, "CI / all-required (push)") == "success"
    assert (
        prod.ci_context_state(
            rows, "Secret scan / Scan diff for credential-shaped strings (push)"
        )
        == "success"
    )


def test_fetch_all_statuses_genuinely_absent_context_stays_missing(monkeypatch):
    # The required context is on NO page → fail-closed: ci_context_state must
    # report "missing", which context_is_satisfied() rejects → gate stays shut.
    page1 = [
        {"id": i, "context": f"noise-{i} (push)", "status": "success"}
        for i in range(100)
    ]
    page2 = [{"id": 200, "context": "some-other (push)", "status": "success"}]
    monkeypatch.setattr(prod, "_api_json_list", _paged_statuses_stub({1: page1, 2: page2}))

    rows = prod.fetch_all_statuses("git.moleculesai.app", "molecule-ai/molecule-core", "b" * 40, "tok")
    state = prod.ci_context_state(rows, "CI / all-required (push)")
    assert state == "missing"
    assert prod.context_is_satisfied(state) is False


def test_fetch_all_statuses_fail_closed_on_page_error(monkeypatch):
    # A page that raises (unreadable) must propagate, never silently truncate
    # the scan and let the caller treat a partial list as complete.
    def boom(url, _token):
        if "page=2" in url:
            raise RuntimeError("GET .../statuses?page=2 -> HTTP 502: bad gateway")
        return [{"id": i, "context": f"n-{i}", "status": "success"} for i in range(100)]

    monkeypatch.setattr(prod, "_api_json_list", boom)
    try:
        prod.fetch_all_statuses("h", "r", "c" * 40, "tok")
    except RuntimeError as exc:
        assert "502" in str(exc)
    else:
        raise AssertionError("expected page-2 error to propagate (fail-closed)")


def test_wait_for_ci_context_succeeds_when_required_status_is_past_page_one(monkeypatch):
    # End-to-end: the gate reads the EXHAUSTIVE list, so a required SUCCESS that
    # only exists past page 1 lets the deploy proceed instead of timing out.
    full = [
        {"id": i, "context": f"noise-{i} (push)", "status": "success"}
        for i in range(100)
    ] + [
        {"id": 500, "context": "CI / all-required (push)", "status": "success"},
        {"id": 501, "context": "Secret scan / Scan diff for credential-shaped strings (push)",
         "status": "success"},
    ]
    monkeypatch.setattr(prod, "fetch_all_statuses", lambda *a, **k: full)
    result = prod.wait_for_ci_context(
        {"GITHUB_SHA": "d" * 40, "GITEA_TOKEN": "tok", "CI_STATUS_TIMEOUT_SECONDS": "30"}
    )
    assert result == "success"


def test_wait_for_ci_context_times_out_fail_closed_when_required_absent(monkeypatch):
    # Genuinely-absent required context across all pages → never satisfied →
    # the gate times out rather than green-lighting the deploy (no fail-open).
    present_but_irrelevant = [
        {"id": 500, "context": "some-other (push)", "status": "success"},
    ]
    monkeypatch.setattr(prod, "fetch_all_statuses", lambda *a, **k: present_but_irrelevant)
    # Zero timeout + 0 interval → single poll then TimeoutError.
    try:
        prod.wait_for_ci_context(
            {
                "GITHUB_SHA": "e" * 40,
                "GITEA_TOKEN": "tok",
                "CI_STATUS_TIMEOUT_SECONDS": "1",
                "CI_STATUS_POLL_INTERVAL_SECONDS": "1",
            }
        )
    except TimeoutError as exc:
        assert "missing" in str(exc)
    else:
        raise AssertionError("expected fail-closed TimeoutError, not a satisfied gate")
