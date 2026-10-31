// Copyright 2026 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package workspace

import (
	"context"
	"crypto/sha1"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/google/ax/pkg/apis/v1alpha1"
)

var gitTimeout = 10 * time.Minute // bounds each git attempt; a variable so tests can shorten it

const (
	// DefaultAXDir is the default directory inside the container used for AX system state and logs.
	DefaultAXDir = "/ax"
	// InitializedMarkerFilename is the base name of the marker file written after a successful maiden run.
	InitializedMarkerFilename = "initialized"
	// markerDirName is the directory under each workspace path that holds its marker. It sits on the
	// workspace volume, so the marker survives a volumes-only resume.
	markerDirName = ".ax"

	defaultWorkspacePath = "/workspace"
	defaultBranch        = "main"
	defaultRepoDirName   = "repo"

	gitSuccessLog = "git-success.log"
	gitErrorLog   = "git-error.log"
	gitRetries    = 5
	gitRetryDelay = 2 * time.Second

	dirPerm  = 0o755
	filePerm = 0o644
)

// AXDir is the directory inside the container used for AX system state and logs.
// Tests override it to keep state out of the workspace.
var AXDir = DefaultAXDir

// SetupResult contains details about the workspace maiden initialization.
type SetupResult struct {
	WorkspacePath string
	ClonedRepos   []string
	SkillsMounted string
	IsMaidenRun   bool
}

// SetupWorkspace prepares the workspace directory on its maiden run: it clones the
// declared Git repositories and creates the skills path. A marker file under
// <targetPath>/.ax records a completed maiden run so subsequent calls are no-ops.
//
// Git failures are logged and recorded under AXDir but do not abort setup. The marker
// is withheld in that case so the next start retries the clone.
func SetupWorkspace(ctx context.Context, ws *v1alpha1.Workspace, targetPath string) (*SetupResult, error) {
	if targetPath == "" {
		targetPath = defaultWorkspacePath
	}
	// Tooling downstream (git) scopes itself to this path, so it must be absolute.
	if abs, err := filepath.Abs(targetPath); err == nil {
		targetPath = abs
	}
	res := &SetupResult{WorkspacePath: targetPath}

	// Check the parents before MkdirAll, which would create directories through a link.
	// A parent inside a volume the agent can write could be turned into a link to somewhere
	// off the volume, so every existing parent is checked. Mount points must be given by
	// their real path.
	if err := refuseSymlinkAncestors(targetPath); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(targetPath, dirPerm); err != nil {
		return nil, fmt.Errorf("creating workspace directory %s: %w", targetPath, err)
	}
	if err := refuseSymlink(targetPath); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(AXDir, dirPerm); err != nil {
		slog.Warn("creating ax state directory", "dir", AXDir, "error", err)
	}

	for _, loc := range markerLocs(targetPath) {
		// Every link on the way to the marker is refused, so a symlinked .ax or marker
		// cannot send the check outside the workspace.
		if err := dropLinkOnMarkerPath(loc); err != nil {
			return nil, fmt.Errorf("clearing marker path %s: %w", loc.path, err)
		}
		if err := checkInsideWorkspace(loc.base, loc.path); err != nil {
			return nil, fmt.Errorf("checking marker %s: %w", loc.path, err)
		}
		// Lstat, not Stat: a dangling symlink at the marker path must not count as "absent".
		if _, err := os.Lstat(loc.path); err == nil {
			slog.Info("workspace already initialized; skipping maiden run setup", "path", targetPath)
			return res, nil
		} else if !os.IsNotExist(err) {
			return nil, fmt.Errorf("checking marker %s: %w", loc.path, err)
		}
	}
	// A marker from before the workspace-volume change lives under AXDir, which a resume
	// does not restore. Move it to the current location so the next boot does not rerun setup.
	legacy := filepath.Join(AXDir, legacyMarkerName(targetPath))
	_, legacyErr := os.Stat(legacy)
	if legacyErr != nil && !os.IsNotExist(legacyErr) {
		return nil, fmt.Errorf("checking legacy marker %s: %w", legacy, legacyErr)
	}
	if legacyErr == nil && legacyOwnedBy(legacy, ws) {
		slog.Info("migrating legacy workspace marker", "path", targetPath)
		recordMarker(targetPath, ws)
		return res, nil
	}

	slog.Info("executing workspace maiden run setup", "path", targetPath)
	res.IsMaidenRun = true

	gitOK := true
	if ws != nil && ws.Spec != nil {
		filesOK := writeFiles(ws.Spec.Files, targetPath)
		res.ClonedRepos, gitOK = cloneRepos(ctx, ws.Spec.Git, targetPath)
		gitOK = gitOK && filesOK
		res.SkillsMounted = setupSkills(ws.Spec.Skills)
	}

	if !gitOK {
		slog.Warn("maiden run workspace setup completed with errors; marker omitted to allow retry", "path", targetPath)
		return res, nil
	}

	recordMarker(targetPath, ws)
	slog.Info("maiden run workspace setup completed successfully", "path", targetPath)
	return res, nil
}

