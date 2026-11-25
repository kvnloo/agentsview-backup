package sync

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/dbtest"
	"go.kenn.io/agentsview/internal/parser"
	"go.kenn.io/agentsview/internal/testjsonl"
)

func TestCodexLabelRefreshAfterDisabledRootChange(t *testing.T) {
	database := openTestDB(t)
	root := filepath.Join(t.TempDir(), "logs")
	sessions := filepath.Join(root, "team", ".codex-run-v1-abc", "sessions")
	const id = "cccccccc-cccc-4ccc-8ccc-cccccccccccc"
	source := filepath.Join(sessions, "2026", "10", "08", "rollout-2026-10-08T12-00-00-"+id+".jsonl")
	dbtest.WriteTestFile(t, source, []byte(testjsonl.CodexSessionMetaJSON(id, "/work/project", "codex_exec", "2026-10-08T12:00:00Z")+"\n"+testjsonl.CodexMsgJSON("user", "review", "2026-10-08T12:00:01Z")+"\n"))
	engine := NewEngine(t.Context(), database, EngineConfig{Machine: "local", AgentDirs: map[parser.AgentType][]string{parser.AgentCodex: {sessions}}})
	t.Cleanup(engine.Close)
	require.Equal(t, 1, engine.SyncAll(t.Context(), nil).Synced)
	config := SourceConfig{AgentDirs: map[parser.AgentType][]string{parser.AgentCodex: {root}}, DisabledAgents: []parser.AgentType{parser.AgentCodex}}
	engine.ReconfigureSources(config)
	require.Zero(t, engine.SyncAll(t.Context(), nil).Failed)
	config.DisabledAgents = nil
	engine.ReconfigureSources(config)
	require.Equal(t, 1, engine.SyncAll(t.Context(), nil).Synced)
	session, err := database.GetSessionFull(t.Context(), "codex:"+id)
	require.NoError(t, err)
	require.NotNil(t, session)
	assert.Equal(t, "team", session.Project)
	assert.Equal(t, parser.SessionKindRoborev, session.SessionKind)
	assert.Zero(t, engine.SyncAll(t.Context(), nil).Synced)
}
