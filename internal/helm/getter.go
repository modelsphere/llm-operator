package helm

import (
	"fmt"

	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/discovery/cached/memory"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/restmapper"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/tools/clientcmd/api"
)

// restGetter adapts a controller-runtime *rest.Config to the Helm SDK.
type restGetter struct {
	cfg       *rest.Config
	namespace string
}

func newRESTGetter(cfg *rest.Config, namespace string) *restGetter {
	return &restGetter{cfg: cfg, namespace: namespace}
}

func (g *restGetter) ToRESTConfig() (*rest.Config, error) {
	if g.cfg == nil {
		return nil, fmt.Errorf("rest config is nil")
	}
	return rest.CopyConfig(g.cfg), nil
}

func (g *restGetter) ToDiscoveryClient() (discovery.CachedDiscoveryInterface, error) {
	cfg, err := g.ToRESTConfig()
	if err != nil {
		return nil, err
	}
	dc, err := discovery.NewDiscoveryClientForConfig(cfg)
	if err != nil {
		return nil, err
	}
	return memory.NewMemCacheClient(dc), nil
}

func (g *restGetter) ToRESTMapper() (meta.RESTMapper, error) {
	dc, err := g.ToDiscoveryClient()
	if err != nil {
		return nil, err
	}
	return restmapper.NewDeferredDiscoveryRESTMapper(dc), nil
}

func (g *restGetter) ToRawKubeConfigLoader() clientcmd.ClientConfig {
	return clientcmd.NewDefaultClientConfig(api.Config{}, &clientcmd.ConfigOverrides{
		Context: api.Context{Namespace: g.namespace},
	})
}
