package parser

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// cbcImageHome lays out a conversation whose prompt attaches one blob-store
// image named, as CodeBuddy names blobs, by the SHA-256 of its bytes.
func cbcImageHome(t *testing.T) (home, blob, blobPath string, png []byte) {
	t.Helper()
	home, project := cbcHome(t)
	png, err := base64.StdEncoding.DecodeString(cbcPNG)
	require.NoError(t, err)
	sum := sha256.Sum256(png)
	blob = hex.EncodeToString(sum[:])
	blobPath = filepath.Join(home, "blobs", blob[:2], blob+".png")
	require.NoError(t, os.MkdirAll(filepath.Dir(blobPath), 0o755))
	require.NoError(t, os.WriteFile(blobPath, png, 0o600))
	writeCBC(t, filepath.Join(project, "s1.jsonl"),
		`{"id":"u1","type":"message","role":"user","timestamp":1000,"content":[{"type":"input_text","text":"what is this"},{"type":"image_blob_ref","blob_id":"`+blob+`","mime":"image/png","original_filename":"shot.png"}]}`,
		`{"id":"a1","parentId":"u1","type":"message","role":"assistant","timestamp":1001,"content":[{"type":"output_text","text":"a pixel"}]}`,
	)
	return home, blob, blobPath, png
}

func parseCBCImageHome(t *testing.T, home, assetsDir string) ParseResultOutcome {
	t.Helper()
	p := newCodeBuddyProviderFactory(AgentDef{Type: AgentCodeBuddy, IDPrefix: "codebuddy:"}).
		NewProvider(ProviderConfig{Roots: []string{home}, Machine: "m", AssetsDir: assetsDir})
	sources, err := p.Discover(t.Context())
	require.NoError(t, err)
	require.Len(t, sources, 1)
	outcome, err := p.Parse(t.Context(), ParseRequest{Source: sources[0], Machine: "m"})
	require.NoError(t, err)
	require.NotEmpty(t, outcome.Results)
	return outcome.Results[0]
}

// When an image's original is gone, the prompt links the copy an earlier
// import saved, but only a copy whose bytes still match the blob's digest.
func TestCodeBuddyCBCMissingOriginalUsesVerifiedStoredCopy(t *testing.T) {
	tests := []struct {
		name     string
		stored   func(png []byte) []byte
		wantLink bool
	}{
		{name: "verified copy", stored: func(png []byte) []byte { return png }, wantLink: true},
		{name: "altered copy", stored: func(png []byte) []byte { return append(append([]byte(nil), png...), 0) }},
		{name: "never archived"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			home, blob, blobPath, png := cbcImageHome(t)
			require.NoError(t, os.Remove(blobPath))
			assetsDir := t.TempDir()
			if tc.stored != nil {
				require.NoError(t, os.WriteFile(filepath.Join(assetsDir, blob+".png"), tc.stored(png), 0o644))
			}
			got := parseCBCImageHome(t, home, assetsDir)
			assert.Equal(t, DataVersionCurrent, got.DataVersion)
			prompt := got.Result.Messages[0].Content
			if tc.wantLink {
				assert.Contains(t, prompt, "![image](asset://"+blob+".png)")
				assert.NotContains(t, prompt, "[image unavailable")
			} else {
				assert.Contains(t, prompt, "[image unavailable: shot.png]")
			}
		})
	}
}
