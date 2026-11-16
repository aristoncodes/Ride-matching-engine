# syntax=docker/dockerfile:1
FROM python:3.12-slim

# Pin uv to a known release; it is copied in as a static binary (no global pip install).
COPY --from=ghcr.io/astral-sh/uv:0.5.11 /uv /usr/local/bin/uv

ENV UV_COMPILE_BYTECODE=1 \
    UV_LINK_MODE=copy \
    UV_PROJECT_ENVIRONMENT=/app/.venv \
    PATH="/app/.venv/bin:$PATH" \
    PYTHONDONTWRITEBYTECODE=1 \
    PYTHONUNBUFFERED=1

WORKDIR /app

# Install dependencies first (cached layer), then the project itself.
COPY pyproject.toml uv.lock README.md ./
RUN uv sync --no-dev --no-install-project

COPY copilot ./copilot
RUN uv sync --no-dev

# Run as a non-root user.
# Numeric UID so Kubernetes can verify runAsNonRoot (matches runAsUser in k8s/base/copilot.yaml).
# The embedding model is downloaded at build time, not at runtime: the pod's root
# filesystem is read-only (k8s/base/copilot.yaml), so a first-use download fails.
ENV FASTEMBED_CACHE_PATH=/opt/fastembed
RUN /app/.venv/bin/python -c "from fastembed import TextEmbedding; TextEmbedding(model_name='BAAI/bge-small-en-v1.5')" \
    && chmod -R a+rX /opt/fastembed

RUN groupadd --system --gid 10001 copilot \
    && useradd --system --uid 10001 --gid 10001 --home /app copilot \
    && chown -R 10001:10001 /app
USER 10001

EXPOSE 8000

# copilot.api.app refuses to import unless COPILOT_API_TOKEN is set; the token
# comes from the copilot-env Secret in k8s/base/copilot.yaml, never from the image.
CMD ["uvicorn", "copilot.api.app:app", "--host", "0.0.0.0", "--port", "8000"]
