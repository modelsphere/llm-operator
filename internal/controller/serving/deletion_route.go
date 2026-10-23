/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package serving

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	k8syaml "k8s.io/apimachinery/pkg/util/yaml"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const modelRouteKind = "ModelRoute"

// modelRouteGroups are the API groups whose ModelRoute objects are drained
// before a release is uninstalled.
var modelRouteGroups = map[string]struct{}{
	"routing.modelsphere.dev": {},
	"routing.gpucluster.io":   {},
}

// modelRouteRef is one ModelRoute rendered into a helm release manifest.
type modelRouteRef struct {
	APIVersion string
	Kind       string
	Namespace  string
	Name       string
}

// modelRoutesFromManifest returns ModelRoute objects in the routing groups.
// An empty namespace is filled from defaultNS. Other documents are ignored.
// A document that does not decode is an error.
func modelRoutesFromManifest(manifest, defaultNS string) ([]modelRouteRef, error) {
	reader := k8syaml.NewYAMLReader(bufio.NewReader(strings.NewReader(manifest)))
	var out []modelRouteRef
	for {
		doc, err := reader.Read()
		if err == io.EOF {
			return out, nil
		}
		if err != nil {
			return nil, fmt.Errorf("decode manifest: %w", err)
		}
		if len(bytes.TrimSpace(doc)) == 0 {
			continue
		}
		obj := map[string]any{}
		if err := k8syaml.Unmarshal(doc, &obj); err != nil {
			return nil, fmt.Errorf("decode manifest: %w", err)
		}
		if len(obj) == 0 {
			continue
		}
		ref, ok, err := routeRef(&unstructured.Unstructured{Object: obj}, defaultNS)
		if err != nil {
			return nil, err
		}
		if ok {
			out = append(out, ref)
		}
	}
}

func routeRef(u *unstructured.Unstructured, defaultNS string) (modelRouteRef, bool, error) {
	if u.GetKind() != modelRouteKind {
		return modelRouteRef{}, false, nil
	}
	group, _, _ := strings.Cut(u.GetAPIVersion(), "/")
	if _, ok := modelRouteGroups[group]; !ok {
		return modelRouteRef{}, false, nil
	}
	if u.GetName() == "" {
		return modelRouteRef{}, false, fmt.Errorf("modelroute is missing metadata.name")
	}
	ns := u.GetNamespace()
	if ns == "" {
		ns = defaultNS
	}
	return modelRouteRef{
		APIVersion: u.GetAPIVersion(),
		Kind:       u.GetKind(),
		Namespace:  ns,
		Name:       u.GetName(),
	}, true, nil
}

func routeObject(ref modelRouteRef) *unstructured.Unstructured {
	u := &unstructured.Unstructured{}
	u.SetAPIVersion(ref.APIVersion)
	u.SetKind(ref.Kind)
	u.SetNamespace(ref.Namespace)
	u.SetName(ref.Name)
	return u
}

// deleteModelRoutes deletes each route and leaves its finalizers in place.
// NotFound is success.
func (r *LLMServiceReconciler) deleteModelRoutes(ctx context.Context, routes []modelRouteRef) error {
	for _, ref := range routes {
		if err := r.Delete(ctx, routeObject(ref)); err != nil && !apierrors.IsNotFound(err) {
			return fmt.Errorf("delete modelroute %s/%s: %w", ref.Namespace, ref.Name, err)
		}
	}
	return nil
}

// modelRoutesGone reports whether every route is NotFound.
func (r *LLMServiceReconciler) modelRoutesGone(ctx context.Context, routes []modelRouteRef) (bool, error) {
	for _, ref := range routes {
		err := r.Get(ctx, client.ObjectKey{Namespace: ref.Namespace, Name: ref.Name}, routeObject(ref))
		if apierrors.IsNotFound(err) {
			continue
		}
		if err != nil {
			return false, fmt.Errorf("get modelroute %s/%s: %w", ref.Namespace, ref.Name, err)
		}
		return false, nil
	}
	return true, nil
}

func routeRemovingMessage(routes []modelRouteRef) string {
	if len(routes) == 1 {
		return fmt.Sprintf("waiting for ModelRoute %s to be removed", routes[0].Name)
	}
	return fmt.Sprintf("waiting for %d ModelRoutes to be removed", len(routes))
}
