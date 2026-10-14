import { describe, expect, it } from "vitest";
import {
  gitopsInsightsRefetchInterval,
  gitopsTreeRefetchInterval,
} from "./client";

type TreeQuery = Parameters<typeof gitopsTreeRefetchInterval>[0];
type InsightQuery = Parameters<typeof gitopsInsightsRefetchInterval>[0];
const tree = (status: string, data: TreeQuery["state"]["data"], failingForMs = 0): TreeQuery => ({
  state: { status, data, dataUpdatedAt: 1_000, errorUpdatedAt: status === "error" ? 1_000 + failingForMs : 0 },
});
const insight = (status: string, data: InsightQuery["state"]["data"], failingForMs = 0): InsightQuery => ({
  state: { status, data, dataUpdatedAt: 1_000, errorUpdatedAt: status === "error" ? 1_000 + failingForMs : 0 },
});
const SIX_MINUTES = 6 * 60_000;

describe("gitopsTreeRefetchInterval", () => {
  it("asks every 2s while Radar connects to argocd-server", () => {
    expect(gitopsTreeRefetchInterval(tree("success", { healthApiPending: true }))).toBe(2000);
  });

  it("asks every 30s while Radar retries, no faster than the server re-checks", () => {
    expect(gitopsTreeRefetchInterval(tree("success", { healthApiRetrying: true }))).toBe(30_000);
  });

  it("stops once health is in", () => {
    expect(gitopsTreeRefetchInterval(tree("success", {}))).toBe(false);
  });

  it("keeps asking slowly after a failed fetch, so a brief outage doesn't strand the page", () => {
    expect(gitopsTreeRefetchInterval(tree("error", { healthApiRetrying: true }))).toBe(30_000);
    expect(gitopsTreeRefetchInterval(tree("error", { healthApiPending: true }))).toBe(30_000);
  });

  it("gives up once fetches have failed for more than five minutes", () => {
    expect(gitopsTreeRefetchInterval(tree("error", { healthApiRetrying: true }, SIX_MINUTES))).toBe(false);
  });

  it("does not start polling after a failed fetch of a settled page", () => {
    expect(gitopsTreeRefetchInterval(tree("error", {}))).toBe(false);
  });
});

describe("gitopsInsightsRefetchInterval", () => {
  it("asks every 2s while a sync runs", () => {
    expect(
      gitopsInsightsRefetchInterval(
        insight("success", { summary: { operationPhase: "Running" } }),
      ),
    ).toBe(2000);
  });

  it("asks every 2s while Radar connects", () => {
    expect(
      gitopsInsightsRefetchInterval(
        insight("success", { capabilities: { argoConnecting: true } }),
      ),
    ).toBe(2000);
  });

  it("asks every 30s while Radar retries", () => {
    expect(
      gitopsInsightsRefetchInterval(
        insight("success", { capabilities: { argoRetrying: true } }),
      ),
    ).toBe(30_000);
    expect(
      gitopsInsightsRefetchInterval(
        insight("success", { summary: { resourceHealthApiRetrying: true } }),
      ),
    ).toBe(30_000);
  });

  it("keeps asking slowly after a failed fetch, including during a running sync", () => {
    expect(
      gitopsInsightsRefetchInterval(insight("error", { capabilities: { argoRetrying: true } })),
    ).toBe(30_000);
    expect(
      gitopsInsightsRefetchInterval(insight("error", { summary: { operationPhase: "Running" } })),
    ).toBe(30_000);
  });

  it("gives up once fetches have failed for more than five minutes", () => {
    expect(
      gitopsInsightsRefetchInterval(insight("error", { capabilities: { argoRetrying: true } }, SIX_MINUTES)),
    ).toBe(false);
  });

  it("does not poll a settled page", () => {
    expect(gitopsInsightsRefetchInterval(insight("success", {}))).toBe(false);
  });
});
