# notify-qq

An HTTP service that sends Markdown notifications with optional link buttons from your scripts to your private chat with an official QQ bot. Built with Go 1.27, Echo v5, Viper, Samber Oops, and the standard library's `log/slog`.

Scripts call `POST /notify` with a Bearer API key. The service sends a proactive C2C message using the bot's AppID/AppSecret and a paired `user_openid`. It obtains and caches QQ access tokens automatically. A separate outbound WebSocket connection receives QQ private-chat events for pairing; event reception supports WebSocket only.

## Project layout

The command entry point lives in `cmd/notify-qq/main.go` and owns OS signals and process exit status. Server implementation packages live under `internal`, following the [Go server-project layout guidance](https://go.dev/doc/modules/layout#server-project).

- `internal/app`: application lifecycle and dependency assembly (`app.go`), Echo routes and QQ event handling (`service.go`), local key commands (`keys.go`), and behavior tests.
- `internal/apikey`: random credential generation, SQLite persistence of credentials and recipients, embedded SQL migrations, one-time pairing, metadata listing, revocation, and context-aware authentication.
- `internal/config`: Viper configuration loading and startup validation.
- `internal/logging`: shared slog configuration, request logger propagation, and typed Oops error attributes.
- `internal/telemetry`: optional OTLP/HTTP trace and metric exporters, request/QQ instrumentation, and bounded SDK lifecycle.
- `internal/qq`: QQ HTTP API protocol, token caching, error classification, message sending, and WebSocket gateway lifecycle (`gateway.go`). Its constructor accepts an HTTP-client option for networking configuration and behavior tests.

`app` assembles these components; configuration and QQ integration depend on the shared logging package. Those components do not depend on the command or application package. The application owns error reporting, while lower-level operations return enriched errors. `app.Run` explicitly opens the key store, creates the QQ client and logger, passes them to a service constructor that performs no I/O, and closes storage after requests drain and workers stop. CLI commands and test fixtures own their separate stores and loggers. The command passes its cancellation context to the server lifecycle. Each HTTP request propagates its own context and deadline into downstream calls.

The project root contains module metadata, documentation, and tooling configuration. Tests stay with their owning package; the application behavior tests cover the complete HTTP-to-QQ flow using a mock HTTPS server. `mise run build` writes the executable to `bin/notify-qq`.

Run `./bin/notify-qq --version` to print JSON containing `version`, `commit`, `built_at`, and `go_version` without loading application configuration or opening storage. Source builds report `dev` with unknown commit/build time; distribution builds embed those values. Startup logs also include the version and commit.

[AGENTS.md](AGENTS.md) records the logging, error handling, configuration, coding style, and testing conventions for this template.

## Install

The repository is [`ceclin/notify-qq`](https://github.com/ceclin/notify-qq), and the Go module is `github.com/ceclin/notify-qq`. Published versions will provide Linux amd64/arm64 archives on [GitHub Releases](https://github.com/ceclin/notify-qq/releases) and multi-platform images at `ghcr.io/ceclin/notify-qq`.

Download the archive for your machine and `SHA256SUMS` from the same release. Verify the downloaded archive before extracting it:

```sh
sha256sum --ignore-missing --check SHA256SUMS
gh attestation verify SHA256SUMS --repo ceclin/notify-qq
gh attestation verify notify-qq_VERSION_linux_amd64.tar.gz --repo ceclin/notify-qq
tar -xzf notify-qq_VERSION_linux_amd64.tar.gz
./notify-qq --version
```

Replace `VERSION` with the archive's version; use `arm64` for ARM machines. Each archive contains the executable, this README, `.env.example`, the project LICENSE, and `THIRD_PARTY_LICENSES` for the Go runtime and dependencies. Keep the license files with redistributed binaries.

Container releases use `ghcr.io/ceclin/notify-qq:vMAJOR.MINOR.PATCH` or a prerelease tag such as `v0.1.0-rc.1`. `latest` follows formal releases; `preview` follows prereleases. Pin a version or digest for deployments. To use a published version with the existing Compose configuration, replace its `build: .` with `image: ghcr.io/ceclin/notify-qq:VERSION`. The volume, environment, listener, and local key commands remain the same.

## Run locally

```sh
cp .env.example .env
chmod 600 .env
```

Fill in `QQ_APP_ID` and `QQ_APP_SECRET` from your [QQ Bot developer console](https://q.qq.com/), then create a script credential and start the service:

```sh
go run ./cmd/notify-qq keys create --name backup
go run ./cmd/notify-qq
```

Save the returned `key` in the script's private environment. Give the separate `pairing_code` to its intended QQ recipient, who sends `bind <pairing_code>` to the bot. Both secrets are shown only once; the code expires after 15 minutes.

Viper reads `.env` from the working directory; environment variables override file values. Configuration is loaded at startup. The default listener is `127.0.0.1:8080`.

## Manage script API keys

Give each script its own credential. Commands use the same `DATA_DIR` as the service and work while the service is running. Build once with `mise run build`, then:

```sh
./bin/notify-qq keys create --name backup
./bin/notify-qq keys create --name deployment
./bin/notify-qq keys pair KEY_ID
./bin/notify-qq keys list
./bin/notify-qq keys revoke KEY_ID
```

`create` returns an API key and a separate pairing code:

```json
{
  "id": "KEY_ID",
  "name": "backup",
  "created_at": "2026-10-12T08:00:00Z",
  "revoked_at": null,
  "user_openid": "",
  "pairing_expires_at": "2026-10-12T08:15:00Z",
  "key": "nqq_...",
  "pairing_code": "nqp_..."
}
```

These are placeholder values. The `nqq_` API key and `nqp_` pairing code each contain 256 independently generated random bits encoded with base64url. Store the API key securely when created; it cannot be retrieved later. The intended recipient sends `bind <pairing_code>` in a private chat with the bot within 15 minutes. The pairing code binds only its associated key, is consumed atomically on success, and cannot authenticate HTTP requests.

`keys pair ID` issues a new 15-minute code for an active key, invalidating any previously issued code. Use it when a code expires or when changing that key's recipient. The existing recipient remains active until the replacement code is consumed. Only the local operator can issue a fresh code, so a consumed code cannot replace a binding. Replay, expired codes, and codes for revoked keys are ignored. Anyone who has an unconsumed code can bind its key, so share it only with the intended recipient.

Names contain 1–100 bytes without control characters. `list` returns metadata for active and revoked keys, including `user_openid` and the pending `pairing_expires_at`, without secrets. Empty `user_openid` means unpaired. `revoke` uses the ID from `create` or `list`, also invalidates pending pairing codes, and is harmless when repeated. Missing/revoked IDs cannot receive new codes. Invalid commands exit with status 1 and emit one structured error to stderr. `--help` prints command usage.

Key commands read only storage/logging configuration and require no QQ credentials. Run them from the same working directory as the service, or set an absolute `DATA_DIR`. Command results go to stdout; structured logs go to stderr. No HTTP key-management endpoints are exposed. Each key's `/notify` and `/status` use only its own recipient. Different keys can bind different QQ users, or the same user. Requests cannot override the recipient.

Scripts send `Authorization: Bearer <key>`. Assign the created secret to each script's `NOTIFY_API_KEY` environment variable; the service reads credentials only from SQLite. Authentication checks SQLite on every request, so new keys and revocations affect the next request without a service restart. A request already authenticated uses the recipient it read and can finish after revocation or rebinding. Database lookup failures return HTTP 500 and never authorize a request. Without any active keys, the service starts with a warning and rejects authenticated endpoints until a key is created; `/healthz` remains available.

SQLite stores SHA-256 digests of API keys and pairing codes, each key's recipient, and metadata in `DATA_DIR/notify.sqlite3`, with owner-only file permissions, WAL journaling, and a bounded busy timeout for service/CLI access. Retain the complete data directory across deployments. For a simple filesystem backup, stop the service and any key commands before copying the directory; copying only a live database file can omit WAL changes. Run one service instance per directory.

## Database migrations

`internal/apikey/migrations/*.sql` contains numbered [Goose](https://pressly.github.io/goose/documentation/provider/) SQL migrations, embedded in the executable. Opening storage at server or CLI startup applies pending migrations transactionally. Successful schema changes and their migration history commit together; a failing migration rolls back its own schema/data/history changes. Opening a database created by a newer migration version fails instead of attempting to downgrade it.

Add the next numbered SQL file with `-- +goose Up` and `-- +goose Down` sections. Keep migrations transactional, retain committed files unchanged, and add behavior tests for upgrade, preserved data, and failure rollback. Runtime applies only upward migrations. No additional tool installation or migration-file mount is needed in the container.

Goose records versions in `goose_db_version`. The first migration creates the `api_keys` table in a new database. Subsequent migrations upgrade the versioned schema while retaining credentials, recipient bindings, revocations, and pending pairing codes.

A bounded cross-process lock in `notify.sqlite3.migrate.lock` serializes version checks and migrations across the server and CLI. Leave this lock file in place; it contains no business data and must not be replaced while processes use the data directory. Migration failures return enriched errors to the owning startup/CLI boundary; successful changes emit `database_migrated` with the version and migration count. Third-party migration logging is disabled.

## API path prefix

To deploy under a URL path, set `API_PATH_PREFIX` in `.env`, for example:

```dotenv
API_PATH_PREFIX=/api/notify-qq
```

All four HTTP endpoints then move under that prefix: `/api/notify-qq/notify`, `/api/notify-qq/status`, `/api/notify-qq/readyz`, and `/api/notify-qq/healthz`. Set scripts' `NOTIFY_URL` to `https://YOUR_DOMAIN/api/notify-qq`. The outbound QQ WebSocket connection is independent of this prefix. The examples below use the default root deployment; add the prefix to their URLs when configured. Unprefixed endpoints return 404.

An empty prefix or `/` selects root deployment. Surrounding whitespace and a single trailing slash are removed. Other prefixes must start with `/`; segments may contain ASCII letters, digits, `-`, `.`, `_`, and `~`. Empty segments, standalone `.` or `..`, percent escapes, query strings, fragments, and Echo route parameters/wildcards are rejected at startup. Health/readiness probes must use the prefixed `/healthz` and `/readyz` URLs too.

## Connect your QQ account

1. Create a bot and enable the **message-list private chat / 单聊** scenario. Configure your QQ account as a sandbox tester while developing and add the test bot to your message list. For production, complete QQ's required review/release process and configure the server's outbound IP whitelist where enabled.
2. Start the service. It obtains the WSS gateway from `/gateway/bot`, connects outbound to QQ, and subscribes to `GROUP_AND_C2C_EVENT` (`1 << 25`) using one shard `[0, 1]`. Wait for the `qq_gateway_connected` log event. No public QQ callback URL is required. The server needs outbound HTTPS/WSS access and any outbound IP whitelist required by your QQ application.
3. Create a key for a script with `keys create --name NAME`. Its intended recipient sends `bind <pairing_code>` from their own QQ private chat using the code returned by the command. Binding affects only that key. Ordinary messages, wrong/expired codes, and replayed codes do not change bindings.
4. Check pairing using your script API key:

   ```sh
   curl --fail-with-body https://YOUR_DOMAIN/status \
     -H "Authorization: Bearer $NOTIFY_API_KEY"
   ```

   Expect `{"paired":true,"user_openid":"...","websocket_connected":true}`. This response describes only the key used in the request. Its recipient and consumed-code state are saved atomically in `DATA_DIR/notify.sqlite3` with owner-only file permissions. Repeat the process for each script/recipient.
5. Enable **allow proactive messages / 允许主动发送** in the bot's QQ settings. QQ may reject notifications if this is disabled or the bot lacks the required permissions.

To change a key's recipient, run `keys pair KEY_ID` and give the newly returned code to the replacement recipient. Neither a service restart nor deleting data is required. The bot-specific `user_openid` shown in status is **not a numeric QQ account number**.

## WebSocket connection lifecycle

The QQ connection authenticates with `QQBot <AccessToken>`, follows the heartbeat interval supplied in Hello, and waits for heartbeat ACKs. Missing ACKs, transport failures, and server-requested reconnects trigger exponential backoff with jitter, capped at 30 seconds. QQ session-start quota resets and HTTP rate-limit `Retry-After` guidance can require a longer wait. Repeated outage errors are reported at increasing intervals; debug logs retain every retry.

After a recoverable disconnect the worker resumes with its in-memory session and the last successfully processed sequence. Event handling is serial and independent of heartbeats. A bounded 16-event queue prevents unbounded memory growth; a full queue triggers reconnect/resume from the processed cursor. Duplicate sequences are skipped, and a RESUMED marker cannot advance past queued events. Rejected/expired sessions use a fresh Identify. Known non-retryable protocol, permission, or banned/offline-bot errors stop the worker, log one failure, and shut down the service with a nonzero exit status.

`GET /status` includes `websocket_connected`: true after READY or RESUMED, false while connecting or disconnected. `/healthz` remains a process liveness check. `/readyz` checks the local `api_keys` table and whether HTTP serving is active; it does not call QQ. Repeated storage failures emit one readiness-degraded warning and recovery emits one info event. This read probe does not guarantee a future write or QQ delivery will succeed. A paired recipient can still receive notifications through the HTTP send API during an event-connection outage, subject to QQ accepting those requests.

Sessions and event cursors are kept in memory; a process restart starts a new session. Recipient pairing persists in the data directory. SIGTERM cancels the event worker, closes its socket, waits for its workers, and drains accepted HTTP requests before exit. Notification sends are not automatically retried.

## Send a notification

```sh
export NOTIFY_API_KEY='the-key-returned-by-keys-create'
curl --fail-with-body --max-time 25 https://YOUR_DOMAIN/notify \
  -H "Authorization: Bearer $NOTIFY_API_KEY" \
  -H 'Content-Type: application/json' \
  -d '{"content":"## Backup completed\n\n**Success**\n\nAll files were uploaded successfully.","buttons":[{"label":"View logs","url":"https://example.com/logs"}]}'
```

Success:

```json
{"status":"accepted","message_id":"QQ_MESSAGE_ID","timestamp":"2026-10-11T20:00:00+08:00"}
```

`accepted` means QQ returned a message ID. It does not guarantee a device notification or that you read the message; client notification settings still apply.

`content` is the complete Markdown message, including any headings, emphasis, or images supplied by the caller. Leading/trailing whitespace is trimmed and the body is forwarded directly. Every message uses QQ `msg_type: 2` and `markdown.content`.

`buttons` is optional, with up to five `{ "label": "View logs", "url": "https://example.com/logs" }` entries. Labels and URLs are trimmed. Labels contain 1–10 Unicode characters without control characters. Targets must be absolute `http://` or `https://` URLs without credentials or whitespace, at most 2048 UTF-8 bytes. The service emits blue outlined link buttons in input order, two per row. No keyboard is sent when the list is empty or null. Callback/command actions and raw QQ keyboard fields are not accepted.

The trimmed Markdown content must fit within 4000 UTF-8 bytes; the JSON body is limited to 64 KiB. Unknown fields and multiple JSON objects are rejected. Every notification targets the recipient bound to the request's API key. Notification text, button labels, and URLs are not logged; workflow logs include the format, button count, content byte count, key ID, and request ID.

QQ supports headings, emphasis, lists, quotes, links, and images in Markdown. For images, use a public URL and QQ's image-size syntax, for example `![Chart #200px #100px](https://example.com/chart.png)`. See [QQ Markdown documentation](https://bot.q.qq.com/wiki/develop/api-v2/server-inter/message/type/markdown.html) and the [single-chat keyboard schema](https://bot.q.qq.com/wiki/develop/api-v2/autogen/api/v2_users_user_openid_messages.post.html). QQ permissions, proactive-message settings, link-domain configuration, and client rendering still apply; local validation does not guarantee QQ accepts every Markdown document or URL.

The shell helper accepts Markdown content, an optional link URL, and optional link label (default `Details`). It requires `curl` and `jq`:

```sh
export NOTIFY_URL=https://YOUR_DOMAIN
export NOTIFY_API_KEY='the-key-returned-by-keys-create'
./examples/notify.sh $'## Job completed\n\n**The scheduled job finished.**' 'https://example.com/logs' 'View logs'
./examples/notify.sh "$(tail -n 10 /path/to/job.log)"
```

A Node.js script can use the same endpoint:

```js
const response = await fetch(`${process.env.NOTIFY_URL}/notify`, {
  method: 'POST',
  headers: {
    Authorization: `Bearer ${process.env.NOTIFY_API_KEY}`,
    'Content-Type': 'application/json',
  },
  body: JSON.stringify({ content: '## Task completed\n\nYour script finished.' }),
  signal: AbortSignal.timeout(25_000),
});
const result = await response.json();
if (!response.ok) throw new Error(JSON.stringify(result));
```

## API and errors

[api/openapi.yaml](api/openapi.yaml) is the OpenAPI 3.1.1 contract for all four HTTP endpoints, authentication, request and response schemas, correlation headers, and error responses. Behavior tests validate successful requests and real HTTP responses against it. Keep the contract, behavior tests, and examples aligned when changing the API. Byte limits and delivery uncertainty are described in the contract because JSON schemas alone cannot enforce them.

- `POST /notify`: authenticated Markdown notification with optional link buttons; returns 200 when accepted by QQ.
- `GET /status`: pairing status and recipient OpenID for the authenticated key, plus the shared WebSocket connection state.
- `GET /readyz`: public SQLite and lifecycle readiness, with a one-second probe deadline; returns 200 (`{"status":"ready"}`) or 503. Empty key tables are healthy. QQ/WebSocket connectivity and recipient pairing are independent.
- `GET /healthz`: public process liveness; does not check QQ connectivity or pairing.

Failures include an `error` object and a `request_id`. Every response also has an `X-Request-ID` header. QQ rejection responses include a `qq` object with its HTTP status, business code, and trace ID when available; raw upstream messages and credentials are omitted.

Unknown paths return 404, and unsupported HTTP methods return 405 with an `Allow` header. These use the same error envelope and correlation headers as application failures. `error.code` identifies the underlying failure, for example `qq_request_rejected`, `qq_transport_failed`, or `panic_recovered`; callers should not match the human-readable message.

```json
{
  "error": {
    "code": "qq_request_rejected",
    "message": "QQ could not accept the notification; check the QQ error code and bot permissions"
  },
  "request_id": "...",
  "qq": {"http_status":200,"code":40034105,"trace_id":"..."}
}
```

The service returns 400 for invalid notification input, 401 for rejected credentials, 500 when authentication storage is unavailable, 409 before pairing, 413 for an oversized body, 415 for unsupported content types, 429 for QQ rate limits (with `Retry-After`), 502 for other QQ failures, and 504 for upstream timeouts. Both QQ's `code` and `err_code` business-error formats are checked, including when HTTP status is 200. Errors expose stable machine codes from the underlying Oops error.

There is no queue, delivery retry loop, or idempotency store. Each valid API request attempts one send. Automatic retries are avoided because a transport failure can happen after QQ accepted a message; blindly retrying may duplicate it. A QQ 401 invalidates the cached access token for the next request. Scripts should retain/report failed requests and decide whether to retry.

## Configuration

Required values:

- `QQ_APP_ID`: your bot AppID.
- `QQ_APP_SECRET`: its AppSecret, used to obtain access tokens for HTTP calls and WebSocket authentication.

Optional values:

- `LISTEN_ADDR`: default `127.0.0.1:8080`.
- `API_PATH_PREFIX`: default empty; optional literal URL path such as `/api/notify-qq`, shared by all endpoints.
- `DATA_DIR`: default `./data`.
- `LOG_LEVEL`: default `info`; supports standard slog levels (`debug`, `info`, `warn`, `error`, case-insensitive) and their numeric offsets. Use `debug` for external API timings and pairing decisions.

The service uses QQ's [current unified API host](https://bot.q.qq.com/wiki/develop/api-v2/dev-prepare/api-call-guide.html), `https://api.bot.qq.com`, including `/app/getAppAccessToken` for access tokens. These addresses are fixed. Configure test accounts and bot permissions in QQ's developer console.

Each application instance and its Echo routes share an explicitly supplied `slog.Logger` and standard `JSONHandler`, writing JSON to stderr with stable `event` values, constant messages, request correlation, workflow results, and timings. Request loggers are attached both to Echo's context and the request's Go context, so downstream QQ and storage events retain the same `request_id`, `method`, and `route`.

Application log calls use `LogAttrs` with typed `slog.String`, `slog.Int`, `slog.Bool`, `slog.Float64`, and `slog.Any` attributes. Shared logger fields use `Handler.WithAttrs([]slog.Attr)`. These APIs enforce attribute arguments at compile time, avoiding unbalanced key/value pairs accepted by the `...any` convenience methods. Oops context fields also use typed attributes.

Oops errors use their native slog support and are logged once at their owning boundary, with metadata under `error`, including `error.context` and `error.stacktrace`. Expected invalid requests and ignored messages are debug events; rate limits/timeouts are warnings; failed operations are errors. Liveness/status access logs are debug to reduce repetitive noise. Startup failures emit an error and exit with status 1. Credentials, pairing codes, and notification text are not logged. Recipient references in workflow logs are hashed.

The log envelope uses standard slog keys (`time`, `level`, `msg`), with uppercase level names. Explicit `duration_ms` and `ttl_ms` fields are numeric milliseconds. The JSON handler adds `trace_id` and `span_id` from the active request context when available.

## OpenTelemetry

Tracing and metrics are disabled until `OTEL_EXPORTER_OTLP_ENDPOINT` is set. To send them to an existing OTLP/HTTP collector or backend, add these settings to `.env` or the environment:

```dotenv
OTEL_EXPORTER_OTLP_ENDPOINT=http://localhost:4318
OTEL_SERVICE_NAME=notify-qq
OTEL_TRACES_SAMPLER_ARG=1
OTEL_EXPORTER_OTLP_HEADERS=
```

The endpoint is a base HTTP(S) URL; the service appends `/v1/traces` and `/v1/metrics`, preserving a configured base path. Credentials, query strings, and fragments in the URL are rejected. Use `OTEL_EXPORTER_OTLP_HEADERS` for collector authentication, for example `Authorization=Bearer%20TOKEN`. Headers are comma-separated `name=value` pairs with percent-encoded values. Keep credentials private. In a container, use a collector address reachable from that container instead of host `localhost`.

`OTEL_SERVICE_NAME` defaults to `notify-qq`; release version metadata supplies `service.version`. The sampler is parent-based with a trace-ID ratio for new traces. `OTEL_TRACES_SAMPLER_ARG` defaults to `1` (all new traces), and accepts values from `0` to `1`. Valid W3C `traceparent`/`tracestate` headers continue the caller's trace and sampling decision. Invalid trace context starts a new trace. Sampling does not disable metrics.

- Server spans cover the HTTP request and use the matched route, including `API_PATH_PREFIX`. Unknown routes and nonstandard methods use bounded names. `/healthz` and `/readyz` are excluded from server traces and metrics.
- Client spans cover QQ token, gateway discovery, and message HTTP calls. QQ business errors at HTTP 200 still mark the client span as failed. Trace headers are not sent to QQ; WebSocket heartbeat/event activity retains its existing structured logs.
- `http.server.request.duration` and `qq.client.request.duration` are histograms in seconds. Their counts, status/result attributes, and buckets support request-rate, failure-rate, and latency queries. Metric series are bounded per instrument.
- `slog` continues writing JSON to stderr with request IDs and active trace/span IDs; logs are not exported through OTLP. Notification text, button labels/URLs, recipient paths, credentials, request query strings, and baggage are excluded from telemetry.

The application owns its providers and injects them into HTTP/QQ components without replacing global providers. Traces use a bounded, nonblocking batch queue; metrics export every 30 seconds. Exports have a three-second deadline. SDK failures emit a sanitized warning at most once per 30 seconds and do not change notification outcomes. After HTTP requests and QQ workers stop, shutdown allows up to five seconds for best-effort export. Data may be dropped when the collector is unavailable or the trace queue fills. Local key commands and `--version` do not initialize telemetry.

## Docker deployment

After filling `.env`, create the first key in the same persistent volume:

```sh
docker compose build
docker compose run --rm --no-deps notify-qq keys create --name backup
docker compose up -d
docker compose logs -f notify-qq
```

Save the returned credential for your script. While the container runs, manage keys with:

```sh
docker compose exec notify-qq /notify-qq keys create --name deployment
docker compose exec notify-qq /notify-qq keys pair KEY_ID
docker compose exec notify-qq /notify-qq keys list
docker compose exec notify-qq /notify-qq keys revoke KEY_ID
```

Compose exposes `127.0.0.1:8080` for a host HTTPS reverse proxy and retains pairing and API keys in a named volume. The runtime image runs as a non-root user with a read-only root filesystem; `/data` remains writable. The container listener and data directory are overridden by Compose regardless of your `.env` values. `docker compose down` keeps pairing; `down -v` deletes pairing, keys, and revocation history.

Compose passes `API_PATH_PREFIX` through from `.env`. Forward the complete request path to the service: the application owns the prefix, so the proxy must preserve it. With `API_PATH_PREFIX=/api/notify-qq`, a Caddy host shared with other services can use:

```caddyfile
notify.example.com {
    handle /api/notify-qq/* {
        reverse_proxy 127.0.0.1:8080
    }
}
```

Use Caddy's [handle](https://caddyserver.com/docs/caddyfile/directives/handle) here; [handle_path](https://caddyserver.com/docs/caddyfile/directives/handle_path) strips the matched prefix and would cause the service to return 404. Call `/api/notify-qq/notify` and use `/api/notify-qq/readyz` for traffic readiness and `/api/notify-qq/healthz` for liveness. A reverse proxy is optional when scripts reach the API over a private network; QQ event reception uses an outbound WSS connection.

For example, a host Caddy reverse proxy can use:

```caddyfile
notify.example.com {
    reverse_proxy 127.0.0.1:8080
}
```

Replace the domain and point its DNS at the server. Scripts can call `/notify` through HTTPS or a private network; retain Bearer authentication. Configure allowed message-link domains in the QQ console if your notification content contains URLs.

## Verification

Install [mise](https://mise.jdx.dev/getting-started.html), then initialize the project tools and run the same validation entry point as CI:

```sh
mise trust
mise install
mise run ci
```

`mise.toml` owns tool version selection and all validation commands. Go uses the `1.27` release series, resolving to an installed patch release; Docker uses the same series. golangci-lint and the vulnerability scanner have explicit version pins. Refresh Go patches within that series with `mise upgrade go`; refresh the Docker builder with `docker compose build --pull`. `go.mod` retains the minimum language version required by the code.

`ci` runs `lint`, `lint:workflows`, `lint:release`, `vet`, and `vuln` in parallel, then runs `test` after all checks pass, and finally runs `build` after the tests pass. Any failure stops progression to the next stage. Tasks run from the project root, including when invoked from a subdirectory.

Run individual checks while developing:

```sh
mise run lint
mise run lint:workflows
mise run lint:release
mise run vet
mise run vuln
mise run test
mise run build
```

Run `mise run format` to apply Go formatting and organize imports before checking changes. `mise run lint` validates `.golangci.yml`, checks `goimports` formatting without rewriting files, and runs the selected linters:

- `sloglint` requires typed attributes, context-aware log calls, constant messages, and snake_case keys. It rejects global logging calls and attributes named `time`, `level`, `msg`, or `source`, which are reserved for the log envelope.
- `noctx` requires context-aware HTTP and network APIs. `rowserrcheck` and `sqlclosecheck` check database iteration errors and resource closure.
- `depguard` keeps `internal/apikey`, `internal/config`, `internal/logging`, `internal/qq`, and `internal/telemetry` independent of application and command packages. Production source cannot import `testing`, `net/http/httptest`, or `internal/testsupport`; tests use those packages for local fixtures.
- `modernize` checks for supported modern Go patterns. `ineffassign`, `durationcheck`, and `bidichk` check ineffective assignments, duration arithmetic, and bidirectional control characters.
- `staticcheck`, `errcheck`, `errorlint`, and `unused` check code correctness, error handling, and unused declarations. Intentionally ignored cleanup errors are explicitly discarded; errors affecting an operation's result are handled.

Do not suppress findings with `nolint` directives or lint exclusions. A linter that produces false positives is removed instead of adding exceptions.

`vuln` runs pinned `govulncheck` using the selected Go toolchain, checking reachable application code against the Go vulnerability database. It requires network access and fails on findings affecting the application or on a failed scan. It may report additional advisories for modules without a reachable vulnerable function. The scanner is a development tool and does not add application module dependencies.

GitHub Actions installs the tools from `mise.toml` and calls `mise run ci` on pushes to `main` and pull requests; the workflow can also be started manually. Actions use full commit SHA pins. A newer run on the same ref cancels an obsolete CI run. `lint:workflows` uses the pinned `jactionlint` version to validate workflow syntax, expressions, and action inputs.

When reusing this repository as a project template, copy `AGENTS.md`, `mise.toml`, `.golangci.yml`, and `.github/workflows/ci.yml` together. Update the module path and imports, command name, configuration, integration packages, and OpenAPI contract for the new service. Also update the local import prefix and dependency-rule module paths in `.golangci.yml`, and the service field in the logging package. Keep the entry point in `cmd` and implementation packages in `internal`. Update tool versions and project-specific tasks in `mise.toml`; the workflow continues to use the `ci` task without duplicating its commands. Keep the Go version compatible with the minimum language version in `go.mod`, and adjust the Docker builder when upgrading the Go release family.

## Publishing releases

The release workflow runs when a `vMAJOR.MINOR.PATCH` tag is pushed to this repository. SemVer prereleases such as `v0.1.0-rc.1` are accepted; numeric identifiers must not contain leading zeroes. Build metadata (`+...`) is not accepted because the same version is used as a container tag.

Before tagging a reviewed commit, run `mise run ci` and build local distribution archives:

```sh
mise run release:snapshot
```

This task collects licenses from both Linux architecture dependency graphs with the pinned `go-licenses` tool, builds with the pinned GoReleaser version, and checks archive checksums, executable permissions, and the complete license/configuration contents. Results go to `dist/`; generated license files go to `THIRD_PARTY_LICENSES/`. Both directories are ignored by version control. Snapshot mode performs no uploads and needs no GitHub credentials or local tags. Without a Git remote or version tags, GoReleaser uses placeholder snapshot metadata.

For a published version, the workflow validates a clean checkout matching the tag and source SHA, runs the same `mise run ci`, builds the archives, and uses those binaries in `Dockerfile.release` for a Linux amd64/arm64 image. Release binaries embed the tag, full commit SHA, build time, and Go version. The runtime image includes CA certificates and project/dependency licenses, runs as UID/GID 65532, and stores writable data under `/data`. `Dockerfile` remains the local source-build entry point.

The workflow stages a draft GitHub Release with generated release notes and publishes the versioned image. It then creates GitHub artifact attestations for both archives, `SHA256SUMS`, and the container digest. After attestation succeeds, it promotes that exact image digest to `preview` or `latest` and publishes the GitHub Release with the corresponding prerelease/latest flags. A failed run can leave a draft release or versioned image; inspect those before retrying. Published version tags must never be moved or reused. Release runs are serialized, and release tool caches are disabled.

The workflow uses the repository's built-in `GITHUB_TOKEN` with release, package, and attestation permissions; no personal token is required. Set the GHCR package visibility to public after the first publication if needed. To verify a downloaded image's provenance, use `gh attestation verify oci://ghcr.io/ceclin/notify-qq@sha256:DIGEST --repo ceclin/notify-qq` with the digest from that release's `digests.txt`.

When copying the publishing setup to another project, update `.goreleaser.yml`, the repository guard and module linker paths in `mise.toml`/GoReleaser, image coordinates, the release workflow, and README links. Retain the new project's own LICENSE.

## Behavior tests

Tests exercise observable HTTP behavior using local QQ HTTPS and WSS servers: WebSocket pairing across restarts, recipient replacement attempts, multiple persisted keys with isolated recipients/status, one-time codes, expiration and rotation, concurrent binding consumption, recipient replacement through fresh codes, immediate CLI creation/revocation, revocations retained across restarts, database failures that deny authorization, secret-free metadata/logs/storage, input/authentication rejection, 404/405 responses, prefixed routes and logs, Markdown headings, link-button mapping and validation, Markdown byte limits, secret-free URL/parser diagnostics, proactive message formatting, concurrent token reuse, QQ business errors at HTTP 200, rate limits, timeouts without replay, client cancellation, panic recovery, and structured error logs without credentials or notification bodies. `kin-openapi` is used only by tests to validate the OpenAPI document and HTTP request/response contracts.

Gateway behavior tests cover Identify, heartbeats during slow processing, missed ACKs, reconnect/resume, queued replay ordering, duplicate suppression, invalid/expired sessions, token refresh, terminal permission errors, and malformed/unsafe gateway responses. Logs are checked for credentials and message-body leaks.

Process behavior tests build the actual command with the race detector and verify prefix normalization and rejection, startup failures, exit codes, one-time error reporting, SIGTERM shutdown, and completion of an in-flight notification before exit. Signal tests require POSIX and are skipped on Windows. Tests use local fixtures and temporary data; they do not contact a live QQ account.

Telemetry behavior tests decode real OTLP/HTTP protobuf exports with a local collector. They verify parent/child spans, log correlation, sampling, metric counts, bounded route/method labels, private-data omission, and process shutdown flushing. Configuration failures and rejected collector exports are exercised through the actual command.

## Official references

- [QQ bot setup and sandbox onboarding](https://bot.q.qq.com/wiki/)
- [Current API host and invocation guide](https://bot.q.qq.com/wiki/develop/api-v2/dev-prepare/api-call-guide.html)
- [Access tokens](https://bot.q.qq.com/wiki/develop/api-v2/dev-prepare/access-token.html)
- [Send private messages](https://bot.q.qq.com/wiki/develop/api-v2/autogen/api/v2_users_user_openid_messages.post.html)
- [Messaging permissions and quotas](https://bot.q.qq.com/wiki/develop/api-v2/server-inter/message/overview.html)
- [WebSocket connection protocol](https://bot.q.qq.com/wiki/develop/api-v2/dev-prepare/event-emit/websocket.html)
- [Gateway payloads and event intents](https://bot.q.qq.com/wiki/develop/api-v2/dev-prepare/event-emit/payload.html)
