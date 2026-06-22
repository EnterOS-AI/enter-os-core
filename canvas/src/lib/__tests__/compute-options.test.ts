import { describe, it, expect } from "vitest";
import {
  parseComputeOptions,
  defaultInstanceForProvider,
  displayDefaultForProvider,
  instanceTypesForProvider,
  normalizeProvider,
} from "../compute-options";

describe("compute-options", () => {
  const serverResponse = {
    providers: ["aws", "hetzner", "gcp"],
    instanceTypes: {
      aws: ["t3.medium", "t3.large", "t3.xlarge"],
      hetzner: ["cpx31", "cpx41"],
      gcp: ["e2-standard-2", "e2-standard-4"],
    },
    defaults: {
      aws: "t3.medium",
      hetzner: "cpx31",
      gcp: "e2-standard-2",
    },
    display_defaults: {
      aws: "t3.xlarge",
      hetzner: "cpx41",
      gcp: "e2-standard-4",
    },
  };

  it("parses /compute/metadata into ComputeOptions", () => {
    const opts = parseComputeOptions(serverResponse);
    expect(opts).not.toBeNull();
    expect(opts?.providers).toEqual(["aws", "hetzner", "gcp"]);
    expect(opts?.defaults).toEqual({ aws: "t3.medium", hetzner: "cpx31", gcp: "e2-standard-2" });
    expect(opts?.displayDefaults).toEqual({
      aws: "t3.xlarge",
      hetzner: "cpx41",
      gcp: "e2-standard-4",
    });
    expect(opts?.instanceTypes.aws).toEqual(["t3.medium", "t3.large", "t3.xlarge"]);
  });

  it("falls back to in-bundle defaults when display_defaults is missing", () => {
    const partial = {
      providers: ["aws"],
      instanceTypes: { aws: ["t3.medium"] },
      defaults: { aws: "t3.medium" },
    };
    const opts = parseComputeOptions(partial);
    expect(opts).toBeNull();
  });

  it("returns null for malformed payloads", () => {
    expect(parseComputeOptions(null)).toBeNull();
    expect(parseComputeOptions({})).toBeNull();
    expect(parseComputeOptions({ providers: [] })).toBeNull();
    expect(parseComputeOptions({ providers: [""], instanceTypes: {}, defaults: {}, display_defaults: {} })).toBeNull();
  });

  it("helpers resolve per-provider values", () => {
    const opts = parseComputeOptions(serverResponse)!;
    expect(defaultInstanceForProvider(opts, "aws")).toBe("t3.medium");
    expect(displayDefaultForProvider(opts, "aws")).toBe("t3.xlarge");
    expect(instanceTypesForProvider(opts, "hetzner")).toEqual(["cpx31", "cpx41"]);
    expect(normalizeProvider(undefined)).toBe("aws");
  });
});
