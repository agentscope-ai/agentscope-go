package evalkit

import (
	"context"
	"fmt"
	"sync"
	"time"
)

type loadScoreState struct {
	mu        sync.Mutex
	open      bool
	remaining int
	pending   map[int]*taskExecution
	rows      []TaskQualityResult
	wake      chan struct{}
}

func scoreLoad(ctx context.Context, cfg *LoadConfig, report *LoadReport, pending map[int]*taskExecution) {
	report.ScoringStartedAt = time.Now()
	phase, cancel := context.WithTimeout(ctx, cfg.ScorePhaseTimeout)
	defer cancel()
	s := &loadScoreState{open: true, remaining: len(pending), pending: pending, rows: report.Results, wake: make(chan struct{}, 1)}
	jobs := make(chan int, len(pending))
	for j := range report.Results {
		if pending[j] != nil {
			jobs <- j
		}
	}
	close(jobs)
	workers := min(cfg.MaxScorers, len(pending))
	for range workers {
		go func() {
			for j := range jobs {
				s.mu.Lock()
				e := s.pending[j]
				if !s.open || phase.Err() != nil || e == nil {
					s.mu.Unlock()
					return
				}
				delete(s.pending, j)
				s.rows[j].QualityStatus = QualityScoreUnfinished
				s.rows[j].ScoreStartedAt = time.Now()
				s.mu.Unlock()
				// A worker owns its actual Score invocation, error inspection and cleanup.
				// A timeout never releases its slot to another still-running scorer.
				scoreCtx, scoreCancel := context.WithTimeout(phase, cfg.ScoreTimeout)
				scorer := cfg.Scorer
				var err error
				if scorer == nil {
					scorer, err = BuildScorer(&e.task.Scorer)
				}
				var score float64
				if err == nil {
					score, err = scorer.Score(scoreCtx, &e.task, &e.outcome)
				}
				failed := err != nil
				detail := ""
				if err != nil {
					detail = errorDetail(err, "scorer failed without a message")
				} else if !validScore(score) {
					failed = true
					detail = "scorer returned a nonfinite or out-of-range score"
				}
				e.close()
				finished := time.Now()
				deadline, _ := scoreCtx.Deadline()
				if scoreCtx.Err() != nil {
					failed = true
					detail = fmt.Sprintf("scoring: %v", scoreCtx.Err())
				} else if !finished.Before(deadline) {
					failed = true
					detail = "scoring: deadline exceeded"
				}
				scoreCancel()
				s.mu.Lock()
				acceptedAt := time.Now()
				phaseDeadline, _ := phase.Deadline()
				if s.open && phase.Err() == nil && acceptedAt.Before(phaseDeadline) {
					if !acceptedAt.Before(deadline) {
						failed = true
						detail = "scoring: deadline exceeded"
					}
					row := &s.rows[j]
					row.ScoreFinishedAt = finished
					if failed {
						row.QualityStatus = QualityScoreError
						row.Task.ScoreError = detail
					} else {
						row.QualityStatus = QualityScored
						row.Task.Score = score
						row.Task.Pass = score >= report.Manifest.QualityThreshold
					}
					s.remaining--
				}
				s.mu.Unlock()
				select {
				case s.wake <- struct{}{}:
				default:
				}
			}
		}()
	}
	for {
		s.mu.Lock()
		done := s.remaining == 0
		s.mu.Unlock()
		if done {
			break
		}
		select {
		case <-s.wake:
		case <-phase.Done():
			goto finish
		}
	}
finish:
	s.mu.Lock()
	report.ScoringFinishedAt = time.Now()
	if deadline, ok := phase.Deadline(); ok && report.ScoringFinishedAt.After(deadline) {
		report.ScoringFinishedAt = deadline
	}
	if report.ScoringFinishedAt.Before(report.ScoringStartedAt) {
		report.ScoringFinishedAt = report.ScoringStartedAt
	}
	s.open = false
	abandoned := s.pending
	s.pending = nil
	s.rows = nil
	s.mu.Unlock()
	for _, e := range abandoned {
		e.close()
	}
}
