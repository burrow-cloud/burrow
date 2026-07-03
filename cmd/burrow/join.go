// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package main

import (
	"context"
	"fmt"
	"io"

	"github.com/spf13/cobra"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"

	"github.com/burrow-cloud/burrow/connect"
	"github.com/burrow-cloud/burrow/internal/jointoken"
	"github.com/burrow-cloud/burrow/localconfig"
)

// joinArgs are the resolved inputs to a `burrow join` run: the pasted join token, an optional
// environment name, and an optional target kubeconfig to record admin access into.
type joinArgs struct {
	token       string
	environment string
	kubeconfig  string
}

// joinConnectFn builds the admin clientset and REST config from a decoded join token. It is a
// package var so a test can substitute a fake clientset (and a REST config carrying the token's
// server/CA) without a real cluster; the real path is adminConnect.
var joinConnectFn = adminConnect

func newJoinCmd() *cobra.Command {
	a := joinArgs{}
	cmd := &cobra.Command{
		Use:   "join <token>",
		Short: "Join a bootstrapped cluster from its join token",
		Long: "Join the single-VPS cluster a one-time `burrow bootstrap` set up, using the join token it\n" +
			"printed (ADR-0044).\n\n" +
			"The token is admin-grade — treat it like a kubeconfig. Running join on your laptop records\n" +
			"admin access for governance commands (guard, upgrade) and lands the scoped agent credential\n" +
			"(ADR-0038), so from then on every operation runs from the laptop. It is idempotent: re-running\n" +
			"refreshes the recorded credentials.",
		Example: "  # Paste the token `burrow bootstrap` printed on the VPS\n" +
			"  burrow join burrowjoin.v1.eyJ2ZXJ...",
		Args: exactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			a.token = args[0]
			return runJoin(cmd.Context(), a, cmd.OutOrStdout())
		},
	}
	cmd.Flags().StringVar(&a.environment, "environment", "", "name for this environment (default: a generated adjective-animal name)")
	cmd.Flags().StringVar(&a.kubeconfig, "kubeconfig", "", "kubeconfig to record admin access into (default: $KUBECONFIG or ~/.kube/config)")
	return cmd
}

// runJoin consumes a join token (ADR-0044): it decodes the token, records admin access into the
// kubeconfig for the privileged/governance path, and lands the scoped agent credential (ADR-0038)
// by reusing the existing join core. It is idempotent — re-running updates the recorded admin
// context and the scoped credential in place.
func runJoin(ctx context.Context, a joinArgs, stdout io.Writer) error {
	tok, err := jointoken.Decode(a.token)
	if err != nil {
		return err
	}

	// Reach the freshly bootstrapped cluster as admin, straight from the token — the cluster is not
	// in the kubeconfig yet, so the admin credential travels in the token, not via an existing
	// context.
	cs, restCfg, err := joinConnectFn(tok)
	if err != nil {
		return err
	}

	// 1. Record admin access into the kubeconfig so the privileged/governance path
	// (commonOpts.client(): `burrow guard`, `burrow upgrade`) resolves this cluster the same way it
	// resolves any other — by context name in the ambient kubeconfig (ADR-0044). This is the human's
	// admin credential; it legitimately belongs in the kubeconfig, unlike the scoped agent credential
	// (ADR-0038), which stays under ~/.burrow.
	if err := recordAdminKubeconfig(a.kubeconfig, tok); err != nil {
		return err
	}

	// 2. Land the scoped agent credential by reusing the ADR-0038 join core: read the existing
	// burrow-agent-token Secret with the admin access, assemble the scoped kubeconfig, write it under
	// ~/.burrow/agents/<env>, and register/pin the environment handle carrying it. Unlike install's
	// join, the admin clientset/REST config come from the token rather than a kubeconfig context, so
	// joinAgentCredential is called directly with them.
	cfg, err := localconfig.Load()
	if err != nil {
		return err
	}
	name, updateExisting := joinEnvironmentName(cfg, tok.ContextName, a.environment)

	agentKubeconfig, agentContext, err := joinAgentCredential(ctx, cs, restCfg, tok.Namespace, name)
	if err != nil {
		return err
	}

	handle := localconfig.Environment{
		Name:                  name,
		Context:               tok.ContextName,
		ControlPlaneNamespace: tok.Namespace,
		AppNamespace:          connect.DefaultAppNamespace,
		Env:                   "",
	}
	if err := saveJoinedEnvironment(cfg, name, updateExisting, handle, agentKubeconfig, agentContext); err != nil {
		return err
	}

	printJoinSummary(stdout, name, tok.ContextName)
	return nil
}

