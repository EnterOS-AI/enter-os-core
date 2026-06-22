import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import {
  appendMessageDeduped,
  appendMessageDedupedById,
  createMessage,
  type ChatMessage,
} from "../types";

// Unit tests for appendMessageDeduped — the helper that collapses the
// race between the HTTP /a2a .then() handler, the A2A_RESPONSE WS event,
// and the send_message_to_user push. All three paths can deliver the
// same agent reply; without dedupe the user sees 2-3 identical bubbles
// with identical timestamps.

describe("appendMessageDeduped", () => {
  beforeEach(() => {
    vi.useFakeTimers();
    // Pin Date.now so "recently added" windows are deterministic across
    // the dedupe + Date.parse calls inside the helper.
    vi.setSystemTime(new Date("2026-04-23T12:00:00.000Z"));
  });

  afterEach(() => {
    vi.useRealTimers();
  });

  it("appends a new message when the history is empty", () => {
    const msg = createMessage("agent", "hello");
    const next = appendMessageDeduped([], msg);
    expect(next).toHaveLength(1);
    expect(next[0]).toBe(msg);
  });

  it("appends when content differs from the recent tail", () => {
    const first = createMessage("agent", "hello");
    vi.advanceTimersByTime(100);
    const second = createMessage("agent", "world");
    const next = appendMessageDeduped([first], second);
    expect(next).toHaveLength(2);
  });

  it("skips a duplicate (same role+content) within the window", () => {
    const first = createMessage("agent", "Hey! How can I help you today?");
    vi.advanceTimersByTime(500); // well inside the 3s window
    const dup = createMessage("agent", "Hey! How can I help you today?");
    const next = appendMessageDeduped([first], dup);
    expect(next).toHaveLength(1);
    // The array is returned unchanged — not a new reference.
    expect(next[0]).toBe(first);
  });

  it("does NOT dedupe across different roles even if content matches", () => {
    // Agent echoing the user's "hi" is a legitimate two-bubble case.
    const user = createMessage("user", "hi");
    vi.advanceTimersByTime(100);
    const agent = createMessage("agent", "hi");
    const next = appendMessageDeduped([user], agent);
    expect(next).toHaveLength(2);
  });

  it("does NOT dedupe once the window has elapsed", () => {
    // A user legitimately sending "hi" a few seconds apart must render
    // both bubbles. Default window is 3000 ms.
    const first = createMessage("user", "hi");
    vi.advanceTimersByTime(4000);
    const repeat = createMessage("user", "hi");
    const next = appendMessageDeduped([first], repeat);
    expect(next).toHaveLength(2);
  });

  it("only checks the tail's content, not the entire history", () => {
    // Same (role, content) appearing earlier in the conversation but
    // outside the dedupe window is not a duplicate.
    const old = createMessage("agent", "hi");
    vi.advanceTimersByTime(10_000);
    const newer = createMessage("agent", "hi");
    const next = appendMessageDeduped([old], newer);
    expect(next).toHaveLength(2);
  });

  it("handles malformed timestamps without throwing", () => {
    // Defense: a history entry with a bogus timestamp shouldn't nuke
    // the append path. The helper should just treat that entry as
    // "too old to dedupe against" and append the new message.
    const garbled: ChatMessage = {
      id: "x",
      role: "agent",
      content: "hi",
      timestamp: "not-a-real-timestamp",
    };
    const fresh = createMessage("agent", "hi");
    expect(() => appendMessageDeduped([garbled], fresh)).not.toThrow();
    const next = appendMessageDeduped([garbled], fresh);
    expect(next).toHaveLength(2);
  });

  it("accepts a custom dedupe window", () => {
    const first = createMessage("agent", "hello");
    vi.advanceTimersByTime(500);
    // Tight 100 ms window — the 500 ms-old first message falls outside.
    const dup = createMessage("agent", "hello");
    const next = appendMessageDeduped([first], dup, 100);
    expect(next).toHaveLength(2);
  });
});

// Cross-device sync deduper (core#2697). The server fans out a
// USER_MESSAGE WS event after a canvas user's outbound chat message
// is durably persisted. Origin device already optimistically added
// the message via onUserMessage with the same id (the id IS the
// crypto.randomUUID() the client sent in the A2A envelope's
// message.messageId). On the WS echo, appendMessageDedupedById
// MUST collapse the duplicate so origin device renders one bubble.
// Other devices (and the origin after a reload) receive the
// broadcast with no prior copy and append fresh.
//
// The id-based dedup is strictly stronger than the time-windowed
// one above: a match on id collapses regardless of timing. This
// is the contract the cross-device-sync feature depends on.

