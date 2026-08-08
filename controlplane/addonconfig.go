// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package controlplane

import (
	"context"
	"fmt"
	"strconv"
	"strings"
)

// This file is ADR-0082: an add-on instance is configured AFTER it exists, not at install.
//
// An instance is created plain — `addon install postgres` takes no shape flag, which is ADR-0081 §1's
// decision — and everything about its shape is set here, on an instance that already holds data. That
// ordering is the record's whole point: the install is the moment nobody can answer "will this need a
// standby", and the day the answer is knowable is months later, in production.
//
// TWO SETTINGS ARE NOT ONE PARAMETER, which is why they are subcommands rather than flags (§1) and
// why each carries its own validation and its own refusal here:
//
//   - standbys grows and shrinks. Growing proceeds; shrinking is held for confirmation, and removing
//     the LAST one also withdraws the read address ADR-0081 §2 wrote and restarts the apps holding it
//     (§3).
//   - storage grows only. A volume cannot shrink, so the shrink is REFUSED at the point of asking
//     rather than written and left to fail in a `Cluster` status field (§2).
//
// NOTHING HERE EVER RUNS BY ITSELF (§5). There is no threshold, no autoscaler, and no path that
// reaches ConfigureAddon other than an operator having typed the command.

// AddonSetting names one configurable property of an add-on instance. It is the second word of
// `burrow addon config <type> <setting> <value>` and the key the audit row records the change under.
type AddonSetting string

const (
	// AddonSettingStandbys is the number of standby pods beside a Postgres instance's primary
	// (ADR-0081). It is expressed as STANDBYS rather than as CloudNativePG's `instances`, which counts
	// the primary too: `--instances 2` would read as "give me a second Postgres server", which is a
	// real thing somebody might want and not what this does.
	AddonSettingStandbys AddonSetting = "standbys"
	// AddonSettingStorage is the size of an instance's data volume, as a Kubernetes quantity ("50Gi").
	// It grows and never shrinks.
	AddonSettingStorage AddonSetting = "storage"
)

// AddonShape is the configurable shape of one add-on instance as it exists right now, read off the
// cluster rather than out of the registry: the registry records that an instance was installed, and
// the question here is what it currently IS.
type AddonShape struct {
	// Standbys is the number of standbys beside the primary. Zero is the shape every instance is
	// created with.
	Standbys int
	// Storage is the data volume's size as a Kubernetes quantity string ("20Gi"), exactly as the
	// object spells it. It is not normalized: an operator reading a value back should see what is
	// written, not Burrow's rendering of it.
	Storage string
}

// ConfigureInstanceRequest is what the Kubernetes seam is asked to change on an existing instance.
// Exactly one field beyond the identity is set, because exactly one setting is changed per call —
// the subcommand-per-setting shape of ADR-0082 §1 carried down to the seam, so a partially-applied
// pair of changes is not a state this can reach.
type ConfigureInstanceRequest struct {
	// Addon and Environment identify the instance, as everywhere else (AddonInstanceName).
	Addon       AddonType
	Environment string
	// Standbys, when non-nil, is the new standby count. The adapter turns it into whatever the
	// operator underneath counts — for CloudNativePG, one more than this.
	Standbys *int
	// Storage, when non-empty, is the new data volume size as a Kubernetes quantity. The engine has
	// already refused a shrink; an adapter is not asked to re-decide it.
	Storage string
}

// AddonSettingInfo is one row of `burrow addon config <type>`: what can be set, what it is set to,
// and what changing it does.
//
// CONSEQUENCE IS A FIELD RATHER THAN HELP TEXT, and that is ADR-0082 §1's argument made structural.
// A flag gets one line of `--help` and no room to say that adding a standby restarts every attached
// app or that a grown volume cannot shrink back; a setting that carries its consequence into the
// listing says it where somebody is looking at the current value and deciding whether to change it.
type AddonSettingInfo struct {
	Setting AddonSetting `json:"setting"`
	// Value is the setting's current value, rendered as the operator would type it.
	Value       string `json:"value"`
	Description string `json:"description"`
	Consequence string `json:"consequence"`
}

// AddonSettingsResult is `burrow addon config <type>`: an instance, and what can be told to it.
type AddonSettingsResult struct {
	Addon       AddonType `json:"addon"`
	Environment string    `json:"environment"`
	// Instance is the instance the values were read from, by the name every consumer resolves it at
	// (AddonInstanceName). An environment holds one (ADR-0067 §1), so this is what `--env` selected.
	Instance string             `json:"instance"`
	Settings []AddonSettingInfo `json:"settings"`
}

