package sync

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// Two configured homes can hold one conversation, such as a backup beside
// the live home. When CodeBuddy deletes the newer transcript, the archive
// keeps its turns and records the source as missing; the older backup takes
// over only once it is modified after the archived transcript was.
func TestSyncCodeBuddyDeletedNewerCopyKeepsArchivedTurns(t *testing.T) {
	for _, tc := range []struct {
		name        string
		sync        func(t *testing.T, e *Engine, paths ...string)
		wantMissing bool
	}{
		{"sync", func(t *testing.T, e *Engine, _ ...string) {
			e.SyncAll(t.Context(), nil)
		}, false},
		{"change events", func(_ *testing.T, e *Engine, paths ...string) {
			e.SyncPaths(paths)
		}, true},
		{"watched-folder check", func(t *testing.T, e *Engine, _ ...string) {
			require.NoError(t, e.ReconcileWatchRoots(t.Context(), nil, true))
		}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			newerHome, newerProject := cbcSyncHome(t)
			olderHome, olderProject := cbcSyncHome(t)
			newer := filepath.Join(newerProject, "conv1.jsonl")
			older := filepath.Join(olderProject, "conv1.jsonl")
			writeCBCSync(t, newer, cbcCopyTurns(3, "third")...)
			writeCBCSync(t, older, cbcCopyTurns(1, "first")...)
			archived := time.Now().Add(-30 * time.Minute)
			stale := time.Now().Add(-time.Hour)
			require.NoError(t, os.Chtimes(newer, archived, archived))
			require.NoError(t, os.Chtimes(older, stale, stale))

			database := openTestDB(t)
			engine := newCBCEngine(t, database, newerHome, olderHome)
			engine.SyncAll(t.Context(), nil)
			assertCBCCopyOwner(t, database, newer, 3, "the newer copy owns the conversation")

			require.NoError(t, os.Remove(newer))
			tc.sync(t, engine, newer, older)
			assertCBCCopyOwner(t, database, newer, 3,
				"the older backup does not replace the deleted transcript's turns")
			if tc.wantMissing {
				sess, err := database.GetSessionFull(t.Context(), "codebuddy:conv1")
				require.NoError(t, err)
				assertSourceMissingState(t, sess)
			}

			writeCBCSync(t, older, cbcCopyTurns(4, "resumed")...)
			resumed := time.Now()
			require.NoError(t, os.Chtimes(older, resumed, resumed))
			tc.sync(t, engine, older)
			assertCBCCopyOwner(t, database, older, 4,
				"a backup modified after the archived transcript takes over")
		})
	}
}
