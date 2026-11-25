package sync

import (
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/parser"
)

// cbcSyncHome lays out a CodeBuddy CLI/IDE 5.x home with one project.
func cbcSyncHome(t *testing.T) (home, project string) {
	t.Helper()
	home = t.TempDir()
	project = filepath.Join(home, "projects", "-work-demo")
	require.NoError(t, os.MkdirAll(project, 0o755))
	return home, project
}

func writeCBCSync(t *testing.T, path string, lines ...string) {
	t.Helper()
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	require.NoError(t, os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o600))
}

func newCBCEngine(t *testing.T, database *db.DB, roots ...string) *Engine {
	t.Helper()
	engine := NewEngine(t.Context(), database, EngineConfig{
		AgentDirs: map[parser.AgentType][]string{parser.AgentCodeBuddy: roots},
		Machine:   "test",
	})
	t.Cleanup(engine.Close)
	return engine
}

func cbcDailyTotals(t *testing.T, database *db.DB) db.UsageTotals {
	t.Helper()
	daily, err := database.GetDailyUsage(t.Context(), db.UsageFilter{Timezone: "UTC"})
	require.NoError(t, err)
	return daily.Totals
}

// A forked transcript repeats the responses it inherited with their native
// identities; usage counts them once across both files.
func TestSyncCodeBuddyForkedTranscriptsCountUsageOnce(t *testing.T) {
	home, project := cbcSyncHome(t)
	shared := []string{
		`{"id":"u1","type":"message","role":"user","timestamp":1767225600000,"content":[{"type":"input_text","text":"start"}]}`,
		`{"id":"a1","parentId":"u1","type":"message","role":"assistant","timestamp":1767225601000,"content":[{"type":"output_text","text":"shared reply"}],"providerData":{"conversationRequestId":"r1","messageId":"m1","stepSeq":1,"model":"glm-x","rawUsage":{"prompt_tokens":100,"completion_tokens":10}}}`,
	}
	writeCBCSync(t, filepath.Join(project, "parent.jsonl"), append(shared,
		`{"id":"u2","parentId":"a1","type":"message","role":"user","timestamp":1767225602000,"content":[{"type":"input_text","text":"parent only"}]}`,
		`{"id":"a2","parentId":"u2","type":"message","role":"assistant","timestamp":1767225603000,"content":[{"type":"output_text","text":"parent reply"}],"providerData":{"conversationRequestId":"r2","messageId":"m2","stepSeq":1,"model":"glm-x","rawUsage":{"prompt_tokens":200,"completion_tokens":20}}}`,
	)...)
	writeCBCSync(t, filepath.Join(project, "child.jsonl"), append(shared,
		`{"id":"u3","parentId":"a1","type":"message","role":"user","timestamp":1767225604000,"content":[{"type":"input_text","text":"child only"}]}`,
		// A real retry with the same text is a new request and counts again.
		`{"id":"a3","parentId":"u3","type":"message","role":"assistant","timestamp":1767225605000,"content":[{"type":"output_text","text":"parent reply"}],"providerData":{"conversationRequestId":"r3","messageId":"m3","stepSeq":1,"model":"glm-x","rawUsage":{"prompt_tokens":300,"completion_tokens":30}}}`,
	)...)
	database := openTestDB(t)
	engine := newCBCEngine(t, database, home)
	require.Equal(t, 2, engine.SyncAll(t.Context(), nil).Synced)

	totals := cbcDailyTotals(t, database)
	assert.Equal(t, 60, totals.OutputTokens, "10 once, plus 20 and 30")
	assert.Equal(t, 600, totals.InputTokens, "100 once, plus 200 and 300")
}

