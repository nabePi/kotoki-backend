package probe

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/nabePi/kotoki-backend/internal/conversation/protocol"
	"github.com/nabePi/kotoki-backend/internal/kotokiclient"
)

// ConverseDrop is the probe of F16 (spec 13.13.2): one conversation of two utterances whose
// connection is dropped between them (the transport is cut without a close handshake, as a network
// does) and resumed with session.resume. It fails when the resume did not happen, the conversation
// did not finish, or CheckEvents finds an event lost out of order, twice or a turn that never ended.
type ConverseDrop struct {
	Common
	DropFor time.Duration
}

// Run is the Probe function of ConverseDrop.
func (p ConverseDrop) Run(ctx context.Context, _ []string) error {
	dropFor := p.DropFor
	if dropFor <= 0 {
		dropFor = 3 * time.Second
	}
	pk, err := p.speech("")
	if err != nil {
		return err
	}
	c, sess, err := p.open(ctx, 300)
	if err != nil {
		return err
	}

	var (
		mu  sync.Mutex
		got []Event
	)
	sink := kotokiclient.SinkFuncs{Event: func(r kotokiclient.Received[protocol.ServerEvent]) {
		mu.Lock()
		got = append(got, toEvent(r.Value))
		mu.Unlock()
	}}
	steps := []kotokiclient.Step{
		kotokiclient.Speak{Packets: pk, Label: "first"},
		kotokiclient.WaitAIDone{Timeout: 30 * time.Second},
		kotokiclient.Drop{For: dropFor},
		kotokiclient.Think{D: time.Second},
		kotokiclient.Speak{Packets: pk, Label: "second"},
		kotokiclient.WaitAIDone{Timeout: 30 * time.Second},
		kotokiclient.End{Timeout: 30 * time.Second},
	}
	rep, err := kotokiclient.RunConversation(ctx, c, kotokiclient.SessionSpec{
		SessionID: sess.ID, WSURL: p.WSURL, Origin: p.Origin,
		Client: &protocol.ClientInfo{Platform: "chaos", AppVersion: "kotoki-chaos"},
	}, steps, sink)
	if err != nil {
		return fmt.Errorf("probe conversation (session %s): %w", sess.ID, err)
	}
	mu.Lock()
	defer mu.Unlock()
	p.logf("F16 probe: session %s, %d events, resume_ms=%v, status=%s", sess.ID, len(got), rep.ResumeMS, rep.Status)
	if len(rep.ResumeMS) == 0 {
		return fmt.Errorf("probe: session %s: the connection was dropped but the client never resumed", sess.ID)
	}
	if err := CheckEvents(got); err != nil {
		return fmt.Errorf("probe: session %s: %w", sess.ID, err)
	}
	return nil
}

func toEvent(v protocol.ServerEvent) Event {
	e := Event{Type: v.Type, Seq: v.ServerSeq, ID: v.EventID, Replay: v.Replay}
	if v.TurnSeq != nil {
		e.TurnSeq = *v.TurnSeq
	}
	if snap, ok := v.Payload.(protocol.SessionSnapshot); ok {
		e.Baseline = snap.ServerSeqBaseline
	}
	return e
}
