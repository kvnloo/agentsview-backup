import { describe, expect, it } from "vite-plus/test";
import { codebuddyCanContinue } from "./codebuddy.js";

const transcript = {
  agent: "codebuddy",
  id: "codebuddy:conv-1",
  source_session_id: "run-1",
  file_path: "/home/u/.codebuddy/projects/-work/conv-1.jsonl",
};

describe("codebuddyCanContinue", () => {
  it("allows a CLI or IDE 5.x conversation, including an IDE fork", () => {
    expect(codebuddyCanContinue(transcript)).toBe(true);
    expect(codebuddyCanContinue({ ...transcript, relationship_type: "fork" })).toBe(true);
  });

  it.each([
    ["a preserved branch", { source_session_id: "" }],
    ["a side question", { session_kind: "btw" }],
    ["a subagent", { relationship_type: "subagent" }],
    ["a 4.x manifest", { file_path: "/data/history/ws/conv-1/index.json" }],
  ])("refuses %s", (_name, edit) => {
    expect(codebuddyCanContinue({ ...transcript, ...edit })).toBe(false);
  });

  it("leaves other agents to their own rules", () => {
    expect(codebuddyCanContinue({ agent: "claude", id: "claude:x" })).toBe(true);
  });
});

