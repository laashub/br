// Copyright 2020 PingCAP, Inc. Licensed under Apache-2.0.

package restore

import (
	"context"
	"fmt"
	"sort"

	"github.com/pingcap/errors"
	"github.com/pingcap/log"
	"github.com/pingcap/parser/model"
	"github.com/pingcap/tidb/kv"
	"go.uber.org/zap"

	"github.com/pingcap/br/pkg/glue"
	"github.com/pingcap/br/pkg/utils"
)

// DB is a TiDB instance, not thread-safe.
type DB struct {
	se glue.Session
}

// NewDB returns a new DB
func NewDB(g glue.Glue, store kv.Storage) (*DB, error) {
	se, err := g.CreateSession(store)
	if err != nil {
		return nil, errors.Trace(err)
	}
	// The session may be nil in raw kv mode
	if se == nil {
		return nil, nil
	}
	// Set SQL mode to None for avoiding SQL compatibility problem
	err = se.Execute(context.Background(), "set @@sql_mode=''")
	if err != nil {
		return nil, errors.Trace(err)
	}
	return &DB{
		se: se,
	}, nil
}

// ExecDDL executes the query of a ddl job.
func (db *DB) ExecDDL(ctx context.Context, ddlJob *model.Job) error {
	var err error
	tableInfo := ddlJob.BinlogInfo.TableInfo
	dbInfo := ddlJob.BinlogInfo.DBInfo
	switch ddlJob.Type {
	case model.ActionCreateSchema:
		err = db.se.CreateDatabase(ctx, dbInfo)
		if err != nil {
			log.Error("create database failed", zap.Stringer("db", dbInfo.Name), zap.Error(err))
		}
		return errors.Trace(err)
	case model.ActionCreateTable:
		err = db.se.CreateTable(ctx, model.NewCIStr(ddlJob.SchemaName), tableInfo)
		if err != nil {
			log.Error("create table failed",
				zap.Stringer("db", dbInfo.Name),
				zap.Stringer("table", tableInfo.Name),
				zap.Error(err))
		}
		return errors.Trace(err)
	}

	if tableInfo != nil {
		switchDbSQL := fmt.Sprintf("use %s;", utils.EncloseName(ddlJob.SchemaName))
		err = db.se.Execute(ctx, switchDbSQL)
		if err != nil {
			log.Error("switch db failed",
				zap.String("query", switchDbSQL),
				zap.String("db", ddlJob.SchemaName),
				zap.Error(err))
			return errors.Trace(err)
		}
	}
	err = db.se.Execute(ctx, ddlJob.Query)
	if err != nil {
		log.Error("execute ddl query failed",
			zap.String("query", ddlJob.Query),
			zap.String("db", ddlJob.SchemaName),
			zap.Int64("historySchemaVersion", ddlJob.BinlogInfo.SchemaVersion),
			zap.Error(err))
	}
	return errors.Trace(err)
}

// CreateDatabase executes a CREATE DATABASE SQL.
func (db *DB) CreateDatabase(ctx context.Context, schema *model.DBInfo) error {
	err := db.se.CreateDatabase(ctx, schema)
	if err != nil {
		log.Error("create database failed", zap.Stringer("db", schema.Name), zap.Error(err))
	}
	return errors.Trace(err)
}

// CreateTable executes a CREATE TABLE SQL.
func (db *DB) CreateTable(ctx context.Context, table *utils.Table) error {
	err := db.se.CreateTable(ctx, table.Db.Name, table.Info)
	if err != nil {
		log.Error("create table failed",
			zap.Stringer("db", table.Db.Name),
			zap.Stringer("table", table.Info.Name),
			zap.Error(err))
		return errors.Trace(err)
	}
	alterAutoIncIDSQL := fmt.Sprintf(
		"alter table %s.%s auto_increment = %d",
		utils.EncloseName(table.Db.Name.O),
		utils.EncloseName(table.Info.Name.O),
		table.Info.AutoIncID)
	err = db.se.Execute(ctx, alterAutoIncIDSQL)
	if err != nil {
		log.Error("alter AutoIncID failed",
			zap.String("query", alterAutoIncIDSQL),
			zap.Stringer("db", table.Db.Name),
			zap.Stringer("table", table.Info.Name),
			zap.Error(err))
	}

	return errors.Trace(err)
}

