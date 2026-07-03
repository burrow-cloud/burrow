// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"

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

	ca := caFromSecret
	if len(ca) == 0 {
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
