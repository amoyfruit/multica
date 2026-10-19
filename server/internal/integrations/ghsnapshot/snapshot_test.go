package ghsnapshot

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// graphqlServer stands in for GitHub's GraphQL endpoint. tokenHandler mints a
// token; the queryFn produces the `data` payload for each GraphQL request,
// receiving the request variables so it can page.
func graphqlServer(t *testing.T, queryFn func(vars map[string]any) string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/access_tokens") {
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"token":"ghs_secret","expires_at":"` +
				time.Now().Add(time.Hour).UTC().Format(time.RFC3339) + `"}`))
			return
		}
		body, _ := io.ReadAll(r.Body)
		var req struct {
			Variables map[string]any `json:"variables"`
		}
		_ = json.Unmarshal(body, &req)
		_, _ = w.Write([]byte(`{"data":` + queryFn(req.Variables) + `}`))
	}))
}

func graphqlSnapshotPage(head string, hasNext bool, endCursor string) string {
	return fmt.Sprintf(`{"repository":{"pullRequest":{
		"headRefOid":%q,"mergeable":"MERGEABLE","mergeStateStatus":"CLEAN",
		"commits":{"nodes":[{"commit":{"statusCheckRollup":{"state":"PENDING","contexts":{
			"pageInfo":{"hasNextPage":%t,"endCursor":%q},"nodes":[]
		}}}}]}}}}`, head, hasNext, endCursor)
}

// TestFetchPRSnapshotPaginatesContexts is the acceptance-criterion-2 test:
// contexts spanning two pages must all be collected — never assume <100.
func TestFetchPRSnapshotPaginatesContexts(t *testing.T) {
	srv := graphqlServer(t, func(vars map[string]any) string {
		cursor, _ := vars["cursor"].(string)
		if cursor == "" {
			// Page 1: two contexts, more to come.
			return `{"repository":{"pullRequest":{
				"headRefOid":"sha1","mergeable":"MERGEABLE","mergeStateStatus":"CLEAN",
				"commits":{"nodes":[{"commit":{"statusCheckRollup":{"state":"FAILURE","contexts":{
					"pageInfo":{"hasNextPage":true,"endCursor":"CUR2"},
					"nodes":[
						{"__typename":"CheckRun","name":"backend","status":"COMPLETED","conclusion":"FAILURE","detailsUrl":"u1"},
						{"__typename":"CheckRun","name":"frontend","status":"COMPLETED","conclusion":"SUCCESS","detailsUrl":"u2"}
					]}}}}]}}}}`
		}
		// Page 2 (cursor==CUR2): one more context, plus a legacy StatusContext.
		return `{"repository":{"pullRequest":{
			"headRefOid":"sha1","mergeable":"MERGEABLE","mergeStateStatus":"CLEAN",
			"commits":{"nodes":[{"commit":{"statusCheckRollup":{"state":"FAILURE","contexts":{
				"pageInfo":{"hasNextPage":false,"endCursor":""},
				"nodes":[
					{"__typename":"CheckRun","name":"e2e","status":"IN_PROGRESS","conclusion":null,"detailsUrl":"u3"},
					{"__typename":"StatusContext","context":"vercel","state":"SUCCESS","targetUrl":"u4"}
				]}}}}]}}}}`
	})
	defer srv.Close()

	c := newTestClient(t, srv.URL)
	snap, err := FetchPRSnapshot(context.Background(), c, 1, "o", "r", 5)
	if err != nil {
		t.Fatalf("FetchPRSnapshot: %v", err)
	}
	if snap.HeadSHA != "sha1" || snap.Mergeable != "MERGEABLE" || snap.MergeStateStatus != "CLEAN" {
		t.Fatalf("head/merge = %+v", snap)
	}
	if !snap.HasChecks || snap.RollupState != "FAILURE" {
		t.Fatalf("rollup = %q hasChecks=%v", snap.RollupState, snap.HasChecks)
	}
	if len(snap.Contexts) != 4 {
		t.Fatalf("collected %d contexts across pages, want 4", len(snap.Contexts))
	}
	// StatusContext normalization: SUCCESS → completed/success, is_status_context.
	last := snap.Contexts[3]
	if last.Name != "vercel" || last.Status != "completed" || last.Conclusion != "success" || !last.IsStatusContext {
		t.Fatalf("status context normalized wrong: %+v", last)
	}
	// In-progress CheckRun stays running.
	if snap.Contexts[2].Status != "in_progress" || snap.Contexts[2].Conclusion != "" {
		t.Fatalf("in-progress run normalized wrong: %+v", snap.Contexts[2])
	}
	if snap.Decided() {
		t.Fatal("snapshot with an in-progress run must be undecided")
	}
}

