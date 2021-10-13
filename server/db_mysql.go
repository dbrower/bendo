package server

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"log"
	"time"

	// no _ in import mysql since we need mysql.NullTime
	"github.com/BurntSushi/migration"
	raven "github.com/getsentry/raven-go"
	"github.com/go-sql-driver/mysql"

	"github.com/ndlib/bendo/items"
)

// This file contains code implementing various caching interfaces to use
// MySQL as a storage medium.

// MsqlCache implements the items.ItemCache interface and the FixityDB interface
// using MySQL as the backing store.
type MsqlCache struct {
	db *sql.DB
}

var _ items.ItemCache = &MsqlCache{}
var _ FixityDB = &MsqlCache{}
var _ BlobDB = &MsqlCache{}

// List of migrations to perform. Add new ones to the end.
// DO NOT change the order of items already in this list.
var mysqlMigrations = []migration.Migrator{
	mysqlschema1,
	mysqlschema2,
	mysqlschema3,
	mysqlschema4,
}

// Adapt the schema versioning for MySQL

var mysqlVersioning = dbVersion{
	GetSQL:    `SELECT max(version) FROM migration_version`,
	SetSQL:    `INSERT INTO migration_version (version, applied) VALUES (?, now())`,
	CreateSQL: `CREATE TABLE migration_version (version INTEGER, applied datetime)`,
}

// NewMysqlCache connects to a MySQL database and returns an item satisifying
// both the ItemCache and FixityDB interfaces.
func NewMysqlCache(dial string) (*MsqlCache, error) {
	db, err := migration.OpenWith(
		"mysql",
		dial,
		mysqlMigrations,
		mysqlVersioning.Get,
		mysqlVersioning.Set)
	if err != nil {
		log.Println("Open Mysql", err)
		return nil, err
	}
	return &MsqlCache{db: db}, nil
}

// Lookup returns a cached Item, if one exists in the database.
// Otherwise it returns nil.
func (ms *MsqlCache) Lookup(id string) *items.Item {
	const dbLookup = `SELECT value FROM items WHERE item = ? LIMIT 1`

	var value string
	err := ms.db.QueryRow(dbLookup, id).Scan(&value)
	if err != nil {
		if err != sql.ErrNoRows {
			// some kind of error...treat it as a miss
			log.Println("Item Cache: ", err)
			raven.CaptureError(err, nil)
		}
		return nil
	}
	// unserialize the json string
	var thisItem = new(items.Item)
	err = json.Unmarshal([]byte(value), thisItem)
	if err != nil {
		log.Println("Item Cache: error in lookup:", err)
		raven.CaptureError(err, nil)
		return nil
	}
	return thisItem
}

// Set adds the given item to the cache under the key id.
func (ms *MsqlCache) Set(id string, thisItem *items.Item) {
	var created, modified time.Time
	var size int64
	for i := range thisItem.Blobs {
		size += thisItem.Blobs[i].Size
	}
	if len(thisItem.Versions) > 0 {
		created = thisItem.Versions[0].SaveDate
		modified = thisItem.Versions[len(thisItem.Versions)-1].SaveDate
	}
	value, err := json.Marshal(thisItem)
	if err != nil {
		log.Println("Item Cache:", err)
		raven.CaptureError(err, nil)
		return
	}
	stmt := `INSERT INTO items (item, created, modified, size, value) VALUES (?, ?, ?, ?, ?) ON DUPLICATE KEY UPDATE created=?, modified=?, size=?, value=?`

	_, err = ms.db.Exec(stmt, id, created, modified, size, value, created, modified, size, value)
	if err != nil {
		log.Printf("Item Cache: %s", err.Error())
		return
	}
	ms.IndexItem(id, thisItem)
}

func (ms *MsqlCache) FindBlob(item string, blobid int) (*items.Blob, error) {
	const query = `
			SELECT size, bundle, created, creator, MD5, SHA256, mimetype,
				deleted, deleter, deletenote
			FROM blobs
			WHERE item = ? AND blobid = ?
			LIMIT 1`

	var b items.Blob
	var dDeleted mysql.NullTime
	var dSave mysql.NullTime
	err := ms.db.QueryRow(query, item, blobid).Scan(&b.Size, &b.Bundle, &dSave, &b.Creator, &b.MD5, &b.SHA256, &b.MimeType, &dDeleted, &b.Deleter, &b.DeleteNote)
	b.ID = items.BlobID(blobid)
	if dSave.Valid {
		b.SaveDate = dSave.Time
	}
	if dDeleted.Valid {
		b.DeleteDate = dDeleted.Time
	}

	if err == sql.ErrNoRows {
		return nil, nil
	}
	return &b, err
}