// ConfigureAddonOptions is everything a change needs beyond the instance, the setting and the value.
type ConfigureAddonOptions struct {
	// Confirm is the operator saying they mean a SHRINK. Growing never consults it: adding capacity
	// breaks nothing that exists and the cost is accepted by having typed the command (ADR-0082 §2).
	//
	// It is not a guardrail's `--confirm`. ADR-0082 §4 puts this verb in ADR-0065's tier 1 — absent
	// from `burrow-agent` entirely — so there is no disposition to satisfy and nothing an agent could
	// confirm for itself; what this satisfies is the hold §2 places on taking capacity away.
	Confirm bool
}

// ConfigureAddonResult is what one change did: the instance, the setting, and the values either side
// of it. The pair is what makes the result legible on its own — "standbys is 1" does not say whether
// anything happened, and "standbys 0 -> 1" does.
type ConfigureAddonResult struct {
	Addon       AddonType    `json:"addon"`
	Environment string       `json:"environment"`
	Instance    string       `json:"instance"`
	Setting     AddonSetting `json:"setting"`
	From        string       `json:"from"`
	To          string       `json:"to"`
	// Changed is false when the instance was already in the shape that was asked for. Nothing was
	// written and nothing was restarted, and saying so is better than reporting a change that did not
	// happen: a re-run of the same command is a normal thing to do.
	Changed bool `json:"changed"`
	// ReadAddress is what happened to ADR-0081 §2's read address, which appears when the first standby
	// does and is withdrawn when the last one goes (ADR-0082 §3). It is always present, so a caller
	// reading this answer is told "nothing" rather than left to infer it from an absent key.
	ReadAddress ReadAddressChange `json:"read_address"`
}

// ReadAddressAction is what a change did to the read address.
type ReadAddressAction string

const (
	// ReadAddressUnchanged: the instance had a standby before and after, or none either side, so the
	// address is exactly as it was.
	ReadAddressUnchanged ReadAddressAction = "unchanged"
	// ReadAddressWritten: the instance gained its first standby, so every attached app was given the
	// read address and restarted onto it (ADR-0081 §2).
	ReadAddressWritten ReadAddressAction = "written"
	// ReadAddressWithdrawn: the instance lost its last standby, so the address was removed from every
	// attached app and they were restarted (ADR-0082 §3). Leaving it would leave a variable resolving
	// to nothing, which fails at the app's next read rather than at the operation that caused it.
	ReadAddressWithdrawn ReadAddressAction = "withdrawn"
)

// ReadAddressChange reports what happened to the read address, by app name. It carries KEY NAMES
// only — never a connection string (ADR-0028/0031).
type ReadAddressChange struct {
	Action ReadAddressAction `json:"action"`
	// Apps is every app the address was written to or withdrawn from, and which was restarted. It is
	// the blast radius of §3's restart, recorded rather than counted.
	Apps []string `json:"apps"`
	// Stranded is the apps the change could not be completed for, one line each. They are reported
	// rather than failing the whole operation: the instance's shape HAS changed by this point, and an
	// error would send an operator to repeat a scale that already happened.
	Stranded []StrandedApp `json:"stranded,omitempty"`
	// Note says, in one line, why nothing was done when the action is unchanged but something might
	// have been expected — the provisioner having no way to compose a read address, most of all.
	Note string `json:"note,omitempty"`
}

// readAddressKey is the variable an app's read address is written under: its own connection-string
// variable with `_READ` on the end.
//
// IT IS DERIVED FROM THE ATTACHMENT'S OWN NAME rather than being a constant. An app may be attached
// under a name of its own (issue #462), and a fixed `DATABASE_READ_URL` beside a `PG_DSN` would be
// two variables that do not read as a pair — the app that renamed its connection string is precisely
// the app that does not follow the convention. Deriving it means the two names always match, and a
// rename moves both.
func readAddressKey(attachmentKey string) string { return attachmentKey + "_READ" }

