# ADR-0078: The CLI points at a target, and `burrow auth` is how you choose one

## Status

🟡 Proposed

## TL;DR

`burrow` decides what to talk to by reading whatever kubeconfig context happens to be current. That
works while a cluster is the only thing it can talk to, and stops working the moment Burrow Cloud
exists.

- **A target is where the control plane is.** Either Burrow Cloud, or a Kubernetes cluster you have a
  kubeconfig for.
- **`burrow auth login` asks which**, defaulting to Burrow Cloud. Choosing a cluster lists the
  contexts you already have.
- **Authenticating is not installing.** Install is a one-time, cluster-admin act on a cluster. Auth is
  per-person and repeatable, so the second person to use a cluster never installs anything.
- **Every command that changes something names the target it changed.** The failure this design
  creates is acting on the wrong one.

Generalizes the implicit "current kubeconfig context" that [ADR-0014](0014-cli-auth-via-kubeconfig.md)
established, and gives [ADR-0038](0038-scoped-agent-credential.md)'s scoped agent credential a place
in a world with more than one kind of target. Cloud ADR-0028 decides what authenticating to the
Burrow Cloud target involves. Supersedes nothing.

## Context

`burrow` authenticates by proxying through the Kubernetes API server with the operator's own
kubeconfig (ADR-0014). Which cluster that is has never been stated anywhere — it is the kubeconfig's
current context, chosen by whatever the person last ran `kubectl config use-context` on, with a
`--context` flag on some commands as an override. There is no concept above it.

That was correct while a Kubernetes cluster was the only thing a control plane could live in. It
breaks in three ways now.

**Burrow Cloud has no kubeconfig at all.** A managed tenant owns no cluster, which is the product. So
the mechanism the CLI uses to decide what to talk to cannot even describe the managed case, and
`burrow` therefore does not work against it — not because a feature is missing, but because the
question "what am I pointed at" has only ever had one kind of answer.

**A person can plausibly have both.** Someone running a cluster who wants to try the managed product
has a kubeconfig *and* a cloud tenant. Inference alone cannot resolve that safely: whichever way it
guesses, it is silently right for one of them and silently wrong for the other, and the wrong answer
is a deploy that went somewhere unintended.

**The interesting journey crosses the boundary.** A self-hoster who finds a cluster more work than it
is worth should be able to move to the managed product without changing tools, relearning a CLI, or
reinstalling anything. That journey only exists if "where Burrow is" is a thing you can point at
something else, rather than a property of the ambient environment.

There is also a smaller confusion worth naming, because it is already live. `burrow install` mints a
scoped kubeconfig — but for the **agent** (`cmd/burrow/agentcred.go`, ADR-0038), written under
`~/.burrow/agents/`. It does not mint anything for the human, whose credential to a self-hosted
cluster is the kubeconfig they already had. So installing and authenticating are already different
acts, and nothing says so.

## Decision

### 1. A target is where the control plane is, and there are two kinds

- **Burrow Cloud** — the managed product. The credential is a token obtained by signing in (cloud
  ADR-0028).
- **A Kubernetes cluster** — any cluster the person has a kubeconfig context for, with Burrow
  installed in it. The credential is that kubeconfig, used exactly as ADR-0014 already uses it.

Two kinds, not three. A managed control plane operated on somebody else's cluster is a shape the
roadmap keeps open, and it is deliberately **not** modelled here: it is not built, not designed, and
inventing a target kind for it now would be inventing the thing it is a target for.

**A kubeconfig target stores the context NAME and never a copy of the credential.** The kubeconfig
remains the single source of truth, so rotating it, re-issuing a certificate, or having it managed by
a cloud provider's CLI all continue to work with nothing in `~/.burrow/` going stale. A copied
credential is a credential nobody remembers to rotate.

### 2. `burrow auth login` asks where, and defaults to the managed product

```
? Where do you use Burrow?  [Use arrows to move, type to filter]
> burrow-cloud.dev
  Other
```

Deliberately the shape `gh auth login` opens with, because it is a question people have already been
asked and answered. The managed product is the default and the first item: someone who came for the
managed product should reach it by pressing return.

**Choosing `Other` reads the kubeconfig** and offers the contexts found there, so a person picks a
cluster by a name they already recognise rather than typing a server URL.

**If there is no kubeconfig, the CLI says exactly that and stops.** Not a prompt for a URL, not a
degraded mode: a Kubernetes target requires a configured Kubernetes client, and a person who has
none needs to hear that rather than be walked further into a flow that cannot complete. The message
names what is missing and that the managed product needs none of it.

