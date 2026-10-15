// @vitest-environment jsdom
import { dehydrate, hydrate, QueryClient } from "@tanstack/react-query";
import { afterEach, expect, it } from "vitest";
import { createQueryClient } from "../query-client";
import { workspaceSlugForPath, setWorkspaceSlugLookup } from "../paths/workspace-slugs";
import { workspaceKeys } from "./queries";

afterEach(() => setWorkspaceSlugLookup(() => false));

it("reads hydration and cache changes, and forgets membership on account cleanup", () => {
  const source = new QueryClient();
  source.setQueryData(workspaceKeys.list(), [{ slug: "dingtalk" }]);
  const client = createQueryClient();
  expect(workspaceSlugForPath("/dingtalk/issues")).toBeNull();
  hydrate(client, dehydrate(source));
  expect(workspaceSlugForPath("/dingtalk/issues")).toBe("dingtalk");
  client.setQueryData(workspaceKeys.list(), [{ slug: "issues" }]);
  expect(workspaceSlugForPath("/dingtalk/issues")).toBeNull();
  expect(workspaceSlugForPath("/issues/issues")).toBe("issues");
  client.clear();
  expect(workspaceSlugForPath("/issues/issues")).toBeNull();
  source.clear();
});
