package backend

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"

	"github.com/openbao/openbao/sdk/v2/framework"
	"github.com/openbao/openbao/sdk/v2/logical"
)

// dbNamePattern bounds a named database to one path segment with no dots, so
// plain mode cannot be walked out of its directory.
var dbNamePattern = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)

func paths(b *backend) []*framework.Path {
	name := &framework.FieldSchema{
		Type:        framework.TypeString,
		Description: "Registered catalog statement name.",
	}
	db := &framework.FieldSchema{
		Type:        framework.TypeString,
		Description: "Database name, opened on demand.",
	}
	return []*framework.Path{
		{
			Pattern: "query/" + framework.GenericNameRegex("name"),
			Fields:  map[string]*framework.FieldSchema{"name": name},
			Operations: map[logical.Operation]framework.OperationHandler{
				logical.ReadOperation: &framework.PathOperation{
					Callback: b.handleQueryRead,
				},
			},
			HelpSynopsis: "Run one catalog query on the default database and return its rows.",
		},
		{
			Pattern: "query/" + framework.GenericNameRegex("db") + "/" + framework.GenericNameRegex("name"),
			Fields:  map[string]*framework.FieldSchema{"db": db, "name": name},
			Operations: map[logical.Operation]framework.OperationHandler{
				logical.ReadOperation: &framework.PathOperation{
					Callback: b.handleNamedQueryRead,
				},
			},
			HelpSynopsis: "Run one catalog query on a named database and return its rows.",
		},
		{
			Pattern: "exec/" + framework.GenericNameRegex("db") + "/" + framework.GenericNameRegex("name"),
			Fields:  map[string]*framework.FieldSchema{"db": db, "name": name},
			Operations: map[logical.Operation]framework.OperationHandler{
				logical.UpdateOperation: &framework.PathOperation{
					Callback: b.handleExec,
				},
			},
			HelpSynopsis: "Run one catalog exec statement on a named database; returns its rows and change count.",
		},
		{
			Pattern: "queries/?$",
			Operations: map[logical.Operation]framework.OperationHandler{
				logical.ListOperation: &framework.PathOperation{
					Callback: b.handleQueriesList,
				},
			},
			HelpSynopsis: "List catalog query names.",
		},
		{
			Pattern: "execs/?$",
			Operations: map[logical.Operation]framework.OperationHandler{
				logical.ListOperation: &framework.PathOperation{
					Callback: b.handleExecsList,
				},
			},
			HelpSynopsis: "List catalog exec names.",
		},
	}
}

func (b *backend) handleQueryRead(ctx context.Context, req *logical.Request, data *framework.FieldData) (*logical.Response, error) {
	if b.store == nil {
		return logical.ErrorResponse("no default database is configured; use query/<db>/<name>"), nil
	}
	return b.runQuery(ctx, req, b.store, "", data.Get("name").(string))
}

func (b *backend) handleNamedQueryRead(ctx context.Context, req *logical.Request, data *framework.FieldData) (*logical.Response, error) {
	db := data.Get("db").(string)
	name := data.Get("name").(string)
	if _, isExec := b.catalog.LookupExec(name); isExec {
		return logical.ErrorResponse("%q is an exec; it does not run through query", name), nil
	}
	store, err := b.named(ctx, db)
	if err != nil {
		return logical.ErrorResponse("open %q: %v", db, err), nil
	}
	return b.runQuery(ctx, req, store, db, name)
}

func (b *backend) runQuery(ctx context.Context, req *logical.Request, store Store, db, name string) (*logical.Response, error) {
	entry, ok := b.catalog.Lookup(name)
	if !ok {
		return logical.ErrorResponse("query %q not registered", name), nil
	}
	args, err := bindArgs(entry, req.Data)
	if err != nil {
		return logical.ErrorResponse("bind args: %v", err), nil
	}
	rows, err := store.Query(ctx, entry.SQL, args...)
	if err != nil {
		return nil, fmt.Errorf("run query %q: %w", name, err)
	}
	out := map[string]interface{}{"name": name, "rows": rows}
	if db != "" {
		out["db"] = db
	}
	return &logical.Response{Data: out}, nil
}

func (b *backend) handleExec(ctx context.Context, req *logical.Request, data *framework.FieldData) (*logical.Response, error) {
	db := data.Get("db").(string)
	name := data.Get("name").(string)
	entry, ok := b.catalog.LookupExec(name)
	if !ok {
		if _, isQuery := b.catalog.Lookup(name); isQuery {
			return logical.ErrorResponse("%q is a query; it does not run through exec", name), nil
		}
		return logical.ErrorResponse("exec %q not registered", name), nil
	}
	store, err := b.named(ctx, db)
	if err != nil {
		return logical.ErrorResponse("open %q: %v", db, err), nil
	}
	args, err := bindArgs(entry, req.Data)
	if err != nil {
		return logical.ErrorResponse("bind args: %v", err), nil
	}
	rows, changes, err := store.Exec(ctx, entry.SQL, args...)
	if err != nil {
		return nil, fmt.Errorf("run exec %q: %w", name, err)
	}
	return &logical.Response{
		Data: map[string]interface{}{
			"name":    name,
			"db":      db,
			"rows":    rows,
			"changes": changes,
		},
	}, nil
}

func (b *backend) handleQueriesList(_ context.Context, _ *logical.Request, _ *framework.FieldData) (*logical.Response, error) {
	return logical.ListResponse(b.catalog.Names()), nil
}

func (b *backend) handleExecsList(_ context.Context, _ *logical.Request, _ *framework.FieldData) (*logical.Response, error) {
	return logical.ListResponse(b.catalog.ExecNames()), nil
}

func bindArgs(entry *CatalogEntry, in map[string]interface{}) ([]interface{}, error) {
	out := make([]interface{}, 0, len(entry.Args))
	for _, name := range entry.Args {
		v, ok := in[name]
		if !ok {
			return nil, fmt.Errorf("missing arg %q", name)
		}
		out = append(out, bindable(v))
	}
	return out, nil
}

// bindable turns the forms OpenBao hands a plugin (JSON numbers from a write,
// single-value lists from a query string) into what database/sql binds.
func bindable(v interface{}) interface{} {
	switch x := v.(type) {
	case json.Number:
		if i, err := x.Int64(); err == nil {
			return i
		}
		if f, err := x.Float64(); err == nil {
			return f
		}
		return x.String()
	case []string:
		if len(x) == 1 {
			return x[0]
		}
	}
	return v
}
