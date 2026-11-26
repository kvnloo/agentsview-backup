package sync

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/dbtest"
	"go.kenn.io/agentsview/internal/export"
	"go.kenn.io/agentsview/internal/parser"
	"go.kenn.io/agentsview/internal/testjsonl"
)

type failingCodexDiscoveryFactory struct {
	parser.ProviderFactory
	fail *bool
}

func (f failingCodexDiscoveryFactory) NewProvider(cfg parser.ProviderConfig) parser.Provider {
	return failingCodexDiscoveryProvider{Provider: f.ProviderFactory.NewProvider(cfg), fail: f.fail}
}

type failingCodexDiscoveryProvider struct {
	parser.Provider
	fail *bool
}

func (p failingCodexDiscoveryProvider) Discover(ctx context.Context) ([]parser.SourceRef, error) {
	if *p.fail {
		return nil, errors.New("discovery failed")
	}
	return p.Provider.Discover(ctx)
}

func (p failingCodexDiscoveryProvider) DiscoverEach(ctx context.Context, yield func(parser.SourceRef) error) error {
	if *p.fail {
		return errors.New("discovery failed")
	}
	return p.Provider.(parser.StreamingDiscoverer).DiscoverEach(ctx, yield)
}

func TestCodexRecordedRepositorySurvivesMissingCheckout(t *testing.T) {
	database := openTestDB(t)
	root := t.TempDir()
	id := "cccccccc-cccc-4ccc-8ccc-cccccccccccc"
	source := filepath.Join(root, "rollout-"+id+".jsonl")
	meta := testjsonl.CodexSessionMetaWithFieldsJSON(
		id, filepath.Join(root, "removed-checkout"), "codex_cli_rs", "2026-10-08T12:00:00Z",
		map[string]any{"git": map[string]any{"repository_url": "https://example.com/team/api.git", "branch": "feature"}},
	)
	dbtest.WriteTestFile(t, source, []byte(meta+"\n"+
		testjsonl.CodexMsgJSON("user", "add rate limiting", "2026-10-08T12:00:01Z")+"\n"))
	engine := NewEngine(t.Context(), database, EngineConfig{Machine: "local", AgentDirs: map[parser.AgentType][]string{parser.AgentCodex: {root}}})
	t.Cleanup(engine.Close)
	require.Equal(t, 1, engine.SyncAll(t.Context(), nil).Synced)
	snapshots, err := database.ListSessionProjectIdentitySnapshots(t.Context())
	require.NoError(t, err)
	require.Len(t, snapshots, 1)
	assert.Equal(t, "codex:"+id, snapshots[0].SessionID)
	assert.Equal(t, "example.com/team/api", snapshots[0].NormalizedRemote)
	assert.Equal(t, "git_remote", snapshots[0].KeySource)
}

