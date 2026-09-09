package main

// Partial-loss regression matrix.
//
// Losing a subset of files in a corpus index directory must cause repair on the
// next search. Each damage row checks the same contract:
//
//   - each live marker is found in exactly its source file;
//   - tombstoned markers do not return;
//   - the first search repairs the damage; and
//   - two later searches do not change the repaired shard family.
//
// Counting documents is not enough. A stale .meta keeps the document count
// constant while serving content that exists nowhere in the tree, and a byte
// flip keeps both count and size constant while hiding a live document. Only
// a marker set proves what a user would actually get back.
//
// Isolation. Every test here calls planGitTestCorpus / planFolderTestCorpus,
// which call setTestUserCache, which t.Setenv HOME, XDG_CACHE_HOME and an
// empty SEEK_CACHE_DIR. Each test therefore owns a private cache root, and
// t.Setenv forbids t.Parallel, so rows never race for a corpus.

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// ---------------------------------------------------------------------------
// Fixture
// ---------------------------------------------------------------------------

// plMarker maps a unique search token to its repository-relative file.
type plMarker struct {
	token string
	file  string
}

// plCorpus is a delta-stacked committed family plus the marker ledger that
// describes what a correct answer looks like.
//
// Each committed delta adds a shard and updates .meta sidecars. The fixture
// therefore contains a real multi-shard family with tombstones.
type plCorpus struct {
	repoDir string
	paths   gitPaths
	plan    corpusPlan

	live []plMarker // must be findable, exactly once, at their own path
	dead []plMarker // tombstoned; must never come back

	golden string // pristine copy of cacheDir, for per-row restore
	head   string // expected fixture HEAD
}

// buildPartialLossCorpus produces a committed family with multiple delta shards
// and two tombstones.
func buildPartialLossCorpus(t *testing.T, ctx context.Context) *plCorpus {
	t.Helper()
	requireTools(t)

	repo := initGitRepo(t, "seed.go", "package main\n// PL_SEED\n")
	paths, plan := planGitTestCorpus(t, repo)
	c := &plCorpus{repoDir: repo, paths: paths, plan: plan}
	reindexGit(t, ctx, paths, plan)
	c.live = append(c.live, plMarker{"PL_SEED", "seed.go"})

	// ghost.go is committed with one marker and later rewritten with another:
	// the first becomes a tombstone, the second stays live.
	writeTrackedFile(t, repo, "ghost.go", "package main\n// PL_GHOST\n")
	reindexGit(t, ctx, paths, plan)

	// gone.go is committed and later deleted: its marker becomes a tombstone.
	writeTrackedFile(t, repo, "gone.go", "package main\n// PL_GONE\n")
	reindexGit(t, ctx, paths, plan)

	// Add three files in separate committed generations.
	for i := range 3 {
		writeTrackedFile(t, repo, fmt.Sprintf("f%d.go", i),
			fmt.Sprintf("package main\n// PL_MARK_%d %s\n", i, strings.Repeat("x", i*7)))
		reindexGit(t, ctx, paths, plan)
		c.live = append(c.live, plMarker{fmt.Sprintf("PL_MARK_%d", i), fmt.Sprintf("f%d.go", i)})
	}

	writeTrackedFile(t, repo, "ghost.go", "package main\n// PL_ALIVE\n")
	reindexGit(t, ctx, paths, plan)
	c.live = append(c.live, plMarker{"PL_ALIVE", "ghost.go"})
	c.dead = append(c.dead, plMarker{"PL_GHOST", "ghost.go"})

	if err := os.Remove(filepath.Join(repo, "gone.go")); err != nil {
		t.Fatal(err)
	}
	gitRunIn(t, repo, "add", "-A")
	gitRunIn(t, repo, "commit", "-m", "remove gone.go")
	reindexGit(t, ctx, paths, plan)
	c.dead = append(c.dead, plMarker{"PL_GONE", "gone.go"})

	if got := committedShardCount(t, plan.indexDir); got < 4 {
		t.Fatalf("fixture must be multi-shard for partial loss to mean anything, got %d shards", got)
	}
	c.assertHealthy(t, ctx, "fixture baseline")

	c.golden = filepath.Join(t.TempDir(), "golden")
	plCopyTree(t, plan.cacheDir, c.golden)
	c.head = gitOutputIn(t, repo, "rev-parse", "HEAD")
	return c
}

