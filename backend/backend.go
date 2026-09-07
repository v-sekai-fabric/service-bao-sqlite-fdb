package backend

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	log "github.com/hashicorp/go-hclog"
	"github.com/openbao/openbao/sdk/v2/framework"
	"github.com/openbao/openbao/sdk/v2/logical"
)

const backendHelp = `
The sqlite-fdb secrets engine runs precanned SQL statements from its catalog
against SQLite databases. When a database is opened through the fabric-store
VFS, its pages live in FoundationDB and no local file is written.

Reads run a catalog query; writes run a catalog exec. Named databases open on
demand and receive the catalog's schema when they open. Consumers cannot
supply SQL.
`

const (
	envCatalog = "BAO_SQLITE_FDB_CATALOG"
	envDB      = "BAO_SQLITE_FDB_DB"
	envCluster = "BAO_SQLITE_FDB_CLUSTER"
	envDSN     = "BAO_SQLITE_FDB_DSN"
	envDir     = "BAO_SQLITE_FDB_DIR"
)

type backend struct {
	*framework.Backend

	logger  log.Logger
	catalog *Catalog
	store   Store // the default database behind query/<name>; nil when none is configured
	fabric  bool
	dir     string
	mu      sync.Mutex
	stores  map[string]Store
}

type storeConfig struct {
	cluster, db, dsn, dir string
}

// Factory wires the OpenBao logical.Backend.
//
// Two modes, decided at startup:
//
//	Fabric mode (production): BAO_SQLITE_FDB_CLUSTER is set. The plugin calls
//	StartFabricStore(cluster) and registers the weft_fdb VFS. BAO_SQLITE_FDB_DB,
//	when set, names the default database behind query/<name>; named databases
//	open on the cluster on demand.
//
//	Plain mode (dev / local tests): BAO_SQLITE_FDB_DSN opens the default database
//	through the unix VFS, and BAO_SQLITE_FDB_DIR holds named databases as files.
//	Either or both. No FDB.
//
// Exactly one of the two modes must be selected. An unmet precondition is
// a startup failure, never a silent default.
func Factory(ctx context.Context, conf *logical.BackendConfig) (logical.Backend, error) {
	catalogPath := os.Getenv(envCatalog)
	if catalogPath == "" {
		return nil, fmt.Errorf("%s not set: plugin cannot start without a catalog", envCatalog)
	}
	catalog, err := LoadCatalog(catalogPath)
	if err != nil {
		return nil, fmt.Errorf("load catalog %s: %w", catalogPath, err)
	}

	cfg, err := configFromEnv()
	if err != nil {
		return nil, err
	}

	b := &backend{
		logger:  conf.Logger,
		catalog: catalog,
		fabric:  cfg.cluster != "",
		dir:     cfg.dir,
		stores:  map[string]Store{},
	}
	if b.fabric {
		if err := StartFabricStore(cfg.cluster); err != nil {
			return nil, fmt.Errorf("start fabric-store: %w", err)
		}
	}
	if err := b.openDefault(cfg); err != nil {
		b.stop()
		return nil, err
	}
	b.Backend = &framework.Backend{
		Help:        strings.TrimSpace(backendHelp),
		BackendType: logical.TypeLogical,
		Paths:       paths(b),
		Clean:       b.clean,
	}
	if err := b.Backend.Setup(ctx, conf); err != nil {
		b.stop()
		return nil, err
	}
	return b, nil
}

func configFromEnv() (storeConfig, error) {
	c := storeConfig{
		cluster: os.Getenv(envCluster),
		db:      os.Getenv(envDB),
		dsn:     os.Getenv(envDSN),
		dir:     os.Getenv(envDir),
	}
	fabric := c.cluster != ""
	plain := c.dsn != "" || c.dir != ""
	switch {
	case fabric && plain:
		return c, fmt.Errorf("%s must not be set with %s or %s: pick one mode", envCluster, envDSN, envDir)
	case !fabric && !plain:
		return c, fmt.Errorf(
			"no database configured: set %s (fabric-store mode) or %s and/or %s (plain mode)",
			envCluster, envDSN, envDir,
		)
	case !fabric && c.db != "":
		return c, fmt.Errorf("%s is only meaningful with %s", envDB, envCluster)
	}
	return c, nil
}

func (b *backend) openDefault(cfg storeConfig) error {
	switch {
	case b.fabric && cfg.db != "":
		s, err := OpenFabricStore(cfg.db)
		if err != nil {
			return fmt.Errorf("open fabric-store db %q: %w", cfg.db, err)
		}
		b.store = s
	case !b.fabric && cfg.dsn != "":
		s, err := OpenStore(cfg.dsn)
		if err != nil {
			return fmt.Errorf("open plain sqlite %q: %w", cfg.dsn, err)
		}
		b.store = s
	}
	return nil
}

// named opens a database on first use and applies the catalog's schema for it.
// In fabric mode the open takes the database's fence.
func (b *backend) named(ctx context.Context, name string) (Store, error) {
	if !dbNamePattern.MatchString(name) {
		return nil, fmt.Errorf("database name %q is not [A-Za-z0-9_-]", name)
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if s, ok := b.stores[name]; ok {
		return s, nil
	}
	var s Store
	var err error
	switch {
	case b.fabric:
		s, err = OpenFabricStore(name)
	case b.dir != "":
		s, err = OpenStore(filepath.Join(b.dir, name+".sqlite"))
	default:
		return nil, fmt.Errorf("named databases need %s in plain mode", envDir)
	}
	if err != nil {
		return nil, err
	}
	if err := s.Apply(ctx, b.catalog.SchemaFor(name)); err != nil {
		_ = s.Close()
		return nil, fmt.Errorf("schema: %w", err)
	}
	b.stores[name] = s
	return s, nil
}

func (b *backend) clean(_ context.Context) { b.stop() }

func (b *backend) stop() {
	b.mu.Lock()
	for name, s := range b.stores {
		if err := s.Close(); err != nil {
			b.warn("store close", "db", name, "error", err)
		}
		delete(b.stores, name)
	}
	b.mu.Unlock()
	if b.store != nil {
		if err := b.store.Close(); err != nil {
			b.warn("store close", "error", err)
		}
		b.store = nil
	}
	if b.fabric {
		StopFabricStore()
	}
}

func (b *backend) warn(msg string, args ...interface{}) {
	if b.logger != nil {
		b.logger.Warn(msg, args...)
	}
}
