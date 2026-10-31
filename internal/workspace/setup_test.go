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

package workspace_test

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/google/ax/internal/workspace"
	"github.com/google/ax/pkg/apis/v1alpha1"
)

func TestSetupWorkspace_MaidenRun(t *testing.T) {
	tempDir, err := os.MkdirTemp("", "ax-ws-test-*")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tempDir)

	ws := &v1alpha1.Workspace{
		Metadata: &v1alpha1.ObjectMeta{
			Name: "test-ws",
		},
		Spec: &v1alpha1.WorkspaceSpec{
			Skills: &v1alpha1.SkillsConfig{
				Path: filepath.Join(tempDir, "skills"),
			},
		},
	}

	stateDir := filepath.Join(tempDir, "ax-state")
	origAXDir := workspace.AXDir
	workspace.AXDir = stateDir
	defer func() { workspace.AXDir = origAXDir }()

	// 1. Maiden run
	res1, err := workspace.SetupWorkspace(context.Background(), ws, tempDir)
	if err != nil {
		t.Fatalf("maiden setup failed: %v", err)
	}
	if !res1.IsMaidenRun {
		t.Errorf("expected maiden run to be true")
	}

	// Verify marker file exists under the workspace's .ax directory, not stateDir
	markerFile := filepath.Join(tempDir, ".ax", workspace.InitializedMarkerFilename)
	if _, err := os.Stat(markerFile); os.IsNotExist(err) {
		t.Errorf("expected marker file %s to exist", markerFile)
	}
	if _, err := os.Stat(filepath.Join(stateDir, workspace.InitializedMarkerFilename)); !os.IsNotExist(err) {
		t.Errorf("expected marker NOT to be written under stateDir")
	}

	// Verify workspace directory has no .ax-initialized file
	if _, err := os.Stat(filepath.Join(tempDir, ".ax-initialized")); !os.IsNotExist(err) {
		t.Errorf("expected .ax-initialized not to be in workspace")
	}

	// 2. Second run (should detect already initialized)
	res2, err := workspace.SetupWorkspace(context.Background(), ws, tempDir)
	if err != nil {
		t.Fatalf("second setup failed: %v", err)
	}
	if res2.IsMaidenRun {
		t.Errorf("expected maiden run to be false on second run")
	}
}

// TestSetupWorkspace_MarkerSurvivesStateLoss simulates a volumes-only resume: the
// workspace volume comes back but the AX state directory does not. Setup must not
// run again, because the marker lives with the workspace.
func TestSetupWorkspace_MarkerSurvivesStateLoss(t *testing.T) {
	tempDir, err := os.MkdirTemp("", "ax-ws-resume-*")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tempDir)

	ws := &v1alpha1.Workspace{
		Metadata: &v1alpha1.ObjectMeta{Name: "resume-ws"},
		Spec: &v1alpha1.WorkspaceSpec{
			Files: []*v1alpha1.File{{Path: "AGENTS.md", Content: "first boot"}},
		},
	}

	origAXDir := workspace.AXDir
	defer func() { workspace.AXDir = origAXDir }()

	workspace.AXDir = filepath.Join(tempDir, "ax-state-before")
	res1, err := workspace.SetupWorkspace(context.Background(), ws, tempDir)
	if err != nil {
		t.Fatalf("maiden setup failed: %v", err)
	}
	if !res1.IsMaidenRun {
		t.Fatalf("expected maiden run on first boot")
	}

	// The agent edits its file, then the container restarts on a fresh state directory.
	agentFile := filepath.Join(tempDir, "AGENTS.md")
	if err := os.WriteFile(agentFile, []byte("edited by agent"), 0o644); err != nil {
		t.Fatalf("failed to edit file: %v", err)
	}
	workspace.AXDir = filepath.Join(tempDir, "ax-state-after")

	res2, err := workspace.SetupWorkspace(context.Background(), ws, tempDir)
	if err != nil {
		t.Fatalf("resume setup failed: %v", err)
	}
	if res2.IsMaidenRun {
		t.Errorf("expected no maiden run after resume when only the workspace survives")
	}
	got, err := os.ReadFile(agentFile)
	if err != nil {
		t.Fatalf("failed to read file after resume: %v", err)
	}
	if string(got) != "edited by agent" {
		t.Errorf("resume overwrote agent edit: got %q", got)
	}
}

func TestSetupWorkspace_GitSubdir(t *testing.T) {
	tempDir, err := os.MkdirTemp("", "ax-ws-git-*")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tempDir)

	// Create a dummy source git repository
	srcDir, err := os.MkdirTemp("", "ax-src-git-*")
	if err != nil {
		t.Fatalf("failed to create src dir: %v", err)
	}
	defer os.RemoveAll(srcDir)

	// Init source repo
	// git init -b requires git 2.28+; symbolic-ref works on any version.
	initCmd := exec.Command("sh", "-c", "git init && git symbolic-ref HEAD refs/heads/main && git config user.email 'test@ax.io' && git config user.name 'AX' && echo 'hello' > README.md && git add . && git commit -m 'initial'")
	initCmd.Dir = srcDir
	if out, err := initCmd.CombinedOutput(); err != nil {
		t.Fatalf("failed to init test git repo: %v (%s)", err, string(out))
	}

	ws := &v1alpha1.Workspace{
		Metadata: &v1alpha1.ObjectMeta{
			Name: "test-git-ws",
		},
		Spec: &v1alpha1.WorkspaceSpec{
			Git: []*v1alpha1.GitRepo{
				{
					Name: "ax",
					Repo: srcDir,
				},
			},
		},
	}

	stateDir := filepath.Join(tempDir, "ax-state")
	origAXDir := workspace.AXDir
	workspace.AXDir = stateDir
	defer func() { workspace.AXDir = origAXDir }()

	res, err := workspace.SetupWorkspace(context.Background(), ws, tempDir)
	if err != nil {
		t.Fatalf("setup failed: %v", err)
	}
	if len(res.ClonedRepos) != 1 {
		t.Errorf("expected 1 cloned repo, got %d", len(res.ClonedRepos))
	}

	// Verify repo was cloned into subdirectory 'ax' inside tempDir
	clonedGit := filepath.Join(tempDir, "ax", ".git")
	if _, err := os.Stat(clonedGit); os.IsNotExist(err) {
		t.Errorf("expected %s to exist", clonedGit)
	}

	// Verify git-success.log is created under stateDir and NOT inside workspace
	logFile := filepath.Join(stateDir, "git-success.log")
	if _, err := os.Stat(logFile); os.IsNotExist(err) {
		t.Errorf("expected git log file %s to exist in stateDir", logFile)
	}
	if _, err := os.Stat(filepath.Join(tempDir, ".ax-git-success.log")); !os.IsNotExist(err) {
		t.Errorf("expected .ax-git-success.log NOT to exist in workspace")
	}
}

