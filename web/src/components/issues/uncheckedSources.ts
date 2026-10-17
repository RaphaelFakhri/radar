import type { IssuesResponse } from "../../api/client";

// The issue sources /api/issues can list in unchecked_sources, and the line
// each one gets wherever Radar's issues would otherwise read as complete.
const SOURCE_LINES: Record<string, string> = {
  helm: "Helm releases couldn't be checked, so failed or stuck releases may be missing.",
};

// Display names for the kinds a missing_ref entry can name.
const RESOURCE_NAMES: Record<string, string> = {
  secrets: "Secrets",
  configmaps: "ConfigMaps",
  persistentvolumeclaims: "PersistentVolumeClaims",
  serviceaccounts: "ServiceAccounts",
  services: "Services",
  deployments: "Deployments",
  statefulsets: "StatefulSets",
  daemonsets: "DaemonSets",
  ingressclasses: "IngressClasses",
  storageclasses: "StorageClasses",
  roles: "Roles",
  clusterroles: "ClusterRoles",
};

// For a source this version doesn't know, sent by a newer Radar.
export const UNKNOWN_SOURCE_LINE = "Some issue checks couldn't run, so issues may be missing.";

function missingRefLine(resources: string[]): string {
  const names = resources.map((r) => RESOURCE_NAMES[r] ?? r);
  const list = names.length <= 2 ? names.join(" and ") : `${names.slice(0, -1).join(", ")} and ${names[names.length - 1]}`;
  const kinds = names.length === 1 ? names[0] : "these kinds";
  return `Missing references to ${list} aren't reported: Radar can list ${kinds} but not watch them, so it can't confirm one is missing.`;
}

/** One line per unchecked source, known sources first, without repeats. */
export function uncheckedSourceLines(sources: IssuesResponse["unchecked_sources"]): string[] {
  const known = new Set<string>();
  const missingRefResources: string[] = [];
  let unknown = false;
  for (const { source, resource } of sources ?? []) {
    if (source === "missing_ref" && resource) {
      if (!missingRefResources.includes(resource)) missingRefResources.push(resource);
    } else if (Object.hasOwn(SOURCE_LINES, source)) known.add(SOURCE_LINES[source]);
    else unknown = true;
  }
  const lines = [...known];
  if (missingRefResources.length > 0) lines.push(missingRefLine(missingRefResources));
  return unknown ? [...lines, UNKNOWN_SOURCE_LINE] : lines;
}
