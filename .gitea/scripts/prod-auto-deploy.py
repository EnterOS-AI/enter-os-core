#!/usr/bin/env python3
"""Production auto-deploy helpers for Gitea Actions.

The workflow keeps network side effects in shell/curl, but centralizes the
release decision shape here so it has unit coverage: disable flag parsing,
target tag selection, CP payload construction, and status-context selection.
"""

from __future__ import annotations

import argparse
import json
import os
import sys
import time
import urllib.error
import urllib.request
from urllib.parse import quote

TRUE_VALUES = {"1", "true", "yes", "on", "disabled", "disable"}
PROD_CP_URL = "https://api.moleculesai.app"
DEFAULT_REQUIRED_CONTEXTS = [
    "CI / all-required (push)",
    "Secret scan / Scan diff for credential-shaped strings (push)",
]
TERMINAL_FAILURE_STATES = {"failure", "error", "cancelled", "canceled", "skipped"}
REDEPLOY_PATH = "/cp/admin/tenants/redeploy-fleet"

# Bounded retry for transient CP/gateway failures (e.g. 502 from an upstream
# dependency like SSM during redeploy-fleet). A 502 on the canary should not
# hard-halt the whole fleet rollout if a quick retry succeeds.
REDEPLOY_RETRY_STATUSES = {502, 503, 504}
# Initial attempt + this many retries. Delays are applied BEFORE each retry.
REDEPLOY_MAX_RETRIES = 3
REDEPLOY_RETRY_DELAYS_SECONDS = [5, 10, 20]

# HTTP read budgets for the CP calls.
#
# DEFAULT_CP_HTTP_TIMEOUT_SECONDS bounds the fast calls (CI-status reads, the
# dry-run plan enumeration) — they return in well under a second.
#
# ROLLOUT_HTTP_TIMEOUT_DEFAULT_SECONDS bounds the REAL (non-dry-run) rollout
# POST, which blocks server-side for the whole batch: the CP redeploys a batch's
# tenants CONCURRENTLY and only responds once the SLOWEST one finishes — up to
# the CP's PerTenantTimeout (5m SSM default) plus the /healthz settle (90s) for a
# stuck or dead box. The old hardcoded 120s read timeout was SHORTER than that
# worst case, so a single unreachable tenant (e.g. a CF-525 box whose SSM agent
# never answers — demo-tenant, RFC#2843 #41) made this client abandon the
# call with an empty response BEFORE the CP could return the per-tenant results
# that the max_stragglers quarantine acts on. Net effect: the healthy majority
# shipped, but the deploy reported ok=false (result_count=0) and SKIPPED the
# :latest promote — so brand-new provisions kept pulling the stale image. Give
# the rollout POST a budget that comfortably exceeds the concurrent-batch worst
# case so the designed quarantine path can actually run.
DEFAULT_CP_HTTP_TIMEOUT_SECONDS = 120
ROLLOUT_HTTP_TIMEOUT_DEFAULT_SECONDS = 600


def truthy_flag(value: str | None) -> bool:
    if value is None:
        return False
    return value.strip().lower() in TRUE_VALUES


def _int_env(env: dict[str, str], name: str, default: int, minimum: int = 1) -> int:
    raw = env.get(name, "")
    if not raw:
        return default
    try:
        value = int(raw)
    except ValueError as exc:
        raise ValueError(f"{name} must be an integer, got {raw!r}") from exc
    if value < minimum:
        raise ValueError(f"{name} must be >= {minimum}, got {value}")
    return value


