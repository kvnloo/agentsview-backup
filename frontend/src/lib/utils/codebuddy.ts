import type { Session } from "../api/types.js";

/** CodeBuddy IDE builds that open a stored conversation by URL. */
export const CODEBUDDY_IDE_OPENER_IDS = new Set(["codebuddy-ide", "codebuddy-ide-insiders"]);

/**
 * Reports whether a CodeBuddy session has a CLI runtime session to resume or
 * an IDE conversation to open. Preserved branches, side questions,
 * subagents, and unconverted 4.x manifests do not, and remote sessions
 * cannot be opened on this machine.
 */
export function codebuddyCanContinue(
  session: Pick<
    Session,
    "agent" | "id" | "relationship_type" | "session_kind" | "source_session_id" | "file_path"
  >,
): boolean {
  if (session.agent !== "codebuddy") return true;
  return (
    !!session.source_session_id &&
    !session.session_kind &&
    session.relationship_type !== "subagent" &&
    (session.file_path ?? "").endsWith(".jsonl")
  );
}

