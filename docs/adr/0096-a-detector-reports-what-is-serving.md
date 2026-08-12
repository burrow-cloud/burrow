# ADR-0096: A detector reports what is serving, not what is registered

## Status

✅ Accepted

## TL;DR

Burrow asked the cluster "is metrics-server there?" and believed the answer. Wrong question. Ask "is
it answering?"

- **A broken metrics-server still looks installed.** The API name stays on the cluster after the
  thing behind it dies. Burrow matched the name.
- So `burrow cluster metrics install` — the command you run to fix it — said *already installed* and
  did nothing. On the cluster that found this, `kubectl top` had been dead two days.
- **Fix: require the API to advertise a working version**, which a dead one does not. Same check
  everywhere it is asked, so the install command and `burrow cluster` cannot disagree.
- **Three answers, not two: absent, registered-but-dead, serving.** Absent installs. Dead reports and
  installs nothing — re-applying a file does not fix a dead process, and the registration may be the
  platform's own. `--force` for the one case where re-applying is the repair.
- **Honest reporting is the fix here.** The operator could not find out the thing was broken; now the
  command tells them.

Fixes a defect in [ADR-0054](0054-install-is-control-plane-only.md) §1's detected baseline without
changing its rule. Reuses [ADR-0066](0066-postgres-on-cloudnativepg.md) §1's present-versus-ready
shape and [ADR-0034](0034-agent-native-onboarding.md)'s no-RBAC discovery read. Supersedes nothing.

## Context

[ADR-0054](0054-install-is-control-plane-only.md) §1 makes metrics-server a *detected* baseline:
install ensures it where it is missing and leaves a vendor's copy (k3s, GKE, AKS) alone.
`burrow cluster metrics install` is the same ensure reached on its own, for the clusters install ran
on before the baseline existed. Detection is one read of API-group discovery, which needs no RBAC —
if the `metrics.k8s.io` group is in `ServerGroups()`, something serves the Metrics API.

That last sentence is false, and it is false in exactly the case the command exists for. The Metrics
API is not served by the API server; it is served through the **aggregation layer**, by a pod behind
an `APIService`. When that pod stops answering, the aggregator does not withdraw the group — it marks
the group's `v1beta1` **version** stale, and client-go's `SplitGroupsAndResources` drops a stale
GroupVersion while appending the group regardless. The group survives its own contents. `kubectl top`
resolves a *version*, which is why one discovery call answered `error: Metrics API not available` for
`kubectl` and *already installed* for Burrow, about the same cluster, in the same minute.

