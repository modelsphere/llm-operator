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
#   PROFILE      comma-separated "concurrency:duration" phases run back to back,
#                overriding CONCURRENCY/DURATION. Concurrency 0 sends nothing for
#                that span, which is how one run covers scale-up AND scale-down.
#   MAX_TOKENS   output tokens per request (<= max-model-len - prompt) (default 512)
#   PROMPT       prompt text                          (default "whats up")
#   TIMEOUT      per-request timeout seconds, 0 = none (default 0)
#
# Examples:
#   CONCURRENCY=400 DURATION=2m ./hack/loadgen.sh
#   PROFILE="400:2m,100:3m,0:15m" ./hack/loadgen.sh   # peak, ease off, then idle
#
set -euo pipefail

ADDR="${ADDR:-localhost:8000}"
MODEL="${MODEL:-facebook/opt-125m}"
CONCURRENCY="${CONCURRENCY:-300}"
DURATION="${DURATION:-90s}"
PROFILE="${PROFILE:-}"
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

  if command -v go >/dev/null 2>&1; then
    # Works on any arch, no sudo.
    GOBIN="$REPO_BIN" go install github.com/rakyll/hey@latest
    export PATH="$REPO_BIN:$PATH"
  else
    # Prebuilt binary fallback (amd64 only — the S3 releases have no arm64 build).
    local arch
    arch="$(uname -m)"
    if [ "$arch" != "x86_64" ] && [ "$arch" != "amd64" ]; then
      echo "No prebuilt hey for arch '$arch'. Install Go, then re-run." >&2
      exit 1
    fi
    mkdir -p "$REPO_BIN"
    echo "Downloading hey_linux_amd64 into $REPO_BIN ..." >&2
    curl -fsSL -o "$REPO_BIN/hey" \
      "https://hey-release.s3.us-east-2.amazonaws.com/hey_linux_amd64"
    chmod +x "$REPO_BIN/hey"
    export PATH="$REPO_BIN:$PATH"
  fi

  if ! command -v hey >/dev/null 2>&1; then
    echo "Failed to install hey. See https://github.com/rakyll/hey" >&2
    exit 1
  fi
  echo "Installed hey: $(command -v hey)" >&2
}

# to_seconds converts a Go-style duration ("90s", "2m", "1m30s", or a bare
# integer meaning seconds) to whole seconds. sleep(1) rejects the compound
# "1m30s" form that hey accepts, so idle phases are converted rather than passed
# through, keeping one duration grammar across both. Returns 1 on anything
# unparseable rather than guessing.
INT_RE='^[0-9]+$'
DUR_RE='^([0-9]+)(h|m|s)(.*)$'
to_seconds() {
  local d="$1" total=0 rest num unit
  if [[ $d =~ $INT_RE ]]; then
    printf '%s' "$d"
    return 0
  fi
  rest="$d"
  while [ -n "$rest" ]; do
    if [[ $rest =~ $DUR_RE ]]; then
      num="${BASH_REMATCH[1]}"
      unit="${BASH_REMATCH[2]}"
      rest="${BASH_REMATCH[3]}"
      case "$unit" in
        h) total=$((total + num * 3600)) ;;
        m) total=$((total + num * 60)) ;;
        s) total=$((total + num)) ;;
      esac
    else
      return 1
    fi
  done
  printf '%s' "$total"
}

# A bare CONCURRENCY/DURATION run is just a one-phase profile.
if [ -z "$PROFILE" ]; then
  PROFILE="${CONCURRENCY}:${DURATION}"
fi

phase_conc=()
phase_dur=()
phase_secs=()
total_secs=0
needs_hey=0

