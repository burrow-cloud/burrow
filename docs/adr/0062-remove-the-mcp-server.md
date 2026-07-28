# ADR-0062: Remove the MCP server from the tree

## Status

✅ Accepted

## TL;DR

Burrow used to give a coding agent its instructions through an **MCP server** — a separate program
speaking the Model Context Protocol. [ADR-0049](0049-burrow-agent-scoped-cli-control-channel.md)
replaced that with **`burrow-agent`**, a small command-line tool built for agents, and v0.12 stopped
shipping the MCP server. But its code is still in the repository, and `burrow mcp install` still wires
it up for anyone who runs it.

This removes it: the `mcp` package, the `burrow-mcp` binary, and the `burrow mcp` command.

The reason is not that the old server is dangerous. A program nobody builds or runs is not reachable.
The reason is that **there are currently two ways to give an agent instructions, and only one of them
gets thought about.** Every rule about what an agent may and may not do has to be enforced in both
places, forever, and the retired one is the one a reviewer will forget. Keeping code that looks
maintained but isn't is the failure that eventually bites — someone adds a capability to the wrong
surface, and nothing objects.

**Supersedes ADR-0049's provision that MCP may be kept as an optional layer**; the rest of that record
— `burrow-agent` as the agent's control channel, and the credential boundary it inherited — stands
unchanged.

## Context

[ADR-0049](0049-burrow-agent-scoped-cli-control-channel.md) retired the MCP server as the agent's
primary interface in favour of `burrow-agent`: a capability-reduced, JSON-first surface that an agent
can pipe, `grep` and `jq`, installed as one binary. It left the door open — "MCP, if kept, is an
optional layer" — which was the right call at the time, when the new channel was unproven.

It is no longer unproven. `burrow-agent` shipped in v0.12, the MCP server was dropped from releases,
and the CLI now tells users `burrow agent <tool> install`. The new channel is what people use.

What remains is residue: the `mcp` package, `cmd/burrow-mcp`, and a `burrow mcp [tool] [install]`
command group still in the operator CLI. Nothing else in the tree depends on the package —
`cmd/burrow-mcp` and the package's own tests are its only importers, and `burrow-agent` does not touch
it. It is decoupled, retired, and still present.

That combination has three costs.

**Every invariant about the agent's surface must be asserted twice.** The agent-facing surface is
deliberately narrower than the operator CLI: it can deploy, scale, roll back, read logs and manage an
app's secrets, but it cannot create namespaces, grant RBAC, install, upgrade or join nodes. That
narrowness is a security property — an agent reads untrusted input by nature, so it must not be able
to widen its own access on the user's cluster. With two surfaces, any guard on that property needs two
allow-lists kept in step, and the retired surface is the one that will drift.

**A retired surface is still installable.** `burrow mcp install` remains a supported command, so a user
following an older guide can wire up the channel that no longer receives attention. That is the one
place where "retired" and "reachable" differ.

**Dead code that looks alive misleads.** A contributor adding an agent capability has no signal that
one of the two obvious places to add it is the wrong one.

None of this is a vulnerability. It is the ordinary cost of carrying a second implementation of
something that already has a maintained one.

## Decision

### 1. Remove the MCP server entirely

Delete the `mcp` package, the `cmd/burrow-mcp` binary, and the `burrow mcp` command group. Update the
documentation that references them.

### 2. `burrow mcp` fails with direction, not confusion

For at least one release, `burrow mcp ...` exits with an error naming its replacement — `burrow agent
<tool> install` — rather than an "unknown command". Someone reaching this is following an old guide and
should be pointed at the current one, not left to guess.

### 3. Rules about the agent's surface are stated once

With a single agent-facing surface, any test or invariant covering what an agent may do is written
against `burrow-agent` alone. One place to assert, one place to review, one place to get wrong.

## Consequences

- **One agent surface instead of two**, so a rule about agent capability has one enforcement point
  rather than two that can disagree.
- **The retired install path closes.** No supported command wires up an unmaintained channel.
- **Anyone still running an old `burrow-mcp` binary is unaffected until they upgrade**, and then it is
  gone. Given it was dropped from releases in v0.12, the population is small and has already been told.
- **The repository loses a working MCP implementation.** If MCP is wanted again — and it may be; it is
  a reasonable protocol with real adoption — it returns as a **thin adapter over `burrow-agent`'s
  surface**, not as a parallel implementation with its own tool registry. That is the shape that does
  not reintroduce the problem this record removes. Nothing here rejects the protocol; it rejects
  maintaining two independent surfaces.
- **Some documentation and older guides will reference a command that no longer exists**, which §2's
  error message exists to absorb.

## Rejected alternatives

- **Keep it as an optional layer** (ADR-0049's position). Correct when `burrow-agent` was new and
  unproven — a fallback has value while the replacement is still being trusted. It is not new any
  more, and the fallback now costs a second surface that every agent-capability rule must be enforced
  against, in service of a channel nothing ships.
- **Deprecate more loudly but keep the code.** Pays the same maintenance and drift cost, indefinitely,
  in exchange for postponing the decision. The signal a contributor needs is that the wrong place to
  add a capability does not exist, and a deprecation notice does not provide it.
- **Reimplement MCP now as a thin adapter over `burrow-agent`.** Plausible and possibly the eventual
  answer, but it is work in service of demand that has not appeared. Removing first and adding back
  on request is cheaper than maintaining an implementation on the chance it is wanted.
- **Leave the `burrow mcp` command and remove only the server.** Leaves a command that installs
  nothing, which is worse than either keeping it or removing it.
