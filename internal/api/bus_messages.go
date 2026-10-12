package api

import (
	"net/http"
	"strconv"

	"github.com/flopwire/flopwire/internal/busproto"
)

func (a *API) busMessages(w http.ResponseWriter, r *http.Request) {
	v := r.URL.Query()
	allowed := map[string]bool{"repo": true, "repo_root": true, "repo_checkout": true, "repo_remote": true, "user": true, "session": true, "thread": true, "since": true, "until": true, "cursor": true, "limit": true, "max_bytes": true}
	for k := range v {
		if !allowed[k] {
			problem(w, 400, "unknown messages filter")
			return
		}
	}
	q := busproto.MessagesQuery{Repo: v.Get("repo"), Roots: v["repo_root"], Mains: v["repo_checkout"], Remotes: v["repo_remote"], User: v.Get("user"), Session: v.Get("session"), Thread: v.Get("thread"), Since: v.Get("since"), Until: v.Get("until"), Before: v.Get("cursor")}
	for key, p := range map[string]*int{"limit": &q.Limit, "max_bytes": &q.MaxBytes} {
		if v.Has(key) {
			n, err := strconv.Atoi(v.Get(key))
			if err != nil || n <= 0 {
				problem(w, 400, key+": a positive number")
				return
			}
			*p = n
		}
	}
	c := a.caller(r)
	out, err := a.bus.Messages(r.Context(), c, q)
	if err != nil {
		a.busFailed(w, r, err)
		return
	}
	ids := []string{}
	for _, m := range out.Threads {
		ids = append(ids, m.ID)
	}
	for _, m := range out.Messages {
		ids = append(ids, m.ID)
	}
	if !a.auditOK(w, r, c.UserID, c.DeviceID, "bus.messages", "bus_message", "", map[string]any{"repo": q.Repo, "thread": q.Thread, "user": q.User, "session": q.Session, "since": q.Since, "until": q.Until, "result_ids": ids}) {
		return
	}
	writeJSON(w, http.StatusOK, out)
}
