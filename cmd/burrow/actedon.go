// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"github.com/burrow-cloud/burrow/connect"
	"github.com/burrow-cloud/burrow/internal/targetname"
	"github.com/burrow-cloud/burrow/localconfig"
)

// Every command that CHANGES something names the target it changed, in its own output (ADR-0078
// §4). This file is the one place that happens, so the naming cannot be worded one way on deploy and
// another on rollback, and so a command cannot omit it by forgetting a line.
//
// Three details follow from why it exists, and each is load-bearing:
//
//   - It goes CLOSE TO THE THING IT DID, appended to the sentence that says what happened, rather
//     than into a header. `burrow` already prints a targeting line to stderr before an operation
//     (ADR-0036), and a line printed before the work is a line that gets skimmed past; the point of
//     this one is that the person reads it in the same breath as "deployed web".
//   - It names what the command ACTUALLY reached, resolved at the connection seam, not what the
//     config happens to record. A command that resolves through the kubeconfig without consulting
//     the target says so plainly; naming the selected target there would be the very mistake this
//     exists to catch.
//   - The JSON result carries it too, so an agent composing a result can say WHERE it happened
//     rather than only what happened.
//
// Read-only commands are deliberately left out. This is about irreversible acts; printing it
// everywhere would make it noise, and noise is what gets skimmed past.

// emitChange prints a changing command's result and names the target it acted on. It is the
// mutating counterpart of emit: same result value, same --json switch, plus the target.
func (o *commonOpts) emitChange(w io.Writer, v any, human string) error {
	if o.json {
		return emitJSONWithTarget(w, v, o.acted)
	}
	fmt.Fprintln(w, withTargetClause(human, o.acted))
	return nil
}

// withTargetClause appends the target clause to the FIRST line of a human result. Several commands
// print a headline followed by a block — a run's captured output, a deploy's dependency report — and
// the headline is the sentence the target belongs to. Appending to the end of the whole message
// would drift the target away from the act it describes and, after a few hundred lines of command
// output, out of the same breath entirely.
func withTargetClause(human string, n targetname.Named) string {
	head, rest, found := strings.Cut(human, "\n")
	head = strings.TrimRight(head, " ") + " " + n.Clause()
	if !found {
		return head
	}
	return head + "\n" + rest
}

// withTargetClauseWhenDecided is withTargetClause for a result line that did NOT carry a target
// before, and it appends one only when something actually decided where the command went: a
// configured target, a --context override, or a --control-plane URL.
//
// The commands that have always printed the clause keep printing it unconditionally, including the
// "no target selected" form — that form is informative there precisely because its neighbours name a
// target. Adding it to a line that never had one is different: for somebody who has never run
// `burrow auth login` it would change familiar output, and break anything matching it, to say only
// that they have not done a thing they were never asked to do.
func withTargetClauseWhenDecided(human string, n targetname.Named) string {
	if n.Name == "" && !n.Override && n.Endpoint == "" {
		return human
	}
	return withTargetClause(human, n)
}

// emitJSONWithTarget prints v as indented JSON with a `target` member added. The result's own fields
// are spliced through unchanged and in their original order rather than round-tripped through a map,
// so adding the target neither reorders nor reshapes what a caller already parses. A result that is
// not a JSON object (a list, a bare value, nothing at all) is wrapped as {"target":…, "result":…},
// which is the only way to attach a member to something that has none.
func emitJSONWithTarget(w io.Writer, v any, n targetname.Named) error {
	body, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("encoding the result: %w", err)
	}
	target, err := json.Marshal(n)
	if err != nil {
		return fmt.Errorf("encoding the target: %w", err)
	}

	var merged []byte
	switch {
	case len(body) > 2 && body[0] == '{':
		merged = append(merged, `{"target":`...)
		merged = append(merged, target...)
		merged = append(merged, ',')
		merged = append(merged, body[1:]...)
	case string(body) == "{}":
		merged = append(merged, `{"target":`...)
		merged = append(merged, target...)
		merged = append(merged, '}')
	default:
		merged = append(merged, `{"target":`...)
		merged = append(merged, target...)
		merged = append(merged, `,"result":`...)
		merged = append(merged, body...)
		merged = append(merged, '}')
	}

	var buf bytes.Buffer
	if err := json.Indent(&buf, merged, "", "  "); err != nil {
		return fmt.Errorf("formatting the result: %w", err)
	}
	buf.WriteByte('\n')
	_, err = w.Write(buf.Bytes())
	return err
}

// namePrivilegedTarget names the target for the privileged path — the commands that act on a cluster
// without being scoped to an app (guard, cluster config, add-ons, credentials, audit, failures).
// resolved is the context clusterContext decided for this invocation: the selected target's, the
// --context override, or empty when no target is selected and the kubeconfig's current context
// applies. An empty value is resolved to that current context here, so the name is a context a
// reader recognises rather than a blank.
//
// A recorded target is named only when it is the one that context belongs to; targetname.For is what
// enforces that, so a command can never claim a target it did not reach.
func (o *commonOpts) namePrivilegedTarget(resolved string) targetname.Named {
	if o.controlPlane != "" {
		return targetname.ForControlPlane(o.controlPlane)
	}
	// A kubeconfig that cannot be read is not worth failing a command over here: the connection
	// itself is about to fail on it and will say so far better than a naming helper can.
	kubeContext, err := connect.TargetContextName(o.kubeconfig, resolved)
	if err != nil {
		kubeContext = resolved
	}
	cfg, err := localconfig.Load()
	if err != nil {
		cfg = nil
	}
	return targetname.For(cfg, kubeContext, o.context != "")
}
