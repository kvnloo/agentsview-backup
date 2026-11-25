package parser

import (
	"context"
	"os"
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
// returns no results.
func parseCodeBuddyCBCFile(
	ctx context.Context, in codebuddyCBCParseInput,
) ([]ParseResult, error) {
	t, err := readCodeBuddyCBCTranscript(ctx, in.path)
	if err != nil {
		return nil, err
	}
	ident := codebuddyCBCIdentityForPath(in.path)
	if t.sessionID != "" && !ident.subagent {
		ident.runtimeID = t.sessionID
	}
	var legacyOrder []string
	if t.converted && !ident.subagent && len(in.legacyRoots) > 0 {
		companion := loadCodeBuddyLegacyCompanion(ctx, in.legacyRoots, t.cwd, ident.rawID)
		in.opts.legacyUsage = codebuddyLegacyUsageMissingFrom(companion.usage, t.items)
		legacyOrder = companion.order
		if blobs, ok := in.opts.assets.(codebuddyBlobAssets); ok {
			blobs.legacyAssets = companion.assetsDir
			in.opts.assets = blobs
		} else if in.opts.assets == nil && companion.assetsDir != "" {
			in.opts.assets = codebuddyBlobAssets{legacyAssets: companion.assetsDir}
		}
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
		if in.catalog.forkParentRawID != "" && !ident.subagent {
			base.ParentSessionID = "codebuddy:" + in.catalog.forkParentRawID
			base.RelationshipType = RelFork
		}
	}
	if len(main.messages) == 0 && !t.truncated {
		return nil, nil
	}
	mainSess := base
	codebuddyFinishSession(&mainSess, main, title)
	mainSess.StartedAt = earlierTime(t.firstTime, main.startedAt)
	mainSess.EndedAt = laterTime(t.lastTime, main.endedAt)
	mainSess.TerminationStatus = Classify(main.messages,
		codebuddyStopReason(t.stopReason, main.messages), t.truncated)
	results := []ParseResult{{Session: mainSess, Messages: main.messages}}
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
		first := t.items[branch.indexes[0]]
		fork := base
		fork.ID = mainSess.ID + "-" + first.id
		fork.ParentSessionID = mainSess.ID
		fork.RelationshipType = RelFork
		fork.SourceSessionID = ""
		fork.IsTruncated = false
		codebuddyFinishSession(&fork, alt, "")
		fork.StartedAt = alt.startedAt
		fork.EndedAt = alt.endedAt
		fork.TerminationStatus = Classify(alt.messages,
			codebuddyStopReason("", alt.messages), false)
		results = append(results, ParseResult{Session: fork, Messages: alt.messages})
	}
	codebuddyLinkSubagentCalls(results, ident, main.subagentCalls)
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
