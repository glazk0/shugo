# AGENTS.md

Shugo is a Discord auto-moderation bot. It sends each guild message, with
context about its author, to Jev (TypeSafe's System One model), then a
deterministic threshold policy turns Jev's typed answers into a flag, delete or
timeout. `README.md` covers the moderation pipeline, every environment variable
and the package layout.

## Tech

- Go, standard library first (`log/slog`, `net/http`). The only direct
  dependency is `github.com/bwmarrin/discordgo`.
- `internal/jev` calls Jev over plain HTTP; there is no SDK.
- Configuration comes from environment variables alone, parsed in
  `internal/config`.
- Deployed as a Docker image through `compose.yaml`.

## Lint and tests

A change is done when `make lint` and `make test` both pass clean; CI runs the
same two gates. `.golangci.yml` is the source of truth for linters and
formatting. Fix findings at the source, and reserve `//nolint` for false
positives, always with a reason.

Every function and method carries a doc comment that lists each parameter with
its type:

```go
// Record stores e for the given member and returns their earlier entries.
//
// Parameters:
//   - guildID (string): guild the message was sent in.
//   - e (Entry): the message to remember.
```

Inline comments explain non-obvious reasoning only.

## Commits

Use Conventional Commits: `type(scope): summary`, imperative and lowercase,
for example `fix(jev): fail fast when Retry-After passes the deadline`. Types
are `feat`, `fix`, `refactor`, `test`, `docs`, `chore`, `ci` and `build`. The
scope is optional and names the package. Keep each commit to one logical
change, and use the body to explain why.
