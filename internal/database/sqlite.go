// Copyright 2019 The Moov Authors
// Use of this source code is governed by an Apache License
// license that can be found in the LICENSE file.

package database

import (
	"database/sql"
	"fmt"
	"io/ioutil"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-kit/kit/log"
	kitprom "github.com/go-kit/kit/metrics/prometheus"
	"github.com/lopezator/migrator"
	"github.com/mattn/go-sqlite3"
	stdprom "github.com/prometheus/client_golang/prometheus"
)

var (
	sqliteConnections = kitprom.NewGaugeFrom(stdprom.GaugeOpts{
		Name: "sqlite_connections",
		Help: "How many sqlite connections and what status they're in.",
	}, []string{"state"})

	sqliteVersionLogOnce sync.Once

	sqliteMigrator = migrator.New(
		execsql(
			"create_customers",
			`create table if not exists customers(customer_id primary key, first_name, middle_name, last_name, nick_name, suffix, birth_date datetime, status, email, created_at datetime, last_modified datetime, deleted_at datetime);`,
		),
		execsql(
			"create_customers_phones",
			`create table if not exists customers_phones(customer_id, number, valid, type, unique (customer_id, number) on conflict abort);`,
		),
		execsql(
			"create_customers_addresses",
			`create table if not exists customers_addresses(address_id primary key, customer_id, type, address1, address2, city, state, postal_code, country, validated, active, unique (customer_id, address1) on conflict abort);`,
		),
		execsql(
			"create_customer_metadata",
			`create table if not exists customer_metadata(customer_id, meta_key, meta_value, unique(meta_key, meta_value));`,
		),
		execsql(
			"customer_status_updates",
			`create table if not exists customer_status_updates(customer_id, future_status, comment, changed_at datetime);`,
		),
		execsql(
			"create_customer_ofac_searches",
			`create table if not exists customer_ofac_searches(customer_id, entity_id, sdn_name, sdn_type, match, created_at datetime);`,
		),
		execsql(
			"create_customer_ssn",
			`create table if not exists customer_ssn(customer_id primary key, ssn, ssn_masked, created_at datetime);`,
		),
		execsql(
			"create_documents",
			`create table if not exists documents(document_id primary key, customer_id, type, content_type, uploaded_at datetime);`,
		),
		execsql(
			"create_disclaimers",
			`create table if not exists disclaimers(disclaimer_id primary key, text, document_id, created_at datetime, deleted_at datetime);`,
		),
		execsql(
			"create_disclaimer_acceptances",
			`create table if not exists disclaimer_acceptances(disclaimer_id, customer_id, accepted_at datetime, unique(disclaimer_id, customer_id) on conflict ignore);`,
		),
		execsql(
			"create_outbound_emails",
			`create table if not exists outbound_emails(email_id, customer_id, to_address, body, created_at datetime, sent_at datetime, deleted_at datetime);`,
		),
		execsql(
			"create_email_activation_codes", // TODO(adam): do we need to HMAC and sign
			`create table if not exists email_activation_codes(email_id, hash, created_at datetime, clicked_at datetime, deleted_at datetime);`,
		),
	)
)

func getSqlitePath() string {
	path := os.Getenv("SQLITE_DB_PATH")
	if path == "" || strings.Contains(path, "..") {
		// set default if empty or trying to escape
		// don't filepath.ABS to avoid full-fs reads
		path = "customers.db"
	}
	return path
}

type sqlite struct {
	path string

	connections *kitprom.Gauge
	logger      log.Logger

	err error
}

func (s *sqlite) Connect() (*sql.DB, error) {
	if s.err != nil {
		return nil, fmt.Errorf("sqlite had error %v", s.err)
	}

	sqliteVersionLogOnce.Do(func() {
		if v, _, _ := sqlite3.Version(); v != "" {
			s.logger.Log("main", fmt.Sprintf("sqlite version %s", v))
		}
	})

	// Connect to our DB and perform a quick sanity check
	db, err := sql.Open("sqlite3", s.path)
	if err != nil {
		return nil, err
	}
	if err := db.Ping(); err != nil {
		return db, err
	}

	// Migrate our database
	if err := sqliteMigrator.Migrate(db); err != nil {
		return db, err
	}

	// Spin up metrics only after everything works
	go func() {
		t := time.NewTicker(1 * time.Minute)
		for range t.C {
			stats := db.Stats()
			s.connections.With("state", "idle").Set(float64(stats.Idle))
			s.connections.With("state", "inuse").Set(float64(stats.InUse))
			s.connections.With("state", "open").Set(float64(stats.OpenConnections))
		}
	}()

	return db, err
}

func sqliteConnection(logger log.Logger, path string) *sqlite {
	return &sqlite{
		path:        path,
		logger:      logger,
		connections: sqliteConnections,
	}
}

// TestSQLiteDB is a wrapper around sql.DB for SQLite connections designed for tests to provide
// a clean database for each testcase.  Callers should cleanup with Close() when finished.
type TestSQLiteDB struct {
	DB *sql.DB

	dir string // temp dir created for sqlite files
}

func (r *TestSQLiteDB) Close() error {
	if err := r.DB.Close(); err != nil {
		return err
	}
	return os.RemoveAll(r.dir)
}

// CreateTestSqliteDB returns a TestSQLiteDB which can be used in tests
// as a clean sqlite database. All migrations are ran on the db before.
//
// Callers should call close on the returned *TestSQLiteDB.
func CreateTestSqliteDB(t *testing.T) *TestSQLiteDB {
	dir, err := ioutil.TempDir("", "customers-sqlite")
	if err != nil {
		t.Fatalf("sqlite test: %v", err)
	}

	db, err := sqliteConnection(log.NewNopLogger(), filepath.Join(dir, "customers.db")).Connect()
	if err != nil {
		t.Fatalf("sqlite test: %v", err)
	}
	return &TestSQLiteDB{db, dir}
}

// SqliteUniqueViolation returns true when the provided error matches the SQLite error
// for duplicate entries (violating a unique table constraint).
func SqliteUniqueViolation(err error) bool {
	match := strings.Contains(err.Error(), "UNIQUE constraint failed")
	if e, ok := err.(sqlite3.Error); ok {
		return match || e.Code == sqlite3.ErrConstraint
	}
	return match
}
