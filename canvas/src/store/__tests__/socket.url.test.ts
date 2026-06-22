import { describe, it, expect, vi, afterEach } from "vitest";

// Helper: reset modules, set env vars, import module, then restore env.
async function importWsUrl(env: Record<string, string | undefined>) {
  vi.resetModules();
  const saved: Record<string, string | undefined> = {};
  for (const [k, v] of Object.entries(env)) {
    saved[k] = process.env[k];
    if (v === undefined) delete process.env[k];
    else process.env[k] = v;
  }
  const mod = await import("@/store/socket");
  // Restore env
  for (const [k, v] of Object.entries(saved)) {
    if (v === undefined) delete process.env[k];
    else process.env[k] = v;
  }
  return mod;
}

describe("socket WS_URL derivation", () => {
  afterEach(() => {
    vi.resetModules();
    delete process.env.NEXT_PUBLIC_PLATFORM_URL;
    delete process.env.NEXT_PUBLIC_WS_URL;
  });

  it("falls back to ws://localhost:8080/ws when no env vars are set", async () => {
    const mod = await importWsUrl({
      NEXT_PUBLIC_PLATFORM_URL: undefined,
      NEXT_PUBLIC_WS_URL: undefined,
    });
    expect(mod.WS_URL).toBe("ws://localhost:8080/ws");
  });

  it("derives WS_URL from NEXT_PUBLIC_PLATFORM_URL by replacing http→ws and appending /ws", async () => {
    const mod = await importWsUrl({
      NEXT_PUBLIC_PLATFORM_URL: "http://api.example.com",
      NEXT_PUBLIC_WS_URL: undefined,
    });
    expect(mod.WS_URL).toBe("ws://api.example.com/ws");
  });

  it("handles https→wss correctly", async () => {
    const mod = await importWsUrl({
      NEXT_PUBLIC_PLATFORM_URL: "https://api.example.com",
      NEXT_PUBLIC_WS_URL: undefined,
    });
    expect(mod.WS_URL).toBe("wss://api.example.com/ws");
  });

  it("NEXT_PUBLIC_WS_URL takes precedence over derived value", async () => {
    const mod = await importWsUrl({
      NEXT_PUBLIC_PLATFORM_URL: "http://api.example.com",
      NEXT_PUBLIC_WS_URL: "wss://ws.example.com/custom",
    });
    expect(mod.WS_URL).toBe("wss://ws.example.com/custom");
  });

  it("PLATFORM_URL in api.ts falls back to localhost:8080", async () => {
    vi.resetModules();
    delete process.env.NEXT_PUBLIC_PLATFORM_URL;
    const mod = await import("@/lib/api");
    expect(mod.PLATFORM_URL).toBe("http://localhost:8080");
  });

  it("PLATFORM_URL in api.ts reads from NEXT_PUBLIC_PLATFORM_URL", async () => {
    vi.resetModules();
    process.env.NEXT_PUBLIC_PLATFORM_URL = "http://prod.example.com";
    const apiMod = await import("@/lib/api");
    expect(apiMod.PLATFORM_URL).toBe("http://prod.example.com");
    delete process.env.NEXT_PUBLIC_PLATFORM_URL;
  });
});
