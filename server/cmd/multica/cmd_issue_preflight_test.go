package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/multica-ai/multica/server/internal/cli"
)

const preflightSHA = "14a994934f51affd54787ae710ee505b4c12875f"

func TestIssuePreflightCompatibility(t *testing.T) {
	now := time.Now().UTC()
	for _, tc := range []struct {
		name, version, profile, status string
		redacted, managed, capability  bool
		want                           string
	}{
		{"compatible", "1.19.3", "", "online", false, true, true, "ready"},
		{"incompatible", "opencode v2.0.10", "", "online", false, true, true, "blocked"},
		{"missing capability", "1.19.3", "", "online", false, true, false, "blocked"},
		{"offline", "1.19.3", "", "offline", false, true, true, "blocked"},
		{"custom profile", "2.0.10", "profile", "online", false, true, true, "unknown"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rt := preflightRuntime{ID: "runtime", Provider: "opencode", ProfileID: tc.profile, Status: tc.status, LastSeenAt: now}
			rt.Metadata.Version = tc.version
			if tc.capability {
				rt.Metadata.Capabilities = []string{"remote-mcp-v1"}
			}
			ag := preflightAgent{ID: "agent", RuntimeID: "runtime", MCPRedacted: tc.redacted}
			report := evaluateIssuePreflight(ag, &rt, nil, preflightOptions{ManagedMCP: tc.managed}, now)
			if report.Status != tc.want {
				t.Fatalf("status = %s, want %s: %+v", report.Status, tc.want, report.Findings)
			}
		})
	}
}

func TestIssuePreflightHeartbeatAndRequirements(t *testing.T) {
	now := time.Now().UTC()
	rt := preflightRuntime{ID: "runtime", Provider: "codex", Status: "online", LastSeenAt: now.Add(-10 * time.Minute)}
	report := evaluateIssuePreflight(preflightAgent{RuntimeID: rt.ID}, &rt, nil, preflightOptions{}, now)
	if report.Status != "blocked" {
		t.Fatalf("stale heartbeat: %+v", report)
	}
	rt.LastSeenAt = now
	report = evaluateIssuePreflight(preflightAgent{RuntimeID: rt.ID}, &rt, nil, preflightOptions{Capabilities: []string{"local-worktree-v1"}}, now)
	if report.Status != "blocked" {
		t.Fatalf("missing requested capability: %+v", report)
	}
}

func TestIssuePreflightQAEvidence(t *testing.T) {
	for _, tc := range []struct{ name, content, revision, want string }{
		{"same revision", "QA PASS\nQA revision: " + preflightSHA, preflightSHA, "pass_same_revision"},
		{"real comment format", "ГАТОВА — **QA PASS**\nbase `" + strings.Repeat("a", 40) + "`, merge/live **`" + preflightSHA + "`**", preflightSHA, "pass_same_revision"},
		{"new revision", "QA PASS\nQA revision: " + preflightSHA, strings.Repeat("a", 40), "stale_revision"},
		{"missing revision", "QA PASS", preflightSHA, "unverified"},
		{"missing current revision", "QA PASS\nQA revision: " + preflightSHA, "", "unverified"},
		{"negated", "No QA PASS\nQA revision: " + preflightSHA, preflightSHA, "unverified"},
		{"quoted", "> QA PASS\nQA revision: " + preflightSHA, preflightSHA, "unverified"},
		{"ambiguous", "QA PASS\nQA revision: " + preflightSHA + "\nQA revision: " + strings.Repeat("a", 40), preflightSHA, "unverified"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			qa := evaluatePreflightQA(preflightComment{ID: "comment", AuthorID: "qa", AuthorType: "agent", Content: tc.content}, preflightOptions{QAComment: "comment", QAAuthor: "qa", Revision: tc.revision})
			if qa.Status != tc.want {
				t.Fatalf("got %+v, want %s", qa, tc.want)
			}
		})
	}
}

func TestIssuePreflightFailureIndependentOfQA(t *testing.T) {
	qa := evaluatePreflightQA(preflightComment{ID: "comment", AuthorID: "qa", AuthorType: "agent", Content: "QA PASS\nQA revision: " + preflightSHA}, preflightOptions{QAComment: "comment", QAAuthor: "qa", Revision: preflightSHA})
	for _, tc := range []struct{ message, want string }{
		{"You’ve hit your usage limit.", "agent_error.provider_quota_limit"},
		{"RESOURCE_EXHAUSTED (code 429): exhausted capacity", "agent_error.provider_capacity_or_rate_limit"},
		{"opencode exited with error: exit status 1", "agent_error.process_failure"},
	} {
		f := summarizePreflightFailure(preflightRun{ID: "failed", Status: "failed", Error: tc.message, Attribution: json.RawMessage(`{"source":"direct_human"}`)})
		if f.Reason != tc.want {
			t.Fatalf("%s: %+v", tc.message, f)
		}
		if qa.Status != "pass_same_revision" || !strings.Contains(string(f.Attribution), "direct_human") {
			t.Fatal("process failure changed QA or attribution")
		}
	}
}