// gitDirOf returns the Git directory of a workspace root, or "" when the root is not a
// Git repo. A worktree or submodule has a .git file whose "gitdir:" line points at the
// real directory.
func gitDirOf(targetPath string) string {
	gitPath := filepath.Join(targetPath, ".git")
	info, err := os.Lstat(gitPath)
	if err != nil || info.Mode()&os.ModeSymlink != 0 {
		return ""
	}
	if info.IsDir() {
		return gitPath
	}
	// Only a regular file can hold a gitdir line. A FIFO or device would block the read.
	if !info.Mode().IsRegular() {
		return ""
	}
	data, err := os.ReadFile(gitPath)
	if err != nil {
		return ""
	}
	line := strings.TrimSpace(string(data))
	if !strings.HasPrefix(line, "gitdir:") {
		return ""
	}
	dir := strings.TrimSpace(strings.TrimPrefix(line, "gitdir:"))
	if dir == "" {
		return ""
	}
	if !filepath.IsAbs(dir) {
		dir = filepath.Join(targetPath, dir)
	}
	// Resolve symlinks before the containment check. A lexically inside path can pass
	// through a link to a directory outside the workspace, and the marker would then be
	// lost on a volumes-only resume.
	resolved, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return ""
	}
	root, err := filepath.EvalSymlinks(targetPath)
	if err != nil {
		return ""
	}
	// A Git directory outside the workspace is not on the durable volume, so the marker
	// would be lost on a volumes-only resume. Fall back to .ax in the workspace.
	if rel, err := filepath.Rel(root, resolved); err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return ""
	}
	if target, err := os.Stat(resolved); err != nil || !target.IsDir() {
		return ""
	}
	return resolved
}

// markerLoc is a place a maiden-run marker can live. base is the directory the path is
// under, so every component between base and path can be checked for symbolic links.
type markerLoc struct{ base, path string }

// markerLocs lists where a maiden-run marker may already exist for a workspace.
func markerLocs(targetPath string) []markerLoc {
	var locs []markerLoc
	if gitDir := gitDirOf(targetPath); gitDir != "" {
		locs = append(locs, markerLoc{gitDir, filepath.Join(gitDir, markerDirName, InitializedMarkerFilename)})
	}
	return append(locs, markerLoc{targetPath, filepath.Join(targetPath, markerDirName, InitializedMarkerFilename)})
}

// markerWriteLoc picks where to record a completed maiden run. A workspace that is a
// Git repo, worktree, or submodule keeps the marker inside its Git directory, which
// `git add -A` and `git clean -fdx` never touch. Other workspaces keep it in .ax.
func markerWriteLoc(targetPath string) markerLoc {
	return markerLocs(targetPath)[0]
}

// legacyMarkerName is the marker file name used before markers moved into the workspace.
func legacyMarkerName(path string) string {
	if path == "" {
		path = defaultWorkspacePath
	}
	clean := strings.Trim(filepath.Clean(path), "/")
	if clean == "" {
		clean = "root"
	}
	// Replace path separators and drive colons so the name is valid on every platform.
	// Linux paths contain neither, so their names are unchanged.
	return InitializedMarkerFilename + "-" + strings.NewReplacer("/", "-", "\\", "-", ":", "-").Replace(clean)
}

