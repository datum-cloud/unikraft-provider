---
handoff:
  id: feat-001
  from: user
  to: product-discovery
  created: 2026-03-31T00:00:00Z
  context_summary: "Serverless functions platform on Datum using Unikraft VMs — point at a hosted repo, platform builds and deploys ephemeral or long-lived functions running in Unikraft-powered microVMs"
  decisions_made: []
  open_questions:
    - "What problem does this solve?"
    - "Who are the target users?"
    - "How does this integrate with the existing Envoy-based proxy?"
    - "What runtime languages will be supported in MVP?"
    - "What are the scaling and cold-start requirements?"
  assumptions: []
---

# Feature Request: Unikraft Functions — Serverless Edge Compute

**Requested by**: User
**Date**: 2026-03-31

## Initial Description

Datum has a proxy powered by Envoy. The user wants to host workloads at Datum using Unikraft VMs. Developers should be able to point Datum at a hosted repository, and the platform will grab that code and spawn a function that can be either long-lived or ephemeral. The function runs in a Unikraft-powered VM on Datum infrastructure.

## Upstream Specification

Specification: https://raw.githubusercontent.com/datum-cloud/enhancements/refs/heads/feat/functions-enhancement/enhancements/compute/functions/README.md
PR: https://github.com/datum-cloud/enhancements/pull/625

### Key Design Points from Spec

**Summary**: Serverless compute at the edge with sub-50ms cold starts via Unikraft unikernels.

**Developer Experience**:
- Git-driven deployment (connect GitHub repo → push code → auto-build and deploy)
- Direct upload option
- Revision history with rollback
- CLI: `datum functions deploy`, `logs`, `revisions`, `rollback`, `invoke`

**Architecture**:
- `Function` resource references a Repository in the Registry Service
- Builds on top of existing `Workload` resource (generates Workload with serverless-optimized defaults)
- Function controller runs in Functions service control plane; creates Workloads in per-consumer namespaces
- Unikraft unikernels: 1–10MB images, 10–50ms cold starts via snapshot/restore, VM-level isolation

**Connectivity**:
- Public access via Gateway (Envoy) — TLS, auth, rate limiting
- Private access via Service Connect — direct connectivity within project

**Scaling**:
- Scale-to-zero when idle
- Automatic scale-up on traffic (target <100ms scale decision)

**MVP Constraints**:
- HTTP triggers only (cron/event triggers are future work)
- Automatic placement only (no region selection)
- Stateless workloads only
- Multi-language: Go, Node.js, Python, Rust

**Dependencies**:
- Registry (artifact storage and discovery)
- Workload (compute runtime)
- Gateway (HTTP routing / public exposure)
- Service Connect (private connectivity)

## Notes

This request is awaiting discovery. The product-discovery agent will:
- Clarify the problem being solved
- Identify target users
- Assess scope boundaries
- Evaluate platform capability requirements
- Produce a discovery brief
