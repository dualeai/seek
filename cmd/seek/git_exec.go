package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// gitHardeningArgs keep repository configuration from running programs. Seek
// runs Git in every repository below the search root, also in repositories
// that the user did not choose. So it trusts no repository setting that names
// a program. These are the fsmonitor hook, hooks, the signature verifier, the
// external diff driver, the ssh command, and ext:: transports. The pager and
// replace-object flags make output depend only on the arguments and the object
// database.
var gitHardeningArgs = []string{
	"-c", "core.fsmonitor=false",
	"-c", "core.hooksPath=/dev/null",
	"-c", "core.sshCommand=",
	"-c", "log.showSignature=false",
	"-c", "diff.external=",
	"-c", "protocol.ext.allow=never",
	"--no-pager",
	"--no-replace-objects",
}

// newGitCommand creates a hardened Git command that runs in repoDir, or in the
// current directory when repoDir is empty. Path arguments are literal names,
// never pathspec magic.
func newGitCommand(ctx context.Context, repoDir string, args ...string) *exec.Cmd {
	return hardenedGitCommand(ctx, repoDir, append([]string{"--literal-pathspecs"}, args...))
}

// newGitCheckIgnoreCommand creates a hardened git check-ignore command. Git
// rejects --literal-pathspecs for check-ignore, so callers pass "./" paths.
func newGitCheckIgnoreCommand(ctx context.Context, repoDir string, args ...string) *exec.Cmd {
	return hardenedGitCommand(ctx, repoDir, append([]string{"check-ignore"}, args...))
}

// hardenedGitCommand turns off the filter drivers that repoDir defines. When
// Seek cannot read the driver list, the command fails without running: a
// driver that stays on would run a program from the repository. In a partial
// clone, the command also fails when the installed Git ignores
// GIT_NO_LAZY_FETCH.
func hardenedGitCommand(ctx context.Context, repoDir string, args []string) *exec.Cmd {
	cmd := gitCommand(ctx, repoDir, args)
	config, err := readRepoGitConfig(ctx, repoDir)
	if err != nil {
		if cmd.Err == nil {
			cmd.Err = fmt.Errorf("read the filter drivers of %s: %w", repoDir, err)
		}
		return cmd
	}
	if config.partialClone {
		if err := requireNoLazyFetch(ctx); err != nil && cmd.Err == nil {
			cmd.Err = err
			return cmd
		}
	}
	cmd.Env = appendConfigEnv(cmd.Env, filterOverrides(config.filterDrivers))
	return cmd
}

// noLazyFetchMinPatch is the first maintenance release of each Git line before
// 2.45 that honors GIT_NO_LAZY_FETCH (the 2024 security releases).
var noLazyFetchMinPatch = map[int]int{39: 4, 40: 2, 41: 1, 42: 2, 43: 4, 44: 1}

// gitVersionNumbers reads the major, minor, and patch numbers of `git version`
// output, such as "git version 2.43.0.windows.1". Each number ends at its
// first character that is not a digit, and a missing number is 0. A patch
// field with no digit is also 0: a Git built from a source snapshot prints
// "git version 2.47.GIT". ok is false when the major or minor number has no
// digit.
func gitVersionNumbers(out []byte) (major, minor, patch int, ok bool) {
	fields := strings.SplitN(strings.TrimPrefix(strings.TrimSpace(string(out)), "git version "), ".", 4)
	var numbers [3]int
	for i := range min(len(fields), len(numbers)) {
		digits := fields[i]
		if end := strings.IndexFunc(digits, func(r rune) bool { return r < '0' || r > '9' }); end >= 0 {
			digits = digits[:end]
		}
		if digits == "" && i == 2 {
			break
		}
		n, err := strconv.Atoi(digits)
		if err != nil {
			return 0, 0, 0, false
		}
		numbers[i] = n
	}
	return numbers[0], numbers[1], numbers[2], true
}

// gitHonorsNoLazyFetch reports whether `git version` output names a Git that
// honors GIT_NO_LAZY_FETCH: 2.45 or later, or a maintenance release listed in
// noLazyFetchMinPatch.
func gitHonorsNoLazyFetch(out []byte) bool {
	major, minor, patch, ok := gitVersionNumbers(out)
	if !ok {
		return false
	}
	if major != 2 {
		return major > 2
	}
	if minor >= 45 {
		return true
	}
	minPatch, ok := noLazyFetchMinPatch[minor]
	return ok && patch >= minPatch
}

// gitVersions holds the `git version` output of each Git executable path that
// this command ran.
var gitVersions sync.Map

// requireNoLazyFetch fails a partial clone's Git commands when the installed
// Git ignores GIT_NO_LAZY_FETCH. Such a Git would fetch missing blobs from the
// network during a search.
func requireNoLazyFetch(ctx context.Context) error {
	out, err := gitVersion(ctx)
	if err != nil {
		return err
	}
	if gitHonorsNoLazyFetch(out) {
		return nil
	}
	return fmt.Errorf("a partial clone needs Git 2.45 or later (or 2.39.4, 2.40.2, 2.41.1, 2.42.2, 2.43.4, 2.44.1) so that a search never fetches; found %s",
		strings.TrimSpace(string(out)))
}

