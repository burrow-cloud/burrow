# ADR-0019: Cobra for the CLI command framework; stdlib tabwriter for output

## Status

Accepted.

## Context

The `burrow` CLI rolls its own command handling: the standard `flag` package, a hand-rolled
`splitArgs` "positionals first, then flags" convention, and manual sub-dispatch per command
group (the `login`/`logout`/`list` switch in `registry.go` is the first taste). v0.1's flat
verb set carried this fine, but v0.2 deepens the command tree substantially —
`expose`/`unexpose`, `domain add`/`remove`, `dns configure`, `ingress install`,
`guard list`/`set` — and per-group manual dispatch, flag parsing, and usage strings do not
scale: every new level is hand-written help and argument handling, and the bespoke
flag-ordering convention diverges from what users expect.

CLAUDE.md is explicit that the dependency graph stays small and **every dependency must
justify itself** — so a framework has to earn its place rather than be adopted by default.

## Decision

**Adopt [spf13/cobra](https://github.com/spf13/cobra) for the CLI command structure** —
commands, nested subcommands, flag parsing (via pflag), generated help/usage, and shell
completion. Cobra is the de-facto standard in the Kubernetes ecosystem Burrow already lives
in (kubectl and the client-go world), is **Apache-2.0** — matching the Apache-2.0 client
surface, so no license-boundary concern ([ADR-0001](0001-license-and-dco.md)) — and directly
retires the hand-rolled dispatch, `splitArgs`, and usage strings. It is the one dependency
whose job (a growing subcommand tree) it does better than we can.

**Keep human-readable output on the standard library:** `text/tabwriter` for aligned tables,
plus the existing `--json` for structured results. No table or styling dependency
(go-pretty, charmbracelet/lipgloss) is taken on now — stdlib covers tabular output, and a
styling library waits until an interactive or richly-formatted surface actually warrants it.

The CLI remains Apache-2.0 and imports no FSL packages; cobra is a **client-surface
dependency only** — the MCP server (`cmd/burrow-mcp`, `mcp/`) and `burrowd` do not depend on
it.

## Consequences

- One new direct dependency (cobra) and its small transitive set (notably pflag), scoped to
  `cmd/burrow`. burrowd and the MCP server are unaffected.
- **Migration:** the existing commands (`install`, `upgrade`, `registry`, `deploy`, `status`,
  `logs`, `rollback`, `scale`) become cobra commands, preserving their flags and behavior.
  The test entry point `run(ctx, args, stdout, stderr)` is preserved by having it build the
  cobra root command and execute it with `args`, so the existing CLI tests keep working
  unchanged. This migration lands before the v0.2 command surface is built, so new commands
  are written on cobra from the start rather than hand-rolled and rewritten.
- pflag uses POSIX-style flags and interleaves flags with positionals, so the bespoke
  "positionals first, then flags" rule (`splitArgs`) is retired — flags may appear anywhere.
  A minor, more-standard UX change.
- Cobra gives generated `--help` per command and shell completion for free, improving the CLI
  as its surface grows.

## Rejected alternatives

- **Keep rolling our own.** Rejected: the subcommand tree is about to deepen, and per-group
  manual dispatch plus hand-written usage strings multiply the maintenance with every level —
  the strain is already visible in `registry.go`.
- **urfave/cli.** A reasonable, lighter option, but with weaker shell completion and far less
  ubiquity in the Kubernetes ecosystem Burrow sits in; cobra's standardization wins given the
  surroundings (and the client-go dependency already present).
- **A table/styling library now** (go-pretty, charmbracelet/lipgloss). Deferred: stdlib
  `text/tabwriter` covers tabular output with no new dependency; revisit only when an
  interactive or styled surface is actually built.
- **Ship the framework choice without an ADR.** Rejected: adding a foundational CLI
  dependency against the "justify every dependency" rule is exactly the kind of decision that
  belongs on the record.
