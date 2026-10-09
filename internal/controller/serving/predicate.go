package serving

import (
	"maps"

	"sigs.k8s.io/controller-runtime/pkg/event"
)

// llmServicePredicate reconciles generation changes, annotation changes, and
// deletion. Status-only updates do not.
type llmServicePredicate struct{}

func (llmServicePredicate) Create(event.CreateEvent) bool { return true }

func (llmServicePredicate) Delete(event.DeleteEvent) bool { return true }

func (llmServicePredicate) Generic(event.GenericEvent) bool { return true }

func (llmServicePredicate) Update(e event.UpdateEvent) bool {
	if e.ObjectOld == nil || e.ObjectNew == nil {
		return true
	}
	if !e.ObjectOld.GetDeletionTimestamp().IsZero() || !e.ObjectNew.GetDeletionTimestamp().IsZero() {
		return true
	}
	if e.ObjectOld.GetGeneration() != e.ObjectNew.GetGeneration() {
		return true
	}
	return !maps.Equal(e.ObjectOld.GetAnnotations(), e.ObjectNew.GetAnnotations())
}
