-- Batch audio (STT/TTS): pricing needs non-token units, and audio DLP
-- scanning is a separate per-alias toggle from the chat layer-2 gate.
ALTER TABLE pricing ADD COLUMN unit text NOT NULL DEFAULT 'tokens'
    CHECK (unit IN ('tokens', 'audio_second', 'text_char'));
ALTER TABLE model_aliases ADD COLUMN dlp_audio_scan boolean NOT NULL DEFAULT true;
