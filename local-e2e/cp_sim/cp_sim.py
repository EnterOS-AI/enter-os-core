"""Tenant control-plane simulator.

Emits the byte-identical JSON-RPC `message/send` wire shape that the
production `workspace-server` POSTs to the runtime's :8000 — see
``workspace-server/internal/handlers/a2a.go`` and the canonical sample
in ``tests/e2e/test_chat_attachments_e2e.sh``.

This file is purposefully small (~250 LoC). It is NOT a re-implementation
of `workspace-server`; it is just the minimum surface required to drive
the 4 session-continuity canaries.

If the runtime asserts on a header / envelope field that the production
platform sets but this simulator omits, FIX THE SIMULATOR — never weaken
the runtime to accept divergent wire shapes. The simulator is the
canonical contract emitter for canary purposes
(``feedback_no_single_source_of_truth``).
"""

from __future__ import annotations

import base64
import json
import os
import uuid
from dataclasses import dataclass
from typing import Any

import httpx


@dataclass
class CPSimConfig:
    runtime_url: str
    """Base URL of the runtime under test (e.g. http://runtime:8000)."""
    request_timeout_s: float = 60.0
    """Per-A2A-call timeout. Generous — canary mode replies are fast,
    but a real Provider-backed runtime under cold cache can take 30+s."""


class CPSim:
    """Thin client matching workspace-server's wire shape."""

    def __init__(self, cfg: CPSimConfig | None = None) -> None:
        self.cfg = cfg or CPSimConfig(
            runtime_url=os.environ.get("RUNTIME_URL", "http://localhost:18000"),
        )
        self._client = httpx.Client(timeout=self.cfg.request_timeout_s)

    # ------------------------------------------------------------------ A2A

    def send_text(
        self,
        text: str,
        *,
        context_id: str,
        task_id: str | None = None,
    ) -> dict[str, Any]:
        """POST a text-only A2A message. Returns the JSON-RPC envelope."""
        msg_id = f"canary-{uuid.uuid4().hex[:12]}"
        payload = {
            "jsonrpc": "2.0",
            "id": msg_id,
            "method": "message/send",
            "params": {
                "message": {
                    "role": "user",
                    "messageId": msg_id,
                    "kind": "message",
                    "contextId": context_id,
                    "taskId": task_id,
                    "parts": [{"kind": "text", "text": text}],
                },
                "configuration": {
                    "acceptedOutputModes": ["text/plain"],
                    "blocking": True,
                },
            },
        }
        return self._post(payload)

    def send_with_file(
        self,
        *,
        context_id: str,
        text: str | None,
        file_name: str,
        file_bytes: bytes,
        mime_type: str = "text/plain",
        task_id: str | None = None,
    ) -> dict[str, Any]:
        """POST an A2A message with an inline file part.

        Uses the inline `bytes` form of A2A file parts (RFC#600 — the
        no-URI variant added precisely so canary tests don't need a
        `/chat/uploads` round-trip). Each runtime's executor calls
        ``extract_attached_files`` which handles both forms — verified
        in ``workspace/executor_helpers.py:903``.
        """
        msg_id = f"canary-{uuid.uuid4().hex[:12]}"
        parts: list[dict[str, Any]] = []
        if text:
            parts.append({"kind": "text", "text": text})
        parts.append(
            {
                "kind": "file",
                "file": {
                    "name": file_name,
                    "mimeType": mime_type,
                    "bytes": base64.b64encode(file_bytes).decode("ascii"),
                },
            }
        )
        payload = {
            "jsonrpc": "2.0",
            "id": msg_id,
            "method": "message/send",
            "params": {
                "message": {
                    "role": "user",
                    "messageId": msg_id,
                    "kind": "message",
                    "contextId": context_id,
                    "taskId": task_id,
                    "parts": parts,
                },
                "configuration": {
                    "acceptedOutputModes": ["text/plain"],
                    "blocking": True,
                },
            },
        }
        return self._post(payload)

    # ------------------------------------------------------------ helpers

    def _post(self, payload: dict[str, Any]) -> dict[str, Any]:
        url = f"{self.cfg.runtime_url}/a2a"
        try:
            r = self._client.post(url, json=payload)
        except httpx.HTTPError as e:
            raise CPSimError(f"A2A POST failed: {e}") from e
        if r.status_code != 200:
            raise CPSimError(
                f"A2A non-200: status={r.status_code} body={r.text[:500]}"
            )
        try:
            return r.json()
        except json.JSONDecodeError as e:
            raise CPSimError(f"A2A body not JSON: {r.text[:500]}") from e

    @staticmethod
    def extract_text_parts(envelope: dict[str, Any]) -> str:
        """Return concatenated text from all text parts of a reply.

        Handles both top-level `result.parts` (the canonical shape) and
        `result.artifacts[*].parts` (which some runtimes emit when the
        reply was streamed as artifact chunks). Matches the extractor in
        ``tests/e2e/test_chat_attachments_e2e.sh``.
        """
        result = envelope.get("result") or {}
        chunks: list[str] = []
        for p in result.get("parts", []) or []:
            if p.get("kind") == "text":
                chunks.append(p.get("text", ""))
        for art in result.get("artifacts", []) or []:
            for p in art.get("parts", []) or []:
                if p.get("kind") == "text":
                    chunks.append(p.get("text", ""))
        # Some runtimes return a status.message instead of/in addition to parts.
        status = result.get("status") or {}
        status_msg = status.get("message") or {}
        for p in status_msg.get("parts", []) or []:
            if p.get("kind") == "text":
                chunks.append(p.get("text", ""))
        return "\n".join(chunks).strip()

    # ----------------------------------------------------- memory probe

    def probe_memory(self, key: str) -> str | None:
        """Read a memory value via the runtime's MCP memory tool.

        Uses the same MCP transport the canvas uses
        (``POST /workspaces/:id/mcp``-shaped JSON-RPC over /mcp).  Returns
        the recalled string or None if the key is missing.
        """
        payload = {
            "jsonrpc": "2.0",
            "id": f"canary-mem-{uuid.uuid4().hex[:8]}",
            "method": "tools/call",
            "params": {"name": "recall_memory", "arguments": {"key": key}},
        }
        try:
            r = self._client.post(f"{self.cfg.runtime_url}/mcp", json=payload)
        except httpx.HTTPError as e:
            raise CPSimError(f"MCP POST failed: {e}") from e
        if r.status_code != 200:
            return None
        body = r.json()
        result = body.get("result") or {}
        # MCP responses wrap the tool output in result.content[*].text per
        # the JSON-RPC tools/call contract.
        for c in result.get("content", []) or []:
            if c.get("type") == "text":
                return c.get("text")
        return None


class CPSimError(RuntimeError):
    """Raised on transport / envelope failures (NOT canary assertion failures).

    Distinct from AssertionError so pytest reports them as ERROR not
    FAILED — a transport-layer fault should be debugged differently from
    a real session-continuity regression.
    """
