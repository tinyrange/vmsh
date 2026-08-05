package desktopapp

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
)

func TestBackgroundImageStagePreparesOnceAndWaitsForCompletion(t *testing.T) {
	var stage backgroundImageStage
	release := make(chan struct{})
	var calls atomic.Int32
	if !stage.start("next", func() error {
		calls.Add(1)
		<-release
		return nil
	}) {
		t.Fatal("first stage did not start")
	}
	if stage.start("other", func() error { return nil }) {
		t.Fatal("second concurrent stage started")
	}

	result := make(chan string, 1)
	go func() {
		name, started, err := stage.take(context.Background())
		if !started || err != nil {
			result <- ""
			return
		}
		result <- name
	}()
	select {
	case <-result:
		t.Fatal("stage completed before its preparation")
	default:
	}
	close(release)
	if name := <-result; name != "next" {
		t.Fatalf("staged name = %q, want next", name)
	}
	if calls.Load() != 1 {
		t.Fatalf("prepare calls = %d, want 1", calls.Load())
	}
}

func TestBackgroundImageStageReturnsPreparationFailure(t *testing.T) {
	var stage backgroundImageStage
	want := errors.New("download failed")
	stage.start("next", func() error { return want })
	name, started, err := stage.take(context.Background())
	if name != "next" || !started || !errors.Is(err, want) {
		t.Fatalf("take = (%q, %v, %v)", name, started, err)
	}
}
