package transcript

// SessionSuperseder is implemented by sinks that accept output from
// multi-session sources (a SQLite store holding many conversations, such as
// Devin's sessions.db). Such a source can lose a whole session, or rows of
// one, without any file-level rewrite that Decide would see.
//
// SupersedeSession marks every live row of the session superseded (spec
// §4.2, Deletions). Rows are never dropped. A Message emitted later in the
// same parse with the same native id (or locator) revives its row: the
// upsert writes superseded = false. A parser resets a session by calling
// SupersedeSession and then re-emitting every row it still finds, so rows
// absent from the source stay superseded.
type SessionSuperseder interface {
	SupersedeSession(agent Agent, sessionID string) error
}

// SupersedeSession records the call and marks the session's collected
// messages superseded, so tests can see both the call and its effect.
func (c *Collector) SupersedeSession(agent Agent, sessionID string) error {
	c.SupersededSessions = append(c.SupersededSessions, sessionID)
	for _, m := range c.Messages {
		if m.SessionID == sessionID {
			m.Superseded = true
		}
	}
	return nil
}
