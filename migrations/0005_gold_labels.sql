-- Add gold_labels column for reviewer-confirmed training spans.
ALTER TABLE capture_index ADD COLUMN IF NOT EXISTS gold_labels jsonb NOT NULL DEFAULT '[]';
