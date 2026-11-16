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
COPY pyproject.toml ./
RUN uv sync --no-dev --no-install-project

COPY copilot ./copilot
RUN uv sync --no-dev

# Run as a non-root user.
RUN groupadd --system copilot && useradd --system --gid copilot --home /app copilot \
    && chown -R copilot:copilot /app
USER copilot

EXPOSE 8000

# copilot.api.app refuses to import unless COPILOT_API_TOKEN is set; the token
# comes from the copilot-env Secret in k8s/base/copilot.yaml, never from the image.
CMD ["uvicorn", "copilot.api.app:app", "--host", "0.0.0.0", "--port", "8000"]
