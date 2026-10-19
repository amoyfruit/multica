-- Targetless actions cannot be represented by the previous schema.
DELETE FROM quick_action WHERE assignee_type = '';
ALTER TABLE quick_action
    DROP CONSTRAINT IF EXISTS quick_action_assignee_type_check,
    ALTER COLUMN assignee_id SET NOT NULL,
    ADD CONSTRAINT quick_action_assignee_type_check
        CHECK (assignee_type IN ('agent', 'squad'));
