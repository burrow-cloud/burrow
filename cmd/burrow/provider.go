// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"
	"golang.org/x/term"

	"github.com/burrow-cloud/burrow/client"
)

// providerCatalog is the provider types this CLI knows about and the capabilities each serves,
// mirroring controlplane's known provider types — the reference behind `provider types` and the
// add command's help. The control plane validates the type authoritatively on `provider add`
// (its error names the supported types), so this is only a hint and never rejects a type itself.
var providerCatalog = []struct {
	Type         string
	Capabilities []string
}{
	{"cloudflare", []string{"dns"}},
	{"digitalocean", []string{"dns"}},
	{"github", []string{"source"}},
	{"gitlab", []string{"source"}},
	{"s3", []string{"object-storage"}},
}

// objectStorageType is the provider type whose credential is a PAIR and whose configuration names a
// destination rather than a vendor (ADR-0063 §1), so `provider add` reads it differently: an
// endpoint, a region, a bucket, an access key id, and a secret access key from stdin.
const objectStorageType = "s3"

func supportedProviderTypes() []string {
	out := make([]string, len(providerCatalog))
	for i, p := range providerCatalog {
		out[i] = p.Type
	}
	return out
}

func providerTypesHint() string { return strings.Join(supportedProviderTypes(), ", ") }

// newProviderCmd manages cloud-provider credentials. `provider add` is a setup command: the token
// travels over burrowd's authenticated control-plane API (TLS), which validates it and writes it
// into the burrow-credentials Secret (ADR-0030). burrowd holds the token; it never travels over the
// agent control channel and the agent never holds it.
func newProviderCmd() *cobra.Command {
	parent := &cobra.Command{
		Use:   "provider",
		Short: "Configure provider credentials (add/list)",
		Long: "provider registers the provider credentials Burrow uses on your behalf — a\n" +
			"DigitalOcean or Cloudflare API token for DNS, or a GitHub/GitLab token whose one\n" +
			"value the in-cluster build uses to clone a PRIVATE git source AND to authenticate\n" +
			"the provider's image registry. The token travels over burrowd's\n" +
			"authenticated control-plane API (TLS), which stores it in a Kubernetes Secret in the\n" +
			"control-plane namespace; it never travels over the agent control channel and the agent\n" +
			"never holds it.",
	}
	parent.AddCommand(newProviderTypesCmd(), newProviderAddCmd(), newProviderListCmd())
	return parent
}

// newProviderTypesCmd lists the provider types Burrow supports and the capabilities each
// serves, so a user can see what is available before configuring one. It needs no cluster.
func newProviderTypesCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "types",
		Short: "List the available provider types and what each supports",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			out := cmd.OutOrStdout()
			tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
			fmt.Fprintln(tw, "TYPE\tSUPPORTS")
			for _, p := range providerCatalog {
				fmt.Fprintf(tw, "%s\t%s\n", p.Type, strings.Join(p.Capabilities, ", "))
			}
			return tw.Flush()
		},
	}
}

