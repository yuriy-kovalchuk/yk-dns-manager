package envtest

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	"github.com/yuriy-kovalchuk/yk-dns-manager/internal/dns/memory"
)

var exampleMap = map[string]string{"*.example.com": "10.0.0.1"}

// errUnavailable is what a failing DNS backend answers.
var errUnavailable = errors.New("backend unavailable")

// TestCreatePublishesRecord covers the chain the unit tests cannot: the first reconcile only adds the finalizer, and the record appears because that update passes the event filter and triggers a second reconcile.
func TestCreatePublishesRecord(t *testing.T) {
	for _, upsert := range []bool{false, true} {
		t.Run(map[bool]string{false: "create mode", true: "upsert mode"}[upsert], func(t *testing.T) {
			h := start(t, harnessOptions{domainMap: exampleMap, upsert: upsert})

			h.createRoute("app", []string{"app.example.com"})

			h.waitFinalizer("app")
			h.eventually("app.example.com published", func() bool {
				return hasRecord(h.dns.Live("app.example.com"), "10.0.0.1")
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
		return hasRecord(h.dns.Live("web.example.com"), "10.0.0.1") && len(h.dns.Live("app.example.com")) == 0
	})
	h.waitOwned("app", `["web.example.com"]`)
}

func TestDeleteRemovesRecordAndReleasesRoute(t *testing.T) {
	h := start(t, harnessOptions{domainMap: exampleMap, upsert: true})

	h.createRoute("app", []string{"app.example.com"})
	h.eventually("app.example.com published", func() bool {
		return hasRecord(h.dns.Live("app.example.com"), "10.0.0.1")
	})

	h.deleteRoute("app")

	h.waitRouteGone("app")
	h.eventually("app.example.com unpublished", func() bool {
		return len(h.dns.Live("app.example.com")) == 0
	})
}

// TestUnmatchedRouteIsIgnored pins that the controller takes no ownership of a route it has nothing to manage: no finalizer, no DNS writes.
func TestUnmatchedRouteIsIgnored(t *testing.T) {
	h := start(t, harnessOptions{domainMap: exampleMap, upsert: true})

	h.createRoute("other", []string{"app.other.org"})

	h.consistently("no finalizer and no DNS writes", func() bool {
		r := h.getRoute("other")
		return r != nil && !slices.Contains(r.Finalizers, finalizerName) && h.dns.Counts().Writes == 0
	})
}

// TestRouteIsDeletableWhileBackendFails pins the "always deletable" guarantee: a failing DNS backend must never leave a route in Terminating, so cleanup is retried for the grace period and then the route goes.
func TestRouteIsDeletableWhileBackendFails(t *testing.T) {
	h := start(t, harnessOptions{domainMap: exampleMap, upsert: true, cleanupGrace: 2 * time.Second})

	h.createRoute("app", []string{"app.example.com"})
	h.waitOwned("app", `["app.example.com"]`)

	h.dns.FailPublish(errUnavailable)

	h.deleteRoute("app")
	h.waitRouteGone("app")
}

// TestRecordPublishedAfterFailedPublish: a failed publish makes the reconcile retry, and the retry finds the record staged, writes nothing, and still publishes it.
func TestRecordPublishedAfterFailedPublish(t *testing.T) {
	h := start(t, harnessOptions{domainMap: exampleMap, upsert: true})
	h.dns.FailPublish(errUnavailable)

	h.createRoute("app", []string{"app.example.com"})
	h.eventually("app.example.com staged", func() bool {
		return hasRecord(h.dns.Stored("app.example.com"), "10.0.0.1")
	})
	if len(h.dns.Live("app.example.com")) != 0 {
		t.Fatal("published while publishing was failing")
	}

	h.dns.FailPublish(nil)
	h.eventually("app.example.com published after recovery", func() bool {
		return hasRecord(h.dns.Live("app.example.com"), "10.0.0.1")
	})
}

// legacy is a record written by a controller version from before ownership was marked.
func legacy(fqdn, ip string) memory.Record {
	return memory.Record{FQDN: fqdn, Value: ip, Origin: memory.Legacy}
}

// TestLegacyRecordIsAdoptedByItsRoute covers the upgrade path: a route that recorded owning a name before ownership was marked adopts its record, rewritten with the marker and the route as owner.
func TestLegacyRecordIsAdoptedByItsRoute(t *testing.T) {
	h := start(t, harnessOptions{domainMap: exampleMap, upsert: true})
	h.dns.Seed(legacy("app.example.com", "10.0.0.1"))

	h.createRoute("app", []string{"app.example.com"})
	h.updateRoute("app", func(r *gatewayv1.HTTPRoute) {
		r.Finalizers = append(r.Finalizers, finalizerName)
		r.Annotations = map[string]string{managedHostnamesAnnotation: `["app.example.com"]`}
	})

	h.eventually("app.example.com adopted with the ownership marker", func() bool {
		r := h.dns.Live("app.example.com")
		return len(r) == 1 && r[0].Origin == memory.Managed && slices.Contains(r[0].Owners, "httproute/"+h.ns+"/app")
	})
	h.waitOwned("app", `["app.example.com"]`)
}

// TestLegacyRecordIsNotAdoptedByANewRoute: a route that never recorded owning a name cannot take over its legacy record; that is a conflict, and the name stays out of the annotation so no later reconcile adopts it.
func TestLegacyRecordIsNotAdoptedByANewRoute(t *testing.T) {
	h := start(t, harnessOptions{domainMap: exampleMap, upsert: true})
	h.dns.Seed(legacy("app.example.com", "10.0.0.50"))

	h.createRoute("app", []string{"app.example.com"})
	h.waitFinalizer("app")
	h.waitReconciled()

	h.consistently("the legacy record is untouched and not claimed", func() bool {
		r := h.dns.Stored("app.example.com")
		route := h.getRoute("app")
		return len(r) == 1 && r[0].Value == "10.0.0.50" && r[0].Origin == memory.Legacy &&
			route != nil && !strings.Contains(route.Annotations[managedHostnamesAnnotation], "app.example.com")
	})

	h.deleteRoute("app")
	h.waitRouteGone("app")
	if len(h.dns.Stored("app.example.com")) != 1 {
		t.Fatal("deleting the route removed a legacy record it never owned")
	}
}

// TestAcceptanceControlsRecord: with --require-accepted-routes, a route gets its record once a Gateway accepts it and loses it when acceptance goes.
func TestAcceptanceControlsRecord(t *testing.T) {
	h := start(t, harnessOptions{domainMap: exampleMap, upsert: true, requireAccepted: true})

	h.createRoute("app", []string{"app.example.com"})
	h.eventually("app.example.com published once accepted", func() bool {
		return hasRecord(h.dns.Live("app.example.com"), "10.0.0.1")
	})

	route := h.getRoute("app")
	cond := &route.Status.Parents[0].Conditions[0]
	cond.Status = metav1.ConditionFalse
	cond.Reason = string(gatewayv1.RouteReasonNotAllowedByListeners)
	if err := h.client.Status().Update(context.Background(), route); err != nil {
		t.Fatalf("marking the route not accepted: %v", err)
	}
	h.eventually("app.example.com released once no longer accepted", func() bool {
		return len(h.dns.Live("app.example.com")) == 0
	})
}
