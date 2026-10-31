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
	if err := os.WriteFile(filepath.Join(stateDir, legacy), []byte("old"), 0o644); err != nil {
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