// cloneRepos fetches each declared repository into the workspace. It returns the
// repositories that succeeded and whether every clone succeeded.
func cloneRepos(ctx context.Context, repos []*v1alpha1.GitRepo, targetPath string) ([]string, bool) {
	var cloned []string
	ok := true
	for _, repo := range repos {
		if repo == nil || repo.Repo == "" {
			continue
		}

		// A value starting with "-" would be read by git as an option, such as
		// --upload-pack, which runs a command. Refuse it.
		if strings.HasPrefix(repo.Repo, "-") || strings.HasPrefix(repo.Branch, "-") {
			slog.Warn("skipping git repo with an option-like repo or branch", "repo", repo.Repo, "branch", repo.Branch)
			ok = false
			continue
		}
		dest := cloneDestination(repo, targetPath)
		// A clone must not land on AX's own state, such as the workspace's .git or .ax.
		if isReservedPath(targetPath, dest) {
			slog.Warn("skipping git repo in AX-reserved directory", "repo", repo.Repo, "dir", dest)
			ok = false
			continue
		}
		if err := checkInsideWorkspace(targetPath, dest); err != nil {
			slog.Warn("skipping git repo outside the workspace", "repo", repo.Repo, "dir", dest, "error", err)
			ok = false
			continue
		}
		// A retry after another repo failed must not run checkout -f over a repo that
		// already has its contents, because that discards the agent's uncommitted edits.
		if has, matches := existingCheckout(ctx, dest, repo.Repo); has {
			// A checkout of another repository is never counted as this one, and it is
			// not overwritten either.
			if !matches {
				slog.Warn("destination holds a checkout of another repo; skipping", "repo", repo.Repo, "dir", dest)
				ok = false
				continue
			}
			cloned = append(cloned, repo.Repo)
			continue
		}
		if err := os.MkdirAll(dest, dirPerm); err != nil {
			slog.Warn("failed to create destination dir", "dir", dest, "error", err)
		}

		branch := repo.Branch
		if branch == "" {
			branch = defaultBranch
		}

		slog.Info("initializing and fetching git repo", "repo", repo.Repo, "branch", branch, "dir", dest, "depth", repo.Depth)
		out, err := retry(ctx, gitRetries, gitRetryDelay, func() ([]byte, error) {
			// Bound each attempt, so a stalled remote cannot block the task from starting.
			attemptCtx, cancel := context.WithTimeout(ctx, gitTimeout)
			defer cancel()
			return fetchRepo(attemptCtx, dest, repo.Repo, branch, repo.Depth)
		})
		if err != nil {
			ok = false
			slog.Warn("git fetch error (continuing setup)", "repo", repo.Repo, "error", err, "output", string(out))
			writeStateFile(gitErrorLog, fmt.Appendf(nil, "error: %v\noutput: %s\n", err, out))
			continue
		}

		cloned = append(cloned, repo.Repo)
		writeStateFile(gitSuccessLog, out)
	}
	return cloned, ok
}

// cloneDestination resolves where a repository is checked out.
//
// An explicit Dir wins: "." means the workspace root, an absolute path is used as-is,
// and anything else is relative to the workspace. Otherwise the directory is named after
// the repository's Name, unless that is empty or a generic placeholder such as "origin",
// in which case the name is derived from the repository URL.
func cloneDestination(repo *v1alpha1.GitRepo, targetPath string) string {
	if repo.Dir != "" {
		switch {
		case repo.Dir == ".":
			return targetPath
		case filepath.IsAbs(repo.Dir):
			return repo.Dir
		default:
			return filepath.Join(targetPath, repo.Dir)
		}
	}

	name := repo.Name
	if name == "" || name == defaultRepoDirName || name == "origin" {
		if derived := RepoDirName(repo.Repo); derived != "" {
			name = derived
		} else if name == "" {
			name = defaultRepoDirName
		}
	}
	return filepath.Join(targetPath, name)
}

