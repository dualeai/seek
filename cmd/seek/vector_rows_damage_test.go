package main

import (
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

// damageVectorRows changes the content ID of each row of name in each
// semanticRowsFile below root, out of the build directories. It then records
// the new file as trusted, as media damage that the stat check does not see
// would leave it. It returns the number of rows that it changed.
func damageVectorRows(t *testing.T, root, name string) int {
	t.Helper()
	changed := 0
	_ = filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil || entry.IsDir() || entry.Name() != semanticRowsFile || strings.Contains(path, "/.build-") {
			return nil
		}
		dir := filepath.Dir(path)
		manifest, err := readSemanticManifestUnchecked(dir)
		if err != nil {
			return nil
		}
		file, err := os.Open(path)
		if err != nil {
			t.Fatal(err)
		}
		rows, err := readSemanticRowsFrom(file, manifest.Rows, false)
		_ = file.Close()
		if err != nil {
			t.Fatal(err)
		}
		local := 0
		for index := range rows {
			if rows[index].path == name {
				rows[index].contentID[0] ^= 0xFF
				local++
			}
		}
		if local == 0 {
			return nil
		}
		tmp := path + ".damaged"
		if _, err := writeSemanticRows(tmp, rows); err != nil {
			t.Fatal(err)
		}
		if err := os.Rename(tmp, path); err != nil {
			t.Fatal(err)
		}
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		refreshSemanticRowsCheck(dir, semanticRowsCheckOf(info))
		changed += local
		return nil
	})
	return changed
}

// repairWarnings counts the warnings in logs from the record at index from:
// shard repairs, vector repairs, and skipped nodes. It also returns the record
// count.
func repairWarnings(logs *testLogRecorder, from int) (shards, vectors, skips, records int) {
	all := logs.Records()
	for _, record := range all[from:] {
		switch {
		case strings.Contains(record.Message, "Index damage detected"):
			shards++
		case strings.Contains(record.Message, "Vector index damage detected"):
			vectors++
		case strings.Contains(record.Message, "Skipped a search node"):
			skips++
		}
	}
	return shards, vectors, skips, len(all)
}

// TestDamagedVectorRowsAreRepairedOnce damages the rows of one file in a
// folder node and in a Git node. This is media damage that the stat check does
// not see. The test model accepts every row. So semantic.go, which holds no
// query word, prints exactly when the search binds its rows. The first search
// repairs the shards, finds the same mismatch on fresh shards, and unbinds the
// vectors. It gives one warning of each, the text hit, and no semantic.go. The
// next two searches print semantic.go with no warning. In the folder node, the
// third search makes no build directory (newBuildDir), so it builds no index.
func TestDamagedVectorRowsAreRepairedOnce(t *testing.T) {
	requireTools(t)
	for _, kind := range []string{"folder", "git"} {
		t.Run(kind, func(t *testing.T) {
			home := setTestUserCache(t)
			var operands []string
			if kind == "folder" {
				folder := t.TempDir()
				writeFileAt(t, folder, "strict.go", "package sample\n// alpha beta\n")
				writeFileAt(t, folder, "semantic.go", "package sample\n// dispatches a request\n")
				operands = []string{folder}
				t.Chdir(t.TempDir())
			} else {
				repo := initGitRepo(t, "strict.go", "package sample\n// alpha beta\n")
				writeFileAt(t, repo, "semantic.go", "package sample\n// dispatches a request\n")
				gitRunIn(t, repo, "add", ".")
				gitRunIn(t, repo, "commit", "-q", "-m", "semantic")
				t.Chdir(repo)
			}
			logs := captureTestLogs(t, slog.LevelWarn)
			search := func() string {
				t.Helper()
				output, _, err := stubSearch(t, payloadAccept, append([]string{"alpha beta"}, operands...)...)
				if err != nil {
					t.Fatalf("err=%v output:\n%s", err, output)
				}
				return output
			}
			if output := search(); !strings.Contains(output, "semantic.go") {
				t.Fatalf("warm-up has no vector hit:\n%s", output)
			}
			if damageVectorRows(t, home, "semantic.go") == 0 {
				t.Fatal("no row of semantic.go to damage")
			}
			from := len(logs.Records())
			output := search()
			shards, vectors, skips, next := repairWarnings(logs, from)
			if shards != 1 || vectors != 1 || skips != 0 || !strings.Contains(output, "strict.go") ||
				strings.Contains(output, "semantic.go") {
				t.Fatalf("first search: shard repairs=%d vector repairs=%d skips=%d, want 1, 1, 0 and the text hit only:\n%s",
					shards, vectors, skips, output)
			}
			for run := 2; run <= 3; run++ {
				buildDirs := buildDirCounter.Load()
				output := search()
				shards, vectors, skips, next = repairWarnings(logs, next)
				if shards+vectors+skips != 0 || !strings.Contains(output, "semantic.go") {
					t.Fatalf("search %d: warnings %d/%d/%d, want none and the vector hit:\n%s", run, shards, vectors, skips, output)
				}
				if built := buildDirCounter.Load() - buildDirs; run == 3 && kind == "folder" && built != 0 {
					t.Fatalf("search 3 made %d build directories, want none", built)
				}
			}
		})
	}
}

