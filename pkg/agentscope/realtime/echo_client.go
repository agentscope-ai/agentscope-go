package realtime

import (
	"context"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/message"
)

// EchoClient/Stream is a reference implementation: it simply echoes the first message and is useful for testing pipelines.

type EchoClient struct{}

type echoStream struct {
	msg  *message.Msg
	done bool
}

func (EchoClient) Start(ctx context.Context, initMsgs []*message.Msg) (Stream, error) {
	_ = ctx
	var first *message.Msg
	if len(initMsgs) > 0 {
		first = initMsgs[0]
	}
	return &echoStream{msg: first}, nil
}

func (s *echoStream) Recv() (*message.Msg, error) {
	if s.done {
		return nil, nil
	}
	s.done = true
	return s.msg, nil
}

func (s *echoStream) Close() error {
	return nil
}
