package parser

import (
	"context"
	"errors"
	"fmt"
	"hash/fnv"
	"os"
	pathpkg "path"
	"path/filepath"
	"strings"
)

// CodeBuddy hosts two storage families under one provider and the codebuddy:
// ID prefix:
//
//   - IDE 4.x keeps each conversation as history/<ws>/<id>/index.json plus
//     messages/<msg>.json under CodeBuddyExtension/Data.
//   - CLI and IDE 5.x keep CBC JSONL transcripts under <home>/projects, with
//     the IDE conversation catalog at <home>/codebuddy.db.
//
// A configured root belongs to a family by layout. A CodeBuddy home root
// contributes its projects directory, and also stays a 4.x root when it holds
// a history directory. A 4.x conversation the IDE converted to CBC keeps its
// 4.x id as the transcript stem, so both families can name the same session;
// the CBC copy wins because it carries the full history and every later turn.

var (
	_ Provider                     = (*codebuddyProvider)(nil)
	_ StreamingDiscoverer          = (*codebuddyProvider)(nil)
	_ ReconciliationSourceRanker   = (*codebuddyProvider)(nil)
	_ StoredSourcePathRanker       = (*codebuddyProvider)(nil)
	_ ReconciliationSourceResolver = (*codebuddyProvider)(nil)
	_ MultiFileStatHasher          = (*codebuddyProvider)(nil)
)

type codebuddyProviderFactory struct {
	def AgentDef
}

func newCodeBuddyProviderFactory(def AgentDef) ProviderFactory {
	return codebuddyProviderFactory{def: cloneAgentDef(def)}
}

func (f codebuddyProviderFactory) Definition() AgentDef {
	return cloneAgentDef(f.def)
}

func (f codebuddyProviderFactory) Capabilities() Capabilities {
	return codeBuddyProviderCapabilities()
}

func (f codebuddyProviderFactory) NewProvider(cfg ProviderConfig) Provider {
	cfg = cfg.Clone()
	legacyRoots, cbcRoots := splitCodeBuddyRoots(cfg.Roots)
	p := &codebuddyProvider{
		ProviderBase: ProviderBase{
			Def: cloneAgentDef(f.def), Caps: codeBuddyProviderCapabilities(),
			Config: cfg,
		},
		legacy: newCodeBuddySourceSet(legacyRoots),
		cbc:    newCodeBuddyCBCSourceSet(cbcRoots, cfg.AssetsDir),
	}
	p.cbc.legacyRoots = legacyRoots
	return p
}

// splitCodeBuddyRoots assigns each configured root to a storage family. A
// directory named projects is a CBC root; a directory holding projects/ or
// codebuddy.db is a CodeBuddy home whose transcripts live in projects/, and
// it also stays a 4.x root when it holds history/; every other root keeps the
// 4.x manifest layout.
func splitCodeBuddyRoots(roots []string) (legacy, cbc []string) {
	for _, root := range roots {
		if strings.HasPrefix(root, "s3://") {
			legacy = append(legacy, root)
			continue
		}
		clean := filepath.Clean(root)
		switch {
		case filepath.Base(clean) == "projects":
			cbc = append(cbc, clean)
		case codebuddyIsHome(clean):
			cbc = append(cbc, filepath.Join(clean, "projects"))
			if info, err := os.Stat(filepath.Join(clean, "history")); err == nil && info.IsDir() {
				legacy = append(legacy, root)
			}
		default:
			legacy = append(legacy, root)
		}
	}
	return legacy, cbc
}

func codebuddyIsHome(root string) bool {
	if info, err := os.Stat(filepath.Join(root, "projects")); err == nil && info.IsDir() {
		return true
	}
	_, err := os.Stat(filepath.Join(root, CodeBuddyCatalogDBName))
	return err == nil
}

type codebuddyProvider struct {
	ProviderBase
	legacy codeBuddySourceSet
	cbc    codebuddyCBCSourceSet
}

