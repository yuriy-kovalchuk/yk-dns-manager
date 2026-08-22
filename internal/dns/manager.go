package dns

import (
	"context"
	"errors"
	"fmt"

	"github.com/go-logr/logr"
)

// Manager fans record operations out to every configured provider
// instance. Each instance is an independent backend; the same record is
// applied to all of them.
type Manager struct {
	instances []instance
	log       logr.Logger
}

// instance is one configured DNS backend and its record policy.
type instance struct {
	name     string
	upsert   bool
	provider Provider
}

// NewManager creates an empty Manager. Instances are added with Add.
func NewManager(log logr.Logger) *Manager {
	return &Manager{log: log}
}

// Add registers a provider instance. name identifies the instance in logs
// and errors; upsert selects its record policy (see EnsureRecord).
func (m *Manager) Add(name string, upsert bool, p Provider) {
	m.instances = append(m.instances, instance{name: name, upsert: upsert, provider: p})
}

// Len returns the number of configured provider instances.
func (m *Manager) Len() int { return len(m.instances) }

// EnsureRecord applies record to every instance according to its policy:
// upsert instances get Upsert; non-upsert instances get the record created
// only when it does not exist yet. Errors from all failing instances are
// joined, each prefixed with the instance name.
func (m *Manager) EnsureRecord(ctx context.Context, record Record) error {
	var errs []error
	for _, inst := range m.instances {
		var err error
		if inst.upsert {
			err = inst.provider.Upsert(ctx, record)
		} else {
			exists, e := inst.provider.Exists(ctx, record.Hostname, record.Type)
			switch {
			case e != nil:
				err = e
			case exists:
				m.log.Info("record already exists and upsert is disabled — IPs may drift if the domain map changes; consider enabling upsert for this instance",
					"instance", inst.name, "hostname", record.Hostname)
				continue
			default:
				err = inst.provider.Create(ctx, record)
			}
		}
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", inst.name, err))
		}
	}
	return errors.Join(errs...)
}

// DeleteRecord removes hostname from every instance. Provider Delete is
// expected to be idempotent. Errors from all failing instances are joined.
func (m *Manager) DeleteRecord(ctx context.Context, hostname, recordType string) error {
	var errs []error
	for _, inst := range m.instances {
		if err := inst.provider.Delete(ctx, hostname, recordType); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", inst.name, err))
		}
	}
	return errors.Join(errs...)
}

// HealthCheck requires every instance to be reachable; it returns a joined
// error listing each failing instance.
func (m *Manager) HealthCheck(ctx context.Context) error {
	var errs []error
	for _, inst := range m.instances {
		if err := inst.provider.HealthCheck(ctx); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", inst.name, err))
		}
	}
	return errors.Join(errs...)
}
