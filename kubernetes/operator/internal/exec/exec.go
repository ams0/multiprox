// Package exec runs commands inside a running pod container over the
// Kubernetes API server's exec endpoint (SPDY).
//
// The operator uses this to drive Proxmox's own CLI tooling — pvecm for
// cluster formation and pveceph for Ceph — from outside the cluster. There is
// no REST API for these operations, so exec is the supported path.
package exec

import (
	"bytes"
	"context"
	"fmt"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/remotecommand"
)

// Request describes a single command execution inside a pod container.
type Request struct {
	RestConfig *rest.Config
	KubeClient kubernetes.Interface

	Namespace string
	Pod       string
	Container string
	Command   []string

	// Stdin is optionally piped to the command.
	Stdin string
}

// Run executes the command and returns its combined stdout+stderr.
//
// A non-zero exit status is returned as an error whose message includes the
// captured output, because Proxmox tooling writes its diagnostics to both
// streams and the reason for a failure is almost always in there.
func Run(ctx context.Context, req Request) (string, error) {
	if req.RestConfig == nil {
		return "", fmt.Errorf("exec: RestConfig is nil (operator not wired for pod exec)")
	}
	if req.KubeClient == nil {
		return "", fmt.Errorf("exec: KubeClient is nil (operator not wired for pod exec)")
	}
	if len(req.Command) == 0 {
		return "", fmt.Errorf("exec: empty command")
	}

	container := req.Container
	if container == "" {
		container = "pve"
	}

	execOpts := &corev1.PodExecOptions{
		Container: container,
		Command:   req.Command,
		Stdin:     req.Stdin != "",
		Stdout:    true,
		Stderr:    true,
		TTY:       false,
	}

	url := req.KubeClient.CoreV1().RESTClient().
		Post().
		Resource("pods").
		Namespace(req.Namespace).
		Name(req.Pod).
		SubResource("exec").
		VersionedParams(execOpts, scheme.ParameterCodec).
		URL()

	executor, err := remotecommand.NewSPDYExecutor(req.RestConfig, "POST", url)
	if err != nil {
		return "", fmt.Errorf("exec: create SPDY executor for %s/%s: %w",
			req.Namespace, req.Pod, err)
	}

	// Interleave stdout and stderr into one buffer so the output reads in the
	// order the script produced it.
	var combined bytes.Buffer
	streamOpts := remotecommand.StreamOptions{
		Stdout: &combined,
		Stderr: &combined,
		Tty:    false,
	}
	if req.Stdin != "" {
		streamOpts.Stdin = strings.NewReader(req.Stdin)
	}

	streamErr := executor.StreamWithContext(ctx, streamOpts)
	out := combined.String()

	if streamErr != nil {
		return out, fmt.Errorf("exec %v in %s/%s: %w — output: %s",
			req.Command, req.Namespace, req.Pod, streamErr, truncate(strings.TrimSpace(out), 600))
	}

	return out, nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
