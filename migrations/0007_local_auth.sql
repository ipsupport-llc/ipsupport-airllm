-- Local auth: persistent password login over the users table.
ALTER TABLE users ADD COLUMN IF NOT EXISTS password_hash   text NOT NULL DEFAULT '';
ALTER TABLE users ADD COLUMN IF NOT EXISTS password_set_at timestamptz NULL;
ALTER TABLE users ADD COLUMN IF NOT EXISTS disabled        bool NOT NULL DEFAULT false;
ALTER TABLE users ADD COLUMN IF NOT EXISTS auth_source     text NOT NULL DEFAULT 'local';