func (ms *MsqlCache) getMaxBlob(item string) (int, error) {
	const maxblob = `
			SELECT max(blobid)
			FROM blobs
			WHERE item = ?`

	var blob sql.NullInt64
	err := ms.db.QueryRow(maxblob, item).Scan(&blob)
	if err == sql.ErrNoRows {
		err = nil
	}
	if blob.Valid {
		return int(blob.Int64), err
	}
	return 0, err
}

func (ms *MsqlCache) getMaxVersion(item string) (int, error) {
	const maxversion = `
			SELECT max(versionid)
			FROM versions
			WHERE item = ?`

	var version sql.NullInt64
	err := ms.db.QueryRow(maxversion, item).Scan(&version)
	if err == sql.ErrNoRows {
		err = nil
	}
	if version.Valid {
		return int(version.Int64), err
	}
	return 0, nil
}

func (ms *MsqlCache) FindBlobBySlot(item string, version int, slot string) (*items.Blob, error) {
	if version == 0 {
		var err error
		version, err = ms.getMaxVersion(item)
		if err != nil || version == 0 {
			return nil, err
		}
	}
	// we do the resolution in two steps for simplicity
	const query = `
			SELECT blobid
			FROM slots
			WHERE item = ? AND versionid = ? AND name = ?
			LIMIT 1`
	var bid int
	err := ms.db.QueryRow(query, item, version, slot).Scan(&bid)
	if bid == 0 || err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return ms.FindBlob(item, bid)
}

// IndexItem adds row entries for every version, slot, and blob
// for the given item. It is ok if some pieces are already in the tables.
func (ms *MsqlCache) IndexItem(item string, thisItem *items.Item) error {
	// first update blobs. This isn't perfect. While a blob record doesn't
	// change often, it is possible. The Bundle id, the mime type or the deleted
	// flags could be changed. Not sure how to handle that. It seems inefficient
	// to check the records already in the table. maybe we need a way to track
	// changes to blob records so we can only update those.

	maxblob, err := ms.getMaxBlob(item)
	if err != nil {
		return err
	}
	maxversion, err := ms.getMaxVersion(item)
	if err != nil {
		return err
	}

	tx, err := ms.db.Begin()
	if err != nil {
		return err
	}

	// add/update blobs
	for _, blob := range thisItem.Blobs {
		var dd mysql.NullTime
		if !blob.DeleteDate.IsZero() {
			dd.Time = blob.DeleteDate
			dd.Valid = true
		}
		if int(blob.ID) > maxblob {
			const insertblob = `INSERT INTO blobs
			(item, blobid, size, bundle, created, creator, MD5, SHA256,
			mimetype, deleted, deleter, deletenote)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`
			_, err = tx.Exec(insertblob, item, blob.ID, blob.Size, blob.Bundle,
				blob.SaveDate, blob.Creator, blob.MD5, blob.SHA256,
				blob.MimeType, dd, blob.Deleter, blob.DeleteNote)
		} else {
			const updateblob = `UPDATE blobs SET
					bundle = ?,
					mimetype = ?,
					deleted = ?,
					deleter = ?,
					deletenote = ?
				WHERE item = ? AND blobid = ?`
			_, err = tx.Exec(updateblob, blob.Bundle, blob.MimeType,
				dd, blob.Deleter, blob.DeleteNote, item, blob.ID)
		}
		if err != nil {
			tx.Rollback()
			return err
		}
	}

	// update the version and slot tables. These should not change once created,
	// so we do not have the update problem as the blobs do
	for _, v := range thisItem.Versions {
		if v.ID <= items.VersionID(maxversion) {
			continue // this version has already been indexed
		}

		const insertver = `INSERT INTO versions
				(item, versionid, created, creator, note)
				VALUES (?, ?, ?, ?, ?)`
		_, err := tx.Exec(insertver, item, v.ID, v.SaveDate, v.Creator, v.Note)
		if err != nil {
			tx.Rollback()
			return err
		}

		for slot, bid := range v.Slots {
			const insertslot = `INSERT INTO slots
					(item, versionid, blobid, name)
					VALUES (?, ?, ?, ?)`
			_, err := tx.Exec(insertslot, item, v.ID, bid, slot)
			if err != nil {
				tx.Rollback()
				return err
			}
		}
	}
	return tx.Commit()
}

