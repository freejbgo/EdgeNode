// Copyright 2022 Liuxiangchao iwind.liu@gmail.com. All rights reserved.

package caches

import (
	"database/sql"
	"errors"
	teaconst "github.com/TeaOSLab/EdgeNode/internal/const"
	"github.com/TeaOSLab/EdgeNode/internal/goman"
	"github.com/TeaOSLab/EdgeNode/internal/remotelogs"
	"github.com/TeaOSLab/EdgeNode/internal/utils"
	"github.com/TeaOSLab/EdgeNode/internal/utils/dbs"
	"github.com/iwind/TeaGo/types"
	timeutil "github.com/iwind/TeaGo/utils/time"
	"runtime"
	"strings"
	"time"
)

type FileListDB struct {
	dbPath string

	readDB  *dbs.DB
	writeDB *dbs.DB

	writeBatch *dbs.Batch

	hashMap *FileListHashMap

	itemsTableName string
	hitsTableName  string

	total int64

	isClosed bool
	isReady  bool

	// cacheItems
	existsByHashStmt *dbs.Stmt // 根据hash检查是否存在

	insertStmt *dbs.Stmt // 写入数据
	insertSQL  string

	selectByHashStmt *dbs.Stmt // 使用hash查询数据

	selectHashListStmt *dbs.Stmt

	deleteByHashStmt *dbs.Stmt // 根据hash删除数据
	deleteByHashSQL  string

	statStmt            *dbs.Stmt // 统计
	purgeStmt           *dbs.Stmt // 清理
	deleteAllStmt       *dbs.Stmt // 删除所有数据
	listOlderItemsStmt  *dbs.Stmt // 读取较早存储的缓存
	updateAccessWeekSQL string    // 修改访问日期

	// hits
	insertHitSQL       string // 写入数据
	increaseHitSQL     string // 增加点击量
	deleteHitByHashSQL string // 根据hash删除数据
}

func NewFileListDB() *FileListDB {
	return &FileListDB{
		hashMap: NewFileListHashMap(),
	}
}

func (this *FileListDB) Open(dbPath string) error {
	this.dbPath = dbPath

	// 动态调整Cache值
	var cacheSize = 32000
	var memoryGB = utils.SystemMemoryGB()
	if memoryGB >= 8 {
		cacheSize += 32000 * memoryGB / 8
	}

	// write db
	writeDB, err := sql.Open("sqlite3", "file:"+dbPath+"?cache=private&mode=rwc&_journal_mode=WAL&_sync=OFF&_cache_size="+types.String(cacheSize)+"&_secure_delete=FAST")
	if err != nil {
		return errors.New("open write database failed: " + err.Error())
	}

	writeDB.SetMaxOpenConns(1)

	// TODO 耗时过长，暂时不整理数据库
	// TODO 需要根据行数来判断是否VACUUM
	// TODO 注意VACUUM反而可能让数据库文件变大
	/**_, err = db.Exec("VACUUM")
	if err != nil {
		return err
	}**/

	this.writeDB = dbs.NewDB(writeDB)
	this.writeBatch = dbs.NewBatch(writeDB, 4)
	this.writeBatch.OnFail(func(err error) {
		remotelogs.Warn("LIST_FILE_DB", "run batch failed: "+err.Error())
	})

	goman.New(func() {
		this.writeBatch.Exec()
	})

	if teaconst.EnableDBStat {
		this.writeBatch.EnableStat(true)
		this.writeDB.EnableStat(true)
	}

	// read db
	readDB, err := sql.Open("sqlite3", "file:"+dbPath+"?cache=private&mode=ro&_journal_mode=WAL&_sync=OFF&_cache_size="+types.String(cacheSize))
	if err != nil {
		return errors.New("open read database failed: " + err.Error())
	}

	readDB.SetMaxOpenConns(runtime.NumCPU())

	this.readDB = dbs.NewDB(readDB)

	if teaconst.EnableDBStat {
		this.readDB.EnableStat(true)
	}

	return nil
}

