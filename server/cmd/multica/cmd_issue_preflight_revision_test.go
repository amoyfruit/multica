package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/multica-ai/multica/server/internal/cli"
)

func TestIssuePreflightRedactionPreventsUnverifiedRerun(t *testing.T) {
	now := time.Now().UTC()
	for _, provider := range []string{"opencode", "antigravity", "codex"} {
		t.Run(provider, func(t *testing.T) {
			rt := preflightRuntime{ID: "runtime", Provider: provider, Status: "online", LastSeenAt: now}
			rt.Metadata.Version = "1.19.3"
			rt.Metadata.Capabilities = []string{"remote-mcp-v1"}
			a := preflightAgent{ID: "agent", RuntimeID: rt.ID}
			visible := evaluateIssuePreflight(a, &rt, nil, preflightOptions{}, now)
			if visible.Status != "ready" {
				t.Fatalf("visible empty config: %+v", visible)
			}
			a.MCPRedacted = true
			hidden := evaluateIssuePreflight(a, &rt, nil, preflightOptions{}, now)
			if hidden.Status != "unknown" || !slices.ContainsFunc(hidden.Findings, func(f preflightFinding) bool {
				return f.Code == "mcp_config_unverified" && strings.Contains(f.Action, "--managed-mcp")
			}) {
				t.Fatalf("hidden config must change the verdict: %+v", hidden)
			}
			posts := 0
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { posts++; w.Write([]byte(`{"id":"unexpected"}`)) }))
			defer srv.Close()
			client := &cli.APIClient{BaseURL: srv.URL, HTTPClient: srv.Client()}
			if err := executePreflightRerun(context.Background(), client, "issue", &hidden); err == nil || posts != 0 {
				t.Fatalf("unknown config started a run: err=%v posts=%d", err, posts)
			}
			declared := evaluateIssuePreflight(a, &rt, nil, preflightOptions{ManagedMCP: true}, now)
			want := "ready"
			if provider == "antigravity" {
				want = "blocked"
			}
			if declared.Status != want {
				t.Fatalf("declared MCP: %+v, want %s", declared, want)
			}
		})
	}
}

func TestIssuePreflightMissingQAEvidenceIsActionable(t *testing.T) {
	now := time.Now().UTC()
	for _, tc := range []struct {
		name     string
		opts     preflightOptions
		comments string
		want     string
	}{
		{"not selected", preflightOptions{}, "[]", "qa_evidence_unselected"},
		{"outside bounded thread", preflightOptions{QAComment: "old-reply", QAAuthor: "qa", Revision: preflightSHA}, `[{"id":"root","author_id":"qa","author_type":"agent","content":"QA PASS"}]`, "qa_comment_unavailable"},
		{"wrong author", preflightOptions{QAComment: "old-reply", QAAuthor: "qa", Revision: preflightSHA}, `[{"id":"old-reply","author_id":"other","author_type":"agent","content":"QA PASS"}]`, "qa_evidence_unverified"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			reads, posts := 0, 0
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodGet {
					posts++
					t.Errorf("unexpected mutation %s", r.Method)
				}
				switch r.URL.Path {
				case "/api/issues/issue":
					w.Write([]byte(`{"assignee_type":"agent","assignee_id":"agent"}`))
				case "/api/agents/agent":
					w.Write([]byte(`{"id":"agent","runtime_id":"runtime"}`))
				case "/api/agents/agent/mcp-servers", "/api/issues/issue/task-runs":
					w.Write([]byte(`[]`))
				case "/api/runtimes":
					json.NewEncoder(w).Encode([]any{map[string]any{"id": "runtime", "provider": "codex", "status": "online", "last_seen_at": now}})
				case "/api/issues/issue/comments":
					reads++
					if r.URL.Query().Get("thread") != "old-reply" || r.URL.Query().Get("tail") != "30" {
						t.Error("QA read must stay bounded")
					}
					w.Write([]byte(tc.comments))
				default:
					t.Errorf("unexpected path %s", r.URL.Path)
					http.NotFound(w, r)
				}
			}))
			defer srv.Close()
			client := &cli.APIClient{BaseURL: srv.URL, HTTPClient: srv.Client()}
			r, err := collectIssuePreflight(context.Background(), client, "issue", tc.opts, now)
			if err != nil {
				t.Fatal(err)
			}
			if !slices.ContainsFunc(r.Findings, func(f preflightFinding) bool {
				return f.Code == tc.want && strings.Contains(f.Action, "--qa-comment") && strings.Contains(f.Action, "--qa-author") && strings.Contains(f.Action, "--revision")
			}) {
				t.Fatalf("missing actionable QA finding: %+v", r.Findings)
			}
			if r.Status != "ready" || posts != 0 {
				t.Fatalf("QA evidence changed runtime admission or mutated state: %+v posts=%d", r, posts)
			}
			if (tc.opts.QAComment == "" && reads != 0) || reads > 1 {
				t.Fatalf("unexpected comment reads: %d", reads)
			}
			encoded, _ := json.Marshal(r.QA)
			if strings.Contains(string(encoded), "comment_id") || strings.Contains(string(encoded), "created_at") {
				t.Fatalf("unavailable/unverified citation must omit identity and time: %s", encoded)
			}
		})
	}
}
