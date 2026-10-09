package helm

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
	chartv2 "helm.sh/helm/v4/pkg/chart/v2"
	"helm.sh/helm/v4/pkg/chart/v2/loader"
	"helm.sh/helm/v4/pkg/registry"
)

func defaultCacheDir() string {
	return filepath.Join(os.TempDir(), "llm-operator-charts")
}

func (c *Client) openChart(ctx context.Context, ref ChartRef, creds *Credentials) (*chartv2.Chart, error) {
	if c.load != nil {
		return c.load(ctx, ref, creds)
	}
	path, err := c.ensureChart(ctx, ref, creds)
	if err != nil {
		return nil, err
	}
	ch, err := loader.Load(path)
	if err != nil {
		return nil, fmt.Errorf("load chart %s %s: %w", ref.Name, ref.Version, err)
	}
	return ch, nil
}

func (c *Client) ensureChart(ctx context.Context, ref ChartRef, creds *Credentials) (string, error) {
	dest := c.chartPath(ref)
	if st, err := os.Stat(dest); err == nil && st.Size() > 0 {
		return dest, nil
	}
	body, err := c.pull(ctx, ref, creds)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return "", err
	}
	tmp, err := os.CreateTemp(filepath.Dir(dest), "chart-*.tgz")
	if err != nil {
		return "", err
	}
	tmpName := tmp.Name()
	_, werr := tmp.Write(body)
	cerr := tmp.Close()
	if werr != nil || cerr != nil {
		_ = os.Remove(tmpName)
		if werr != nil {
			return "", werr
		}
		return "", cerr
	}
	if err := os.Rename(tmpName, dest); err != nil {
		_ = os.Remove(tmpName)
		return "", err
	}
	return dest, nil
}

func (c *Client) chartPath(ref ChartRef) string {
	return filepath.Join(c.cacheDir, sanitize(ref.Repo), sanitize(ref.Name), sanitize(ref.Version), "chart.tgz")
}

func sanitize(s string) string {
	return strings.NewReplacer("/", "_", ":", "_", "\\", "_", "?", "_").Replace(s)
}

func (c *Client) pull(ctx context.Context, ref ChartRef, creds *Credentials) ([]byte, error) {
	switch {
	case strings.HasPrefix(ref.Repo, "oci://"):
		return pullOCI(ref, creds)
	case strings.HasPrefix(ref.Repo, "https://"):
		return pullHTTPS(ctx, ref, creds)
	default:
		return nil, fmt.Errorf("chart repo %q must start with https:// or oci://", ref.Repo)
	}
}

func pullOCI(ref ChartRef, creds *Credentials) ([]byte, error) {
	opts := []registry.ClientOption{registry.ClientOptWriter(io.Discard)}
	if creds != nil && (creds.Username != "" || creds.Password != "") {
		opts = append(opts, registry.ClientOptBasicAuth(creds.Username, creds.Password))
	}
	rc, err := registry.NewClient(opts...)
	if err != nil {
		return nil, fmt.Errorf("oci registry client: %w", err)
	}
	base := strings.TrimSuffix(strings.TrimPrefix(ref.Repo, "oci://"), "/")
	tag := strings.ReplaceAll(ref.Version, "+", "_")
	pullRef := base + "/" + ref.Name + ":" + tag
	res, err := rc.Pull(pullRef, registry.PullOptIgnoreMissingProv(true))
	if err != nil {
		return nil, fmt.Errorf("pull %s: %w", pullRef, err)
	}
	if res == nil || res.Chart == nil || len(res.Chart.Data) == 0 {
		return nil, fmt.Errorf("pull %s: chart layer is empty", pullRef)
	}
	return res.Chart.Data, nil
}

func pullHTTPS(ctx context.Context, ref ChartRef, creds *Credentials) ([]byte, error) {
	indexURL := strings.TrimSuffix(ref.Repo, "/") + "/index.yaml"
	body, err := httpGet(ctx, indexURL, creds)
	if err != nil {
		return nil, fmt.Errorf("chart index %s: %w", indexURL, err)
	}
	var idx struct {
		Entries map[string][]struct {
			Version string   `yaml:"version"`
			URLs    []string `yaml:"urls"`
		} `yaml:"entries"`
	}
	if err := yaml.Unmarshal(body, &idx); err != nil {
		return nil, fmt.Errorf("chart index %s: %w", indexURL, err)
	}
	var raw string
	for _, e := range idx.Entries[ref.Name] {
		if e.Version == ref.Version && len(e.URLs) > 0 {
			raw = e.URLs[0]
			break
		}
	}
	if raw == "" {
		return nil, fmt.Errorf("chart index %s has no %s %s", indexURL, ref.Name, ref.Version)
	}
	base, err := url.Parse(strings.TrimSuffix(ref.Repo, "/") + "/")
	if err != nil {
		return nil, err
	}
	u, err := url.Parse(raw)
	if err != nil {
		return nil, err
	}
	chartURL := base.ResolveReference(u).String()
	archive, err := httpGet(ctx, chartURL, creds)
	if err != nil {
		return nil, fmt.Errorf("download %s: %w", chartURL, err)
	}
	return archive, nil
}

func httpGet(ctx context.Context, raw string, creds *Credentials) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, raw, nil)
	if err != nil {
		return nil, err
	}
	if creds != nil && (creds.Username != "" || creds.Password != "") {
		req.SetBasicAuth(creds.Username, creds.Password)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s: %s", raw, resp.Status)
	}
	return body, nil
}
