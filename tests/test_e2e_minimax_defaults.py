from pathlib import Path


ROOT = Path(__file__).resolve().parents[1]


def test_staging_e2e_workflows_use_stable_minimax_default() -> None:
    """Keep cron/push E2E on the same MiniMax model as the smoke-tested script."""
    workflow_paths = [
        ".gitea/workflows/e2e-staging-saas.yml",
        ".gitea/workflows/staging-smoke.yml",
        ".gitea/workflows/continuous-synth-e2e.yml",
    ]

    for rel in workflow_paths:
        text = (ROOT / rel).read_text()
        assert "MiniMax-M2.7-highspeed" not in text
        assert "MiniMax-M2" in text
