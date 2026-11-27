package parser

import (
	"context"
	"os"
	"path/filepath"
	"strings"
)

// codebuddyCBCIdentity names the sessions one transcript produces.
type codebuddyCBCIdentity struct {
	// rawID is the archive identity without the codebuddy: prefix.
	rawID string
	// parentRawID links a subagent transcript to its parent session.
	parentRawID string
	// runtimeID is the CLI session id used by codebuddy --resume.
	runtimeID string
	subagent  bool
}

// codebuddyCBCIdentityForPath derives archive identity from the transcript
// layout. Main transcripts use the filename stem; subagents nest under the
// parent session directory. Converted 4.x sessions keep their 4.x id, which
// is also their CBC filename stem.
func codebuddyCBCIdentityForPath(path string) codebuddyCBCIdentity {
	_, sessionStem, agentStem := codebuddyCBCPathParts(path)
	if agentStem != "" {
		return codebuddyCBCIdentity{
			rawID:       sessionStem + ":subagent:" + agentStem,
			parentRawID: sessionStem,
			runtimeID:   agentStem,
			subagent:    true,
		}
	}
	return codebuddyCBCIdentity{rawID: sessionStem, runtimeID: sessionStem}
}

// codebuddyCBCParseInput carries everything one transcript parse needs.
type codebuddyCBCParseInput struct {
	path    string
	project string
	machine string
	opts    codebuddyCBCBuildOptions
	catalog *codebuddyCatalogEntry
	// legacyRoots are the configured 4.x roots, read for a converted
	// conversation's request totals and pasted images.
	legacyRoots []string
}