def build_plan(env: dict[str, str]) -> dict:
    sha = env.get("GITHUB_SHA", "").strip()
    if not sha:
        raise ValueError("GITHUB_SHA is required")

    disabled_value = env.get("PROD_AUTO_DEPLOY_DISABLED", "")
    if truthy_flag(disabled_value):
        return {
            "enabled": False,
            "sha": sha,
            "disabled_reason": f"PROD_AUTO_DEPLOY_DISABLED={disabled_value}",
        }

    short_sha = sha[:7]
    target_tag = env.get("PROD_AUTO_DEPLOY_TARGET_TAG", "").strip() or f"staging-{short_sha}"
    canary_slug = env.get("PROD_AUTO_DEPLOY_CANARY_SLUG", "hongming").strip()
    body = {
        "target_tag": target_tag,
        "soak_seconds": _int_env(env, "PROD_AUTO_DEPLOY_SOAK_SECONDS", 60, minimum=0),
        "batch_size": _int_env(env, "PROD_AUTO_DEPLOY_BATCH_SIZE", 3),
        # Tolerate a small minority of individually-stuck tenants (e.g. a wedged
        # data volume that won't recreate). They are QUARANTINED — shipped past
        # so the healthy majority still lands the build — and reported for
        # separate recovery, instead of one stuck tenant blocking the whole
        # fleet deploy. The canary still must pass, the CP halts a batch the
        # moment failures exceed this, and the cross-batch coverage gate below
        # enforces the same tolerance globally. Default 1.
        "max_stragglers": _int_env(env, "PROD_AUTO_DEPLOY_MAX_STRAGGLERS", 1, minimum=0),
        "dry_run": truthy_flag(env.get("PROD_AUTO_DEPLOY_DRY_RUN", "")),
        # confirm:true ack required by CP /cp/admin/tenants/redeploy-fleet
        # contract (cp#228 / task #308) for fleet-wide intent. Empty body
        # / {confirm:false} / {only_slugs:[]} → 400. This caller is the
        # production auto-deploy step that rolls every live tenant (canary
        # + fan-out), no slug scoping, so confirm:true is correct.
        "confirm": True,
    }
    if canary_slug:
        body["canary_slug"] = canary_slug

    cp_url = env.get("CP_URL", "").strip() or PROD_CP_URL
    if cp_url != PROD_CP_URL and not truthy_flag(env.get("PROD_ALLOW_NON_PROD_CP_URL", "")):
        raise ValueError(
            f"Refusing production deploy to CP_URL={cp_url!r}; "
            f"set PROD_ALLOW_NON_PROD_CP_URL=true for an explicit non-prod drill"
        )

    return {
        "enabled": True,
        "sha": sha,
        "short_sha": short_sha,
        "target_tag": target_tag,
        "cp_url": cp_url,
        "body": body,
        # Read budget for the real rollout POST (see ROLLOUT_HTTP_TIMEOUT_*).
        # Floor at the fast-call default so a misconfig can't shrink it below
        # the dry-run budget.
        "rollout_http_timeout": _int_env(
            env,
            "PROD_AUTO_DEPLOY_ROLLOUT_HTTP_TIMEOUT_SECONDS",
            ROLLOUT_HTTP_TIMEOUT_DEFAULT_SECONDS,
            minimum=DEFAULT_CP_HTTP_TIMEOUT_SECONDS,
        ),
    }


def latest_status_for_context(statuses: list[dict], context: str) -> dict | None:
    """Return the NEWEST status row for ``context`` (highest ``id``).

    This must work for BOTH orderings Gitea exposes: the combined
    ``/status`` view is newest-first, but the exhaustively-paginated
    ``/statuses`` list (see ``fetch_all_statuses``) is NEWEST-first on this
    Gitea (verified 2026-06-12; an older comment claimed ascending). Selecting by max ``id`` collapses duplicate context rows
    to the current one regardless of input order, so a stale earlier run can
    never shadow the latest result. Rows without an ``id`` are treated as
    oldest (id -1) so a well-formed newer row always wins.
    """
    newest: dict | None = None
    newest_id = -1
    for status in statuses:
        if status.get("context") != context:
            continue
        raw_id = status.get("id")
        sid = raw_id if isinstance(raw_id, int) else -1
        if newest is None or sid >= newest_id:
            newest = status
            newest_id = sid
    return newest


def ci_context_state(statuses: list[dict], context: str) -> str:
    status = latest_status_for_context(statuses, context)
    if not status:
        return "missing"
    return str(status.get("status") or status.get("state") or "missing").lower()


def context_is_satisfied(state: str) -> bool:
    return state == "success"


def context_is_terminal_failure(state: str) -> bool:
    return state in TERMINAL_FAILURE_STATES


def required_contexts(env: dict[str, str]) -> list[str]:
    raw = env.get("PROD_AUTO_DEPLOY_REQUIRED_CONTEXTS", "")
    if not raw.strip():
        return DEFAULT_REQUIRED_CONTEXTS
    return [line.strip() for line in raw.replace(",", "\n").splitlines() if line.strip()]


