package envtest

import (
	"slices"
	"testing"
	"time"

	"k8s.io/utils/ptr"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	"github.com/yuriy-kovalchuk/yk-dns-manager/internal/dns/memory"
)

// The tests in this file are regression tests for audit findings (S1, B2 from the 2026-10-12 audit; H1, H2 from the follow-up); each comment names the bug it regresses, not the behaviour that survives.

// bumpGeneration makes a spec change that does not touch hostnames, so the controller reconciles the route without anything it manages changing.
func (h *harness) bumpGeneration(name string) {
	h.t.Helper()
	h.updateRoute(name, func(r *gatewayv1.HTTPRoute) {
		r.Spec.Rules = append(r.Spec.Rules, gatewayv1.HTTPRouteRule{
			BackendRefs: []gatewayv1.HTTPBackendRef{{BackendRef: gatewayv1.BackendRef{
				BackendObjectReference: gatewayv1.BackendObjectReference{Name: "svc", Port: ptr.To(gatewayv1.PortNumber(80))},
			}}},
		})
	})
}

// waitReconciled waits until a reconcile has read the DNS backend since the last ResetCounts, which every reconcile of a managed route does.
func (h *harness) waitReconciled() {
	h.t.Helper()
	h.eventually("a reconcile reached the DNS backend", func() bool {
		return h.dns.Counts().Reads > 0
	})
}

// handMade is a record someone configured on the backend by hand, which the controller must never change or remove.
func handMade(fqdn, ip string) memory.Record {
	return memory.Record{FQDN: fqdn, Value: ip, Origin: memory.Foreign}
}

// TestAnnotationCannotDeleteForeignRecords: the ownership annotation is writable by anyone who can edit the route, and every name in it used to be deleted from the DNS backend, whether or not the domain map covers it.
func TestAnnotationCannotDeleteForeignRecords(t *testing.T) {
	tamper := func(r *gatewayv1.HTTPRoute) {
		r.Annotations[managedHostnamesAnnotation] = `["app.example.com","vpn.corp.example"]`
	}

	t.Run("on a spec change", func(t *testing.T) {
		h := start(t, harnessOptions{domainMap: exampleMap})
		h.dns.Seed(handMade("vpn.corp.example", "192.168.1.1"))
		h.createRoute("app", []string{"app.example.com"})
		h.waitOwned("app", `["app.example.com"]`)

		h.dns.ResetCounts()
		h.updateRoute("app", tamper)
		h.bumpGeneration("app")
		h.waitReconciled()

		h.consistently("vpn.corp.example is still configured", func() bool {
			return hasRecord(h.dns.Stored("vpn.corp.example"), "192.168.1.1")
		})
	})

	t.Run("on route deletion", func(t *testing.T) {
		h := start(t, harnessOptions{domainMap: exampleMap})
		h.dns.Seed(handMade("vpn.corp.example", "192.168.1.1"))
		h.createRoute("app", []string{"app.example.com"})
		h.waitOwned("app", `["app.example.com"]`)

		h.updateRoute("app", tamper)
		h.deleteRoute("app")
		h.waitRouteGone("app")

		if !hasRecord(h.dns.Stored("vpn.corp.example"), "192.168.1.1") {
			t.Fatal("deleting the route removed vpn.corp.example, which the domain map does not cover")
		}
	})
}

// TestUnmanagedRecordIsNotOverwritten: in upsert mode a route claiming a name with a hand-made override used to repoint that override at the gateway.
func TestUnmanagedRecordIsNotOverwritten(t *testing.T) {
	h := start(t, harnessOptions{domainMap: exampleMap, upsert: true})
	h.dns.Seed(handMade("nas.example.com", "10.0.0.50"))

	h.createRoute("nas", []string{"nas.example.com"})
	h.waitFinalizer("nas")

	h.consistently("nas.example.com still points at 10.0.0.50", func() bool {
		return hasRecord(h.dns.Stored("nas.example.com"), "10.0.0.50")
	})
}

// TestUnmanagedRecordSurvivesRouteDeletion: a hand-made override used to be recorded as owned all the same, so deleting the route used to delete it.
func TestUnmanagedRecordSurvivesRouteDeletion(t *testing.T) {
	h := start(t, harnessOptions{domainMap: exampleMap})
	h.dns.Seed(handMade("nas.example.com", "10.0.0.50"))

	h.createRoute("nas", []string{"nas.example.com"})
	h.waitFinalizer("nas")
	h.waitReconciled()

	h.deleteRoute("nas")
	h.waitRouteGone("nas")

	if !hasRecord(h.dns.Stored("nas.example.com"), "10.0.0.50") {
		t.Fatal("deleting the route removed the hand-made nas.example.com override")
	}
}