On the cluster that surfaced this ([issue #561](https://github.com/burrow-cloud/burrow/issues/561))
the metrics-server pod was `0/1` with 337 restarts over two days, every scrape timing out on a
kubelet too slow to answer. `kubectl top` had been dead the whole time, HPA autoscaling with it, and
`burrow cluster` said the baseline was serving the Metrics API.

Three things make this worse than a wrong line of output:

- **The idempotent install inverts.** Re-running the ensure is the obvious repair for a broken
  baseline, and it is the one action guaranteed to do nothing here. The operator is told by the
  repair command that there is nothing to repair.
- **The false claim is load-bearing elsewhere.** The same group-name match answers
  `MetricsAPIAvailable`, so an applied HorizontalPodAutoscaler skipped the warning that says it will
  never scale. Two commands, one wrong premise.
- **Nothing else was going to notice.** Utilization is the layer that reports what is *consuming*
  CPU. Its absence is invisible until somebody goes looking, which is the argument
  [ADR-0054](0054-install-is-control-plane-only.md) §1 made for having the baseline at all.

The same detection shape appears for cert-manager, CloudNativePG and the pgBackRest plugin, and this
failure does not reach them: their groups are **CRDs**, served by the API server itself, and a CRD
group has no backing service to become unreachable. The two Postgres detectors already report
`Ready` beside `Present` for the neighbouring defect — CRDs outliving the controller that installed
them — which is the shape this record borrows.

## Decision

### 1. Present means serving, and registered is a separate fact

A detector reports the claim its callers will act on. For the Metrics API that claim is *an answer
comes back*, so `Present` requires the `metrics.k8s.io` group to advertise **a usable version**, not
merely to exist. A group whose versions are all stale arrives with an empty version list, so the test
is `len(g.Versions) > 0` and the bug's exact shape is what it rejects.

Any usable version counts, rather than `v1beta1` by name. `v1beta1` is the only version the Metrics
API has ever had, so today the two rules agree; if a later metrics-server serves a newer one, matching
the name would report it absent and invite install to write a second copy over a working one. The
version-by-name rule trades a real future hazard for no present gain.

`Registered` is reported alongside, and is true whenever the group exists. It is not a detail: the
three states need three different things done about them, and *registered and answering nothing* is
neither *installed* nor *absent*. `burrow cluster` renders all three, and the middle one names the
workload to go and look at rather than a command (§3 declines that cluster).

This is a two-fact capability for the reason [ADR-0066](0066-postgres-on-cloudnativepg.md) §1 gives
for a three-fact one: the facts fail apart, and from the outside each failure looks like the others.

### 2. One detector, wherever the question is asked

`detectMetricsServer` is the only place the question is answered. The capability report renders it,
`MetricsAPIAvailable` returns its `Present` to the HPA warning, and
`burrow cluster metrics install` decides on it — reaching it through the exported `DetectMetricsServer`
the way `burrow cluster postgres install` already reaches `DetectCertManager`.

The defect was in one copy of a loop that existed in two, and the second copy is why an HPA and a
capability listing could have disagreed about one cluster. Sharing the detector is not tidiness: it
is what makes the fix arrive everywhere the claim is made.

### 3. A registered-but-dead baseline is reported, not repaired

`burrow cluster metrics install` finding the Metrics API registered and serving nothing **applies
nothing**, prints what is true, and exits non-zero. Three reasons, in order of weight:

- **Discovery says the group is registered; it does not say by whom.** Applying the pinned baseline
  would replace a k3s/GKE/AKS metrics-server that is merely down, slow, or mid-restart with Burrow's
  copy — turning a transient outage into a permanent substitution, and doing it in the command
  documented as never installing over a copy the platform ships.
- **Where the registration is Burrow's own, the apply changes nothing.** The manifest on the cluster
  is already the manifest that would be applied. The only observable effect is a second false
  success line printed over a cluster where `kubectl top` is still dead.
- **The causes live outside the manifest.** A kubelet too slow to scrape, a serving certificate the
  API server rejects, a node with nothing left to schedule — none is repaired by re-applying a
  Deployment that is already correct. On the cluster in issue #561 it was the first of those.

So the report is the repair. The operator arrived unable to discover that the thing they were
installing was already installed and broken; the command now says so, says nothing was applied and
why, and names the three reads that identify which failure it is.

`--force` applies the baseline regardless, for the one case where re-applying genuinely repairs
something: the workload deleted and the `APIService` left behind. It is opt-in because the same apply
is the vendor substitution above, and an operator who has looked at the pod is the one who can tell
the two apart. The automatic ensure inside `install` and `bootstrap` never sets it — an install must
not replace a registration it did not make without being asked.

### 4. What this still does not detect

Discovery reports what the aggregation layer advertises. That is a stronger claim than the group
name and a weaker one than a metrics query, and the gap is stated rather than left to be found:

- A metrics-server **answering with no data** — up, registered, version fresh, returning an empty
  node list because it cannot scrape any kubelet — reads as serving. `kubectl top` prints
  `no resources found` and Burrow says the baseline is fine.
- A **partially** serving one — some nodes scraped, others timing out — reads as serving, which is
  also what an HPA on a scraped workload would want it to say.
- A **freshly restarted** one may advertise a fresh version moments before it answers usefully, and a
  crash-looping one alternates between two of these states on the aggregator's refresh interval, so
  two runs a minute apart can disagree.

Closing those needs a `nodes.metrics.k8s.io` list — what `kubectl top` does — which is a second call,
needs RBAC discovery does not, and fails for reasons that are not *absent*: a forbidden list on a
scoped credential would report a working cluster broken. The detector stays a discovery read, and a
capability report says what it saw rather than implying a probe it did not run.

## Consequences

- A cluster whose Metrics API is registered and dead now fails `burrow cluster metrics install`
  where it used to pass. That is the point, and it is a behaviour change for any script that treats
  that command's exit code as *the baseline is fine*.
- An HPA applied on such a cluster now carries the warning it should always have carried. Nothing is
  blocked — the warning was always advisory.
- `burrow cluster` gained a third metrics-server line, and the capability JSON gained a `registered`
  field. It is additive and omitted when false, so an older reader of that payload is unaffected.
- The operator on a broken baseline is given a diagnosis and no button. That is the honest position
  and it is less satisfying than a repair; `--force` exists so the one repairable shape is still
  reachable without them reverse-engineering which shape they have.
- cert-manager is still detected by group name alone, and can still report installed with no
  controller running. It is the CloudNativePG defect rather than this one — a CRD group cannot go
  stale — and closing it means a controller-Deployment read, which is its own change.
- Two test packages now carry a discovery fake, because the standard fake clientset derives groups
  from the versions it is given and cannot express a group without one. A fake that cannot express
  the bug is why the old tests passed.

## Rejected alternatives

- **Match `metrics.k8s.io/v1beta1` by name.** Fixes the reported bug with one string and reports a
  future v1-only metrics-server as absent, which invites install to write a second copy over a
  working one. §1 takes the same fix without the trap.
- **Probe `nodes.metrics.k8s.io` and report what came back.** The strongest signal and the wrong
  price: a second call on every capability read, RBAC that discovery does not need, and a forbidden
  or timed-out list that reports a healthy cluster as broken. §4 names what the cheaper read misses
  instead of pretending it misses nothing.
- **Re-apply the baseline whenever the API is not serving.** The behaviour issue #561 asks for
  literally, and it silently replaces a vendor's metrics-server on the strength of a transient
  failure, while fixing nothing in the case that produced the issue.
- **Read the `APIService`'s `Available` condition.** The aggregation layer's own verdict, with its
  own reason string — and a different client, cluster-scoped RBAC on `apiservices`, and a second
  vocabulary of failure to render. The empty version list is that verdict, already in a call Burrow
  makes.
- **Check whether a metrics-server Deployment exists to decide between reporting and re-applying.**
  It would automate §3's `--force`, and it decides the vendor-substitution question on a label
  selector: a vendor copy under different labels reads as *no workload*, which is precisely the
  branch that overwrites it.
- **Report the broken state and exit zero.** Keeps scripts green and makes the command's exit code
  mean *I looked* rather than *it works*, which is the ambiguity that hid this for two days.
- **Fold `Registered` into `Present` and describe the difference in prose.** One boolean cannot carry
  three states, and every caller would re-derive the third from a string.
