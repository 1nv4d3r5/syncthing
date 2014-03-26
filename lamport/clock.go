package lamport

import "sync"

var clockVal uint64
var clockMut sync.Mutex

func Clock(c uint64) uint64 {
	clockMut.Lock()
	if c > clockVal {
		clockVal = c + 1
		clockMut.Unlock()
		return c + 1
	} else {
		clockVal++
		clockMut.Unlock()
		return clockVal
	}
}
