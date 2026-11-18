package session

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"

	"maunium.net/go/mautrix"
	"maunium.net/go/mautrix/crypto"
	"maunium.net/go/mautrix/crypto/olm"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"

	"github.com/gammons/slk/internal/matrix/store"
)

// The sync loop is a loop around Client.FullSyncRequest rather than
// Client.SyncWithContext, because the loop needs things SyncWithContext
// decides on its own: the connection state the UI shows, an exponential
// backoff (SyncWithContext waits a fixed 10s), saving the sync token only
// after the consumer has the batch, and cutting a long poll short when
// room keys for undecrypted events arrive (from a key backup restore) so
// that LateDecrypted is not held up for up to pollTimeout. It still uses
// the client's DefaultSyncer for the handlers e2ee registers (crypto:
// to-device events, device lists, one-time key counts; the state store),
// by passing each response to its ProcessResponse before decrypting.

const (
	// timelineLimit is the most timeline events per room in one sync. A
	// room with more new events comes back limited, with a prev_batch to
	// page back from.
	timelineLimit = 50
	// pollTimeout is how long the server may hold a sync open.
	pollTimeout = 30 * time.Second
	// maxBackoff caps the wait between failed syncs.
	maxBackoff = 30 * time.Second
	// maxPending caps the undecrypted events kept for retry.
	maxPending = 10000
)

// tokenStore is where Run keeps the sync token (next_batch).
type tokenStore interface {
	GetNextBatch(ctx context.Context) (string, error)
	PutNextBatch(ctx context.Context, token string) error
}

// storeTokens keeps the sync token in the account database's sync state.
type storeTokens struct {
	st   *store.Store
	user id.UserID
}

func (t storeTokens) GetNextBatch(ctx context.Context) (string, error) {
	return t.st.LoadNextBatch(ctx, t.user)
}

func (t storeTokens) PutNextBatch(ctx context.Context, token string) error {
	return t.st.SaveNextBatch(ctx, t.user, token)
}

// syncFilter is the inline filter every sync sends: members are lazy-loaded
// (only the senders of events in the response), up to timelineLimit
// timeline events per room with per-thread unread counts, and everything
// else (account data, presence, receipts, typing, to-device) unfiltered.
func syncFilter() *mautrix.Filter {
	return &mautrix.Filter{
		Room: &mautrix.RoomFilter{
			State: &mautrix.FilterPart{LazyLoadMembers: true},
			Timeline: &mautrix.FilterPart{
				Limit:                     timelineLimit,
				LazyLoadMembers:           true,
				UnreadThreadNotifications: true,
			},
			Ephemeral: &mautrix.FilterPart{
				Types: []event.Type{event.EphemeralEventReceipt, event.EphemeralEventTyping},
			},
		},
	}
}

// backoff is the wait after the failures-th consecutive failed sync: 1s,
// 2s, 4s, 8s, 16s, then maxBackoff.
func backoff(failures int) time.Duration {
	if failures < 1 {
		return 0
	}
	if failures > 6 {
		return maxBackoff
	}
	return min(time.Second<<(failures-1), maxBackoff)
}

// LoggedOutError is Run's fatal error when the homeserver no longer
// accepts the access token. SoftLogout is the server's soft_logout flag:
// when true, the device and its keys still exist and logging in again as
// the same device would keep them; when false, the device is gone. Either
// way slk needs a new login (slk --matrix-login), which today creates a new
// device.
type LoggedOutError struct {
	SoftLogout bool
	Err        error
}

func (e *LoggedOutError) Error() string {
	if e.SoftLogout {
		return fmt.Sprintf("session: logged out (soft logout): %v", e.Err)
	}
	return fmt.Sprintf("session: logged out: %v", e.Err)
}

func (e *LoggedOutError) Unwrap() error { return e.Err }

// fatal turns a sync error into Run's fatal error, or nil if the sync
// should be retried.
func fatal(err error) error {
	if !errors.Is(err, mautrix.MUnknownToken) && !errors.Is(err, mautrix.MMissingToken) {
		return nil
	}
	var resp mautrix.RespError
	soft := false
	if errors.As(err, &resp) {
		soft, _ = resp.ExtraData["soft_logout"].(bool)
	}
	return &LoggedOutError{SoftLogout: soft, Err: err}
}

// retryAfter is how long a rate-limited (M_LIMIT_EXCEEDED) response asks
// the client to wait, or 0.
func retryAfter(err error) time.Duration {
	var resp mautrix.RespError
	if !errors.As(err, &resp) || resp.ErrCode != mautrix.MLimitExceeded.ErrCode {
		return 0
	}
	ms, _ := resp.ExtraData["retry_after_ms"].(float64)
	return time.Duration(ms) * time.Millisecond
}

