package inference

import (
	"context"
	"crypto/tls"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/net/http2"
)

func TestManagedHTTP2RefusedStreamDoesNotReplay(t *testing.T) {
	var sends atomic.Int32
	server := httptest.NewUnstartedServer(http.NotFoundHandler())
	server.EnableHTTP2 = true
	server.Config.TLSNextProto = map[string]func(*http.Server, *tls.Conn, http.Handler){"h2": func(_ *http.Server, conn *tls.Conn, _ http.Handler) {
		defer conn.Close()
		preface := make([]byte, len(http2.ClientPreface))
		if _, err := io.ReadFull(conn, preface); err != nil {
			return
		}
		framer := http2.NewFramer(conn, conn)
		if err := framer.WriteSettings(); err != nil {
			return
		}
		for {
			frame, err := framer.ReadFrame()
			if err != nil {
				return
			}
			switch f := frame.(type) {
			case *http2.SettingsFrame:
				if !f.IsAck() {
					if err := framer.WriteSettingsAck(); err != nil {
						return
					}
				}
			case *http2.HeadersFrame:
				sends.Add(1)
				if f.StreamEnded() {
					if err := framer.WriteRSTStream(f.StreamID, http2.ErrCodeRefusedStream); err != nil {
						return
					}
				}
			case *http2.DataFrame:
				if f.StreamEnded() {
					if err := framer.WriteRSTStream(f.StreamID, http2.ErrCodeRefusedStream); err != nil {
						return
					}
				}
			}
		}
	}}
	server.StartTLS()
	defer server.Close()
	client := server.Client()
	transport := client.Transport.(*http.Transport).Clone()
	transport.ForceAttemptHTTP2 = true
	client.Transport = transport
	d, ledger := httpDeployment(t, server.URL, 1)
	ctx, cancel := context.WithTimeout(WithIdentity(context.Background(), Identity{TenantID: "host"}), 2*time.Second)
	defer cancel()
	ctx, finish := d.Start(ctx, "reasoning")
	err := CurrentOperation(ctx).DoJSON(ctx, client, "POST", server.URL, struct{}{}, nil, nil)
	finish(err)
	if err == nil {
		t.Fatal("refused stream succeeded")
	}
	if sends.Load() != 1 || len(ledger.Snapshot().Attempts) != 1 {
		t.Fatalf("HTTP2 replay escaped attempt cap: sends=%d err=%v", sends.Load(), err)
	}
}