func TestSetupWorkspace_GitError(t *testing.T) {
	tempDir, err := os.MkdirTemp("", "ax-ws-git-err-*")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tempDir)

	ws := &v1alpha1.Workspace{
		Metadata: &v1alpha1.ObjectMeta{
			Name: "test-git-err-ws",
		},
		Spec: &v1alpha1.WorkspaceSpec{
			Git: []*v1alpha1.GitRepo{
				{
					Name: "invalid-repo",
					Repo: "https://127.0.0.1:9/nonexistent/repo.git",
				},
			},
		},
	}

	stateDir := filepath.Join(tempDir, "ax-state")
	origAXDir := workspace.AXDir
	workspace.AXDir = stateDir
	defer func() { workspace.AXDir = origAXDir }()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	_, _ = workspace.SetupWorkspace(ctx, ws, tempDir)

	// Verify git-error.log is created under stateDir and NOT inside workspace
	errLog := filepath.Join(stateDir, "git-error.log")
	if _, err := os.Stat(errLog); os.IsNotExist(err) {
		t.Errorf("expected git error log %s to exist under stateDir", errLog)
	}

	// Verify workspace directory does not have .ax-git-error.log or .ax
	if _, err := os.Stat(filepath.Join(tempDir, ".ax-git-error.log")); !os.IsNotExist(err) {
		t.Errorf("expected .ax-git-error.log NOT to exist in workspace")
	}
	if _, err := os.Stat(filepath.Join(tempDir, ".ax")); !os.IsNotExist(err) {
		t.Errorf("expected .ax directory NOT to exist in workspace")
	}

	// Verify initialization marker was not created due to failure
	markerFile := filepath.Join(tempDir, ".ax", workspace.InitializedMarkerFilename)
	if _, err := os.Stat(markerFile); !os.IsNotExist(err) {
		t.Errorf("expected marker file %s NOT to exist after failure", markerFile)
	}
}

func TestSetupWorkspace_GitDepth(t *testing.T) {
	tempDir, err := os.MkdirTemp("", "ax-ws-git-depth-*")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tempDir)

	// Create a dummy source git repository with 3 commits
	srcDir, err := os.MkdirTemp("", "ax-src-git-*")
	if err != nil {
		t.Fatalf("failed to create src dir: %v", err)
	}
	defer os.RemoveAll(srcDir)

	initCmd := exec.Command("sh", "-c", `
git init &&
git symbolic-ref HEAD refs/heads/main &&
git config user.email 'test@ax.io' &&
git config user.name 'AX' &&
echo '1' > file.txt && git add . && git commit -m 'commit 1' &&
echo '2' > file.txt && git add . && git commit -m 'commit 2' &&
echo '3' > file.txt && git add . && git commit -m 'commit 3'
`)
	initCmd.Dir = srcDir
	if out, err := initCmd.CombinedOutput(); err != nil {
		t.Fatalf("failed to init test git repo with commits: %v (%s)", err, string(out))
	}

	ws := &v1alpha1.Workspace{
		Metadata: &v1alpha1.ObjectMeta{
			Name: "test-shallow-git-ws",
		},
		Spec: &v1alpha1.WorkspaceSpec{
			Git: []*v1alpha1.GitRepo{
				{
					Name:  "ax",
					Repo:  srcDir,
					Depth: 1,
				},
			},
		},
	}

	stateDir := filepath.Join(tempDir, "ax-state")
	origAXDir := workspace.AXDir
	workspace.AXDir = stateDir
	defer func() { workspace.AXDir = origAXDir }()

	res, err := workspace.SetupWorkspace(context.Background(), ws, tempDir)
	if err != nil {
		t.Fatalf("setup failed: %v", err)
	}
	if len(res.ClonedRepos) != 1 {
		t.Errorf("expected 1 cloned repo, got %d", len(res.ClonedRepos))
	}

	// Verify commit count in cloned repo is exactly 1
	countCmd := exec.Command("git", "rev-list", "--count", "HEAD")
	countCmd.Dir = filepath.Join(tempDir, "ax")
	out, err := countCmd.Output()
	if err != nil {
		t.Fatalf("failed to count commits: %v", err)
	}
	if got := string(out); got != "1\n" && got != "1" {
		t.Errorf("expected 1 commit with depth=1, got %q", got)
	}
}

func TestSetupWorkspace_InlinedFiles(t *testing.T) {
	tempDir, err := os.MkdirTemp("", "ax-ws-files-*")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tempDir)

	ws := &v1alpha1.Workspace{
		Metadata: &v1alpha1.ObjectMeta{Name: "files-ws"},
		Spec: &v1alpha1.WorkspaceSpec{
			Files: []*v1alpha1.File{
				{Path: "config/settings.json", Content: `{"env": "production"}`},
				{Path: "notes.txt", Content: "Hello AX"},
			},
		},
	}

	stateDir := filepath.Join(tempDir, "ax-state")
	origAXDir := workspace.AXDir
	workspace.AXDir = stateDir
	defer func() { workspace.AXDir = origAXDir }()

	res, err := workspace.SetupWorkspace(context.Background(), ws, tempDir)
	if err != nil {
		t.Fatalf("setup failed: %v", err)
	}
	if !res.IsMaidenRun {
		t.Errorf("expected maiden run to be true")
	}

	configData, err := os.ReadFile(filepath.Join(tempDir, "config", "settings.json"))
	if err != nil {
		t.Fatalf("reading config/settings.json: %v", err)
	}
	if string(configData) != `{"env": "production"}` {
		t.Errorf("unexpected content in config/settings.json: %s", string(configData))
	}

	notesData, err := os.ReadFile(filepath.Join(tempDir, "notes.txt"))
	if err != nil {
		t.Fatalf("reading notes.txt: %v", err)
	}
	if string(notesData) != "Hello AX" {
		t.Errorf("unexpected content in notes.txt: %s", string(notesData))
	}
}

// TestSetupWorkspace_MarkerInsideGitRepo checks that a workspace that is a Git repo
// keeps its marker inside .git, so `git clean -fdx` cannot remove it.
func TestSetupWorkspace_MarkerInsideGitRepo(t *testing.T) {
	tempDir, err := os.MkdirTemp("", "ax-ws-gitmarker-*")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tempDir)

	for _, args := range [][]string{{"init", "-q"}, {"config", "user.email", "test@ax.io"}, {"config", "user.name", "AX"}} {
		cmd := exec.Command("git", args...)
		cmd.Dir = tempDir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v failed: %v (%s)", args, err, string(out))
		}
	}

	origAXDir := workspace.AXDir
	workspace.AXDir = filepath.Join(tempDir, "..", "ax-state-gitmarker")
	defer func() { workspace.AXDir = origAXDir }()

	ws := &v1alpha1.Workspace{Metadata: &v1alpha1.ObjectMeta{Name: "gitmarker-ws"}}
	res, err := workspace.SetupWorkspace(context.Background(), ws, tempDir)
	if err != nil {
		t.Fatalf("setup failed: %v", err)
	}
	if !res.IsMaidenRun {
		t.Fatalf("expected maiden run on first setup")
	}
	if _, err := os.Stat(filepath.Join(tempDir, ".git", ".ax", workspace.InitializedMarkerFilename)); err != nil {
		t.Fatalf("expected marker inside .git: %v", err)
	}
	if _, err := os.Stat(filepath.Join(tempDir, ".ax")); !os.IsNotExist(err) {
		t.Errorf("expected no .ax directory in the worktree")
	}

	cleanCmd := exec.Command("git", "clean", "-fdx")
	cleanCmd.Dir = tempDir
	if out, err := cleanCmd.CombinedOutput(); err != nil {
		t.Fatalf("git clean failed: %v (%s)", err, string(out))
	}

	res2, err := workspace.SetupWorkspace(context.Background(), ws, tempDir)
	if err != nil {
		t.Fatalf("second setup failed: %v", err)
	}
	if res2.IsMaidenRun {
		t.Errorf("expected no maiden run after git clean -fdx")
	}
}

