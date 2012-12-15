package main

import (
	"database/sql"
	"errors"
	"fmt"
	_ "github.com/bmizerany/pq"
	"log"
	"os"
	"path"
	"sort"
	"strconv"
	"strings"
	"time"
)

type DBVersion struct {
	VersionId int
	TStamp    time.Time
}

type Migration struct {
	Next     int    // next version, or -1 if none
	Previous int    // previous version, -1 if none
	Source   string // .go or .sql script
}

type MigrationMap struct {
	Versions   []int             // sorted slice of version keys
	Migrations map[int]Migration // sources (.sql or .go) keyed by version
	Direction  bool              // sort direction: true -> Up, false -> Down
}

func runMigrations(conf *DBConf, target int) {

	db, err := sql.Open(conf.Driver, conf.OpenStr)
	if err != nil {
		log.Fatal("couldn't open DB:", err)
	}

	current, e := ensureDBVersion(db)
	if e != nil {
		log.Fatal("couldn't get/set DB version")
	}

	mm, err := collectMigrations(path.Join(*dbFolder, "migrations"), current, target)
	if err != nil {
		log.Fatal(err)
	}

	if len(mm.Versions) == 0 {
		fmt.Printf("goose: no migrations to run. current version: %d\n", current)
		return
	}

	fmt.Printf("goose: migrating db configuration '%v', current version: %d, target: %d\n",
		conf.Name, mm.Versions[0], mm.Versions[len(mm.Versions)-1])

	for _, v := range mm.Versions {

		var numStatements int
		var e error

		filepath := mm.Migrations[v].Source

		switch path.Ext(filepath) {
		case ".go":
			numStatements, e = runGoMigration(conf, filepath, v, mm.Direction)
		case ".sql":
			numStatements, e = runSQLMigration(db, filepath, v, mm.Direction)
		}

		if e != nil {
			log.Fatalf("FAIL %v, quitting migration", e)
		}

		fmt.Printf("OK   %s (%d statements)\n", path.Base(filepath), numStatements)
	}
}

// collect all the valid looking migration scripts in the 
// migrations folder, and key them by version
func collectMigrations(dirpath string, current, target int) (mm *MigrationMap, err error) {

	dir, err := os.Open(dirpath)
	if err != nil {
		log.Fatal(err)
	}

	names, err := dir.Readdirnames(0)
	if err != nil {
		log.Fatal(err)
	}

	mm = &MigrationMap{
		Migrations: make(map[int]Migration),
	}

	// if target is the default -1,
	// we need to find the most recent possible version to target
	if target < 0 {
		target = mostRecentVersionAvailable(names)
	}

	// extract the numeric component of each migration,
	// filter out any uninteresting files,
	// and ensure we only have one file per migration version.
	for _, name := range names {

		if ext := path.Ext(name); ext != ".go" && ext != ".sql" {
			continue
		}

		v, e := numericComponent(name)
		if e != nil {
			continue
		}

		if _, ok := mm.Migrations[v]; ok {
			log.Fatalf("more than one file specifies the migration for version %d (%s and %s)",
				v, mm.Versions[v], path.Join(dirpath, name))
		}

		if versionFilter(v, current, target) {
			mm.Append(v, path.Join(dirpath, name))
		}
	}

	if len(mm.Versions) > 0 {
		mm.Sort(current < target)
	}

	return mm, nil
}

// helper to identify the most recent possible version
// within a folder of migration scripts
func mostRecentVersionAvailable(names []string) int {

	mostRecent := -1

	for _, name := range names {

		if ext := path.Ext(name); ext != ".go" && ext != ".sql" {
			continue
		}

		v, e := numericComponent(name)
		if e != nil {
			continue
		}

		if v > mostRecent {
			mostRecent = v
		}
	}

	return mostRecent
}

func versionFilter(v, current, target int) bool {

	// special case - default target value
	if target < 0 {
		return v > current
	}

	if target > current {
		return v > current && v <= target
	}

	if target < current {
		return v <= current && v >= target
	}

	return false
}

func (m *MigrationMap) Append(v int, source string) {
	m.Versions = append(m.Versions, v)
	m.Migrations[v] = Migration{
		Next:     -1,
		Previous: -1,
		Source:   source,
	}
}

func (m *MigrationMap) Sort(direction bool) {
	sort.Ints(m.Versions)

	// set direction, and reverse order if need be
	m.Direction = direction
	if m.Direction == false {
		for i, j := 0, len(m.Versions)-1; i < j; i, j = i+1, j-1 {
			m.Versions[i], m.Versions[j] = m.Versions[j], m.Versions[i]
		}
	}

	// now that we're sorted in the appropriate direction,
	// populate next and previous for each migration
	//
	// work around http://code.google.com/p/go/issues/detail?id=3117
	previousV := -1
	for _, v := range m.Versions {
		cur := m.Migrations[v]
		cur.Previous = previousV

		// if a migration exists at prev, its next is now v
		if prev, ok := m.Migrations[previousV]; ok {
			prev.Next = v
			m.Migrations[previousV] = prev
		}

		previousV = v
	}
}

// look for migration scripts with names in the form:
//  XXX_descriptivename.ext
// where XXX specifies the version number
// and ext specifies the type of migration
func numericComponent(path string) (int, error) {
	idx := strings.Index(path, "_")
	if idx < 0 {
		return 0, errors.New("no separator found")
	}
	return strconv.Atoi(path[:idx])
}

// retrieve the current version for this DB.
// Create and initialize the DB version table if it doesn't exist.
func ensureDBVersion(db *sql.DB) (int, error) {

	dbversion := int(0)
	row := db.QueryRow("SELECT version_id from goose_db_version ORDER BY tstamp DESC LIMIT 1;")

	if err := row.Scan(&dbversion); err == nil {
		return dbversion, nil
	}

	// if we failed, assume that the table didn't exist, and try to create it
	txn, err := db.Begin()
	if err != nil {
		return 0, err
	}

	// create the table and insert an initial value of 0
	create := `CREATE TABLE goose_db_version (
                version_id int NOT NULL,
                tstamp timestamp NULL default now(),
                PRIMARY KEY(tstamp)
              );`
	insert := "INSERT INTO goose_db_version (version_id) VALUES (0);"

	for _, str := range []string{create, insert} {
		if _, err := txn.Exec(str); err != nil {
			txn.Rollback()
			return 0, err
		}
	}

	return 0, txn.Commit()
}