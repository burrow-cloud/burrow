// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

// Package localconfig models Burrow's client-side selector state: the human-edited
// ~/.burrow/config file that names environment handles and records which one a command
// targets (ADR-0036). A handle maps a user-chosen name to {context, control-plane
// namespace, app namespace}; the current selection is either a pinned handle or, by
// default, whatever kube context kubectl points at ("follow"). This is selector state like
// the kubeconfig, never agent configuration, so it lives client-side: both `burrow` (the
// CLI) and `burrow-mcp` consume this package, hence it is a shared top-level package rather
// than living under cmd/burrow.
//
// This package is foundation only (ADR-0036 slice 1): the config model plus the resolution
// that decides the active target. Command wiring (`burrow env`, install, MCP) lands in
// later slices.
package localconfig

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"
)

const (
	// APIVersion is the schema version stamped into the config header so the format can be
	// migrated safely across Burrow versions (ADR-0036).
	APIVersion = "burrow.dev/v1"
	// Kind identifies the document, mirroring the Kubernetes-style header on ~/.kube/config.
	Kind = "Config"
	// DefaultControlPlaneNamespace is where burrowd runs unless a handle says otherwise. The
	// dimension is carried from day one so a future multi-burrowd-per-cluster setup is a
	// non-default value, not a breaking change (ADR-0036).
	DefaultControlPlaneNamespace = "burrow"
	// envConfigPath overrides the config location, mirroring how $KUBECONFIG overrides
	// ~/.kube/config.
	envConfigPath = "BURROW_CONFIG"
)

// Config is the on-disk selector state. APIVersion/Kind form the migratable header; Current
// names the pinned handle, or is empty to follow the current kube context (the default);
// Environments is the set of named handles.
type Config struct {
	APIVersion   string        `yaml:"apiVersion"`
	Kind         string        `yaml:"kind"`
	Current      string        `yaml:"current,omitempty"`
	Environments []Environment `yaml:"environments,omitempty"`
}

// Environment is a user-named handle resolving to a kube context and the namespaces the
// environment lives in (ADR-0036). ControlPlaneNamespace defaults to DefaultControlPlaneNamespace
// when empty; AppNamespace empty means callers fall back to the burrowd default app namespace.
//
// Env is the burrowd-registered environment NAME a command sends with each operation, which
// burrowd maps to the operation's namespace and per-environment guardrails. Empty means the
// cluster's default app namespace and the global guardrails (the cluster-per-environment case,
// where the whole cluster is the environment); a namespace-per-environment handle carries the
// same name it was registered with via `burrow env add`. It is deliberately distinct from
// AppNamespace, which is for display only: burrowd resolves a registered NAME, not a raw namespace.
type Environment struct {
	Name                  string `yaml:"name"`
	Context               string `yaml:"context"`
	ControlPlaneNamespace string `yaml:"controlPlaneNamespace,omitempty"`
	AppNamespace          string `yaml:"appNamespace,omitempty"`
	Env                   string `yaml:"env,omitempty"`
	// AgentKubeconfig is the path to the self-contained, burrowd-only kubeconfig `burrow install`
	// mints for the scoped agent credential (ADR-0038), written under ~/.burrow/ (never
	// ~/.kube/config). AgentContext names the single context inside it. Both are empty for handles
	// created before the scoped credential existed or joined out of band; consumers fall back to
	// the ambient kubeconfig then. No consumer reads them yet — that wiring is ADR-0038 phase 2.
	AgentKubeconfig string `yaml:"agentKubeconfig,omitempty"`
	AgentContext    string `yaml:"agentContext,omitempty"`
}

// controlPlaneNamespaceOrDefault returns the handle's control-plane namespace, or the
// default when it sets none.
func (e Environment) controlPlaneNamespaceOrDefault() string {
	if e.ControlPlaneNamespace == "" {
		return DefaultControlPlaneNamespace
	}
	return e.ControlPlaneNamespace
}

// Path resolves the config location: $BURROW_CONFIG when set, otherwise ~/.burrow/config
// (mirroring how the kubeconfig resolves $KUBECONFIG else ~/.kube/config).
func Path() (string, error) {
	if p := os.Getenv(envConfigPath); p != "" {
		return p, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("localconfig: resolving home directory: %w", err)
	}
	return filepath.Join(home, ".burrow", "config"), nil
}

// Load reads and parses the config from Path. A missing file is not an error: it returns a
// zero/empty Config (first run). Use Exists to detect the first-run case. An empty file is
// tolerated as first-run; a present apiVersion/kind is validated so future migrations are safe.
func Load() (*Config, error) {
	path, err := Path()
	if err != nil {
		return nil, err
	}
	return loadFrom(path)
}

