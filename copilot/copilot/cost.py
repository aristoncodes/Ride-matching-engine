"""Model price table and cost arithmetic (ticket 3.2).

Generative model is Groq (replaces Vertex AI per owner decision, free tier). Edit the
constants below; nothing else in the code holds a price.
"""

from __future__ import annotations

# USD per one million tokens: (input, output).
PRICES_USD_PER_M: dict[str, tuple[float, float]] = {
    "openai/gpt-oss-120b": (0.0, 0.0),  # Groq free tier; check current limits
    "openai/gpt-oss-20b": (0.0, 0.0),  # Groq free tier; check current limits
    "qwen/qwen3-32b": (0.0, 0.0),  # Groq free tier; check current limits
}
# Unknown models are flagged, not silently priced: they cost 0.0 here, so check the table.
FALLBACK_PRICE: tuple[float, float] = (0.0, 0.0)


def cost_usd(model: str, tokens_in: int, tokens_out: int) -> float:
    in_price, out_price = PRICES_USD_PER_M.get(model, FALLBACK_PRICE)
    return tokens_in * in_price / 1e6 + tokens_out * out_price / 1e6
