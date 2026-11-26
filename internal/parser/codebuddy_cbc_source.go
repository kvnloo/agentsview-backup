package parser

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
)

// codebuddyCBCSource is the opaque payload of one CBC transcript source.
type codebuddyCBCSource struct {
	Root string
	Path string
}

// codebuddyCBCSourceSet discovers CBC transcripts under projects roots:
// <root>/<project>/<session>.jsonl and
// <root>/<project>/<session>/subagents/agent-*.jsonl.
type codebuddyCBCSourceSet struct {
	roots     []string
	assetsDir string
	// legacyRoots are the configured 4.x roots. A converted conversation
	// reads its pre-conversion usage from them.
	legacyRoots []string
}

func newCodeBuddyCBCSourceSet(roots []string, assetsDir string) codebuddyCBCSourceSet {
	return codebuddyCBCSourceSet{roots: cleanJSONLRoots(roots), assetsDir: assetsDir}
}

// codebuddyCBCSourcePath reports whether path is a main or subagent CBC
// transcript under root.
func codebuddyCBCSourcePath(root, path string) bool {
	rel, ok := relUnder(filepath.Clean(root), filepath.Clean(path))
	return ok && codebuddyCBCTranscriptRel(strings.Split(filepath.ToSlash(rel), "/"))
}

// codebuddyCBCTranscriptRel reports whether parts, the components of a path
// relative to a projects root, name a main or subagent CBC transcript.
func codebuddyCBCTranscriptRel(parts []string) bool {
	if len(parts) == 0 || !strings.HasSuffix(parts[len(parts)-1], ".jsonl") {
		return false
	}
	switch len(parts) {
	case 2:
		stem := strings.TrimSuffix(parts[1], ".jsonl")
		return parts[0] != "" && IsValidSessionID(stem) && !strings.HasPrefix(stem, "agent-")
	case 4:
		stem := strings.TrimSuffix(parts[3], ".jsonl")
		return parts[2] == "subagents" && IsValidSessionID(parts[1]) &&
			strings.HasPrefix(stem, "agent-") && IsValidSessionID(stem)
	}
	return false
}

func (s codebuddyCBCSourceSet) Discover(ctx context.Context) ([]SourceRef, error) {
	return collectDiscoveredSources(ctx, s.DiscoverEach)
}

func (s codebuddyCBCSourceSet) DiscoverEach(
	ctx context.Context, yield func(SourceRef) error,
) error {
	var incomplete error
	for _, root := range s.roots {
		if err := ctx.Err(); err != nil {
			return err
		}
		err := streamDirectoryEntries(ctx, root, func(project os.DirEntry) error {
			isDir, dirErr := streamingDirCandidateOrIncomplete(
				AgentCodeBuddy, "CodeBuddy project directory", project, root,
			)
			if dirErr != nil {
				incomplete = errors.Join(incomplete, dirErr)
				return nil
			}
			if !isDir {
				return nil
			}
			err := s.discoverProject(ctx, root, filepath.Join(root, project.Name()), yield)
			if err == nil {
				return nil
			}
			if _, ok := discoveryYieldCause(err); ok || ctx.Err() != nil {
				return err
			}
			// One unreadable project must not hide the projects after it.
			incomplete = errors.Join(incomplete, err)
			return nil
		})
		if cause, ok := discoveryYieldCause(err); ok {
			return cause
		}
		if err != nil {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return ctxErr
			}
			// An unreadable projects root must not hide the other roots or
			// the 4.x history searched after them.
			incomplete = errors.Join(incomplete,
				incompleteDiscoveryError(AgentCodeBuddy, "read "+root, err))
		}
	}
	return incomplete
}

// discoverProject yields the main and subagent transcripts of one project
// directory. An unreadable subagents directory makes the result incomplete
// without stopping the rest of the project.
func (s codebuddyCBCSourceSet) discoverProject(
	ctx context.Context, root, projectDir string, yield func(SourceRef) error,
) error {
	entries, err := os.ReadDir(projectDir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return incompleteDiscoveryError(AgentCodeBuddy, "read "+projectDir, err)
	}
	var incomplete error
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return err
		}
		path := filepath.Join(projectDir, entry.Name())
		if entry.IsDir() {
			subagents := filepath.Join(path, "subagents")
			subs, err := os.ReadDir(subagents)
			if err != nil {
				if !errors.Is(err, os.ErrNotExist) && !errors.Is(err, syscall.ENOTDIR) {
					incomplete = errors.Join(incomplete,
						incompleteDiscoveryError(AgentCodeBuddy, "read "+subagents, err))
				}
				continue
			}
			for _, sub := range subs {
				subPath := filepath.Join(subagents, sub.Name())
				if sub.Type().IsRegular() && codebuddyCBCSourcePath(root, subPath) {
					if err := yield(s.sourceRef(root, subPath)); err != nil {
						return discoveryYieldError{cause: err}
					}
				}
			}
			continue
		}
		if entry.Type().IsRegular() && codebuddyCBCSourcePath(root, path) {
			if err := yield(s.sourceRef(root, path)); err != nil {
				return discoveryYieldError{cause: err}
			}
		}
	}
	return incomplete
}

