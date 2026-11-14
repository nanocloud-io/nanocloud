// Package template stores reusable VM configurations: the shape of a machine
// without the machine.
//
// Templates live in their own file because metadata.json is rewritten from
// {schema_version, vms} on every write and would drop any other top-level key.
//
// # On-disk format
//
//	{"schema_version": 1, "templates": {"web server": {...}}}
package template

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
	"unicode"

	"github.com/nanocloud-io/nanocloud/internal/atomicfile"
	"github.com/nanocloud-io/nanocloud/internal/metadata"
)

// CurrentSchemaVersion is the on-disk format this binary writes. Adding fields
// to Template does not need a bump; an incompatible restructure would.
const CurrentSchemaVersion = 1

var (
	// ErrNotFound is returned by Get and Delete for an unknown template name.
	ErrNotFound = errors.New("template: no template with that name")
	// ErrExists is returned by Create instead of overwriting an existing
	// template.
	ErrExists = errors.New("template: a template with that name already exists")
	// ErrInvalidName is returned for a name that cannot be stored or addressed.
	ErrInvalidName = errors.New("template: invalid name")
	// ErrSchemaTooNew is returned when the file was written by a newer binary.
	// Writing it back would strip every field this binary does not recognise.
	ErrSchemaTooNew = errors.New("template: file schema is newer than this binary understands")
)

// Template is a saved machine configuration.
type Template struct {
	// Name is the display name and the key in the file.
	Name string `json:"name"`
	// CreatedAtUnix is when the template was saved.
	CreatedAtUnix int64 `json:"created_at_unix"`
	// SourceVM is the VM the configuration was captured from. It is
	// provenance only; that VM may since have changed or been deleted.
	SourceVM string `json:"source_vm,omitempty"`
	// Spec is the configuration, held as a VMEntry so new VM fields are
	// templatable automatically. Per-machine fields are blanked; see FromEntry.
	Spec metadata.VMEntry `json:"spec"`
}

// FromEntry captures a VM's configuration as a template, dropping everything
// that belongs to that one machine.
//
// A second VM must not inherit the name, hostname, power state, or libvirt
// identity (a copied UUID would redefine the source domain instead of creating
// a new one). Attached disks are dropped too, since their paths point at the
// source VM's own files. Any new identity field on VMEntry must be blanked here.
func FromEntry(name string, e metadata.VMEntry) Template {
	e.ID = ""
	e.Hostname = ""
	e.PoolPath = ""
	e.UUID = ""
	e.MAC = ""
	e.Running = nil
	e.StartedAtUnix = 0
	e.Disks = nil
	e.Service = ""

	return Template{
		Name:          name,
		CreatedAtUnix: time.Now().Unix(),
		Spec:          e,
	}
}

// ValidateName reports whether a name can be stored and addressed. The name is
// also an API path segment, so it may not contain a slash or a control
// character; otherwise it is free text.
func ValidateName(name string) error {
	trimmed := strings.TrimSpace(name)
	switch {
	case trimmed == "":
		return fmt.Errorf("%w: a template needs a name", ErrInvalidName)
	case len(trimmed) > maxNameLen:
		return fmt.Errorf("%w: %q is longer than %d characters", ErrInvalidName, trimmed, maxNameLen)
	}
	for _, r := range trimmed {
		if r == '/' || r == '\\' || unicode.IsControl(r) {
			return fmt.Errorf("%w: %q may not contain slashes or control characters", ErrInvalidName, trimmed)
		}
	}
	return nil
}

const maxNameLen = 63

// Store is a file-backed collection of templates, keyed by name.
type Store struct {
	file *atomicfile.File
}

// New returns a Store backed by the file at path, which need not exist yet.
func New(path string) *Store {
	return &Store{file: atomicfile.NewPrivate(path)}
}

type document struct {
	SchemaVersion int                 `json:"schema_version"`
	Templates     map[string]Template `json:"templates"`
}

func decode(data []byte) (map[string]Template, error) {
	if len(data) == 0 {
		return make(map[string]Template), nil
	}
	var doc document
	if err := json.Unmarshal(data, &doc); err != nil {
		return nil, fmt.Errorf("template: parse document: %w", err)
	}
	if doc.SchemaVersion > CurrentSchemaVersion {
		return nil, fmt.Errorf("%w: file is v%d, this binary understands v%d",
			ErrSchemaTooNew, doc.SchemaVersion, CurrentSchemaVersion)
	}
	if doc.Templates == nil {
		doc.Templates = make(map[string]Template)
	}
	return doc.Templates, nil
}

func encode(templates map[string]Template) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetIndent("", "  ")
	if err := enc.Encode(document{SchemaVersion: CurrentSchemaVersion, Templates: templates}); err != nil {
		return nil, fmt.Errorf("template: encode document: %w", err)
	}
	return buf.Bytes(), nil
}

// List returns every template, ordered by name.
func (s *Store) List() ([]Template, error) {
	data, err := s.file.Read()
	if err != nil {
		return nil, err
	}
	templates, err := decode(data)
	if err != nil {
		return nil, err
	}
	out := make([]Template, 0, len(templates))
	for _, t := range templates {
		out = append(out, t)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// Get returns one template, or ErrNotFound.
func (s *Store) Get(name string) (Template, error) {
	data, err := s.file.Read()
	if err != nil {
		return Template{}, err
	}
	templates, err := decode(data)
	if err != nil {
		return Template{}, err
	}
	t, ok := templates[name]
	if !ok {
		return Template{}, fmt.Errorf("%w: %q", ErrNotFound, name)
	}
	return t, nil
}

// Create stores a new template, refusing a name already taken.
func (s *Store) Create(t Template) error {
	if err := ValidateName(t.Name); err != nil {
		return err
	}
	t.Name = strings.TrimSpace(t.Name)
	return s.file.Update(func(data []byte) ([]byte, error) {
		templates, err := decode(data)
		if err != nil {
			return nil, err
		}
		if _, taken := templates[t.Name]; taken {
			return nil, fmt.Errorf("%w: %q", ErrExists, t.Name)
		}
		templates[t.Name] = t
		return encode(templates)
	})
}

// Delete removes a template, returning ErrNotFound if there was none, so a
// mistyped name is reported rather than treated as success.
func (s *Store) Delete(name string) error {
	return s.file.Update(func(data []byte) ([]byte, error) {
		templates, err := decode(data)
		if err != nil {
			return nil, err
		}
		if _, ok := templates[name]; !ok {
			return nil, fmt.Errorf("%w: %q", ErrNotFound, name)
		}
		delete(templates, name)
		return encode(templates)
	})
}
