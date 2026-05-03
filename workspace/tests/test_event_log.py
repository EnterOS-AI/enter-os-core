"""Tests for workspace/event_log.py — append/query/eviction/disabled backend."""

import threading
import time

import pytest

from event_log import (
    DisabledEventLog,
    Event,
    InMemoryEventLog,
    create_event_log,
)


# ---------------------------------------------------------------------------
# InMemoryEventLog — append + query basics
# ---------------------------------------------------------------------------


def test_append_returns_event_with_assigned_id():
    """append() returns the persisted Event with a monotonic id starting at 1."""
    log = InMemoryEventLog()

    e1 = log.append("turn.started", {"task_id": "t1"})
    e2 = log.append("turn.completed", {"task_id": "t1"})

    assert e1.id == 1
    assert e2.id == 2
    assert e1.kind == "turn.started"
    assert e2.kind == "turn.completed"
    assert e1.payload == {"task_id": "t1"}


def test_append_with_no_payload_yields_empty_dict():
    """payload omitted → empty dict, not None — so JSON serialisers don't choke."""
    log = InMemoryEventLog()
    e = log.append("ping")
    assert e.payload == {}
    assert isinstance(e.payload, dict)


def test_append_copies_payload_so_caller_mutations_dont_leak():
    """The persisted payload must NOT alias the caller's dict — otherwise
    a downstream mutation of the original silently rewrites history."""
    log = InMemoryEventLog()
    payload = {"k": "v"}
    e = log.append("evt", payload)
    payload["k"] = "MUTATED"
    assert e.payload == {"k": "v"}
    assert log.query()[0].payload == {"k": "v"}


def test_query_no_args_returns_all_resident_events_in_order():
    """query() with no cursor returns every resident event, ascending by id."""
    log = InMemoryEventLog()
    log.append("a")
    log.append("b")
    log.append("c")

    out = log.query()
    assert [e.kind for e in out] == ["a", "b", "c"]
    assert [e.id for e in out] == [1, 2, 3]


def test_query_since_cursor_returns_only_newer_events():
    """query(since=N) returns only events with id > N — strict greater-than."""
    log = InMemoryEventLog()
    log.append("a")
    log.append("b")
    log.append("c")

    out = log.query(since=2)
    assert [e.kind for e in out] == ["c"]
    assert out[0].id == 3


def test_query_since_at_or_past_tip_returns_empty():
    """A cursor at the current tip (or past it) yields no events."""
    log = InMemoryEventLog()
    log.append("a")
    log.append("b")

    assert log.query(since=2) == []
    assert log.query(since=999) == []


def test_query_limit_caps_returned_slice():
    """limit caps the slice; unspecified means unlimited."""
    log = InMemoryEventLog()
    for i in range(5):
        log.append(f"e{i}")

    capped = log.query(limit=2)
    assert [e.kind for e in capped] == ["e0", "e1"]

    unlimited = log.query()
    assert len(unlimited) == 5


def test_query_limit_zero_returns_empty_list():
    """limit=0 is a valid request for the empty slice (some pagination
    UIs probe for "any new events?" with limit=0 + since=cursor)."""
    log = InMemoryEventLog()
    log.append("a")
    assert log.query(limit=0) == []


def test_query_combined_since_and_limit():
    """since + limit compose: skip past cursor, then cap."""
    log = InMemoryEventLog()
    for i in range(10):
        log.append(f"e{i}")

    out = log.query(since=3, limit=2)
    assert [e.id for e in out] == [4, 5]


# ---------------------------------------------------------------------------
# Eviction — TTL + max_entries
# ---------------------------------------------------------------------------


def test_max_entries_evicts_oldest_first_fifo():
    """Exceeding max_entries evicts in FIFO order — newest survive."""
    log = InMemoryEventLog(max_entries=3)
    for i in range(5):
        log.append(f"e{i}")

    out = log.query()
    assert [e.kind for e in out] == ["e2", "e3", "e4"]
    assert [e.id for e in out] == [3, 4, 5]


def test_max_entries_evicted_ids_never_resurface_via_cursor():
    """A cursor pointing past evicted ids returns the resident tail.
    Important: the reader does NOT see an error — they see "everything
    after my cursor that's still here". This is the documented
    at-most-once-while-resident contract."""
    log = InMemoryEventLog(max_entries=2)
    for i in range(5):
        log.append(f"e{i}")

    # Reader's last seen cursor was id=1, but events 1+2 have aged out.
    # They should still get the resident tail (4, 5) without a crash.
    out = log.query(since=1)
    assert [e.id for e in out] == [4, 5]


def test_ttl_evicts_entries_older_than_ttl_seconds():
    """TTL eviction triggers on append when the oldest entry has aged
    past ttl_seconds. Uses an injected clock so the test is hermetic."""
    clock = [1000.0]
    log = InMemoryEventLog(ttl_seconds=10, now=lambda: clock[0])

    log.append("old")  # timestamp 1000
    clock[0] = 1005.0
    log.append("mid")  # timestamp 1005
    clock[0] = 1015.0  # past TTL of "old" (1000+10=1010 < 1015)
    log.append("new")  # this triggers eviction sweep

    out = log.query()
    assert [e.kind for e in out] == ["mid", "new"]


def test_ttl_evicts_on_query_when_appends_pause():
    """Read-side TTL sweep — covers the case where appends stop but
    a reader keeps polling. Without this, a stale tail would survive
    forever once writes pause."""
    clock = [1000.0]
    log = InMemoryEventLog(ttl_seconds=10, now=lambda: clock[0])

    log.append("only")
    # No more appends. Advance well past TTL.
    clock[0] = 2000.0

    assert log.query() == []


