package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"slices"
	"time"

	"github.com/coder/websocket"
	"github.com/google/uuid"

	"github.com/nabePi/kotoki-backend/internal/conversation/protocol"
	"github.com/nabePi/kotoki-backend/internal/kotokiclient"
)

var (
	allowedDurations = []int{300, 600, 900, 1800, 2700, 3600}
	startModes       = []string{"USER_FIRST", "AI_FIRST"}
)

// pollInterval is how often `evaluation get --wait` asks again (§14.11.2: every 2 s).
var pollInterval = 2 * time.Second

// sessionFlags are the flags that describe a new session (`session create`, `converse`).
type sessionFlags struct {
	character string
	scenario  string
	duration  int
	startMode string
}

func (sf *sessionFlags) bind(fs *flag.FlagSet) {
	fs.StringVar(&sf.character, "character", "", "character id or code (default: the first active one)")
	fs.StringVar(&sf.scenario, "scenario", "", "scenario id or code (default: the first active one)")
	fs.IntVar(&sf.duration, "duration", 300, "session length in seconds (300/600/900/1800/2700/3600)")
	fs.StringVar(&sf.startMode, "start-mode", "USER_FIRST", "USER_FIRST or AI_FIRST")
}

func (sf sessionFlags) validate() error {
	if !slices.Contains(allowedDurations, sf.duration) {
		return usagef("--duration must be one of %v, got %d", allowedDurations, sf.duration)
	}
	if !slices.Contains(startModes, sf.startMode) {
		return usagef("--start-mode must be USER_FIRST or AI_FIRST, got %q", sf.startMode)
	}
	return nil
}

func parseSessionID(s string) (uuid.UUID, error) {
	if s == "" {
		return uuid.Nil, usagef("--session is required")
	}
	id, err := uuid.Parse(s)
	if err != nil {
		return uuid.Nil, usagef("--session %q is not a UUID", s)
	}
	return id, nil
}

// createSession reserves a session from the active catalog: the first character and scenario
// unless named by id or code; the topic is the scenario's.
func (a *app) createSession(ctx context.Context, cc *cliClient, sf sessionFlags) (kotokiclient.Session, error) {
	var cat kotokiclient.Catalog
	if err := cc.run(ctx, func() (e error) { cat, e = cc.Catalog(ctx); return e }); err != nil {
		return kotokiclient.Session{}, err
	}
	var scenario *kotokiclient.Scenario
	for i := range cat.Scenarios {
		s := &cat.Scenarios[i]
		if sf.scenario == "" || sf.scenario == s.ID.String() || sf.scenario == s.Code {
			scenario = s
			break
		}
	}
	if scenario == nil {
		return kotokiclient.Session{}, fmt.Errorf("no active scenario matches %q (see `kotoki-cli catalog`)", sf.scenario)
	}
	var character *kotokiclient.Character
	for i := range cat.Characters {
		c := &cat.Characters[i]
		if sf.character == "" || sf.character == c.ID.String() || sf.character == c.Code {
			character = c
			break
		}
	}
	if character == nil {
		return kotokiclient.Session{}, fmt.Errorf("no active character matches %q (see `kotoki-cli catalog`)", sf.character)
	}
	req := kotokiclient.CreateSessionRequest{
		TopicID: scenario.TopicID, ScenarioID: scenario.ID, CharacterID: character.ID,
		DurationSeconds: sf.duration, ConversationStartMode: sf.startMode,
	}
	var s kotokiclient.Session
	err := cc.run(ctx, func() (e error) { s, e = cc.CreateSession(ctx, req); return e })
	return s, err
}

func (a *app) cmdSession(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return usagef("session: expected create, get, cancel, end or transcript")
	}
	sub, args := args[0], args[1:]
	switch sub {
	case "create":
		return a.sessionCreate(ctx, args)
	case "get":
		return a.sessionGet(ctx, args)
	case "cancel":
		return a.sessionCancel(ctx, args)
	case "end":
		return a.sessionEnd(ctx, args)
	case "transcript":
		return a.sessionTranscript(ctx, args)
	default:
		return usagef("session: unknown subcommand %q", sub)
	}
}

func (a *app) sessionCreate(ctx context.Context, args []string) error {
	fs := a.newFlagSet("session create")
	var sf sessionFlags
	sf.bind(fs)
	if err := a.parse(fs, args); err != nil {
		return err
	}
	if err := sf.validate(); err != nil {
		return err
	}
	cc, err := a.open()
	if err != nil {
		return err
	}
	s, err := a.createSession(ctx, cc, sf)
	if err != nil {
		return err
	}
	if a.g.JSON {
		return a.printJSON(s)
	}
	a.printf("%s\n", s.ID)
	return nil
}

