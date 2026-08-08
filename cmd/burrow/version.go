// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os/exec"
	"runtime/debug"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"
	"golang.org/x/mod/module"
	"golang.org/x/mod/semver"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

	"github.com/burrow-cloud/burrow/connect"
	"github.com/burrow-cloud/burrow/localconfig"
)

// latestReleaseURL is the unauthenticated GitHub API endpoint that returns this repository's
// latest published release. The repository is public, so it reads without a token — and this
// request must carry no Burrow credential, which is why it goes through outboundHTTPClient. It is
// a package var so a test can point the fetch at an httptest server and assert exactly that,
// without reaching GitHub.
var latestReleaseURL = "https://api.github.com/repos/burrow-cloud/burrow/releases/latest"

// latestReleaseTimeout bounds the latest-release check so `burrow version` never hangs on a slow
// or unreachable network: the check is best-effort and skipped silently on any failure.
const latestReleaseTimeout = 3 * time.Second

// fetchLatestRelease returns the tag of the latest published release (e.g. "v0.7.2"), or an error.
// It is a package var so tests can fake it with no network. It is best-effort by contract: any
// failure (offline, timeout, non-200, rate-limited, malformed body) is returned so the caller can
// skip the release check silently rather than fail `burrow version`.
var fetchLatestRelease = func(ctx context.Context) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, latestReleaseTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, latestReleaseURL, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	resp, err := outboundHTTPClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("github releases API returned %s", resp.Status)
	}
	var body struct {
		TagName string `json:"tag_name"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return "", err
	}
	return body.TagName, nil
}

// releasesURL lists this repository's published releases, newest first. Unlike /releases/latest it
// INCLUDES prereleases, which is the only way to answer "is there a newer rc than the one I am
// running". One page is plenty: the question is only ever about the newest few tags.
//
// It is a package var for the same reason latestReleaseURL is: so a test can point the fetch at an
// httptest server and assert that this request carries no Burrow credential either, without reaching
// GitHub.
var releasesURL = "https://api.github.com/repos/burrow-cloud/burrow/releases?per_page=30"

// fetchReleaseTags returns the tags of the most recently published releases, prereleases included
// and drafts excluded (a draft is not published, so it is not something to upgrade to). It is a
// package var so tests can fake it with no network, and it is best-effort by contract exactly as
// fetchLatestRelease is: every failure is returned for the caller to skip the check silently.
//
// Like its sibling it goes through outboundHTTPClient and not http.DefaultClient: api.github.com is
// a third party with no business seeing a control-plane credential, and this is the request an rc
// cycle makes on every `burrow version` (issue #442).
var fetchReleaseTags = func(ctx context.Context) ([]string, error) {
	ctx, cancel := context.WithTimeout(ctx, latestReleaseTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, releasesURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	resp, err := outboundHTTPClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("github releases API returned %s", resp.Status)
	}
	var body []struct {
		TagName string `json:"tag_name"`
		Draft   bool   `json:"draft"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return nil, err
	}
	tags := make([]string, 0, len(body))
	for _, r := range body {
		if !r.Draft {
			tags = append(tags, r.TagName)
		}
	}
	return tags, nil
}

// isPrerelease reports whether a version is a release candidate or other prerelease tag, and not a
// Go pseudo-version. A pseudo-version carries a prerelease field of its own ("v0.7.3-0.2026…") but
// it is a local source build rather than something published to compare against.
func isPrerelease(v string) bool {
	return semver.IsValid(v) && semver.Prerelease(v) != "" && !module.IsPseudoVersion(v)
}

// newestTag returns the highest valid semver tag in the list, or "" when none is valid. GitHub
// returns releases in publication order, which is not version order once a patch is cut on an older
// line, so the comparison is done here rather than trusting the first element.
func newestTag(tags []string) string {
	var newest string
	for _, t := range tags {
		if !semver.IsValid(t) {
			continue
		}
		if newest == "" || semver.Compare(t, newest) > 0 {
			newest = t
		}
	}
	return newest
}

