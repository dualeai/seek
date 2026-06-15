#!/usr/bin/env bash
#:HELP_BEGIN
# bench-field.sh — Self-contained field benchmarks for `seek`.
#
# Runs cold-index, warm-search, and dirty-reindex (PR-scale mutations)
# across real Git repos (cobra, prometheus, k8s, optionally linux) and
# synthetic non-Git folders at 10k and 100k file scales.
#
# Emits a Markdown table on stdout. Cache is sandboxed via SEEK_CACHE_DIR
# so the script never touches the developer's real ~/Library/Caches/seek.
#
# Usage:
#   ./cicd/bench-field.sh                # default workdir + all repos
#   ./cicd/bench-field.sh -w /tmp/bw     # custom workdir
#   ./cicd/bench-field.sh --no-linux     # skip linux (huge clone)
#   ./cicd/bench-field.sh --keep         # keep workdir for re-runs
#
# Requires: git, seek, universal-ctags.
# Time budget: ~3-15 min depending on linux inclusion.
#:HELP_END

set -euo pipefail

WORKDIR="${TMPDIR:-/tmp}/seek-field-bench"
KEEP=0
INCLUDE_LINUX=1
WARM_SAMPLES=5

while [ $# -gt 0 ]; do
  case "$1" in
    -w|--workdir)
      [ $# -ge 2 ] || { echo "$1 requires a value" >&2; exit 2; }
      WORKDIR="$2"; shift 2 ;;
    --keep)        KEEP=1; shift ;;
    --no-linux)    INCLUDE_LINUX=0; shift ;;
    -h|--help)
      sed -n '/^#:HELP_BEGIN/,/^#:HELP_END/p' "$0" \
        | sed '1d;$d;s/^#[[:space:]]\{0,1\}//'
      exit 0 ;;
    *)
      echo "unknown flag: $1" >&2
      exit 2 ;;
  esac
done

case "$WORKDIR" in
  ""|/|/bin|/etc|/home|/root|/usr|/var|/tmp|/private|/Users|/Volumes|"$HOME"|"$HOME/")
    echo "refusing dangerous workdir: $WORKDIR" >&2; exit 2 ;;
esac

mkdir -p "$WORKDIR"
echo "workdir: $WORKDIR" >&2

# Sandbox the seek cache so cold-index iterations don't wipe the
# developer's real cache. Honoured by seek's seekUserCacheRoot.
export SEEK_CACHE_DIR="$WORKDIR/cache"
mkdir -p "$SEEK_CACHE_DIR"

cleanup() {
  local rc=$?
  if [ "$KEEP" -eq 0 ] && [ -d "$WORKDIR" ]; then
    rm -rf "$WORKDIR"
  fi
  exit "$rc"
}
trap cleanup EXIT INT TERM

need() { command -v "$1" >/dev/null 2>&1 || { echo "missing tool: $1" >&2; exit 2; }; }
need git
need seek
if ! command -v universal-ctags >/dev/null 2>&1; then
  if ! command -v ctags >/dev/null 2>&1 || ! ctags --version 2>/dev/null | grep -qi 'Universal Ctags'; then
    echo "missing tool: universal-ctags" >&2; exit 2
  fi
fi

# time_ns CMD ...: print elapsed nanoseconds. /usr/bin/time -p prints
# "real X.XX" on stderr. Capture stderr to a tmp file (subshell redirect
# folding doesn't carry command's stderr correctly with set -e).
# Tolerates non-zero exit (seek exits 1 on "no match" per POSIX grep).
time_ns() {
  local tmp real
  tmp=$(mktemp)
  /usr/bin/time -p "$@" >/dev/null 2>"$tmp" || true
  real=$(awk '$1=="real"{print $2}' "$tmp")
  rm -f "$tmp"
  awk -v r="${real:-0}" 'BEGIN{printf "%d\n", r * 1000000000}'
}

# fmt_dur NS: render nanoseconds as ms or s with one decimal.
fmt_dur() {
  awk -v ns="$1" 'BEGIN{
    ms = ns/1000000.0
    if (ms < 1000) printf "%dms\n", int(ms+0.5)
    else printf "%.1fs\n", ms/1000.0
  }'
}

count_files() {
  find "$1" \( -name .git -prune \) -o -type f -print | wc -l | tr -d ' '
}

# clear_cache wipes the sandboxed cache so cold-index runs honestly.
clear_cache() {
  rm -rf "$SEEK_CACHE_DIR" 2>/dev/null || true
  mkdir -p "$SEEK_CACHE_DIR"
}

clone_repo() {
  local url="$1" dst="$2"
  if [ ! -d "$dst/.git" ]; then
    echo "cloning $url" >&2
    git clone --depth=1 "$url" "$dst" >/dev/null 2>&1
  fi
}