// A converted 4.x conversation is read from its CBC transcript, which holds
// later turns; the frozen 4.x manifest does not add a second copy.
func TestSyncCodeBuddyConvertedConversationKeepsOneSession(t *testing.T) {
	home, project := cbcSyncHome(t)
	legacyRoot := t.TempDir()
	convDir := filepath.Join(legacyRoot, "history", "ws", "conv1")
	require.NoError(t, os.MkdirAll(filepath.Join(convDir, "messages"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(convDir, "index.json"), []byte(`{"messages":[{"id":"u1","role":"user"},{"id":"a1","role":"assistant"}]}`), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(convDir, "messages", "u1.json"), []byte(`{"role":"user","message":{"content":[{"type":"text","text":"old prompt"}]}}`), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(convDir, "messages", "a1.json"), []byte(`{"role":"assistant","createdAt":"2026-01-01T00:00:02Z","extra":{"requestId":"req1","modelId":"glm-x","statsSnapshot":{"inputTokens":300,"outputTokens":40,"cachedInputTokens":200}},"message":{"content":[{"type":"text","text":"old reply"}]}}`), 0o600))
	writeCBCSync(t, filepath.Join(project, "conv1.jsonl"),
		`{"type":"session-meta","id":"m","sessionId":"conv1","meta":{"codebuddy.ai/converterVersion":"ide-main-craft-jsonl/29"}}`,
		`{"id":"u1","type":"message","role":"user","timestamp":1767225601000,"content":[{"type":"input_text","text":"old prompt"}]}`,
		`{"id":"a1","parentId":"u1","type":"message","role":"assistant","timestamp":1767225602000,"content":[{"type":"output_text","text":"old reply"}]}`,
		`{"id":"u2","parentId":"a1","type":"message","role":"user","timestamp":1767225700000,"content":[{"type":"input_text","text":"continued in 5.x"}]}`,
	)
	database := openTestDB(t)
	engine := newCBCEngine(t, database, home, legacyRoot)
	engine.SyncAll(t.Context(), nil)

	sess, err := database.GetSessionFull(t.Context(), "codebuddy:conv1")
	require.NoError(t, err)
	require.NotNil(t, sess)
	require.NotNil(t, sess.FilePath)
	assert.Equal(t, filepath.Join(project, "conv1.jsonl"), *sess.FilePath)
	assert.Equal(t, 2, sess.UserMessageCount)
	totals := cbcDailyTotals(t, database)
	assert.Equal(t, 40, totals.OutputTokens, "the 4.x request totals count once")

	// A later sync, after either copy changes, keeps the CBC owner.
	require.NoError(t, os.WriteFile(filepath.Join(convDir, "messages", "u1.json"), []byte(`{"role":"user","message":{"content":[{"type":"text","text":"old prompt!"}]}}`), 0o600))
	engine.SyncAll(t.Context(), nil)
	sess, err = database.GetSessionFull(t.Context(), "codebuddy:conv1")
	require.NoError(t, err)
	require.NotNil(t, sess)
	assert.Equal(t, filepath.Join(project, "conv1.jsonl"), *sess.FilePath)
	assert.Equal(t, 2, sess.UserMessageCount)
}

// Renaming a conversation in the IDE changes only codebuddy.db. A catalog
// event reaches the renamed conversation.
func TestSyncCodeBuddyCatalogRenameUpdatesTitle(t *testing.T) {
	home, project := cbcSyncHome(t)
	writeCBCSync(t, filepath.Join(project, "run-a.jsonl"),
		`{"id":"u1","type":"message","role":"user","timestamp":1767225600000,"sessionId":"run-a","content":[{"type":"input_text","text":"hello"}]}`,
		`{"id":"a1","parentId":"u1","type":"message","role":"assistant","timestamp":1767225601000,"content":[{"type":"output_text","text":"hi"}]}`,
	)
	catalog := filepath.Join(home, parser.CodeBuddyCatalogDBName)
	conn, err := sql.Open("sqlite3", catalog)
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })
	_, err = conn.ExecContext(t.Context(), `CREATE TABLE conversations (id TEXT PRIMARY KEY, title TEXT NOT NULL DEFAULT '', custom_title TEXT, runtime_session_id TEXT);
		INSERT INTO conversations VALUES ('conv-a', 'Before', NULL, 'run-a')`)
	require.NoError(t, err)

	database := openTestDB(t)
	engine := newCBCEngine(t, database, home)
	engine.SyncAll(t.Context(), nil)
	sess, err := database.GetSessionFull(t.Context(), "codebuddy:run-a")
	require.NoError(t, err)
	require.NotNil(t, sess.SessionName)
	assert.Equal(t, "Before", *sess.SessionName)

	_, err = conn.ExecContext(t.Context(), `UPDATE conversations SET custom_title = 'Renamed in IDE' WHERE id = 'conv-a'`)
	require.NoError(t, err)
	engine.SyncPaths([]string{catalog})
	sess, err = database.GetSessionFull(t.Context(), "codebuddy:run-a")
	require.NoError(t, err)
	require.NotNil(t, sess.SessionName)
	assert.Equal(t, "Renamed in IDE", *sess.SessionName)
}

