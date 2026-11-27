package sync

import (
	"encoding/base64"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/db"
)

// cbcConvertedImageTurn is a converted conversation whose prompt attaches a
// 4.x image and whose reply recorded request a1 in its 4.x folder.
var cbcConvertedImageTurn = []string{
	`{"type":"session-meta","id":"m","sessionId":"conv1","meta":{"codebuddy.ai/converterVersion":"ide-main-craft-jsonl/29"}}`,
	`{"id":"u1","type":"message","role":"user","timestamp":1767225601000,"content":[{"type":"input_text","text":"what is this"},{"type":"input_image","image":"codebuddy-asset://assets/pic.png"}]}`,
	`{"id":"a1","parentId":"u1","type":"message","role":"assistant","timestamp":1767225602000,"content":[{"type":"output_text","text":"a pixel"}]}`,
}

// cbcConvertedImageSync imports the converted conversation with its 4.x
// folder holding the image and the given request totals, and returns the
// folder, the transcript, the engine, and the archived image link.
func cbcConvertedImageSync(
	t *testing.T, database *db.DB, outputs map[string]int,
) (convDir, transcript string, engine *Engine, link string) {
	t.Helper()
	home, project := cbcSyncHome(t)
	legacyRoot := t.TempDir()
	convDir = filepath.Join(legacyRoot, "history", "ws", "conv1")
	writeCodeBuddyLegacyUsage(t, convDir, outputs)
	png, err := base64.StdEncoding.DecodeString(codebuddyBlobPNG)
	require.NoError(t, err)
	require.NoError(t, os.MkdirAll(filepath.Join(convDir, "assets"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(convDir, "assets", "pic.png"), png, 0o600))
	transcript = filepath.Join(project, "conv1.jsonl")
	writeCBCSync(t, transcript, cbcConvertedImageTurn...)
	database.SetAssetsDir(t.TempDir())
	engine = newCBCEngine(t, database, home, legacyRoot)
	engine.SyncAll(t.Context(), nil)
	link = cbcAssetLinkRe.FindString(cbcFirstContent(t, database, "codebuddy:conv1"))
	require.NotEmpty(t, link, "the first sync archives the image")
	return convDir, transcript, engine, link
}

func cbcFirstContent(t *testing.T, database *db.DB, sessionID string) string {
	t.Helper()
	msgs, err := database.GetMessages(t.Context(), sessionID, 0, 100, true)
	require.NoError(t, err)
	require.NotEmpty(t, msgs, sessionID)
	return msgs[0].Content
}

// A 4.x manifest that no longer reads as a message list keeps the request
// totals the archive read from it.
func TestSyncCodeBuddyConvertedKeepsTotalsAfterManifestBreaks(t *testing.T) {
	for _, tc := range []struct{ name, manifest string }{
		{"malformed", `{"messages":[`},
		{"no messages list", `{"title":"old"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			database := openTestDB(t)
			convDir, _, engine, link := cbcConvertedImageSync(t, database, map[string]int{"a1": 40})
			require.Equal(t, 40, cbcDailyTotals(t, database).OutputTokens)

			require.NoError(t, os.WriteFile(filepath.Join(convDir, "index.json"), []byte(tc.manifest), 0o600))
			engine.SyncAll(t.Context(), nil)
			assert.Equal(t, 40, cbcDailyTotals(t, database).OutputTokens,
				"the archived 4.x total stays")
			assert.Contains(t, cbcFirstContent(t, database, "codebuddy:conv1"), link)
		})
	}
}

// A deleted 4.x message file keeps the total the archive read from it, while
// a readable message corrected to zero shows zero.
func TestSyncCodeBuddyConvertedKeepsTotalOfDeletedLegacyMessage(t *testing.T) {
	database := openTestDB(t)
	convDir, _, engine, _ := cbcConvertedImageSync(t, database, map[string]int{"a1": 40, "a2": 5})
	require.Equal(t, 45, cbcDailyTotals(t, database).OutputTokens)

	writeCodeBuddyLegacyUsage(t, convDir, map[string]int{"a1": 0, "a2": 5})
	require.NoError(t, os.Remove(filepath.Join(convDir, "messages", "a2.json")))
	engine.SyncAll(t.Context(), nil)
	assert.Equal(t, 5, cbcDailyTotals(t, database).OutputTokens,
		"a1 now reads 0 and the archived a2 total stays")
}

// A deleted 4.x assets folder keeps the image link the archive saved.
func TestSyncCodeBuddyConvertedKeepsImageAfterAssetsDeleted(t *testing.T) {
	database := openTestDB(t)
	convDir, _, engine, link := cbcConvertedImageSync(t, database, map[string]int{"a1": 40})

	require.NoError(t, os.RemoveAll(filepath.Join(convDir, "assets")))
	engine.SyncAll(t.Context(), nil)
	got := cbcFirstContent(t, database, "codebuddy:conv1")
	assert.Contains(t, got, link)
	assert.NotContains(t, got, "[image unavailable")
	assert.Equal(t, 40, cbcDailyTotals(t, database).OutputTokens)
}

// Rewinding an image-bearing turn into a branch after the 4.x folder is gone
// shows, in the new branch, the image the conversation archived.
func TestSyncCodeBuddyConvertedRewindKeepsArchivedImage(t *testing.T) {
	for _, tc := range []struct {
		name string
		sync func(*testing.T, *Engine, string)
	}{
		{"full sync", func(t *testing.T, e *Engine, _ string) { e.SyncAll(t.Context(), nil) }},
		{"changed path", func(_ *testing.T, e *Engine, path string) { e.SyncPaths([]string{path}) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			database := openTestDB(t)
			convDir, transcript, engine, link := cbcConvertedImageSync(t, database, map[string]int{"a1": 40})
			require.NoError(t, os.RemoveAll(convDir))
			engine.SyncAll(t.Context(), nil)
			require.Contains(t, cbcFirstContent(t, database, "codebuddy:conv1"), link)

			writeCBCSync(t, transcript, append(cbcConvertedImageTurn,
				`{"id":"n1","type":"resend-fork-notice","timestamp":1767225604000}`,
				`{"id":"u2","type":"message","role":"user","timestamp":1767225605000,"content":[{"type":"input_text","text":"again"}]}`,
				`{"id":"a2","parentId":"u2","type":"message","role":"assistant","timestamp":1767225606000,"content":[{"type":"output_text","text":"ok"}]}`,
			)...)
			tc.sync(t, engine, transcript)
			branch := cbcFirstContent(t, database, "codebuddy:conv1-u1")
			assert.Contains(t, branch, link, "the branch shows the archived image")
			assert.NotContains(t, branch, "[image unavailable")
			assert.Equal(t, 40, cbcDailyTotals(t, database).OutputTokens,
				"the request still counts once")
		})
	}
}
