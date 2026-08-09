// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package controlplane

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

// The engine's half of ADR-0091 §2: an instance's name in the cluster is LOOKED UP, not derived.
//
// Everything that used to call AddonInstanceName(type, environment) to find out which server an
// operation acts on comes through here instead. The registry row is the mapping between the label a
// person types and the name the cluster knows, and it is the only mapping — nothing recovers an
// environment or a label by splitting a name.

// resolveInstance returns the instance of add-on type t that an operation in environment env named
// with label, or ErrNotFound.
//
// AN EMPTY LABEL IS THE ENVIRONMENT'S DEFAULT INSTANCE, which is what every add-on command has
// always meant and what a single-instance operator never has to say (ADR-0091 §1). The default is the
// environment's FIRST instance, labelled with the name it already carries in the cluster
// (AddonInstanceName) — so an install that predates this record is reached by exactly the path it
// always was, and nothing an operator has typed changes.
//
// The type is checked against the row rather than assumed. A label is unique within an environment
// and not within a (type, environment) pair — that is the granularity ADR-0085's `<env>.<name>.<code>`
// guardrail key needs — so a label naming an instance of a DIFFERENT type is a miss, not a match.
func (e *Engine) resolveInstance(ctx context.Context, t AddonType, env, label string) (AddonInfo, error) {
	if env == "" {
		return AddonInfo{}, fmt.Errorf("%s instance: no environment named; every add-on instance belongs to exactly one environment: %w", t, ErrInvalid)
	}
	wanted := label
	if wanted == "" {
		derived, err := AddonInstanceName(t, env)
		if err != nil {
			return AddonInfo{}, err
		}
		wanted = derived
	} else if err := ValidateInstanceLabel(wanted); err != nil {
		return AddonInfo{}, err
	}
	info, err := e.db.AddonByLabel(ctx, env, wanted)
	if errors.Is(err, ErrNotFound) {
		return AddonInfo{}, e.noSuchInstance(ctx, t, env, label, wanted)
	}
	if err != nil {
		return AddonInfo{}, fmt.Errorf("reading the %s add-on in environment %s: %w", t, env, err)
	}
	if info.Type != t {
		return AddonInfo{}, fmt.Errorf("%q in environment %s is a %s instance, not a %s one: %w", wanted, env, info.Type, t, ErrNotFound)
	}
	return info, nil
}

// noSuchInstance composes the refusal for a label nothing answers to, and it says a different thing
// depending on whether the caller named one.
//
// A caller who named none is missing the add-on: the useful answer is the command that installs it. A
// caller who named one may have typo'd it or may be thinking of another environment, so the answer
// lists what this environment does hold — which is the surface an operator holding a generated name
// from a log needs anyway (ADR-0091 §Consequences).
func (e *Engine) noSuchInstance(ctx context.Context, t AddonType, env, label, wanted string) error {
	if label == "" {
		return fmt.Errorf("no %s add-on is installed in environment %s (install one with `burrow addon install %s --env %s`): %w",
			t, env, t, env, ErrNotFound)
	}
	known, err := e.db.AddonsInEnvironment(ctx, t, env)
	if err != nil || len(known) == 0 {
		return fmt.Errorf("environment %s has no %s instance called %q: %w", env, t, wanted, ErrNotFound)
	}
	labels := make([]string, 0, len(known))
	for _, a := range known {
		labels = append(labels, a.Label)
	}
	return fmt.Errorf("environment %s has no %s instance called %q; it has %s: %w",
		env, t, wanted, strings.Join(labels, ", "), ErrNotFound)
}

