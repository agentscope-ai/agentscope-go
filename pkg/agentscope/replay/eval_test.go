package replay

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
)

func TestEvalTape_ExactMatch(t *testing.T) {
	expected := &Tape{
		Version: "1.0",
		Entries: []Entry{
			{Index: 0, Response: json.RawMessage(`{"text":"hello"}`)},
			{Index: 1, Response: json.RawMessage(`{"text":"world"}`)},
		},
	}
	recorded := &Tape{
		Version: "1.0",
		Entries: []Entry{
			{Index: 0, Response: json.RawMessage(`{"text":"hello"}`)},
			{Index: 1, Response: json.RawMessage(`{"text":"world"}`)},
		},
	}

	report, err := EvalTape(context.Background(), expected, recorded, ExactMatchScorer, 1.0)
	if err != nil {
		t.Fatal(err)
	}
	if report.Passed != 2 || report.Failed != 0 {
		t.Errorf("expected 2 passed, got passed=%d failed=%d", report.Passed, report.Failed)
	}
	if report.MeanScore != 1.0 {
		t.Errorf("expected mean 1.0, got %.3f", report.MeanScore)
	}
}

func TestEvalTape_Mismatch(t *testing.T) {
	expected := &Tape{
		Version: "1.0",
		Entries: []Entry{
			{Index: 0, Response: json.RawMessage(`{"text":"hello"}`)},
		},
	}
	recorded := &Tape{
		Version: "1.0",
		Entries: []Entry{
			{Index: 0, Response: json.RawMessage(`{"text":"goodbye"}`)},
		},
	}

	report, err := EvalTape(context.Background(), expected, recorded, ExactMatchScorer, 1.0)
	if err != nil {
		t.Fatal(err)
	}
	if report.Passed != 0 || report.Failed != 1 {
		t.Errorf("expected 0 passed 1 failed, got passed=%d failed=%d", report.Passed, report.Failed)
	}
}

func TestEvalTape_LengthMismatch(t *testing.T) {
	expected := &Tape{Version: "1.0", Entries: []Entry{{}, {}}}
	recorded := &Tape{Version: "1.0", Entries: []Entry{{}}}

	_, err := EvalTape(context.Background(), expected, recorded, ExactMatchScorer, 1.0)
	if err == nil {
		t.Fatal("expected error for entry count mismatch")
	}
}

func TestContainsScorer(t *testing.T) {
	s := ContainsScorer("revenue")
	score, err := s.Score(context.Background(), nil, json.RawMessage(`"The revenue grew by 15%."`))
	if err != nil {
		t.Fatal(err)
	}
	if score != 1.0 {
		t.Errorf("expected 1.0, got %.3f", score)
	}

	score, _ = s.Score(context.Background(), nil, json.RawMessage(`"No relevant data."`))
	if score != 0.0 {
		t.Errorf("expected 0.0, got %.3f", score)
	}
}

func TestJSONFieldScorer(t *testing.T) {
	s := JSONFieldScorer("status")
	score, _ := s.Score(context.Background(),
		json.RawMessage(`{"status":"ok","data":1}`),
		json.RawMessage(`{"status":"ok","data":99}`))
	if score != 1.0 {
		t.Errorf("expected 1.0 (field match), got %.3f", score)
	}

	score, _ = s.Score(context.Background(),
		json.RawMessage(`{"status":"ok"}`),
		json.RawMessage(`{"status":"fail"}`))
	if score != 0.0 {
		t.Errorf("expected 0.0 (field mismatch), got %.3f", score)
	}
}

func TestTextContainsScorer(t *testing.T) {
	s := TextContainsScorer("Go", "concurrency")
	score, _ := s.Score(context.Background(), nil, json.RawMessage(`"Go has great concurrency support."`))
	if score != 1.0 {
		t.Errorf("expected 1.0, got %.3f", score)
	}

	score, _ = s.Score(context.Background(), nil, json.RawMessage(`"Python is great for ML."`))
	if score != 0.0 {
		t.Errorf("expected 0.0, got %.3f", score)
	}
}

func TestCompositeScorer(t *testing.T) {
	s := CompositeScorer(ExactMatchScorer, ContainsScorer("hello"))
	score, _ := s.Score(context.Background(),
		json.RawMessage(`"hello world"`),
		json.RawMessage(`"hello world"`))
	if score != 1.0 {
		t.Errorf("expected 1.0, got %.3f", score)
	}

	// Exact mismatch (0) + contains match (1) = 0.5
	score, _ = s.Score(context.Background(),
		json.RawMessage(`"hello world"`),
		json.RawMessage(`"hello earth"`))
	if score != 0.5 {
		t.Errorf("expected 0.5, got %.3f", score)
	}
}

