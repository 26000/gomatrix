package gomatrix

import (
	"encoding/json"
	"fmt"
	"runtime/debug"
	"time"
)

// Syncer represents an interface that must be satisfied in order to do /sync requests on a client.
type Syncer interface {
	// Process the /sync response. The since parameter is the since= value that was used to produce the response.
	// This is useful for detecting the very first sync (since=""). If an error is return, Syncing will be stopped
	// permanently.
	ProcessResponse(resp *RespSync, since string) error
	// Interface for saving and loading the "next_batch" sync token.
	NextBatchStorer() NextBatchStorer
	// Interface for saving and loading the filter ID for sync.
	FilterStorer() FilterStorer
	// OnFailedSync returns either the time to wait before retrying or an error to stop syncing permanently.
	OnFailedSync(res *RespSync, err error) (time.Duration, error)
}

// DefaultSyncer is the default syncing implementation. You can either write your own syncer, or selectively
// replace parts of this default syncer (e.g. the NextBatch/Filter storers, or the ProcessResponse method).
type DefaultSyncer struct {
	UserID         string
	Rooms          map[string]*Room
	NextBatchStore NextBatchStorer
	FilterStore    FilterStorer
	listeners      map[string][]OnEventListener // event type to listeners array
}

// OnEventListener can be used with DefaultSyncer.OnEventType to be informed of incoming events.
type OnEventListener func(*Event)

// NewDefaultSyncer returns an instantiated DefaultSyncer
func NewDefaultSyncer(userID string, nextBatch NextBatchStorer, filterStore FilterStorer) *DefaultSyncer {
	return &DefaultSyncer{
		UserID:         userID,
		Rooms:          make(map[string]*Room),
		NextBatchStore: nextBatch,
		FilterStore:    filterStore,
		listeners:      make(map[string][]OnEventListener),
	}
}

// ProcessResponse processes the /sync response in a way suitable for bots. "Suitable for bots" means a stream of
// unrepeating events.
func (s *DefaultSyncer) ProcessResponse(res *RespSync, since string) (err error) {
	if !s.shouldProcessResponse(res, since) {
		return
	}

	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("ProcessResponse panicked! userID=%s since=%s panic=%s\n%s", s.UserID, since, r, debug.Stack())
		}
	}()

	for roomID, roomData := range res.Rooms.Join {
		room := s.getOrCreateRoom(roomID)
		for _, event := range roomData.State.Events {
			event.RoomID = roomID
			room.UpdateState(&event)
			s.notifyListeners(&event)
		}
		for _, event := range roomData.Timeline.Events {
			event.RoomID = roomID
			s.notifyListeners(&event)
		}
	}
	for roomID, roomData := range res.Rooms.Invite {
		room := s.getOrCreateRoom(roomID)
		for _, event := range roomData.State.Events {
			event.RoomID = roomID
			room.UpdateState(&event)
			s.notifyListeners(&event)
		}
	}
	return
}

// OnEventType allows callers to be notified when there are new events for the given event type.
// There are no duplicate checks.
func (s *DefaultSyncer) OnEventType(eventType string, callback OnEventListener) {
	_, exists := s.listeners[eventType]
	if !exists {
		s.listeners[eventType] = []OnEventListener{}
	}
	s.listeners[eventType] = append(s.listeners[eventType], callback)
}

