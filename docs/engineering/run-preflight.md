# Issue run preflight

`multica issue preflight ISSUE --output json` reads the current assignee, runtime,
enabled workspace MCP bindings and last failed run. It does not start an agent.
`--rerun --purpose infrastructure_diagnostic|feature_work` uses the existing
rerun endpoint only after this same check succeeds. The purpose is recorded in
the command's JSON report; it is not a new task database field. Save the report
when handing off an infrastructure diagnostic.

Required capabilities are supplied with repeatable `--require-capability` flags.
`--managed-mcp` declares that the task requires managed MCP, including servers
supplied by integrations or plugins. Those overlays are not exposed by the
agent-read API. Redacted agent MCP configuration is unknown, never absent.
OpenCode 2.x with required or observed managed MCP is refused before rerun;
unknown MCP delivery on OpenCode 2.x also prevents rerun. Custom runtime profiles
cannot be inferred compatible from the wrapped binary's version.

The report distinguishes `blocked`, `unknown`, and `ready` for the facts checked.
Runtime heartbeat must be online and recent. Model selection is displayed;
the read APIs used here do not provide an authoritative account/model catalog or
quota balance. `--model-request ID` validates an existing, fresh, completed
model-list response through its real runtime API, including runtime scope and
fallback status. It does not start discovery or poll a pending request. Without
that evidence, model availability and future quota remain explicitly unverified.
No runtime/model selection or MCP configuration is changed. There is no cooldown.
The server and daemon remain authoritative after the snapshot; other dispatch
paths do not go through this command.

QA is existing comment evidence, not a new server field. `--qa-comment ID
--qa-author ID --revision FULL_SHA` reads a bounded thread and selects exactly
that author's comment. It recognizes an affirmative `QA PASS` heading and a
single distinct full commit SHA. Missing, ambiguous, negated, unversioned, or
different-revision evidence never becomes a current PASS. The report labels
this a caller-selected citation, not a search for the latest workspace-wide QA.
A failed process is reported independently; it does not mutate that evidence or
the issue's status. Historical run attribution is preserved verbatim.

Examples (use the CLI built from the reviewed source):

```sh
multica issue preflight RAD-92 --managed-mcp --require-capability remote-mcp-v1
multica issue preflight RAD-92 --rerun --purpose infrastructure_diagnostic
multica issue preflight RAD-70 --revision FULL_SHA --qa-comment COMMENT_UUID --qa-author QA_AGENT_UUID
```

Regression cases live beside the implementation: compatible and incompatible
OpenCode, missing capabilities, offline/stale runtime, redacted MCP, custom
profile, quota/capacity/process failure, same-SHA QA evidence, missing/stale/
ambiguous/negated QA, and HTTP checks that blocked reruns make no POST.