def chunks(items: list[str], size: int) -> list[list[str]]:
    return [items[i : i + size] for i in range(0, len(items), size)]


class RolloutFailed(RuntimeError):
    def __init__(self, message: str, response: dict):
        super().__init__(message)
        self.response = response


def slugs_from_redeploy_response(body: dict) -> list[str]:
    slugs: list[str] = []
    for row in body.get("results") or []:
        slug = str(row.get("slug") or "").strip()
        if slug:
            slugs.append(slug)
    return slugs


def scoped_redeploy_body(base: dict, slugs: list[str]) -> dict:
    body = dict(base)
    body.pop("canary_slug", None)
    body["only_slugs"] = slugs
    body["soak_seconds"] = 0
    body["batch_size"] = max(1, len(slugs))
    return body


def cp_api_json(
    method: str,
    url: str,
    token: str,
    body: dict | None = None,
    timeout: int = DEFAULT_CP_HTTP_TIMEOUT_SECONDS,
) -> tuple[int, dict]:
    data = None
    headers = {
        "Authorization": f"Bearer {token}",
        "Accept": "application/json",
    }
    if body is not None:
        data = json.dumps(body).encode("utf-8")
        headers["Content-Type"] = "application/json"
    req = urllib.request.Request(url, data=data, headers=headers, method=method)
    try:
        with urllib.request.urlopen(req, timeout=timeout) as resp:
            return resp.status, json.loads(resp.read())
    except urllib.error.HTTPError as exc:
        raw = exc.read().decode("utf-8", errors="replace")
        try:
            parsed = json.loads(raw)
        except json.JSONDecodeError:
            parsed = {"error": raw[:500]}
        return exc.code, parsed
    except (TimeoutError, urllib.error.URLError) as exc:
        # A socket read timeout (or transient connection drop) is NOT an HTTP
        # status — without this branch it propagates as an unhandled exception
        # and crashes the run with a bare "read operation timed out" (the
        # RFC#2843 #41 symptom). Surface it as a synthetic 504 so the bounded
        # redeploy retry treats it like an upstream gateway timeout.
        reason = getattr(exc, "reason", exc)
        return 504, {"error": f"request to {url} timed out or failed: {reason}"}


def plan_rollout_slugs(cp_url: str, token: str, body: dict, redeploy=None) -> list[str]:
    if redeploy is None:
        redeploy = redeploy_scoped
    dry_run_body = dict(body)
    dry_run_body["dry_run"] = True
    status, resp = redeploy(cp_url, token, dry_run_body)
    if status != 200:
        raise RuntimeError(f"dry-run redeploy-fleet returned HTTP {status}: {resp.get('error', '')}")
    if resp.get("ok") is not True:
        raise RuntimeError(f"dry-run redeploy-fleet reported ok={resp.get('ok')}: {resp.get('error', '')}")
    slugs = slugs_from_redeploy_response(resp)
    if not slugs:
        raise RuntimeError("dry-run redeploy-fleet returned no rollout candidates")
    return slugs


def _redeploy_error_detail(body: dict, max_len: int = 200) -> str:
    """Extract a short, safe diagnostic string from a CP error body."""
    detail = body.get("error") or body.get("message") or ""
    if not detail:
        detail = json.dumps(body)
    return detail[:max_len]