// TestSetupWorkspace_HonorsLegacyMarker checks that a workspace set up before the
// marker moved still counts as initialized, so an upgrade does not re-run setup.
func TestSetupWorkspace_HonorsLegacyMarker(t *testing.T) {
	tempDir, err := os.MkdirTemp("", "ax-ws-legacy-*")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tempDir)

	stateDir := filepath.Join(tempDir, "ax-state")
	if err := os.MkdirAll(stateDir, 0o755); err != nil {
		t.Fatal(err)
	}
	legacy := "initialized-" + strings.NewReplacer("/", "-", "\\", "-", ":", "-").Replace(strings.Trim(filepath.Clean(tempDir), "/"))
	if err := os.WriteFile(filepath.Join(stateDir, legacy), []byte("workspace: legacy-ws\ninitialized_at: 2026-01-01T00:00:00Z\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	origAXDir := workspace.AXDir
	workspace.AXDir = stateDir
	defer func() { workspace.AXDir = origAXDir }()

	ws := &v1alpha1.Workspace{
		Metadata: &v1alpha1.ObjectMeta{Name: "legacy-ws"},
		Spec: &v1alpha1.WorkspaceSpec{
			Files: []*v1alpha1.File{{Path: "AGENTS.md", Content: "from spec"}},
		},
	}
	res, err := workspace.SetupWorkspace(context.Background(), ws, tempDir)
	if err != nil {
		t.Fatalf("setup failed: %v", err)
	}
	if res.IsMaidenRun {
		t.Errorf("expected no maiden run when a legacy marker exists")
	}
	if _, err := os.Stat(filepath.Join(tempDir, "AGENTS.md")); !os.IsNotExist(err) {
		t.Errorf("expected spec files not to be rewritten on a legacy-initialized workspace")
	}
}

// TestSetupWorkspace_MarkerOnVolumeForWorktree checks that a linked worktree keeps its
// marker in the workspace (the volume), because its Git directory is outside it.
func TestSetupWorkspace_MarkerOnVolumeForWorktree(t *testing.T) {
	tempDir, err := os.MkdirTemp("", "ax-ws-worktree-*")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tempDir)

	mainDir := filepath.Join(tempDir, "main")
	wtDir := filepath.Join(tempDir, "wt")
	if err := os.MkdirAll(mainDir, 0o755); err != nil {
		t.Fatal(err)
	}
	cmds := [][]string{
		{"init", "-q", "-b", "main"},
		{"config", "user.email", "test@ax.io"},
		{"config", "user.name", "AX"},
		{"commit", "-q", "--allow-empty", "-m", "init"},
		{"worktree", "add", "-q", wtDir},
	}
	for _, args := range cmds {
		cmd := exec.Command("git", args...)
		cmd.Dir = mainDir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v failed: %v (%s)", args, err, string(out))
		}
	}

	origAXDir := workspace.AXDir
	workspace.AXDir = filepath.Join(tempDir, "ax-state-worktree")
	defer func() { workspace.AXDir = origAXDir }()

	ws := &v1alpha1.Workspace{Metadata: &v1alpha1.ObjectMeta{Name: "worktree-ws"}}
	res, err := workspace.SetupWorkspace(context.Background(), ws, wtDir)
	if err != nil {
		t.Fatalf("setup failed: %v", err)
	}
	if !res.IsMaidenRun {
		t.Fatalf("expected maiden run on first setup")
	}
	if _, err := os.Stat(filepath.Join(wtDir, ".ax", workspace.InitializedMarkerFilename)); err != nil {
		t.Fatalf("expected marker in the workspace volume: %v", err)
	}

	res2, err := workspace.SetupWorkspace(context.Background(), ws, wtDir)
	if err != nil {
		t.Fatalf("second setup failed: %v", err)
	}
	if res2.IsMaidenRun {
		t.Errorf("expected no maiden run on the next boot")
	}
}

// TestSetupWorkspace_ReservedAxDirFilesSkipped checks that an inlined file aimed at
// the AX marker directory is not written, so it cannot replace the marker.
func TestSetupWorkspace_ReservedAxDirFilesSkipped(t *testing.T) {
	tempDir, err := os.MkdirTemp("", "ax-ws-reserved-*")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tempDir)

	origAXDir := workspace.AXDir
	workspace.AXDir = filepath.Join(tempDir, "ax-state-reserved")
	defer func() { workspace.AXDir = origAXDir }()

	ws := &v1alpha1.Workspace{
		Metadata: &v1alpha1.ObjectMeta{Name: "reserved-ws"},
		Spec: &v1alpha1.WorkspaceSpec{
			Files: []*v1alpha1.File{{Path: ".ax/initialized", Content: "user content"}},
		},
	}
	res, err := workspace.SetupWorkspace(context.Background(), ws, tempDir)
	if err != nil {
		t.Fatalf("setup failed: %v", err)
	}
	if !res.IsMaidenRun {
		t.Fatalf("expected maiden run on first setup")
	}
	data, err := os.ReadFile(filepath.Join(tempDir, ".ax", workspace.InitializedMarkerFilename))
	if err != nil {
		t.Fatalf("expected marker to exist: %v", err)
	}
	if strings.Contains(string(data), "user content") {
		t.Errorf("inlined file overwrote the marker")
	}

	res2, err := workspace.SetupWorkspace(context.Background(), ws, tempDir)
	if err != nil {
		t.Fatalf("second setup failed: %v", err)
	}
	if res2.IsMaidenRun {
		t.Errorf("expected no maiden run on second setup")
	}
}

// TestSetupWorkspace_FailsOnMarkerStatError checks that a marker path that cannot be
// checked (here .ax is a regular file) fails setup instead of rerunning it.
func TestSetupWorkspace_FailsOnMarkerStatError(t *testing.T) {
	tempDir, err := os.MkdirTemp("", "ax-ws-stat-*")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tempDir)

	if err := os.WriteFile(filepath.Join(tempDir, ".ax"), []byte("not a dir"), 0o644); err != nil {
		t.Fatal(err)
	}
	origAXDir := workspace.AXDir
	workspace.AXDir = filepath.Join(tempDir, "ax-state-stat")
	defer func() { workspace.AXDir = origAXDir }()

	ws := &v1alpha1.Workspace{Metadata: &v1alpha1.ObjectMeta{Name: "stat-ws"}}
	if _, err := workspace.SetupWorkspace(context.Background(), ws, tempDir); err == nil {
		t.Errorf("expected an error when the marker path cannot be checked")
	}
}