def test_clear_drops_all_but_preserves_id_counter():
    """clear() drops every resident event but does NOT reset the id
    counter — the cursor contract is monotonic ids across the
    process lifetime, even across clears (which are test-only)."""
    log = InMemoryEventLog()
    log.append("a")
    log.append("b")

    log.clear()
    assert log.query() == []

    e = log.append("c")
    assert e.id == 3  # counter resumes, not reset


def test_non_positive_ttl_falls_back_to_default():
    """Defensive: a 0 or negative ttl_seconds at construction falls
    back to the documented 3600s default. Disabling eviction silently
    would leak memory; that's what backend=disabled is for."""
    log = InMemoryEventLog(ttl_seconds=0)
    assert log._ttl_seconds == InMemoryEventLog._DEFAULT_TTL_SECONDS

    log2 = InMemoryEventLog(ttl_seconds=-5)
    assert log2._ttl_seconds == InMemoryEventLog._DEFAULT_TTL_SECONDS


def test_non_positive_max_entries_falls_back_to_default():
    """Same defensive shape for max_entries."""
    log = InMemoryEventLog(max_entries=0)
    assert log._max_entries == InMemoryEventLog._DEFAULT_MAX_ENTRIES

    log2 = InMemoryEventLog(max_entries=-1)
    assert log2._max_entries == InMemoryEventLog._DEFAULT_MAX_ENTRIES


# ---------------------------------------------------------------------------
# Event.to_dict — wire-format ownership pinning
# ---------------------------------------------------------------------------


def test_event_to_dict_contains_all_fields():
    """to_dict() returns the JSON-serialisable shape API consumers expect.
    Pinning the wire format here means a future rename of ``kind`` flips
    in event_log.py rather than in every reader."""
    e = Event(id=42, timestamp=1700.5, kind="turn.started", payload={"x": 1})
    d = e.to_dict()
    assert d == {"id": 42, "timestamp": 1700.5, "kind": "turn.started", "payload": {"x": 1}}


def test_event_timestamp_is_set_at_append():
    """timestamp on a logged event is the value of the injected clock at
    append time, not query time — so the wire timestamp reflects when
    the event happened, not when it was read."""
    clock = [1234.5]
    # Wide ttl so the read-side TTL sweep doesn't evict the event we
    # just wrote when we advance the clock to read it back.
    log = InMemoryEventLog(ttl_seconds=100_000, now=lambda: clock[0])
    log.append("evt")
    clock[0] = 9999.0
    [e] = log.query()
    assert e.timestamp == 1234.5


# ---------------------------------------------------------------------------
# DisabledEventLog — no-op contract
# ---------------------------------------------------------------------------


def test_disabled_query_always_empty():
    """Disabled backend never retains anything — query is always []."""
    log = DisabledEventLog()
    log.append("a")
    log.append("b")
    assert log.query() == []
    assert log.query(since=0) == []


def test_disabled_append_returns_event_with_monotonic_ids():
    """Even when nothing is persisted, append returns an Event with a
    monotonic id so callers that propagate the id (e.g. for a debug
    log) don't crash."""
    log = DisabledEventLog()
    e1 = log.append("a")
    e2 = log.append("b")
    assert e1.id == 1
    assert e2.id == 2
    assert e1.kind == "a"


def test_disabled_clear_is_a_no_op():
    """clear() on disabled returns None and changes nothing."""
    log = DisabledEventLog()
    log.append("a")
    log.clear()
    assert log.query() == []


# ---------------------------------------------------------------------------
# create_event_log factory
# ---------------------------------------------------------------------------


@pytest.mark.parametrize(
    "name", ["memory", "MEMORY", " memory ", "", "redis", "unknown"]
)
def test_create_event_log_memory_default(name):
    """Default + unknown + redis-not-yet-wired all resolve to in-memory.
    A typo or future-backend name should NOT silently disable telemetry."""
    log = create_event_log(backend=name)
    assert isinstance(log, InMemoryEventLog)


@pytest.mark.parametrize("name", ["disabled", "DISABLED", " off ", "none"])
def test_create_event_log_disabled_aliases(name):
    """``disabled``, ``off``, ``none`` all opt the workspace out."""
    log = create_event_log(backend=name)
    assert isinstance(log, DisabledEventLog)


def test_create_event_log_passes_bounds_through():
    """ttl_seconds and max_entries flow into the InMemoryEventLog instance."""
    log = create_event_log(backend="memory", ttl_seconds=42, max_entries=99)
    assert isinstance(log, InMemoryEventLog)
    assert log._ttl_seconds == 42
    assert log._max_entries == 99


# ---------------------------------------------------------------------------
# Concurrency — append from multiple threads under contention
# ---------------------------------------------------------------------------


def test_concurrent_appends_assign_unique_monotonic_ids():
    """Multiple writer threads must not collide on the id counter.
    Heartbeat thread + main loop + A2A executor all append concurrently
    in production; a duplicated id would break cursor-based readers."""
    log = InMemoryEventLog(max_entries=10_000)
    n_threads = 8
    n_per_thread = 200

    def worker():
        for _ in range(n_per_thread):
            log.append("e")

    threads = [threading.Thread(target=worker) for _ in range(n_threads)]
    for t in threads:
        t.start()
    for t in threads:
        t.join()

    out = log.query()
    ids = [e.id for e in out]
    assert len(ids) == n_threads * n_per_thread
    assert len(set(ids)) == len(ids)  # all unique
    assert ids == sorted(ids)  # ascending order preserved


def test_real_clock_default_uses_time_time():
    """When ``now`` is not passed, the log uses ``time.time`` — sanity
    check that the production path is wired and that an event's
    timestamp matches the wall clock within a small epsilon."""
    log = InMemoryEventLog()
    before = time.time()
    e = log.append("evt")
    after = time.time()
    assert before <= e.timestamp <= after