// NextFixity returns the earliest scheduled fixity record
// that is before the cutoff time. If there is no such record
// it returns 0
func (mc *MsqlCache) NextFixity(cutoff time.Time) int64 {
	const query = `
		SELECT id
		FROM fixity
		WHERE status = "scheduled" AND scheduled_time <= ?
		ORDER BY scheduled_time
		LIMIT 1`

	var id int64
	err := mc.db.QueryRow(query, cutoff).Scan(&id)
	if err == sql.ErrNoRows {
		return 0
	} else if err != nil {
		log.Println("nextfixity", err)
		raven.CaptureError(err, nil)
		return 0
	}
	return id
}

// GetFixity
func (mc *MsqlCache) GetFixity(id int64) *Fixity {
	const query = `
		SELECT id, item, scheduled_time, status, notes
		FROM fixity
		WHERE id = ?
		LIMIT 1`

	var rec Fixity
	var when mysql.NullTime

	err := mc.db.QueryRow(query, id).Scan(&rec.ID, &rec.Item, &when, &rec.Status, &rec.Notes)
	if err == sql.ErrNoRows {
		return nil
	} else if err != nil {
		log.Println("GetFixtyByID  MySQL queryrow", err)
		raven.CaptureError(err, nil)
		return nil
	}
	// Handle for null time value
	if when.Valid {
		rec.ScheduledTime = when.Time
	}
	return &rec
}

// SearchFixity
func (mc *MsqlCache) SearchFixity(start, end time.Time, item string, status string) []*Fixity {
	query, args := buildQuery(start, end, item, status)
	var results []*Fixity

	rows, err := mc.db.Query(query, args...)
	if err == sql.ErrNoRows {
		// no next record
		return nil
	} else if err != nil {
		log.Println("GetFixity Query MySQL", err)
		raven.CaptureError(err, nil)
		return nil
	}
	defer rows.Close()

	for rows.Next() {
		var rec = new(Fixity)
		var when mysql.NullTime
		err = rows.Scan(&rec.ID, &rec.Item, &when, &rec.Status, &rec.Notes)
		if err != nil {
			log.Println("GetFixity Scan MySQL", err)
			raven.CaptureError(err, nil)
			continue
		}
		if when.Valid {
			rec.ScheduledTime = when.Time
		}
		results = append(results, rec)
	}
	return results
}

// construct an return an sql query and parameter list, using the parameters passed
func buildQuery(start, end time.Time, item string, status string) (string, []interface{}) {
	var query bytes.Buffer
	// The mysql driver does not have positional parameters, so we build the
	// parameter list in parallel to the query.
	var args []interface{}
	query.WriteString("SELECT id, item, scheduled_time, status, notes FROM fixity")

	conjunction := " WHERE "

	if !start.IsZero() {
		query.WriteString(conjunction + "scheduled_time >= ?")
		conjunction = " AND "
		args = append(args, start)
	}
	if !end.IsZero() {
		query.WriteString(conjunction + "scheduled_time <= ?")
		conjunction = " AND "
		args = append(args, end)
	}
	if item != "" {
		query.WriteString(conjunction + "item = ?")
		conjunction = " AND "
		args = append(args, item)
	}
	if status != "" {
		query.WriteString(conjunction + "status = ?")
		args = append(args, status)
	}
	query.WriteString(" ORDER BY scheduled_time")
	return query.String(), args
}

// UpdateFixity updates or creates the given fixity record. The record is created if
// ID is == 0. Otherwise the given record is updated so long as
// the record in the database has status "scheduled".
// The ID of the new or updated record is returned.
func (mc *MsqlCache) UpdateFixity(record Fixity) (int64, error) {
	if record.Status == "" {
		record.Status = "scheduled"
	}

	if record.ID == 0 {
		// new record
		const stmt = `INSERT INTO fixity (item, scheduled_time, status, notes) VALUES (?,?,?,?)`

		result, err := mc.db.Exec(stmt, record.Item, record.ScheduledTime, record.Status, record.Notes)
		var id int64
		if err == nil {
			id, _ = result.LastInsertId()
		}
		return id, err
	}

	// update existing record
	const stmt = `
		UPDATE fixity
		SET item = ?, status = ?, notes = ?, scheduled_time = ?
		WHERE id = ? and status = "scheduled"
		LIMIT 1`

	_, err := mc.db.Exec(stmt, record.Item, record.Status, record.Notes, record.ScheduledTime, record.ID)
	return record.ID, err
}

