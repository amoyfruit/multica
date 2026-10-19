import { beforeEach, describe, expect, it, vi } from "vitest";
import { render, screen } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { I18nProvider } from "@multica/core/i18n/react";
import type { ListGitHubQueuedPullRequestsResponse } from "@multica/core/types";
import enCommon from "../../locales/en/common.json";
import enIssues from "../../locales/en/issues.json";

const TEST_RESOURCES = { en: { common: enCommon, issues: enIssues } };

vi.mock("@multica/core/hooks", () => ({
  useWorkspaceId: () => "ws-1",
}));

const queryFn = vi.fn(async () => mockResponse);

vi.mock("@multica/core/github", () => ({
  useGitHubSettings: () => githubSettings,
  queuedPullRequestsOptions: (wsId: string) => ({
    queryKey: ["github", wsId, "pull-requests", "queued"],
    queryFn,
    enabled: !!wsId,
  }),
}));

import { IssueMergeQueueIndicator, IssueMergeQueueProvider } from "./issue-merge-queue-indicator";

let mockResponse: ListGitHubQueuedPullRequestsResponse = { queued_pull_requests: [] };
let githubSettings = { enabled: true, prSidebar: true, coAuthor: true, autoLinkPRs: true };

beforeEach(() => {
  githubSettings = { enabled: true, prSidebar: true, coAuthor: true, autoLinkPRs: true };
  queryFn.mockClear();
});

function renderIndicators(issueIds: string[] = ["issue-1"], { provider = true } = {}) {
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  const indicators = issueIds.map((id) => <IssueMergeQueueIndicator key={id} issueId={id} />);
  return render(
    <QueryClientProvider client={qc}>
      <I18nProvider resources={TEST_RESOURCES} locale="en">
        {provider ? <IssueMergeQueueProvider>{indicators}</IssueMergeQueueProvider> : indicators}
      </I18nProvider>
    </QueryClientProvider>,
  );
}

describe("IssueMergeQueueIndicator", () => {
  it("marks a card whose PR is waiting in the queue, in GitHub's word", async () => {
    mockResponse = {
      queued_pull_requests: [{ issue_id: "issue-1", merge_queue_state: "queued" }],
    };
    renderIndicators();
    expect(await screen.findByText("Queued")).toBeInTheDocument();
  });

  it("distinguishes an entry the queue cannot merge", async () => {
    mockResponse = {
      queued_pull_requests: [{ issue_id: "issue-1", merge_queue_state: "unmergeable" }],
    };
    renderIndicators();
    expect(await screen.findByText("Queue blocked")).toBeInTheDocument();
    expect(screen.queryByText("Queued")).not.toBeInTheDocument();
  });

  it("treats every other entry state as waiting, not blocked", async () => {
    mockResponse = {
      queued_pull_requests: [{ issue_id: "issue-1", merge_queue_state: "awaiting_checks" }],
    };
    renderIndicators();
    expect(await screen.findByText("Queued")).toBeInTheDocument();
  });

  it("gives every card its own entry from one shared read", async () => {
    // A board mounts one indicator per card. The provider is the only
    // observer of the query, so a whole board costs one request — and one
    // polling timer — rather than one per card.
    mockResponse = {
      queued_pull_requests: [
        { issue_id: "issue-1", merge_queue_state: "queued" },
        { issue_id: "issue-3", merge_queue_state: "unmergeable" },
      ],
    };
    renderIndicators(["issue-1", "issue-2", "issue-3"]);
    expect(await screen.findByText("Queued")).toBeInTheDocument();
    expect(screen.getByText("Queue blocked")).toBeInTheDocument();
    expect(screen.getAllByText(/Queue/)).toHaveLength(2);
    expect(queryFn).toHaveBeenCalledTimes(1);
  });

  it("renders nothing for an issue that is not in the response", async () => {
    // The response is the complete set of queued issues, so absence is a
    // definite "not queued" — the card must stay clean, not show a placeholder.
    mockResponse = {
      queued_pull_requests: [{ issue_id: "other-issue", merge_queue_state: "queued" }],
    };
    const { container } = renderIndicators(["issue-1"]);
    await vi.waitFor(() => expect(queryFn).toHaveBeenCalled());
    expect(container).toBeEmptyDOMElement();
  });

  it("does not poll when the workspace has GitHub switched off", async () => {
    githubSettings = { enabled: false, prSidebar: false, coAuthor: false, autoLinkPRs: false };
    mockResponse = {
      queued_pull_requests: [{ issue_id: "issue-1", merge_queue_state: "queued" }],
    };
    const { container } = renderIndicators();
    await Promise.resolve();
    expect(container).toBeEmptyDOMElement();
    expect(queryFn).not.toHaveBeenCalled();
  });

  it("renders nothing, and fetches nothing, outside a provider", async () => {
    mockResponse = {
      queued_pull_requests: [{ issue_id: "issue-1", merge_queue_state: "queued" }],
    };
    const { container } = renderIndicators(["issue-1"], { provider: false });
    await Promise.resolve();
    expect(container).toBeEmptyDOMElement();
    expect(queryFn).not.toHaveBeenCalled();
  });
});
