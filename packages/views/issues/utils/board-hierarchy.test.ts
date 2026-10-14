// @vitest-environment node
import { describe, expect, it } from "vitest";
import type { Issue } from "@multica/core/types";
import {
  buildBoardColumnEntries,
  nestingParentId,
  orderColumnByHierarchy,
  siblingIds,
  visibleColumnIds,
} from "./board-hierarchy";

function mk(id: string, parent: string | null = null): Issue {
  return {
    id,
    workspace_id: "ws-1",
    number: 1,
    identifier: `MUL-${id}`,
    title: id,
    description: null,
    status: "todo",
    priority: "none",
    assignee_type: null,
    assignee_id: null,
    creator_type: "member",
    creator_id: "user-1",
    parent_issue_id: parent,
    project_id: null,
    position: 0,
    stage: null,
    start_date: null,
    due_date: null,
    metadata: {},
    properties: {},
    labels: [],
    created_at: "2025-01-01T00:00:00Z",
    updated_at: "2025-01-01T00:00:00Z",
  };
}

function mapOf(...issues: Issue[]) {
  return new Map(issues.map((issue) => [issue.id, issue]));
}

const NONE = new Set<string>();

describe("board hierarchy", () => {
  it("nests a child under its parent in the same column", () => {
    const map = mapOf(mk("p"), mk("x"), mk("c", "p"));
    expect(orderColumnByHierarchy(["p", "x", "c"], map)).toEqual(["p", "c", "x"]);
    const entries = buildBoardColumnEntries(["p", "x", "c"], map, NONE);
    expect(entries.map((e) => [e.issue.id, e.depth])).toEqual([
      ["p", 0],
      ["c", 1],
      ["x", 0],
    ]);
    expect(entries[0]!.nestedChildIds).toEqual(["c"]);
  });

  it("keeps a child whose parent is in another column as a root", () => {
    const map = mapOf(mk("p"), mk("c", "p"));
    expect(nestingParentId("c", ["c"], map)).toBeNull();
    expect(buildBoardColumnEntries(["c"], map, NONE)[0]!.depth).toBe(0);
  });

  it("keeps a grandchild as a root when its parent is nested", () => {
    const map = mapOf(mk("g"), mk("p", "g"), mk("c", "p"));
    const entries = buildBoardColumnEntries(["g", "p", "c"], map, NONE);
    expect(entries.map((e) => [e.issue.id, e.depth])).toEqual([
      ["g", 0],
      ["p", 1],
      ["c", 0],
    ]);
  });

  it("nests a grandchild under its parent when the grandparent is elsewhere", () => {
    const map = mapOf(mk("g"), mk("p", "g"), mk("c", "p"));
    expect(nestingParentId("c", ["p", "c"], map)).toBe("p");
  });

  it("pulls a child loaded before its parent beneath the parent", () => {
    const map = mapOf(mk("c", "p"), mk("x"), mk("p"));
    expect(orderColumnByHierarchy(["c", "x", "p"], map)).toEqual(["x", "p", "c"]);
  });

  it("keeps children of a collapsed parent in the order but hides them", () => {
    const map = mapOf(mk("p"), mk("c1", "p"), mk("c2", "p"), mk("x"));
    const ids = ["p", "c1", "x", "c2"];
    const collapsed = new Set(["p"]);
    expect(orderColumnByHierarchy(ids, map)).toEqual(["p", "c1", "c2", "x"]);
    const entries = buildBoardColumnEntries(ids, map, collapsed);
    expect(entries.map((e) => e.issue.id)).toEqual(["p", "x"]);
    expect(entries[0]).toMatchObject({ collapsed: true, nestedChildIds: ["c1", "c2"] });
    expect(visibleColumnIds(orderColumnByHierarchy(ids, map), map, collapsed)).toEqual(["p", "x"]);
  });

  it("does not mark a childless card collapsed", () => {
    const map = mapOf(mk("p"));
    expect(buildBoardColumnEntries(["p"], map, new Set(["p"]))[0]!.collapsed).toBe(false);
  });

  it("returns siblings on the dragged card's level", () => {
    const map = mapOf(mk("p"), mk("c1", "p"), mk("c2", "p"), mk("x"));
    const ordered = ["p", "c1", "c2", "x"];
    expect(siblingIds(ordered, "x", map)).toEqual(["p", "x"]);
    expect(siblingIds(ordered, "c2", map)).toEqual(["c1", "c2"]);
  });
});
