package managedtask

import (
	"context"
	"encoding/json"
	"errors"
	"maps"
	"reflect"
	"strings"
	"time"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/classifier"
	ae "github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/errors"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/model"
)

func (r *Router) snapshotClassification(in *ClassificationConfig) (*ClassificationConfig, error) {
	c := *in
	if c.Client == nil || (reflect.ValueOf(c.Client).Kind() == reflect.Pointer && reflect.ValueOf(c.Client).IsNil()) || c.Authorize == nil || c.Project == nil || c.MaxAttempts <= 0 || c.MaxAttempts > r.limits.MaxAttempts || len(c.Decisions) == 0 || strings.TrimSpace(c.Target.ID) == "" || r.deployments[c.Target.ID] != nil {
		return nil, ErrConfig
	}
	endpoint, name := c.Client.Target()
	if c.Target.Endpoint != endpoint || c.Target.Model != name || strings.TrimSpace(name) == "" || strings.TrimSpace(endpoint) == "" {
		return nil, ErrConfig
	}
	c.Decisions = append([]ChoiceDecision(nil), c.Decisions...)
	questions := map[string]classifier.Question{}
	for i := range c.Decisions {
		d := &c.Decisions[i]
		if _, ok := questions[d.QuestionID]; ok {
			return nil, ErrConfig
		}
		if !finite(d.MinConfidence) || d.MinConfidence < 0 || d.MinConfidence > 1 || len(d.Categories) != len(d.Question.Choices) {
			return nil, ErrConfig
		}
		d.Question.Choices = maps.Clone(d.Question.Choices)
		d.Categories = maps.Clone(d.Categories)
		for choice := range d.Question.Choices {
			category, ok := d.Categories[choice]
			if !ok {
				return nil, ErrConfig
			}
			if category != "" {
				if _, ok = r.categories[category]; !ok {
					return nil, ErrConfig
				}
			}
		}
		questions[d.QuestionID] = &d.Question
	}
	if classifier.ValidateRequest(classifier.Request{OperationID: "validate", State: json.RawMessage(`"configuration"`), Questions: questions}) != nil {
		return nil, ErrConfig
	}
	if c.Prices != nil {
		p := *c.Prices
		if strings.TrimSpace(p.Currency) == "" || p.CachedInputPerToken != nil {
			return nil, ErrConfig
		}
		for _, rate := range []**float64{&p.InputPerToken, &p.OutputPerToken} {
			if *rate != nil {
				v := **rate
				if !finite(v) || v < 0 {
					return nil, ErrConfig
				}
				*rate = &v
			}
		}
		c.Prices = &p
	}
	return &c, nil
}

func (r *Router) classify(ctx context.Context, task Task, state *execution) (string, error) {
	c := r.classification
	if err := c.Authorize(ctx, task, c.Target); err != nil {
		return "", hostError(ctx, err, ErrDenied)
	}
	if ctx.Err() != nil {
		return "", ctx.Err()
	}
	request, err := c.Project(ctx, task, c.Target)
	if err != nil {
		return "", hostError(ctx, err, ErrProjection)
	}
	if ctx.Err() != nil {
		return "", ctx.Err()
	}
	request.OperationID = task.ID + "/classification"
	if classifier.ValidateRequest(request) != nil || !c.matches(request) {
		return "", ErrRoute
	}
	raw, err := json.Marshal(request)
	if err != nil || len(raw) > r.limits.MaxRequestBytes {
		return "", ErrRoute
	}
	// Own projected state/questions across retries even when a host reuses buffers.
	request.State = append(json.RawMessage(nil), request.State...)
	request.Questions = make(map[string]classifier.Question, len(c.Decisions))
	for i := range c.Decisions {
		d := &c.Decisions[i]
		q := d.Question
		q.Choices = maps.Clone(q.Choices)
		request.Questions[d.QuestionID] = &q
	}
	for n := 0; n < c.MaxAttempts; n++ {
		if !state.remaining(r.limits.MaxAttempts) {
			return "", ErrCapacity
		}
		queueCtx, stop := context.WithTimeout(ctx, r.limits.QueueTimeout)
		err = r.admission.acquire(queueCtx)
		stop()
		if err != nil {
			if ctx.Err() != nil {
				return "", ctx.Err()
			}
			return "", ErrCapacity
		}
		out, callErr := r.classificationSend(ctx, task, request, state)
		r.admission.release()
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		if callErr == nil {
			return c.category(out)
		}
		if errors.Is(callErr, context.Canceled) || errors.Is(callErr, context.DeadlineExceeded) {
			return "", hostError(ctx, callErr, ErrRoute)
		}
		if !ae.IsRetryable(callErr) || n+1 == c.MaxAttempts {
			return "", ErrRoute
		}
		delay := r.limits.RetryBackoff
		if after, ok := ae.RetryAfterOf(callErr); ok && after > delay {
			delay = after
		}
		timer := time.NewTimer(delay)
		select {
		case <-timer.C:
		case <-ctx.Done():
			timer.Stop()
			return "", ctx.Err()
		}
	}
	return "", ErrRoute
}

