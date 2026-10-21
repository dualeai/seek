package main

import "testing"

// TestDecideGitBuild checks the Git build's decisions under its lock, with no
// Git and no file system: what makes a node current, and what each change
// builds. It also checks which plans read the commit and which plans make a
// build directory.
func TestDecideGitBuild(t *testing.T) {
	// warm is a clean node whose index holds its commit.
	warm := gitBuildInputs{
		head: "c1", preState: "s1", cachedState: "s1",
		familyIntact: true, committedIntact: true, committedMember: true, committedReady: true,
		joinedReady: true, semanticOn: true, publishedHead: "c1",
	}
	for _, test := range []struct {
		name    string
		edit    func(*gitBuildInputs)
		current bool
		want    gitBuildPlan
	}{
		{name: "warm", edit: func(*gitBuildInputs) {}, current: true,
			want: gitBuildPlan{baseBound: true, overlayBound: true}},
		{name: "state changed, same commit", edit: func(in *gitBuildInputs) { in.cachedState = "s0" },
			want: gitBuildPlan{baseBound: true, overlayBound: true}},
		{name: "head moved", edit: func(in *gitBuildInputs) { in.head, in.preState = "c2", "s2" },
			want: gitBuildPlan{committed: true, dropJoined: true, baseBound: true, overlayBound: true}},
		{name: "committed family damaged", edit: func(in *gitBuildInputs) { in.familyIntact, in.committedIntact = false, false },
			want: gitBuildPlan{committed: true, dropJoined: true, baseBound: true, overlayBound: true}},
		{name: "lexical blob arrived", edit: func(in *gitBuildInputs) { in.arrival.lexical = true },
			want: gitBuildPlan{committed: true, dropJoined: true, baseBound: true, overlayBound: true}},
		{name: "committed snapshot not ready", edit: func(in *gitBuildInputs) { in.committedReady = false },
			want: gitBuildPlan{baseBound: true, overlayBound: true}},
		{name: "uncommitted family torn", edit: func(in *gitBuildInputs) { in.familyIntact = false },
			want: gitBuildPlan{baseBound: true, overlayBound: true}},
		{name: "overlay present", edit: func(in *gitBuildInputs) {
			in.dirty, in.overlay, in.overlayPresent, in.cachedState = true, true, true, "s0"
		}, want: gitBuildPlan{dropJoined: true, baseBound: true, overlayBound: true}},
		{name: "no head with committed shards", edit: func(in *gitBuildInputs) { in.head, in.publishedHead = "no-head", "" },
			want: gitBuildPlan{clearCommitted: true, dropJoined: true, baseBound: true, overlayBound: true}},
		{name: "vectors missing", edit: func(in *gitBuildInputs) { in.vectors, in.joinedReady = true, false },
			want: gitBuildPlan{semantic: true, overlayBound: true}},
		{name: "vectors present", edit: func(in *gitBuildInputs) { in.vectors, in.basePresent, in.cachedState = true, true, "s0" },
			want: gitBuildPlan{baseBound: true, overlayBound: true}},
		{name: "semantic blob arrived", edit: func(in *gitBuildInputs) { in.vectors, in.basePresent, in.arrival.semantic = true, true, true },
			want: gitBuildPlan{semantic: true, dropJoined: true, overlayBound: true}},
		{name: "overlay missing", edit: func(in *gitBuildInputs) { in.dirty, in.overlay, in.joinedReady = true, true, false },
			want: gitBuildPlan{overlay: true, dropJoined: true, baseBound: true}},
		{name: "dirty with no vector policy", edit: func(in *gitBuildInputs) { in.dirty, in.semanticOn, in.cachedState = true, false, "s0" },
			want: gitBuildPlan{baseBound: true, overlayBound: true}},
		{name: "no-head overlay only", edit: func(in *gitBuildInputs) {
			in.head, in.publishedHead, in.committedMember, in.committedReady = "no-head", "", false, true
			in.dirty, in.overlay, in.joinedReady = true, true, false
		}, want: gitBuildPlan{overlay: true, dropJoined: true, baseBound: true}},
	} {
		t.Run(test.name, func(t *testing.T) {
			in := warm
			test.edit(&in)
			if got := gitBuildCurrent(in); got != test.current {
				t.Fatalf("current=%t, want %t", got, test.current)
			}
			if got := decideGitBuild(in); got != test.want {
				t.Fatalf("plan=%+v, want %+v", got, test.want)
			}
		})
	}
	if (gitBuildPlan{overlay: true}).snapshot() || !(gitBuildPlan{semantic: true}).snapshot() {
		t.Fatal("only the committed family and the base generation read the commit")
	}
	if (gitBuildPlan{}).buildDir() || !(gitBuildPlan{clearCommitted: true}).buildDir() {
		t.Fatal("only a build that writes makes a build directory")
	}
}

// TestDecideFolderBuild checks the folder build's decisions with no file
// system: an unchanged folder builds nothing, a change takes a delta on a
// usable family, and a torn family rebuilds in full and loses .state on a
// failure. The vector generation builds only when it is missing.
func TestDecideFolderBuild(t *testing.T) {
	unchanged := folderBuildInputs{stateHash: "s1", currentState: "s1", hasShard: true, intact: true}
	for _, test := range []struct {
		name string
		edit func(*folderBuildInputs)
		want folderBuildPlan
	}{
		{name: "unchanged", edit: func(*folderBuildInputs) {}, want: folderBuildPlan{keepState: true}},
		{name: "changed with a usable family", edit: func(in *folderBuildInputs) { in.stateHash = "s2" },
			want: folderBuildPlan{lexical: true, delta: true, keepState: true}},
		{name: "torn family", edit: func(in *folderBuildInputs) { in.intact = false },
			want: folderBuildPlan{lexical: true, tornWarn: true}},
		{name: "first build", edit: func(in *folderBuildInputs) { in.currentState, in.hasShard, in.intact = "", false, false },
			want: folderBuildPlan{lexical: true}},
		{name: "state lost, family usable", edit: func(in *folderBuildInputs) { in.currentState = "" },
			want: folderBuildPlan{lexical: true, keepState: true}},
		{name: "empty manifest, no shard", edit: func(in *folderBuildInputs) { in.hasShard = false },
			want: folderBuildPlan{lexical: true}},
		{name: "vectors missing", edit: func(in *folderBuildInputs) { in.vectors = true },
			want: folderBuildPlan{semantic: true, keepState: true}},
		{name: "vectors present", edit: func(in *folderBuildInputs) { in.vectors, in.semanticPresent = true, true },
			want: folderBuildPlan{keepState: true}},
	} {
		t.Run(test.name, func(t *testing.T) {
			in := unchanged
			test.edit(&in)
			if got := decideFolderBuild(in); got != test.want {
				t.Fatalf("plan=%+v, want %+v", got, test.want)
			}
		})
	}
}