func (s codebuddyCBCSourceSet) sourceRef(root, path string) SourceRef {
	projectDir, _, _ := codebuddyCBCPathParts(path)
	return SourceRef{
		Provider:       AgentCodeBuddy,
		ConfiguredRoot: root,
		Key:            path,
		DisplayPath:    path,
		FingerprintKey: path,
		// Copies of one conversation under different roots or projects name
		// the same session; the engine ranks each against the archived copy.
		ReconciliationIdentity: codebuddyCBCIdentityForPath(path).rawID,
		ProjectHint:            filepath.Base(projectDir),
		Opaque:                 codebuddyCBCSource{Root: root, Path: path},
	}
}

func (s codebuddyCBCSourceSet) rootFor(path string) (string, bool) {
	for _, root := range s.roots {
		if codebuddyCBCSourcePath(root, path) {
			return root, true
		}
	}
	return "", false
}

func (s codebuddyCBCSourceSet) WatchRoots() []WatchRoot {
	roots := make([]WatchRoot, 0, len(s.roots)*2)
	for _, root := range s.roots {
		roots = append(roots, WatchRoot{
			Path: root, Recursive: true,
			DebounceKey: string(AgentCodeBuddy) + ":cbc:" + root,
		})
		if catalog := codebuddyCatalogPathForProjects(root); catalog != "" {
			roots = append(roots, WatchRoot{
				Path: filepath.Dir(catalog), Recursive: false,
				IncludeGlobs: []string{CodeBuddyCatalogDBName, CodeBuddyCatalogDBName + "-wal"},
				DebounceKey:  string(AgentCodeBuddy) + ":catalog:" + catalog,
			})
		}
	}
	return roots
}

// SourcesForChangedPath maps a transcript, a persisted tool output, or a
// catalog write back to the transcripts it affects. A catalog write selects
// only the conversations whose catalog rows changed since the previous
// catalog event, so the work follows the changed rows rather than the size
// of the archive.
func (s codebuddyCBCSourceSet) SourcesForChangedPath(
	ctx context.Context, req ChangedPathRequest,
) ([]SourceRef, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	path := filepath.Clean(req.Path)
	for _, root := range s.roots {
		if req.WatchRoot != "" && !samePath(req.WatchRoot, root) &&
			!samePath(req.WatchRoot, filepath.Dir(codebuddyCatalogPathForProjects(root))) {
			continue
		}
		if codebuddyCBCSourcePath(root, path) {
			if IsRegularFile(path) || jsonlMissingPathFallbackAllowed(req) {
				return []SourceRef{s.sourceRef(root, path)}, nil
			}
			return nil, nil
		}
		if readers, ok := codebuddyCBCToolResultReaders(root, path); ok {
			var out []SourceRef
			for _, reader := range readers {
				if IsRegularFile(reader) {
					out = append(out, s.sourceRef(root, reader))
				}
			}
			return out, nil
		}
		if catalog := codebuddyCatalogPathForProjects(root); catalog != "" &&
			strings.TrimSuffix(path, "-wal") == catalog {
			return s.sourcesForCatalogChange(ctx, root, catalog)
		}
	}
	return nil, nil
}

func (s codebuddyCBCSourceSet) sourcesForCatalogChange(
	ctx context.Context, root, catalog string,
) ([]SourceRef, error) {
	changed, known, err := codebuddyCatalogChangedRuntimes(ctx, catalog)
	if err != nil {
		return nil, err
	}
	one := codebuddyCBCSourceSet{roots: []string{root}, assetsDir: s.assetsDir}
	if !known {
		// No earlier read to compare against: fingerprints still skip
		// sessions whose rows did not change.
		return one.Discover(ctx)
	}
	var out []SourceRef
	for _, runtimeID := range changed {
		source, ok, err := one.FindSource(ctx, FindSourceRequest{RawSessionID: runtimeID})
		if err != nil {
			return nil, err
		}
		if ok {
			out = append(out, source)
		}
	}
	return out, nil
}