def redeploy_scoped(
    cp_url: str,
    token: str,
    body: dict,
    timeout: int = DEFAULT_CP_HTTP_TIMEOUT_SECONDS,
) -> tuple[int, dict]:
    """POST /cp/admin/tenants/redeploy-fleet with bounded transient retry.

    CP can return 502/503/504 when an upstream dependency (SSM, ECS, etc.)
    flakes. Retry a small number of times with increasing backoff before
    giving up and letting the caller surface the failure.

    ``timeout`` is the per-attempt HTTP read budget. The real rollout passes
    ROLLOUT_HTTP_TIMEOUT_* so a slow/dead tenant can't time the client out
    before the CP returns the per-tenant results (RFC#2843 #41); dry-run /
    plan calls keep the fast default.
    """
    url = f"{cp_url}{REDEPLOY_PATH}"
    slugs = body.get("only_slugs") or []
    slugs_text = ",".join(slugs)
    total_attempts = 1 + REDEPLOY_MAX_RETRIES
    status = 0
    resp: dict = {}
    for attempt in range(total_attempts):
        status, resp = cp_api_json("POST", url, token, body, timeout=timeout)
        if status not in REDEPLOY_RETRY_STATUSES:
            return status, resp
        detail = _redeploy_error_detail(resp)
        if attempt < REDEPLOY_MAX_RETRIES:
            delay = REDEPLOY_RETRY_DELAYS_SECONDS[attempt]
            print(
                f"::warning::redeploy-fleet returned HTTP {status} for "
                f"only_slugs={slugs_text} at {url} "
                f"(attempt {attempt + 1}/{total_attempts}, detail={detail!r}); "
                f"retrying in {delay}s"
            )
            time.sleep(delay)
        else:
            print(
                f"::warning::redeploy-fleet returned HTTP {status} for "
                f"only_slugs={slugs_text} at {url} "
                f"(attempt {attempt + 1}/{total_attempts}, detail={detail!r}); "
                f"retries exhausted"
            )
    return status, resp


def _raise_for_redeploy_result(status: int, body: dict, slugs: list[str]) -> None:
    if status != 200 or body.get("ok") is not True:
        # Surface the CP error body when available so the operator sees the
        # tenant-level reason (e.g. SSM timeout) instead of just the status.
        detail = _redeploy_error_detail(body, max_len=500)
        raise RuntimeError(
            "redeploy scoped call failed for "
            f"{','.join(slugs)}: HTTP {status}, ok={body.get('ok')}, detail={detail!r}"
        )


def rollout_stragglers(enumerated: list[str], results: list[dict]) -> list[str]:
    """Return every enumerated tenant NOT proven on the target build.

    A straggler is any tenant the rollout was supposed to cover that the
    CP could not verify is running the target image tag — whether it
    errored, was skipped, or SSM-succeeded onto the wrong image
    (internal#724). CP marks each per-tenant result row with
    ``verified_on_target`` (the REDEPLOY_RUNNING_IMAGE docker-inspect
    proof). A tenant enumerated for the rollout but absent from the
    result set (no batch ever ran it) is also a straggler — that is the
    exact agents-team silent-skip class.

    Backward-compat: an OLDER CP that doesn't emit ``verified_on_target``
    yet returns rows without the key. Treat a missing key as verified so
    this surfacing degrades to the previous (ok-based) behavior against an
    un-upgraded CP, rather than failing every deploy spuriously. Once the
    CP fix is deployed the key is always present and real stragglers are
    caught.
    """

    verified: set[str] = set()
    for row in results:
        if str(row.get("ssm_status") or "") == "DryRun":
            continue
        slug = str(row.get("slug") or "").strip()
        if not slug:
            continue
        # Missing key (old CP) => assume verified; present key is authoritative.
        if "verified_on_target" not in row or row.get("verified_on_target"):
            verified.add(slug)
    return sorted(s for s in dict.fromkeys(enumerated) if s not in verified)


def assert_full_coverage(
    enumerated: list[str], aggregate: dict, dry_run: bool, max_stragglers: int = 0
) -> None:
    """Gate the rollout on coverage, tolerating a quarantined straggler minority.

    This is the no-silent-skip gate (internal#724) made resilient: every
    enumerated tenant must be PROVEN on the target build, EXCEPT up to
    ``max_stragglers`` individually-stuck tenants which are quarantined (shipped
    past) and reported for separate recovery instead of blocking the whole
    fleet deploy. Exceeding the tolerance is a systemic failure → RolloutFailed.
    A dry run proves nothing landed, so coverage is not asserted for it.
    """

    if dry_run:
        return
    stragglers = rollout_stragglers(enumerated, aggregate.get("results") or [])
    if not stragglers:
        return
    # Surface the stragglers (for the step summary + recovery), gate or not.
    aggregate["stragglers"] = stragglers
    if len(stragglers) > max_stragglers:
        msg = (
            f"incomplete rollout: {len(stragglers)} tenant(s) not verified on target "
            f"after redeploy-fleet (max tolerated {max_stragglers}): {', '.join(stragglers)} "
            f"(enumerated {len(set(enumerated))})"
        )
        aggregate["ok"] = False
        aggregate["error"] = msg
        raise RolloutFailed(msg, aggregate)
    # Within tolerance: shipped to the healthy majority; quarantine is loud,
    # not fatal. The deploy succeeds; the stragglers need individual recovery.
    print(
        f"::warning::quarantined {len(stragglers)} straggler(s) (<= max {max_stragglers}); "
        f"shipped to the rest of the fleet — these need recovery: {', '.join(stragglers)}"
    )