// installTarget decides what an install acts on: the label it is addressed by, and the name the
// instance has (or will have) in the cluster.
//
// Three cases, and the middle one is the whole of ADR-0091 §2's promise that nothing moves:
//
//   - The label already exists in this environment. This is a RE-INSTALL of that instance, so the
//     name is the one the registry already holds, whatever it is.
//   - No label was given, or the label is the environment's own derived name. This is the
//     environment's FIRST instance, and it is named the way it has always been named
//     (AddonInstanceName) — `burrow-postgres` in the default environment, `burrow-postgres-staging`
//     in staging — so an install that predates this record adopts exactly the instance, the volume
//     and the superuser Secret it already has.
//   - Any other label. A second instance, with a generated cluster name the operator never types.
func (e *Engine) installTarget(ctx context.Context, t AddonType, env, label string) (string, string, error) {
	derived, err := AddonInstanceName(t, env)
	if err != nil {
		return "", "", err
	}
	if label == "" {
		label = derived
	} else if err := ValidateInstanceLabel(label); err != nil {
		return "", "", err
	}
	existing, err := e.db.AddonByLabel(ctx, env, label)
	switch {
	case err == nil:
		// A label is unique within an environment and not within a (type, environment) pair, so a
		// label already held by another type is a refusal rather than a second row: two instances
		// answering to one guardrail key would make `<env>.<name>.<code>` ambiguous, which is the
		// property ADR-0085's key shape depends on.
		if existing.Type != t {
			return "", "", fmt.Errorf("environment %s already has a %s instance called %q, and a name means one instance there: %w",
				env, existing.Type, label, ErrInvalid)
		}
		return label, existing.Name, nil
	case !errors.Is(err, ErrNotFound):
		return "", "", fmt.Errorf("reading the %s add-on in environment %s: %w", t, env, err)
	}
	if label == derived {
		return label, derived, nil
	}
	name, err := e.newInstanceName(ctx, t)
	if err != nil {
		return "", "", err
	}
	return label, name, nil
}

// installConsequence is what the addon.install hold asks the operator to approve. It names the
// instance's LABEL, and says plainly when the install is a second instance rather than the
// environment's own — because a pod and a volume beside an existing pod and volume is a different
// thing to be agreeing to than standing the environment's database up (ADR-0091 §5).
func installConsequence(t AddonType, image, env, label string) string {
	if derived, err := AddonInstanceName(t, env); err == nil && label == derived {
		return fmt.Sprintf("installing the %s add-on (%s) in environment %s", t, image, env)
	}
	return fmt.Sprintf("installing a SECOND %s instance %q (%s) in environment %s, beside the one already there: a separate pod and a separate volume, both billed",
		t, label, image, env)
}

// removalTarget resolves what `addon remove <argument>` names, and it tries the REGISTRY NAME first.
//
// That order is what keeps every removal anybody has ever typed working unchanged. Before ADR-0091
// the argument was the instance's name in the cluster and there was no `--env`, so `addon remove
// burrow-postgres-staging` has to keep meaning what it meant — and it does, because the name is the
// registry's primary key and finding a row by it needs no environment at all. Only an argument that
// is not a registry name is treated as a label, which is the only way a generated instance can be
// addressed.
//
// NAMING BOTH INCONSISTENTLY IS A REFUSAL. `addon remove burrow-postgres-staging --env prod` resolves
// by name to staging's instance while the operator said prod, and a removal is the wrong operation to
// resolve a contradiction quietly.
func (e *Engine) removalTarget(ctx context.Context, arg, env string) (AddonInfo, error) {
	info, err := e.db.Addon(ctx, arg)
	switch {
	case err == nil:
		if env != "" {
			named, nerr := e.resolveEnvironmentName(ctx, env)
			if nerr != nil {
				return AddonInfo{}, nerr
			}
			if named != info.Environment {
				return AddonInfo{}, fmt.Errorf("%q is the %s instance of environment %s, not of %s: %w",
					arg, info.Type, info.Environment, named, ErrInvalid)
			}
		}
		return info, nil
	case !errors.Is(err, ErrNotFound):
		return AddonInfo{}, err
	}
	// Not a registry name, so it is a label — and a label needs an environment to be unique in.
	targetEnv, err := e.resolveEnvironmentName(ctx, env)
	if err != nil {
		return AddonInfo{}, err
	}
	info, err = e.db.AddonByLabel(ctx, targetEnv, arg)
	if errors.Is(err, ErrNotFound) {
		return AddonInfo{}, fmt.Errorf("environment %s has no add-on instance called %q: %w", targetEnv, arg, ErrNotFound)
	}
	if err != nil {
		return AddonInfo{}, err
	}
	return info, nil
}

// resolveEnvironmentName canonicalizes an environment for an operation that acts on an add-on
// instance, using the same resolution a mutating operation gets — an empty value while several
// environments are registered is refused rather than landing on whichever one happens to be first
// (ADR-0047 §1).
func (e *Engine) resolveEnvironmentName(ctx context.Context, env string) (string, error) {
	name, _, err := e.resolveMutatingEnvironment(ctx, env)
	return name, err
}

