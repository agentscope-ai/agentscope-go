package rag

import (
	"context"
	"errors"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"math"
	"net"
	"sync/atomic"
	"testing"
	"time"

	"github.com/qdrant/go-client/qdrant"
	"google.golang.org/grpc"
)

type filterServer struct {
	qdrant.UnimplementedPointsServer
	requests chan *qdrant.QueryPoints
	upserts  chan *qdrant.UpsertPoints
	fail     atomic.Bool
	empty    atomic.Bool
}

func (s *filterServer) Query(_ context.Context, r *qdrant.QueryPoints) (*qdrant.QueryResponse, error) {
	s.requests <- r
	if s.fail.Load() {
		return nil, status.Error(codes.InvalidArgument, "fixture rejection")
	}
	if s.empty.Load() {
		return &qdrant.QueryResponse{}, nil
	}
	return &qdrant.QueryResponse{Result: []*qdrant.ScoredPoint{{Id: qdrant.NewID("00000000-0000-0000-0000-000000000001"), Score: 0.75, Payload: qdrant.NewValueMap(map[string]any{"content": "result"})}}}, nil
}
func newFilterClient(t *testing.T) (*qdrant.Client, *filterServer) {
	t.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := grpc.NewServer()
	fixture := &filterServer{requests: make(chan *qdrant.QueryPoints, 10), upserts: make(chan *qdrant.UpsertPoints, 10)}
	qdrant.RegisterPointsServer(srv, fixture)
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(srv.Stop)
	c, err := qdrant.NewClient(&qdrant.Config{Host: "127.0.0.1", Port: lis.Addr().(*net.TCPAddr).Port, SkipCompatibilityCheck: true})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c, fixture
}

type filterEmbedder struct{ calls int }

