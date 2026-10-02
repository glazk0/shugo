package bot_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/glazk0/shugo/internal/bot"
)

func TestTrackerCloseWaitsForRunningWork(t *testing.T) {
	t.Parallel()

	var tracker bot.Tracker
	started, release := make(chan struct{}), make(chan struct{})
	finished := make(chan struct{})
	go func() {
		tracker.Do(func() {
			close(started)
			<-release
		})
		close(finished)
	}()
	<-started

	closed := make(chan error, 1)
	go func() { closed <- tracker.Close(t.Context()) }()

	select {
	case err := <-closed:
		t.Fatalf("Close() returned %v while work was running", err)
	case <-time.After(20 * time.Millisecond):
	}

	close(release)
	if err := <-closed; err != nil {
		t.Errorf("Close() error = %v", err)
	}
	<-finished
}

func TestTrackerRejectsWorkAfterClose(t *testing.T) {
	t.Parallel()

	var tracker bot.Tracker
	if err := tracker.Close(t.Context()); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	ran := false
	if tracker.Do(func() { ran = true }) || ran {
		t.Error("Do() ran work after Close")
	}
}

func TestTrackerCloseGivesUpAtDeadline(t *testing.T) {
	t.Parallel()

	var tracker bot.Tracker
	started, release := make(chan struct{}), make(chan struct{})
	go tracker.Do(func() {
		close(started)
		<-release
	})
	<-started
	defer close(release)

	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
	defer cancel()
	if err := tracker.Close(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("Close() error = %v, want deadline exceeded", err)
	}
}
