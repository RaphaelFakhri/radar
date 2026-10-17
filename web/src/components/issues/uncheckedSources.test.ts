import { describe, expect, it } from "vitest";
import { UNKNOWN_SOURCE_LINE, uncheckedSourceLines } from "./uncheckedSources";

describe("uncheckedSourceLines", () => {
  it("names one list-only kind", () => {
    expect(uncheckedSourceLines([{ source: "missing_ref", reason: "list_only", resource: "secrets" }])).toEqual([
      "Missing references to Secrets aren't reported: Radar can list Secrets but not watch them, so it can't confirm one is missing. Grant Radar watch access to Secrets to restore the check.",
    ]);
  });

  it("merges several kinds into one line after Helm", () => {
    expect(
      uncheckedSourceLines([
        { source: "missing_ref", reason: "stale", resource: "secrets" },
        { source: "helm", reason: "read_failed" },
        { source: "missing_ref", reason: "list_only", resource: "configmaps" },
        { source: "missing_ref", reason: "list_only", resource: "serviceaccounts" },
      ]),
    ).toEqual([
      "Helm releases couldn't be checked, so failed or stuck releases may be missing.",
      "Missing references to Secrets, ConfigMaps and ServiceAccounts aren't reported: Radar can list these kinds but not watch them, so it can't confirm one is missing. Grant Radar watch access to these kinds to restore the check. Radar's copy has not refreshed for several minutes.",
    ]);
  });

  it("treats a missing_ref entry without a resource as unknown", () => {
    expect(uncheckedSourceLines([{ source: "missing_ref", reason: "list_only" }])).toEqual([UNKNOWN_SOURCE_LINE]);
  });
});
