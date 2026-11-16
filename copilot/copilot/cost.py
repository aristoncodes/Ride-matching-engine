"""Model price table and cost arithmetic (ticket 3.2).

PLACEHOLDER PRICES: verify against the current Vertex price list before quoting them anywhere.
Edit the constants below; nothing else in the code holds a price.
"""

from __future__ import annotations

# USD per one million tokens: (input, output). Placeholder values, see the note above.
PRICES_USD_PER_M: dict[str, tuple[float, float]] = {
    "gemini-2.5-flash": (0.30, 2.50),
    "gemini-2.5-flash-lite": (0.10, 0.40),
    "gemini-2.5-pro": (1.25, 10.00),
}
# Used for a model id not in the table, so cost is never silently zero.
FALLBACK_MODEL = "gemini-2.5-flash"


def cost_usd(model: str, tokens_in: int, tokens_out: int) -> float:
    in_price, out_price = PRICES_USD_PER_M.get(model) or PRICES_USD_PER_M[FALLBACK_MODEL]
    return tokens_in * in_price / 1e6 + tokens_out * out_price / 1e6
