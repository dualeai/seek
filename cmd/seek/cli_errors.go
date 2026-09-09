package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"path/filepath"
	"strings"
	"sync/atomic"
)

// errNoMatch means that the query ran successfully but found no results.
// Search commands use exit code 1 for this normal outcome and print no error.
var errNoMatch = errors.New("no match")

func exitCodeForError(err error) int {
	switch {
	case err == nil:
		return 0
	case errors.Is(err, errNoMatch):
		return 1
	default:
		return 2
	}
}

// reportCLIError writes one normal error and, in verbose mode, its wrapped
// details.
func reportCLIError(w io.Writer, err error, verbose bool) {
	if err == nil || errors.Is(err, errNoMatch) {
		return
	}
	_, _ = fmt.Fprintln(w, formatCLIError(err))
	if verbose {
		newCLILogger(w, true).Error("Command failed", "error", err)
	}
}

func newCLILogger(w io.Writer, verbose bool) *slog.Logger {
	level := slog.LevelWarn
	if verbose {
		level = slog.LevelDebug
	}
	h := slog.NewTextHandler(w, &slog.HandlerOptions{Level: level})
	return slog.New(zoektWarningHandler{Handler: h, promoted: new(atomic.Int64)})
}

// zoektWarningHandler promotes Zoekt index-damage notices from Info to Warn.
// Other Info records stay hidden unless --verbose is active.
type zoektWarningHandler struct {
	slog.Handler
	promoted *atomic.Int64
}

// maxPromotedDamageNotices caps promoted lines. Zoekt can write one notice for
// each affected file.
const maxPromotedDamageNotices = 20

func (h zoektWarningHandler) Enabled(ctx context.Context, level slog.Level) bool {
	if level == slog.LevelInfo {
		return true // decided per record in Handle
	}
	return h.Handler.Enabled(ctx, level)
}

func (h zoektWarningHandler) Handle(ctx context.Context, r slog.Record) error {
	// Filter only when the underlying handler would drop the record. With
	// --verbose, all Info records keep their original level.
	if r.Level == slog.LevelInfo && !h.Handler.Enabled(ctx, slog.LevelInfo) {
		if !isZoektDamageNotice(r.Message) {
			return nil
		}
		if h.promoted.Add(1) > maxPromotedDamageNotices {
			// Report at most one notice per affected file up to the limit, then
			// report one summary.
			if h.promoted.Load() == maxPromotedDamageNotices+1 {
				sum := slog.NewRecord(r.Time, slog.LevelWarn,
					"Further index-damage notices suppressed; the index is incomplete", 0)
				return h.Handler.Handle(ctx, sum)
			}
			return nil
		}
		r.Level = slog.LevelWarn
	}
	return h.Handler.Handle(ctx, r)
}

// isZoektDamageNotice identifies the Zoekt warnings that can mean an incomplete
// index. This includes its warning prefixes and its cat-file fallback notice.
func isZoektDamageNotice(msg string) bool {
	for _, prefix := range zoektDamagePrefixes {
		if strings.HasPrefix(msg, prefix) {
			return true
		}
	}
	return false
}

// zoektDamagePrefixes identify messages that can mean an incomplete index. The
// cat-file fallback matters because it removes the missing-object signal.
var zoektDamagePrefixes = []string{
	"warning: blob ",
	"warn: ",
	"WARN: ",
	"WARN ",
	"git cat-file does not support",
}

func (h zoektWarningHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return zoektWarningHandler{Handler: h.Handler.WithAttrs(attrs), promoted: h.promoted}
}

func (h zoektWarningHandler) WithGroup(name string) slog.Handler {
	return zoektWarningHandler{Handler: h.Handler.WithGroup(name), promoted: h.promoted}
}

