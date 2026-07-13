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
MAX_CHARS = int(os.environ.get("DLP_MAX_CHARS", "65536"))
STRIDE = int(os.environ.get("DLP_STRIDE", "128"))

ner = pipeline(
    "token-classification",
    model=MODEL,
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
