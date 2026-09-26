package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	// The pure-Go SQLite driver, BSD-3-Clause, so the orchestrator image needs
	// no cgo and no shared library.
	_ "modernc.org/sqlite"
)

// The states a VM is in as far as the orchestrator is concerned. They are what
// it did to the VM last, not what the VM is doing: the bucket holds a VM's
// authority and the host running it holds its guest, and this table is only the
// control plane's account of where things went.
const (
	// stateCreating and stateRecovering are a VM that is being started
	// somewhere, by a create or a fork, or reopened from its last checkpoint.
	stateCreating   = "creating"
	stateRecovering = "recovering"
	// stateStarting is a stopped VM being opened on a host again. It is the same
	// operation a recovery runs and it is named apart from one because what it
	// says about the deployment is the opposite: a VM that was stopped on
	// purpose is coming back, rather than one whose host was lost being repaired.
	stateStarting = "starting"
	// stateRunning is a host that answers and says it runs the VM.
	stateRunning = "running"
	// stateMigrating is a VM between two hosts, with the pair recorded.
	stateMigrating = "migrating"
	// stateStopped is a VM no live host runs: its state is its last checkpoint,
	// whether it was never started here, or its host was killed. Recover opens it.
	stateStopped = "stopped"
)

// vmRecord is one row of the VM table.
type vmRecord struct {
	ID    string `json:"id"`
	Host  string `json:"host,omitempty"`
	State string `json:"state"`
	// From and To are the hosts of a migration in flight, and empty otherwise.
	// A drain reports its start before the orchestrator has chosen a
	// destination, so To can be empty while From is not.
	From     string `json:"from,omitempty"`
	To       string `json:"to,omitempty"`
	Template string `json:"template,omitempty"`
	// Parent is the VM this one was forked from, empty for one that was
	// created or recovered.
	Parent string `json:"parent,omitempty"`
	// Memory is the guest RAM this VM has, which is what a placement admits it
	// against. It starts as its template's, written down at a create and
	// carried to a child at a fork, and a cold start changes it: from then on a
	// VM's committed RAM is its own and not its template's. Zero is a VM
	// nothing wrote one down for, which is admitted against its template as it
	// was before, and against nothing when the table has no template either.
	Memory  uint64    `json:"memory,omitempty"`
	Updated time.Time `json:"updated"`
}

// schema is the whole table. Nothing here is authority: it is rebuilt from a
// survey and the bucket whenever the orchestrator starts, so a schema that has
// changed under an old file costs nothing but that rebuild.
//
// There is no table of hosts. Where the hosts are is the Kubernetes API's
// answer and what they run is their own, both of them read fresh for every
// request that needs them; a copy here would be a second account of the same
// thing that nothing reads and that is wrong the moment a pod moves.
const schema = `
CREATE TABLE IF NOT EXISTS vms (
	id        TEXT PRIMARY KEY,
	host      TEXT NOT NULL DEFAULT '',
	state     TEXT NOT NULL,
	from_host TEXT NOT NULL DEFAULT '',
	to_host   TEXT NOT NULL DEFAULT '',
	template  TEXT NOT NULL DEFAULT '',
	parent    TEXT NOT NULL DEFAULT '',
	memory    INTEGER NOT NULL DEFAULT 0,
	updated   INTEGER NOT NULL
);
DROP TABLE IF EXISTS hosts;`

// table is where the orchestrator writes down which host every VM is on and
// what it was last asked to do with it. It is not authority for anything —
// every answer it gives can be rebuilt by surveying the hosts and listing the
// bucket, which is what a reconcile does — and it exists so that a VM can be
// addressed without surveying first, and so that a migration or a drain that is
// in flight is visible while it is happening.
type table struct {
	db *sql.DB
	// aging is how long a row is taken at its word, inFlightFor unless a test
	// shortens it.
	aging time.Duration
}

// openTable opens the table at path, creating it if this is a fresh node. The
// connection pool is one: SQLite serialises writers anyway, and one connection
// is what keeps a write from failing on a busy file rather than waiting.
func openTable(ctx context.Context, path string) (*table, error) {
	db, err := sql.Open("sqlite", path+"?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)")
	if err != nil {
		return nil, fmt.Errorf("opening the VM table at %s: %w", path, err)
	}
	db.SetMaxOpenConns(1)
	if _, err := db.ExecContext(ctx, schema); err != nil {
		return nil, errors.Join(fmt.Errorf("creating the VM table at %s", path), err, db.Close())
	}
	if err := addColumns(ctx, db); err != nil {
		return nil, errors.Join(fmt.Errorf("creating the VM table at %s", path), err, db.Close())
	}
	return &table{db: db}, nil
}

