package envtest

import (
	"net/http"
	"testing"

	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	"github.com/yuriy-kovalchuk/yk-dns-manager/internal/testenv"
)

const exampleMap = "example.com: 10.0.0.1\n"

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
// guarantee: a failing appliance must never leave a route stuck in Terminating.
func TestRouteIsDeletableWhileApplianceFails(t *testing.T) {
	h := start(t, harnessOptions{domainMap: exampleMap, upsert: true})

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
