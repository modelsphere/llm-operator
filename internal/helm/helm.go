// Package helm installs LLMService releases through the Helm SDK.
// Release storage is the Secret driver, so helm list keeps working.
package helm

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"time"

	"helm.sh/helm/v4/pkg/action"
	chartv2 "helm.sh/helm/v4/pkg/chart/v2"
	"helm.sh/helm/v4/pkg/kube"
	"helm.sh/helm/v4/pkg/release"
	releasev1 "helm.sh/helm/v4/pkg/release/v1"
	"helm.sh/helm/v4/pkg/storage/driver"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/client-go/rest"

	"github.com/modelsphere/llm-operator/internal/common"
)

// ApplyTimeout is how long one helm install, upgrade, uninstall, or rollback may run.
const ApplyTimeout = 5 * time.Minute

// Helm is the release operations the reconciler uses.
type Helm interface {
	// Get returns the latest release. It returns nil, nil when the release is absent.
	Get(ctx context.Context, ns, releaseName string) (*Release, error)
	// History returns every revision of the release, newest first.
	// It returns nil, nil when the release is absent.
	History(ctx context.Context, ns, releaseName string) ([]Release, error)
	Install(ctx context.Context, ns, releaseName string, ch ChartRef, values map[string]any, opts ApplyOpts) (*Release, error)
	Upgrade(ctx context.Context, ns, releaseName string, ch ChartRef, values map[string]any, opts ApplyOpts) (*Release, error)
	// Rollback rolls the release back to revision. The new revision is deployed
	// with server-side apply and no resource wait.
	Rollback(ctx context.Context, ns, releaseName string, revision int) error
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
	// Labels are the labels stored on the release.
	Labels map[string]string
	// Updated is the release info last-deployed time.
	Updated time.Time
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
	// Labels are stored on the release by Install and Upgrade.
	Labels map[string]string
}

// ChartLoader supplies a chart without pulling it. Tests set it.
type ChartLoader func(ctx context.Context, ref ChartRef, creds *Credentials) (*chartv2.Chart, error)

// Client is the Helm SDK implementation of Helm.
type Client struct {
	restConfig *rest.Config
	cacheDir   string
	load       ChartLoader
	newConfig  func(namespace string) (*action.Configuration, error)
	// mapper tells namespaced kinds from cluster-scoped ones for the
	// namespace guard. Nil reads the cluster's discovery, once per apply.
	mapper meta.RESTMapper
}

// New builds a client that stores releases as Secrets and caches charts under cacheDir.
// An empty cacheDir uses os.TempDir()/llm-operator-charts.
func New(cfg *rest.Config, cacheDir string) *Client {
	if cacheDir == "" {
		cacheDir = defaultCacheDir()
	}
	// Process-wide in the Helm SDK; this package is its only caller.
	kube.ManagedFieldsManager = common.HelmFieldManager
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

func (c *Client) History(ctx context.Context, ns, releaseName string) ([]Release, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	cfg, err := c.config(ns)
	if err != nil {
		return nil, err
	}
	rels, err := action.NewHistory(cfg).Run(releaseName)
	if err != nil {
		if errors.Is(err, driver.ErrReleaseNotFound) {
			return nil, nil
		}
		return nil, err
	}
	out := make([]Release, 0, len(rels))
	for _, rel := range rels {
		got, err := toRelease(rel)
		if err != nil {
			return nil, err
		}
		out = append(out, *got)
	}
	slices.SortFunc(out, func(a, b Release) int { return cmp.Compare(b.Revision, a.Revision) })
	return out, nil
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
	guard, err := c.guard(cfg, ns)
	if err != nil {
		return nil, err
	}
	inst := action.NewInstall(cfg)
	inst.PostRenderer = guard
	inst.ReleaseName = releaseName
	inst.Namespace = ns
	inst.ServerSideApply = true
	inst.ForceConflicts = opts.ForceConflicts
	inst.WaitStrategy = kube.HookOnlyStrategy
	inst.Timeout = ApplyTimeout
	inst.Labels = maps.Clone(opts.Labels)
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
	guard, err := c.guard(cfg, ns)
	if err != nil {
		return nil, err
	}
	up := action.NewUpgrade(cfg)
	up.PostRenderer = guard
	up.Namespace = ns
	up.ServerSideApply = "true"
	up.ForceConflicts = opts.ForceConflicts
	up.WaitStrategy = kube.HookOnlyStrategy
	up.Timeout = ApplyTimeout
	up.Labels = maps.Clone(opts.Labels)
	rel, err := up.RunWithContext(ctx, releaseName, chart, values)
	if err != nil {
		return nil, err
	}
	return toRelease(rel)
}

// guard is the namespace guard for one apply into ns.
func (c *Client) guard(cfg *action.Configuration, ns string) (namespaceGuard, error) {
	if c.mapper != nil {
		return namespaceGuard{namespace: ns, mapper: c.mapper}, nil
	}
	if cfg.RESTClientGetter == nil {
		return namespaceGuard{}, fmt.Errorf("no REST mapper to check the scope of rendered objects")
	}
	mapper, err := cfg.RESTClientGetter.ToRESTMapper()
	if err != nil {
		return namespaceGuard{}, fmt.Errorf("REST mapper: %w", err)
	}
	return namespaceGuard{namespace: ns, mapper: mapper}, nil
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
	un.Timeout = ApplyTimeout
	_, err = un.Run(releaseName)
	if err != nil && errors.Is(err, driver.ErrReleaseNotFound) {
		return nil
	}
	return err
}

func (c *Client) Rollback(ctx context.Context, ns, releaseName string, revision int) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	cfg, err := c.config(ns)
	if err != nil {
		return err
	}
	rb := action.NewRollback(cfg)
	rb.Version = revision
	rb.ServerSideApply = "true"
	rb.WaitStrategy = kube.HookOnlyStrategy
	rb.Timeout = ApplyTimeout
	return rb.Run(releaseName)
}

func toRelease(rel release.Releaser) (*Release, error) {
	v1rel, ok := rel.(*releasev1.Release)
	if !ok || v1rel == nil {
		return nil, fmt.Errorf("unexpected helm release type %T", rel)
	}
	out := &Release{
		Revision: v1rel.Version,
		Config:   v1rel.Config,
		Labels:   maps.Clone(v1rel.Labels),
	}
	if v1rel.Info != nil {
		out.Status = string(v1rel.Info.Status)
		out.Updated = v1rel.Info.LastDeployed
	}
	if v1rel.Chart != nil && v1rel.Chart.Metadata != nil {
		out.ChartName = v1rel.Chart.Metadata.Name
		out.ChartVersion = v1rel.Chart.Metadata.Version
	}
	return out, nil
}
