"""BERT-NER DLP sidecar.

A tiny HTTP service that runs a transformers token-classification (NER) model
and returns labelled character spans, so the gateway's DLP layer 2 can detect
fuzzy/contextual PII the deterministic rules miss. The gateway calls POST
/scan; this service never sees API keys (only prompt text) and stores nothing.
"""

import os

from fastapi import FastAPI
from pydantic import BaseModel
from transformers import pipeline

MODEL = os.environ.get("DLP_MODEL", "dslim/bert-base-NER")
# DLP_MODEL_REVISION is the commit of the model baked into the image
# (DLP_BAKED_MODEL). With HF_HUB_OFFLINE=1 the cache only resolves the
# revision that was downloaded, so load exactly that one — but only for that
# model: a runtime DLP_MODEL override is a different repo, loaded at its
# default revision (it needs network access or its own cache).
BAKED_MODEL = os.environ.get("DLP_BAKED_MODEL", "")
REVISION = (os.environ.get("DLP_MODEL_REVISION") or None) if MODEL == BAKED_MODEL else None
MAX_CHARS = int(os.environ.get("DLP_MAX_CHARS", "65536"))
STRIDE = int(os.environ.get("DLP_STRIDE", "128"))

ner = pipeline(
    "token-classification",
    model=MODEL,
    revision=REVISION,
    aggregation_strategy="simple",
    stride=STRIDE,
)

app = FastAPI(title="airllm-dlp-bert")


class ScanRequest(BaseModel):
    text: str


@app.get("/healthz")
def healthz():
    return {"status": "ok", "model": MODEL}


@app.post("/scan")
def scan(req: ScanRequest):
    text = req.text
    truncated = len(text) > MAX_CHARS
    if truncated:
        text = text[:MAX_CHARS]
    findings = []
    for ent in ner(text):
        findings.append(
            {
                "label": str(ent["entity_group"]),
                "start": int(ent["start"]),
                "end": int(ent["end"]),
                "score": float(ent["score"]),
            }
        )
    return {"findings": findings, "truncated": truncated}
