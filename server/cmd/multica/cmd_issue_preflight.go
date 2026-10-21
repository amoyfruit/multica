package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/multica-ai/multica/server/internal/cli"
	"github.com/multica-ai/multica/server/pkg/agent"
	"github.com/multica-ai/multica/server/pkg/taskfailure"
	"github.com/spf13/cobra"
)

var issuePreflightCmd = &cobra.Command{
	Use:   "preflight <issue-id>",
	Short: "Check runtime readiness and cite versioned QA before a manual rerun",
	Long: "Read-only unless --rerun is supplied. Checks the current assignee only. " +
		"Reports known runtime/MCP incompatibility, capabilities, process failure and " +
		"caller-selected QA evidence separately. Model availability and future quota " +
		"are not guaranteed. Other dispatch paths retain their existing checks.",
	Args: exactArgs(1), RunE: runIssuePreflight,
}

func init() {
	issueCmd.AddCommand(issuePreflightCmd)
	f := issuePreflightCmd.Flags()
	f.String("output", "json", "Output format: table or json")
	f.Bool("rerun", false, "Enqueue the current assignee only when preflight is ready")
	f.String("purpose", "", "Required for --rerun: infrastructure_diagnostic or feature_work (recorded in this report)")
	f.StringSlice("require-capability", nil, "Required runtime capability; repeatable")
	f.Bool("managed-mcp", false, "This task requires managed MCP, including integration/plugin overlays")
	f.String("revision", "", "Full current commit SHA; never inferred from a failed process")
	f.String("qa-comment", "", "Exact accepted QA comment to cite; does not search for the latest QA")
	f.String("qa-author", "", "Expected author UUID of --qa-comment")
	f.String("model-request", "", "Existing completed runtime model-list request UUID to validate once (no discovery or polling)")
}

type preflightOptions struct {
	Purpose, Revision, QAComment, QAAuthor, ModelRequest string
	Capabilities                                         []string
	ManagedMCP                                           bool
}

type preflightAgent struct {
	ID          string          `json:"id"`
	RuntimeID   string          `json:"runtime_id"`
	Model       string          `json:"model"`
	ArchivedAt  *time.Time      `json:"archived_at"`
	MCPRedacted bool            `json:"mcp_config_redacted"`
	MCP         json.RawMessage `json:"mcp_config"`
}

type preflightRuntime struct {
	ID         string    `json:"id"`
	Provider   string    `json:"provider"`
	ProfileID  string    `json:"profile_id"`
	Status     string    `json:"status"`
	LastSeenAt time.Time `json:"last_seen_at"`
	Metadata   struct {
		Version      string   `json:"version"`
		Capabilities []string `json:"capabilities"`
	} `json:"metadata"`
}

type preflightFinding struct {
	Code   string `json:"code"`
	Action string `json:"action"`
}

type issuePreflightReport struct {
	IssueID           string             `json:"issue_id"`
	CheckedAt         time.Time          `json:"checked_at"`
	Purpose           string             `json:"purpose"`
	Status            string             `json:"status"`
	AgentID           string             `json:"agent_id"`
	RuntimeID         string             `json:"runtime_id"`
	Provider          string             `json:"provider"`
	Version           string             `json:"version"`
	RequestedRevision string             `json:"requested_revision,omitempty"`
	Model             string             `json:"selected_model"`
	ModelCheck        string             `json:"model_check"`
	Findings          []preflightFinding `json:"findings"`
	Limitations       []string           `json:"limitations"`
	QA                preflightQA        `json:"qa_evidence"`
	Failure           *preflightFailure  `json:"last_failed_process,omitempty"`
	RerunID           string             `json:"rerun_id,omitempty"`
}

type preflightComment struct {
	ID         string     `json:"id"`
	AuthorID   string     `json:"author_id"`
	AuthorType string     `json:"author_type"`
	Content    string     `json:"content"`
	DeletedAt  *time.Time `json:"deleted_at"`
	CreatedAt  time.Time  `json:"created_at"`
}

type preflightQA struct {
	Status    string     `json:"status"`
	CommentID string     `json:"comment_id,omitempty"`
	Revision  string     `json:"revision,omitempty"`
	Source    string     `json:"source"`
	CreatedAt *time.Time `json:"created_at,omitempty"`
}

type preflightRun struct {
	ID          string          `json:"id"`
	Status      string          `json:"status"`
	Error       string          `json:"error"`
	Reason      string          `json:"failure_reason"`
	CreatedAt   time.Time       `json:"created_at"`
	Attribution json.RawMessage `json:"attribution"`
}

