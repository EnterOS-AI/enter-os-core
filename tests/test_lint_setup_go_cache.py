"""Unit tests for lint_setup_go_cache — fixture catch + clean proofs."""
import importlib.util
import os
import textwrap

import pytest

HERE = os.path.dirname(__file__)
SCRIPT = os.path.join(HERE, "..", ".gitea", "scripts", "lint_setup_go_cache.py")
spec = importlib.util.spec_from_file_location("lint_setup_go_cache", SCRIPT)
mod = importlib.util.module_from_spec(spec)
spec.loader.exec_module(mod)


def _write(tmp_path, body):
    p = tmp_path / "wf.yml"
    p.write_text(textwrap.dedent(body))
    return str(p)


def test_cache_true_explicit_flagged(tmp_path):
    p = _write(tmp_path, """\
        jobs:
          build:
            runs-on: docker-host
            steps:
              - uses: actions/setup-go@v5
                with:
                  go-version: 'stable'
                  cache: true
                  cache-dependency-path: go.sum
    """)
    viols = mod.scan_file(p)
    assert len(viols) == 1
    assert "cache: true" in viols[0][1]


def test_default_true_with_cachedep_flagged(tmp_path):
    p = _write(tmp_path, """\
        jobs:
          build:
            runs-on: docker-host
            steps:
              - uses: actions/setup-go@v5
                with:
                  go-version: 'stable'
                  cache-dependency-path: go.sum
    """)
    viols = mod.scan_file(p)
    assert len(viols) == 1
    assert "defaults to true" in viols[0][1]


def test_bare_setup_go_default_true_flagged(tmp_path):
    p = _write(tmp_path, """\
        jobs:
          build:
            runs-on: docker-host
            steps:
              - uses: actions/setup-go@v5
                with:
                  go-version: 'stable'
              - run: go build ./...
    """)
    viols = mod.scan_file(p)
    assert len(viols) == 1
    assert "defaults to true" in viols[0][1]


def test_cache_false_clean(tmp_path):
    p = _write(tmp_path, """\
        jobs:
          build:
            runs-on: docker-host
            steps:
              - uses: actions/setup-go@v5
                with:
                  go-version: 'stable'
                  cache: false
                  cache-dependency-path: go.sum
    """)
    assert mod.scan_file(p) == []


def test_no_setup_go_clean(tmp_path):
    p = _write(tmp_path, """\
        jobs:
          build:
            runs-on: docker-host
            steps:
              - uses: actions/checkout@v4
              - run: echo hi
    """)
    assert mod.scan_file(p) == []


def test_multiple_steps_only_bad_flagged(tmp_path):
    p = _write(tmp_path, """\
        jobs:
          build:
            runs-on: docker-host
            steps:
              - uses: actions/setup-go@v5
                with:
                  go-version: 'stable'
                  cache: false
              - uses: actions/setup-go@v6
                with:
                  go-version: '1.25'
                  cache: true
    """)
    viols = mod.scan_file(p)
    assert len(viols) == 1
    assert "cache: true" in viols[0][1]