// TestSharedHostnameSurvivesOneRouteDeletion: two routes serving the same name each record it as owned, so deleting either used to remove the record the other still needs.
func TestSharedHostnameSurvivesOneRouteDeletion(t *testing.T) {
	h := start(t, harnessOptions{domainMap: exampleMap, upsert: true})
	h.createRoute("blue", []string{"app.example.com"})
	h.createRoute("green", []string{"app.example.com"})
	h.waitOwned("blue", `["app.example.com"]`)
	h.waitOwned("green", `["app.example.com"]`)

	h.deleteRoute("blue")
	h.waitRouteGone("blue")

	h.consistently("app.example.com is still published for green", func() bool {
		return hasRecord(h.dns.Live("app.example.com"), "10.0.0.1")
	})
}

// TestUnchangedRouteCausesNoWrites: upsert mode used to rewrite the override and reload Unbound on every reconcile, even when the record was already right.
func TestUnchangedRouteCausesNoWrites(t *testing.T) {
	h := start(t, harnessOptions{domainMap: exampleMap, upsert: true})
	h.createRoute("app", []string{"app.example.com"})
	h.waitOwned("app", `["app.example.com"]`)
	h.eventually("app.example.com published", func() bool {
		return hasRecord(h.dns.Live("app.example.com"), "10.0.0.1")
	})

	h.dns.ResetCounts()
	h.bumpGeneration("app")
	h.waitReconciled()

	h.consistently("no writes and no publish", func() bool {
		c := h.dns.Counts()
		return c.Writes == 0 && c.Publishes == 0
	})
}

// TestDisabledManagedRecordIsReenabled: a disabled override the controller created counted as existing, so in create mode the name never resolved again.
func TestDisabledManagedRecordIsReenabled(t *testing.T) {
	h := start(t, harnessOptions{domainMap: exampleMap})
	h.dns.Seed(memory.Record{FQDN: "app.example.com", Value: "10.0.0.1", Origin: memory.Managed, Disabled: true})

	h.createRoute("app", []string{"app.example.com"})

	h.eventually("app.example.com published and enabled", func() bool {
		return hasRecord(h.dns.Live("app.example.com"), "10.0.0.1")
	})
}

// TestDeleteIsPublishedAfterFailedPublish is audit B9: when publishing fails during route deletion the route is held until the deletion is published, instead of disappearing while its record keeps resolving.
func TestDeleteIsPublishedAfterFailedPublish(t *testing.T) {
	h := start(t, harnessOptions{domainMap: exampleMap, upsert: true})
	h.createRoute("app", []string{"app.example.com"})
	h.eventually("app.example.com published", func() bool {
		return hasRecord(h.dns.Live("app.example.com"), "10.0.0.1")
	})

	h.dns.FailPublish(errUnavailable)
	h.deleteRoute("app")
	h.consistently("the route is held while the deletion cannot be published", func() bool {
		return h.getRoute("app") != nil
	})
	h.dns.FailPublish(nil)

	h.waitRouteGone("app")
	if len(h.dns.Live("app.example.com")) != 0 {
		t.Fatal("the route is gone but its record is still published")
	}
}

// TestUnacceptedRouteGetsNoRecord is audit S3: with --require-accepted-routes a route no Gateway admitted gets no DNS, so a namespace the gateway owner locked out cannot claim names under the domain map.
func TestUnacceptedRouteGetsNoRecord(t *testing.T) {
	h := start(t, harnessOptions{domainMap: exampleMap, upsert: true, requireAccepted: true})
	h.createUnacceptedRoute("app", []string{"app.example.com"})

	h.consistently("no finalizer and no DNS writes", func() bool {
		r := h.getRoute("app")
		return r != nil && !slices.Contains(r.Finalizers, finalizerName) && h.dns.Counts().Writes == 0
	})
}

// TestWildcardRouteHostnameGetsNoRecord is audit H2: a route hostname of "*.example.com" matched the domain map's wildcard key and became a wildcard override, so one route answered for every name below the domain.
func TestWildcardRouteHostnameGetsNoRecord(t *testing.T) {
	h := start(t, harnessOptions{domainMap: exampleMap, upsert: true})
	h.createRoute("wild", []string{"*.example.com", "app.example.com"})
	h.waitOwned("wild", `["app.example.com"]`)

	h.eventually("app.example.com published", func() bool {
		return hasRecord(h.dns.Live("app.example.com"), "10.0.0.1")
	})
	h.consistently("no wildcard record exists", func() bool {
		return len(h.dns.Stored("*.example.com")) == 0
	})
}

