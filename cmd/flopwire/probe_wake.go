package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/flopwire/flopwire/internal/agent"
	"github.com/flopwire/flopwire/internal/busproto"
	"github.com/flopwire/flopwire/internal/transcript"
)

// nativeDaemon manages only the probe's copied-login scratch CODEX_HOME.
func (r *harnessRun) nativeDaemon(ctx context.Context, action string) error {
	cctx, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()
	cmd := exec.CommandContext(cctx, "codex", "app-server", "daemon", action)
	cmd.Env, cmd.Dir = r.env, r.proj
	// Detached daemon helpers may inherit stdio. A pipe-backed CombinedOutput
	// would then wait for the daemon to exit, even after startup succeeded.
	log, err := os.OpenFile(filepath.Join(r.root, "daemon-control.log"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	defer log.Close()
	cmd.Stdout, cmd.Stderr = log, log
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("scratch daemon %s: %w (see daemon-control.log)", action, err)
	}
	if action == "stop" {
		return stopProbeDaemonUpdater(cctx, r.home)
	}
	return nil
}

// nativeWake never submits a recipient prompt after requesting wake. Only
// the actual native transport may start the recipient's next turn.
func (r *harnessRun) nativeWake(ctx context.Context, res probeResult, marker string) probeResult {
	fail := func(format string, args ...any) probeResult { res.Evidence = fmt.Sprintf(format, args...); return res }
	r.settle(ctx)
	before := r.recv.TurnAt()
	passive, err := r.send(ctx, r.recv, "inform", probeBody(marker+"-PASSIVE"))
	if err != nil {
		return fail("passive send: %v", err)
	}
	select {
	case <-ctx.Done():
		return fail("%v", ctx.Err())
	case <-time.After(r.p.o.idleWait):
	}
	if r.recv.TurnAt() != before || r.state(ctx, passive.ID) != "queued" {
		return fail("ordinary send changed idle recipient or left queued state")
	}
	// Verify transport capability before spending another native model turn.
	deadline := time.Now().Add(20 * time.Second)
	supported := false
	for ctx.Err() == nil && time.Now().Before(deadline) {
		peers, err := agent.Call(ctx, r.p.sock, agent.Request{Op: "peers", Peers: &busproto.PeersQuery{Session: r.sender.ID(), Agent: string(r.name)}})
		if err == nil && peers.Peers != nil {
			for _, p := range peers.Peers.Peers {
				if p.Session == r.recv.ID() && p.Own && p.WakeSupported && !p.Busy {
					supported = true
				}
			}
		}
		if supported {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if !supported {
		return fail("idle recipient did not advertise native wake support; inspect agent.log")
	}
	quote := func(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }
	body := "Bounded native-wake test. Run exactly one shell command: " + quote(r.p.exe) + " send " + quote(r.sender.ID()) + " --socket " + quote(r.p.sock) + " --intent done --reply-to MESSAGE_ID -- " + quote(marker) + ". Replace MESSAGE_ID with this message's id attribute from its Flopwire wrapper. Do not read files or do other work. Then respond with exactly " + marker + "."
	if r.name == transcript.AgentCodex {
		body = "Bounded native-wake test. Call flopwire_send once with to=" + r.sender.ID() + ", intent=done, reply_to set to this message's id attribute from its wrapper, and message=" + marker + ". Use the Flopwire MCP tool. Do not use shell commands, read files, or do other work. Then respond with exactly " + marker + "."
	}
	sent, err := agent.Call(ctx, r.p.sock, agent.Request{Op: "wake-send-v1", Send: &busproto.SendRequest{FromSession: r.sender.ID(), FromAgent: string(r.name), To: r.recv.ID(), Body: body, Intent: "request", Wake: true}})
	if err != nil || sent.Sent == nil {
		return fail("wake send: %v", err)
	}
	res.Message = sent.Sent.ID
	if !sent.Sent.WakeRequested {
		return fail("wake receipt did not retain explicit intent")
	}
	senderBefore := r.sender.TurnAt()
	var replyID string
	for ctx.Err() == nil {
		inbox, err := agent.Call(ctx, r.p.sock, agent.Request{Op: "inbox", Inbox: &busproto.InboxQuery{Session: r.sender.ID(), Agent: string(r.name), Thread: res.Message, Limit: 20}})
		if err == nil && inbox.Inbox != nil {
			for _, m := range inbox.Inbox.Messages {
				if m.Direction == "received" && m.ReplyTo == res.Message && m.From == r.recv.ID() && strings.TrimSpace(m.Body) == marker && m.Intent == busproto.IntentDone && !m.WakeRequested {
					replyID = m.ID
				}
			}
		}
		if replyID != "" {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	if replyID == "" {
		return fail("native wake produced no matching Flopwire reply: %v", ctx.Err())
	}
	if r.recv.TurnAt() <= before {
		return fail("no native recipient turn observed")
	}
	delivery := r.delivered(ctx, r.recv.ID(), res.Message)
	if delivery.err != nil || delivery.w == nil || delivery.n != 1 {
		return fail("recipient wrapper missing from native hook context: %v", delivery.err)
	}
	if err := delivery.w.whole(res.Message); err != nil {
		return fail("%v", err)
	}
	if delivery.w.attrs["from"] != r.sender.ID() || !strings.Contains(delivery.w.body, marker) {
		return fail("wrong native context sender/body")
	}
	// Returning a reply must not automatically wake the sender.
	select {
	case <-ctx.Done():
		return fail("%v", ctx.Err())
	case <-time.After(3 * time.Second):
	}
	if r.sender.TurnAt() != senderBefore {
		return fail("reply unexpectedly woke the sender")
	}
	answer, err := r.sender.Turn(ctx, "Do not run tools. "+quoteTags)
	if err != nil || !strings.Contains(answer, marker) {
		return fail("sender did not see marker reply: %q %v", answer, err)
	}
	replyDelivery := r.delivered(ctx, r.sender.ID(), replyID)
	if replyDelivery.err != nil || replyDelivery.w == nil || replyDelivery.n != 1 {
		return fail("reply missing from sender native hook context: %v", replyDelivery.err)
	}
	if err := replyDelivery.w.whole(replyID); err != nil {
		return fail("reply context: %v", err)
	}
	for ctx.Err() == nil && r.state(ctx, res.Message) != "read" {
		time.Sleep(200 * time.Millisecond)
	}
	if state := r.state(ctx, res.Message); state != "read" {
		return fail("request read receipt: %s", state)
	}
	// Read the reply's delivery state from its actual sending session.
	var replyState string
	for ctx.Err() == nil {
		inbox, err := agent.Call(ctx, r.p.sock, agent.Request{Op: "inbox", Inbox: &busproto.InboxQuery{Session: r.recv.ID(), Agent: string(r.name), SentOnly: true, Thread: res.Message, Limit: 20}})
		if err == nil && inbox.Inbox != nil {
			for _, m := range inbox.Inbox.Messages {
				if m.ID == replyID {
					replyState = string(m.State)
				}
			}
		}
		if replyState == "read" {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	if replyState != "read" {
		return fail("reply read receipt: %s", replyState)
	}
	res.Pass = true
	res.Evidence = fmt.Sprintf("ordinary message %s stayed queued for %s; native wake started recipient without a prompt; request %s and reply %s reached native hook context with read receipts; reply did not wake sender", passive.ID, r.p.o.idleWait, res.Message, replyID)
	return res
}

// Codex's PID backend leaves its updater alive after daemon stop. Terminate
// only the updater recorded in this scratch home, with matching start time
// and executable path; never act on an unverified or reused PID.
func stopProbeDaemonUpdater(ctx context.Context, home string) error {
	raw, err := os.ReadFile(filepath.Join(home, "app-server-daemon", "daemon-updater.pid"))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	var record struct {
		PID   int    `json:"pid"`
		Start string `json:"processStartTime"`
	}
	if err := json.Unmarshal(raw, &record); err != nil {
		return err
	}
	if record.PID <= 1 || record.Start == "" {
		return errors.New("invalid scratch updater identity")
	}
	output, err := exec.CommandContext(ctx, "ps", "-p", strconv.Itoa(record.PID), "-o", "lstart=", "-o", "command=").Output()
	if err != nil {
		var exit *exec.ExitError
		if errors.As(err, &exit) && exit.ExitCode() == 1 {
			return nil
		}
		return err
	}
	fields := strings.Fields(string(output))
	if len(fields) < 6 {
		return errors.New("unreadable scratch updater process")
	}
	command := strings.Join(fields[5:], " ")
	if strings.Join(fields[:5], " ") != strings.Join(strings.Fields(record.Start), " ") || !strings.HasPrefix(command, filepath.Join(home, "packages", "app-server-daemon")+"/") || !strings.HasSuffix(command, " app-server daemon pid-update-loop") {
		return errors.New("scratch updater identity mismatch; left untouched")
	}
	process, err := os.FindProcess(record.PID)
	if err != nil {
		return err
	}
	if err := process.Signal(syscall.SIGTERM); err != nil && !errors.Is(err, os.ErrProcessDone) {
		return err
	}
	return nil
}
