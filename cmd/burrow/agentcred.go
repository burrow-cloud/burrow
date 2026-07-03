// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"

	"github.com/burrow-cloud/burrow/connect"
	"github.com/burrow-cloud/burrow/localconfig"
)

// defaultPrincipal is the caller-identity constant threaded through credential provisioning while
// all agents share one ServiceAccount (ADR-0038 §5). Per-user ServiceAccounts keyed on an SSO
// identity are a deliberate, additive future step; seeding the principal now keeps that later step
// additive rather than a rewrite.
const defaultPrincipal = "shared-agent"

// agentServiceAccountName is the name of the shared scoped ServiceAccount `burrow install` mints
// for the agent (ADR-0038). It is also the Role/RoleBinding name and is threaded into the install
// manifests as {{.AgentServiceAccount}}.
const agentServiceAccountName = "burrow-agent"

// agentTokenSecretName is the long-lived ServiceAccount-token Secret the install manifest creates
// for the agent ServiceAccount; the token controller populates its token and ca.crt.
const agentTokenSecretName = "burrow-agent-token"

// agentKubeContextName is the single context name inside the minted scoped kubeconfig. It is a
// stable constant so the recorded AgentContext and the kubeconfig agree.
const agentKubeContextName = "burrow-agent"

// agentServiceAccountFn is the provisioning seam (ADR-0038 §5): it returns the ServiceAccount to
// mint a credential for, given a principal. Today it is a constant (the shared agent); later it can
// mint a per-user ServiceAccount keyed on an SSO identity without changing the callers.
var agentServiceAccountFn = func(principal string) string { return agentServiceAccountName }

// agentTokenPollTimeout / agentTokenPollInterval bound how long mintAgentKubeconfig waits for the
// token controller to populate the ServiceAccount-token Secret. They are package vars so a test can
// shrink them without a real cluster.
var (
	agentTokenPollTimeout  = 30 * time.Second
	agentTokenPollInterval = 2 * time.Second
)

// mintAgentKubeconfig builds a self-contained, burrowd-only kubeconfig for the scoped agent
// credential (ADR-0038). It polls the ServiceAccount-token Secret until the token controller has
// populated its bearer token (and, when present, ca.crt), then assembles a single-cluster,
// single-user, single-context kubeconfig carrying the API server URL (from restCfg.Host), the
// cluster CA, and the token. The CA comes from the Secret's ca.crt, falling back to the REST
// config's CAData or CAFile. It never touches ~/.kube/config; the caller writes the bytes under
// ~/.burrow/ via writeAgentKubeconfig.
func mintAgentKubeconfig(ctx context.Context, cs kubernetes.Interface, restCfg *rest.Config, namespace, saTokenSecret string) ([]byte, error) {
	token, caFromSecret, err := pollAgentToken(ctx, cs, namespace, saTokenSecret)
	if err != nil {
		return nil, err
	}
	return assembleAgentKubeconfig(restCfg, namespace, token, caFromSecret)
}

// assembleAgentKubeconfig serializes a self-contained, single-cluster/user/context kubeconfig from a
// bearer token and the cluster coordinates. It is the shared build+serialize step for both the
// fresh-mint path (mintAgentKubeconfig, which freshly mints and polls the token) and the join path
// (joinAgentCredential, which reads an existing token) — the only difference between them is how the
// token is obtained. The CA comes from the token Secret's ca.crt when present, falling back to the
// REST config's CAData or CAFile so the kubeconfig never references a CA file the agent cannot read.
func assembleAgentKubeconfig(restCfg *rest.Config, namespace, token string, caFromSecret []byte) ([]byte, error) {
	ca := caFromSecret
	if len(ca) == 0 {
		var err error
		ca, err = restConfigCA(restCfg)
		if err != nil {
			return nil, err
		}
	}

	const (
		clusterName = "burrow"
		userName    = agentServiceAccountName
	)
	cfg := clientcmdapi.NewConfig()
	cluster := clientcmdapi.NewCluster()
	cluster.Server = restCfg.Host
	cluster.CertificateAuthorityData = ca
	cfg.Clusters[clusterName] = cluster

	authInfo := clientcmdapi.NewAuthInfo()
	authInfo.Token = token
	cfg.AuthInfos[userName] = authInfo

	kubeCtx := clientcmdapi.NewContext()
	kubeCtx.Cluster = clusterName
	kubeCtx.AuthInfo = userName
	kubeCtx.Namespace = namespace
	cfg.Contexts[agentKubeContextName] = kubeCtx
	cfg.CurrentContext = agentKubeContextName

	data, err := clientcmd.Write(*cfg)
	if err != nil {
		return nil, fmt.Errorf("serializing scoped agent kubeconfig: %w", err)
	}
	return data, nil
}

