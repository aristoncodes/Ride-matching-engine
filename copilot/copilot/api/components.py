"""Real agent components built from the environment, shared by the API and the eval harness.

Importing this module has no side effects (unlike copilot.api.app, which builds the app).
"""

from __future__ import annotations

import logging
import os
from dataclasses import dataclass
from typing import Any

DEFAULT_PROMETHEUS_URL = "http://prometheus:9090"


@dataclass
class AgentComponents:
    pool_ro: Any
    prom_client: Any
    prom_base_url: str
    embedder: Any
    guarded_model: Any


async def build_agent_components() -> AgentComponents:
    """The read-only pool, Prometheus client, embedder and guarded model, from env."""
    import httpx

    from copilot import db
    from copilot.agents.groq import GroqClient
    from copilot.agents.model import GuardedModel, RedisCache
    from copilot.embed import LocalEmbedder

    if not os.environ.get("GROQ_API_KEY", "").strip():
        logging.getLogger(__name__).warning(
            "GROQ_API_KEY is not set; /diagnose will use the fallback response"
        )

    return AgentComponents(
        pool_ro=await db.connect(os.environ["COPILOT_RO_DSN"]),
        prom_client=httpx.AsyncClient(timeout=10.0),
        prom_base_url=os.environ.get("PROMETHEUS_URL", DEFAULT_PROMETHEUS_URL),
        embedder=LocalEmbedder(),
        guarded_model=GuardedModel(
            GroqClient(),
            cache=RedisCache(),
            timeout_s=float(os.environ.get("MODEL_CALL_TIMEOUT_S", "20")),
        ),
    )
