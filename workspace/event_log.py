"""Workspace event log — append-and-query buffer for runtime events.

Hermes-style declarative observability primitive. Adapter and platform
code emit semantic events (turn started, tool invoked, peer message
delivered) and external readers — the canvas Activity tab, A2A peers,
and the platform's `/workspaces/:id/activity` endpoint — query them
with a cursor.

Today's PR ships the in-memory backend only. Redis backend lands in
the follow-up that wires platform-side fan-out (#119 PR-3 follow-up).
The Protocol shape lets a future backend swap in without touching the
emitting sites.

Eviction is the load-bearing invariant: the workspace runtime is
long-lived, so an unbounded list would leak memory. Every append
prunes by both TTL and max_entries; readers that fall behind past
the eviction frontier see a contiguous tail without an error — the
cursor protocol only guarantees "events with id > since that are
still resident", not "every event ever appended". A reader that
needs at-least-once delivery must poll faster than the eviction TTL.
"""

from __future__ import annotations

import threading
import time
from collections import deque
from dataclasses import asdict, dataclass, field
from typing import Any, Deque, Iterable, Optional, Protocol


@dataclass(frozen=True)
class Event:
    """One immutable entry in the event log.

    ``id`` is a monotonic integer assigned at append time. It SURVIVES
    eviction — the counter is never reset when an old event drops out
    of the buffer, so a reader's cursor stays valid even if the event
    it points to has aged out (the next query just returns the resident
    tail). This is the contract that lets a slow reader reconnect
    without resetting to id=0.
    """

    id: int
    timestamp: float
    """Seconds since the Unix epoch — the same shape as ``time.time()``
    so callers can format with ``datetime.fromtimestamp`` without an
    extra conversion. Float, not int, because event-bursts within the
    same second need stable ordering for downstream merging."""

    kind: str
    """Short tag categorising the event: ``turn.started``, ``tool.invoked``,
    ``peer.message.delivered``, etc. Convention is dotted snake_case so
    the canvas can group by prefix without a parser."""

    payload: dict = field(default_factory=dict)
    """Arbitrary JSON-serialisable dict. Keep small — the in-memory
    backend holds every event in process RAM. Large blobs (file
    contents, full transcripts) belong in the platform's blob store
    with a reference here, not the value itself."""

    def to_dict(self) -> dict:
        """Plain-dict shape for JSON serialisation in the API layer.

        Wrapping ``dataclasses.asdict`` rather than relying on the
        consumer to call it themselves means the wire format stays
        owned by this module — a rename of ``kind`` to ``type`` (or
        whatever the canvas eventually settles on) flips here, not in
        every reader.
        """
        return asdict(self)


class EventLogBackend(Protocol):
    """Backend Protocol — the swap point for memory ↔ redis ↔ disabled.

    Implementations must be safe to call from multiple threads. The
    workspace runtime appends from the heartbeat thread, the agent's
    main loop, and any A2A executor concurrently; readers run on the
    HTTP server thread. A backend that needs locking owns it.
    """

    def append(self, kind: str, payload: Optional[dict] = None) -> Event:
        """Add an event and return the persisted record (with id assigned)."""
        ...

    def query(self, since: Optional[int] = None, limit: Optional[int] = None) -> list[Event]:
        """Return events with ``id > since`` (or all resident if ``since`` is None).

        Order is ascending by id. ``limit`` caps the returned slice;
        if the resident tail is shorter than ``limit``, returns what
        is available.
        """
        ...

    def clear(self) -> None:
        """Drop all entries. Provided for test isolation, not for production callers."""
        ...


