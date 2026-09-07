package backend

import (
	"context"
	"database/sql"
	"fmt"

	sqlite3 "github.com/mattn/go-sqlite3"
)

const (
	// FabricStoreVFS is the VFS name fabric-store's fdb_vfs.c registers.
	FabricStoreVFS = "weft_fdb"

	driverPlain  = "sqlite3"
	driverFabric = "sqlite3_fabric"

	pragmaJournal = "PRAGMA journal_mode=MEMORY"
	pragmaLocking = "PRAGMA locking_mode=EXCLUSIVE"
)

func init() {
	// A second driver that applies fabric-store's required pragmas on every
	// new connection. Registered even when no FDB is around; the driver only
	// dials sqlite3, and the pragmas are per-connection and benign locally.
	sql.Register(driverFabric, &sqlite3.SQLiteDriver{
		ConnectHook: func(conn *sqlite3.SQLiteConn) error {
			for _, p := range []string{pragmaJournal, pragmaLocking} {
				if _, err := conn.Exec(p, nil); err != nil {
					return fmt.Errorf("apply %q: %w", p, err)
				}
			}
			return nil
		},
	})
}

type Store interface {
	Query(ctx context.Context, sqlText string, args ...interface{}) ([]map[string]interface{}, error)
	Exec(ctx context.Context, sqlText string, args ...interface{}) ([]map[string]interface{}, int64, error)
	Apply(ctx context.Context, statements []string) error
	Close() error
}

type sqlStore struct{ db *sql.DB }

// OpenStore opens a SQLite database at dsn using the default unix VFS. Used
// for local tests and non-fabric-store deployments.
func OpenStore(dsn string) (Store, error) {
	return openWith(driverPlain, dsn)
}

// OpenFabricStore opens a SQLite database whose pages live in FoundationDB
// via fabric-store's weft_fdb VFS. StartFabricStore must have been called
// first. dbName is the actor / database name — it becomes weft/db/<name>/
// in the FDB key space.
func OpenFabricStore(dbName string) (Store, error) {
	dsn := fmt.Sprintf("file:%s?vfs=%s", dbName, FabricStoreVFS)
	return openWith(driverFabric, dsn)
}

// One connection per database: a weft_fdb database has one writer and holds
// locking_mode=EXCLUSIVE, so a pool would only queue behind itself.
func openWith(driver, dsn string) (Store, error) {
	db, err := sql.Open(driver, dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	if err := db.Ping(); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("ping %s: %w", dsn, err)
	}
	return &sqlStore{db: db}, nil
}

func (s *sqlStore) Query(ctx context.Context, sqlText string, args ...interface{}) ([]map[string]interface{}, error) {
	rows, err := s.db.QueryContext(ctx, sqlText, args...)
	if err != nil {
		return nil, err
	}
	return collect(rows)
}

// Exec runs one statement on a pinned connection so the change count read
// afterwards belongs to it; RETURNING rows come back like a query's.
func (s *sqlStore) Exec(ctx context.Context, sqlText string, args ...interface{}) ([]map[string]interface{}, int64, error) {
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return nil, 0, err
	}
	defer conn.Close()

	rows, err := conn.QueryContext(ctx, sqlText, args...)
	if err != nil {
		return nil, 0, err
	}
	out, err := collect(rows)
	if err != nil {
		return nil, 0, err
	}
	var changes int64
	if err := conn.QueryRowContext(ctx, "SELECT changes()").Scan(&changes); err != nil {
		return nil, 0, err
	}
	return out, changes, nil
}

func (s *sqlStore) Apply(ctx context.Context, statements []string) error {
	for _, st := range statements {
		if _, err := s.db.ExecContext(ctx, st); err != nil {
			return fmt.Errorf("%s: %w", st, err)
		}
	}
	return nil
}

func (s *sqlStore) Close() error { return s.db.Close() }

// collect reads every row into a column-keyed map and closes the cursor. An
// empty result is an empty list, never null.
func collect(rows *sql.Rows) ([]map[string]interface{}, error) {
	defer rows.Close()
	cols, err := rows.Columns()
	if err != nil {
		return nil, err
	}
	out := make([]map[string]interface{}, 0)
	for rows.Next() {
		cells := make([]interface{}, len(cols))
		ptrs := make([]interface{}, len(cols))
		for i := range cells {
			ptrs[i] = &cells[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			return nil, err
		}
		row := make(map[string]interface{}, len(cols))
		for i, c := range cols {
			row[c] = cells[i]
		}
		out = append(out, row)
	}
	return out, rows.Err()
}