// fetchRepo initializes dir as a git repository pointed at repoURL, fetches branch,
// and checks it out. If depth > 0, it performs a shallow fetch. It is idempotent so it
// can be retried against a partially initialized directory. Arguments are passed
// directly to git, never through a shell.
func fetchRepo(ctx context.Context, dir, repoURL, branch string, depth int32) ([]byte, error) {
	var combined []byte
	run := func(args ...string) error {
		cmd := exec.CommandContext(ctx, "git", args...)
		cmd.Dir = dir
		out, err := cmd.CombinedOutput()
		combined = append(combined, out...)
		if err != nil {
			return fmt.Errorf("git %s: %w", args[0], err)
		}
		return nil
	}

	if err := run("init"); err != nil {
		return combined, err
	}
	// A retry lands on an already initialized directory, so fall back to updating the remote.
	if err := run("remote", "add", "origin", repoURL); err != nil {
		if err := run("remote", "set-url", "origin", repoURL); err != nil {
			return combined, err
		}
	}
	fetchArgs := []string{"fetch"}
	if depth > 0 {
		fetchArgs = append(fetchArgs, fmt.Sprintf("--depth=%d", depth))
	}
	fetchArgs = append(fetchArgs, "origin", branch)
	if err := run(fetchArgs...); err != nil {
		return combined, err
	}
	// Files left by an earlier checkout that was interrupted hold exactly the repo's content,
	// so they are removed and the checkout recreates them. Any other file is kept, and the
	// checkout below refuses to overwrite it.
	if err := removeIdenticalFiles(ctx, dir, "FETCH_HEAD"); err != nil {
		return combined, err
	}
	// No -f: git refuses to overwrite a tracked file with local edits, or an untracked file
	// that the checkout would replace.
	if err := run("checkout", "FETCH_HEAD"); err != nil {
		return combined, err
	}
	return combined, nil
}

// removeIdenticalFiles deletes the files in dir whose content matches the blob git would
// check out for ref. Removing them loses nothing, because the checkout writes the same bytes.
func removeIdenticalFiles(ctx context.Context, dir, ref string) error {
	cmd := exec.CommandContext(ctx, "git", "ls-tree", "-r", "-z", ref)
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		return fmt.Errorf("git ls-tree %s: %w", ref, err)
	}
	// Each entry is "<mode> SP <type> SP <object> TAB <path>", NUL-terminated.
	for _, entry := range strings.Split(string(out), "\x00") {
		meta, path, ok := strings.Cut(entry, "\t")
		if !ok {
			continue
		}
		fields := strings.Fields(meta)
		if len(fields) != 3 || fields[1] != "blob" || fields[0] == "120000" {
			continue
		}
		p := filepath.Join(dir, filepath.FromSlash(path))
		if checkInsideWorkspace(dir, p) != nil {
			continue
		}
		info, err := os.Lstat(p)
		if err != nil || !info.Mode().IsRegular() {
			continue
		}
		data, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		// A git blob id is the SHA-1 of "blob <size>\0" followed by the content.
		sum := sha1.Sum(append([]byte(fmt.Sprintf("blob %d\x00", len(data))), data...))
		if hex.EncodeToString(sum[:]) == fields[2] {
			if err := os.Remove(p); err != nil {
				return fmt.Errorf("removing %s: %w", p, err)
			}
		}
	}
	return nil
}

// dropLinkOnMarkerPath removes the first symbolic link found between a marker's base and
// the marker path. The agent can create such a link inside the workspace, and leaving it
// would make every boot fail. The link itself is removed, never its target.
func dropLinkOnMarkerPath(loc markerLoc) error {
	rel, err := filepath.Rel(loc.base, loc.path)
	if err != nil {
		return err
	}
	current := loc.base
	for _, part := range strings.Split(rel, string(filepath.Separator)) {
		current = filepath.Join(current, part)
		info, err := os.Lstat(current)
		if err != nil {
			if os.IsNotExist(err) {
				return nil
			}
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			slog.Warn("removing symbolic link on the marker path", "path", current)
			return os.Remove(current)
		}
	}
	return nil
}

// legacyOwnedBy reports whether the legacy marker at path was written for this workspace.
// Two paths can share a legacy name (/a/b and /a-b), so the recorded name is checked too.
func legacyOwnedBy(path string, ws *v1alpha1.Workspace) bool {
	data, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	name := ws.GetMetadata().GetName()
	if name == "" {
		name = "unknown" // the name the old writer used when the workspace had none
	}
	return strings.HasPrefix(string(data), "workspace: "+name+"\n")
}

