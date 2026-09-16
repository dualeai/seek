#!/usr/bin/env -S uv run --script
# /// script
# requires-python = ">=3.10,<3.13"
# dependencies = []
# ///

"""Run and check the retained cold joined-index benchmark.

The benchmark always builds and validates a joined Zoekt and semantic index.
It retains its work directory and evidence. Report-only mode still runs all
samples and correctness checks; it disables performance gates and their minimum
sample count.
"""

from __future__ import annotations

import argparse
import hashlib
import math
import os
from pathlib import Path
import re
import shutil
import statistics
import subprocess
import sys
import tempfile
import time


PROOF_HEAD = "912ec3583d7733a240dad6a3755f5f2f6b76be3e"
PROOF_FILES = 31_250
PROOF_ROWS = 204_694
RUNS_HEADER = (
    "sample\treal_s\tuser_s\tsys_s\tmean_cpu_pct\tmedian_cpu_pct\t"
    "min_window_cpu_pct\tpeak_rss_kib\tswap_delta_kib\tindex_kib\tfiles\trows\t"
    "max_in_flight\tfinal_limit\tcall_limit\tinput_empty_polls\n"
)

BENCH_EPILOG = """environment:
  SEEK_BENCH_REPO                  required clean Git checkout
  SEEK_BIN                         report-only binary; gate mode builds from source
  SEEK_BENCH_HEAD                  expected commit
  SEEK_BENCH_MODEL_SAMPLES         fixed 100,000-row model sample count
  SEEK_BENCH_EXPECTED_FILES        expected indexed file count
  SEEK_BENCH_EXPECTED_ROWS         expected semantic row count
  SEEK_BENCH_QUERY                 search query
  SEEK_BENCH_CPUS                  expected effective Go CPU count
  SEEK_BENCH_MODEL_P50_SECONDS     model p50 limit
  SEEK_BENCH_P95_SECONDS           cold joined-index p95 limit
  SEEK_BENCH_MEAN_CPU_PERCENT      normalized mean CPU floor
  SEEK_BENCH_MEDIAN_CPU_PERCENT    normalized median CPU floor
  SEEK_BENCH_WINDOW_CPU_PERCENT    lowest five-second CPU floor

scheduler columns in results/runs.tsv:
  max_in_flight     highest number of concurrent model calls
  final_limit       adaptive model-call limit at completion
  call_limit        current CPU-derived model-call ceiling
  input_empty_polls non-blocking checks with no prepared batch; count, not time

The performance gate requires a clean Seek source tree. Its commit must match the
tested binary's Go build record because the model benchmark runs from source. The
script restores the native tokenizer library from its tracked archive before use.
It verifies the source, binary, and native asset identities again before it writes
the summary.
"""


def stop(message: str, status: int = 2) -> None:
    print(message, file=sys.stderr)
    raise SystemExit(status)


def positive_int(name: str, value: str | int) -> int:
    try:
        parsed = int(value)
    except (TypeError, ValueError):
        stop(f"{name} must be a positive integer")
    if parsed < 1:
        stop(f"{name} must be a positive integer")
    return parsed


def env_float(name: str, default: float) -> float:
    try:
        return float(os.environ.get(name, str(default)))
    except ValueError:
        stop(f"{name} must be a number")


def require_tool(name: str) -> str:
    path = shutil.which(name)
    if path is None:
        stop(f"missing tool: {name}")
    return path


def output(command: list[str], *, cwd: Path | None = None) -> str:
    result = subprocess.run(command, cwd=cwd, text=True, capture_output=True)
    if result.returncode != 0:
        detail = result.stderr.strip() or result.stdout.strip()
        stop(f"command failed: {' '.join(command)}{': ' + detail if detail else ''}")
    return result.stdout


def sha256_file(path: Path) -> str:
    digest = hashlib.sha256()
    try:
        with path.open("rb") as source:
            while block := source.read(1024 * 1024):
                digest.update(block)
    except OSError as error:
        stop(f"cannot hash file {path}: {error}", 1)
    return digest.hexdigest()


def source_identity(path: Path) -> tuple[str, str]:
    head = subprocess.run(
        ["git", "-C", str(path), "rev-parse", "HEAD"],
        text=True,
        capture_output=True,
    )
    if head.returncode != 0:
        return "unavailable", "unavailable\n"
    status = output(
        ["git", "-C", str(path), "status", "--porcelain=v1", "--untracked-files=all"]
    )
    return head.stdout.strip(), status


