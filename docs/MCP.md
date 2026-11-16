# Connecting AI agents (MCP)

`clockwork-mcp` is an [MCP](https://modelcontextprotocol.io) server. It lets an AI agent, such as Claude Code or another MCP client, create, run and inspect Clockwork tasks through ready-made tools, instead of learning the REST API.

It runs on the agent's machine and talks to the agent over stdio. It calls Clockwork only through the public `/api/v1` API, with **one application's API key**. The agent therefore works inside that application, with the same rules as any other integration:

- It sees and changes only that application's tasks and runs.
- It cannot create shell tasks.
- Its HTTP tasks may only call public addresses, unless the server sets `CLOCKWORK_APP_HTTP_ALLOW_PRIVATE=true`.

It refuses to start with the admin token.

## Set up

1. **Create an application and a key** for the agent, in the web UI under **Integrations**, or with the admin token (see the README's *Integrating an application*). Give each agent or project its own application, so you can revoke it on its own.

2. **Install the server** (needs Go 1.26+):

   ```sh
   go install github.com/farhan-hilmy/clockwork/cmd/clockwork-mcp@latest
   ```

   Or build it from a checkout with `go build -o clockwork-mcp ./cmd/clockwork-mcp`.

3. **Register it with your MCP client.** It is configured through two environment variables:

   | Variable | Default | |
   |---|---|---|
   | `CLOCKWORK_URL` | `http://localhost:8080` | Clockwork's base URL. |
   | `CLOCKWORK_API_KEY` | required | The application API key (`cwk_…`). |

   Claude Code:

   ```sh
   claude mcp add clockwork \
     --env CLOCKWORK_URL=http://localhost:8080 \
     --env CLOCKWORK_API_KEY=cwk_... \
     -- clockwork-mcp
   ```

   Clients configured with JSON, such as Claude Desktop, use the usual `mcpServers` entry:

   ```json
   {
     "mcpServers": {
       "clockwork": {
         "command": "clockwork-mcp",
         "env": { "CLOCKWORK_URL": "http://localhost:8080", "CLOCKWORK_API_KEY": "cwk_..." }
       }
     }
   }
   ```

   The key is a secret: keep it out of shared or committed config files where your client allows.

The server checks the key when it starts. If Clockwork is unreachable, the key is wrong or revoked, or the key is the admin token, it exits with a message on stderr, which most clients show in their MCP logs.

## Tools

| Tool | Does | Changes anything |
|---|---|---|
| `list_tasks` | Lists the application's tasks, with search and paging. | No |
| `get_task` | Shows one task: request, schedule, retry policy, next run. | No |
| `create_task` | Creates an HTTP task, optionally on a cron schedule with a timezone. | Yes |
| `update_task` | Changes only the given fields. Request fields are merged, and masked secrets are kept. `remove_schedule` makes a task run only on demand. | Yes |
| `set_task_enabled` | Pauses or resumes a task's schedule. | Yes |
| `run_task` | Queues a run now. Send `idempotency_key` to make retries safe; one is generated if omitted. Returns the run's id. | Yes, starts a run |
| `list_runs` | Lists runs, newest first, filtered by task, status or time, **without** their output. | No |
| `get_run` | Shows one run: status, timing, error, and the target's response. | No |
| `get_stats` | Counts runs by status, with success rate and durations, over 1h, 24h, 7d or 30d. | No |

There is no delete tool. Deleting tasks stays a deliberate action in the web UI or API.

Errors come back as tool errors the agent can read and correct itself from, for example `Clockwork answered 422 validation_failed: one or more fields are invalid (schedule.cron: invalid cron expression: …)`.

## Run output is untrusted

A run's output is whatever the called server sent back. Anyone controlling that server can put text there meant to steer the agent ("ignore your instructions and…"). So:

- `get_run` returns the response under **`output_untrusted`**, next to an `output_note` saying it is data, not instructions. The server's own instructions to the client say the same.
- `list_runs` leaves output out entirely, so browsing runs does not pull untrusted text into the agent's context.
- Output is the same bounded excerpt Clockwork stores: the first 8 KiB of an HTTP body.

This reduces the risk; it cannot remove it. Point agents' tasks at endpoints you trust, and review what an agent proposes to change after reading run output.