// gitVersion returns the `git version` output of the Git on PATH, once per
// executable path in a command.
func gitVersion(ctx context.Context) ([]byte, error) {
	gitPath, err := exec.LookPath("git")
	if err != nil {
		return nil, &gitUnavailableError{cause: err}
	}
	if out, cached := gitVersions.Load(gitPath); cached {
		return out.([]byte), nil
	}
	version, err := gitOutput(ctx, gitCommand(ctx, "", []string{"version"}), "git version")
	if err != nil {
		return nil, err
	}
	out, _ := gitVersions.LoadOrStore(gitPath, version)
	return out.([]byte), nil
}

// gitCommand creates a Git command with gitHardeningArgs and a sanitized
// environment; a missing executable gives a gitUnavailableError. It uses a
// graceful signal on cancellation so Git can release its locks. It does not
// set GIT_OPTIONAL_LOCKS=0 because Git must refresh its stat cache to detect
// some same-second edits.
func gitCommand(ctx context.Context, repoDir string, args []string) *exec.Cmd {
	argv := make([]string, 0, len(gitHardeningArgs)+len(args))
	argv = append(append(argv, gitHardeningArgs...), args...)
	cmd := exec.CommandContext(ctx, "git", argv...)
	if errors.Is(cmd.Err, exec.ErrNotFound) {
		cmd.Err = &gitUnavailableError{cause: cmd.Err}
	}
	cmd.Dir = repoDir
	cmd.Env = sanitizedGitEnv(os.Environ())
	cmd.Cancel = func() error {
		// SIGTERM lets Git release its index locks before it exits.
		return cmd.Process.Signal(syscall.SIGTERM)
	}
	cmd.WaitDelay = 3 * time.Second
	return cmd
}

// gitUnavailableError reports that Seek cannot find the Git executable.
type gitUnavailableError struct {
	cause error
}

func (e *gitUnavailableError) Error() string {
	return fmt.Sprintf("Git executable is unavailable: %v", e.cause)
}

func (e *gitUnavailableError) Unwrap() error {
	return e.cause
}

// sanitizedGitEnv removes variables that change pathspec rules, prompts,
// locale, or the target repository, then sets stable values. A search never
// reaches the network: GIT_NO_LAZY_FETCH stops a partial clone from fetching
// missing objects.
func sanitizedGitEnv(env []string) []string {
	filtered := env[:0]
	for _, kv := range env {
		name, _, _ := strings.Cut(kv, "=")
		switch name {
		case "GIT_LITERAL_PATHSPECS", "GIT_GLOB_PATHSPECS", "GIT_NOGLOB_PATHSPECS", "GIT_ICASE_PATHSPECS",
			"GIT_TERMINAL_PROMPT", "GIT_NO_LAZY_FETCH", "LC_ALL", "LANG":
			continue
		// Corpus identity comes from the filesystem. A location variable could
		// make Git describe a different repository from the indexed corpus.
		case "GIT_DIR", "GIT_WORK_TREE", "GIT_INDEX_FILE", "GIT_OBJECT_DIRECTORY", "GIT_COMMON_DIR":
			continue
		default:
			filtered = append(filtered, kv)
		}
	}
	filtered = append(filtered,
		"GIT_TERMINAL_PROMPT=0",
		"GIT_NO_LAZY_FETCH=1",
		"LC_ALL=C",
		"LANG=C",
	)
	return filtered
}

// gitOutput runs a buffered Git command and returns its stdout. A failure
// carries Git's stderr.
func gitOutput(ctx context.Context, cmd *exec.Cmd, operation string) ([]byte, error) {
	var stderr boundedGitStderr
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return out, gitCommandError(ctx, operation, err, &stderr)
	}
	return out, nil
}

// gitCommandError wraps a failed Git command as "operation: <error>: <stderr>".
// It keeps the cause for errors.Is and errors.As, and returns the context
// error when the command stopped because the search was canceled.
func gitCommandError(ctx context.Context, operation string, err error, stderr *boundedGitStderr) error {
	if ctxErr := ctx.Err(); ctxErr != nil {
		return ctxErr
	}
	if err == nil {
		return nil
	}
	message := stderr.String()
	if message == "" {
		return fmt.Errorf("%s: %w", operation, err)
	}
	return fmt.Errorf("%s: %w: %s", operation, err, message)
}

// repoGitConfig holds what Seek reads once from a repository's config.
type repoGitConfig struct {
	// filterDrivers are the filter drivers that the repository defines in its
	// own config (local or worktree scope, includes too). Seek turns them off.
	// Drivers from the global and system config, such as Git LFS, stay on: the
	// user installed them.
	filterDrivers []string
	// partialClone reports a promisor remote: missing blobs are expected and
	// are never fetched (GIT_NO_LAZY_FETCH). In other repositories a missing
	// blob is damage.
	partialClone bool
}

