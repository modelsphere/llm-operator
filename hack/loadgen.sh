#!/usr/bin/env bash
#
# loadgen.sh — drive sustained load at a vLLM endpoint with `hey` so the
# LLMScaler's QueueDepth metric actually rises and triggers autoscaling.
#
# Why `hey -z` (duration) instead of a curl loop: num_requests_waiting is an
# instant gauge sampled by Prometheus (~15s) and the operator (~10s). A one-shot
# burst drains between scrapes and is never observed. Holding concurrency for a
# duration keeps the queue non-empty across several scrape intervals.
#
# Config via env vars (all optional):
#   ADDR         host:port of the vLLM server        (default localhost:8000)
#   MODEL        served-model-name                    (default facebook/opt-125m)
#   CONCURRENCY  concurrent workers hey keeps busy    (default 300)
#   DURATION     how long to sustain load             (default 90s)
#   MAX_TOKENS   output tokens per request (<= max-model-len - prompt) (default 512)
#   PROMPT       prompt text                          (default "whats up")
#   TIMEOUT      per-request timeout seconds, 0 = none (default 0)
#
# Example:
#   CONCURRENCY=400 DURATION=2m ./hack/loadgen.sh
#
set -euo pipefail

ADDR="${ADDR:-localhost:8000}"
MODEL="${MODEL:-facebook/opt-125m}"
CONCURRENCY="${CONCURRENCY:-300}"
DURATION="${DURATION:-90s}"
MAX_TOKENS="${MAX_TOKENS:-512}"
PROMPT="${PROMPT:-whats up}"
TIMEOUT="${TIMEOUT:-0}"

# Repo-local bin (kubebuilder tools dir) for a no-sudo download fallback.
REPO_BIN="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)/bin"

# ensure_hey makes `hey` available on PATH, installing it if needed.
ensure_hey() {
  if command -v hey >/dev/null 2>&1; then
    return
  fi

  echo "hey not found — attempting to install it..." >&2

  if command -v brew >/dev/null 2>&1; then
    brew install hey
  elif command -v go >/dev/null 2>&1; then
    # Reliable on any OS/arch (incl. arm64), no sudo.
    GOBIN="$REPO_BIN" go install github.com/rakyll/hey@latest
    export PATH="$REPO_BIN:$PATH"
  else
    # Prebuilt binary fallback (amd64 only — the S3 releases have no arm64 build).
    local os arch
    os="$(uname -s | tr '[:upper:]' '[:lower:]')"
    arch="$(uname -m)"
    if [ "$arch" != "x86_64" ] && [ "$arch" != "amd64" ]; then
      echo "No prebuilt hey for arch '$arch'. Install Go or Homebrew, then re-run." >&2
      exit 1
    fi
    mkdir -p "$REPO_BIN"
    echo "Downloading hey_${os}_amd64 into $REPO_BIN ..." >&2
    curl -fsSL -o "$REPO_BIN/hey" \
      "https://hey-release.s3.us-east-2.amazonaws.com/hey_${os}_amd64"
    chmod +x "$REPO_BIN/hey"
    export PATH="$REPO_BIN:$PATH"
  fi

  if ! command -v hey >/dev/null 2>&1; then
    echo "Failed to install hey. See https://github.com/rakyll/hey" >&2
    exit 1
  fi
  echo "Installed hey: $(command -v hey)" >&2
}

ensure_hey

if [ "$MAX_TOKENS" -gt 1024 ]; then
  echo "WARNING: MAX_TOKENS=$MAX_TOKENS may exceed the server's --max-model-len" \
       "(1024 in the vllm-mock chart) and get rejected with HTTP 400." >&2
fi

BODY="$(printf '{"model":"%s","prompt":"%s","max_tokens":%s}' \
  "$MODEL" "$PROMPT" "$MAX_TOKENS")"
URL="http://${ADDR}/v1/completions"

cat >&2 <<EOF
Driving load:
  url          $URL
  model        $MODEL
  concurrency  $CONCURRENCY
  duration     $DURATION
  max_tokens   $MAX_TOKENS   (per-request timeout: ${TIMEOUT}s; 0 = none)

Watch the scaler react in another terminal:
  kubectl get llmscaler -w
  kubectl get deploy -l app -w
EOF

exec hey \
  -z "$DURATION" \
  -c "$CONCURRENCY" \
  -t "$TIMEOUT" \
  -m POST \
  -T "application/json" \
  -d "$BODY" \
  "$URL"
