package cloudwatchlogs

import (
	"container/list"
	"log"
	"sync"
)

// It is a FIFO obsolete pusher queue with the functionality that force obsolete pusher cleanup if the queue size reaches to the maxSize
type ObsoletePusherQueue struct {
	queue   *list.List
	maxSize int
	sync.Mutex
}

func NewObsoletePusherQueue(size int) *ObsoletePusherQueue {
	if size <= 0 {
		panic("Queue Size should be larger than 0!")
	}
	return &ObsoletePusherQueue{
		queue:   list.New(),
		maxSize: size,
	}
}

func (u *ObsoletePusherQueue) Dequeue() (interface{}, bool) {
	u.Lock()
	defer u.Unlock()

	if u.queue.Len() == 0 {
		return nil, false
	}

	return u.queue.Remove(u.queue.Front()), true
}

func (u *ObsoletePusherQueue) Enqueue(value interface{}) bool {
	u.Lock()
	defer u.Unlock()

	u.queue.PushBack(value)
	if u.queue.Len() == u.maxSize {
		log.Printf("W! Force cleaning up obsolete pusher queue since queue size reaches limit")
		// return true to trigger force cleanup signal
		return true
	}
	return false
}

func (u *ObsoletePusherQueue) DoubleSizeLimit() {
	u.Lock()
	defer u.Unlock()

	u.maxSize = u.maxSize * 2
	log.Printf("I! Doubled obsolete pusher queue size limit to %d", u.maxSize)
}

func (u *ObsoletePusherQueue) GetCurrentSizeLimit() int {
	u.Lock()
	defer u.Unlock()

	return u.maxSize
}
