// Package store persists desired state, observed status and the event log.
// bbolt: a single file, ACID, no server -- the right size for a control plane
// whose state is thousands of objects, not millions.
package store

import (
	"encoding/json"
	"errors"
	"fmt"
	"time"

	bolt "go.etcd.io/bbolt"

	"github.com/swaroop002/berth/internal/spec"
)

var (
	ErrNotFound = errors.New("store: not found")
	ErrConflict = errors.New("store: conflict")
)

var (
	bEnvs   = []byte("environments")
	bEvents = []byte("events")
)

// Event is one line of an environment's history.
type Event struct {
	At      time.Time `json:"at"`
	Env     string    `json:"env"`
	Type    string    `json:"type"` // Applied, Reconciling, StepReady, StepFailed, Ready, Degraded, Deleting, Deleted, Drift
	Message string    `json:"message"`
}

// Store is safe for concurrent use.
type Store struct{ db *bolt.DB }

// Open creates or opens the database file.
func Open(path string) (*Store, error) {
	db, err := bolt.Open(path, 0o600, &bolt.Options{Timeout: 2 * time.Second})
	if err != nil {
		return nil, err
	}
	err = db.Update(func(tx *bolt.Tx) error {
		for _, b := range [][]byte{bEnvs, bEvents} {
			if _, err := tx.CreateBucketIfNotExists(b); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		db.Close()
		return nil, err
	}
	return &Store{db: db}, nil
}

func (s *Store) Close() error { return s.db.Close() }

// Apply upserts the desired state. A changed spec bumps the generation;
// status is preserved so the reconciler sees desired != observed.
// Returns the stored object and whether the spec changed.
func (s *Store) Apply(env *spec.Environment) (*spec.Environment, bool, error) {
	if err := env.Validate(); err != nil {
		return nil, false, err
	}
	var out spec.Environment
	changed := false
	err := s.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket(bEnvs)
		cur, err := get(b, env.Metadata.Name)
		switch {
		case errors.Is(err, ErrNotFound):
			out = *env
			out.Metadata.Generation = 1
			out.Metadata.CreatedAt = time.Now().UTC()
			out.Metadata.DeletionTimestamp = nil
			out.Status = spec.Status{Phase: spec.PhasePending}
			changed = true
		case err != nil:
			return err
		default:
			if cur.Metadata.DeletionTimestamp != nil {
				return fmt.Errorf("%w: %s is being deleted", ErrConflict, env.Metadata.Name)
			}
			out = *cur
			if !sameSpec(cur.Spec, env.Spec) || cur.Metadata.Team != env.Metadata.Team || cur.Metadata.Owner != env.Metadata.Owner {
				out.Spec = env.Spec
				out.Metadata.Team, out.Metadata.Owner, out.Metadata.Labels = env.Metadata.Team, env.Metadata.Owner, env.Metadata.Labels
				out.Metadata.Generation++
				changed = true
			}
		}
		return put(b, &out)
	})
	if err != nil {
		return nil, false, err
	}
	return &out, changed, nil
}

// UpdateStatus writes a new status for an existing environment.
func (s *Store) UpdateStatus(name string, st spec.Status) error {
	return s.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket(bEnvs)
		cur, err := get(b, name)
		if err != nil {
			return err
		}
		cur.Status = st
		return put(b, cur)
	})
}

// MarkDeleting sets the deletion timestamp; the reconciler tears down and
// then calls Remove. Idempotent.
func (s *Store) MarkDeleting(name string) (*spec.Environment, error) {
	var out *spec.Environment
	err := s.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket(bEnvs)
		cur, err := get(b, name)
		if err != nil {
			return err
		}
		if cur.Metadata.DeletionTimestamp == nil {
			now := time.Now().UTC()
			cur.Metadata.DeletionTimestamp = &now
			cur.Status.Phase = spec.PhaseDeleting
		}
		out = cur
		return put(b, cur)
	})
	return out, err
}

// Remove deletes the object once its resources are gone.
func (s *Store) Remove(name string) error {
	return s.db.Update(func(tx *bolt.Tx) error { return tx.Bucket(bEnvs).Delete([]byte(name)) })
}

// Get returns one environment.
func (s *Store) Get(name string) (*spec.Environment, error) {
	var out *spec.Environment
	err := s.db.View(func(tx *bolt.Tx) error {
		var err error
		out, err = get(tx.Bucket(bEnvs), name)
		return err
	})
	return out, err
}

// List returns every environment, by name.
func (s *Store) List() ([]*spec.Environment, error) {
	var out []*spec.Environment
	err := s.db.View(func(tx *bolt.Tx) error {
		return tx.Bucket(bEnvs).ForEach(func(k, v []byte) error {
			var e spec.Environment
			if err := json.Unmarshal(v, &e); err != nil {
				return err
			}
			out = append(out, &e)
			return nil
		})
	})
	return out, err
}

// Record appends an event. Keys are time-ordered so a prefix scan per
// environment returns history in order.
func (s *Store) Record(ev Event) error {
	if ev.At.IsZero() {
		ev.At = time.Now().UTC()
	}
	return s.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket(bEvents)
		seq, _ := b.NextSequence()
		key := fmt.Sprintf("%s/%020d/%012d", ev.Env, ev.At.UnixNano(), seq)
		v, _ := json.Marshal(ev)
		return b.Put([]byte(key), v)
	})
}

// Events returns the newest `limit` events of an environment, oldest first.
func (s *Store) Events(env string, limit int) ([]Event, error) {
	var out []Event
	err := s.db.View(func(tx *bolt.Tx) error {
		c := tx.Bucket(bEvents).Cursor()
		prefix := []byte(env + "/")
		for k, v := c.Seek(prefix); k != nil && len(k) > len(prefix) && string(k[:len(prefix)]) == string(prefix); k, v = c.Next() {
			var ev Event
			if err := json.Unmarshal(v, &ev); err != nil {
				return err
			}
			out = append(out, ev)
		}
		return nil
	})
	if limit > 0 && len(out) > limit {
		out = out[len(out)-limit:]
	}
	return out, err
}

func get(b *bolt.Bucket, name string) (*spec.Environment, error) {
	v := b.Get([]byte(name))
	if v == nil {
		return nil, fmt.Errorf("%w: %s", ErrNotFound, name)
	}
	var e spec.Environment
	if err := json.Unmarshal(v, &e); err != nil {
		return nil, err
	}
	return &e, nil
}

func put(b *bolt.Bucket, e *spec.Environment) error {
	v, err := json.Marshal(e)
	if err != nil {
		return err
	}
	return b.Put([]byte(e.Metadata.Name), v)
}

func sameSpec(a, b spec.Spec) bool {
	ja, _ := json.Marshal(a)
	jb, _ := json.Marshal(b)
	return string(ja) == string(jb)
}
