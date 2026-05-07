// @vitest-environment node
import { describe, it, expect, beforeEach, afterEach, vi } from "vitest";

// Tests for the boot-time matched-pair guard added to next.config.ts.
//
// Why this lives in src/lib/__tests__ even though the function is in
// canvas/next.config.ts:
//   - next.config.ts runs as ESM-but-also-CJS depending on which
//     consumer loads it (Next.js dev server vs Next.js build); we
//     want the test to be a plain ESM module Vitest already handles.
//   - Importing from "../../../next.config" pulls in the rest of the
//     file (loadMonorepoEnv, the default export, etc.) which has
//     side effects on module load (it runs loadMonorepoEnv()
//     immediately). To keep the test hermetic we don't import — we
//     duplicate the function under test.
//
// Sourcing the function from a shared module would be cleaner, but
// next.config.ts is required to be a single self-contained file by
// Next.js's loader on some host configurations. Pin invariant: the
// duplicated function below MUST stay byte-identical to the one in
// next.config.ts. If you change one, change the other and bump this
// comment.

function checkAdminTokenPair(): void {
  const serverSet = !!process.env.ADMIN_TOKEN;
  const clientSet = !!process.env.NEXT_PUBLIC_ADMIN_TOKEN;
  if (serverSet === clientSet) return;
  if (serverSet && !clientSet) {
    // eslint-disable-next-line no-console
    console.error(
      "[next.config] ADMIN_TOKEN is set but NEXT_PUBLIC_ADMIN_TOKEN is not — " +
        "canvas will 401 against workspace-server because the bearer header " +
        "is never attached. Set both to the same value, or unset both.",
    );
  } else {
    // eslint-disable-next-line no-console
    console.error(
      "[next.config] NEXT_PUBLIC_ADMIN_TOKEN is set but ADMIN_TOKEN is not — " +
        "workspace-server will reject the bearer because no AdminAuth gate " +
        "is configured. Set both to the same value, or unset both.",
    );
  }
}

describe("checkAdminTokenPair", () => {
  // Snapshot env so individual tests can stomp on it without leaking.
  // Rebuild from snapshot in afterEach so the next test sees a known
  // baseline regardless of mutation pattern.
  let originalEnv: Record<string, string | undefined>;
  let errorSpy: ReturnType<typeof vi.spyOn>;

  beforeEach(() => {
    originalEnv = {
      ADMIN_TOKEN: process.env.ADMIN_TOKEN,
      NEXT_PUBLIC_ADMIN_TOKEN: process.env.NEXT_PUBLIC_ADMIN_TOKEN,
    };
    delete process.env.ADMIN_TOKEN;
    delete process.env.NEXT_PUBLIC_ADMIN_TOKEN;
    errorSpy = vi.spyOn(console, "error").mockImplementation(() => {});
  });

  afterEach(() => {
    if (originalEnv.ADMIN_TOKEN === undefined) delete process.env.ADMIN_TOKEN;
    else process.env.ADMIN_TOKEN = originalEnv.ADMIN_TOKEN;
    if (originalEnv.NEXT_PUBLIC_ADMIN_TOKEN === undefined) delete process.env.NEXT_PUBLIC_ADMIN_TOKEN;
    else process.env.NEXT_PUBLIC_ADMIN_TOKEN = originalEnv.NEXT_PUBLIC_ADMIN_TOKEN;
    errorSpy.mockRestore();
  });

  it("emits no warning when both are unset", () => {
    checkAdminTokenPair();
    expect(errorSpy).not.toHaveBeenCalled();
  });

  it("emits no warning when both are set (matched pair, the happy path)", () => {
    process.env.ADMIN_TOKEN = "local-dev-admin";
    process.env.NEXT_PUBLIC_ADMIN_TOKEN = "local-dev-admin";
    checkAdminTokenPair();
    expect(errorSpy).not.toHaveBeenCalled();
  });

  it("warns when ADMIN_TOKEN is set but NEXT_PUBLIC_ADMIN_TOKEN is not", () => {
    process.env.ADMIN_TOKEN = "local-dev-admin";
    checkAdminTokenPair();
    expect(errorSpy).toHaveBeenCalledTimes(1);
    // Exact-string assertion — substring would also pass when the
    // function's branch logic is broken (e.g. emits both messages, or
    // emits the wrong one). Pin the exact message that operators will
    // see in their dev console so regressions are visible.
    expect(errorSpy).toHaveBeenCalledWith(
      "[next.config] ADMIN_TOKEN is set but NEXT_PUBLIC_ADMIN_TOKEN is not — " +
        "canvas will 401 against workspace-server because the bearer header " +
        "is never attached. Set both to the same value, or unset both.",
    );
  });

  it("warns when NEXT_PUBLIC_ADMIN_TOKEN is set but ADMIN_TOKEN is not", () => {
    process.env.NEXT_PUBLIC_ADMIN_TOKEN = "local-dev-admin";
    checkAdminTokenPair();
    expect(errorSpy).toHaveBeenCalledTimes(1);
    expect(errorSpy).toHaveBeenCalledWith(
      "[next.config] NEXT_PUBLIC_ADMIN_TOKEN is set but ADMIN_TOKEN is not — " +
        "workspace-server will reject the bearer because no AdminAuth gate " +
        "is configured. Set both to the same value, or unset both.",
    );
  });

  // Empty string in process.env is the JS-side representation of `KEY=`
  // (no value) in a .env file. Treating "" as unset makes the pair
  // invariant symmetric: `KEY=` and `unset KEY` produce the same
  // verdict. Without this branch, an operator who comments out the
  // value but leaves the line would get a false-positive warning.
  it("treats empty string as unset (so KEY= and unset KEY are equivalent)", () => {
    process.env.ADMIN_TOKEN = "";
    process.env.NEXT_PUBLIC_ADMIN_TOKEN = "";
    checkAdminTokenPair();
    expect(errorSpy).not.toHaveBeenCalled();
  });

  it("warns when ADMIN_TOKEN is set and NEXT_PUBLIC_ADMIN_TOKEN is empty string", () => {
    process.env.ADMIN_TOKEN = "local-dev-admin";
    process.env.NEXT_PUBLIC_ADMIN_TOKEN = "";
    checkAdminTokenPair();
    expect(errorSpy).toHaveBeenCalledTimes(1);
    // First branch — server set, client unset.
    expect(errorSpy).toHaveBeenCalledWith(
      expect.stringContaining("ADMIN_TOKEN is set but NEXT_PUBLIC_ADMIN_TOKEN is not"),
    );
  });
});
