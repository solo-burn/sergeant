package main

import (
	"database/sql"
	"log"
	"slices"

	"github.com/go-jet/jet/v2/generator/metadata"
	jetgen "github.com/go-jet/jet/v2/generator/sqlite"
	"github.com/go-jet/jet/v2/generator/template"
	"github.com/go-jet/jet/v2/sqlite"
	_ "modernc.org/sqlite"

	"sergeant/internal/database/schema"
)

func main() {
	if err := generate(); err != nil {
		log.Fatal(err)
	}
}

func generate() error {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		return err
	}
	defer func() { _ = db.Close() }()
	db.SetMaxOpenConns(1)
	// The generator needs both engines' tables in a single catalog.
	steps := slices.Concat([]string{schema.Bootstrap}, schema.BalanceMigrations,
		[]string{schema.ShadowTrial, schema.ShadowQueue, schema.Ledger})
	for _, step := range steps {
		if _, err := sqlite.RawStatement(step).Exec(db); err != nil {
			return err
		}
	}
	bindings := template.Default(sqlite.Dialect).UseSchema(func(s metadata.Schema) template.Schema {
		tables := template.DefaultSQLBuilder().UseView(func(v metadata.Table) template.ViewSQLBuilder {
			builder := template.DefaultViewSQLBuilder(v)
			builder.Skip = v.Name == "account_postings"
			return builder
		})
		return template.DefaultSchema(s).UseSQLBuilder(tables).UseModel(template.DefaultModel().ShouldSkip(true))
	})
	return jetgen.GenerateDB(db, "jet", bindings)
}
