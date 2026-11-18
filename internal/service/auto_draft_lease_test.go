package service

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

type renewingAutoDraftStore struct {
	*autoDraftCompletionStore
	mu          sync.Mutex
	renewCount  int
	loseLease   bool
	renewSignal chan struct{}
}

type terminalRaceAutoDraftStore struct {
	*autoDraftCompletionStore
	renewStarted chan struct{}
	terminalDone chan struct{}
}

func (s *terminalRaceAutoDraftStore) RenewAutoDraftJobLease(ctx context.Context, jobID, claimToken string, lease time.Duration) (bool, error) {
	close(s.renewStarted)
	select {
	case <-s.terminalDone:
		return false, nil
	case <-ctx.Done():
		return false, ctx.Err()
	}
}

func (s *terminalRaceAutoDraftStore) MarkAutoDraftJobDone(ctx context.Context, jobID, claimToken string) (bool, error) {
	select {
	case <-s.renewStarted:
	case <-ctx.Done():
		return false, ctx.Err()
	}
	done, err := s.autoDraftCompletionStore.MarkAutoDraftJobDone(ctx, jobID, claimToken)
	close(s.terminalDone)
	// Wait until the renewal miss has canceled the processing context. This
	// makes the regression deterministic: process completion returns nil while
	// the renewal monitor has already recorded lease loss.
	<-ctx.Done()
	return done, err
}

func (s *renewingAutoDraftStore) RenewAutoDraftJobLease(context.Context, string, string, time.Duration) (bool, error) {
	s.mu.Lock()
	s.renewCount++
	lose := s.loseLease
	s.mu.Unlock()
	select {
	case s.renewSignal <- struct{}{}:
	default:
	}
	return !lose, nil
}

func (s *renewingAutoDraftStore) renewals() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.renewCount
}

type delayedAutoDraftPlanner struct {
	once     sync.Once
	started  chan struct{}
	release  chan struct{}
	canceled chan struct{}
	next     *perAliasRetryPlanner
}

func (p *delayedAutoDraftPlanner) Next(ctx context.Context, req AgentPlanRequest) (AgentPlanStep, error) {
	blocked := false
	p.once.Do(func() {
		blocked = true
		close(p.started)
	})
	if blocked {
		select {
		case <-p.release:
		case <-ctx.Done():
			close(p.canceled)
			return AgentPlanStep{}, ctx.Err()
		}
	}
	return p.next.Next(ctx, req)
}

func TestAutoDraftRenewsLeaseDuringLongAgentRun(t *testing.T) {
	harness := &renewingAutoDraftStore{
		autoDraftCompletionStore: newAutoDraftCompletionStore(),
		renewSignal:              make(chan struct{}, 8),
	}
	planner := &delayedAutoDraftPlanner{
		started:  make(chan struct{}),
		release:  make(chan struct{}),
		canceled: make(chan struct{}),
		next:     &perAliasRetryPlanner{calls: map[string]int{}},
	}
	svc := &Service{
		Store:                  harness,
		MailboxID:              "m1",
		MailboxName:            "primary",
		AgentPlanner:           planner,
		autoDraftLease:         80 * time.Millisecond,
		autoDraftRenewInterval: 10 * time.Millisecond,
		autoDraftRenewTimeout:  20 * time.Millisecond,
	}
	result := make(chan error, 1)
	go func() { result <- svc.flushAutoDraftJobs(context.Background(), 1) }()

	select {
	case <-planner.started:
	case <-time.After(time.Second):
		t.Fatal("agent planner did not start")
	}
	for i := 0; i < 2; i++ {
		select {
		case <-harness.renewSignal:
		case <-time.After(time.Second):
			t.Fatal("lease was not renewed during delayed run")
		}
	}
	close(planner.release)
	if err := <-result; err != nil {
		t.Fatalf("flush delayed auto-draft job: %v", err)
	}
	if harness.renewals() < 2 {
		t.Fatalf("renewals = %d, want at least 2", harness.renewals())
	}
	if harness.doneCalls != 1 || harness.retryCalls != 0 {
		t.Fatalf("terminal transitions done=%d retry=%d", harness.doneCalls, harness.retryCalls)
	}
}

func TestAutoDraftLostLeaseCancelsAgentAndRejectsTerminalUpdates(t *testing.T) {
	harness := &renewingAutoDraftStore{
		autoDraftCompletionStore: newAutoDraftCompletionStore(),
		loseLease:                true,
		renewSignal:              make(chan struct{}, 1),
	}
	planner := &delayedAutoDraftPlanner{
		started:  make(chan struct{}),
		release:  make(chan struct{}),
		canceled: make(chan struct{}),
		next:     &perAliasRetryPlanner{calls: map[string]int{}},
	}
	svc := &Service{
		Store:                  harness,
		MailboxID:              "m1",
		MailboxName:            "primary",
		AgentPlanner:           planner,
		autoDraftLease:         80 * time.Millisecond,
		autoDraftRenewInterval: 10 * time.Millisecond,
		autoDraftRenewTimeout:  20 * time.Millisecond,
	}
	err := svc.flushAutoDraftJobs(context.Background(), 1)
	if !errors.Is(err, errAutoDraftLeaseLost) {
		t.Fatalf("flush error = %v, want lease lost", err)
	}
	select {
	case <-planner.canceled:
	case <-time.After(time.Second):
		t.Fatal("lost lease did not cancel in-flight agent")
	}
	if harness.doneCalls != 0 || harness.retryCalls != 0 {
		t.Fatalf("stale worker terminal transitions done=%d retry=%d", harness.doneCalls, harness.retryCalls)
	}
	completed, err := harness.CompletedAutoDraftAliases(context.Background(), harness.job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(completed) != 0 {
		t.Fatalf("stale worker persisted aliases: %#v", completed)
	}
}

func TestAutoDraftSuccessfulTerminalUpdateWinsConcurrentRenewalMiss(t *testing.T) {
	harness := &terminalRaceAutoDraftStore{
		autoDraftCompletionStore: newAutoDraftCompletionStore(),
		renewStarted:             make(chan struct{}),
		terminalDone:             make(chan struct{}),
	}
	svc := &Service{
		Store:                  harness,
		MailboxID:              "m1",
		MailboxName:            "primary",
		AgentPlanner:           &perAliasRetryPlanner{calls: map[string]int{}},
		autoDraftLease:         100 * time.Millisecond,
		autoDraftRenewInterval: time.Millisecond,
		autoDraftRenewTimeout:  50 * time.Millisecond,
	}

	if err := svc.flushAutoDraftJobs(context.Background(), 1); err != nil {
		t.Fatalf("successful terminal transition reported renewal race: %v", err)
	}
	if harness.doneCalls != 1 || harness.retryCalls != 0 {
		t.Fatalf("terminal transitions done=%d retry=%d", harness.doneCalls, harness.retryCalls)
	}
}
