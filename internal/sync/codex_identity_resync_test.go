package sync

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/dbtest"
	"go.kenn.io/agentsview/internal/parser"
	"go.kenn.io/agentsview/internal/testjsonl"
)

func TestCodexResyncRestoresGitProjectWhenRootIsSessions(t *testing.T) {
	database := openTestDB(t)
	dir := t.TempDir()
	checkout := filepath.Join(dir, "api")
	require.NoError(t, os.MkdirAll(filepath.Join(checkout, ".git"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(checkout, ".git", "config"), []byte("[remote \"origin\"]\nurl = https://example.com/team/api.git\n"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(checkout, ".git", "HEAD"), []byte("ref: refs/heads/main\n"), 0o644))
	sessions := filepath.Join(dir, "logs", "team", ".codex-run-v1-abc", "sessions")
	const id = "cccccccc-cccc-4ccc-8ccc-cccccccccccc"
	source := filepath.Join(sessions, "2026", "10", "08", "rollout-2026-10-08T12-00-00-"+id+".jsonl")
	meta := testjsonl.CodexSessionMetaWithFieldsJSON(id, checkout, "codex_cli_rs", "2026-10-08T12:00:00Z", map[string]any{"git": map[string]any{"repository_url": "https://example.com/team/api.git"}})
	dbtest.WriteTestFile(t, source, []byte(meta+"\n"+testjsonl.CodexMsgJSON("user", "review", "2026-10-08T12:00:01Z")+"\n"))
	engine := NewEngine(t.Context(), database, EngineConfig{Machine: "local", AgentDirs: map[parser.AgentType][]string{parser.AgentCodex: {sessions}}})
	t.Cleanup(engine.Close)
	require.Equal(t, 1, engine.SyncAll(t.Context(), nil).Synced)
	session, err := database.GetSessionFull(t.Context(), "codex:"+id)
	require.NoError(t, err)
	require.Equal(t, "api", session.Project)
	meta = testjsonl.CodexSessionMetaWithFieldsJSON(id, filepath.Join(dir, "team"), "codex_cli_rs", "2026-10-08T12:00:00Z", map[string]any{"git": map[string]any{"repository_url": "https://example.com/team/api.git"}})
	dbtest.WriteTestFile(t, source, []byte(meta+"\n"+testjsonl.CodexMsgJSON("user", "review", "2026-10-08T12:00:01Z")+"\n"))
	require.NoError(t, os.Rename(checkout, filepath.Join(dir, "removed-checkout")))
	stats := engine.ResyncAll(t.Context(), nil)
	require.False(t, stats.Aborted, "%v", stats.Warnings)
	require.Zero(t, stats.Failed)
	session, err = database.GetSessionFull(t.Context(), "codex:"+id)
	require.NoError(t, err)
	require.NotNil(t, session)
	assert.Equal(t, "api", session.Project)
}
