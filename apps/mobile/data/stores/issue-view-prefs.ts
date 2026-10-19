/**
 * Persistence for the issue-list view stores (`useMyIssuesViewStore`,
 * `useIssuesViewStore`): scope, status/priority filters and "show completed".
 *
 * Mobile counterpart of web's `createWorkspaceAwareStorage` +
 * `registerForWorkspaceRehydration` (packages/core/platform/workspace-storage.ts):
 * keys are namespaced at call time by the active scope, and registered stores
 * reset and rehydrate when that scope changes, so each workspace keeps its own
 * view and a switch never carries filters across.
 *
 * The scope is user + workspace id rather than web's workspace slug.
 * SecureStore cannot enumerate keys, so sign-out cannot sweep them the way
 * web's session cleanup does; keying by user keeps the next account on this
 * device from reading the previous one's view. The id survives a slug rename.
 *
 * Backed by expo-secure-store, the persistence the binary already ships for
 * theme, locale and the last workspace, so no native module is added. A
 * snapshot is a few hundred bytes, below the ~2 KB value size some iOS
 * releases reject.
 */
import { useEffect } from "react";
import * as SecureStore from "expo-secure-store";
import {
  createJSONStorage,
  type PersistOptions,
  type StateStorage,
} from "zustand/middleware";
import type { IssuePriority, IssueStatus } from "@multica/core/types";
import { PRIORITY_LABEL } from "@/lib/issue-status";

export interface IssueViewPrefs<S extends string> {
  scope: S;
  statusFilters: IssueStatus[];
  priorityFilters: IssuePriority[];
  /** Off by default: issues in the done/closed categories stay hidden unless
   *  their exact status is selected in `statusFilters`. */
  showCompleted: boolean;
}

let activeScope: string | null = null;

// SecureStore accepts only [A-Za-z0-9._-] in keys.
function storageKey(name: string, scope: string): string {
  return `${name}.${scope}`.replace(/[^A-Za-z0-9._-]/g, "_");
}

const scopedSecureStorage: StateStorage = {
  // A failed read counts as nothing saved, so hydration still completes and
  // the store keeps its defaults.
  getItem: (name) =>
    activeScope
      ? SecureStore.getItemAsync(storageKey(name, activeScope)).catch(() => null)
      : null,
  // Without an active scope there is no owner for the value; this is also
  // what drops the writes caused by the reset in `setIssueViewPrefsScope`.
  setItem: (name, value) => {
    if (!activeScope) return;
    return SecureStore.setItemAsync(storageKey(name, activeScope), value).catch(
      () => {
        // Keep the in-memory choice if secure storage is unavailable.
      },
    );
  },
  removeItem: (name) => {
    if (!activeScope) return;
    return SecureStore.deleteItemAsync(storageKey(name, activeScope)).catch(
      () => {},
    );
  },
};

interface PersistedViewStore<T> {
  getInitialState: () => T;
  setState: (state: T, replace: true) => void;
  persist: { rehydrate: () => unknown };
}

const registeredStores: { reset: () => void; rehydrate: () => unknown }[] = [];

/** Call once per store, next to its definition. */
export function registerIssueViewPrefsStore<T>(store: PersistedViewStore<T>) {
  registeredStores.push({
    reset: () => store.setState(store.getInitialState(), true),
    rehydrate: () => store.persist.rehydrate(),
  });
}

/**
 * Points every registered store at the given user's view of the given
 * workspace. Stores fall back to defaults first, so a workspace with nothing
 * saved never inherits the previous workspace's filters while its own
 * snapshot loads. `null` for either id detaches persistence.
 */
export function setIssueViewPrefsScope(
  userId: string | null,
  workspaceId: string | null,
) {
  const next = userId && workspaceId ? `${userId}.${workspaceId}` : null;
  if (next === activeScope) return;
  activeScope = null;
  for (const store of registeredStores) store.reset();
  activeScope = next;
  if (!next) return;
  for (const store of registeredStores) void store.rehydrate();
}

/** Mounted once by the workspace layout, which owns the active workspace. */
export function useIssueViewPrefsScope(
  userId: string | null,
  workspaceId: string | null,
) {
  useEffect(() => {
    setIssueViewPrefsScope(userId, workspaceId);
  }, [userId, workspaceId]);
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
    storage: createJSONStorage<IssueViewPrefs<S>>(() => scopedSecureStorage),
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