func (e *filterEmbedder) Embed(context.Context, []string) ([][]float32, error) {
	e.calls++
	return [][]float32{{1, 0}}, nil
}
func TestQdrantFilterWireAndHostConstraint(t *testing.T) {
	client, s := newFilterClient(t)
	embed := &filterEmbedder{}
	host := MetadataFilter{MetadataEqualString("tenant", "host")}
	idx, err := NewQdrantTextIndex(QdrantTextConfig{Client: client, Collection: "test", Embedder: embed, RequiredFilter: host})
	if err != nil {
		t.Fatal(err)
	}
	host[0] = MetadataEqualString("tenant", "changed")
	lo, hi := 1.0, 2.0
	filter := MetadataFilter{MetadataEqualString("tenant", "caller"), MetadataEqualBool("active", false), MetadataEqualInt("revision", math.MaxInt64), MetadataEqualFloat("score", 1.5), MetadataInRange("rating", NumericRange{GT: &lo, LTE: &hi})}
	lo = 99 // constructor owns its bound values
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	docs, err := idx.QueryWithFilter(ctx, "query", 2, filter)
	if err != nil {
		t.Fatal(err)
	}
	if len(docs) != 1 || docs[0].Content != "result" || docs[0].Score != 0.75 {
		t.Fatalf("docs=%+v", docs)
	}
	req := <-s.requests
	if req.GetLimit() != 2 || len(req.Filter.Must) != 6 {
		t.Fatalf("request=%v", req)
	}
	fields := req.Filter.Must
	if fields[0].GetField().Key != "tenant" || fields[0].GetField().Match.GetKeyword() != "host" || fields[1].GetField().Match.GetKeyword() != "caller" {
		t.Fatal("host filter overwritten or payload path incorrect")
	}
	if value, ok := fields[2].GetField().Match.MatchValue.(*qdrant.Match_Boolean); !ok || value.Boolean {
		t.Fatal("false boolean match missing")
	}
	if fields[3].GetField().Match.GetInteger() != math.MaxInt64 {
		t.Fatal("integer precision lost")
	}
	if r := fields[4].GetField().Range; r == nil || r.Gte == nil || r.Lte == nil || r.GetGte() != 1.5 || r.GetLte() != 1.5 {
		t.Fatalf("float equality=%v", r)
	}
	if r := fields[5].GetField().Range; r.Gt == nil || r.Lte == nil || r.GetGt() != 1 || r.GetLte() != 2 {
		t.Fatalf("range=%v", r)
	}
	if _, err := idx.Query(ctx, "legacy", 0); err != nil {
		t.Fatal(err)
	}
	if req = <-s.requests; req.GetLimit() != 10 || len(req.Filter.Must) != 1 {
		t.Fatalf("legacy query lost host constraint: %v", req)
	}
}
func TestQdrantRejectsInvalidFiltersBeforeEmbedding(t *testing.T) {
	client, _ := newFilterClient(t)
	embed := &filterEmbedder{}
	idx, err := NewQdrantTextIndex(QdrantTextConfig{Client: client, Collection: "test", Embedder: embed})
	if err != nil {
		t.Fatal(err)
	}
	a, b := 2.0, 1.0
	inf := math.Inf(1)
	cases := []MetadataCondition{{}, MetadataEqualString("", "x"), MetadataEqualString("nested.key", "x"), MetadataEqualString("content", "x"), MetadataEqualString("vector", "x"), MetadataEqualFloat("f", math.NaN()), MetadataEqualFloat("f", inf), MetadataInRange("f", NumericRange{}), MetadataInRange("f", NumericRange{GT: &a, LTE: &b}), MetadataInRange("f", NumericRange{GT: &a, LTE: &a}), MetadataInRange("f", NumericRange{GT: &a, GTE: &b}), MetadataInRange("f", NumericRange{LTE: &inf})}
	for _, c := range cases {
		if _, err := idx.QueryWithFilter(context.Background(), "x", 1, MetadataFilter{c}); err == nil {
			t.Errorf("accepted invalid condition %+v", c)
		}
	}
	if embed.calls != 0 {
		t.Fatalf("invalid filter caused %d embedding requests", embed.calls)
	}
	if _, err := NewQdrantIndex(QdrantConfig{Client: client, Collection: "test", RequiredFilter: MetadataFilter{{}}}); err == nil {
		t.Fatal("accepted invalid host filter")
	}
}
func TestQdrantVectorQueryWithoutFilter(t *testing.T) {
	client, s := newFilterClient(t)
	idx, err := NewQdrantIndex(QdrantConfig{Client: client, Collection: "test"})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if _, err := idx.QueryVector(ctx, []float32{1, 0}, 1, nil); err != nil {
		t.Fatal(err)
	}
	if req := <-s.requests; req.Filter != nil {
		t.Fatalf("unexpected filter: %v", req.Filter)
	}
	for _, v := range [][]float32{nil, {float32(math.NaN())}, {float32(math.Inf(1))}} {
		if _, err := idx.QueryVector(ctx, v, 1, nil); err == nil {
			t.Fatalf("accepted vector %v", v)
		}
	}
}

func TestQdrantQueryErrorsAndEmptyResults(t *testing.T) {
	client, s := newFilterClient(t)
	idx, err := NewQdrantIndex(QdrantConfig{Client: client, Collection: "test"})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	s.empty.Store(true)
	docs, err := idx.QueryVector(ctx, []float32{1}, 1, nil)
	if err != nil || len(docs) != 0 {
		t.Fatalf("empty=%v %v", docs, err)
	}
	s.fail.Store(true)
	if _, err := idx.QueryVector(ctx, []float32{1}, 1, nil); err == nil {
		t.Fatal("RPC error lost")
	}
	canceled, stop := context.WithCancel(ctx)
	stop()
	if _, err := idx.QueryVector(canceled, []float32{1}, 1, nil); err == nil {
		t.Fatal("canceled RPC succeeded")
	}
}

type queryErrorEmbedder struct {
	vectors [][]float32
	err     error
}