func newProviderAddCmd() *cobra.Command {
	o := &commonOpts{}
	var name, secretKey string
	var endpoint, region, bucket, accessKeyID string
	var createBucket, confirm bool
	var retentionDays int
	cmd := &cobra.Command{
		Use:   "add <type>",
		Short: "Register a provider credential (type: " + providerTypesHint() + ")",
		Long: "add registers one provider credential and stores it in the burrow-credentials Secret. You\n" +
			"are prompted for the token with the input hidden, so it never lands in your shell history\n" +
			"or the process table; a script pipes it in instead. The token travels over burrowd's\n" +
			"authenticated control-plane API (TLS), never over the agent control channel, and is never\n" +
			"logged.\n\n" +
			"A DNS provider (cloudflare, digitalocean) is validated against the vendor before anything\n" +
			"is written, so a wrong token fails here rather than at the first record.\n\n" +
			"A source provider (github, gitlab) supplies ONE token that both clones a private repo and\n" +
			"authenticates that provider's image registry. A fine-grained token scoped to the repos you\n" +
			"build, plus read:packages where the registry is shared, keeps the blast radius small.\n\n" +
			"An s3 provider is a BACKUP DESTINATION, addressed by S3-compatible endpoint rather than by\n" +
			"vendor. Its credential is a pair: --access-key-id, and a secret access key read from stdin.\n" +
			"Registering it verifies the destination — the bucket is created or checked, a probe object\n" +
			"is written and deleted so a wrong key fails now rather than at the first backup, and a\n" +
			"bucket whose lifecycle rules would expire a backup early is refused. Scope this credential\n" +
			"to one bucket where the vendor permits it: it is the most consequential key in\n" +
			"burrow-credentials.\n\n" +
			"Pass --name to register more than one provider of the same type.\n\n" +
			"Supported types: " + providerTypesHint() + " (see `burrow config provider types`).",
		Example: "  burrow config provider add cloudflare\n" +
			"  burrow config provider add digitalocean --name do-dns\n" +
			"  echo \"$GH_PAT\" | burrow config provider add github\n" +
			"  printf '%s' \"$B2_SECRET\" | burrow config provider add s3 \\\n" +
			"      --endpoint https://s3.us-west-002.backblazeb2.com --region us-west-002 \\\n" +
			"      --access-key-id \"$B2_KEY_ID\" --create-bucket --retention-days 30 --confirm",
		ValidArgs: supportedProviderTypes(),
		Args:      exactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			providerType := args[0]
			// Refuse a target with no cluster BEFORE the credential is asked for. This command writes
			// into a cluster's burrow-credentials Secret, so with Burrow Cloud selected there is
			// nowhere for it to go (clusteronly.go) — and prompting for a token first would have
			// somebody produce a credential for a registration that was never going to happen.
			if err := o.requireCluster(); err != nil {
				return err
			}
			if providerType == objectStorageType {
				return addObjectStorageProvider(cmd, o, objectStorageOpts{
					name:          name,
					endpoint:      endpoint,
					region:        region,
					bucket:        bucket,
					createBucket:  createBucket,
					retentionDays: retentionDays,
					accessKeyID:   accessKeyID,
					confirm:       confirm,
				})
			}
			token, err := readToken(cmd.InOrStdin(), cmd.OutOrStdout(), fmt.Sprintf("Enter the %s API token: ", providerType))
			if err != nil {
				return err
			}
			if token == "" {
				return errors.New("no token provided")
			}

			providerName := name
			if providerName == "" {
				providerName = providerType
			}
			key := secretKey
			if key == "" {
				key = providerName
			}

			// Send the token to burrowd over its authenticated control-plane API (TLS). burrowd
			// validates it against the vendor, writes it into the burrow-credentials Secret, and
			// records the registry entry — a rejected token writes nothing (ADR-0030). The token
			// travels only in the request body; it never crosses the agent control channel and is
			// never logged.
			c, err := o.client(ctx, cmd.ErrOrStderr())
			if err != nil {
				return err
			}
			p, err := c.AddProvider(ctx, client.AddProviderRequest{Name: providerName, Type: providerType, SecretKey: key, Token: token})
			if err != nil {
				return err
			}

			out := cmd.OutOrStdout()
			return o.emitChange(out, p, tokenProviderSummary(out, p))
		},
	}
	bindCommon(cmd.Flags(), o)
	cmd.Flags().StringVar(&name, "name", "", "name for this provider (default: the type)")
	cmd.Flags().StringVar(&secretKey, "secret-key", "", "key in the burrow-credentials Secret to store the token under (default: the name)")
	cmd.Flags().StringVar(&endpoint, "endpoint", "", "s3: the S3-compatible API endpoint (the vendor is whoever answers it)")
	cmd.Flags().StringVar(&region, "region", "", "s3: the region the request is signed for (default: us-east-1)")
	cmd.Flags().StringVar(&bucket, "bucket", "", "s3: an existing bucket to use; mutually exclusive with --create-bucket")
	cmd.Flags().BoolVar(&createBucket, "create-bucket", false, "s3: have Burrow create and record its own bucket (guardrail: bucket.create)")
	cmd.Flags().IntVar(&retentionDays, "retention-days", 0, "s3: how long backups here must stay restorable; bucket lifecycle rules are refused if they expire sooner")
	cmd.Flags().StringVar(&accessKeyID, "access-key-id", "", "s3: the access key id (its secret access key is read from stdin)")
	cmd.Flags().BoolVar(&confirm, "confirm", false, "confirm an operation a guardrail holds for confirmation")
	return cmd
}

