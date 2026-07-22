package retry

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"
)

func TestLoopReturnsErrorForZeroAttempts(t *testing.T) {
	ctx := context.Background()
	backoff := func(attempt int) time.Duration { return time.Millisecond }
	callCount := 0
	fn := func(attempt int) (string, error, bool) {
		callCount++
		return "", nil, false
	}

	_, err := Loop(ctx, 0, backoff, fn)

	if err == nil {
		t.Error("expected error for attempts=0, got nil")
	}
	if callCount > 0 {
		t.Errorf("expected fn not to be called, but was called %d times", callCount)
	}
}

func TestLoopReturnsErrorForNegativeAttempts(t *testing.T) {
	ctx := context.Background()
	backoff := func(attempt int) time.Duration { return time.Millisecond }
	callCount := 0
	fn := func(attempt int) (string, error, bool) {
		callCount++
		return "", nil, false
	}

	_, err := Loop(ctx, -5, backoff, fn)

	if err == nil {
		t.Error("expected error for attempts=-5, got nil")
	}
	if callCount > 0 {
		t.Errorf("expected fn not to be called, but was called %d times", callCount)
	}
}

func TestLoopSucceedsOnFirstAttempt(t *testing.T) {
	ctx := context.Background()
	backoff := func(attempt int) time.Duration { return time.Millisecond }
	callCount := 0
	expectedResult := "success"

	fn := func(attempt int) (string, error, bool) {
		callCount++
		return expectedResult, nil, false // success, don't retry
	}

	result, err := Loop(ctx, 3, backoff, fn)

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result != expectedResult {
		t.Errorf("expected result %q, got %q", expectedResult, result)
	}
	if callCount != 1 {
		t.Errorf("expected fn to be called once, was called %d times", callCount)
	}
}

func TestLoopRetriesUntilSuccess(t *testing.T) {
	ctx := context.Background()
	backoff := func(attempt int) time.Duration { return time.Millisecond }
	callCount := 0
	expectedResult := "success"

	fn := func(attempt int) (string, error, bool) {
		callCount++
		if callCount < 3 {
			return "", fmt.Errorf("attempt %d failed", callCount), true // retry
		}
		return expectedResult, nil, false // success
	}

	result, err := Loop(ctx, 3, backoff, fn)

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result != expectedResult {
		t.Errorf("expected result %q, got %q", expectedResult, result)
	}
	if callCount != 3 {
		t.Errorf("expected fn to be called 3 times, was called %d times", callCount)
	}
}

func TestLoopExhaustsAttemptsAndReturnsLastError(t *testing.T) {
	ctx := context.Background()
	backoff := func(attempt int) time.Duration { return time.Millisecond }
	callCount := 0
	expectedError := errors.New("persistent failure")

	fn := func(attempt int) (string, error, bool) {
		callCount++
		return "", expectedError, true // always retry
	}

	_, err := Loop(ctx, 3, backoff, fn)

	if err != expectedError {
		t.Errorf("expected error %v, got %v", expectedError, err)
	}
	if callCount != 3 {
		t.Errorf("expected fn to be called 3 times, was called %d times", callCount)
	}
}

func TestLoopRespectsCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	backoff := func(attempt int) time.Duration { return 100 * time.Millisecond }
	callCount := 0

	fn := func(attempt int) (string, error, bool) {
		callCount++
		return "", errors.New("will retry"), true
	}

	// Cancel after first attempt (during the backoff sleep)
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()

	_, err := Loop(ctx, 10, backoff, fn)

	if err != context.Canceled {
		t.Errorf("expected context.Canceled, got %v", err)
	}
	if callCount != 1 {
		t.Errorf("expected fn to be called once before cancellation, was called %d times", callCount)
	}
}