type preflightFailure struct {
	ID          string          `json:"id"`
	Reason      string          `json:"reason"`
	Action      string          `json:"action"`
	Attribution json.RawMessage `json:"attribution,omitempty"`
	CreatedAt   time.Time       `json:"created_at"`
}

type preflightModelCatalog struct {
	RuntimeID string    `json:"runtime_id"`
	Status    string    `json:"status"`
	Supported bool      `json:"supported"`
	Fallback  bool      `json:"fallback"`
	UpdatedAt time.Time `json:"updated_at"`
	Models    []struct {
		ID string `json:"id"`
	} `json:"models"`
}

func applyPreflightModelCatalog(r *issuePreflightReport, c preflightModelCatalog, now time.Time) {
	r.ModelCheck = "unverified"
	if c.RuntimeID != r.RuntimeID || c.Status != "completed" || !c.Supported || c.Fallback || c.UpdatedAt.IsZero() || now.Sub(c.UpdatedAt) > 2*time.Minute || c.UpdatedAt.After(now.Add(time.Minute)) || r.Model == "" {
		if r.Status == "ready" {
			r.Status = "unknown"
		}
		r.Findings = append(r.Findings, preflightFinding{"model_catalog_unverified", "Use a fresh completed model-list request for this runtime; no polling or discovery was started."})
		return
	}
	for _, model := range c.Models {
		if model.ID == r.Model {
			r.ModelCheck = "verified_catalog"
			return
		}
	}
	r.ModelCheck = "unavailable"
	r.Status = "blocked"
	r.Findings = append(r.Findings, preflightFinding{"model_unavailable", "The selected model is absent from this fresh runtime catalog; review the model/provider selection before retrying."})
}

func evaluateIssuePreflight(a preflightAgent, rt *preflightRuntime, bindings []workspaceMcpServer, opts preflightOptions, now time.Time) issuePreflightReport {
	r := issuePreflightReport{CheckedAt: now, Purpose: opts.Purpose, Status: "ready", AgentID: a.ID, RuntimeID: a.RuntimeID, Model: a.Model, ModelCheck: "unverified", RequestedRevision: opts.Revision,
		Findings: []preflightFinding{}, QA: preflightQA{Status: "unverified", Source: "no QA comment selected"},
		Limitations: []string{
			"Selected model is configuration, not proof of account/model availability; model discovery uses a separate asynchronous runtime API.",
			"No quota balance API is available here; future provider quota/capacity cannot be guaranteed.",
			"Integration/plugin/runtime MCP overlays are composed later; use --managed-mcp when the task requires them.",
			"Snapshot only: the server and daemon remain authoritative; this command does not guard other dispatch paths.",
		}}
	block := func(code, action string) {
		r.Status = "blocked"
		r.Findings = append(r.Findings, preflightFinding{code, action})
	}
	unknown := func(code, action string) {
		if r.Status == "ready" {
			r.Status = "unknown"
		}
		r.Findings = append(r.Findings, preflightFinding{code, action})
	}
	if a.ArchivedAt != nil {
		block("target_unavailable", "Use an active agent after reviewing its assignment.")
	}
	if rt == nil {
		block("runtime_unavailable", "Bind the agent to an accessible runtime; inspect runtime list.")
		return r
	}
	r.Provider, r.Version = rt.Provider, rt.Metadata.Version
	if rt.Status != "online" {
		block("runtime_offline", "Bring the selected runtime online before rerunning.")
	}
	if rt.LastSeenAt.IsZero() || now.Sub(rt.LastSeenAt) > 2*time.Minute || rt.LastSeenAt.After(now.Add(time.Minute)) {
		block("runtime_heartbeat_unverified", "Wait for a fresh runtime heartbeat; inspect runtime list.")
	}
	managed := opts.ManagedMCP
	for _, binding := range bindings {
		if binding.Enabled == nil || *binding.Enabled {
			managed = true
		}
	}
	if string(a.MCP) != "null" && len(a.MCP) > 0 {
		var config map[string]json.RawMessage
		if json.Unmarshal(a.MCP, &config) != nil {
			unknown("mcp_config_unverified", "Inspect the agent MCP configuration without exposing credentials.")
		} else {
			for _, key := range []string{"mcpServers", "mcp"} {
				var servers map[string]json.RawMessage
				if raw, ok := config[key]; ok {
					if json.Unmarshal(raw, &servers) != nil {
						unknown("mcp_config_unverified", "Inspect the agent MCP configuration.")
					} else if len(servers) > 0 {
						managed = true
					}
				}
			}
		}
	}
	if a.MCPRedacted && !managed {
		unknown("mcp_config_unverified", "Agent MCP configuration is redacted. Declare the task's MCP requirement with --managed-mcp to check compatibility and capabilities without revealing credentials.")
	}
	required := slices.Clone(opts.Capabilities)
	if managed {
		required = append(required, "remote-mcp-v1")
	}
	for _, capability := range required {
		if !slices.Contains(rt.Metadata.Capabilities, capability) {
			block("missing_capability", "The runtime must advertise required capability "+capability+"; update its implementation before rerunning.")
		}
	}
	if rt.ProfileID != "" {
		unknown("custom_runtime_contract_unverified", "Verify the custom runtime profile's actual protocol and MCP delivery contract.")
	} else if managed && slices.Contains([]string{"antigravity", "copilot", "muse", "zeroclaw"}, rt.Provider) {
		block("runtime_mcp_unsupported", "This built-in adapter does not forward managed MCP. Select a compatible runtime after approval; keep the required MCP servers.")
	} else if rt.Provider == "opencode" {
		if err := agent.CheckMinVersion("opencode", rt.Metadata.Version); err != nil {
			block("runtime_version_unsupported", "Use a supported OpenCode version and refresh runtime registration.")
		} else if agent.OpenCodeManagedMCPUnsupported(rt.Metadata.Version, managed) {
			block("runtime_mcp_incompatible", "OpenCode 2.x cannot receive managed MCP through this adapter. Select a compatible runtime after approval; keep the required MCP servers.")
		} else if agent.OpenCodeManagedMCPUnsupported(rt.Metadata.Version, true) {
			unknown("mcp_delivery_unverified", "OpenCode 2.x needs confirmation that no managed MCP is delivered, including redacted config and integration/plugin/runtime overlays.")
		}
	}
	return r
}