func TestIssuePreflightBlockedDoesNotRerunOrLeakSecrets(t *testing.T) {
	var posts int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			posts++
			t.Errorf("unexpected mutation %s", r.URL.Path)
		}
		switch r.URL.Path {
		case "/api/issues/issue":
			w.Write([]byte(`{"assignee_type":"agent","assignee_id":"agent"}`))
		case "/api/agents/agent":
			w.Write([]byte(`{"id":"agent","runtime_id":"runtime","mcp_config":{"mcpServers":{"secret":{"headers":{"Authorization":"NEVER-PRINT"}}}}}`))
		case "/api/agents/agent/mcp-servers":
			w.Write([]byte(`[]`))
		case "/api/runtimes":
			json.NewEncoder(w).Encode([]any{map[string]any{"id": "runtime", "provider": "opencode", "status": "online", "last_seen_at": time.Now(), "metadata": map[string]any{"version": "2.0.10", "capabilities": []string{"remote-mcp-v1"}}}})
		case "/api/issues/issue/task-runs":
			w.Write([]byte(`[]`))
		default:
			t.Errorf("unexpected path %s", r.URL)
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	client := &cli.APIClient{BaseURL: srv.URL, HTTPClient: srv.Client()}
	report, err := collectIssuePreflight(context.Background(), client, "issue", preflightOptions{}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if err := executePreflightRerun(context.Background(), client, "issue", &report); err == nil {
		t.Fatal("blocked preflight allowed a rerun")
	}
	encoded, _ := json.Marshal(report)
	if posts != 0 || strings.Contains(string(encoded), "NEVER-PRINT") {
		t.Fatalf("posts=%d, output=%s", posts, encoded)
	}
}

func TestIssuePreflightModelCatalog(t *testing.T) {
	now := time.Now().UTC()
	for _, tc := range []struct {
		name, model, status string
		age                 time.Duration
		fallback            bool
		want                string
	}{
		{"available", "provider/model", "completed", 0, false, "verified_catalog"},
		{"absent", "provider/missing", "completed", 0, false, "unavailable"},
		{"pending", "provider/model", "pending", 0, false, "unverified"},
		{"stale", "provider/model", "completed", 10 * time.Minute, false, "unverified"},
		{"fallback", "provider/model", "completed", 0, true, "unverified"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := issuePreflightReport{Status: "ready", RuntimeID: "runtime", Model: tc.model}
			var catalog preflightModelCatalog
			body, _ := json.Marshal(map[string]any{"runtime_id": "runtime", "status": tc.status, "supported": true, "fallback": tc.fallback, "updated_at": now.Add(-tc.age), "models": []any{map[string]any{"id": "provider/model"}}})
			if err := json.Unmarshal(body, &catalog); err != nil {
				t.Fatal(err)
			}
			applyPreflightModelCatalog(&r, catalog, now)
			if r.ModelCheck != tc.want {
				t.Fatalf("got %+v, want %s", r, tc.want)
			}
			if tc.want != "verified_catalog" && r.Status == "ready" {
				t.Fatal("explicit catalog check failed open")
			}
		})
	}
}

func TestIssuePreflightRerunCarriesReportAndSeparateQA(t *testing.T) {
	now := time.Now().UTC()
	posts := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/issues/issue":
			w.Write([]byte(`{"assignee_type":"agent","assignee_id":"agent"}`))
		case "/api/agents/agent":
			w.Write([]byte(`{"id":"agent","runtime_id":"runtime","mcp_config_redacted":true}`))
		case "/api/agents/agent/mcp-servers":
			w.Write([]byte(`[]`))
		case "/api/runtimes":
			json.NewEncoder(w).Encode([]any{map[string]any{"id": "runtime", "provider": "codex", "status": "online", "last_seen_at": now, "metadata": map[string]any{"capabilities": []string{"remote-mcp-v1"}}}})
		case "/api/issues/issue/task-runs":
			json.NewEncoder(w).Encode([]any{map[string]any{"id": "failed", "status": "failed", "created_at": now, "error": "usage limit"}})
		case "/api/issues/issue/comments":
			if r.URL.Query().Get("thread") != "qa-comment" || r.URL.Query().Get("tail") != "30" {
				t.Error("QA read must be bounded")
			}
			json.NewEncoder(w).Encode([]any{map[string]any{"id": "qa-comment", "author_id": "qa", "author_type": "agent", "created_at": now.Add(-time.Hour), "content": "QA PASS\nQA revision: " + preflightSHA}})
		case "/api/issues/issue/rerun":
			if r.Method != http.MethodPost {
				t.Errorf("rerun method = %s", r.Method)
			}
			posts++
			w.WriteHeader(http.StatusAccepted)
			w.Write([]byte(`{"id":"new-run"}`))
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	client := &cli.APIClient{BaseURL: srv.URL, HTTPClient: srv.Client()}
	report, err := collectIssuePreflight(context.Background(), client, "issue", preflightOptions{Purpose: "infrastructure_diagnostic", ManagedMCP: true, Revision: preflightSHA, QAComment: "qa-comment", QAAuthor: "qa"}, now)
	if err != nil {
		t.Fatal(err)
	}
	if posts != 0 {
		t.Fatal("read-only preflight started a run")
	}
	if report.QA.Status != "pass_same_revision" || report.Failure == nil || report.Failure.Reason != "agent_error.provider_quota_limit" {
		t.Fatalf("independent outcomes lost: %+v", report)
	}
	if err := executePreflightRerun(context.Background(), client, "issue", &report); err != nil {
		t.Fatal(err)
	}
	if posts != 1 || report.RerunID != "new-run" || report.Purpose != "infrastructure_diagnostic" {
		t.Fatalf("rerun not recorded once: %+v, posts=%d", report, posts)
	}
}
