<div align="center">

# Shugo

**Context-aware Discord auto-moderation powered by [Jev](https://docs.typesafe.ai).**

[![CI](https://github.com/glazk0/shugo/actions/workflows/ci.yml/badge.svg)](https://github.com/glazk0/shugo/actions/workflows/ci.yml)
[![Docker](https://github.com/glazk0/shugo/actions/workflows/docker.yml/badge.svg)](https://github.com/glazk0/shugo/actions/workflows/docker.yml)
[![Go version](https://img.shields.io/github/go-mod/go-version/glazk0/shugo)](go.mod)
[![Image](https://img.shields.io/badge/image-ghcr.io%2Fglazk0%2Fshugo-blue?logo=docker)](https://github.com/glazk0/shugo/pkgs/container/shugo)

[![Deploy on Railway](https://railway.com/button.svg)](https://railway.com/deploy/shugo?referralCode=8yi4-9&utm_medium=integration&utm_source=template&utm_campaign=generic)

</div>

Shugo reads every message in your Discord server and asks Jev, TypeSafe's
System One model, whether it breaks the rules. Jev sees more than the text. It
also gets the author's account age, how long they have been a member, and their
recent messages. A deterministic policy turns Jev's answers into a flag, a
delete or a timeout, and every action is reported to the log channels each
server picks with `/settings`.

## Contents

- [Features](#features)
- [Quick start](#quick-start)
- [Discord setup](#discord-setup)
- [Configuration](#configuration)
- [Server settings](#server-settings)
- [How it works](#how-it-works)
- [Development](#development)
- [Limitations](#limitations)
- [Contributing](#contributing)

## Features

- **Context-aware verdicts.** Account age, membership age and recent activity
  let Shugo catch raids, spam bursts and compromised accounts, not just bad
  words.
- **Graduated enforcement.** Borderline messages are flagged for a human.
  Confident violations are deleted, and severe ones also earn a timeout.
- **Predictable policy.** Jev gives probabilities, and you set the thresholds.
  The same answers always lead to the same action.
- **Edit-aware.** Editing a harmless message into spam gets it re-moderated.
- **Moderators are exempt.** Members with *Administrator* or
  *Manage Messages* are left alone, threads included.
- **Per-server settings.** Each server picks its own report channels and
  exempt channels and roles with `/settings`, stored in SQLite.
- **Dry-run mode.** Report what Shugo would do, without touching anything.
- **Built for production.** A single static binary in a distroless image, with
  bounded concurrency, retries with backoff, and a graceful shutdown that
  finishes in-flight messages.

## Quick start

You need a Discord bot token (see [Discord setup](#discord-setup)) and a
[TypeSafe](https://docs.typesafe.ai) API key.

```sh
git clone https://github.com/glazk0/shugo.git
cd shugo
cp .env.example .env   # fill in DISCORD_TOKEN and TYPESAFE_API_KEY
docker compose up -d --build
```

To skip the clone, run the published image directly:

```sh
docker run -d --name shugo --env-file .env ghcr.io/glazk0/shugo:latest
```

To run from source with Go (version in [`go.mod`](go.mod)), use `make run`.
It loads `.env` before starting the bot. A plain `go run` does not read
`.env`.

> [!TIP]
> Start with `DRY_RUN=true` and set a report channel with
> `/settings log-channel set`. That way you can tune the thresholds on real
> traffic before Shugo deletes anything.

## Discord setup

1. Create an application in the
   [developer portal](https://discord.com/developers/applications) and add a
   bot.
2. Under **Bot → Privileged Gateway Intents**, enable
   **Message Content Intent**.
3. Invite the bot with the `bot` and `applications.commands` scopes and these
   permissions: *View Channels*,
   *Read Message History*, *Manage Messages*, *Moderate Members*,
   *Send Messages* and *Embed Links*.
4. Move the bot's role above the roles of the members it should moderate.
   Discord rejects timeouts on members with a higher role.

## Configuration

The operator configures Shugo through environment variables, which apply to
every server. [`.env.example`](.env.example) lists them all. Settings that each
server chooses for itself are covered under
[Server settings](#server-settings).

| Variable | Default | Description |
|---|---|---|
| `DISCORD_TOKEN` | — | Bot token (required) |
| `TYPESAFE_API_KEY` | — | TypeSafe API key (required) |
| `JEV_ENDPOINT` | `https://api.typesafe.ai/v1/systemone` | System One endpoint |
| `JEV_MODEL` | `jev-latest` | Model alias or pinned version, e.g. `jev-1.13.0` |
| `DATABASE_PATH` | `shugo.db` (`/data/shugo.db` in the image) | SQLite file holding per-server settings |
| `DRY_RUN` | `false` | Report only, never delete or time out |
| `TIMEOUT_DURATION` | `10m` | Timeout length, at most `672h` (28 days) |
| `FLAG_RISK` | `0.5` | Risk needed to flag a message |
| `DELETE_RISK` | `0.85` | Risk needed to delete a message |
| `TIMEOUT_SEVERITY` | `0.75` | Severity that adds a timeout to a delete |
| `SUSPICION_THRESHOLD` | `0.8` | Author suspicion that raises the action by one step |
| `HISTORY_SIZE` | `10` | Messages remembered per member |
| `HISTORY_TTL` | `15m` | How long a remembered message stays relevant |
| `MAX_CONCURRENCY` | `16` | Maximum Jev requests in flight |
| `QUEUE_TIMEOUT` | `5s` | How long a message waits for a free slot before it is dropped |
| `EVALUATION_TIMEOUT` | `20s` | Time limit for the Jev evaluation of a message, retries included |
| `ENFORCEMENT_TIMEOUT` | `10s` | Time limit for the delete and timeout calls, and separately for the report |
| `SHUTDOWN_TIMEOUT` | `30s` | How long shutdown waits for in-flight messages |
| `LOG_LEVEL` | `info` | `debug`, `info`, `warn` or `error` |

### Persisting the database

Server settings live in a SQLite file, and Shugo applies any pending schema
migrations to it at startup. Keep the file on persistent storage, or every
server loses its settings when the container is replaced.

- **Docker Compose** mounts the `shugo-data` named volume at `/data`.
- **`docker run`**: add `-v shugo-data:/data`.
- **Railway**: attach a volume mounted at `/data`. Railway mounts volumes as
  root, so also set `RAILWAY_RUN_UID=0`, or the image's nonroot user cannot
  write to it.

`LOG_CHANNEL_ID` is no longer read. Shugo logs a warning at startup if it is
still set. Pick report channels with `/settings log-channel set` instead.

### Using OpenRouter

Jev is also served through OpenRouter's Decisions API, which accepts the same
request body. To use it, set
`JEV_ENDPOINT=https://openrouter.ai/api/alpha/decisions` and
`JEV_MODEL=typesafe/jev-1.13`, and put your OpenRouter key in
`TYPESAFE_API_KEY`.

## Server settings

Members with *Manage Server* configure Shugo for their server with `/settings`.
Replies are only visible to the person who ran the command. Server admins can
let other roles use it under *Server Settings → Integrations*.

| Command | Effect |
|---|---|
| `/settings show` | Show the current settings |
| `/settings log-channel set kind channel` | Send flag reports, or delete and timeout reports, to a channel |
| `/settings log-channel clear kind` | Remove that channel |
| `/settings exempt-channel add/remove channel` | Skip moderation in a channel and its threads |
| `/settings exempt-role add/remove role` | Skip moderation for members with a role |

Flags need a human, while deletes and timeouts are already done, so the two
kinds of report can go to separate channels. When only one is set, it receives
both. With neither set, Shugo still moderates but reports nothing. Shugo needs
*Send Messages* and *Embed Links* in the report channels.

## How it works

Shugo processes each new message, and each message an author edits, in six
steps:

1. **Filter.** Shugo skips DMs, system messages, bots, webhooks, and members
   with *Administrator* or *Manage Messages* in the channel. A message in a
   thread is checked against the parent channel's permissions. It also skips
   the channels (threads included) and roles that the server exempted.
2. **Remember.** The message goes into an in-memory history keyed by guild and
   member, which keeps the last `HISTORY_SIZE` messages for `HISTORY_TTL`. An
   edit replaces the original version instead of adding a second entry.
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

5. **Decide.** A deterministic policy turns the answers into an action. The
   first matching row wins:

   | Condition | Action |
   |---|---|
   | risk ≥ `DELETE_RISK` and (severity ≥ `TIMEOUT_SEVERITY` or suspicious) | delete + timeout |
   | risk ≥ `DELETE_RISK` | delete |
   | risk ≥ `FLAG_RISK` and suspicious | delete |
   | risk ≥ `FLAG_RISK` | flag (report only) |

   *Risk* is `1 − P(none)`, and *suspicious* means
   `P(suspicious_author) ≥ SUSPICION_THRESHOLD`. A suspicious author never
   triggers an action alone. It only escalates a message that is already
   borderline. When Jev's top choice is `none` but the risk is still high
   enough to act, the report names the most likely violation instead.
6. **Enforce and report.** Shugo deletes the message and/or times out the
   author, then posts an embed to the server's report channel for that
   action. With `DRY_RUN=true` it only posts the report.

## Development

```sh
make test   # race-enabled tests
make lint   # golangci-lint
make cover  # coverage report
make vuln   # govulncheck
make build  # binary in bin/shugo
```

| Package | Responsibility |
|---|---|
| `cmd/shugo` | Wiring, signal handling, graceful shutdown |
| `internal/config` | Environment parsing and validation |
| `internal/database` | SQLite connection and embedded schema migrations |
| `internal/guild` | Guilds and their settings: report channels and exemptions |
| `internal/commands` | Slash command router and one file per command, such as `/settings` |
| `internal/jev` | Jev HTTP client with retries on 429/529/5xx |
| `internal/history` | Bounded, TTL-based per-member message cache |
| `internal/moderation` | State building, Jev questions, decision policy |
| `internal/bot` | Discord adapter, message handling, enforcement, reports |

The database holds three tables: `guilds`, kept in step with the servers
Shugo is in (`removed_at` marks one it left, with its settings kept for a
re-invite); `guild_settings`; and `guild_exemptions`. Each row carries
`created_at`, and the mutable ones `updated_at`, as UTC ISO 8601 text. WAL
journaling and foreign keys are enabled on every connection.

To add a slash command, write a constructor returning a `commands.Command`
in its own file under `internal/commands`: its Discord definition plus one
handler per subcommand path, such as `"log-channel set"`. Then pass it to
`commands.NewRouter` in `cmd/shugo`. The router refuses to start if a
subcommand has no handler.

Schema changes go in a new file in `internal/database/migrations`, named with
the next number (`0002_description.sql`). Shugo applies pending migrations in
order at startup and records the version in `PRAGMA user_version`. Never edit
a migration that has already shipped.

CI runs lint, race-enabled tests and govulncheck on every push and pull
request. Pushes to `main` and `v*` tags publish a multi-arch
(`linux/amd64`, `linux/arm64`) image to `ghcr.io/glazk0/shugo`.

## Limitations

- **History is in memory.** It resets when the bot restarts, which is fine with
  the default 15-minute TTL.
- **One replica.** Running several would need a shared store, such as Redis,
  behind the same `history` API, and a shared database in place of the local
  SQLite file.
- **Text only.** Attachment-only messages are remembered but not sent to Jev.

## Contributing

Issues and pull requests are welcome. Before you open a pull request:

1. Make sure `make lint` and `make test` pass.
2. Write commit messages in the
   [Conventional Commits](https://www.conventionalcommits.org) format, e.g.
   `fix(jev): fail fast when Retry-After passes the deadline`.

[`AGENTS.md`](AGENTS.md) has the full project conventions.