func TestFetchPRSnapshotRejectsHeadChangeDuringPagination(t *testing.T) {
	calls := 0
	srv := graphqlServer(t, func(vars map[string]any) string {
		calls++
		if calls == 1 {
			return graphqlSnapshotPage("shaA", true, "CUR2")
		}
		return graphqlSnapshotPage("shaB", false, "")
	})
	defer srv.Close()

	c := newTestClient(t, srv.URL)
	snap, err := FetchPRSnapshot(context.Background(), c, 1, "o", "r", 5)
	if err == nil || !strings.Contains(err.Error(), "head changed during pagination") {
		t.Fatalf("FetchPRSnapshot error = %v, want head-change rejection", err)
	}
	if snap != nil {
		t.Fatalf("head-change response returned partial snapshot: %+v", snap)
	}
	if calls != 2 {
		t.Fatalf("GraphQL calls = %d, want 2", calls)
	}
}

func TestFetchPRSnapshotRejectsPaginationBeyondLimit(t *testing.T) {
	calls := 0
	srv := graphqlServer(t, func(vars map[string]any) string {
		calls++
		return graphqlSnapshotPage("shaA", true, fmt.Sprintf("CUR%d", calls))
	})
	defer srv.Close()

	c := newTestClient(t, srv.URL)
	snap, err := FetchPRSnapshot(context.Background(), c, 1, "o", "r", 5)
	if err == nil || !strings.Contains(err.Error(), "pagination exceeds page limit") {
		t.Fatalf("FetchPRSnapshot error = %v, want page-limit rejection", err)
	}
	if snap != nil {
		t.Fatalf("page-limit response returned truncated snapshot: %+v", snap)
	}
	if calls != maxSnapshotContextPages {
		t.Fatalf("GraphQL calls = %d, want %d", calls, maxSnapshotContextPages)
	}
}

// TestFetchPRSnapshotNullRollup is the acceptance-criterion-5 test: a null
// statusCheckRollup means "no checks yet" (HasChecks=false), never passed.
func TestFetchPRSnapshotNullRollup(t *testing.T) {
	srv := graphqlServer(t, func(vars map[string]any) string {
		return `{"repository":{"pullRequest":{
			"headRefOid":"sha9","mergeable":"UNKNOWN","mergeStateStatus":"UNKNOWN",
			"commits":{"nodes":[{"commit":{"statusCheckRollup":null}}]}}}}`
	})
	defer srv.Close()

	c := newTestClient(t, srv.URL)
	snap, err := FetchPRSnapshot(context.Background(), c, 1, "o", "r", 5)
	if err != nil {
		t.Fatalf("FetchPRSnapshot: %v", err)
	}
	if snap.HasChecks {
		t.Fatal("null rollup must yield HasChecks=false")
	}
	if snap.RollupState != "" || len(snap.Contexts) != 0 {
		t.Fatalf("null rollup must have empty rollup/contexts, got %+v", snap)
	}
	if snap.Decided() {
		t.Fatal("UNKNOWN mergeable must be undecided")
	}
}

