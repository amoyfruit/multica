import type { Issue } from "@multica/core/types";

/**
 * Board sub-issue nesting is one level deep and scoped to a single column:
 * an issue nests under its parent only when that parent sits in the same
 * column as a root card. Everything else — a sub-issue whose parent lives in
 * another column, or a grandchild whose parent is itself nested — stays a
 * root card, so nesting never hides an issue from the board.
 */
export interface BoardColumnEntry {
  issue: Issue;
  depth: 0 | 1;
  /** Ids nested under this root card, in column order. Empty for children. */
  nestedChildIds: string[];
  collapsed: boolean;
}

function resolveNestingParent(
  id: string,
  inColumn: ReadonlySet<string>,
  issueMap: ReadonlyMap<string, Issue>,
): string | null {
  const parentId = issueMap.get(id)?.parent_issue_id;
  if (!parentId || parentId === id || !inColumn.has(parentId)) return null;
  // A parent that is itself nested cannot host children (one level only).
  const grandparentId = issueMap.get(parentId)?.parent_issue_id;
  if (grandparentId && grandparentId !== parentId && inColumn.has(grandparentId)) {
    return null;
  }
  return parentId;
}

/** The card this issue nests under in the given column, or null for a root. */
export function nestingParentId(
  id: string,
  ids: readonly string[],
  issueMap: ReadonlyMap<string, Issue>,
): string | null {
  return resolveNestingParent(id, new Set(ids), issueMap);
}

function groupByNestingParent(
  ids: readonly string[],
  issueMap: ReadonlyMap<string, Issue>,
) {
  const inColumn = new Set(ids);
  const roots: string[] = [];
  const children = new Map<string, string[]>();
  for (const id of ids) {
    const parentId = resolveNestingParent(id, inColumn, issueMap);
    if (parentId === null) {
      roots.push(id);
      continue;
    }
    const siblings = children.get(parentId);
    if (siblings) siblings.push(id);
    else children.set(parentId, [id]);
  }
  return { roots, children };
}

/**
 * Column ids in display order: roots keep their server order and each is
 * followed by its nested children (also in server order). Children of
 * collapsed parents are kept, so drag math always sees the whole column.
 */
export function orderColumnByHierarchy(
  ids: readonly string[],
  issueMap: ReadonlyMap<string, Issue>,
): string[] {
  const { roots, children } = groupByNestingParent(ids, issueMap);
  return roots.flatMap((id) => [id, ...(children.get(id) ?? [])]);
}

/** Visible column entries; children of collapsed parents are omitted. */
export function buildBoardColumnEntries(
  ids: readonly string[],
  issueMap: ReadonlyMap<string, Issue>,
  collapsedParentIds: ReadonlySet<string>,
): BoardColumnEntry[] {
  const { roots, children } = groupByNestingParent(ids, issueMap);
  const entries: BoardColumnEntry[] = [];
  for (const rootId of roots) {
    const issue = issueMap.get(rootId);
    if (!issue) continue;
    const nestedChildIds = children.get(rootId) ?? [];
    const collapsed = nestedChildIds.length > 0 && collapsedParentIds.has(rootId);
    entries.push({ issue, depth: 0, nestedChildIds, collapsed });
    if (collapsed) continue;
    for (const childId of nestedChildIds) {
      const child = issueMap.get(childId);
      if (child) {
        entries.push({ issue: child, depth: 1, nestedChildIds: [], collapsed: false });
      }
    }
  }
  return entries;
}

/** Ids on the same nesting level as `activeId` (roots, or one parent's children). */
export function siblingIds(
  ids: readonly string[],
  activeId: string,
  issueMap: ReadonlyMap<string, Issue>,
): string[] {
  const inColumn = new Set(ids);
  const level = resolveNestingParent(activeId, inColumn, issueMap);
  return ids.filter(
    (id) => resolveNestingParent(id, inColumn, issueMap) === level,
  );
}

/** Column ids that are currently visible (children of collapsed parents dropped). */
export function visibleColumnIds(
  ids: readonly string[],
  issueMap: ReadonlyMap<string, Issue>,
  collapsedParentIds: ReadonlySet<string>,
): string[] {
  if (collapsedParentIds.size === 0) return [...ids];
  const inColumn = new Set(ids);
  return ids.filter((id) => {
    const parentId = resolveNestingParent(id, inColumn, issueMap);
    return parentId === null || !collapsedParentIds.has(parentId);
  });
}
