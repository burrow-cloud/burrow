// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package kube

import (
	"context"
	"fmt"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/burrow-cloud/burrow/controlplane"
)

// ControlPlaneDatabaseName is the name of both shapes of the control plane's own database in the
// control-plane namespace: the `Cluster` a CloudNativePG install creates, and the Deployment a
// plain one creates (ADR-0086 §2). They share it because the Service in front of them does, which
// is what lets one connection URL address either.
const ControlPlaneDatabaseName = "postgres"

// detectControlPlaneDatabase reports which shape the control plane's own database runs in, read
// live from the control-plane namespace (ADR-0086 §2).
//
// It looks for the `Cluster` FIRST and the Deployment second, so the answer is a positive
// identification of the object that is there rather than an inference from one that is not.
// Neither found is reported as an empty Kind: on a cluster this read cannot resolve, saying nothing
// is honest and claiming "plain" would be a guess about the thing this exists to stop people
// guessing about.
//
// Every way of not seeing the `Cluster` is one answer. A dynamic client that was never wired, a
// CustomResourceDefinition that is not served because the operator was never installed, and a
// FORBIDDEN from a burrowd whose Role predates this all mean the same thing here: this read cannot
// show a CloudNativePG database, so the Deployment is asked next. The capability is a report, and
// degrading it to an empty answer is better than failing the whole `burrow cluster` call over it.
func (p *Prober) detectControlPlaneDatabase(ctx context.Context) (controlplane.ControlPlaneDatabaseCapability, error) {
	if p.controlPlaneNamespace == "" {
		return controlplane.ControlPlaneDatabaseCapability{}, nil
	}

	if p.dynamic != nil {
		u, err := p.dynamic.Resource(cnpgClusterGVR).Namespace(p.controlPlaneNamespace).
			Get(ctx, ControlPlaneDatabaseName, metav1.GetOptions{})
		switch {
		case err == nil:
			return controlplane.ControlPlaneDatabaseCapability{
				Kind:     controlplane.ControlPlaneDatabaseCloudNativePG,
				Ready:    cnpgClusterReady(u),
				BackedUp: cnpgClusterArchives(u),
			}, nil
		case apierrors.IsNotFound(err), apierrors.IsForbidden(err):
			// Fall through to the plain Deployment.
		default:
			return controlplane.ControlPlaneDatabaseCapability{},
				fmt.Errorf("kube: reading the control plane's database: %w", err)
		}
	}

	d, err := p.client.AppsV1().Deployments(p.controlPlaneNamespace).Get(ctx, ControlPlaneDatabaseName, metav1.GetOptions{})
	switch {
	case err == nil:
		// A plain database cannot archive anywhere: it is one Deployment on one volume, with no
		// component behind it that takes a backup (ADR-0086 §2).
		return controlplane.ControlPlaneDatabaseCapability{
			Kind:     controlplane.ControlPlaneDatabasePlain,
			Ready:    d.Status.ReadyReplicas > 0,
			BackedUp: false,
		}, nil
	case apierrors.IsNotFound(err), apierrors.IsForbidden(err):
		return controlplane.ControlPlaneDatabaseCapability{}, nil
	default:
		return controlplane.ControlPlaneDatabaseCapability{},
			fmt.Errorf("kube: reading the control plane's database Deployment: %w", err)
	}
}
