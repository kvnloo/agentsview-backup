package parser

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
)

// CodeBuddyCutoffMtime returns the stat-only freshness signal an incremental
// sync compares with its cutoff for a CodeBuddy CLI or IDE 5.x transcript.
// Each input the parse reads counts by the later of its modification and
// change times, so a rewrite that restores the modification time, or a file
// restored from a backup with its old time, still looks recent. The inputs
// are the transcript and its saved tool outputs, the IDE catalog beside the
// projects folder (titles and renames live there), every blob-store path the
// transcript's images can be read from (a missing image counts as nothing,
// so one that appears later looks recent), and, for a conversation the IDE
// converted from 4.x, the 4.x directory whose request totals and pasted
// images the parse still reads. Listing the images reads the transcript only
// when its metadata changed since this process last scanned it. Copies of a
// conversation are ranked by the transcript time alone, so this value only
// decides whether a quick sync looks at the file again. roots are the
// configured CodeBuddy roots.
func CodeBuddyCutoffMtime(ctx context.Context, path string, roots []string) (int64, error) {
	if _, err := os.Stat(path); err != nil {
		return 0, err
	}
	paths := []string{path}
	for _, dir := range codebuddyCBCToolResultDirs(path) {
		// The session directory outlives a removed tool-results directory,
		// and directory times record removed outputs.
		paths = append(paths, filepath.Dir(dir))
		err := filepath.WalkDir(dir, func(p string, _ fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			paths = append(paths, p)
			return ctx.Err()
		})
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return 0, err
		}
	}
	if root, ok := codebuddyCBCRootFromPath(path); ok {
		if catalog := codebuddyCatalogPathForProjects(root); catalog != "" {
			paths = append(paths, catalog, catalog+"-wal")
		}
		blobs, err := codebuddyTranscriptBlobPaths(ctx, filepath.Dir(root), path)
		if err != nil {
			return 0, err
		}
		paths = append(paths, blobs...)
	}
	legacy, err := codebuddyLegacyCutoffPaths(ctx, path, roots)
	if err != nil {
		return 0, err
	}
	paths = append(paths, legacy...)
	var mtime int64
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

// codebuddyLegacyCutoffPaths lists the 4.x directory, manifest, and files a
// converted transcript's parse reads. The directory times record removed
// entries. Transcripts the IDE did not convert, and sync setups without 4.x
// roots, have none.
func codebuddyLegacyCutoffPaths(ctx context.Context, path string, roots []string) ([]string, error) {
	legacyRoots, _ := splitCodeBuddyRoots(roots)
	if len(legacyRoots) == 0 {
		return nil, nil
	}
	header := readCodeBuddyCBCHeader(ctx, path)
	if !header.converted {
		return nil, nil
	}
	dir := codebuddyLegacySessionDir(ctx, legacyRoots, header.cwd,
		codebuddyCBCIdentityForPath(path).rawID)
	if dir == "" {
		return nil, ctx.Err()
	}
	messages := filepath.Join(dir, "messages")
	assets := filepath.Join(dir, "assets")
	paths := []string{dir, filepath.Join(dir, "index.json"), messages, assets}
	for _, child := range []string{messages, assets} {
		entries, _ := os.ReadDir(child)
		for _, entry := range entries {
			if entry.Type().IsRegular() {
				paths = append(paths, filepath.Join(child, entry.Name()))
			}
		}
	}
	return paths, nil
}
