package metrics

import (
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// idSegment matches path segments that name one object (a UUID, a number or a long hex id), which must not become label values.
var idSegment = regexp.MustCompile(`^([0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}|[0-9]+|[0-9a-fA-F]{16,})$`)

// InstrumentTransport returns a wrapper that counts and times every request a provider sends, so the provider itself carries no metrics code.
func InstrumentTransport(provider string) func(http.RoundTripper) http.RoundTripper {
	return func(next http.RoundTripper) http.RoundTripper {
		return roundTripFunc(func(req *http.Request) (*http.Response, error) {
			endpoint := endpointOf(req.URL.Path)
			start := time.Now()
			resp, err := next.RoundTrip(req)
			ProviderRequestDuration.WithLabelValues(provider, endpoint).Observe(time.Since(start).Seconds())
			code := "error"
			if err == nil {
				code = strconv.Itoa(resp.StatusCode)
			}
			ProviderRequests.WithLabelValues(provider, endpoint, code).Inc()
			return resp, err
		})
	}
}

// endpointOf names a request by the last path segment that is not an object id: ".../setHostOverride/<uuid>" is setHostOverride.
func endpointOf(path string) string {
	segments := strings.Split(strings.Trim(path, "/"), "/")
	for i := len(segments) - 1; i >= 0; i-- {
		if segments[i] != "" && !idSegment.MatchString(segments[i]) {
			return segments[i]
		}
	}
	return "/"
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }
