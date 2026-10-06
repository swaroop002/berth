// Package spec is forge's API: what a user asks for (an Environment) and what
// the platform reports back (its Status). Versioned like a Kubernetes object
// so a later operator can serve the same type as a CRD unchanged.
package spec

import (
	"errors"
	"fmt"
	"regexp"
	"time"
)

const (
	APIVersion = "forge.swaroop002.dev/v1alpha1"
	KindEnv    = "Environment"
)

// Environment is the unit forge manages: compute plus the plumbing around it.
type Environment struct {
	APIVersion string   `json:"apiVersion" yaml:"apiVersion"`
	Kind       string   `json:"kind" yaml:"kind"`
	Metadata   Metadata `json:"metadata" yaml:"metadata"`
	Spec       Spec     `json:"spec" yaml:"spec"`
	Status     Status   `json:"status,omitempty" yaml:"status,omitempty"`
}

type Metadata struct {
	Name   string            `json:"name" yaml:"name"`
	Team   string            `json:"team" yaml:"team"`
	Owner  string            `json:"owner" yaml:"owner"` // who to ask before deleting it
	Labels map[string]string `json:"labels,omitempty" yaml:"labels,omitempty"`
	// Generation increments on every spec change; the reconciler records the
	// generation it last converged, which is how "is it up to date" is answered.
	Generation        int64      `json:"generation,omitempty" yaml:"generation,omitempty"`
	CreatedAt         time.Time  `json:"createdAt,omitempty" yaml:"createdAt,omitempty"`
	DeletionTimestamp *time.Time `json:"deletionTimestamp,omitempty" yaml:"deletionTimestamp,omitempty"`
}

type Spec struct {
	Size   string `json:"size" yaml:"size"`     // small | medium | large
	Region string `json:"region" yaml:"region"` // provider region
	Image  string `json:"image,omitempty" yaml:"image,omitempty"`
	// What to attach. Each is reconciled by its own step, in dependency order.
	Storage   *StorageSpec   `json:"storage,omitempty" yaml:"storage,omitempty"`
	DNS       *DNSSpec       `json:"dns,omitempty" yaml:"dns,omitempty"`
	Route     *RouteSpec     `json:"route,omitempty" yaml:"route,omitempty"`
	Telemetry *TelemetrySpec `json:"telemetry,omitempty" yaml:"telemetry,omitempty"`
	// TTL: the environment is deleted this long after creation. Zero = keep.
	TTL time.Duration `json:"ttl,omitempty" yaml:"ttl,omitempty"`
}

type StorageSpec struct {
	SizeGB int `json:"sizeGB" yaml:"sizeGB"`
}
type DNSSpec struct {
	Hostname string `json:"hostname" yaml:"hostname"`
}
type RouteSpec struct {
	Port int  `json:"port" yaml:"port"`
	Auth bool `json:"auth" yaml:"auth"` // authentication at the edge
}
type TelemetrySpec struct {
	Tenant string `json:"tenant" yaml:"tenant"` // pulse tenant to provision for it
}

// Phase is the coarse state; Conditions carry the detail.
type Phase string

const (
	PhasePending  Phase = "Pending"
	PhaseReady    Phase = "Ready"
	PhaseDegraded Phase = "Degraded"
	PhaseDeleting Phase = "Deleting"
	PhaseFailed   Phase = "Failed"
)

type Status struct {
	Phase              Phase             `json:"phase" yaml:"phase"`
	ObservedGeneration int64             `json:"observedGeneration" yaml:"observedGeneration"`
	Conditions         []Condition       `json:"conditions,omitempty" yaml:"conditions,omitempty"`
	Resources          map[string]string `json:"resources,omitempty" yaml:"resources,omitempty"` // step -> provider id
	Endpoints          map[string]string `json:"endpoints,omitempty" yaml:"endpoints,omitempty"`
	Attempts           int               `json:"attempts,omitempty" yaml:"attempts,omitempty"`
	LastReconciled     time.Time         `json:"lastReconciled,omitempty" yaml:"lastReconciled,omitempty"`
}

// Condition follows the Kubernetes convention: one per aspect, with a reason.
type Condition struct {
	Type    string `json:"type" yaml:"type"`     // Compute, Storage, DNS, Route, Telemetry, Ready
	Status  string `json:"status" yaml:"status"` // True | False | Unknown
	Reason  string `json:"reason,omitempty" yaml:"reason,omitempty"`
	Message string `json:"message,omitempty" yaml:"message,omitempty"`
	// ObservedGeneration is the spec generation this condition describes, so
	// a retry after a failure can skip steps already good for this generation.
	ObservedGeneration int64     `json:"observedGeneration,omitempty" yaml:"observedGeneration,omitempty"`
	Since              time.Time `json:"since" yaml:"since"`
}

