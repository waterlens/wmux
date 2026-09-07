package terminal

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// Attachment is one browser's view of a session; only runtimeSession.attach
// builds one, so its session and client fields are always set.
type Attachment struct {
	// Prelude is the multiplexer's attach-time terminal setup for a client
	// that skipped the transcript; it is delivered before Initial and is not
	// part of the sequence space.
	Prelude []byte
	// Initial contains durable frames newer than the requested sequence.
	Initial        []OutputFrame
	Truncated      bool
	OldestSequence uint64
	LatestSequence uint64
	Frames         <-chan OutputFrame
	WriterChanges  <-chan bool
	// States carries runtime status changes and keeps only the newest one.
	States <-chan SessionStatus
	Closed <-chan AttachmentCloseReason

	session  *runtimeSession
	clientID string
	client   *subscriber
}

func (a *Attachment) WriteContext(ctx context.Context, data []byte) (int, error) {
	s := a.session
	s.mu.Lock()
	client := s.clients[a.clientID]
	if client != a.client {
		s.mu.Unlock()
		return 0, ErrAttachmentClosed
	}
	if s.writerID != a.clientID {
		s.mu.Unlock()
		return 0, ErrNotWriter
	}
	b := s.backend
	s.mu.Unlock()
	if b == nil {
		return 0, ErrUnavailable
	}
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	return b.WriteContext(ctx, data)
}

func (a *Attachment) Resize(cols, rows uint16) error {
	if cols == 0 || rows == 0 {
		return errors.New("terminal: terminal dimensions must be positive")
	}
	s := a.session
	s.mu.Lock()
	if s.clients[a.clientID] != a.client {
		s.mu.Unlock()
		return ErrAttachmentClosed
	}
	if s.writerID != a.clientID {
		s.mu.Unlock()
		return ErrNotWriter
	}
	if s.closed || s.terminating {
		s.mu.Unlock()
		return ErrUnavailable
	}
	// Record the size first; applySize then pushes the newest one.
	s.cols, s.rows = cols, rows
	s.mu.Unlock()
	return s.applySize()
}

func (a *Attachment) TakeControl() error {
	s := a.session
	s.mu.Lock()
	if s.clients[a.clientID] != a.client {
		s.mu.Unlock()
		return ErrAttachmentClosed
	}
	changed := s.writerID != a.clientID
	s.writerID = a.clientID
	if changed {
		s.notifyWritersLocked()
	}
	status := s.notifyLocked()
	s.mu.Unlock()
	if changed {
		s.manager.cfg.Callbacks.OnSessionState(status)
	}
	return nil
}

func (a *Attachment) IsWriter() bool {
	s := a.session
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.clients[a.clientID] == a.client && s.writerID == a.clientID
}

// Close is idempotent: detach ignores a client that is no longer registered.
func (a *Attachment) Close() error {
	a.session.detach(a.clientID, a.client, AttachmentClientClosed)
	return nil
}

func (s *runtimeSession) attach(clientID string, after uint64) (*Attachment, error) {
	if clientID == "" {
		return nil, errors.New("terminal: client ID is required")
	}
	s.mu.Lock()
	if s.closed || s.terminating {
		s.mu.Unlock()
		return nil, ErrUnavailable
	}
	if _, exists := s.clients[clientID]; exists {
		s.mu.Unlock()
		return nil, ErrClientExists
	}

	oldest, newest := s.log.Bounds()
	plan, err := s.replayLocked(after, oldest, newest)
	if err != nil {
		s.mu.Unlock()
		return nil, err
	}

	s.joinCounter++
	client := &subscriber{
		id:      clientID,
		joined:  s.joinCounter,
		ch:      make(chan OutputFrame, s.manager.cfg.ClientBuffer),
		writers: make(chan bool, 1),
		states:  make(chan SessionStatus, 1),
		closed:  make(chan AttachmentCloseReason, 1),
	}
	s.clients[clientID] = client
	if s.writerID == "" {
		s.writerID = clientID
	}
	s.notifyWritersLocked()
	status := s.notifyLocked()
	s.mu.Unlock()
	s.signal()
	if plan.redraw {
		// The client is subscribed, so the repaint reaches it as live output.
		go s.redraw()
	}
	s.manager.cfg.Callbacks.OnSessionState(status)
	return &Attachment{
		Prelude:        plan.prelude,
		Initial:        plan.initial,
		Truncated:      plan.truncated,
		OldestSequence: oldest,
		LatestSequence: newest,
		Frames:         client.ch,
		WriterChanges:  client.writers,
		States:         client.states,
		Closed:         client.closed,
		session:        s,
		clientID:       clientID,
		client:         client,
	}, nil
}

