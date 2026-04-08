# ufo

Unikraft Functions — serverless edge compute for Datum Cloud.

## Resuming work

This repo has an active feature in progress. At the start of any session, check memory for current state:

- Pipeline state: `.claude/pipeline/state/feat-001.json`
- Architecture doc: `.claude/pipeline/designs/feat-001-architecture.md`
- Discovery brief: `.claude/pipeline/briefs/feat-001-discovery-brief.md`

If the user says something like "continue", "pick up where we left off", or "what's next", read the pipeline state and memory, then present the remaining stages and ask which to tackle.

## Building

```
nix-shell -p go --run "go build ./..."
nix-shell -p go --run "go vet ./..."
nix-shell -p kubectl --run "kubectl kustomize config/base"
```

## Agents to use

| Task | Agent |
|------|-------|
| Go implementation | `datum-platform:api-dev` |
| Tests | `datum-platform:test-engineer` |
| Kustomize / CI / Dockerfiles | `datum-platform:sre` |
| Architecture decisions | `datum-platform:plan` |
| Code review | `datum-platform:code-reviewer` |