// parseCodeBuddyCBCFile parses one CBC transcript into its active session and
// any preserved alternative branches. A transcript with no conversation
// returns no results. A conversation rewound before its first message has no
// active session, but its earlier history still returns as branches.
func parseCodeBuddyCBCFile(
	ctx context.Context, in codebuddyCBCParseInput,
) ([]ParseResult, error) {
	t, err := readCodeBuddyCBCTranscript(ctx, in.path)
	if err != nil {
		return nil, err
	}
	ident := codebuddyCBCIdentityForPath(in.path)
	// A transcript with no conversation item does not replace the 4.x copy
	// of the same conversation, which still holds its turns.
	if !ident.subagent && !t.hasConversation() &&
		codebuddyLegacySessionDir(ctx, in.legacyRoots, t.cwd, ident.rawID) != "" {
		return nil, nil
	}
	if t.sessionID != "" && !ident.subagent {
		ident.runtimeID = t.sessionID
	}
	var legacyOrder []string
	legacy := t.converted && !ident.subagent
	// Without its full 4.x directory a converted conversation lacks request
	// totals and pasted images only that directory holds.
	companionIncomplete := legacy
	if legacy && len(in.legacyRoots) > 0 {
		companion := loadCodeBuddyLegacyCompanion(ctx, in.legacyRoots, t.cwd, ident.rawID)
		companionIncomplete = !companion.complete
		in.opts.legacyUsage = codebuddyLegacyUsageMissingFrom(companion.usage, t.items)
		legacyOrder = companion.order
		if blobs, ok := in.opts.assets.(codebuddyBlobAssets); ok {
			blobs.legacyAssets = companion.assetsDir
			in.opts.assets = blobs
		} else if in.opts.assets == nil && companion.assetsDir != "" {
			in.opts.assets = codebuddyBlobAssets{legacyAssets: companion.assetsDir}
		}
	}
	if ident.subagent {
		in.opts.taskAssignment, in.opts.hasTaskAssignment = codebuddyTeamTaskAssignment(t.items)
	}
	active := codebuddyCBCReplay(t.items)
	main, err := buildCodeBuddyCBCMessages(ctx, t.items, active, in.opts)
	if err != nil {
		return nil, err
	}
	main.messages = codebuddyInsertLegacyUsage(main.messages, in.opts.legacyUsage,
		legacyOrder, main.legacyClaimed)
	codebuddyFillUsageModels(main.messages)
	project := in.project
	if t.cwd != "" {
		if extracted := ExtractProjectFromCwdWithBranchContext(ctx, t.cwd, ""); extracted != "" {
			project = extracted
		}
	}
	if project == "" {
		project = GetProjectName(in.project)
	}
	custom, generated := codebuddyCBCTitles(t.titleFacts)
	base := ParsedSession{
		ID:             "codebuddy:" + ident.rawID,
		Project:        project,
		Machine:        in.machine,
		Agent:          AgentCodeBuddy,
		Entrypoint:     t.entrypoint,
		Cwd:            t.cwd,
		MalformedLines: t.malformed,
		IsTruncated:    t.truncated,
		File:           codebuddyCBCFileInfo(in.path),
	}
	if !ident.subagent {
		base.SourceSessionID = ident.runtimeID
	}
	if t.teamMember != "" {
		base.AgentLabel = t.teamMember
	} else if ident.subagent {
		base.AgentLabel = t.speaker
	}
	if ident.subagent {
		base.ParentSessionID = "codebuddy:" + ident.parentRawID
		base.RelationshipType = RelSubagent
		base.SourceSessionID = ident.runtimeID
	}
	title := firstNonEmptyJSONLString(custom, generated)
	if in.catalog != nil {
		title = firstNonEmptyJSONLString(in.catalog.customTitle, custom,
			in.catalog.title, generated)
		if in.catalog.forkParentRuntimeID != "" && !ident.subagent {
			parent, err := codebuddyCBCRawIDForRuntime(ctx, in.path,
				in.catalog.forkParentRuntimeID)
			if err != nil {
				return nil, err
			}
			base.ParentSessionID = "codebuddy:" + parent
			base.RelationshipType = RelFork
		}
	}
	var results []ParseResult
	// An empty active session is left out: the sidebar hides an empty
	// session together with its children, so emitting one would also hide
	// the preserved branches. The branches still name it as their parent and
	// nest under it once the conversation continues.
	if len(main.messages) > 0 || t.truncated {
		mainSess := base
		codebuddyFinishSession(&mainSess, main, title)
		mainSess.LegacyCompanionIncomplete = companionIncomplete ||
			(legacy && main.legacyImageMissing)
		mainSess.StartedAt = earlierTime(t.firstTime, main.startedAt)
		mainSess.EndedAt = laterTime(t.lastTime, main.endedAt)
		mainSess.TerminationStatus = Classify(main.messages,
			codebuddyStopReason(t.stopReason, main.messages), t.truncated)
		results = append(results, ParseResult{Session: mainSess, Messages: main.messages})
	}
	// A call in a preserved branch names its child transcript only in that
	// branch's result, so every path contributes its mappings.
	calls := main.subagentCalls
	for _, branch := range codebuddyCBCAlternatives(t.items, active) {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		alt, err := buildCodeBuddyCBCMessages(ctx, t.items, branch.indexes, in.opts)
		if err != nil {
			return nil, err
		}
		if alt.userCount == 0 && !codebuddyHasAssistantContent(alt.messages) {
			continue
		}
		for callID, agentID := range alt.subagentCalls {
			if _, ok := calls[callID]; !ok {
				calls[callID] = agentID
			}
		}
		fork := base
		fork.ID = base.ID + "-" + branch.key
		fork.ParentSessionID = base.ID
		fork.RelationshipType = RelFork
		fork.SourceSessionID = ""
		fork.IsTruncated = false
		codebuddyFinishSession(&fork, alt, "")
		fork.LegacyCompanionIncomplete = companionIncomplete ||
			(legacy && alt.legacyImageMissing)
		fork.StartedAt = alt.startedAt
		fork.EndedAt = alt.endedAt
		fork.TerminationStatus = Classify(alt.messages,
			codebuddyStopReason("", alt.messages), false)
		results = append(results, ParseResult{Session: fork, Messages: alt.messages})
	}
	if len(results) == 0 {
		return nil, nil
	}
	codebuddyLinkSubagentCalls(results, ident, calls)
	return results, nil
}