// codebuddyCBCToolResultReaders maps a file under a tool-results directory
// to the transcripts that read it. An agent reads its own directory and its
// parent session's, so a parent output refreshes the parent and every agent
// transcript of that session.
func codebuddyCBCToolResultReaders(root, path string) ([]string, bool) {
	rel, ok := relUnder(filepath.Clean(root), filepath.Clean(path))
	if !ok {
		return nil, false
	}
	parts := strings.Split(filepath.ToSlash(rel), "/")
	switch {
	case len(parts) == 4 && parts[2] == "tool-results":
		readers := []string{filepath.Join(root, parts[0], parts[1]+".jsonl")}
		subagents := filepath.Join(root, parts[0], parts[1], "subagents")
		entries, _ := os.ReadDir(subagents)
		for _, entry := range entries {
			if agent := filepath.Join(subagents, entry.Name()); codebuddyCBCSourcePath(root, agent) {
				readers = append(readers, agent)
			}
		}
		return readers, true
	case len(parts) == 6 && parts[2] == "subagents" && parts[4] == "tool-results":
		return []string{filepath.Join(root, parts[0], parts[1], "subagents", parts[3]+".jsonl")}, true
	}
	return nil, false
}

func (s codebuddyCBCSourceSet) FindSource(
	ctx context.Context, req FindSourceRequest,
) (SourceRef, bool, error) {
	if err := ctx.Err(); err != nil {
		return SourceRef{}, false, err
	}
	for _, stored := range []string{req.StoredFilePath, req.FingerprintKey} {
		if stored == "" {
			continue
		}
		if root, ok := s.rootFor(stored); ok && IsRegularFile(stored) {
			return s.sourceRef(root, filepath.Clean(stored)), true, nil
		}
	}
	raw := req.RawSessionID
	if raw == "" {
		return SourceRef{}, false, nil
	}
	sessionID, agentID, subagent := strings.Cut(raw, ":subagent:")
	if !IsValidSessionID(sessionID) || subagent && !IsValidSessionID(agentID) {
		return SourceRef{}, false, nil
	}
	for _, root := range s.roots {
		projects, err := os.ReadDir(root)
		if err != nil {
			continue
		}
		for _, project := range projects {
			if !isDirOrSymlink(project, root) {
				continue
			}
			candidate := filepath.Join(root, project.Name(), sessionID+".jsonl")
			if subagent {
				candidate = filepath.Join(root, project.Name(), sessionID, "subagents", agentID+".jsonl")
			}
			if IsRegularFile(candidate) {
				return s.sourceRef(root, candidate), true, nil
			}
		}
	}
	return SourceRef{}, false, nil
}

func (s codebuddyCBCSourceSet) pathFromSource(source SourceRef) (string, string, bool) {
	switch src := source.Opaque.(type) {
	case codebuddyCBCSource:
		return src.Root, src.Path, src.Path != ""
	case *codebuddyCBCSource:
		if src != nil {
			return src.Root, src.Path, src.Path != ""
		}
	case MaterializedFileSource:
		return "", src.Path, src.Path != ""
	}
	for _, candidate := range []string{source.DisplayPath, source.FingerprintKey, source.Key} {
		if root, ok := s.rootFor(candidate); ok {
			return root, filepath.Clean(candidate), true
		}
	}
	return "", "", false
}

// Fingerprint hashes the transcript, its persisted tool outputs, and its
// catalog row, so a change to any of them reparses the session while
// unrelated catalog writes leave it untouched.
func (s codebuddyCBCSourceSet) Fingerprint(
	ctx context.Context, source SourceRef,
) (SourceFingerprint, error) {
	root, path, ok := s.pathFromSource(source)
	if !ok {
		return SourceFingerprint{}, errors.New("codebuddy source path unavailable")
	}
	info, err := os.Stat(path)
	if err != nil {
		return SourceFingerprint{}, fmt.Errorf("stat %s: %w", path, err)
	}
	transcriptHash, err := hashJSONLSourceFileContext(ctx, path)
	if err != nil {
		return SourceFingerprint{}, err
	}
	h := sha256.New()
	_, _ = fmt.Fprintf(h, "transcript\x00%s\x00", transcriptHash)
	size := info.Size()
	mtime := info.ModTime().UnixNano()
	sidecars, err := codebuddyCBCSidecarFiles(ctx, path)
	if err != nil {
		return SourceFingerprint{}, err
	}
	for _, sidecar := range sidecars {
		sidecarInfo, err := os.Stat(sidecar)
		if err != nil {
			continue
		}
		rel, _ := filepath.Rel(filepath.Dir(path), sidecar)
		_, _ = fmt.Fprintf(h, "sidecar\x00%s\x00%d\x00%d\x00",
			filepath.ToSlash(rel), sidecarInfo.Size(), sidecarInfo.ModTime().UnixNano())
		size += sidecarInfo.Size()
		mtime = max(mtime, sidecarInfo.ModTime().UnixNano())
	}
	if root != "" {
		catalog, err := loadCodeBuddyCatalog(ctx, codebuddyCatalogPathForProjects(root))
		if err == nil {
			_, _ = fmt.Fprintf(h, "catalog\x00%s\x00",
				catalog.fingerprint(codebuddyCBCIdentityForPath(path).runtimeID))
		}
	}
	inode, device := sourceFileIdentity(info)
	return SourceFingerprint{
		Key:     firstNonEmptyJSONLString(source.FingerprintKey, source.Key, path),
		Size:    size,
		MTimeNS: mtime,
		Inode:   inode,
		Device:  device,
		Hash:    hex.EncodeToString(h.Sum(nil)),
	}, nil
}