func (a *app) sessionGet(ctx context.Context, args []string) error {
	fs := a.newFlagSet("session get")
	sid := fs.String("session", "", "session id")
	if err := a.parse(fs, args); err != nil {
		return err
	}
	id, err := parseSessionID(*sid)
	if err != nil {
		return err
	}
	cc, err := a.open()
	if err != nil {
		return err
	}
	var s kotokiclient.Session
	if err := cc.run(ctx, func() (e error) { s, e = cc.GetSession(ctx, id); return e }); err != nil {
		return err
	}
	if a.g.JSON {
		return a.printJSON(s)
	}
	a.printf("session %s %s  start_mode=%s duration=%ds charged=%ds\n", s.ID, s.Status, s.ConversationStartMode, s.DurationSeconds, s.ChargedSeconds)
	if s.EndReason != nil {
		a.printf("end_reason %s\n", *s.EndReason)
	}
	return nil
}

func (a *app) sessionCancel(ctx context.Context, args []string) error {
	fs := a.newFlagSet("session cancel")
	sid := fs.String("session", "", "session id")
	if err := a.parse(fs, args); err != nil {
		return err
	}
	id, err := parseSessionID(*sid)
	if err != nil {
		return err
	}
	cc, err := a.open()
	if err != nil {
		return err
	}
	if err := cc.run(ctx, func() error { return cc.CancelSession(ctx, id) }); err != nil {
		return err
	}
	if a.g.JSON {
		return a.printJSON(map[string]string{"session_id": id.String(), "status": "CANCELLED"})
	}
	a.printf("cancelled %s\n", id)
	return nil
}

func (a *app) sessionTranscript(ctx context.Context, args []string) error {
	fs := a.newFlagSet("session transcript")
	sid := fs.String("session", "", "session id")
	if err := a.parse(fs, args); err != nil {
		return err
	}
	id, err := parseSessionID(*sid)
	if err != nil {
		return err
	}
	cc, err := a.open()
	if err != nil {
		return err
	}
	var after *int
	for {
		var (
			page []kotokiclient.Turn
			next string
		)
		if err := cc.run(ctx, func() (e error) { page, next, e = cc.Turns(ctx, id, after, 100); return e }); err != nil {
			return err
		}
		for _, t := range page {
			if a.g.JSON {
				if err := a.printJSON(t); err != nil {
					return err
				}
				continue
			}
			a.printf("#%d [%s]\n", t.SequenceNumber, t.Status)
			if t.UserText != nil {
				a.printf("  user: %s\n", *t.UserText)
			}
			if t.AssistantText != nil {
				a.printf("  ai:   %s\n", *t.AssistantText)
			}
		}
		if next == "" || len(page) == 0 {
			return nil
		}
		last := page[len(page)-1].SequenceNumber
		after = &last
	}
}

func (a *app) cmdTicket(ctx context.Context, args []string) error {
	fs := a.newFlagSet("ticket")
	sid := fs.String("session", "", "session id")
	if err := a.parse(fs, args); err != nil {
		return err
	}
	id, err := parseSessionID(*sid)
	if err != nil {
		return err
	}
	cc, err := a.open()
	if err != nil {
		return err
	}
	var ticket string
	if err := cc.run(ctx, func() (e error) { ticket, e = cc.RealtimeTicket(ctx, id); return e }); err != nil {
		return err
	}
	if a.g.JSON {
		return a.printJSON(map[string]string{"ticket": ticket})
	}
	a.printf("%s\n", ticket)
	return nil
}

