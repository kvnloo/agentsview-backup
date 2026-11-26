package sync

import (
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/db"
)

func cbcCopyTurn(id, parent, text string, ts int) string {
	parentField := ""
	if parent != "" {
		parentField = `"parentId":"` + parent + `",`
	}
	return `{"id":"` + id + `",` + parentField + `"type":"message","role":"user","timestamp":` +
		strconv.Itoa(ts) + `,"content":[{"type":"input_text","text":"` + text + `"}]}`
}

// cbcCopyTurns returns a conversation of n user turns, the last one carrying
// text.
func cbcCopyTurns(n int, text string) []string {
	lines := make([]string, 0, n)
	parent := ""
	for i := 1; i <= n; i++ {
		id := "u" + strconv.Itoa(i)
		body := "turn " + strconv.Itoa(i)
		if i == n {
			body = text
		}
		lines = append(lines, cbcCopyTurn(id, parent, body, 1767225600000+i*1000))
		parent = id
	}
	return lines
}

func assertCBCCopyOwner(t *testing.T, database *db.DB, path string, turns int, msg string) {
	t.Helper()
	sess, err := database.GetSessionFull(t.Context(), "codebuddy:conv1")
	require.NoError(t, err)
	require.NotNil(t, sess, msg)
	require.NotNil(t, sess.FilePath, msg)
	assert.Equal(t, path, *sess.FilePath, msg)
	assert.Equal(t, turns, sess.UserMessageCount, msg)
}

// Two homes can hold copies of one conversation with the same modification
// time, such as a backup that kept the original's timestamps. Discovery keeps
// the copy with the smaller path, and a change event on the other copy keeps
// that choice instead of switching to it until the next full sync.
func TestSyncCodeBuddyEqualTimeCopiesKeepOneOwner(t *testing.T) {
	for _, tc := range []struct {
		name                string
		smallerTurns, other int
	}{
		{"smaller path holds more turns", 2, 1},
		{"smaller path holds fewer turns", 1, 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			firstHome, firstProject := cbcSyncHome(t)
			secondHome, secondProject := cbcSyncHome(t)
			smaller := filepath.Join(firstProject, "conv1.jsonl")
			larger := filepath.Join(secondProject, "conv1.jsonl")
			require.Less(t, smaller, larger)
			same := time.Now().Add(-time.Hour).Truncate(time.Second)
			writeCopies := func(otherText string) {
				t.Helper()
				writeCBCSync(t, smaller, cbcCopyTurns(tc.smallerTurns, "kept")...)
				writeCBCSync(t, larger, cbcCopyTurns(tc.other, otherText)...)
				for _, path := range []string{smaller, larger} {
					require.NoError(t, os.Chtimes(path, same, same))
				}
			}
			writeCopies("other")

			database := openTestDB(t)
			engine := newCBCEngine(t, database, secondHome, firstHome)
			engine.SyncAll(t.Context(), nil)
			assertCBCCopyOwner(t, database, smaller, tc.smallerTurns, "discovery keeps the smaller path")

			writeCopies("other, edited")
			engine.SyncPaths([]string{larger})
			assertCBCCopyOwner(t, database, smaller, tc.smallerTurns,
				"a change event on the other copy keeps the same owner")

			engine.SyncAll(t.Context(), nil)
			assertCBCCopyOwner(t, database, smaller, tc.smallerTurns, "a full sync agrees")
		})
	}
}