// instanceLabel is the string an operator addresses info by: its label, falling back to its cluster
// name for a row written before labels existed and never re-saved. The fallback is the right answer
// rather than a defensive one — an environment's first instance is labelled with its own name, and
// that is exactly the row that can predate the column.
func instanceLabel(info AddonInfo) string {
	if info.Label != "" {
		return info.Label
	}
	return info.Name
}

// attachmentKey is the variable app's attachment to inst is written under: the recorded name, or
// AppDatabaseURLKey for an attachment made before the name was a choice (issue #462).
//
// THE FALLBACK IS ONLY THE ENVIRONMENT'S DEFAULT INSTANCE. Every unrecorded attachment predates the
// record and was written against the one instance the environment then had, so that is the only
// instance the constant can be the answer for. An unrecorded attachment to any other instance is
// simply not an attachment, and answering `DATABASE_URL` for it would hand a caller another
// attachment's variable.
func (e *Engine) attachmentKey(ctx context.Context, t AddonType, app, env string, inst AddonInfo) (string, error) {
	key, recorded, err := e.db.AddonEnvKey(ctx, string(t), app, env, inst.Name)
	if err != nil {
		return "", err
	}
	if recorded {
		return key, nil
	}
	if isDefaultInstance(inst) {
		return AppDatabaseURLKey, nil
	}
	return "", fmt.Errorf("%q holds no attachment for %s in environment %s: %w", instanceLabel(inst), app, env, ErrNotFound)
}

// isDefaultInstance reports whether info is its environment's FIRST instance — the one every command
// that names none acts on. It is decided by the LABEL, because that is what selects an instance
// (ADR-0091 §2), and an environment's first instance is labelled with its own derived name.
func isDefaultInstance(info AddonInfo) bool {
	derived, err := AddonInstanceName(info.Type, info.Environment)
	return err == nil && instanceLabel(info) == derived
}

// newInstanceName mints the cluster name for an instance PAST an environment's first:
// `burrow-<type>-<id>` with a short generated id (ADR-0091 §2).
//
// THE REGISTRY DECIDES UNIQUENESS, NOT THE ENTROPY. Each candidate is checked against the registry
// before it is used, and a name already taken sends the loop round again — so a collision costs
// another id rather than an instance quietly adopting somebody else's `Cluster`, its volume and its
// data. The registry's own primary key is the backstop if two mints race.
//
// The id comes from the injected IDSource, so the engine reads no ambient randomness (ADR-0010) and a
// test gets the same names every run. It is squeezed into cloud ADR-0029's alphabet — lowercase
// alphanumeric, the intersection of what a Kubernetes object name and everything Burrow composes from
// it will accept — rather than a new source being introduced for one caller.
func (e *Engine) newInstanceName(ctx context.Context, t AddonType) (string, error) {
	const attempts = 5
	for i := 0; i < attempts; i++ {
		id := instanceIDFrom(e.ids.NewID())
		if id == "" {
			continue
		}
		name, err := GenerateAddonInstanceName(t, id)
		if err != nil {
			continue
		}
		_, err = e.db.Addon(ctx, name)
		if errors.Is(err, ErrNotFound) {
			return name, nil
		}
		if err != nil {
			return "", fmt.Errorf("checking whether the instance name %q is free: %w", name, err)
		}
	}
	return "", fmt.Errorf("could not mint an unused name for a new %s instance after %d attempts", t, attempts)
}

// instanceIDLen is how much of a minted id an instance name carries. Short enough to read off
// `kubectl get cluster` and say out loud, long enough that a collision is a retry rather than a
// pattern.
const instanceIDLen = 6

// instanceIDFrom squeezes a minted identifier into the instance-id alphabet: lowercase alphanumeric,
// truncated. It DISCARDS rather than transliterates — a dash or a brace is dropped, never mapped to a
// letter — because the id carries no meaning that a mapping could preserve. An identifier with no
// usable characters yields "" and the caller mints another.
func instanceIDFrom(raw string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(raw) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
		}
		if b.Len() == instanceIDLen {
			break
		}
	}
	return b.String()
}