// A persisted output can be written after its transcript line, or removed by
// CodeBuddy's cleanup later. The archive keeps the full text it once read.
func TestSyncCodeBuddyPersistedOutputLateAndRemoved(t *testing.T) {
	home, project := cbcSyncHome(t)
	side := filepath.Join(project, "s1", "tool-results", "c1.txt")
	writeCBCSync(t, filepath.Join(project, "s1.jsonl"),
		`{"id":"u1","type":"message","role":"user","timestamp":1767225600000,"content":[{"type":"input_text","text":"go"}]}`,
		`{"id":"c1","parentId":"u1","type":"function_call","timestamp":1767225601000,"name":"Bash","callId":"c1","arguments":"{}"}`,
		`{"id":"r1","parentId":"c1","type":"function_call_result","timestamp":1767225602000,"name":"Bash","callId":"c1","output":"<persisted-output>\nOutput too large (1KB). Full output saved to: `+filepath.ToSlash(side)+`\n</persisted-output>"}`,
	)
	database := openTestDB(t)
	engine := newCBCEngine(t, database, home)
	engine.SyncAll(t.Context(), nil)
	resultText := func() string {
		msgs, err := database.GetMessages(t.Context(), "codebuddy:s1", 0, 100, true)
		require.NoError(t, err)
		for _, m := range msgs {
			for _, call := range m.ToolCalls {
				if call.ToolUseID == "c1" {
					return call.ResultContent
				}
			}
		}
		return ""
	}
	assert.Contains(t, resultText(), "persisted-output")

	require.NoError(t, os.MkdirAll(filepath.Dir(side), 0o755))
	require.NoError(t, os.WriteFile(side, []byte("the full output"), 0o600))
	engine.SyncPaths([]string{side})
	assert.Equal(t, "the full output", resultText())

	require.NoError(t, os.Remove(side))
	engine.SyncPaths([]string{side})
	assert.Equal(t, "the full output", resultText(), "cleanup of the side file keeps the archived text")
}

// Deleting a conversation in CodeBuddy removes its transcript; the archive
// keeps it, and a removed branch keeps its archived copy too.
func TestSyncCodeBuddySourceDeletionKeepsArchive(t *testing.T) {
	home, project := cbcSyncHome(t)
	transcript := filepath.Join(project, "s1.jsonl")
	writeCBCSync(t, transcript,
		`{"id":"u1","type":"message","role":"user","timestamp":1767225600000,"content":[{"type":"input_text","text":"first"}]}`,
		`{"id":"a1","parentId":"u1","type":"message","role":"assistant","timestamp":1767225601000,"content":[{"type":"output_text","text":"old"}]}`,
		`{"id":"n1","type":"resend-fork-notice","timestamp":1767225602000}`,
		`{"id":"u2","type":"message","role":"user","timestamp":1767225603000,"content":[{"type":"input_text","text":"second"}]}`,
		`{"id":"a2","parentId":"u2","type":"message","role":"assistant","timestamp":1767225604000,"content":[{"type":"output_text","text":"new"}]}`,
	)
	database := openTestDB(t)
	engine := newCBCEngine(t, database, home)
	engine.SyncAll(t.Context(), nil)
	for _, id := range []string{"codebuddy:s1", "codebuddy:s1-u1"} {
		sess, err := database.GetSessionFull(t.Context(), id)
		require.NoError(t, err)
		require.NotNil(t, sess, id)
	}

	// The producer compacts the file without the old branch.
	writeCBCSync(t, transcript,
		`{"id":"u2","type":"message","role":"user","timestamp":1767225603000,"content":[{"type":"input_text","text":"second"}]}`,
		`{"id":"a2","parentId":"u2","type":"message","role":"assistant","timestamp":1767225604000,"content":[{"type":"output_text","text":"new"}]}`,
	)
	engine.SyncAll(t.Context(), nil)
	branch, err := database.GetSessionFull(t.Context(), "codebuddy:s1-u1")
	require.NoError(t, err)
	require.NotNil(t, branch, "a branch missing from the source stays archived")

	require.NoError(t, os.Remove(transcript))
	engine.SyncAll(t.Context(), nil)
	sess, err := database.GetSessionFull(t.Context(), "codebuddy:s1")
	require.NoError(t, err)
	require.NotNil(t, sess, "a deleted transcript keeps its archived session")
	msgs, err := database.GetMessages(t.Context(), "codebuddy:s1", 0, 100, true)
	require.NoError(t, err)
	assert.Len(t, msgs, 2)
}
