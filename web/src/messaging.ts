// What accepting a sender means (issue #77). The CLI prints the same words
// (cmd/flopwire/accept.go acceptStatement; a Go test keeps them equal).
export const ACCEPT_STATEMENT =
  "Accepting lets this person's agents send messages to all of your agent sessions. Your agents may act on their requests, within each session's own permissions: a session that skips permission prompts may act without asking you. Smaller models do not reliably treat these messages as information only. You can revoke at any time.";

export function plural(n: number, one: string, many: string) {
  return n === 1 ? one : many;
}
