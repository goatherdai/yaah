package pipeline

import "testing"

func TestPipeline_GoatherdTraceMiddleware_NilWhenNotInPipeline(t *testing.T) {
	p := NewPipeline(&SteerMiddleware{}, &LoopDetectionMiddleware{count: 3, window: 5})
	if mw := p.GoatherdTraceMiddleware(); mw != nil {
		t.Error("should return nil when goatherd_trace is not in pipeline")
	}
}

func TestPipeline_GoatherdTraceMiddleware_NoopNotReturned(t *testing.T) {
	p := NewPipeline(&noopGoatherdTraceMiddleware{})
	if mw := p.GoatherdTraceMiddleware(); mw != nil {
		t.Error("should return nil for noop (not a real *GoatherdTraceMiddleware)")
	}
}