// TestDriftOnTheBackendIsHealed is audit M1: the event filter used to drop informer resyncs, so a record an operator deleted on the DNS backend stayed deleted, even in upsert mode, until something in the cluster changed.
func TestDriftOnTheBackendIsHealed(t *testing.T) {
	h := start(t, harnessOptions{domainMap: exampleMap, upsert: true, syncPeriod: 500 * time.Millisecond})
	h.createRoute("app", []string{"app.example.com"})
	h.eventually("app.example.com published", func() bool {
		return hasRecord(h.dns.Live("app.example.com"), "10.0.0.1")
	})

	h.dns.Drop("app.example.com")

	h.eventually("a resync republishes it", func() bool {
		return hasRecord(h.dns.Live("app.example.com"), "10.0.0.1")
	})
}

// TestBurstOfRoutesCostsOnePublish is audit M4: every batch that changed something used to reconfigure Unbound, so applying a burst of routes restarted the resolver once per route.
func TestBurstOfRoutesCostsOnePublish(t *testing.T) {
	h := start(t, harnessOptions{domainMap: exampleMap, upsert: true, publishInterval: 5 * time.Second})
	var names []string
	for _, n := range []string{"a", "b", "c", "d", "e"} {
		h.createRoute(n, []string{n + ".example.com"})
		names = append(names, n+".example.com")
	}

	h.eventually("all five records published", func() bool {
		for _, fqdn := range names {
			if !hasRecord(h.dns.Live(fqdn), "10.0.0.1") {
				return false
			}
		}
		return true
	})
	if n := h.dns.Counts().Publishes; n > 2 {
		t.Errorf("%d publishes for five routes created at once, want at most 2", n)
	}
}

// TestAnnotationCannotDeleteAnotherRoutesRecord is audit H1: the ownership marker named no route, so a route whose annotation listed a hostname could delete the record another route wrote.
func TestAnnotationCannotDeleteAnotherRoutesRecord(t *testing.T) {
	h := start(t, harnessOptions{domainMap: exampleMap, upsert: true})
	h.createRoute("app", []string{"app.example.com"})
	h.waitOwned("app", `["app.example.com"]`)

	h.createRoute("squatter", []string{"squatter.example.com"})
	h.waitOwned("squatter", `["squatter.example.com"]`)

	h.updateRoute("squatter", func(r *gatewayv1.HTTPRoute) {
		r.Annotations[managedHostnamesAnnotation] = `["app.example.com","squatter.example.com"]`
	})
	h.deleteRoute("squatter")
	h.waitRouteGone("squatter")

	h.consistently("app.example.com is still published for app", func() bool {
		return hasRecord(h.dns.Live("app.example.com"), "10.0.0.1")
	})
}

// TestSharedHostnameIsReleasedByTheLastRoute: two routes serving one name are both owners of its record, so the last of them to go can still release it.
func TestSharedHostnameIsReleasedByTheLastRoute(t *testing.T) {
	h := start(t, harnessOptions{domainMap: exampleMap, upsert: true})
	h.createRoute("blue", []string{"app.example.com"})
	h.createRoute("green", []string{"app.example.com"})
	h.waitOwned("blue", `["app.example.com"]`)
	h.waitOwned("green", `["app.example.com"]`)

	h.deleteRoute("blue")
	h.waitRouteGone("blue")
	h.deleteRoute("green")
	h.waitRouteGone("green")

	if n := len(h.dns.Live("app.example.com")); n != 0 {
		t.Errorf("%d records left for app.example.com after both routes went", n)
	}
}

// TestRecordFromBeforeOwnersIsStillReleased: a record the controller wrote before owners were marked belongs to no route in particular, so the route serving it can still release it.
func TestRecordFromBeforeOwnersIsStillReleased(t *testing.T) {
	h := start(t, harnessOptions{domainMap: exampleMap, upsert: true})
	h.dns.Seed(memory.Record{FQDN: "app.example.com", Value: "10.0.0.1", Origin: memory.Managed})

	h.createRoute("app", []string{"app.example.com"})
	h.waitOwned("app", `["app.example.com"]`)
	h.deleteRoute("app")
	h.waitRouteGone("app")

	if n := len(h.dns.Stored("app.example.com")); n != 0 {
		t.Errorf("%d records left for app.example.com after the route went", n)
	}
}