// errAgentCredentialAbsent marks the case where the scoped agent credential is simply not present on
// the cluster yet — a pre-Phase-1 install, or a token Secret the controller has not populated. It is
// a sentinel so callers that tolerate a missing credential (`burrow env scan`, the `upgrade`
// backfill) can distinguish it from an access-denied failure via errors.Is and record a handle
// without a scoped cred instead of failing.
var errAgentCredentialAbsent = errors.New("the scoped agent credential is not present on this cluster")

// joinAgentCredentialFn is the seam install, `env scan`, and `upgrade` use to read the existing
// scoped agent credential from a cluster and write the local kubeconfig for it (ADR-0038 §4). It is a
// package var so tests substitute it without a real REST config or token Secret; the real path is
// joinAgentCredentialForContext.
var joinAgentCredentialFn = joinAgentCredentialForContext

// joinAgentCredentialForContext builds the REST config and clientset for a kube context and joins the
// existing agent credential there (ADR-0038 §4). envName names the local kubeconfig file, mirroring
// the fresh-mint path so each environment keeps its own scoped kubeconfig under ~/.burrow/agents/.
func joinAgentCredentialForContext(ctx context.Context, kubeconfig, kubeContext, namespace, envName string) (agentKubeconfigPath, agentContext string, err error) {
	restCfg, err := connect.RESTConfig(kubeconfig, kubeContext)
	if err != nil {
		return "", "", err
	}
	cs, err := clientsetForContext(kubeconfig, kubeContext)
	if err != nil {
		return "", "", err
	}
	return joinAgentCredential(ctx, cs, restCfg, namespace, envName)
}

// joinAgentCredential reads the EXISTING scoped agent credential — the burrow-agent-token Secret's
// token and ca.crt — with the caller's own (admin or ambient) access, builds the self-contained
// kubeconfig via the shared assembleAgentKubeconfig, and writes it locally via writeAgentKubeconfig
// (ADR-0038 §4). It mints no cluster resources: a second user joining an already-installed cluster,
// and the upgrade/scan backfills, all read the one credential `install` already provisioned. envName
// names the local kubeconfig file. If the token Secret cannot be read it returns a clear, actionable
// error (see readAgentToken); a merely-absent credential wraps errAgentCredentialAbsent so tolerant
// callers can skip it.
func joinAgentCredential(ctx context.Context, cs kubernetes.Interface, restCfg *rest.Config, namespace, envName string) (agentKubeconfigPath, agentContext string, err error) {
	token, caFromSecret, err := readAgentToken(ctx, cs, namespace, agentTokenSecretName)
	if err != nil {
		return "", "", err
	}
	data, err := assembleAgentKubeconfig(restCfg, namespace, token, caFromSecret)
	if err != nil {
		return "", "", err
	}
	path, err := writeAgentKubeconfig(envName, data)
	if err != nil {
		return "", "", err
	}
	return path, agentKubeContextName, nil
}

