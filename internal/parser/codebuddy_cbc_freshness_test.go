package parser

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A saved tool output rewritten in place can keep its size and modification
// time. The fingerprint still changes, so the archived text is refreshed.
func TestCodeBuddyCBCFingerprintFollowsSavedOutputContents(t *testing.T) {
	_, project := cbcHome(t)
	projects := filepath.Dir(project)
	path := filepath.Join(project, "run-1.jsonl")
	writeCBC(t, path, `{"id":"u1","type":"message","role":"user","timestamp":1000,"content":[{"type":"input_text","text":"hi"}]}`)
	output := filepath.Join(project, "run-1", "tool-results", "call-1.txt")
	writeCBC(t, output, "first output")
	info, err := os.Stat(output)
	require.NoError(t, err)
	set := newCodeBuddyCBCSourceSet([]string{projects}, "")
	source := set.sourceRef(projects, path)
	before, err := set.Fingerprint(t.Context(), source)
	require.NoError(t, err)

	writeCBC(t, output, "other output")
	require.NoError(t, os.Chtimes(output, info.ModTime(), info.ModTime()))
	rewritten, err := os.Stat(output)
	require.NoError(t, err)
	require.Equal(t, info.Size(), rewritten.Size())
	require.Equal(t, info.ModTime(), rewritten.ModTime())

	after, err := set.Fingerprint(t.Context(), source)
	require.NoError(t, err)
	assert.Equal(t, before.Size, after.Size)
	assert.Equal(t, before.MTimeNS, after.MTimeNS)
	assert.NotEqual(t, before.Hash, after.Hash,
		"a same-size rewrite with the old modification time changes the fingerprint")
}