def binary_build_identity(path: Path) -> tuple[str, str]:
    result = subprocess.run(
        ["go", "version", "-m", str(path)],
        text=True,
        capture_output=True,
    )
    if result.returncode != 0:
        return "unavailable", "unavailable"
    settings: dict[str, str] = {}
    for line in result.stdout.splitlines():
        fields = line.strip().split("\t")
        if len(fields) != 2 or fields[0] != "build":
            continue
        key, separator, value = fields[1].lstrip("-").partition("=")
        if separator:
            settings[key] = value
    return settings.get("vcs.revision", "unavailable"), settings.get(
        "vcs.modified", "unavailable"
    )


def restore_tokenizer_library(repo_root: Path) -> tuple[Path, str, Path, str]:
    go_env = output(["go", "env", "GOOS", "GOARCH", "CGO_ENABLED"]).splitlines()
    if len(go_env) != 3 or go_env[2] != "1":
        stop("Seek benchmarks require CGO_ENABLED=1")
    target = f"{go_env[0]}_{go_env[1]}"
    asset_dir = repo_root / "cmd" / "seek" / f"rerank_tokenizer_assets_{target}"
    native_dir = repo_root / "cmd" / "seek" / f"rerank_tokenizer_native_{target}"
    archive = asset_dir / "libtokenizers.tar.gz"
    library = native_dir / "libtokenizers.a"
    if not archive.is_file():
        stop(f"tracked tokenizer archive is missing for {target}: {archive}")
    status = subprocess.run(
        ["make", "-B", "tokenizer-native"], cwd=repo_root
    ).returncode
    if status != 0 or not library.is_file():
        stop(f"native tokenizer restore failed for {target}", 1)
    return archive, sha256_file(archive), library, sha256_file(library)


def run_to_files(
    command: list[str],
    stdout_path: Path,
    stderr_path: Path,
    *,
    cwd: Path | None = None,
    env: dict[str, str] | None = None,
) -> int:
    with stdout_path.open("wb") as stdout_file, stderr_path.open("wb") as stderr_file:
        return subprocess.run(
            command,
            cwd=cwd,
            env=env,
            stdout=stdout_file,
            stderr=stderr_file,
        ).returncode


def validate_repo(repo: Path, expected_head: str) -> None:
    if not repo.is_dir():
        stop(f"benchmark repo is not a directory: {repo}")
    git_dir = subprocess.run(
        ["git", "-C", str(repo), "rev-parse", "--git-dir"],
        text=True,
        capture_output=True,
    )
    if git_dir.returncode != 0:
        stop(f"benchmark repo is not a Git checkout: {repo}")
    head = output(["git", "-C", str(repo), "rev-parse", "HEAD"]).strip()
    if head != expected_head:
        stop(f"benchmark HEAD is {head}, want {expected_head}")
    status = output(
        ["git", "-C", str(repo), "status", "--porcelain=v1", "--untracked-files=all"]
    )
    if status:
        stop("benchmark repo must be clean")


def prepare_workdir(value: str | None) -> Path:
    if value is None:
        path = Path(tempfile.mkdtemp(prefix="seek-semantic-proof."))
    else:
        path = Path(value).expanduser().resolve()
        if path.exists():
            if not path.is_dir() or any(path.iterdir()):
                stop(f"workdir must be an absent or empty directory: {path}")
        else:
            path.mkdir(parents=True)
    (path / ".seek-semantic-benchmark").write_text("owned\n")
    return path


def reset_corpus_cache(workdir: Path, cache_dir: Path) -> Path:
    if not (workdir / ".seek-semantic-benchmark").is_file():
        stop("benchmark workdir marker is missing", 1)
    corpora = cache_dir / "corpora"
    if cache_dir.parent.resolve() != workdir.resolve():
        stop("benchmark cache is outside its workdir", 1)
    shutil.rmtree(corpora, ignore_errors=True)
    corpora.mkdir(parents=True)
    return corpora


def read_meminfo() -> dict[str, int]:
    values: dict[str, int] = {}
    try:
        for line in Path("/proc/meminfo").read_text().splitlines():
            fields = line.split()
            if len(fields) >= 2 and fields[0].endswith(":"):
                values[fields[0][:-1]] = int(fields[1])
    except (OSError, ValueError):
        pass
    return values


def swap_used_kib() -> int:
    if sys.platform == "darwin":
        result = subprocess.run(
            ["/usr/sbin/sysctl", "-n", "vm.swapusage"], text=True, capture_output=True
        )
        match = re.search(r"used\s*=\s*([0-9.]+)M", result.stdout)
        return round(float(match.group(1)) * 1024) if match else 0
    if sys.platform.startswith("linux"):
        values = read_meminfo()
        return values.get("SwapTotal", 0) - values.get("SwapFree", 0)
    return 0