var preflightFullSHA = regexp.MustCompile(`(?i)\b[0-9a-f]{40}(?:[0-9a-f]{24})?\b`)
var preflightRevisionLabel = regexp.MustCompile("(?im)(?:QA revision:|merge/live)\\s*[*`]*([0-9a-f]{40}(?:[0-9a-f]{24})?)\\b")

func evaluatePreflightQA(c preflightComment, opts preflightOptions) preflightQA {
	q := preflightQA{Status: "unverified", Source: "caller-selected comment; not a latest-QA search"}
	if c.ID != opts.QAComment || c.AuthorID != opts.QAAuthor || opts.QAAuthor == "" || c.DeletedAt != nil || c.AuthorType != "agent" {
		return q
	}
	q.CommentID = c.ID
	if !c.CreatedAt.IsZero() {
		q.CreatedAt = &c.CreatedAt
	}
	first := strings.TrimSpace(strings.SplitN(c.Content, "\n", 2)[0])
	first = strings.TrimSpace(strings.TrimLeft(strings.ReplaceAll(first, "**", ""), "#"))
	if !(strings.HasPrefix(first, "QA PASS") || strings.HasPrefix(first, "ГАТОВА — QA PASS")) {
		return q
	}
	versions := map[string]bool{}
	for _, match := range preflightRevisionLabel.FindAllStringSubmatch(c.Content, -1) {
		versions[strings.ToLower(match[1])] = true
	}
	if len(versions) == 0 {
		for _, sha := range preflightFullSHA.FindAllString(c.Content, -1) {
			versions[strings.ToLower(sha)] = true
		}
	}
	if len(versions) != 1 {
		return q
	}
	for sha := range versions {
		q.Revision = sha
	}
	if !isPreflightSHA(opts.Revision) {
		return q
	}
	q.Status = "stale_revision"
	if strings.EqualFold(q.Revision, opts.Revision) {
		q.Status = "pass_same_revision"
	}
	return q
}

func applyPreflightQAEvidence(r *issuePreflightReport, comments []preflightComment, opts preflightOptions) {
	const selectionAction = "Read the latest accepted QA report with issue comment list --roots-only --summary, then --thread ID --tail 30; cite it with --qa-comment ID --qa-author ID --revision FULL_SHA."
	if opts.QAComment == "" {
		r.Findings = append(r.Findings, preflightFinding{"qa_evidence_unselected", selectionAction})
		return
	}
	for _, c := range comments {
		if c.ID == opts.QAComment {
			r.QA = evaluatePreflightQA(c, opts)
			if r.QA.Status == "unverified" {
				r.Findings = append(r.Findings, preflightFinding{"qa_evidence_unverified", "The selected comment does not verify an accepted QA revision for the expected author. " + selectionAction})
			}
			return
		}
	}
	r.QA = preflightQA{Status: "unverified", Source: "selected comment unavailable in bounded thread read"}
	r.Findings = append(r.Findings, preflightFinding{"qa_comment_unavailable", "The selected comment was not returned by the bounded thread read; inspect that thread's pagination. " + selectionAction})
}