var errReplayGapTooLarge = errors.New("terminal: replay gap exceeds the byte cap")

// replayPlan is what a joining client receives before live output.
type replayPlan struct {
	prelude   []byte
	initial   []OutputFrame
	truncated bool
	// redraw asks the backend to repaint once the client is subscribed.
	redraw bool
}

// replayLocked decides what a joining client receives from the transcript and
// whether the screen has to be repainted afterwards. The caller holds s.mu.
//
// tmux and screen hold the whole screen themselves, and their transcript is
// mostly earlier repaints of it, so a client that starts from scratch or fell
// further behind than ReplayGapBytes skips the transcript and gets a repaint
// instead. A fresh client also gets the prelude: the repaint does not repeat
// the terminal setup (alternate screen, bracketed paste, keypad modes) that the
// multiplexer sent when wmux attached. A direct PTY cannot repaint, so its
// clients replay what the transcript holds; when that replay was cut short, a
// window-size nudge lets full-screen programs repaint over the partial state.
func (s *runtimeSession) replayLocked(after, oldest, newest uint64) (replayPlan, error) {
	limit := s.manager.cfg.ReplayLimit
	if s.resolved == PersistenceTmux || s.resolved == PersistenceScreen {
		switch {
		case after == newest:
			return replayPlan{}, nil
		case after == 0:
			return replayPlan{prelude: append([]byte(nil), s.prelude...), redraw: true}, nil
		case after > newest, oldest > 1 && after < oldest-1, newest-after > uint64(limit):
			return replayPlan{redraw: true}, nil
		}
		initial, err := s.collectReplay(after, limit, s.manager.cfg.ReplayGapBytes)
		if errors.Is(err, errReplayGapTooLarge) {
			return replayPlan{redraw: true}, nil
		}
		return replayPlan{initial: initial}, err
	}
	effectiveAfter := after
	truncated := oldest > 1 && after < oldest-1
	if newest > effectiveAfter && newest-effectiveAfter > uint64(limit) {
		effectiveAfter = newest - uint64(limit)
		truncated = true
	}
	initial, err := s.collectReplay(effectiveAfter, limit, 0)
	return replayPlan{initial: initial, truncated: truncated, redraw: truncated}, err
}

// collectReplay copies the frames after `after` out of the transcript. A
// positive maxBytes gives up with errReplayGapTooLarge once they exceed it.
func (s *runtimeSession) collectReplay(after uint64, limit, maxBytes int) ([]OutputFrame, error) {
	var frames []OutputFrame
	total := 0
	err := s.log.Replay(after, limit, func(sequence uint64, _ time.Time, data []byte) error {
		total += len(data)
		if maxBytes > 0 && total > maxBytes {
			return errReplayGapTooLarge
		}
		frames = append(frames, OutputFrame{Sequence: sequence, Data: append([]byte(nil), data...)})
		return nil
	})
	if errors.Is(err, errReplayGapTooLarge) {
		return nil, err
	}
	if err != nil {
		return nil, fmt.Errorf("terminal: replay transcript: %w", err)
	}
	return frames, nil
}

func (s *runtimeSession) detach(clientID string, expected *subscriber, reason AttachmentCloseReason) {
	s.mu.Lock()
	client := s.clients[clientID]
	if client != expected {
		s.mu.Unlock()
		return
	}
	delete(s.clients, clientID)
	s.closeSubscriberLocked(client, reason)
	if s.writerID == clientID {
		s.writerID = ""
		s.assignWriterLocked()
		s.notifyWritersLocked()
	}
	status := s.notifyLocked()
	s.mu.Unlock()
	s.signal()
	s.manager.cfg.Callbacks.OnSessionState(status)
}