func (e queryErrorEmbedder) Embed(context.Context, []string) ([][]float32, error) {
	return e.vectors, e.err
}
func TestQdrantRejectsBadQueryEmbeddings(t *testing.T) {
	client, _ := newFilterClient(t)
	for _, embed := range []queryErrorEmbedder{{err: errors.New("embedding failed")}, {}, {vectors: [][]float32{{1}, {2}}}, {vectors: [][]float32{{}}}} {
		idx, err := NewQdrantTextIndex(QdrantTextConfig{Client: client, Collection: "test", Embedder: embed})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := idx.Query(context.Background(), "q", 1); err == nil {
			t.Fatal("bad embedding accepted")
		}
	}
}
func TestQdrantRangeBoundaries(t *testing.T) {
	zero := 0.0
	one := 1.0
	cases := []NumericRange{{GTE: &zero, LTE: &zero}, {GTE: &zero, LT: &one}, {GT: &zero}, {LT: &one}, {LTE: &zero}}
	for _, bounds := range cases {
		c := MetadataInRange("number", bounds)
		condition, err := c.qdrantCondition("vector")
		if err != nil {
			t.Fatal(err)
		}
		r := condition.GetField().Range
		if (r.Gt != nil) != (bounds.GT != nil) || (r.Gte != nil) != (bounds.GTE != nil) || (r.Lt != nil) != (bounds.LT != nil) || (r.Lte != nil) != (bounds.LTE != nil) {
			t.Fatalf("bounds lost: %+v", r)
		}
	}
}

func (s *filterServer) Upsert(_ context.Context, r *qdrant.UpsertPoints) (*qdrant.PointsOperationResponse, error) {
	s.upserts <- r
	for _, p := range r.Points {
		if _, ok := p.Payload["vector"]; ok {
			return nil, status.Error(codes.InvalidArgument, "vector duplicated in payload")
		}
	}
	return &qdrant.PointsOperationResponse{Result: &qdrant.UpdateResult{Status: qdrant.UpdateStatus_Completed}}, nil
}
func TestQdrantTextIndexIngestionDoesNotPanic(t *testing.T) {
	client, _ := newFilterClient(t)
	idx, err := NewQdrantTextIndex(QdrantTextConfig{Client: client, Collection: "test", Embedder: &filterEmbedder{}})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	docs := []Document{{ID: "00000000-0000-0000-0000-000000000001", Content: "hello", Meta: map[string]any{"tenant": "alpha"}}}
	if err := idx.AddDocuments(ctx, docs); err != nil {
		t.Fatal(err)
	}
}

func TestQdrantIngestionValidatesWholeBatch(t *testing.T) {
	client, s := newFilterClient(t)
	idx, err := NewQdrantIndex(QdrantConfig{Client: client, Collection: "test", VectorMetaKey: "embedding"})
	if err != nil {
		t.Fatal(err)
	}
	valid := Document{ID: "00000000-0000-0000-0000-000000000001", Meta: map[string]any{"embedding": []float32{1, 2}, "content": "hello", "tenant": "alpha"}}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := idx.AddDocuments(ctx, []Document{valid}); err != nil {
		t.Fatal(err)
	}
	var req *qdrant.UpsertPoints
	select {
	case req = <-s.upserts:
	case <-ctx.Done():
		t.Fatal("missing upsert request:", ctx.Err())
	}
	p := req.Points[0]
	if _, ok := p.Payload["embedding"]; ok {
		t.Fatal("duplicated custom vector key")
	}
	if p.Payload["content"].GetStringValue() != "hello" || p.Payload["tenant"].GetStringValue() != "alpha" {
		t.Fatal("metadata lost")
	}
	if data := p.Vectors.GetVector().GetDense().GetData(); len(data) != 2 || data[0] != 1 || data[1] != 2 {
		t.Fatalf("native vector=%v", data)
	}
	for _, bad := range []map[string]any{nil, {"embedding": "invalid"}, {"embedding": []float32{}}, {"embedding": []float32{float32(math.NaN())}}, {"embedding": []float32{float32(math.Inf(1))}}, {"embedding": []float32{1}, "nested": map[string]any{"unsupported": make(chan int)}}} {
		if err := idx.AddDocuments(ctx, []Document{valid, {ID: "bad", Meta: bad}}); err == nil {
			t.Fatalf("bad metadata accepted: %v", bad)
		}
	}
	if len(s.upserts) != 0 {
		t.Fatal("invalid later document caused a partial upsert")
	}
}
