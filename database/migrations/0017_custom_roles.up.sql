-- SOBH 0017: binding a member to a named role, and one call session per
-- participant.
--
-- `group_roles` has been in the schema since migration 0005, described as the
-- permission bundles an owner hands to moderators. Nothing could be handed
-- one: there was no column on `chat_members` naming which bundle a member
-- holds, so the table could be filled and never consulted.
--
-- The reference is ON DELETE SET NULL rather than CASCADE. Deleting a role
-- must not remove the people who held it from the chat; they fall back to
-- their built-in role's permissions, which is the safe direction — losing a
-- grant, not losing a membership.
ALTER TABLE chat_members
    ADD COLUMN custom_role_id UUID REFERENCES group_roles (id) ON DELETE SET NULL;

CREATE INDEX chat_members_custom_role_idx ON chat_members (custom_role_id)
    WHERE custom_role_id IS NOT NULL;

-- One session row per participant per call.
--
-- `call_sessions` has an `id` primary key and nothing stopping a second row
-- for the same pair, so the negotiation record would have been append-only:
-- a long call sending candidates for minutes would leave a row per signal,
-- and reading "what did this person negotiate" would mean merging them. One
-- row per participant, updated as the negotiation progresses, is what the
-- table was for.
CREATE UNIQUE INDEX call_sessions_participant_key
    ON call_sessions (call_id, user_id);