func TestSnapshotDecided(t *testing.T) {
	cases := []struct {
		name string
		snap PRSnapshot
		want bool
	}{
		{"clean passed", PRSnapshot{Mergeable: "MERGEABLE", HasChecks: true, RollupState: "SUCCESS",
			Contexts: []CheckContext{{Status: "completed", Conclusion: "success"}}}, true},
		{"conflicting decided", PRSnapshot{Mergeable: "CONFLICTING", HasChecks: false}, true},
		{"mergeable unknown", PRSnapshot{Mergeable: "UNKNOWN", HasChecks: true, RollupState: "SUCCESS"}, false},
		{"rollup pending", PRSnapshot{Mergeable: "MERGEABLE", HasChecks: true, RollupState: "PENDING"}, false},
		{"running context", PRSnapshot{Mergeable: "MERGEABLE", HasChecks: true, RollupState: "SUCCESS",
			Contexts: []CheckContext{{Status: "in_progress"}}}, false},
		{"no checks but mergeable", PRSnapshot{Mergeable: "MERGEABLE", HasChecks: false}, true},
		// A queue entry always ends, so a queued PR is never settled: the chase
		// and the TTL sweep must keep re-reading it.
		{"queued with checks passed", PRSnapshot{Mergeable: "MERGEABLE", MergeQueueState: "QUEUED", HasChecks: true, RollupState: "SUCCESS",
			Contexts: []CheckContext{{Status: "completed", Conclusion: "success"}}}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.snap.Decided(); got != tc.want {
				t.Fatalf("Decided() = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestFetchPRSnapshotMergeQueue covers the merge-queue signal. A queued PR is
// still an open PR whose mergeStateStatus reads BLOCKED, so the entry state is
// the only field that says "waiting its turn to merge".
func TestFetchPRSnapshotMergeQueue(t *testing.T) {
	cases := []struct {
		name string
		pr   string
		want string
	}{
		{
			name: "entry state is preferred",
			pr:   `"isInMergeQueue":true,"mergeQueueEntry":{"state":"AWAITING_CHECKS"},`,
			want: "AWAITING_CHECKS",
		},
		{
			name: "unreadable entry falls back to QUEUED",
			pr:   `"isInMergeQueue":true,"mergeQueueEntry":null,`,
			want: "QUEUED",
		},
		{
			name: "not queued",
			pr:   `"isInMergeQueue":false,"mergeQueueEntry":null,`,
			want: "",
		},
		{
			name: "repository without a merge queue omits both fields",
			pr:   "",
			want: "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := graphqlServer(t, func(map[string]any) string {
				return `{"repository":{"pullRequest":{
					"headRefOid":"sha1","mergeable":"MERGEABLE","mergeStateStatus":"BLOCKED",` + tc.pr + `
					"commits":{"nodes":[{"commit":{"statusCheckRollup":null}}]}}}}`
			})
			defer srv.Close()

			snap, err := FetchPRSnapshot(context.Background(), newTestClient(t, srv.URL), 1, "o", "r", 5)
			if err != nil {
				t.Fatalf("FetchPRSnapshot: %v", err)
			}
			if snap.MergeQueueState != tc.want {
				t.Fatalf("MergeQueueState = %q, want %q", snap.MergeQueueState, tc.want)
			}
		})
	}
}

// TestPRSnapshotQueryRequestsMergeQueueFields pins the query text: dropping
// either field silently reverts every queued PR to an ordinary open PR.
func TestPRSnapshotQueryRequestsMergeQueueFields(t *testing.T) {
	for _, field := range []string{"isInMergeQueue", "mergeQueueEntry{state}"} {
		if !strings.Contains(prSnapshotQuery, field) {
			t.Fatalf("prSnapshotQuery is missing %q", field)
		}
	}
}

// mergeQueueRefusingServer answers like GitHub does for an installation that
// may not read merge-queue data: any query naming those fields gets HTTP 200
// with an `errors` array of errType and no usable data; the same query without
// them succeeds. It counts GraphQL requests by kind.
func mergeQueueRefusingServer(t *testing.T, errType string, withQueue, withoutQueue *int) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/access_tokens") {
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"token":"ghs_secret","expires_at":"` +
				time.Now().Add(time.Hour).UTC().Format(time.RFC3339) + `"}`))
			return
		}
		body, _ := io.ReadAll(r.Body)
		var req struct {
			Query string `json:"query"`
		}
		_ = json.Unmarshal(body, &req)
		if strings.Contains(req.Query, "mergeQueueEntry") {
			*withQueue++
			_, _ = w.Write([]byte(`{"data":{"repository":{"pullRequest":null}},"errors":[{"type":"` + errType +
				`","path":["repository","pullRequest","mergeQueueEntry"],"message":"Resource not accessible by integration"}]}`))
			return
		}
		*withoutQueue++
		_, _ = w.Write([]byte(`{"data":` + graphqlSnapshotPage("sha1", false, "") + `}`))
	}))
}

