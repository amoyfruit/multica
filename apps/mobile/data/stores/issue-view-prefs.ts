/**
 * Shared shape and `persist` options for the issue-list view stores
 * (`useMyIssuesViewStore`, `useIssuesViewStore`): scope, status/priority
 * filters and "show completed". Storage and the per-workspace hydration
 * lifecycle live in `./view-prefs-storage.ts`.
 */
import { createJSONStorage, type PersistOptions } from "zustand/middleware";
import type { IssuePriority, IssueStatus } from "@multica/core/types";
import { PRIORITY_LABEL } from "@/lib/issue-status";
import { viewPrefsStorage } from "./view-prefs-storage";

export interface IssueViewPrefs<S extends string> {
  scope: S;
  statusFilters: IssueStatus[];
  priorityFilters: IssuePriority[];
  /** Off by default: issues in the done/closed categories stay hidden unless
   *  their exact status is selected in `statusFilters`. */
  showCompleted: boolean;
}

const PRIORITIES = Object.keys(PRIORITY_LABEL);

function isPriority(value: unknown): value is IssuePriority {
  return typeof value === "string" && PRIORITIES.includes(value);
}

/**
 * Takes each persisted field only when it still has the expected shape, so a
 * snapshot written by another app version cannot put the store into a state
 * the UI does not handle. Status keys stay open: custom statuses are valid.
 */
export function mergeIssueViewPrefs<S extends string, T extends IssueViewPrefs<S>>(
  persisted: unknown,
  current: T,
  scopes: readonly S[],
): T {
  if (!persisted || typeof persisted !== "object") return current;
  const p = persisted as Record<string, unknown>;
  return {
    ...current,
    scope: scopes.find((scope) => scope === p.scope) ?? current.scope,
    statusFilters: Array.isArray(p.statusFilters)
      ? p.statusFilters.filter((s): s is IssueStatus => typeof s === "string")
      : current.statusFilters,
    priorityFilters: Array.isArray(p.priorityFilters)
      ? p.priorityFilters.filter(isPriority)
      : current.priorityFilters,
    showCompleted:
      typeof p.showCompleted === "boolean"
        ? p.showCompleted
        : current.showCompleted,
  };
}

/** Shared `persist` options; mirrors web's `viewStorePersistOptions`. */
export function issueViewPersistOptions<
  S extends string,
  T extends IssueViewPrefs<S>,
>(name: string, scopes: readonly S[]): PersistOptions<T, IssueViewPrefs<S>> {
  return {
    name,
    storage: createJSONStorage<IssueViewPrefs<S>>(() => viewPrefsStorage),
    // The workspace layout hydrates once it knows the user and workspace.
    skipHydration: true,
    partialize: (state) => ({
      scope: state.scope,
      statusFilters: state.statusFilters,
      priorityFilters: state.priorityFilters,
      showCompleted: state.showCompleted,
    }),
    merge: (persisted, current) =>
      mergeIssueViewPrefs(persisted, current, scopes),
  };
}