func (this *FileListDB) Init() error {
	this.itemsTableName = "cacheItems"
	this.hitsTableName = "hits"

	// 创建
	var err = this.initTables(1)
	if err != nil {
		return errors.New("init tables failed: " + err.Error())
	}

	// 读取总数量
	row := this.readDB.QueryRow(`SELECT COUNT(*) FROM "` + this.itemsTableName + `"`)
	if row.Err() != nil {
		return row.Err()
	}
	var total int64
	err = row.Scan(&total)
	if err != nil {
		return err
	}
	this.total = total

	// 常用语句
	this.existsByHashStmt, err = this.readDB.Prepare(`SELECT "expiredAt" FROM "` + this.itemsTableName + `" INDEXED BY "hash" WHERE "hash"=? AND expiredAt>? LIMIT 1`)
	if err != nil {
		return err
	}

	this.insertSQL = `INSERT INTO "` + this.itemsTableName + `" ("hash", "key", "headerSize", "bodySize", "metaSize", "expiredAt", "staleAt", "host", "serverId", "createdAt", "accessWeek") VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`
	this.insertStmt, err = this.writeDB.Prepare(this.insertSQL)
	if err != nil {
		return err
	}

	this.selectByHashStmt, err = this.readDB.Prepare(`SELECT "key", "headerSize", "bodySize", "metaSize", "expiredAt" FROM "` + this.itemsTableName + `" WHERE "hash"=? LIMIT 1`)
	if err != nil {
		return err
	}

	this.selectHashListStmt, err = this.readDB.Prepare(`SELECT "id", "hash" FROM "` + this.itemsTableName + `" WHERE id>:id ORDER BY id ASC LIMIT 2000`)

	this.deleteByHashSQL = `DELETE FROM "` + this.itemsTableName + `" WHERE "hash"=?`
	this.deleteByHashStmt, err = this.writeDB.Prepare(this.deleteByHashSQL)
	if err != nil {
		return err
	}

	this.statStmt, err = this.readDB.Prepare(`SELECT COUNT(*), IFNULL(SUM(headerSize+bodySize+metaSize), 0), IFNULL(SUM(headerSize+bodySize), 0) FROM "` + this.itemsTableName + `"`)
	if err != nil {
		return err
	}

	this.purgeStmt, err = this.readDB.Prepare(`SELECT "hash" FROM "` + this.itemsTableName + `" WHERE staleAt<=? LIMIT ?`)
	if err != nil {
		return err
	}

	this.deleteAllStmt, err = this.writeDB.Prepare(`DELETE FROM "` + this.itemsTableName + `"`)
	if err != nil {
		return err
	}

	this.listOlderItemsStmt, err = this.readDB.Prepare(`SELECT "hash" FROM "` + this.itemsTableName + `" ORDER BY "accessWeek" ASC, "id" ASC LIMIT ?`)

	this.updateAccessWeekSQL = `UPDATE "` + this.itemsTableName + `" SET "accessWeek"=? WHERE "hash"=?`

	this.insertHitSQL = `INSERT INTO "` + this.hitsTableName + `" ("hash", "week2Hits", "week") VALUES (?, 1, ?)`

	this.increaseHitSQL = `INSERT INTO "` + this.hitsTableName + `" ("hash", "week2Hits", "week") VALUES (?, 1, ?) ON CONFLICT("hash") DO UPDATE SET "week1Hits"=IIF("week"=?, "week1Hits", "week2Hits"), "week2Hits"=IIF("week"=?, "week2Hits"+1, 1), "week"=?`

	this.deleteHitByHashSQL = `DELETE FROM "` + this.hitsTableName + `" WHERE "hash"=?`

	this.isReady = true

	// 加载HashMap
	go func() {
		err := this.hashMap.Load(this)
		if err != nil {
			remotelogs.Error("LIST_FILE_DB", "load hash map failed: "+err.Error()+"(file: "+this.dbPath+")")
		}
	}()

	return nil
}

func (this *FileListDB) IsReady() bool {
	return this.isReady
}

func (this *FileListDB) Total() int64 {
	return this.total
}

func (this *FileListDB) AddAsync(hash string, item *Item) error {
	this.hashMap.Add(hash)

	if item.StaleAt == 0 {
		item.StaleAt = item.ExpiredAt
	}

	this.writeBatch.Add(this.insertSQL, hash, item.Key, item.HeaderSize, item.BodySize, item.MetaSize, item.ExpiredAt, item.StaleAt, item.Host, item.ServerId, utils.UnixTime(), timeutil.Format("YW"))
	return nil

}