The selection is stored and becomes the active target.

### 3. Authenticating is not installing

They are separate commands because they are separate acts, done by different people at different
times:

| | `burrow install` | `burrow auth login` |
| --- | --- | --- |
| What it is | Putting a control plane into a cluster | Choosing which control plane you talk to, and proving who you are |
| How often | Once per cluster | Once per person per target, and again whenever that changes |
| Needs | cluster-admin | Access to the target |

**The second person to use a cluster never installs anything.** They bring their own kubeconfig
context and select it. Any design where authenticating implies installing would have them re-run a
cluster-admin operation against a cluster that already has one, which is at best a no-op and at worst
an unintended upgrade.

Install continues to act on a kubeconfig context, since installing into Burrow Cloud is not a thing
that can be asked for.

### 4. The active target is visible, switchable, and named on every change

- `burrow auth status` lists the targets that are configured, which is active, and what each one is.
- `burrow auth switch` changes the active one without re-authenticating.
- **Every command that changes something states the target it changed**, in its own output.

The last one is the point rather than a nicety. The failure this record introduces is acting on the
wrong target — deploying to a cluster while believing you were deploying to the managed product, or
the reverse. It cannot be prevented by design, since both are legitimate things to do, so it has to
be made **immediately visible**: the recoverable form of that mistake is noticing in the same breath,
and the unrecoverable form is discovering it later from somebody else.

The existing `--context` flag continues to work, as a per-invocation override for a kubeconfig
target.

## Consequences

**The migration a self-hoster wants becomes a single command.** Someone who decides a cluster is more
trouble than it is worth runs `burrow auth login`, picks the managed product, and keeps every other
command they know. Nothing is reinstalled and no second tool is learned. That journey is the reason
this is a target model rather than a flag.

**The CLI grows state it did not have.** Today it is stateless with respect to targeting — it reads
the ambient kubeconfig and acts. Now there is a file recording targets and which is active, which can
be stale, edited by hand, or out of step with a kubeconfig that has moved on. `burrow auth status`
exists to make that legible, and kubeconfig targets storing only a context name is what keeps the
staleness shallow.

**Two people on one cluster now have separate, legible identities to it**, since each authenticates
with their own kubeconfig rather than sharing whatever the installer had. That was already true and
was previously invisible.

**The open-source CLI carries a command group whose default target is a commercial product.** That is
a real thing to be uncomfortable about and is accepted deliberately: the alternative is a separate
managed CLI, which fragments the surface and makes the migration above impossible. The self-hosted
path stays fully functional with no account, no network call to the managed product, and no
degradation — `Other` is one keystroke away, and nothing about choosing it is second-class.

**A person with no kubeconfig and no interest in one is never shown a Kubernetes concept.** They
press return at the first prompt and are done. This is the property that decides whether the managed
product is usable by people who do not run clusters, and it is why the default is the managed one.

## Rejected alternatives

**Inferring the target.** Use a stored cloud credential if one exists, otherwise the kubeconfig. It
resolves correctly for the two easy populations — the managed user with no cluster, the self-hoster
who never signed in — and silently guesses for anyone with both. The guess is wrong half the time and
its failure mode is a deploy landing somewhere unintended, which is exactly the class of mistake
worth spending a prompt to avoid.

**A `--target` flag on every command, with no stored selection.** Explicit and unambiguous, and it
taxes every single invocation to protect against a mistake that is rare per-command. It also makes
the managed product's experience worse than the self-hosted one, which inverts the intent.

**A separate `burrow-cloud` CLI.** Clean separation, and it forecloses the migration this record
exists to enable: a self-hoster moving to the managed product would install a different binary and
relearn its surface. It also doubles the maintenance of every command that is identical on both
sides, which is nearly all of them.

**Modelling a managed control plane on the customer's own cluster as a third target kind.** The tier
is one the roadmap keeps open, and it has no design behind it yet. A target kind invented for it now
would be a guess about a product shape, embedded in the CLI's core concept, and wrong in whatever way
the eventual design differs. Two kinds is what the evidence supports.

**Making `auth login` install Burrow when the chosen cluster lacks it.** Convenient, and it merges a
per-person act with a cluster-admin one. The second person to authenticate against a cluster would
trigger an install against a cluster that already has one; the failure is silent when it is a no-op
and serious when it is not.
