"""Unit tests for resolve_provider_routing in adapter_base.

Covers provider routing, URL-override precedence, and the missing-key error path.
Each adapter defines its own registry; this test file defines one inline that
mirrors what the openclaw adapter uses.
"""
from __future__ import annotations

import pytest

from adapter_base import ProviderRegistry, resolve_provider_routing

# Mirror of the registry in openclaw's adapter.py — kept in sync manually.
PROVIDER_REGISTRY: ProviderRegistry = {
    "openai":     (("OPENAI_API_KEY",),                     "https://api.openai.com/v1"),
    "groq":       (("GROQ_API_KEY",),                       "https://api.groq.com/openai/v1"),
    "openrouter": (("OPENROUTER_API_KEY",),                 "https://openrouter.ai/api/v1"),
    "qianfan":    (("QIANFAN_API_KEY", "AISTUDIO_API_KEY"), "https://qianfan.baidubce.com/v2"),
    "minimax":    (("MINIMAX_API_KEY",),                    "https://api.minimaxi.com/v1"),
    "moonshot":   (("KIMI_API_KEY",),                       "https://api.moonshot.ai/v1"),
}


class TestProviderRouting:

    def test_openai_key_and_url(self):
        api_key, base_url, model_id = resolve_provider_routing(
            "openai:gpt-4o", {"OPENAI_API_KEY": "sk-openai"}, registry=PROVIDER_REGISTRY
        )
        assert api_key == "sk-openai"
        assert base_url == "https://api.openai.com/v1"
        assert model_id == "gpt-4o"

    def test_groq_key_and_url(self):
        api_key, base_url, model_id = resolve_provider_routing(
            "groq:llama-3.3-70b", {"GROQ_API_KEY": "sk-groq"}, registry=PROVIDER_REGISTRY
        )
        assert api_key == "sk-groq"
        assert base_url == "https://api.groq.com/openai/v1"
        assert model_id == "llama-3.3-70b"

    def test_openrouter_key_and_url(self):
        api_key, base_url, model_id = resolve_provider_routing(
            "openrouter:anthropic/claude-sonnet-4-5", {"OPENROUTER_API_KEY": "sk-or"}, registry=PROVIDER_REGISTRY
        )
        assert api_key == "sk-or"
        assert base_url == "https://openrouter.ai/api/v1"
        assert model_id == "anthropic/claude-sonnet-4-5"

    def test_qianfan_primary_key(self):
        api_key, _, _ = resolve_provider_routing(
            "qianfan:ernie-4.5", {"QIANFAN_API_KEY": "sk-qf", "AISTUDIO_API_KEY": "sk-ai"}, registry=PROVIDER_REGISTRY
        )
        assert api_key == "sk-qf"

    def test_qianfan_fallback_to_aistudio(self):
        api_key, base_url, _ = resolve_provider_routing(
            "qianfan:ernie-4.5", {"AISTUDIO_API_KEY": "sk-ai"}, registry=PROVIDER_REGISTRY
        )
        assert api_key == "sk-ai"
        assert base_url == "https://qianfan.baidubce.com/v2"

    def test_minimax_key_and_url(self):
        api_key, base_url, model_id = resolve_provider_routing(
            "minimax:MiniMax-M2.7", {"MINIMAX_API_KEY": "sk-mm"}, registry=PROVIDER_REGISTRY
        )
        assert api_key == "sk-mm"
        assert base_url == "https://api.minimaxi.com/v1"
        assert model_id == "MiniMax-M2.7"

    def test_moonshot_key_and_url(self):
        api_key, base_url, model_id = resolve_provider_routing(
            "moonshot:kimi-k2.5", {"KIMI_API_KEY": "sk-kimi"}, registry=PROVIDER_REGISTRY
        )
        assert api_key == "sk-kimi"
        assert base_url == "https://api.moonshot.ai/v1"
        assert model_id == "kimi-k2.5"

    def test_bare_model_id_defaults_to_openai(self):
        api_key, base_url, model_id = resolve_provider_routing(
            "gpt-4o", {"OPENAI_API_KEY": "sk-openai"}, registry=PROVIDER_REGISTRY
        )
        assert base_url == "https://api.openai.com/v1"
        assert model_id == "gpt-4o"

    def test_unknown_prefix_falls_back_to_openai_url(self):
        api_key, base_url, model_id = resolve_provider_routing(
            "custom-shim:my-model", {"OPENAI_API_KEY": "sk-openai"}, registry=PROVIDER_REGISTRY
        )
        assert base_url == "https://api.openai.com/v1"
        assert model_id == "my-model"


class TestUrlOverridePrecedence:

    def test_env_base_url_beats_registry_default(self):
        _, base_url, _ = resolve_provider_routing(
            "minimax:MiniMax-M2.7",
            {"MINIMAX_API_KEY": "sk-mm", "MINIMAX_BASE_URL": "https://api.minimax.chat/v1"},
            registry=PROVIDER_REGISTRY,
        )
        assert base_url == "https://api.minimax.chat/v1"

    def test_runtime_config_provider_url_beats_registry_default(self):
        _, base_url, _ = resolve_provider_routing(
            "openai:gpt-4o",
            {"OPENAI_API_KEY": "sk-openai"},
            registry=PROVIDER_REGISTRY,
            runtime_config={"provider_url": "https://proxy.example.com/v1"},
        )
        assert base_url == "https://proxy.example.com/v1"

    def test_env_base_url_beats_runtime_config(self):
        _, base_url, _ = resolve_provider_routing(
            "openai:gpt-4o",
            {"OPENAI_API_KEY": "sk-openai", "OPENAI_BASE_URL": "https://env-wins.com/v1"},
            registry=PROVIDER_REGISTRY,
            runtime_config={"provider_url": "https://config-loses.com/v1"},
        )
        assert base_url == "https://env-wins.com/v1"


class TestMissingKey:

    def test_raises_when_no_key_set(self):
        with pytest.raises(RuntimeError, match="No API key found for provider 'minimax'"):
            resolve_provider_routing("minimax:MiniMax-M2.7", {}, registry=PROVIDER_REGISTRY)

    def test_raises_lists_checked_vars_in_message(self):
        with pytest.raises(RuntimeError, match="MINIMAX_API_KEY"):
            resolve_provider_routing("minimax:MiniMax-M2.7", {}, registry=PROVIDER_REGISTRY)


class TestRegistryCompleteness:
    """Smoke-check that every provider in the registry has a non-empty entry."""

    @pytest.mark.parametrize("prefix", PROVIDER_REGISTRY)
    def test_all_providers_have_key_vars_and_url(self, prefix):
        env_vars, base_url = PROVIDER_REGISTRY[prefix]
        assert env_vars, f"{prefix}: env_vars is empty"
        assert base_url.startswith("https://"), f"{prefix}: base_url looks wrong: {base_url}"
