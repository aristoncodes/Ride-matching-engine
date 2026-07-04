# Documentation Index

This folder holds the engineering documentation for the **B2B Geospatial Ride-Matching & Dynamic Pricing Engine**, structured the way enterprise teams (Google/Amazon-style) gate work before and during a build.

Read in roughly this order.

## 1. Why / What (product)
| Doc | Purpose |
|-----|---------|
| [PR_FAQ.md](PR_FAQ.md) | Amazon-style "working backwards" press release + FAQ — the customer-framed *why*. |
| [Product_Requirements.md](Product_Requirements.md) | PRD: personas, user stories, success metrics, scope & non-goals. |
| [Scope_and_Production_Comparison.md](Scope_and_Production_Comparison.md) | Honest boundary: what this build is vs. a production Ola/Uber backend, component by component. |
| [Extra_Additions.md](Extra_Additions.md) | Post-project idea backlog (e.g. ride-pooling / shared rides) to build after v1 is done. |

## 2. How (design)
| Doc | Purpose |
|-----|---------|
| [Technical_Design_Document.md](Technical_Design_Document.md) | The master TDD: architecture, tenets, 24-week plan, risks. |
| [Architecture.md](Architecture.md) | System + sequence diagrams; how a ride request flows end-to-end. |
| [Data_Model.md](Data_Model.md) | Canonical entities, fields, ID strategy, and lifecycle states. |
| [api/matching.proto](api/matching.proto) | gRPC contract between the Go bridge and the C++ engine. |
| [api/rest-openapi.yaml](api/rest-openapi.yaml) | Public REST API contract for rider/driver requests. |
| [adr/](adr/) | Architecture Decision Records — one file per significant decision. |

## 3. How we run it (operations)
| Doc | Purpose |
|-----|---------|
| [SLO_SLA.md](SLO_SLA.md) | Turns "sub-millisecond" and "zero dropped requests" into measurable targets. |
| [Threat_Model.md](Threat_Model.md) | Security & multi-tenancy isolation analysis. |
| [Test_Strategy.md](Test_Strategy.md) | Unit / integration / load / chaos testing approach and correctness anchors. |
| [Observability.md](Observability.md) | Metrics, logs, traces, and dashboards to ship *before* launch. |
| [Runbook.md](Runbook.md) | On-call operational procedures for common failures. |
| [Rollout_Rollback.md](Rollout_Rollback.md) | Deployment, canary, and rollback strategy. |
| [templates/postmortem-coe-template.md](templates/postmortem-coe-template.md) | Blameless incident write-up template (Amazon "COE" / Google postmortem). |

## Status legend
Throughout these docs: ✅ done · 🚧 in progress · ⬜ not started.

Current position: **Week 2 of 24 complete** (Coordinate generator + Quadtree). See the TDD for the full schedule.