// TestSetupWorkspace_RetryKeepsAgentEdits checks that when one repo fails, the retry does
// not re-checkout the repo that succeeded or rewrite inlined files the agent edited.
func TestSetupWorkspace_RetryKeepsAgentEdits(t *testing.T) {
	tempDir, err := os.MkdirTemp("", "ax-ws-retry-*")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tempDir)

	srcDir, err := os.MkdirTemp("", "ax-src-retry-*")
	if err != nil {
		t.Fatalf("failed to create src dir: %v", err)
	}
	defer os.RemoveAll(srcDir)
	initCmd := exec.Command("sh", "-c", "git init -q -b main && git config user.email t@ax.io && git config user.name AX && echo base > tracked.txt && git add . && git commit -q -m init")
	initCmd.Dir = srcDir
	if out, err := initCmd.CombinedOutput(); err != nil {
		t.Fatalf("failed to init src repo: %v (%s)", err, string(out))
	}

	origAXDir := workspace.AXDir
	workspace.AXDir = filepath.Join(tempDir, "ax-state-retry")
	defer func() { workspace.AXDir = origAXDir }()

	target := filepath.Join(tempDir, "ws")
	ws := &v1alpha1.Workspace{
		Metadata: &v1alpha1.ObjectMeta{Name: "retry-ws"},
		Spec: &v1alpha1.WorkspaceSpec{
			Git: []*v1alpha1.GitRepo{
				{Name: "good", Repo: srcDir, Dir: "good"},
				{Name: "bad", Repo: filepath.Join(tempDir, "missing-repo"), Dir: "bad"},
			},
			Files: []*v1alpha1.File{{Path: "AGENTS.md", Content: "first"}},
		},
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	if _, err := workspace.SetupWorkspace(ctx, ws, target); err != nil {
		t.Fatalf("first setup failed: %v", err)
	}

	// The agent edits a tracked file and an inlined file, then the setup runs again.
	if err := os.WriteFile(filepath.Join(target, "good", "tracked.txt"), []byte("agent edit"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(target, "AGENTS.md"), []byte("agent edit"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := workspace.SetupWorkspace(ctx, ws, target); err != nil {
		t.Fatalf("retry setup failed: %v", err)
	}

	got, _ := os.ReadFile(filepath.Join(target, "good", "tracked.txt"))
	if string(got) != "agent edit" {
		t.Errorf("retry discarded the agent's edit to a tracked file: got %q", got)
	}
	agents, _ := os.ReadFile(filepath.Join(target, "AGENTS.md"))
	if string(agents) != "agent edit" {
		t.Errorf("retry overwrote the agent's inlined file: got %q", agents)
	}
}

// TestSetupWorkspace_RejectsEscapingFile checks that an inlined path cannot write outside
// the workspace, and that the failure withholds the marker.
func TestSetupWorkspace_RejectsEscapingFile(t *testing.T) {
	tempDir, err := os.MkdirTemp("", "ax-ws-escape-*")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tempDir)

	origAXDir := workspace.AXDir
	workspace.AXDir = filepath.Join(tempDir, "ax-state-escape")
	defer func() { workspace.AXDir = origAXDir }()

	target := filepath.Join(tempDir, "ws")
	ws := &v1alpha1.Workspace{
		Metadata: &v1alpha1.ObjectMeta{Name: "escape-ws"},
		Spec: &v1alpha1.WorkspaceSpec{
			Files: []*v1alpha1.File{{Path: "../escape.txt", Content: "outside"}},
		},
	}
	if _, err := workspace.SetupWorkspace(context.Background(), ws, target); err != nil {
		t.Fatalf("setup failed: %v", err)
	}
	if _, err := os.Stat(filepath.Join(tempDir, "escape.txt")); !os.IsNotExist(err) {
		t.Errorf("inlined file was written outside the workspace")
	}
	if _, err := os.Stat(filepath.Join(target, ".ax", workspace.InitializedMarkerFilename)); !os.IsNotExist(err) {
		t.Errorf("marker was written even though a file was rejected")
	}
}

// TestSetupWorkspace_RejectsSymlinkedPath checks that an inlined file cannot be written
// through a symbolic link that points outside the workspace.
func TestSetupWorkspace_RejectsSymlinkedPath(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("creating symbolic links needs extra privileges on Windows")
	}
	tempDir, err := os.MkdirTemp("", "ax-ws-symlink-*")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tempDir)

	outside := filepath.Join(tempDir, "outside")
	if err := os.MkdirAll(outside, 0o755); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(tempDir, "ws")
	if err := os.MkdirAll(target, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(target, "link")); err != nil {
		t.Fatal(err)
	}

	origAXDir := workspace.AXDir
	workspace.AXDir = filepath.Join(tempDir, "ax-state-symlink")
	defer func() { workspace.AXDir = origAXDir }()

	ws := &v1alpha1.Workspace{
		Metadata: &v1alpha1.ObjectMeta{Name: "symlink-ws"},
		Spec: &v1alpha1.WorkspaceSpec{
			Files: []*v1alpha1.File{{Path: "link/hooks.sh", Content: "bad"}},
		},
	}
	if _, err := workspace.SetupWorkspace(context.Background(), ws, target); err != nil {
		t.Fatalf("setup failed: %v", err)
	}
	if _, err := os.Stat(filepath.Join(outside, "hooks.sh")); !os.IsNotExist(err) {
		t.Errorf("inlined file was written through a symbolic link outside the workspace")
	}
}

// TestSetupWorkspace_AbsoluteFilePath checks that an absolute inlined path is written at
// that path when it is inside the workspace, and rejected when it is outside.
func TestSetupWorkspace_AbsoluteFilePath(t *testing.T) {
	tempDir, err := os.MkdirTemp("", "ax-ws-abs-*")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tempDir)

	origAXDir := workspace.AXDir
	workspace.AXDir = filepath.Join(tempDir, "ax-state-abs")
	defer func() { workspace.AXDir = origAXDir }()

	target := filepath.Join(tempDir, "ws")
	inside := filepath.Join(target, "config", "inside.txt")
	outside := filepath.Join(tempDir, "outside.txt")
	ws := &v1alpha1.Workspace{
		Metadata: &v1alpha1.ObjectMeta{Name: "abs-ws"},
		Spec: &v1alpha1.WorkspaceSpec{
			Files: []*v1alpha1.File{
				{Path: inside, Content: "inside"},
				{Path: outside, Content: "outside"},
			},
		},
	}
	if _, err := workspace.SetupWorkspace(context.Background(), ws, target); err != nil {
		t.Fatalf("setup failed: %v", err)
	}
	got, err := os.ReadFile(inside)
	if err != nil || string(got) != "inside" {
		t.Errorf("absolute path inside the workspace was not written where requested: %v %q", err, got)
	}
	if _, err := os.Stat(outside); !os.IsNotExist(err) {
		t.Errorf("absolute path outside the workspace was written")
	}
}

// TestSetupWorkspace_RefusesSymlinkedRoot checks that a workspace folder that is itself
// a symbolic link is refused.
func TestSetupWorkspace_RefusesSymlinkedRoot(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("creating symbolic links needs extra privileges on Windows")
	}
	tempDir, err := os.MkdirTemp("", "ax-ws-root-link-*")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tempDir)

	real := filepath.Join(tempDir, "real")
	if err := os.MkdirAll(real, 0o755); err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(tempDir, "ws")
	if err := os.Symlink(real, root); err != nil {
		t.Fatal(err)
	}
	origAXDir := workspace.AXDir
	workspace.AXDir = filepath.Join(tempDir, "ax-state-root")
	defer func() { workspace.AXDir = origAXDir }()

	ws := &v1alpha1.Workspace{Metadata: &v1alpha1.ObjectMeta{Name: "root-link-ws"}}
	if _, err := workspace.SetupWorkspace(context.Background(), ws, root); err == nil {
		t.Errorf("expected a symlinked workspace root to be refused")
	}
}