var (
	ErrInvalid = errors.New("spec: invalid environment")
	nameRe     = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)
	sizes      = map[string]bool{"small": true, "medium": true, "large": true}
	hostRe     = regexp.MustCompile(`^([a-z0-9]([a-z0-9-]*[a-z0-9])?\.)+[a-z]{2,}$`)
)

// Validate checks everything that does not need a provider.
func (e *Environment) Validate() error {
	var errs []string
	if e.APIVersion != APIVersion {
		errs = append(errs, fmt.Sprintf("apiVersion must be %s", APIVersion))
	}
	if e.Kind != KindEnv {
		errs = append(errs, fmt.Sprintf("kind must be %s", KindEnv))
	}
	if !nameRe.MatchString(e.Metadata.Name) {
		errs = append(errs, "metadata.name must be a DNS label (lowercase, digits, hyphens, max 63)")
	}
	if e.Metadata.Team == "" {
		errs = append(errs, "metadata.team is required")
	}
	if e.Metadata.Owner == "" {
		errs = append(errs, "metadata.owner is required")
	}
	if !sizes[e.Spec.Size] {
		errs = append(errs, "spec.size must be small, medium or large")
	}
	if e.Spec.Region == "" {
		errs = append(errs, "spec.region is required")
	}
	if e.Spec.Storage != nil && (e.Spec.Storage.SizeGB < 1 || e.Spec.Storage.SizeGB > 4096) {
		errs = append(errs, "spec.storage.sizeGB must be 1..4096")
	}
	if e.Spec.DNS != nil && !hostRe.MatchString(e.Spec.DNS.Hostname) {
		errs = append(errs, "spec.dns.hostname must be a fully qualified hostname")
	}
	if e.Spec.Route != nil {
		if e.Spec.Route.Port < 1 || e.Spec.Route.Port > 65535 {
			errs = append(errs, "spec.route.port must be 1..65535")
		}
		if e.Spec.DNS == nil {
			errs = append(errs, "spec.route needs spec.dns (a route is published on a hostname)")
		}
	}
	if e.Spec.Telemetry != nil && !nameRe.MatchString(e.Spec.Telemetry.Tenant) {
		errs = append(errs, "spec.telemetry.tenant must be a DNS label")
	}
	if e.Spec.TTL < 0 {
		errs = append(errs, "spec.ttl cannot be negative")
	}
	if len(errs) > 0 {
		return fmt.Errorf("%w: %v", ErrInvalid, errs)
	}
	return nil
}

// Steps lists the reconciliation steps this spec needs, in dependency order.
// Deletion runs them in reverse.
func (e *Environment) Steps() []string {
	steps := []string{"Compute"}
	if e.Spec.Storage != nil {
		steps = append(steps, "Storage")
	}
	if e.Spec.DNS != nil {
		steps = append(steps, "DNS")
	}
	if e.Spec.Route != nil {
		steps = append(steps, "Route")
	}
	if e.Spec.Telemetry != nil {
		steps = append(steps, "Telemetry")
	}
	return steps
}

// SetCondition upserts a condition, keeping Since when nothing changed.
func (s *Status) SetCondition(c Condition) {
	for i := range s.Conditions {
		if s.Conditions[i].Type == c.Type {
			if s.Conditions[i].Status == c.Status && s.Conditions[i].Reason == c.Reason {
				s.Conditions[i].Message = c.Message
				s.Conditions[i].ObservedGeneration = c.ObservedGeneration
				return
			}
			s.Conditions[i] = c
			return
		}
	}
	s.Conditions = append(s.Conditions, c)
}

// Condition returns the condition of a type, or nil.
func (s *Status) Condition(t string) *Condition {
	for i := range s.Conditions {
		if s.Conditions[i].Type == t {
			return &s.Conditions[i]
		}
	}
	return nil
}

// Expired reports whether the TTL has passed.
func (e *Environment) Expired(now time.Time) bool {
	return e.Spec.TTL > 0 && !e.Metadata.CreatedAt.IsZero() && now.After(e.Metadata.CreatedAt.Add(e.Spec.TTL))
}
