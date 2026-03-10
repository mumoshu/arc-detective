package logcollector

import (
	"context"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/kubernetes"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

// Collector is the interface for collecting pod logs.
type Collector interface {
	CollectLogs(ctx context.Context, pod *corev1.Pod) ([]string, error)
}

// PodLogCollector collects logs from pod containers and writes them to storage.
type PodLogCollector struct {
	clientset kubernetes.Interface
	storage   Storage
}

func NewPodLogCollector(clientset kubernetes.Interface, storage Storage) *PodLogCollector {
	return &PodLogCollector{
		clientset: clientset,
		storage:   storage,
	}
}

func (c *PodLogCollector) CollectLogs(ctx context.Context, pod *corev1.Pod) ([]string, error) {
	logger := log.FromContext(ctx)
	var logPaths []string

	containers := allContainerNames(pod)
	timestamp := time.Now().UTC().Format("2006-01-02T15-04-05")

	for _, containerName := range containers {
		logPath := fmt.Sprintf("%s/%s/%s/%s.log", pod.Namespace, pod.Name, containerName, timestamp)

		req := c.clientset.CoreV1().Pods(pod.Namespace).GetLogs(pod.Name, &corev1.PodLogOptions{
			Container: containerName,
		})

		stream, err := req.Stream(ctx)
		if err != nil {
			logger.Error(err, "Failed to stream logs", "container", containerName)
			continue
		}

		if err := c.storage.Write(logPath, stream); err != nil {
			stream.Close()
			logger.Error(err, "Failed to write logs", "container", containerName)
			continue
		}
		stream.Close()

		logPaths = append(logPaths, logPath)
		logger.V(1).Info("Collected logs", "container", containerName, "path", logPath)
	}

	if len(logPaths) == 0 && len(containers) > 0 {
		return nil, fmt.Errorf("failed to collect any logs from %d containers", len(containers))
	}
	return logPaths, nil
}

func allContainerNames(pod *corev1.Pod) []string {
	var names []string
	for _, c := range pod.Spec.InitContainers {
		names = append(names, c.Name)
	}
	for _, c := range pod.Spec.Containers {
		names = append(names, c.Name)
	}
	return names
}
