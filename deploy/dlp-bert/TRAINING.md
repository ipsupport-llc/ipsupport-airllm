# DLP model fine-tuning runbook

This document explains how to retrain the token-classification (NER) model used
by the `dlp-bert` sidecar, using labeled data collected by the gateway's capture
flywheel.

---

## Step 1 — Export the labeled dataset

The gateway accumulates reviewed captures in `capture_index`. Captures with
`review_status = 'confirmed'` or `'false_negative'` are eligible for export.

### 1a. Trigger the export

```sh
curl -s -X POST https://<gateway-host>/api/admin/dataset/export \
  -H "Authorization: Bearer <admin-session-token>" \
  | jq .
# → {"artifact_key": "datasets/20260627-143012.jsonl", "count": 412}
```

The artifact is written to the configured blob store (filesystem path under
`CAPTURE_BLOB_DIR` in dev; MinIO/GCS bucket in production).

### 1b. Fetch the JSONL artifact

**Dev (filesystem blob):**
```sh
cat /var/lib/airllm/blobs/datasets/20260627-143012.jsonl > train.jsonl
```

**MinIO:**
```sh
mc cp airllm/blobs/datasets/20260627-143012.jsonl train.jsonl
```

### JSONL format

One line per message (not per capture) so span offsets always align with the
`text` field:

```json
{"text":"Contact John at john@example.com","spans":[{"label":"EMAIL","start":15,"end":32}]}
```

Spans use byte offsets into `text`. Gold labels (set by reviewers) take
precedence over machine-detected labels.

---

## Step 2 — Fine-tune the token-classification model

### Prerequisites

```sh
pip install transformers datasets seqeval accelerate
```

### Prepare the dataset

```python
# convert.py — convert JSONL to HuggingFace NER format
import json, sys
from datasets import Dataset

rows = [json.loads(l) for l in open("train.jsonl")]

def to_hf(row):
    text = row["text"]
    tokens, labels = [], []
    prev = 0
    # Simple whitespace tokenizer — swap in your tokenizer of choice.
    for span in sorted(row["spans"], key=lambda s: s["start"]):
        if prev < span["start"]:
            tokens.append(text[prev:span["start"]])
            labels.append("O")
        tokens.append(text[span["start"]:span["end"]])
        labels.append("B-" + span["label"])
        prev = span["end"]
    if prev < len(text):
        tokens.append(text[prev:])
        labels.append("O")
    return {"tokens": tokens, "ner_tags": labels}

ds = Dataset.from_list([to_hf(r) for r in rows])
ds.save_to_disk("ner_dataset")
```

### Fine-tune with HuggingFace Trainer

```python
# train.py
from transformers import (AutoTokenizer, AutoModelForTokenClassification,
                          TrainingArguments, Trainer, DataCollatorForTokenClassification)
from datasets import load_from_disk
import numpy as np
from seqeval.metrics import classification_report

BASE_MODEL = "Isotonic/distilbert_finetuned_ai4privacy_v2"  # or your current DLP_MODEL
OUTPUT_DIR = "./dlp-bert-finetuned"

tokenizer = AutoTokenizer.from_pretrained(BASE_MODEL)
ds = load_from_disk("ner_dataset")

label_list = sorted({t for row in ds["ner_tags"] for t in row})
label2id  = {l: i for i, l in enumerate(label_list)}
id2label  = {i: l for l, i in label2id.items()}

def tokenize_and_align(examples):
    tok = tokenizer(examples["tokens"], truncation=True,
                    is_split_into_words=True)
    labels = []
    for i, label in enumerate(examples["ner_tags"]):
        word_ids = tok.word_ids(batch_index=i)
        prev = None
        row = []
        for wid in word_ids:
            if wid is None:
                row.append(-100)
            elif wid != prev:
                row.append(label2id[label[wid]])
            else:
                row.append(-100)  # ignore subword continuations
            prev = wid
        labels.append(row)
    tok["labels"] = labels
    return tok

tokenized = ds.map(tokenize_and_align, batched=True)

model = AutoModelForTokenClassification.from_pretrained(
    BASE_MODEL, num_labels=len(label_list),
    id2label=id2label, label2id=label2id, ignore_mismatched_sizes=True)

args = TrainingArguments(
    output_dir=OUTPUT_DIR,
    num_train_epochs=3,
    per_device_train_batch_size=16,
    learning_rate=2e-5,
    weight_decay=0.01,
    save_strategy="epoch",
    fp16=True,          # remove if no GPU
)

trainer = Trainer(
    model=model, args=args,
    train_dataset=tokenized,
    data_collator=DataCollatorForTokenClassification(tokenizer),
)
trainer.train()
trainer.save_model(OUTPUT_DIR)
tokenizer.save_pretrained(OUTPUT_DIR)
print("Saved to", OUTPUT_DIR)
```

Run on the GPU node:

```sh
python convert.py
python train.py
```

Training on ~500 samples takes a few minutes on a single GPU; scale
`num_train_epochs` and `per_device_train_batch_size` to your dataset size.

---

## Step 3 — Deploy the new model

### Push weights to the registry / shared volume

```sh
# Option A: push to HuggingFace Hub
huggingface-cli login
huggingface-cli upload <your-org>/dlp-bert-airllm ./dlp-bert-finetuned

# Option B: copy to a shared volume that the sidecar mounts
rsync -a ./dlp-bert-finetuned/ gpu-node:/mnt/models/dlp-bert-finetuned/
```

### Rebuild and restart the sidecar

**docker compose (dev):**
```sh
DLP_MODEL=./dlp-bert-finetuned \
  docker compose -f deploy/docker-compose.yml --profile bert \
  up -d --build dlp-bert
```

**k8s / production:**
Update the `DLP_MODEL` env var in the `dlp-bert` Deployment to point at the
HuggingFace Hub model ID or the mounted local path, then roll the Deployment:

```sh
kubectl set env deployment/dlp-bert DLP_MODEL=<your-org>/dlp-bert-airllm
kubectl rollout restart deployment/dlp-bert
kubectl rollout status deployment/dlp-bert
```

---

## Step 4 — Confirm the new model is active and measure FN-rate improvement

### Verify the sidecar is using the new model

```sh
curl http://dlp-bert:8000/healthz
# → {"status":"ok","model":"<your-org>/dlp-bert-airllm"}
```

### Update the gateway DLP config to point at the sidecar

```sh
curl -s -X PUT https://<gateway-host>/api/admin/dlp \
  -H "Authorization: Bearer <admin-token>" \
  -H "Content-Type: application/json" \
  -d '{"enabled":true,"action":"redact","model_enabled":true,
       "model_url":"http://dlp-bert:8000","model_min_score":0.7}'
```

### Measure FN-rate over time

After deploying, new captures and secondpass results will reflect the updated
model's detections. Track the flywheel metrics:

- **False-negative rate**: in `capture_index`, compare rows where
  `secondpass_status = 'false_negative'` vs `'confirmed'` over time.
  A drop after the new model version goes live confirms improvement.

```sql
SELECT
  date_trunc('day', ts)           AS day,
  secondpass_status,
  count(*)                        AS n
FROM capture_index
WHERE ts > now() - interval '14 days'
GROUP BY 1, 2
ORDER BY 1, 2;
```

- **Gold label accumulation**: continue the review → export → retrain cycle
  each time `n ≥ 200` new gold labels accumulate, or after any significant
  secondpass spike (webhook `dlp.false_negative`).

> **Tip:** tag each training run and record the `artifact_key` alongside the
> model version so you can correlate FN-rate changes with dataset snapshots.
