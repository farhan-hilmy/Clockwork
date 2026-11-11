# Example webhook receiver

A minimal HTTP endpoint for Clockwork webhooks, using only the Go standard library. The `verify` function can be copied into any Go service.

A receiver must:

1. **Verify the signature.** `Clockwork-Signature: t=<unix seconds>,v1=<hex>`, where the hex value is HMAC-SHA256 of `"<t>.<raw body>"` keyed with the endpoint's secret. Compare in constant time, and reject timestamps more than a few minutes old to stop replays. Verify the raw bytes, before parsing the JSON.
2. **Answer 2xx quickly.** Anything else, a redirect, or no answer within 10 seconds is retried: after 10s, 30s, 2m, 10m, 30m, 1h, 3h, 6h and 15h, for 10 attempts in total, then the delivery is marked `failed`. Do slow work asynchronously.
3. **Deduplicate on `Clockwork-Delivery`.** Delivery is at-least-once, and a retry keeps the same delivery ID.

Other headers: `Clockwork-Event` (for example `execution.failed`) and `Clockwork-Event-Id`.

## Run it

```sh
# Register an endpoint (with an application's API key, or the admin token) and keep the secret it returns:
curl -s localhost:8080/api/v1/webhooks -H "Authorization: Bearer $KEY" -H "Content-Type: application/json" \
  -d '{"url":"https://your-host.example.com/","events":["execution.succeeded","execution.failed"]}'

WEBHOOK_SECRET=whsec_... go run ./examples/webhook-receiver   # listens on :9000 (ADDR to change)

# Send a test event:
curl -s -X POST localhost:8080/api/v1/webhooks/$WEBHOOK_ID/test -H "Authorization: Bearer $KEY"
```

Endpoints owned by applications must be public addresses, unless the server sets `CLOCKWORK_APP_HTTP_ALLOW_PRIVATE=true`. For local testing, register the endpoint with the admin token: endpoints of the default application may use `localhost`.
