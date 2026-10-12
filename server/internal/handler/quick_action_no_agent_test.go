package handler

import (
	"net/http"
	"testing"

	"github.com/multica-ai/multica/server/internal/testutil"
)

func TestQuickActionClearsExistingAgentTarget(t *testing.T) {
	const prompt = "Please check the latest changes when you pick this up."
	agentID := dbfx.Agent(t, "Quick action target", testRuntimeID)
	var action QuickActionResponse
	testutil.Call(t, testHandler.CreateQuickAction, newRequest(http.MethodPost, "/api/quick-actions", map[string]any{
		"name": "Check changes", "prompt": prompt, "visibility": "private",
		"assignee_type": "agent", "assignee_id": agentID,
	})).Want(http.StatusCreated).JSON(&action)
	dbfx.Cleanup(t, `DELETE FROM quick_action WHERE id = $1`, action.ID)
	if action.AssigneeType != "agent" || action.AssigneeID != agentID {
		t.Fatalf("expected an existing agent target, got %+v", action)
	}

	var updated QuickActionResponse
	testutil.Call(t, testHandler.UpdateQuickAction, testutil.WithURLParams(
		newRequest(http.MethodPatch, "/api/quick-actions/"+action.ID, map[string]any{
			"assignee_type": "", "assignee_id": "",
		}), "id", action.ID,
	)).Want(http.StatusOK).JSON(&updated)
	if updated.AssigneeType != "" || updated.AssigneeID != "" {
		t.Fatalf("clearing the target must empty both fields, got type %q and id %q", updated.AssigneeType, updated.AssigneeID)
	}

	issueID := dbfx.Issue(t, "Quick action cleared target", nil)
	var preview struct {
		Content string `json:"content"`
	}
	path := "/api/issues/" + issueID + "/quick-actions/" + action.ID + "/render"
	testutil.Call(t, testHandler.RenderQuickAction, testutil.WithURLParams(
		newRequest(http.MethodPost, path, nil), "id", issueID, "quickActionId", action.ID,
	)).Want(http.StatusOK).JSON(&preview)
	if preview.Content != prompt {
		t.Fatalf("preview after clearing target = %q, want the prompt without mention markup %q", preview.Content, prompt)
	}
}

func TestQuickActionWithoutAgentPostsPromptWithoutMentions(t *testing.T) {
	const prompt = "Please check the latest changes when you pick this up."
	var action QuickActionResponse
	testutil.Call(t, testHandler.CreateQuickAction, newRequest(http.MethodPost, "/api/quick-actions", map[string]any{
		"name": "Check changes", "prompt": prompt, "visibility": "public",
	})).Want(http.StatusCreated).JSON(&action)
	dbfx.Cleanup(t, `DELETE FROM quick_action WHERE id = $1`, action.ID)

	// The issue can have an assignee without the action explicitly mentioning it.
	agentID := dbfx.Agent(t, "Issue assignee", testRuntimeID)
	issueID := dbfx.Issue(t, "Quick action message", testutil.Cols{
		"assignee_type": "agent", "assignee_id": agentID,
	})
	dbfx.Cleanup(t, `DELETE FROM comment WHERE issue_id = $1`, issueID)
	dbfx.Cleanup(t, `DELETE FROM agent_task_queue WHERE issue_id = $1`, issueID)
	path := "/api/issues/" + issueID + "/quick-actions/" + action.ID
	request := func(suffix string) *http.Request {
		return testutil.WithURLParams(newRequest(http.MethodPost, path+suffix, nil),
			"id", issueID, "quickActionId", action.ID)
	}

	var preview struct {
		Content string `json:"content"`
	}
	testutil.Call(t, testHandler.RenderQuickAction, request("/render")).Want(http.StatusOK).JSON(&preview)
	if preview.Content != prompt {
		t.Fatalf("preview = %q, want the prompt without mention markup %q", preview.Content, prompt)
	}

	var comment CommentResponse
	testutil.Call(t, testHandler.RunQuickAction, request("/run")).Want(http.StatusCreated).JSON(&comment)
	if comment.ID == "" || comment.IssueID != issueID || comment.Content != prompt {
		t.Fatalf("expected an issue comment containing only the prompt, got %+v", comment)
	}
	if comment.QuickActionID == nil || *comment.QuickActionID != action.ID {
		t.Fatalf("comment must retain its quick action attribution, got %+v", comment.QuickActionID)
	}
	if len(comment.TriggerOutcomes) != 0 {
		t.Fatalf("a mention-free action must not explicitly trigger a target, got %+v", comment.TriggerOutcomes)
	}
}