func (a *app) cmdEvaluation(ctx context.Context, args []string) error {
	if len(args) == 0 || args[0] != "get" {
		return usagef("evaluation: expected `get`")
	}
	fs := a.newFlagSet("evaluation get")
	sid := fs.String("session", "", "session id")
	wait := fs.Duration("wait", 0, "poll every 2s until COMPLETED or FAILED, at most this long")
	if err := a.parse(fs, args[1:]); err != nil {
		return err
	}
	id, err := parseSessionID(*sid)
	if err != nil {
		return err
	}
	cc, err := a.open()
	if err != nil {
		return err
	}
	deadline := time.Now().Add(*wait)
	for {
		var ev kotokiclient.Evaluation
		err := cc.run(ctx, func() (e error) { ev, e = cc.GetEvaluation(ctx, id); return e })
		var ae *kotokiclient.APIError
		notYet := errors.As(err, &ae) && ae.Status == http.StatusNotFound && *wait > 0
		switch {
		case err != nil && !notYet:
			return err
		case err == nil && (ev.Status == "COMPLETED" || ev.Status == "FAILED" || *wait == 0):
			return a.printEvaluation(ev)
		}
		if !time.Now().Add(pollInterval).Before(deadline) {
			if err != nil {
				return fmt.Errorf("evaluation not available after %s: %w", *wait, err)
			}
			return fmt.Errorf("evaluation still %s after %s", ev.Status, *wait)
		}
		select {
		case <-time.After(pollInterval):
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

func (a *app) printEvaluation(ev kotokiclient.Evaluation) error {
	if a.g.JSON {
		var buf bytes.Buffer
		if err := json.Compact(&buf, ev.Raw); err != nil {
			return err
		}
		a.printf("%s\n", buf.Bytes())
		return nil
	}
	var buf bytes.Buffer
	if err := json.Indent(&buf, ev.Raw, "", "  "); err != nil {
		return err
	}
	a.printf("%s\n", buf.Bytes())
	return nil
}

// sessionEnd ends an ACTIVE session. REST has no such endpoint (the end is a WebSocket command),
// so this opens a short connection: session.resume, then session.end{USER_ENDED}, then waits for
// the terminal event.
func (a *app) sessionEnd(ctx context.Context, args []string) error {
	fs := a.newFlagSet("session end")
	sid := fs.String("session", "", "session id")
	wait := fs.Duration("wait", 15*time.Second, "how long to wait for the session to complete")
	if err := a.parse(fs, args); err != nil {
		return err
	}
	id, err := parseSessionID(*sid)
	if err != nil {
		return err
	}
	cc, err := a.open()
	if err != nil {
		return err
	}
	var ticket string
	if err := cc.run(ctx, func() (e error) { ticket, e = cc.RealtimeTicket(ctx, id); return e }); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, *wait)
	defer cancel()
	rc, err := kotokiclient.Dial(ctx, a.g.WSURL, ticket, a.g.Origin)
	if err != nil {
		return err
	}
	defer func() { _ = rc.Close(websocket.StatusNormalClosure, "session end") }()
	go func() { // the reader blocks on a full buffer: nobody here wants the audio
		for range rc.Audio() {
		}
	}()

	audio := protocol.AudioParams{Codec: "opus", SampleRate: 48000, Channels: 1, FrameMS: 20}
	send := func(typ string, payload any) error {
		return rc.Send(ctx, protocol.ClientEvent{Type: typ, SessionID: id, Payload: payload})
	}
	stage := "session.ready"
	for {
		select {
		case e, ok := <-rc.Events():
			if !ok {
				return fmt.Errorf("connection closed while waiting for %s: %w", stage, rc.Err())
			}
			switch p := e.Value.Payload.(type) {
			case protocol.SessionReady:
				if p.ExpectedCommand != "session.resume" {
					return fmt.Errorf("session is not active (server expects %s); use `session cancel` for a RESERVED session", p.ExpectedCommand)
				}
				if err := send("session.resume", protocol.SessionResume{Audio: audio}); err != nil {
					return err
				}
				stage = "session.snapshot"
			case protocol.SessionSnapshot:
				if stage != "session.snapshot" {
					continue
				}
				if err := send("session.end", protocol.SessionEnd{Reason: "USER_ENDED"}); err != nil {
					return err
				}
				stage = "session.completed"
			case protocol.SessionCompleted:
				return a.printEnded(id, "COMPLETED", p.EndReason, p.ChargedSeconds)
			case protocol.SessionFailed:
				return a.printEnded(id, "FAILED", p.EndReason, p.ChargedSeconds)
			case protocol.ErrorPayload:
				if p.Fatal {
					return fmt.Errorf("server error %s: %s", p.Error.Code, p.Error.Message)
				}
			}
		case <-ctx.Done():
			return fmt.Errorf("timed out waiting for %s", stage)
		}
	}
}

func (a *app) printEnded(id uuid.UUID, status, reason string, charged int) error {
	if a.g.JSON {
		return a.printJSON(map[string]any{"session_id": id.String(), "status": status, "end_reason": reason, "charged_seconds": charged})
	}
	a.printf("session %s %s (%s) charged_seconds=%d\n", id, status, reason, charged)
	return nil
}
