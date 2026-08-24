package web

import (
	"context"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

var (
	sseHeartbeatTick         = 5 * time.Second
	sseHeartbeatIdle         = 15 * time.Second
	sseHeartbeatWriteTimeout = 5 * time.Second
)

type sseResponseWriter struct {
	http.ResponseWriter
	mu         sync.Mutex
	flusher    http.Flusher
	started    chan struct{}
	stop       chan struct{}
	headerOnce sync.Once
	stopOnce   sync.Once
	lastWrite  atomic.Int64
}

func newSSEResponseWriter(w http.ResponseWriter) *sseResponseWriter {
	s := &sseResponseWriter{
		ResponseWriter: w,
		started:        make(chan struct{}),
		stop:           make(chan struct{}),
	}
	s.flusher, _ = w.(http.Flusher)
	s.lastWrite.Store(time.Now().UnixNano())
	return s
}

func (s *sseResponseWriter) Unwrap() http.ResponseWriter { return s.ResponseWriter }

func (s *sseResponseWriter) startIfSSELocked() {
	if !strings.HasPrefix(strings.ToLower(s.Header().Get("Content-Type")), "text/event-stream") {
		return
	}
	s.headerOnce.Do(func() {
		h := s.Header()
		h.Set("Cache-Control", "no-cache")
		h.Set("Connection", "keep-alive")
		h.Set("X-Accel-Buffering", "no")
		close(s.started)
	})
}

func (s *sseResponseWriter) WriteHeader(code int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.startIfSSELocked()
	s.ResponseWriter.WriteHeader(code)
}

func (s *sseResponseWriter) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.startIfSSELocked()
	n, err := s.ResponseWriter.Write(p)
	if n > 0 {
		s.lastWrite.Store(time.Now().UnixNano())
	}
	if s.flusher != nil {
		s.flusher.Flush()
	}
	return n, err
}

func (s *sseResponseWriter) Flush() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.startIfSSELocked()
	if s.flusher != nil {
		s.flusher.Flush()
	}
}

func (s *sseResponseWriter) heartbeat(ctx context.Context, cancel context.CancelFunc) {
	select {
	case <-s.started:
	case <-s.stop:
		return
	case <-ctx.Done():
		return
	}
	ticker := time.NewTicker(sseHeartbeatTick)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			last := time.Unix(0, s.lastWrite.Load())
			if time.Since(last) < sseHeartbeatIdle {
				continue
			}
			if err := s.writeHeartbeat(); err != nil {
				cancel()
				return
			}
		case <-s.stop:
			return
		case <-ctx.Done():
			return
		}
	}
}

func (s *sseResponseWriter) writeHeartbeat() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	controller := http.NewResponseController(s.ResponseWriter)
	_ = controller.SetWriteDeadline(time.Now().Add(sseHeartbeatWriteTimeout))
	n, err := s.ResponseWriter.Write([]byte(": keepalive\n\n"))
	_ = controller.SetWriteDeadline(time.Time{})
	if n > 0 {
		s.lastWrite.Store(time.Now().UnixNano())
	}
	if err != nil {
		return err
	}
	if s.flusher != nil {
		s.flusher.Flush()
	}
	return nil
}

func (s *sseResponseWriter) closeStop() {
	s.stopOnce.Do(func() { close(s.stop) })
}

func sseKeepaliveMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithCancel(r.Context())
		defer cancel()
		r = r.WithContext(ctx)

		sw := newSSEResponseWriter(w)
		done := make(chan struct{})
		go func() {
			defer close(done)
			sw.heartbeat(ctx, cancel)
		}()
		next.ServeHTTP(sw, r)
		sw.closeStop()
		<-done
	})
}
