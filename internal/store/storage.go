package store

import (
	"database/sql"
	"errors"
	"fmt"
	"os"
	"slices"
	"strconv"
	"strings"
)

// DuckDB only writes ZSTD segments into files whose storage version is
// v1.2.0 or newer. On older files force_compression='zstd' is silently
// ignored, and CHECKPOINT never rewrites row groups that did not change,
// so existing data can only be recompressed by copying it into a new file.
const (
	zstdStorageVersion = "v1.2.0"
	storageConfig      = "storage_compatibility_version=" + zstdStorageVersion + "&force_compression=zstd"
)

func dsn(path string) string { return path + "?" + storageConfig }

// upgradeStorage rewrites a database file created with a storage version
// that cannot hold ZSTD segments. Every table is copied into a new file
// with forced ZSTD, row counts are checked, and the new file replaces the
// old one. Indexes are not copied: init recreates them, and the catalog
// SQL of the HNSW index has lost its metric option. A missing file or one
// already on a ZSTD-capable version is left untouched.
func upgradeStorage(path string) error {
	if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
		return nil
	}
	sdb, err := sql.Open("duckdb", "?"+storageConfig)
	if err != nil {
		return err
	}
	sdb.SetMaxOpenConns(1)
	defer sdb.Close()
	// vss must be loaded so the HNSW index can be copied.
	if err := loadExtensions(sdb); err != nil {
		return err
	}
	if _, err := sdb.Exec(`ATTACH ` + sqlString(path) + ` AS old_db`); err != nil {
		return err
	}
	var version string
	if err := sdb.QueryRow(`SELECT tags['storage_version'] FROM duckdb_databases() WHERE database_name='old_db'`).Scan(&version); err != nil {
		return err
	}
	if supportsZstd(version) {
		return nil
	}
	tmp := path + ".zstd-tmp"
	_ = os.Remove(tmp)
	_ = os.Remove(tmp + ".wal")
	// Fold the WAL into the old file so nothing is left behind in it.
	for _, q := range []string{
		`CHECKPOINT old_db`,
		`ATTACH ` + sqlString(tmp) + ` AS new_db (STORAGE_VERSION '` + zstdStorageVersion + `')`,
	} {
		if _, err := sdb.Exec(q); err != nil {
			_ = os.Remove(tmp)
			return fmt.Errorf("zstd storage upgrade: %s: %w", q, err)
		}
	}
	if err := copyTables(sdb); err != nil {
		_, _ = sdb.Exec(`DETACH new_db`)
		_ = os.Remove(tmp)
		_ = os.Remove(tmp + ".wal")
		return fmt.Errorf("zstd storage upgrade: %w", err)
	}
	for _, q := range []string{`USE memory`, `CHECKPOINT new_db`, `DETACH new_db`, `DETACH old_db`} {
		if _, err := sdb.Exec(q); err != nil {
			_ = os.Remove(tmp)
			return fmt.Errorf("zstd storage upgrade: %s: %w", q, err)
		}
	}
	if err := os.Rename(tmp, path); err != nil {
		return err
	}
	// The old file was checkpointed, so any WAL left is empty; a stale one
	// must not be replayed against the new file.
	if err := os.Remove(path + ".wal"); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

// supportsZstd reports whether a storage_version tag such as "v1.0.0+"
// is at least zstdStorageVersion.
func supportsZstd(version string) bool {
	parts := strings.SplitN(strings.TrimSuffix(strings.TrimPrefix(version, "v"), "+"), ".", 3)
	if len(parts) < 2 {
		return false
	}
	major, err1 := strconv.Atoi(parts[0])
	minor, err2 := strconv.Atoi(parts[1])
	if err1 != nil || err2 != nil {
		return false
	}
	return major > 1 || major == 1 && minor >= 2
}

// copyTables recreates every old_db table in new_db, referenced tables
// before the tables whose foreign keys point at them, and checks that each
// copy has the same row count. (COPY FROM DATABASE ignores that order and
// fails on the foreign keys.)
func copyTables(sdb *sql.DB) error {
	tables := map[string]string{}
	rows, err := sdb.Query(`SELECT table_name, sql FROM duckdb_tables() WHERE database_name='old_db' AND schema_name='main'`)
	if err != nil {
		return err
	}
	for rows.Next() {
		var name, create string
		if err := rows.Scan(&name, &create); err != nil {
			rows.Close()
			return err
		}
		tables[name] = create
	}
	rows.Close()
	deps := map[string][]string{}
	rows, err = sdb.Query(`SELECT table_name, referenced_table FROM duckdb_constraints()
		WHERE database_name='old_db' AND schema_name='main' AND constraint_type='FOREIGN KEY'`)
	if err != nil {
		return err
	}
	for rows.Next() {
		var t, ref string
		if err := rows.Scan(&t, &ref); err != nil {
			rows.Close()
			return err
		}
		if ref != t {
			deps[t] = append(deps[t], ref)
		}
	}
	rows.Close()

	if _, err := sdb.Exec(`USE new_db`); err != nil {
		return err
	}
	copied := map[string]bool{}
	for len(copied) < len(tables) {
		progress := false
		for name, create := range tables {
			if copied[name] || slices.ContainsFunc(deps[name], func(d string) bool { return !copied[d] }) {
				continue
			}
			quoted := `"` + strings.ReplaceAll(name, `"`, `""`) + `"`
			if _, err := sdb.Exec(create); err != nil {
				return fmt.Errorf("%s: %w", create, err)
			}
			var a, b int64
			if err := sdb.QueryRow(`SELECT count(*) FROM old_db.main.` + quoted).Scan(&a); err != nil {
				return err
			}
			if _, err := sdb.Exec(`INSERT INTO new_db.main.` + quoted + ` SELECT * FROM old_db.main.` + quoted); err != nil {
				return fmt.Errorf("copy %s: %w", name, err)
			}
			if err := sdb.QueryRow(`SELECT count(*) FROM new_db.main.` + quoted).Scan(&b); err != nil {
				return err
			}
			if a != b {
				return fmt.Errorf("%s has %d rows, copy has %d", name, a, b)
			}
			copied[name] = true
			progress = true
		}
		if !progress {
			return fmt.Errorf("foreign keys between tables form a cycle")
		}
	}
	return nil
}
