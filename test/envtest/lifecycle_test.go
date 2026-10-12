package envtest

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	"github.com/yuriy-kovalchuk/yk-dns-manager/internal/dns/opnsense"
	"github.com/yuriy-kovalchuk/yk-dns-manager/internal/testenv"
)

const exampleMap = "\"*.example.com\": 10.0.0.1\n"

// TestCreatePublishesRecord covers the chain the unit tests cannot: the first
// reconcile only adds the finalizer, and the record appears only because the
// finalizer update passes the event filter and triggers a second reconcile.
func TestCreatePublishesRecord(t *testing.T) {
	for _, upsert := range []bool{false, true} {
		t.Run(map[bool]string{false: "create mode", true: "upsert mode"}[upsert], func(t *testing.T) {
			h := start(t, harnessOptions{domainMap: exampleMap, upsert: upsert})

			h.createRoute("app", []string{"app.example.com"})

			h.waitFinalizer("app")
			h.eventually("app.example.com published", func() bool {
				return hasRow(h.fake.Published(), "app.example.com", "10.0.0.1")
			})
			h.waitOwned("app", `["app.example.com"]`)
		})
	}
}

func TestHostnameChangeMovesRecord(t *testing.T) {
	h := start(t, harnessOptions{domainMap: exampleMap, upsert: true})

	h.createRoute("app", []string{"app.example.com"})
	h.waitOwned("app", `["app.example.com"]`)

	h.updateRoute("app", func(r *gatewayv1.HTTPRoute) {
		r.Spec.Hostnames = []gatewayv1.Hostname{"web.example.com"}
	})

	h.eventually("record moved from app to web", func() bool {
		published := h.fake.Published()
		return hasRow(published, "web.example.com", "10.0.0.1") && len(rows(published, "app.example.com")) == 0
	})
	h.waitOwned("app", `["web.example.com"]`)
}

func TestDeleteRemovesRecordAndReleasesRoute(t *testing.T) {
	h := start(t, harnessOptions{domainMap: exampleMap, upsert: true})

	h.createRoute("app", []string{"app.example.com"})
	h.eventually("app.example.com published", func() bool {
		return hasRow(h.fake.Published(), "app.example.com", "10.0.0.1")
	})

	h.deleteRoute("app")

	h.waitRouteGone("app")
	h.eventually("app.example.com unpublished", func() bool {
		return len(rows(h.fake.Published(), "app.example.com")) == 0
	})
}

// TestUnmatchedRouteIsIgnored pins that the controller does not take ownership
// of a route it has nothing to manage: no finalizer, no appliance writes.
func TestUnmatchedRouteIsIgnored(t *testing.T) {
	h := start(t, harnessOptions{domainMap: exampleMap, upsert: true})

	h.createRoute("other", []string{"app.other.org"})

	h.consistently("no finalizer and no appliance writes", func() bool {
		r := h.getRoute("other")
		return r != nil && !hasFinalizer(r) && h.countCalls("addHostOverride") == 0
	})
}

// TestRouteIsDeletableWhileApplianceFails pins the "always deletable"
// guarantee: a failing appliance must never leave a route stuck in
// Terminating. Cleanup is retried for the grace period, then the route goes.
func TestRouteIsDeletableWhileApplianceFails(t *testing.T) {
	h := start(t, harnessOptions{domainMap: exampleMap, upsert: true, cleanupGrace: 2 * time.Second})

	h.createRoute("app", []string{"app.example.com"})
	h.waitOwned("app", `["app.example.com"]`)

	h.fake.SetFailure(testenv.EndpointSearch, http.StatusServiceUnavailable)
	h.fake.SetFailure(testenv.EndpointDel, http.StatusServiceUnavailable)
	h.fake.SetFailure(testenv.EndpointReconfigure, http.StatusServiceUnavailable)

	h.deleteRoute("app")
	h.waitRouteGone("app")
}

