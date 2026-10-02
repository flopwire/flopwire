package main

// `flopwire accept`, `flopwire revoke` and `flopwire accepts`: the person's
// decision whose agents may message theirs (plan B7, §4 "Permission").
//
// They are human actions. An agent must not accept on its human's behalf,
// nor read what a sender its human has not accepted wrote, so all three:
//
//   - refuse unless stdin is a terminal (an agent's shell tool has none);
//   - use the login session `flopwire login` saved, never the device
//     credential the agent's hooks and tools use (the server refuses a
//     device or minted token on these routes);
//   - have no MCP tool.
//
// accept states what accepting means and waits for the word "accept"
// typed on the terminal. revoke is one step. Without a server every
// session on the device is the same person's, so nothing is ever held.
//
// Output follows the bus verbs (issue #55): compact JSON by default,
// --text for the readable form, a failure as {"kind":"error",...} on
// stderr with exit status 1.

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"golang.org/x/term"

	"github.com/flopwire/flopwire/internal/busproto"
	"github.com/flopwire/flopwire/internal/client"
)

// acceptStatement is what accepting a sender means (#77). The web console
// shows the same words (web/src/messaging.tsx; a test keeps them equal).
const acceptStatement = "Accepting lets this person's agents send messages to all of your agent sessions. " +
	"Your agents may act on their requests, within each session's own permissions: a session that skips permission prompts may act without asking you. " +
	"Smaller models do not reliably treat these messages as information only. You can revoke at any time."

// consoleRoute is the web console page for held messages and senders.
const consoleRoute = "/#messages"

// Error codes of the accept verbs.
const (
	codeTerminalRequired = "terminal_required"
	codeLoginRequired    = "login_required"
	codeLocalOnly        = "local_only"
	codeNotConfirmed     = "not_confirmed"
	codeServerError      = "server_error"
)

// acceptIO is where the accept verbs read and write, and how they learn
// whether a person is at the terminal; tests replace it.
type acceptIO struct {
	in       io.Reader // the typed confirmation
	out      io.Writer // the answer
	errOut   io.Writer // the statement and prompt, and JSON errors
	terminal func() bool
	load     func() (client.Config, error)
}

func acceptMain(ctx context.Context, verb string, args []string) error {
	return acceptCmd(ctx, verb, args, acceptIO{in: os.Stdin, out: os.Stdout, errOut: os.Stderr,
		terminal: func() bool { return term.IsTerminal(int(os.Stdin.Fd())) }, load: client.Load})
}