def gpu_percent() -> float:
    if sys.platform != "darwin" or not Path("/usr/sbin/ioreg").is_file():
        return math.nan
    result = subprocess.run(
        ["/usr/sbin/ioreg", "-r", "-d", "1", "-w", "0", "-c", "AGXAccelerator"],
        text=True,
        capture_output=True,
    )
    match = re.search(r'"Device Utilization %"=([0-9]+)', result.stdout)
    return float(match.group(1)) if match else math.nan


def memory_free_percent() -> float:
    if sys.platform == "darwin":
        result = subprocess.run(
            ["/usr/bin/memory_pressure", "-Q"], text=True, capture_output=True
        )
        match = re.search(r"free percentage:\s*([0-9]+)%", result.stdout)
        return float(match.group(1)) if match else math.nan
    if sys.platform.startswith("linux"):
        values = read_meminfo()
        total = values.get("MemTotal", 0)
        return 100 * values.get("MemAvailable", 0) / total if total else math.nan
    return math.nan


def cpu_seconds(value: str) -> float:
    days = 0
    if "-" in value:
        day_text, value = value.split("-", 1)
        days = int(day_text)
    fields = [float(field) for field in value.split(":")]
    seconds = fields[-1]
    if len(fields) >= 2:
        seconds += fields[-2] * 60
    if len(fields) >= 3:
        seconds += fields[-3] * 3600
    return days * 86_400 + seconds


def sample_process_tree(root_pid: int, elapsed: float, sample_path: Path) -> tuple[int, int, str]:
    result = subprocess.run(
        ["ps", "-axo", "ppid=,pid=,time=,rss=,state="],
        text=True,
        capture_output=True,
    )
    records: dict[int, tuple[int, float, int, str]] = {}
    for line in result.stdout.splitlines():
        fields = line.split()
        if len(fields) < 5:
            continue
        try:
            ppid, pid = int(fields[0]), int(fields[1])
            records[pid] = (ppid, cpu_seconds(fields[2]), int(fields[3]), fields[4])
        except ValueError:
            continue

    active = {root_pid}
    for _ in records:
        added = {pid for pid, record in records.items() if record[0] in active}
        if added <= active:
            break
        active.update(added)

    rss = 0
    state = "gone"
    lines: list[str] = []
    for pid in sorted(active):
        record = records.get(pid)
        if record is None:
            continue
        rss += record[2]
        lines.append(f"{elapsed:.3f}\t{pid}\t{record[1]:.2f}\n")
        if pid == root_pid:
            state = record[3]
    with sample_path.open("a") as sample_file:
        sample_file.writelines(lines)
    return rss, len(lines), state


def parse_time_file(path: Path) -> tuple[float, float, float]:
    values: dict[str, float] = {}
    for line in path.read_text().splitlines():
        fields = line.split()
        if len(fields) == 2 and fields[0] in {"real", "user", "sys"}:
            values[fields[0]] = float(fields[1])
    if values.keys() != {"real", "user", "sys"}:
        stop(f"invalid time output: {path}", 1)
    return values["real"], values["user"], values["sys"]


def derive_cpu_intervals(path: Path, cpus: int) -> list[tuple[float, float, float]]:
    last: dict[int, tuple[float, float]] = {}
    totals: dict[float, float] = {}
    times: list[float] = []
    for line in path.read_text().splitlines()[1:]:
        at_text, pid_text, cpu_text = line.split("\t")
        at, pid, cpu = float(at_text), int(pid_text), float(cpu_text)
        if at not in totals:
            totals[at] = 0
            times.append(at)
        if pid in last:
            last_at, last_cpu = last[pid]
            if at > last_at and cpu >= last_cpu:
                totals[at] += 100 * (cpu - last_cpu) / (at - last_at) / cpus
        last[pid] = (at, cpu)
    return [(times[index - 1], at, totals[at]) for index, at in enumerate(times[1:], 1)]


def weighted_median(values: list[tuple[float, float]]) -> float:
    total = sum(weight for _, weight in values)
    if not values or total <= 0:
        return math.nan
    cumulative = 0.0
    for value, weight in sorted(values):
        cumulative += weight
        if cumulative * 2 >= total:
            return value
    return math.nan