func (c *ClassificationConfig) matches(request classifier.Request) bool {
	if len(request.Questions) != len(c.Decisions) {
		return false
	}
	for i := range c.Decisions {
		d := &c.Decisions[i]
		q, ok := request.Questions[d.QuestionID].(*classifier.ChoiceQuestion)
		if !ok || !reflect.DeepEqual(q, &d.Question) {
			return false
		}
	}
	return true
}
func (c *ClassificationConfig) category(out *classifier.Response) (string, error) {
	category := ""
	if out.Model != c.Target.Model {
		return "", ErrRoute
	}
	for i := range c.Decisions {
		d := &c.Decisions[i]
		a := out.Answers[d.QuestionID].(*classifier.ChoiceAnswer)
		selected := d.Categories[a.Choice]
		if a.Confidence < d.MinConfidence || selected == "" || (category != "" && category != selected) {
			return "", ErrRoute
		}
		category = selected
	}
	return category, nil
}

func (r *Router) classificationSend(ctx context.Context, task Task, request classifier.Request, state *execution) (*classifier.Response, error) {
	c := r.classification
	index := -1
	kind := classifier.AttemptKind("")
	out, err := c.Client.ClassifyWithDispatch(ctx, request, classifier.DispatchLimits{MaxRequestBytes: int64(r.limits.MaxRequestBytes), MaxResponseBytes: int64(r.limits.MaxResponseBytes)}, func(k classifier.AttemptKind) error {
		if k != classifier.PhysicalAttempt && k != classifier.SimulatedAttempt {
			return ErrRoute
		}
		if err := c.Authorize(ctx, task, c.Target); err != nil {
			return hostError(ctx, err, ErrDenied)
		}
		state.mu.Lock()
		defer state.mu.Unlock()
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if state.terminal || len(state.result.Attempts) >= r.limits.MaxAttempts || index >= 0 {
			return ErrCapacity
		}
		index = len(state.result.Attempts)
		kind = k
		state.starts = append(state.starts, time.Now())
		state.result.Attempts = append(state.result.Attempts, Attempt{Kind: AttemptKind(k), Number: index + 1, OperationID: request.OperationID, DeploymentID: c.Target.ID, Endpoint: c.Target.Endpoint, RequestedModel: c.Target.Model})
		return nil
	})
	reportErr := classifier.ValidateReport(request, out)
	if reportErr != nil {
		err = ErrRoute
	} else if (index < 0 && len(out.Attempts) > 0) || (index >= 0 && (len(out.Attempts) != 1 || out.Attempts[0].Kind != kind || out.Attempts[0].RequestedModel != "" && out.Attempts[0].RequestedModel != c.Target.Model)) {
		err = ErrRoute
		reportErr = ErrRoute
	}
	if err == nil {
		if index < 0 {
			err = ErrRoute
		}
		if classifier.ValidateResponse(request, out) != nil {
			err = ErrRoute
		}
	}
	safeErr := error(nil)
	if err != nil {
		safeErr = hostError(ctx, err, ErrRoute)
	}
	state.mu.Lock()
	if index >= 0 && !state.terminal {
		a := &state.result.Attempts[index]
		a.Duration = time.Since(state.starts[index])
		a.Completed = true
		a.Err = safeErr
		if reportErr == nil && len(out.Attempts) == 1 {
			report := out.Attempts[0]
			a.Model = report.Model
			if report.Usage != nil {
				a.Usage = &model.TextUsage{InputTokens: copyCount(report.Usage.InputTokens), OutputTokens: copyCount(report.Usage.OutputTokens)}
			}
		}
	}
	state.mu.Unlock()
	return out, err
}
