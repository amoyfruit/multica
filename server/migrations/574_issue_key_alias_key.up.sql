CREATE UNIQUE INDEX CONCURRENTLY idx_issue_key_alias_key ON issue_key_alias (source_workspace_id,prefix,number);