// Discover returns every source it could read. When a directory is
// unreadable it still returns the readable sources, together with the
// DiscoveryIncompleteError, so the engine syncs them without tombstoning.
// When several CLI/5.x transcripts hold one conversation, only the copy
// bestCBCCopies picks is returned.
func (p *codebuddyProvider) Discover(ctx context.Context) ([]SourceRef, error) {
	var sources []SourceRef
	seen := make(map[string]struct{})
	err := p.DiscoverEach(ctx, func(source SourceRef) error {
		addJSONLSource(source, &sources, seen)
		return nil
	})
	sources = p.bestCBCCopies(ctx, sources)
	sortJSONLSources(sources)
	return sources, err
}

// bestCBCCopies keeps one transcript per conversation when configured roots
// or project directories hold several copies of it, such as a backup beside
// the live home: the highest-ranked copy that holds a turn. A sync parses the
// copies side by side, and the archive check that keeps newer turns sees only
// what is already written, so on a first import an older copy written last
// would replace the newer turns. When no copy holds a turn, every copy stays,
// which keeps the 4.x history in place as before. 4.x manifests always stay.
func (p *codebuddyProvider) bestCBCCopies(ctx context.Context, sources []SourceRef) []SourceRef {
	copies := make(map[string]int)
	for _, source := range sources {
		if source.ReconciliationIdentity != "" && p.isCBC(source) {
			copies[source.ReconciliationIdentity]++
		}
	}
	type pick struct {
		index int
		rank  ReconciliationSourceRank
	}
	best := make(map[string]pick)
	for i, source := range sources {
		id := source.ReconciliationIdentity
		if copies[id] < 2 || !p.isCBC(source) {
			continue
		}
		_, path, ok := p.cbc.pathFromSource(source)
		if !ok || !codebuddyCBCHasConversation(ctx, path) {
			continue
		}
		rank := p.ReconciliationSourceRank(source)
		rank.Path = path
		if current, ok := best[id]; !ok || codebuddyRankLower(current.rank, rank) {
			best[id] = pick{index: i, rank: rank}
		}
	}
	if len(best) == 0 {
		return sources
	}
	kept := sources[:0]
	for i, source := range sources {
		if winner, ok := best[source.ReconciliationIdentity]; ok && i != winner.index && p.isCBC(source) {
			continue
		}
		kept = append(kept, source)
	}
	return kept
}

// codebuddyRankLower orders copies as the engine's archive check does: class,
// then recency, then the smaller path.
func codebuddyRankLower(a, b ReconciliationSourceRank) bool {
	if a.Class != b.Class {
		return a.Class < b.Class
	}
	if a.Recency != b.Recency {
		return a.Recency < b.Recency
	}
	return a.Path > b.Path
}

// DiscoverEach yields the CLI and IDE 5.x transcripts, then the IDE 4.x
// manifests. An unreadable CLI directory does not stop the 4.x roots from
// being searched; cancellation and yield failures still stop at once.
func (p *codebuddyProvider) DiscoverEach(
	ctx context.Context, yield func(SourceRef) error,
) error {
	cbcErr := p.cbc.DiscoverEach(ctx, yield)
	if cbcErr != nil {
		if _, incomplete := errors.AsType[DiscoveryIncompleteError](cbcErr); !incomplete || ctx.Err() != nil {
			return cbcErr
		}
	}
	return errors.Join(cbcErr, p.legacy.DiscoverEach(ctx, yield))
}

func (p *codebuddyProvider) WatchPlan(ctx context.Context) (WatchPlan, error) {
	legacy, err := p.legacy.WatchPlan(ctx)
	if err != nil {
		return WatchPlan{}, err
	}
	return WatchPlan{Roots: append(p.cbc.WatchRoots(), legacy.Roots...)}, nil
}

func (p *codebuddyProvider) WatchRoots(ctx context.Context) ([]WatchRoot, error) {
	legacy, err := p.legacy.WatchRoots(ctx)
	if err != nil {
		return nil, err
	}
	return append(watchRootMetadata(p.cbc.WatchRoots()), legacy...), nil
}