func TestCodexLabelRefreshPreservesNormalizedCheckoutProject(t *testing.T) {
	const id = "eeeeeeee-eeee-4eee-8eee-eeeeeeeeeeee"
	database := openTestDB(t)
	parent := t.TempDir()
	checkout := filepath.Join(parent, "my-app")
	cwd := filepath.Join(checkout, "working-subdirectory")
	require.NoError(t, os.MkdirAll(filepath.Join(checkout, ".git"), 0o755))
	require.NoError(t, os.MkdirAll(cwd, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(checkout, ".git", "config"), []byte("[remote \"origin\"]\nurl = https://example.com/team/my-app.git\n"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(checkout, ".git", "HEAD"), []byte("ref: refs/heads/main\n"), 0o644))
	root := filepath.Join(parent, "logs")
	sessions := filepath.Join(root, "my_app", ".codex-run-v1-abc", "sessions")
	source := filepath.Join(sessions, "2026", "10", "08", "rollout-2026-10-08T12-00-00-"+id+".jsonl")
	meta := testjsonl.CodexSessionMetaWithFieldsJSON(
		id, cwd, "codex_exec", "2026-10-08T12:00:00Z",
		map[string]any{"git": map[string]any{"repository_url": "https://example.com/team/my-app.git"}},
	)
	dbtest.WriteTestFile(t, source, []byte(meta+"\n"+testjsonl.CodexMsgJSON("user", "review", "2026-10-08T12:00:01Z")+"\n"))
	engine := NewEngine(t.Context(), database, EngineConfig{
		Machine: "local", AgentDirs: map[parser.AgentType][]string{parser.AgentCodex: {root}},
	})
	t.Cleanup(engine.Close)
	require.Equal(t, 1, engine.SyncAll(t.Context(), nil).Synced)
	snapshots, err := database.ListSessionProjectIdentitySnapshotsByID(t.Context(), []string{"codex:" + id})
	require.NoError(t, err)
	require.NotEmpty(t, snapshots["codex:"+id].RepositoryPath)

	movedCheckout := filepath.Join(parent, "moved-checkout")
	require.NoError(t, os.Rename(checkout, movedCheckout))
	engine.ReconfigureSources(SourceConfig{
		AgentDirs: map[parser.AgentType][]string{parser.AgentCodex: {sessions}},
	})
	stats := engine.SyncAll(t.Context(), nil)
	require.Zero(t, stats.Failed)
	session, err := database.GetSessionFull(t.Context(), "codex:"+id)
	require.NoError(t, err)
	require.NotNil(t, session)
	assert.Equal(t, "my_app", session.Project)
	assert.Zero(t, engine.SyncAll(t.Context(), nil).Synced)
}

func TestCodexLabelRefreshPreservesMissingLinkedWorktree(t *testing.T) {
	for _, rebuild := range []bool{false, true} {
		t.Run(fmt.Sprintf("rebuild=%t", rebuild), func(t *testing.T) {
			const id = "eeeeeeee-eeee-4eee-8eee-eeeeeeeeeeee"
			database := openTestDB(t)
			parent := t.TempDir()
			repo := filepath.Join(parent, "api")
			worktree := filepath.Join(parent, "feature-wt")
			gitdir := filepath.Join(repo, ".git", "worktrees", "feature-wt")
			dbtest.WriteTestFile(t, filepath.Join(repo, ".git", "config"), []byte("[remote \"origin\"]\nurl = https://example.com/team/api.git\n"))
			dbtest.WriteTestFile(t, filepath.Join(repo, ".git", "HEAD"), []byte("ref: refs/heads/main\n"))
			dbtest.WriteTestFile(t, filepath.Join(gitdir, "commondir"), []byte("../..\n"))
			dbtest.WriteTestFile(t, filepath.Join(worktree, ".git"), []byte("gitdir: "+gitdir+"\n"))
			root := filepath.Join(parent, "logs")
			sessions := filepath.Join(root, "api", ".codex-run-v1-abc", "sessions")
			source := filepath.Join(sessions, "2026", "10", "08", "rollout-2026-10-08T12-00-00-"+id+".jsonl")
			meta := testjsonl.CodexSessionMetaWithFieldsJSON(id, worktree, "codex_exec", "2026-10-08T12:00:00Z", map[string]any{"git": map[string]any{"repository_url": "https://example.com/team/api.git"}})
			dbtest.WriteTestFile(t, source, []byte(meta+"\n"+testjsonl.CodexMsgJSON("user", "review", "2026-10-08T12:00:01Z")+"\n"))
			engine := NewEngine(t.Context(), database, EngineConfig{Machine: "local", AgentDirs: map[parser.AgentType][]string{parser.AgentCodex: {root}}})
			t.Cleanup(engine.Close)
			require.Equal(t, 1, engine.SyncAll(t.Context(), nil).Synced)
			snapshots, err := database.ListSessionProjectIdentitySnapshotsByID(t.Context(), []string{"codex:" + id})
			require.NoError(t, err)
			assert.Equal(t, repo, snapshots["codex:"+id].RepositoryPath)
			require.NoError(t, os.Rename(repo, filepath.Join(parent, "moved-repo")))
			require.NoError(t, os.Rename(worktree, filepath.Join(parent, "moved-worktree")))
			engine.ReconfigureSources(SourceConfig{AgentDirs: map[parser.AgentType][]string{parser.AgentCodex: {sessions}}})
			var stats SyncStats
			if rebuild {
				stats = engine.ResyncAll(t.Context(), nil)
			} else {
				stats = engine.SyncAll(t.Context(), nil)
			}
			require.False(t, stats.Aborted, "%v", stats.Warnings)
			require.Zero(t, stats.Failed)
			session, err := database.GetSessionFull(t.Context(), "codex:"+id)
			require.NoError(t, err)
			require.NotNil(t, session)
			assert.Equal(t, "api", session.Project)
		})
	}
}

func TestCodexLabelRefreshAfterRootChange(t *testing.T) {
	const id = "cccccccc-cccc-4ccc-8ccc-cccccccccccc"
	type fixture struct {
		database               *db.DB
		engine                 *Engine
		root, sessions, parent string
	}
	setup := func(t *testing.T, initiallyLabeled bool, cwdName string) fixture {
		t.Helper()
		database := openTestDB(t)
		parent := t.TempDir()
		root := filepath.Join(parent, "logs")
		inner := filepath.Join(root, "team")
		sessions := filepath.Join(inner, ".codex-run-v1-abc", "sessions")
		source := filepath.Join(sessions, "2026", "10", "08", "rollout-2026-10-08T12-00-00-"+id+".jsonl")
		meta := testjsonl.CodexSessionMetaWithFieldsJSON(id, filepath.Join(parent, cwdName), "codex_exec", "2026-10-08T12:00:00Z", map[string]any{"git": map[string]any{"repository_url": "https://example.com/team/api.git"}})
		dbtest.WriteTestFile(t, source, []byte(meta+"\n"+testjsonl.CodexMsgJSON("user", "review", "2026-10-08T12:00:01Z")+"\n"))
		initialRoot := sessions
		if initiallyLabeled {
			initialRoot = root
		}
		engine := NewEngine(t.Context(), database, EngineConfig{Machine: "local", AgentDirs: map[parser.AgentType][]string{parser.AgentCodex: {initialRoot}}})
		t.Cleanup(engine.Close)
		require.Equal(t, 1, engine.SyncAll(t.Context(), nil).Synced)
		engine.Close()
		engine = NewEngine(t.Context(), database, EngineConfig{Machine: "local", AgentDirs: map[parser.AgentType][]string{parser.AgentCodex: {root}}})
		t.Cleanup(engine.Close)
		return fixture{database, engine, root, sessions, parent}
	}
	check := func(t *testing.T, f fixture, stats SyncStats, label string) {
		t.Helper()
		require.Zero(t, stats.Failed)
		assert.Equal(t, 1, stats.Synced)
		session, err := f.database.GetSessionFull(t.Context(), "codex:"+id)
		require.NoError(t, err)
		require.NotNil(t, session)
		assert.Equal(t, label, session.Project)
		assert.Zero(t, f.engine.SyncAll(t.Context(), nil).Synced)
	}
	neverLabeledProject := func(t *testing.T, f fixture) (string, string) {
		t.Helper()
		database := openTestDB(t)
		engine := NewEngine(t.Context(), database, EngineConfig{Machine: "local", AgentDirs: map[parser.AgentType][]string{parser.AgentCodex: {f.sessions}}})
		t.Cleanup(engine.Close)
		require.Equal(t, 1, engine.SyncAll(t.Context(), nil).Synced)
		session, err := database.GetSessionFull(t.Context(), "codex:"+id)
		require.NoError(t, err)
		require.NotNil(t, session)
		snapshots, err := database.ListSessionProjectIdentitySnapshotsByID(t.Context(), []string{"codex:" + id})
		require.NoError(t, err)
		return session.Project, snapshots["codex:"+id].Project
	}
	t.Run("narrow with temporarily unavailable source", func(t *testing.T) {
		f := setup(t, true, "team")
		expected, _ := neverLabeledProject(t, f)
		missing := filepath.Join(f.parent, "temporarily-unavailable")
		require.NoError(t, os.Rename(f.sessions, missing))
		f.engine.ReconfigureSources(SourceConfig{AgentDirs: map[parser.AgentType][]string{parser.AgentCodex: {f.sessions}}})
		assert.Zero(t, f.engine.SyncAll(t.Context(), nil).Synced)
		require.NoError(t, os.Rename(missing, f.sessions))
		check(t, f, f.engine.SyncAll(t.Context(), nil), expected)
	})
	for _, missingSignature := range []bool{false, true} {
		t.Run(fmt.Sprintf("rebuild with temporarily unavailable source missing signature=%t", missingSignature), func(t *testing.T) {
			f := setup(t, true, "team")
			expected, _ := neverLabeledProject(t, f)
			other := filepath.Join(f.parent, "ordinary")
			otherID := "dddddddd-dddd-4ddd-8ddd-dddddddddddd"
			otherSource := filepath.Join(other, "2026", "10", "08", "rollout-2026-10-08T12-00-00-"+otherID+".jsonl")
			meta := testjsonl.CodexSessionMetaJSON(otherID, "/work/checkout", "codex_exec", "2026-10-08T12:00:00Z")
			dbtest.WriteTestFile(t, otherSource, []byte(meta+"\n"+testjsonl.CodexMsgJSON("user", "review", "2026-10-08T12:00:01Z")+"\n"))
			f.engine.ReconfigureSources(SourceConfig{AgentDirs: map[parser.AgentType][]string{parser.AgentCodex: {f.root, other}}})
			f.engine.SyncAll(t.Context(), nil)
			if missingSignature {
				require.NoError(t, f.database.DeleteSyncState(t.Context(), codexLabelRootsKey))
			}
			missing := filepath.Join(f.parent, "temporarily-unavailable")
			require.NoError(t, os.Rename(f.sessions, missing))
			f.engine.ReconfigureSources(SourceConfig{AgentDirs: map[parser.AgentType][]string{parser.AgentCodex: {f.sessions, other}}})
			stats := f.engine.ResyncAll(t.Context(), nil)
			require.False(t, stats.Aborted, "%v", stats.Warnings)
			require.Zero(t, stats.Failed)
			require.NoError(t, os.Rename(missing, f.sessions))
			check(t, f, f.engine.SyncAll(t.Context(), nil), expected)
		})
	}
	t.Run("narrowed with missing signature", func(t *testing.T) {
		f := setup(t, true, "team")
		other := filepath.Join(f.parent, "ordinary")
		otherID := "dddddddd-dddd-4ddd-8ddd-dddddddddddd"
		source := filepath.Join(other, "2026", "10", "08", "rollout-2026-10-08T12-00-00-"+otherID+".jsonl")
		meta := testjsonl.CodexSessionMetaJSON(otherID, "/work/checkout", "codex_exec", "2026-10-08T12:00:00Z")
		dbtest.WriteTestFile(t, source, []byte(meta+"\n"+testjsonl.CodexMsgJSON("user", "review", "2026-10-08T12:00:01Z")+"\n"))
		f.engine.ReconfigureSources(SourceConfig{AgentDirs: map[parser.AgentType][]string{parser.AgentCodex: {f.root, other}}})
		require.Equal(t, 1, f.engine.SyncAll(t.Context(), nil).Synced)
		require.NoError(t, f.database.DeleteSyncState(t.Context(), codexLabelRootsKey))
		expected, _ := neverLabeledProject(t, f)
		f.engine.ReconfigureSources(SourceConfig{AgentDirs: map[parser.AgentType][]string{parser.AgentCodex: {f.sessions, other}}})
		stats := f.engine.SyncAll(t.Context(), nil)
		require.Zero(t, stats.Failed)
		assert.Equal(t, 1, stats.Synced)
		session, err := f.database.GetSessionFull(t.Context(), "codex:"+id)
		require.NoError(t, err)
		require.NotNil(t, session)
		assert.Equal(t, expected, session.Project)
		assert.NotEqual(t, parser.SessionKindRoborev, session.SessionKind)
		assert.Zero(t, f.engine.SyncAll(t.Context(), nil).Synced)
	})
	t.Run("other provider discovery failure", func(t *testing.T) {
		f := setup(t, false, "team")
		other := t.TempDir()
		f.engine.ReconfigureSources(SourceConfig{AgentDirs: map[parser.AgentType][]string{parser.AgentCodex: {f.root}, parser.AgentClaude: {other}}})
		fail := true
		factories := f.engine.sources().providerFactories
		factories[parser.AgentClaude] = failingCodexDiscoveryFactory{factories[parser.AgentClaude], &fail}
		stats := f.engine.SyncAll(t.Context(), nil)
		assert.Positive(t, stats.providerFailures)
		assert.Equal(t, 1, stats.Synced)
		session, err := f.database.GetSessionFull(t.Context(), "codex:"+id)
		require.NoError(t, err)
		assert.Equal(t, "team", session.Project)
		assert.Zero(t, f.engine.SyncAll(t.Context(), nil).Synced)
	})
	t.Run("widen with rebuild", func(t *testing.T) {
		f := setup(t, false, "team")
		stats := f.engine.ResyncAll(t.Context(), nil)
		f.engine.SyncAll(t.Context(), nil)
		check(t, f, stats, "team")
	})
	t.Run("narrow without checkout rebuild", func(t *testing.T) {
		f := setup(t, true, "checkout")
		before, err := f.database.ListSessionProjectIdentitySnapshotsByID(t.Context(), []string{"codex:" + id})
		require.NoError(t, err)
		expected, _ := neverLabeledProject(t, f)
		f.engine.ReconfigureSources(SourceConfig{AgentDirs: map[parser.AgentType][]string{parser.AgentCodex: {f.sessions}}})
		stats := f.engine.ResyncAll(t.Context(), nil)
		require.False(t, stats.Aborted, "%v", stats.Warnings)
		require.Zero(t, stats.Failed)
		session, err := f.database.GetSessionFull(t.Context(), "codex:"+id)
		require.NoError(t, err)
		require.NotNil(t, session)
		assert.Equal(t, expected, session.Project)
		after, err := f.database.ListSessionProjectIdentitySnapshotsByID(t.Context(), []string{"codex:" + id})
		require.NoError(t, err)
		previous := before["codex:"+id]
		previous.Project = expected
		assert.Equal(t, previous, after["codex:"+id])
		observations, err := f.database.ListProjectIdentityObservations(t.Context(), []string{"team"})
		require.NoError(t, err)
		assert.Empty(t, observations)
		stats = f.engine.ResyncAll(t.Context(), nil)
		require.False(t, stats.Aborted, "%v", stats.Warnings)
		session, err = f.database.GetSessionFull(t.Context(), "codex:"+id)
		require.NoError(t, err)
		require.NotNil(t, session)
		assert.Equal(t, expected, session.Project)
	})
}

func TestCodexRemoteWorktreeProjectSurvivesAppendAndRebuild(t *testing.T) {
	for _, cwd := range []string{
		"/remote/code/sample-root-package",
		"/remote/code/sample/.worktrees/feature",
		"/remote/.config/superpowers/worktrees/sample/feature",
		"/remote/scratchpad/pr123",
	} {
		t.Run(cwd, func(t *testing.T) {
			database := openTestDB(t)
			root := t.TempDir()
			id := "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb"
			source := filepath.Join(root, "rollout-"+id+".jsonl")
			meta := testjsonl.CodexSessionMetaWithFieldsJSON(id, cwd, "codex_cli_rs", "2026-10-08T12:00:00Z",
				map[string]any{"git": map[string]any{"repository_url": "https://example.com/team/sample.git", "branch": "embedding/root-package"}})
			dbtest.WriteTestFile(t, source, []byte(meta+"\n"+
				testjsonl.CodexMsgJSON("user", "add rate limiting", "2026-10-08T12:00:01Z")+"\n"))
			engine := NewEngine(t.Context(), database, EngineConfig{
				Machine: "remote", DisableFilesystemProjectDiscovery: true,
				AgentDirs: map[parser.AgentType][]string{parser.AgentCodex: {root}},
			})
			t.Cleanup(engine.Close)
			require.Equal(t, 1, engine.SyncAll(t.Context(), nil).Synced)
			session, err := database.GetSessionFull(t.Context(), "codex:"+id)
			require.NoError(t, err)
			require.NotNil(t, session)
			assert.Equal(t, "sample", session.Project)

			f, err := os.OpenFile(source, os.O_APPEND|os.O_WRONLY, 0o600)
			require.NoError(t, err)
			_, err = f.WriteString(testjsonl.CodexMsgJSON("assistant", "added a limiter", "2026-10-08T12:00:02Z") + "\n")
			require.NoError(t, err)
			require.NoError(t, f.Close())
			require.Equal(t, 1, engine.SyncAll(t.Context(), nil).Synced)
			session, err = database.GetSessionFull(t.Context(), "codex:"+id)
			require.NoError(t, err)
			assert.True(t, session.LastWriteIncremental)
			assert.Equal(t, "sample", session.Project)

			stats := engine.ResyncAll(t.Context(), nil)
			require.Zero(t, stats.Failed)
			session, err = database.GetSessionFull(t.Context(), "codex:"+id)
			require.NoError(t, err)
			require.NotNil(t, session)
			assert.Equal(t, "sample", session.Project)
			messages, err := database.GetAllMessages(t.Context(), session.ID)
			require.NoError(t, err)
			require.Len(t, messages, 2)
			assert.Equal(t, "added a limiter", messages[1].Content)
		})
	}
}

func TestRecordedRepositoryIdentityWritePaths(t *testing.T) {
	for _, mode := range []string{"ordinary", "bulk", "full"} {
		t.Run(mode, func(t *testing.T) {
			database := openTestDB(t)
			root := t.TempDir()
			require.NoError(t, os.Mkdir(filepath.Join(root, ".git"), 0o755))
			require.NoError(t, os.WriteFile(filepath.Join(root, ".git", "config"), []byte("[remote \"origin\"]\nurl = https://example.com/team/replacement.git\n"), 0o644))
			require.NoError(t, os.WriteFile(filepath.Join(root, ".git", "HEAD"), []byte("ref: refs/heads/replacement\n"), 0o644))
			engine := NewEngine(t.Context(), database, EngineConfig{Machine: "local"})
			t.Cleanup(engine.Close)
			pw := pendingWrite{sess: parser.ParsedSession{
				ID: "recorded-repository", Project: "api", Machine: "local", Agent: parser.AgentCodex,
				Cwd: root, GitRemote: "https://example.com/team/original.git", StartedAt: time.Now(),
			}}
			if mode == "full" {
				require.NoError(t, engine.writeSessionFull(pw))
			} else {
				writeMode := syncWriteDefault
				if mode == "bulk" {
					writeMode = syncWriteBulk
				}
				written, _, failed, _ := engine.writeBatch([]pendingWrite{pw}, writeMode, true)
				require.Equal(t, 1, written)
				require.Zero(t, failed)
			}
			snapshots, err := database.ListSessionProjectIdentitySnapshots(t.Context())
			require.NoError(t, err)
			require.Len(t, snapshots, 1)
			assert.Equal(t, "example.com/team/original", snapshots[0].NormalizedRemote)
			assert.Equal(t, export.ProjectResolutionResolved, snapshots[0].RemoteResolution)
			assert.Empty(t, snapshots[0].GitRemoteName, "the source does not name a git config remote")
		})
	}
}

func TestCodexIncrementalAppendKeepsRecordedRepository(t *testing.T) {
	for _, oldRemote := range []string{"", "https://example.com/team/api.git", "https://example.com/team/previous.git"} {
		t.Run(oldRemote, func(t *testing.T) {
			database := openTestDB(t)
			sessionsDir := t.TempDir()
			checkout := t.TempDir()
			require.NoError(t, os.Mkdir(filepath.Join(checkout, ".git"), 0o755))
			require.NoError(t, os.WriteFile(filepath.Join(checkout, ".git", "config"), []byte("[remote \"origin\"]\nurl = https://example.com/team/replacement.git\n"), 0o644))
			require.NoError(t, os.WriteFile(filepath.Join(checkout, ".git", "HEAD"), []byte("ref: refs/heads/main\n"), 0o644))
			id := "eeeeeeee-eeee-4eee-8eee-eeeeeeeeeeee"
			source := filepath.Join(sessionsDir, "rollout-"+id+".jsonl")
			meta := testjsonl.CodexSessionMetaWithFieldsJSON(
				id, checkout, "codex_cli_rs", "2026-10-08T12:00:00Z",
				map[string]any{"git": map[string]any{"repository_url": "https://example.com/team/api.git"}},
			)
			dbtest.WriteTestFile(t, source, []byte(meta+"\n"+
				testjsonl.CodexMsgJSON("user", "add rate limiting", "2026-10-08T12:00:01Z")+"\n"))
			engine := NewEngine(t.Context(), database, EngineConfig{Machine: "local", AgentDirs: map[parser.AgentType][]string{parser.AgentCodex: {sessionsDir}}})
			t.Cleanup(engine.Close)
			require.Equal(t, 1, engine.SyncAll(t.Context(), nil).Synced)
			if oldRemote != "" {
				engine.Close()
				identity := export.BuildStoredProjectIdentity(export.ProjectIdentityInput{RootPath: checkout, GitRemote: oldRemote, GitRemoteName: "origin"})
				require.NoError(t, database.Update(t.Context(), func(tx *sql.Tx) error {
					for _, table := range []string{"session_project_identity_snapshots", "project_identity_observations"} {
						_, err := tx.ExecContext(t.Context(), "UPDATE "+table+" SET git_remote=?, git_remote_name='origin', normalized_remote=?, key=?, key_source=?", oldRemote, identity.NormalizedRemote, identity.Key, identity.KeySource)
						if err != nil {
							return err
						}
					}
					_, err := tx.ExecContext(t.Context(), "UPDATE sessions SET data_version=137; PRAGMA user_version=137")
					return err
				}))
				require.NoError(t, database.Update(t.Context(), func(tx *sql.Tx) error {
					_, err := tx.ExecContext(t.Context(), "ALTER TABLE session_project_identity_snapshots DROP COLUMN recorded_git_remote")
					return err
				}))
				path := database.Path()
				require.NoError(t, database.Close())
				var err error
				database, err = db.OpenIsolated(t.Context(), path)
				require.NoError(t, err)
				t.Cleanup(func() { require.NoError(t, database.Close()) })
				cfg := EngineConfig{Machine: "local", AgentDirs: map[parser.AgentType][]string{parser.AgentCodex: {sessionsDir}}}
				engine = NewEngine(t.Context(), database, cfg)
				require.Equal(t, 1, engine.SyncAll(t.Context(), nil).Synced)
				engine.Close()
				// Reopen so the append must use persisted source evidence.
				require.NoError(t, database.Close())
				database, err = db.OpenIsolated(t.Context(), path)
				require.NoError(t, err)
				engine = NewEngine(t.Context(), database, cfg)
				t.Cleanup(engine.Close)
			}

			f, err := os.OpenFile(source, os.O_APPEND|os.O_WRONLY, 0o644)
			require.NoError(t, err)
			_, err = f.WriteString(testjsonl.CodexMsgJSON("assistant", "added a limiter", "2026-10-08T12:00:02Z") + "\n")
			require.NoError(t, err)
			require.NoError(t, f.Close())
			require.Equal(t, 1, engine.SyncAll(t.Context(), nil).Synced)

			sess, err := database.GetSessionFull(t.Context(), "codex:"+id)
			require.NoError(t, err)
			require.NotNil(t, sess)
			require.True(t, sess.LastWriteIncremental, "the append must take the incremental path")
			observations, err := database.ListProjectIdentityObservations(t.Context(), nil)
			require.NoError(t, err)
			wantRemotes := []string{"example.com/team/api"}
			if oldRemote == "https://example.com/team/previous.git" {
				wantRemotes = append(wantRemotes, "example.com/team/previous")
			}
			var remotes []string
			for _, obs := range observations {
				remotes = append(remotes, obs.NormalizedRemote)
			}
			assert.ElementsMatch(t, wantRemotes, remotes, "the append must not add the checkout's conflicting remote")
			snapshots, err := database.ListSessionProjectIdentitySnapshots(t.Context())
			require.NoError(t, err)
			require.Len(t, snapshots, 1)
			wantSnapshot := "example.com/team/api"
			if oldRemote == "https://example.com/team/previous.git" {
				wantSnapshot = "example.com/team/previous"
			}
			assert.Equal(t, wantSnapshot, snapshots[0].NormalizedRemote)
		})
	}
}

func TestCodexLabeledRunResyncAndExplicitWorktreeMappingWins(t *testing.T) {
	database := openTestDB(t)
	root := t.TempDir()
	const id = "cccccccc-cccc-4ccc-8ccc-cccccccccccc"
	cwd := filepath.Join(root, "checkout")
	source := filepath.Join(root, "experiment", "case", ".codex-run-v1-abc", "sessions", "2026", "10", "08", "rollout-2026-10-08T12-00-00-"+id+".jsonl")
	meta := testjsonl.CodexSessionMetaWithFieldsJSON(id, cwd, "codex_cli_rs", "2026-10-08T12:00:00Z", map[string]any{"git": map[string]any{"repository_url": "https://example.com/team/api.git", "branch": "feature"}})
	dbtest.WriteTestFile(t, source, []byte(meta+"\n"+testjsonl.CodexMsgJSON("user", "review", "2026-10-08T12:00:01Z")+"\n"))
	engine := NewEngine(t.Context(), database, EngineConfig{Machine: "local", DisableFilesystemProjectDiscovery: true, AgentDirs: map[parser.AgentType][]string{parser.AgentCodex: {root}}})
	t.Cleanup(engine.Close)
	require.Equal(t, 1, engine.SyncAll(t.Context(), nil).Synced)
	snapshots, err := database.ListSessionProjectIdentitySnapshots(t.Context())
	require.NoError(t, err)
	require.Len(t, snapshots, 1)
	assert.Equal(t, "example.com/team/api", snapshots[0].NormalizedRemote)
	repair, err := database.RepairRepositoryProjects(t.Context(), "")
	require.NoError(t, err)
	assert.Zero(t, repair.Sessions)
	f, err := os.OpenFile(source, os.O_APPEND|os.O_WRONLY, 0o600)
	require.NoError(t, err)
	_, err = f.WriteString(testjsonl.CodexMsgJSON("assistant", "done", "2026-10-08T12:00:02Z") + "\n")
	require.NoError(t, err)
	require.NoError(t, f.Close())
	require.NoError(t, engine.SyncPathsContext(t.Context(), []string{source}))
	session, err := database.GetSessionFull(t.Context(), "codex:"+id)
	require.NoError(t, err)
	require.NotNil(t, session)
	assert.Equal(t, "experiment/case", session.Project)
	assert.True(t, session.LastWriteIncremental)
	require.Zero(t, engine.ResyncAll(t.Context(), nil).Failed)
	session, err = database.GetSessionFull(t.Context(), "codex:"+id)
	require.NoError(t, err)
	assert.Equal(t, "experiment/case", session.Project)
	_, err = database.CreateWorktreeProjectMapping(t.Context(), db.WorktreeProjectMapping{
		Machine: "local", PathPrefix: cwd, Layout: db.WorktreeMappingLayoutExplicit,
		Project: "mapped_project", Enabled: true,
	})
	require.NoError(t, err)
	require.Zero(t, engine.ResyncAll(t.Context(), nil).Failed)
	session, err = database.GetSessionFull(t.Context(), "codex:"+id)
	require.NoError(t, err)
	assert.Equal(t, "mapped_project", session.Project)
}

func TestCodexLabeledCopyWinsSync(t *testing.T) {
	for _, mode := range []string{"quick sync", "older path with both copies present", "usage refresh", "missed event", "single session"} {
		t.Run(mode, func(t *testing.T) {
			database := openTestDB(t)
			root := t.TempDir()
			ordinary, logs := filepath.Join(root, "ordinary"), filepath.Join(root, "logs")
			const id = "cccccccc-cccc-4ccc-8ccc-cccccccccccc"
			name := "rollout-2026-10-08T12-00-00-" + id + ".jsonl"
			paths := []string{filepath.Join(ordinary, "2026", "10", "08", name), filepath.Join(logs, "a", ".codex-run-v1-abc", "sessions", "2026", "10", "08", name), filepath.Join(logs, "b", ".codex-run-v1-abc", "sessions", "2026", "10", "08", name)}
			meta := testjsonl.CodexSessionMetaJSON(id, "/work/project", "codex_exec", "2026-10-08T12:00:00Z")
			write := func(i int, path string) {
				dbtest.WriteTestFile(t, path, []byte(meta+"\n"+testjsonl.CodexMsgJSON("user", []string{"ordinary", "a", "b"}[i], "2026-10-08T12:00:01Z")+"\n"))
				mtime := time.Now().Add(time.Duration(i) * time.Hour)
				require.NoError(t, os.Chtimes(path, mtime, mtime))
			}
			write(0, paths[0])
			if mode != "single session" && mode != "usage refresh" && mode != "missed event" {
				write(1, paths[1])
				write(2, paths[2])
			}
			engine := NewEngine(t.Context(), database, EngineConfig{Machine: "local", DisableFilesystemProjectDiscovery: true, AgentDirs: map[parser.AgentType][]string{parser.AgentCodex: {ordinary, logs}}})
			t.Cleanup(engine.Close)
			switch mode {
			case "older path with both copies present":
				require.NoError(t, engine.SyncPathsContext(t.Context(), []string{paths[1]}))
			case "quick sync":
				now := time.Now()
				require.NoError(t, os.Chtimes(paths[1], now.Add(-2*time.Hour), now.Add(-2*time.Hour)))
				require.NoError(t, os.Chtimes(paths[2], now.Add(-time.Hour), now.Add(-time.Hour)))
				require.Zero(t, engine.SyncAllSince(t.Context(), now.Add(-30*time.Minute), nil).Failed)
			case "usage refresh":
				require.NoError(t, engine.SyncPathsContext(t.Context(), []string{paths[0]}))
				write(1, paths[1])
				write(2, paths[2])
				require.NoError(t, engine.TrySyncSessionWithSubagentsContext(t.Context(), "codex:"+id))
			case "single session":
				require.NoError(t, engine.SyncPathsContext(t.Context(), []string{paths[0]}))
				write(1, paths[1])
				write(2, paths[2])
				require.NoError(t, engine.SyncSingleSessionContext(t.Context(), "codex:"+id))
				saved, err := database.GetSessionFull(t.Context(), "codex:"+id)
				require.NoError(t, err)
				require.NotNil(t, saved)
				require.NotNil(t, saved.FilePath)
				assert.Equal(t, paths[0], *saved.FilePath)
				require.NoError(t, engine.RefreshSessionByIDContext(t.Context(), "codex:"+id))
			case "missed event":
				write(1, paths[1])
				require.NoError(t, engine.SyncPathsContext(t.Context(), []string{paths[1]}))
				write(2, paths[2])
				require.NoError(t, engine.ReconcileWatchRoots(t.Context(), []string{ordinary, logs}, false))

			}
			session, err := database.GetSessionFull(t.Context(), "codex:"+id)
			require.NoError(t, err)
			require.NotNil(t, session)
			assert.Equal(t, "b", session.Project)
			require.NotNil(t, session.FilePath)
			assert.Equal(t, paths[2], *session.FilePath)
			messages, err := database.GetMessages(t.Context(), "codex:"+id, 0, 100, true)
			require.NoError(t, err)
			require.NotEmpty(t, messages)
			assert.Equal(t, "b", messages[0].Content)
		})
	}
}

func TestCodexLabeledRunIndexRename(t *testing.T) {
	database := openTestDB(t)
	root := t.TempDir()
	const id = "cccccccc-cccc-4ccc-8ccc-cccccccccccc"
	home := filepath.Join(root, "label", ".codex-run-v1-abc")
	source := filepath.Join(home, "sessions", "2026", "10", "08", "rollout-2026-10-08T12-00-00-"+id+".jsonl")
	dbtest.WriteTestFile(t, source, []byte(testjsonl.CodexSessionMetaJSON(id, "/work/project", "codex_cli_rs", "2026-10-08T12:00:00Z")+"\n"+testjsonl.CodexMsgJSON("user", "review", "2026-10-08T12:00:01Z")+"\n"))
	index := filepath.Join(home, parser.CodexSessionIndexFilename)
	dbtest.WriteTestFile(t, index, []byte(`{"id":"`+id+`","thread_name":"Old"}`+"\n"))
	engine := NewEngine(t.Context(), database, EngineConfig{Machine: "local", AgentDirs: map[parser.AgentType][]string{parser.AgentCodex: {root}}})
	t.Cleanup(engine.Close)
	require.NoError(t, engine.SyncPathsContext(t.Context(), []string{source}))
	require.NoError(t, os.WriteFile(index, []byte(`{"id":"`+id+`","thread_name":"New"}`+"\n"), 0o600))
	require.NoError(t, engine.SyncPathsContext(t.Context(), []string{index}))
	name, found, err := database.GetSessionName(t.Context(), "codex:"+id)
	require.NoError(t, err)
	require.True(t, found)
	assert.Equal(t, "New", name)
}

func TestCodexLabelIsNotRepositoryAlias(t *testing.T) {
	database := openTestDB(t)
	root := t.TempDir()
	checkout := filepath.Join(t.TempDir(), "checkout")
	require.NoError(t, os.MkdirAll(filepath.Join(checkout, ".git"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(checkout, ".git", "config"), []byte("[remote \"origin\"]\nurl = https://example.com/team/api.git\n"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(checkout, ".git", "HEAD"), []byte("ref: refs/heads/main\n"), 0o644))
	engine := NewEngine(t.Context(), database, EngineConfig{Machine: "local", AgentDirs: map[parser.AgentType][]string{parser.AgentCodex: {root}}})
	t.Cleanup(engine.Close)
	write := func(id, cwd, path string) {
		meta := testjsonl.CodexSessionMetaWithFieldsJSON(id, cwd, "codex_cli_rs", "2026-10-08T12:00:00Z", map[string]any{"git": map[string]any{"repository_url": "https://example.com/team/api.git"}})
		dbtest.WriteTestFile(t, path, []byte(meta+"\n"+testjsonl.CodexMsgJSON("user", "hello", "2026-10-08T12:00:01Z")+"\n"))
		require.NoError(t, engine.SyncPathsContext(t.Context(), []string{path}))
	}
	const first = "cccccccc-cccc-4ccc-8ccc-cccccccccccc"
	write(first, checkout, filepath.Join(root, "label", ".codex-run-v1-abc", "sessions", "2026", "10", "08", "rollout-2026-10-08T12-00-00-"+first+".jsonl"))
	const second = "dddddddd-dddd-4ddd-8ddd-dddddddddddd"
	write(second, filepath.Join(root, "missing"), filepath.Join(root, "rollout-"+second+".jsonl"))
	session, err := database.GetSessionFull(t.Context(), "codex:"+second)
	require.NoError(t, err)
	require.NotNil(t, session)
	assert.Equal(t, "api", session.Project)
}
