package backend

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/hclsimple"
)

type Catalog struct {
	mu      sync.RWMutex
	entries map[string]*CatalogEntry
	execs   map[string]*CatalogEntry
	schemas []*SchemaEntry
}

type CatalogEntry struct {
	Name string   `hcl:"name,label"`
	SQL  string   `hcl:"sql"`
	Args []string `hcl:"args,optional"`
}

// SchemaEntry is applied when a named database is opened. Its label is the database
// name, or a prefix ending in "*"; exactly one of sql and file carries the statements.
type SchemaEntry struct {
	Match      string `hcl:"match,label"`
	SQL        string `hcl:"sql,optional"`
	File       string `hcl:"file,optional"`
	statements []string
}

type catalogFile struct {
	Queries []*CatalogEntry `hcl:"query,block"`
	Execs   []*CatalogEntry `hcl:"exec,block"`
	Schemas []*SchemaEntry  `hcl:"schema,block"`
	Remain  hcl.Body        `hcl:",remain"`
}

func LoadCatalog(path string) (*Catalog, error) {
	if _, err := os.Stat(path); err != nil {
		return nil, err
	}
	var file catalogFile
	if err := hclsimple.DecodeFile(path, nil, &file); err != nil {
		return nil, fmt.Errorf("decode %s: %w", path, err)
	}
	c := &Catalog{
		entries: make(map[string]*CatalogEntry, len(file.Queries)),
		execs:   make(map[string]*CatalogEntry, len(file.Execs)),
	}
	for _, q := range file.Queries {
		if err := add(c.entries, q, "query", path); err != nil {
			return nil, err
		}
	}
	for _, e := range file.Execs {
		if err := add(c.execs, e, "exec", path); err != nil {
			return nil, err
		}
		if _, both := c.entries[e.Name]; both {
			return nil, fmt.Errorf("%q in %s is both a query and an exec", e.Name, path)
		}
	}
	for _, s := range file.Schemas {
		if err := s.load(filepath.Dir(path)); err != nil {
			return nil, fmt.Errorf("schema %q in %s: %w", s.Match, path, err)
		}
		c.schemas = append(c.schemas, s)
	}
	return c, nil
}

func add(into map[string]*CatalogEntry, e *CatalogEntry, kind, path string) error {
	if e.Name == "" {
		return fmt.Errorf("%s in %s missing name", kind, path)
	}
	if e.SQL == "" {
		return fmt.Errorf("%s %q in %s missing sql", kind, e.Name, path)
	}
	if _, dup := into[e.Name]; dup {
		return fmt.Errorf("%s %q defined twice in %s", kind, e.Name, path)
	}
	into[e.Name] = e
	return nil
}

func (s *SchemaEntry) load(dir string) error {
	text := s.SQL
	switch {
	case s.Match == "":
		return fmt.Errorf("missing label")
	case s.SQL != "" && s.File != "":
		return fmt.Errorf("set sql or file, not both")
	case s.File != "":
		b, err := os.ReadFile(filepath.Join(dir, s.File))
		if err != nil {
			return err
		}
		text = string(b)
	case s.SQL == "":
		return fmt.Errorf("set sql or file")
	}
	for _, st := range strings.Split(text, ";") {
		if st = strings.TrimSpace(st); st != "" {
			s.statements = append(s.statements, st)
		}
	}
	return nil
}

func (c *Catalog) Lookup(name string) (*CatalogEntry, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	e, ok := c.entries[name]
	return e, ok
}

func (c *Catalog) LookupExec(name string) (*CatalogEntry, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	e, ok := c.execs[name]
	return e, ok
}

func (c *Catalog) Names() []string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return sortedKeys(c.entries)
}

func (c *Catalog) ExecNames() []string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return sortedKeys(c.execs)
}

// SchemaFor returns the statements for a database: an exact label wins, then the
// longest "prefix*" label; nil when nothing matches.
func (c *Catalog) SchemaFor(db string) []string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	var best *SchemaEntry
	for _, s := range c.schemas {
		switch {
		case s.Match == db:
			return s.statements
		case strings.HasSuffix(s.Match, "*") && strings.HasPrefix(db, strings.TrimSuffix(s.Match, "*")):
			if best == nil || len(s.Match) > len(best.Match) {
				best = s
			}
		}
	}
	if best == nil {
		return nil
	}
	return best.statements
}

func sortedKeys(m map[string]*CatalogEntry) []string {
	names := make([]string, 0, len(m))
	for n := range m {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}
