"use client";

import { createContext, memo, useContext, useMemo, type ReactNode } from "react";
import { useQuery } from "@tanstack/react-query";
import { ListOrdered, TriangleAlert } from "lucide-react";
import { useWorkspaceId } from "@multica/core/hooks";
import { queuedPullRequestsOptions, useGitHubSettings } from "@multica/core/github";
import type { GitHubPullRequestMergeQueueState } from "@multica/core/types";
import { cn } from "@multica/ui/lib/utils";
import { useT } from "../../i18n";

type QueueStateByIssue = ReadonlyMap<string, GitHubPullRequestMergeQueueState>;

const QueuedPullRequestsContext = createContext<QueueStateByIssue | null>(null);

/**
 * Owns the one workspace-wide queued-PR read for a board and hands each card
 * its own entry. Mount it once around the cards.
 *
 * The read lives here rather than in each indicator because a React Query
 * refetch interval belongs to the observer, not the query: hundreds of card
 * observers sharing one key would still run hundreds of polling timers. With
 * one owner there is one timer, and realtime pull request events invalidate
 * the same query (see use-realtime-sync).
 *
 * The poll does not run at all in a workspace with GitHub switched off.
 */
export function IssueMergeQueueProvider({ children }: { children: ReactNode }) {
  const wsId = useWorkspaceId();
  const github = useGitHubSettings();
  const options = queuedPullRequestsOptions(wsId);
  const { data } = useQuery({ ...options, enabled: options.enabled && github.enabled });
  const byIssue = useMemo<QueueStateByIssue>(
    () =>
      new Map(
        (data?.queued_pull_requests ?? []).map((entry) => [entry.issue_id, entry.merge_queue_state]),
      ),
    [data],
  );
  return (
    <QueuedPullRequestsContext.Provider value={byIssue}>{children}</QueuedPullRequestsContext.Provider>
  );
}

interface IssueMergeQueueIndicatorProps {
  issueId: string;
}

/**
 * "This issue's PR is waiting in a merge queue" chip for board cards, worded
 * the way GitHub states the PR: "Queued".
 *
 * A queued PR is the last state before an issue's work actually lands, but it
 * is invisible on a board: the card payload carries no pull request data, and
 * GitHub reports a queued PR as an ordinary open one. So an issue sitting in
 * the In Review column looks identical whether nobody has touched its PR or it
 * is next in line to merge.
 *
 * Reads its entry from the nearest IssueMergeQueueProvider. Renders nothing
 * when the issue has no queued PR, or outside a provider — no chrome, no
 * placeholder.
 */
export const IssueMergeQueueIndicator = memo(function IssueMergeQueueIndicator({
  issueId,
}: IssueMergeQueueIndicatorProps) {
  const { t } = useT("issues");
  const queueState = useContext(QueuedPullRequestsContext)?.get(issueId) ?? null;

  if (!queueState) return null;

  // `unmergeable` is the one queue state that is not progress: GitHub is about
  // to evict the entry, so it must not read like the others.
  const blocked = queueState === "unmergeable";
  const Icon = blocked ? TriangleAlert : ListOrdered;

  return (
    <span
      className={cn(
        "inline-flex shrink-0 items-center gap-1 rounded-full px-1.5 py-0.5 text-micro",
        blocked
          ? "bg-rose-500/10 text-rose-600 dark:text-rose-400"
          : "bg-amber-500/10 text-amber-600 dark:text-amber-400",
      )}
    >
      <Icon className="h-3 w-3" />
      {blocked ? t(($) => $.card.merge_queue_blocked) : t(($) => $.card.merge_queue)}
    </span>
  );
});