def execute_scoped_rollout(
    plan: dict,
    token: str,
    list_slugs=plan_rollout_slugs,
    redeploy=redeploy_scoped,
    sleep=time.sleep,
) -> dict:
    cp_url = plan["cp_url"]
    base_body = plan["body"]
    rollout_timeout = int(plan.get("rollout_http_timeout") or ROLLOUT_HTTP_TIMEOUT_DEFAULT_SECONDS)
    all_slugs = list_slugs(cp_url, token, base_body)
    batch_size = int(base_body.get("batch_size") or 1)
    canary_slug = str(base_body.get("canary_slug") or "").strip()
    dry_run = bool(base_body.get("dry_run"))
    aggregate = {"ok": True, "results": []}

    # The real rollout POST blocks for the whole concurrent batch; give it the
    # elevated read budget so a slow/dead tenant can't time the client out
    # before the CP returns results (RFC#2843 #41). Only the production
    # redeploy_scoped accepts a timeout; an injected test double keeps its
    # 3-arg shape, so wrap only when we hold the real callable.
    if redeploy is redeploy_scoped:
        def rollout_redeploy(u, t, b):
            return redeploy_scoped(u, t, b, timeout=rollout_timeout)
    else:
        rollout_redeploy = redeploy

    if canary_slug:
        if canary_slug not in all_slugs:
            raise RuntimeError(f"configured canary slug {canary_slug!r} is not a running tenant")
        body = scoped_redeploy_body(base_body, [canary_slug])
        print(f"POST {cp_url}{REDEPLOY_PATH} only_slugs={','.join(body['only_slugs'])}")
        status, resp = rollout_redeploy(cp_url, token, body)
        aggregate["results"].extend(resp.get("results") or [])
        try:
            _raise_for_redeploy_result(status, resp, [canary_slug])
        except RuntimeError as exc:
            aggregate["ok"] = False
            aggregate["error"] = str(exc)
            raise RolloutFailed(str(exc), aggregate) from exc
        soak_seconds = int(base_body.get("soak_seconds") or 0)
        if soak_seconds > 0 and not dry_run:
            print(f"Canary passed; soaking locally for {soak_seconds}s")
            sleep(soak_seconds)

    remaining = [slug for slug in all_slugs if slug != canary_slug]
    for group in chunks(remaining, batch_size):
        body = scoped_redeploy_body(base_body, group)
        print(f"POST {cp_url}{REDEPLOY_PATH} only_slugs={','.join(group)}")
        status, resp = rollout_redeploy(cp_url, token, body)
        aggregate["results"].extend(resp.get("results") or [])
        try:
            _raise_for_redeploy_result(status, resp, group)
        except RuntimeError as exc:
            aggregate["ok"] = False
            aggregate["error"] = str(exc)
            raise RolloutFailed(str(exc), aggregate) from exc

    # No-silent-skip coverage gate (internal#724): every enumerated tenant
    # must be PROVEN on the target build. A per-tenant HTTP-200/ok response
    # is not proof — a tenant that SSM-succeeded but stayed on the old tag,
    # or one enumerated but never batched, is a straggler. Surfacing it as
    # a RolloutFailed makes the deploy step exit non-zero instead of
    # silently reporting success (the exact agents-team failure mode).
    max_stragglers = int(base_body.get("max_stragglers") or 0)
    assert_full_coverage(all_slugs, aggregate, dry_run, max_stragglers)

    return aggregate