// adminConnect builds the admin clientset and REST config from a join token: a single-context
// kubeconfig assembled from the token's server, CA, and admin credential (client cert+key or bearer
// token). The REST config's Host and CA flow into the scoped agent kubeconfig, so the agent reaches
// the cluster at the token's public API-server URL.
func adminConnect(tok jointoken.Token) (kubernetes.Interface, *rest.Config, error) {
	restCfg, err := clientcmd.NewDefaultClientConfig(*adminAPIConfig(tok), &clientcmd.ConfigOverrides{}).ClientConfig()
	if err != nil {
		return nil, nil, fmt.Errorf("building admin config from the join token: %w", err)
	}
	cs, err := kubernetes.NewForConfig(restCfg)
	if err != nil {
		return nil, nil, fmt.Errorf("building admin clientset from the join token: %w", err)
	}
	return cs, restCfg, nil
}

// adminAPIConfig builds a self-contained, single cluster/user/context kubeconfig object carrying the
// token's admin access, named for the token's context. It backs both the REST config (adminConnect)
// and the kubeconfig merge (recordAdminKubeconfig), so the recorded admin context and the connection
// agree.
func adminAPIConfig(tok jointoken.Token) *clientcmdapi.Config {
	cfg := clientcmdapi.NewConfig()

	cluster := clientcmdapi.NewCluster()
	cluster.Server = tok.Server
	cluster.CertificateAuthorityData = tok.CertificateAuthorityData
	cfg.Clusters[tok.ContextName] = cluster

	auth := clientcmdapi.NewAuthInfo()
	if len(tok.ClientCertificateData) > 0 {
		auth.ClientCertificateData = tok.ClientCertificateData
		auth.ClientKeyData = tok.ClientKeyData
	} else {
		auth.Token = tok.BearerToken
	}
	cfg.AuthInfos[tok.ContextName] = auth

	kctx := clientcmdapi.NewContext()
	kctx.Cluster = tok.ContextName
	kctx.AuthInfo = tok.ContextName
	kctx.Namespace = tok.Namespace
	cfg.Contexts[tok.ContextName] = kctx
	cfg.CurrentContext = tok.ContextName

	return cfg
}

// recordAdminKubeconfig merges the token's admin cluster/user/context into the kubeconfig the CLI
// resolves (the explicit --kubeconfig, else $KUBECONFIG's primary file, else ~/.kube/config) and
// sets it as the current context, so both the privileged path (commonOpts.client(), which reads the
// ambient current context) and plain `kubectl` reach the new cluster without further setup — matching
// how a cloud provider's `kubeconfig save` lands a new cluster. It is idempotent: re-running
// overwrites the same-named entries rather than duplicating them.
func recordAdminKubeconfig(kubeconfigPath string, tok jointoken.Token) error {
	po := clientcmd.NewDefaultPathOptions()
	if kubeconfigPath != "" {
		po.LoadingRules.ExplicitPath = kubeconfigPath
	}
	existing, err := po.GetStartingConfig()
	if err != nil {
		return fmt.Errorf("loading kubeconfig to record admin access: %w", err)
	}
	admin := adminAPIConfig(tok)
	existing.Clusters[tok.ContextName] = admin.Clusters[tok.ContextName]
	existing.AuthInfos[tok.ContextName] = admin.AuthInfos[tok.ContextName]
	existing.Contexts[tok.ContextName] = admin.Contexts[tok.ContextName]
	existing.CurrentContext = tok.ContextName
	if err := clientcmd.ModifyConfig(po, *existing, true); err != nil {
		return fmt.Errorf("recording admin access into the kubeconfig: %w", err)
	}
	return nil
}

// printJoinSummary reports the joined environment: which env/context was registered, that both admin
// (governance) and the scoped agent credential are set up locally, and the next step.
func printJoinSummary(stdout io.Writer, name, kubeContext string) {
	fmt.Fprintf(stdout, "\nJoined the bootstrapped cluster. Everything now runs from here.\n")
	fmt.Fprintf(stdout, "Environment %q (context %q) is now your current environment.\n", name, kubeContext)
	fmt.Fprintln(stdout, "Set up locally:")
	fmt.Fprintln(stdout, "  - admin access for governance (burrow guard, burrow upgrade)")
	fmt.Fprintln(stdout, "  - the scoped agent credential for day-to-day operations (ADR-0038)")
	fmt.Fprintf(stdout, "Rename it any time:  burrow env rename %s <new-name>\n\n", name)
	fmt.Fprint(stdout, postInstallGuidance)
}
