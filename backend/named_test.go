package backend

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	log "github.com/hashicorp/go-hclog"
	"github.com/openbao/openbao/sdk/v2/logical"
)

// Named databases in plain mode: BAO_SQLITE_FDB_DIR and no default DSN. The
// catalog carries a schema by sql and one by file, one exec per table, and the
// queries that read them back.
func setupNamedBackend(t *testing.T) (logical.Backend, *logical.InmemStorage, string) {
	t.Helper()
	dir := t.TempDir()
	catPath := filepath.Join(dir, "catalog.hcl")
	if err := os.WriteFile(filepath.Join(dir, "session.sql"), []byte(`
CREATE TABLE IF NOT EXISTS acp_event (
  ordinal INTEGER PRIMARY KEY,
  method TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS acp_event_method ON acp_event (method);
`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(catPath, []byte(`
schema "acp_registry" {
  sql = "CREATE TABLE IF NOT EXISTS acp_session (session_id TEXT PRIMARY KEY, cwd TEXT NOT NULL)"
}
schema "acp_*" {
  file = "session.sql"
}
exec "session_insert" {
  sql  = "INSERT INTO acp_session (session_id, cwd) VALUES (?, ?)"
  args = ["session_id", "cwd"]
}
exec "event_append" {
  sql  = "INSERT INTO acp_event (ordinal, method) VALUES ((SELECT COALESCE(MAX(ordinal), 0) + 1 FROM acp_event), ?) RETURNING ordinal"
  args = ["method"]
}
query "events_after" {
  sql  = "SELECT ordinal, method FROM acp_event WHERE ordinal > ? ORDER BY ordinal"
  args = ["after"]
}
query "sessions" {
  sql = "SELECT session_id, cwd FROM acp_session"
}
`), 0o600); err != nil {
		t.Fatal(err)
	}

	t.Setenv(envCatalog, catPath)
	t.Setenv(envDB, "")
	t.Setenv(envCluster, "")
	t.Setenv(envDSN, "")
	t.Setenv(envDir, dir)

	storage := &logical.InmemStorage{}
	b, err := Factory(context.Background(), &logical.BackendConfig{
		Logger:      log.NewNullLogger(),
		StorageView: storage,
	})
	if err != nil {
		t.Fatal(err)
	}
	return b, storage, dir
}

func request(t *testing.T, b logical.Backend, storage *logical.InmemStorage, op logical.Operation, path string, data map[string]interface{}) *logical.Response {
	t.Helper()
	resp, err := b.HandleRequest(context.Background(), &logical.Request{
		Operation: op,
		Path:      path,
		Data:      data,
		Storage:   storage,
	})
	if err != nil {
		t.Fatalf("%s %s: %v", op, path, err)
	}
	return resp
}

func TestNamed_ExecThenQuery(t *testing.T) {
	b, storage, dir := setupNamedBackend(t)

	resp := request(t, b, storage, logical.UpdateOperation, "exec/acp_registry/session_insert",
		map[string]interface{}{"session_id": "s1", "cwd": "/w"})
	if resp.IsError() {
		t.Fatalf("exec: %v", resp.Data)
	}
	if changes := resp.Data["changes"].(int64); changes != 1 {
		t.Fatalf("changes=%d", changes)
	}

	for i, want := range []int64{1, 2} {
		resp = request(t, b, storage, logical.UpdateOperation, "exec/acp_s1/event_append",
			map[string]interface{}{"method": "session/new"})
		if resp.IsError() {
			t.Fatalf("append %d: %v", i, resp.Data)
		}
		rows := resp.Data["rows"].([]map[string]interface{})
		if len(rows) != 1 || rows[0]["ordinal"].(int64) != want {
			t.Fatalf("append %d rows %v", i, rows)
		}
	}

	resp = request(t, b, storage, logical.ReadOperation, "query/acp_s1/events_after",
		map[string]interface{}{"after": "1"})
	if resp.IsError() {
		t.Fatalf("query: %v", resp.Data)
	}
	rows := resp.Data["rows"].([]map[string]interface{})
	if len(rows) != 1 || rows[0]["ordinal"].(int64) != 2 {
		t.Fatalf("rows %v", rows)
	}

	resp = request(t, b, storage, logical.ReadOperation, "query/acp_registry/sessions", map[string]interface{}{})
	rows = resp.Data["rows"].([]map[string]interface{})
	if len(rows) != 1 || rows[0]["session_id"] != "s1" {
		t.Fatalf("sessions %v", rows)
	}
	if _, err := os.Stat(filepath.Join(dir, "acp_s1.sqlite")); err != nil {
		t.Fatalf("named database file: %v", err)
	}
}

func TestNamed_EmptyResultIsAList(t *testing.T) {
	b, storage, _ := setupNamedBackend(t)
	resp := request(t, b, storage, logical.ReadOperation, "query/acp_registry/sessions", map[string]interface{}{})
	if resp.IsError() {
		t.Fatalf("query: %v", resp.Data)
	}
	rows, ok := resp.Data["rows"].([]map[string]interface{})
	if !ok || rows == nil || len(rows) != 0 {
		t.Fatalf("rows %#v", resp.Data["rows"])
	}
}

func TestNamed_RefusesTheOtherKind(t *testing.T) {
	b, storage, _ := setupNamedBackend(t)

	resp := request(t, b, storage, logical.ReadOperation, "query/acp_registry/session_insert",
		map[string]interface{}{"session_id": "x", "cwd": "y"})
	if !resp.IsError() {
		t.Fatalf("an exec name ran through query: %v", resp.Data)
	}
	resp = request(t, b, storage, logical.UpdateOperation, "exec/acp_registry/sessions", map[string]interface{}{})
	if !resp.IsError() {
		t.Fatalf("a query name ran through exec: %v", resp.Data)
	}
	// Control: the same names on their own kind answer, so the refusals above are by kind.
	resp = request(t, b, storage, logical.ReadOperation, "query/acp_registry/sessions", map[string]interface{}{})
	if resp.IsError() {
		t.Fatalf("control query: %v", resp.Data)
	}
}

func TestNamed_BadDatabaseName(t *testing.T) {
	b, storage, dir := setupNamedBackend(t)
	// The path regex admits a dot; the database name check must not.
	resp := request(t, b, storage, logical.ReadOperation, "query/a.b/sessions", map[string]interface{}{})
	if !resp.IsError() {
		t.Fatalf("a dotted database name was opened: %v", resp.Data)
	}
	if _, err := os.Stat(filepath.Join(dir, "a.b.sqlite")); err == nil {
		t.Fatal("a file was created for the refused name")
	}
}

func TestNamed_NeedsDirInPlainMode(t *testing.T) {
	t.Setenv(envDir, "")
	b, storage := setupBackend(t)
	resp := request(t, b, storage, logical.ReadOperation, "query/acp_x/greeting", map[string]interface{}{"lang": "en"})
	if !resp.IsError() {
		t.Fatalf("a named database opened without %s: %v", envDir, resp.Data)
	}
}

func TestNamed_ListExecs(t *testing.T) {
	b, storage, _ := setupNamedBackend(t)
	resp := request(t, b, storage, logical.ListOperation, "execs", nil)
	keys, ok := resp.Data["keys"].([]string)
	if !ok || len(keys) != 2 || keys[0] != "event_append" || keys[1] != "session_insert" {
		t.Fatalf("keys %v", resp.Data)
	}
}

func TestFactory_DefaultDBNeedsFabricMode(t *testing.T) {
	dir := t.TempDir()
	catPath := filepath.Join(dir, "catalog.hcl")
	_ = os.WriteFile(catPath, []byte(`query "q" { sql = "SELECT 1" }`), 0o600)
	t.Setenv(envCatalog, catPath)
	t.Setenv(envDB, "actor1")
	t.Setenv(envCluster, "")
	t.Setenv(envDSN, filepath.Join(dir, "x.db"))
	t.Setenv(envDir, "")
	_, err := Factory(context.Background(), &logical.BackendConfig{
		Logger:      log.NewNullLogger(),
		StorageView: &logical.InmemStorage{},
	})
	if err == nil {
		t.Fatalf("expected an error when %s is set without %s", envDB, envCluster)
	}
}

func TestLoadCatalog_ExecAndQueryNamesAreDisjoint(t *testing.T) {
	p := writeCatalog(t, `
query "same" { sql = "SELECT 1" }
exec "same" { sql = "DELETE FROM t" }
`)
	if _, err := LoadCatalog(p); err == nil {
		t.Fatal("expected an error for a name that is both a query and an exec")
	}
}

func TestLoadCatalog_SchemaFor(t *testing.T) {
	p := writeCatalog(t, `
schema "acp_registry" { sql = "CREATE TABLE a (x)" }
schema "acp_*" { sql = "CREATE TABLE b (y); CREATE INDEX b_y ON b (y)" }
schema "acp_long_*" { sql = "CREATE TABLE c (z)" }
`)
	c, err := LoadCatalog(p)
	if err != nil {
		t.Fatal(err)
	}
	if got := c.SchemaFor("acp_registry"); len(got) != 1 || got[0] != "CREATE TABLE a (x)" {
		t.Fatalf("exact %v", got)
	}
	if got := c.SchemaFor("acp_s1"); len(got) != 2 {
		t.Fatalf("prefix %v", got)
	}
	if got := c.SchemaFor("acp_long_s1"); len(got) != 1 || got[0] != "CREATE TABLE c (z)" {
		t.Fatalf("longest prefix %v", got)
	}
	if got := c.SchemaFor("other"); got != nil {
		t.Fatalf("no match %v", got)
	}
}

func TestLoadCatalog_SchemaNeedsSQLOrFile(t *testing.T) {
	if _, err := LoadCatalog(writeCatalog(t, `schema "x" { }`)); err == nil {
		t.Fatal("expected an error for a schema with neither sql nor file")
	}
	if _, err := LoadCatalog(writeCatalog(t, `schema "x" { sql = "SELECT 1" file = "a.sql" }`)); err == nil {
		t.Fatal("expected an error for a schema with both sql and file")
	}
	if _, err := LoadCatalog(writeCatalog(t, `schema "x" { file = "missing.sql" }`)); err == nil {
		t.Fatal("expected an error for a schema file that does not exist")
	}
}