func loadFrom(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return &Config{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("localconfig: reading %s: %w", path, err)
	}
	cfg := &Config{}
	if len(bytes.TrimSpace(data)) == 0 {
		return cfg, nil
	}
	if err := yaml.Unmarshal(data, cfg); err != nil {
		return nil, fmt.Errorf("localconfig: parsing %s: %w", path, err)
	}
	if err := cfg.validateHeader(); err != nil {
		return nil, fmt.Errorf("localconfig: %s: %w", path, err)
	}
	return cfg, nil
}

// validateHeader rejects an unrecognized apiVersion/kind when present, so a config written
// by a future incompatible version is not silently misread. An absent header is tolerated
// (first-run / hand-started file).
func (c *Config) validateHeader() error {
	if c.APIVersion != "" && c.APIVersion != APIVersion {
		return fmt.Errorf("unsupported apiVersion %q (want %q)", c.APIVersion, APIVersion)
	}
	if c.Kind != "" && c.Kind != Kind {
		return fmt.Errorf("unsupported kind %q (want %q)", c.Kind, Kind)
	}
	return nil
}

// Save writes the config to Path, creating ~/.burrow (0700) as needed and writing the file
// 0600. The apiVersion/kind header is always stamped.
func (c *Config) Save() error {
	path, err := Path()
	if err != nil {
		return err
	}
	return c.saveTo(path)
}

func (c *Config) saveTo(path string) error {
	c.APIVersion = APIVersion
	c.Kind = Kind
	data, err := yaml.Marshal(c)
	if err != nil {
		return fmt.Errorf("localconfig: encoding config: %w", err)
	}
	if dir := filepath.Dir(path); dir != "" {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return fmt.Errorf("localconfig: creating %s: %w", dir, err)
		}
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		return fmt.Errorf("localconfig: writing %s: %w", path, err)
	}
	return nil
}

// Exists reports whether the config file is present, for first-run detection.
func Exists() (bool, error) {
	path, err := Path()
	if err != nil {
		return false, err
	}
	return existsAt(path)
}

func existsAt(path string) (bool, error) {
	_, err := os.Stat(path)
	if err == nil {
		return true, nil
	}
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	return false, fmt.Errorf("localconfig: stat %s: %w", path, err)
}

// Lookup returns the handle with the given name, and whether it was found.
func (c *Config) Lookup(name string) (Environment, bool) {
	for _, e := range c.Environments {
		if e.Name == name {
			return e, true
		}
	}
	return Environment{}, false
}

// LookupByContext returns the handle registered for a kube context name, and whether one
// matched. It backs follow-mode resolution, where the current context is matched to a handle,
// and the MCP server's per-context resolution of the scoped agent kubeconfig (ADR-0038).
func (c *Config) LookupByContext(context string) (Environment, bool) {
	for _, e := range c.Environments {
		if e.Context == context {
			return e, true
		}
	}
	return Environment{}, false
}

// Add registers a new handle. The name must be non-empty and not already in use.
func (c *Config) Add(env Environment) error {
	if env.Name == "" {
		return errors.New("localconfig: environment name is required")
	}
	if _, ok := c.Lookup(env.Name); ok {
		return fmt.Errorf("localconfig: environment %q already exists", env.Name)
	}
	c.Environments = append(c.Environments, env)
	return nil
}

// Remove deletes the named handle. Removing the pinned handle reverts the selection to
// follow mode. It errors if the name is not registered.
func (c *Config) Remove(name string) error {
	for i, e := range c.Environments {
		if e.Name == name {
			c.Environments = append(c.Environments[:i], c.Environments[i+1:]...)
			if c.Current == name {
				c.Current = ""
			}
			return nil
		}
	}
	return fmt.Errorf("localconfig: environment %q is not in the config", name)
}

// Rename changes a handle's name, carrying the pin if the renamed handle was pinned. The new
// name must be non-empty and unused; the old name must be registered.
func (c *Config) Rename(oldName, newName string) error {
	if newName == "" {
		return errors.New("localconfig: new environment name is required")
	}
	if _, ok := c.Lookup(newName); ok {
		return fmt.Errorf("localconfig: environment %q already exists", newName)
	}
	for i := range c.Environments {
		if c.Environments[i].Name == oldName {
			c.Environments[i].Name = newName
			if c.Current == oldName {
				c.Current = newName
			}
			return nil
		}
	}
	return fmt.Errorf("localconfig: environment %q is not in the config", oldName)
}