// latestReleaseFor returns the release this CLI should be compared against.
//
// A CLI on a stable release is compared against the latest STABLE release: an rc is not an upgrade
// for somebody who did not ask for one, and offering it would be telling them to leave the released
// line. A CLI on a prerelease is compared against the newest release of either kind, because the
// whole point of running an rc is to test the current one, and /releases/latest cannot see rcs at
// all — during an rc cycle it names a tag OLDER than what is already installed.
//
// Both paths are best-effort, as the release check has always been: a failure is returned and the
// caller skips the check silently rather than failing `burrow version`.
func latestReleaseFor(ctx context.Context, cliVer string) (string, error) {
	if !isPrerelease(cliVer) {
		return fetchLatestRelease(ctx)
	}
	tags, err := fetchReleaseTags(ctx)
	if err != nil {
		return "", err
	}
	newest := newestTag(tags)
	if newest == "" {
		return "", fmt.Errorf("no published release carries a version tag")
	}
	return newest, nil
}

// agentBinary is the agent's control-channel executable (ADR-0049 §1), looked up on PATH so
// `burrow version` can report the version the AGENT would send to the control plane, not just the
// CLI's. The two binaries ship together but are installed and updated independently, so they drift;
// before this line existed the drift was invisible until every agent call started being refused.
const agentBinary = "burrow-agent"

// agentProbeTimeout bounds the `burrow-agent --version` probe. It is a local exec, so this is a
// guard against a wedged binary rather than a latency budget.
const agentProbeTimeout = 3 * time.Second

// probeAgentVersion returns the path of the burrow-agent on PATH and the version it reports, or an
// error when it is absent or unreadable. It is a package var so tests exercise the rendering and the
// skew comparison without a burrow-agent on the test machine's PATH. Best effort by contract: every
// failure is returned for the caller to render, never to fail `burrow version`.
var probeAgentVersion = func(ctx context.Context) (path, version string, err error) {
	path, err = exec.LookPath(agentBinary)
	if err != nil {
		return "", "", err
	}
	ctx, cancel := context.WithTimeout(ctx, agentProbeTimeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, path, "--version").Output()
	if err != nil {
		return path, "", err
	}
	var body struct {
		Version string `json:"version"`
	}
	if err := json.Unmarshal(out, &body); err != nil || body.Version == "" {
		// A burrow-agent old enough to predate the --version flag prints usage to stderr and exits
		// non-zero (handled above) or prints something unparseable here. Either way its version is
		// unknown, which is itself the useful signal: it is older than this CLI.
		return path, "", fmt.Errorf("%s does not report a version (it predates `--version`)", agentBinary)
	}
	return path, body.Version, nil
}

// agentValue renders the value cell of the "burrow-agent" line: its version and where it is
// installed, or why it could not be read. The PATH is included because the two binaries can be
// installed by different means into different prefixes, and which copy is first on PATH is exactly
// the thing a stranded user cannot see. It is pure so all three renderings are unit-tested.
func agentValue(path, agentVer string, err error) string {
	switch {
	case err == nil:
		return fmt.Sprintf("%s (%s)", agentVer, path)
	case path != "":
		return fmt.Sprintf("unknown version (%s)", path)
	default:
		return fmt.Sprintf("not found on PATH (install it: %s)", agentInstallHint)
	}
}

// agentInstallHint is the one-liner for a machine with no burrow-agent at all.
const agentInstallHint = "`brew install burrow-cloud/tap/burrow` installs burrow and burrow-agent together"