// AddonSettings reports what can be configured on environment env's instance of add-on t, and what
// each setting is currently set to (ADR-0082 §1's bare `burrow addon config <type>`).
//
// It reads the CLUSTER, not the registry. The registry records that an instance was installed and
// with what image; the question this answers is what that instance IS right now, which is a property
// of the object the operator is reconciling and can have been changed since.
func (e *Engine) AddonSettings(ctx context.Context, t AddonType, env string) (AddonSettingsResult, error) {
	if err := configurableAddon(t); err != nil {
		return AddonSettingsResult{}, err
	}
	// A read resolves the environment the way every other read does, so `addon config postgres` and
	// `addon config postgres standbys 1` name the same instance from the same flags.
	targetEnv, _, err := e.resolveMutatingEnvironment(ctx, env)
	if err != nil {
		return AddonSettingsResult{}, fmt.Errorf("addon config %s: %w", t, err)
	}
	instance, err := AddonInstanceName(t, targetEnv)
	if err != nil {
		return AddonSettingsResult{}, fmt.Errorf("addon config %s: %w", t, err)
	}
	shape, err := e.k8s.AddonInstanceShape(ctx, t, targetEnv)
	if err != nil {
		return AddonSettingsResult{}, fmt.Errorf("addon config %s: %w", t, err)
	}
	return AddonSettingsResult{
		Addon:       t,
		Environment: targetEnv,
		Instance:    instance,
		Settings: []AddonSettingInfo{
			{
				Setting:     AddonSettingStandbys,
				Value:       strconv.Itoa(shape.Standbys),
				Description: "standby pods beside the primary; a standby takes over on failover and serves reads (ADR-0081)",
				Consequence: "adding the first one restarts every attached app so it picks up the read address; removing the last one withdraws that address and restarts them again",
			},
			{
				Setting:     AddonSettingStorage,
				Value:       shape.Storage,
				Description: "the size of the instance's data volume, as a Kubernetes quantity (50Gi)",
				Consequence: "CANNOT BE UNDONE: a volume grows and never shrinks, so an over-provisioned one is paid for until the instance is rebuilt",
			},
		},
	}, nil
}

// ConfigureAddon changes one setting on an instance that already exists (ADR-0082).
//
// THE SHAPE IS READ BEFORE IT IS WRITTEN, and every refusal is made against what was read rather than
// against what the caller believed. That is what "refused at the point of asking" means for a volume
// shrink (§2): Burrow says so here, with both sizes in the message, instead of writing a smaller size
// into a `Cluster` and leaving an operator to find the explanation in a status field.
//
// A CHANGE THAT IS NOT A CHANGE IS NOT ONE. Asking for the shape the instance already has writes
// nothing, restarts nothing, and says so — a re-run of the same command is ordinary, and a
// no-op reported as a scale-down would be a confirmation prompt for an operation that does not exist.
//
// The audit row records the setting, the value it came from and the value it went to, against the
// instance (ADR-0027). No secret value reaches it: the read address is a connection string, and only
// the KEY it was written under and the apps it reached are recorded.
func (e *Engine) ConfigureAddon(ctx context.Context, t AddonType, env string, setting AddonSetting, value string, opts ConfigureAddonOptions) (ConfigureAddonResult, error) {
	if err := configurableAddon(t); err != nil {
		return ConfigureAddonResult{}, err
	}
	targetEnv, ns, err := e.resolveMutatingEnvironment(ctx, env)
	if err != nil {
		return ConfigureAddonResult{}, fmt.Errorf("addon config %s: %w", t, err)
	}
	instance, err := AddonInstanceName(t, targetEnv)
	if err != nil {
		return ConfigureAddonResult{}, fmt.Errorf("addon config %s: %w", t, err)
	}
	// The redacted audit args carry NAMES and NUMBERS only. `from` and `to` are added as soon as the
	// current shape is known, so a row written on a refusal still says what was being attempted and
	// what it was being attempted against (ADR-0082's "from what to what, and on which instance").
	args := map[string]string{"addon": string(t), "env": targetEnv, "instance": instance, "setting": string(setting)}

	shape, err := e.k8s.AddonInstanceShape(ctx, t, targetEnv)
	if err != nil {
		e.recordExecution(ctx, auditOpAddonConfig, instance, args, err)
		return ConfigureAddonResult{}, fmt.Errorf("addon config %s: %w", t, err)
	}

	switch setting {
	case AddonSettingStandbys:
		return e.configureStandbys(ctx, t, targetEnv, ns, instance, shape, value, args, opts)
	case AddonSettingStorage:
		return e.configureStorage(ctx, t, targetEnv, instance, shape, value, args)
	default:
		err := unknownSettingError(t, setting)
		e.recordExecution(ctx, auditOpAddonConfig, instance, args, err)
		return ConfigureAddonResult{}, fmt.Errorf("addon config %s: %w", t, err)
	}
}