// AlterTiflashReplica alters the replica count of tiflash
func (db *DB) AlterTiflashReplica(ctx context.Context, table *utils.Table, count int) error {
	switchDbSQL := fmt.Sprintf("use %s;", utils.EncloseName(table.Db.Name.O))
	err := db.se.Execute(ctx, switchDbSQL)
	if err != nil {
		log.Error("switch db failed",
			zap.String("SQL", switchDbSQL),
			zap.Stringer("db", table.Db.Name),
			zap.Error(err))
		return errors.Trace(err)
	}
	alterTiFlashSQL := fmt.Sprintf(
		"alter table %s set tiflash replica %d",
		utils.EncloseName(table.Info.Name.O),
		count,
	)
	err = db.se.Execute(ctx, alterTiFlashSQL)
	if err != nil {
		log.Error("alter tiflash replica failed",
			zap.String("query", alterTiFlashSQL),
			zap.Stringer("db", table.Db.Name),
			zap.Stringer("table", table.Info.Name),
			zap.Error(err))
		return err
	} else if table.TiFlashReplicas > 0 {
		log.Warn("alter tiflash replica done",
			zap.Stringer("db", table.Db.Name),
			zap.Stringer("table", table.Info.Name),
			zap.Int("originalReplicaCount", table.TiFlashReplicas),
			zap.Int("replicaCount", count))

	}
	return nil
}

// Close closes the connection
func (db *DB) Close() {
	db.se.Close()
}

// FilterDDLJobs filters ddl jobs
func FilterDDLJobs(allDDLJobs []*model.Job, tables []*utils.Table) (ddlJobs []*model.Job) {
	// Sort the ddl jobs by schema version in descending order.
	sort.Slice(allDDLJobs, func(i, j int) bool {
		return allDDLJobs[i].BinlogInfo.SchemaVersion > allDDLJobs[j].BinlogInfo.SchemaVersion
	})
	dbs := getDatabases(tables)
	for _, db := range dbs {
		// These maps is for solving some corner case.
		// e.g. let "t=2" indicates that the id of database "t" is 2, if the ddl execution sequence is:
		// rename "a" to "b"(a=1) -> drop "b"(b=1) -> create "b"(b=2) -> rename "b" to "a"(a=2)
		// Which we cannot find the "create" DDL by name and id directly.
		// To cover †his case, we must find all names and ids the database/table ever had.
		dbIDs := make(map[int64]bool)
		dbIDs[db.ID] = true
		dbNames := make(map[string]bool)
		dbNames[db.Name.String()] = true
		for _, job := range allDDLJobs {
			if job.BinlogInfo.DBInfo != nil {
				if dbIDs[job.SchemaID] || dbNames[job.BinlogInfo.DBInfo.Name.String()] {
					ddlJobs = append(ddlJobs, job)
					// The the jobs executed with the old id, like the step 2 in the example above.
					dbIDs[job.SchemaID] = true
					// For the jobs executed after rename, like the step 3 in the example above.
					dbNames[job.BinlogInfo.DBInfo.Name.String()] = true
				}
			}
		}
	}

	type namePair struct {
		db    string
		table string
	}

	for _, table := range tables {
		tableIDs := make(map[int64]bool)
		tableIDs[table.Info.ID] = true
		tableNames := make(map[namePair]bool)
		name := namePair{table.Db.Name.String(), table.Info.Name.String()}
		tableNames[name] = true
		for _, job := range allDDLJobs {
			if job.BinlogInfo.TableInfo != nil {
				name := namePair{job.SchemaName, job.BinlogInfo.TableInfo.Name.String()}
				if tableIDs[job.TableID] || tableNames[name] {
					ddlJobs = append(ddlJobs, job)
					tableIDs[job.TableID] = true
					// For truncate table, the id may be changed
					tableIDs[job.BinlogInfo.TableInfo.ID] = true
					tableNames[name] = true
				}
			}
		}
	}
	return ddlJobs
}

func getDatabases(tables []*utils.Table) (dbs []*model.DBInfo) {
	dbIDs := make(map[int64]bool)
	for _, table := range tables {
		if !dbIDs[table.Db.ID] {
			dbs = append(dbs, table.Db)
			dbIDs[table.Db.ID] = true
		}
	}
	return
}