// addColumns adds the columns a file written before them does not have. The
// schema above creates the table a fresh node needs; a file that is already
// there keeps the columns it was made with, and every read would then name one
// that is not in it. Nothing here is authority — a reconcile rebuilds every row
// from a survey and the bucket — so a column added to an old file starts at its
// default and is correct again at the next reconcile.
//
// A column the file already has is what the table is for, so that is not an
// error: SQLite has no ADD COLUMN IF NOT EXISTS and says what it found instead.
func addColumns(ctx context.Context, db *sql.DB) error {
	for _, statement := range []string{
		`ALTER TABLE vms ADD COLUMN memory INTEGER NOT NULL DEFAULT 0`,
	} {
		if _, err := db.ExecContext(ctx, statement); err != nil &&
			!strings.Contains(err.Error(), "duplicate column name") {
			return fmt.Errorf("%s: %w", statement, err)
		}
	}
	return nil
}

func (t *table) Close() error { return t.db.Close() }

// Record writes what one VM is now. It is called on every create, fork,
// migration, recovery and drain report, before the operation and again after
// it, so a table read during a long migration shows it in flight.
func (t *table) Record(ctx context.Context, record vmRecord) error {
	if record.Updated.IsZero() {
		record.Updated = time.Now()
	}
	_, err := t.db.ExecContext(ctx, `
		INSERT INTO vms (id, host, state, from_host, to_host, template, parent, memory, updated)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET
			host = excluded.host, state = excluded.state,
			from_host = excluded.from_host, to_host = excluded.to_host,
			-- A record written by an operation that does not know the template,
			-- the parent or the memory keeps the one the table already has.
			template = CASE WHEN excluded.template = '' THEN vms.template ELSE excluded.template END,
			parent = CASE WHEN excluded.parent = '' THEN vms.parent ELSE excluded.parent END,
			memory = CASE WHEN excluded.memory = 0 THEN vms.memory ELSE excluded.memory END,
			updated = excluded.updated`,
		record.ID, record.Host, record.State, record.From, record.To, record.Template,
		record.Parent, record.Memory, record.Updated.UnixMilli())
	if err != nil {
		return fmt.Errorf("recording %s as %s: %w", record.ID, record.State, err)
	}
	return nil
}

// Forget removes a VM that has been deleted.
func (t *table) Forget(ctx context.Context, id string) error {
	if _, err := t.db.ExecContext(ctx, `DELETE FROM vms WHERE id = ?`, id); err != nil {
		return fmt.Errorf("forgetting %s: %w", id, err)
	}
	return nil
}

// VM reads one row. A VM the table has never heard of is not an error: the
// caller surveys instead.
func (t *table) VM(ctx context.Context, id string) (vmRecord, bool, error) {
	rows, err := t.query(ctx, `SELECT id, host, state, from_host, to_host, template, parent, memory, updated
		FROM vms WHERE id = ?`, id)
	if err != nil || len(rows) == 0 {
		return vmRecord{}, false, err
	}
	return rows[0], true, nil
}

// VMs reads the whole table, in identity order, which is creation order: the
// identities are ULIDs.
func (t *table) VMs(ctx context.Context) ([]vmRecord, error) {
	return t.query(ctx, `SELECT id, host, state, from_host, to_host, template, parent, memory, updated
		FROM vms ORDER BY id`)
}

// querier is whatever the rows are read through: the database, or the
// transaction a reconcile is running in. With one connection in the pool, a
// read that went to the database while a transaction held it would wait for a
// connection the transaction is never going to give back.
type querier interface {
	QueryContext(ctx context.Context, statement string, args ...any) (*sql.Rows, error)
}

func (t *table) query(ctx context.Context, statement string, args ...any) ([]vmRecord, error) {
	return query(ctx, t.db, statement, args...)
}

func query(ctx context.Context, from querier, statement string, args ...any) ([]vmRecord, error) {
	rows, err := from.QueryContext(ctx, statement, args...)
	if err != nil {
		return nil, fmt.Errorf("reading the VM table: %w", err)
	}
	defer rows.Close()
	var records []vmRecord
	for rows.Next() {
		var record vmRecord
		var updated int64
		if err := rows.Scan(&record.ID, &record.Host, &record.State,
			&record.From, &record.To, &record.Template, &record.Parent, &record.Memory,
			&updated); err != nil {
			return nil, fmt.Errorf("reading the VM table: %w", err)
		}
		record.Updated = time.UnixMilli(updated)
		records = append(records, record)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("reading the VM table: %w", err)
	}
	return records, nil
}

// surveyed is what one survey of the deployment found: the host pods the
// Kubernetes API listed, which of them said what they are running, and which
// host reports each VM.
//
// The two lists of hosts are different evidence. A pod that is listed and did
// not answer says nothing about what it runs — its guests may be perfectly well
// behind one dropped request — so its rows are left where they are. A pod the
// Kubernetes API no longer lists runs nothing, because there is no longer a
// process to run it, and a row still naming it is the deployment reporting a VM
// on a host it does not have.
type surveyed struct {
	listed   []string
	answered []string
	running  map[string]string
}

// accounted reports a host whose silence about a VM means the VM is not running
// there: one that answered this survey, or one the cluster no longer has.
func (s surveyed) accounted(host string) bool {
	return slices.Contains(s.answered, host) || !slices.Contains(s.listed, host)
}

