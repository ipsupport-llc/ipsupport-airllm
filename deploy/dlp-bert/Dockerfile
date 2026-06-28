# BERT-NER DLP sidecar. CPU-only torch; the model is baked in at build time so
# the image is self-contained (no download at runtime).
FROM python:3.11-slim

WORKDIR /app

# CPU-only torch (avoids the large CUDA wheels), then the serving deps.
RUN pip install --no-cache-dir torch --index-url https://download.pytorch.org/whl/cpu \
 && pip install --no-cache-dir fastapi "uvicorn[standard]" transformers

ARG DLP_MODEL=dslim/bert-base-NER
ENV DLP_MODEL=${DLP_MODEL}

# Pre-download the model weights into the image.
RUN python -c "from transformers import pipeline; pipeline('token-classification', model='${DLP_MODEL}', aggregation_strategy='simple')"

COPY app.py .

EXPOSE 8000
CMD ["uvicorn", "app:app", "--host", "0.0.0.0", "--port", "8000"]
