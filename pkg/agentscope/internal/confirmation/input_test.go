package confirmation

import (
	"context"
	"testing"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/event"
)

func TestNestedOwnersKeepTheirOwnInput(t *testing.T) {
	parent, child := new(int), new(int)
	parentInput, childInput := make(chan event.UserConfirmResultEvent, 1), make(chan event.UserConfirmResultEvent, 1)
	parentCtx := WithInput(context.Background(), parent, parentInput)
	childCtx := WithInput(parentCtx, child, childInput)
	parentInput <- event.NewUserConfirmResultEvent("parent", nil)
	childInput <- event.NewUserConfirmResultEvent("child", nil)
	if Input(childCtx, parent) != nil || Input(parentCtx, child) != nil || Input(context.Background(), parent) != nil {
		t.Fatal("confirmation escaped its owner context")
	}
	if result := <-Input(childCtx, child); result.ReplyID != "child" {
		t.Fatalf("child received %s", result.ReplyID)
	}
	if result := <-Input(parentCtx, parent); result.ReplyID != "parent" {
		t.Fatalf("parent received %s", result.ReplyID)
	}
}