func (this *FileListDB) AddSync(hash string, item *Item) error {
	this.hashMap.Add(hash)

	if item.StaleAt == 0 {
		item.StaleAt = item.ExpiredAt
	}

	_, err := this.insertStmt.Exec(hash, item.Key, item.HeaderSize, item.BodySize, item.MetaSize, item.ExpiredAt, item.StaleAt, item.Host, item.ServerId, utils.UnixTime(), timeutil.Format("YW"))
	if err != nil {
		return this.WrapError(err)
	}

	return nil
}

func (this *FileListDB) DeleteAsync(hash string) error {
	this.hashMap.Delete(hash)

	this.writeBatch.Add(this.deleteByHashSQL, hash)
	return nil
}

func (this *FileListDB) DeleteSync(hash string) error {
	this.hashMap.Delete(hash)

	_, err := this.deleteByHashStmt.Exec(hash)
	if err != nil {
		return err
	}
	return nil
}

func (this *FileListDB) ListExpiredItems(count int) (hashList []string, err error) {
	if !this.isReady {
		return nil, nil
	}

	if count <= 0 {
		count = 100
	}

	rows, err := this.purgeStmt.Query(time.Now().Unix(), count)
	if err != nil {
		return nil, err
	}
	defer func() {
		_ = rows.Close()
	}()

	for rows.Next() {
		var hash string
		err = rows.Scan(&hash)
		if err != nil {
			return nil, err
		}
		hashList = append(hashList, hash)
	}
	return hashList, nil
}

func (this *FileListDB) ListLFUItems(count int) (hashList []string, err error) {
	if !this.isReady {
		return nil, nil
	}

	if count <= 0 {
		count = 100
	}

	// 先找过期的
	hashList, err = this.ListExpiredItems(count)
	if err != nil {
		return
	}
	var l = len(hashList)

	// 直接删除旧缓存，不再从hits表里查询
	return this.listOlderItems(count - l)
}

func (this *FileListDB) ListHashes(lastId int64) (hashList []string, maxId int64, err error) {
	rows, err := this.selectHashListStmt.Query(lastId)
	if err != nil {
		return nil, 0, err
	}
	var id int64
	var hash string
	for rows.Next() {
		err = rows.Scan(&id, &hash)
		if err != nil {
			_ = rows.Close()
			return
		}
		maxId = id
		hashList = append(hashList, hash)
	}

	_ = rows.Close()
	return
}

func (this *FileListDB) IncreaseHitAsync(hash string) error {
	var week = timeutil.Format("YW")
	this.writeBatch.Add(this.increaseHitSQL, hash, week, week, week, week)
	this.writeBatch.Add(this.updateAccessWeekSQL, week, hash)
	return nil
}

func (this *FileListDB) DeleteHitAsync(hash string) error {
	this.writeBatch.Add(this.deleteHitByHashSQL, hash)
	return nil
}

func (this *FileListDB) CleanPrefix(prefix string) error {
	if !this.isReady {
		return nil
	}
	var count = int64(10000)
	var staleLife = 600             // TODO 需要可以设置
	var unixTime = utils.UnixTime() // 只删除当前的，不删除新的
	for {
		result, err := this.writeDB.Exec(`UPDATE "`+this.itemsTableName+`" SET expiredAt=0,staleAt=? WHERE id IN (SELECT id FROM "`+this.itemsTableName+`" WHERE expiredAt>0 AND createdAt<=? AND INSTR("key", ?)=1 LIMIT `+types.String(count)+`)`, unixTime+int64(staleLife), unixTime, prefix)
		if err != nil {
			return this.WrapError(err)
		}
		affectedRows, err := result.RowsAffected()
		if err != nil {
			return err
		}
		if affectedRows < count {
			return nil
		}
	}
}

func (this *FileListDB) CleanAll() error {
	if !this.isReady {
		return nil
	}

	_, err := this.deleteAllStmt.Exec()
	if err != nil {
		return this.WrapError(err)
	}

	this.hashMap.Clean()

	return nil
}

