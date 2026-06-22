// Org switching across tenant subdomains.
//
// Each org is its own tenant at <slug>.<apex> (e.g. agents-team.moleculesai.app),
// so switching orgs from the canvas topbar means navigating to the target org's
// subdomain. switchOrgUrl derives that URL from the current location, or returns
// null when it's a no-op (same org / empty target) or the apex can't be resolved.

export function switchOrgUrl(
  host: string,
  protocol: string,
  currentSlug: string,
  targetSlug: string,
): string | null {
  if (!targetSlug || targetSlug === currentSlug) return null;
  // Prefer stripping the known current-org label; otherwise drop the first
  // label as a best-effort apex (covers hosts we didn't seed a slug for).
  // Guard: the derived apex must contain at least one dot so a 2-label host
  // with an empty currentSlug does not yield a foreign apex (e.g.
  // moleculesai.app → <slug>.app). (core#2509)
  const apex =
    currentSlug && host.startsWith(`${currentSlug}.`)
      ? host.slice(currentSlug.length + 1)
      : host.split(".").slice(1).join(".");
  if (!apex || !apex.includes(".")) return null;
  return `${protocol}//${targetSlug}.${apex}`;
}