// tokenProviderSummary is what a human is told after a single-token registration — a DNS or a source
// provider. It is the same shape as objectStorageSummary and for the same reason (issue #465): one
// status per line, each marked as a result the control plane confirmed, so a reader scanning for
// "did it work" reads marks rather than sentences.
//
// Both lines tick because both are results: the registration is recorded, and the token is in
// burrow-credentials under the key named — burrowd wrote nothing at all if it rejected the token
// (ADR-0030), so arriving here IS the confirmation. The key NAME is reported and the value never is.
func tokenProviderSummary(w io.Writer, p client.Provider) string {
	ok := okMark(w) + " "
	return fmt.Sprintf("%sregistered provider %q (type %s, capabilities %s)\n"+
		"%stoken: stored in burrow-credentials under key %q",
		ok, p.Name, p.Type, strings.Join(p.Capabilities, ", "), ok, p.SecretKey)
}

// objectStorageOpts are the flags of an object-storage registration. The secret access key is NOT
// among them: it is read from stdin like every other credential value, so it never lands in shell
// history or the process table.
type objectStorageOpts struct {
	name          string
	endpoint      string
	region        string
	bucket        string
	createBucket  bool
	retentionDays int
	accessKeyID   string
	confirm       bool
}

// addObjectStorageProvider registers an S3-compatible backup destination (ADR-0063). The control
// plane does the work that matters — it creates or verifies the bucket, writes and deletes a probe
// object so a wrong key fails NOW rather than at the first scheduled backup, and refuses a bucket
// whose lifecycle rules would expire a backup that must stay restorable. This side reads the
// credential pair and reports what the control plane observed.
func addObjectStorageProvider(cmd *cobra.Command, o *commonOpts, opts objectStorageOpts) error {
	ctx := cmd.Context()
	if opts.accessKeyID == "" {
		return errors.New("--access-key-id is required for an s3 provider (the secret access key is read from stdin)")
	}
	secret, err := readToken(cmd.InOrStdin(), cmd.OutOrStdout(), "Enter the S3 secret access key: ")
	if err != nil {
		return err
	}
	if secret == "" {
		return errors.New("no secret access key provided")
	}

	providerName := opts.name
	if providerName == "" {
		providerName = objectStorageType
	}
	c, err := o.client(ctx, cmd.ErrOrStderr())
	if err != nil {
		return err
	}
	p, err := c.AddProvider(ctx, client.AddProviderRequest{
		Name:            providerName,
		Type:            objectStorageType,
		Endpoint:        opts.endpoint,
		Region:          opts.region,
		Bucket:          opts.bucket,
		CreateBucket:    opts.createBucket,
		RetentionDays:   opts.retentionDays,
		AccessKeyID:     opts.accessKeyID,
		SecretAccessKey: secret,
		Confirm:         opts.confirm,
	})
	if err != nil {
		return err
	}
	out := cmd.OutOrStdout()
	return o.emitChange(out, p, objectStorageSummary(out, p))
}