// assertRepoUntouched verifies that a shared-fixture row did not change the
// repository. restorePristine restores only the cache. Rows that change the
// repository must set ownFixture.
func (c *plCorpus) assertRepoUntouched(t *testing.T) {
	t.Helper()
	if got := gitOutputIn(t, c.repoDir, "rev-parse", "HEAD"); got != c.head {
		t.Fatalf("row moved the shared fixture HEAD (%s -> %s); mark it ownFixture",
			c.head, got)
	}
	if dirty := gitOutputIn(t, c.repoDir, "status", "--porcelain"); dirty != "" {
		t.Fatalf("row left the shared fixture worktree dirty:\n%s\nmark it ownFixture", dirty)
	}
}

// restorePristine puts the corpus back in its post-build state so each row
// starts from the same family.
func (c *plCorpus) restorePristine(t *testing.T) {
	t.Helper()
	plChmodTreeWritable(c.plan.indexDir)
	if err := os.RemoveAll(c.plan.cacheDir); err != nil {
		t.Fatalf("clear cache dir: %v", err)
	}
	plCopyTree(t, c.golden, c.plan.cacheDir)
}

// ---------------------------------------------------------------------------
// The assertion
// ---------------------------------------------------------------------------

// seekOnce runs the freshness check, any required rebuild, and the search.
func (c *plCorpus) seekOnce(ctx context.Context, token string) ([]string, error) {
	return runSeekInPlannedGitCorpus(ctx, token, c.paths, c.plan)
}

// searchOnly reads the published family without another freshness check.
func (c *plCorpus) searchOnly(ctx context.Context, token string) ([]string, error) {
	matches, err := searchPlannedCorpusForTest(ctx, c.plan, token)
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(matches))
	for _, m := range matches {
		names = append(names, m.FileName)
	}
	return names, nil
}

// assertHealthy runs one complete search, then checks all markers against the
// shard family that the search published.
func (c *plCorpus) assertHealthy(t *testing.T, ctx context.Context, label string) {
	t.Helper()
	check := func(kind string, m plMarker, files []string, err error) {
		t.Helper()
		if err != nil {
			t.Errorf("%s: %s marker %s: search failed: %v", label, kind, m.token, err)
			return
		}
		if kind == "dead" {
			if len(files) != 0 {
				t.Errorf("%s: tombstoned marker %s resurrected in %v — content "+
					"that exists nowhere in the worktree or in HEAD is being served",
					label, m.token, files)
			}
			return
		}
		// The hit set must be the one file that carries the marker. A duplicate
		// document shows that tombstoning stopped at a shard gap.
		if got := plUnique(files); len(got) != 1 || got[0] != m.file {
			t.Errorf("%s: live marker %s: want exactly [%s], got %v (raw %v)",
				label, m.token, m.file, got, files)
		}
	}

	probe := c.live[0]
	files, err := c.seekOnce(ctx, probe.token)
	check("live", probe, files, err)
	for _, m := range c.live[1:] {
		got, sErr := c.searchOnly(ctx, m.token)
		check("live", m, got, sErr)
	}
	for _, m := range c.dead {
		got, sErr := c.searchOnly(ctx, m.token)
		check("dead", m, got, sErr)
	}
}

