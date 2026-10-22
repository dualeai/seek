package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
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
		if promoted := h.promoted.Add(1); promoted > maxPromotedDamageNotices {
			// Promote the first maxPromotedDamageNotices notices, then write
			// one summary and drop the rest.
			if promoted == maxPromotedDamageNotices+1 {
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

// isZoektDamageNotice identifies Zoekt warnings that can mean an incomplete
// index.
func isZoektDamageNotice(msg string) bool {
	for _, prefix := range zoektDamagePrefixes {
		if strings.HasPrefix(msg, prefix) {
			return true
		}
	}
	return false
}

// zoektDamagePrefixes identify messages that can mean an incomplete index.
var zoektDamagePrefixes = []string{
	"warning: blob ",
	"warn: ",
	"WARN: ",
	"WARN ",
}

func (h zoektWarningHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return zoektWarningHandler{Handler: h.Handler.WithAttrs(attrs), promoted: h.promoted}
}

func (h zoektWarningHandler) WithGroup(name string) slog.Handler {
	return zoektWarningHandler{Handler: h.Handler.WithGroup(name), promoted: h.promoted}
}

// formatCLIError returns the message that the CLI prints for err, with a hint
// line for the errors that have one.
func formatCLIError(err error) string {
	if budgetErr, ok := errors.AsType[*cacheBudgetError](err); ok {
		message := fmt.Sprintf("seek: this search needs about %s of index cache (%s new), more than the cache budget of %s\n",
			humanBytes(budgetErr.planned), humanBytes(budgetErr.growth), humanBytes(budgetErr.budget))
		// A text search builds no meaning-based index: name it when it fits.
		textOnly := ""
		if budgetErr.textFits {
			textOnly = "; or use --lexical-only, which builds no meaning-based index"
		}
		if budgetErr.freeBound {
			// Seek keeps 5 % of the volume free; the override replaces that
			// floor, so the hint names the disk first.
			return message + fmt.Sprintf("hint: the cache volume is nearly full and Seek keeps 5 %% of it free; "+
				"free disk space, or set %s (it replaces that floor)%s", envCacheBudget, textOnly)
		}
		return message + fmt.Sprintf("hint: set %s to a larger size, for example %s=%s%s",
			envCacheBudget, envCacheBudget, budgetSuggestion(budgetErr.planned), textOnly)
	}
	if queryErr, ok := errors.AsType[*querySyntaxError](err); ok {
		return fmt.Sprintf("seek: invalid query %q: %v", queryErr.query, queryErr.cause)
	}
	if pathErr, ok := errors.AsType[*pathOperandError](err); ok {
		return formatPathOperandError(pathErr)
	}
	if folderErr, ok := errors.AsType[*folderCorpusContextError](err); ok && errors.Is(err, fs.ErrNotExist) {
		return fmt.Sprintf("seek: path does not exist: %q", folderErr.root)
	}
	if _, ok := errors.AsType[*gitUnavailableError](err); ok {
		return "seek: Git is required but was not found\n" +
			"hint: install Git and ensure git is on PATH"
	}
	if ctagsErr, ok := errors.AsType[*ctagsUnavailableError](err); ok {
		problem, hint := ctagsUserMessage(ctagsErr)
		return "seek: " + problem + "\nhint: " + hint
	}
	return "seek: " + err.Error()
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
// remains usable. A cancellation writes only a debug line. Dirty files kept by
// name after a read error get one warning with their count, the first path,
// and the cause. Ctags discovery errors get an actionable normal message.
// Debug output keeps the full typed error and its corpus context.
func reportStaleIndexWarning(err error) {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		slog.Debug("Index update stopped after cancellation", "error", err)
		return
	}
	if kept, ok := errors.AsType[*dirtyFilesKept](err); ok && len(kept.files) > 0 {
		// The build published every other change. The first name holds the
		// path, so the cause leaves it out.
		cause := kept.cause
		if pathErr, ok := errors.AsType[*fs.PathError](cause); ok {
			cause = pathErr.Err
		}
		slog.Warn("Some uncommitted files could not be read; they are searchable by name only until a later search reads them",
			"files", len(kept.files), "first", kept.files[0], "cause", cause.Error())
		slog.Debug("Index update failure detail", "error", err)
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
	case errors.Is(err, errGitDirectoryOperand):
		return fmt.Sprintf("seek: cannot search a Git directory: %q", err.operand)
	case errors.Is(err, fs.ErrNotExist):
		return fmt.Sprintf("seek: path does not exist: %q", err.operand)
	case errors.Is(err, fs.ErrPermission):
		return fmt.Sprintf("seek: cannot read path %q: permission denied", err.operand)
	default:
		return fmt.Sprintf("seek: cannot %s path %q: %v", err.operation, err.operand, err.cause)
	}
}