// writeFileAtomic writes data to a temporary file beside path, then renames it into place,
// so a failed or interrupted write never leaves a truncated file at path.
func writeFileAtomic(path string, data []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".ax-write-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmp.Name(), filePerm); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

// retry runs op up to attempts times, waiting delay between failures. It stops early
// when ctx is done and never sleeps after the final attempt.
func retry(ctx context.Context, attempts int, delay time.Duration, op func() ([]byte, error)) ([]byte, error) {
	var (
		out []byte
		err error
	)
	for attempt := 1; attempt <= attempts; attempt++ {
		out, err = op()
		if err == nil {
			return out, nil
		}
		if attempt == attempts {
			break
		}
		slog.Warn("git operation failed, will retry", "attempt", attempt, "max", attempts, "error", err, "output", string(out))
		select {
		case <-ctx.Done():
			return out, errors.Join(err, ctx.Err())
		case <-time.After(delay):
		}
	}
	return out, err
}

// setupSkills creates the skills directory if one is declared and returns its path.
func setupSkills(skills *v1alpha1.SkillsConfig) string {
	if skills == nil || skills.Path == "" {
		return ""
	}
	if err := os.MkdirAll(skills.Path, dirPerm); err != nil {
		slog.Warn("creating skills dir", "path", skills.Path, "error", err)
	}
	return skills.Path
}

// recordMarker writes the maiden-run marker. A failure is logged, not returned: setup
// already succeeded, and returning an error would stop the task from ever becoming ready.
// The next boot then reruns setup, which keeps existing files and checkouts.
func recordMarker(targetPath string, ws *v1alpha1.Workspace) {
	if err := writeMarker(markerWriteLoc(targetPath), ws); err != nil {
		slog.Warn("could not record maiden-run marker; setup will rerun on the next boot", "path", targetPath, "error", err)
	}
}

// writeMarker records a completed maiden run.
func writeMarker(loc markerLoc, ws *v1alpha1.Workspace) error {
	path := loc.path
	name := "unknown"
	if ws != nil && ws.GetMetadata() != nil && ws.GetMetadata().GetName() != "" {
		name = ws.GetMetadata().GetName()
	}
	content := fmt.Sprintf("workspace: %s\ninitialized_at: %s\n", name, time.Now().UTC().Format(time.RFC3339))
	// Refuse any symbolic link between the base and the marker, including the file itself,
	// because WriteFile would follow it.
	if err := checkInsideWorkspace(loc.base, path); err != nil {
		return err
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, dirPerm); err != nil {
		return fmt.Errorf("creating marker directory %s: %w", dir, err)
	}
	if err := os.WriteFile(path, []byte(content), filePerm); err != nil {
		return fmt.Errorf("writing marker file %s: %w", path, err)
	}
	return nil
}

// refuseSymlinkAncestors fails when any existing parent of dir is a symbolic link.
func refuseSymlinkAncestors(dir string) error {
	for parent := filepath.Dir(dir); ; parent = filepath.Dir(parent) {
		if err := refuseSymlink(parent); err != nil {
			return err
		}
		if filepath.Dir(parent) == parent {
			return nil
		}
	}
}

// refuseSymlink fails when dir exists and is a symbolic link, so a committed .ax link
// cannot send the marker to a path outside the workspace.
func refuseSymlink(dir string) error {
	info, err := os.Lstat(dir)
	if err == nil && info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("%s is a symbolic link", dir)
	}
	return nil
}

// writeStateFile writes a diagnostic file under AXDir, logging rather than failing on error.
func writeStateFile(name string, data []byte) {
	path := filepath.Join(AXDir, name)
	if err := os.WriteFile(path, data, filePerm); err != nil {
		slog.Warn("failed to write state file", "path", path, "error", err)
	}
}

// RepoDirName extracts the repository name from a git URL or local path.
// For example "https://github.com/chalk/chalk.git" yields "chalk".
func RepoDirName(repoURL string) string {
	trimmed := strings.TrimSuffix(strings.TrimSpace(repoURL), "/")
	trimmed = strings.TrimSuffix(trimmed, ".git")
	idx := strings.LastIndexAny(trimmed, "/:")
	if idx < 0 {
		return trimmed
	}
	return trimmed[idx+1:]
}

