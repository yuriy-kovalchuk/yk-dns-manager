package controller

import (
	"fmt"
	"strings"

	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
)

// FormatHTTPRoute returns a human-readable string representation of an HTTPRoute.
func FormatHTTPRoute(route *gatewayv1.HTTPRoute) string {
	var b strings.Builder

	fmt.Fprintf(&b, "HTTPRoute %s/%s\n", route.Namespace, route.Name)

	// Hostnames
	if len(route.Spec.Hostnames) > 0 {
		fmt.Fprintf(&b, "  Hostnames:\n")
		for _, h := range route.Spec.Hostnames {
			fmt.Fprintf(&b, "    - %s\n", h)
		}
	}

	// Parent refs
	if len(route.Spec.ParentRefs) > 0 {
		fmt.Fprintf(&b, "  ParentRefs:\n")
		for _, ref := range route.Spec.ParentRefs {
			ns := "<same>"
			if ref.Namespace != nil {
				ns = string(*ref.Namespace)
			}
			section := ""
			if ref.SectionName != nil {
				section = fmt.Sprintf(" sectionName=%s", *ref.SectionName)
			}
			fmt.Fprintf(&b, "    - %s/%s%s\n", ns, ref.Name, section)
		}
	}

	// Rules
	for i, rule := range route.Spec.Rules {
		fmt.Fprintf(&b, "  Rule[%d]:\n", i)

		// Matches
		for j, match := range rule.Matches {
			fmt.Fprintf(&b, "    Match[%d]:", j)
			if match.Path != nil {
				pathType := "PathPrefix"
				if match.Path.Type != nil {
					pathType = string(*match.Path.Type)
				}
				pathValue := "/"
				if match.Path.Value != nil {
					pathValue = *match.Path.Value
				}
				fmt.Fprintf(&b, " path(%s %s)", pathType, pathValue)
			}
			if match.Method != nil {
				fmt.Fprintf(&b, " method(%s)", *match.Method)
			}
			for _, h := range match.Headers {
				headerType := "Exact"
				if h.Type != nil {
					headerType = string(*h.Type)
				}
				fmt.Fprintf(&b, " header(%s %s=%s)", headerType, h.Name, h.Value)
			}
			for _, q := range match.QueryParams {
				qType := "Exact"
				if q.Type != nil {
					qType = string(*q.Type)
				}
				fmt.Fprintf(&b, " query(%s %s=%s)", qType, q.Name, q.Value)
			}
			fmt.Fprintln(&b)
		}

		// Filters
		for k, f := range rule.Filters {
			fmt.Fprintf(&b, "    Filter[%d]: type=%s\n", k, f.Type)
		}

		// Backend refs
		for k, backend := range rule.BackendRefs {
			port := ""
			if backend.Port != nil {
				port = fmt.Sprintf(":%d", *backend.Port)
			}
			weight := ""
			if backend.Weight != nil {
				weight = fmt.Sprintf(" weight=%d", *backend.Weight)
			}
			fmt.Fprintf(&b, "    BackendRef[%d]: %s%s%s\n", k, backend.Name, port, weight)
		}
	}

	return b.String()
}
