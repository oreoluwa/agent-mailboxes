package service

import (
	"context"
	"errors"
	"fmt"
	"log"
	"time"

	"github.com/oreoluwa/agent-mailboxes/internal/model"
	"github.com/oreoluwa/agent-mailboxes/internal/store"
)

var errAutoDraftLeaseLost = errors.New("auto-draft job lease lost")

const (
	defaultAutoDraftLease         = 5 * time.Minute
	defaultAutoDraftRenewInterval = time.Minute
	defaultAutoDraftRenewTimeout  = 5 * time.Second
)

func (s *Service) enqueueAutoDraftJob(ctx context.Context, messageID string) error {
	settings, err := s.Store.GetMailboxAgentSettings(ctx, s.MailboxID)
	if err != nil {
		return err
	}
	if !settings.AutoDraftEnabled {
		return nil
	}
	_, _, err = s.Store.QueueAutoDraftJob(ctx, s.MailboxID, messageID, settings.PolicyVersion)
	return err
}

func (s *Service) runAutoDraftWorker(ctx context.Context, interval time.Duration) {
	if s.AgentPlanner == nil {
		return
	}
	if interval <= 0 {
		interval = 5 * time.Second
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		if err := s.flushAutoDraftJobs(ctx, 8); err != nil && ctx.Err() == nil {
			log.Printf("auto draft worker flush failed mailbox=%s: %v", s.MailboxID, err)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (s *Service) flushAutoDraftJobs(ctx context.Context, maxJobs int) error {
	if maxJobs <= 0 {
		maxJobs = 8
	}
	lease, renewInterval, renewTimeout := s.autoDraftLeaseSettings()
	for processed := 0; processed < maxJobs; processed++ {
		job, claimed, err := s.Store.ClaimAutoDraftJob(ctx, s.MailboxID, lease)
		if err != nil {
			return err
		}
		if !claimed {
			return nil
		}
		if err := s.processClaimedAutoDraftJob(ctx, job, lease, renewInterval, renewTimeout); err != nil {
			return err
		}
	}
	return nil
}

func (s *Service) autoDraftLeaseSettings() (time.Duration, time.Duration, time.Duration) {
	lease := s.autoDraftLease
	if lease <= 0 {
		lease = defaultAutoDraftLease
	}
	renewInterval := s.autoDraftRenewInterval
	if renewInterval <= 0 {
		renewInterval = defaultAutoDraftRenewInterval
	}
	renewTimeout := s.autoDraftRenewTimeout
	if renewTimeout <= 0 {
		renewTimeout = defaultAutoDraftRenewTimeout
	}
	return lease, renewInterval, renewTimeout
}

func (s *Service) processClaimedAutoDraftJob(parent context.Context, job store.AutoDraftJob, lease, renewInterval, renewTimeout time.Duration) error {
	ctx, cancel := context.WithCancelCause(parent)
	renewDone := make(chan error, 1)
	go s.renewAutoDraftLease(ctx, cancel, job, lease, renewInterval, renewTimeout, renewDone)

	err := s.runClaimedAutoDraftJob(ctx, job)
	cause := context.Cause(ctx)
	cancel(context.Canceled)
	renewErr := <-renewDone
	// A nil processing result means the job's fenced terminal update already
	// succeeded. A renewal that was concurrently in flight can observe the
	// cleared claim token and report false after that successful transition.
	if err == nil {
		return nil
	}
	if cause != nil && !errors.Is(cause, context.Canceled) {
		return cause
	}
	if renewErr != nil {
		return renewErr
	}
	return err
}

func (s *Service) renewAutoDraftLease(ctx context.Context, cancel context.CancelCauseFunc, job store.AutoDraftJob, lease, interval, timeout time.Duration, done chan<- error) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			done <- nil
			return
		case <-ticker.C:
			renewCtx, renewCancel := context.WithTimeout(ctx, timeout)
			renewed, err := s.Store.RenewAutoDraftJobLease(renewCtx, job.ID, job.ClaimToken, lease)
			renewCancel()
			if err != nil {
				err = fmt.Errorf("renew auto-draft job lease: %w", err)
			} else if !renewed {
				err = errAutoDraftLeaseLost
			}
			if err != nil {
				cancel(err)
				done <- err
				return
			}
		}
	}
}

