package controller

import (
	"context"
	"time"

	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/manager"

	v1alpha1 "github.com/mumoshu/arc-detective/api/v1alpha1"
	"github.com/mumoshu/arc-detective/internal/logcollector"
)

const (
	defaultRetentionPeriod = 7 * 24 * time.Hour
	cleanupInterval        = 5 * time.Minute
)

// Cleanup periodically prunes old Investigation CRs and their logs.
type Cleanup struct {
	client.Client
	storage logcollector.Storage
}

func NewCleanup(c client.Client, storage logcollector.Storage) *Cleanup {
	return &Cleanup{Client: c, storage: storage}
}

func (c *Cleanup) Start(ctx context.Context) error {
	logger := log.FromContext(ctx).WithName("cleanup")
	ticker := time.NewTicker(cleanupInterval)
	defer ticker.Stop()

	logger.Info("Starting cleanup controller", "interval", cleanupInterval)

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			if err := c.runCleanup(ctx); err != nil {
				logger.Error(err, "Cleanup cycle failed")
			}
		}
	}
}

func (c *Cleanup) runCleanup(ctx context.Context) error {
	logger := log.FromContext(ctx).WithName("cleanup")

	retention := c.getRetentionPeriod(ctx)

	var invList v1alpha1.InvestigationList
	if err := c.List(ctx, &invList); err != nil {
		return err
	}

	now := time.Now()
	for i := range invList.Items {
		inv := &invList.Items[i]
		age := now.Sub(inv.CreationTimestamp.Time)
		if age <= retention {
			continue
		}

		logger.Info("Pruning old investigation", "name", inv.Name, "namespace", inv.Namespace, "age", age)

		// Delete logs
		for _, logPath := range inv.Spec.LogPaths {
			if err := c.storage.Delete(logPath); err != nil {
				logger.Error(err, "Failed to delete log", "path", logPath)
			}
		}

		// Delete the Investigation CR
		if err := c.Delete(ctx, inv); err != nil {
			logger.Error(err, "Failed to delete investigation", "name", inv.Name)
		}
	}

	// Check storage usage against maxSizeMB
	c.enforceStorageLimit(ctx, logger)

	return nil
}

func (c *Cleanup) getRetentionPeriod(ctx context.Context) time.Duration {
	var configs v1alpha1.DetectiveConfigList
	if err := c.List(ctx, &configs); err != nil || len(configs.Items) == 0 {
		return defaultRetentionPeriod
	}
	if configs.Items[0].Spec.RetentionPeriod != nil {
		return configs.Items[0].Spec.RetentionPeriod.Duration
	}
	return defaultRetentionPeriod
}

func (c *Cleanup) enforceStorageLimit(ctx context.Context, logger interface {
	Error(error, string, ...interface{})
	Info(string, ...interface{})
}) {
	if c.storage == nil {
		return
	}

	var configs v1alpha1.DetectiveConfigList
	if err := c.List(ctx, &configs); err != nil || len(configs.Items) == 0 {
		return
	}
	config := &configs.Items[0]
	if config.Spec.LogStorage.MaxSizeMB == nil || *config.Spec.LogStorage.MaxSizeMB <= 0 {
		return
	}

	maxBytes := int64(*config.Spec.LogStorage.MaxSizeMB) * 1024 * 1024
	usage, err := c.storage.UsageBytes()
	if err != nil {
		logger.Error(err, "Failed to check storage usage")
		return
	}

	if usage <= maxBytes {
		return
	}

	// Need to prune — storage backend handles oldest-first if it's DiskStorage
	diskStorage, ok := c.storage.(*logcollector.DiskStorage)
	if !ok {
		return
	}
	oldest, err := diskStorage.OldestFiles()
	if err != nil {
		logger.Error(err, "Failed to list oldest files")
		return
	}

	for _, path := range oldest {
		if usage <= maxBytes {
			break
		}
		if err := c.storage.Delete(path); err != nil {
			continue
		}
		// Re-check usage (approximate)
		usage, _ = c.storage.UsageBytes()
		logger.Info("Pruned log file to free space", "path", path)
	}
}

var _ manager.Runnable = &Cleanup{}
