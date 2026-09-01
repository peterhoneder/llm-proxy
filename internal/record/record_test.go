package record

import (
	"net/http/httptest"
	"testing"
	"time"
)

func newTestRequest(t *testing.T) *Request {
	t.Helper()
	r := httptest.NewRequest("POST", "/v1/chat/completions", nil)
	return New("req-1", "conn-1", "vendor", "openai", r, time.Now())
}

// ClientLeftMidResponse is what stops a client's departure from rewriting a
// verdict it had no part in. The distinction it draws is causal, not temporal:
// a client is only implicated if the proxy still had bytes for it.
func TestClientLeftMidResponse(t *testing.T) {
	t.Parallel()

	now := time.Now()

	t.Run("gone with bytes still undelivered", func(t *testing.T) {
		t.Parallel()
		rec := newTestRequest(t)
		rec.AddRead(64, now)
		rec.AddDelivered(20, now)
		rec.SetClientGone(now)
		if !rec.ClientLeftMidResponse() {
			t.Error("a client that left with 44 bytes still owed to it interrupted the response")
		}
	})

	// The case that made the truncated-body verdict a coin toss: a client
	// without keep-alives closes the connection on every successful request,
	// and the watcher is still armed while the verdict is drawn.
	t.Run("gone having received everything", func(t *testing.T) {
		t.Parallel()
		rec := newTestRequest(t)
		rec.AddRead(64, now)
		rec.AddDelivered(64, now)
		rec.SetClientGone(now)
		if rec.ClientLeftMidResponse() {
			t.Error("a client that received every byte read from the upstream interrupted nothing")
		}
	})

	t.Run("still connected", func(t *testing.T) {
		t.Parallel()
		rec := newTestRequest(t)
		rec.AddRead(64, now)
		rec.AddDelivered(20, now)
		if rec.ClientLeftMidResponse() {
			t.Error("the client never went away")
		}
	})

	// A bodyless response reads and delivers nothing. Equal counts, so no
	// departure can be blamed on the response being unfinished.
	t.Run("gone with nothing to deliver", func(t *testing.T) {
		t.Parallel()
		rec := newTestRequest(t)
		rec.SetClientGone(now)
		if rec.ClientLeftMidResponse() {
			t.Error("with no bytes on either side there is nothing the client cut short")
		}
	})
}
