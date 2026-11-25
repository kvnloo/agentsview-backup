package server

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestCodeBuddyIDEConversationURL(t *testing.T) {
	assert.Equal(t, "codebuddycn://codebuddy-ide/conversation/open?id=conv_1-a",
		codebuddyIDEConversationURL("codebuddycn", "conv_1-a"))
	assert.Equal(t, "codebuddycn-insiders://codebuddy-ide/conversation/open?id=abc",
		codebuddyIDEConversationURL("codebuddycn-insiders", "abc"))
	for _, bad := range []string{"", "a/b", "a&id=b", "a b", strings.Repeat("x", 129)} {
		assert.Empty(t, codebuddyIDEConversationURL("codebuddycn", bad), bad)
	}
}

func TestOpenURLCommandPerPlatform(t *testing.T) {
	const link = "codebuddycn://codebuddy-ide/conversation/open?id=abc"
	for _, tc := range []struct {
		goos string
		want []string
	}{
		{"darwin", []string{"open", link}},
		{"windows", []string{"rundll32", "url.dll,FileProtocolHandler", link}},
		{"linux", []string{"xdg-open", link}},
	} {
		cmd := openURLCommand(t.Context(), tc.goos, link)
		assert.Equal(t, tc.want[0], filepath.Base(cmd.Args[0]), tc.goos)
		assert.Equal(t, tc.want[1:], cmd.Args[1:], tc.goos)
	}
}