// agentSkewHint returns the nudge to update burrow-agent when it is behind the CLI, or "" when it is
// not or when either version is a local build with nothing to upgrade to. This is the ACTIVE half of
// keeping the two binaries in step: neither binary updates the other (distribution is Homebrew and
// release archives with the CLI as the unit a user installs, ADR-0016; burrow-agent is
// capability-reduced and does not rewrite binaries, ADR-0049 §2a), so the CLI's job is to make the
// drift visible here, before the control plane has to refuse a call to reveal it. It names both the
// Homebrew and source remedies because the CLI cannot know how the agent binary was installed;
// burrow-agent itself narrows that further when it is refused, where the installed path is knowable.
//
// The Homebrew remedy names the formula TAP-QUALIFIED, as every Homebrew instruction in Burrow does:
// homebrew-core has an unrelated `burrow` formula, so a bare `brew upgrade burrow` upgrades somebody
// else's project or fails as not-installed.
func agentSkewHint(cliVer, agentVer string) string {
	if !semver.IsValid(cliVer) || !semver.IsValid(agentVer) {
		return ""
	}
	if module.IsPseudoVersion(cliVer) || module.IsPseudoVersion(agentVer) {
		return ""
	}
	if semver.Compare(semver.MajorMinor(agentVer), semver.MajorMinor(cliVer)) >= 0 {
		return ""
	}
	return fmt.Sprintf("Your %s (%s) is behind your burrow CLI (%s). They are separate binaries: "+
		"`brew upgrade burrow-cloud/tap/burrow` updates both, or from source "+
		"`go install github.com/burrow-cloud/burrow/cmd/burrow-agent@%s`. "+
		"Restart your agent session afterwards so it runs the new binary.",
		agentBinary, agentVer, cliVer, cliVer)
}

