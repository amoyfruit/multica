// @vitest-environment node
import { beforeEach, describe, expect, it, vi } from "vitest";

const keychain = vi.hoisted(() => ({
  items: new Map<string, string>(),
  failReads: false,
}));

vi.mock("expo-secure-store", () => ({
  getItemAsync: vi.fn(async (key: string) => {
    if (keychain.failReads) throw new Error("keychain unavailable");
    return keychain.items.get(key) ?? null;
  }),
  setItemAsync: vi.fn(async (key: string, value: string) => {
    keychain.items.set(key, value);
  }),
  deleteItemAsync: vi.fn(async (key: string) => {
    keychain.items.delete(key);
  }),
}));

import { setViewPrefsScope } from "./view-prefs-storage";
import { useMyIssuesViewStore } from "./my-issues-view-store";
import { useIssuesViewStore } from "./issues-view-store";

const MY_KEY = "multica_my_issues_view.user-1.ws-a";

// Hydration resolves over a few chained promises.
const flush = () => new Promise((resolve) => setTimeout(resolve, 0));

async function enter(userId: string | null, wsId: string | null) {
  setViewPrefsScope(userId, wsId);
  await flush();
}

function saved(key: string) {
  const raw = keychain.items.get(key);
  return raw ? JSON.parse(raw).state : undefined;
}

describe("view prefs persistence", () => {
  beforeEach(async () => {
    await enter(null, null);
    keychain.items.clear();
    keychain.failReads = false;
  });

  it("starts with completed issues hidden and no filters", async () => {
    await enter("user-1", "ws-a");
    const state = useMyIssuesViewStore.getState();
    expect(state.showCompleted).toBe(false);
    expect(state.statusFilters).toEqual([]);
    expect(state.scope).toBe("assigned");
  });

  it("saves changes under the active user and workspace", async () => {
    await enter("user-1", "ws-a");
    useMyIssuesViewStore.getState().toggleStatusFilter("in_review");
    useMyIssuesViewStore.getState().setScope("created");
    useMyIssuesViewStore.getState().setShowCompleted(true);
    await flush();
    expect(saved(MY_KEY)).toEqual({
      scope: "created",
      statusFilters: ["in_review"],
      priorityFilters: [],
      showCompleted: true,
    });
  });

  it("restores the saved view after a restart", async () => {
    keychain.items.set(
      MY_KEY,
      JSON.stringify({
        state: { scope: "agents", statusFilters: ["todo"], priorityFilters: ["high"], showCompleted: true },
        version: 0,
      }),
    );
    await enter("user-1", "ws-a");
    const state = useMyIssuesViewStore.getState();
    expect(state.scope).toBe("agents");
    expect(state.statusFilters).toEqual(["todo"]);
    expect(state.priorityFilters).toEqual(["high"]);
    expect(state.showCompleted).toBe(true);
  });

  it("keeps each workspace's view and restores it on return", async () => {
    await enter("user-1", "ws-a");
    useIssuesViewStore.getState().toggleStatusFilter("blocked");
    await flush();

    await enter("user-1", "ws-b");
    expect(useIssuesViewStore.getState().statusFilters).toEqual([]);

    await enter("user-1", "ws-a");
    expect(useIssuesViewStore.getState().statusFilters).toEqual(["blocked"]);
  });

  it("does not overwrite the previous workspace while switching", async () => {
    await enter("user-1", "ws-a");
    useMyIssuesViewStore.getState().toggleStatusFilter("todo");
    await flush();

    await enter("user-1", "ws-b");
    expect(saved(MY_KEY)?.statusFilters).toEqual(["todo"]);
  });

  it("does not show one account's view to another", async () => {
    await enter("user-1", "ws-a");
    useMyIssuesViewStore.getState().toggleStatusFilter("todo");
    await flush();

    await enter("user-2", "ws-a");
    expect(useMyIssuesViewStore.getState().statusFilters).toEqual([]);
  });

  it("drops fields a different app version saved in another shape", async () => {
    keychain.items.set(
      MY_KEY,
      JSON.stringify({
        state: { scope: "everything", statusFilters: ["todo", 3], priorityFilters: ["huge", "low"], showCompleted: "yes" },
        version: 0,
      }),
    );
    await enter("user-1", "ws-a");
    const state = useMyIssuesViewStore.getState();
    expect(state.scope).toBe("assigned");
    expect(state.statusFilters).toEqual(["todo"]);
    expect(state.priorityFilters).toEqual(["low"]);
    expect(state.showCompleted).toBe(false);
  });

  it("falls back to defaults when the keychain cannot be read", async () => {
    keychain.failReads = true;
    await enter("user-1", "ws-a");
    expect(useMyIssuesViewStore.persist.hasHydrated()).toBe(true);
    expect(useMyIssuesViewStore.getState().showCompleted).toBe(false);
  });
});
