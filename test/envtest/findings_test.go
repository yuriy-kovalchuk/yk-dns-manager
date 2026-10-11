package envtest

import (
	"net/http"
	"os"
	"testing"

	"k8s.io/utils/ptr"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	"github.com/yuriy-kovalchuk/yk-dns-manager/internal/testenv"
)

// The tests in this file pin findings from the 2026-10-12 audit that are
// not fixed yet. Each one fails on the current code and is skipped through
// pending; the change that fixes a finding deletes its pending call.

// pending skips a test that pins behaviour from the audit
// (2026-10-12) that is not fixed yet. Each one fails on the
// current code. The fix removes the call; ENVTEST_RUN_PENDING=1 runs them
// anyway, to watch them fail or to check a fix in progress.
func pending(t *testing.T, finding string) {
	t.Helper()
	if os.Getenv("ENVTEST_RUN_PENDING") != "1" {
		t.Skipf("pending: %s (set ENVTEST_RUN_PENDING=1 to run)", finding)
	}
}

// unaccepted leaves the route without an Accepted parent, as if no Gateway
// admitted it.
func unaccepted() routeOption { return func(s *routeSpec) { s.unaccepted = true } }

// bumpGeneration makes a spec change that does not touch hostnames, so the
// controller reconciles the route without anything it manages changing.
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

// waitReconciled waits until the controller has asked the appliance for its
// overrides since the last ResetCalls, which every reconcile of a managed route
// does.
func (h *harness) waitReconciled() {
	h.t.Helper()
	h.eventually("a reconcile reached the appliance", func() bool {
		return h.countCalls(testenv.EndpointSearch) > 0
	})
}

// handMade is an override someone configured on the appliance by hand, which
// the controller must never change or remove.
func handMade(hostname, domain, ip string) testenv.HostOverride {
	return testenv.HostOverride{
		Enabled: "1", Hostname: hostname, Domain: domain, RR: "A", Server: ip,
		Description: "configured by hand",
	}
}

// TestAnnotationCannotDeleteForeignRecords: the ownership annotation is
// writable by anyone who can edit the route, and every name in it is deleted
// from the appliance, whether or not the domain map covers it.
func TestAnnotationCannotDeleteForeignRecords(t *testing.T) {
	pending(t, "audit S1, roadmap phase 0")

	tamper := func(r *gatewayv1.HTTPRoute) {
		r.Annotations[managedHostnamesAnnotation] = `["app.example.com","vpn.corp.example"]`
	}

	t.Run("on a spec change", func(t *testing.T) {
		h := start(t, harnessOptions{domainMap: exampleMap})
		h.fake.Seed(handMade("vpn", "corp.example", "192.168.1.1"))
		h.createRoute("app", []string{"app.example.com"})
		h.waitOwned("app", `["app.example.com"]`)

		h.fake.ResetCalls()
		h.updateRoute("app", tamper)
		h.bumpGeneration("app")
		h.waitReconciled()

		h.consistently("vpn.corp.example is still configured", func() bool {
			return hasRow(h.fake.Snapshot(), "vpn.corp.example", "192.168.1.1")
		})
	})

	t.Run("on route deletion", func(t *testing.T) {
		h := start(t, harnessOptions{domainMap: exampleMap})
		h.fake.Seed(handMade("vpn", "corp.example", "192.168.1.1"))
		h.createRoute("app", []string{"app.example.com"})
		h.waitOwned("app", `["app.example.com"]`)

		h.updateRoute("app", tamper)
		h.deleteRoute("app")
		h.waitRouteGone("app")

		if !hasRow(h.fake.Snapshot(), "vpn.corp.example", "192.168.1.1") {
			t.Fatal("deleting the route removed vpn.corp.example, which the domain map does not cover")
		}
	})
}

// TestUnmanagedRecordIsNotOverwritten: in upsert mode a route claiming a name
// that already has a hand-made override repoints it at the gateway.
func TestUnmanagedRecordIsNotOverwritten(t *testing.T) {
	pending(t, "audit S2, roadmap phase 3")

	h := start(t, harnessOptions{domainMap: exampleMap, upsert: true})
	h.fake.Seed(handMade("nas", "example.com", "10.0.0.50"))

	h.createRoute("nas", []string{"nas.example.com"})
	h.waitFinalizer("nas")

	h.consistently("nas.example.com still points at 10.0.0.50", func() bool {
		return hasRow(h.fake.Snapshot(), "nas.example.com", "10.0.0.50")
	})
}