// isReservedPath reports whether dest falls inside the AX marker directory of the
// workspace at targetPath. Inlined files must not replace AX's own state.
func isReservedPath(targetPath, dest string) bool {
	if rel, err := filepath.Rel(targetPath, dest); err == nil {
		// Windows ignores trailing dots and spaces and accepts 8.3 short names (GIT~1),
		// so normalize each segment before comparing.
		parts := strings.Split(strings.ToLower(filepath.ToSlash(rel)), "/")
		for i := range parts {
			parts[i] = strings.TrimRight(parts[i], ". ")
		}
		if first := parts[0]; first == ".git" || first == markerDirName || strings.HasPrefix(first, "git~") || strings.HasPrefix(first, "ax~") {
			return true
		}
	}
	if rel, err := filepath.Rel(AXDir, dest); err == nil && !strings.HasPrefix(rel, "..") {
		return strings.HasPrefix(strings.ToLower(rel), strings.ToLower(InitializedMarkerFilename))
	}
	return false
}

// writeFiles writes inlined files into the workspace directory.
// writeFiles writes inlined files into the workspace. A file that already exists is kept,
// so a retried setup does not overwrite the agent's edits. It reports whether every file
// that should be written was written.
func writeFiles(files []*v1alpha1.File, targetPath string) bool {
	ok := true
	for _, f := range files {
		if f == nil || f.GetPath() == "" {
			continue
		}
		dest := f.GetPath()
		if !filepath.IsAbs(dest) {
			dest = filepath.Join(targetPath, dest)
		}
		if isReservedPath(targetPath, dest) {
			slog.Warn("skipping workspace file in AX-reserved directory", "path", f.GetPath())
			continue
		}
		if err := checkInsideWorkspace(targetPath, dest); err != nil {
			slog.Warn("skipping workspace file outside the workspace", "path", f.GetPath(), "error", err)
			ok = false
			continue
		}
		if info, err := os.Lstat(dest); err == nil {
			// A directory where the file should be is not a completed file setup.
			if info.IsDir() {
				slog.Warn("workspace file path is a directory", "path", f.GetPath())
				ok = false
				continue
			}
			slog.Info("keeping existing workspace file", "path", dest)
			continue
		}
		if err := os.MkdirAll(filepath.Dir(dest), dirPerm); err != nil {
			slog.Warn("failed to create directory for workspace file", "path", dest, "error", err)
			ok = false
			continue
		}
		if err := writeFileAtomic(dest, []byte(f.GetContent())); err != nil {
			slog.Warn("failed to write workspace file", "path", dest, "error", err)
			ok = false
			continue
		}
		slog.Info("wrote inlined workspace file", "path", dest)
	}
	return ok
}

// checkInsideWorkspace fails when dest is outside targetPath, or when any existing part of
// the path between targetPath and dest is a symbolic link.
func checkInsideWorkspace(targetPath, dest string) error {
	// An absolute path with ".." is not cleaned before use, so the kernel would resolve it
	// through any symbolic link on the way. Refuse it outright.
	for _, part := range strings.Split(filepath.ToSlash(dest), "/") {
		if part == ".." {
			return fmt.Errorf("%s contains a parent directory reference", dest)
		}
	}
	rel, err := filepath.Rel(targetPath, dest)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return fmt.Errorf("%s is outside %s", dest, targetPath)
	}
	if rel == "." {
		return nil
	}
	parts := strings.Split(rel, string(filepath.Separator))
	current := targetPath
	for i, part := range parts {
		current = filepath.Join(current, part)
		info, err := os.Lstat(current)
		if err != nil {
			if os.IsNotExist(err) {
				return nil
			}
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("%s is a symbolic link", current)
		}
		// A file where a parent directory belongs cannot hold anything below it. Windows
		// reports that as "not found" rather than "not a directory", so check it here.
		if !info.IsDir() && i < len(parts)-1 {
			return fmt.Errorf("%s is not a directory", current)
		}
	}
	return nil
}

// existingCheckout reports whether dir already holds a Git checkout with a commit at HEAD
// (has), and whether its origin is repoURL (matches).
func existingCheckout(ctx context.Context, dir, repoURL string) (has, matches bool) {
	// Require a .git here, so a parent repository's HEAD does not count.
	if _, err := os.Lstat(filepath.Join(dir, ".git")); err != nil {
		return false, false
	}
	head := exec.CommandContext(ctx, "git", "rev-parse", "--verify", "-q", "HEAD")
	head.Dir = dir
	if head.Run() != nil {
		return false, false
	}
	origin := exec.CommandContext(ctx, "git", "config", "--get", "remote.origin.url")
	origin.Dir = dir
	out, err := origin.Output()
	if err != nil {
		return true, false
	}
	return true, strings.TrimSpace(string(out)) == repoURL
}
