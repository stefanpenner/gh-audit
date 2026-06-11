package github

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A generic 403 whose one-shot retry hits a 429 throttle must cool the
// token down (secondary rate limit) instead of surfacing the raw 429 —
// mirrors the 429-path's handling of a follow-up 403.
func TestGeneric403RetryHitting429CoolsDownToken(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls == 1 {
			w.WriteHeader(403)
			fmt.Fprint(w, `{"message": "something transient"}`) // unclassifiable 403
			return
		}
		w.Header().Set("Retry-After", "7")
		w.WriteHeader(429)
		fmt.Fprint(w, `{"message": "you have exceeded a secondary rate limit"}`)
	}))
	defer srv.Close()

	token := &ManagedToken{ID: "test"}
	token.rateRemaining.Store(5000)
	transport := &rateLimitTransport{
		base:   http.DefaultTransport,
		token:  token,
		logger: testLogger(),
		sleep:  instantSleep,
	}

	req, _ := http.NewRequest("GET", srv.URL, nil)
	_, err := transport.doRoundTrip(req)
	require.Error(t, err)
	var secondary *SecondaryRateLimitError
	require.ErrorAs(t, err, &secondary, "429 on the 403-retry must classify as secondary rate limit")
	assert.Equal(t, 2, calls)
	assert.Greater(t, token.abuseCooldownUntil.Load(), int64(0), "token must enter cooldown")
}
