package service

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// seedNoDeliverableTask inserts a RUNNING issue task started an hour ago and
// returns its id, so HasAgentCommentedSince's since-window covers any comment
// the test posts.
func seedNoDeliverableTask(t *testing.T, pool *pgxpool.Pool, agentID, issueID string) string {
	t.Helper()
	var taskID string
	if err := pool.QueryRow(context.Background(), `
		INSERT INTO agent_task_queue (agent_id, runtime_id, issue_id, status, priority, started_at)
		VALUES ($1, (SELECT runtime_id FROM agent WHERE id = $1), $2, 'running', 0, now() - interval '1 hour')
		RETURNING id`, agentID, issueID).Scan(&taskID); err != nil {
		t.Fatalf("seed running task: %v", err)
	}
	t.Cleanup(func() { pool.Exec(context.Background(), `DELETE FROM agent_task_queue WHERE id = $1`, taskID) })
	return taskID
}

// TestNoDeliverableCompletion is the MULT-22 guard test: a clean completion
// with no output and no agent comment on its issue must be flagged for the
// failure reroute; every deliberate or already-delivered shape must not.
func TestNoDeliverableCompletion(t *testing.T) {
	pool := newResolveOriginatorPool(t)
	ctx := context.Background()
	svc := NewTaskService(db.New(pool), nil, nil, nil)
	workspaceID, _, agentID, issueID := seedAttributionFixture(t, pool)
	taskID := seedNoDeliverableTask(t, pool, agentID, issueID)
	tid := util.MustParseUUID(taskID)

	if svc.NoDeliverableCompletion(ctx, tid, "final answer posted") {
		t.Fatal("non-empty output must not be rerouted")
	}
	if !svc.NoDeliverableCompletion(ctx, tid, "  \n	 \n") {
		t.Fatal("whitespace-only output with no comment must be rerouted")
	}
	if !svc.NoDeliverableCompletion(ctx, tid, "") {
		t.Fatal("empty output with no comment must be rerouted")
	}

	// An agent comment posted during the run IS the deliverable.
	if _, err := pool.Exec(ctx, `
		INSERT INTO comment (issue_id, workspace_id, author_type, author_id, content)
		VALUES ($1, $2, 'agent', $3, 'progress update')`, issueID, workspaceID, agentID); err != nil {
		t.Fatalf("seed agent comment: %v", err)
	}
	if svc.NoDeliverableCompletion(ctx, tid, "") {
		t.Fatal("a posted agent comment must not be rerouted")
	}

	// A task that is no longer running takes the normal idempotent path.
	if _, err := pool.Exec(ctx, `UPDATE agent_task_queue SET status = 'completed' WHERE id = $1`, taskID); err != nil {
		t.Fatalf("complete task: %v", err)
	}
	if svc.NoDeliverableCompletion(ctx, tid, "") {
		t.Fatal("a finalized task must not be rerouted")
	}

	// A task with no issue (chat / quick-create) has no issue deliverable
	// contract; chat already owns a visible no_response outcome.
	var chatTaskID string
	if err := pool.QueryRow(ctx, `
		INSERT INTO agent_task_queue (agent_id, runtime_id, status, priority)
		VALUES ($1, (SELECT runtime_id FROM agent WHERE id = $1), 'running', 0)
		RETURNING id`, agentID).Scan(&chatTaskID); err != nil {
		t.Fatalf("seed issue-less task: %v", err)
	}
	t.Cleanup(func() { pool.Exec(context.Background(), `DELETE FROM agent_task_queue WHERE id = $1`, chatTaskID) })
	if svc.NoDeliverableCompletion(ctx, util.MustParseUUID(chatTaskID), "") {
		t.Fatal("a task with no issue must not be rerouted")
	}
}