func codebuddyHasAssistantContent(msgs []ParsedMessage) bool {
	for _, msg := range msgs {
		if msg.Role == RoleAssistant && !msg.IsSystem && strings.TrimSpace(msg.Content) != "" {
			return true
		}
	}
	return false
}

// codebuddyCBCRawIDForRuntime returns the archive id of the main transcript
// that records runtimeID, searched under the projects root holding path. The
// IDE catalog names a conversation by the session id its transcript records,
// while the archive names it by the transcript's filename, and the two can
// differ. The transcript named after the id is checked first; otherwise the
// start of each transcript is read. Without such a transcript the id itself is
// returned.
func codebuddyCBCRawIDForRuntime(
	ctx context.Context, path, runtimeID string,
) (string, error) {
	root, ok := codebuddyCBCRootFromPath(path)
	if !ok {
		return runtimeID, nil
	}
	projectDir, _, _ := codebuddyCBCPathParts(path)
	named := filepath.Join(projectDir, runtimeID+".jsonl")
	if named != filepath.Clean(path) && codebuddyCBCSourcePath(root, named) {
		info, err := os.Stat(named)
		if err == nil && info.Mode().IsRegular() &&
			readCodeBuddyCBCHeader(ctx, named).catalogKey(named) == runtimeID {
			return runtimeID, nil
		}
	}
	sources, err := codebuddyCBCSourceSet{roots: []string{root}}.
		sourcesRecordingSessionIDs(ctx, map[string]bool{runtimeID: true})
	if err != nil {
		return "", err
	}
	for _, source := range sources {
		if filepath.Clean(source.DisplayPath) != filepath.Clean(path) {
			return codebuddyCBCIdentityForPath(source.DisplayPath).rawID, nil
		}
	}
	return runtimeID, nil
}

// codebuddyStopReason prefers the producer's recorded terminal state and
// falls back to how the final assistant response ended.
func codebuddyStopReason(recorded string, msgs []ParsedMessage) string {
	if recorded != "" {
		return recorded
	}
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Role == RoleAssistant && !msgs[i].IsSystem {
			return msgs[i].StopReason
		}
	}
	return ""
}

func codebuddyFinishSession(sess *ParsedSession, built codebuddyCBCBuilt, title string) {
	sess.FirstMessage = built.firstMessage
	sess.MessageCount = len(built.messages)
	sess.UserMessageCount = built.userCount
	if title == "" && built.firstMessage != "" {
		title = truncate(built.firstMessage, 80)
	}
	sess.SessionName = title
	accumulateMessageTokenUsage(sess, built.messages)
}

// codebuddyLinkSubagentCalls points each Agent/Task call at the subagent
// session its result names.
func codebuddyLinkSubagentCalls(
	results []ParseResult, ident codebuddyCBCIdentity, calls map[string]string,
) {
	if len(calls) == 0 || ident.subagent {
		return
	}
	for r := range results {
		for m := range results[r].Messages {
			for c := range results[r].Messages[m].ToolCalls {
				call := &results[r].Messages[m].ToolCalls[c]
				agentID, ok := calls[call.ToolUseID]
				if !ok {
					continue
				}
				call.SubagentSessionID = "codebuddy:" + ident.rawID + ":subagent:" + agentID
			}
		}
	}
}

func codebuddyCBCFileInfo(path string) FileInfo {
	info := FileInfo{Path: path}
	if st, err := os.Stat(path); err == nil && st.Mode().IsRegular() {
		info.Size = st.Size()
		info.Mtime = st.ModTime().UnixNano()
	}
	return info
}
