package serving

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"

	servingv1alpha1 "github.com/modelsphere/llm-operator/api/serving/v1alpha1"
)

// mergeLayers merges helm values in list order. Maps merge recursively, every
// other value replaces, and a null deletes the key.
func mergeLayers(layers []servingv1alpha1.Layer) (map[string]any, error) {
	dst := map[string]any{}
	for _, layer := range layers {
		if layer.Values == nil || len(layer.Values.Raw) == 0 || string(layer.Values.Raw) == "null" {
			continue
		}
		var src map[string]any
		if err := json.Unmarshal(layer.Values.Raw, &src); err != nil {
			return nil, fmt.Errorf("layer %q: %w", layer.Name, err)
		}
		if src == nil {
			continue
		}
		dst = mergeMaps(dst, src)
	}
	return dst, nil
}

func mergeMaps(dst, src map[string]any) map[string]any {
	if dst == nil {
		dst = map[string]any{}
	}
	for k, v := range src {
		if v == nil {
			delete(dst, k)
			continue
		}
		srcMap, srcIsMap := v.(map[string]any)
		dstMap, dstIsMap := dst[k].(map[string]any)
		if srcIsMap && dstIsMap {
			dst[k] = mergeMaps(dstMap, srcMap)
			continue
		}
		dst[k] = cloneValue(v)
	}
	return dst
}

func cloneValue(v any) any {
	switch t := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, e := range t {
			out[k] = cloneValue(e)
		}
		return out
	case []any:
		out := make([]any, len(t))
		for i := range t {
			out[i] = cloneValue(t[i])
		}
		return out
	default:
		return v
	}
}

// appliedHash is sha256 over the resolved chart and the merged values.
// encoding/json sorts map keys, so the digest does not depend on insertion order.
func appliedHash(chartName, chartVersion string, values map[string]any) (string, error) {
	if values == nil {
		values = map[string]any{}
	}
	payload := map[string]any{
		"chart": map[string]any{
			"name":    chartName,
			"version": chartVersion,
		},
		"values": values,
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(raw)
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}

// jsonEqual compares values after a JSON round trip, treating nil and an empty
// map as equal. Helm omits an empty user-values map.
func jsonEqual(a, b any) (bool, error) {
	ab, err := canonicalJSON(a)
	if err != nil {
		return false, err
	}
	bb, err := canonicalJSON(b)
	if err != nil {
		return false, err
	}
	return string(ab) == string(bb), nil
}

func canonicalJSON(v any) ([]byte, error) {
	raw, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	var out any
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, err
	}
	if out == nil {
		out = map[string]any{}
	}
	return json.Marshal(out)
}
