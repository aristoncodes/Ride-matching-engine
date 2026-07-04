# Postmortem / Correction of Errors (COE) — Template

> Copy this file to `docs/incidents/YYYY-MM-DD-<short-title>.md` for each incident.
> **Blameless:** focus on systems and processes, never on individuals. The goal is that this class of failure cannot recur, not that someone is at fault. (Amazon calls this a COE; Google calls it a postmortem — same intent.)

---

## Summary
*One paragraph: what happened, when, and the customer impact — readable by a non-engineer.*

## Impact
- **Duration:** start → detection → mitigation → resolution (with timestamps).
- **Scope:** which tenants / what fraction of traffic.
- **SLA/SLO breached?** which one, by how much.
- **Requests lost?** (if >0, this is the most important line in the doc.)

## Timeline (UTC)
| Time | Event |
|------|-------|
| HH:MM | trigger (e.g., deploy X shipped) |
| HH:MM | first symptom |
| HH:MM | alert fired / detected |
| HH:MM | mitigation started |
| HH:MM | service restored |

## Detection
- How was it detected — alert, dashboard, or a customer report?
- If a customer beat our monitoring, **that gap is itself an action item.**

## Root cause (the 5 Whys)
1. Why did the impact happen? →
2. Why? →
3. Why? →
4. Why? →
5. Why? → *(the systemic root cause)*

## What went well / what went poorly
- Went well:
- Went poorly:
- Where we got lucky: *(luck is not a control — turn it into one)*

## Action items
| # | Action | Type (prevent/detect/mitigate) | Owner | Due |
|---|--------|-------------------------------|-------|-----|
| 1 | | | | |
| 2 | | | | |

*Every Sev-1 must produce at least one **prevention** action, not only faster detection.*

## Lessons / follow-ups
- New test added to lock in the fix? (regression anchor)
- Runbook / alert / dashboard updated?
- Any ADR that should be written or revised as a result?