// configurableAddon refuses a type that has no settings, naming what it would have to gain to have
// one. Only Postgres has a shape today; the cache inherits the verb when its topologies land, which
// is why ADR-0082 is its own record rather than a flag on a Postgres command.
func configurableAddon(t AddonType) error {
	if t != AddonPostgres {
		return fmt.Errorf("addon config %s: the %s add-on has nothing that can be configured after it is installed; only postgres does (its standby count and its volume size): %w", t, t, ErrInvalid)
	}
	return nil
}

// unknownSettingError names what CAN be set rather than only what cannot, because a caller who
// reached here has the type right and the word wrong.
func unknownSettingError(t AddonType, setting AddonSetting) error {
	return fmt.Errorf("%q is not a setting of the %s add-on; it has %s and %s. `burrow addon config %s` lists them with their current values: %w",
		setting, t, AddonSettingStandbys, AddonSettingStorage, t, ErrInvalid)
}

// configureStandbys changes how many standbys an instance runs beside its primary (ADR-0081).
//
// GROWING AND SHRINKING ARE DIFFERENT OPERATIONS THROUGH ONE COMMAND, and the asymmetry is §2's:
// adding a standby breaks nothing that exists, while removing one takes away something an app may be
// using — so only one of them asks. The confirmation names the apps, not a count, for the reason
// ADR-0064 §2 gives: a person about to break something should see what.
func (e *Engine) configureStandbys(ctx context.Context, t AddonType, targetEnv, ns, instance string, shape AddonShape, value string, args map[string]string, opts ConfigureAddonOptions) (ConfigureAddonResult, error) {
	to, err := parseStandbys(value)
	if err != nil {
		e.recordExecution(ctx, auditOpAddonConfig, instance, args, err)
		return ConfigureAddonResult{}, fmt.Errorf("addon config %s: %w", t, err)
	}
	from := shape.Standbys
	args["from"], args["to"] = strconv.Itoa(from), strconv.Itoa(to)
	result := ConfigureAddonResult{
		Addon: t, Environment: targetEnv, Instance: instance,
		Setting: AddonSettingStandbys, From: args["from"], To: args["to"],
		ReadAddress: ReadAddressChange{Action: ReadAddressUnchanged, Apps: []string{}},
	}
	if to == from {
		e.recordExecution(ctx, auditOpAddonConfig, instance, args, nil)
		return result, nil
	}

	// The apps are enumerated BEFORE the shrink is refused, so the refusal can name them. It is the
	// same enumeration a physical restore uses and it is asked of the APPS rather than of the
	// database, because the answer has to be available when the instance is the thing that is
	// struggling — which is one reason somebody reaches for a scale-down.
	apps, err := e.appsAttachedInEnvironment(ctx, ns, targetEnv)
	if err != nil {
		e.recordExecution(ctx, auditOpAddonConfig, instance, args, err)
		return ConfigureAddonResult{}, fmt.Errorf("addon config %s: %w", t, err)
	}
	if len(apps) > 0 {
		args["apps"] = strings.Join(apps, ",")
	}
	if to < from && !opts.Confirm {
		err := standbyShrinkNotConfirmed(t, instance, targetEnv, from, to, apps)
		e.recordExecution(ctx, auditOpAddonConfig, instance, args, err)
		return ConfigureAddonResult{}, fmt.Errorf("addon config %s: %w", t, err)
	}

	if err := e.k8s.ConfigureAddonInstance(ctx, ConfigureInstanceRequest{Addon: t, Environment: targetEnv, Standbys: &to}); err != nil {
		e.recordExecution(ctx, auditOpAddonConfig, instance, args, err)
		return ConfigureAddonResult{}, fmt.Errorf("addon config %s in environment %s: %w", t, targetEnv, err)
	}
	result.Changed = true

	// The read address appears with the FIRST standby and goes with the LAST one (ADR-0081 §2,
	// ADR-0082 §3). Every other move — 1 to 2, 2 to 1 — leaves it exactly as it was, because what it
	// points at is unchanged: `-ro` selects standbys, and there are still some.
	switch {
	case from == 0 && to > 0:
		result.ReadAddress = e.writeReadAddress(ctx, t, targetEnv, ns, apps)
	case from > 0 && to == 0:
		result.ReadAddress = e.withdrawReadAddress(ctx, t, targetEnv, ns, apps)
	}
	if result.ReadAddress.Action != ReadAddressUnchanged {
		args["read_address"] = string(result.ReadAddress.Action)
	}
	if len(result.ReadAddress.Stranded) > 0 {
		args["stranded"] = strings.Join(strandedNames(result.ReadAddress.Stranded), ",")
	}
	e.recordExecution(ctx, auditOpAddonConfig, instance, args, nil)
	return result, nil
}

