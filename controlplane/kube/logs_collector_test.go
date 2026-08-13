// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package kube_test

import (
	"context"
	"regexp"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	cp "github.com/burrow-cloud/burrow/controlplane"
	"github.com/burrow-cloud/burrow/controlplane/kube"
)

// podField is the record field the logs collector writes the pod name into. It is the same literal
// controlplane/logs.VictoriaLogs reads it back out of, and the two are a contract: the query path
// returned a blank pod on every line for as long as nothing wrote this field (issue #586).
const podField = "kubernetes_pod_name"

// deployLogsCollectorForTest installs the logs add-on into a fake cluster and returns the
// collector's ConfigMap and DaemonSet.
func deployLogsCollectorForTest(t *testing.T) (*corev1.ConfigMap, *corev1.PodSpec) {
	t.Helper()
	ctx := context.Background()
	client := fake.NewSimpleClientset()
	const addonNS = "burrow-addons"
	a := kube.New(client, ns).WithAddonNamespace(addonNS)

	spec := cp.AddonSpec{Type: cp.AddonLogs, Backend: "victorialogs", Image: "victoria-logs:test", Port: 9428, StorageGi: 5, Capabilities: []string{"logs"}}
	if _, err := a.DeployAddon(ctx, spec, cp.DefaultEnvironment, testInstanceOf(spec, cp.DefaultEnvironment), nil); err != nil {
		t.Fatalf("DeployAddon: %v", err)
	}
	cm, err := client.CoreV1().ConfigMaps(addonNS).Get(ctx, "burrow-logs-collector", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("collector config: %v", err)
	}
	ds, err := client.AppsV1().DaemonSets(addonNS).Get(ctx, "burrow-logs-collector", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("collector daemonset: %v", err)
	}
	return cm, &ds.Spec.Template.Spec
}

// TestLogsCollectorWritesThePodOnEveryRecord asserts the shipped collector config derives the pod
// from the container log's path and emits it under the field the VictoriaLogs adapter reads, so
// each returned line names the replica that emitted it.
func TestLogsCollectorWritesThePodOnEveryRecord(t *testing.T) {
	cm, _ := deployLogsCollectorForTest(t)

	conf := cm.Data["fluent-bit.conf"]
	for _, want := range []string{
		"Parsers_File burrow-parsers.conf", // the parser definition is loaded
		"Key_Name     filename",            // parsed out of the tail input's path key
		"Parser       burrow_container_filename",
		"Preserve_Key On", // ...which keeps filename itself, the stream field
		"Reserve_Data On", // ...and the log line beside the new field
	} {
		if !strings.Contains(conf, want) {
			t.Errorf("fluent-bit.conf missing %q:\n%s", want, conf)
		}
	}

	// The parser is what actually names the pod: run its regex over the path the kubelet writes
	// (<pod>_<namespace>_<container>-<container-id>.log). Fluent Bit parses with Onigmo and this
	// runs under Go's engine, which agrees with it on the subset used here — a literal, a negated
	// character class, and a named group.
	re := parserRegex(t, cm.Data["burrow-parsers.conf"])
	if got := namedMatch(re, "/var/log/containers/web-7d9c8b6f5-2xk4t_burrow-apps_web-1f2e3d.log", podField); got != "web-7d9c8b6f5-2xk4t" {
		t.Errorf("pod from a container log path = %q, want web-7d9c8b6f5-2xk4t", got)
	}
	// A path in some other shape does not match, which leaves Fluent Bit forwarding the record
	// unparsed: an unfamiliar layout costs a blank pod, never a dropped line.
	if re.MatchString("/var/log/containers/not-a-kubelet-path.log") {
		t.Error("a path that is not <pod>_<namespace>_<container>.log should not parse")
	}
}

// TestLogsCollectorKeepsThePodOutOfTheStreamFields pins the cost of the field above. A store's
// expense is per-stream cardinality, so the pod is sent as an ordinary record field: the filename
// that already keys the stream contains the pod name, so promoting it would add a label without
// adding a distinction.
func TestLogsCollectorKeepsThePodOutOfTheStreamFields(t *testing.T) {
	cm, _ := deployLogsCollectorForTest(t)
	conf := cm.Data["fluent-bit.conf"]
	if !strings.Contains(conf, "_stream_fields=filename&") {
		t.Errorf("the stream is keyed by filename alone:\n%s", conf)
	}
	if strings.Contains(conf, "_stream_fields=filename,") || strings.Contains(conf, "_stream_fields="+podField) {
		t.Errorf("the pod must not become a stream field — that is cardinality:\n%s", conf)
	}
}

// TestLogsCollectorMountsItsParsers asserts the parser file the config names is actually mounted
// beside the config, since [SERVICE] Parsers_File resolves relative to it.
func TestLogsCollectorMountsItsParsers(t *testing.T) {
	_, podSpec := deployLogsCollectorForTest(t)
	var mount *corev1.VolumeMount
	for i, m := range podSpec.Containers[0].VolumeMounts {
		if m.MountPath == "/fluent-bit/etc/burrow-parsers.conf" {
			mount = &podSpec.Containers[0].VolumeMounts[i]
		}
	}
	if mount == nil {
		t.Fatalf("no mount for the parsers file: %+v", podSpec.Containers[0].VolumeMounts)
	}
	if mount.SubPath != "burrow-parsers.conf" || mount.Name != "config" {
		t.Errorf("parsers mount = %+v, want subPath burrow-parsers.conf from the config volume", *mount)
	}
}

// parserRegex pulls the Regex line out of a Fluent Bit parsers file and compiles it.
func parserRegex(t *testing.T, parsers string) *regexp.Regexp {
	t.Helper()
	for _, line := range strings.Split(parsers, "\n") {
		fields := strings.Fields(line)
		if len(fields) == 2 && fields[0] == "Regex" {
			re, err := regexp.Compile(fields[1])
			if err != nil {
				t.Fatalf("compiling the parser regex %q: %v", fields[1], err)
			}
			return re
		}
	}
	t.Fatalf("no Regex line in the parsers file:\n%s", parsers)
	return nil
}

// namedMatch returns the named capture group's value for s, or "" when s does not match.
func namedMatch(re *regexp.Regexp, s, group string) string {
	m := re.FindStringSubmatch(s)
	if m == nil {
		return ""
	}
	for i, name := range re.SubexpNames() {
		if name == group {
			return m[i]
		}
	}
	return ""
}