# synth_folder DIR COUNT: generate COUNT Go-ish files under DIR.
# Files are ~250 bytes each with 5 functions per file so zoekt's
# shardMax (10 MiB content) yields multiple shards at the 100k scale —
# without this realism a single-shard fixture serializes the cold-index
# build into one goroutine and skews bench numbers vs real repos
# (see indexer.go shardMax TODO).
#
# Uses printf (builtin) instead of `cat <<EOF` (subprocess fork per
# file) — heredoc spawn cost is multi-minutes at 100k scale.
synth_folder() {
  local dir="$1" count="$2"
  [ -d "$dir" ] && [ "$(count_files "$dir")" -eq "$count" ] && return
  rm -rf "$dir"
  mkdir -p "$dir"
  local i d pkg
  for ((i = 0; i < count; i++)); do
    pkg="pkg$((i / 1000))"
    d="$dir/$pkg"
    [ -d "$d" ] || mkdir -p "$d"
    printf 'package %s\n\n// generated file %d\nfunc foo%d() string { return "foo_%d" }\nfunc bar%d() string { return "bar_%d" }\nfunc baz%d() string { return "baz_%d" }\nfunc qux%d() string { return "qux_%d" }\nfunc sum%d() string { return "sum_%d" }\n' \
      "$pkg" "$i" "$i" "$i" "$i" "$i" "$i" "$i" "$i" "$i" "$i" "$i" \
      > "$d/f$i.go"
  done
}

# mutate_pct DIR PCT: append a comment to PCT% of files under DIR.
mutate_pct() {
  local dir="$1" pct="$2" n
  n=$(( $(count_files "$dir") * pct / 100 ))
  [ "$n" -ge 1 ] || n=1
  find "$dir" \( -name .git -prune \) -o -type f -print \
    | awk 'BEGIN{srand()} {print rand(), $0}' \
    | sort -n | head -n "$n" | cut -d' ' -f2- \
    | xargs -I{} sh -c 'echo "// mut $$" >> "$1"' _ {}
}

bench_cold() {
  clear_cache
  time_ns seek 'package' "$1"
}

bench_warm() {
  local root="$1" n="$2" i mid
  mid=$(( (n + 1) / 2 ))
  { for ((i = 0; i < n; i++)); do time_ns seek 'package' "$root"; done; } \
    | sort -n | sed -n "${mid}p"
}

# bench_dirty_reindex ROOT PCT: warm baseline; mutate PCT%; time reindex.
# State carries across calls: dirty 1% then dirty 10% leaves the workload
# with 11% cumulative mutations. Seek's state hash detects only newly
# mutated files, so each measurement still reflects the per-PCT delta.
bench_dirty_reindex() {
  local root="$1" pct="$2"
  seek 'package' "$root" >/dev/null 2>&1 || true
  mutate_pct "$root" "$pct"
  time_ns seek 'package' "$root"
}

WORKLOADS=(
  "git|spf13/cobra|https://github.com/spf13/cobra"
  "git|prometheus/prometheus|https://github.com/prometheus/prometheus"
  "git|kubernetes/kubernetes|https://github.com/kubernetes/kubernetes"
)
[ "$INCLUDE_LINUX" -eq 1 ] && \
  WORKLOADS+=("git|torvalds/linux|https://github.com/torvalds/linux")
WORKLOADS+=(
  "folder|synthetic-10k|10000"
  "folder|synthetic-100k|100000"
)

results=()
for entry in "${WORKLOADS[@]}"; do
  IFS='|' read -r kind label src <<<"$entry"
  dir="$WORKDIR/${label//\//_}"
  case "$kind" in
    git)    clone_repo "$src" "$dir" ;;
    folder) echo "synth $label ($src files)" >&2; synth_folder "$dir" "$src" ;;
  esac

  files=$(count_files "$dir")
  echo "[$label] files=$files" >&2

  cold_ns=$(bench_cold "$dir")
  warm_ns=$(bench_warm "$dir" "$WARM_SAMPLES")
  d1_ns=$(bench_dirty_reindex "$dir" 1)
  d10_ns=$(bench_dirty_reindex "$dir" 10)

  results+=("$kind|$label|$files|$cold_ns|$warm_ns|$d1_ns|$d10_ns")
done

echo
echo "## Field benchmarks"
echo
echo "Machine: $(uname -sm) — $(date -u +%Y-%m-%d)"
echo "Seek: $(seek --version 2>/dev/null || echo unknown)"
echo
echo "| Kind | Workload | Files | Cold index | Warm search | Dirty 1% | Dirty 10% |"
echo "|------|----------|-------|------------|-------------|----------|-----------|"
for row in "${results[@]}"; do
  IFS='|' read -r kind label files cold warm d1 d10 <<<"$row"
  printf "| %s | %s | %s | %s | %s | %s | %s |\n" \
    "$kind" "$label" "$files" \
    "$(fmt_dur "$cold")" "$(fmt_dur "$warm")" \
    "$(fmt_dur "$d1")" "$(fmt_dur "$d10")"
done
echo