func formatCLIError(err error) string {
	if objErr, ok := errors.AsType[*unreadableObjectsError](err); ok {
		if objErr.alternate != "" {
			return fmt.Sprintf(
				"seek: cannot index Git repository %q: its objects live in the alternate "+
					"%q, which seek's Git library cannot reach\n"+
					"hint: copy them into this repository, then drop the alternate:\n"+
					"        git -C %s repack -a -d\n"+
					"        rm %s",
				objErr.repoDir, objErr.alternate, objErr.repoDir,
				filepath.Join(objErr.repoDir, ".git", "objects", "info", "alternates"))
		}
		return fmt.Sprintf(
			"seek: cannot index Git repository %q: its objects live in %q, a pack "+
				"seek's Git library cannot discover\n"+
				"hint: repack them under a discoverable name:\n"+
				"        git -C %s repack -a -d",
			objErr.repoDir, objErr.pack, objErr.repoDir)
	}
	if queryErr, ok := errors.AsType[*querySyntaxError](err); ok {
		return fmt.Sprintf("seek: invalid query %q: %v", queryErr.query, queryErr.cause)
	}
	if folderErr, ok := errors.AsType[*folderCorpusContextError](err); ok {
		if capErr, ok := errors.AsType[indexCapExceededError](err); ok {
			if message, ok := formatCorpusCapError(folderErr.root, false, capErr); ok {
				return message
			}
		}
	}
	if gitErr, ok := errors.AsType[*gitCorpusContextError](err); ok {
		if capErr, ok := errors.AsType[indexCapExceededError](err); ok {
			if message, ok := formatCorpusCapError(gitErr.root, true, capErr); ok {
				return message
			}
		}
	}
	if pathErr, ok := errors.AsType[*pathOperandError](err); ok {
		return formatPathOperandError(pathErr)
	}
	if _, ok := errors.AsType[*gitUnavailableError](err); ok {
		return "seek: Git is required but was not found\n" +
			"hint: install Git and ensure git is on PATH"
	}
	if _, ok := errors.AsType[*searchRootError](err); ok {
		return "seek: cannot determine a default Git search root\n" +
			"hint: run seek in a Git worktree or pass a path after the query, for example: seek 'query' ."
	}
	if ctagsErr, ok := errors.AsType[*ctagsUnavailableError](err); ok {
		problem, hint := ctagsUserMessage(ctagsErr)
		return "seek: " + problem + "\nhint: " + hint
	}
	return "seek: " + err.Error()
}

func formatCorpusCapError(root string, git bool, capErr indexCapExceededError) (string, bool) {
	target := fmt.Sprintf("%q", root)
	indexedSubject := "indexable files"
	candidateSubject := "files"
	limitScope := "folder"
	if git {
		target = fmt.Sprintf("Git repository %q", root)
		family := "working-tree"
		if errors.Is(capErr, errGitCommittedCapExceeded) {
			family = "committed-tree"
		}
		indexedSubject = family + " indexable files"
		candidateSubject = family + " entries"
		limitScope = "Git index-family"
	}
	const hint = "hint: pass smaller paths after the query; file: and -file: filters apply after indexing"
	switch capErr.metric {
	case indexCapIndexedBytes:
		return fmt.Sprintf(
			"seek: cannot index %s: the total size of %s exceeds the %d GiB %s limit\n%s",
			target,
			indexedSubject,
			capErr.limit/(1024*1024*1024),
			limitScope,
			hint,
		), true
	case indexCapCandidateFiles:
		return fmt.Sprintf(
			"seek: cannot index %s: more than %d %s are candidates for indexing\n%s",
			target,
			capErr.limit,
			candidateSubject,
			hint,
		), true
	default:
		return "", false
	}
}

func ctagsUserMessage(err *ctagsUnavailableError) (problem, hint string) {
	if err.command != "" {
		return fmt.Sprintf("cannot use Universal Ctags command %q", err.command),
			"set CTAGS_COMMAND to an executable Universal Ctags path"
	}
	return "Universal Ctags is not available",
		"install Universal Ctags or set CTAGS_COMMAND=/path/to/ctags"
}

// reportStaleIndexWarning owns warnings for update failures when a prior index
// remains usable. Ctags discovery errors get an actionable normal message.
// Debug output keeps the full typed error and its corpus context.
func reportStaleIndexWarning(err error) {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		slog.Debug("Index update stopped after cancellation", "error", err)
		return
	}
	if ctagsErr, ok := errors.AsType[*ctagsUnavailableError](err); ok {
		problem, hint := ctagsUserMessage(ctagsErr)
		slog.Warn("Index update failed; using the existing index", "cause", problem, "hint", hint)
		slog.Debug("Index update failure detail", "error", err)
		return
	}
	slog.Warn("Index update failed; using the existing index", "hint", "run with --verbose for details")
	slog.Debug("Index update failure detail", "error", err)
}

func formatPathOperandError(err *pathOperandError) string {
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return fmt.Sprintf("seek: path does not exist: %q", err.operand)
	case errors.Is(err, fs.ErrPermission):
		return fmt.Sprintf("seek: cannot read path %q: permission denied", err.operand)
	default:
		return fmt.Sprintf("seek: cannot %s path %q: %v", err.operation, err.operand, err.cause)
	}
}