def rollout_from_plan_file(plan_path: str, response_path: str, env: dict[str, str]) -> None:
    token = env.get("CP_ADMIN_API_TOKEN", "").strip()
    if not token:
        raise ValueError("CP_ADMIN_API_TOKEN is required for production auto-deploy")
    with open(plan_path, "r", encoding="utf-8") as fh:
        plan = json.load(fh)
    if not plan.get("enabled"):
        raise RuntimeError("production auto-deploy plan is disabled")
    try:
        response = execute_scoped_rollout(plan, token)
    except RolloutFailed as exc:
        response = exc.response
        with open(response_path, "w", encoding="utf-8") as fh:
            json.dump(response, fh, sort_keys=True)
            fh.write("\n")
        raise
    with open(response_path, "w", encoding="utf-8") as fh:
        json.dump(response, fh, sort_keys=True)
        fh.write("\n")


def _api_json(url: str, token: str) -> dict:
    req = urllib.request.Request(url, headers={"Authorization": f"token {token}"})
    try:
        with urllib.request.urlopen(req, timeout=20) as resp:
            return json.loads(resp.read())
    except urllib.error.HTTPError as exc:
        body = exc.read().decode("utf-8", errors="replace")[:500]
        raise RuntimeError(f"GET {url} -> HTTP {exc.code}: {body}") from exc


def _api_json_list(url: str, token: str) -> list:
    """GET a Gitea list endpoint and return the JSON array.

    Like ``_api_json`` but asserts the body is a list. Fail-closed: a non-list
    body (or HTTP error) raises so the caller never mistakes an unreadable page
    for "no more statuses" and silently truncates the required-context scan.
    """
    req = urllib.request.Request(url, headers={"Authorization": f"token {token}"})
    try:
        with urllib.request.urlopen(req, timeout=20) as resp:
            body = json.loads(resp.read())
    except urllib.error.HTTPError as exc:
        detail = exc.read().decode("utf-8", errors="replace")[:500]
        raise RuntimeError(f"GET {url} -> HTTP {exc.code}: {detail}") from exc
    if not isinstance(body, list):
        raise RuntimeError(f"GET {url} -> expected JSON array, got {type(body).__name__}")
    return body


def fetch_all_statuses(host: str, repo: str, sha: str, token: str, page_size: int = 100) -> list[dict]:
    """Return EVERY commit-status row for ``sha``, paginating to exhaustion.

    The combined ``/commits/{sha}/status`` endpoint caps its embedded
    ``statuses`` array at the Gitea default page size (~30). On a high-churn
    commit, an older-but-still-current required-context SUCCESS row is pushed
    PAST that cap, so a reader of the combined view sees the required context
    as ``missing`` and either blocks (force-merge audit) or waits forever
    (this deploy gate). We instead walk ``/commits/{sha}/statuses`` page by
    page until a short/empty page, accumulating ALL rows.

    Fail-closed: any page that errors or is not a list raises (see
    ``_api_json_list``) — we never degrade to a partial list and call a deploy
    green. A genuinely-absent required context simply never appears on ANY
    page, so the caller's ``ci_context_state`` still reports ``missing`` and
    the gate stays closed.
    """
    base = f"https://{host}/api/v1/repos/{repo}/commits/{sha}/statuses"
    results: list[dict] = []
    page = 1
    while True:
        page_url = f"{base}?page={page}&limit={page_size}"
        rows = _api_json_list(page_url, token)
        results.extend(r for r in rows if isinstance(r, dict))
        # Termination MUST be empty-page, not len(rows) < page_size: Gitea
        # silently CLAMPS limit to its server max (50). With page_size=100
        # every full page reads as "short" and pagination stopped after page
        # 1 — and because /statuses returns NEWEST-first, a required context
        # whose rows sat below the newest 50 was reported "missing" until the
        # 3600s timeout (2026-06-12: e6307e2f prod auto-deploy blocked for an
        # hour while the Secret scan SUCCESS row existed the whole time).
        # One extra (empty) request per fetch is the price of correctness.
        if not rows:
            break
        page += 1
    return results


def _api_json_optional(url: str, token: str) -> tuple[int, dict | None]:
    req = urllib.request.Request(url, headers={"Authorization": f"token {token}"})
    try:
        with urllib.request.urlopen(req, timeout=20) as resp:
            return resp.status, json.loads(resp.read())
    except urllib.error.HTTPError as exc:
        if exc.code == 404:
            return exc.code, None
        body = exc.read().decode("utf-8", errors="replace")[:300]
        print(f"::warning::GET {url} -> HTTP {exc.code}: {body}", file=sys.stderr)
        return exc.code, None