func (s *Service) runClaimedAutoDraftJob(ctx context.Context, job store.AutoDraftJob) error {
	run, err := s.Store.CreateAutoDraftRun(ctx, s.MailboxID, job.MessageID, job.ID, job.PolicyVersion, "running", nil)
	if err != nil {
		return s.retryClaimedAutoDraftJob(ctx, job, err.Error())
	}
	settings, err := s.Store.GetMailboxAgentSettings(ctx, s.MailboxID)
	if err != nil {
		s.updateAutoDraftRun(ctx, run.ID, "failed", err)
		return s.retryClaimedAutoDraftJob(ctx, job, "auto draft retry scheduled")
	}
	if !settings.AutoDraftEnabled {
		err = errors.New("auto draft disabled")
		s.updateAutoDraftRun(ctx, run.ID, "blocked", err)
		return s.finishClaimedAutoDraftJob(ctx, job)
	}
	if settings.PolicyVersion != job.PolicyVersion {
		err = fmt.Errorf("stale auto-draft policy version: job=%d current=%d", job.PolicyVersion, settings.PolicyVersion)
		s.updateAutoDraftRun(ctx, run.ID, "blocked", err)
		return s.finishClaimedAutoDraftJob(ctx, job)
	}

	audience, err := s.autoDraftAudienceClaims(ctx, job.MessageID)
	completed := map[string]bool{}
	if err == nil {
		var ids []string
		ids, err = s.Store.CompletedAutoDraftAliases(ctx, job.ID)
		for _, id := range ids {
			completed[id] = true
		}
	}
	if err == nil {
		for _, claims := range audience {
			if claims.AliasID == nil || completed[*claims.AliasID] {
				continue
			}
			msgID := job.MessageID
			_, err = s.RunAgent(ctx, claims, RunAgentInput{
				MessageID: &msgID,
				Prompt:    "Draft a helpful reply for human review. Use tools as needed, create or revise a draft, then complete.",
			})
			if err == nil {
				var completedAlias bool
				completedAlias, err = s.Store.CompleteAutoDraftAlias(ctx, job.ID, *claims.AliasID, job.ClaimToken)
				if err == nil && !completedAlias {
					err = errAutoDraftLeaseLost
				}
			}
			if err != nil {
				break
			}
		}
	}
	if err != nil {
		if cause := context.Cause(ctx); cause != nil {
			s.updateAutoDraftRunAfterCancellation(context.WithoutCancel(ctx), run.ID, cause)
			return cause
		}
		reason := err
		status := "failed"
		finish := errors.Is(err, errAutoDraftDisabled) || job.Attempts >= 4
		if finish {
			status = "blocked"
		}
		s.updateAutoDraftRun(ctx, run.ID, status, reason)
		if finish {
			return s.finishClaimedAutoDraftJob(ctx, job)
		}
		return s.retryClaimedAutoDraftJob(ctx, job, "auto draft retry scheduled")
	}
	s.updateAutoDraftRun(ctx, run.ID, "completed", nil)
	return s.finishClaimedAutoDraftJob(ctx, job)
}

func (s *Service) updateAutoDraftRunAfterCancellation(ctx context.Context, runID string, runErr error) {
	cleanupCtx, cancel := context.WithTimeout(ctx, defaultAutoDraftRenewTimeout)
	defer cancel()
	s.updateAutoDraftRun(cleanupCtx, runID, "failed", runErr)
}

func (s *Service) updateAutoDraftRun(ctx context.Context, runID, status string, runErr error) {
	var reason *string
	if runErr != nil {
		text := runErr.Error()
		reason = &text
	}
	if err := s.Store.UpdateAutoDraftRunStatus(ctx, runID, status, reason); err != nil {
		log.Printf("auto-draft run status update failed mailbox=%s run=%s status=%s: %v", s.MailboxID, runID, status, err)
	}
}

func (s *Service) finishClaimedAutoDraftJob(ctx context.Context, job store.AutoDraftJob) error {
	if cause := context.Cause(ctx); cause != nil {
		return cause
	}
	done, err := s.Store.MarkAutoDraftJobDone(ctx, job.ID, job.ClaimToken)
	if err != nil {
		return err
	}
	if !done {
		return errAutoDraftLeaseLost
	}
	return nil
}

func (s *Service) retryClaimedAutoDraftJob(ctx context.Context, job store.AutoDraftJob, reason string) error {
	if cause := context.Cause(ctx); cause != nil {
		return cause
	}
	retried, err := s.Store.MarkAutoDraftJobRetry(ctx, job.ID, job.ClaimToken, reason, 2*time.Minute)
	if err != nil {
		return fmt.Errorf("retry auto draft job: %w", err)
	}
	if !retried {
		return errAutoDraftLeaseLost
	}
	return nil
}

// Automatic drafting uses the same alias boundary as interactive agents. The
// mailbox operator controls policy, but each run owns only one alias's context.
func (s *Service) autoDraftAudienceClaims(ctx context.Context, messageID string) ([]model.TokenClaims, error) {
	aliases, err := s.Store.ListAliases(ctx, s.MailboxID)
	if err != nil {
		return nil, err
	}
	out := []model.TokenClaims{}
	for _, alias := range aliases {
		if !alias.Enabled {
			continue
		}
		aliasID, mailboxID := alias.ID, s.MailboxID
		claims := model.TokenClaims{TokenID: "auto-draft", AliasID: &aliasID, MailboxID: &mailboxID, Scopes: map[model.Scope]bool{model.ScopeRead: true, model.ScopeSend: true}}
		if _, err := s.Store.ReadMessage(ctx, claims, messageID); err != nil {
			if errors.Is(err, store.ErrUnauthorized) || errors.Is(err, store.ErrNotFound) {
				continue
			}
			return nil, err
		}
		out = append(out, claims)
	}
	return out, nil
}