// assertConvergesAndIsStable checks that the first search repairs the family
// and that two later searches do not change it.
func (c *plCorpus) assertConvergesAndIsStable(t *testing.T, ctx context.Context) {
	t.Helper()
	c.assertHealthy(t, ctx, "run 1 (must converge in one search)")
	if t.Failed() {
		return
	}
	after1 := plFamilySnapshot(t, c.plan.indexDir)
	c.assertHealthy(t, ctx, "run 2")
	c.assertHealthy(t, ctx, "run 3")
	if after2 := plFamilySnapshot(t, c.plan.indexDir); after2 != after1 {
		t.Errorf("family kept changing after it converged (rebuild loop?):\n run1 %s\n run3 %s",
			after1, after2)
	}
}

// ---------------------------------------------------------------------------
// Topology helpers
// ---------------------------------------------------------------------------

func committedShardPaths(t *testing.T, indexDir string) []string {
	t.Helper()
	return plFamilyGlob(t, indexDir, "*.zoekt")
}

func committedMetaPaths(t *testing.T, indexDir string) []string {
	t.Helper()
	return plFamilyGlob(t, indexDir, "*.zoekt.meta")
}

func plFamilyGlob(t *testing.T, indexDir, pattern string) []string {
	t.Helper()
	all, err := filepath.Glob(filepath.Join(indexDir, pattern))
	if err != nil {
		t.Fatalf("glob %s: %v", pattern, err)
	}
	out := make([]string, 0, len(all))
	for _, p := range all {
		if !isUncommittedShard(filepath.Base(p)) {
			out = append(out, p)
		}
	}
	sort.Strings(out)
	return out
}

// shardCarrying returns the committed shard whose bytes contain token. Shard
// numbering is a zoekt implementation detail, so rows address shards by the
// marker they hold: "the shard that holds PL_MARK_1", never "shard 4".
func shardCarrying(t *testing.T, indexDir, token string) string {
	t.Helper()
	for _, p := range committedShardPaths(t, indexDir) {
		b, err := os.ReadFile(p)
		if err != nil {
			t.Fatalf("read shard %s: %v", p, err)
		}
		if bytes.Contains(b, []byte(token)) {
			return p
		}
	}
	t.Fatalf("no committed shard carries %q", token)
	return ""
}

// plFamilySnapshot renders the family as a stable "name=size" list, used to
// prove the build stopped moving once it converged.
func plFamilySnapshot(t *testing.T, indexDir string) string {
	t.Helper()
	var parts []string
	for _, p := range append(committedShardPaths(t, indexDir), committedMetaPaths(t, indexDir)...) {
		st, err := os.Stat(p)
		if err != nil {
			parts = append(parts, filepath.Base(p)+"=?")
			continue
		}
		parts = append(parts, fmt.Sprintf("%s=%d", filepath.Base(p), st.Size()))
	}
	sort.Strings(parts)
	return strings.Join(parts, " ")
}

func plUnique(in []string) []string {
	seen := map[string]struct{}{}
	out := make([]string, 0, len(in))
	for _, s := range in {
		if _, ok := seen[s]; ok {
			continue
		}
		seen[s] = struct{}{}
		out = append(out, s)
	}
	sort.Strings(out)
	return out
}

func plCopyTree(t *testing.T, src, dst string) {
	t.Helper()
	err := filepath.Walk(src, func(p string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, rerr := filepath.Rel(src, p)
		if rerr != nil {
			return rerr
		}
		target := filepath.Join(dst, rel)
		if info.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return nil
		}
		b, rerr := os.ReadFile(p)
		if rerr != nil {
			return rerr
		}
		return os.WriteFile(target, b, info.Mode().Perm())
	})
	if err != nil {
		t.Fatalf("copy tree %s -> %s: %v", src, dst, err)
	}
}

// plChmodTreeWritable undoes a chmod-000 row so the restore can delete it.
func plChmodTreeWritable(dir string) {
	_ = filepath.Walk(dir, func(p string, info os.FileInfo, err error) error {
		if err != nil {
			return nil
		}
		if info.IsDir() {
			_ = os.Chmod(p, 0o755)
			return nil
		}
		_ = os.Chmod(p, 0o644)
		return nil
	})
}

