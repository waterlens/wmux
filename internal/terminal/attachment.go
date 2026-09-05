package terminal

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"
)

type Attachment struct {
	// Initial contains durable frames newer than the requested sequence.
	Initial        []OutputFrame
	Truncated      bool
	OldestSequence uint64
	LatestSequence uint64
	Frames         <-chan OutputFrame
	WriterChanges  <-chan bool
	// States carries every runtime status change - state, client count and
	// write lease - so adapters can forward it without polling. It has room
	// for one status: a newer one replaces an unread older one.
	States <-chan SessionStatus
	Closed <-chan AttachmentCloseReason

	session  *runtimeSession
	clientID string
	client   *subscriber
	once     sync.Once
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
	effectiveAfter := after
	truncated := oldest > 1 && after < oldest-1
	limit := s.manager.cfg.ReplayLimit
	if limit > 0 && newest > effectiveAfter && newest-effectiveAfter > uint64(limit) {
		effectiveAfter = newest - uint64(limit)
		truncated = true
	}
	initial := make([]OutputFrame, 0)
	err := s.log.Replay(effectiveAfter, limit, func(sequence uint64, at time.Time, data []byte) error {
		initial = append(initial, OutputFrame{Sequence: sequence, Time: at, Data: append([]byte(nil), data...)})
		return nil
	})
	if err != nil {
		s.mu.Unlock()
		return nil, fmt.Errorf("terminal: replay transcript: %w", err)
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
	writerChanged := false
	if s.writerID == "" {
		s.writerID = clientID
		writerChanged = true
	}
	s.notifyWritersLocked()
	writer := s.writerID
	status := s.notifyLocked()
	s.mu.Unlock()
	s.signal()
	if writerChanged {
		s.manager.callbacks.OnWriterChanged(s.spec.ID, writer)
	}
	s.manager.callbacks.OnSessionState(status)
	return &Attachment{
		Initial:        initial,
		Truncated:      truncated,
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

func (a *Attachment) Write(data []byte) (int, error) {
	return a.WriteContext(context.Background(), data)
}

func (a *Attachment) WriteContext(ctx context.Context, data []byte) (int, error) {
	if a == nil || a.session == nil {
		return 0, ErrAttachmentClosed
	}
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
	if a == nil || a.session == nil {
		return ErrAttachmentClosed
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
	// Record the desired size first; applySize then pushes whatever the newest
	// size is, so a slow older resize can never win over a newer one.
	s.cols, s.rows = cols, rows
	s.mu.Unlock()
	return s.applySize()
}

func (a *Attachment) TakeControl() error {
	if a == nil || a.session == nil {
		return ErrAttachmentClosed
	}
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
		s.manager.callbacks.OnWriterChanged(s.spec.ID, a.clientID)
		s.manager.callbacks.OnSessionState(status)
	}
	return nil
}

func (a *Attachment) IsWriter() bool {
	if a == nil || a.session == nil {
		return false
	}
	s := a.session
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.clients[a.clientID] == a.client && s.writerID == a.clientID
}

func (a *Attachment) Close() error {
	if a == nil || a.session == nil {
		return nil
	}
	a.once.Do(func() { a.session.detach(a.clientID, a.client, AttachmentClientClosed) })
	return nil
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
	writerChanged := false
	if s.writerID == clientID {
		s.writerID = ""
		s.assignWriterLocked()
		s.notifyWritersLocked()
		writerChanged = true
	}
	writer := s.writerID
	status := s.notifyLocked()
	s.mu.Unlock()
	s.signal()
	if writerChanged {
		s.manager.callbacks.OnWriterChanged(s.spec.ID, writer)
	}
	s.manager.callbacks.OnSessionState(status)
}