func (this *FileListDB) Close() error {
	this.isClosed = true
	this.isReady = false

	if this.existsByHashStmt != nil {
		_ = this.existsByHashStmt.Close()
	}
	if this.insertStmt != nil {
		_ = this.insertStmt.Close()
	}
	if this.selectByHashStmt != nil {
		_ = this.selectByHashStmt.Close()
	}
	if this.selectHashListStmt != nil {
		_ = this.selectHashListStmt.Close()
	}
	if this.deleteByHashStmt != nil {
		_ = this.deleteByHashStmt.Close()
	}
	if this.statStmt != nil {
		_ = this.statStmt.Close()
	}
	if this.purgeStmt != nil {
		_ = this.purgeStmt.Close()
	}
	if this.deleteAllStmt != nil {
		_ = this.deleteAllStmt.Close()
	}
	if this.listOlderItemsStmt != nil {
		_ = this.listOlderItemsStmt.Close()
	}

	var errStrings []string

	if this.readDB != nil {
		err := this.readDB.Close()
		if err != nil {
			errStrings = append(errStrings, err.Error())
		}
	}

	if this.writeDB != nil {
		err := this.writeDB.Close()
		if err != nil {
			errStrings = append(errStrings, err.Error())
		}
	}

	if this.writeBatch != nil {
		this.writeBatch.Close()
	}

	if len(errStrings) == 0 {
		return nil
	}
	return errors.New("close database failed: " + strings.Join(errStrings, ", "))
}

func (this *FileListDB) WrapError(err error) error {
	if err == nil {
		return nil
	}
	return errors.New(err.Error() + "(file: " + this.dbPath + ")")
}

// 初始化
func (this *FileListDB) initTables(times int) error {
	{
		// expiredAt - 过期时间，用来判断有无过期
		// staleAt - 过时缓存最大时间，用来清理缓存
		_, err := this.writeDB.Exec(`CREATE TABLE IF NOT EXISTS "` + this.itemsTableName + `" (
  "id" integer NOT NULL PRIMARY KEY AUTOINCREMENT,
  "hash" varchar(32),
  "key" varchar(1024),
  "tag" varchar(64),
  "headerSize" integer DEFAULT 0,
  "bodySize" integer DEFAULT 0,
  "metaSize" integer DEFAULT 0,
  "expiredAt" integer DEFAULT 0,
  "staleAt" integer DEFAULT 0,
  "createdAt" integer DEFAULT 0,
  "host" varchar(128),
  "serverId" integer,
  "accessWeek" varchar(6)
);

DROP INDEX IF EXISTS "createdAt";
DROP INDEX IF EXISTS "expiredAt";
DROP INDEX IF EXISTS "serverId";

CREATE INDEX IF NOT EXISTS "staleAt"
ON "` + this.itemsTableName + `" (
  "staleAt" ASC
);

CREATE UNIQUE INDEX IF NOT EXISTS "hash"
ON "` + this.itemsTableName + `" (
  "hash" ASC
);

ALTER TABLE "cacheItems" ADD "accessWeek" varchar(6);
`)

		if err != nil {
			// 忽略可以预期的错误
			if strings.Contains(err.Error(), "duplicate column name") {
				err = nil
			}

			// 尝试删除重建
			if err != nil {
				if times < 3 {
					_, dropErr := this.writeDB.Exec(`DROP TABLE "` + this.itemsTableName + `"`)
					if dropErr == nil {
						return this.initTables(times + 1)
					}
					return this.WrapError(err)
				}

				return this.WrapError(err)
			}
		}
	}

	{
		_, err := this.writeDB.Exec(`CREATE TABLE IF NOT EXISTS "` + this.hitsTableName + `" (
  "id" integer NOT NULL PRIMARY KEY AUTOINCREMENT,
  "hash" varchar(32),
  "week1Hits" integer DEFAULT 0,
  "week2Hits" integer DEFAULT 0,
  "week" varchar(6)
);

CREATE UNIQUE INDEX IF NOT EXISTS "hits_hash"
ON "` + this.hitsTableName + `" (
  "hash" ASC
);
`)
		if err != nil {
			// 尝试删除重建
			if times < 3 {
				_, dropErr := this.writeDB.Exec(`DROP TABLE "` + this.hitsTableName + `"`)
				if dropErr == nil {
					return this.initTables(times + 1)
				}
				return this.WrapError(err)
			}

			return this.WrapError(err)
		}
	}

	return nil
}

func (this *FileListDB) listOlderItems(count int) (hashList []string, err error) {
	rows, err := this.listOlderItemsStmt.Query(count)
	if err != nil {
		return nil, err
	}
	defer func() {
		_ = rows.Close()
	}()

	for rows.Next() {
		var hash string
		err = rows.Scan(&hash)
		if err != nil {
			return nil, err
		}
		hashList = append(hashList, hash)
	}

	return hashList, nil
}