def steady_cpu(
    intervals: list[tuple[float, float, float]], real_seconds: float
) -> tuple[list[tuple[float, float]], list[float]]:
    end_limit = real_seconds - 5
    steady: list[tuple[float, float]] = []
    weighted: dict[int, float] = {}
    covered: dict[int, float] = {}
    for interval_start, interval_end, cpu in intervals:
        start = max(interval_start, 5)
        end = min(interval_end, end_limit)
        if end <= start:
            continue
        steady.append((cpu, end - start))
        while start < end:
            bucket = int(start / 5)
            part_end = min(end, (bucket + 1) * 5)
            span = part_end - start
            weighted[bucket] = weighted.get(bucket, 0) + cpu * span
            covered[bucket] = covered.get(bucket, 0) + span
            start = part_end
    windows = [
        weighted[bucket] / covered[bucket]
        for bucket in sorted(weighted)
        if bucket * 5 >= 5
        and bucket * 5 + 5 <= end_limit
        and covered[bucket] >= 4.5
    ]
    return steady, windows


def percentile_nearest(values: list[float], fraction: float) -> float:
    ordered = sorted(values)
    return ordered[max(0, math.ceil(fraction * len(ordered)) - 1)]


def log_field(line: str, name: str) -> int:
    match = re.search(rf"(?:^|\s){re.escape(name)}=([0-9]+)(?:\s|$)", line)
    if match is None:
        stop(f"log field {name} is missing", 1)
    return int(match.group(1))


def directory_kib(path: Path) -> int:
    result = subprocess.run(["du", "-sk", str(path)], text=True, capture_output=True)
    if result.returncode != 0:
        stop(f"cannot measure index size: {path}", 1)
    return int(result.stdout.split()[0])


def parse_args() -> argparse.Namespace:
    parser = argparse.ArgumentParser(
        description=__doc__,
        epilog=BENCH_EPILOG,
        formatter_class=argparse.RawDescriptionHelpFormatter,
    )
    parser.add_argument(
        "--samples",
        type=int,
        default=10,
        help="number of cold index samples; performance gates require at least 10",
    )
    parser.add_argument(
        "--workdir",
        metavar="DIR",
        help="absent or empty directory to retain; default creates a temporary directory",
    )
    parser.add_argument(
        "--report-only",
        action="store_true",
        help="run and validate all samples but do not apply performance gates",
    )
    return parser.parse_args()