// errWoken is syncOnce's result when it cut the long poll short because
// keys for pending events arrived.
var errWoken = errors.New("woken for late decryption")

func (s *Session) run(ctx context.Context) error {
	since, err := s.tokens.GetNextBatch(ctx)
	if err != nil {
		return fmt.Errorf("session: read sync token: %w", err)
	}
	filter, err := json.Marshal(s.client.Syncer.GetFilterJSON(s.client.UserID))
	if err != nil {
		return fmt.Errorf("session: sync filter: %w", err)
	}
	s.keys.setRunning(true)
	defer s.keys.setRunning(false)

	// Key backup uploads run beside the sync loop and end with it.
	backupCtx, stopBackup := context.WithCancel(ctx)
	backupDone := make(chan struct{})
	go func() {
		defer close(backupDone)
		s.backupLoop(backupCtx)
	}()
	defer func() {
		stopBackup()
		<-backupDone
	}()

	if !s.emit(ctx, Connection{State: Connecting}) {
		return ctx.Err()
	}
	initial, connected, failures := true, false, 0
	for {
		resp, err := s.syncOnce(ctx, since, string(filter), !initial)
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if errors.Is(err, errWoken) {
			if !s.deliverLate(ctx) {
				return ctx.Err()
			}
			continue
		}
		if err == nil {
			// Crypto (to-device room keys, device lists, one-time key
			// counts) and the state store see the response before
			// decryption, so keys that arrive with an event open it.
			err = s.client.Syncer.ProcessResponse(ctx, resp, since)
		}
		if err != nil {
			if f := fatal(err); f != nil {
				s.emit(ctx, Connection{State: Disconnected, Err: f})
				return f
			}
			failures++
			connected = false
			wait := max(backoff(failures), retryAfter(err))
			s.log.Warn().Err(err).Int("failures", failures).Dur("retry_in", wait).Msg("Sync failed")
			if !s.emit(ctx, Connection{State: Disconnected, Err: err}) || !sleep(ctx, wait) ||
				!s.emit(ctx, Connection{State: Connecting}) {
				return ctx.Err()
			}
			continue
		}
		failures = 0
		if !connected {
			if !s.emit(ctx, Connection{State: Connected}) {
				return ctx.Err()
			}
			connected = true
		}

		s.decryptTimelines(ctx, resp)
		if !s.emit(ctx, SyncBatch{Resp: resp, Initial: initial}) {
			return ctx.Err()
		}
		initial = false
		since = resp.NextBatch
		// The consumer has the batch, so save its token even if ctx was
		// cancelled meanwhile.
		if err := s.tokens.PutNextBatch(context.WithoutCancel(ctx), since); err != nil {
			// Not fatal: the next start resumes from an older token and
			// delivers some batches again.
			s.log.Err(err).Msg("Failed to save sync token")
		}
		if !s.deliverLate(ctx) {
			return ctx.Err()
		}
	}
}

// syncOnce makes one /sync request, long-polling only when poll is set. A
// wake-up (keys for pending events arrived) cancels the request and returns
// errWoken.
func (s *Session) syncOnce(ctx context.Context, since, filter string, poll bool) (*mautrix.RespSync, error) {
	reqCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	woken := make(chan bool, 1)
	go func() {
		select {
		case <-s.keys.wake:
			cancel()
			woken <- true
		case <-reqCtx.Done():
			woken <- false
		}
	}()
	timeout := pollTimeout
	if !poll {
		// Until Run has a first response, answer at once instead of
		// holding the request open: the UI waits for that response
		// before it shows the workspace, and a resumed sync with nothing
		// new would otherwise take the whole poll timeout.
		timeout = 0
	}
	presence, _ := s.presence.Load().(event.Presence)
	resp, err := s.client.FullSyncRequest(reqCtx, mautrix.ReqSync{
		Timeout:     int(timeout / time.Millisecond),
		Since:       since,
		FilterID:    filter,
		SetPresence: presence,
	})
	cancel()
	if <-woken && err != nil && ctx.Err() == nil {
		return nil, errWoken
	}
	return resp, err
}

// emit delivers ev, or gives up when ctx is done.
func (s *Session) emit(ctx context.Context, ev Event) bool {
	select {
	case s.events <- ev:
		return true
	case <-ctx.Done():
		return false
	}
}

func sleep(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return true
	case <-ctx.Done():
		return false
	}
}

// decryptTimelines replaces the encrypted timeline events of resp's joined
// and left rooms with their decrypted form where it has the keys, and keeps
// the rest for deliverLate.
func (s *Session) decryptTimelines(ctx context.Context, resp *mautrix.RespSync) {
	for _, room := range resp.Rooms.Join {
		s.decryptEvents(ctx, room.Timeline.Events)
	}
	for _, room := range resp.Rooms.Leave {
		s.decryptEvents(ctx, room.Timeline.Events)
	}
}

