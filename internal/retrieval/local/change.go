package local

import (
	"context"
	"os"
	"strings"

	"github.com/flopwire/flopwire/internal/retrieval/format"
)

// Change returns commit observations, including the caller's own session tree.
func (b *Backend) Change(ctx context.Context, q format.ChangeQuery, f format.Filters) (*format.Changes, error) {
	q, err := format.NormalizeChange(q, f)
	if err != nil {
		return nil, err
	}
	f.ExcludeConversation = ""
	f.ExcludeSession = ""
	f.IncludeSuperseded = false
	f.IncludeBranches = false
	lf, err := b.filter(ctx, f)
	if err != nil {
		return nil, err
	}
	secret, err := b.Store.ChangeCursorSecret(ctx)
	if err != nil {
		return nil, err
	}
	scope := format.ChangeScope(q, f)
	scope = format.ChangeResolvedScope(scope, lf)
	var cursor format.ChangeCursor
	cutoff := int64(-1)
	after := int64(0)
	if q.Cursor != "" {
		instance, parseErr := format.ChangeCursorIndex(q.Cursor)
		if parseErr != nil {
			return nil, parseErr
		}
		current, stateErr := b.Store.ChangeState(ctx)
		if stateErr != nil {
			return nil, stateErr
		}
		if instance != current.IndexID {
			return &format.Changes{Kind: "change", Matches: []format.ChangeMatch{}, RestartRequired: true, Coverage: format.ChangeCoverage{VisibleIndex: true}}, nil
		}
		cursor, err = format.VerifyChangeCursor(q.Cursor, secret)
		if err != nil {
			return nil, err
		}
		if cursor.Scope != scope {
			return nil, bad("change cursor belongs to another query or visibility scope")
		}
		cutoff = cursor.Cutoff
		after = cursor.After
	}
	records, state, lag, err := b.Store.ChangeRecords(ctx, q.Commits, lf, cutoff, after)
	if err != nil {
		return nil, err
	}
	out := &format.Changes{Kind: "change", Matches: []format.ChangeMatch{}, Coverage: format.ChangeCoverage{VisibleIndex: true, IndexingLag: lag, SourceTextTruncated: state.SourceTextTruncated}}
	if q.Cursor != "" && (cursor.IndexID != state.IndexID || cursor.Revision != state.Revision) {
		out.RestartRequired = true
		return out, nil
	}
	out.HasMore = len(records) > q.Limit
	if out.HasMore {
		records = records[:q.Limit]
	}
	ids := make([]int64, 0, len(records))
	for _, r := range records {
		ids = append(ids, r.ConversationID)
	}
	infos, err := b.infos(ctx, ids)
	if err != nil {
		return nil, err
	}
	unavailable := map[string]bool{}
	usedBytes := 0
	var lastID int64
	for _, r := range records {
		e := r.Evidence
		c, ok := infos[r.ConversationID]
		if !ok {
			out.RestartRequired = true
			out.Matches = []format.ChangeMatch{}
			out.HasMore = false
			return out, nil
		}
		var inputs []string
		kind := "prefix"
		for _, h := range q.Commits {
			if strings.HasPrefix(h, e.Hash) {
				inputs = append(inputs, h)
				if h == e.Hash {
					kind = "exact"
				}
			}
		}
		m := format.ChangeMatch{Session: c, InputCommits: inputs, ObservedHash: e.Hash, Match: kind, Operation: e.Operation, Outcome: e.Outcome, CommandAddress: id(r.CommandID), ResultAddress: id(r.ResultID), CommandDirectory: e.CommandDirectory, DirectoryKnown: e.DirectoryKnown}
		m.CommandRaw = r.CommandID == r.ResultID
		if e.RevealCommand != nil {
			m.RevealCommandRaw = r.RevealCommandID.Valid && r.RevealResultID.Valid && r.RevealCommandID.Int64 == r.RevealResultID.Int64
			m.RevealCommandAddress = id(r.RevealCommandID.Int64)
		}
		if e.RevealResult != nil {
			m.RevealResultAddress = id(r.RevealResultID.Int64)
		}
		m, size := format.BoundedChangeMatch(m)
		if size > format.ChangePageBudget && usedBytes == 0 {
			return nil, bad("stored change evidence exceeds the page byte limit")
		}
		if usedBytes+size > format.ChangePageBudget {
			out.HasMore = true
			break
		}
		if b.Remote == nil && r.SourcePath != "" {
			if _, err := os.Stat(r.SourcePath); os.IsNotExist(err) {
				unavailable[r.SourcePath] = true
			}
		}
		usedBytes += size
		lastID = r.ID
		out.Matches = append(out.Matches, m)
		out.Coverage.UnknownDirectory = out.Coverage.UnknownDirectory || !e.DirectoryKnown
	}
	check, err := b.Store.ChangeState(ctx)
	if err != nil {
		return nil, err
	}
	if check.IndexID != state.IndexID || check.Revision != state.Revision {
		out.RestartRequired = true
		out.Matches = []format.ChangeMatch{}
		out.HasMore = false
		return out, nil
	}
	if out.HasMore && lastID > 0 {
		out.NextCursor = format.SignChangeCursor(format.ChangeCursor{Scope: scope, IndexID: state.IndexID, Revision: state.Revision, Cutoff: state.Cutoff, After: lastID}, secret)
	}
	out.Coverage.UnavailableSources = len(unavailable)
	out.Coverage.Truncated = out.HasMore
	return out, nil
}
