package metrics

import (
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"
)

// TestManagedRecordsReplacesARoutesSeries: a route's set replaces all its series, other routes keep theirs, and nil forgets the route.
func TestManagedRecordsReplacesARoutesSeries(t *testing.T) {
	ManagedRecords.Reset()
	SetManagedRecords("web", "blue", map[string]string{"a.example.com": "10.0.0.1", "b.example.com": "10.0.0.1"})
	SetManagedRecords("web", "green", map[string]string{"a.example.com": "10.0.0.1"})
	SetManagedRecords("web", "blue", map[string]string{"c.example.com": "10.0.0.2"})

	if n := testutil.CollectAndCount(ManagedRecords); n != 2 {
		t.Fatalf("%d series, want blue's c and green's a", n)
	}
	if v := testutil.ToFloat64(ManagedRecords.WithLabelValues("web", "blue", "c.example.com", "10.0.0.2")); v != 1 {
		t.Errorf("blue's new record = %v, want 1", v)
	}

	SetManagedRecords("web", "green", nil)
	SetManagedRecords("web", "blue", nil)
	if n := testutil.CollectAndCount(ManagedRecords); n != 0 {
		t.Errorf("%d series left after both routes were forgotten", n)
	}
}