// TestSetupWorkspace_MarkerNotThroughOutsideGitDirLink checks that a .git file whose gitdir
// is lexically inside the workspace but is a symbolic link to a directory outside it does
// not send the marker outside the workspace volume.
func TestSetupWorkspace_MarkerNotThroughOutsideGitDirLink(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("creating symbolic links needs extra privileges on Windows")
	}
	tempDir, err := os.MkdirTemp("", "ax-ws-gitdir-link-*")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tempDir)

	outsideGit := filepath.Join(tempDir, "outside", "gitdir")
	if err := os.MkdirAll(outsideGit, 0o755); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(tempDir, "ws")
	if err := os.MkdirAll(target, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outsideGit, filepath.Join(target, "link-git")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(target, ".git"), []byte("gitdir: link-git\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	origAXDir := workspace.AXDir
	workspace.AXDir = filepath.Join(tempDir, "ax-state-gitdir")
	defer func() { workspace.AXDir = origAXDir }()

	ws := &v1alpha1.Workspace{Metadata: &v1alpha1.ObjectMeta{Name: "gitdir-link-ws"}}
	if _, err := workspace.SetupWorkspace(context.Background(), ws, target); err != nil {
		t.Fatalf("setup failed: %v", err)
	}
	if _, err := os.Stat(filepath.Join(outsideGit, ".ax", workspace.InitializedMarkerFilename)); !os.IsNotExist(err) {
		t.Errorf("marker was written outside the workspace through a symbolic link")
	}
	if _, err := os.Stat(filepath.Join(target, ".ax", workspace.InitializedMarkerFilename)); err != nil {
		t.Errorf("marker missing from the workspace fallback: %v", err)
	}
}

// TestSetupWorkspace_RefusesDanglingMarkerLink checks that a symbolic link at the marker
// path, even one whose target does not exist, is refused rather than followed.
func TestSetupWorkspace_RefusesDanglingMarkerLink(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("creating symbolic links needs extra privileges on Windows")
	}
	tempDir, err := os.MkdirTemp("", "ax-ws-dangling-*")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tempDir)

	outside := filepath.Join(tempDir, "outside-marker")
	target := filepath.Join(tempDir, "ws")
	if err := os.MkdirAll(filepath.Join(target, ".ax"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(target, ".ax", workspace.InitializedMarkerFilename)); err != nil {
		t.Fatal(err)
	}

	origAXDir := workspace.AXDir
	workspace.AXDir = filepath.Join(tempDir, "ax-state-dangling")
	defer func() { workspace.AXDir = origAXDir }()

	ws := &v1alpha1.Workspace{Metadata: &v1alpha1.ObjectMeta{Name: "dangling-ws"}}
	if _, err := workspace.SetupWorkspace(context.Background(), ws, target); err != nil {
		t.Fatalf("setup failed: %v", err)
	}
	if _, err := os.Stat(outside); !os.IsNotExist(err) {
		t.Errorf("marker was written through a dangling symbolic link")
	}
	if info, err := os.Lstat(filepath.Join(target, ".ax", workspace.InitializedMarkerFilename)); err != nil || !info.Mode().IsRegular() {
		t.Errorf("marker should be a regular file inside the workspace: %v", err)
	}
}

// TestSetupWorkspace_DirectoryCollisionNotMarked checks that a declared file whose path is
// already a directory leaves setup incomplete, so no maiden-run marker is written.
func TestSetupWorkspace_DirectoryCollisionNotMarked(t *testing.T) {
	tempDir := t.TempDir()
	target := filepath.Join(tempDir, "ws")
	if err := os.MkdirAll(filepath.Join(target, "config.yaml"), 0o755); err != nil {
		t.Fatal(err)
	}

	origAXDir := workspace.AXDir
	workspace.AXDir = filepath.Join(tempDir, "ax-state-dir")
	defer func() { workspace.AXDir = origAXDir }()

	ws := &v1alpha1.Workspace{
		Metadata: &v1alpha1.ObjectMeta{Name: "dir-collision-ws"},
		Spec:     &v1alpha1.WorkspaceSpec{Files: []*v1alpha1.File{{Path: "config.yaml", Content: "x"}}},
	}
	if _, err := workspace.SetupWorkspace(context.Background(), ws, target); err != nil {
		t.Fatalf("setup failed: %v", err)
	}
	if _, err := os.Stat(filepath.Join(target, ".ax", workspace.InitializedMarkerFilename)); !os.IsNotExist(err) {
		t.Errorf("marker written although a declared file was not created")
	}
}

// TestSetupWorkspace_RefusesSymlinkedMarkerDir checks that a .ax directory that is a link to
// a directory outside the workspace, holding an initialized marker, is not trusted.
func TestSetupWorkspace_RefusesSymlinkedMarkerDir(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("creating symbolic links needs extra privileges on Windows")
	}
	tempDir := t.TempDir()
	outside := filepath.Join(tempDir, "outside")
	if err := os.MkdirAll(outside, 0o755); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(outside, workspace.InitializedMarkerFilename)
	if err := os.WriteFile(marker, []byte("planted"), 0o644); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(tempDir, "ws")
	if err := os.MkdirAll(target, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(target, ".ax")); err != nil {
		t.Fatal(err)
	}

	origAXDir := workspace.AXDir
	workspace.AXDir = filepath.Join(tempDir, "ax-state-dir-link")
	defer func() { workspace.AXDir = origAXDir }()

	ws := &v1alpha1.Workspace{Metadata: &v1alpha1.ObjectMeta{Name: "marker-dir-link-ws"}}
	if _, err := workspace.SetupWorkspace(context.Background(), ws, target); err != nil {
		t.Fatalf("setup failed: %v", err)
	}
	if data, _ := os.ReadFile(marker); string(data) != "planted" {
		t.Errorf("marker outside the workspace was modified")
	}
	if info, err := os.Lstat(filepath.Join(target, ".ax")); err != nil || !info.IsDir() {
		t.Errorf(".ax should be a real directory after setup: %v", err)
	}
}

// TestSetupWorkspace_ForeignCheckoutNotCounted checks that a destination already holding a
// checkout of another repository is neither counted as the declared clone nor overwritten.
func TestSetupWorkspace_ForeignCheckoutNotCounted(t *testing.T) {
	tempDir := t.TempDir()
	target := filepath.Join(tempDir, "ws")
	dest := filepath.Join(target, "code")
	if err := os.MkdirAll(dest, 0o755); err != nil {
		t.Fatal(err)
	}
	git := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", dest, "-c", "user.name=t", "-c", "user.email=t@example.invalid"}, args...)...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	git("init", "-q")
	git("commit", "-q", "--allow-empty", "-m", "foreign")
	git("remote", "add", "origin", "https://example.invalid/foreign.git")

	origAXDir := workspace.AXDir
	workspace.AXDir = filepath.Join(tempDir, "ax-state-foreign")
	defer func() { workspace.AXDir = origAXDir }()

	ws := &v1alpha1.Workspace{
		Metadata: &v1alpha1.ObjectMeta{Name: "foreign-ws"},
		Spec: &v1alpha1.WorkspaceSpec{
			Git: []*v1alpha1.GitRepo{{Name: "wanted", Repo: "https://example.invalid/wanted.git", Dir: "code"}},
		},
	}
	if _, err := workspace.SetupWorkspace(context.Background(), ws, target); err != nil {
		t.Fatalf("setup failed: %v", err)
	}
	if _, err := os.Stat(filepath.Join(target, ".ax", workspace.InitializedMarkerFilename)); !os.IsNotExist(err) {
		t.Errorf("marker written although the declared repo was not checked out")
	}
	out, _ := exec.Command("git", "-C", dest, "config", "--get", "remote.origin.url").Output()
	if got := string(out); got != "https://example.invalid/foreign.git\n" {
		t.Errorf("foreign checkout's origin changed to %q", got)
	}
}

// TestSetupWorkspace_RefusesSymlinkedAncestor checks that a workspace whose parent directory
// is a symbolic link is refused, even when the workspace folder itself is a real directory.
func TestSetupWorkspace_RefusesSymlinkedAncestor(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("creating symbolic links needs extra privileges on Windows")
	}
	tempDir := t.TempDir()
	real := filepath.Join(tempDir, "real")
	if err := os.MkdirAll(real, 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(tempDir, "link")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(link, "ws")

	origAXDir := workspace.AXDir
	workspace.AXDir = filepath.Join(tempDir, "ax-state-ancestor")
	defer func() { workspace.AXDir = origAXDir }()

	ws := &v1alpha1.Workspace{Metadata: &v1alpha1.ObjectMeta{Name: "ancestor-link-ws"}}
	if _, err := workspace.SetupWorkspace(context.Background(), ws, root); err == nil {
		t.Errorf("expected a workspace under a symlinked parent to be refused")
	}
	if _, err := os.Stat(filepath.Join(real, "ws")); !os.IsNotExist(err) {
		t.Errorf("workspace was created through a symbolic link parent")
	}
}