def current_branch_head(env: dict[str, str]) -> str | None:
    """Return the SHA at the tip of the deploy branch (main) per Gitea, or None.

    Used to detect a *superseded* deploy job (see `superseded_by`). Fail-safe:
    any read error / missing token returns None so the caller treats the job as
    NOT superseded and the strict /buildinfo verify still runs. We never let an
    unreadable head silently green a deploy.
    """

    token = env.get("GITEA_TOKEN", "").strip()
    if not token:
        return None
    host = env.get("GITEA_HOST", "git.moleculesai.app")
    repo = env.get("GITHUB_REPOSITORY", "molecule-ai/molecule-core")
    # Deploy lane is on: push:main; the branch is always main here, but read it
    # from the ref name when present so a future branch rename doesn't break us.
    branch = env.get("GITHUB_REF_NAME", "").strip() or "main"
    url = f"https://{host}/api/v1/repos/{repo}/branches/{quote(branch, safe='')}"
    status, body = _api_json_optional(url, token)
    if status != 200 or not isinstance(body, dict):
        return None
    commit = body.get("commit")
    if isinstance(commit, dict):
        head = commit.get("id") or commit.get("sha")
        if isinstance(head, str) and head.strip():
            return head.strip()
    return None


def superseded_by(env: dict[str, str]) -> str | None:
    """Return the newer head SHA if THIS deploy job has been superseded, else None.

    This workflow runs with no `concurrency:` (intentional — Gitea 1.22.6 cancels
    queued runs, which is unacceptable for a prod deploy). When two main pushes
    land close together, BOTH deploy-production jobs run. The newer push rolls the
    fleet forward first; the OLDER job's strict /buildinfo verify then sees tenants
    on the NEWER SHA and false-reds with "$slug is stale" — even though the fleet
    is AHEAD, not behind. Git SHAs aren't ordered, so the verify can't tell ahead
    from behind on its own (and /buildinfo exposes only git_sha, no build time).

    Resolve it at the source of truth for ordering — the branch ref: if main's
    current head is a DIFFERENT SHA than the one this job is deploying, a newer
    commit has landed and this job is superseded; the newest job's verify is the
    authoritative one. We return that head SHA so the caller can log it and exit
    success early, skipping the strict-equality verify for this stale job.

    Fail-safe: returns None (NOT superseded) when the head can't be read or equals
    our SHA, so a genuinely-behind tenant under the LATEST deploy job still fails
    the strict verify loudly. This never suppresses a real-stale signal — it only
    excuses a job that is no longer the latest from asserting exact equality.
    """

    sha = env.get("GITHUB_SHA", "").strip()
    if not sha:
        return None
    head = current_branch_head(env)
    if not head:
        return None
    # SHA lengths can differ (short vs full); compare on the shorter prefix.
    n = min(len(head), len(sha))
    if head[:n].lower() == sha[:n].lower():
        return None
    return head


def live_disable_flag(env: dict[str, str]) -> str:
    """Return a live disable value from Gitea variables when readable.

    Gitea evaluates `${{ vars.* }}` once when the job starts. This API read is
    the emergency re-check immediately before production side effects.
    """

    token = env.get("GITEA_TOKEN", "").strip()
    if not token:
        return ""
    host = env.get("GITEA_HOST", "git.moleculesai.app")
    repo = env.get("GITHUB_REPOSITORY", "molecule-ai/molecule-core")
    variable = quote("PROD_AUTO_DEPLOY_DISABLED", safe="")
    url = f"https://{host}/api/v1/repos/{repo}/actions/variables/{variable}"
    status, body = _api_json_optional(url, token)
    if status != 200 or not isinstance(body, dict):
        return ""
    return str(body.get("data") or body.get("value") or "")


def assert_not_disabled(env: dict[str, str]) -> None:
    plan = build_plan(env)
    if not plan.get("enabled"):
        raise RuntimeError(plan.get("disabled_reason", "production auto-deploy disabled"))
    live_value = live_disable_flag(env)
    if truthy_flag(live_value):
        raise RuntimeError(f"PROD_AUTO_DEPLOY_DISABLED={live_value} (live Gitea variable)")


