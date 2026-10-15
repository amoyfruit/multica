import { isReservedSlug } from "./reserved-slugs";
import { WORKSPACE_PAGES } from "./route-icons";

let lookup: (slug: string) => boolean = () => false;

/** Bind the browser's account query cache without copying server state. */
export function setWorkspaceSlugLookup(next: (slug: string) => boolean): void {
  lookup = next;
}

const workspaceRoutes = new Set([
  ...Object.values(WORKSPACE_PAGES).map((page) => page.segment),
  "members",
  "attachments",
]);

/**
 * Reservation governs creation, not existing workspace membership. For a
 * reserved slug, require a workspace route too so /dingtalk/bind and other
 * global endpoints remain global. Saved groups may supply their own slug
 * until the shell validates them against the loaded membership list.
 */
export function workspaceSlugForPath(path: string, savedGroupSlug?: string): string | null {
  const pathname = path.split(/[?#]/, 1)[0] ?? "";
  const [rawSlug, route] = pathname.split("/").filter(Boolean);
  if (!rawSlug) return null;
  let slug: string;
  try {
    slug = decodeURIComponent(rawSlug);
  } catch {
    return null;
  }
  if (isReservedSlug(slug.toLowerCase())) {
    if (slug !== savedGroupSlug && !lookup(slug)) return null;
    if (!route || !workspaceRoutes.has(route)) return null;
  }
  return slug;
}
