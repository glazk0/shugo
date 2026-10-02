# Shugo

Shugo is a Discord auto-moderation bot. It sends every guild message to
[Jev](https://docs.typesafe.ai), TypeSafe's System One model, along with
context about the author, then acts on the answers.

## How it works

Each `MESSAGE_CREATE` event goes through these steps:

1. **Filter.** Shugo skips DMs, system messages, bots, webhooks, and members
   with *Administrator* or *Manage Messages* in the channel.
2. **Remember.** The message goes into an in-memory history keyed by guild and
   member, which keeps the last `HISTORY_SIZE` messages for `HISTORY_TTL`.
3. **Describe.** Shugo builds a JSON state from:
   - the message content, attachment count and mentions
   - the account age, read from the user ID snowflake (e.g. `5 hours (brand new account)`)
   - the time since the member joined the server (e.g. `1 minute (just joined)`)
   - the member's recent messages, with which channel each was sent in and how long ago
   - computed signals: links, identical recent messages, channels used, and messages in the last minute
4. **Ask Jev.** All three questions go out in a single request:
   | Key | Type | Question |
   |---|---|---|
   | `violation` | Choice | none, spam, scam, advertising, harassment, hate, sexual |
   | `severity` | Score | severe → moderate → minor → harmless |
   | `suspicious_author` | Noul | Does the author look like a spam bot, raider or compromised account? |
5. **Decide.** A deterministic policy turns the answers into an action:
   | Condition | Action |
   |---|---|
   | risk ≥ `DELETE_RISK` and (severity ≥ `TIMEOUT_SEVERITY` or suspicious) | delete + timeout |
   | risk ≥ `DELETE_RISK` | delete |
   | risk ≥ `FLAG_RISK` and suspicious | delete |
   | risk ≥ `FLAG_RISK` | flag (report only) |

   Here *risk* is `1 − P(none)` and *suspicious* means
   `P(suspicious_author) ≥ SUSPICION_THRESHOLD`. A suspicious author on its own
   never triggers an action. It only raises the action for a message that is
   already borderline.
6. **Enforce and report.** Shugo deletes the message and/or times the author
   out, then posts an embed to `LOG_CHANNEL_ID`. With `DRY_RUN=true` it only
   posts the report.

## Discord setup

1. Create an application in the [developer portal](https://discord.com/developers/applications)
   and add a bot.
2. Under **Bot → Privileged Gateway Intents**, enable **Message Content Intent**.
3. Invite the bot with the `bot` scope and these permissions: *View Channels*,
   *Read Message History*, *Manage Messages*, *Moderate Members*,
   *Send Messages* and *Embed Links*.
4. Put the bot's role above the roles of the members it should moderate.
   Discord rejects timeouts on members with a higher role.

## Configuration

Shugo reads its configuration from environment variables. `.env.example` lists them all.

| Variable | Default | Description |
|---|---|---|
| `DISCORD_TOKEN` | — | Bot token (required) |
| `TYPESAFE_API_KEY` | — | TypeSafe API key (required) |
| `JEV_ENDPOINT` | `https://api.typesafe.ai/v1/systemone` | System One endpoint |
| `JEV_MODEL` | `jev-latest` | Model alias or pinned version, e.g. `jev-1.13.0` |
| `LOG_CHANNEL_ID` | — | Channel that receives reports; empty disables them |
| `DRY_RUN` | `false` | Report only, never delete or time out |
| `TIMEOUT_DURATION` | `10m` | Timeout length, at most `672h` (28 days) |
| `FLAG_RISK` | `0.5` | Risk needed to flag a message |
| `DELETE_RISK` | `0.85` | Risk needed to delete a message |
| `TIMEOUT_SEVERITY` | `0.75` | Severity that adds a timeout to a delete |
| `SUSPICION_THRESHOLD` | `0.8` | Author suspicion that raises the action by one step |
| `HISTORY_SIZE` | `10` | Messages remembered per member |
| `HISTORY_TTL` | `15m` | How long a remembered message stays relevant |
| `MAX_CONCURRENCY` | `16` | Maximum Jev requests in flight |
| `EVALUATION_TIMEOUT` | `20s` | Time limit per message, retries included |
| `LOG_LEVEL` | `info` | `debug`, `info`, `warn` or `error` |

Jev is also served through OpenRouter's Decisions API, which accepts the same
request body. To use it, set `JEV_ENDPOINT=https://openrouter.ai/api/alpha/decisions`,
`JEV_MODEL=typesafe/jev-1.13`, and put your OpenRouter key in `TYPESAFE_API_KEY`.

## Running

```sh
cp .env.example .env   # then fill in the tokens
docker compose up -d --build
```

Or run it directly with Go 1.27+:

```sh
set -a; . ./.env; set +a
make run
```

Start with `DRY_RUN=true` to tune the thresholds on real traffic before the bot
deletes anything.

## Development

```sh
make test   # go test -race ./...
make cover  # coverage report
make lint   # golangci-lint v2
make vuln   # govulncheck
```

| Package | Responsibility |
|---|---|
| `cmd/shugo` | Wiring, signal handling, graceful shutdown |
| `internal/config` | Environment parsing and validation |
| `internal/jev` | Jev HTTP client with retries on 429/529/5xx |
| `internal/history` | Bounded, TTL-based per-member message cache |
| `internal/moderation` | State building, Jev questions, decision policy |
| `internal/bot` | Discord adapter, message handling, enforcement, reports |

CI runs lint, race-enabled tests and govulncheck on every push and pull
request. Pushes to `main` and `v*` tags publish a multi-arch image to
`ghcr.io/glazk0/shugo`.

The message history lives in memory, so it resets when the bot restarts. That
is fine with the default 15-minute TTL. Running more than one replica would
need a shared store such as Redis behind the same `history` API.