def main() -> int:
    args = parse_args()
    samples = positive_int("samples", args.samples)
    model_samples = positive_int(
        "SEEK_BENCH_MODEL_SAMPLES", os.environ.get("SEEK_BENCH_MODEL_SAMPLES", samples)
    )
    expected_files = positive_int(
        "SEEK_BENCH_EXPECTED_FILES", os.environ.get("SEEK_BENCH_EXPECTED_FILES", PROOF_FILES)
    )
    expected_rows = positive_int(
        "SEEK_BENCH_EXPECTED_ROWS", os.environ.get("SEEK_BENCH_EXPECTED_ROWS", PROOF_ROWS)
    )
    gate = not args.report_only
    if gate and samples < 10:
        stop("the gate requires at least 10 samples; use --report-only for a smoke run")
    if gate and model_samples < 10:
        stop("the model gate requires at least 10 samples")

    repo_text = os.environ.get("SEEK_BENCH_REPO", "")
    if not repo_text:
        stop("SEEK_BENCH_REPO is required")
    bench_repo = Path(repo_text).expanduser().resolve()
    seek_text = os.environ.get("SEEK_BIN", "")
    if gate and seek_text:
        stop("SEEK_BIN is only valid with --report-only; gate mode builds from source")
    bench_head = os.environ.get("SEEK_BENCH_HEAD") or PROOF_HEAD
    if gate and (
        bench_head != PROOF_HEAD or expected_files != PROOF_FILES or expected_rows != PROOF_ROWS
    ):
        stop("the gate requires the pinned Kubernetes revision and fixed coverage counts")

    for tool in ("du", "git", "go", "make", "ps"):
        require_tool(tool)
    ctags = shutil.which("universal-ctags") or shutil.which("ctags")
    if ctags is None or "Universal Ctags" not in output([ctags, "--version"]):
        stop("missing tool: universal-ctags")
    validate_repo(bench_repo, bench_head)

    repo_root = Path(__file__).resolve().parent.parent
    source_commit, source_status = source_identity(repo_root)
    source_state = (
        "unavailable"
        if source_commit == "unavailable"
        else "dirty" if source_status else "clean"
    )
    if gate and (source_commit == "unavailable" or source_status):
        stop("the performance gate requires a clean Seek source tree")
    supplied_seek_bin: Path | None = None
    if seek_text:
        supplied_seek_bin = Path(seek_text).expanduser().resolve()
        if not supplied_seek_bin.is_file() or not os.access(
            supplied_seek_bin, os.X_OK
        ):
            stop(f"SEEK_BIN is not an executable file: {supplied_seek_bin}")
    (
        tokenizer_archive,
        tokenizer_archive_sha256,
        tokenizer_library,
        tokenizer_library_sha256,
    ) = restore_tokenizer_library(repo_root)
    workdir = prepare_workdir(args.workdir)
    cache_dir = workdir / "cache"
    results_dir = workdir / "results"
    go_cache_dir = workdir / "go-build-cache"
    cache_dir.mkdir()
    results_dir.mkdir()
    go_cache_dir.mkdir()
    source_env = os.environ | {"GOCACHE": str(go_cache_dir)}

    if supplied_seek_bin is not None:
        seek_bin = supplied_seek_bin
    else:
        seek_bin = workdir / "seek-proof-bin"
        status = subprocess.run(
            ["make", "build", f"OUTPUT={seek_bin}"],
            cwd=repo_root,
            env=source_env,
        ).returncode
        if status != 0:
            stop("Seek build failed", 1)
    binary_sha256 = sha256_file(seek_bin)
    binary_revision, binary_modified = binary_build_identity(seek_bin)
    if gate and (
        binary_revision != source_commit or binary_modified != "false"
    ):
        stop(
            "the performance gate requires a binary built from the clean "
            f"Seek source commit; binary revision={binary_revision} "
            f"modified={binary_modified}, source commit={source_commit}"
        )

    (results_dir / "model-source-status.txt").write_text(source_status)

    bench_query = os.environ.get("SEEK_BENCH_QUERY", "request authentication flow")
    model_p50_limit = env_float("SEEK_BENCH_MODEL_P50_SECONDS", 30)
    p95_limit = env_float("SEEK_BENCH_P95_SECONDS", 60)
    mean_cpu_floor = env_float("SEEK_BENCH_MEAN_CPU_PERCENT", 90)
    median_cpu_floor = env_float("SEEK_BENCH_MEDIAN_CPU_PERCENT", 90)
    window_cpu_floor = env_float("SEEK_BENCH_WINDOW_CPU_PERCENT", 85)

    (results_dir / "runs.tsv").write_text(RUNS_HEADER)
    prime_dir = workdir / "provider-prime"
    prime_dir.mkdir()
    (prime_dir / "prime.go").write_text("package prime\n// provider prime input\n")
    print("priming bundled model and native runtimes", file=sys.stderr)
    seek_env = os.environ | {"SEEK_CACHE_DIR": str(cache_dir)}
    status = run_to_files(
        [str(seek_bin), "--verbose", "-n", "1", "prime", str(prime_dir)],
        results_dir / "prime.stdout",
        results_dir / "prime.stderr",
        env=seek_env,
    )
    prime_log = (results_dir / "prime.stderr").read_text()
    if status != 0 or "Built semantic index" not in prime_log:
        stop("provider prime did not build semantic data", 1)
    match = re.search(
        r"Calibrating semantic inference concurrency.*cpu_limit=([0-9]+)", prime_log
    )
    if match is None:
        stop("Seek did not report its effective CPU count", 1)
    effective_cpus = positive_int("effective CPU count", match.group(1))
    expected_cpus = os.environ.get("SEEK_BENCH_CPUS")
    if (
        expected_cpus is not None
        and positive_int("SEEK_BENCH_CPUS", expected_cpus) != effective_cpus
    ):
        stop(
            f"Seek reported {effective_cpus} CPUs, want SEEK_BENCH_CPUS={expected_cpus}", 1
        )

    print(f"running {model_samples} fixed 100,000-row model samples", file=sys.stderr)
    model_log = results_dir / "model-pass.log"
    with model_log.open("wb") as log_file:
        status = subprocess.run(
            [
                "go",
                "test",
                "./cmd/seek",
                "-run",
                "^$",
                "-bench",
                "^BenchmarkSemanticModelPass100k$",
                "-benchtime=1x",
                f"-count={model_samples}",
                "-timeout=45m",
            ],
            cwd=repo_root,
            env=source_env,
            stdout=log_file,
            stderr=subprocess.STDOUT,
        ).returncode
    model_seconds = [
        float(value) / 1_000_000_000
        for value in re.findall(
            r"^BenchmarkSemanticModelPass100k-\d+\s+\d+\s+([0-9.]+)\s+ns/op",
            model_log.read_text(),
            re.MULTILINE,
        )
    ]
    if status != 0 or len(model_seconds) != model_samples:
        stop(
            f"model benchmark returned {len(model_seconds)} samples, want {model_samples}", 1
        )
    (results_dir / "model-seconds.txt").write_text(
        "".join(f"{value:.6f}\n" for value in model_seconds)
    )
    model_p50 = statistics.median(model_seconds)

    run_rows: list[tuple[float, float, float, int, int, int]] = []
    all_steady: list[tuple[float, float]] = []
    all_windows: list[float] = []
    all_gpu: list[float] = []
    all_memory: list[float] = []
    time_command = require_tool("/usr/bin/time")

    for sample in range(1, samples + 1):
        run_dir = results_dir / f"run-{sample:02d}"
        run_dir.mkdir()
        corpora = reset_corpus_cache(workdir, cache_dir)
        swap_before = swap_used_kib()
        samples_path = run_dir / "samples.tsv"
        process_path = run_dir / "process-samples.tsv"
        samples_path.write_text(
            "elapsed_s\ttree_rss_kib\tprocesses\troot_state\tgpu_pct\tmemory_free_pct\n"
        )
        process_path.write_text("elapsed_s\tpid\tcpu_seconds\n")
        print(f"sample {sample}/{samples}", file=sys.stderr)

        command = [
            time_command,
            "-p",
            "-o",
            str(run_dir / "time.txt"),
            str(seek_bin),
            "--verbose",
            "-n",
            "1",
            bench_query,
            str(bench_repo),
        ]
        started = time.monotonic()
        with (run_dir / "command.stdout").open("wb") as stdout_file, (
            run_dir / "command.stderr"
        ).open("wb") as stderr_file:
            process = subprocess.Popen(
                command, env=seek_env, stdout=stdout_file, stderr=stderr_file
            )
            try:
                while process.poll() is None:
                    elapsed = time.monotonic() - started
                    rss, count, state = sample_process_tree(
                        process.pid, elapsed, process_path
                    )
                    gpu = gpu_percent()
                    memory = memory_free_percent()
                    with samples_path.open("a") as sample_file:
                        sample_file.write(
                            f"{elapsed:.3f}\t{rss}\t{count}\t{state}\t"
                            f"{gpu if math.isfinite(gpu) else 'nan'}\t"
                            f"{memory if math.isfinite(memory) else 'nan'}\n"
                        )
                    time.sleep(1)
            except BaseException:
                process.terminate()
                try:
                    process.wait(timeout=5)
                except subprocess.TimeoutExpired:
                    process.kill()
                raise
            status = process.wait()
        if status != 0:
            stop(f"sample {sample} failed with status {status}; see {run_dir}", status)
        if (run_dir / "command.stdout").stat().st_size == 0:
            stop(f"sample {sample} returned no search result; see {run_dir}", 1)

        log_lines = (run_dir / "command.stderr").read_text().splitlines()
        build_lines = [line for line in log_lines if 'msg="Built semantic index"' in line]
        if len(build_lines) != 1:
            stop(
                f"sample {sample} logged {len(build_lines)} completed semantic builds, want one",
                1,
            )
        built_files = log_field(build_lines[0], "files")
        built_rows = log_field(build_lines[0], "rows")
        if built_files != expected_files or built_rows != expected_rows:
            stop(
                f"sample {sample} built files={built_files} rows={built_rows}; "
                f"want files={expected_files} rows={expected_rows}",
                1,
            )
        scheduler_lines = [
            line
            for line in log_lines
            if 'msg="Semantic inference scheduler finished"' in line
        ]
        if len(scheduler_lines) != 1:
            stop(
                f"sample {sample} logged {len(scheduler_lines)} scheduler summaries, want one",
                1,
            )
        scheduler = scheduler_lines[0]
        scheduler_rows = log_field(scheduler, "rows")
        max_in_flight = log_field(scheduler, "max_in_flight")
        final_limit = log_field(scheduler, "final_limit")
        call_limit = log_field(scheduler, "call_limit")
        input_empty_polls = log_field(scheduler, "input_empty_polls")
        if scheduler_rows != expected_rows or not final_limit <= max_in_flight <= call_limit:
            stop(
                f"sample {sample} scheduler rows={scheduler_rows} max={max_in_flight} "
                f"final={final_limit} ceiling={call_limit}",
                1,
            )
        calibration_line = next(
            (
                index
                for index, line in enumerate(log_lines)
                if "Calibrating semantic inference concurrency" in line
            ),
            None,
        )
        zoekt_lines = [
            index
            for index, line in enumerate(log_lines)
            if re.search(r"finished shard .*\.zoekt:", line)
        ]
        if calibration_line is None or not zoekt_lines or calibration_line >= zoekt_lines[-1]:
            stop(f"sample {sample} did not prove overlapping Zoekt and semantic work", 1)
        log = "\n".join(log_lines)
        if re.search(r"Semantic .*failed|Joined search failed|Re-ranking failed", log):
            stop(
                f"sample {sample} used a fallback path; see {run_dir / 'command.stderr'}", 1
            )
        if 'msg="Searched semantic index" backend=usearch' not in log:
            stop(f"sample {sample} did not use USearch for semantic retrieval", 1)
        if len(list(corpora.rglob(".joined-v1"))) != 1 or len(
            list(corpora.glob("**/semantic-*/manifest.json"))
        ) != 1:
            stop(f"sample {sample} did not publish one joined generation", 1)

        verify_status = run_to_files(
            [str(seek_bin), "--verbose", "-n", "1", bench_query, str(bench_repo)],
            run_dir / "verify.stdout",
            run_dir / "verify.stderr",
            env=seek_env,
        )
        verify_out = (run_dir / "verify.stdout").read_text()
        verify_log = (run_dir / "verify.stderr").read_text()
        if (
            verify_status != 0
            or not verify_out
            or "Built semantic index" in verify_log
            or re.search(r"Semantic .*failed|Joined search failed|Re-ranking failed", verify_log)
        ):
            stop(f"sample {sample} did not reopen its joined index cleanly", 1)

        real_seconds, user_seconds, sys_seconds = parse_time_file(run_dir / "time.txt")
        swap_delta = swap_used_kib() - swap_before
        sample_rows = [
            line.split("\t") for line in samples_path.read_text().splitlines()[1:]
        ]
        peak_rss = max((int(row[1]) for row in sample_rows), default=0)
        index_kib = directory_kib(corpora)
        intervals = derive_cpu_intervals(process_path, effective_cpus)
        (run_dir / "cpu-percent.tsv").write_text(
            "start_s\tend_s\tcpu_pct\n"
            + "".join(
                f"{start:.3f}\t{end:.3f}\t{cpu:.6f}\n"
                for start, end, cpu in intervals
            )
        )
        steady, windows = steady_cpu(intervals, real_seconds)
        (run_dir / "steady-cpu-percent.txt").write_text(
            "".join(f"{cpu:.6f}\t{weight:.6f}\n" for cpu, weight in steady)
        )
        (run_dir / "window-cpu-percent.txt").write_text(
            "".join(f"{value:.6f}\n" for value in sorted(windows))
        )
        mean_cpu = 100 * (user_seconds + sys_seconds) / real_seconds / effective_cpus
        median_cpu = weighted_median(steady)
        min_window = min(windows, default=math.nan)
        all_steady.extend(steady)
        all_windows.extend(windows)
        for row in sample_rows:
            if row[4] != "nan":
                all_gpu.append(float(row[4]))
            if row[5] != "nan":
                all_memory.append(float(row[5]))
        run_rows.append(
            (real_seconds, user_seconds, sys_seconds, peak_rss, swap_delta, index_kib)
        )
        with (results_dir / "runs.tsv").open("a") as runs_file:
            runs_file.write(
                f"{sample}\t{real_seconds}\t{user_seconds}\t{sys_seconds}\t"
                f"{mean_cpu:.3f}\t{median_cpu:.3f}\t{min_window:.3f}\t{peak_rss}\t"
                f"{swap_delta}\t{index_kib}\t{built_files}\t{built_rows}\t"
                f"{max_in_flight}\t{final_limit}\t{call_limit}\t{input_empty_polls}\n"
            )

    real_values = [row[0] for row in run_rows]
    p95 = percentile_nearest(real_values, 0.95)
    total_real = sum(row[0] for row in run_rows)
    mean_cpu = (
        100
        * sum(row[1] + row[2] for row in run_rows)
        / total_real
        / effective_cpus
    )
    median_cpu = weighted_median(all_steady)
    min_window = min(all_windows, default=math.nan)
    max_rss = max(row[3] for row in run_rows)
    max_swap_delta = max(row[4] for row in run_rows)
    max_index_kib = max(row[5] for row in run_rows)
    median_gpu = statistics.median(all_gpu) if all_gpu else math.nan
    min_memory = min(all_memory, default=math.nan)

    final_source_commit, final_source_status = source_identity(repo_root)
    final_binary_sha256 = sha256_file(seek_bin)
    final_tokenizer_archive_sha256 = sha256_file(tokenizer_archive)
    final_tokenizer_library_sha256 = sha256_file(tokenizer_library)
    validate_repo(bench_repo, bench_head)
    if (final_source_commit, final_source_status) != (source_commit, source_status):
        stop("Seek source changed during the benchmark; results are invalid", 1)
    if final_binary_sha256 != binary_sha256:
        stop("Seek binary changed during the benchmark; results are invalid", 1)
    if final_tokenizer_archive_sha256 != tokenizer_archive_sha256:
        stop(
            "tracked tokenizer archive changed during the benchmark; results are invalid",
            1,
        )
    if final_tokenizer_library_sha256 != tokenizer_library_sha256:
        stop(
            "native tokenizer library changed during the benchmark; results are invalid",
            1,
        )

    summary = f"""# Seek cold joined-index proof

- Mode: {"performance gate" if gate else "report only; limits not enforced"}
- Binary: {seek_bin}
- Binary SHA-256: {binary_sha256}
- Binary source commit: {binary_revision}
- Binary source modified: {binary_modified}
- Model source: {repo_root}
- Model source commit: {source_commit}
- Model source state: {source_state}
- Isolated Go build cache: {go_cache_dir}
- Tokenizer archive SHA-256: {tokenizer_archive_sha256}
- Native tokenizer library SHA-256: {tokenizer_library_sha256}
- Benchmark repository: {bench_repo}
- Benchmark repository commit: {bench_head}
- Samples: {samples}
- Fixed model samples: {model_samples}
- Effective CPUs: {effective_cpus}
- Query: {bench_query}
- Fixed model-pass p50: {model_p50:.3f} s (limit {model_p50_limit:g} s)
- Nearest-rank p95: {p95:.3f} s (limit {p95_limit:g} s)
- Semantic coverage per run: {expected_files} files, {expected_rows} rows
- Mean CPU from total process time: {mean_cpu:.3f}% (floor {mean_cpu_floor:g}%)
- Median sampled CPU: {median_cpu:.3f}% (floor {median_cpu_floor:g}%)
- Lowest complete five-second window: {min_window:.3f}% (floor {window_cpu_floor:g}%)
- Median sampled GPU device use when available: {median_gpu:.3f}%
- Lowest sampled free-memory percentage when available: {min_memory:.3f}%
- Maximum sampled process-tree RSS: {max_rss} KiB
- Maximum joined cache size: {max_index_kib} KiB
- Maximum swap increase: {max_swap_delta} KiB

The mean CPU value uses child-inclusive user and system time from /usr/bin/time.
Median and window values use changes in each live process CPU clock. Sampling can
omit the last part of a short child process. It can make CPU use lower, but it
cannot make CPU use higher. The check excludes the first and last five seconds.
The denominator is Seek's logged Go CPU limit. GPU and memory samples give
context. They do not waive a CPU failure.
"""
    (results_dir / "summary.md").write_text(summary)
    print(summary, end="")
    print(f"evidence: {workdir}", file=sys.stderr)

    if not gate:
        return 0
    failures: list[str] = []
    if model_p50 >= model_p50_limit:
        failures.append(
            f"model p50 gate failed: {model_p50:.3f} s >= {model_p50_limit:g} s"
        )
    if p95 > p95_limit:
        failures.append(f"p95 gate failed: {p95:.3f} s > {p95_limit:g} s")
    if mean_cpu < mean_cpu_floor:
        failures.append(
            f"mean CPU gate failed: {mean_cpu:.3f}% < {mean_cpu_floor:g}%"
        )
    if not math.isfinite(median_cpu) or median_cpu < median_cpu_floor:
        failures.append(
            f"median CPU gate failed: {median_cpu:.3f}% < {median_cpu_floor:g}%"
        )
    if not math.isfinite(min_window) or min_window < window_cpu_floor:
        failures.append(
            f"five-second CPU window gate failed: {min_window:.3f}% < {window_cpu_floor:g}%"
        )
    if max_swap_delta > 0:
        failures.append(f"swap gate failed: increased by {max_swap_delta} KiB")
    for failure in failures:
        print(failure, file=sys.stderr)
    return 1 if failures else 0


if __name__ == "__main__":
    raise SystemExit(main())
