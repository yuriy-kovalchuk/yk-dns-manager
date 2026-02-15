// Package providers imports all DNS provider packages to trigger their init() registration.
package providers

import (
	_ "github.com/yuriy-kovalchuk/yk-dns-manager/internal/dns/opnsense"
)