// TestSetupWorkspace_InlineFileBeforeRootClone checks that an inlined file in the workspace
// root does not stop a clone of the same root.
func TestSetupWorkspace_InlineFileBeforeRootClone(t *testing.T) {
	tempDir := t.TempDir()
	srcDir := filepath.Join(tempDir, "src")
	if err := os.MkdirAll(srcDir, 0o755); err != nil {
		t.Fatal(err)
	}
	initCmd := exec.Command("sh", "-c", "git init -q && git symbolic-ref HEAD refs/heads/main && git config user.email t@ax.io && git config user.name AX && echo hi > README.md && git add . && git commit -qm init")
	initCmd.Dir = srcDir
	if out, err := initCmd.CombinedOutput(); err != nil {
		t.Fatalf("git init: %v (%s)", err, out)
	}

	target := filepath.Join(tempDir, "ws")
	origAXDir := workspace.AXDir
	workspace.AXDir = filepath.Join(tempDir, "ax-state-inline-clone")
	defer func() { workspace.AXDir = origAXDir }()

	ws := &v1alpha1.Workspace{
		Metadata: &v1alpha1.ObjectMeta{Name: "inline-clone-ws"},
		Spec: &v1alpha1.WorkspaceSpec{
			Files: []*v1alpha1.File{{Path: "a.txt", Content: "x"}},
			Git:   []*v1alpha1.GitRepo{{Name: "root", Repo: srcDir, Dir: "."}},
		},
	}
	res, err := workspace.SetupWorkspace(context.Background(), ws, target)
	if err != nil {
		t.Fatalf("setup failed: %v", err)
	}
	if len(res.ClonedRepos) != 1 {
		t.Errorf("root repo not cloned: %v", res.ClonedRepos)
	}
	if _, err := os.Stat(filepath.Join(target, ".ax", workspace.InitializedMarkerFilename)); err != nil {
		if _, err2 := os.Stat(filepath.Join(target, ".git", ".ax", workspace.InitializedMarkerFilename)); err2 != nil {
			t.Errorf("marker missing after a successful clone")
		}
	}
}

// initSourceRepo makes a git repo at dir with one committed file, README.md = content.
func initSourceRepo(t *testing.T, dir, content string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("sh", "-c", "git init -q && git symbolic-ref HEAD refs/heads/main && git config user.email t@ax.io && git config user.name AX && git add . && git commit -qm init")
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git init: %v (%s)", err, out)
	}
}

