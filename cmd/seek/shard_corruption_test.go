package main

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime/metrics"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sourcegraph/zoekt/query"
)

const (
	// shardScanEnv holds the scan of TestShardScanHelper: the shard path, the
	// first offset, and the content query, one per line.
	shardScanEnv = "SEEK_TEST_SHARD_SCAN"
	// shardScanHeapCap ends a scan child whose heap passes it. Zoekt's delta
	// readers append forever on a cut-off varint (index/bits.go
	// fromSizedDeltas and unmarshalDocSections), which reached 12 GB in 1.7 s.
	// A cap of 128 MiB finds the same offsets as 512 MiB. The -race child then
	// peaks near 390 MB of memory instead of 2 GB.
	// TODO(https://github.com/sourcegraph/zoekt/issues/1205): fail on a hang
	// in place of the heap cap once Zoekt returns an error on a cut-off
	// varint.
	shardScanHeapCap  = 128 << 20
	shardScanHangExit = 3
)

// TestShardScanHelper is the child process of shardScan. It changes one byte
// of the shard at a time. It prints the outcome of each Zoekt call of Seek on
// a shard: the open, the content search, the name listing and the repository
// list. A heap above shardScanHeapCap ends the child with
// shardScanHangExit after it prints the offset. The child ends with os.Exit,
// so it writes only into the directory of the shard copy, which its parent
// owns and removes.
func TestShardScanHelper(t *testing.T) {
	spec := os.Getenv(shardScanEnv)
	if spec == "" {
		return
	}
	parts := strings.Split(spec, "\n")
	original, err := os.ReadFile(parts[0])
	if err != nil {
		t.Fatal(err)
	}
	start, err := strconv.Atoi(parts[1])
	if err != nil {
		t.Fatal(err)
	}
	content, err := query.Parse(parts[2])
	if err != nil {
		t.Fatal(err)
	}
	var current atomic.Int64
	go func() {
		samples := []metrics.Sample{{Name: "/memory/classes/heap/objects:bytes"}}
		for range time.Tick(5 * time.Millisecond) {
			metrics.Read(samples)
			if samples[0].Value.Uint64() > shardScanHeapCap {
				fmt.Printf("%d hang\n", current.Load())
				os.Exit(shardScanHangExit)
			}
		}
	}()
	flipped := filepath.Join(filepath.Dir(parts[0]), "flipped.zoekt")
	for offset := start; offset < len(original); offset++ {
		current.Store(int64(offset))
		data := bytes.Clone(original)
		data[offset] ^= 0xFF
		if err := os.WriteFile(flipped, data, 0o600); err != nil {
			t.Fatal(err)
		}
		fmt.Printf("%d %s\n", offset, shardScanOutcome(flipped, content))
	}
	os.Exit(0)
}

// shardScanOutcome runs each of Seek's Zoekt shard calls on path and gives
// the class of each. The result is "open=<class>" when the open fails, else
// "listing=<class> list=<class> search=<class>". The current-copy listing and
// the BM25 counts read the name listing, the BM25 counts read the repository
// list, and search is the content search.
func shardScanOutcome(path string, content query.Q) string {
	searcher, err := openShard(path)
	if err != nil {
		return "open=" + shardScanClass(fmt.Errorf("%w: %w", errShardUnloadable, err))
	}
	defer searcher.Close()
	ctx := context.Background()
	opts := searchOpts
	listing := &query.Type{Type: query.TypeFileName, Child: &query.Const{Value: true}}
	_, listingErr := zoektSearch(ctx, searcher, listing, &opts)
	_, listErr := zoektList(ctx, searcher)
	_, searchErr := zoektSearch(ctx, searcher, content, &opts)
	return "listing=" + shardScanClass(listingErr) + " list=" + shardScanClass(listErr) + " search=" + shardScanClass(searchErr)
}

// shardScanClass names the class of err: ok, skip for an error that is not
// index damage, or the kind of damage from the error text.
func shardScanClass(err error) string {
	if err == nil {
		return "ok"
	}
	message := err.Error()
	switch {
	case !errors.Is(err, errShardUnloadable):
		return "skip"
	case strings.Contains(message, "corrupt shard"):
		return "corrupt-shard"
	case strings.Contains(message, "corrupt index"):
		return "corrupt-index"
	case strings.Contains(message, "runtime error"):
		return "runtime-error"
	case strings.Contains(message, "out of bounds: "):
		return "out-of-bounds"
	default:
		return "damage"
	}
}

