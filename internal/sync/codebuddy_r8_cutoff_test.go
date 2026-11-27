package sync

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A quick sync refreshes a converted conversation whose 4.x request totals
// changed even when its transcript is older than the sync cutoff. Ranking
// still uses the transcript time, so the archived file time stays put.
func TestSyncAllSinceCodeBuddyConvertedFollowsLegacyTotals(t *testing.T) {
	home, project := cbcSyncHome(t)
	legacyRoot := t.TempDir()
	convDir := filepath.Join(legacyRoot, "history", "ws", "conv1")
	transcript := filepath.Join(project, "conv1.jsonl")
	writeCBCSync(t, transcript,
		`{"type":"session-meta","id":"m","sessionId":"conv1","meta":{"codebuddy.ai/converterVersion":"ide-main-craft-jsonl/29"}}`,
		`{"id":"u1","type":"message","role":"user","timestamp":1767225601000,"content":[{"type":"input_text","text":"old prompt"}]}`,
		`{"id":"a1","parentId":"u1","type":"message","role":"assistant","timestamp":1767225602000,"content":[{"type":"output_text","text":"old reply"}]}`,
	)
	writeCodeBuddyLegacyUsage(t, convDir, map[string]int{"a1": 40})
	old := time.Now().Add(-30 * 24 * time.Hour)
	for _, path := range []string{
		transcript,
		filepath.Join(convDir, "index.json"),
		filepath.Join(convDir, "messages", "a1.json"),
		filepath.Join(convDir, "messages"),
	} {
		require.NoError(t, os.Chtimes(path, old, old))
	}

	database := openTestDB(t)
	engine := newCBCEngine(t, database, home, legacyRoot)
	engine.SyncAll(t.Context(), nil)
	require.Equal(t, 40, cbcDailyTotals(t, database).OutputTokens)
	before, err := database.GetSessionFull(t.Context(), "codebuddy:conv1")
	require.NoError(t, err)
	require.NotNil(t, before)
	require.NotNil(t, before.FileMtime)

	since := time.Now().Add(-time.Minute)
	require.NoError(t, os.WriteFile(filepath.Join(convDir, "messages", "a1.json"),
		[]byte(`{"role":"assistant","createdAt":"2026-01-01T00:00:02Z","extra":{"requestId":"req-a1","modelId":"glm-x","statsSnapshot":{"inputTokens":300,"outputTokens":75}}}`), 0o600))
	engine.SyncAllSince(t.Context(), since, nil)

	assert.Equal(t, 75, cbcDailyTotals(t, database).OutputTokens,
		"a quick sync reads changed 4.x totals of an old converted transcript")
	after, err := database.GetSessionFull(t.Context(), "codebuddy:conv1")
	require.NoError(t, err)
	require.NotNil(t, after)
	require.NotNil(t, after.FilePath)
	require.NotNil(t, after.FileMtime)
	assert.Equal(t, transcript, *after.FilePath, "the transcript still owns the conversation")
	assert.Equal(t, *before.FileMtime, *after.FileMtime,
		"the archived file time stays the transcript time")
}
