# Agent Architecture — Snowplow Cache Team

## Team Structure

```
                          ┌─────────────┐
                          │    Diego    │
                          │  (Product   │
                          │   Owner)    │
                          └──────┬──────┘
                                 │
                          direction, decisions,
                          approvals, priorities
                                 │
                          ┌──────▼──────┐
                          │  Claude Code │
                          │ (Orchestrator│
                          │  + Executor) │
                          └──────┬──────┘
                                 │
                 spawns agents via Agent tool
                 delegates research, analysis,
                 implementation to specialists
                                 │
              ┌──────────────────┼──────────────────┐
              │                  │                  │
     ┌────────▼────────┐ ┌──────▼──────┐ ┌────────▼────────┐
     │   Team Lead     │ │  Explore    │ │  Single-Role    │
     │ (cache-team-    │ │  Agent      │ │  Agents         │
     │  lead)          │ │ (codebase   │ │ (direct spawn)  │
     │                 │ │  search)    │ │                 │
     │ Orchestrates    │ │             │ │ cache-architect │
     │ 4 sub-agents    │ │ Fast file   │ │ cache-developer │
     │ for meetings    │ │ search,     │ │ cache-tester    │
     │ and plans       │ │ grep, read  │ │ cache-pm        │
     └────────┬────────┘ └─────────────┘ └─────────────────┘
              │
    spawns 4 specialists
    as sub-agents within
    its own context
              │
   ┌──────────┼──────────┬──────────────┐
   │          │          │              │
┌──▼───┐  ┌──▼───┐  ┌───▼──┐  ┌───────▼┐
│Archi-│  │Devel-│  │Test- │  │  PM    │
│tect  │  │oper  │  │er    │  │        │
└──────┘  └──────┘  └──────┘  └────────┘
```

## Agent Roles and Capabilities

### Orchestrator (Claude Code — main conversation)

- **Role**: Executes Diego's instructions, writes code, runs commands, deploys
- **Tools**: ALL (Bash, Read, Write, Edit, Grep, Glob, Agent, etc.)
- **Spawns**: Any agent type for delegation
- **Key rule**: Never duplicates work an agent is doing

### cache-team-lead

- **Role**: Orchestrates the 4-agent team for meetings, plans, reviews
- **When used**: "reunite the team", "team meeting", "team analysis"
- **Spawns internally**: architect, developer, tester, PM sub-agents
- **Output**: Synthesized plan from all 4 perspectives
- **Tools**: ALL

### cache-architect

- **Role**: Architecture analysis, bottleneck identification, design reviews
- **When used**: Design decisions, trace analysis, code structure reviews
- **Expertise**: 3-tier cache (L3/L1/Redis), K8s informers, singleflight, RBAC
- **Output**: Architecture assessments, risk analysis, design docs
- **Tools**: ALL

### cache-developer

- **Role**: Go code implementation, bug fixes, builds, releases
- **When used**: Code changes, dead code cleanup, refactoring
- **Can run in**: Isolated worktree (parallel development without conflicts)
- **Output**: Committed code, build verification
- **Tools**: ALL

### cache-tester

- **Role**: QA, test execution, regression detection, measurement
- **When used**: Stress tests, browser tests, validation, comparison
- **Expertise**: Playwright, curl benchmarks, OTel trace analysis
- **Output**: Test reports with PASS/FAIL verdicts
- **Tools**: ALL

### cache-pm

- **Role**: Product metrics, customer requirements, milestone tracking
- **When used**: North-star evaluation, shipping decisions, priority calls
- **Output**: Metrics tables, milestone status, risk registers
- **Tools**: ALL

### Explore Agent

- **Role**: Fast codebase exploration and search
- **When used**: "find where X is defined", "search for pattern Y"
- **Output**: File paths, code snippets, search results
- **Tools**: Read-only (Glob, Grep, Read, WebFetch, WebSearch)

## Interaction Patterns

### Pattern 1: Direct Execution (most common)
```
Diego → Claude Code → executes directly (code, deploy, test)
```
Used for: quick fixes, single-file edits, curl tests, helm upgrades

### Pattern 2: Single Agent Delegation
```
Diego → Claude Code → spawns cache-architect → receives analysis
                    → Claude Code acts on findings
```
Used for: design docs, code audits, architecture reviews

### Pattern 3: Parallel Agents
```
Diego → Claude Code → spawns cache-developer (worktree, background)
                    → spawns stress test (Bash, background)
                    → Claude Code monitors both
```
Used for: code cleanup + test running simultaneously

### Pattern 4: Team Meeting
```
Diego → Claude Code → spawns cache-team-lead
                         → spawns architect (sub-agent)
                         → spawns developer (sub-agent)
                         → spawns tester (sub-agent)
                         → spawns PM (sub-agent)
                         → synthesizes into unified plan
                    → Claude Code reports to Diego
```
Used for: plan creation, stress test analysis, milestone reviews

### Pattern 5: War Room (live monitoring)
```
Diego → Claude Code → spawns stress test (background)
                    → periodically checks progress (Bash)
                    → spawns team-lead for analysis when data arrives
                    → fixes bugs found during test (direct execution)
                    → re-deploys and re-tests
```
Used for: Phase 6+7 stress test sessions

## Communication Rules

1. **Agents cannot talk to each other directly** — all communication goes through Claude Code or the team-lead orchestrator
2. **Agents share context via files** — docs/, memory files, test logs, Redis state
3. **Background agents notify on completion** — Claude Code receives task notifications
4. **Worktree agents get isolated git state** — changes merged by Claude Code after review
5. **Diego's decisions override all agents** — agents propose, Diego decides
6. **Zero trust between agents** — each agent verifies data independently, never trusts another agent's claims without checking

## File-Based Shared Context

```
docs/
├── scaling-roadmap.md          ← original plan (A/B/C/D phases)
├── updated-plan.md             ← current source of truth (team output)
├── code-audit.md               ← 33 findings with file/line numbers
├── phase2-table-topn.md        ← archived (Phase 2 cancelled)
├── phase2-aggregate-simulation.md ← archived
└── agent-architecture.md       ← this file

memory/
├── project_phase1_complete.md  ← Phase 1 is final, no Phase 2
├── project_north_star.md       ← 1s cold / 500ms warm targets
├── project_cache_v0.25.176_stress_test.md ← latest test results
├── feedback_*.md               ← Diego's preferences and rules
└── MEMORY.md                   ← index of all memory files

/tmp/
├── warroom-run*.log            ← stress test output (ephemeral)
├── snowplow_test_results.json  ← structured test results
└── phase7_user_scaling_results.json ← user scaling data
```
