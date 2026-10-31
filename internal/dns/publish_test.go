package dns

import (
	"context"
	"errors"
	"testing"
	"time"
)

// publishCounter returns a publish func and the number of times it ran.
func publishCounter() (func(context.Context) error, *int) {
	n := 0
	return func(context.Context) error { n++; return nil }, &n
}

// TestPublisherFirstPublishIsOwed is audit B10: a new process cannot know whether a previous one left staged changes, so it publishes once even with nothing staged.
func TestPublisherFirstPublishIsOwed(t *testing.T) {
	p := NewPublisher(0)
	publish, n := publishCounter()
	for range 2 {
		if err := p.Publish(context.Background(), publish); err != nil {
			t.Fatalf("Publish: %v", err)
		}
	}
	if *n != 1 {
		t.Errorf("published %d times, want exactly the one a new provider owes", *n)
	}
}

// TestPublisherFailedPublishStaysOwed is audit B10 within a process: after a failed publish the next call publishes even though nothing new was staged.
func TestPublisherFailedPublishStaysOwed(t *testing.T) {
	p := NewPublisher(0)
	if err := p.Publish(context.Background(), func(context.Context) error { return errors.New("down") }); err == nil {
		t.Fatal("expected the failing publish's error")
	}
	publish, n := publishCounter()
	if err := p.Publish(context.Background(), publish); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	if *n != 1 {
		t.Errorf("published %d times after a failure, want 1", *n)
	}
}

// TestPublisherWriteDuringPublishStaysOwed is audit D3: a write landing while a publish is in flight is not covered by it.
func TestPublisherWriteDuringPublishStaysOwed(t *testing.T) {
	p := NewPublisher(0)
	if err := p.Publish(context.Background(), func(context.Context) error { p.Staged(); return nil }); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	publish, n := publishCounter()
	if err := p.Publish(context.Background(), publish); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	if *n != 1 {
		t.Errorf("the racing write was not published: %d publishes", *n)
	}
}

// TestPublisherIntervalCoalesces is audit M4: a burst of writes inside the interval is held back and covered by one later publish.
func TestPublisherIntervalCoalesces(t *testing.T) {
	p := NewPublisher(50 * time.Millisecond)
	publish, n := publishCounter()
	ctx := context.Background()

	if err := p.Publish(ctx, publish); err != nil {
		t.Fatalf("first Publish: %v", err)
	}
	for range 2 {
		p.Staged()
		var deferred *PublishDeferred
		if err := p.Publish(ctx, publish); !errors.As(err, &deferred) {
			t.Fatalf("Publish inside the interval = %v, want *PublishDeferred", err)
		}
		if deferred.After <= 0 || deferred.After > 50*time.Millisecond {
			t.Errorf("After = %s, want a wait inside the interval", deferred.After)
		}
	}

	time.Sleep(60 * time.Millisecond)
	if err := p.Publish(ctx, publish); err != nil {
		t.Fatalf("Publish after the wait: %v", err)
	}
	if *n != 2 {
		t.Errorf("%d publishes, want 2: the first and one covering the burst", *n)
	}
}
