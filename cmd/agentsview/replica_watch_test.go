package main

import (
	"fmt"
	"log"
	"os"
	"path/filepath"
	"testing"

	"github.com/gofrs/flock"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestReplicaPushRespectsSavedNoDaemonAutostart(t *testing.T) {
	for _, watch := range []bool{false, true} {
		t.Run(fmt.Sprintf("watch=%t", watch), func(t *testing.T) {
			dir := t.TempDir()
			isolateDeploymentEnv(t)
			t.Setenv("AGENTSVIEW_DATA_DIR", dir)
			t.Setenv("AGENTSVIEW_NO_DAEMON", "")
			restoreTestLogOutput(t)
			writeTestConfig(t, dir, `no_daemon_autostart = true
[pg]
url = "postgres://user@127.0.0.1:1/archive"
allow_insecure = true
`)
			require.NoError(t, os.Mkdir(filepath.Join(dir, "sessions.db"), 0o700))
			forbidStartBackgroundServeForTransport(t, "saved setting must suppress replica daemon start")
			var err error
			if watch {
				err = runReplicaPushWatch(t.Context(), pgReplica{}, ReplicaPushConfig{}, "")
			} else {
				err = runReplicaPush(t.Context(), pgReplica{}, ReplicaPushConfig{}, "")
			}
			require.Error(t, err)
			assert.NotContains(t, err.Error(), "no_daemon_autostart")
		})
	}
}

func TestReplicaWatchLockLeavesDataDirUntouched(t *testing.T) {
	dir, lockPath := replicaWatchLockFixture(t)
	check := replicaWatchDataDirCheck(t, dir)

	err := runReplicaPushWatch(t.Context(), pgReplica{}, ReplicaPushConfig{}, "")
	require.EqualError(t, err, "already locked ("+lockPath+")")
	check()
}

func replicaWatchLockFixture(t *testing.T) (string, string) {
	t.Helper()
	dir := t.TempDir()
	isolateDeploymentEnv(t)
	t.Setenv("AGENTSVIEW_DATA_DIR", dir)
	output := log.Writer()
	t.Cleanup(func() {
		watchLog := log.Writer()
		log.SetOutput(output)
		if f, ok := watchLog.(*os.File); ok && watchLog != output {
			require.NoError(t, f.Close())
		}
	})
	writeTestConfig(t, dir, `
[pg]
url = "postgres://user@127.0.0.1:1/archive"
allow_insecure = true
`)
	lockPath := filepath.Join(dir, "pg-watch.lock")
	lock := flock.New(lockPath)
	require.NoError(t, lock.Lock())
	t.Cleanup(func() { require.NoError(t, lock.Close()) })
	return dir, lockPath
}
