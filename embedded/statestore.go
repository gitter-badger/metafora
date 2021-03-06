package embedded

import (
	"sync"

	"github.com/lytics/metafora/statemachine"
)

type StateChanged struct {
	TaskID string
	State  *statemachine.State
}

// StateStore is an in-memory implementation of statemachine.StateStore
// intended for use in tests.
type StateStore struct {
	mu    *sync.RWMutex
	store map[string]*statemachine.State

	// Stored is intended for tests to block until a Store() is called as an
	// alternative to time.Sleep()s.
	//
	// Will deliver asynchronously and drop states if there's no receivers.
	Stored chan StateChanged
}

func NewStateStore() statemachine.StateStore {
	return &StateStore{
		mu:     &sync.RWMutex{},
		store:  map[string]*statemachine.State{},
		Stored: make(chan StateChanged, 1),
	}
}

func (s *StateStore) Load(tid string) (*statemachine.State, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	state, ok := s.store[tid]
	if !ok {
		return &statemachine.State{Code: statemachine.Runnable}, nil
	}
	return state, nil
}

func (s *StateStore) Store(tid string, state *statemachine.State) error {
	s.mu.Lock()
	s.store[tid] = state
	s.mu.Unlock()
	stored := StateChanged{TaskID: tid, State: state}
	select {
	case s.Stored <- stored:
	default:
	}
	return nil
}
