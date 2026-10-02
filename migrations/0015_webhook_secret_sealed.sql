-- webhooks.secret was stored in plaintext — the one place in this codebase
-- that stores a secret unencrypted (provider creds use cred_enc/bytea and
-- AES-GCM via internal/secrets; this should too). Dropping the plaintext
-- column means any already-configured webhook's signing secret is cleared
-- by this upgrade; re-enter it afterward to resume signed delivery. A
-- webhook with no secret still delivers, just unsigned (secret is optional).
ALTER TABLE webhooks ADD COLUMN secret_enc bytea;
ALTER TABLE webhooks DROP COLUMN secret;
