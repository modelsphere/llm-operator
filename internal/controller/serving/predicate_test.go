package serving

import (
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/event"

	servingv1alpha1 "github.com/modelsphere/llm-operator/api/serving/v1alpha1"
)

func TestLLMServicePredicate(t *testing.T) {
	p := llmServicePredicate{}
	base := &servingv1alpha1.LLMService{}
	base.Generation = 1
	base.Annotations = map[string]string{"a": "1"}

	same := base.DeepCopy()
	same.Status.Phase = servingv1alpha1.PhaseApplied
	if p.Update(event.UpdateEvent{ObjectOld: base, ObjectNew: same}) {
		t.Fatal("status-only update reconciled")
	}

	gen := base.DeepCopy()
	gen.Generation = 2
	if !p.Update(event.UpdateEvent{ObjectOld: base, ObjectNew: gen}) {
		t.Fatal("generation change ignored")
	}

	ann := base.DeepCopy()
	ann.Annotations = map[string]string{"a": "2"}
	if !p.Update(event.UpdateEvent{ObjectOld: base, ObjectNew: ann}) {
		t.Fatal("annotation change ignored")
	}

	now := metav1.Now()
	del := base.DeepCopy()
	del.DeletionTimestamp = &now
	if !p.Update(event.UpdateEvent{ObjectOld: base, ObjectNew: del}) {
		t.Fatal("deletion ignored")
	}
	if !p.Create(event.CreateEvent{Object: base}) || !p.Delete(event.DeleteEvent{Object: base}) {
		t.Fatal("create or delete ignored")
	}
}
