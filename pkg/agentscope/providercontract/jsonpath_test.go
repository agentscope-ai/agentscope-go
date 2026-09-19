package providercontract

import "testing"

func TestJSONPathValue(t *testing.T) {
	body := []byte(`{"max_tokens":77,"generation_config":{"maxOutputTokens":42,"topP":0.5},"tools":[]}`)

	tests := []struct {
		name      string
		path      string
		wantFound bool
		want      any
	}{
		{"top level", "max_tokens", true, float64(77)},
		{"nested", "generation_config.maxOutputTokens", true, float64(42)},
		{"missing top level", "max_completion_tokens", false, nil},
		{"missing nested", "generation_config.maxTokens", false, nil},
		{"intermediate not an object", "max_tokens.value", false, nil},
		{"intermediate is an array", "tools.0", false, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, found, err := jsonPathValue(body, tt.path)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if found != tt.wantFound {
				t.Fatalf("found = %v, want %v", found, tt.wantFound)
			}
			if found && got != tt.want {
				t.Errorf("value = %v (%T), want %v", got, got, tt.want)
			}
		})
	}

	// A value that is numerically different must not satisfy the wall's
	// `== 77` comparison: the substring check this replaced accepted 770.
	got, found, err := jsonPathValue([]byte(`{"max_tokens":770}`), "max_tokens")
	if err != nil || !found {
		t.Fatalf("decode: found=%v err=%v", found, err)
	}
	if n, ok := got.(float64); !ok || n == 77 {
		t.Errorf("770 must not compare equal to 77, got %v", got)
	}

	if _, _, err := jsonPathValue([]byte(`not json`), "max_tokens"); err == nil {
		t.Error("expected an error for a non-JSON body")
	}
	if _, _, err := jsonPathValue([]byte(`[1,2,3]`), "max_tokens"); err == nil {
		t.Error("expected an error for a non-object JSON body")
	}
}
