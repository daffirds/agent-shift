package session

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/daffirds/agent-shift/internal/app"
	"github.com/daffirds/agent-shift/internal/proc"
)

// createWorktree makes a detached git worktree of workdir's repository for one session,
// so parallel sessions never edit the same checkout. It returns the worktree root and the
// session's working directory inside it (keeping workdir's sub-directory, if any).
func createWorktree(ctx context.Context, workdir, id string) (root, dir string, err error) {
	top, err := git(ctx, workdir, "rev-parse", "--show-toplevel")
	if err != nil {
		return "", "", fmt.Errorf("%s is not inside a git repository: %w", workdir, err)
	}
	prefix, err := git(ctx, workdir, "rev-parse", "--show-prefix")
	if err != nil {
		return "", "", err
	}
	root = filepath.Join(app.WorktreesDir(), id)
	if _, err := git(ctx, top, "worktree", "add", "--detach", root, "HEAD"); err != nil {
		return "", "", err
	}
	return root, filepath.Join(root, filepath.FromSlash(prefix)), nil
}

// RemoveWorktree deletes a session's worktree (used when pruning ended sessions).
func RemoveWorktree(ctx context.Context, repoDir, root string) error {
	_, err := git(ctx, repoDir, "worktree", "remove", "--force", root)
	return err
}

func git(ctx context.Context, dir string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", dir}, args...)...)
	proc.HideWindow(cmd)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return strings.TrimSpace(string(out)), nil
}