func (s *Session) decryptEvents(ctx context.Context, evts []*event.Event) {
	for i, evt := range evts {
		if evt.Type != event.EventEncrypted {
			continue
		}
		dec, err := s.decrypt(ctx, evt)
		if err != nil {
			continue
		}
		evts[i] = dec
	}
}

// decrypt decrypts evt. If that fails for want of a key that may still
// arrive, it keeps evt for deliverLate.
func (s *Session) decrypt(ctx context.Context, evt *event.Event) (*event.Event, error) {
	dec, err := s.crypto.Decrypt(ctx, evt)
	if err == nil {
		dec.Mautrix.EventSource |= event.SourceDecrypted
		return dec, nil
	}
	log := s.log.Debug()
	if keyMayArrive(err) {
		if !s.keys.add(evt) {
			log = s.log.Warn().Bool("dropped", true)
		}
	} else {
		log = s.log.Warn()
	}
	log.Err(err).Stringer("room_id", evt.RoomID).Stringer("event_id", evt.ID).Msg("Failed to decrypt event")
	return nil, err
}

// keyMayArrive reports whether a decryption error is for want of a key
// (or of an earlier ratchet index of it) that a later to-device message or
// a key backup can still supply.
func keyMayArrive(err error) bool {
	return errors.Is(err, crypto.ErrNoSessionFound) ||
		errors.Is(err, olm.ErrUnknownMessageIndex) ||
		errors.Is(err, crypto.ErrGroupSessionWithheld)
}

// deliverLate retries the pending events whose sessions have arrived and
// emits what now decrypts as LateDecrypted, one per room.
func (s *Session) deliverLate(ctx context.Context) bool {
	ready := s.keys.takeArrived()
	if len(ready) == 0 {
		return true
	}
	byRoom := map[id.RoomID][]*event.Event{}
	for _, evt := range ready {
		if dec, err := s.decrypt(ctx, evt); err == nil {
			byRoom[dec.RoomID] = append(byRoom[dec.RoomID], dec)
		}
	}
	rooms := make([]id.RoomID, 0, len(byRoom))
	for room := range byRoom {
		rooms = append(rooms, room)
	}
	sort.Slice(rooms, func(i, j int) bool { return rooms[i] < rooms[j] })
	for _, room := range rooms {
		evts := byRoom[room]
		sort.SliceStable(evts, func(i, j int) bool { return evts[i].Timestamp < evts[j].Timestamp })
		if !s.emit(ctx, LateDecrypted{Room: room, Events: evts}) {
			return false
		}
	}
	return true
}

// pendingKeys holds the events that failed to decrypt for want of a key,
// by Megolm session, and the sessions that have arrived since the sync
// loop last looked. The crypto machine reports an arrival (a room key from
// a to-device message, or a key backup restore) from whichever goroutine
// stored it; the sync loop does the retrying.
type pendingKeys struct {
	mu        sync.Mutex
	running   bool
	bySession map[id.SessionID][]*event.Event
	n         int
	arrived   map[id.SessionID]struct{}
	// wake has room for one signal: keys for a pending event arrived.
	wake chan struct{}
}

func newPendingKeys() *pendingKeys {
	return &pendingKeys{
		bySession: map[id.SessionID][]*event.Event{},
		arrived:   map[id.SessionID]struct{}{},
		wake:      make(chan struct{}, 1),
	}
}

func (p *pendingKeys) setRunning(on bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.running = on
	clear(p.arrived)
}

// add keeps evt for a retry, unless maxPending events are already kept.
func (p *pendingKeys) add(evt *event.Event) bool {
	session := evt.Content.AsEncrypted().SessionID
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.n >= maxPending {
		return false
	}
	p.bySession[session] = append(p.bySession[session], evt)
	p.n++
	return true
}

// received records that session's key has arrived. Arrivals are recorded
// whether or not an event waits for them yet, because the sync loop may be
// between a failed decryption and add; the loop only wakes up early for
// sessions that already have events waiting.
func (p *pendingKeys) received(session id.SessionID) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.running {
		return
	}
	p.arrived[session] = struct{}{}
	if _, ok := p.bySession[session]; ok {
		select {
		case p.wake <- struct{}{}:
		default:
		}
	}
}

// takeArrived removes and returns the pending events whose sessions have
// arrived, and forgets the arrivals.
func (p *pendingKeys) takeArrived() []*event.Event {
	p.mu.Lock()
	defer p.mu.Unlock()
	select {
	case <-p.wake:
	default:
	}
	var out []*event.Event
	for session := range p.arrived {
		out = append(out, p.bySession[session]...)
		p.n -= len(p.bySession[session])
		delete(p.bySession, session)
	}
	clear(p.arrived)
	return out
}
