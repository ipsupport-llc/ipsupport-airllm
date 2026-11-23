# BERT-NER DLP sidecar. CPU-only torch; the model is baked in at build time so
# the image is self-contained (no download at runtime).
#
# The tag is pinned to its multi-arch index digest; bump both together
# (`docker buildx imagetools inspect python:3.11-slim`).
FROM python:3.11-slim@sha256:07d4824036af339fa1b6ef769cc99d9c10a141769538b52b3b597c84f7a7b535

WORKDIR /app

# CPU-only torch (avoids the large CUDA wheels), then the pinned serving deps.
COPY requirements.txt .
RUN pip install --no-cache-dir torch==2.13.0 --index-url https://download.pytorch.org/whl/cpu \
 && pip install --no-cache-dir -r requirements.txt

# DLP_MODEL_REVISION pins the exact model commit (a Hugging Face commit sha;
# a branch name like "main" also works). Override it together with DLP_MODEL.
ARG DLP_MODEL=dslim/bert-base-NER
ARG DLP_MODEL_REVISION=d1a3e8f13f8c3566299d95fcfc9a8d2382a9affc
# DLP_BAKED_MODEL records which model the pinned revision belongs to; app.py
# applies the revision only when DLP_MODEL still names that model.
ENV DLP_MODEL=${DLP_MODEL} \
    DLP_BAKED_MODEL=${DLP_MODEL} \
    DLP_MODEL_REVISION=${DLP_MODEL_REVISION} \
    HF_HOME=/opt/hf

# Pre-download the model weights into the image, readable by the runtime user.
RUN python -c "from transformers import pipeline; pipeline('token-classification', model='${DLP_MODEL}', revision='${DLP_MODEL_REVISION}', aggregation_strategy='simple')" \
 && chmod -R a+rX /opt/hf

# Never reach the Hugging Face Hub at runtime: serve only the baked-in model.
ENV HF_HUB_OFFLINE=1 \
    PORT=8000

COPY app.py .

RUN useradd --uid 10001 --no-create-home --shell /usr/sbin/nologin app
USER 10001

EXPOSE 8000
# PORT lets the Helm chart's dlpBert.service.port move the listener.
CMD ["sh", "-c", "exec uvicorn app:app --host 0.0.0.0 --port \"${PORT}\""]
