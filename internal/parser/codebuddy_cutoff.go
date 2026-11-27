package parser

import (
	"context"
	"os"
	"path/filepath"
)

// CodeBuddyCutoffMtime returns the stat-only freshness signal an incremental
// sync compares with its cutoff for a CodeBuddy CLI or IDE 5.x transcript. It
// covers the transcript and its saved tool outputs and, for a conversation
// the IDE converted from 4.x, the 4.x directory whose request totals and
// pasted images the parse still reads. Copies of a conversation are ranked by
// the transcript time alone, so this value only decides whether a quick sync
// looks at the file again. roots are the configured CodeBuddy roots.
func CodeBuddyCutoffMtime(ctx context.Context, path string, roots []string) (int64, error) {
	mtime, err := ClaudeLayoutCompositeMtime(path)
	if err != nil {
		return 0, err
	}
	legacyRoots, _ := splitCodeBuddyRoots(roots)
	if len(legacyRoots) == 0 {
		return mtime, nil
	}
	header := readCodeBuddyCBCHeader(ctx, path)
	if !header.converted {
		return mtime, nil
	}
	dir := codebuddyLegacySessionDir(ctx, legacyRoots, header.cwd,
		codebuddyCBCIdentityForPath(path).rawID)
	if dir == "" {
		return mtime, ctx.Err()
	}
	messages := filepath.Join(dir, "messages")
	assets := filepath.Join(dir, "assets")
	// The conversation and child directory times record removed entries.
	paths := []string{dir, filepath.Join(dir, "index.json"), messages, assets}
	for _, child := range []string{messages, assets} {
		entries, _ := os.ReadDir(child)
		for _, entry := range entries {
			if entry.Type().IsRegular() {
				paths = append(paths, filepath.Join(child, entry.Name()))
			}
		}
	}
	for _, p := range paths {
		if err := ctx.Err(); err != nil {
			return 0, err
		}
		info, err := os.Stat(p)
		if err != nil {
			continue
		}
		ctime, _ := codexIndexChangeTime(p, info)
		mtime = max(mtime, info.ModTime().UnixNano(), ctime)
	}
	return mtime, nil
}