func (p *codebuddyProvider) SourcesForChangedPath(
	ctx context.Context, req ChangedPathRequest,
) ([]SourceRef, error) {
	cbc, err := p.cbc.SourcesForChangedPath(ctx, req)
	if err != nil || len(cbc) > 0 {
		return cbc, err
	}
	return p.legacy.SourcesForChangedPath(ctx, req)
}

// FindSource prefers the CBC transcript: a converted 4.x conversation keeps
// its id there and carries every later turn.
func (p *codebuddyProvider) FindSource(
	ctx context.Context, req FindSourceRequest,
) (SourceRef, bool, error) {
	req = ProviderFindRequestWithRawSessionID(p.Def, req)
	if source, ok, err := p.cbc.FindSource(ctx, req); ok || err != nil {
		if err != nil || !p.cbcYieldsToLegacy(ctx, source, req) {
			return source, ok, err
		}
	}
	raw := req.RawSessionID
	if base, rest, ok := strings.Cut(raw, ":"); ok && !strings.HasPrefix(rest, "subagent:") {
		// Side questions name their parent conversation.
		req.RawSessionID = base
		if source, ok, err := p.cbc.FindSource(ctx, req); ok || err != nil {
			return source, ok, err
		}
		req.RawSessionID = raw
	}
	return p.legacy.FindSource(ctx, req)
}

// cbcYieldsToLegacy reports whether a found CBC transcript holds no turn of
// the conversation while a 4.x copy of the same session exists, so lookups
// keep reaching the 4.x copy until the transcript gains a turn.
func (p *codebuddyProvider) cbcYieldsToLegacy(
	ctx context.Context, source SourceRef, req FindSourceRequest,
) bool {
	_, path, ok := p.cbc.pathFromSource(source)
	if !ok || codebuddyCBCIdentityForPath(path).subagent || codebuddyCBCHasConversation(ctx, path) {
		return false
	}
	legacyReq := req
	legacyReq.StoredFilePath, legacyReq.FingerprintKey = "", ""
	legacyReq.RawSessionID = codebuddyCBCIdentityForPath(path).rawID
	_, found, err := p.legacy.FindSource(ctx, legacyReq)
	return err == nil && found
}

func (p *codebuddyProvider) isCBC(source SourceRef) bool {
	switch source.Opaque.(type) {
	case codebuddyCBCSource, *codebuddyCBCSource:
		return true
	case JSONLSource, *JSONLSource:
		return false
	case MaterializedFileSource:
		return strings.HasSuffix(source.DisplayPath, ".jsonl") ||
			strings.HasSuffix(source.Key, ".jsonl")
	}
	_, _, ok := p.cbc.pathFromSource(source)
	return ok
}

func (p *codebuddyProvider) Fingerprint(
	ctx context.Context, source SourceRef,
) (SourceFingerprint, error) {
	if p.isCBC(source) {
		return p.cbc.Fingerprint(ctx, source)
	}
	return p.legacy.Fingerprint(ctx, source)
}

func (p *codebuddyProvider) Parse(
	ctx context.Context, req ParseRequest,
) (ParseOutcome, error) {
	req.Machine = firstNonEmptyJSONLString(req.Machine, p.Config.Machine)
	if p.isCBC(req.Source) {
		return p.cbc.Parse(ctx, req)
	}
	if p.legacySuperseded(ctx, req.Source) {
		// The CBC transcript owns this conversation; parsing the frozen 4.x
		// manifest would overwrite later turns with the pre-conversion copy.
		return ParseOutcome{ResultSetComplete: true, SkipReason: SkipNoSession}, nil
	}
	return p.legacy.Parse(ctx, req)
}