// TestUnmanagedRecordSurvivesRouteDeletion: in create mode the existing
// override is left alone, but the name is still recorded as owned, so deleting
// the route deletes the hand-made override.
func TestUnmanagedRecordSurvivesRouteDeletion(t *testing.T) {
	pending(t, "audit S2, roadmap phase 3")

	h := start(t, harnessOptions{domainMap: exampleMap})
	h.fake.Seed(handMade("nas", "example.com", "10.0.0.50"))

	h.createRoute("nas", []string{"nas.example.com"})
	h.waitFinalizer("nas")
	h.waitReconciled()

	h.deleteRoute("nas")
	h.waitRouteGone("nas")

	if !hasRow(h.fake.Snapshot(), "nas.example.com", "10.0.0.50") {
		t.Fatal("deleting the route removed the hand-made nas.example.com override")
	}
}

// TestSharedHostnameSurvivesOneRouteDeletion: two routes serving the same name
// each record it as owned, and deleting either removes the record the other
// still needs.
func TestSharedHostnameSurvivesOneRouteDeletion(t *testing.T) {
	pending(t, "audit B1, roadmap phase 3")

	h := start(t, harnessOptions{domainMap: exampleMap, upsert: true})
	h.createRoute("blue", []string{"app.example.com"})
	h.createRoute("green", []string{"app.example.com"})
	h.waitOwned("blue", `["app.example.com"]`)
	h.waitOwned("green", `["app.example.com"]`)

	h.deleteRoute("blue")
	h.waitRouteGone("blue")

	h.consistently("app.example.com is still published for green", func() bool {
		return hasRow(h.fake.Published(), "app.example.com", "10.0.0.1")
	})
}

// TestUnchangedRouteCausesNoWrites: upsert mode rewrites the override and
// reloads Unbound on every reconcile, even when the record is already right.
func TestUnchangedRouteCausesNoWrites(t *testing.T) {
	pending(t, "audit B2, roadmap phase 2")

	h := start(t, harnessOptions{domainMap: exampleMap, upsert: true})
	h.createRoute("app", []string{"app.example.com"})
	h.waitOwned("app", `["app.example.com"]`)
	h.eventually("app.example.com published", func() bool {
		return hasRow(h.fake.Published(), "app.example.com", "10.0.0.1")
	})

	h.fake.ResetCalls()
	h.bumpGeneration("app")
	h.waitReconciled()

	h.consistently("no writes and no reconfigure", func() bool {
		return h.countCalls(testenv.EndpointAdd) == 0 &&
			h.countCalls(testenv.EndpointSet) == 0 &&
			h.countCalls(testenv.EndpointReconfigure) == 0
	})
}

// TestDisabledManagedRecordIsReenabled: a disabled override the controller
// created counts as existing, so in create mode the name never resolves again.
func TestDisabledManagedRecordIsReenabled(t *testing.T) {
	pending(t, "audit B8, roadmap phase 2")

	h := start(t, harnessOptions{domainMap: exampleMap})
	h.fake.Seed(testenv.HostOverride{
		Enabled: "0", Hostname: "app", Domain: "example.com", RR: "A", Server: "10.0.0.1",
		Description: "managed by yk-dns-manager",
	})

	h.createRoute("app", []string{"app.example.com"})

	h.eventually("app.example.com published and enabled", func() bool {
		return hasRow(h.fake.Published(), "app.example.com", "10.0.0.1")
	})
}

// TestDeleteIsPublishedAfterReconfigureRecovers: when reconfigure fails during
// route deletion the finalizer is already gone, the retry finds no route and
// returns before flushing, and the deleted record keeps resolving.
func TestDeleteIsPublishedAfterReconfigureRecovers(t *testing.T) {
	pending(t, "audit B9, roadmap phase 4")

	h := start(t, harnessOptions{domainMap: exampleMap, upsert: true})
	h.createRoute("app", []string{"app.example.com"})
	h.eventually("app.example.com published", func() bool {
		return hasRow(h.fake.Published(), "app.example.com", "10.0.0.1")
	})

	h.fake.SetFailure(testenv.EndpointReconfigure, http.StatusServiceUnavailable)
	h.deleteRoute("app")
	h.waitRouteGone("app")
	h.fake.SetFailure(testenv.EndpointReconfigure, 0)

	h.eventually("app.example.com unpublished once reconfigure recovers", func() bool {
		return len(rows(h.fake.Published(), "app.example.com")) == 0
	})
}

// TestUnacceptedRouteGetsNoRecord: a route no Gateway admitted still gets DNS,
// which lets any namespace claim names under the domain map.
func TestUnacceptedRouteGetsNoRecord(t *testing.T) {
	pending(t, "audit S3, roadmap phase 4")

	h := start(t, harnessOptions{domainMap: exampleMap, upsert: true})
	h.createRoute("app", []string{"app.example.com"}, unaccepted())

	h.consistently("no finalizer and no appliance writes", func() bool {
		r := h.getRoute("app")
		return r != nil && !hasFinalizer(r) && h.countCalls(testenv.EndpointAdd) == 0
	})
}