class InMemoryEventLog:
    """Bounded in-memory ring buffer with TTL eviction.

    Two eviction triggers, both checked on every ``append`` (and on
    ``query`` for read-side freshness when older entries have aged
    past the TTL but no append has happened to evict them):

    - **TTL:** entries older than ``ttl_seconds`` are dropped.
    - **max_entries:** when the deque exceeds ``max_entries``, oldest
      drop until back at the cap.

    Both bounds are advisory at construction — non-positive values
    fall back to permissive defaults rather than disabling the log,
    because a misconfigured value should not silently lose events.
    To disable the log, use ``DisabledEventLog`` instead.

    The id counter is monotonic across the entire process lifetime;
    eviction does not reset it. A query with ``since=last_seen_id``
    returns the resident tail past that cursor, which may be empty if
    the reader is too far behind.
    """

    _DEFAULT_TTL_SECONDS = 3600  # 1 hour — covers a long agentic loop without leaking
    _DEFAULT_MAX_ENTRIES = 10_000  # ~1 MB at 100 bytes/event, safely under workspace RAM budget

    def __init__(
        self,
        ttl_seconds: int = _DEFAULT_TTL_SECONDS,
        max_entries: int = _DEFAULT_MAX_ENTRIES,
        now: Optional[Any] = None,
    ) -> None:
        self._ttl_seconds: int = ttl_seconds if ttl_seconds > 0 else self._DEFAULT_TTL_SECONDS
        self._max_entries: int = max_entries if max_entries > 0 else self._DEFAULT_MAX_ENTRIES
        # Injected clock for deterministic TTL tests. Production passes
        # ``time.time``; tests pass a callable that returns a controlled value.
        self._now = now if callable(now) else time.time
        self._lock = threading.Lock()
        self._next_id: int = 1
        self._buf: Deque[Event] = deque()

    def append(self, kind: str, payload: Optional[dict] = None) -> Event:
        with self._lock:
            event = Event(
                id=self._next_id,
                timestamp=self._now(),
                kind=kind,
                payload=dict(payload) if payload else {},
            )
            self._next_id += 1
            self._buf.append(event)
            self._evict_locked()
            return event

    def query(self, since: Optional[int] = None, limit: Optional[int] = None) -> list[Event]:
        with self._lock:
            # Read-side TTL sweep — covers the case where appends pause
            # but a reader keeps polling. Without this, a stale tail
            # would survive forever once writes stop.
            self._evict_locked()
            cutoff = since if since is not None else 0
            tail: Iterable[Event] = (e for e in self._buf if e.id > cutoff)
            if limit is not None and limit >= 0:
                if limit == 0:
                    # Explicit empty-slice probe — used by pagination
                    # UIs to ask "are there any new events?" without
                    # paying for the data. Distinct from limit=None
                    # (no cap) — return empty rather than the first event.
                    return []
                out: list[Event] = []
                for e in tail:
                    out.append(e)
                    if len(out) >= limit:
                        break
                return out
            return list(tail)

    def clear(self) -> None:
        with self._lock:
            self._buf.clear()
            # NOTE: do NOT reset _next_id — the cursor contract is that
            # ids are monotonic across the lifetime of the process, even
            # across explicit clears (which only happen in tests).

    def _evict_locked(self) -> None:
        """Caller MUST hold self._lock."""
        if not self._buf:
            return
        cutoff = self._now() - self._ttl_seconds
        while self._buf and self._buf[0].timestamp < cutoff:
            self._buf.popleft()
        # max_entries bound after TTL — a long buffer that fits the
        # window can still be capped if the burst rate exceeded design.
        while len(self._buf) > self._max_entries:
            self._buf.popleft()


class DisabledEventLog:
    """No-op backend for ``backend: disabled``.

    Append returns a synthetic event so callers that want the id
    don't crash; query always returns empty. The synthetic event is
    NOT cached anywhere — the contract for ``backend: disabled`` is
    that no state is retained. Operators who pick this backend opt
    out of the canvas Activity tab and the `/activity` endpoint.
    """

    def __init__(self) -> None:
        self._next_id: int = 1
        self._lock = threading.Lock()

    def append(self, kind: str, payload: Optional[dict] = None) -> Event:
        # Single-shot id increment — keeps the returned event ids
        # monotonic for callers that compare them, even though we
        # never persist anything.
        with self._lock:
            event = Event(
                id=self._next_id,
                timestamp=time.time(),
                kind=kind,
                payload=dict(payload) if payload else {},
            )
            self._next_id += 1
            return event

    def query(self, since: Optional[int] = None, limit: Optional[int] = None) -> list[Event]:
        return []

    def clear(self) -> None:
        return None


def create_event_log(
    backend: str = "memory",
    ttl_seconds: int = InMemoryEventLog._DEFAULT_TTL_SECONDS,
    max_entries: int = InMemoryEventLog._DEFAULT_MAX_ENTRIES,
) -> EventLogBackend:
    """Factory — pick a backend by name from EventLogConfig.

    Unknown backend strings fall back to ``memory`` rather than
    raising at boot. A typo'd config value should degrade to the
    safe default, not crash the workspace before any event can be
    recorded. The redis backend lands in a follow-up; until then
    ``backend: redis`` also resolves to in-memory.
    """
    name = (backend or "memory").strip().lower()
    if name in ("disabled", "off", "none"):
        return DisabledEventLog()
    # memory is the default; redis falls through here until it's wired.
    return InMemoryEventLog(ttl_seconds=ttl_seconds, max_entries=max_entries)
