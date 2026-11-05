# notify-qq

Send notifications from your scripts to QQ private chats or groups through an official QQ bot. Messages support Markdown and optional link buttons. Each API key is paired with one user or group.

## Set up your QQ bot

Create a bot in the [QQ Bot developer console](https://q.qq.com/) and enable the scenarios you need: **message-list private chat / 单聊** or **group chat / 群聊**. You need its AppID and AppSecret.

For sandbox testing, configure your test QQ account or group in the developer console. Add the bot to your message list or target group and enable **allow proactive messages / 允许主动发送** where required. Configure the server's outbound IP whitelist if your application requires it.

The server needs outbound HTTPS and WebSocket access to QQ. No public QQ callback URL is needed.

## Install

### Docker Compose

Create a `.env` file:

```dotenv
QQ_APP_ID=your-app-id
QQ_APP_SECRET=your-app-secret
```

Create `compose.yaml` in the same directory:

```yaml
services:
  notify-qq:
    image: ghcr.io/ceclin/notify-qq:preview
    restart: unless-stopped
    env_file: .env
    ports:
      - "127.0.0.1:8080:8080"
    volumes:
      - notify-data:/data

volumes:
  notify-data:
```

`preview` follows prereleases. You can use a specific [release tag](https://github.com/ceclin/notify-qq/releases) to pin the version.

```sh
chmod 600 .env
docker compose up -d
```

The HTTP API is available at `http://127.0.0.1:8080`. For scripts on another machine, place an HTTPS reverse proxy in front of this address.

The image includes a Docker readiness check; `docker compose ps` shows its health status.

### Binary

Alternatively, download the Linux amd64 or arm64 archive from [Releases](https://github.com/ceclin/notify-qq/releases), extract it, and create `.env` as shown above:

```sh
./notify-qq serve
```

The default listener is `127.0.0.1:8080`, and data is saved in `./data`. Run `./notify-qq` or add `--help` to any command for usage. For the key commands below, replace `docker compose exec notify-qq /notify-qq` with `./notify-qq`.

## Pair a recipient

Create a key for the script that will send notifications:

```sh
docker compose exec notify-qq /notify-qq keys create --name backup
```

Save the API key for your script. Send the entire pairing code within 5 minutes, without a command prefix:

- For a private chat, paste it directly into your chat with the bot.
- For a group, add the bot first, then **@ the bot** and paste the code in the same message.

```text
nqp_...
```

The bot confirms a successful binding in that chat or group. The API key and pairing code are shown only once; each pairing code can be used once. Give each script its own key, and share its pairing code only with the intended user or group.

The bot explains when a pairing code is invalid or expired. Other private messages and group messages that @ the bot receive a short acknowledgement, such as “Got it.”

## Send a notification

In your script's environment, set the service URL and the API key returned above:

```sh
export NOTIFY_URL=http://127.0.0.1:8080
export NOTIFY_API_KEY='your-api-key'

curl --fail-with-body --max-time 25 "$NOTIFY_URL/notify" \
  -H "Authorization: Bearer $NOTIFY_API_KEY" \
  -H 'Content-Type: application/json' \
  -d '{
    "content": "## Backup completed\n\nAll files were uploaded successfully.",
    "buttons": [{"label": "View logs", "url": "https://example.com/logs"}]
  }'
```

A successful response looks like:

```json
{"status":"accepted","message_id":"QQ_MESSAGE_ID","timestamp":"2026-10-11T20:00:00+08:00"}
```

`accepted` means QQ accepted the message. Device notifications still depend on the recipient's QQ settings.

- `content`: Markdown, up to 4000 UTF-8 bytes.
- `buttons`: optional, up to five links. Labels are 1–10 characters; URLs must use HTTP or HTTPS. Configure allowed link domains in the QQ console when needed.

For a ready-to-use shell helper, see [examples/notify.sh](examples/notify.sh) (requires `curl` and `jq`).

## Check status

```sh
curl --fail-with-body "$NOTIFY_URL/status" \
  -H "Authorization: Bearer $NOTIFY_API_KEY"
```

Expect `paired: true` and `websocket_connected: true`. The `recipient` object contains `type` (`user` or `group`) and `openid`; it is `null` before pairing. If the binding confirmation did not arrive, use this endpoint to check whether pairing succeeded.

| Endpoint | Authentication | Purpose |
|---|---|---|
| `POST /notify` | Bearer API key | Send a notification to the key's paired recipient |
| `GET /status` | Bearer API key | Check pairing and the QQ connection |
| `GET /readyz` | None | Check local service and storage readiness |
| `GET /healthz` | None | Check whether the process is alive |

Health and readiness checks do not verify QQ delivery. See [api/openapi.yaml](api/openapi.yaml) for the full HTTP API reference.

## Manage keys

```sh
docker compose exec notify-qq /notify-qq keys list
docker compose exec notify-qq /notify-qq keys pair KEY_ID
docker compose exec notify-qq /notify-qq keys revoke KEY_ID
```

Use `keys pair` when a pairing code expires or to change a key's recipient. It issues a fresh 5-minute code; the existing recipient stays active until the new recipient binds. Use the `id` from `keys list` to pair or revoke a key. Key changes take effect without restarting the service.

Commands print readable text and tables by default; add `--json` for scripts. `keys list` shows active keys; add `--all` to include revoked keys.

Commands only report errors on stderr by default. Add `--verbose` to include success and debug diagnostics.

## Configuration

Settings are read from `.env` in the working directory and from environment variables. Environment variables take precedence; restart the service after changing them.

| Variable | Purpose / default |
|---|---|
| `QQ_APP_ID` | Required: QQ bot AppID |
| `QQ_APP_SECRET` | Required: QQ bot AppSecret |
| `LISTEN_ADDR` | `127.0.0.1:8080`; the Docker image uses `0.0.0.0:8080` inside the container |
| `DATA_DIR` | `./data`; the Docker image uses `/data` |
| `LOG_LEVEL` | `info` for service logs; use `debug` for troubleshooting |
| `API_PATH_PREFIX` | Empty by default; set `/api/notify-qq` to serve under that path |

When using a path prefix, include it in `NOTIFY_URL`, forward it unchanged through the reverse proxy, and update health-check URLs accordingly.

Optional OpenTelemetry settings are listed in [.env.example](.env.example).

## Back up and update

Keep the entire data directory or Docker volume: it contains API keys and recipient bindings. Stop the service and any key-management commands before taking a filesystem backup. `docker compose down -v` deletes this data.

To update the container while retaining its data:

```sh
docker compose pull
docker compose up -d
```

For a binary installation, stop the service, back up `DATA_DIR`, replace the executable, and start it again.

## Troubleshooting

```sh
docker compose logs --tail 100 notify-qq
```

| Symptom | What to check |
|---|---|
| HTTP 401 | Use the API key returned by `keys create` in the Bearer header |
| HTTP 409 | Pair the key's recipient first |
| HTTP 429 | QQ rate limit; wait for the returned `Retry-After` interval |
| HTTP 502 | Check the returned QQ error code, bot permissions, outbound IP whitelist, and allowed link domains |
| HTTP 504 or a network timeout | Delivery is uncertain; check before retrying to avoid duplicates |
| `websocket_connected: false` | Check AppID/AppSecret, bot availability, and outbound HTTPS/WebSocket connectivity |

Notifications are not automatically retried. Error responses include a `request_id` for finding the corresponding log entry.
