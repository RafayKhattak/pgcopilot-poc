# pgcopilot

**A Universal AI Agent for PostgreSQL Observability — Powered by pgwatch v5, LLM Tool Calling, and the Model Context Protocol**

pgcopilot is an agentic AI system that brings **autonomous database diagnostics** to PostgreSQL. It connects to [pgwatch](https://github.com/cybertec-postgresql/pgwatch) v5, gives a large language model direct access to time-series performance data and live database state through sandboxed, read-only tools, and produces expert-level Root Cause Analysis for database incidents.

It operates as a **standalone CLI** (`ask`, `watch`) and as a **Universal Agent** via the Model Context Protocol (`mcp`), exposing the same secure toolset to external AI clients like Cursor and Claude Desktop over `stdio`.

Built as a **Google Summer of Code 2026** proposal for the pgwatch project.

---

## Table of Contents

- [Why pgcopilot](#why-pgcopilot)
- [Core Architecture](#core-architecture)
  - [Provider-Agnostic LLM Layer](#provider-agnostic-llm-layer)
  - [Tri-Mode CLI](#tri-mode-cli)
  - [The Agentic Loop](#the-agentic-loop)
- [The Universal Agent: MCP Integration](#the-universal-agent-mcp-integration)
- [Deep Diagnostic Mode: Live DB Queries](#deep-diagnostic-mode-live-db-queries)
- [Production Alerting: Webhook Routing](#production-alerting-webhook-routing)
- [Security and Safety: The Sandbox](#security-and-safety-the-sandbox)
- [Multi-Tenant Data Layer](#multi-tenant-data-layer)
  - [SQL Injection Prevention](#sql-injection-prevention)
  - [Tenant Isolation via sys_id Scoping](#tenant-isolation-via-sys_id-scoping)
  - [Dynamic Baselining (Server-Side)](#dynamic-baselining-server-side)
- [Tools](#tools)
  - [get_metric_trends](#get_metric_trends)
  - [evaluate_hypothetical_index](#evaluate_hypothetical_index)
  - [get_active_queries](#get_active_queries)
- [Epistemic Humility: Structured LLM Output](#epistemic-humility-structured-llm-output)
- [End-to-End Stress Testing](#end-to-end-stress-testing)
- [Future Roadmap](#future-roadmap)
- [Getting Started](#getting-started)
- [Project Structure](#project-structure)
- [License](#license)

---

## Why pgcopilot

Modern PostgreSQL deployments generate vast amounts of observability data — connection counts, transaction throughput, cache hit ratios, WAL activity, lock contention, and more. **pgwatch v5** excels at collecting and storing this data, but the final step — **interpreting it** — still falls on the DBA.

pgcopilot closes that gap. It implements an **agentic loop** where an LLM can:

1. **Receive** a natural-language question about database health.
2. **Decide** which metrics to fetch by emitting structured tool calls.
3. **Execute** those tool calls against pgwatch databases through a read-only security sandbox.
4. **Analyze** the returned data (dynamic baselines, percentage deviations, live query states).
5. **Respond** with a structured Root Cause Analysis grounded in real data — including a confidence score and a declaration of missing context.

This is not a chatbot wrapper. The LLM never sees raw SQL or has direct database access. Every interaction is mediated by typed Go tools with strict permission controls, tenant isolation, and a compiled security boundary.

---

## Core Architecture

```
                              ┌──────────────────────────────┐
                              │    External AI Clients       │
                              │  (Cursor, Claude Desktop)    │
                              └──────────────┬───────────────┘
                                             │ stdio (MCP)
┌────────────────────────────────────────────┼───────────────────────────┐
│                        CLI (Cobra/Viper)   │                           │
│            ask (reactive) │ watch (daemon) │ mcp (universal agent)     │
└───────────────┬───────────────────┬────────┴──────────┬────────────────┘
                │                   │                   │
                ▼                   ▼                   ▼
┌───────────────────────────────────────────────────────────────────────┐
│                        Agent / MCP Handler                            │
│  ┌──────────┐   ┌──────────┐   ┌──────────┐   ┌───────────────────┐   │
│  │ System   │──▶│   LLM    │──▶│  Tool    │──▶│    Sandbox      │   │
│  │ Prompt   │   │ Provider │◀──│  Calls   │◀──│  (ModeReadOnly)  │   │
│  └──────────┘   └──────────┘   └──────────┘   └────────┬──────────┘   │
│                max 10 iterations                        │             │
└─────────────────────────────────────────────────────────┼─────────────┘
                                                          │
          ┌───────────────────────────┬───────────────────┤
          ▼                           ▼                   ▼
┌───────────────────┐   ┌──────────────────────┐   ┌──────────────────┐
│ get_metric_trends │   │ evaluate_hypothetical │   │ get_active_      │
│ (Sink DB)         │   │ _index (HypoPG)       │   │ queries (Live DB)│
└────────┬──────────┘   └──────────┬───────────┘   └────────┬─────────┘
         │                         │                        │
         ▼                         ▼                        ▼
┌───────────────────┐   ┌──────────────────────┐   ┌──────────────────┐
│  pgwatch_metrics  │   │   Target Database    │   │   Config DB      │
│  (JSONB, pgxpool) │   │   (EXPLAIN plans)    │   │   (pgwatch.source│
└───────────────────┘   └──────────────────────┘   │    → live DSN)   │
                                                   └──────────────────┘
                                                          │
                                                          ▼
                                                   ┌──────────────────┐
                                                   │ Monitored DB     │
                                                   │ (pg_stat_activity│
                                                   │  temporary pool) │
                                                   └──────────────────┘
```

### Provider-Agnostic LLM Layer

The `internal/provider` package defines a **vendor-neutral interface** that decouples the entire application from any single LLM provider:

```go
type Provider interface {
    Complete(ctx context.Context, req *Request) (*Response, error)
    ModelID() string
}
```

Providers register themselves via a **thread-safe factory registry** using Go's `init()` pattern. Switching LLM backends is a one-line change — swap the blank import and the `provider.New()` call.

**Currently implemented providers:**

| Provider | Backend | Default Model | SDK |
|----------|---------|---------------|-----|
| `gemini` | Google Gemini API | `gemini-2.0-flash` | `google.golang.org/genai` |
| `groq` | Groq Inference API | `llama-3.3-70b-versatile` | `github.com/sashabaranov/go-openai` |

The Groq provider points the OpenAI-compatible SDK at `https://api.groq.com/openai/v1`, making it trivial to add any OpenAI-compatible endpoint (Together AI, Fireworks, local vLLM, etc.) by changing a single base URL.

### Tri-Mode CLI

pgcopilot operates in three distinct modes via [Cobra](https://github.com/spf13/cobra) subcommands:

**Reactive Mode (`ask`)** — A DBA pastes a question and gets an immediate, data-backed analysis:

```bash
pgcopilot ask "What happened to TPS in my_test_db over the last 30 minutes?"
```

**Proactive Mode (`watch`)** — A long-running daemon that wakes up on a configurable `time.Ticker`, fetches metrics, dispatches webhook alerts on anomalies, and handles `SIGINT`/`SIGTERM` for graceful shutdown:

```bash
pgcopilot watch --sys-id 7618200670381232155 --dbname my_test_db --interval 5m \
  --webhook-url https://hooks.slack.com/services/T.../B.../xxx
```

**Universal Agent Mode (`mcp`)** — An MCP server over `stdio` that exposes the same tools to external AI clients:

```bash
pgcopilot mcp
```

Each `watch` tick creates a **fresh Agent** with a clean conversation history, ensuring context from a previous cycle never leaks into the next one.

### The Agentic Loop

The `internal/agent` package implements the core reasoning loop:

1. The user's prompt is appended to the conversation as a `RoleUser` message.
2. The full conversation (system prompt + history) and tool definitions are sent to the LLM.
3. If the LLM responds with **tool calls**, each one is executed through the sandbox and the result is appended as a `RoleTool` message. The loop repeats from step 2.
4. If the LLM responds with **plain text** (no tool calls), the loop terminates and the text is returned.
5. A **hard ceiling of 10 iterations** prevents infinite loops. If the model keeps calling tools without converging, the agent returns a descriptive error rather than running indefinitely.

This architecture means the LLM can make **multi-step decisions**: fetch one metric, realize it needs another, fetch that too, inspect live queries, and then synthesize a final answer — all within a single invocation.

---

## The Universal Agent: MCP Integration

pgcopilot is not just a standalone CLI. The `mcp` command implements the [Model Context Protocol](https://modelcontextprotocol.io) over `stdio`, transforming pgcopilot into a **universal tool server** that any MCP-compatible AI client can connect to.

### Why This Matters

Traditional monitoring tools force a choice: either you build a standalone agent, or you build IDE integrations. pgcopilot does both with a single codebase. Because every tool implements a strict `tool.Tool` Go interface, the MCP command reuses the **exact same tools, sandbox, and security boundary** as `ask` and `watch`.

### How It Works

```
┌─────────────────┐  stdio   ┌──────────────────────────────────┐
│   Cursor IDE    │◀───────▶│  pgcopilot mcp                   │
│   Claude Desktop│          │  ┌────────────┐  ┌─────────────┐ │
│   Any MCP Client│          │  │ MCP Server │──│  Sandbox    │ │
└─────────────────┘          │  │ (mark3labs)│  │ (ReadOnly)  │ │
                             │  └──────┬─────┘  └──────┬──────┘ │
                             │         │               │        │
                             │         ▼               ▼        │
                             │  ┌──────────────────────────┐    │
                             │  │ tool.Tool interface      │    │
                             │  │ get_metric_trends        │    │
                             │  │ evaluate_hypothetical_idx│    │
                             │  │ get_active_queries       │    │
                             │  └──────────────────────────┘    │
                             └──────────────────────────────────┘
```

The `convertToMCPTool` function unmarshals each tool's JSON Schema into the MCP `ToolInputSchema` format and propagates `ReadOnlyHint` annotations from the tool's `Permission()`. The `makeMCPHandler` bridges each MCP `CallToolRequest` through the sandbox — marshalling arguments to `json.RawMessage`, executing via `sandbox.Execute`, and returning results as `mcp.CallToolResult`.

### Deployment Scenarios

| Scenario | Command | Client |
|----------|---------|--------|
| DBA investigates an incident from terminal | `pgcopilot ask "..."` | Human operator |
| 24/7 server monitoring with Slack alerts | `pgcopilot watch --webhook-url ...` | Autonomous daemon |
| Developer queries DB health from their IDE | `pgcopilot mcp` | Cursor, Claude Desktop |

All three scenarios execute through the same sandbox, the same tools, and the same tenant isolation. Zero code duplication.

---

## Deep Diagnostic Mode: Live DB Queries

The `get_active_queries` tool implements **Dual-DB Routing** — the ability to query the *live monitored database* in real time, not just historical metrics.

### The Problem

When a database incident is active (lock contention, runaway queries, CPU spikes), historical metrics tell you *what* happened but not *what is happening right now*. You need live access to `pg_stat_activity` on the monitored database.

The naive approach — letting an LLM generate and execute raw SQL against a production database — is an unacceptable security risk. pgcopilot solves this with a pre-compiled, parameterless query behind a sandboxed tool.

### How Dual-DB Routing Works

```
 1. LLM calls get_active_queries(db_name="production")
                        │
                        ▼
 2. Tool queries pgwatch Config DB:
    SELECT connstr FROM pgwatch.source
    WHERE name = $1 AND is_enabled = true LIMIT 1
                        │
                        ▼
 3. DSN resolved: postgres://user:pass@prod-host:5432/production
                        │
                        ▼
 4. Tool opens TEMPORARY pgxpool connection (defer pool.Close())
                        │
                        ▼
 5. Executes pre-compiled read-only query:
    SELECT pid, state, extract(epoch FROM (now() - query_start)), query
    FROM pg_stat_activity
    WHERE state != 'idle' AND pid != pg_backend_pid()
    ORDER BY duration DESC LIMIT 5
                        │
                        ▼
 6. Returns formatted string to LLM. Pool is closed.
```

**Security properties:**

- The SQL query is a **compiled Go constant** — the LLM cannot modify it, inject into it, or extend it.
- The connection to the live database is **temporary** — opened per-invocation and immediately closed via `defer pool.Close()`.
- The `db_name` parameter is bound via `$1` in the Config DB lookup — SQL injection is impossible.
- All errors (connection failures, missing sources, disabled databases) are returned as **tool output strings**, never as Go panics, so the agent loop remains stable.
- The tool's `Permission()` is `PermissionReadOnly` — the sandbox permits it.

---

## Production Alerting: Webhook Routing

The `watch` daemon is a production-grade monitoring system, not just a terminal printer. When configured with `--webhook-url` (or the `PGCOPILOT_WEBHOOK_URL` environment variable), it acts as an **intelligent alert dispatcher**.

### Alert Flow

```
 Ticker fires (e.g., every 5m)
         │
         ▼
 Fresh Agent analyzes db_stats
         │
         ▼
 LLM returns structured analysis
         │
         ├── Contains "STATUS: OK"  →  No alert. Print to terminal only.
         │
         └── Anomaly detected       →  POST to webhook URL
                                        {"text": "<full LLM analysis>"}
                                        Content-Type: application/json
                                        Timeout: 5 seconds
```

### Key Design Decisions

- **Selective dispatching**: Only anomalies trigger webhooks. Normal `STATUS: OK` cycles are suppressed to avoid alert fatigue.
- **Slack-compatible payload**: The `{"text": "..."}` JSON format works with Slack Incoming Webhooks, PagerDuty, and any endpoint that accepts this standard format.
- **5-second timeout**: A non-responsive webhook endpoint never blocks the daemon's ticker loop. The `context.WithTimeout` ensures the next analysis cycle fires on schedule.
- **Graceful error handling**: Webhook failures are logged (`[DAEMON] webhook alert failed: ...`) but never propagate — the daemon survives transient network failures, DNS resolution failures, and non-2xx HTTP responses.
- **Viper binding**: The webhook URL can be set via CLI flag (`--webhook-url`), environment variable (`PGCOPILOT_WEBHOOK_URL`), or `.env` file — all three work interchangeably via Viper's `BindPFlag`.

---

## Security and Safety: The Sandbox

The `internal/sandbox` package is the **single security chokepoint** between every LLM tool-call decision and the real side-effects of executing that tool.

Every tool declares a permission level:

| Level | Value | Meaning |
|-------|-------|---------|
| `PermissionReadOnly` | 0 | Reads data only — no mutations |
| `PermissionWrite` | 1 | Mutates state (e.g., `CREATE INDEX`) |
| `PermissionDangerous` | 2 | Irreversible operations (e.g., `DROP TABLE`) |

The sandbox enforces a **Mode** that governs what permission levels are allowed:

- **`ModeReadOnly`** (current default): Any tool with `Permission() > PermissionReadOnly` is **unconditionally blocked** before `Execute()` is ever called. The LLM receives a descriptive error message explaining why the call was denied.
- **`ModeConfirm`** (future): Will prompt the operator for interactive approval before allowing write/dangerous operations.

This design ensures that **even if an LLM hallucinates a tool call with mutating intent, the Go runtime physically prevents execution**. The security boundary is enforced in compiled Go code, not in prompt engineering. This applies equally to the `ask`, `watch`, and `mcp` code paths — the MCP server routes every external tool call through the same sandbox.

---

## Multi-Tenant Data Layer

The `internal/db` package wraps `pgxpool` and implements the data access layer for pgwatch metrics. It enforces critical security properties at the SQL level.

### SQL Injection Prevention

pgwatch stores each metric in its own table (e.g., `public.db_stats`, `public.wal_stats`). Since **PostgreSQL does not allow parameterized table names** (`$1` cannot be used in `FROM` clauses), the metric name must be interpolated into the SQL string.

To prevent SQL injection, every metric name and JSONB field name is validated against a **strict allowlist regex** before interpolation:

```go
var safeIdentifier = regexp.MustCompile(`^[a-zA-Z0-9_]+$`)
```

Any identifier containing dots, dashes, spaces, quotes, semicolons, or any other character that could escape the identifier context is **rejected before the query is ever built**. Only identifiers are interpolated; all filter values use standard `$1`/`$2` parameterized binding.

### Tenant Isolation via sys_id Scoping

A single pgwatch metrics database may contain data from **many monitored PostgreSQL clusters**. To prevent cross-tenant data leaks, every query is scoped by two mandatory dimensions:

```sql
WHERE dbname          = $1           -- monitored database name
  AND data->>'sys_id' = $2           -- pgwatch system identifier (unique per cluster)
```

The `sys_id` is extracted from inside the JSONB `data` column. This ensures a caller can only see rows belonging to their own cluster, even when multiple clusters report the same database name. Both values are **always** bound as parameterized values.

### Dynamic Baselining (Server-Side)

The `FetchMetricBaseline` function computes **two server-side averages in a single SQL query** using PostgreSQL's `FILTER` clause:

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

This design has three advantages over the previous Go-side half-interval approach:

1. **Efficiency**: Only two scalar values cross the network, not thousands of JSONB rows.
2. **Accuracy**: The 24-hour and 1-hour windows are exact, computed by PostgreSQL's timestamp arithmetic.
3. **Single scan**: The `FILTER` clause extracts both averages from the same set of rows in one index scan.

---

## Tools

All tools are `PermissionReadOnly`. The LLM never writes to any database.

### `get_metric_trends`

Compares the last 1-hour average of a metric field against its 24-hour baseline to detect deviations. All math is performed server-side in PostgreSQL.

**Parameters:**

| Name | Type | Description |
|------|------|-------------|
| `metric_name` | string | The pgwatch metric table (e.g., `db_stats`, `wal_stats`) |
| `sys_id` | string | The pgwatch system identifier for the target cluster |
| `db_name` | string | The monitored database name |
| `field_name` | string | The numeric JSONB field to analyse (e.g., `tps`, `blks_hit`) |

**Example tool output:**
```
Analyzed db_stats.tps. The 24-hour baseline average is 412.3500.
The last 1-hour average is 891.7200.
This represents a deviation of +116.22%.
```

### `evaluate_hypothetical_index`

Uses [HypoPG](https://hypopg.readthedocs.io/) to evaluate whether a proposed index would improve a query's execution plan **without creating a real index**.

**Parameters:**

| Name | Type | Description |
|------|------|-------------|
| `query` | string | The SQL SELECT query to test |
| `index_statement` | string | The `CREATE INDEX` statement to evaluate |

**Execution flow:**

1. Acquires a **single connection** from the pool (HypoPG indexes exist only in the creating backend's memory).
2. Runs `EXPLAIN` on the query and parses the **total cost** from the top-level plan node.
3. Creates the hypothetical index via `SELECT * FROM hypopg_create_index($1)`.
4. Runs `EXPLAIN` again with the hypothetical index visible to the planner.
5. Calls `hypopg_reset()` to clean up.
6. Returns the before/after costs and the percentage improvement.

### `get_active_queries`

Fetches the top 5 longest-running active queries from a live monitored database using Dual-DB Routing. See [Deep Diagnostic Mode](#deep-diagnostic-mode-live-db-queries) for the full architecture.

**Parameters:**

| Name | Type | Description |
|------|------|-------------|
| `db_name` | string | The name of the monitored database as registered in pgwatch |

**Example tool output:**
```
Live Active Queries for production:
  PID: 12345, State: active, Duration: 45.2s, Query: SELECT * FROM orders WHERE customer_id = ...
  PID: 12389, State: active, Duration: 12.8s, Query: UPDATE inventory SET quantity = quantity - ...
```

---

## Epistemic Humility: Structured LLM Output

The system prompt enforces a strict 4-part Markdown structure for every LLM response, preventing overconfident or vague diagnoses:

```
**1. Evidence:** [Hard data and metric numbers from tools]
**2. Likely Root Cause:** [Diagnosis grounded in evidence]
**3. Confidence Score:** [0-100% — quantified uncertainty]
**4. Missing Context:** [What data is unavailable, e.g., app logs, disk IO]
```

When the LLM has no data (e.g., metric tables don't exist), it reports **0% confidence** and explicitly lists what it cannot see. This is a deliberate design choice: a monitoring system that admits uncertainty is more trustworthy than one that fabricates diagnoses.

---

## End-to-End Stress Testing

To validate the full pipeline under realistic conditions, we performed a **load generation test** using `pgbench` against a live pgwatch-monitored PostgreSQL cluster.

### Load Generation

```bash
pgbench -c 50 -j 2 -T 60 -U postgres test_db
```

| Metric | Result |
|--------|--------|
| Concurrent clients | 50 |
| Duration | 60 seconds |
| Transactions processed | **48,637** |
| Sustained TPS | **810.88** |
| Failed transactions | 0 (0.000%) |
| Average latency | 61.66 ms |

### Metrics Verification

After a 60-second scrape window for pgwatch to ingest the data, we verified the spike was recorded in `pgwatch_metrics`:

| Metric | Value |
|--------|-------|
| Rows in 5-minute window | 5 |
| Max `xact_commit` (cumulative) | **51,841** |
| Min `xact_commit` (baseline) | 3,043 |
| **Commit delta** | **48,798 transactions** |
| Max `blks_hit` | 3,054,800 |
| Max `numbackends` | **53** (50 pgbench clients + monitoring) |

The commit delta of **48,798** aligns with pgbench's reported **48,637** transactions (the small difference accounts for pgwatch's own monitoring queries).

---

## Future Roadmap

| Item | Status | Description |
|------|--------|-------------|
| Dynamic Baselining | **Implemented** | Server-side 1h vs 24h comparison via `FILTER` clause |
| Dual-DB Routing | **Implemented** | DSN resolution from `pgwatch.source` for live queries |
| MCP Integration | **Implemented** | `stdio` server for Cursor/Claude Desktop |
| Webhook Alerting | **Implemented** | Slack-compatible anomaly dispatching |
| Epistemic Humility | **Implemented** | 4-part structured output with confidence scoring |
| Schema Discovery Tool | Planned | Query `pgwatch.metric` + sample JSONB keys at runtime |
| `ModeConfirm` Sandbox | Planned | Interactive approval for write operations |
| Streaming Responses | Planned | Token-by-token output for long analyses |
| Context Window Management | Planned | Handle conversations exceeding token limits |
| Additional Tools | Planned | `list_available_metrics`, `get_active_locks`, `evaluate_pg_config` |

---

## Getting Started

### Prerequisites

- **Go 1.22+**
- **A running pgwatch v5 instance** with a PostgreSQL metrics sink
- **A Groq API key** (free at [console.groq.com](https://console.groq.com))

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
# Reactive mode — ask a question
pgcopilot ask "Analyze the db_stats.tps trends for my_test_db (sys_id 7618200670381232155)."

# Proactive mode — continuous monitoring with Slack alerts
pgcopilot watch --sys-id 7618200670381232155 --dbname my_test_db --interval 5m \
  --webhook-url https://hooks.slack.com/services/T.../B.../xxx

# Universal Agent mode — expose tools to Cursor / Claude Desktop
pgcopilot mcp
```

### MCP Client Configuration (Cursor)

Add to your Cursor MCP settings:

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
│   └── pgcopilot/
│       ├── main.go              # Entry point, loads .env
│       ├── root.go              # Cobra root command, Viper config
│       ├── ask.go               # Reactive single-shot mode
│       ├── watch.go             # Proactive daemon mode + webhook alerting
│       └── mcp.go               # MCP server over stdio (Universal Agent)
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
│       │   └── hypopg.go        # evaluate_hypothetical_index
│       └── diagnostics/
│           └── active_queries.go # get_active_queries (Dual-DB Routing)
├── .env                         # API keys and DB URLs (not committed)
├── go.mod
└── go.sum
```

---

## License

This project is developed as part of a GSoC 2026 proposal for the [pgwatch](https://github.com/cybertec-postgresql/pgwatch) project.