// configureStorage grows an instance's data volume (ADR-0082 §2).
//
// A SHRINK IS A REFUSAL RATHER THAN A CONFIRMATION, and that is the one place this file departs from
// "shrinking asks". A volume cannot shrink — the data does not fit, and no operator asked to type a
// name would be agreeing to something achievable — so there is nothing to hold for confirmation. The
// refusal names both sizes, because the interesting failure is a typo (`5Gi` for `50Gi`) rather than
// a considered decision to make a database smaller.
func (e *Engine) configureStorage(ctx context.Context, t AddonType, targetEnv, instance string, shape AddonShape, value string, args map[string]string) (ConfigureAddonResult, error) {
	to, err := parseStorageSize(value)
	if err != nil {
		e.recordExecution(ctx, auditOpAddonConfig, instance, args, err)
		return ConfigureAddonResult{}, fmt.Errorf("addon config %s: %w", t, err)
	}
	from, err := parseStorageSize(shape.Storage)
	if err != nil {
		// The instance's own size is unreadable, so whether this is a grow cannot be established. That
		// is refused rather than assumed either way: assuming a grow would let a shrink through the one
		// check that exists to stop it.
		err = fmt.Errorf("the instance %q reports its volume size as %q, which is not a size Burrow can compare against %q — so it cannot tell whether this grows the volume or shrinks it, and a shrink is not possible. Set the size on the instance to a plain quantity (50Gi) and re-run: %w",
			instance, shape.Storage, value, ErrInvalid)
		e.recordExecution(ctx, auditOpAddonConfig, instance, args, err)
		return ConfigureAddonResult{}, fmt.Errorf("addon config %s: %w", t, err)
	}
	args["from"], args["to"] = shape.Storage, value
	result := ConfigureAddonResult{
		Addon: t, Environment: targetEnv, Instance: instance,
		Setting: AddonSettingStorage, From: shape.Storage, To: value,
		ReadAddress: ReadAddressChange{Action: ReadAddressUnchanged, Apps: []string{}},
	}
	switch {
	case to == from:
		e.recordExecution(ctx, auditOpAddonConfig, instance, args, nil)
		return result, nil
	case to < from:
		err := fmt.Errorf("the volume of %q is %s and cannot be made smaller: a PersistentVolumeClaim grows and never shrinks, so %s is not a size this instance can be given. Nothing was written — the alternative is a `Cluster` sitting in a failed state explaining this in a status field. To reclaim the storage the instance has to be rebuilt from a backup: %w",
			instance, shape.Storage, value, ErrInvalid)
		e.recordExecution(ctx, auditOpAddonConfig, instance, args, err)
		return ConfigureAddonResult{}, fmt.Errorf("addon config %s: %w", t, err)
	}

	if err := e.k8s.ConfigureAddonInstance(ctx, ConfigureInstanceRequest{Addon: t, Environment: targetEnv, Storage: value}); err != nil {
		e.recordExecution(ctx, auditOpAddonConfig, instance, args, err)
		return ConfigureAddonResult{}, fmt.Errorf("addon config %s in environment %s: %w", t, targetEnv, err)
	}
	result.Changed = true
	e.recordExecution(ctx, auditOpAddonConfig, instance, args, nil)
	return result, nil
}

// standbyShrinkNotConfirmed is the hold ADR-0082 §2 places on taking capacity away. It names the
// apps rather than counting them, and it says which of the two shrinks this is: dropping to zero
// takes the read address with it, and dropping to a smaller non-zero number does not.
func standbyShrinkNotConfirmed(t AddonType, instance, env string, from, to int, apps []string) error {
	what := fmt.Sprintf("taking the %s instance %q in environment %s from %s to %s", t, instance, env, plural(from, "standby", "standbys"), plural(to, "standby", "standbys"))
	if to == 0 {
		what += ", which removes the endpoint the read address resolves to. The read address is withdrawn from every attached app and each is restarted, so an app reading from the replica loses it at the moment of the change rather than at its next query"
	} else {
		what += ", which reduces how much of the instance survives losing a pod"
	}
	switch len(apps) {
	case 0:
		what += ". No app is attached to it"
	case 1:
		what += fmt.Sprintf(". %s is attached to it", apps[0])
	default:
		what += fmt.Sprintf(". %d apps are attached to it: %s", len(apps), strings.Join(apps, ", "))
	}
	return fmt.Errorf("%s. Re-run with a confirmation to proceed: %w", what, ErrInvalid)
}

