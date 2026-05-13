import importlib.util
import pathlib


SCRIPT = pathlib.Path(__file__).with_name("gate_check.py")


def load_gate_check():
    spec = importlib.util.spec_from_file_location("gate_check", SCRIPT)
    mod = importlib.util.module_from_spec(spec)
    assert spec.loader is not None
    spec.loader.exec_module(mod)
    return mod


def test_run_skips_pr_not_targeting_default_branch(monkeypatch):
    mod = load_gate_check()

    def fake_api_get(path):
        assert path == "/repos/molecule-ai/molecule-core/pulls/843"
        return {
            "number": 843,
            "base": {"ref": "staging"},
            "head": {"sha": "84b9ca3a129075b8d5159eda5e678f68be1af20f"},
        }

    monkeypatch.setenv("DEFAULT_BRANCH", "main")
    monkeypatch.setattr(mod, "api_get", fake_api_get)

    result = mod.run("molecule-ai/molecule-core", 843, post_comment=False)

    assert result["verdict"] == "CLEAR"
    assert result["skipped"] is True
    assert "staging" in result["reason"]