// readAgentToken reads the existing agent-token Secret with a single Get (no polling — the token
// controller populated it at install time), returning the bearer token and ca.crt. It maps the two
// failure shapes to clear, actionable errors (ADR-0038 §4):
//   - RBAC-denied: the joining user lacks read access to the shared agent credential, so an operator
//     must grant `get` on the Secret or hand over the scoped kubeconfig out of band;
//   - not-found / not-yet-populated: the credential is not present (a pre-Phase-1 install, or the
//     token controller has not filled it), wrapped in errAgentCredentialAbsent so tolerant callers
//     can skip rather than fail.
func readAgentToken(ctx context.Context, cs kubernetes.Interface, namespace, secret string) (token string, caCrt []byte, err error) {
	s, getErr := cs.CoreV1().Secrets(namespace).Get(ctx, secret, metav1.GetOptions{})
	if getErr != nil {
		if apierrors.IsForbidden(getErr) {
			return "", nil, fmt.Errorf("cannot read the scoped agent credential (secret %s/%s): %w\n"+
				"joining an existing Burrow install needs read access to the burrow-agent token; ask an "+
				"operator to grant `get` on that Secret, or to hand you the scoped kubeconfig from "+
				"~/.burrow/agents/ out of band", namespace, secret, getErr)
		}
		if apierrors.IsNotFound(getErr) {
			return "", nil, fmt.Errorf("%w: secret %s/%s was not found; an operator can mint it by running "+
				"`burrow upgrade` on this cluster (ADR-0038)", errAgentCredentialAbsent, namespace, secret)
		}
		return "", nil, fmt.Errorf("reading the agent token secret %s/%s: %w", namespace, secret, getErr)
	}
	tok := s.Data["token"]
	if len(tok) == 0 {
		return "", nil, fmt.Errorf("%w: secret %s/%s carries no token yet (the token controller has not "+
			"populated it); re-run once it is ready", errAgentCredentialAbsent, namespace, secret)
	}
	return string(tok), s.Data["ca.crt"], nil
}

// pollAgentToken reads the ServiceAccount-token Secret until the token controller has populated its
// token, returning the bearer token and the ca.crt (empty when the Secret carries none). It fails
// with a clear error if the token is not populated within agentTokenPollTimeout.
func pollAgentToken(ctx context.Context, cs kubernetes.Interface, namespace, secret string) (token string, caCrt []byte, err error) {
	deadline := time.Now().Add(agentTokenPollTimeout)
	for {
		s, getErr := cs.CoreV1().Secrets(namespace).Get(ctx, secret, metav1.GetOptions{})
		if getErr == nil {
			if tok := s.Data["token"]; len(tok) > 0 {
				return string(tok), s.Data["ca.crt"], nil
			}
		}
		if time.Now().After(deadline) {
			if getErr != nil {
				return "", nil, fmt.Errorf("waiting for the agent token secret %s/%s: %w", namespace, secret, getErr)
			}
			return "", nil, fmt.Errorf("agent token secret %s/%s was not populated by the token controller within %s", namespace, secret, agentTokenPollTimeout)
		}
		time.Sleep(agentTokenPollInterval)
	}
}

// restConfigCA returns the cluster CA from a REST config: its inline CAData, or the contents of its
// CAFile. It errors when the config carries neither, since a scoped kubeconfig must be
// self-contained (it cannot reference a CA file the agent may not be able to read).
func restConfigCA(restCfg *rest.Config) ([]byte, error) {
	if len(restCfg.TLSClientConfig.CAData) > 0 {
		return restCfg.TLSClientConfig.CAData, nil
	}
	if restCfg.TLSClientConfig.CAFile != "" {
		data, err := os.ReadFile(restCfg.TLSClientConfig.CAFile)
		if err != nil {
			return nil, fmt.Errorf("reading cluster CA file %s: %w", restCfg.TLSClientConfig.CAFile, err)
		}
		return data, nil
	}
	return nil, fmt.Errorf("cannot mint a self-contained agent kubeconfig: the cluster config carries no CA (neither inline data nor a CA file)")
}

// agentDir is the directory the scoped agent kubeconfigs live in: a sibling "agents" directory next
// to the local config file, so $BURROW_CONFIG keeps them together (ADR-0038). It is never
// ~/.kube/config.
func agentDir() (string, error) {
	p, err := localconfig.Path()
	if err != nil {
		return "", err
	}
	return filepath.Join(filepath.Dir(p), "agents"), nil
}

// writeAgentKubeconfig writes the minted scoped kubeconfig to agentDir()/name at 0600 under a 0700
// directory and returns its path. It deliberately writes only under ~/.burrow/ (never
// ~/.kube/config), so the human's admin kubeconfig is untouched.
func writeAgentKubeconfig(name string, data []byte) (string, error) {
	dir, err := agentDir()
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("creating agent kubeconfig directory %s: %w", dir, err)
	}
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		return "", fmt.Errorf("writing agent kubeconfig %s: %w", path, err)
	}
	return path, nil
}