// shardScan runs TestShardScanHelper on a copy of shard until stop accepts an
// outcome, and restarts it after the offset of each hang. It returns the
// number of hangs. A child that stops in any other way, such as a crash,
// fails the test.
func shardScan(t *testing.T, shard, content string, stop func(offset int, outcome string) bool) int {
	t.Helper()
	data, err := os.ReadFile(shard)
	if err != nil {
		t.Fatal(err)
	}
	copyPath := filepath.Join(t.TempDir(), "scan.zoekt")
	if err := os.WriteFile(copyPath, data, 0o600); err != nil {
		t.Fatal(err)
	}
	hangs := 0
	for start := 0; start < len(data); {
		cmd := exec.CommandContext(t.Context(), os.Args[0], "-test.run=^TestShardScanHelper$")
		cmd.Env = append(cmd.Environ(), shardScanEnv+"="+copyPath+"\n"+strconv.Itoa(start)+"\n"+content)
		var stderr bytes.Buffer
		cmd.Stderr = &stderr
		stdout, err := cmd.StdoutPipe()
		if err != nil {
			t.Fatal(err)
		}
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		next, stopped := len(data), false
		lines := bufio.NewScanner(stdout)
		for lines.Scan() {
			field, outcome, ok := strings.Cut(lines.Text(), " ")
			offset, convErr := strconv.Atoi(field)
			if !ok || convErr != nil {
				continue
			}
			if outcome == "hang" {
				hangs++
				next = offset + 1
				continue
			}
			if stop(offset, outcome) {
				stopped = true
				break
			}
		}
		if stopped {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
			return hangs
		}
		err = cmd.Wait()
		if exitErr, ok := errors.AsType[*exec.ExitError](err); err != nil && (!ok || exitErr.ExitCode() != shardScanHangExit) {
			t.Fatalf("the shard scan child from offset %d stopped: %v\n%s", start, err, stderr.String())
		}
		start = next
	}
	return hangs
}

