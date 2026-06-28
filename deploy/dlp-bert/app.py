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

MODEL = os.environ.get("DLP_MODEL", "Isotonic/distilbert_finetuned_ai4privacy_v2")

ner = pipeline("token-classification", model=MODEL, aggregation_strategy="simple")

app = FastAPI(title="airllm-dlp-bert")


class ScanRequest(BaseModel):
    text: str


@app.get("/healthz")
def healthz():
    return {"status": "ok", "model": MODEL}


@app.post("/scan")
def scan(req: ScanRequest):
    findings = []
    for ent in ner(req.text):
        findings.append(
            {
                "label": str(ent["entity_group"]),
                "start": int(ent["start"]),
                "end": int(ent["end"]),
                "score": float(ent["score"]),
            }
        )
    return {"findings": findings}
