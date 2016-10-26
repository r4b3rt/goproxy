package sutils

import (
	"errors"
	"sync"
	"time"
)

var (
	UPDATE_INTERVAL    uint32 = 10
	ErrUpdaterNotFound        = errors.New("updater not found")
)

type Updater interface {
	Update()
}

var (
	update_lock     sync.RWMutex
	update_set map[Updater]struct{}
)

func init() {
	update_set = make(map[Updater]struct{}, 0)
	go func() {
		for {
			time.Sleep(time.Duration(UPDATE_INTERVAL) * time.Second)
			update_all()
		}
	}()
}

func update_all() {
	update_lock.RLock()
	defer update_lock.RUnlock()

	for u, _ := range update_set {
		u.Update()
	}
}

func UpdateAdd(u Updater) {
	update_lock.Lock()
	defer update_lock.Unlock()

	update_set[u] = struct{}{}
}

func UpdateRemove(u Updater) (err error) {
	update_lock.Lock()
	defer update_lock.Unlock()

	if _, ok := update_set[u]; !ok {
		return ErrUpdaterNotFound
	}
	delete(update_set, u)
	return
}
