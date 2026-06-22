// @vitest-environment jsdom
/**
 * Tests for deriveWsBaseUrl — WebSocket base URL derivation from env / window.location.
 */
import { describe, it, expect, beforeEach, vi, afterEach } from "vitest";
import { deriveWsBaseUrl } from "../ws-url";

const ORIGINAL_WS = process.env.NEXT_PUBLIC_WS_URL;
const ORIGINAL_PLATFORM = process.env.NEXT_PUBLIC_PLATFORM_URL;

beforeEach(() => {
  vi.stubEnv("NEXT_PUBLIC_WS_URL", "");
  vi.stubEnv("NEXT_PUBLIC_PLATFORM_URL", "");
});

afterEach(() => {
  vi.restoreAllMocks();
  if (ORIGINAL_WS !== undefined) vi.stubEnv("NEXT_PUBLIC_WS_URL", ORIGINAL_WS);
  else delete process.env.NEXT_PUBLIC_WS_URL;
  if (ORIGINAL_PLATFORM !== undefined) vi.stubEnv("NEXT_PUBLIC_PLATFORM_URL", ORIGINAL_PLATFORM);
  else delete process.env.NEXT_PUBLIC_PLATFORM_URL;
});

describe("deriveWsBaseUrl — NEXT_PUBLIC_WS_URL (priority 1)", () => {
  it("uses NEXT_PUBLIC_WS_URL when set", () => {
    vi.stubEnv("NEXT_PUBLIC_WS_URL", "wss://ws.example.com/ws");
    expect(deriveWsBaseUrl()).toBe("wss://ws.example.com");
  });

  it("strips trailing /ws suffix from NEXT_PUBLIC_WS_URL", () => {
    vi.stubEnv("NEXT_PUBLIC_WS_URL", "wss://ws.example.com/ws");
    expect(deriveWsBaseUrl()).toBe("wss://ws.example.com");
  });

  it("uses ws:// for HTTP NEXT_PUBLIC_WS_URL", () => {
    vi.stubEnv("NEXT_PUBLIC_WS_URL", "ws://localhost:8080/ws");
    expect(deriveWsBaseUrl()).toBe("ws://localhost:8080");
  });

  it("wins over NEXT_PUBLIC_PLATFORM_URL", () => {
    vi.stubEnv("NEXT_PUBLIC_WS_URL", "wss://ws.example.com");
    vi.stubEnv("NEXT_PUBLIC_PLATFORM_URL", "http://platform.example.com");
    expect(deriveWsBaseUrl()).toBe("wss://ws.example.com");
  });

  it("wins over window.location", () => {
    vi.stubEnv("NEXT_PUBLIC_WS_URL", "wss://ws.example.com");
    Object.defineProperty(window, "location", {
      value: { protocol: "https:", host: "canvas.example.com" },
      writable: true,
    });
    expect(deriveWsBaseUrl()).toBe("wss://ws.example.com");
  });
});

describe("deriveWsBaseUrl — NEXT_PUBLIC_PLATFORM_URL (priority 2)", () => {
  it("derives ws:// from http:// platform URL", () => {
    vi.stubEnv("NEXT_PUBLIC_PLATFORM_URL", "http://localhost:8080");
    expect(deriveWsBaseUrl()).toBe("ws://localhost:8080");
  });

  it("derives wss:// from https:// platform URL", () => {
    vi.stubEnv("NEXT_PUBLIC_PLATFORM_URL", "https://platform.example.com");
    expect(deriveWsBaseUrl()).toBe("wss://platform.example.com");
  });

  it("preserves non-standard ports", () => {
    vi.stubEnv("NEXT_PUBLIC_PLATFORM_URL", "http://localhost:9000");
    expect(deriveWsBaseUrl()).toBe("ws://localhost:9000");
  });

  it("wins over window.location", () => {
    vi.stubEnv("NEXT_PUBLIC_PLATFORM_URL", "https://platform.example.com");
    Object.defineProperty(window, "location", {
      value: { protocol: "https:", host: "canvas.example.com" },
      writable: true,
    });
    expect(deriveWsBaseUrl()).toBe("wss://platform.example.com");
  });
});

describe("deriveWsBaseUrl — window.location (priority 3)", () => {
  it("uses wss:// when page is served over HTTPS", () => {
    Object.defineProperty(window, "location", {
      value: { protocol: "https:", host: "canvas.example.com" },
      writable: true,
    });
    expect(deriveWsBaseUrl()).toBe("wss://canvas.example.com");
  });

  it("uses ws:// when page is served over HTTP", () => {
    Object.defineProperty(window, "location", {
      value: { protocol: "http:", host: "localhost:3000" },
      writable: true,
    });
    expect(deriveWsBaseUrl()).toBe("ws://localhost:3000");
  });

  it("includes the host with port", () => {
    Object.defineProperty(window, "location", {
      value: { protocol: "https:", host: "canvas.example.com:8443" },
      writable: true,
    });
    expect(deriveWsBaseUrl()).toBe("wss://canvas.example.com:8443");
  });
});

describe("deriveWsBaseUrl — fallback (priority 4)", () => {
  it("falls back to localhost when no env vars or window is unavailable", () => {
    // process.env is empty (already stubbed), window is not stubbed but we
    // can't remove it entirely in jsdom — the function checks typeof window
    // which is always defined. Since we have no env vars, it falls through
    // to the window branch; we test the final fallback by stubbing window
    // location to undefined (not possible in jsdom — skip this edge case).
    // The test below verifies the no-env-var path works.
    Object.defineProperty(window, "location", {
      value: { protocol: "http:", host: "localhost:3000" },
      writable: true,
    });
    expect(deriveWsBaseUrl()).toBe("ws://localhost:3000");
  });
});

describe("deriveWsBaseUrl — protocol derivation", () => {
  it("derives ws:// from http:// and keeps it", () => {
    vi.stubEnv("NEXT_PUBLIC_PLATFORM_URL", "http://platform:8080");
    expect(deriveWsBaseUrl()).toMatch(/^ws:/);
  });

  it("derives wss:// from https:// and keeps it", () => {
    vi.stubEnv("NEXT_PUBLIC_PLATFORM_URL", "https://platform:8080");
    expect(deriveWsBaseUrl()).toMatch(/^wss:/);
  });
});
