package helm

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"testing"

	"helm.sh/helm/v4/pkg/action"
	chartcommon "helm.sh/helm/v4/pkg/chart/common"
	chartv2 "helm.sh/helm/v4/pkg/chart/v2"
	"helm.sh/helm/v4/pkg/chart/v2/loader"
	kubefake "helm.sh/helm/v4/pkg/kube/fake"
	"helm.sh/helm/v4/pkg/storage"
	"helm.sh/helm/v4/pkg/storage/driver"
)

func testClient(t *testing.T, chartDir string) *Client {
	t.Helper()
	mem := driver.NewMemory()
	cfg := action.NewConfiguration()
	cfg.Releases = storage.Init(mem)
	cfg.KubeClient = &kubefake.FailingKubeClient{
		PrintingKubeClient: kubefake.PrintingKubeClient{Out: io.Discard, LogOutput: io.Discard},
	}
	cfg.Capabilities = chartcommon.DefaultCapabilities
	return &Client{
		cacheDir: t.TempDir(),
		load: func(context.Context, ChartRef, *Credentials) (*chartv2.Chart, error) {
			return loader.Load(chartDir)
		},
		newConfig: func(ns string) (*action.Configuration, error) {
			mem.SetNamespace(ns)
			return cfg, nil
		},
	}
}

func writeTinyChart(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "templates"), 0o755); err != nil {
		t.Fatal(err)
	}
	chartYAML := "apiVersion: v2\nname: tiny\nversion: 0.1.0\n"
	if err := os.WriteFile(filepath.Join(dir, "Chart.yaml"), []byte(chartYAML), 0o644); err != nil {
		t.Fatal(err)
	}
	tpl := "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: {{ .Release.Name }}\ndata:\n  v: {{ .Values.v | quote }}\n"
	if err := os.WriteFile(filepath.Join(dir, "templates", "cm.yaml"), []byte(tpl), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestGetInstallUpgradeUninstall(t *testing.T) {
	const valueOne = "one"
	c := testClient(t, writeTinyChart(t))
	ctx := t.Context()
	const ns, name = "default", "tiny"
	ref := ChartRef{Repo: "oci://example.com/charts", Name: "tiny", Version: "0.1.0"}

	got, err := c.Get(ctx, ns, name)
	if err != nil || got != nil {
		t.Fatalf("absent get = %#v, %v", got, err)
	}

	installed, err := c.Install(ctx, ns, name, ref, map[string]any{"v": valueOne}, ApplyOpts{ForceConflicts: true})
	if err != nil {
		t.Fatalf("install: %v", err)
	}
	if installed.Revision != 1 || installed.Status != "deployed" || installed.ChartName != "tiny" || installed.ChartVersion != "0.1.0" {
		t.Fatalf("install release = %#v", installed)
	}
	if installed.Config["v"] != valueOne {
		t.Fatalf("install config = %#v", installed.Config)
	}

	got, err = c.Get(ctx, ns, name)
	if err != nil || got == nil || got.Revision != 1 || got.Config["v"] != valueOne {
		t.Fatalf("get after install = %#v, %v", got, err)
	}

	upgraded, err := c.Upgrade(ctx, ns, name, ref, map[string]any{"v": "two"}, ApplyOpts{})
	if err != nil {
		t.Fatalf("upgrade: %v", err)
	}
	if upgraded.Revision != 2 || upgraded.Config["v"] != "two" || upgraded.Status != "deployed" {
		t.Fatalf("upgrade release = %#v", upgraded)
	}

	if err := c.Uninstall(ctx, ns, name); err != nil {
		t.Fatalf("uninstall: %v", err)
	}
	got, err = c.Get(ctx, ns, name)
	if err != nil || got != nil {
		t.Fatalf("get after uninstall = %#v, %v", got, err)
	}
	if err := c.Uninstall(ctx, ns, name); err != nil {
		t.Fatalf("uninstall absent: %v", err)
	}
}
