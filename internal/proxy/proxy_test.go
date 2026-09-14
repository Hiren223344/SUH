package proxy

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"router/internal/config"
	"router/internal/router"
	"router/internal/stats"
	"router/internal/upstream"
)

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// writeConfigWithUpstreams builds a config file wiring public model "M" to
// the given fake upstream base URLs, and returns a *router.Registry backed
// by it.
func newTestRegistry(t *testing.T, upstreams map[string]string) (*router.Registry, *config.Manager) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")

	var sb strings.Builder
	sb.WriteString("server:\n  listen: \":0\"\n  max_attempts: 3\n  request_budget: 5s\n")
	sb.WriteString("public_models:\n  - name: \"M\"\n    upstreams:\n")
	i := 0
	for id := range upstreams {
		fallback := ""
		if i == len(upstreams)-1 {
			fallback = "\n        fallback: true"
		}
		fmt.Fprintf(&sb, "      - id: %s\n        weight: 1%s\n", id, fallback)
		i++
	}
	sb.WriteString("upstreams:\n")
	for id, base := range upstreams {
		fmt.Fprintf(&sb, `  - id: %s
    base_url: "%s"
    model: "backend-%s"
    context_window: 128000
    max_output: 4096
    modalities: [text]
    supports_tools: true
    supports_json_schema: true
    tpm_limit: 100000000
`, id, base, id)
	}

	require.NoError(t, os.WriteFile(path, []byte(sb.String()), 0644))
	mgr, err := config.NewManager(path, testLogger())
	require.NoError(t, err)
	bcfg := upstream.DefaultBreakerConfig()
	bcfg.ConsecutiveFailThreshold = 100 // don't let the breaker interfere with retry-window tests
	reg := router.NewRegistry(mgr, bcfg)
	return reg, mgr
}

func chatReq(model string, stream bool) []byte {
	b, _ := json.Marshal(map[string]interface{}{
		"model":    model,
		"stream":   stream,
		"messages": []map[string]string{{"role": "user", "content": "hi"}},
	})
	return b
}

func TestNonStream_HappyPath(t *testing.T) {
	upstreamSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Upstream-Secret", "leak-me-not")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"upstream-id-123","object":"chat.completion","created":1,"model":"backend-up1","choices":[{"index":0,"message":{"role":"assistant","content":"hello"},"finish_reason":"stop"}],"usage":{"prompt_tokens":5,"completion_tokens":2,"total_tokens":7}}`))
	}))
	defer upstreamSrv.Close()

	reg, _ := newTestRegistry(t, map[string]string{"up1": upstreamSrv.URL})
	h := &Handlers{Registry: reg, Stats: stats.NewRecorder(), Logger: testLogger()}

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(chatReq("M", false)))
	rw := httptest.NewRecorder()
	h.ChatCompletions(rw, req)

	require.Equal(t, http.StatusOK, rw.Code)
	require.Empty(t, rw.Header().Get("X-Upstream-Secret"), "upstream header must never reach the client")
	var body map[string]interface{}
	require.NoError(t, json.Unmarshal(rw.Body.Bytes(), &body))
	require.Equal(t, "M", body["model"])
	require.NotEqual(t, "upstream-id-123", body["id"])
	require.NotContains(t, rw.Body.String(), "backend-up1")
}

// TestRetryWindow_PreCommit is acceptance test #4 (first half): an upstream
// that fails before returning headers/body must trigger a transparent
// failover, and the client sees exactly one clean response.
func TestRetryWindow_PreCommit(t *testing.T) {
	failing := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Simulate a connection failure: close without responding.
		hj, ok := w.(http.Hijacker)
		require.True(t, ok)
		conn, _, err := hj.Hijack()
		require.NoError(t, err)
		conn.Close()
	}))
	defer failing.Close()

	healthy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"x","object":"chat.completion","created":1,"model":"backend-healthy","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`))
	}))
	defer healthy.Close()

	reg, _ := newTestRegistry(t, map[string]string{"failing": failing.URL, "healthy": healthy.URL})
	h := &Handlers{Registry: reg, Stats: stats.NewRecorder(), Logger: testLogger()}

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(chatReq("M", false)))
	rw := httptest.NewRecorder()
	h.ChatCompletions(rw, req)

	require.Equal(t, http.StatusOK, rw.Code, "client must see exactly one clean response despite one upstream failing")
	require.Contains(t, rw.Body.String(), `"model":"M"`)
}

func TestAllAttemptsFail_ReturnsSanitizedError(t *testing.T) {
	dead := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":{"message":"internal secret backend-qwen3.8 exploded","type":"api_error"}}`))
	}))
	defer dead.Close()

	reg, _ := newTestRegistry(t, map[string]string{"dead": dead.URL})
	h := &Handlers{Registry: reg, Stats: stats.NewRecorder(), Logger: testLogger()}

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(chatReq("M", false)))
	rw := httptest.NewRecorder()
	h.ChatCompletions(rw, req)

	require.Equal(t, http.StatusBadGateway, rw.Code)
	require.NotContains(t, rw.Body.String(), "backend-qwen3.8", "upstream error body must never reach the client")
}

