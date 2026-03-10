package controller

import (
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	v1alpha1 "github.com/mumoshu/arc-detective/api/v1alpha1"
	"github.com/stretchr/testify/assert"
)

func TestBuildTimeline(t *testing.T) {
	t1 := metav1.NewTime(time.Date(2025, 1, 1, 10, 0, 0, 0, time.UTC))
	t2 := metav1.NewTime(time.Date(2025, 1, 1, 10, 0, 5, 0, time.UTC))
	t3 := metav1.NewTime(time.Date(2025, 1, 1, 10, 0, 10, 0, time.UTC))
	t4 := metav1.NewTime(time.Date(2025, 1, 1, 10, 0, 15, 0, time.UTC))
	t5 := metav1.NewTime(time.Date(2025, 1, 1, 10, 0, 20, 0, time.UTC))
	t6 := metav1.NewTime(time.Date(2025, 1, 1, 10, 0, 25, 0, time.UTC))

	ghEvents := []v1alpha1.TimelineEvent{
		{Timestamp: t1, Source: "github", Type: "job.queued", Message: "Job queued"},
		{Timestamp: t3, Source: "github", Type: "job.in_progress", Message: "Job started"},
		{Timestamp: t6, Source: "github", Type: "job.completed", Message: "Job failed"},
	}
	k8sEvents := []v1alpha1.TimelineEvent{
		{Timestamp: t2, Source: "pod", Type: "pod.scheduled", Message: "Pod scheduled on node-1"},
		{Timestamp: t4, Source: "pod", Type: "container.started", Message: "Container runner started"},
		{Timestamp: t5, Source: "pod", Type: "container.terminated", Message: "OOMKilled exit 137"},
	}

	timeline := BuildTimeline(ghEvents, k8sEvents)

	assert.Len(t, timeline, 6)
	assert.Equal(t, "job.queued", timeline[0].Type)
	assert.Equal(t, "pod.scheduled", timeline[1].Type)
	assert.Equal(t, "job.in_progress", timeline[2].Type)
	assert.Equal(t, "container.started", timeline[3].Type)
	assert.Equal(t, "container.terminated", timeline[4].Type)
	assert.Equal(t, "job.completed", timeline[5].Type)
}

func TestBuildTimelineEmpty(t *testing.T) {
	timeline := BuildTimeline(nil, nil)
	assert.Empty(t, timeline)
}

func TestBuildTimelineSingleSource(t *testing.T) {
	now := metav1.Now()
	events := []v1alpha1.TimelineEvent{
		{Timestamp: now, Source: "pod", Type: "test", Message: "msg"},
	}
	timeline := BuildTimeline(events)
	assert.Len(t, timeline, 1)
}