// objectStorageSummary is what a human is told after a registration. It reports the bucket Burrow
// RECORDED (the only one it will ever write to), that the probe object was written and deleted, and
// the lifecycle verdict.
//
// Every line is one status, and a line is TICKED only when it reports something Burrow verified
// (issue #465). Six flowing sentences read the same whether they were a result or standing advice,
// so a reader scanning for "did it work" had to read all of them. Two lines deliberately do not
// tick:
//
//   - lifecycle UNKNOWN. ADR-0063 §3 forbids reporting an unverifiable invariant as verified, and a
//     tick beside UNKNOWN would do exactly that.
//   - the closing advice about scoping the credential. It is not a result, and ticking it would say
//     it had been checked.
//
// The marks go through okMark/note/warning (style.go), so they colour on a terminal and degrade to
// plain labels in piped output and logs. `created this bucket` stays its own line: a bucket Burrow
// created is a different fact from one it verified, and a reader should be able to tell.
func objectStorageSummary(w io.Writer, p client.Provider) string {
	ok := okMark(w) + " "
	var b strings.Builder
	fmt.Fprintf(&b, "%sregistered provider %q (type %s, capabilities %s)\n",
		ok, p.Name, p.Type, strings.Join(p.Capabilities, ", "))
	if p.ObjectStore != nil {
		fmt.Fprintf(&b, "%sbucket: %s at %s\n", ok, p.ObjectStore.Bucket, p.ObjectStore.Endpoint)
		fmt.Fprintf(&b, "%scredential: stored in burrow-credentials under keys %q and %q\n",
			ok, p.ObjectStore.AccessKeyIDKey, p.ObjectStore.SecretAccessKeyKey)
	}
	if p.Verification == nil {
		return strings.TrimRight(b.String(), "\n")
	}
	if p.Verification.BucketCreated {
		fmt.Fprintf(&b, "%sbucket created: Burrow writes only to the bucket it recorded\n", ok)
	}
	if p.Verification.ProbeObject {
		fmt.Fprintf(&b, "%sprobe: an object was written and deleted\n", ok)
	}
	switch p.Verification.Lifecycle.Status {
	case "unknown":
		fmt.Fprintf(&b, "%slifecycle UNKNOWN. %s\n", note(w), p.Verification.Lifecycle.Detail)
	default:
		fmt.Fprintf(&b, "%slifecycle %s: %s\n", ok, p.Verification.Lifecycle.Status, p.Verification.Lifecycle.Detail)
	}
	fmt.Fprintf(&b, "%sScope this credential to this one bucket at the vendor where it permits it. "+
		"It is the most consequential key in burrow-credentials.", warning(w))
	return b.String()
}

func newProviderListCmd() *cobra.Command {
	o := &commonOpts{}
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List configured providers",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := cmd.Context()
			c, err := o.client(ctx, cmd.ErrOrStderr())
			if err != nil {
				return err
			}
			providers, err := c.Providers(ctx)
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			if o.json {
				return emit(out, true, providers, "")
			}
			if len(providers) == 0 {
				fmt.Fprintf(out, "No providers configured. Add one with `burrow config provider add <type>`.\n"+
					"Supported types: %s (see `burrow config provider types`).\n", providerTypesHint())
				return nil
			}
			tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
			fmt.Fprintln(tw, "NAME\tTYPE\tCAPABILITIES")
			for _, p := range providers {
				fmt.Fprintf(tw, "%s\t%s\t%s\n", p.Name, p.Type, strings.Join(p.Capabilities, ","))
			}
			return tw.Flush()
		},
	}
	bindCommon(cmd.Flags(), o)
	return cmd
}

// readToken reads a secret value: a registry token, an S3 secret access key, a secret env value.
// When in is an interactive terminal it prints prompt and reads the value without echoing it (so it
// never shows on screen or in shell history); when in is piped or redirected (a script,
// `echo "$TOKEN" | …`) it reads the value from there instead. Surrounding whitespace is trimmed.
//
// The terminal test goes through the stdinIsTerminal seam (registry.go) so a test can force the
// non-interactive path without a TTY, and so the whole CLI decides "is this a terminal" once.
func readToken(in io.Reader, out io.Writer, prompt string) (string, error) {
	if f, ok := in.(*os.File); ok && stdinIsTerminal(in) {
		fmt.Fprint(out, prompt)
		b, err := term.ReadPassword(int(f.Fd()))
		fmt.Fprintln(out) // terminate the line the hidden input was typed on
		if err != nil {
			return "", fmt.Errorf("reading token: %w", err)
		}
		return strings.TrimSpace(string(b)), nil
	}
	b, err := io.ReadAll(in)
	if err != nil {
		return "", fmt.Errorf("reading token from standard input: %w", err)
	}
	return strings.TrimSpace(string(b)), nil
}
