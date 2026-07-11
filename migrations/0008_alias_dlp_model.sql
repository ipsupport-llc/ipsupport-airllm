-- Per-alias switch for the layer-2 (BERT) DLP model scan. Layer-1
-- deterministic scanning is not affected by this flag.
ALTER TABLE model_aliases
    ADD COLUMN dlp_model_scan boolean NOT NULL DEFAULT true;
