#!/usr/bin/env python3
"""Stub Gitea API for sop-tier-refire test scenarios.

Reads $FIXTURE_STATE_DIR/scenario to decide what to return for each
endpoint the sop-tier-refire.sh + sop-tier-check.sh scripts call.
Captures every POST to /statuses/{sha} into posted_statuses.jsonl so
the test can assert what the script tried to write.

Scenarios:
  T1_success         — tier:low + APPROVED by engineer → tier-check passes
  T2_no_tier_label   — no tier label → tier-check exits 1 before POST
  T3_no_approvals    — tier:low but zero approving reviews → exits 1
  T4_closed          — PR state=closed → refire is a no-op
  T5_rate_limited    — last status update 5 seconds ago → skip

Usage:
  FIXTURE_STATE_DIR=/tmp/x python3 _refire_fixture.py 8080
"""

import datetime
import http.server
import json
import os
import re
import sys
import urllib.parse


STATE_DIR = os.environ["FIXTURE_STATE_DIR"]


def scenario() -> str:
    p = os.path.join(STATE_DIR, "scenario")
    if not os.path.isfile(p):
        return "T1_success"
    with open(p) as f:
        return f.read().strip()


def now_iso() -> str:
    return datetime.datetime.now(datetime.timezone.utc).isoformat()


def append_post(body: dict) -> None:
    with open(os.path.join(STATE_DIR, "posted_statuses.jsonl"), "a") as f:
        f.write(json.dumps(body) + "\n")


def pr_payload() -> dict:
    sc = scenario()
    state = "closed" if sc == "T4_closed" else "open"
    return {
        "number": 999,
        "state": state,
        "head": {"sha": "deadbeef0000111122223333444455556666"},
        "user": {"login": "feature-author"},
    }


def labels_payload() -> list:
    sc = scenario()
    if sc == "T2_no_tier_label":
        return [{"name": "bug"}]
    # All other scenarios use tier:low
    return [{"name": "tier:low"}, {"name": "ci"}]


def reviews_payload() -> list:
    sc = scenario()
    if sc == "T3_no_approvals":
        return []
    # All other scenarios have one APPROVED review by an engineer
    return [
        {
            "state": "APPROVED",
            "user": {"login": "reviewer-engineer"},
        }
    ]


def teams_payload() -> list:
    # Mirror the real molecule-ai org teams referenced in TIER_EXPR
    return [
        {"id": 5, "name": "ceo"},
        {"id": 2, "name": "engineers"},
        {"id": 6, "name": "managers"},
    ]


def statuses_payload() -> list:
    sc = scenario()
    if sc == "T5_rate_limited":
        recent = (
            datetime.datetime.now(datetime.timezone.utc)
            - datetime.timedelta(seconds=5)
        ).isoformat()
        return [
            {
                "context": "sop-tier-check / tier-check (pull_request)",
                "state": "failure",
                "updated_at": recent,
            }
        ]
    return []


def user_payload() -> dict:
    # Mirrors the WHOAMI probe in sop-tier-check.sh
    return {"login": "sop-tier-bot-fixture"}


class Handler(http.server.BaseHTTPRequestHandler):
    # Quiet — keep stdout for explicit logs only.
    def log_message(self, *args, **kwargs):  # noqa: D401
        pass

    def _json(self, code: int, body) -> None:
        payload = json.dumps(body).encode()
        self.send_response(code)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(payload)))
        self.end_headers()
        self.wfile.write(payload)

    def _empty(self, code: int) -> None:
        self.send_response(code)
        self.send_header("Content-Length", "0")
        self.end_headers()

    def do_GET(self):  # noqa: N802
        u = urllib.parse.urlparse(self.path)
        path = u.path

        if path == "/_ping":
            return self._json(200, {"ok": True})
        if path == "/api/v1/user":
            return self._json(200, user_payload())

        # /api/v1/repos/{owner}/{name}/pulls/{n}
        m = re.match(r"^/api/v1/repos/[^/]+/[^/]+/pulls/(\d+)$", path)
        if m:
            return self._json(200, pr_payload())

        # /api/v1/repos/{owner}/{name}/issues/{n}/labels
        if re.match(r"^/api/v1/repos/[^/]+/[^/]+/issues/\d+/labels$", path):
            return self._json(200, labels_payload())

        # /api/v1/repos/{owner}/{name}/pulls/{n}/reviews
        if re.match(r"^/api/v1/repos/[^/]+/[^/]+/pulls/\d+/reviews$", path):
            return self._json(200, reviews_payload())

        # /api/v1/orgs/{owner}/teams
        if re.match(r"^/api/v1/orgs/[^/]+/teams$", path):
            return self._json(200, teams_payload())

        # /api/v1/teams/{id}/members/{login} → 204 if user is an engineer
        m = re.match(r"^/api/v1/teams/(\d+)/members/([^/]+)$", path)
        if m:
            team_id, login = m.group(1), m.group(2)
            # In our fixture reviewer-engineer ∈ engineers (id=2)
            if team_id == "2" and login == "reviewer-engineer":
                return self._empty(204)
            return self._empty(404)

        # /api/v1/orgs/{owner}/members/{login} — fallback path used when
        # team-member probes all 403. We don't need it for these tests.
        if re.match(r"^/api/v1/orgs/[^/]+/members/[^/]+$", path):
            return self._empty(404)

        # /api/v1/repos/{owner}/{name}/statuses/{sha}
        if re.match(r"^/api/v1/repos/[^/]+/[^/]+/statuses/[^/]+$", path):
            return self._json(200, statuses_payload())

        return self._json(404, {"path": path, "msg": "fixture: no route"})

    def do_POST(self):  # noqa: N802
        u = urllib.parse.urlparse(self.path)
        path = u.path
        length = int(self.headers.get("Content-Length") or 0)
        raw = self.rfile.read(length) if length else b""
        try:
            body = json.loads(raw) if raw else {}
        except Exception:
            body = {"_raw": raw.decode(errors="replace")}

        if re.match(r"^/api/v1/repos/[^/]+/[^/]+/statuses/[^/]+$", path):
            append_post(body)
            # Echo back something status-shaped — script only checks HTTP code.
            return self._json(
                201,
                {
                    "context": body.get("context"),
                    "state": body.get("state"),
                    "created_at": now_iso(),
                },
            )

        return self._json(404, {"path": path, "msg": "fixture: no route"})


def main():
    port = int(sys.argv[1])
    srv = http.server.ThreadingHTTPServer(("127.0.0.1", port), Handler)
    srv.serve_forever()


if __name__ == "__main__":
    main()