// TestSetupWorkspace_ResumesInterruptedCheckout checks that files left by an interrupted
// checkout, identical to the repo, do not block the clone on the next run.
func TestSetupWorkspace_ResumesInterruptedCheckout(t *testing.T) {
	tempDir := t.TempDir()
	src := filepath.Join(tempDir, "src")
	initSourceRepo(t, src, "hi\n")

	target := filepath.Join(tempDir, "ws")
	dest := filepath.Join(target, "code")
	if err := os.MkdirAll(dest, 0o755); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("sh", "-c", "git init -q && git remote add origin "+src)
	cmd.Dir = dest
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git init: %v (%s)", err, out)
	}
	if err := os.WriteFile(filepath.Join(dest, "README.md"), []byte("hi\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	origAXDir := workspace.AXDir
	workspace.AXDir = filepath.Join(tempDir, "ax-state-resume")
	defer func() { workspace.AXDir = origAXDir }()

	ws := &v1alpha1.Workspace{
		Metadata: &v1alpha1.ObjectMeta{Name: "resume-ws"},
		Spec:     &v1alpha1.WorkspaceSpec{Git: []*v1alpha1.GitRepo{{Name: "code", Repo: src, Dir: "code"}}},
	}
	res, err := workspace.SetupWorkspace(context.Background(), ws, target)
	if err != nil {
		t.Fatalf("setup failed: %v", err)
	}
	if len(res.ClonedRepos) != 1 {
		t.Errorf("repo not cloned after an interrupted checkout: %v", res.ClonedRepos)
	}
	if _, err := os.Stat(filepath.Join(target, ".ax", workspace.InitializedMarkerFilename)); err != nil {
		t.Errorf("marker missing after resuming the checkout: %v", err)
	}
}

// TestSetupWorkspace_KeepsEditedFileInRepoDir checks that a file in the repo's directory
// whose content differs from the repo is kept, and the checkout does not overwrite it.
func TestSetupWorkspace_KeepsEditedFileInRepoDir(t *testing.T) {
	tempDir := t.TempDir()
	src := filepath.Join(tempDir, "src")
	initSourceRepo(t, src, "repo\n")

	target := filepath.Join(tempDir, "ws")
	dest := filepath.Join(target, "code")
	if err := os.MkdirAll(dest, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dest, "README.md"), []byte("agent edit\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	origAXDir := workspace.AXDir
	workspace.AXDir = filepath.Join(tempDir, "ax-state-edit")
	defer func() { workspace.AXDir = origAXDir }()

	ws := &v1alpha1.Workspace{
		Metadata: &v1alpha1.ObjectMeta{Name: "edit-ws"},
		Spec:     &v1alpha1.WorkspaceSpec{Git: []*v1alpha1.GitRepo{{Name: "code", Repo: src, Dir: "code"}}},
	}
	res, err := workspace.SetupWorkspace(context.Background(), ws, target)
	if err != nil {
		t.Fatalf("setup failed: %v", err)
	}
	data, _ := os.ReadFile(filepath.Join(dest, "README.md"))
	if string(data) != "agent edit\n" {
		t.Errorf("agent edit overwritten: %q", data)
	}
	if len(res.ClonedRepos) != 0 {
		t.Errorf("repo with an edited file counted as cloned: %v", res.ClonedRepos)
	}
}

// TestSetupWorkspace_RefusesOptionLikeRepoValues checks that a branch or repo starting with
// "-" is never passed to git as an option, so a branch cannot run a command.
func TestSetupWorkspace_RefusesOptionLikeRepoValues(t *testing.T) {
	tempDir := t.TempDir()
	pwn := filepath.Join(tempDir, "pwn")

	target := filepath.Join(tempDir, "ws")
	origAXDir := workspace.AXDir
	workspace.AXDir = filepath.Join(tempDir, "ax-state-opt")
	defer func() { workspace.AXDir = origAXDir }()

	ws := &v1alpha1.Workspace{
		Metadata: &v1alpha1.ObjectMeta{Name: "opt-ws"},
		Spec: &v1alpha1.WorkspaceSpec{Git: []*v1alpha1.GitRepo{
			{Name: "branch", Repo: "https://example.invalid/r.git", Dir: "a", Branch: "--upload-pack=touch " + pwn},
			{Name: "repo", Repo: "--upload-pack=touch " + pwn, Dir: "b"},
		}},
	}
	res, err := workspace.SetupWorkspace(context.Background(), ws, target)
	if err != nil {
		t.Fatalf("setup failed: %v", err)
	}
	if len(res.ClonedRepos) != 0 {
		t.Errorf("option-like repo was cloned: %v", res.ClonedRepos)
	}
	if _, err := os.Stat(pwn); !os.IsNotExist(err) {
		t.Errorf("option value ran a command")
	}
}

// TestSetupWorkspace_GitAttemptTimeout checks that a git attempt past its deadline fails,
// so a stalled remote cannot block setup.
func TestSetupWorkspace_GitAttemptTimeout(t *testing.T) {
	tempDir := t.TempDir()
	src := filepath.Join(tempDir, "src")
	initSourceRepo(t, src, "hi\n")

	defer workspace.SetGitTimeoutForTest(time.Nanosecond)()

	origAXDir := workspace.AXDir
	workspace.AXDir = filepath.Join(tempDir, "ax-state-timeout")
	defer func() { workspace.AXDir = origAXDir }()

	ws := &v1alpha1.Workspace{
		Metadata: &v1alpha1.ObjectMeta{Name: "timeout-ws"},
		Spec:     &v1alpha1.WorkspaceSpec{Git: []*v1alpha1.GitRepo{{Name: "r", Repo: src, Dir: "code"}}},
	}
	res, err := workspace.SetupWorkspace(context.Background(), ws, filepath.Join(tempDir, "ws"))
	if err != nil {
		t.Fatalf("setup failed: %v", err)
	}
	if len(res.ClonedRepos) != 0 {
		t.Errorf("clone succeeded past its deadline: %v", res.ClonedRepos)
	}
}

// TestSetupWorkspace_RefusesAbsoluteDotDotPath checks that an absolute inlined path with ".."
// is refused, so a symbolic link on the way cannot redirect the write.
func TestSetupWorkspace_RefusesAbsoluteDotDotPath(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("creating symbolic links needs extra privileges on Windows")
	}
	tempDir := t.TempDir()
	outside := filepath.Join(tempDir, "outside")
	if err := os.MkdirAll(filepath.Join(outside, "deep"), 0o755); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(tempDir, "ws")
	if err := os.MkdirAll(target, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(outside, "deep"), filepath.Join(target, "link")); err != nil {
		t.Fatal(err)
	}

	origAXDir := workspace.AXDir
	workspace.AXDir = filepath.Join(tempDir, "ax-state-dotdot")
	defer func() { workspace.AXDir = origAXDir }()

	ws := &v1alpha1.Workspace{
		Metadata: &v1alpha1.ObjectMeta{Name: "dotdot-ws"},
		Spec: &v1alpha1.WorkspaceSpec{
			Files: []*v1alpha1.File{{Path: target + "/link/../evil.txt", Content: "bad"}},
		},
	}
	if _, err := workspace.SetupWorkspace(context.Background(), ws, target); err != nil {
		t.Fatalf("setup failed: %v", err)
	}
	if _, err := os.Stat(filepath.Join(outside, "evil.txt")); !os.IsNotExist(err) {
		t.Errorf("file written outside the workspace through a parent reference")
	}
}

// TestSetupWorkspace_AgentSymlinkOnMarkerPath checks that a symbolic link the workspace holds
// at .ax is removed, so the marker is recorded and the next boot is not blocked.
func TestSetupWorkspace_AgentSymlinkOnMarkerPath(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("creating symbolic links needs extra privileges on Windows")
	}
	tempDir := t.TempDir()
	target := filepath.Join(tempDir, "ws")
	if err := os.MkdirAll(filepath.Join(target, "elsewhere"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(target, "elsewhere"), filepath.Join(target, ".ax")); err != nil {
		t.Fatal(err)
	}

	origAXDir := workspace.AXDir
	workspace.AXDir = filepath.Join(tempDir, "ax-state-agent-link")
	defer func() { workspace.AXDir = origAXDir }()

	ws := &v1alpha1.Workspace{Metadata: &v1alpha1.ObjectMeta{Name: "agent-link-ws"}}
	if _, err := workspace.SetupWorkspace(context.Background(), ws, target); err != nil {
		t.Fatalf("setup failed: %v", err)
	}
	if info, err := os.Lstat(filepath.Join(target, ".ax")); err != nil || !info.IsDir() {
		t.Errorf(".ax should be a real directory after setup: %v", err)
	}
	if _, err := os.Stat(filepath.Join(target, ".ax", workspace.InitializedMarkerFilename)); err != nil {
		t.Errorf("marker missing: %v", err)
	}
}

// TestSetupWorkspace_IgnoresLegacyMarkerOfOtherWorkspace checks that a legacy marker written
// for a different workspace does not suppress this workspace's setup.
func TestSetupWorkspace_IgnoresLegacyMarkerOfOtherWorkspace(t *testing.T) {
	tempDir := t.TempDir()
	target := filepath.Join(tempDir, "ws")
	axDir := filepath.Join(tempDir, "ax-state-legacy")
	if err := os.MkdirAll(axDir, 0o755); err != nil {
		t.Fatal(err)
	}
	legacy := filepath.Join(axDir, workspace.LegacyMarkerNameForTest(target))
	if err := os.WriteFile(legacy, []byte("workspace: other-ws\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	origAXDir := workspace.AXDir
	workspace.AXDir = axDir
	defer func() { workspace.AXDir = origAXDir }()

	ws := &v1alpha1.Workspace{
		Metadata: &v1alpha1.ObjectMeta{Name: "this-ws"},
		Spec:     &v1alpha1.WorkspaceSpec{Files: []*v1alpha1.File{{Path: "x.txt", Content: "x"}}},
	}
	if _, err := workspace.SetupWorkspace(context.Background(), ws, target); err != nil {
		t.Fatalf("setup failed: %v", err)
	}
	if _, err := os.Stat(filepath.Join(target, "x.txt")); err != nil {
		t.Errorf("setup was skipped because of another workspace's legacy marker: %v", err)
	}
}

// TestSetupWorkspace_StalledFetchIsCutOff checks that a fetch that never finishes is stopped
// by the per-attempt timeout, so setup returns instead of blocking the task forever.
func TestSetupWorkspace_StalledFetchIsCutOff(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses a shell script as a stand-in for git")
	}
	tempDir := t.TempDir()
	src := filepath.Join(tempDir, "src")
	initSourceRepo(t, src, "hi\n")

	realGit, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	// A git stand-in that stalls on fetch. exec replaces the shell, so the timeout kills the
	// process that holds the output pipe.
	fakeDir := t.TempDir()
	script := "#!/bin/sh\nif [ \"$1\" = fetch ]; then exec sleep 30; fi\nexec " + realGit + " \"$@\"\n"
	if err := os.WriteFile(filepath.Join(fakeDir, "git"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", fakeDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	defer workspace.SetGitTimeoutForTest(200 * time.Millisecond)()

	origAXDir := workspace.AXDir
	workspace.AXDir = filepath.Join(tempDir, "ax-state-stall")
	defer func() { workspace.AXDir = origAXDir }()

	ws := &v1alpha1.Workspace{
		Metadata: &v1alpha1.ObjectMeta{Name: "stall-ws"},
		Spec:     &v1alpha1.WorkspaceSpec{Git: []*v1alpha1.GitRepo{{Name: "r", Repo: src, Dir: "code"}}},
	}
	start := time.Now()
	res, err := workspace.SetupWorkspace(context.Background(), ws, filepath.Join(tempDir, "ws"))
	if err != nil {
		t.Fatalf("setup failed: %v", err)
	}
	// Without the timeout each attempt would wait 30s, so the bound below separates the two.
	if elapsed := time.Since(start); elapsed > 20*time.Second {
		t.Errorf("setup took %v; a stalled fetch was not cut off", elapsed)
	}
	if len(res.ClonedRepos) != 0 {
		t.Errorf("stalled repo counted as cloned: %v", res.ClonedRepos)
	}
}

// TestSetupWorkspace_KeepsMatchingCheckout checks that a checkout whose origin is the declared
// repo is counted as cloned and left as is, including local edits to tracked files.
func TestSetupWorkspace_KeepsMatchingCheckout(t *testing.T) {
	tempDir := t.TempDir()
	src := filepath.Join(tempDir, "src")
	initSourceRepo(t, src, "repo\n")

	target := filepath.Join(tempDir, "ws")
	dest := filepath.Join(target, "code")
	if out, err := exec.Command("git", "clone", "-q", src, dest).CombinedOutput(); err != nil {
		t.Fatalf("git clone: %v (%s)", err, out)
	}
	if err := os.WriteFile(filepath.Join(dest, "README.md"), []byte("local edit\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	origAXDir := workspace.AXDir
	workspace.AXDir = filepath.Join(tempDir, "ax-state-matching")
	defer func() { workspace.AXDir = origAXDir }()

	ws := &v1alpha1.Workspace{
		Metadata: &v1alpha1.ObjectMeta{Name: "matching-ws"},
		Spec:     &v1alpha1.WorkspaceSpec{Git: []*v1alpha1.GitRepo{{Name: "code", Repo: src, Dir: "code"}}},
	}
	res, err := workspace.SetupWorkspace(context.Background(), ws, target)
	if err != nil {
		t.Fatalf("setup failed: %v", err)
	}
	if len(res.ClonedRepos) != 1 {
		t.Errorf("matching checkout not counted as cloned: %v", res.ClonedRepos)
	}
	if data, _ := os.ReadFile(filepath.Join(dest, "README.md")); string(data) != "local edit\n" {
		t.Errorf("local edit to a matching checkout was overwritten: %q", data)
	}
}

// TestSetupWorkspace_BoundsExistingCheckoutProbe checks that a stalled git config read during
// a retry is cut off by the per-attempt timeout, instead of holding setup forever.
func TestSetupWorkspace_BoundsExistingCheckoutProbe(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses a shell script as a stand-in for git")
	}
	tempDir := t.TempDir()
	src := filepath.Join(tempDir, "src")
	initSourceRepo(t, src, "hi\n")

	target := filepath.Join(tempDir, "ws")
	dest := filepath.Join(target, "code")
	if out, err := exec.Command("git", "clone", "-q", src, dest).CombinedOutput(); err != nil {
		t.Fatalf("git clone: %v (%s)", err, out)
	}

	realGit, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	// A git stand-in that stalls on config. exec replaces the shell, so the timeout kills it.
	fakeDir := t.TempDir()
	script := "#!/bin/sh\nif [ \"$1\" = config ]; then exec sleep 30; fi\nexec " + realGit + " \"$@\"\n"
	if err := os.WriteFile(filepath.Join(fakeDir, "git"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", fakeDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	defer workspace.SetGitTimeoutForTest(200 * time.Millisecond)()

	origAXDir := workspace.AXDir
	workspace.AXDir = filepath.Join(tempDir, "ax-state-probe")
	defer func() { workspace.AXDir = origAXDir }()

	ws := &v1alpha1.Workspace{
		Metadata: &v1alpha1.ObjectMeta{Name: "probe-ws"},
		Spec:     &v1alpha1.WorkspaceSpec{Git: []*v1alpha1.GitRepo{{Name: "code", Repo: src, Dir: "code"}}},
	}
	start := time.Now()
	res, err := workspace.SetupWorkspace(context.Background(), ws, target)
	if err != nil {
		t.Fatalf("setup failed: %v", err)
	}
	// Without the bound each probe would wait 30s, so the limit below separates the two.
	if elapsed := time.Since(start); elapsed > 20*time.Second {
		t.Errorf("setup took %v; a stalled config probe was not cut off", elapsed)
	}
	if len(res.ClonedRepos) != 0 {
		t.Errorf("repo counted as cloned while its probe was stalled: %v", res.ClonedRepos)
	}
}

// TestSetupWorkspace_KeepsExecBitEdit checks that a file whose bytes match the repo but whose
// executable bit was changed locally is kept during a resumed checkout.
func TestSetupWorkspace_KeepsExecBitEdit(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("file mode bits are not meaningful on Windows")
	}
	tempDir := t.TempDir()
	src := filepath.Join(tempDir, "src")
	initSourceRepo(t, src, "hi\n")

	target := filepath.Join(tempDir, "ws")
	dest := filepath.Join(target, "code")
	if err := os.MkdirAll(dest, 0o755); err != nil {
		t.Fatal(err)
	}
	initCmd := exec.Command("sh", "-c", "git init -q && git remote add origin "+src)
	initCmd.Dir = dest
	if out, err := initCmd.CombinedOutput(); err != nil {
		t.Fatalf("git init: %v (%s)", err, out)
	}
	file := filepath.Join(dest, "README.md")
	if err := os.WriteFile(file, []byte("hi\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(file, 0o755); err != nil {
		t.Fatal(err)
	}

	origAXDir := workspace.AXDir
	workspace.AXDir = filepath.Join(tempDir, "ax-state-exec")
	defer func() { workspace.AXDir = origAXDir }()

	ws := &v1alpha1.Workspace{
		Metadata: &v1alpha1.ObjectMeta{Name: "exec-ws"},
		Spec:     &v1alpha1.WorkspaceSpec{Git: []*v1alpha1.GitRepo{{Name: "code", Repo: src, Dir: "code"}}},
	}
	res, err := workspace.SetupWorkspace(context.Background(), ws, target)
	if err != nil {
		t.Fatalf("setup failed: %v", err)
	}
	info, err := os.Stat(file)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm()&0o111 == 0 {
		t.Errorf("executable-bit edit was discarded")
	}
	if len(res.ClonedRepos) != 0 {
		t.Errorf("repo counted as cloned over a locally edited file: %v", res.ClonedRepos)
	}
}

// TestSetupWorkspace_ResumesCRLFCheckout checks that a file left by an interrupted checkout is
// recognised when the repo converts line endings: its CRLF bytes are the checkout output.
func TestSetupWorkspace_ResumesCRLFCheckout(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses a shell to build the repos")
	}
	tempDir := t.TempDir()
	src := filepath.Join(tempDir, "src")
	if err := os.MkdirAll(src, 0o755); err != nil {
		t.Fatal(err)
	}
	mk := exec.Command("sh", "-c", "git init -q && git symbolic-ref HEAD refs/heads/main && git config user.email t@ax.io && git config user.name AX && printf 'hi\n' > README.md && printf 'README.md text eol=crlf\n' > .gitattributes && git add . && git commit -qm init")
	mk.Dir = src
	if out, err := mk.CombinedOutput(); err != nil {
		t.Fatalf("git init: %v (%s)", err, out)
	}

	target := filepath.Join(tempDir, "ws")
	dest := filepath.Join(target, "code")
	if err := os.MkdirAll(dest, 0o755); err != nil {
		t.Fatal(err)
	}
	initCmd := exec.Command("sh", "-c", "git init -q && git remote add origin "+src)
	initCmd.Dir = dest
	if out, err := initCmd.CombinedOutput(); err != nil {
		t.Fatalf("git init dest: %v (%s)", err, out)
	}
	if err := os.WriteFile(filepath.Join(dest, "README.md"), []byte("hi\r\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	origAXDir := workspace.AXDir
	workspace.AXDir = filepath.Join(tempDir, "ax-state-crlf")
	defer func() { workspace.AXDir = origAXDir }()

	ws := &v1alpha1.Workspace{
		Metadata: &v1alpha1.ObjectMeta{Name: "crlf-ws"},
		Spec:     &v1alpha1.WorkspaceSpec{Git: []*v1alpha1.GitRepo{{Name: "code", Repo: src, Dir: "code"}}},
	}
	res, err := workspace.SetupWorkspace(context.Background(), ws, target)
	if err != nil {
		t.Fatalf("setup failed: %v", err)
	}
	if len(res.ClonedRepos) != 1 {
		t.Errorf("CRLF checkout left by an interrupted run was not resumed: %v", res.ClonedRepos)
	}
}
