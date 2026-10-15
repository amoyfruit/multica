import { QueryClient } from "@tanstack/react-query";
import { setWorkspaceSlugLookup } from "./paths/workspace-slugs";
import { workspaceKeys } from "./workspace/queries";
import type { Workspace } from "./types";

export function createQueryClient(): QueryClient {
  const client = new QueryClient({
    defaultOptions: {
      queries: {
        staleTime: Infinity,
        gcTime: 10 * 60 * 1000, // 10 minutes
        refetchOnWindowFocus: false,
        refetchOnReconnect: true,
        retry: 1,
      },
      mutations: {
        retry: false,
      },
    },
  });
  // Never share account state between server-rendered requests. Reading the
  // cache directly covers fetches, hydration, mutations and logout/clear.
  if (typeof window !== "undefined") {
    setWorkspaceSlugLookup((slug) =>
      client
        .getQueryData<Workspace[]>(workspaceKeys.list())
        ?.some((workspace) => workspace.slug === slug) ?? false,
    );
  }
  return client;
}
