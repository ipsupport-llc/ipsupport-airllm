-- Per-user budgets and user groups.
--
-- Groups are for reporting only: one group per user, a user without one
-- reports as "Ungrouped", and deleting a group ungroups its members.
-- budgets holds the user's calendar-period caps, e.g.
-- {"tokens":{"day":0,"week":2000000,"month":0},"cost_usd":{"day":5,"week":0,"month":50}};
-- a missing key or 0 means no cap. Adding a column with a constant default
-- is a catalog-only change, so neither ALTER rewrites the users table.
CREATE TABLE user_groups (
    id          uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    name        text UNIQUE NOT NULL,
    description text NOT NULL DEFAULT '',
    created_at  timestamptz NOT NULL DEFAULT now()
);

ALTER TABLE users ADD COLUMN group_id uuid NULL REFERENCES user_groups(id) ON DELETE SET NULL;
ALTER TABLE users ADD COLUMN budgets jsonb NOT NULL DEFAULT '{}';