// legacySuperseded reports whether a CBC transcript now owns the session a
// 4.x manifest source names. A transcript owns it once it holds a turn of
// the conversation; an empty, meta-only, or unreadable transcript leaves the
// 4.x copy in place.
func (p *codebuddyProvider) legacySuperseded(ctx context.Context, source SourceRef) bool {
	if len(p.cbc.roots) == 0 {
		return false
	}
	path, ok, err := p.legacy.pathFromSource(ctx, source)
	if err != nil || !ok || filepath.Base(path) != "index.json" {
		return false
	}
	id := filepath.Base(filepath.Dir(path))
	cbc, found, err := p.cbc.FindSource(ctx, FindSourceRequest{RawSessionID: id})
	if err != nil || !found {
		return false
	}
	_, cbcPath, ok := p.cbc.pathFromSource(cbc)
	return ok && codebuddyCBCHasConversation(ctx, cbcPath)
}

// ReconciliationSourceRank ranks a CBC transcript above a 4.x manifest for
// the same session, then newer copies above older ones.
func (p *codebuddyProvider) ReconciliationSourceRank(source SourceRef) ReconciliationSourceRank {
	class := int64(1)
	path := source.DisplayPath
	if p.isCBC(source) {
		class = 2
		if _, cbcPath, ok := p.cbc.pathFromSource(source); ok {
			path = cbcPath
		}
	}
	var recency int64
	if info, err := os.Stat(path); err == nil {
		recency = info.ModTime().UnixNano()
	}
	return ReconciliationSourceRank{Class: class, Recency: recency}
}

// RankStoredSourcePath ranks an archived CBC transcript path after CodeBuddy
// deleted the file, such as by its cleanupPeriodDays sweep. The archive then
// keeps the turns added after a 4.x conversation was converted, instead of
// taking the frozen 4.x copy, and the missing transcript is tombstoned. A
// 4.x path carries no rank, so a 4.x session whose manifest is gone stays
// replaceable as before.
func (p *codebuddyProvider) RankStoredSourcePath(path string) (ReconciliationSourceRank, bool) {
	if !codebuddyStoredCBCTranscript(path) {
		return ReconciliationSourceRank{}, false
	}
	return ReconciliationSourceRank{Class: 2}, true
}