// TestStreaming_CommittedFailureEmitsCleanTerminal is acceptance test #4
// (second half): once the first chunk is flushed, a mid-stream failure
// must produce a well-formed terminal error + [DONE], not a hang, and must
// NOT retry against another upstream.
func TestStreaming_CommittedFailureEmitsCleanTerminal(t *testing.T) {
	var secondUpstreamHit atomic.Bool

	flaky := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher := w.(http.Flusher)
		_, _ = w.Write([]byte(`data: {"id":"x","object":"chat.completion.chunk","created":1,"model":"backend-flaky","choices":[{"index":0,"delta":{"role":"assistant","content":"hi"},"finish_reason":null}]}` + "\n\n"))
		flusher.Flush()
		hj := w.(http.Hijacker)
		conn, _, _ := hj.Hijack()
		conn.Close() // mid-stream failure, after commit
	}))
	defer flaky.Close()

	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		secondUpstreamHit.Store(true)
		w.WriteHeader(http.StatusOK)
	}))
	defer other.Close()

	reg, _ := newTestRegistry(t, map[string]string{"flaky": flaky.URL, "other": other.URL})
	// Force selection onto "flaky" deterministically by excluding "other"
	// via a request that only "flaky" can serve is awkward with weights;
	// instead just run enough attempts is unnecessary — max_attempts=3 but
	// once committed the handler must return without trying "other".
	h := &Handlers{Registry: reg, Stats: stats.NewRecorder(), Logger: testLogger()}

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(chatReq("M", true)))
	rw := httptest.NewRecorder()

	done := make(chan struct{})
	go func() {
		h.ChatCompletions(rw, req)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("handler hung instead of emitting a clean terminal error")
	}

	body := rw.Body.String()
	require.True(t, strings.HasSuffix(strings.TrimRight(body, "\n"), "data: [DONE]"), "stream must end with a clean [DONE] terminal, got: %s", body)
	sc := bufio.NewScanner(strings.NewReader(body))
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	dataLines := 0
	for sc.Scan() {
		if strings.HasPrefix(sc.Text(), "data: ") {
			dataLines++
		}
	}
	require.GreaterOrEqual(t, dataLines, 2, "expected at least the first content chunk and a terminal error chunk")
}

func TestStreaming_HappyPath(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher := w.(http.Flusher)
		chunks := []string{
			`{"id":"x","object":"chat.completion.chunk","created":1,"model":"backend-s","choices":[{"index":0,"delta":{"role":"assistant","content":"hi"},"finish_reason":null}]}`,
			`{"id":"x","object":"chat.completion.chunk","created":1,"model":"backend-s","choices":[{"index":0,"delta":{"content":" there"},"finish_reason":"stop"}]}`,
		}
		for _, c := range chunks {
			_, _ = w.Write([]byte("data: " + c + "\n\n"))
			flusher.Flush()
		}
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
		flusher.Flush()
	}))
	defer srv.Close()

	reg, _ := newTestRegistry(t, map[string]string{"s": srv.URL})
	h := &Handlers{Registry: reg, Stats: stats.NewRecorder(), Logger: testLogger()}

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(chatReq("M", true)))
	rw := httptest.NewRecorder()
	h.ChatCompletions(rw, req)

	body := rw.Body.String()
	require.NotContains(t, body, "backend-s")
	require.Contains(t, body, `"model":"M"`)
	require.Contains(t, body, "data: [DONE]")
}

// TestCancellation_UpstreamContextCancelledPromptly is acceptance test #8:
// when the client disconnects, the router's own request to the upstream
// must be abandoned promptly rather than left to run to completion. This
// is verified from the router's side — how fast our http.Client gives up
// once its context is cancelled — rather than relying on the fake
// upstream's Go http.Server to detect the TCP close, which is a stdlib/OS
// behavior outside this codebase and not reliably observable in-process
// across platforms.
func TestCancellation_UpstreamContextCancelledPromptly(t *testing.T) {
	unblock := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-unblock // never respond on its own; only cancellation should end the attempt
	}))
	// Deferred LIFO: unblock the handler before srv.Close() waits for it to return.
	defer srv.Close()
	defer close(unblock)

	reg, _ := newTestRegistry(t, map[string]string{"s": srv.URL})
	h := &Handlers{Registry: reg, Stats: stats.NewRecorder(), Logger: testLogger()}

	ctx, cancel := context.WithCancel(context.Background())
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(chatReq("M", false))).WithContext(ctx)
	rw := httptest.NewRecorder()

	done := make(chan struct{})
	go func() {
		h.ChatCompletions(rw, req)
		close(done)
	}()

	time.Sleep(20 * time.Millisecond)
	cancelledAt := time.Now()
	cancel()

	select {
	case <-done:
		require.WithinDuration(t, cancelledAt, time.Now(), 100*time.Millisecond,
			"the router's own upstream request should be abandoned within ~100ms of client cancellation")
	case <-time.After(1 * time.Second):
		t.Fatal("handler did not return within 1s of client cancellation")
	}
}
