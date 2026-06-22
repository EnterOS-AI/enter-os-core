from pathlib import Path

import yaml


ROOT = Path(__file__).resolve().parents[1]


def workflow_on(path: Path):
    doc = yaml.safe_load(path.read_text())
    return doc.get("on") or doc.get(True)


def test_browser_e2e_workflows_are_not_unconditional_pr_heavy_lanes():
    workflows = [
        ROOT / ".gitea/workflows/e2e-chat.yml",
        ROOT / ".gitea/workflows/e2e-staging-canvas.yml",
    ]

    for path in workflows:
        text = path.read_text()
        events = workflow_on(path)

        assert "workflow_dispatch" in events
        assert "schedule" in events
        assert "merge-queue" in text
        assert "/issues/${{ github.event.pull_request.number }}/labels" in text
        assert "PR is not in merge-queue" in text
