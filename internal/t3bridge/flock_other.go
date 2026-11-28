//go:build !unix

package t3bridge

import "sync"

var fallbackLock sync.Mutex

func withFileLock(_ string, fn func() error) error {
	fallbackLock.Lock()
	defer fallbackLock.Unlock()
	return fn()
}
