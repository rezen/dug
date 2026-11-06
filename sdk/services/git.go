package services

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"time"
)

// GitClientOptions configures Git subprocesses. Zero values use git from PATH
// and a five-minute deadline per operation. A negative timeout disables the
// service deadline; the caller's context still controls cancellation.
type GitClientOptions struct {
	Binary  string
	Timeout time.Duration
}

// GitClient provides repository operations using the Git executable. It can be
// reused concurrently for different repositories; callers must serialize writes
// to the same repository. Authentication uses the user's Git configuration.
type GitClient struct {
	binary  string
	timeout time.Duration
}

func NewDugGitClient(options GitClientOptions) *GitClient {
	if options.Binary == "" {
		options.Binary = "git"
	}
	if options.Timeout == 0 {
		options.Timeout = 5 * time.Minute
	}
	return &GitClient{binary: options.Binary, timeout: options.Timeout}
}

// Clone downloads a repository without checking out its working tree. Git
// refuses to overwrite a nonempty destination. Submodules are not fetched.
func (c *GitClient) Clone(ctx context.Context, remote, directory string) error {
	if remote == "" || directory == "" {
		return fmt.Errorf("git clone: remote and destination are required")
	}
	return c.run(ctx, "", io.Discard, "clone", "--no-checkout", "--", remote, directory)
}

// Fetch updates origin's remote-tracking branches and removes stale ones.
func (c *GitClient) Fetch(ctx context.Context, directory string) error {
	return c.run(ctx, directory, io.Discard, "fetch", "--prune", "origin")
}

// ResolveRevision resolves a branch, tag, or revision expression to a commit ID.
func (c *GitClient) ResolveRevision(ctx context.Context, directory, revision string) (string, error) {
	if revision == "" {
		return "", fmt.Errorf("git resolve: revision is required")
	}
	var output bytes.Buffer
	if err := c.run(ctx, directory, &output, "rev-parse", "--verify", "--end-of-options", revision+"^{commit}"); err != nil {
		return "", err
	}
	return strings.TrimSpace(output.String()), nil
}

// Archive writes a commit's tree as tar without buffering the archive in memory
// or changing the working tree. The writer can contain partial data on failure.
func (c *GitClient) Archive(ctx context.Context, directory, revision string, output io.Writer) error {
	if output == nil {
		return fmt.Errorf("git archive: output is required")
	}
	ctx, cancel := c.operationContext(ctx)
	defer cancel()
	commit, err := c.ResolveRevision(ctx, directory, revision)
	if err != nil {
		return err
	}
	return c.run(ctx, directory, output, "archive", "--format=tar", commit)
}

func (c *GitClient) operationContext(ctx context.Context) (context.Context, context.CancelFunc) {
	if c.timeout > 0 {
		return context.WithTimeout(ctx, c.timeout)
	}
	return context.WithCancel(ctx)
}

func (c *GitClient) run(ctx context.Context, directory string, output io.Writer, args ...string) error {
	if directory == "" && args[0] != "clone" {
		return fmt.Errorf("git %s: repository directory is required", args[0])
	}
	ctx, cancel := c.operationContext(ctx)
	defer cancel()
	cmd := exec.CommandContext(ctx, c.binary, args...)
	cmd.Dir = directory
	// Avoid waiting for terminal input in a source process. Existing credential
	// helpers and SSH configuration still apply.
	cmd.Env = append(cmd.Environ(), "GIT_TERMINAL_PROMPT=0")
	cmd.WaitDelay = time.Second
	cmd.Stdout = output
	// Do not include stderr or arguments in errors: Git may echo credentialed
	// remote URLs or helper output. Preserve the exit/cancellation error.
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			err = ctx.Err()
		}
		return fmt.Errorf("git %s: %w", args[0], err)
	}
	return nil
}
