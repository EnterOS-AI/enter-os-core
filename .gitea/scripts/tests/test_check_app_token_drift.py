import importlib.util
import sys
from pathlib import Path
from unittest.mock import patch, MagicMock

SCRIPT = Path(__file__).resolve().parents[1] / "check_app_token_drift.py"
spec = importlib.util.spec_from_file_location("check_app_token_drift", SCRIPT)
drift = importlib.util.module_from_spec(spec)
sys.modules[spec.name] = drift
spec.loader.exec_module(drift)


SAMPLE_CANVAS_CSS = """
@theme {
  --color-surface: #ffffff;
  --color-surface-elevated: #fafafa;
  --color-surface-sunken: #f5f5f5;
  --color-surface-card: #ffffff;
  --color-line: #e5e5e5;
  --color-line-soft: #f0f0f0;
  --color-ink: #111111;
  --color-ink-mid: #555555;
  --color-ink-soft: #888888;
  --color-accent: #0066cc;
  --color-accent-strong: #0055aa;
  --color-warm: #ff9900;
  --color-good: #0c8a52;
  --color-bad: #c2403c;
}

[data-theme="dark"] {
  --color-surface: #111111;
  --color-surface-elevated: #1a1a1a;
  --color-surface-sunken: #0a0a0a;
  --color-surface-card: #1a1a1a;
  --color-line: #333333;
  --color-line-soft: #222222;
  --color-ink: #f5f5f5;
  --color-ink-mid: #aaaaaa;
  --color-ink-soft: #777777;
  --color-accent: #4d9fff;
  --color-accent-strong: #66adff;
  --color-warm: #ffaa33;
  --color-good: #2a6e44;
  --color-bad: #b0463f;
}
"""


def test_extract_theme_block_finds_first_theme_block():
    block = drift.extract_theme_block(SAMPLE_CANVAS_CSS)
    assert "--color-surface: #ffffff" in block
    assert "[data-theme" not in block


def test_extract_data_theme_dark_finds_dark_block():
    block = drift.extract_data_theme_dark(SAMPLE_CANVAS_CSS)
    assert "--color-surface: #111111" in block
    assert "--color-good: #2a6e44" in block


def test_parse_tokens_extracts_shared_tokens():
    block = drift.extract_theme_block(SAMPLE_CANVAS_CSS)
    tokens = drift.parse_tokens(block)
    assert tokens["--color-surface"] == "#ffffff"
    assert tokens["--color-good"] == "#0c8a52"
    assert len(tokens) == len(drift.SHARED_TOKEN_NAMES)


def test_compare_tokens_detects_no_drift():
    canvas = drift.extract_shared_tokens(SAMPLE_CANVAS_CSS)
    ok, differences = drift.compare_tokens(canvas, canvas)
    assert ok is True
    assert differences == {"light": {}, "dark": {}}


def test_compare_tokens_detects_drift():
    canvas = drift.extract_shared_tokens(SAMPLE_CANVAS_CSS)
    app_css = SAMPLE_CANVAS_CSS.replace("--color-good: #0c8a52;", "--color-good: #00ff00;")
    app = drift.extract_shared_tokens(app_css)
    ok, differences = drift.compare_tokens(canvas, app)
    assert ok is False
    assert "--color-good" in differences["light"]
    assert "#0c8a52" in differences["light"]["--color-good"]
    assert "#00ff00" in differences["light"]["--color-good"]


def test_main_skips_when_token_missing(capsys):
    with patch.dict("os.environ", {}, clear=True):
        assert drift.main() == 0
    captured = capsys.readouterr()
    assert "APP_SSOT_READ_TOKEN not set" in captured.out


def test_main_errors_when_canvas_file_missing(capsys):
    with patch.dict("os.environ", {"APP_SSOT_READ_TOKEN": "fake"}, clear=True):
        with patch.object(drift, "CANVAS_FILE_PATH", "nonexistent/globals.css"):
            assert drift.main() == 1
    captured = capsys.readouterr()
    assert "not found in working tree" in captured.out


def _fake_urlopen_response(body: bytes):
    """Return a context-manager-compatible mock response."""
    mock_resp = MagicMock()
    mock_resp.read.return_value = body
    mock_cm = MagicMock()
    mock_cm.__enter__ = MagicMock(return_value=mock_resp)
    mock_cm.__exit__ = MagicMock(return_value=False)
    return mock_cm


def test_main_reports_drift(capsys, tmp_path):
    canvas_file = tmp_path / "globals.css"
    canvas_file.write_text(SAMPLE_CANVAS_CSS)
    app_css = SAMPLE_CANVAS_CSS.replace("--color-good: #0c8a52;", "--color-good: #00ff00;")

    fake_payload = {"content": __import__("base64").b64encode(app_css.encode()).decode()}
    fake_response = _fake_urlopen_response(__import__("json").dumps(fake_payload).encode())

    with patch.dict("os.environ", {"APP_SSOT_READ_TOKEN": "fake"}, clear=True):
        with patch.object(drift, "CANVAS_FILE_PATH", str(canvas_file)):
            with patch("urllib.request.urlopen", return_value=fake_response):
                assert drift.main() == 1

    captured = capsys.readouterr()
    assert "Canvas↔app token SSOT drift detected" in captured.out
    assert "--color-good" in captured.out


def test_main_reports_aligned(capsys, tmp_path):
    canvas_file = tmp_path / "globals.css"
    canvas_file.write_text(SAMPLE_CANVAS_CSS)

    fake_payload = {"content": __import__("base64").b64encode(SAMPLE_CANVAS_CSS.encode()).decode()}
    fake_response = _fake_urlopen_response(__import__("json").dumps(fake_payload).encode())

    with patch.dict("os.environ", {"APP_SSOT_READ_TOKEN": "fake"}, clear=True):
        with patch.object(drift, "CANVAS_FILE_PATH", str(canvas_file)):
            with patch("urllib.request.urlopen", return_value=fake_response):
                assert drift.main() == 0

    captured = capsys.readouterr()
    assert "Canvas↔app token SSOT is aligned" in captured.out
