# pgcopilot

**An AI Copilot for PostgreSQL Observability — Powered by pgwatch v5 and LLM Tool Calling**

pgcopilot is a proof-of-concept CLI tool that brings **agentic AI** to PostgreSQL database operations. It connects to a [pgwatch](https://github.com/cybertec-postgresql/pgwatch) metrics database, gives a large language model direct access to your time-series performance data through sandboxed tools, and produces expert-level Root Cause Analysis for database incidents — all from a single terminal command.

Built as a **Google Summer of Code 2026** proposal for the pgwatch project.

---

## Table of Contents

- [Project Overview](#project-overview)
- [Core Architecture](#core-architecture)
  - [Provider-Agnostic LLM Layer](#provider-agnostic-llm-layer)
  - [Dual-Mode CLI](#dual-mode-cli)
  - [The Agentic Loop](#the-agentic-loop)
- [Security and Safety: The Sandbox](#security-and-safety-the-sandbox)
- [Multi-Tenant Data Layer](#multi-tenant-data-layer)
  - [SQL Injection Prevention](#sql-injection-prevention)
  - [Tenant Isolation via sys_id Scoping](#tenant-isolation-via-sys_id-scoping)
- [Tools](#tools)
  - [get_metric_trends](#get_metric_trends)
  - [evaluate_hypothetical_index](#evaluate_hypothetical_index)
- [End-to-End Stress Testing](#end-to-end-stress-testing)
- [Architectural Discoveries and Future Roadmap](#architectural-discoveries-and-future-roadmap)
- [Getting Started](#getting-started)
- [Project Structure](#project-structure)
- [License](#license)

---

## Project Overview

Modern PostgreSQL deployments generate vast amounts of observability data — connection counts, transaction throughput, cache hit ratios, WAL activity, lock contention, and more. **pgwatch v5** excels at collecting and storing this data, but the final step — **interpreting it** — still falls on the DBA.

pgcopilot closes that gap. It implements an **agentic loop** where an LLM can:

1. **Receive** a natural-language question about database health.
2. **Decide** which metrics to fetch by emitting structured tool calls.
3. **Execute** those tool calls against the pgwatch metrics database through a read-only security sandbox.
4. **Analyze** the returned data (half-interval averages, percentage changes, time bounds).
5. **Respond** with a technical Root Cause Analysis grounded in real data — not hallucinated guesses.

This is not a chatbot wrapper. The LLM never sees raw SQL or has direct database access. Every interaction is mediated by typed Go tools with strict permission controls.

---

## Core Architecture

```
┌──────────────────────────────────────────────────────────────────┐
│                          CLI (Cobra/Viper)                       │
│                      ask (reactive) │ watch (daemon)             │
└──────────────┬───────────────────────────────┬───────────────────┘
               │                               │
               ▼                               ▼
┌──────────────────────────────────────────────────────────────────┐
│                        Agent (Agentic Loop)                      │
│  ┌─────────┐    ┌──────────┐    ┌──────────┐    ┌────────────┐  │
│  │ System   │───▶│   LLM    │───▶│  Tool    │───▶│  Sandbox   │  │
│  │ Prompt   │    │ Provider │◀───│  Calls   │◀───│  (R/O Gate)│  │
│  └─────────┘    └──────────┘    └──────────┘    └──────┬─────┘  │
│                 max 10 iterations                       │        │
└─────────────────────────────────────────────────────────┼────────┘
                                                          │
               ┌──────────────────────────────────────────┤
               ▼                                          ▼
┌──────────────────────┐                   ┌──────────────────────┐
│  get_metric_trends   │                   │ evaluate_hypothetical│
│  (pgwatch_metrics DB)│                   │ _index (HypoPG)      │
└──────────┬───────────┘                   └──────────┬───────────┘
           │                                          │
           ▼                                          ▼
┌──────────────────────┐                   ┌──────────────────────┐
│   pgwatch_metrics    │                   │   Target Database    │
│   (pgxpool, JSONB)   │                   │   (EXPLAIN plans)    │
└──────────────────────┘                   └──────────────────────┘
```

### Provider-Agnostic LLM Layer

The `internal/provider` package defines a **vendor-neutral interface** that decouples the entire application from any single LLM provider:

```go
type Provider interface {
    Complete(ctx context.Context, req *Request) (*Response, error)
    ModelID() string
}
```

Providers register themselves via a **thread-safe factory registry** using Go's `init()` pattern:

```go
func init() {
    provider.Register("groq", NewGroqProvider)
}
```

Switching LLM backends is a one-line change — swap the blank import and the `provider.New()` call. No other code in the application needs to change.

**Currently implemented providers:**

| Provider | Backend | Default Model | SDK |
|----------|---------|---------------|-----|
| `gemini` | Google Gemini API | `gemini-2.0-flash` | `google.golang.org/genai` |
| `groq` | Groq Inference API | `llama-3.3-70b-versatile` | `github.com/sashabaranov/go-openai` |

The Groq provider points the OpenAI-compatible SDK at `https://api.groq.com/openai/v1`, making it trivial to add any OpenAI-compatible endpoint (Together AI, Fireworks, local vLLM, etc.) by changing a single base URL.

### Dual-Mode CLI

pgcopilot operates in two distinct modes via [Cobra](https://github.com/spf13/cobra) subcommands:

**Reactive Mode (`ask`)** — A DBA pastes a question into the terminal and gets an immediate, data-backed analysis:

```bash
pgcopilot ask "What happened to TPS in my_test_db over the last 30 minutes?"
```

The agent runs a single conversation loop, calls tools as needed, and prints the final answer.

**Proactive Mode (`watch`)** — A long-running daemon that wakes up on a configurable `time.Ticker` interval, fetches metrics, and prints a status report:

```bash
pgcopilot watch --sys-id 7618200670381232155 --dbname my_test_db --interval 5m
```

Each tick creates a **fresh Agent** with a clean conversation history, ensuring that context from a previous analysis cycle never leaks into the next one. The daemon handles `SIGINT`/`SIGTERM` for graceful shutdown.

### The Agentic Loop

The `internal/agent` package implements the core reasoning loop:

1. The user's prompt is appended to the conversation as a `RoleUser` message.
2. The full conversation (system prompt + history) and tool definitions are sent to the LLM.
3. If the LLM responds with **tool calls**, each one is executed through the sandbox and the result is appended as a `RoleTool` message. The loop repeats from step 2.
4. If the LLM responds with **plain text** (no tool calls), the loop terminates and the text is returned to the user.
5. A **hard ceiling of 10 iterations** prevents infinite loops. If the model keeps calling tools without converging on an answer, the agent returns a descriptive error rather than running indefinitely.

This architecture means the LLM can make **multi-step decisions**: fetch one metric, realize it needs another, fetch that too, and then synthesize a final answer — all within a single `ask` invocation.

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

This design ensures that **even if an LLM hallucinates a tool call with mutating intent, the Go runtime physically prevents execution**. The security boundary is enforced in compiled Go code, not in prompt engineering.

---

## Multi-Tenant Data Layer

The `internal/db` package wraps `pgxpool` and implements the data access layer for pgwatch metrics. It enforces two critical security properties at the SQL level.

### SQL Injection Prevention

pgwatch stores each metric in its own table (e.g., `public.db_stats`, `public.wal_stats`). Since **PostgreSQL does not allow parameterized table names** (`$1` cannot be used in `FROM` clauses), the metric name must be interpolated into the SQL string.

To prevent SQL injection, every metric name is validated against a **strict allowlist regex** before interpolation:

```go
var safeIdentifier = regexp.MustCompile(`^[a-zA-Z0-9_]+$`)
```

Any metric name containing dots, dashes, spaces, quotes, semicolons, or any other character that could escape the identifier context is **rejected before the query is ever built**. Only the table name is interpolated; all other filter values use standard `$1`/`$2`/`$3` parameterized binding.

### Tenant Isolation via sys_id Scoping

A single pgwatch metrics database may contain data from **many monitored PostgreSQL clusters**. To prevent cross-tenant data leaks, every query is scoped by two mandatory dimensions:

```sql
WHERE dbname          = $1           -- monitored database name
  AND data->>'sys_id' = $2           -- pgwatch system identifier (unique per cluster)
  AND time           >= NOW() - $3::interval
```

The `sys_id` is extracted from inside the JSONB `data` column. This ensures a caller can only see rows belonging to their own cluster, even when multiple clusters report the same database name. Both `dbname` and `sys_id` are bound as parameterized values — they are **never** interpolated into SQL text.

---

## Tools

### `get_metric_trends`

Fetches time-series data from a pgwatch metric table and computes **half-interval trend analysis**.

**Parameters:**

| Name | Type | Description |
|------|------|-------------|
| `metric_name` | string | The pgwatch metric table (e.g., `db_stats`, `wal_stats`) |
| `sys_id` | string | The pgwatch system identifier for the target cluster |
| `db_name` | string | The monitored database name |
| `interval` | string | PostgreSQL interval expression (e.g., `1h`, `30m`) |

**Analysis pipeline:**

1. Fetches all rows from `public.<metric>` within the time window, scoped by `dbname` and `sys_id`.
2. Selects the best numeric field from the JSONB `data` column (prefers `tps`, `commits`, `rollbacks`, `blks_hit`, `blks_read`; falls back to the first numeric field found).
3. Splits the dataset chronologically into two halves.
4. Computes the **average value** for each half and the **percentage change** between them.
5. Returns a structured summary string that the LLM can directly reason about.

**Example tool output:**
```
Successfully analyzed 5 rows for metric 'db_stats' (field: 'blks_hit', sys_id=7618200670381232155, db=my_test_db).
Time range: 2026-03-18T18:57:03Z to 2026-03-18T19:11:03Z.
In the first half of the interval (older), the average blks_hit was 1234567.0000.
In the second half (more recent), the average blks_hit was 1954321.0000.
This represents a 58.10% increase.
```

### `evaluate_hypothetical_index`

Uses the [HypoPG](https://hypopg.readthedocs.io/) extension to evaluate whether a proposed index would improve a query's execution plan **without creating a real index**.

**Parameters:**

| Name | Type | Description |
|------|------|-------------|
| `query` | string | The SQL SELECT query to test |
| `index_statement` | string | The `CREATE INDEX` statement to evaluate |

**Execution flow:**

1. Acquires a **single connection** from the pool (HypoPG indexes exist only in the creating backend's memory).
2. Runs `EXPLAIN` on the query and parses the **total cost** from the top-level plan node.
3. Creates the hypothetical index via `SELECT * FROM hypopg_create_index($1)`.
4. Runs `EXPLAIN` again — the planner now sees the hypothetical index.
5. Calls `hypopg_reset()` to clean up.
6. Returns the before/after costs and the percentage improvement.

All SQL errors (including "HypoPG extension not installed") are caught and returned as descriptive strings to the LLM, keeping the agent loop stable.

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

The commit delta of **48,798** aligns with pgbench's reported **48,637** transactions (the small difference accounts for pgwatch's own monitoring queries). The `get_metric_trends` tool successfully extracted the half-interval averages and fed them to Llama 3.3, which produced an accurate performance analysis in under 5 seconds.

---

## Architectural Discoveries and Future Roadmap

Building this PoC surfaced two important architectural constraints that will shape the full GSoC implementation.

### 1. Schema Mapping: LLMs Don't Know pgwatch's JSONB Layout

When asked about "commits" or "backends," Llama 3.3 attempted to query `public.commits` and `public.backends` as separate metric tables. In reality, these are **fields inside the `db_stats` JSONB `data` column** (`data->>'xact_commit'`, `data->>'numbackends'`).

**Impact:** The LLM has no inherent knowledge of pgwatch's schema. It does not know that `xact_commit` lives inside `db_stats` rather than in its own table.

**Planned solution:** Introduce a **schema discovery tool** that queries `pgwatch.metric` for available metrics and samples the JSONB keys from the first row of each metric table. This gives the LLM a runtime-accurate map of what fields exist and where, eliminating hallucinated table references.

### 2. Dual-DB Routing: Metrics vs. Target Database

The `get_metric_trends` tool correctly queries `pgwatch_metrics` — that's where the time-series data lives. But tools like `evaluate_hypothetical_index` need to run `EXPLAIN` against the **monitored database itself** (e.g., `test_db`), not the metrics store.

In our stress test, the HypoPG tool failed because it attempted to `EXPLAIN SELECT * FROM pgbench_accounts` against `pgwatch_metrics`, where that table doesn't exist.

**Planned solution:** Implement **dynamic connection resolution** — when a tool needs to operate on the target database, it queries `pgwatch.source` to retrieve the monitored database's connection string, then establishes a dedicated connection to the correct host. This keeps the tool layer self-contained and supports multi-cluster environments.

### Additional Roadmap Items

- **Streaming responses** for long-running analysis sessions.
- **`ModeConfirm` sandbox** for interactive approval of write operations.
- **Additional tools:** `list_available_metrics`, `get_active_locks`, `get_slow_queries`, `evaluate_pg_config`.
- **Prometheus/Grafana integration** for pushing alerts from `watch` mode.
- **Context window management** for conversations that exceed token limits.

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
```

### Build and Run

```bash
# Reactive mode — ask a question
go run ./cmd/pgcopilot ask "Analyze the db_stats trends for my_test_db (sys_id 7618200670381232155) over the last 1h."

# Proactive mode — continuous monitoring
go run ./cmd/pgcopilot watch --sys-id 7618200670381232155 --dbname my_test_db --interval 5m
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
│       ├── main.go            # Entry point, loads .env
│       ├── root.go            # Cobra root command, Viper config
│       ├── ask.go             # Reactive single-shot mode
│       └── watch.go           # Proactive daemon mode
├── internal/
│   ├── agent/
│   │   └── agent.go           # Agentic loop (10-iteration cap)
│   ├── db/
│   │   ├── db.go              # pgxpool client wrapper
│   │   ├── query.go           # FetchMetricData with tenant scoping
│   │   └── metadata.go        # Schema discovery helpers
│   ├── provider/
│   │   ├── provider.go        # Provider interface & shared types
│   │   ├── registry.go        # Thread-safe factory registry
│   │   ├── gemini/
│   │   │   └── gemini.go      # Google Gemini implementation
│   │   └── groq/
│   │       └── groq.go        # Groq/OpenAI-compatible implementation
│   ├── sandbox/
│   │   └── sandbox.go         # Permission enforcement gate
│   └── tool/
│       ├── tool.go            # Tool interface & permission types
│       └── metrics/
│           ├── trends.go      # get_metric_trends tool
│           └── hypopg.go      # evaluate_hypothetical_index tool
├── .env                       # API keys and DB URLs (not committed)
├── go.mod
└── go.sum
```

---

## License

This project is developed as part of a GSoC 2026 proposal for the [pgwatch](https://github.com/cybertec-postgresql/pgwatch) project.
