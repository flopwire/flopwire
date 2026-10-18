package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net/url"
	"os"
	"strconv"
	"strings"

	"github.com/flopwire/flopwire/internal/localindex"
	"github.com/flopwire/flopwire/internal/retrieval/format"
	"github.com/flopwire/flopwire/internal/retrieval/local"
)

// Optional for compatibility with retrieval implementations predating the index.
type changeBackend interface {
	Change(context.Context, format.ChangeQuery, format.Filters) (*format.Changes, error)
}

func lookupChange(ctx context.Context, r *retriever, v url.Values) (*format.Changes, error) {
	q, err := format.ParseChangeQuery(v)
	if err != nil {
		return nil, badArg(err)
	}
	f := format.Filters{Repo: local.ResolveRepo(v.Get("repo"))}
	if r.teamRepo != nil && f.Repo != "" {
		rp, err := r.teamRepo(ctx, f.Repo)
		if err != nil {
			return nil, err
		}
		f.Repo, f.RepoRoots, f.RepoMains, f.RepoRemotes = rp.Repo, rp.Roots, rp.Mains, rp.Remotes
	}
	q, err = format.NormalizeChange(q, f)
	if err != nil {
		return nil, badArg(err)
	}
	b, ok := r.backend.(changeBackend)
	if !ok {
		return nil, errors.New("commit evidence lookup is unavailable on this backend")
	}
	return b.Change(ctx, q, f)
}

func changeCmd(ctx context.Context, name string, args []string) error {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	repo := fs.String("repo", "", "repository scope (required)")
	commits := fs.String("commits", "", "comma-separated full commit IDs (or supply positional IDs)")
	limit := fs.Int("limit", 20, "evidence events per page (maximum 100)")
	cursor := fs.String("cursor", "", "next_cursor from a previous page")
	shared := fs.Bool("server", false, "query shared server")
	device := fs.Bool("local", false, "query this device")
	index := fs.String("index", "", "local index path")
	fs.Bool("json", true, "JSON output")
	if err := fs.Parse(args); err != nil {
		return badArg(err)
	}
	v := url.Values{"repo": {*repo}, "commits": strings.FieldsFunc(*commits, func(r rune) bool { return r == ',' }), "limit": {strconv.Itoa(*limit)}, "cursor": {*cursor}}
	for _, commit := range fs.Args() {
		v.Add("commits", commit)
	}
	q, err := format.ParseChangeQuery(v)
	if err != nil {
		return badArg(err)
	}
	if _, err := format.NormalizeChange(q, format.Filters{Repo: *repo}); err != nil {
		return badArg(err)
	}
	server, err := retrievalServer(*shared, *device, *index)
	if err != nil {
		return err
	}
	r, err := openRetriever(server, *index)
	if err != nil {
		return err
	}
	defer r.close()
	page, err := lookupChange(ctx, r, v)
	if err != nil {
		return err
	}
	return json.NewEncoder(os.Stdout).Encode(page)
}

func changeMCPTool() any {
	return map[string]any{"name": "flopwire_change", "title": "Find sessions behind a change", "annotations": map[string]any{"title": "Find sessions behind a change", "readOnlyHint": true, "destructiveHint": false, "idempotentHint": true, "openWorldHint": false}, "description": "Find AI-agent sessions with captured evidence of a commit, including your own sessions and descendants. Returns command/result addresses for flopwire_read. Read the evidence before attributing a change; a prefix observation remains a prefix match. When command_raw or reveal_command_raw is true, read the corresponding command address with raw=true to inspect captured metadata. For squash merges, supply the PR's original full commit IDs; Flopwire does not query GitHub. Follow next_cursor for more evidence, and restart when restart_required is true.", "inputSchema": map[string]any{"type": "object", "additionalProperties": false, "required": []string{"repo", "commits"}, "properties": map[string]any{"repo": prop("string", "Repository scope"), "commits": map[string]any{"type": "array", "minItems": 1, "maxItems": 100, "items": map[string]any{"type": "string"}, "description": "Full 40 or 64 hexadecimal commit IDs"}, "limit": map[string]any{"type": "integer", "minimum": 1, "maximum": 100, "default": 20, "description": "Evidence events per page (default 20, at most 100)"}, "cursor": prop("string", "next_cursor from the previous page")}}}
}

func changeMCPCall(ctx context.Context, r *retriever, args map[string]any) (string, error) {
	v := url.Values{}
	for k, x := range args {
		switch k {
		case "repo", "cursor":
			s, ok := x.(string)
			if !ok {
				return "", badArg(fmt.Errorf("%s: want a string", k))
			}
			v.Set(k, s)
		case "limit":
			n, ok := x.(float64)
			if !ok || n != float64(int(n)) {
				return "", badArg(errors.New("limit: want an integer"))
			}
			v.Set(k, strconv.Itoa(int(n)))
		case "commits":
			list, ok := x.([]any)
			if !ok {
				return "", badArg(errors.New("commits: want an array of full commit IDs"))
			}
			for _, item := range list {
				s, ok := item.(string)
				if !ok {
					return "", badArg(errors.New("commits: want strings"))
				}
				v.Add(k, s)
			}
		default:
			return "", badArg(fmt.Errorf("flopwire_change: unknown argument %q", k))
		}
	}
	page, err := lookupChange(ctx, r, v)
	if err != nil {
		return "", err
	}
	b, err := json.Marshal(changeMCPOutput(page))
	return string(b), err
}

func (syncOnlyBackend) Change(context.Context, format.ChangeQuery, format.Filters) (*format.Changes, error) {
	return nil, localindex.ErrSyncOnly
}
func (b *lazyIndexBackend) Change(ctx context.Context, q format.ChangeQuery, f format.Filters) (*format.Changes, error) {
	opened, err := b.open()
	if err != nil {
		return nil, err
	}
	cb, ok := opened.(changeBackend)
	if !ok {
		return nil, errors.New("commit evidence lookup unavailable")
	}
	return cb.Change(ctx, q, f)
}

type changeJSON struct {
	*format.Changes
	Hint string `json:"hint"`
}

func changeMCPOutput(p *format.Changes) changeJSON {
	hint := "Read relevant command/result addresses, compare the evidence with the requested claim, and state remaining gaps."
	if len(p.Matches) == 0 {
		hint = "No matching evidence in this query scope; adjust commits or repository scope if more evidence is needed."
	}
	if p.NextCursor != "" {
		hint = "If more evidence is needed, pass next_cursor as cursor with unchanged repo and commits."
	}
	if p.RestartRequired {
		hint = "Restart this lookup without cursor, keeping repo and commits unchanged."
	}
	return changeJSON{p, hint}
}
