package service

import (
	"context"
	"sync"
	"testing"
	"time"
)

// TestBackgroundTaskManager_ConcurrentListNoRace exercises List/Get while the
// Submit goroutine mutates task fields. With raw pointers this raced (torn reads
// of Status/Error/DoneAt); with snapshot copies it is clean under -race.
func TestBackgroundTaskManager_ConcurrentListNoRace(t *testing.T) {
	m := NewBackgroundTaskManager()

	release := make(chan struct{})
	var ids []string
	for i := 0; i < 8; i++ {
		ids = append(ids, m.Submit("job", func(ctx context.Context) error {
			<-release // keep the task running while readers read
			return nil
		}))
	}

	var wg sync.WaitGroup
	stop := make(chan struct{})
	for r := 0; r < 4; r++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				for _, t := range m.List() {
					_ = t.Status
					_ = t.DoneAt
				}
				for _, id := range ids {
					if task, ok := m.Get(id); ok {
						_ = task.Status
						_ = task.Error
					}
				}
			}
		}()
	}

	time.Sleep(50 * time.Millisecond)
	close(release) // let tasks finish, mutating Status/DoneAt concurrently with readers
	time.Sleep(50 * time.Millisecond)
	close(stop)
	wg.Wait()
}
