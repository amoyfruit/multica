package handler

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/multica-ai/multica/server/internal/auth"
	"github.com/multica-ai/multica/server/internal/middleware"
	"github.com/multica-ai/multica/server/internal/testutil"
)

func TestCurrentTaskIdentityAuthBoundary(t *testing.T) {
	agentID := dbfx.Agent(t, "Identity test", "")
	for i, tc := range []struct {
		name, status, source, kind             string
		expired, crossWorkspace, missingMember bool
	}{
		{"human", "running", "direct_human", "human", false, false, false},
		{"automation", "dispatched", "trigger_owner", "automation", false, false, false},
		{"terminal token not yet revoked", "completed", "direct_human", "", false, false, false},
		{"queued task cannot act", "queued", "direct_human", "", false, false, false},
		{"expired token", "running", "direct_human", "", true, false, false},
		{"fallback is not consent", "running", "owner_fallback", "", false, false, false},
		{"task in another workspace", "running", "direct_human", "", false, true, false},
		{"requester left workspace", "running", "direct_human", "", false, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			taskAgent := agentID
			if tc.crossWorkspace {
				ws := dbfx.Workspace(t, "Other identity workspace", fmt.Sprintf("identity-other-%d", i))
				taskAgent = dbfx.Agent(t, "Other identity agent", "", testutil.Cols{"workspace_id": ws})
			}
			requester := testUserID
			if tc.missingMember {
				requester = dbfx.User(t, "Former member", "identity-former@example.com")
			}
			cols := testutil.Cols{"status": tc.status, "originator_source": tc.source, "accountable_user_id": requester}
			if tc.source != "owner_fallback" {
				cols["originator_user_id"] = requester
			}
			taskID := dbfx.Task(t, taskAgent, cols)
			token := fmt.Sprintf("mat_identity_test_%d", i)
			expiry := testutil.Raw("now() + interval '1 hour'")
			if tc.expired {
				expiry = testutil.Raw("now() - interval '1 hour'")
			}
			dbfx.Insert(t, "task_token", testutil.Cols{"token_hash": auth.HashToken(token), "task_id": taskID, "agent_id": agentID, "workspace_id": testWorkspaceID, "user_id": testUserID, "expires_at": expiry})
			h := middleware.Auth(testHandler.Queries, nil, nil, nil)(middleware.RequireWorkspaceMember(testHandler.Queries)(http.HandlerFunc(testHandler.GetCurrentTaskIdentity)))
			req := httptest.NewRequest(http.MethodGet, "/api/tasks/current/identity?workspace_id=forged&task_id=forged", nil)
			req.Header.Set("Authorization", "Bearer "+token)
			for _, name := range []string{"X-Task-ID", "X-User-ID", "X-Agent-ID", "X-Workspace-ID", "X-Actor-Source"} {
				req.Header.Set(name, "forged")
			}
			want := http.StatusOK
			if tc.kind == "" {
				want = http.StatusForbidden
			}
			if tc.expired {
				want = http.StatusUnauthorized
			}
			response := testutil.Call(t, h.ServeHTTP, req).Want(want)
			if tc.kind != "" {
				var out map[string]string
				response.JSON(&out)
				if out["task_id"] != taskID || out["workspace_id"] != testWorkspaceID || out["kind"] != tc.kind {
					t.Fatalf("wrong task binding: %v", out)
				}
				if tc.kind == "human" && out["user_id"] != testUserID {
					t.Fatalf("wrong human: %v", out)
				}
				if tc.kind == "automation" && out["user_id"] != "" {
					t.Fatal("automation selected trigger owner")
				}
			}
		})
	}
	// Even a handler invocation with readable task IDs is insufficient without
	// the server-stamped task-token auth path.
	req := httptest.NewRequest(http.MethodGet, "/api/tasks/current/identity", nil)
	req.Header.Set("X-Agent-ID", agentID)
	testutil.Call(t, testHandler.GetCurrentTaskIdentity, req).Want(http.StatusForbidden)
}