// shouldProcessResponse returns true if the response should be processed. May modify the response to remove
// stuff that shouldn't be processed.
func (s *DefaultSyncer) shouldProcessResponse(resp *RespSync, since string) bool {
	if since == "" {
		return false
	}
	// This is a horrible hack because /sync will return the most recent messages for a room
	// as soon as you /join it. We do NOT want to process those events in that particular room
	// because they may have already been processed (if you toggle the bot in/out of the room).
	//
	// Work around this by inspecting each room's timeline and seeing if an m.room.member event for us
	// exists and is "join" and then discard processing that room entirely if so.
	// TODO: We probably want to process messages from after the last join event in the timeline.
	for roomID, roomData := range resp.Rooms.Join {
		for i := len(roomData.Timeline.Events) - 1; i >= 0; i-- {
			e := roomData.Timeline.Events[i]
			if e.Type == "m.room.member" && e.StateKey == s.UserID {
				m := e.Content["membership"]
				mship, ok := m.(string)
				if !ok {
					continue
				}
				if mship == "join" {
					_, ok := resp.Rooms.Join[roomID]
					if !ok {
						continue
					}
					delete(resp.Rooms.Join, roomID)   // don't re-process messages
					delete(resp.Rooms.Invite, roomID) // don't re-process invites
					break
				}
			}
		}
	}
	return true
}

// getOrCreateRoom must only be called by the Sync() goroutine which calls ProcessResponse()
func (s *DefaultSyncer) getOrCreateRoom(roomID string) *Room {
	room := s.Rooms[roomID]
	if room == nil { // create a new Room
		room = NewRoom(roomID)
		s.Rooms[roomID] = room
	}
	return room
}

func (s *DefaultSyncer) notifyListeners(event *Event) {
	listeners, exists := s.listeners[event.Type]
	if !exists {
		return
	}
	for _, fn := range listeners {
		fn(event)
	}
}

// NextBatchStorer returns the provided NextBatchStorer
func (s *DefaultSyncer) NextBatchStorer() NextBatchStorer {
	return s.NextBatchStore
}

// FilterStorer returns the provided FilterStorer
func (s *DefaultSyncer) FilterStorer() FilterStorer {
	return s.FilterStore
}

// OnFailedSync always returns a 10 second wait period between failed /syncs.
func (s *DefaultSyncer) OnFailedSync(res *RespSync, err error) (time.Duration, error) {
	return 10 * time.Second, nil
}

// NextBatchStorer controls loading/saving of next_batch tokens for users
type NextBatchStorer interface {
	// SaveNextBatch saves a next_batch token for a given user. Best effort.
	SaveNextBatch(userID, nextBatch string)
	// LoadNextBatch loads a next_batch token for a given user. Return an empty string if no token exists.
	LoadNextBatch(userID string) string
}

// InMemoryNextBatchStore stores next batch tokens in memory.
type InMemoryNextBatchStore struct {
	UserToNextBatch map[string]string
}

// SaveNextBatch saves the mapping in-memory.
func (s *InMemoryNextBatchStore) SaveNextBatch(userID, nextBatch string) {
	s.UserToNextBatch[userID] = nextBatch
}

// LoadNextBatch loads an existing mapping. Returns an empty string if not found
func (s *InMemoryNextBatchStore) LoadNextBatch(userID string) string {
	return s.UserToNextBatch[userID]
}

// FilterStorer controls loading/saving of filter IDs for users
type FilterStorer interface {
	// SaveFilter saves a filter ID for a given user. Best effort.
	SaveFilter(userID, filterID string)
	// LoadFilter loads a filter ID for a given user. Return an empty string if no token exists.
	LoadFilter(userID string) string
	// GetFilterJSON for the given user ID.
	GetFilterJSON(userID string) json.RawMessage
}

// InMemoryFilterStore stores filter IDs in memory. It always returns the filter JSON given in the struct.
type InMemoryFilterStore struct {
	Filter       json.RawMessage
	UserToFilter map[string]string
}

// SaveFilter saves the user->filter ID mapping in memory
func (s *InMemoryFilterStore) SaveFilter(userID, filterID string) {
	s.UserToFilter[userID] = filterID
}

// LoadFilter loads a previously saved user->filter ID mapping from memory. Returns the empty string if not found.
func (s *InMemoryFilterStore) LoadFilter(userID string) string {
	return s.UserToFilter[userID]
}

// GetFilterJSON returns InMemoryFilterStore.Filter
func (s *InMemoryFilterStore) GetFilterJSON(userID string) json.RawMessage {
	return s.Filter
}
