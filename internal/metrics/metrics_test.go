package metrics

import (
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"
)

// TestManagedHostnamesSumsRoutes: each route's count replaces its last one, and 0 removes the route from the total.
func TestManagedHostnamesSumsRoutes(t *testing.T) {
	steps := []struct {
		route string
		n     int
		want  float64
	}{
		{"httproute/a/one", 2, 2},
		{"httproute/a/two", 3, 5},
		{"httproute/a/one", 1, 4},
		{"httproute/a/two", 0, 1},
		{"httproute/a/one", 0, 0},
	}
	for _, s := range steps {
		SetManagedHostnames(s.route, s.n)
		if got := testutil.ToFloat64(ManagedHostnames); got != s.want {
			t.Fatalf("after %s=%d: gauge = %v, want %v", s.route, s.n, got, s.want)
		}
	}
}
