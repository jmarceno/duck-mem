package store

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Repack writes a fresh ZSTD database without changing source. Destination
// must not exist. Index storage is rebuilt, never copied from the old file,
// and a legacy HNSW index is left behind, which reclaims its leaked blocks.
// All table values and sequence positions are verified/preserved before the
// new file becomes visible. Callers must keep the result offline until swap.
func Repack(source, destination string) (err error) {
	source, err = filepath.Abs(source)
	if err != nil {
		return err
	}
	destination, err = filepath.Abs(destination)
	if err != nil {
		return err
	}
	if source == destination {
		return fmt.Errorf("repack requires a separate destination")
	}
	if _, err := os.Stat(source); err != nil {
		return err
	}
	if _, err := os.Lstat(destination); !os.IsNotExist(err) {
		return fmt.Errorf("destination must not exist: %s", destination)
	}
	dir, err := os.MkdirTemp(filepath.Dir(destination), ".duck-mem-repack-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	tmp := filepath.Join(dir, "memory.duckdb")
	sdb, err := sql.Open("duckdb", "?"+storageConfig)
	if err != nil {
		return err
	}
	sdb.SetMaxOpenConns(1)
	defer sdb.Close()
	// Loading installed extensions is sufficient; never install or initialize
	// the source database while copying it.
	for _, q := range []string{
		`LOAD duckpgq`, `LOAD fts`,
		`ATTACH ` + sqlString(source) + ` AS old_db (READ_ONLY)`,
		`ATTACH ` + sqlString(tmp) + ` AS new_db (STORAGE_VERSION '` + zstdStorageVersion + `')`,
	} {
		if _, err := sdb.Exec(q); err != nil {
			return err
		}
	}
	// Refuse a partial copy of a future schema we don't yet support.
	var extra int
	if err := sdb.QueryRow(`SELECT count(*) FROM duckdb_tables() WHERE database_name='old_db' AND schema_name<>'main'`).Scan(&extra); err != nil {
		return err
	}
	if extra != 0 {
		return fmt.Errorf("repack only supports tables in main schema")
	}
	if err := sdb.QueryRow(`SELECT count(*) FROM duckdb_views() WHERE database_name='old_db' AND NOT internal`).Scan(&extra); err != nil {
		return err
	}
	if extra != 0 {
		return fmt.Errorf("repack does not support user views")
	}
	if err := copySequences(sdb); err != nil {
		return err
	}
	if err := copyTables(sdb); err != nil {
		return err
	}
	if err := verifyRepackTables(sdb); err != nil {
		return err
	}
	// Preserve explicit indexes, including any added outside the app, except
	// the legacy HNSW index that search no longer uses.
	rows, err := sdb.Query(`SELECT index_name,sql FROM duckdb_indexes() WHERE database_name='old_db' AND schema_name='main'`)
	if err != nil {
		return err
	}
	var indexes []string
	for rows.Next() {
		var name, q string
		if err := rows.Scan(&name, &q); err != nil {
			rows.Close()
			return err
		}
		if name == "idx_messages_hnsw" {
			continue
		}
		if strings.Contains(strings.ToUpper(q), "USING HNSW") {
			rows.Close()
			return fmt.Errorf("repack does not support HNSW index %s", name)
		}
		indexes = append(indexes, q)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	for _, q := range indexes {
		if _, err := sdb.Exec(q); err != nil {
			return fmt.Errorf("rebuild index: %w", err)
		}
	}
	for _, q := range []string{`USE memory`, `CHECKPOINT new_db`, `DETACH new_db`, `DETACH old_db`} {
		if _, err := sdb.Exec(q); err != nil {
			return err
		}
	}
	if err := sdb.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmp, 0o600); err != nil {
		return err
	}
	f, err := os.Open(tmp)
	if err != nil {
		return err
	}
	err = f.Sync()
	closeErr := f.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	// A hard link publishes atomically without overwriting a racing creator.
	if err := os.Link(tmp, destination); err != nil {
		return err
	}
	d, err := os.Open(filepath.Dir(destination))
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}

func quoteIdentifier(s string) string { return `"` + strings.ReplaceAll(s, `"`, `""`) + `"` }

func copySequences(sdb *sql.DB) error {
	rows, err := sdb.Query(`SELECT sql FROM duckdb_sequences() WHERE database_name='old_db' AND schema_name='main'`)
	if err != nil {
		return err
	}
	var queries []string
	for rows.Next() {
		var q string
		if err := rows.Scan(&q); err != nil {
			rows.Close()
			return err
		}
		// DuckDB's exported SQL includes the next persisted sequence value.
		queries = append(queries, q)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	if _, err := sdb.Exec(`USE new_db`); err != nil {
		return err
	}
	for _, q := range queries {
		if _, err := sdb.Exec(q); err != nil {
			return err
		}
	}
	return nil
}

// Count plus order-independent hashes over complete rows catch changed values
// as well as omissions, without decoding the full database into Go memory.
func verifyRepackTables(sdb *sql.DB) error {
	rows, err := sdb.Query(`SELECT table_name FROM duckdb_tables() WHERE database_name='old_db' AND schema_name='main'`)
	if err != nil {
		return err
	}
	var tables []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			rows.Close()
			return err
		}
		tables = append(tables, name)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	for _, name := range tables {
		var count [2]int64
		var hash [2]string
		for i, catalog := range []string{"old_db", "new_db"} {
			q := `SELECT count(*), coalesce(bit_xor(hash(t)),0)::VARCHAR FROM ` + catalog + `.main.` + quoteIdentifier(name) + ` t`
			if err := sdb.QueryRow(q).Scan(&count[i], &hash[i]); err != nil {
				return err
			}
		}
		if count[0] != count[1] || hash[0] != hash[1] {
			return fmt.Errorf("repack verification failed for %s", name)
		}
	}
	return nil
}