def wait_for_ci_context(env: dict[str, str]) -> str:
    host = env.get("GITEA_HOST", "git.moleculesai.app")
    repo = env.get("GITHUB_REPOSITORY", "molecule-ai/molecule-core")
    sha = env.get("GITHUB_SHA", "").strip()
    token = env.get("GITEA_TOKEN", "").strip()
    contexts = required_contexts(env)
    interval = _int_env(env, "CI_STATUS_POLL_INTERVAL_SECONDS", 15)
    timeout = _int_env(env, "CI_STATUS_TIMEOUT_SECONDS", 1800)

    if not sha:
        raise ValueError("GITHUB_SHA is required")
    if not token:
        raise ValueError("GITEA_TOKEN is required to wait for CI status")

    deadline = time.time() + timeout
    last_states: dict[str, str] = {}
    while time.time() <= deadline:
        # Read the FULL, exhaustively-paginated /statuses list — NOT the
        # combined /status view, whose embedded `statuses` array is capped at
        # the Gitea page size (~30). On a high-churn commit a required-context
        # SUCCESS row lands past that cap and the combined view would report
        # it `missing`, so this gate would wait until timeout and refuse a
        # legitimate prod deploy. Fetching every page closes that hole.
        # Fail-closed is preserved: a genuinely-absent required context is on
        # NO page, so ci_context_state() still returns "missing" → never
        # satisfied → the deploy stays blocked.
        statuses = fetch_all_statuses(host, repo, sha, token)
        states = {context: ci_context_state(statuses, context) for context in contexts}
        for context, state in states.items():
            if state != last_states.get(context):
                print(f"CI context {context!r}: {state}", file=sys.stderr)
        last_states = states

        failures = [
            f"{context}={state}"
            for context, state in states.items()
            if context_is_terminal_failure(state)
        ]
        if failures:
            raise RuntimeError(
                "Required CI context failed; refusing production deploy: "
                + ", ".join(failures)
            )
        if all(context_is_satisfied(state) for state in states.values()):
            return "success"
        time.sleep(interval)
    last = ", ".join(f"{context}={state}" for context, state in last_states.items()) or "none"
    raise TimeoutError(f"Timed out waiting {timeout}s for required CI contexts; last_states={last}")


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    sub = parser.add_subparsers(dest="command", required=True)
    sub.add_parser("plan", help="print production deploy plan as JSON")
    sub.add_parser("assert-enabled", help="fail if production deploy is currently disabled")
    sub.add_parser("wait-ci", help="block until required CI context is green")
    sub.add_parser(
        "check-superseded",
        help=(
            "exit 0 if a newer commit has landed on the deploy branch (this job "
            "is superseded; prints the newer head SHA), exit 10 if this job is "
            "still the latest"
        ),
    )
    rollout_parser = sub.add_parser("rollout", help="execute canary-first scoped production rollout")
    rollout_parser.add_argument("--plan", required=True, help="path to prod-auto-deploy plan JSON")
    rollout_parser.add_argument("--response", required=True, help="path to write aggregate response JSON")
    args = parser.parse_args()

    try:
        if args.command == "plan":
            print(json.dumps(build_plan(dict(os.environ)), sort_keys=True))
            return 0
        if args.command == "assert-enabled":
            assert_not_disabled(dict(os.environ))
            return 0
        if args.command == "wait-ci":
            wait_for_ci_context(dict(os.environ))
            return 0
        if args.command == "check-superseded":
            newer = superseded_by(dict(os.environ))
            if newer:
                print(newer)
                return 0
            # Exit 10 (not 0, not 1): "this job is still the latest". The
            # workflow treats only exit 0 as superseded; 10 means proceed to
            # the strict verify. A non-zero code here is informational, not a
            # failure — the workflow step swallows it.
            return 10
        if args.command == "rollout":
            rollout_from_plan_file(args.plan, args.response, dict(os.environ))
            return 0
    except Exception as exc:  # noqa: BLE001 - CLI should render operator-friendly errors.
        print(f"::error::{exc}", file=sys.stderr)
        return 1
    return 2


if __name__ == "__main__":
    raise SystemExit(main())
