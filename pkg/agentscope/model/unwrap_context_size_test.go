package model

import "testing"

// unwrapModel exposes an inner model through ChatModelUnwrapper and reports no
// window of its own.
type unwrapModel struct {
	*connMockModel
	inner ChatModel
}

func (m *unwrapModel) Unwrap() ChatModel { return m.inner }

func wrapped(inner ChatModel) *unwrapModel {
	return &unwrapModel{connMockModel: &connMockModel{name: "wrapper"}, inner: inner}
}

// sizedUnwrapModel is a wrapper that also reports its own window.
type sizedUnwrapModel struct {
	*unwrapModel
	contextSize int
}

func (m *sizedUnwrapModel) ContextSize() int { return m.contextSize }

// namedUnwrapModel is a wrapper that also reports its own model name.
type namedUnwrapModel struct {
	*unwrapModel
	modelName string
}

func (m *namedUnwrapModel) ModelName() string { return m.modelName }

func TestResolveContextSize_ChatModelUnwrapper(t *testing.T) {
	o3 := cardModel(t, "o3", 200000)
	if _, err := GetModelCard("no-such-model"); err == nil {
		t.Fatal("fixture name unexpectedly has a model card")
	}
	small := windowModel("small", 8192)
	tests := []struct {
		name     string
		model    ChatModel
		fallback int
		want     int
	}{
		{"inner window", wrapped(small), testContextFallback, 8192},
		{"inner window above caller fallback", wrapped(windowModel("large", 200000)), testContextFallback, 200000},
		{"inner card window", wrapped(o3), testContextFallback, 200000},
		{"inner reports no window", wrapped(windowModel("silent", 0)), testContextFallback, testContextFallback},
		{"inner reports no window, small caller fallback", wrapped(windowModel("silent", 0)), 4096, 4096},
		{"nil inner", wrapped(nil), testContextFallback, testContextFallback},
		{"own window wins", &sizedUnwrapModel{unwrapModel: wrapped(small), contextSize: 4096}, testContextFallback, 4096},
		{"own zero window falls through", &sizedUnwrapModel{unwrapModel: wrapped(small), contextSize: 0}, testContextFallback, 8192},
		{"own card wins", &namedUnwrapModel{unwrapModel: wrapped(small), modelName: "o3"}, testContextFallback, 200000},
		{"own name without card falls through", &namedUnwrapModel{unwrapModel: wrapped(small), modelName: "no-such-model"}, testContextFallback, 8192},
		{"nested wrappers", wrapped(wrapped(small)), testContextFallback, 8192},
		// With the caller's fallback passed on, the silent cloud model counts
		// as 128000 and the local 8192 window wins. Resolving the inner model
		// with 0 would make the minimum 0, which reads as unknown.
		{"inner connectivity-aware model", wrapped(NewConnectivityAwareModel(small, windowModel("cloud", 0))), testContextFallback, 8192},
		{"inner fallback chain", wrapped(NewFallbackChain(windowModel("large", 200000), small)), testContextFallback, 8192},
		{"wrapped chain member", NewFallbackChain(windowModel("large", 200000), wrapped(small)), testContextFallback, 8192},
		{"wrapped connectivity-aware member", NewConnectivityAwareModel(wrapped(small), windowModel("cloud", 200000)), testContextFallback, 8192},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ResolveContextSize(tt.model, tt.fallback); got != tt.want {
				t.Errorf("ResolveContextSize = %d, want %d", got, tt.want)
			}
		})
	}
}
