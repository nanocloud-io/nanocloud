// Package metadata is a durable, concurrency-safe store for the per-VM spec.
// Every read-modify-write cycle goes through internal/atomicfile, which owns
// the locking and atomic replacement.
//
// # On-disk format
//
//	{"schema_version": 1, "vms": {"web-01": {...}}}
//
// Version 0 is a bare top-level object of entries with no envelope. Both are
// readable.
package metadata

import (
	"bytes"
	"encoding/json"
	"fmt"

	"github.com/nanocloud-io/nanocloud/internal/atomicfile"
)

// Envelope keys. Both are also valid VM names, which is why decodeDocument
// type-checks rather than probing for presence.
const (
	schemaVersionKey = "schema_version"
	vmsKey           = "vms"
)

// Store is a file-backed key/value store of VMEntry records, keyed by VM ID.
type Store struct {
	file *atomicfile.File
}

// New returns a Store backed by the file at path. The file need not exist yet;
// it is created on the first write.
func New(path string) *Store {
	return &Store{file: atomicfile.NewPrivate(path)}
}

// document is the current on-disk envelope.
type document struct {
	SchemaVersion int                `json:"schema_version"`
	VMs           map[string]VMEntry `json:"vms"`
}

// decodeDocument parses either on-disk format and reports which one it found.
// A file is v1 only if schema_version is present and a JSON number: a v0 file
// may hold a VM named "schema_version", whose value is an object.
func decodeDocument(data []byte) (entries map[string]VMEntry, onDiskVersion int, err error) {
	var top map[string]json.RawMessage
	if err := json.Unmarshal(data, &top); err != nil {
		return nil, 0, fmt.Errorf("metadata: parse document: %w", err)
	}

	if raw, ok := top[schemaVersionKey]; ok {
		var v int
		if json.Unmarshal(raw, &v) == nil {
			if v > CurrentSchemaVersion {
				return nil, v, fmt.Errorf("%w: file is v%d, this binary understands v%d",
					ErrSchemaTooNew, v, CurrentSchemaVersion)
			}
			vms := make(map[string]VMEntry)
			if rawVMs, hasVMs := top[vmsKey]; hasVMs {
				if err := json.Unmarshal(rawVMs, &vms); err != nil {
					return nil, v, fmt.Errorf("metadata: parse %q: %w", vmsKey, err)
				}
			}
			return vms, v, nil
		}
		// Not a number: a VM named "schema_version" in a v0 file.
	}

	legacy := make(map[string]VMEntry)
	if err := json.Unmarshal(data, &legacy); err != nil {
		return nil, 0, fmt.Errorf("metadata: parse legacy document: %w", err)
	}
	return legacy, 0, nil
}

// decode turns raw file contents into entries. An absent or empty file is an
// empty fleet at the current version, not an error.
func decode(data []byte) (map[string]VMEntry, int, error) {
	if len(data) == 0 {
		return make(map[string]VMEntry), CurrentSchemaVersion, nil
	}
	return decodeDocument(data)
}

// encode renders the current envelope.
func encode(entries map[string]VMEntry) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetIndent("", "  ")
	if err := enc.Encode(document{SchemaVersion: CurrentSchemaVersion, VMs: entries}); err != nil {
		return nil, fmt.Errorf("metadata: encode document: %w", err)
	}
	return buf.Bytes(), nil
}

// Read returns the full entry map, or an empty map if the file does not exist.
func (s *Store) Read() (map[string]VMEntry, error) {
	data, err := s.file.Read()
	if err != nil {
		return nil, err
	}
	entries, _, err := decode(data)
	return entries, err
}

// Get returns the entry for id, or ErrNotFound if there is none.
func (s *Store) Get(id string) (VMEntry, error) {
	entries, err := s.Read()
	if err != nil {
		return VMEntry{}, err
	}
	e, ok := entries[id]
	if !ok {
		return VMEntry{}, fmt.Errorf("%w: %q", ErrNotFound, id)
	}
	return e, nil
}

// Write replaces the file with the given entry map.
func (s *Store) Write(entries map[string]VMEntry) error {
	return s.file.Update(func([]byte) ([]byte, error) { return encode(entries) })
}

// Migrate rewrites a v0 file in the current envelope format. It is a no-op when
// the file is current, empty, or missing.
//
// It converts format only. Running is left nil for the reconcile loop to fill
// from live libvirt state; guessing here while libvirtd is still starting would
// mark a running fleet as off.
func (s *Store) Migrate() error {
	return s.file.Update(func(data []byte) ([]byte, error) {
		entries, onDisk, err := decode(data)
		if err != nil {
			return nil, err
		}
		if onDisk >= CurrentSchemaVersion {
			return nil, nil
		}
		return encode(entries)
	})
}

// modify performs a read-modify-write cycle for a single entry.
func (s *Store) modify(id string, fn func(*VMEntry), create bool) error {
	if fn == nil {
		return nil
	}
	return s.file.Update(func(data []byte) ([]byte, error) {
		entries, _, err := decode(data)
		if err != nil {
			return nil, err
		}
		entry, ok := entries[id]
		if !ok && !create {
			return nil, fmt.Errorf("%w: %q", ErrNotFound, id)
		}
		fn(&entry)
		entries[id] = entry
		return encode(entries)
	})
}

// Update mutates an existing entry and returns ErrNotFound for an unknown id;
// use Upsert to create. If fn is nil, Update is a no-op.
func (s *Store) Update(id string, fn func(*VMEntry)) error {
	return s.modify(id, fn, false)
}

// Upsert mutates an entry, creating a zero-valued one first if it is absent.
func (s *Store) Upsert(id string, fn func(*VMEntry)) error {
	return s.modify(id, fn, true)
}

// Delete removes the entry for id. If id is not present, Delete is a no-op.
func (s *Store) Delete(id string) error {
	return s.file.Update(func(data []byte) ([]byte, error) {
		entries, _, err := decode(data)
		if err != nil {
			return nil, err
		}
		delete(entries, id)
		return encode(entries)
	})
}
