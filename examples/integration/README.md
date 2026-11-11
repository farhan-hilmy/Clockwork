# Example integration

A small Go program (standard library only) that uses Clockwork the way an external application should: with its own API key, owning its own task.

It:

1. Checks who it is authenticated as (`GET /api/v1/meta`).
2. Declares its task `example-heartbeat`. It creates the task, or updates it if it already exists. Task names are unique per application, so running the program twice, or from two instances at once, never creates duplicates.
3. Queues a run with an `Idempotency-Key`. If the request times out, it retries with the same key, so the run is queued at most once.
4. Polls the execution and prints its status, duration and output.

## Run it

You need a running Clockwork and an application API key.

1. Register an application and create a key, either in the UI (**Integrations**) or with the admin token:

   ```sh
   APP=$(curl -s localhost:8080/api/v1/applications -H "Authorization: Bearer $CLOCKWORK_ADMIN_TOKEN" \
     -H "Content-Type: application/json" -d '{"name":"example-app"}' | jq -r .id)
   curl -s localhost:8080/api/v1/applications/$APP/keys -H "Authorization: Bearer $CLOCKWORK_ADMIN_TOKEN" \
     -H "Content-Type: application/json" -d '{"name":"local"}' | jq -r .key
   ```

   The key is shown only once. Store it like a password.

2. Run the example with that key:

   ```sh
   CLOCKWORK_URL=http://localhost:8080 CLOCKWORK_API_KEY=cwk_... go run ./examples/integration
   ```

   Optional: `TARGET_URL` sets what the task calls (default `https://api.github.com/zen`). Application tasks may only call public addresses, unless the server sets `CLOCKWORK_APP_HTTP_ALLOW_PRIVATE=true`.

Example output:

```
authenticated as application "example-app"
task: tsk_01m75...
queued execution: exe_01m75...
status: succeeded (attempt 1)
duration: 412 ms
output: {"status_code":200,"content_type":"text/plain;charset=utf-8","body":"Design for failure.","body_truncated":false,"body_bytes":19}
```

The full API is described by `GET /api/v1/openapi.json`.