// TestDamagedRowsAreRepairedWhileAnotherFileChanges damages the rows of one
// folder file while another file changes during every build. Each search sees
// the tree change and reads the node again. The retry compares each file with
// the published folder manifest (evidenceMismatch). So the changing file gives
// no vector hit, and the unchanged file with damaged rows makes the vectors
// rebuild. The next search finds the damaged file with no warning. The test
// model accepts every row. So semantic.go and writer.log, which hold no query
// word, print exactly when the search binds their rows. Both print in the
// warm-up. writer.log prints in no search while it changes, and semantic.go
// prints again after the repair.
func TestDamagedRowsAreRepairedWhileAnotherFileChanges(t *testing.T) {
	requireTools(t)
	home := setTestUserCache(t)
	t.Chdir(t.TempDir())
	folder := t.TempDir()
	writeFileAt(t, folder, "strict.go", "package sample\n// alpha beta\n")
	writeFileAt(t, folder, "semantic.go", "package sample\n// dispatches a request\n")
	writeFileAt(t, folder, "writer.log", "version 0\n")
	search := func() string {
		t.Helper()
		output, _, err := stubSearch(t, payloadAccept, "alpha beta", folder)
		if err != nil {
			t.Fatalf("err=%v output:\n%s", err, output)
		}
		return output
	}
	if output := search(); !strings.Contains(output, "semantic.go") || !strings.Contains(output, "writer.log") {
		t.Fatalf("warm-up has no vector hit of semantic.go and writer.log:\n%s", output)
	}
	if damageVectorRows(t, home, "semantic.go") == 0 {
		t.Fatal("no row of semantic.go to damage")
	}
	// From now on, writer.log changes after each read, so every build reads
	// it twice with different bytes.
	var versions atomic.Int32
	realOpen := folderOpenFile
	folderOpenFile = func(path string) (*os.File, error) {
		file, err := realOpen(path)
		if err == nil && filepath.Base(path) == "writer.log" {
			writeFileAt(t, folder, "writer.log", "version "+strings.Repeat("x", int(versions.Add(1)))+"\n")
		}
		return file, err
	}
	t.Cleanup(func() { folderOpenFile = realOpen })
	logs := captureTestLogs(t, slog.LevelWarn)
	output := search()
	_, vectors, skips, next := repairWarnings(logs, 0)
	if vectors != 1 || skips != 0 || !strings.Contains(output, "strict.go") ||
		strings.Contains(output, "semantic.go") || strings.Contains(output, "writer.log") {
		t.Fatalf("first search: vector repairs=%d skips=%d, want 1 and 0 and the text hit only:\n%s", vectors, skips, output)
	}
	output = search()
	shards, vectors, skips, _ := repairWarnings(logs, next)
	if shards+vectors+skips != 0 || !strings.Contains(output, "semantic.go") || strings.Contains(output, "writer.log") {
		t.Fatalf("second search: warnings %d/%d/%d, want none, the vector hit of semantic.go and no writer.log:\n%s",
			shards, vectors, skips, output)
	}
}

// TestFileOfHalfTheShardFormatGetsNoVectorRows builds a folder under scaled
// guard limits (a 2.5 MiB shard format). A shard takes at least 2 bytes for
// each byte of content, so the shard of a 1.5 MiB file always passes the
// format. The shard guard then keeps the file by name only, and the vector
// build embeds none of it. A 512 KiB file gets its rows.
func TestFileOfHalfTheShardFormatGetsNoVectorRows(t *testing.T) {
	requireTools(t)
	swapGuardLimitsForTest(t)
	setTestUserCache(t)
	t.Chdir(t.TempDir())
	folder := t.TempDir()
	writeFileAt(t, folder, "strict.go", "package sample\n// alpha beta\n")
	line := "// dispatches a request\n"
	writeFileAt(t, folder, "large.go", strings.Repeat(line, (3<<19)/len(line)+1))
	writeFileAt(t, folder, "small.go", strings.Repeat(line, (1<<19)/len(line)))
	output, scorer, err := stubSearch(t, payloadAccept, "alpha beta", folder)
	if err != nil {
		t.Fatalf("err=%v output:\n%s", err, output)
	}
	embedded := map[string]int{}
	for _, path := range scorer.unitPaths() {
		embedded[path]++
	}
	if embedded["large.go"] != 0 || embedded["small.go"] == 0 {
		t.Fatalf("embedded units: large.go=%d small.go=%d, want 0 and some", embedded["large.go"], embedded["small.go"])
	}
}

// TestDamagedRowsRepairWaitsForNoBuild checks that the repair of damaged rows
// keeps every generation while a build holds the node's build lock. That build
// can still link their graph files. Once the lock is free, the repair removes
// the generations and the bound descriptor (joinedGenerationFile).
func TestDamagedRowsRepairWaitsForNoBuild(t *testing.T) {
	cacheDir := t.TempDir()
	plan := node{cacheDir: cacheDir, indexDir: filepath.Join(cacheDir, "index"), root: t.TempDir()}
	generation := filepath.Join(plan.indexDir, semanticGenerationPrefix+"damaged")
	writeFixtureFile(t, filepath.Join(generation, semanticRowsFile), "rows\n")
	writeFixtureFile(t, filepath.Join(cacheDir, lockFile), "")
	writeFixtureFile(t, filepath.Join(cacheDir, joinedGenerationFile), "{}\n")
	build, ok, err := tryBuildLock(cacheDir)
	if err != nil || !ok {
		t.Fatalf("take the build lock: ok=%v err=%v", ok, err)
	}
	removeDamagedVectors(t.Context(), plan)
	if _, err := os.Stat(generation); err != nil {
		t.Fatalf("the repair removed a generation while a build ran: %v", err)
	}
	releaseLock(build)
	removeDamagedVectors(t.Context(), plan)
	if _, err := os.Stat(generation); !os.IsNotExist(err) {
		t.Fatalf("the repair kept the damaged generation with no build: %v", err)
	}
	if _, err := os.Stat(filepath.Join(cacheDir, joinedGenerationFile)); !os.IsNotExist(err) {
		t.Fatalf("the repair kept the bound descriptor: %v", err)
	}
}
