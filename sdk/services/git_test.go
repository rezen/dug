package services

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestGitRepositoryOperations(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	origin := t.TempDir()
	git := func(args ...string) string {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = origin
		cmd.Env = append(cmd.Environ(), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL="+os.DevNull)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	git("init", "--initial-branch=main")
	git("config", "user.name", "Dug Test")
	git("config", "user.email", "dug@example.invalid")
	commit := func(contents string) string {
		t.Helper()
		if err := os.WriteFile(filepath.Join(origin, "file.txt"), []byte(contents), 0600); err != nil {
			t.Fatal(err)
		}
		git("add", "file.txt")
		git("-c", "commit.gpgsign=false", "commit", "-m", "fixture")
		return git("rev-parse", "HEAD")
	}
	first := commit("first")
	git("tag", "v1")
	directory := filepath.Join(t.TempDir(), "clone with spaces")
	client := NewDugGitClient(GitClientOptions{})
	ctx := context.Background()
	if err := client.Clone(ctx, origin, directory); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(directory, "file.txt")); !os.IsNotExist(err) {
		t.Fatalf("clone unexpectedly checked out files: %v", err)
	}
	if actual, err := client.ResolveRevision(ctx, directory, "v1"); err != nil || actual != first {
		t.Fatalf("resolve tag: %q, %v; want %s", actual, err, first)
	}
	second := commit("second")
	if err := client.Fetch(ctx, directory); err != nil {
		t.Fatal(err)
	}
	if actual, err := client.ResolveRevision(ctx, directory, "origin/main"); err != nil || actual != second {
		t.Fatalf("resolve fetched branch: %q, %v; want %s", actual, err, second)
	}
	var archive bytes.Buffer
	if err := client.Archive(ctx, directory, "origin/main", &archive); err != nil {
		t.Fatal(err)
	}
	reader := tar.NewReader(&archive)
	header, err := reader.Next()
	if err == nil && header.Typeflag == tar.TypeXGlobalHeader {
		header, err = reader.Next()
	}
	if err != nil || header.Name != "file.txt" {
		t.Fatalf("archive entry: %v, %v", header, err)
	}
	data, err := io.ReadAll(reader)
	if err != nil || string(data) != "second" {
		t.Fatalf("archive contents: %q, %v", data, err)
	}
	if _, err := client.ResolveRevision(ctx, directory, "--help"); err == nil {
		t.Fatal("option-like revision must not become a command option")
	}
	if err := client.Clone(ctx, origin, directory); err == nil {
		t.Fatal("clone must not overwrite an existing repository")
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := client.ResolveRevision(canceled, directory, "HEAD"); !errors.Is(err, context.Canceled) {
		t.Fatalf("expected cancellation, got %v", err)
	}
	deadline := NewDugGitClient(GitClientOptions{Timeout: time.Nanosecond})
	if err := deadline.Fetch(ctx, directory); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected service deadline, got %v", err)
	}
}