// TestFetchPRSnapshotSurvivesRefusedMergeQueueFields: a refusal of the
// merge-queue fields must never cost the PR its CI and mergeability snapshot,
// and a permission refusal must not be paid again on every fetch.
func TestFetchPRSnapshotSurvivesRefusedMergeQueueFields(t *testing.T) {
	var withQueue, withoutQueue int
	srv := mergeQueueRefusingServer(t, "FORBIDDEN", &withQueue, &withoutQueue)
	defer srv.Close()
	c := newTestClient(t, srv.URL)

	snap, err := FetchPRSnapshot(context.Background(), c, 1, "o", "r", 5)
	if err != nil {
		t.Fatalf("FetchPRSnapshot: %v", err)
	}
	if snap.HeadSHA != "sha1" || snap.Mergeable != "MERGEABLE" || snap.RollupState != "PENDING" {
		t.Fatalf("snapshot lost its non-queue fields: %+v", snap)
	}
	if snap.MergeQueueState != "" {
		t.Fatalf("MergeQueueState = %q, want empty when the fields were refused", snap.MergeQueueState)
	}
	if withQueue != 1 || withoutQueue != 1 {
		t.Fatalf("first fetch: %d queries with queue fields, %d without; want 1 and 1", withQueue, withoutQueue)
	}

	if _, err := FetchPRSnapshot(context.Background(), c, 1, "o", "r", 5); err != nil {
		t.Fatalf("second FetchPRSnapshot: %v", err)
	}
	if withQueue != 1 || withoutQueue != 2 {
		t.Fatalf("second fetch retried the refused fields: %d with, %d without", withQueue, withoutQueue)
	}

	// Another installation has its own permissions and still asks.
	if _, err := FetchPRSnapshot(context.Background(), c, 2, "o", "r", 5); err != nil {
		t.Fatalf("other installation FetchPRSnapshot: %v", err)
	}
	if withQueue != 2 {
		t.Fatalf("a refusal for installation 1 suppressed installation 2's queue read")
	}

	// The skip expires, so granting the permission takes effect on its own.
	c.now = func() time.Time { return time.Now().Add(mergeQueueRetryAfter + time.Minute) }
	if c.mergeQueueUnreadable(1) {
		t.Fatal("merge-queue skip did not expire")
	}
}

// TestFetchPRSnapshotNonPermissionErrorIsNotRemembered: any GraphQL-level
// error still falls back to a queue-less snapshot, but only a permission
// refusal is remembered — a one-off failure must not hide queues for an hour.
func TestFetchPRSnapshotNonPermissionErrorIsNotRemembered(t *testing.T) {
	var withQueue, withoutQueue int
	srv := mergeQueueRefusingServer(t, "INTERNAL", &withQueue, &withoutQueue)
	defer srv.Close()
	c := newTestClient(t, srv.URL)

	for i := 0; i < 2; i++ {
		if _, err := FetchPRSnapshot(context.Background(), c, 1, "o", "r", 5); err != nil {
			t.Fatalf("FetchPRSnapshot #%d: %v", i+1, err)
		}
	}
	if withQueue != 2 || withoutQueue != 2 {
		t.Fatalf("got %d queries with queue fields, %d without; want 2 and 2", withQueue, withoutQueue)
	}
}

// TestFetchPRSnapshotReturnsErrorWhenFallbackFails: when the query fails for a
// reason unrelated to the queue fields, the fallback fails too and the caller
// gets the error — never an empty snapshot.
func TestFetchPRSnapshotReturnsErrorWhenFallbackFails(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/access_tokens") {
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"token":"ghs_secret","expires_at":"` +
				time.Now().Add(time.Hour).UTC().Format(time.RFC3339) + `"}`))
			return
		}
		_, _ = w.Write([]byte(`{"data":null,"errors":[{"type":"FORBIDDEN","message":"Resource not accessible by integration"}]}`))
	}))
	defer srv.Close()
	c := newTestClient(t, srv.URL)

	if _, err := FetchPRSnapshot(context.Background(), c, 1, "o", "r", 5); err == nil {
		t.Fatal("expected an error when the PR itself is unreadable")
	}
	if c.mergeQueueUnreadable(1) {
		t.Fatal("a refusal unrelated to the queue fields marked the installation")
	}
}
