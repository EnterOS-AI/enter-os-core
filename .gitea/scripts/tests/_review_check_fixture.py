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
  T15_comments_agent_approval — reviews empty; comments have "[core-qa-agent] APPROVED" → exit 0
  T16_comments_generic_approval — reviews empty; comments have "APPROVED" by team member → exit 0
  T17_comments_no_approval   — reviews empty; comments have no approval keywords → exit 1
  T18_review_wrong_team_comment_right_team — review candidate 404s, comment candidate passes
  T19_ai_sop_ack_approved — ai-sop-ack member APPROVED review → team probe 404 → exit 1

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
    with open(p, encoding="utf-8") as f:
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
            pr_num = m.group(3)
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
            if sc in ("T4_reviews_empty", "T5_reviews_only_author",
                      "T15_comments_agent_approval", "T16_comments_generic_approval",
                      "T17_comments_no_approval"):
                return self._json(200, [])
            if sc == "T6_reviews_dismissed":
                return self._json(200, [{
                    "state": "APPROVED",
                    "dismissed": True,
                    "official": True,
                    "user": {"login": "core-devops"},
                    "commit_id": "deadbeef0000111122223333444455556666",
                }])
            if sc == "T3_reviews_approved_non_author":
                return self._json(200, [
                    {"state": "CHANGES_REQUESTED", "dismissed": False, "official": True, "user": {"login": "bob"}, "commit_id": "deadbeef0000111122223333444455556666"},
                    {"state": "APPROVED", "dismissed": False, "official": True, "user": {"login": "core-devops"}, "commit_id": "deadbeef0000111122223333444455556666"},
                ])
            if sc == "T19_ai_sop_ack_approved":
                # ai-sop-ack member submitted APPROVED review — must NOT count
                # toward qa-review (team_id=20) or security-review (team_id=21).
                return self._json(200, [
                    {"state": "APPROVED", "dismissed": False, "official": True, "user": {"login": "ai-reviewer"}, "commit_id": "deadbeef0000111122223333444455556666"},
                ])
            if sc == "T21_stale_head_approved":
                # APPROVED review but on an old commit (stale head) → must be rejected
                return self._json(200, [
                    {"state": "APPROVED", "dismissed": False, "official": True, "user": {"login": "core-devops"}, "commit_id": "oldsha0000000000000000000000000000"},
                ])
            if sc == "T22_missing_official":
                # APPROVED review with no official field → must be rejected
                return self._json(200, [
                    {"state": "APPROVED", "dismissed": False, "user": {"login": "core-devops"}, "commit_id": "deadbeef0000111122223333444455556666"},
                ])
            if sc == "T23_missing_commit_id":
                # APPROVED review with NO commit_id field — the SEV-1
                # internal#812 / closed-#843 spoof-bug signature. The
                # fail-closed SSOT must REJECT (not silently accept as
                # "older Gitea row" the way the old pre-fix code did).
                return self._json(200, [
                    {"state": "APPROVED", "official": True, "dismissed": False, "user": {"login": "core-devops"}},
                ])
            # Default: one non-author APPROVED (current head, official)
            return self._json(200, [
                {"state": "APPROVED", "dismissed": False, "official": True, "user": {"login": "core-devops"}, "commit_id": "deadbeef0000111122223333444455556666"},
            ])

        # GET /repos/{owner}/{name}/issues/{pr_number}/comments
        m = re.match(r"^/api/v1/repos/([^/]+)/([^/]+)/issues/(\d+)/comments$", path)
        if m:
            if sc == "T15_comments_agent_approval":
                return self._json(200, [
                    {"user": {"login": "core-qa-agent"}, "body": "[core-qa-agent] APPROVED this PR. Good changes.", "id": 1},
                    {"user": {"login": "alice"}, "body": "I authored this PR", "id": 2},
                    {"user": {"login": "random-user"}, "body": "Looks okay to me", "id": 3},
                ])
            if sc == "T16_comments_generic_approval":
                return self._json(200, [
                    {"user": {"login": "core-qa-agent"}, "body": "APPROVED — all acceptance criteria met", "id": 1},
                    {"user": {"login": "alice"}, "body": "-authored", "id": 2},
                ])
            if sc == "T17_comments_no_approval":
                return self._json(200, [
                    {"user": {"login": "alice"}, "body": "I authored this PR", "id": 1},
                    {"user": {"login": "random-user"}, "body": "Looks okay to me", "id": 2},
                ])
            if sc == "T18_review_wrong_team_comment_right_team":
                return self._json(200, [
                    {"user": {"login": "core-qa-agent"}, "body": "[core-qa-agent] APPROVED after focused review", "id": 1},
                ])
            # Default scenarios (T1–T9, T14): no comments
            return self._json(200, [])

        # GET /teams/{team_id}/members/{username}
        m = re.match(r"^/api/v1/teams/(\d+)/members/([^/]+)$", path)
        if m:
            login = m.group(2)
            if sc == "T8_team_not_member":
                return self._empty(404)
            if sc == "T9_team_403":
                return self._empty(403)
            if sc == "T18_review_wrong_team_comment_right_team" and login == "core-devops":
                return self._empty(404)
            if sc == "T19_ai_sop_ack_approved" and login == "ai-reviewer":
                # ai-sop-ack member is NOT in qa (20) or security (21).
                return self._empty(404)
            # T7_team_member: member
            return self._empty(204)

        # GET /repos/{owner}/{name}/statuses/{sha} — for N/A declaration check
        m = re.match(r"^/api/v1/repos/([^/]+)/([^/]+)/statuses/([a-f0-9]+)$", path)
        if m:
            # All comment-based scenarios have no N/A declarations
            return self._json(200, [])

        return self._json(404, {"path": path, "msg": "fixture: no route"})

    def do_POST(self):
        self._json(404, {"path": self.path, "msg": "fixture: no POST routes"})


def main():
    port = int(sys.argv[1])
    srv = http.server.ThreadingHTTPServer(("127.0.0.1", port), Handler)
    srv.serve_forever()


if __name__ == "__main__":
    main()
