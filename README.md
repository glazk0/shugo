<div align="center">

# Shugo

**Context-aware Discord auto-moderation powered by [Jev](https://docs.typesafe.ai).**

[![CI](https://github.com/glazk0/shugo/actions/workflows/ci.yml/badge.svg)](https://github.com/glazk0/shugo/actions/workflows/ci.yml)
[![Docker](https://github.com/glazk0/shugo/actions/workflows/docker.yml/badge.svg)](https://github.com/glazk0/shugo/actions/workflows/docker.yml)
[![Go version](https://img.shields.io/github/go-mod/go-version/glazk0/shugo)](go.mod)
[![Image](https://img.shields.io/badge/image-ghcr.io%2Fglazk0%2Fshugo-blue?logo=docker)](https://github.com/glazk0/shugo/pkgs/container/shugo)

</div>

Shugo reads every message in your Discord server and asks Jev, TypeSafe's
System One model, whether it breaks the rules. Jev sees more than the text. It
also gets the author's account age, how long they have been a member, and their
recent messages. A deterministic policy turns Jev's answers into a flag, a
delete or a timeout, and every action is reported to a log channel.

## Contents

- [Features](#features)
- [Quick start](#quick-start)
- [Discord setup](#discord-setup)
- [Configuration](#configuration)
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
> Start with `DRY_RUN=true` and a `LOG_CHANNEL_ID`. That way you can tune the
> thresholds on real traffic before Shugo deletes anything.

## Discord setup

1. Create an application in the
   [developer portal](https://discord.com/developers/applications) and add a
   bot.
2. Under **Bot → Privileged Gateway Intents**, enable
   **Message Content Intent**.
3. Invite the bot with the `bot` scope and these permissions: *View Channels*,
   *Read Message History*, *Manage Messages*, *Moderate Members*,
   *Send Messages* and *Embed Links*.
4. Move the bot's role above the roles of the members it should moderate.
   Discord rejects timeouts on members with a higher role.

## Configuration

Shugo is configured entirely through environment variables.
[`.env.example`](.env.example) lists them all.

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
| `QUEUE_TIMEOUT` | `5s` | How long a message waits for a free slot before it is dropped |
| `EVALUATION_TIMEOUT` | `20s` | Time limit for the Jev evaluation of a message, retries included |
| `ENFORCEMENT_TIMEOUT` | `10s` | Time limit for the delete and timeout calls, and separately for the report |
| `SHUTDOWN_TIMEOUT` | `30s` | How long shutdown waits for in-flight messages |
| `LOG_LEVEL` | `info` | `debug`, `info`, `warn` or `error` |

### Using OpenRouter

Jev is also served through OpenRouter's Decisions API, which accepts the same
request body. To use it, set
`JEV_ENDPOINT=https://openrouter.ai/api/alpha/decisions` and
`JEV_MODEL=typesafe/jev-1.13`, and put your OpenRouter key in
`TYPESAFE_API_KEY`.

## How it works

Shugo processes each new message, and each message an author edits, in six
steps:

1. **Filter.** Shugo skips DMs, system messages, bots, webhooks, and members
   with *Administrator* or *Manage Messages* in the channel. A message in a
   thread is checked against the parent channel's permissions.
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
   author, then posts an embed to `LOG_CHANNEL_ID`. With `DRY_RUN=true` it only
   posts the report.

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
| `internal/jev` | Jev HTTP client with retries on 429/529/5xx |
| `internal/history` | Bounded, TTL-based per-member message cache |
| `internal/moderation` | State building, Jev questions, decision policy |
| `internal/bot` | Discord adapter, message handling, enforcement, reports |

CI runs lint, race-enabled tests and govulncheck on every push and pull
request. Pushes to `main` and `v*` tags publish a multi-arch
(`linux/amd64`, `linux/arm64`) image to `ghcr.io/glazk0/shugo`.

## Limitations

- **History is in memory.** It resets when the bot restarts, which is fine with
  the default 15-minute TTL.
- **One replica.** Running several would need a shared store, such as Redis,
  behind the same `history` API.
- **Text only.** Attachment-only messages are remembered but not sent to Jev.

## Contributing

Issues and pull requests are welcome. Before you open a pull request:

1. Make sure `make lint` and `make test` pass.
2. Write commit messages in the
   [Conventional Commits](https://www.conventionalcommits.org) format, e.g.
   `fix(jev): fail fast when Retry-After passes the deadline`.

[`AGENTS.md`](AGENTS.md) has the full project conventions.