func plRemove(t *testing.T, paths ...string) {
	t.Helper()
	for _, p := range paths {
		if err := os.Remove(p); err != nil {
			t.Fatalf("remove %s: %v", p, err)
		}
	}
}

// ---------------------------------------------------------------------------
// The matrix
// ---------------------------------------------------------------------------

// plRow is one damage mode.
type plRow struct {
	// name is the subtest name; keep it -run selectable.
	name string
	// ownFixture marks a row that mutates the repository (a commit or an
	// untracked file). restorePristine rewinds only the cache, so such a row
	// gets a private repository and cache instead of the shared fixture.
	ownFixture bool
	// slow marks a row that builds a second full family; skipped under -short.
	slow bool
	// damage mutates the corpus on disk between two seek invocations, exactly
	// as an external process would.
	damage func(t *testing.T, ctx context.Context, c *plCorpus)
}

func partialLossRows() []plRow {
	return []plRow{
		{
			name: "shard_zero_deleted",
			damage: func(t *testing.T, _ context.Context, c *plCorpus) {
				plRemove(t, shardCarrying(t, c.plan.indexDir, "PL_SEED"))
			},
		},
		{
			name: "middle_shard_deleted",
			damage: func(t *testing.T, _ context.Context, c *plCorpus) {
				plRemove(t, shardCarrying(t, c.plan.indexDir, "PL_MARK_0"))
			},
		},
		{
			name: "trailing_shard_deleted",
			damage: func(t *testing.T, _ context.Context, c *plCorpus) {
				plRemove(t, shardCarrying(t, c.plan.indexDir, "PL_ALIVE"))
			},
		},
		{
			name: "incident_shape_all_but_shard_zero",
			damage: func(t *testing.T, _ context.Context, c *plCorpus) {
				keep := shardCarrying(t, c.plan.indexDir, "PL_SEED")
				for _, p := range committedShardPaths(t, c.plan.indexDir) {
					if p != keep {
						plRemove(t, p)
					}
				}
			},
		},
		{
			name: "every_committed_shard_deleted",
			damage: func(t *testing.T, _ context.Context, c *plCorpus) {
				plRemove(t, committedShardPaths(t, c.plan.indexDir)...)
			},
		},
		{
			name: "index_dir_removed",
			damage: func(t *testing.T, _ context.Context, c *plCorpus) {
				if err := os.RemoveAll(c.plan.indexDir); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "index_dir_replaced_by_file",
			damage: func(t *testing.T, _ context.Context, c *plCorpus) {
				if err := os.RemoveAll(c.plan.indexDir); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(c.plan.indexDir, []byte("not a directory\n"), 0o644); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "shard_truncated",
			damage: func(t *testing.T, _ context.Context, c *plCorpus) {
				p := shardCarrying(t, c.plan.indexDir, "PL_MARK_1")
				st, err := os.Stat(p)
				if err != nil {
					t.Fatal(err)
				}
				if err := os.Truncate(p, st.Size()*6/10); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "shard_unreadable",
			damage: func(t *testing.T, _ context.Context, c *plCorpus) {
				if err := os.Chmod(shardCarrying(t, c.plan.indexDir, "PL_MARK_1"), 0o000); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "shard_replaced_by_directory",
			damage: func(t *testing.T, _ context.Context, c *plCorpus) {
				p := shardCarrying(t, c.plan.indexDir, "PL_MARK_1")
				plRemove(t, p)
				if err := os.MkdirAll(p, 0o755); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "shard_symlinked_to_devnull",
			damage: func(t *testing.T, _ context.Context, c *plCorpus) {
				p := shardCarrying(t, c.plan.indexDir, "PL_MARK_1")
				plRemove(t, p)
				if err := os.Symlink(os.DevNull, p); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			// A shard replaced by a link to another shard can return only a
			// subset of the documents. The manifest must reject the replacement.
			name: "shard_symlinked_to_sibling",
			damage: func(t *testing.T, _ context.Context, c *plCorpus) {
				victim := shardCarrying(t, c.plan.indexDir, "PL_MARK_1")
				donor := shardCarrying(t, c.plan.indexDir, "PL_SEED")
				plRemove(t, victim)
				if err := os.Symlink(donor, victim); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "meta_sidecar_deleted",
			damage: func(t *testing.T, _ context.Context, c *plCorpus) {
				// The tombstone for PL_GHOST lives in the .meta of the shard
				// that still carries the superseded document.
				plRemove(t, shardCarrying(t, c.plan.indexDir, "PL_GHOST")+".meta")
			},
		},
		{
			name: "every_meta_sidecar_deleted",
			damage: func(t *testing.T, _ context.Context, c *plCorpus) {
				plRemove(t, committedMetaPaths(t, c.plan.indexDir)...)
			},
		},
		{
			name: "orphan_meta_shard_deleted",
			damage: func(t *testing.T, _ context.Context, c *plCorpus) {
				plRemove(t, shardCarrying(t, c.plan.indexDir, "PL_MARK_1"))
			},
		},
		{
			name: "random_subset_deleted",
			damage: func(t *testing.T, _ context.Context, c *plCorpus) {
				shards := committedShardPaths(t, c.plan.indexDir)
				for i, p := range shards {
					if i%2 == 0 {
						plRemove(t, p)
					}
				}
			},
		},
		{
			// Add a commit after a shard gap. The rebuild must not publish the
			// damaged family or return superseded content.
			name:       "gap_then_commit_serves_stale_content",
			ownFixture: true,
			slow:       true,
			damage: func(t *testing.T, ctx context.Context, c *plCorpus) {
				plRemove(t, shardCarrying(t, c.plan.indexDir, "PL_SEED"))
				// A commit that supersedes a live marker. After the rebuild
				// PL_MARK_2 must be gone and PL_SUPERSEDER must be the only
				// hit. The new token deliberately shares no substring with the
				// old one, so a hit on the old token can only be stale content.
				writeTrackedFile(t, c.repoDir, "f2.go", "package main\n// PL_SUPERSEDER\n")
				c.dead = append(c.dead, plMarker{"PL_MARK_2", "f2.go"})
				for i := range c.live {
					if c.live[i].token == "PL_MARK_2" {
						c.live[i] = plMarker{"PL_SUPERSEDER", "f2.go"}
					}
				}
			},
		},
		{
			// An uncommitted shard must not make a missing committed family look
			// complete.
			name:       "committed_family_lost_with_dirty_file_present",
			ownFixture: true,
			damage: func(t *testing.T, ctx context.Context, c *plCorpus) {
				if err := os.WriteFile(filepath.Join(c.repoDir, "dirty.go"),
					[]byte("package main\n// PL_DIRTY\n"), 0o644); err != nil {
					t.Fatal(err)
				}
				if files, err := c.seekOnce(ctx, "PL_DIRTY"); err != nil || len(files) == 0 {
					t.Fatalf("dirty file must be indexed before the damage: %v %v", files, err)
				}
				c.live = append(c.live, plMarker{"PL_DIRTY", "dirty.go"})
				plRemove(t, committedShardPaths(t, c.plan.indexDir)...)
				plRemove(t, committedMetaPaths(t, c.plan.indexDir)...)
			},
		},
		{
			// A manifest must not keep a missing uncommitted family current.
			name:       "uncommitted_shard_lost_manifest_kept",
			ownFixture: true,
			damage: func(t *testing.T, ctx context.Context, c *plCorpus) {
				if err := os.WriteFile(filepath.Join(c.repoDir, "dirty.go"),
					[]byte("package main\n// PL_DIRTY\n"), 0o644); err != nil {
					t.Fatal(err)
				}
				if files, err := c.seekOnce(ctx, "PL_DIRTY"); err != nil || len(files) == 0 {
					t.Fatalf("dirty file must be indexed before the damage: %v %v", files, err)
				}
				c.live = append(c.live, plMarker{"PL_DIRTY", "dirty.go"})
				un, err := filepath.Glob(filepath.Join(c.plan.indexDir, repoUncommitted+"_v*"))
				if err != nil || len(un) == 0 {
					t.Fatalf("expected an uncommitted family: %v %v", un, err)
				}
				plRemove(t, un...)
			},
		},
		{
			// Keep the existing state-file recovery path covered.
			name: "sidecars_cleared_only",
			damage: func(t *testing.T, _ context.Context, c *plCorpus) {
				_ = os.Remove(filepath.Join(c.plan.cacheDir, stateFile))
				_ = os.Remove(filepath.Join(c.plan.cacheDir, headFile))
			},
		},
		{
			name: "swapping_marker_present",
			damage: func(t *testing.T, _ context.Context, c *plCorpus) {
				// Add the marker that makes recoverIncompleteSwap clear the
				// incomplete family before the rebuild.
				keep := shardCarrying(t, c.plan.indexDir, "PL_SEED")
				for _, p := range committedShardPaths(t, c.plan.indexDir) {
					if p != keep {
						plRemove(t, p)
					}
				}
				if err := os.WriteFile(filepath.Join(c.plan.cacheDir, swappingMarkerFile),
					[]byte(familyCommitted.label()), 0o644); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "extra_out_of_range_shard",
			damage: func(t *testing.T, _ context.Context, c *plCorpus) {
				donor := shardCarrying(t, c.plan.indexDir, "PL_SEED")
				b, err := os.ReadFile(donor)
				if err != nil {
					t.Fatal(err)
				}
				name := strings.Replace(filepath.Base(donor), ".00000.", ".00042.", 1)
				if err := os.WriteFile(filepath.Join(c.plan.indexDir, name), b, 0o644); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "state_and_head_garbage",
			damage: func(t *testing.T, _ context.Context, c *plCorpus) {
				for _, f := range []string{stateFile, headFile} {
					if err := os.WriteFile(filepath.Join(c.plan.cacheDir, f),
						[]byte("\x00\x01garbage\n"), 0o644); err != nil {
						t.Fatal(err)
					}
				}
			},
		},
	}
}

// plPrepareRow restores the shared cache for a cache-only row. It creates a
// private repository and cache for a row that changes repository data. It also
// returns a post-run check.
func plPrepareRow(t *testing.T, ctx context.Context, shared *plCorpus, row plRow) (*plCorpus, func()) {
	t.Helper()
	if row.ownFixture {
		return buildPartialLossCorpus(t, ctx), func() {}
	}
	local := *shared
	local.live = append([]plMarker(nil), shared.live...)
	local.dead = append([]plMarker(nil), shared.dead...)
	local.restorePristine(t)
	return &local, func() { shared.assertRepoUntouched(t) }
}

// TestPartialLoss_GitCommittedFamily is the matrix. One shared fixture, one
// pristine restore per row, one row per damage mode.
func TestPartialLoss_GitCommittedFamily(t *testing.T) {
	ctx := context.Background()
	shared := buildPartialLossCorpus(t, ctx)

	for _, row := range partialLossRows() {
		t.Run(row.name, func(t *testing.T) {
			if row.slow && testing.Short() {
				t.Skip("slow row: builds a second family")
			}
			c, guard := plPrepareRow(t, ctx, shared, row)
			defer guard()
			row.damage(t, ctx, c)
			c.assertConvergesAndIsStable(t, ctx)
		})
	}
}

// ---------------------------------------------------------------------------
// Folder corpus parity
// ---------------------------------------------------------------------------

// TestPartialLoss_FolderFamily applies the same check to a folder corpus.
// Folder shards rotate on indexWindowBytes rather than commits. The test holds
// testReadSemMu while it changes and restores that package state.
func TestPartialLoss_FolderFamily(t *testing.T) {
	requireTools(t)
	ctx := context.Background()

	testReadSemMu.Lock()
	restoreWindow := swapIndexWindowBytesForTest(4 * 1024)
	defer func() {
		restoreWindow()
		testReadSemMu.Unlock()
	}()

	root := t.TempDir()
	var markers []plMarker
	for i := range 12 {
		name := fmt.Sprintf("n%02d.txt", i)
		token := fmt.Sprintf("PLF_MARK_%02d", i)
		body := "// " + token + "\n" + strings.Repeat("filler line for bulk\n", 60)
		if err := os.WriteFile(filepath.Join(root, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		markers = append(markers, plMarker{token, name})
	}
	plan := planFolderTestCorpus(t, root)

	assertAll := func(label string) {
		t.Helper()
		// One refresh — the healing invocation — then read the family it
		// published, exactly as the git matrix does.
		if st, err := ensureFolderCorpusFresh(ctx, plan); err != nil || st != corpusSearchable {
			t.Errorf("%s: folder refresh: st=%v err=%v", label, st, err)
			return
		}
		for _, m := range markers {
			files, err := searchPlannedCorpusForTest(ctx, plan, m.token)
			if err != nil {
				t.Errorf("%s: %s: search: %v", label, m.token, err)
				continue
			}
			names := make([]string, 0, len(files))
			for _, f := range files {
				names = append(names, f.FileName)
			}
			if got := plUnique(names); len(got) != 1 || got[0] != m.file {
				t.Errorf("%s: %s: want exactly [%s], got %v", label, m.token, m.file, got)
			}
		}
	}
	assertAll("baseline")

	shards, err := filepath.Glob(filepath.Join(plan.indexDir, "*.zoekt"))
	if err != nil {
		t.Fatal(err)
	}
	if len(shards) < 2 {
		t.Fatalf("fixture must be multi-shard, got %d (indexWindowBytes swap failed?)", len(shards))
	}
	sort.Strings(shards)
	plRemove(t, shards[len(shards)-1])
	assertAll("run 1 (must converge in one search)")
	assertAll("run 2")
}

// ---------------------------------------------------------------------------
// Documented limit
// ---------------------------------------------------------------------------

// TestPartialLoss_ContentCorruptionIsNotDetected records one damage mode that
// the manifest does not detect: a byte flipped inside a shard's
// content with the header intact. Size, document count, and shard membership
// stay unchanged, so the publish manifest cannot find it. Content hashes would
// be needed to detect this case.
//
// This test records the current limit. If content hashes are added, move this
// case into partialLossRows.
func TestPartialLoss_ContentCorruptionIsNotDetected(t *testing.T) {
	ctx := context.Background()
	c := buildPartialLossCorpus(t, ctx)

	const token = "PL_MARK_1"
	shard := shardCarrying(t, c.plan.indexDir, token)
	before, err := os.Stat(shard)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(shard)
	if err != nil {
		t.Fatal(err)
	}
	at := bytes.Index(raw, []byte(token))
	if at < 0 {
		t.Fatalf("marker %q not present verbatim in %s", token, shard)
	}
	raw[at+5] = 'Z' // PL_MA|R|K_1 -> PL_MAZK_1, one byte, same length
	if err := os.WriteFile(shard, raw, 0o644); err != nil {
		t.Fatal(err)
	}
	after, err := os.Stat(shard)
	if err != nil {
		t.Fatal(err)
	}
	if before.Size() != after.Size() {
		t.Fatalf("the flip must preserve size, %d -> %d", before.Size(), after.Size())
	}

	files, err := c.seekOnce(ctx, token)
	if err != nil {
		t.Fatalf("search after byte flip: %v", err)
	}
	if len(files) != 0 {
		t.Fatalf("behavior changed: the flipped marker is findable again (%v). "+
			"If per-shard content hashes landed, move this row into "+
			"partialLossRows and delete this test.", files)
	}
	// Documented limit: a live document silently disappeared, exit 0, no
	// warning, stable across searches.
	if files, err := c.seekOnce(ctx, token); err != nil || len(files) != 0 {
		t.Fatalf("behavior changed on the second search: files=%v err=%v", files, err)
	}
}

// ---------------------------------------------------------------------------
// Process level: the router contract
// ---------------------------------------------------------------------------

// plSoleCorpusIndexDir finds the one populated corpus under an isolated cache
// root. Used only by the process-level row, which cannot share the in-process
// corpus plan.
func plSoleCorpusIndexDir(t *testing.T, cacheRoot string) string {
	t.Helper()
	entries, err := filepath.Glob(filepath.Join(cacheRoot, corporaDir, "*", "index"))
	if err != nil {
		t.Fatal(err)
	}
	var populated []string
	for _, dir := range entries {
		if s, _ := filepath.Glob(filepath.Join(dir, "*.zoekt")); len(s) > 0 {
			populated = append(populated, dir)
		}
	}
	if len(populated) != 1 {
		t.Fatalf("want exactly one populated corpus under %s, got %v", cacheRoot, populated)
	}
	return populated[0]
}

// TestPartialLoss_RouterContractOnRealProcess checks the process-level result:
// exit 1, empty stderr, no match, and the same result on the next run.
//
// runCLIProcessWithCache starts the test binary as the CLI with an isolated
// SEEK_CACHE_DIR. This lets the test check the exit code and output streams.
//
// The CLI also calls runOpportunisticGC after the search. The isolated cache
// and .last-gc throttle keep that call inside the test fixture.
func TestPartialLoss_RouterContractOnRealProcess(t *testing.T) {

	requireTools(t)
	cacheDir := t.TempDir()
	repo := initGitRepo(t, "app.go", "package main\n// PL_CLI_BASE\n")

	if r := runCLIProcessWithCache(t, cacheDir, repo, []string{"PL_CLI_BASE"}, nil); r.code != 0 {
		t.Fatalf("cold run: code=%d stderr=%q", r.code, r.stderr)
	}
	// Two commits, each published by its own CLI run, so the family stacks.
	for i := range 2 {
		writeTrackedFile(t, repo, fmt.Sprintf("c%d.go", i), "package main\n// PL_CLI_STACK\n")
		if r := runCLIProcessWithCache(t, cacheDir, repo, []string{"PL_CLI_STACK"}, nil); r.code != 0 {
			t.Fatalf("stack run %d: code=%d stderr=%q", i, r.code, r.stderr)
		}
	}

	indexDir := plSoleCorpusIndexDir(t, cacheDir)
	shards := committedShardPaths(t, indexDir)
	if len(shards) < 3 {
		t.Fatalf("fixture must be multi-shard, got %d", len(shards))
	}
	plRemove(t, shards[1:]...) // keep only the base shard

	for _, run := range []string{"run 1 (must converge)", "run 2 (must stay converged)"} {
		r := runCLIProcessWithCache(t, cacheDir, repo, []string{"PL_CLI_STACK"}, nil)
		if r.code != 0 {
			t.Fatalf("%s: content that exists in the worktree returned exit %d "+
				"with stderr %q — the router reads this as \"this code does not "+
				"exist\"", run, r.code, r.stderr)
		}
		if !strings.Contains(r.stdout, "PL_CLI_STACK") {
			t.Fatalf("%s: stdout must carry the restored match, got %q", run, r.stdout)
		}
		if strings.Contains(r.stdout, "Index was incomplete") {
			t.Fatalf("%s: the repair notice must never reach stdout — the router "+
				"pipes it, stdout=%q", run, r.stdout)
		}
	}
}
