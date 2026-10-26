// Package linelock serializes per-logical-device mode changes with legacy
// physical operations. Locks are process-wide and intentionally retained for
// the process lifetime so an ID can never acquire a different lock instance.
package linelock

import "sync"

var lines sync.Map

func For(id string) *sync.RWMutex {
	lock, _ := lines.LoadOrStore(id, new(sync.RWMutex))
	return lock.(*sync.RWMutex)
}
