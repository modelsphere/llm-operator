package serving

import (
	"strings"
	"testing"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"

	servingv1alpha1 "github.com/modelsphere/llm-operator/api/serving/v1alpha1"
)

func raw(s string) *apiextensionsv1.JSON {
	return &apiextensionsv1.JSON{Raw: []byte(s)}
}

func TestMergeLayers(t *testing.T) {
	got, err := mergeLayers([]servingv1alpha1.Layer{
		{Name: "a", Values: raw(`{"x":1,"m":{"a":1,"b":2},"s":[1],"keep":true}`)},
		{Name: "b", Values: raw(`{"x":2,"m":{"b":null,"c":3},"s":[2,3],"n":null}`)},
		{Name: "empty"},
		{Name: "scalar", Values: raw(`{"m":"replaced"}`)},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got["x"] != float64(2) {
		t.Errorf("x = %#v", got["x"])
	}
	if _, ok := got["n"]; ok {
		t.Errorf("null key n was kept: %#v", got["n"])
	}
	if got["keep"] != true {
		t.Errorf("keep = %#v", got["keep"])
	}
	m, ok := got["m"].(string)
	if !ok || m != "replaced" {
		t.Errorf("map replaced by scalar = %#v", got["m"])
	}
	s, ok := got["s"].([]any)
	if !ok || len(s) != 2 || s[0] != float64(2) || s[1] != float64(3) {
		t.Errorf("slice = %#v", got["s"])
	}

	nested, err := mergeLayers([]servingv1alpha1.Layer{
		{Name: "a", Values: raw(`{"m":{"a":1,"b":{"c":1}}}`)},
		{Name: "b", Values: raw(`{"m":{"b":{"d":2}}}`)},
	})
	if err != nil {
		t.Fatal(err)
	}
	inner := nested["m"].(map[string]any)["b"].(map[string]any)
	if inner["c"] != float64(1) || inner["d"] != float64(2) {
		t.Errorf("nested merge = %#v", nested["m"])
	}
}

func TestAppliedHashIsStableAndSensitive(t *testing.T) {
	a, err := appliedHash("sglang", "0.8.6", map[string]any{"b": 1, "a": map[string]any{"d": 1, "c": 2}})
	if err != nil {
		t.Fatal(err)
	}
	b, err := appliedHash("sglang", "0.8.6", map[string]any{"a": map[string]any{"c": 2, "d": 1}, "b": 1})
	if err != nil {
		t.Fatal(err)
	}
	if a != b || !strings.HasPrefix(a, "sha256:") || len(a) != len("sha256:")+64 {
		t.Fatalf("hash = %s and %s", a, b)
	}
	if same, err := jsonEqual(nil, map[string]any{}); err != nil || !same {
		t.Fatalf("nil and empty map: %v %v", same, err)
	}
	changed, err := appliedHash("sglang", "0.8.7", map[string]any{"b": 1, "a": map[string]any{"d": 1, "c": 2}})
	if err != nil || changed == a {
		t.Fatalf("version change did not move the hash: %s", changed)
	}
	changed, err = appliedHash("sglang", "0.8.6", map[string]any{"b": 2})
	if err != nil || changed == a {
		t.Fatalf("values change did not move the hash: %s", changed)
	}
}