// plural renders a count with the right noun, so a message says "1 standby" rather than "1 standbys".
func plural(n int, one, many string) string {
	if n == 1 {
		return fmt.Sprintf("%d %s", n, one)
	}
	return fmt.Sprintf("%d %s", n, many)
}

// writeReadAddress gives every attached app the read address the instance's first standby just made
// real, and restarts it onto the variable (ADR-0081 §2).
//
// THE RESTART IS THE POINT AND IT COSTS NOTHING. An app does not benefit from a replica by existing
// near one — somebody has to route read-only queries down the second connection, which is a code
// change and a deploy anyway — so a restart at the moment the standby is provisioned is paid out of a
// budget that was already going to be spent.
//
// A per-app failure is recorded rather than raised. The standby exists by the time this runs; failing
// the whole call would send an operator to repeat a scale-up that already happened.
func (e *Engine) writeReadAddress(ctx context.Context, t AddonType, targetEnv, ns string, apps []string) ReadAddressChange {
	out := ReadAddressChange{Action: ReadAddressWritten, Apps: []string{}}
	reader, ok := e.dbProvisioner.(AppReadAddresser)
	if !ok {
		// No composed read address is not a failure of the scale-up: the standby is there, it is
		// serving, and every app's primary connection is untouched. What is missing is the second
		// variable, and saying so plainly is better than a silent success that teaches an operator the
		// address exists.
		return ReadAddressChange{
			Action: ReadAddressUnchanged,
			Apps:   []string{},
			Note:   "this control plane cannot compose a read address, so none was written; the standby is serving and every app's existing connection string is unchanged",
		}
	}
	k := e.k8s.WithNamespace(ns)
	for _, app := range apps {
		key, err := e.db.AddonEnvKey(ctx, string(t), app, targetEnv)
		if err != nil {
			out.Stranded = append(out.Stranded, StrandedApp{App: app, Reason: readAddressStranded(t, app, targetEnv, "the variable its connection string is written under could not be read, so no read address was written for it")})
			continue
		}
		// The returned url is a SECRET value: from here it is handed only to SetSecretValue and never
		// logged, audited, or returned.
		url, err := reader.AppReadURL(ctx, app, targetEnv)
		if err != nil {
			out.Stranded = append(out.Stranded, StrandedApp{App: app, Reason: readAddressStranded(t, app, targetEnv, "its read address could not be composed")})
			continue
		}
		if err := k.SetSecretValue(ctx, app, readAddressKey(key), url); err != nil {
			// The error names the app and key only — never the value.
			out.Stranded = append(out.Stranded, StrandedApp{App: app, Reason: readAddressStranded(t, app, targetEnv, "its "+readAddressKey(key)+" could not be written")})
			continue
		}
		// A key the app has never held, so this rolls through the one helper that knows whether the
		// app's pod template names its secret keys (ADR-0089 §4). A plain restart would bring an
		// enumerated app back with the variable in its Secret and absent from its environment.
		if err := e.rollForSecretChange(ctx, k, "addon config "+string(t), app, targetEnv); err != nil {
			out.Stranded = append(out.Stranded, StrandedApp{App: app, Reason: "its read address was written but the app could not be restarted onto it, so its pods will not see the variable until something restarts them"})
			continue
		}
		out.Apps = append(out.Apps, app)
	}
	return out
}

