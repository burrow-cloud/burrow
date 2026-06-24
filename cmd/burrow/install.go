// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"flag"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"text/template"

	"github.com/burrow-cloud/burrow/connect"
)

// defaultBurrowdImage is where the control-plane image is expected to live. Until it is
// published, `burrow install` needs --burrowd-image pointing at an image the cluster can
// pull (the e2e builds one and imports it into k3d).
const defaultBurrowdImage = "ghcr.io/burrow-cloud/burrowd:v0.1.0"

// installOptions are the values rendered into the install manifests.
type installOptions struct {
	Namespace  string
	Image      string
	Token      string
	DBPassword string
	Port       int
}

func cmdInstall(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("install", flag.ContinueOnError)
	fs.SetOutput(stderr)
	namespace := fs.String("namespace", connect.DefaultNamespace, "namespace to install Burrow into")
	image := fs.String("burrowd-image", defaultBurrowdImage, "burrowd container image to deploy (must be pullable by the cluster)")
	kubeconfig := fs.String("kubeconfig", "", "path to kubeconfig (default: ambient)")
	dryRun := fs.Bool("dry-run", false, "print the manifests instead of applying them")
	_, flagArgs := splitArgs(args)
	if err := fs.Parse(flagArgs); err != nil {
		return err
	}

	token, err := randHex(16)
	if err != nil {
		return err
	}
	dbPassword, err := randHex(12)
	if err != nil {
		return err
	}

	manifests, err := renderManifests(installOptions{
		Namespace:  *namespace,
		Image:      *image,
		Token:      token,
		DBPassword: dbPassword,
		Port:       connect.DefaultPort,
	})
	if err != nil {
		return err
	}

	if *dryRun {
		fmt.Fprint(stdout, manifests)
		return nil
	}

	if err := kubectlApply(ctx, *kubeconfig, manifests, stdout, stderr); err != nil {
		return err
	}
	fmt.Fprintf(stdout, "\nBurrow installed into namespace %q.\n"+
		"Once burrowd is ready, deploy with your kubeconfig — no further config:\n"+
		"  burrow deploy <app> --image <ref>\n", *namespace)
	return nil
}

// kubectlApply pipes the manifests to `kubectl apply -f -`.
func kubectlApply(ctx context.Context, kubeconfig, manifests string, stdout, stderr io.Writer) error {
	args := []string{"apply", "-f", "-"}
	if kubeconfig != "" {
		args = append([]string{"--kubeconfig", kubeconfig}, args...)
	}
	cmd := exec.CommandContext(ctx, "kubectl", args...)
	cmd.Stdin = strings.NewReader(manifests)
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("kubectl apply: %w", err)
	}
	return nil
}

func randHex(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generating random secret: %w", err)
	}
	return hex.EncodeToString(b), nil
}

func renderManifests(o installOptions) (string, error) {
	var sb strings.Builder
	if err := installTemplate.Execute(&sb, o); err != nil {
		return "", fmt.Errorf("rendering manifests: %w", err)
	}
	return sb.String(), nil
}

var installTemplate = template.Must(template.New("install").Parse(installManifests))

// installManifests bundles everything Burrow needs in the cluster: the namespace, a
// least-privilege ServiceAccount (manage Deployments, read Pods/logs — ADR), an
// in-cluster Postgres for control-plane state (ADR-0012), the secrets, and burrowd.
const installManifests = `apiVersion: v1
kind: Namespace
metadata:
  name: {{.Namespace}}
  labels: { app.kubernetes.io/managed-by: burrow }
---
apiVersion: v1
kind: ServiceAccount
metadata:
  name: burrowd
  namespace: {{.Namespace}}
---
apiVersion: rbac.authorization.k8s.io/v1
kind: Role
metadata:
  name: burrowd
  namespace: {{.Namespace}}
rules:
  - apiGroups: ["apps"]
    resources: ["deployments"]
    verbs: ["get", "list", "watch", "create", "update", "patch", "delete"]
  - apiGroups: [""]
    resources: ["pods"]
    verbs: ["get", "list"]
  - apiGroups: [""]
    resources: ["pods/log"]
    verbs: ["get"]
---
apiVersion: rbac.authorization.k8s.io/v1
kind: RoleBinding
metadata:
  name: burrowd
  namespace: {{.Namespace}}
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: Role
  name: burrowd
subjects:
  - kind: ServiceAccount
    name: burrowd
    namespace: {{.Namespace}}
---
apiVersion: v1
kind: Secret
metadata:
  name: burrowd-api-token
  namespace: {{.Namespace}}
type: Opaque
stringData:
  token: "{{.Token}}"
---
apiVersion: v1
kind: Secret
metadata:
  name: burrowd-db
  namespace: {{.Namespace}}
type: Opaque
stringData:
  password: "{{.DBPassword}}"
  url: "postgres://burrow:{{.DBPassword}}@postgres:5432/burrow?sslmode=disable"
---
apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: postgres-data
  namespace: {{.Namespace}}
spec:
  accessModes: ["ReadWriteOnce"]
  resources:
    requests:
      storage: 1Gi
---
apiVersion: apps/v1
kind: Deployment
metadata:
  name: postgres
  namespace: {{.Namespace}}
spec:
  replicas: 1
  selector: { matchLabels: { app: postgres } }
  strategy: { type: Recreate }
  template:
    metadata: { labels: { app: postgres } }
    spec:
      containers:
        - name: postgres
          image: postgres:18
          env:
            - { name: POSTGRES_USER, value: burrow }
            - { name: POSTGRES_DB, value: burrow }
            - name: POSTGRES_PASSWORD
              valueFrom: { secretKeyRef: { name: burrowd-db, key: password } }
            - { name: PGDATA, value: /var/lib/postgresql/data/pgdata }
          ports: [ { containerPort: 5432 } ]
          readinessProbe:
            exec: { command: ["pg_isready", "-U", "burrow", "-d", "burrow"] }
            initialDelaySeconds: 5
            periodSeconds: 5
          volumeMounts:
            - { name: data, mountPath: /var/lib/postgresql/data }
      volumes:
        - name: data
          persistentVolumeClaim: { claimName: postgres-data }
---
apiVersion: v1
kind: Service
metadata:
  name: postgres
  namespace: {{.Namespace}}
spec:
  selector: { app: postgres }
  ports: [ { port: 5432, targetPort: 5432 } ]
---
apiVersion: apps/v1
kind: Deployment
metadata:
  name: burrowd
  namespace: {{.Namespace}}
  labels: { app: burrowd }
spec:
  replicas: 1
  selector: { matchLabels: { app: burrowd } }
  template:
    metadata: { labels: { app: burrowd } }
    spec:
      serviceAccountName: burrowd
      containers:
        - name: burrowd
          image: {{.Image}}
          args: ["-listen=:{{.Port}}"]
          env:
            - { name: BURROW_NAMESPACE, value: {{.Namespace}} }
            - name: BURROW_API_TOKEN
              valueFrom: { secretKeyRef: { name: burrowd-api-token, key: token } }
            - name: BURROW_DATABASE_URL
              valueFrom: { secretKeyRef: { name: burrowd-db, key: url } }
          ports: [ { containerPort: {{.Port}} } ]
          readinessProbe:
            httpGet: { path: /healthz, port: {{.Port}} }
            initialDelaySeconds: 3
            periodSeconds: 5
---
apiVersion: v1
kind: Service
metadata:
  name: burrowd
  namespace: {{.Namespace}}
spec:
  selector: { app: burrowd }
  ports: [ { port: {{.Port}}, targetPort: {{.Port}} } ]
`
