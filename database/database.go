package database

import (
	"fmt"
	"g-redis/datastruct/dict"
	"g-redis/interface/database"
	"g-redis/interface/redis"
	"g-redis/pkg/timewheel"
	"g-redis/redis/protocol"
	"strings"
	"time"
)

// cmdLint database 内部流转的结构体，包含了客户端发送的命令名称和数据
type cmdLint struct {
	cmdName   string
	cmdData   [][]byte
	cmdString []string
}

type CommandContext struct {
	db   *DB
	conn redis.Connection
}

func (c *CommandContext) GetDb() *DB {
	return c.db
}

func (c *CommandContext) GetConn() redis.Connection {
	return c.conn
}

func MakeCommandContext(db *DB, conn redis.Connection) *CommandContext {
	return &CommandContext{
		db:   db,
		conn: conn,
	}
}

func (lint *cmdLint) GetCmdName() string {
	return lint.cmdName
}

func (lint *cmdLint) GetCmdData() [][]byte {
	return lint.cmdData
}

func (lint *cmdLint) GetArgNum() int {
	return len(lint.cmdData)
}

// parseToLint 将resp 协议的字节流转为为 database 内部流转的结构体
func parseToLint(cmdLine database.CmdLine) *cmdLint {
	cmdName := strings.ToLower(string(cmdLine[0]))
	cmdData := cmdLine[1:]
	cmdString := make([]string, len(cmdData))
	for i := 0; i < len(cmdData); i++ {
		cmdString[i] = "'" + string(cmdData[i]) + "'"
	}
	return &cmdLint{
		cmdName:   cmdName,
		cmdData:   cmdData,
		cmdString: cmdString,
	}
}

type ExeFunc func(cmdCtx *CommandContext, cmdLint *cmdLint) redis.Reply

// DB 存储数据的DB
type DB struct {
	index   int
	dbEngin database.DBEngine
	data    dict.Dict
	ttlMap  dict.Dict
}

// MakeSimpleDb 使用map的实现，无锁结构
func MakeSimpleDb(index int, dbEngin database.DBEngine) *DB {
	return &DB{
		index:   index,
		dbEngin: dbEngin,
		data:    dict.MakeSimpleDict(),
		ttlMap:  dict.MakeSimpleDict(),
	}
}

// MakeSimpleSync 使用sync.Map的实现
func MakeSimpleSync(index int) *DB {
	return &DB{
		index: index,
		data:  dict.MakeSimpleSync(),
	}
}

func (db *DB) Exec(c redis.Connection, lint *cmdLint) redis.Reply {
	cmdName := lint.GetCmdName()
	cmd := getCommand(cmdName)
	if cmd == nil {
		return protocol.MakeStandardErrReply(fmt.Sprintf("ERR unknown command `%s`, with args beginning with: %s",
			cmdName, strings.Join(lint.cmdString, ", ")))
	}
	ctx := MakeCommandContext(db, c)
	return cmd.exeFunc(ctx, lint)
}

/* ---- Data Access ----- */

// GetEntity getData
func (db *DB) GetEntity(key string) (*database.DataEntity, bool) {
	row, exists := db.data.Get(key)
	if !exists {
		return nil, false
	}
	entity, _ := row.(*database.DataEntity)
	return entity, true
}

func (db *DB) PutEntity(key string, entry *database.DataEntity) int {
	return db.data.Put(key, entry)
}

func (db *DB) PutIfExists(key string, entity *database.DataEntity) int {
	return db.data.PutIfExists(key, entity)
}

func (db *DB) PutIfAbsent(key string, entity *database.DataEntity) int {
	return db.data.PutIfAbsent(key, entity)
}

// Remove 删除数据
func (db *DB) Remove(key string) {
	db.data.Remove(key)
	db.ttlMap.Remove(key)
	expireKey := db.getExpireKey(key)
	timewheel.Cancel(expireKey)
}

func (db *DB) Removes(keys ...string) (deleted int) {
	deleted = 0
	for _, key := range keys {
		_, exists := db.data.Get(key)
		if exists {
			db.Remove(key)
			deleted++
		}
	}
	return deleted
}

// Exists 返回一组key是否存在
// eg: k1 -> v1, k2 -> v2。 input: k1 k2 return 2
func (db *DB) Exists(keys []string) int64 {
	var result int64 = 0
	for _, key := range keys {
		_, ok := db.data.Get(key)
		if ok {
			result++
		}
	}
	return result
}

func (db *DB) Flush() {
	length := db.data.Len()
	if length > 0 {
		db.data.Clear()
	}
}

/* ---- Data TTL ----- */

// Expire 为key设置过期时间
func (db *DB) Expire(key string, expireTime time.Time) {
	// 记录key的过期时间
	db.ttlMap.Put(key, expireTime)
	// 为任务生成一个名称
	taskKey := db.getExpireKey(key)
	timewheel.At(expireTime, taskKey, func() {
		_, ok := db.ttlMap.Get(key)
		if !ok {
			return
		}
		db.Remove(key)
	})
}

// getExpireKey 拼接一个过期时间的key
func (db *DB) getExpireKey(key string) string {
	return fmt.Sprintf("expireKey:%d:%s", db.index, key)
}

// IsExpired 检查key是否过期了，如果发现过期就从db中移除
func (db *DB) IsExpired(key string) bool {
	rawExpireTime, ok := db.ttlMap.Get(key)
	if !ok {
		return false
	}
	expireTime := rawExpireTime.(time.Time)
	expired := time.Now().After(expireTime)
	if expired {
		db.Remove(key)
	}
	return expired
}

// RemoveTTl 移除ttlMap中的数据，关闭过期检查任务
func (db *DB) RemoveTTl(key string) {
	db.ttlMap.Remove(key)
	expireTaskKey := db.getExpireKey(key)
	timewheel.Cancel(expireTaskKey)
}
