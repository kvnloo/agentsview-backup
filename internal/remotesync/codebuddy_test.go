package remotesync

import (
	"bytes"
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/parser"
)

// writeCodeBuddyRemoteHome lays out a CodeBuddy CLI/IDE 5.x home with one
// transcript that references one stored image, one unreferenced image, a
// settings file, and an IDE catalog with a pending WAL write.
func writeCodeBuddyRemoteHome(t *testing.T) (home string, catalogWriter *sql.DB) {
	t.Helper()
	home = t.TempDir()
	project := filepath.Join(home, "projects", "-work")
	require.NoError(t, os.MkdirAll(project, 0o755))
	used := strings.Repeat("ab", 32)
	unused := strings.Repeat("cd", 32)
	for _, blob := range []string{used, unused} {
		dir := filepath.Join(home, "blobs", blob[:2])
		require.NoError(t, os.MkdirAll(dir, 0o755))
		require.NoError(t, os.WriteFile(filepath.Join(dir, blob+".png"), []byte("png"), 0o600))
	}
	require.NoError(t, os.WriteFile(filepath.Join(home, "settings.json"), []byte(`{"token":"secret"}`), 0o600))
	transcript := `{"id":"u1","type":"message","role":"user","timestamp":1767225600000,"sessionId":"run-1","content":[{"type":"input_text","text":"hello"},{"type":"image_blob_ref","blob_id":"` + used + `","mime":"image/png"}]}` + "\n" +
		`{"id":"a1","parentId":"u1","type":"message","role":"assistant","timestamp":1767225601000,"content":[{"type":"output_text","text":"hi"}]}` + "\n"
	require.NoError(t, os.WriteFile(filepath.Join(project, "run-1.jsonl"), []byte(transcript), 0o600))

	catalogWriter, err := sql.Open("sqlite3", filepath.Join(home, parser.CodeBuddyCatalogDBName))
	require.NoError(t, err)
	t.Cleanup(func() { _ = catalogWriter.Close() })
	_, err = catalogWriter.ExecContext(t.Context(), `PRAGMA journal_mode=WAL; PRAGMA wal_autocheckpoint=0;
		CREATE TABLE conversations (id TEXT PRIMARY KEY, title TEXT NOT NULL DEFAULT '', custom_title TEXT, runtime_session_id TEXT);
		INSERT INTO conversations VALUES ('conv-1', 'Remote title', NULL, 'run-1');`)
	require.NoError(t, err)
	return home, catalogWriter
}

func TestCodeBuddyRemoteTargetsCarryCatalogAndReferencedImages(t *testing.T) {
	home, _ := writeCodeBuddyRemoteHome(t)
	targets := resolveTargetsForTest(t, config.Config{AgentDirs: map[parser.AgentType][]string{
		parser.AgentCodeBuddy: {home},
	}})
	assert.Equal(t, []string{filepath.Join(home, "projects")}, targets.Dirs[parser.AgentCodeBuddy])

	var archive bytes.Buffer
	require.NoError(t, WriteArchive(t.Context(), &archive, targets))
	entries := archiveEntries(t, archive.Bytes())
	names := strings.Join(keys(entries), "\n")
	assert.Contains(t, names, "run-1.jsonl")
	assert.Contains(t, names, strings.Repeat("ab", 32)+".png", "a referenced image travels")
	assert.NotContains(t, names, strings.Repeat("cd", 32), "an unreferenced image stays home")
	assert.NotContains(t, names, "settings.json")
	assert.NotContains(t, names, parser.CodeBuddyCatalogDBName+"-wal", "the catalog travels as one snapshot")

	var snapshot []byte
	for name, body := range entries {
		if strings.HasSuffix(name, "/"+parser.CodeBuddyCatalogDBName) {
			snapshot = body
		}
	}
	require.NotEmpty(t, snapshot)
	path := filepath.Join(t.TempDir(), parser.CodeBuddyCatalogDBName)
	require.NoError(t, os.WriteFile(path, snapshot, 0o600))
	reader, err := sql.Open("sqlite3", path)
	require.NoError(t, err)
	defer reader.Close()
	var title string
	require.NoError(t, reader.QueryRowContext(t.Context(), "SELECT title FROM conversations").Scan(&title))
	assert.Equal(t, "Remote title", title, "the snapshot includes WAL-only writes")
}

func TestImporterReadsCodeBuddyCatalogFromRemoteHome(t *testing.T) {
	database, err := db.Open(t.Context(), filepath.Join(t.TempDir(), "test.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, database.Close()) })
	home, _ := writeCodeBuddyRemoteHome(t)
	targets := resolveTargetsForTest(t, config.Config{AgentDirs: map[parser.AgentType][]string{
		parser.AgentCodeBuddy: {home},
	}})
	var archive bytes.Buffer
	require.NoError(t, WriteArchive(t.Context(), &archive, targets))
	extracted := t.TempDir()
	_, err = ExtractTarStream(t.Context(), bytes.NewReader(archive.Bytes()), extracted)
	require.NoError(t, err)

	stats, err := Importer{Host: "devbox", DB: database}.ImportExtracted(t.Context(), targets, extracted)
	require.NoError(t, err)
	assert.Equal(t, 1, stats.SessionsSynced)
	sess, err := database.GetSessionFull(t.Context(), "devbox~codebuddy:run-1")
	require.NoError(t, err)
	require.NotNil(t, sess)
	require.NotNil(t, sess.SessionName)
	assert.Equal(t, "Remote title", *sess.SessionName, "the remote catalog supplies the title")
	require.NotNil(t, sess.FilePath)
	assert.Equal(t, "devbox:"+filepath.Join(home, "projects", "-work", "run-1.jsonl"), *sess.FilePath)
}
