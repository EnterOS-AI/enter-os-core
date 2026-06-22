"""Tests for the sweep-aws-secrets.sh decision logic (#890).

Run locally: ``python3 -m unittest scripts/ops/test_sweep_aws_decide.py -v``

Why this exists: the inline Python heredoc in sweep-aws-secrets.sh decides
which AWS Secrets Manager secrets to delete. A misclassification could nuke
a LIVE tenant's bootstrap secret or a live workspace's config secret. These
tests pin the rule order for BOTH managed namespaces:

  - molecule/tenant/<org_id>/bootstrap  (org_id in the NAME)
  - molecule/workspace/<ws_id>/config   (owning org on the OrgID TAG)

To avoid drift, the test EXTRACTS the `decide`/`org_tag`/`parse_iso` helpers
straight out of the shell script's heredoc at runtime and execs them — so a
change to the shell logic that isn't reflected here (or vice-versa) fails.
"""
from __future__ import annotations

import os
import re
import unittest
from datetime import datetime, timedelta, timezone

SCRIPT = os.path.join(os.path.dirname(__file__), "sweep-aws-secrets.sh")


def _load_decide():
    """Extract the decision helpers from the shell heredoc and exec them."""
    src = open(SCRIPT, encoding="utf-8").read()
    # The heredoc body lives between `DECISIONS=$(echo "$SECRET_JSON" | python3 -c '`
    # and the closing `')`. Grab the python source between the single quotes.
    m = re.search(r"DECISIONS=\$\(echo \"\$SECRET_JSON\" \| python3 -c '(.*?)'\)", src, re.S)
    if not m:
        raise AssertionError("could not locate the decision heredoc in sweep-aws-secrets.sh")
    body = m.group(1)
    # The heredoc reads PROD_IDS/STAGING_IDS/GRACE_HOURS from os.environ and
    # reads stdin at the bottom. We only want the pure functions, so exec the
    # whole body in a namespace where env + stdin are pre-seeded, but stop it
    # from consuming stdin by trimming everything from the final stdin loop.
    cut = body.find("d = json.loads(sys.stdin.read())")
    if cut != -1:
        body = body[:cut]
    ns: dict = {}
    os.environ.setdefault("PROD_IDS", "live-org-1")
    os.environ.setdefault("STAGING_IDS", "live-org-2")
    os.environ.setdefault("GRACE_HOURS", "24")
    exec(compile(body, "<heredoc>", "exec"), ns)
    return ns


NS = _load_decide()
ALL_IDS = {"live-org-1", "live-org-2"}
GRACE = timedelta(hours=24)
NOW = datetime.now(timezone.utc)
OLD = (NOW - timedelta(days=30)).isoformat()
FRESH = NOW.isoformat()
U = "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"


def decide(secret: dict):
    action, reason, _arn, _name = NS["decide"](secret, ALL_IDS, GRACE, NOW)
    return action, reason


class TestNonManaged(unittest.TestCase):
    def test_arbitrary_secret_kept(self):
        self.assertEqual(decide({"Name": "molecule/random/x", "CreatedDate": OLD}),
                         ("keep", "not-a-managed-secret"))

    def test_bad_uuid_tenant_kept(self):
        self.assertEqual(decide({"Name": "molecule/tenant/not-a-uuid/bootstrap", "CreatedDate": OLD}),
                         ("keep", "not-a-managed-secret"))


class TestTenant(unittest.TestCase):
    def test_orphan_tenant_deleted(self):
        self.assertEqual(decide({"Name": f"molecule/tenant/{U}/bootstrap", "CreatedDate": OLD}),
                         ("delete", "orphan-tenant"))

    def test_live_tenant_kept(self):
        self.assertEqual(decide({"Name": "molecule/tenant/live-org-1xxxxxxxxxxxxxxxxxxxxxxxxxxx/bootstrap",
                                 "CreatedDate": OLD}),
                         ("keep", "not-a-managed-secret"))  # wrong length → not managed

    def test_live_tenant_real_uuid_kept(self):
        live = "11111111-2222-3333-4444-555555555555"
        os.environ["PROD_IDS"] = "live-org-1"
        # use ALL_IDS that includes the uuid
        action, reason, _, _ = NS["decide"](
            {"Name": f"molecule/tenant/{live}/bootstrap", "CreatedDate": OLD},
            ALL_IDS | {live}, GRACE, NOW)
        self.assertEqual((action, reason), ("keep", "live-tenant"))


class TestWorkspace(unittest.TestCase):
    def test_orphan_workspace_deleted(self):
        self.assertEqual(decide({"Name": f"molecule/workspace/{U}/config", "CreatedDate": OLD,
                                 "Tags": [{"Key": "OrgID", "Value": "dead-org"}]}),
                         ("delete", "orphan-workspace"))

    def test_live_org_workspace_kept(self):
        self.assertEqual(decide({"Name": f"molecule/workspace/{U}/config", "CreatedDate": OLD,
                                 "Tags": [{"Key": "OrgID", "Value": "live-org-2"}]}),
                         ("keep", "workspace-live-org"))

    def test_no_org_tag_kept(self):
        self.assertEqual(decide({"Name": f"molecule/workspace/{U}/config", "CreatedDate": OLD,
                                 "Tags": []}),
                         ("keep", "workspace-no-org-tag"))


class TestGrace(unittest.TestCase):
    def test_fresh_orphan_workspace_kept(self):
        self.assertEqual(decide({"Name": f"molecule/workspace/{U}/config", "CreatedDate": FRESH,
                                 "Tags": [{"Key": "OrgID", "Value": "dead-org"}]}),
                         ("keep", "in-grace-window"))

    def test_fresh_orphan_tenant_kept(self):
        self.assertEqual(decide({"Name": f"molecule/tenant/{U}/bootstrap", "CreatedDate": FRESH}),
                         ("keep", "in-grace-window"))


if __name__ == "__main__":
    unittest.main()