func isPreflightSHA(s string) bool {
	return (len(s) == 40 || len(s) == 64) && preflightFullSHA.FindString(s) == s
}

func summarizePreflightFailure(run preflightRun) preflightFailure {
	reason := taskfailure.NormalizeDaemonReason(run.Reason, run.Error)
	if reason == "" {
		reason = taskfailure.Classify(run.Error)
	}
	action := "Inspect this process's transcript; it is not a new QA verdict."
	switch reason {
	case taskfailure.ReasonAgentProviderQuotaLimit:
		action = "Check the provider account's quota/credits before another manual attempt; future quota is unverified."
	case taskfailure.ReasonAgentProviderCapacityOrRateLimit:
		action = "Check provider capacity/rate limits before another manual attempt."
	}
	if strings.Contains(run.Error, "Multica-managed MCP servers cannot be delivered to an OpenCode 2.x runtime") {
		action = "OpenCode 2.x MCP delivery is incompatible; select a compatible runtime after approval and keep the required MCP servers."
	}
	return preflightFailure{ID: run.ID, Reason: string(reason), Action: action, Attribution: run.Attribution, CreatedAt: run.CreatedAt}
}

func preflightGet(ctx context.Context, client *cli.APIClient, path string, out any) error {
	if err := client.GetJSON(ctx, path, out); err != nil {
		var httpErr *cli.HTTPError
		if errors.As(err, &httpErr) {
			return fmt.Errorf("preflight read failed: HTTP %d (%s)", httpErr.StatusCode, path)
		}
		return fmt.Errorf("preflight read failed (%s); inspect connectivity", path)
	}
	return nil
}

func collectIssuePreflight(ctx context.Context, client *cli.APIClient, issueID string, opts preflightOptions, now time.Time) (issuePreflightReport, error) {
	var issue struct {
		AssigneeType string `json:"assignee_type"`
		AssigneeID   string `json:"assignee_id"`
	}
	base := "/api/issues/" + url.PathEscape(issueID)
	if err := preflightGet(ctx, client, base, &issue); err != nil {
		return issuePreflightReport{}, err
	}
	agentID := issue.AssigneeID
	if issue.AssigneeType == "squad" && agentID != "" {
		var squad struct {
			LeaderID string `json:"leader_id"`
		}
		if err := preflightGet(ctx, client, "/api/squads/"+url.PathEscape(agentID), &squad); err != nil {
			return issuePreflightReport{}, err
		}
		agentID = squad.LeaderID
	} else if issue.AssigneeType != "agent" {
		agentID = ""
	}
	var a preflightAgent
	if agentID != "" {
		if err := preflightGet(ctx, client, "/api/agents/"+url.PathEscape(agentID), &a); err != nil {
			return issuePreflightReport{}, err
		}
	}
	var runtimes []preflightRuntime
	if err := preflightGet(ctx, client, "/api/runtimes", &runtimes); err != nil {
		return issuePreflightReport{}, err
	}
	var rt *preflightRuntime
	for i := range runtimes {
		if runtimes[i].ID == a.RuntimeID {
			rt = &runtimes[i]
			break
		}
	}
	var bindings []workspaceMcpServer
	if agentID != "" {
		if err := preflightGet(ctx, client, agentMcpPath(agentID), &bindings); err != nil {
			return issuePreflightReport{}, err
		}
	}
	r := evaluateIssuePreflight(a, rt, bindings, opts, now)
	r.IssueID = issueID
	var runs []preflightRun
	if err := preflightGet(ctx, client, base+"/task-runs", &runs); err != nil {
		return r, err
	}
	var latest *preflightRun
	for i := range runs {
		if runs[i].Status == "failed" && (latest == nil || runs[i].CreatedAt.After(latest.CreatedAt)) {
			latest = &runs[i]
		}
	}
	if latest != nil {
		failure := summarizePreflightFailure(*latest)
		r.Failure = &failure
	}
	if opts.ModelRequest != "" && rt != nil {
		var catalog preflightModelCatalog
		path := "/api/runtimes/" + url.PathEscape(rt.ID) + "/models/" + url.PathEscape(opts.ModelRequest)
		if err := preflightGet(ctx, client, path, &catalog); err != nil {
			return r, err
		}
		applyPreflightModelCatalog(&r, catalog, now)
	}
	var comments []preflightComment
	if opts.QAComment != "" {
		path := base + "/comments?thread=" + url.QueryEscape(opts.QAComment) + "&tail=30"
		if err := preflightGet(ctx, client, path, &comments); err != nil {
			return r, err
		}
	}
	applyPreflightQAEvidence(&r, comments, opts)
	return r, nil
}