// codebuddyStoredCBCTranscript reports whether an archived path names a main
// or subagent CBC transcript under a projects directory. A remote sync can
// archive paths from another operating system, such as a Windows path behind
// a host: prefix on a macOS receiver, so the path is read as text: split on
// slashes alone, then on either slash, without the receiver's path rules.
func codebuddyStoredCBCTranscript(path string) bool {
	for _, form := range []string{path, strings.ReplaceAll(path, `\`, "/")} {
		parts := strings.Split(pathpkg.Clean(form), "/")
		for _, depth := range []int{4, 2} {
			if len(parts) > depth && parts[len(parts)-depth-1] == "projects" &&
				codebuddyCBCTranscriptRel(parts[len(parts)-depth:]) {
				return true
			}
		}
	}
	return false
}

func (p *codebuddyProvider) SourceForReconciliation(
	ctx context.Context, path, project string,
) (SourceRef, bool, error) {
	if err := ctx.Err(); err != nil {
		return SourceRef{}, false, err
	}
	clean := filepath.Clean(path)
	if root, ok := p.cbc.rootFor(clean); ok {
		if !IsRegularFile(clean) {
			return SourceRef{}, false, nil
		}
		return p.cbc.sourceRef(root, clean), true, nil
	}
	return p.legacy.SourceForReconciliation(ctx, path, project)
}

// ComputeMultiFileStatHash digests the transcript, each tool-results
// directory it reads (an agent also reads its parent's) and every saved
// output in them, and the session's own catalog row, keyed by the session id
// the transcript records, so a warm process can skip unchanged CBC sources
// without hashing them, while a catalog write for another conversation leaves
// this digest alone. The engine calls it on a rootless probe provider, so the
// root comes from the path's own layout. 4.x manifests keep content
// verification.
func (p *codebuddyProvider) ComputeMultiFileStatHash(path string) uint64 {
	root, ok := codebuddyCBCRootFromPath(path)
	if !ok {
		return 0
	}
	// Each saved tool output counts by its own metadata: rewriting one in
	// place leaves the directory's metadata unchanged.
	sidecars, err := codebuddyCBCSidecarFiles(context.Background(), path)
	if err != nil {
		return 0
	}
	stats := append([]string{path}, codebuddyCBCToolResultDirs(path)...)
	files := fileStatTupleDigest(0xCB, append(stats, sidecars...)...)
	if files == 0 {
		return 0
	}
	row, err := codebuddyCatalogRowFingerprint(
		context.Background(), codebuddyCatalogPathForProjects(root),
		readCodeBuddyCBCHeader(context.Background(), path).catalogKey(path),
	)
	if err != nil {
		return 0
	}
	h := fnv.New64a()
	_, _ = fmt.Fprintf(h, "%d\x00%s", files, row)
	if sum := h.Sum64(); sum != 0 {
		return sum
	}
	return 1
}

// codebuddyCBCRootFromPath returns the projects root a main or subagent CBC
// transcript path implies.
func codebuddyCBCRootFromPath(path string) (string, bool) {
	clean := filepath.Clean(path)
	// Check the deeper subagent layout first: a subagent path also reads as
	// a main transcript under its session directory.
	for _, depth := range []int{4, 2} {
		root := clean
		for range depth {
			root = filepath.Dir(root)
		}
		if codebuddyCBCSourcePath(root, clean) {
			return root, true
		}
	}
	return "", false
}

func (p *codebuddyProvider) ResolveReconciliationScopes(
	_ context.Context, req ReconciliationScopeRequest,
) (ReconciliationScopePlan, error) {
	roots := append(append([]string(nil), p.cbc.roots...), p.legacy.roots...)
	requested := make([]string, 0, len(req.Roots))
	for _, root := range req.Roots {
		if _, cbc := splitCodeBuddyRoots([]string{root}); len(cbc) == 1 {
			root = cbc[0]
		}
		requested = append(requested, root)
	}
	if err := ValidateReconciliationScopeRoots(p.Def.Type, roots, requested); err != nil {
		return ReconciliationScopePlan{}, err
	}
	return directoryReconciliationScopePlan(roots, requested), nil
}

func codeBuddyProviderCapabilities() Capabilities {
	return Capabilities{
		Source: SourceCapabilities{
			DiscoverSources:      CapabilitySupported,
			StreamingDiscovery:   CapabilitySupported,
			WatchSources:         CapabilitySupported,
			WatchRoots:           CapabilitySupported,
			ClassifyChangedPath:  CapabilitySupported,
			FindSource:           CapabilitySupported,
			CompositeFingerprint: CapabilitySupported,
			MultiSessionSource:   CapabilitySupported,
			PerSessionErrors:     CapabilityNotApplicable,
			ExcludedSessions:     CapabilityNotApplicable,
			ForceReplaceOnParse:  CapabilitySupported,
			MultiFileStatHash:    CapabilitySupported,
		},
		Sync: ProviderSyncSemantics{
			FingerprintHashRequiredForFreshness: true,
			FingerprintHashInCacheKey:           true,
			// An unreadable CLI directory must not hide the readable
			// conversations beside it or the IDE 4.x history.
			PartialDiscovery: true,
		},
		Content: ContentCapabilities{
			FirstMessage:         CapabilitySupported,
			SessionName:          CapabilitySupported,
			Cwd:                  CapabilitySupported,
			Relationships:        CapabilitySupported,
			Subagents:            CapabilitySupported,
			Thinking:             CapabilitySupported,
			ToolCalls:            CapabilitySupported,
			ToolResults:          CapabilitySupported,
			PerMessageTokenUsage: CapabilitySupported,
			TerminationStatus:    CapabilitySupported,
			MalformedLineCount:   CapabilitySupported,
			TruncationStatus:     CapabilitySupported,
			Model:                CapabilitySupported,
			StopReason:           CapabilitySupported,
		},
	}
}
