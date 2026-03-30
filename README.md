# pgcopilot

**A Universal AI Agent for PostgreSQL Observability — Powered by pgwatch v5, LLM Tool Calling, and the Model Context Protocol**

pgcopilot is an agentic AI system that connects to [pgwatch](https://github.com/cybertec-postgresql/pgwatch) v5, gives a large language model direct access to time-series performance data and live database state through sandboxed, read-only tools, and produces structured Root Cause Analysis for database incidents.

It operates as a **standalone CLI** (`ask`, `watch`) and as a **Universal Agent** via the Model Context Protocol (`mcp`), exposing the same secure toolset to external AI clients like Cursor and Claude Desktop over `stdio`.

Built as a **Google Summer of Code 2026** proposal for the pgwatch project.

---

## Table of Contents

- [Project Overview and Vision](#project-overview-and-vision)
- [Core Architecture](#core-architecture)
- [Key Engineering Pillars](#key-engineering-pillars)
  - [Multi-Tenant Data Isolation](#1-multi-tenant-data-isolation)
  - [Dual-DB Routing and Live Diagnostics](#2-dual-db-routing-and-live-diagnostics)
  - [The Security Sandbox](#3-the-security-sandbox)
  - [Dynamic Server-Side Baselining](#4-dynamic-server-side-baselining)
  - [Epistemic Humility](#5-epistemic-humility)
- [Tri-Mode Execution](#tri-mode-execution)
  - [Reactive Mode (ask)](#reactive-mode-ask)
  - [Proactive Mode (watch)](#proactive-mode-watch)
  - [Universal Agent Mode (mcp)](#universal-agent-mode-mcp)
- [End-to-End Stress Testing](#end-to-end-stress-testing)
- [Available Tools](#available-tools)
- [Getting Started](#getting-started)
- [Project Structure](#project-structure)
- [License](#license)

---

## Project Overview and Vision

Modern PostgreSQL deployments generate vast amounts of observability data — connection counts, transaction throughput, cache hit ratios, WAL activity, lock contention, and more. **pgwatch v5** excels at collecting and storing this data, but the final step — **interpreting it** — still falls on the DBA.

pgcopilot closes that gap. It implements an **agentic loop** where an LLM can:

1. **Receive** a natural-language question about database health.
2. **Decide** which metrics to fetch by emitting structured tool calls.
3. **Execute** those tool calls against pgwatch databases through a read-only security sandbox.
4. **Analyze** the returned data (dynamic baselines, percentage deviations, live query states, lock contention).
5. **Respond** with a structured Root Cause Analysis grounded in real data — including a confidence score and a declaration of missing context.

This is not a chatbot wrapper. The LLM never sees raw SQL or has direct database access. Every interaction is mediated by typed Go tools with strict permission controls, tenant isolation, and a compiled security boundary.

---

## Core Architecture

```mermaid
graph TB
    subgraph CLI["CLI Layer (Cobra/Viper)"]
        ASK["ask<br/><i>reactive single-shot</i>"]
        WATCH["watch<br/><i>proactive daemon</i>"]
        MCP["mcp<br/><i>universal agent stdio</i>"]
    end

    subgraph AGENT["Agentic Loop (max 10 iterations)"]
        SYSPROMPT["System Prompt<br/><i>4-part Epistemic Structure</i>"]
        LLM["LLM Provider<br/><i>Groq / Gemini</i>"]
        TOOLCALLS["Tool Call<br/>Dispatcher"]
        SANDBOX["Sandbox<br/><i>ModeReadOnly</i>"]
    end

    subgraph TOOLS["Read-Only Tool Layer"]
        T1["get_metric_trends<br/><i>Sink DB</i>"]
        T2["evaluate_hypothetical_index<br/><i>Live DB via HypoPG</i>"]
        T3["get_active_queries<br/><i>Live DB pg_stat_activity</i>"]
        T4["get_active_locks<br/><i>Live DB pg_locks</i>"]
    end

    subgraph DBS["Database Layer"]
        METRICSDB[("pgwatch_metrics<br/><i>Sink DB — JSONB time-series</i>")]
        CONFIGDB[("pgwatch<br/><i>Config DB — pgwatch.source</i>")]
        LIVEDB[("Monitored DB<br/><i>Temporary pgxpool per call</i>")]
    end

    EXT["External AI Clients<br/><i>Cursor, Claude Desktop</i>"]

    EXT -->|stdio| MCP
    ASK --> AGENT
    WATCH --> AGENT
    MCP --> AGENT

    SYSPROMPT --> LLM
    LLM --> TOOLCALLS
    TOOLCALLS --> SANDBOX
    SANDBOX --> TOOLS

    T1 --> METRICSDB
    T2 --> CONFIGDB
    T2 -.->|DSN resolution| LIVEDB
    T3 --> CONFIGDB
    T3 -.->|DSN resolution| LIVEDB
    T4 --> CONFIGDB
    T4 -.->|DSN resolution| LIVEDB
```

**Data flow:** The CLI command (`ask`, `watch`, or `mcp`) initializes the Agent with a system prompt, an LLM provider, a sandbox, and a set of tools. The Agent sends the conversation to the LLM. When the LLM emits tool calls, each one passes through the sandbox's permission gate before execution. Tools either query the pgwatch metrics sink (for historical data) or resolve a live connection string from the config DB and open a temporary pool to the monitored instance (for real-time diagnostics). Results flow back as `RoleTool` messages, and the loop repeats until the LLM produces a final text response or the 10-iteration ceiling is hit.

### Provider-Agnostic LLM Layer

The `internal/provider` package defines a vendor-neutral interface:

```go
type Provider interface {
    Complete(ctx context.Context, req *Request) (*Response, error)
    ModelID() string
}
```

Providers register themselves via a thread-safe factory registry using Go's `init()` pattern. Switching LLM backends is a one-line change — swap the blank import and the `provider.New()` call.

| Provider | Backend | Default Model | SDK |
|----------|---------|---------------|-----|
| `groq` | Groq Inference API | `llama-3.3-70b-versatile` | `github.com/sashabaranov/go-openai` |
| `gemini` | Google Gemini API | `gemini-2.0-flash` | `google.golang.org/genai` |

The Groq provider points the OpenAI-compatible SDK at `https://api.groq.com/openai/v1`, making it trivial to add any OpenAI-compatible endpoint (Together AI, Fireworks, local vLLM) by changing a single base URL.

---

## Key Engineering Pillars

### 1. Multi-Tenant Data Isolation

pgwatch v5 stores time-series data in `pgwatch_metrics` (the Sink DB). Multiple PostgreSQL clusters write to the same database. To prevent cross-cluster data leaks, every query against metric tables is scoped by two mandatory dimensions:

```sql
WHERE dbname          = $1           -- monitored database name
  AND data->>'sys_id' = $2           -- pgwatch system identifier (unique per cluster)
```

The `sys_id` is extracted from inside the JSONB `data` column. Both values are **always** bound as parameterized values via `$1`/`$2` — never interpolated.

**SQL Injection Prevention:** Because PostgreSQL does not allow parameterized table names (`$1` cannot appear in a `FROM` clause), metric table names must be interpolated into the SQL string. Every identifier is validated against a strict allowlist regex before interpolation:

```go
var safeIdentifier = regexp.MustCompile(`^[a-zA-Z0-9_]+$`)
```

Any identifier containing dots, dashes, spaces, quotes, semicolons, or any character that could escape the identifier context is rejected before the query is ever built.

### 2. Dual-DB Routing and Live Diagnostics

Tools that need real-time data from a monitored database implement Dual-DB Routing:

```
 1. LLM calls get_active_queries(db_name="production")
                        │
                        ▼
 2. Tool queries pgwatch Config DB:
    SELECT connstr FROM pgwatch.source
    WHERE name = $1 AND is_enabled = true LIMIT 1
                        │
                        ▼
 3. DSN resolved → postgres://user:pass@host:5432/production
                        │
                        ▼
 4. Tool opens TEMPORARY pgxpool connection (defer pool.Close())
                        │
                        ▼
 5. Executes pre-compiled read-only query
    (pg_stat_activity, pg_locks, HypoPG EXPLAIN)
                        │
                        ▼
 6. Returns formatted string to LLM. Pool is closed.
```

Security properties:
- SQL queries are **compiled Go constants** — the LLM cannot modify, inject into, or extend them.
- Connections to live databases are **temporary** — opened per-invocation and immediately closed via `defer pool.Close()`.
- The `db_name` parameter is bound via `$1` in the Config DB lookup — SQL injection is impossible.
- All errors (connection failures, missing sources, disabled databases) are returned as **tool output strings**, not Go panics, so the agent loop remains stable.

### 3. The Security Sandbox

The `internal/sandbox` package is the single security chokepoint between every LLM tool-call decision and real side-effects.

Every tool declares a permission level:

| Level | Value | Meaning |
|-------|-------|---------|
| `PermissionReadOnly` | 0 | Reads data only — no mutations |
| `PermissionWrite` | 1 | Mutates state (e.g., `CREATE INDEX`) |
| `PermissionDangerous` | 2 | Irreversible operations (e.g., `DROP TABLE`) |

The sandbox enforces a `Mode` that governs what permission levels are allowed:

- **`ModeReadOnly`** (current default): Any tool with `Permission() > PermissionReadOnly` is **unconditionally blocked** before `Execute()` is ever called. The LLM receives a descriptive error message explaining why the call was denied.
- **`ModeConfirm`** (planned): Will prompt the operator for interactive approval before allowing write/dangerous operations.

This design ensures that **even if an LLM hallucinates a tool call with mutating intent, the Go runtime physically prevents execution**. The security boundary is enforced in compiled Go code, not in prompt engineering. This applies equally to the `ask`, `watch`, and `mcp` code paths.

### 4. Dynamic Server-Side Baselining

Raw time-series rows are never sent to the LLM. Go tools fetch data and compute math server-side, returning deterministic summary strings.

`FetchMetricBaseline` computes two averages in a single SQL query using PostgreSQL's `FILTER` clause:

```sql
SELECT
    COALESCE(AVG((data->>'field')::numeric)
        FILTER (WHERE time >= NOW() - INTERVAL '1 hour'),  0)  AS current_avg,
    COALESCE(AVG((data->>'field')::numeric),                0)  AS baseline_avg
FROM   public.<metric>
WHERE  dbname          = $1
  AND  data->>'sys_id' = $2
  AND  time           >= NOW() - INTERVAL '24 hours'
```

Advantages over sending raw data to the LLM:
1. **Efficiency**: Only two scalar values cross the network, not thousands of JSONB rows.
2. **Accuracy**: The 24-hour and 1-hour windows are exact, computed by PostgreSQL's timestamp arithmetic.
3. **Single scan**: The `FILTER` clause extracts both averages from the same set of rows in one index scan.
4. **No math hallucinations**: The LLM sees `"deviation of +57.91%"`, not a table of numbers it could misinterpret.

### 5. Epistemic Humility

The system prompt enforces a strict 4-part Markdown structure for every LLM response:

```
**1. Evidence:** [Hard data and metric numbers from tools]
**2. Likely Root Cause:** [Diagnosis grounded in evidence]
**3. Confidence Score:** [0-100% — quantified uncertainty]
**4. Missing Context:** [What data is unavailable, e.g., app logs, disk IO]
```

When the LLM has no data (e.g., metric tables don't exist), it reports **0% confidence** and explicitly lists what it cannot see. A monitoring system that admits uncertainty is more trustworthy than one that fabricates diagnoses.

---

## Tri-Mode Execution

### Reactive Mode (`ask`)

A DBA pastes a question and gets an immediate, data-backed analysis:

```bash
pgcopilot ask "Analyze the xact_commit trends for tenant3_db (sys_id 7622686022115713051)."
```

The Agent sends the prompt to the LLM, which autonomously decides which tools to call (and how many times), gathers evidence, and returns a structured diagnosis. The entire pipeline — tool calling, sandbox enforcement, DB queries — completes in seconds.

### Proactive Mode (`watch`)

A long-running daemon that wakes up on a configurable `time.Ticker`, performs a **pre-LLM threshold check**, and only dispatches the LLM when a real anomaly is detected:

```bash
pgcopilot watch \
  --sys-id 7622686022115713051 \
  --dbname tenant3_db \
  --interval 5m \
  --watch-metric db_stats \
  --watch-field xact_commit \
  --critical-threshold 50.0 \
  --webhook-url https://hooks.slack.com/services/T.../B.../xxx
```

**Pre-LLM Threshold Gate:** Before every cycle, the daemon calls `FetchMetricBaseline` directly in Go. If the deviation is below `--critical-threshold`, the cycle is skipped entirely — zero LLM tokens consumed. This prevents alert fatigue and reduces API costs during normal operation.

**Edge cases handled:**
- `baselineAvg == 0 && currentAvg > 0`: Treated as a spike from zero — forces LLM analysis regardless of threshold.
- `baselineAvg == 0 && currentAvg == 0`: No activity — skipped.
- `FetchMetricBaseline` error: Gate is bypassed and LLM runs anyway, so real incidents are never silently dropped.

**Webhook Alerting:** When the LLM response indicates an anomaly (does not contain `STATUS: OK`), a Slack-compatible `{"text": "..."}` JSON payload is POSTed to the webhook URL with a 5-second timeout. Non-responsive endpoints never block the ticker loop.

Each tick creates a **fresh Agent** with a clean conversation history, ensuring context from a previous cycle never leaks into the next one. The daemon handles `SIGINT`/`SIGTERM` for graceful shutdown.

### Universal Agent Mode (`mcp`)

An MCP server over `stdio` that exposes the same tools to external AI clients:

```bash
pgcopilot mcp
```

The `mcp` command implements the [Model Context Protocol](https://modelcontextprotocol.io) using `mark3labs/mcp-go`. Every tool implements the same `tool.Tool` Go interface, so the MCP command reuses the exact same tools, sandbox, and security boundary as `ask` and `watch`. No LLM API key is required — the external client (Cursor, Claude Desktop) provides its own model.

**Cursor MCP Configuration:**

```json
{
  "mcpServers": {
    "pgcopilot": {
      "command": "pgcopilot",
      "args": ["mcp"]
    }
  }
}
```

| Scenario | Command | Client |
|----------|---------|--------|
| DBA investigates an incident from terminal | `pgcopilot ask "..."` | Human operator |
| 24/7 server monitoring with Slack alerts | `pgcopilot watch --webhook-url ...` | Autonomous daemon |
| Developer queries DB health from their IDE | `pgcopilot mcp` | Cursor, Claude Desktop |

All three scenarios execute through the same sandbox, the same tools, and the same tenant isolation.

---

## End-to-End Stress Testing

To validate the full pipeline under realistic multi-tenant conditions, we executed a comprehensive stress test against a live pgwatch-monitored PostgreSQL cluster.

### Environment Setup

A Go script (`cmd/setup_env/main.go`) programmatically provisioned the test environment:

1. **Created 5 tenant databases** (`tenant1_db` through `tenant5_db`) via superuser connection.
2. **Enabled extensions** on each: `pg_stat_statements` and `hypopg` (v1.4.2 for PostgreSQL 18).
3. **Registered all 5** in `pgwatch.source` with `preset_config=basic` and `is_enabled=true`.

The script is fully idempotent — uses `CREATE DATABASE` with `42P04` error handling and `ON CONFLICT (name) DO NOTHING` for source registration.

![Go Setup Script Output](./assets/setup-script-output.png)

All 5 databases created, extensions enabled, and sources registered in pgwatch — confirmed in the Web UI:

![pgwatch Web UI Tenants](./assets/web-ui-tenants.png)

### Load Generation

We initialized `pgbench` tables across all 5 databases, then fired **100 concurrent clients** (20 per database) for 60 seconds:

```bash
# Inside the postgres Docker container
for i in 1 2 3 4 5; do
  pgbench -c 20 -j 2 -T 60 -U postgres "tenant${i}_db" &
done
wait
```

![100-Client pgbench Stress Test](./assets/pgbench-stress-test.png)

| Metric | Result |
|--------|--------|
| PostgreSQL version | 18.3 (Debian) |
| Concurrent clients | 100 (20 per database) |
| Duration | 60 seconds |
| Transactions per DB | ~33,000 |
| Sustained TPS per DB | ~550 |
| **Aggregate TPS** | **~2,750** |
| Failed transactions | 0 (0.000%) |
| `xact_commit` jump (tenant3_db) | 1,092 → 34,196 in 60 seconds |
| `numbackends` peak | 22 (20 pgbench + monitoring) |

### AI Diagnosis of the Spike

After a 90-second pgwatch scrape window, we ran pgcopilot in `ask` mode against `tenant3_db`:

```
**1. Evidence:** The 24-hour baseline average of 'xact_commit' is 4253.3711,
while the last 1-hour average is 6716.6000, showing a deviation of +57.91%.
**2. Likely Root Cause:** The significant increase in transaction commits
suggests a sudden surge in database activity...
**3. Confidence Score:** 80%
**4. Missing Context:** Application logs, OS-level disk IO metrics...
```

The LLM also evaluated a hypothetical index using HypoPG on the live `tenant3_db` database:

```
Cost before: 2950.00. Cost after: 17.24. Improvement: 99.42% (improvement).
Index statement: CREATE INDEX ON pgbench_accounts(abalance)
```

![Ask Mode - TPS Spike Diagnosis](./assets/ask-tps-spike-rca.png)

![Ask Mode - HypoPG Evaluation](./assets/ask-hypopg-evaluation.png)

We also tested the `get_active_locks` tool by artificially creating lock contention — one transaction held an exclusive row lock while another blocked waiting for it. The LLM autonomously selected `get_active_locks` (not `get_active_queries`) and correctly identified the blocking PIDs and the contending `UPDATE` statements:

![Ask Mode - Active Locks Diagnosis](./assets/ask-active-locks.png)

### Proactive Daemon with Webhook Alerting

The `watch` daemon detected the deviation, dispatched LLM analysis, and fired webhooks:

```
Proactive Watch Mode started for sys-id 7622686022115713051 (db=tenant3_db) at interval 10s
Watching: db_stats.xact_commit | Critical threshold: 50.0%
Webhook alerts enabled → https://webhook.site/...

[DAEMON] db_stats.xact_commit deviation: 100.00% (current=0.00, baseline=4253.37)
[DAEMON] Deviation (100.0%) exceeds threshold (50.0%). Dispatching LLM analysis...
[DAEMON] webhook alert dispatched (HTTP 200)
```

![Watch Mode Daemon Output](./assets/watch-daemon-alert.png)

The webhook payload was received and verified on webhook.site:

![Webhook Site JSON Payload](./assets/webhook-site-payload.png)

### MCP Integration

The `mcp` command exposes all 4 tools over `stdio` for external AI clients. Verified with the MCP Inspector:

![MCP Inspector Web UI](./assets/mcp-inspector-ui.png)

---

## Available Tools

All tools are `PermissionReadOnly`. The LLM never writes to any database.

| Tool | Description | DB Target |
|------|-------------|-----------|
| `get_metric_trends` | Compares the last 1-hour average of a metric field against its 24-hour baseline. All math is server-side via PostgreSQL `FILTER` clauses. | Sink DB (`pgwatch_metrics`) |
| `evaluate_hypothetical_index` | Uses HypoPG to evaluate whether a proposed index would improve a query's execution plan without creating a real index. Returns before/after EXPLAIN costs. | Live DB via Dual-DB Routing |
| `get_active_queries` | Fetches the top 5 longest-running active queries from `pg_stat_activity` on the live monitored database. | Live DB via Dual-DB Routing |
| `get_active_locks` | Fetches the top 5 blocking lock relationships from `pg_locks` joined with `pg_stat_activity` on the live monitored database. | Live DB via Dual-DB Routing |

---

## Getting Started

### Prerequisites

- **Go 1.22+**
- **A running pgwatch v5 instance** with a PostgreSQL metrics sink
- **A Groq API key** (free at [console.groq.com](https://console.groq.com)) or a Gemini API key

### Configuration

Create a `.env` file in the project root:

```env
GROQ_API_KEY=gsk_your_api_key_here
PGWATCH_METRICS_DB_URL=postgres://pgwatch:password@127.0.0.1:5432/pgwatch_metrics?sslmode=disable
PGWATCH_DB_URL=postgres://pgwatch:password@127.0.0.1:5432/pgwatch?sslmode=disable

# Optional: enable webhook alerts for watch mode
PGCOPILOT_WEBHOOK_URL=https://hooks.slack.com/services/T.../B.../xxx
```

### Build and Run

```bash
# Build
go build -o pgcopilot ./cmd/pgcopilot

# Reactive mode — ask a question
pgcopilot ask "Analyze the db_stats.xact_commit trends for tenant3_db (sys_id 7622686022115713051)."

# Proactive mode — continuous monitoring with Slack alerts
pgcopilot watch \
  --sys-id 7622686022115713051 \
  --dbname tenant3_db \
  --interval 5m \
  --critical-threshold 50.0 \
  --webhook-url https://hooks.slack.com/services/T.../B.../xxx

# Universal Agent mode — expose tools to Cursor / Claude Desktop
pgcopilot mcp
```

### Using a Different LLM Provider

To switch to Gemini, update the blank import in `cmd/pgcopilot/ask.go`:

```go
_ "github.com/RafayKhattak/pgcopilot/internal/provider/gemini"
```

And change the provider initialization:

```go
llm, err := provider.New("gemini", viper.GetString("GEMINI_API_KEY"), "gemini-2.0-flash")
```

No other code changes are required.

---

## Project Structure

```
pgcopilot/
├── cmd/
│   ├── pgcopilot/
│   │   ├── main.go              # Entry point, loads .env
│   │   ├── root.go              # Cobra root command, Viper config
│   │   ├── ask.go               # Reactive single-shot mode
│   │   ├── watch.go             # Proactive daemon + threshold gate + webhook alerting
│   │   └── mcp.go               # MCP server over stdio (Universal Agent)
│   ├── setup_env/
│   │   └── main.go              # Multi-tenant test environment provisioner
│   └── verify_load/
│       └── main.go              # pgbench load spike verifier
├── internal/
│   ├── agent/
│   │   └── agent.go             # Agentic loop (10-iteration cap)
│   ├── db/
│   │   ├── db.go                # pgxpool client wrapper
│   │   ├── query.go             # FetchMetricData + FetchMetricBaseline
│   │   └── metadata.go          # GetMonitoredDBs, GetMonitoredDBConnStr
│   ├── provider/
│   │   ├── provider.go          # Provider interface & shared types
│   │   ├── registry.go          # Thread-safe factory registry
│   │   ├── gemini/
│   │   │   └── gemini.go        # Google Gemini implementation
│   │   └── groq/
│   │       └── groq.go          # Groq/OpenAI-compatible implementation
│   ├── sandbox/
│   │   └── sandbox.go           # Permission enforcement gate
│   └── tool/
│       ├── tool.go              # Tool interface & permission types
│       ├── metrics/
│       │   ├── trends.go        # get_metric_trends (dynamic baselining)
│       │   └── hypopg.go        # evaluate_hypothetical_index (Dual-DB Routing)
│       └── diagnostics/
│           ├── active_queries.go # get_active_queries (Dual-DB Routing)
│           └── active_locks.go   # get_active_locks (Dual-DB Routing)
├── .env                         # API keys and DB URLs (not committed)
├── go.mod
└── go.sum
```

---

## License

This project is developed as part of a GSoC 2026 proposal for the [pgwatch](https://github.com/cybertec-postgresql/pgwatch) project.