describe("appendMessageDedupedById", () => {
  // Same setup as the appendMessageDeduped block above: the
  // cross-device-sync tests don't strictly need fake timers (the
  // id-based dedup is time-independent), but the timer-advance
  // case in the "content matches but id differs" test (line 180
  // in the prior head) requires vi.useFakeTimers to be active,
  // otherwise vi.advanceTimersByTime is a no-op. Adding the
  // same hooks here is consistent with the sibling describe
  // block + protects any future test in this block from the
  // same trap.
  beforeEach(() => {
    vi.useFakeTimers();
    vi.setSystemTime(new Date("2026-04-23T12:00:00.000Z"));
  });
  afterEach(() => {
    vi.useRealTimers();
  });

  it("appends a new message when no prior entry shares the id", () => {
    const msg = createMessage("user", "hello");
    const next = appendMessageDedupedById([], msg);
    expect(next).toHaveLength(1);
    expect(next[0]).toBe(msg);
  });

  it("collapses a duplicate with the same id (origin device's WS echo)", () => {
    // Origin device already optimistically added the message with
    // the id. Server fans out a USER_MESSAGE event with the same
    // id. The deduper MUST collapse to a single bubble.
    const optimistic = createMessage("user", "hello");
    // The server's WS echo carries the same id as the optimistic
    // add. We simulate that by reusing optimistic.id on a fresh
    // ChatMessage object (mirrors the broadcast shape).
    const echo: ChatMessage = {
      id: optimistic.id,
      role: "user",
      content: "hello",
      timestamp: new Date().toISOString(),
    };
    const next = appendMessageDedupedById([optimistic], echo);
    expect(next).toHaveLength(1);
    // The original entry is preserved (the array is not a new
    // reference, no re-render).
    expect(next[0]).toBe(optimistic);
  });

  it("appends when ids differ (other device receives a fresh broadcast)", () => {
    const first = createMessage("user", "hello");
    const second = createMessage("user", "hello");
    // Different crypto.randomUUID() per createMessage call — ids
    // are independent even when content matches.
    expect(first.id).not.toBe(second.id);
    const next = appendMessageDedupedById([first], second);
    expect(next).toHaveLength(2);
  });

  it("does NOT dedupe when msg.id is empty (fallback path)", () => {
    // Defense: a message without an id (e.g. a legacy shape from
    // an older broadcast or a test fixture) must NOT match against
    // the entire history — that would silently drop a legitimate
    // second message. Append fresh.
    const first = createMessage("user", "hello");
    const noId: ChatMessage = {
      id: "",
      role: "user",
      content: "world",
      timestamp: new Date().toISOString(),
    };
    const next = appendMessageDedupedById([first], noId);
    expect(next).toHaveLength(2);
  });

  it("does NOT collapse entries that share content but not id", () => {
    // Same content + different id = two distinct user messages
    // (the user typed the same thing twice). Must render both.
    const first = createMessage("user", "hi");
    vi.advanceTimersByTime(50);
    const second = createMessage("user", "hi");
    const next = appendMessageDedupedById([first], second);
    expect(next).toHaveLength(2);
  });
});

// createMessage id threading (core#2697 regression guard).
//
// The cross-device dedup above is only correct if the optimistic
// bubble's id EQUALS the messageId the sender puts in the A2A
// envelope (which the server echoes back as the USER_MESSAGE
// message_id). The original ship generated those as TWO independent
// crypto.randomUUID()s — so the echo never matched and the origin
// device rendered its own message twice. The fix threads one id:
// useChatSend mints `messageId` once, passes it to createMessage AND
// the payload. These tests pin that createMessage honors a supplied
// id so the wiring can't silently regress.
describe("createMessage id threading", () => {
  it("uses a supplied id verbatim (sender threads its messageId)", () => {
    const mid = "11111111-2222-3333-4444-555555555555";
    const msg = createMessage("user", "hi", undefined, undefined, mid);
    expect(msg.id).toBe(mid);
  });

  it("generates a uuid when no id is supplied (back-compat)", () => {
    const a = createMessage("agent", "hi");
    const b = createMessage("agent", "hi");
    expect(a.id).toBeTruthy();
    expect(a.id).not.toBe(b.id);
  });

  it("the threaded id makes the USER_MESSAGE echo a no-op (end-to-end of the fix)", () => {
    // Simulate the real send: one id for both the optimistic bubble
    // and the server echo (which carries the same messageId).
    const mid = "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee";
    const optimistic = createMessage("user", "can you check this issue for me", undefined, undefined, mid);
    const echo: ChatMessage = {
      id: mid, // server broadcast pins message_id == sent messageId
      role: "user",
      content: "can you check this issue for me",
      timestamp: new Date().toISOString(),
    };
    const next = appendMessageDedupedById([optimistic], echo);
    expect(next).toHaveLength(1); // single bubble — the reported dup is gone
    expect(next[0]).toBe(optimistic);
  });
});