// codebuddyCBCSidecarFiles lists the persisted tool outputs a transcript can
// read, sorted for a stable fingerprint. An agent transcript also reads its
// parent session's outputs.
func codebuddyCBCSidecarFiles(ctx context.Context, path string) ([]string, error) {
	var files []string
	for _, dir := range codebuddyCBCToolResultDirs(path) {
		err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return ctxErr
			}
			if err != nil {
				return err
			}
			if d.Type().IsRegular() {
				files = append(files, p)
			}
			return nil
		})
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return nil, err
		}
	}
	slices.Sort(files)
	return slices.Compact(files), nil
}

// codebuddyCBCToolResultDirs lists the tool-results directories the
// persisted-output reader accepts for a transcript, without duplicates.
func codebuddyCBCToolResultDirs(path string) []string {
	var dirs []string
	for _, dir := range claudeToolResultDirs(path) {
		if !slices.Contains(dirs, dir) {
			dirs = append(dirs, dir)
		}
	}
	return dirs
}

func (s codebuddyCBCSourceSet) Parse(
	ctx context.Context, req ParseRequest,
) (ParseOutcome, error) {
	if err := ctx.Err(); err != nil {
		return ParseOutcome{}, err
	}
	root, path, ok := s.pathFromSource(req.Source)
	if !ok {
		return ParseOutcome{}, errors.New("codebuddy source path unavailable")
	}
	in := codebuddyCBCParseInput{
		path: path, project: req.Source.ProjectHint, machine: req.Machine,
		legacyRoots: s.legacyRoots,
	}
	if root != "" {
		catalog, err := loadCodeBuddyCatalog(ctx, codebuddyCatalogPathForProjects(root))
		if err != nil {
			return ParseOutcome{}, err
		}
		if catalog != nil {
			in.catalog = catalog.byRuntime[codebuddyCBCIdentityForPath(path).runtimeID]
		}
		in.opts.assets = codebuddyBlobAssets{
			home: filepath.Dir(filepath.Clean(root)), assetsDir: s.assetsDir,
		}
	}
	in.opts.persisted = codebuddyPersistedOutputReader(path, req.StoredPathResolver, req.Machine)
	results, err := parseCodeBuddyCBCFile(ctx, in)
	if err != nil {
		return ParseOutcome{}, err
	}
	if in.catalog != nil {
		results = append(results, codebuddySideQuestionResults(results, in.catalog)...)
	}
	if len(results) == 0 {
		return ParseOutcome{ResultSetComplete: true, SkipReason: SkipNoSession}, nil
	}
	out := make([]ParseResultOutcome, 0, len(results))
	for _, result := range results {
		if req.Fingerprint.Hash != "" {
			result.Session.File.Hash = req.Fingerprint.Hash
		}
		if req.Fingerprint.Size > 0 {
			result.Session.File.Size = req.Fingerprint.Size
		}
		if req.Fingerprint.MTimeNS > 0 {
			result.Session.File.Mtime = req.Fingerprint.MTimeNS
		}
		// An unreadable persisted output stays flagged on its tool result so
		// archive reconciliation keeps the text saved earlier. A late file
		// changes the fingerprint, which reparses the session.
		out = append(out, ParseResultOutcome{Result: result, DataVersion: DataVersionCurrent})
	}
	return ParseOutcome{Results: out, ResultSetComplete: true, ForceReplace: true}, nil
}

// codebuddyPersistedOutputReader reads a persisted tool output only from the
// transcript's own tool-results directories.
func codebuddyPersistedOutputReader(
	transcript string, resolve func(string) (string, bool), machine string,
) codebuddyPersistedOutputs {
	return func(ctx context.Context, path string) (string, bool, error) {
		if resolve != nil {
			if local, ok := resolve(path); ok {
				path = local
			} else if local, ok := resolve(machine + ":" + path); ok {
				path = local
			}
		}
		return readClaudePersistedToolResultContext(ctx, transcript, path)
	}
}