// repoConfigCache holds one repoGitConfig per repository directory. One search
// command is one process, so an entry lives for one command. A failed read is
// not cached.
var repoConfigCache sync.Map

// repoIsPartialClone reports a promisor remote. A config that Seek cannot read
// reports false: every Git command of that repository fails anyway.
func repoIsPartialClone(ctx context.Context, repoDir string) bool {
	config, err := readRepoGitConfig(ctx, repoDir)
	return err == nil && config.partialClone
}

// readRepoGitConfig reads the filter drivers and the partial-clone mark of
// repoDir, once per command.
func readRepoGitConfig(ctx context.Context, repoDir string) (repoGitConfig, error) {
	if cached, ok := repoConfigCache.Load(repoDir); ok {
		return cached.(repoGitConfig), nil
	}
	// Git config runs no driver, so this read needs no overrides.
	cmd := gitCommand(ctx, repoDir, []string{
		"config", "--show-scope", "--includes", "--null", "--get-regexp",
		`^(filter\..*\.(clean|smudge|process|required)|extensions\.partialclone|remote\..*\.promisor)$`,
	})
	out, err := gitOutput(ctx, cmd, "git config")
	// Exit 1 with no output means no key matched. A directory outside any
	// repository reads only the global and system config.
	if exitErr, ok := errors.AsType[*exec.ExitError](err); ok && exitErr.ExitCode() == 1 && len(out) == 0 {
		err = nil
	}
	if err != nil {
		return repoGitConfig{}, err
	}
	config := parseRepoGitConfig(out)
	repoConfigCache.Store(repoDir, config)
	return config, nil
}

// parseRepoGitConfig parses `git config --show-scope --null` records: scope
// NUL key LF value NUL, or scope NUL key NUL for a key with no value.
func parseRepoGitConfig(out []byte) repoGitConfig {
	fields := bytes.Split(out, []byte{0})
	var config repoGitConfig
	for i := 0; i+1 < len(fields); i += 2 {
		scope := string(fields[i])
		keyRaw, valueRaw, hasValue := bytes.Cut(fields[i+1], []byte{'\n'})
		key := string(keyRaw)
		switch {
		case key == "extensions.partialclone":
			config.partialClone = true
		case strings.HasPrefix(key, "remote.") && strings.HasSuffix(key, ".promisor"):
			if gitConfigBool(string(valueRaw), hasValue) {
				config.partialClone = true
			}
		case scope == "local" || scope == "worktree":
			if name, ok := filterDriverName(key); ok && !slices.Contains(config.filterDrivers, name) {
				config.filterDrivers = append(config.filterDrivers, name)
			}
		}
	}
	return config
}

// gitConfigBool reads a value the way Git reads a boolean. A key with no value
// is true, and so are true, yes, on, and a nonzero integer.
func gitConfigBool(value string, hasValue bool) bool {
	if !hasValue {
		return true
	}
	switch strings.ToLower(value) {
	case "true", "yes", "on":
		return true
	case "false", "no", "off", "":
		return false
	}
	n, err := strconv.Atoi(value)
	return err == nil && n != 0
}

// filterDriverName returns the driver name of a filter.<name>.<attribute> key.
// The name can be empty and can hold dots and "=".
func filterDriverName(key string) (string, bool) {
	rest, ok := strings.CutPrefix(key, "filter.")
	if !ok {
		return "", false
	}
	dot := strings.LastIndexByte(rest, '.')
	if dot < 0 {
		return "", false
	}
	return rest[:dot], true
}

// filterOverrides returns the config pairs that turn off each driver.
func filterOverrides(drivers []string) [][2]string {
	pairs := make([][2]string, 0, 4*len(drivers))
	for _, name := range drivers {
		prefix := "filter." + name + "."
		pairs = append(pairs,
			[2]string{prefix + "clean", ""},
			[2]string{prefix + "smudge", ""},
			[2]string{prefix + "process", ""},
			[2]string{prefix + "required", "false"},
		)
	}
	return pairs
}

// appendConfigEnv adds config pairs through GIT_CONFIG_COUNT. A "-c" argument
// cannot carry them: Git splits it at the first "=", and a driver name can
// hold "=". Pairs the caller's environment already sets keep their slots.
func appendConfigEnv(env []string, pairs [][2]string) []string {
	if len(pairs) == 0 {
		return env
	}
	count := 0
	countIndex := -1
	for i, kv := range env {
		if value, ok := strings.CutPrefix(kv, "GIT_CONFIG_COUNT="); ok {
			count, _ = strconv.Atoi(value)
			countIndex = i
		}
	}
	for i, pair := range pairs {
		n := strconv.Itoa(count + i)
		env = append(env, "GIT_CONFIG_KEY_"+n+"="+pair[0], "GIT_CONFIG_VALUE_"+n+"="+pair[1])
	}
	total := "GIT_CONFIG_COUNT=" + strconv.Itoa(count+len(pairs))
	if countIndex >= 0 {
		env[countIndex] = total
		return env
	}
	return append(env, total)
}