// withdrawReadAddress removes the read address from every attached app and restarts them, which is
// ADR-0082 §3 — the exact inverse of the operation that added it.
//
// THE ALTERNATIVE IS A VARIABLE POINTING AT NOTHING. `-ro` selects standbys, so with the last one
// gone it resolves to no endpoint at all, and an app that kept the variable would fail at its next
// read rather than at the operation that caused it. A failure at the moment of the change is one
// somebody can connect to the change.
func (e *Engine) withdrawReadAddress(ctx context.Context, t AddonType, targetEnv, ns string, apps []string) ReadAddressChange {
	out := ReadAddressChange{Action: ReadAddressWithdrawn, Apps: []string{}}
	k := e.k8s.WithNamespace(ns)
	for _, app := range apps {
		key, err := e.db.AddonEnvKey(ctx, string(t), app, targetEnv)
		if err != nil {
			out.Stranded = append(out.Stranded, StrandedApp{App: app, Reason: readAddressStranded(t, app, targetEnv, "the variable its connection string is written under could not be read, so its read address was left in place and now resolves to nothing")})
			continue
		}
		// Removing an absent key is a no-op, which is what makes this safe on an app that never had the
		// address — one attached while the instance already had a standby has it, one attached before
		// the standby existed and never re-attached does not, and the withdrawal does not need to know
		// which.
		if err := k.UnsetSecretKey(ctx, app, readAddressKey(key)); err != nil {
			out.Stranded = append(out.Stranded, StrandedApp{App: app, Reason: readAddressStranded(t, app, targetEnv, "its "+readAddressKey(key)+" could not be removed, so it still holds an address that resolves to nothing")})
			continue
		}
		if err := e.rollForSecretChange(ctx, k, "addon config "+string(t), app, targetEnv); err != nil {
			out.Stranded = append(out.Stranded, StrandedApp{App: app, Reason: "its read address was removed but the app could not be restarted, so its pods are still holding the address until something restarts them"})
			continue
		}
		out.Apps = append(out.Apps, app)
	}
	return out
}

// readAddressStranded phrases one app's failure with the command that fixes it. A re-attach is the
// repair for every one of them: it rewrites the connection string and, at an instance with a standby,
// the read address beside it.
func readAddressStranded(t AddonType, app, env, what string) string {
	return fmt.Sprintf("%s; re-attach it with `burrow addon attach %s %s --env %s`", what, t, app, env)
}

// parseStandbys reads the standby count an operator typed. It refuses a negative number and anything
// that is not a plain integer, because "how many standbys" has no other spelling.
func parseStandbys(value string) (int, error) {
	n, err := strconv.Atoi(strings.TrimSpace(value))
	if err != nil {
		return 0, fmt.Errorf("%q is not a standby count; it is a whole number of standbys to run beside the primary, so 0 for none and 1 for one: %w", value, ErrInvalid)
	}
	if n < 0 {
		return 0, fmt.Errorf("%d is not a standby count; the fewest an instance can run is 0, which is a primary on its own: %w", n, ErrInvalid)
	}
	return n, nil
}

// parseStorageSize reads a Kubernetes quantity as a number of bytes, so two sizes can be compared.
//
// It accepts the binary suffixes a volume is actually written with (Ki/Mi/Gi/Ti/Pi), their decimal
// counterparts, and a bare byte count. It does NOT accept a fractional mantissa: `1.5Gi` is a size a
// storage class may round anyway, and comparing a rounded value against the one that was typed is how
// a shrink gets waved through as "the same size".
//
// It lives here rather than reaching for apimachinery's resource.Quantity because this package holds
// no Kubernetes dependency — the adapter has one, and the adapter is not where a refusal that has to
// be made "at the point of asking" belongs.
func parseStorageSize(value string) (int64, error) {
	v := strings.TrimSpace(value)
	if v == "" {
		return 0, fmt.Errorf("a volume size is required, as a Kubernetes quantity (50Gi): %w", ErrInvalid)
	}
	suffixes := []struct {
		suffix string
		unit   int64
	}{
		{"Ki", 1 << 10}, {"Mi", 1 << 20}, {"Gi", 1 << 30}, {"Ti", 1 << 40}, {"Pi", 1 << 50},
		{"k", 1e3}, {"M", 1e6}, {"G", 1e9}, {"T", 1e12}, {"P", 1e15},
	}
	for _, s := range suffixes {
		digits, ok := strings.CutSuffix(v, s.suffix)
		if !ok {
			continue
		}
		n, err := strconv.ParseInt(digits, 10, 64)
		if err != nil || n < 0 {
			return 0, invalidStorageSize(value)
		}
		return n * s.unit, nil
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil || n < 0 {
		return 0, invalidStorageSize(value)
	}
	return n, nil
}

func invalidStorageSize(value string) error {
	return fmt.Errorf("%q is not a volume size; give it as a whole Kubernetes quantity, for example 50Gi: %w", value, ErrInvalid)
}