func executePreflightRerun(ctx context.Context, client *cli.APIClient, issueID string, report *issuePreflightReport) error {
	if report.Status != "ready" {
		return fmt.Errorf("preflight %s: no rerun was requested", report.Status)
	}
	var task struct {
		ID string `json:"id"`
	}
	if err := client.PostJSON(ctx, "/api/issues/"+url.PathEscape(issueID)+"/rerun", map[string]any{}, &task); err != nil {
		return fmt.Errorf("preflight passed but rerun was not confirmed; inspect issue runs before retrying")
	}
	report.RerunID = task.ID
	return nil
}

func runIssuePreflight(cmd *cobra.Command, args []string) error {
	f := cmd.Flags()
	opts := preflightOptions{}
	opts.Purpose, _ = f.GetString("purpose")
	opts.Revision, _ = f.GetString("revision")
	opts.QAComment, _ = f.GetString("qa-comment")
	opts.QAAuthor, _ = f.GetString("qa-author")
	opts.ModelRequest, _ = f.GetString("model-request")
	opts.Capabilities, _ = f.GetStringSlice("require-capability")
	opts.ManagedMCP, _ = f.GetBool("managed-mcp")
	rerun, _ := f.GetBool("rerun")
	output, _ := f.GetString("output")
	if output != "json" && output != "table" {
		return fmt.Errorf("--output must be json or table")
	}
	if opts.Purpose != "" && opts.Purpose != "infrastructure_diagnostic" && opts.Purpose != "feature_work" {
		return fmt.Errorf("invalid --purpose")
	}
	if rerun && opts.Purpose == "" {
		return fmt.Errorf("--rerun requires --purpose infrastructure_diagnostic or feature_work")
	}
	if opts.Revision != "" && !isPreflightSHA(opts.Revision) {
		return fmt.Errorf("--revision requires a full commit SHA")
	}
	if (opts.QAComment == "") != (opts.QAAuthor == "") {
		return fmt.Errorf("--qa-comment and --qa-author must be supplied together")
	}
	if opts.Purpose == "" {
		opts.Purpose = "read_only"
	}
	client, err := newAPIClient(cmd)
	if err != nil {
		return err
	}
	ctx, cancel := cli.APIContext(context.Background())
	defer cancel()
	ref, err := resolveIssueRef(ctx, client, args[0])
	if err != nil {
		return err
	}
	report, err := collectIssuePreflight(ctx, client, ref.ID, opts, time.Now().UTC())
	if err != nil {
		return err
	}
	var runErr error
	if rerun {
		runErr = executePreflightRerun(ctx, client, ref.ID, &report)
	}
	if output == "json" {
		if err := cli.PrintJSON(os.Stdout, report); err != nil {
			return err
		}
	} else {
		cli.PrintTable(os.Stdout, []string{"STATUS", "RUNTIME", "PROVIDER", "MODEL", "QA"}, [][]string{{report.Status, report.RuntimeID, report.Provider, report.Model, report.QA.Status}})
		for _, f := range report.Findings {
			fmt.Fprintf(os.Stdout, "%s: %s\n", f.Code, f.Action)
		}
		if report.QA.CommentID != "" {
			fmt.Fprintf(os.Stdout, "QA citation %s: %s; cited revision %s; caller-supplied current revision %s\n", report.QA.CommentID, report.QA.Status, report.QA.Revision, report.RequestedRevision)
		}
		if report.Failure != nil {
			fmt.Fprintf(os.Stdout, "Failed process %s: %s. %s\n", report.Failure.ID, report.Failure.Reason, report.Failure.Action)
		}
		for _, limit := range report.Limitations {
			fmt.Fprintln(os.Stdout, limit)
		}
	}
	if runErr != nil {
		return runErr
	}
	if report.Status != "ready" {
		return fmt.Errorf("preflight %s; inspect findings", report.Status)
	}
	return nil
}
