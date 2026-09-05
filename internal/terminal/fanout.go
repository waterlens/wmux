package terminal

// subscriber is one attached client's set of delivery channels; each one maps to
// a message type of the WebSocket protocol.
type subscriber struct {
	id      string
	joined  uint64
	ch      chan OutputFrame
	writers chan bool
	states  chan SessionStatus
	closed  chan AttachmentCloseReason
}

// publish appends output to the transcript and fans it out to every client,
// evicting any client whose buffer is full.
func (s *runtimeSession) publish(data []byte) {
	copyOfData := append([]byte(nil), data...)
	var dropped []string
	s.mu.Lock()
	sequence, err := s.log.Append(copyOfData)
	if err != nil {
		s.lastErr = err.Error()
		status := s.notifyLocked()
		s.mu.Unlock()
		s.manager.cfg.Callbacks.OnSessionState(status)
		return
	}
	frame := OutputFrame{Sequence: sequence, Data: copyOfData}
	writerDropped := false
	for id, client := range s.clients {
		select {
		case client.ch <- frame:
		default:
			delete(s.clients, id)
			s.closeSubscriberLocked(client, AttachmentEvicted)
			dropped = append(dropped, id)
			if s.writerID == id {
				s.writerID = ""
				writerDropped = true
			}
		}
	}
	if writerDropped {
		s.assignWriterLocked()
		s.notifyWritersLocked()
	}
	var status SessionStatus
	if len(dropped) != 0 {
		status = s.notifyLocked()
	}
	s.mu.Unlock()
	for _, id := range dropped {
		s.manager.cfg.Callbacks.OnClientDropped(s.spec.ID, id, "output buffer is full")
	}
	if len(dropped) != 0 {
		s.manager.cfg.Callbacks.OnSessionState(status)
	}
}

func (s *runtimeSession) statusLocked() SessionStatus {
	persistence := s.resolved
	if persistence == "" {
		persistence = s.spec.Persistence
	}
	return SessionStatus{
		ID:          s.spec.ID,
		Generation:  s.spec.Generation,
		State:       s.state,
		Persistence: persistence,
		WriterID:    s.writerID,
		Clients:     len(s.clients),
		LastError:   s.lastErr,
	}
}

// notifyLocked hands the current status to every client, newest status only, and
// returns it for the caller to pass on outside the lock.
func (s *runtimeSession) notifyLocked() SessionStatus {
	status := s.statusLocked()
	for _, client := range s.clients {
		replaceLatest(client.states, status)
	}
	return status
}

func (s *runtimeSession) notifyWritersLocked() {
	for id, client := range s.clients {
		replaceLatest(client.writers, id == s.writerID)
	}
}

// replaceLatest keeps only the newest value in a capacity-1 channel. The caller
// must hold s.mu so that no other producer can refill the slot in between.
func replaceLatest[T any](ch chan T, value T) {
	select {
	case ch <- value:
	default:
		select {
		case <-ch:
		default:
		}
		ch <- value
	}
}

// assignWriterLocked hands a vacant write lease to the longest-attached client.
func (s *runtimeSession) assignWriterLocked() {
	if s.writerID != "" {
		return
	}
	var selected *subscriber
	for _, client := range s.clients {
		if selected == nil || client.joined < selected.joined {
			selected = client
		}
	}
	if selected != nil {
		s.writerID = selected.id
	}
}

func (s *runtimeSession) closeSubscriberLocked(client *subscriber, reason AttachmentCloseReason) {
	select {
	case client.closed <- reason:
	default:
	}
	close(client.closed)
	close(client.writers)
	close(client.states)
	close(client.ch)
}

func (s *runtimeSession) closeClients(reason AttachmentCloseReason) {
	s.mu.Lock()
	count := len(s.clients)
	for _, client := range s.clients {
		s.closeSubscriberLocked(client, reason)
	}
	s.clients = make(map[string]*subscriber)
	s.writerID = ""
	status := s.statusLocked()
	s.mu.Unlock()
	if count != 0 {
		s.manager.cfg.Callbacks.OnSessionState(status)
	}
}