// newVersionCmd reports this CLI's version, the burrow-agent on PATH, and, best effort, the version
// of the control plane installed in the cluster — read from the burrowd Deployment's image, so it
// works even if burrowd is unhealthy and needs no API token.
func newVersionCmd() *cobra.Command {
	var kubeconfig, kubeContextFlag, namespace string
	cmd := &cobra.Command{
		Use:   "version",
		Short: "Print the CLI version and the installed control-plane version",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			out := cmd.OutOrStdout()
			// Render the status lines through one tabwriter so the CLI, control-plane, and
			// latest-release values column-align regardless of their differing label widths. The
			// CLI line prints first and always; control-plane connectivity never blocks it.
			tw := tabwriter.NewWriter(out, 0, 0, 3, ' ', 0)
			fmt.Fprintf(tw, "burrow (CLI):\t%s\n", cliVersion())

			// The agent's binary is the one that goes stale unnoticed after a CLI upgrade, so it gets
			// its own line rather than being inferred from a failing agent session (issue #308).
			agentPath, agentVer, agentErr := probeAgentVersion(cmd.Context())
			fmt.Fprintf(tw, "burrow-agent:\t%s\n", agentValue(agentPath, agentVer, agentErr))

			// cpVer is the control plane's release version, read from the burrowd image tag on a
			// successful probe and left empty otherwise (not installed, unreachable, or not a cluster
			// at all). It feeds the latest-release comparison below.
			var cpVer string

			// The managed product is answered here and no cluster is read for it. It owns no cluster,
			// so there is nothing a kubeconfig could be asked; before this branch existed the read
			// fell through to whatever context kubectl pointed at and reported on it, which is a fact
			// about a cluster the reader never named presented as the answer to their question
			// (cloud#209). An explicit --context is still a person naming a cluster for this one
			// invocation and still wins, exactly as it does everywhere else (ADR-0078 §4).
			cloud, onCloud := localconfig.Target{}, false
			if kubeContextFlag == "" {
				cloud, onCloud = selectedCloudTarget()
			}
			if onCloud {
				fmt.Fprintf(tw, "control plane:\t%s\n", cloudControlPlaneValue(cloud))
			} else {
				// The cluster read is the one the active target names, like every other command that
				// reaches a cluster (ADR-0084 §4), resolved by the same function they use so the
				// precedence cannot drift into a second version of itself here.
				//
				// Its errors are swallowed rather than returned, which is the one thing this command
				// does differently and is deliberate: `burrow version` is what a person runs when
				// something is already wrong, and a target whose context has been renamed away is
				// exactly such a moment. Refusing to print the CLI's own version over it would be
				// useless. It degrades to the kubeconfig and names the context it used either way.
				kubeContext, ctxErr := (&commonOpts{kubeconfig: kubeconfig, context: kubeContextFlag}).clusterContext(cmd.ErrOrStderr())
				if ctxErr != nil {
					kubeContext = kubeContextFlag
				}

				// Name the targeted context so the control-plane line is legible in both the success
				// and failure cases. Best effort: a missing or unreadable kubeconfig leaves it empty,
				// which the failure path below still reports cleanly.
				ctxName, _ := connect.TargetContextName(kubeconfig, kubeContext)

				ctx, cancel := context.WithTimeout(cmd.Context(), connect.ProbeTimeout)
				defer cancel()

				cs, err := clientsetForContext(kubeconfig, kubeContext)
				if err != nil {
					fmt.Fprintf(tw, "control plane:\t%s\n", controlPlaneValue("", err, ctxName, namespace))
				} else if img, imgErr := burrowdImage(ctx, cs, namespace); imgErr != nil {
					fmt.Fprintf(tw, "control plane:\t%s\n", controlPlaneValue(img, imgErr, ctxName, namespace))
				} else {
					fmt.Fprintf(tw, "control plane:\t%s\n", controlPlaneValue(img, nil, ctxName, namespace))
					cpVer = imageTag(img)
				}
			}

			// Best effort: compare against the latest published release and flag an outdated CLI or
			// control plane. Any failure (offline, timeout, rate-limited) is skipped silently, so
			// `burrow version` still works with no network and never hangs.
			var hints []string
			if latest, lerr := latestReleaseFor(cmd.Context(), cliVersion()); lerr == nil && latest != "" {
				fmt.Fprintf(tw, "latest release:\t%s\n", latest)
				hints = upgradeHints(cliVersion(), cpVer, latest)
			}
			// The agent-skew nudge needs no network, so it prints whether or not the release check
			// succeeded, and it leads: a stale burrow-agent is a hard refusal on every agent call,
			// while a lagging CLI or control plane is only an advisory.
			if hint := agentSkewHint(cliVersion(), agentVer); hint != "" {
				hints = append([]string{hint}, hints...)
			}
			// Flush the aligned block before the hints, which print at the left margin unaligned.
			_ = tw.Flush()
			for _, hint := range hints {
				fmt.Fprintln(out, hint)
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&kubeconfig, "kubeconfig", "", "path to kubeconfig (default: ambient)")
	cmd.Flags().StringVar(&kubeContextFlag, "context", "", "kubeconfig context to target (default: the active target, else the current context)")
	cmd.Flags().StringVar(&namespace, "namespace", connect.DefaultNamespace, "namespace the control plane is installed in")
	return cmd
}

// cloudControlPlaneValue renders the "control plane" line for a Burrow Cloud target.
//
// It says the line does not apply and names the target it does not apply to, rather than being
// omitted. Which of those to do is a product call and not a plumbing one, and this is the cheaper
// mistake of the two: a reader who finds the line missing cannot tell whether Burrow decided it was
// irrelevant or failed to read it, whereas a reader who finds it stated has been told which target
// they are on — the very fact that was wrong before (cloud#209) — and can be given a version instead
// the moment the managed product exposes one.
//
// What it must never do is report a number. The managed product's control plane is the operator's
// and is upgraded by the operator; a tenant has nothing to do about its version, and no route serves
// it. Printing the tenant's nearest cluster there was the bug.
func cloudControlPlaneValue(t localconfig.Target) string {
	return fmt.Sprintf("not applicable: %s runs its own, upgraded for you", t.Describe())
}

// controlPlaneValue renders the value cell of the "control plane" line from a probe result: the
// burrowd image tag and the error from reading its Deployment, plus the targeted context and
// namespace. It is the cell content only (no "control plane:" label) so the caller can align it in
// a tabwriter, and it is pure so the success, not-installed, and unreachable renderings are
// unit-tested without a cluster.
func controlPlaneValue(img string, err error, ctxName, namespace string) string {
	switch {
	case err == nil:
		return fmt.Sprintf("%s (context %q, namespace %q)", imageTag(img), ctxName, namespace)
	case apierrors.IsNotFound(err):
		return fmt.Sprintf("not installed (context %q, namespace %q)", ctxName, namespace)
	default:
		return fmt.Sprintf("unreachable via context %q (%s)", ctxName, connect.FailureReason(err))
	}
}

// version is the CLI release version, stamped at build time with
// `-ldflags "-X main.version=<tag>"` (goreleaser injects the release tag on a tagged build).
// It is empty for a local `go build`, a `go install …@version`, or a test binary, in which
// case cliVersion falls back to the build info — keeping the Go pseudo-version for a local
// source build rather than overwriting it with a stale constant.
var version string

// cliVersion returns this CLI's release version: the ldflags-stamped tag for a release build,
// otherwise the main-module version from the build info — set when it is installed with
// `go install …@version` or a Go pseudo-version for a local source build — or "dev" for an
// unversioned build.
func cliVersion() string {
	if version != "" {
		return version
	}
	if bi, ok := debug.ReadBuildInfo(); ok && bi.Main.Version != "" && bi.Main.Version != "(devel)" {
		return bi.Main.Version
	}
	return "dev"
}

// upgradeHints compares the CLI and control-plane versions against the latest published release and
// returns the applicable upgrade hint lines. It is pure so the comparison is unit-testable without a
// cluster or network:
//   - a control plane on a valid release older than latest gets the `burrow cluster upgrade` hint;
//   - a CLI on a valid, non-pseudo release older than latest gets the `brew upgrade` hint (a local
//     dev/pseudo build is exempt, since there is nothing to brew-upgrade). When the newer tag is
//     itself a prerelease the hint names the prerelease route instead, because the Homebrew tap
//     ships released versions and `brew upgrade` would not install an rc;
//   - a CLI AHEAD of the latest release is told so, which is the normal state during an rc cycle
//     and is not the same statement as being on the latest release (issue #442);
//   - when neither is behind and neither is ahead, a single reassurance that this is the latest
//     release.
//
// It assumes latest is a non-empty tag (the caller only calls it when the release check succeeded).
func upgradeHints(cliVer, cpVer, latest string) []string {
	var hints []string
	if semver.IsValid(cpVer) && semver.Compare(cpVer, latest) < 0 {
		hints = append(hints, fmt.Sprintf("Your control plane is behind. Run `burrow cluster upgrade` to update it to %s.", latest))
	}
	if semver.IsValid(cliVer) && !module.IsPseudoVersion(cliVer) && semver.Compare(cliVer, latest) < 0 {
		if isPrerelease(latest) {
			hints = append(hints, fmt.Sprintf("A newer prerelease (%s) is available. Install it from the release page, "+
				"or with `go install github.com/burrow-cloud/burrow/cmd/burrow@%s`.", latest, latest))
		} else {
			hints = append(hints, fmt.Sprintf("A newer burrow (%s) is available. Run `brew upgrade burrow-cloud/tap/burrow`.", latest))
		}
	}
	if len(hints) == 0 {
		if semver.IsValid(cliVer) && !module.IsPseudoVersion(cliVer) && semver.Compare(cliVer, latest) > 0 {
			hints = append(hints, fmt.Sprintf("You are on %s, ahead of the latest release %s.", cliVer, latest))
		} else {
			hints = append(hints, "You are on the latest release.")
		}
	}
	return hints
}

// imageTag returns just the version tag of an image reference (the part after the last colon,
// ignoring any registry-host port and stripping a digest), e.g.
// "ghcr.io/burrow-cloud/burrowd:v0.2.1" -> "v0.2.1". An untagged image is returned unchanged.
func imageTag(image string) string {
	if at := strings.Index(image, "@"); at >= 0 {
		image = image[:at] // drop a digest: name@sha256:...
	}
	if colon := strings.LastIndex(image, ":"); colon > strings.LastIndex(image, "/") {
		return image[colon+1:]
	}
	return image
}

// burrowdImage returns the image of the installed burrowd Deployment, or an error — an
// IsNotFound error when no control plane is installed in the namespace.
func burrowdImage(ctx context.Context, cs kubernetes.Interface, namespace string) (string, error) {
	d, err := cs.AppsV1().Deployments(namespace).Get(ctx, "burrowd", metav1.GetOptions{})
	if err != nil {
		return "", err
	}
	for _, c := range d.Spec.Template.Spec.Containers {
		if c.Name == "burrowd" {
			return c.Image, nil
		}
	}
	return "", fmt.Errorf("the burrowd deployment in %s has no burrowd container", namespace)
}
