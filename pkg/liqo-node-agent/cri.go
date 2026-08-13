// SPDX-License-Identifier: Apache-2.0
// Copyright 2019-2026 The Liqo Authors

package nodeagent

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	corev1 "k8s.io/api/core/v1"
	cri "k8s.io/cri-api/pkg/apis/runtime/v1"
)

// CRIResolver resolves the sandbox (pause) container PID for a pod using the
// Container Runtime Interface (CRI). It is required because Kubernetes does not
// expose pause container PIDs in the Pod API.
type CRIResolver struct {
	socketPath string
	client     cri.RuntimeServiceClient
	conn       *grpc.ClientConn
}

// NewCRIResolver creates a CRI resolver connected to the given Unix socket.
func NewCRIResolver(socketPath string) (*CRIResolver, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	conn, err := grpc.DialContext(ctx, "unix://"+socketPath,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithBlock(),
	)
	if err != nil {
		return nil, fmt.Errorf("connecting to CRI socket %s: %w", socketPath, err)
	}

	return &CRIResolver{
		socketPath: socketPath,
		client:     cri.NewRuntimeServiceClient(conn),
		conn:       conn,
	}, nil
}

// Close closes the underlying gRPC connection.
func (r *CRIResolver) Close() error {
	if r.conn != nil {
		return r.conn.Close()
	}
	return nil
}

// PodPID returns the sandbox PID for the given pod, or 0 if it cannot be resolved.
func (r *CRIResolver) PodPID(ctx context.Context, pod *corev1.Pod) (int, error) {
	// Look up the pod sandbox by namespace/name/UID. The container IDs in
	// pod.Status.ContainerStatuses refer to app containers, not the pause
	// sandbox, so they cannot be used directly with PodSandboxStatus.
	pods, err := r.client.ListPodSandbox(ctx, &cri.ListPodSandboxRequest{})
	if err != nil {
		return 0, fmt.Errorf("listing pod sandboxes for %s/%s: %w", pod.Namespace, pod.Name, err)
	}

	var sandboxID string
	for _, sb := range pods.Items {
		if sb.Metadata == nil {
			continue
		}
		if sb.Metadata.Namespace == pod.Namespace &&
			sb.Metadata.Name == pod.Name &&
			sb.Metadata.Uid == string(pod.UID) {
			sandboxID = sb.Id
			break
		}
	}
	if sandboxID == "" {
		return 0, nil
	}

	resp, err := r.client.PodSandboxStatus(ctx, &cri.PodSandboxStatusRequest{
		PodSandboxId: sandboxID,
		Verbose:      true,
	})
	if err != nil {
		return 0, fmt.Errorf("getting pod sandbox status for %s/%s: %w", pod.Namespace, pod.Name, err)
	}

	pid, err := pidFromSandboxInfo(resp.Info)
	if err != nil {
		return 0, fmt.Errorf("extracting pid for pod %s/%s: %w", pod.Namespace, pod.Name, err)
	}
	return pid, nil
}

// pidFromSandboxInfo extracts the sandbox PID from the CRI verbose info map.
// Containerd returns the info as a JSON string containing a top-level "pid" field.
func pidFromSandboxInfo(info map[string]string) (int, error) {
	if info == nil {
		return 0, nil
	}

	data, ok := info["info"]
	if !ok {
		return 0, nil
	}

	var sandbox struct {
		PID int `json:"pid"`
	}
	if err := json.Unmarshal([]byte(data), &sandbox); err != nil {
		return 0, fmt.Errorf("unmarshaling sandbox info: %w", err)
	}
	return sandbox.PID, nil
}
