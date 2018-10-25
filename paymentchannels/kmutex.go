package paymentchannels

import "sync"

// Kmutex is a keyed mutex that locks and unlocks
// a given key
type Kmutex struct {
	m *sync.Map
}

// NewKmutex is the Kmutex constructor
func NewKmutex() Kmutex {
	m := sync.Map{}
	return Kmutex{&m}
}

// Unlock will unlock the mutex for the give key and delete
// the key from the map.
func (s Kmutex) Unlock(key interface{}) {
	l, exist := s.m.Load(key)
	if !exist {
		panic("kmutex: unlock of unlocked mutex")
	}
	l_ := l.(*sync.Mutex)
	s.m.Delete(key)
	l_.Unlock()
}

// Lock will lock the mutex for the given key
func (s Kmutex) Lock(key interface{}) {
	m := sync.Mutex{}
	m_, _ := s.m.LoadOrStore(key, &m)
	mm := m_.(*sync.Mutex)
	mm.Lock()
	if mm != &m {
		mm.Unlock()
		s.Lock(key)
		return
	}
	return
}