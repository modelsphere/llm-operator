// Package helm installs LLMService releases through the Helm SDK.
// Release storage is the Secret driver, so helm list keeps working.
package helm

import (
	"context"
	"errors"
	"fmt"
	"time"

	"helm.sh/helm/v4/pkg/action"
	chartv2 "helm.sh/helm/v4/pkg/chart/v2"
	"helm.sh/helm/v4/pkg/kube"
	"helm.sh/helm/v4/pkg/release"
	releasev1 "helm.sh/helm/v4/pkg/release/v1"
	"helm.sh/helm/v4/pkg/storage/driver"
	"k8s.io/client-go/rest"
)

const applyTimeout = 5 * time.Minute

// Helm is the release operations the reconciler uses.
type Helm interface {
	// Get returns the latest release. It returns nil, nil when the release is absent.
	Get(ctx context.Context, ns, releaseName string) (*Release, error)
	Install(ctx context.Context, ns, releaseName string, ch ChartRef, values map[string]any, opts ApplyOpts) (*Release, error)
	Upgrade(ctx context.Context, ns, releaseName string, ch ChartRef, values map[string]any, opts ApplyOpts) (*Release, error)
	Uninstall(ctx context.Context, ns, releaseName string) error
}

// Release is the part of a helm release the reconciler compares and records.
// Config is the user-supplied values only.
type Release struct {
	Revision     int
	Status       string
	ChartName    string
	ChartVersion string
	Config       map[string]any
}

// ChartRef selects one exact chart version in a repository.
type ChartRef struct {
	Repo    string
	Name    string
	Version string
}

// Credentials is a basic-auth pair from a credentialsRef Secret.
// The Secret's data keys are username and password.
type Credentials struct {
	Username string
	Password string
}

// ApplyOpts controls one install or upgrade.
type ApplyOpts struct {
	ForceConflicts bool
	Credentials    *Credentials
}

// ChartLoader supplies a chart without pulling it. Tests set it.
type ChartLoader func(ctx context.Context, ref ChartRef, creds *Credentials) (*chartv2.Chart, error)

// Client is the Helm SDK implementation of Helm.
type Client struct {
	restConfig *rest.Config
	cacheDir   string
	load       ChartLoader
	newConfig  func(namespace string) (*action.Configuration, error)
}

// New builds a client that stores releases as Secrets and caches charts under cacheDir.
// An empty cacheDir uses os.TempDir()/llm-operator-charts.
func New(cfg *rest.Config, cacheDir string) *Client {
	if cacheDir == "" {
		cacheDir = defaultCacheDir()
	}
	return &Client{restConfig: cfg, cacheDir: cacheDir}
}

func (c *Client) config(namespace string) (*action.Configuration, error) {
	if c.newConfig != nil {
		return c.newConfig(namespace)
	}
	cfg := action.NewConfiguration()
	if err := cfg.Init(newRESTGetter(c.restConfig, namespace), namespace, "secret"); err != nil {
		return nil, fmt.Errorf("helm configuration: %w", err)
	}
	return cfg, nil
}

func (c *Client) Get(ctx context.Context, ns, releaseName string) (*Release, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	cfg, err := c.config(ns)
	if err != nil {
		return nil, err
	}
	rel, err := action.NewGet(cfg).Run(releaseName)
	if err != nil {
		if errors.Is(err, driver.ErrReleaseNotFound) {
			return nil, nil
		}
		return nil, err
	}
	return toRelease(rel)
}

func (c *Client) Install(ctx context.Context, ns, releaseName string, ch ChartRef, values map[string]any, opts ApplyOpts) (*Release, error) {
	cfg, err := c.config(ns)
	if err != nil {
		return nil, err
	}
	chart, err := c.openChart(ctx, ch, opts.Credentials)
	if err != nil {
		return nil, err
	}
	inst := action.NewInstall(cfg)
	inst.ReleaseName = releaseName
	inst.Namespace = ns
	inst.ServerSideApply = true
	inst.ForceConflicts = opts.ForceConflicts
	inst.WaitStrategy = kube.HookOnlyStrategy
	inst.Timeout = applyTimeout
	inst.CreateNamespace = false
	rel, err := inst.RunWithContext(ctx, chart, values)
	if err != nil {
		return nil, err
	}
	return toRelease(rel)
}

func (c *Client) Upgrade(ctx context.Context, ns, releaseName string, ch ChartRef, values map[string]any, opts ApplyOpts) (*Release, error) {
	cfg, err := c.config(ns)
	if err != nil {
		return nil, err
	}
	chart, err := c.openChart(ctx, ch, opts.Credentials)
	if err != nil {
		return nil, err
	}
	up := action.NewUpgrade(cfg)
	up.Namespace = ns
	up.ServerSideApply = "true"
	up.ForceConflicts = opts.ForceConflicts
	up.WaitStrategy = kube.HookOnlyStrategy
	up.Timeout = applyTimeout
	rel, err := up.RunWithContext(ctx, releaseName, chart, values)
	if err != nil {
		return nil, err
	}
	return toRelease(rel)
}

func (c *Client) Uninstall(ctx context.Context, ns, releaseName string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	cfg, err := c.config(ns)
	if err != nil {
		return err
	}
	un := action.NewUninstall(cfg)
	un.IgnoreNotFound = true
	un.WaitStrategy = kube.HookOnlyStrategy
	un.Timeout = applyTimeout
	_, err = un.Run(releaseName)
	if err != nil && errors.Is(err, driver.ErrReleaseNotFound) {
		return nil
	}
	return err
}

func toRelease(rel release.Releaser) (*Release, error) {
	v1rel, ok := rel.(*releasev1.Release)
	if !ok || v1rel == nil {
		return nil, fmt.Errorf("unexpected helm release type %T", rel)
	}
	out := &Release{
		Revision: v1rel.Version,
		Config:   v1rel.Config,
	}
	if v1rel.Info != nil {
		out.Status = string(v1rel.Info.Status)
	}
	if v1rel.Chart != nil && v1rel.Chart.Metadata != nil {
		out.ChartName = v1rel.Chart.Metadata.Name
		out.ChartVersion = v1rel.Chart.Metadata.Version
	}
	return out, nil
}