func (mc *MsqlCache) DeleteFixity(id int64) error {
	const stmt = `DELETE FROM fixity WHERE id = ? AND status = "scheduled"`
	_, err := mc.db.Exec(stmt, id)
	return err
}

// LookupCheck will return the time of the earliest scheduled fixity
// check for the given item. If there is no pending fixity check for
// the item, it returns the zero time.
func (mc *MsqlCache) LookupCheck(item string) (time.Time, error) {
	const query = `
		SELECT scheduled_time
		FROM fixity
		WHERE item = ? AND status = "scheduled"
		ORDER BY scheduled_time
		LIMIT 1`

	var when mysql.NullTime
	err := mc.db.QueryRow(query, item).Scan(&when)
	if err == sql.ErrNoRows {
		err = nil
	}
	if when.Valid {
		return when.Time, err
	}
	return time.Time{}, err
}

// database migrations. each one is a go function. Add them to the
// list mysqlMigrations at top of this file for them to be run.

func mysqlschema1(tx migration.LimitedTx) error {
	var s = []string{
		`CREATE TABLE IF NOT EXISTS items (
		id varchar(255),
		created datetime,
		modified datetime,
		size int,
		value text)`,

		`CREATE TABLE IF NOT EXISTS fixity (
		id varchar(255),
		scheduled_time datetime,
		status varchar(32),
		notes text)`,
	}
	return execlist(tx, s)
}

func mysqlschema2(tx migration.LimitedTx) error {
	var s = []string{
		`ALTER TABLE items CHANGE COLUMN id item varchar(255)`,
		`ALTER TABLE fixity CHANGE COLUMN id item varchar(255)`,
		`ALTER TABLE fixity ADD COLUMN id int PRIMARY KEY AUTO_INCREMENT FIRST`,
		`ALTER TABLE items ADD COLUMN id int PRIMARY KEY AUTO_INCREMENT FIRST`,
	}

	return execlist(tx, s)
}

func mysqlschema3(tx migration.LimitedTx) error {
	var s = []string{
		`CREATE TEMPORARY TABLE mult_ids AS SELECT item FROM items GROUP BY item HAVING count(*) > 1`,
		`DELETE FROM items WHERE item IN (SELECT * from mult_ids)`,
		`ALTER TABLE items ADD UNIQUE INDEX items_item (item), CHANGE COLUMN value value LONGTEXT, CHANGE COLUMN size size BIGINT`,
	}

	return execlist(tx, s)
}

func mysqlschema4(tx migration.LimitedTx) error {
	var s = []string{
		`CREATE TABLE IF NOT EXISTS blobs (
				id int PRIMARY KEY AUTO_INCREMENT,
				item varchar(255),
				blobid int,
				size int,
				bundle int,
				created datetime,
				creator varchar(64),
				MD5 binary(16),
				SHA256 binary(32),
				mimetype varchar(64),
				deleted datetime,
				deleter varchar(64),
				deletenote text,
				INDEX i_item (item),
				INDEX i_itemblob (item, blobid)
				)`,

		`CREATE TABLE IF NOT EXISTS versions (
				id int PRIMARY KEY AUTO_INCREMENT,
				item varchar(255),
				versionid int,
				created datetime,
				creator varchar(64),
				note text,
				INDEX i_item (item),
				INDEX i_itemversion (item, versionid) )`,

		`CREATE TABLE IF NOT EXISTS slots (
				id int PRIMARY KEY AUTO_INCREMENT,
				item varchar(255),
				versionid int,
				blobid int,
				name varchar(1024),
				INDEX i_item (item),
				INDEX i_name (name),
				INDEX i_itemversion (item, versionid) )`,
	}

	return execlist(tx, s)
}

// execlist exec's each item in the list, return if there is an error.
// Used to work around mysql driver not handling compound exec statements.
func execlist(tx migration.LimitedTx, stms []string) error {
	var err error
	for _, s := range stms {
		_, err = tx.Exec(s)
		if err != nil {
			break
		}
	}
	return err
}
