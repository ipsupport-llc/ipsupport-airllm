-- Cloud-hosted provider kinds need structured configuration that neither a
-- credential nor base_url can carry: Vertex AI is addressed by a cloud project
-- and a location, and an assembled endpoint URL cannot be taken apart again.
-- Deliberately generic rather than a pair of vertex_* columns — the next
-- cloud-hosted kind needs the same shape, and a later embeddings capability
-- assembles a different endpoint from these same two values.
--
-- Additive with a default, so it cannot disturb the providers already stored.
ALTER TABLE providers ADD COLUMN config jsonb NOT NULL DEFAULT '{}'::jsonb;