// acceptCmd runs accept, revoke or accepts.
func acceptCmd(ctx context.Context, verb string, args []string, io_ acceptIO) error {
	fs := flag.NewFlagSet(verb, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	text := fs.Bool("text", false, "print the readable form instead of JSON")
	st := busStyle{JSON: true}
	err := fs.Parse(args)
	st.JSON = !*text
	if err == nil {
		err = runAccept(ctx, verb, fs.Args(), io_, st)
	} else {
		err = badUsage(err.Error(), acceptUsage(verb))
	}
	if err == nil || !st.JSON {
		return err
	}
	if werr := writeOut(io_.errOut, errorJSON{Kind: "error", Error: asBusErr(err)}); werr != nil {
		return err
	}
	return errReported
}

func acceptUsage(verb string) string {
	switch verb {
	case "accept":
		return "flopwire accept alex@example.com"
	case "revoke":
		return "flopwire revoke alex@example.com"
	}
	return "flopwire accepts --text"
}

// acceptSession is the server and login session the verbs use.
type acceptSession struct {
	api     client.HTTP
	console string
}

func runAccept(ctx context.Context, verb string, pos []string, io_ acceptIO, st busStyle) error {
	switch {
	case verb == "accepts" && len(pos) != 0:
		return badUsage("accepts takes no arguments", acceptUsage(verb))
	case verb != "accepts" && len(pos) != 1:
		return badUsage(verb+" takes one person: an email, its local part, or a name", acceptUsage(verb))
	}
	cfg, cfgErr := io_.load()
	console := ""
	if cfgErr == nil && cfg.Server != "" {
		console = strings.TrimRight(cfg.Server, "/") + consoleRoute
	}
	// First, before anything is read from the server: a person must be at
	// the terminal.
	if !io_.terminal() {
		e := &busErr{Code: codeTerminalRequired, Detail: "flopwire " + verb + " needs a person at a terminal: whose agents may message yours is your decision, and an agent must not make it or read what an unaccepted sender wrote",
			Fix: "run it yourself in a terminal"}
		if console != "" {
			e.Fix += ", or open the web console at " + console
		}
		e.Example = acceptUsage(verb)
		return e
	}
	if local := errors.Is(cfgErr, os.ErrNotExist) || cfg.Server == ""; !local && cfgErr != nil {
		return &busErr{Code: codeAgentError, Detail: "the client configuration could not be read: " + cfgErr.Error(), Fix: "check it, or log in again", Example: "flopwire login --server https://flopwire.example.com"}
	} else if local {
		if verb == "accepts" {
			out := acceptsJSON{Kind: "accepts", Local: true, Accepted: []busproto.Accepted{}, Held: []busproto.HeldGroup{}}
			return st.emit(io_.out, out, func() error {
				_, err := io.WriteString(io_.out, "messaging is local to this device (no server is configured): only your own sessions message each other, so nothing is held and there is no one to accept\n")
				return err
			})
		}
		return &busErr{Code: codeLocalOnly, Detail: "no server is configured: messaging is local to this device, where every session is yours, so there is no other person to " + verb,
			Fix: "join a team server first", Example: "flopwire login --server https://flopwire.example.com"}
	}
	if cfg.FromEnv {
		return &busErr{Code: codeLoginRequired, Detail: "FLOPWIRE_TOKEN is a minted token; accepting senders needs your own login session",
			Fix: "unset FLOPWIRE_TOKEN and run flopwire login, or use the web console at " + console, Example: "flopwire login"}
	}
	token, err := cfg.SessionCredential()
	if err != nil {
		return loginRequired(console)
	}
	s := acceptSession{api: cfg.API(token), console: console}
	switch verb {
	case "accept":
		return s.accept(ctx, pos[0], io_, st)
	case "revoke":
		return s.revoke(ctx, pos[0], io_, st)
	}
	return s.list(ctx, io_, st)
}

func loginRequired(console string) *busErr {
	fix := "run flopwire login in a terminal (it saves a login session for 24 hours)"
	if console != "" {
		fix += ", or use the web console at " + console
	}
	return &busErr{Code: codeLoginRequired, Detail: "accepting and revoking senders needs your login session, not this device's credential, and there is no valid one", Fix: fix, Example: "flopwire login"}
}

// call is one request with the login session; a failure says what to do.
func (s acceptSession) call(ctx context.Context, method, path string, in, out any) error {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	err := s.api.JSON(ctx, method, path, in, out)
	if err == nil {
		return nil
	}
	var ae *client.APIError
	if !errors.As(err, &ae) {
		return &busErr{Code: codeServerError, Detail: "the server could not be reached: " + client.TrustHint(err).Error(), Fix: "check the network and the server, then try again", Example: "flopwire agent status"}
	}
	switch {
	case ae.StatusCode == http.StatusUnauthorized, ae.Code == busproto.CodeLoginRequired:
		return loginRequired(s.console)
	case ae.StatusCode == http.StatusNotImplemented:
		return &busErr{Code: codeServerError, Status: ae.StatusCode, Detail: "this server has no message bus", Fix: "ask your administrator to upgrade the server"}
	case ae.Code == busproto.CodeUnknownRecipient:
		return &busErr{Code: ae.Code, Status: ae.StatusCode, Refused: true, Detail: ae.Detail, Fix: "name a member by email", Example: "flopwire accepts --text"}
	case ae.Code == busproto.CodeAmbiguousRecipient:
		return &busErr{Code: ae.Code, Status: ae.StatusCode, Refused: true, Detail: "the name matches several people", Fix: "use their email", Example: "flopwire accept alex@example.com"}
	case ae.StatusCode == http.StatusBadRequest:
		return &busErr{Code: busproto.CodeBadRequest, Status: ae.StatusCode, Refused: true, Detail: ae.Detail}
	}
	return &busErr{Code: codeServerError, Status: ae.StatusCode, Detail: ae.Error(), Fix: "try again; if it persists, check the server log"}
}

// acceptsJSON is accepts' answer: whom the person accepts, and the held
// messages by sender (previews only).
type acceptsJSON struct {
	Kind     string               `json:"kind"` // "accepts"
	Local    bool                 `json:"local,omitempty"`
	Accepted []busproto.Accepted  `json:"accepted"`
	Held     []busproto.HeldGroup `json:"held"`
	Console  string               `json:"console,omitempty"`
}

func (s acceptSession) list(ctx context.Context, io_ acceptIO, st busStyle) error {
	var acc busproto.AcceptsResponse
	if err := s.call(ctx, "GET", busproto.PathAccepts, nil, &acc); err != nil {
		return err
	}
	var held busproto.HeldResponse
	if err := s.call(ctx, "GET", busproto.PathHeld, nil, &held); err != nil {
		return err
	}
	for i := range held.Senders {
		for j := range held.Senders[i].Messages {
			// The server cuts previews; cut again, so a terminal never
			// receives control sequences from another person's agent.
			m := &held.Senders[i].Messages[j]
			m.Preview = busproto.Preview(m.Preview)
		}
	}
	out := acceptsJSON{Kind: "accepts", Accepted: acc.Accepted, Held: held.Senders, Console: s.console}
	if out.Accepted == nil {
		out.Accepted = []busproto.Accepted{}
	}
	return st.emit(io_.out, out, func() error {
		_, err := io.WriteString(io_.out, acceptsText(out))
		return err
	})
}

func acceptsText(a acceptsJSON) string {
	var b strings.Builder
	if len(a.Held) == 0 {
		b.WriteString("held: nothing; no unaccepted sender has messaged you\n")
	} else {
		n := 0
		for _, g := range a.Held {
			n += g.Count
		}
		fmt.Fprintf(&b, "held: %d %s from %d %s you have not accepted\n", n, plural(n, "message", "messages"), len(a.Held), plural(len(a.Held), "person", "people"))
		for _, g := range a.Held {
			fmt.Fprintf(&b, "  %s  %d held, newest %s\n", busproto.Preview(g.User), g.Count, g.Newest.Local().Format("2006-01-02 15:04"))
			for _, m := range g.Messages {
				fmt.Fprintf(&b, "    %s  %s %s  %s  %q\n", m.ID, m.Agent, repoBranch(m.Repo, m.Branch), m.Intent, m.Preview)
			}
			if g.More > 0 {
				fmt.Fprintf(&b, "    and %d more\n", g.More)
			}
		}
		b.WriteString("Accepting a sender releases their held messages to your sessions: flopwire accept EMAIL\n")
	}
	if len(a.Accepted) == 0 {
		b.WriteString("accepted: nobody\n")
	} else {
		fmt.Fprintf(&b, "accepted: %d\n", len(a.Accepted))
		for _, v := range a.Accepted {
			fmt.Fprintf(&b, "  %s  since %s\n", v.User, v.AcceptedAt.Local().Format("2006-01-02 15:04"))
		}
		b.WriteString("Revoking holds their next messages and any not yet delivered: flopwire revoke EMAIL\n")
	}
	if a.Console != "" {
		fmt.Fprintf(&b, "console: %s\n", a.Console)
	}
	return b.String()
}

// acceptJSON is accept's and revoke's answer.
type acceptJSON struct {
	Kind string `json:"kind"` // "accept" or "revoke"
	busproto.AcceptResponse
}

func (s acceptSession) accept(ctx context.Context, sender string, io_ acceptIO, st busStyle) error {
	// What is waiting from this sender, when anything is: the person sees
	// whom they are accepting before they confirm.
	var held busproto.HeldResponse
	if err := s.call(ctx, "GET", busproto.PathHeld, nil, &held); err != nil {
		return err
	}
	name := strings.ToLower(strings.TrimPrefix(strings.TrimSpace(sender), "@"))
	var b strings.Builder
	b.WriteString(wrap(acceptStatement, 76))
	b.WriteString("\n\n")
	for _, g := range held.Senders {
		local, _, _ := strings.Cut(strings.ToLower(g.User), "@")
		if name == strings.ToLower(g.User) || name == local || name == strings.ToLower(g.UserName) || name == g.UserID {
			fmt.Fprintf(&b, "%s has %d held %s; accepting releases %s to your sessions:\n", busproto.Preview(g.User), g.Count, plural(g.Count, "message", "messages"), plural(g.Count, "it", "them"))
			for _, m := range g.Messages {
				fmt.Fprintf(&b, "  %s %s  %s  %q\n", m.Agent, repoBranch(m.Repo, m.Branch), m.Intent, busproto.Preview(m.Preview))
			}
			if g.More > 0 {
				fmt.Fprintf(&b, "  and %d more\n", g.More)
			}
			b.WriteString("\n")
		}
	}
	fmt.Fprintf(&b, "Accept messages from %s? Type accept to confirm: ", busproto.Preview(sender))
	if _, err := io.WriteString(io_.errOut, b.String()); err != nil {
		return err
	}
	line, err := bufio.NewReader(io_.in).ReadString('\n')
	if strings.TrimSpace(line) != "accept" {
		if err != nil && !errors.Is(err, io.EOF) {
			return err
		}
		return &busErr{Code: codeNotConfirmed, Detail: "not accepted: the confirmation was not the word accept", Example: acceptUsage("accept")}
	}
	var out busproto.AcceptResponse
	if err := s.call(ctx, "POST", busproto.PathAccepts, busproto.AcceptRequest{Sender: sender}, &out); err != nil {
		return err
	}
	r := acceptJSON{Kind: "accept", AcceptResponse: out}
	return st.emit(io_.out, r, func() error {
		_, err := fmt.Fprintf(io_.out, "accepted %s: %d held %s released to your sessions; their agents' messages now reach yours. Revoke: flopwire revoke %s\n",
			out.User, out.Released, plural(out.Released, "message", "messages"), out.User)
		return err
	})
}

func (s acceptSession) revoke(ctx context.Context, sender string, io_ acceptIO, st busStyle) error {
	var out busproto.AcceptResponse
	if err := s.call(ctx, "DELETE", busproto.PathAccepts+"/"+url.PathEscape(strings.TrimPrefix(strings.TrimSpace(sender), "@")), nil, &out); err != nil {
		return err
	}
	r := acceptJSON{Kind: "revoke", AcceptResponse: out}
	return st.emit(io_.out, r, func() error {
		_, err := fmt.Fprintf(io_.out, "revoked %s: %d undelivered %s held again; their next messages are held until you accept them again. A message a session already received stays with it.\n",
			out.User, out.Reheld, plural(out.Reheld, "message", "messages"))
		return err
	})
}

// wrap breaks text into lines of at most width runes at spaces.
func wrap(text string, width int) string {
	var b strings.Builder
	n := 0
	for i, w := range strings.Fields(text) {
		if i > 0 {
			if n+1+len([]rune(w)) > width {
				b.WriteString("\n")
				n = 0
			} else {
				b.WriteString(" ")
				n++
			}
		}
		b.WriteString(w)
		n += len([]rune(w))
	}
	return b.String()
}