func TestEvalTape_AllErrors(t *testing.T) {
	// A scorer that always fails.
	failScorer := ScorerFunc(func(_ context.Context, _, _ json.RawMessage) (float64, error) {
		return 0, fmt.Errorf("scorer boom")
	})

	tape := &Tape{
		Version: "1.0",
		Entries: []Entry{
			{Index: 0, Response: json.RawMessage(`"a"`)},
			{Index: 1, Response: json.RawMessage(`"b"`)},
			{Index: 2, Response: json.RawMessage(`"c"`)},
		},
	}

	report, err := EvalTape(context.Background(), tape, tape, failScorer, 1.0)
	if err != nil {
		t.Fatal(err)
	}
	if report.Errors != 3 {
		t.Errorf("expected 3 errors, got %d", report.Errors)
	}
	// MeanScore should be 0 (not NaN or panic from div-by-zero).
	if report.MeanScore != 0 {
		t.Errorf("expected MeanScore 0, got %f", report.MeanScore)
	}
}

func TestEvalTape_ThresholdZero(t *testing.T) {
	expected := &Tape{
		Version: "1.0",
		Entries: []Entry{
			{Index: 0, Response: json.RawMessage(`"hello"`)},
		},
	}
	recorded := &Tape{
		Version: "1.0",
		Entries: []Entry{
			{Index: 0, Response: json.RawMessage(`"goodbye"`)},
		},
	}

	report, err := EvalTape(context.Background(), expected, recorded, ExactMatchScorer, 0.0)
	if err != nil {
		t.Fatal(err)
	}
	// Score 0.0 >= threshold 0.0, so should pass.
	if report.Passed != 1 {
		t.Errorf("expected 1 passed with threshold=0, got passed=%d failed=%d", report.Passed, report.Failed)
	}
}

func TestEvalTape_NilTape(t *testing.T) {
	tape := &Tape{Version: "1.0", Entries: []Entry{{Response: json.RawMessage(`"x"`)}}}
	_, err := EvalTape(context.Background(), nil, tape, ExactMatchScorer, 1.0)
	if err == nil {
		t.Fatal("expected error for nil expected tape")
	}
	_, err = EvalTape(context.Background(), tape, nil, ExactMatchScorer, 1.0)
	if err == nil {
		t.Fatal("expected error for nil recorded tape")
	}
}

func TestEvalTape_NilScorer(t *testing.T) {
	tape := &Tape{
		Version: "1.0",
		Entries: []Entry{{Response: json.RawMessage(`"x"`)}},
	}
	_, err := EvalTape(context.Background(), tape, tape, nil, 1.0)
	if err == nil {
		t.Fatal("expected error for nil scorer")
	}
}

func TestCompositeScorer_Empty(t *testing.T) {
	s := CompositeScorer() // zero scorers
	score, err := s.Score(context.Background(),
		json.RawMessage(`"a"`), json.RawMessage(`"b"`))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if score != 0 {
		t.Errorf("expected 0, got %.3f", score)
	}
}

func TestTextContainsScorer_Empty(t *testing.T) {
	s := TextContainsScorer() // zero substrings
	score, err := s.Score(context.Background(), nil, json.RawMessage(`"anything"`))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// No substrings to match → vacuously true → 1.0
	if score != 1.0 {
		t.Errorf("expected 1.0 (vacuous truth), got %.3f", score)
	}
}

func TestEvalTape_ScorerError(t *testing.T) {
	// Scorer that errors on odd indices.
	n := 0
	partialScorer := ScorerFunc(func(_ context.Context, _, _ json.RawMessage) (float64, error) {
		n++
		if n%2 == 0 {
			return 0, fmt.Errorf("boom on even call")
		}
		return 1.0, nil
	})

	tape := &Tape{
		Version: "1.0",
		Entries: []Entry{
			{Index: 0, Response: json.RawMessage(`"a"`)},
			{Index: 1, Response: json.RawMessage(`"b"`)},
			{Index: 2, Response: json.RawMessage(`"c"`)},
			{Index: 3, Response: json.RawMessage(`"d"`)},
		},
	}

	report, err := EvalTape(context.Background(), tape, tape, partialScorer, 1.0)
	if err != nil {
		t.Fatal(err)
	}
	// Calls: 1(ok) 2(err) 3(ok) 4(err) → 2 errors, 2 passed
	if report.Errors != 2 {
		t.Errorf("expected 2 errors, got %d", report.Errors)
	}
	if report.Passed != 2 {
		t.Errorf("expected 2 passed, got %d", report.Passed)
	}
}

func TestAssertTape_PassesOnMatch(t *testing.T) {
	tape := &Tape{
		Version: "1.0",
		Entries: []Entry{{Response: json.RawMessage(`"ok"`)}},
	}
	// AssertTape should not fail for a perfect match.
	// We can't easily test that it *doesn't* call t.Errorf, but we can
	// at least run it and confirm no panic.
	AssertTape(t, tape, tape, ExactMatchScorer, 1.0)
}
