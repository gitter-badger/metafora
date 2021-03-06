package m_etcd

import (
	"encoding/json"
	"fmt"
	"path"
	"sync"
	"time"

	"github.com/coreos/go-etcd/etcd"
	"github.com/lytics/metafora"
)

// Don't depend directly on etcd.Client to make testing easier.
type client interface {
	Create(key, value string, ttl uint64) (*etcd.Response, error)
	Delete(key string, recursive bool) (*etcd.Response, error)
	CompareAndDelete(key, prevValue string, index uint64) (*etcd.Response, error)
	CompareAndSwap(key, value string, ttl uint64, prevValue string, index uint64) (*etcd.Response, error)
}

// taskStates hold channels to communicate task state transitions.
type taskStates struct {
	done     chan struct{} // tell taskmgr to mark the task as done
	release  chan struct{} // tell taskmgr to release the task
	finished chan struct{} // taskmgr informing caller done/release are finished
}

// taskManager bumps claims to keep them from expiring and deletes them on
// release.
type taskManager struct {
	ctx    metafora.CoordinatorContext
	client client
	tasks  map[string]taskStates // map of task ID to state chans
	taskL  sync.Mutex            // protect tasks from concurrent access
	path   string                // etcd path to tasks
	node   string                // node ID

	ttl      uint64 // seconds
	interval time.Duration
}

func newManager(ctx metafora.CoordinatorContext, client client, path, nodeID string, ttl uint64) *taskManager {
	if ttl == 0 {
		panic("refresher: TTL must be > 0")
	}

	// refresh more often than strictly necessary to be safe
	interval := time.Duration((ttl>>1)+(ttl>>2)) * time.Second
	if ttl == 1 {
		interval = 750 * time.Millisecond
		metafora.Warnf("Dangerously low TTL: %d; consider raising.", ttl)
	}
	return &taskManager{
		ctx:      ctx,
		client:   client,
		tasks:    make(map[string]taskStates),
		path:     path,
		node:     nodeID,
		ttl:      ttl,
		interval: interval,
	}
}

func (m *taskManager) taskPath(taskID string) string {
	return path.Join(m.path, taskID)
}

func (m *taskManager) ownerKey(taskID string) string {
	return path.Join(m.taskPath(taskID), OwnerMarker)
}

func (m *taskManager) ownerNode(taskID string) (key, value string) {
	p, err := json.Marshal(&ownerValue{Node: m.node})
	if err != nil {
		panic(fmt.Sprintf("coordinator: error marshalling node body: %v", err))
	}
	return m.ownerKey(taskID), string(p)
}

// add starts refreshing a given key+value pair for a task asynchronously.
func (m *taskManager) add(taskID string) bool {
	// Attempt to claim the node
	key, value := m.ownerNode(taskID)
	_, err := m.client.Create(key, value, m.ttl)
	if err != nil {
		etcdErr, ok := err.(*etcd.EtcdError)
		if !ok || etcdErr.ErrorCode != EcodeNodeExist {
			metafora.Errorf("Claim of %s failed with an unexpected error: %v", key, err)
		} else {
			metafora.Debugf("Claim of %s failed, already claimed", key)
		}
		return false
	}

	// Claim successful, start the refresher
	metafora.Debugf("Claim successful: %s", key)
	done := make(chan struct{})
	release := make(chan struct{})
	finished := make(chan struct{})
	m.taskL.Lock()
	m.tasks[taskID] = taskStates{done: done, release: release, finished: finished}
	m.taskL.Unlock()

	metafora.Debugf("Starting claim refresher for task %s", taskID)
	go func() {
		defer func() {
			m.taskL.Lock()
			delete(m.tasks, taskID)
			m.taskL.Unlock()
			close(finished)
		}()

		for {
			select {
			case <-time.After(m.interval):
				// Try to refresh the claim node (0 index means compare by value)
				if _, err := m.client.CompareAndSwap(key, value, m.ttl, value, 0); err != nil {
					metafora.Errorf("Error trying to update task %s ttl: %v", taskID, err)
					m.ctx.Lost(taskID)
					// On errors, don't even try to Delete as we're in a bad state
					return
				}
			case <-done:
				metafora.Debugf("Deleting directory for task %s as it's done.", taskID)
				const recursive = true
				if _, err := m.client.Delete(m.taskPath(taskID), recursive); err != nil {
					metafora.Errorf("Error deleting task %s while stopping: %v", taskID, err)
				}
				return
			case <-release:
				metafora.Debugf("Deleting claim for task %s as it's released.", taskID)
				// Not done, releasing; just delete the claim node
				if _, err := m.client.CompareAndDelete(key, value, 0); err != nil {
					metafora.Warnf("Error releasing task %s while stopping: %v", taskID, err)
				}
				return
			}
		}
	}()
	return true
}

// remove tells a single task's refresher to stop and blocks until the task is
// handled.
func (m *taskManager) remove(taskID string, done bool) {
	m.taskL.Lock()
	states, ok := m.tasks[taskID]
	if !ok {
		m.taskL.Unlock()
		metafora.Debugf("Cannot remove task %s from refresher: not present.", taskID)
		return
	}

	select {
	case <-states.release:
		// already stopping
	case <-states.done:
		// already stopping
	default:
		if done {
			close(states.done)
		} else {
			close(states.release)
		}
	}
	m.taskL.Unlock()

	// Block until task is released/deleted to prevent races on shutdown where
	// the process could exit before all tasks are released.
	<-states.finished
}
