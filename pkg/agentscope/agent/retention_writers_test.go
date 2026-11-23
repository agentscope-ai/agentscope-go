package agent

import (
	"context"
	"errors"
	"fmt"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/message"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/model"
	"testing"
)

func TestRetentionObserveInvalidates(t *testing.T) {
	a := retentionTestAgent(&seamModel{})
	before := a.retentionVersion
	if err := a.Observe(context.Background(), []*message.Msg{message.UserMsg("u", "new")}); err != nil {
		t.Fatal(err)
	}
	if a.retentionVersion == before {
		t.Fatal("Observe did not invalidate retained version")
	}
}

func TestRetentionCompactionAdmission(t *testing.T) {
	for _, strict := range []bool{false, true} {
		t.Run(fmt.Sprint(strict), func(t *testing.T) {
			entered, release := make(chan struct{}), make(chan struct{})
			m := &seamModel{tokens: func([]*message.Msg) int { return 1 }}
			m.respond = func(int, []*message.Msg, *model.CallOptions) (*model.ChatResponse, error) {
				close(entered)
				<-release
				return strictSummaryResponse(), nil
			}
			a := retentionTestAgent(m)
			before := a.retentionVersion
			done := make(chan error, 1)
			go func() {
				var err error
				if strict {
					_, err = a.CompactStrict(context.Background(), CompactStrictOptions{})
				} else {
					_, err = a.Compact(context.Background())
				}
				done <- err
			}()
			<-entered
			a.mu.Lock()
			idle, version, writers := a.retentionIdleLocked(), a.retentionVersion, a.retentionWriters
			a.mu.Unlock()
			close(release)
			if err := <-done; err != nil {
				t.Fatal(err)
			}
			if idle || writers == 0 || version == before {
				t.Fatal("unlocked model call was not admitted as a writer")
			}
			a.mu.Lock()
			defer a.mu.Unlock()
			if !a.retentionIdleLocked() || a.retentionVersion == version {
				t.Fatal("completion did not release and invalidate writer")
			}
		})
	}
}

func TestRetentionPublicWriters(t *testing.T) {
	tests := []struct {
		name string
		run  func(*UnifiedAgent) error
	}{
		{"thread update", func(a *UnifiedAgent) error {
			return a.ApplyThreadUpdate(context.Background(), ThreadUpdate{Reset: true})
		}},
		{"budget", func(a *UnifiedAgent) error { a.SetThreadSummaryTokens(10); return nil }},
		{"conversation", func(a *UnifiedAgent) error {
			c, err := a.NewConversation(context.Background())
			if err == nil {
				c.Close()
			}
			return err
		}},
		{"checkpoint failure", func(a *UnifiedAgent) error { _ = a.SaveCheckpoint(context.Background()); return nil }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			a := retentionTestAgent(&seamModel{})
			before := a.retentionVersion
			if err := tt.run(a); err != nil {
				t.Fatal(err)
			}
			if a.retentionVersion == before || a.retentionWriters != 0 {
				t.Fatal("writer failed to invalidate or leaked admission")
			}
		})
	}
}

func TestRetentionVersionExhaustion(t *testing.T) {
	a := retentionTestAgent(&seamModel{})
	a.retentionVersion.epoch = ^uint64(0)
	if err := a.Observe(context.Background(), []*message.Msg{message.UserMsg("u", "new")}); err != nil {
		t.Fatal(err)
	}
	if a.retentionIdleLocked() || !a.retentionVersion.exhausted {
		t.Fatal("exhausted version must stay invalid")
	}
}

func retentionTestAgent(m *seamModel) *UnifiedAgent {
	a := strictAgent(m)
	a.retentionTracking = true
	return a
}

func TestRetentionDisabled(t *testing.T) {
	a := strictAgent(&seamModel{})
	if err := a.Observe(context.Background(), []*message.Msg{message.UserMsg("u", "new")}); err != nil {
		t.Fatal(err)
	}
	if a.retentionVersion != (retentionVersion{}) || a.retentionIdleLocked() {
		t.Fatal("ordinary agent enabled preview tracking")
	}
}

func TestRetentionCompactionFailureReleases(t *testing.T) {
	for _, cancel := range []bool{false, true} {
		t.Run(fmt.Sprint(cancel), func(t *testing.T) {
			entered, release := make(chan struct{}), make(chan struct{})
			ctx, stop := context.WithCancel(context.Background())
			defer stop()
			m := &seamModel{tokens: func([]*message.Msg) int { return 1 }}
			m.respond = func(int, []*message.Msg, *model.CallOptions) (*model.ChatResponse, error) {
				close(entered)
				<-release
				if cancel {
					return nil, context.Canceled
				}
				return nil, errors.New("fixture error")
			}
			a := retentionTestAgent(m)
			done := make(chan error, 1)
			go func() { _, err := a.CompactStrict(ctx, CompactStrictOptions{}); done <- err }()
			<-entered
			if cancel {
				stop()
			}
			close(release)
			if err := <-done; err == nil {
				t.Fatal("expected generation failure")
			}
			a.mu.Lock()
			defer a.mu.Unlock()
			if !a.retentionIdleLocked() || a.retentionWriters != 0 {
				t.Fatal("failed operation leaked writer")
			}
		})
	}
}

func TestRetentionABA(t *testing.T) {
	a := retentionTestAgent(&seamModel{})
	before := a.retentionVersion
	original := cloneMsgs(a.state.Context)
	if err := a.ApplyThreadUpdate(context.Background(), ThreadUpdate{Reset: true}); err != nil {
		t.Fatal(err)
	}
	if err := a.Observe(context.Background(), original); err != nil {
		t.Fatal(err)
	}
	if a.retentionVersion == before {
		t.Fatal("restoring content revived a previous version")
	}
}

func TestRetentionReadOnlyView(t *testing.T) {
	a := retentionTestAgent(&seamModel{})
	before := a.retentionVersion
	_ = a.ThreadView()
	if a.retentionVersion != before {
		t.Fatal("read-only view changed version")
	}
}

func TestRetentionSummaryWriters(t *testing.T) {
	for _, offer := range []bool{false, true} {
		t.Run(fmt.Sprint(offer), func(t *testing.T) {
			a, snap, _, _ := tailFixture(t)
			a.retentionTracking = true
			before := a.retentionVersion
			var err error
			if offer {
				err = a.OfferSummary(context.Background(), snap, "notes", SummaryApplyOptions{})
			} else {
				err = a.ApplySummary(context.Background(), snap, "notes", SummaryApplyOptions{})
			}
			if err != nil {
				t.Fatal(err)
			}
			if a.retentionVersion == before || a.retentionWriters != 0 {
				t.Fatal("summary write not tracked")
			}
			if offer && a.retentionIdleLocked() {
				t.Fatal("pending offer admitted as idle")
			}
		})
	}
}

func TestRetentionReplyAdmission(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	m := &seamModel{respond: func(int, []*message.Msg, *model.CallOptions) (*model.ChatResponse, error) {
		close(entered)
		<-release
		return seamText("answer"), nil
	}}
	a := retentionTestAgent(m)
	before := a.retentionVersion
	done := make(chan error, 1)
	go func() { _, err := a.Reply(context.Background(), "hello"); done <- err }()
	<-entered
	a.mu.Lock()
	idle, version := a.retentionIdleLocked(), a.retentionVersion
	a.mu.Unlock()
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if idle || version == before {
		t.Fatal("running reply not tracked")
	}
}
