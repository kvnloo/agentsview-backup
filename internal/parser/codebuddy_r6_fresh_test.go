package parser

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// codebuddyRewriteInPlace replaces path's contents with body, which must
// have the old size, and restores the old modification time, as an editor
// or a repair tool can.
func codebuddyRewriteInPlace(t *testing.T, path string, body []byte) {
	t.Helper()
	info, err := os.Stat(path)
	require.NoError(t, err)
	require.Len(t, body, int(info.Size()), "the rewrite keeps the size")
	require.NoError(t, os.WriteFile(path, body, info.Mode().Perm()))
	require.NoError(t, os.Chtimes(path, info.ModTime(), info.ModTime()))
	after, err := os.Stat(path)
	require.NoError(t, err)
	require.Equal(t, info.Size(), after.Size())
	require.Equal(t, info.ModTime(), after.ModTime())
}

// A converted conversation reads request totals and images from its 4.x
// directory. Editing one of those files in place, keeping its size and
// modification time, still changes the fingerprint, so the conversation
// shows the corrected usage or the repaired image.
func TestCodeBuddyCBCFingerprintFollowsLegacyCompanionContents(t *testing.T) {
	for _, tc := range []struct {
		name, rel, before, after string
	}{
		{
			name:   "request totals",
			rel:    filepath.Join("messages", "a1.json"),
			before: `{"role":"assistant","createdAt":"2026-01-01T00:00:02Z","extra":{"requestId":"req1","statsSnapshot":{"inputTokens":300,"outputTokens":40}}}`,
			after:  `{"role":"assistant","createdAt":"2026-01-01T00:00:02Z","extra":{"requestId":"req1","statsSnapshot":{"inputTokens":300,"outputTokens":45}}}`,
		},
		{
			name:   "manifest",
			rel:    "index.json",
			before: `{"messages":[{"id":"a1","role":"assistant"}]}`,
			after:  `{"messages":[{"id":"a2","role":"assistant"}]}`,
		},
		{
			name:   "image",
			rel:    filepath.Join("assets", "pic.png"),
			before: "partial image bytes",
			after:  "complete image byte",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, project := cbcHome(t)
			projects := filepath.Dir(project)
			legacyRoot := t.TempDir()
			convDir := filepath.Join(legacyRoot, "history", "ws", "conv1")
			files := map[string]string{
				"index.json":                         `{"messages":[{"id":"a1","role":"assistant"}]}`,
				filepath.Join("messages", "a1.json"): `{"role":"assistant","createdAt":"2026-01-01T00:00:02Z","extra":{"requestId":"req1","statsSnapshot":{"inputTokens":300,"outputTokens":40}}}`,
				filepath.Join("assets", "pic.png"):   "partial image bytes",
			}
			files[tc.rel] = tc.before
			for rel, body := range files {
				writeCBC(t, filepath.Join(convDir, rel), body)
			}
			path := filepath.Join(project, "conv1.jsonl")
			writeCBC(t, path,
				`{"type":"session-meta","id":"m","sessionId":"conv1","meta":{"codebuddy.ai/converterVersion":"ide-main-craft-jsonl/29"}}`,
				`{"id":"u1","type":"message","role":"user","timestamp":1767225601000,"content":[{"type":"input_text","text":"old prompt"},{"type":"input_image","image":"codebuddy-asset://assets/pic.png"}]}`,
			)
			set := newCodeBuddyCBCSourceSet([]string{projects}, "")
			set.legacyRoots = []string{legacyRoot}
			source := set.sourceRef(projects, path)
			before, err := set.Fingerprint(t.Context(), source)
			require.NoError(t, err)

			codebuddyRewriteInPlace(t, filepath.Join(convDir, tc.rel), []byte(tc.after+"\n"))

			after, err := set.Fingerprint(t.Context(), source)
			require.NoError(t, err)
			assert.NotEqual(t, before.Hash, after.Hash,
				"an in-place edit of a 4.x file the conversation reads changes the fingerprint")
		})
	}
}
