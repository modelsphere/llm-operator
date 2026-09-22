#!/usr/bin/env bash
# Regenerate the parts of dist/chart that are derived from config/.
#
# Why this exists: `make manifests` runs controller-gen over the Go types and
# writes config/crd/bases and config/rbac/role.yaml -- but nothing propagates
# those into the chart. The chart was scaffolded once by
# `kubebuilder edit --plugins=helm/v2-alpha` and has been hand-carried since,
# so an API change updates config/ and leaves the chart's CRD quietly a version
# behind. Nothing failed when that happened; the stale copy just shipped.
#
#   sync            rewrite the derived files in place
#   sync --check    rewrite into a temp dir and diff; non-zero if they differ
#
# Only the derived files are touched. Everything else in dist/chart -- the
# manager Deployment, values, helpers -- is hand-maintained and left alone.
set -euo pipefail
MODE="${1:-}"
ROOT="$(cd "$(dirname "$0")/.." && pwd)"
OUT="$ROOT/dist/chart/templates"
if [ "$MODE" = "--check" ]; then
  OUT="$(mktemp -d)/templates"
  mkdir -p "$OUT/crd" "$OUT/rbac"
fi

python3 - "$ROOT" "$OUT" <<'PY'
import sys, os, io, glob
root, out = sys.argv[1], sys.argv[2]

# ── CRD ───────────────────────────────────────────────────────────────────────
# Wrapped in the chart's crd.enabled switch, with the resource-policy annotation
# inserted under metadata.annotations behind crd.keep.
for src in sorted(glob.glob(os.path.join(root, "config/crd/bases/*.yaml"))):
    body = io.open(src, encoding="utf-8").read().splitlines()
    if body and body[0].strip() == "---":
        body = body[1:]
    lines, done = [], False
    for l in body:
        lines.append(l)
        if not done and l.rstrip() == "  annotations:":
            lines += ['    {{- if .Values.crd.keep }}',
                      '    "helm.sh/resource-policy": keep',
                      '    {{- end }}']
            done = True
    if not done:
        sys.exit("FAIL %s: no 'annotations:' line to anchor the keep policy on" % src)
    # group_kind.yaml -> kind.group.yaml, the chart's naming
    base = os.path.basename(src)[:-len(".yaml")]
    group, kind = base.split("_", 1)
    dst = os.path.join(out, "crd", "%s.%s.yaml" % (kind, group))
    text = "{{- if .Values.crd.enabled }}\n" + "\n".join(lines) + "\n{{- end }}\n"
    io.open(dst, "w", encoding="utf-8").write(text)
    print("  crd   %s" % os.path.relpath(dst, root))

# ── manager role ──────────────────────────────────────────────────────────────
# The chart file's head carries the Role/ClusterRole switch and the name helper;
# everything from `rules:` down is controller-gen's output verbatim.
role_src = os.path.join(root, "config/rbac/role.yaml")
dst = os.path.join(out, "rbac", "manager-role.yaml")
cur = os.path.join(root, "dist/chart/templates/rbac/manager-role.yaml")
head = []
for l in io.open(cur, encoding="utf-8").read().splitlines():
    head.append(l)
    if l.rstrip() == "rules:":
        break
else:
    sys.exit("FAIL %s: no 'rules:' line; cannot tell head from generated body" % cur)
rules = io.open(role_src, encoding="utf-8").read().splitlines()
i = next((n for n, l in enumerate(rules) if l.rstrip() == "rules:"), None)
if i is None:
    sys.exit("FAIL %s: no 'rules:' line" % role_src)
io.open(dst, "w", encoding="utf-8").write("\n".join(head + rules[i+1:]) + "\n")
print("  rbac  %s" % os.path.relpath(dst, root))
PY

if [ "$MODE" = "--check" ]; then
  if diff -r "$ROOT/dist/chart/templates/crd" "$OUT/crd" >/dev/null 2>&1 \
     && diff "$ROOT/dist/chart/templates/rbac/manager-role.yaml" "$OUT/rbac/manager-role.yaml" >/dev/null 2>&1; then
    echo "chart is in sync with config/"
  else
    echo "chart is OUT OF SYNC with config/ -- run 'make chart-sync' and commit the result:"
    diff -r "$ROOT/dist/chart/templates/crd" "$OUT/crd" || true
    diff "$ROOT/dist/chart/templates/rbac/manager-role.yaml" "$OUT/rbac/manager-role.yaml" || true
    exit 1
  fi
fi