// Observe writes down one survey.
//
// A VM the table had on a host that accounted for itself and no longer reports
// it is stopped.
func (t *table) Observe(ctx context.Context, found surveyed) error {
	return t.reconcile(ctx, found, nil, false)
}

// Reconcile is Observe plus the bucket's own list of VMs, which is the only
// thing that can tell a VM that was deleted from one whose host is gone. It is
// what rebuilds the table when the orchestrator starts and on its own timer
// after that.
func (t *table) Reconcile(ctx context.Context, found surveyed, inBucket []string) error {
	return t.reconcile(ctx, found, inBucket, true)
}

// inFlight are the states in which no host reporting a VM means nothing is
// wrong: the operation that will make one report it has not finished.
var inFlight = map[string]bool{stateCreating: true, stateRecovering: true, stateStarting: true,
	stateMigrating: true}

// inFlightFor is how long a row is taken at its word. An operation that died
// with the process driving it — an orchestrator killed mid-migration — leaves a
// row saying it is still going, and everything that reads the table defers to
// that: the source's pages are left served, the reconcile leaves the row where
// it is, and a recovery is refused. Without a bound all three stay open for as
// long as the deployment runs.
//
// It is under the bound a host puts on one handover of its own, four checkpoint
// intervals, so a row stops holding a handover open before the host holding
// those pages gives them up by itself. It bounds how long a row outlives the
// process that wrote it, not how long an operation may take: one that runs
// longer writes its rows again while it runs, which is what a flight is.
const inFlightFor = 2 * time.Minute

// stillInFlight reports a row whose operation may yet finish: one in an
// in-flight state that is young enough to believe.
func (t *table) stillInFlight(row vmRecord) bool {
	return inFlight[row.State] && time.Since(row.Updated) < t.believed()
}

// believed is how long this table takes a row at its word.
func (t *table) believed() time.Duration {
	if t.aging > 0 {
		return t.aging
	}
	return inFlightFor
}

func (t *table) reconcile(ctx context.Context, found surveyed,
	inBucket []string, fromBucket bool) error {
	now := time.Now()
	transaction, err := t.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("reconciling the table: %w", err)
	}
	defer func() { _ = transaction.Rollback() }()

	existing, err := query(ctx, transaction, `SELECT id, host, state, from_host, to_host, template, parent, memory, updated FROM vms`)
	if err != nil {
		return err
	}
	known := map[string]vmRecord{}
	for _, row := range existing {
		known[row.ID] = row
	}
	settled := map[string]bool{}
	for id, host := range found.running {
		settled[id] = true
		if err := record(ctx, transaction, vmRecord{ID: id, Host: host, State: stateRunning,
			Template: known[id].Template, Parent: known[id].Parent, Memory: known[id].Memory,
			Updated: now}); err != nil {
			return err
		}
	}
	// A VM whose own host accounted for itself without naming it has no host
	// running it.
	for id, row := range known {
		if settled[id] || t.stillInFlight(row) || row.Host == "" || !found.accounted(row.Host) {
			continue
		}
		settled[id] = true
		if err := record(ctx, transaction, vmRecord{ID: id, State: stateStopped,
			Template: row.Template, Parent: row.Parent, Memory: row.Memory, Updated: now}); err != nil {
			return err
		}
	}
	if fromBucket {
		for _, id := range inBucket {
			if settled[id] || t.stillInFlight(known[id]) {
				continue
			}
			settled[id] = true
			if err := record(ctx, transaction, vmRecord{ID: id, State: stateStopped,
				Template: known[id].Template, Parent: known[id].Parent, Memory: known[id].Memory,
				Updated: now}); err != nil {
				return err
			}
		}
		// Nothing runs it and the bucket has no record of it: it is deleted,
		// by this orchestrator or by another.
		for id, row := range known {
			if settled[id] || t.stillInFlight(row) {
				continue
			}
			if _, err := transaction.ExecContext(ctx, `DELETE FROM vms WHERE id = ?`, id); err != nil {
				return fmt.Errorf("forgetting the deleted %s: %w", id, err)
			}
		}
	}
	if err := transaction.Commit(); err != nil {
		return fmt.Errorf("reconciling the table: %w", err)
	}
	return nil
}

// record is Record inside a transaction.
func record(ctx context.Context, tx *sql.Tx, row vmRecord) error {
	_, err := tx.ExecContext(ctx, `
		INSERT INTO vms (id, host, state, from_host, to_host, template, parent, memory, updated)
		VALUES (?, ?, ?, '', '', ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET
			host = excluded.host, state = excluded.state,
			from_host = '', to_host = '',
			template = CASE WHEN excluded.template = '' THEN vms.template ELSE excluded.template END,
			parent = CASE WHEN excluded.parent = '' THEN vms.parent ELSE excluded.parent END,
			memory = CASE WHEN excluded.memory = 0 THEN vms.memory ELSE excluded.memory END,
			updated = excluded.updated`,
		row.ID, row.Host, row.State, row.Template, row.Parent, row.Memory, row.Updated.UnixMilli())
	if err != nil {
		return fmt.Errorf("recording %s as %s: %w", row.ID, row.State, err)
	}
	return nil
}
