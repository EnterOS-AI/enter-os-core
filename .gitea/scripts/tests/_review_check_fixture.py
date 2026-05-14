#!/usr/bin/env python3
"""Stub Gitea API for review-check.sh test scenarios.

Reads $FIXTURE_STATE_DIR/scenario to decide what to return for each
endpoint the review-check.sh script calls.
Reads $FIXTURE_STATE_DIR/token_owner_in_teams to decide whether
the team membership probe returns 200/204 (member) or 403 (not in team).

Scenarios:
  T1_pr_open          — open PR, author=alice, sha=deadbeef → continue
  T2_pr_closed        — closed PR → script exits 0 (no-op)
  T3_reviews_approved_non_author  — one APPROVED from non-author → candidates exist
  T4_reviews_empty             — zero APPROVED non-author → exit 1 (no candidates)
  T5_reviews_only_author        — only author reviews → exit 1 (no candidates)
  T6_reviews_dismissed          — dismissed APPROVED → treated as no approval
  T7_team_member              — team membership → 204 (member) → exit 0
  T8_team_not_member          — team membership → 404 (not a member) → exit 1
  T9_team_403                — team membership → 403 (token not in team) → exit 1
  T14_non_default_base        — open PR targeting staging → script exits 0 (no-op)

Usage:
  FIXTURE_STATE_DIR=/tmp/x python3 _review_check_fixture.py 8080
"""

import http.server
import json
import os
import re
import sys
import urllib.parse


STATE_DIR = os.environ.get("FIXTURE_STATE_DIR", "/tmp")


def scenario() -> str:
    p = os.path.join(STATE_DIR, "scenario")
    if not os.path.isfile(p):
        return "T1_pr_open"
    with open(p) as f:
        return f.read().strip()


class Handler(http.server.BaseHTTPRequestHandler):
    def log_message(self, *args, **kwargs):
        pass  # keep stdout for explicit logs only

    def _json(self, code: int, body: dict) -> None:
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

    def _text(self, code: int, body: str) -> None:
        payload = body.encode()
        self.send_response(code)
        self.send_header("Content-Type", "text/plain")
        self.send_header("Content-Length", str(len(payload)))
        self.end_headers()
        self.wfile.write(payload)

    def do_GET(self):
        u = urllib.parse.urlparse(self.path)
        path = u.path
        sc = scenario()

        if path == "/_ping":
            return self._json(200, {"ok": True})

        # GET /repos/{owner}/{name}/pulls/{pr_number}
        m = re.match(r"^/api/v1/repos/([^/]+)/([^/]+)/pulls/(\d+)$", path)
        if m:
            owner, name, pr_num = m.group(1), m.group(2), m.group(3)
            if sc == "T2_pr_closed":
                return self._json(200, {
                    "number": int(pr_num),
                    "state": "closed",
                    "head": {"sha": "deadbeef0000111122223333444455556666"},
                    "base": {"ref": "main"},
                    "user": {"login": "alice"},
                })
            return self._json(200, {
                "number": int(pr_num),
                "state": "open",
                "head": {"sha": "deadbeef0000111122223333444455556666"},
                "base": {"ref": "staging" if sc == "T14_non_default_base" else "main"},
                "user": {"login": "alice"},
            })

        # GET /repos/{owner}/{name}/pulls/{pr_number}/reviews
        m = re.match(r"^/api/v1/repos/([^/]+)/([^/]+)/pulls/(\d+)/reviews$", path)
        if m:
            if sc in ("T4_reviews_empty", "T5_reviews_only_author"):
                return self._json(200, [])
            if sc == "T6_reviews_dismissed":
                return self._json(200, [{
                    "state": "APPROVED",
                    "dismissed": True,
                    "user": {"login": "core-devops"},
                    "commit_id": "abc1234",
                }])
            if sc == "T3_reviews_approved_non_author":
                return self._json(200, [
                    {"state": "CHANGES_REQUESTED", "dismissed": False, "user": {"login": "bob"}, "commit_id": "abc1234"},
                    {"state": "APPROVED", "dismissed": False, "user": {"login": "core-devops"}, "commit_id": "abc1234"},
                ])
            # Default: one non-author APPROVED
            return self._json(200, [
                {"state": "APPROVED", "dismissed": False, "user": {"login": "core-devops"}, "commit_id": "abc1234"},
            ])

        # GET /teams/{team_id}/members/{username}
        m = re.match(r"^/api/v1/teams/(\d+)/members/([^/]+)$", path)
        if m:
            team_id, login = m.group(1), m.group(2)
            if sc == "T8_team_not_member":
                return self._empty(404)
            if sc == "T9_team_403":
                return self._empty(403)
            # T7_team_member: member
            return self._empty(204)

        return self._json(404, {"path": path, "msg": "fixture: no route"})

    def do_POST(self):
        self._json(404, {"path": self.path, "msg": "fixture: no POST routes"})


def main():
    port = int(sys.argv[1])
    srv = http.server.ThreadingHTTPServer(("127.0.0.1", port), Handler)
    srv.serve_forever()


if __name__ == "__main__":
    main()
