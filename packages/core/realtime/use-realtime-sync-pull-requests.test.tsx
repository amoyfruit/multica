/** @vitest-environment jsdom */
import { QueryClientProvider } from "@tanstack/react-query";
import { act, cleanup, renderHook, waitFor } from "@testing-library/react";
import type { ReactNode } from "react";
import { afterEach, expect, it, vi } from "vitest";
import type { WSClient } from "../api/ws-client";
import { githubKeys } from "../github/queries";
import { createQueryClient } from "../query-client";
import { useRealtimeSync, type RealtimeSyncStores } from "./use-realtime-sync";

vi.mock("../platform/workspace-storage", () => ({
  getCurrentWsId: () => "ws-1",
  getCurrentSlug: () => "test-ws",
  createWorkspaceAwareStorage: (adapter: unknown) => adapter,
  registerForWorkspaceRehydration: () => {},
}));
vi.mock("../paths", () => ({
  useHasOnboarded: () => true,
  resolvePostAuthDestination: () => "/",
}));

afterEach(() => {
  cleanup();
  vi.restoreAllMocks();
});

// The board's merge-queue read is keyed by workspace, so the per-issue
// ["github", "pull-requests"] prefix does not reach it. A snapshot that enters
// or leaves a queue arrives as pull_request:updated; without this the board
// would wait for its safety-net poll to notice.
it.each(["pull_request:updated", "pull_request:linked", "pull_request:unlinked"])(
  "invalidates the board's queued pull requests on %s",
  async (event) => {
    const qc = createQueryClient();
    qc.setQueryData(githubKeys.queuedPullRequests("ws-1"), { queued_pull_requests: [] });
    qc.setQueryData(githubKeys.pullRequests("issue-1"), { pull_requests: [] });
    qc.setQueryData(githubKeys.queuedPullRequests("ws-2"), { queued_pull_requests: [] });

    let onAny: ((msg: { type: string; payload: unknown }) => void) | undefined;
    const ws = {
      on: () => () => {},
      onAny: (handler: (msg: { type: string; payload: unknown }) => void) => {
        onAny = handler;
        return () => {
          onAny = undefined;
        };
      },
      onReconnect: () => () => {},
    } as unknown as WSClient;
    const stores = {
      authStore: { getState: () => ({ user: { id: "u1" } }) },
    } as unknown as RealtimeSyncStores;
    function Wrapper({ children }: { children: ReactNode }) {
      return <QueryClientProvider client={qc}>{children}</QueryClientProvider>;
    }
    renderHook(() => useRealtimeSync(ws, stores), { wrapper: Wrapper });

    await act(async () => {
      onAny!({ type: event, payload: {} });
    });
    await waitFor(() =>
      expect(qc.getQueryState(githubKeys.queuedPullRequests("ws-1"))?.isInvalidated).toBe(true),
    );
    expect(qc.getQueryState(githubKeys.pullRequests("issue-1"))?.isInvalidated).toBe(true);
    // Only the current workspace's board.
    expect(qc.getQueryState(githubKeys.queuedPullRequests("ws-2"))?.isInvalidated).toBe(false);
  },
);