IFS=',' read -r -a profile_entries <<< "$PROFILE"
for entry in "${profile_entries[@]}"; do
  entry="$(printf '%s' "$entry" | tr -d '[:space:]')"
  [ -n "$entry" ] || continue

  conc="${entry%%:*}"
  dur="${entry#*:}"
  if [ "$conc" = "$entry" ] || [ -z "$conc" ] || [ -z "$dur" ]; then
    echo "PROFILE: '$entry' is not concurrency:duration (e.g. 300:90s)" >&2
    exit 1
  fi
  if ! [[ $conc =~ $INT_RE ]]; then
    echo "PROFILE: concurrency '$conc' in '$entry' is not a non-negative integer" >&2
    exit 1
  fi
  if ! secs="$(to_seconds "$dur")"; then
    echo "PROFILE: duration '$dur' in '$entry' is not a duration (e.g. 90s, 2m, 1m30s)" >&2
    exit 1
  fi
  if [ "$secs" -eq 0 ]; then
    echo "PROFILE: duration '$dur' in '$entry' is zero" >&2
    exit 1
  fi

  phase_conc+=("$conc")
  phase_dur+=("$dur")
  phase_secs+=("$secs")
  total_secs=$((total_secs + secs))
  [ "$conc" -gt 0 ] && needs_hey=1
done

if [ "${#phase_conc[@]}" -eq 0 ]; then
  echo "PROFILE is empty" >&2
  exit 1
fi

# An all-idle profile drives no traffic, so don't go install a load generator.
if [ "$needs_hey" -eq 1 ]; then
  ensure_hey
fi

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
  max_tokens   $MAX_TOKENS   (per-request timeout: ${TIMEOUT}s; 0 = none)
  phases       ${#phase_conc[@]}, ${total_secs}s total
EOF

offset=0
for ((i = 0; i < ${#phase_conc[@]}; i++)); do
  if [ "${phase_conc[$i]}" -eq 0 ]; then
    label="idle"
  else
    label="c=${phase_conc[$i]}"
  fi
  printf '    %d) t+%-6s %-10s for %s\n' \
    "$((i + 1))" "${offset}s" "$label" "${phase_dur[$i]}" >&2
  offset=$((offset + phase_secs[i]))
done

cat >&2 <<EOF

Watch the scaler react in another terminal:
  kubectl get llmscaler -w
  kubectl get deploy -l app -w
EOF

trap 'echo "" >&2; echo "interrupted at $(date +%H:%M:%S)" >&2; exit 130' INT

status=0
started="$(date +%s)"
for ((i = 0; i < ${#phase_conc[@]}; i++)); do
  conc="${phase_conc[$i]}"
  dur="${phase_dur[$i]}"
  elapsed=$(($(date +%s) - started))

  if [ "$conc" -eq 0 ]; then
    printf '\n=== phase %d/%d  %s (t+%ds)  idle for %s ===\n' \
      "$((i + 1))" "${#phase_conc[@]}" "$(date +%H:%M:%S)" "$elapsed" "$dur" >&2
    sleep "${phase_secs[$i]}"
    continue
  fi

  printf '\n=== phase %d/%d  %s (t+%ds)  c=%s for %s ===\n' \
    "$((i + 1))" "${#phase_conc[@]}" "$(date +%H:%M:%S)" "$elapsed" "$conc" "$dur" >&2

  # Keep going if a phase fails: a later idle phase is usually the point of the
  # run (it is what triggers scale-down), so don't abandon it over one bad phase.
  # `|| rc=$?` rather than `if ! hey`, which would capture the negation's status.
  rc=0
  hey \
    -z "$dur" \
    -c "$conc" \
    -t "$TIMEOUT" \
    -m POST \
    -T "application/json" \
    -d "$BODY" \
    "$URL" || rc=$?
  if [ "$rc" -ne 0 ]; then
    echo "phase $((i + 1)) (c=$conc, $dur): hey exited $rc — continuing" >&2
    status=$rc
  fi
done

printf '\ndone at %s (t+%ds)\n' "$(date +%H:%M:%S)" "$(($(date +%s) - started))" >&2
exit "$status"
