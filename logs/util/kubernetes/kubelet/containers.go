//go:build !no_logs

// Unless explicitly stated otherwise all files in this repository are licensed
// under the Apache License Version 2.0.
// This product includes software developed at Datadog (https://www.datadoghq.com/).
// Copyright 2016-present Datadog, Inc.

package kubelet

import (
	"context"
	"fmt"
	"log"
	"net"
	"time"

	"flashcat.cloud/categraf/logs/util/containers"
	"flashcat.cloud/categraf/logs/util/containers/providers"
)

// ListContainers lists all non-excluded running containers, and retrieves their performance metrics
func (ku *KubeUtil) ListContainers(ctx context.Context) ([]*containers.Container, error) {
	pods, err := ku.GetLocalPodList(ctx)
	if err != nil {
		return nil, fmt.Errorf("could not get pod list: %s", err)
	}

	err = providers.ContainerImpl().Prefetch()
	if err != nil {
		return nil, fmt.Errorf("could not fetch container metrics: %s", err)
	}

	var ctrList []*containers.Container

	for _, pod := range pods {
		for _, c := range pod.Status.GetAllContainers() {
			if ku.filter.IsExcluded(c.Name, c.Image, pod.Metadata.Namespace) {
				continue
			}
			container, err := parseContainerInPod(c, pod)
			if err != nil {
				log.Printf("Cannot parse container %s in pod %s: %s", c.ID, pod.Metadata.Name, err)
				continue
			}
			if container == nil {
				// Skip nil containers
				continue
			}
			if !providers.ContainerImpl().ContainerExists(container.ID) {
				log.Printf("No ContainerImplementation found for container %s in pod %s, skipping", container.ID, pod.Metadata.Name)
				continue
			}
			ctrList = append(ctrList, container)

			ku.getContainerDetails(container)
		}
	}

	return ctrList, err
}

// UpdateContainerMetrics updates cgroup / network performance metrics for
// a provided list of Container objects
func (ku *KubeUtil) UpdateContainerMetrics(ctrList []*containers.Container) error {
	err := providers.ContainerImpl().Prefetch()
	if err != nil {
		return fmt.Errorf("could not fetch container metrics: %s", err)
	}

	return nil
}

// getContainerMetrics calls a ContainerImplementation, caller should always call Prefetch() before
func (ku *KubeUtil) getContainerDetails(ctn *containers.Container) {
	var err error
	ctn.StartedAt, err = providers.ContainerImpl().GetContainerStartTime(ctn.ID)
	if err != nil {
		log.Printf("ContainerImplementation cannot get StartTime for container %s, err: %s", ctn.ID[:12], err)
		return
	}
}

// getContainerMetrics calls a ContainerImplementation, calling function should always call Prefetch() before

func parseContainerInPod(status ContainerStatus, pod *Pod) (*containers.Container, error) {
	entity, err := KubeContainerIDToTaggerEntityID(status.ID)
	if err != nil {
		return nil, fmt.Errorf("Skipping container %s from pod %s: %s", status.Name, pod.Metadata.Name, err)
	}
	c := &containers.Container{
		Type:     "kubelet",
		ID:       TrimRuntimeFromCID(status.ID),
		EntityID: entity,
		Name:     fmt.Sprintf("%s-%s", pod.Metadata.Name, status.Name),
		Image:    status.Image,
	}

	switch {
	case status.State.Waiting != nil:
		// We don't display waiting containers
		log.Printf("Skipping waiting container %s", c.ID)
		return nil, nil
	case status.State.Running != nil:
		c.State = containers.ContainerRunningState
		c.Created = status.State.Running.StartedAt.Unix()
		c.Health = parseContainerReadiness(status, pod)
		c.AddressList = parseContainerNetworkAddresses(status, pod)
	case status.State.Terminated != nil:
		if status.State.Terminated.ExitCode == 0 {
			c.State = containers.ContainerExitedState
		} else {
			c.State = containers.ContainerDeadState
		}
		c.Created = status.State.Terminated.StartedAt.Unix()
	default:
		return nil, fmt.Errorf("container %s is in an unknown state, skipping", c.ID)
	}

	return c, nil
}

func parseContainerNetworkAddresses(status ContainerStatus, pod *Pod) []containers.NetworkAddress {
	addrList := []containers.NetworkAddress{}
	podIP := net.ParseIP(pod.Status.PodIP)
	if podIP == nil {
		log.Printf("Unable to parse pod IP: %v for pod: %s", pod.Status.PodIP, pod.Metadata.Name)
		return addrList
	}
	hostIP := net.ParseIP(pod.Status.HostIP)
	if hostIP == nil {
		log.Printf("Unable to parse host IP: %v for pod: %s", pod.Status.HostIP, pod.Metadata.Name)
		return addrList
	}
	// Look for the ports in container spec
	for _, s := range pod.Spec.Containers {
		if s.Name == status.Name {
			for _, port := range s.Ports {
				if port.HostPort > 0 {
					addrList = append(addrList, containers.NetworkAddress{
						IP:       hostIP,
						Port:     port.HostPort,
						Protocol: port.Protocol,
					})
				}
				if port.ContainerPort > 0 && !pod.Spec.HostNetwork {
					addrList = append(addrList, containers.NetworkAddress{
						IP:       podIP,
						Port:     port.ContainerPort,
						Protocol: port.Protocol,
					})
				}
			}
			break
		}
	}

	return addrList
}

func parseContainerReadiness(status ContainerStatus, pod *Pod) string {
	// Quick return if container is ready
	if status.Ready {
		return containers.ContainerHealthy
	}

	// Look for readinessProbe in container spec
	var probe *ContainerProbe
	for _, s := range pod.Spec.Containers {
		if s.Name == status.Name {
			probe = s.ReadinessProbe
			break
		}
	}
	if probe == nil {
		return containers.ContainerUnknownHealth
	}

	// Look for container start time
	if status.State.Running == nil {
		return containers.ContainerUnknownHealth
	}
	startTime := status.State.Running.StartedAt

	// Compute grace time before which the container is starting
	probeGraceTime := startTime.Add(time.Duration(probe.InitialDelaySeconds) * time.Second)

	if time.Now().Before(probeGraceTime) {
		return containers.ContainerStartingHealth
	}
	return containers.ContainerUnhealthy
}