// TestCorruptShardIsRepairedNeverCrashes changes one byte of a shard at a
// time. A child process runs Seek's Zoekt shard calls on each change. A panic
// that escaped zoektOpen, zoektSearch or zoektList would crash the child and
// fail the test. Then a CLI child process searches the first change of each
// case:
//   - folder shard: a Zoekt corrupt-shard panic, a corrupt-index panic, a Go
//     runtime error, and a read past the end of the file;
//   - uncommitted shard: a failed name listing, which the current-copy
//     listing reads;
//   - committed shard with a new time: a failed listing or repository list,
//     which the BM25 counts read.
//
// The search must not crash. The folder and uncommitted cases must warn once
// for the repair. The committed case need not repair: when it does not warn
// once, the test writes the original bytes back. The next search must find
// every file with no warning. The shard bytes differ from build to build, so
// the test scans for the offsets.
func TestCorruptShardIsRepairedNeverCrashes(t *testing.T) {
	requireTools(t)
	const marker = "zqcorruptmark"
	f := newGraphFixture(t)
	folder := f.path("folder")
	writeFileAt(t, folder, "a.go", "package a\n// "+marker+" alpha\nfunc A() {}\n")
	writeFileAt(t, folder, "b.txt", marker+" beta\nmore text here\n")
	writeFileAt(t, folder, "c.md", "# title\n\n"+marker+" gamma\n")
	repo := f.GitRepo("repo", "c.go", "package c\n// "+marker+" committed\nfunc C() {}\n")
	writeFileAt(t, repo, "u.go", "package c\n// "+marker+" untracked\nfunc U() {}\n")

	damage := func(outcome, call string) bool {
		_, class, _ := strings.Cut(outcome, call+"=")
		class, _, _ = strings.Cut(class, " ")
		return class != "" && class != "ok" && class != "skip"
	}
	for _, test := range []struct {
		name    string
		dir     string
		files   int
		shard   func(base string) bool
		newTime bool
		repairs bool
		// want maps each case to the outcome rule of its first offset.
		want map[string]func(outcome string) bool
	}{
		{"folder shard", folder, 3, func(string) bool { return true }, false, true, map[string]func(string) bool{
			"corrupt-shard": func(o string) bool { return strings.Contains(o, "=corrupt-shard") },
			"corrupt-index": func(o string) bool { return strings.Contains(o, "=corrupt-index") },
			"runtime-error": func(o string) bool { return strings.Contains(o, "=runtime-error") },
			// The open already counts any error as damage (loadShardPaths), so
			// this case needs the error from a call after the open.
			"out-of-bounds": func(o string) bool { return !strings.HasPrefix(o, "open=") && strings.Contains(o, "=out-of-bounds") },
		}},
		{"uncommitted shard", repo, 2, isUncommittedShard, false, true, map[string]func(string) bool{
			"listing": func(o string) bool { return damage(o, "listing") },
		}},
		{"committed shard with a new time", repo, 2, func(base string) bool { return !isUncommittedShard(base) }, true, false, map[string]func(string) bool{
			"bm25 counts": func(o string) bool { return damage(o, "listing") || damage(o, "list") },
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			cache := t.TempDir()
			args := []string{"--lexical-only", marker, test.dir}
			search := func() cliProcessResult {
				result := runCLIProcessWithCache(t, cache, test.dir, args, nil)
				if result.code == 2 && (strings.Contains(result.stderr, "panic:") || strings.Contains(result.stderr, "fatal error:")) {
					t.Fatalf("the search crashed:\n%s", result.stderr)
				}
				return result
			}
			if result := search(); result.code != 0 || strings.Count(result.stdout, "## ") != test.files {
				t.Fatalf("first build: code=%d stdout=%q stderr=%q", result.code, result.stdout, result.stderr)
			}
			shard, _, _ := corruptionTarget(t, cache, test.shard)
			found := map[string]int{}
			hangs := shardScan(t, shard, marker, func(offset int, outcome string) bool {
				for name, rule := range test.want {
					if _, done := found[name]; !done && rule(outcome) {
						found[name] = offset
					}
				}
				return len(found) == len(test.want)
			})
			t.Logf("first offsets %v; %d hangs", found, hangs)
			for name, offset := range found {
				shard, original, info := corruptionTarget(t, cache, test.shard)
				flipped := bytes.Clone(original)
				flipped[offset] ^= 0xFF
				writeShardBytes(t, shard, flipped, info.ModTime(), test.newTime)
				first := search()
				repaired := strings.Count(first.stderr, "Index damage detected") == 1
				if test.repairs && !repaired {
					t.Fatalf("%s at offset %d: want one repair, got code=%d stderr=%q", name, offset, first.code, first.stderr)
				}
				if !repaired {
					writeShardBytes(t, shard, original, info.ModTime(), false)
				}
				if next := search(); next.code != 0 || next.stderr != "" || strings.Count(next.stdout, "## ") != test.files {
					t.Fatalf("%s at offset %d: the next search gave code=%d stdout=%q stderr=%q",
						name, offset, next.code, next.stdout, next.stderr)
				}
			}
			for name := range test.want {
				if _, ok := found[name]; !ok {
					t.Errorf("no one-byte change gave the %q case", name)
				}
			}
		})
	}
}

// corruptionTarget returns the one shard in cache that keep accepts, with its
// bytes and file information.
func corruptionTarget(t *testing.T, cache string, keep func(base string) bool) (string, []byte, os.FileInfo) {
	t.Helper()
	var shards []string
	_ = filepath.WalkDir(cache, func(path string, entry fs.DirEntry, err error) error {
		if err == nil && !entry.IsDir() && strings.HasSuffix(path, ".zoekt") &&
			!strings.Contains(path, "/.build-") && keep(filepath.Base(path)) {
			shards = append(shards, path)
		}
		return nil
	})
	if len(shards) != 1 {
		t.Fatalf("shards %v, want one", shards)
	}
	data, err := os.ReadFile(shards[0])
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(shards[0])
	if err != nil {
		t.Fatal(err)
	}
	return shards[0], data, info
}

// writeShardBytes writes data over the shard in place, so the shard keeps its
// inode. The shard keeps the time modTime, or gets a later time with newTime.
func writeShardBytes(t *testing.T, shard string, data []byte, modTime time.Time, newTime bool) {
	t.Helper()
	if err := os.WriteFile(shard, data, 0o600); err != nil {
		t.Fatal(err)
	}
	if newTime {
		modTime = modTime.Add(time.Hour)
	}
	if err := os.Chtimes(shard, modTime, modTime); err != nil {
		t.Fatal(err)
	}
}