// TestRecordPublishedAfterReconfigureRecovers: a failed publish makes the
// reconcile fail and retry. The retry finds the override already staged and
// writes nothing, but must still publish it.
func TestRecordPublishedAfterReconfigureRecovers(t *testing.T) {
	h := start(t, harnessOptions{domainMap: exampleMap, upsert: true})
	h.fake.SetFailure(testenv.EndpointReconfigure, http.StatusServiceUnavailable)

	h.createRoute("app", []string{"app.example.com"})
	h.eventually("app.example.com staged", func() bool {
		return hasRow(h.fake.Snapshot(), "app.example.com", "10.0.0.1")
	})
	if len(rows(h.fake.Published(), "app.example.com")) != 0 {
		t.Fatal("published while reconfigure was failing")
	}

	h.fake.SetFailure(testenv.EndpointReconfigure, 0)
	h.eventually("app.example.com published after recovery", func() bool {
		return hasRow(h.fake.Published(), "app.example.com", "10.0.0.1")
	})
}

// legacy is an override written by a controller version from before
// ownership was marked.
func legacy(hostname, domain, ip string) testenv.HostOverride {
	return testenv.HostOverride{
		Enabled: "1", Hostname: hostname, Domain: domain, RR: "A", Server: ip,
		Description: opnsense.LegacyDescription,
	}
}

// TestLegacyRecordIsAdoptedByItsRoute covers the upgrade path: a route that
// recorded owning a name before ownership was marked adopts its record, which
// is rewritten with the ownership marker.
func TestLegacyRecordIsAdoptedByItsRoute(t *testing.T) {
	h := start(t, harnessOptions{domainMap: exampleMap, upsert: true})
	h.fake.Seed(legacy("app", "example.com", "10.0.0.1"))

	h.createRoute("app", []string{"app.example.com"})
	h.updateRoute("app", func(r *gatewayv1.HTTPRoute) {
		r.Finalizers = append(r.Finalizers, finalizerName)
		r.Annotations = map[string]string{managedHostnamesAnnotation: `["app.example.com"]`}
	})

	h.eventually("app.example.com adopted with the ownership marker", func() bool {
		r := rows(h.fake.Published(), "app.example.com")
		return len(r) == 1 && r[0].Description == opnsense.ManagedDescription
	})
	h.waitOwned("app", `["app.example.com"]`)
}

// TestLegacyRecordIsNotAdoptedByANewRoute: a route that never recorded owning
// a name cannot take over a legacy record for it. It is a conflict, and the
// name stays out of the route's annotation so no later reconcile adopts it.
func TestLegacyRecordIsNotAdoptedByANewRoute(t *testing.T) {
	h := start(t, harnessOptions{domainMap: exampleMap, upsert: true})
	h.fake.Seed(legacy("app", "example.com", "10.0.0.50"))

	h.createRoute("app", []string{"app.example.com"})
	h.waitFinalizer("app")
	h.waitReconciled()

	h.consistently("the legacy record is untouched and not claimed", func() bool {
		r := rows(h.fake.Snapshot(), "app.example.com")
		route := h.getRoute("app")
		return len(r) == 1 && r[0].Server == "10.0.0.50" && r[0].Description == opnsense.LegacyDescription &&
			route != nil && !strings.Contains(route.Annotations[managedHostnamesAnnotation], "app.example.com")
	})

	h.deleteRoute("app")
	h.waitRouteGone("app")
	if len(rows(h.fake.Snapshot(), "app.example.com")) != 1 {
		t.Fatal("deleting the route removed a legacy record it never owned")
	}
}

// TestAcceptanceControlsRecord: with --require-accepted-routes, a route gets
// its record once a Gateway accepts it, and loses it when acceptance goes.
func TestAcceptanceControlsRecord(t *testing.T) {
	h := start(t, harnessOptions{domainMap: exampleMap, upsert: true, requireAccepted: true})

	h.createRoute("app", []string{"app.example.com"})
	h.eventually("app.example.com published once accepted", func() bool {
		return hasRow(h.fake.Published(), "app.example.com", "10.0.0.1")
	})

	route := h.getRoute("app")
	cond := &route.Status.Parents[0].Conditions[0]
	cond.Status = metav1.ConditionFalse
	cond.Reason = string(gatewayv1.RouteReasonNotAllowedByListeners)
	if err := h.client.Status().Update(context.Background(), route); err != nil {
		t.Fatalf("marking the route not accepted: %v", err)
	}
	h.eventually("app.example.com released once no longer accepted", func() bool {
		return len(rows(h.fake.Published(), "app.example.com")) == 0
	})
}
