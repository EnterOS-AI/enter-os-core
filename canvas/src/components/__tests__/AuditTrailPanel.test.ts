// @vitest-environment jsdom
/**
 * Unit tests for formatAuditRelativeTime — pure date formatter from AuditTrailPanel.
 */
import { describe, it, expect } from "vitest";
import { formatAuditRelativeTime } from "../AuditTrailPanel";

describe("formatAuditRelativeTime", () => {
  it('returns "just now" for timestamps within the last minute', () => {
    const now = 1_700_000_000_000;
    const thirtySecAgo = new Date(now - 30_000).toISOString();
    expect(formatAuditRelativeTime(thirtySecAgo, now)).toBe("just now");
  });

  it('returns "Xm ago" for timestamps within the last hour', () => {
    const now = 1_700_000_000_000;
    const fiveMinAgo = new Date(now - 5 * 60_000).toISOString();
    expect(formatAuditRelativeTime(fiveMinAgo, now)).toBe("5m ago");
  });

  it('returns "Xh ago" for timestamps within the last day', () => {
    const now = 1_700_000_000_000;
    const threeHoursAgo = new Date(now - 3 * 3_600_000).toISOString();
    expect(formatAuditRelativeTime(threeHoursAgo, now)).toBe("3h ago");
  });

  it("returns locale date string for timestamps older than 24h", () => {
    const now = 1_700_000_000_000;
    const twoDaysAgo = new Date(now - 2 * 86_400_000).toISOString();
    const result = formatAuditRelativeTime(twoDaysAgo, now);
    // Should be a date string (not "Xh ago" or "Xm ago")
    expect(result).not.toMatch(/m ago|h ago|just now/);
    expect(result).toBe(new Date(twoDaysAgo).toLocaleDateString());
  });

  it("handles the boundary between minute and hour correctly", () => {
    const now = 1_700_000_000_000;
    const exactlyOneHourAgo = new Date(now - 3_600_000).toISOString();
    expect(formatAuditRelativeTime(exactlyOneHourAgo, now)).toBe("1h ago");
  });

  it("handles the boundary between hour and day correctly", () => {
    const now = 1_700_000_000_000;
    // 23h ago is < 24h so it shows "23h ago"; exactly 24h falls through to date string
    const twentyThreeHoursAgo = new Date(now - 23 * 3_600_000).toISOString();
    expect(formatAuditRelativeTime(twentyThreeHoursAgo, now)).toBe("23h ago");
  });

  it("returns locale date string for exactly 24h ago (boundary)", () => {
    const now = 1_700_000_000_000;
    const exactlyOneDayAgo = new Date(now - 86_400_000).toISOString();
    const result = formatAuditRelativeTime(exactlyOneDayAgo, now);
    // diff is exactly 86_400_000, which is NOT < 86_400_000, so it falls through
    expect(result).toBe(new Date(exactlyOneDayAgo).toLocaleDateString());
  });

  it("future timestamps return 'just now' (negative diff < 60_000)", () => {
    const now = 1_700_000_000_000;
    const future = new Date(now + 60_000).toISOString();
    // Negative diff passes diff < 60_000, returning "just now"
    expect(formatAuditRelativeTime(future, now)).toBe("just now");
  });
});
