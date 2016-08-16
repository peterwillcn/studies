package tsdb // import "github.com/influxdata/influxdb/tsdb"

import (
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"sync"
	"time"

	"github.com/influxdata/influxdb/influxql"
	"github.com/influxdata/influxdb/models"
	"github.com/influxdata/influxdb/pkg/limiter"
)

var (
	// ErrShardNotFound gets returned when trying to get a non existing shard.
	ErrShardNotFound = fmt.Errorf("shard not found")
	// ErrStoreClosed gets returned when trying to use a closed Store.
	ErrStoreClosed = fmt.Errorf("store is closed")
)

// Store manages shards and indexes for databases.
type Store struct {
	mu   sync.RWMutex
	path string			// 数据库文件在磁盘上的存储路径

	// 数据库索引文件，key 为数据库名
	databaseIndexes map[string]*DatabaseIndex

	// shards is a map of shard IDs to the associated Shard.
	// 所有 shards 的索引，key 为其 shard ID
	shards map[uint64]*Shard

	EngineOptions EngineOptions		// 存储引擎选项
	Logger        *log.Logger

	// logOutput is where output from the underlying databases will go.
	logOutput io.Writer

	closing chan struct{}
	wg      sync.WaitGroup
	opened  bool
}

// NewStore returns a new store with the given path and a default configuration.
// The returned store must be initialized by calling Open before using it.
// 创建 store 对象
func NewStore(path string) *Store {
	// 当前版本的默认存储引擎(tsm1)
	opts := NewEngineOptions()

	return &Store{
		path:          path,
		EngineOptions: opts,
		Logger:        log.New(os.Stderr, "[store] ", log.LstdFlags),
		logOutput:     os.Stderr,
	}
}

// SetLogOutput sets the writer to which all logs are written. It must not be
// called after Open is called.
func (s *Store) SetLogOutput(w io.Writer) {
	s.Logger = log.New(w, "[store] ", log.LstdFlags)
	s.logOutput = w
	for _, s := range s.shards {
		s.SetLogOutput(w)
	}
}

// 获取数据库相关的所有统计信息
func (s *Store) Statistics(tags map[string]string) []models.Statistic {
	var statistics []models.Statistic

	s.mu.RLock()
	indexes := make([]models.Statistic, 0, len(s.databaseIndexes))
	for _, dbi := range s.databaseIndexes {
		indexes = append(indexes, dbi.Statistics(tags)...)
	}
	shards := s.shardsSlice()
	s.mu.RUnlock()

	for _, shard := range shards {
		statistics = append(statistics, shard.Statistics(tags)...)
	}

	statistics = append(statistics, indexes...)
	return statistics
}

// Path returns the store's root path.
// 返回当前数据库文件所在的目录
func (s *Store) Path() string { return s.path }

// Open initializes the store, creating all necessary directories, loading all
// shards and indexes and initializing periodic maintenance of all shards.
// 创建或者加载已经存在的数据库
// 主要是创建各种资源管理对象，wal, tsm file 等等
// 还需要遍历一些文件的索引信息，将部分内容在内存中构建索引，加速之后的查询操作
func (s *Store) Open() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.closing = make(chan struct{})

	s.shards = map[uint64]*Shard{}
	s.databaseIndexes = map[string]*DatabaseIndex{}

	s.Logger.Printf("Using data dir: %v", s.Path())

	// Create directory.
	// 如果没有此目录，创建
	if err := os.MkdirAll(s.path, 0777); err != nil {
		return err
	}

	// TODO: Start AE for Node
	// 加载数据库索引，只是创建一些空的对象，没有数据，在之后的 loadShards 函数中填充
	if err := s.loadIndexes(); err != nil {
		return err
	}

	// 加载所有 shard 中的资源
	// 将 wal 文件中的内容加载到 cache 中
	// 根据 tsm file 中的索引信息，在内存中构建整个数据库的 measurement 和 tags, field 的索引信息，便于加速之后的查询操作
	if err := s.loadShards(); err != nil {
		return err
	}

	s.opened = true

	return nil
}

// 加载数据库索引，这个时候仍然是空的，只是初始化对象
func (s *Store) loadIndexes() error {
	// data 目录下每一个数据库都有一个和数据库同名的目录
	dbs, err := ioutil.ReadDir(s.path)
	if err != nil {
		return err
	}
	// 遍历每一个数据库目录
	for _, db := range dbs {
		if !db.IsDir() {
			s.Logger.Printf("Skipping database dir: %s. Not a directory", db.Name())
			continue
		}
		// 根据数据库名初始化一个空的索引对象
		s.databaseIndexes[db.Name()] = NewDatabaseIndex(db.Name())
	}
	return nil
}

// 加载所有 shard 中的资源
// 将 wal 文件中的内容加载到 cache 中
// 根据 tsm file 中的索引信息，在内存中构建整个数据库的 measurement 和 tags, field 的索引信息，便于加速之后的查询操作
func (s *Store) loadShards() error {
	// struct to hold the result of opening each reader in a goroutine
	type res struct {
		s   *Shard
		err error
	}

	// 这里是用于控制并发
	// runtime.GOMAXPROCS 的参数为0，不会修改当前配置，而是只返回之前设置的使用 cpu 核心的数量
	// 猜测应该是控制只能有和 cpu 核心数一样的协程并发处理
	t := limiter.NewFixed(runtime.GOMAXPROCS(0))

	// 这个通道用于异步获取所有协程的返回内容
	resC := make(chan *res)
	var n int

	// loop through the current database indexes
	// 遍历数据库索引，向其中填充数据
	for db := range s.databaseIndexes {
		rps, err := ioutil.ReadDir(filepath.Join(s.path, db))
		if err != nil {
			return err
		}

		// 遍历每一个数据库中的所有存储策略
		for _, rp := range rps {
			// retention policies should be directories.  Skip anything that is not a dir.
			if !rp.IsDir() {
				s.Logger.Printf("Skipping retention policy dir: %s. Not a directory", rp.Name())
				continue
			}

			shards, err := ioutil.ReadDir(filepath.Join(s.path, db, rp.Name()))
			if err != nil {
				return err
			}
			// 遍历每一个存储策略中的所有 shard
			for _, sh := range shards {
				n++
				go func(index *DatabaseIndex, db, rp, sh string) {
					t.Take()
					defer t.Release()

					start := time.Now()
					// shard 所在路径
					path := filepath.Join(s.path, db, rp, sh)
					// 对应的 wal 文件所在路径
					walPath := filepath.Join(s.EngineOptions.Config.WALDir, db, rp, sh)

					// Shard file names are numeric shardIDs
					shardID, err := strconv.ParseUint(sh, 10, 64)
					if err != nil {
						resC <- &res{err: fmt.Errorf("%s is not a valid ID. Skipping shard.", sh)}
						return
					}

					// 创建 shard 对象
					shard := NewShard(shardID, s.databaseIndexes[db], path, walPath, s.EngineOptions)
					shard.SetLogOutput(s.logOutput)

					// 打开 shard
					// 创建 shard 对应的底层存储引擎对象，初始化 wal, tsm file, cache 等管理对象的服务，从 tsm file 中获取信息建立 measurement 以及 tags, filed 相关的在内存中的索引信息
					err = shard.Open()
					if err != nil {
						resC <- &res{err: fmt.Errorf("Failed to open shard: %d: %s", shardID, err)}
						return
					}

					// 返回创建成功的 shard 对象
					resC <- &res{s: shard}
					s.Logger.Printf("%s opened in %s", path, time.Now().Sub(start))
				}(s.databaseIndexes[db], db, rp.Name(), sh.Name())
			}
		}
	}

	for i := 0; i < n; i++ {
		res := <-resC
		if res.err != nil {
			s.Logger.Println(res.err)
			continue
		}
		s.shards[res.s.id] = res.s
	}
	close(resC)
	return nil
}

// Close closes the store and all associated shards. After calling Close accessing
// shards through the Store will result in ErrStoreClosed being returned.
func (s *Store) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.opened {
		close(s.closing)
	}
	s.wg.Wait()

	for _, sh := range s.shards {
		if err := sh.Close(); err != nil {
			return err
		}
	}
	s.opened = false
	s.shards = nil
	s.databaseIndexes = nil

	return nil
}

// DatabaseIndexN returns the number of databases indicies in the store.
func (s *Store) DatabaseIndexN() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.databaseIndexes)
}

// Shard returns a shard by id.
func (s *Store) Shard(id uint64) *Shard {
	s.mu.RLock()
	defer s.mu.RUnlock()
	sh, ok := s.shards[id]
	if !ok {
		return nil
	}
	return sh
}

// Shards returns a list of shards by id.
func (s *Store) Shards(ids []uint64) []*Shard {
	s.mu.RLock()
	defer s.mu.RUnlock()
	a := make([]*Shard, 0, len(ids))
	for _, id := range ids {
		sh, ok := s.shards[id]
		if !ok {
			continue
		}
		a = append(a, sh)
	}
	return a
}

// ShardN returns the number of shards in the store.
func (s *Store) ShardN() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.shards)
}

// CreateShard creates a shard with the given id and retention policy on a database.
func (s *Store) CreateShard(database, retentionPolicy string, shardID uint64, enabled bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	select {
	case <-s.closing:
		return ErrStoreClosed
	default:
	}

	// shard already exists
	if _, ok := s.shards[shardID]; ok {
		return nil
	}

	// created the db and retention policy dirs if they don't exist
	if err := os.MkdirAll(filepath.Join(s.path, database, retentionPolicy), 0700); err != nil {
		return err
	}

	// create the WAL directory
	walPath := filepath.Join(s.EngineOptions.Config.WALDir, database, retentionPolicy, fmt.Sprintf("%d", shardID))
	if err := os.MkdirAll(walPath, 0700); err != nil {
		return err
	}

	// create the database index if it does not exist
	db, ok := s.databaseIndexes[database]
	if !ok {
		db = NewDatabaseIndex(database)
		s.databaseIndexes[database] = db
	}

	path := filepath.Join(s.path, database, retentionPolicy, strconv.FormatUint(shardID, 10))
	shard := NewShard(shardID, db, path, walPath, s.EngineOptions)
	shard.SetLogOutput(s.logOutput)
	shard.EnableOnOpen = enabled

	if err := shard.Open(); err != nil {
		return err
	}

	s.shards[shardID] = shard

	return nil
}

// CreateShardSnapShot will create a hard link to the underlying shard and return a path
// The caller is responsible for cleaning up (removing) the file path returned
func (s *Store) CreateShardSnapshot(id uint64) (string, error) {
	sh := s.Shard(id)
	if sh == nil {
		return "", ErrShardNotFound
	}

	return sh.CreateSnapshot()
}

// SetShardEnabled enables or disables a shard for read and writes
func (s *Store) SetShardEnabled(shardID uint64, enabled bool) error {
	sh := s.Shard(shardID)
	if sh == nil {
		return ErrShardNotFound
	}
	sh.SetEnabled(enabled)
	return nil
}

// DeleteShard removes a shard from disk.
func (s *Store) DeleteShard(shardID uint64) error {
	sh := s.Shard(shardID)
	if sh == nil {
		return nil
	}

	if err := sh.Close(); err != nil {
		return err
	}

	if err := os.RemoveAll(sh.path); err != nil {
		return err
	}

	if err := os.RemoveAll(sh.walPath); err != nil {
		return err
	}

	s.mu.Lock()
	delete(s.shards, shardID)
	s.mu.Unlock()

	return nil
}

// ShardIteratorCreator returns an iterator creator for a shard.
// 根据 shardID 创建 shard 的迭代器
func (s *Store) ShardIteratorCreator(id uint64) influxql.IteratorCreator {
	sh := s.Shard(id)
	if sh == nil {
		return nil
	}
	return &shardIteratorCreator{sh: sh}
}

// DeleteDatabase will close all shards associated with a database and remove the directory and files from disk.
func (s *Store) DeleteDatabase(name string) error {
	s.mu.RLock()
	shards := s.filterShards(func(sh *Shard) bool {
		return sh.database == name
	})
	s.mu.RUnlock()

	if err := s.walkShards(shards, func(sh *Shard) error {
		if sh.database != name {
			return nil
		}

		return sh.Close()
	}); err != nil {
		return err
	}

	if err := os.RemoveAll(filepath.Join(s.path, name)); err != nil {
		return err
	}
	if err := os.RemoveAll(filepath.Join(s.EngineOptions.Config.WALDir, name)); err != nil {
		return err
	}

	s.mu.Lock()
	for _, sh := range shards {
		delete(s.shards, sh.id)
	}
	delete(s.databaseIndexes, name)
	s.mu.Unlock()

	return nil
}

// DeleteRetentionPolicy will close all shards associated with the
// provided retention policy, remove the retention policy directories on
// both the DB and WAL, and remove all shard files from disk.
func (s *Store) DeleteRetentionPolicy(database, name string) error {
	s.mu.RLock()
	shards := s.filterShards(func(sh *Shard) bool {
		return sh.database == database && sh.retentionPolicy == name
	})
	s.mu.RUnlock()

	// Close and delete all shards under the retention policy on the
	// database.
	if err := s.walkShards(shards, func(sh *Shard) error {
		if sh.database != database || sh.retentionPolicy != name {
			return nil
		}

		return sh.Close()
	}); err != nil {
		return err
	}

	// Remove the rentention policy folder.
	if err := os.RemoveAll(filepath.Join(s.path, database, name)); err != nil {
		return err
	}

	// Remove the retention policy folder from the the WAL.
	if err := os.RemoveAll(filepath.Join(s.EngineOptions.Config.WALDir, database, name)); err != nil {
		return err
	}

	s.mu.Lock()
	for _, sh := range shards {
		delete(s.shards, sh.id)
	}
	s.mu.Unlock()
	return nil
}

// DeleteMeasurement removes a measurement and all associated series from a database.
func (s *Store) DeleteMeasurement(database, name string) error {
	// Find the database.
	s.mu.RLock()
	db := s.databaseIndexes[database]
	s.mu.RUnlock()
	if db == nil {
		return nil
	}

	// Find the measurement.
	m := db.Measurement(name)
	if m == nil {
		return influxql.ErrMeasurementNotFound(name)
	}

	seriesKeys := m.SeriesKeys()

	s.mu.RLock()
	shards := s.filterShards(func(sh *Shard) bool {
		return sh.database == database
	})
	s.mu.RUnlock()

	if err := s.walkShards(shards, func(sh *Shard) error {
		if err := sh.DeleteMeasurement(m.Name, seriesKeys); err != nil {
			return err
		}
		return nil
	}); err != nil {
		return err
	}

	// Remove measurement from index.
	db.DropMeasurement(m.Name)

	return nil
}

// filterShards returns a slice of shards where fn returns true
// for the shard.
func (s *Store) filterShards(fn func(sh *Shard) bool) []*Shard {
	shards := make([]*Shard, 0, len(s.shards))
	for _, sh := range s.shards {
		if fn(sh) {
			shards = append(shards, sh)
		}
	}
	return shards
}

// walkShards apply a function to each shard in parallel.  If any of the
// functions return an error, the first error is returned.
func (s *Store) walkShards(shards []*Shard, fn func(sh *Shard) error) error {
	// struct to hold the result of opening each reader in a goroutine
	type res struct {
		err error
	}

	t := limiter.NewFixed(runtime.GOMAXPROCS(0))

	resC := make(chan res)
	var n int

	for _, sh := range shards {
		n++

		go func(sh *Shard) {
			t.Take()
			defer t.Release()

			if err := fn(sh); err != nil {
				resC <- res{err: fmt.Errorf("shard %d: %s", sh.id, err)}
				return
			}

			resC <- res{}
		}(sh)
	}

	var err error
	for i := 0; i < n; i++ {
		res := <-resC
		if res.err != nil {
			err = res.err
		}
	}
	close(resC)
	return err
}

// ShardIDs returns a slice of all ShardIDs under management.
func (s *Store) ShardIDs() []uint64 {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.shardIDs()
}

func (s *Store) shardIDs() []uint64 {
	a := make([]uint64, 0, len(s.shards))
	for shardID := range s.shards {
		a = append(a, shardID)
	}
	return a
}

// shardsSlice returns an ordered list of shards.
func (s *Store) shardsSlice() []*Shard {
	a := make([]*Shard, 0, len(s.shards))
	for _, sh := range s.shards {
		a = append(a, sh)
	}
	sort.Sort(Shards(a))
	return a
}

// DatabaseIndex returns the index for a database by its name.
func (s *Store) DatabaseIndex(name string) *DatabaseIndex {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.databaseIndexes[name]
}

// Databases returns all the databases in the indexes
func (s *Store) Databases() []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	databases := make([]string, 0, len(s.databaseIndexes))
	for db := range s.databaseIndexes {
		databases = append(databases, db)
	}
	return databases
}

// Measurement returns a measurement by name from the given database.
func (s *Store) Measurement(database, name string) *Measurement {
	s.mu.RLock()
	db := s.databaseIndexes[database]
	s.mu.RUnlock()
	if db == nil {
		return nil
	}
	return db.Measurement(name)
}

// DiskSize returns the size of all the shard files in bytes.  This size does not include the WAL size.
func (s *Store) DiskSize() (int64, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var size int64
	for _, shardID := range s.ShardIDs() {
		shard := s.Shard(shardID)
		sz, err := shard.DiskSize()
		if err != nil {
			return 0, err
		}
		size += sz
	}
	return size, nil
}

// BackupShard will get the shard and have the engine backup since the passed in time to the writer
func (s *Store) BackupShard(id uint64, since time.Time, w io.Writer) error {
	shard := s.Shard(id)
	if shard == nil {
		return fmt.Errorf("shard %d doesn't exist on this server", id)
	}

	path, err := relativePath(s.path, shard.path)
	if err != nil {
		return err
	}

	return shard.engine.Backup(w, path, since)
}

// RestoreShard restores a backup from r to a given shard.
// This will only overwrite files included in the backup.
func (s *Store) RestoreShard(id uint64, r io.Reader) error {
	shard := s.Shard(id)
	if shard == nil {
		return fmt.Errorf("shard %d doesn't exist on this server", id)
	}

	path, err := relativePath(s.path, shard.path)
	if err != nil {
		return err
	}

	return shard.Restore(r, path)
}

// ShardRelativePath will return the relative path to the shard. i.e. <database>/<retention>/<id>
func (s *Store) ShardRelativePath(id uint64) (string, error) {
	shard := s.Shard(id)
	if shard == nil {
		return "", fmt.Errorf("shard %d doesn't exist on this server", id)
	}
	return relativePath(s.path, shard.path)
}

// DeleteSeries loops through the local shards and deletes the series data and metadata for the passed in series keys
func (s *Store) DeleteSeries(database string, sources []influxql.Source, condition influxql.Expr) error {
	// Expand regex expressions in the FROM clause.
	a, err := s.ExpandSources(sources)
	if err != nil {
		return err
	} else if sources != nil && len(sources) != 0 && len(a) == 0 {
		return nil
	}
	sources = a

	// Determine deletion time range.
	min, max, err := influxql.TimeRangeAsEpochNano(condition)
	if err != nil {
		return err
	}

	s.mu.RLock()
	defer s.mu.RUnlock()

	// Find the database.
	db := s.DatabaseIndex(database)
	if db == nil {
		return nil
	}

	measurements, err := measurementsFromSourcesOrDB(db, sources...)
	if err != nil {
		return err
	}

	var seriesKeys []string
	for _, m := range measurements {
		var ids SeriesIDs
		var filters FilterExprs
		if condition != nil {
			// Get series IDs that match the WHERE clause.
			ids, filters, err = m.walkWhereForSeriesIds(condition)
			if err != nil {
				return err
			}

			// Delete boolean literal true filter expressions.
			// These are returned for `WHERE tagKey = 'tagVal'` type expressions and are okay.
			filters.DeleteBoolLiteralTrues()

			// Check for unsupported field filters.
			// Any remaining filters means there were fields (e.g., `WHERE value = 1.2`).
			if filters.Len() > 0 {
				return errors.New("fields not supported in WHERE clause during deletion")
			}
		} else {
			// No WHERE clause so get all series IDs for this measurement.
			ids = m.seriesIDs
		}

		for _, id := range ids {
			seriesKeys = append(seriesKeys, m.seriesByID[id].Key)
		}
	}

	// delete the raw series data
	if err := s.deleteSeries(database, seriesKeys, min, max); err != nil {
		return err
	}

	return nil
}

func (s *Store) deleteSeries(database string, seriesKeys []string, min, max int64) error {
	db := s.databaseIndexes[database]
	if db == nil {
		return influxql.ErrDatabaseNotFound(database)
	}

	s.mu.RLock()
	shards := s.filterShards(func(sh *Shard) bool {
		return sh.database == database
	})
	s.mu.RUnlock()

	return s.walkShards(shards, func(sh *Shard) error {
		if sh.database != database {
			return nil
		}
		if err := sh.DeleteSeriesRange(seriesKeys, min, max); err != nil {
			return err
		}

		// The keys we passed in may be fully deleted from the shard, if so,
		// we need to remove the shard from all the meta data indexes
		existing, err := sh.ContainsSeries(seriesKeys)
		if err != nil {
			return err
		}

		for k, exists := range existing {
			if !exists {
				db.UnassignShard(k, sh.id)
			}
		}
		return nil
	})
}

// ExpandSources expands sources against all local shards.
func (s *Store) ExpandSources(sources influxql.Sources) (influxql.Sources, error) {
	return s.IteratorCreators().ExpandSources(sources)
}

// IteratorCreators returns a set of all local shards as iterator creators.
func (s *Store) IteratorCreators() influxql.IteratorCreators {
	s.mu.RLock()
	defer s.mu.RUnlock()

	a := make(influxql.IteratorCreators, 0, len(s.shards))
	for _, sh := range s.shards {
		a = append(a, sh)
	}
	return a
}

// 创建数据查询迭代器，每一个 shard 创建一个 ShardIteratorCreator
func (s *Store) IteratorCreator(shards []uint64, opt *influxql.SelectOptions) (influxql.IteratorCreator, error) {
	// Generate iterators for each node.
	ics := make([]influxql.IteratorCreator, 0)
	if err := func() error {
		for _, id := range shards {
			ic := s.ShardIteratorCreator(id)
			if ic == nil {
				continue
			}
			ics = append(ics, ic)
		}

		return nil
	}(); err != nil {
		influxql.IteratorCreators(ics).Close()
		return nil, err
	}

	return influxql.IteratorCreators(ics), nil
}

// WriteToShard writes a list of points to a shard identified by its ID.
// 将 Points 数据写入到指定的 shard 中
func (s *Store) WriteToShard(shardID uint64, points []models.Point) error {
	s.mu.RLock()

	select {
	case <-s.closing:
		s.mu.RUnlock()
		return ErrStoreClosed
	default:
	}

	// 获取 shard 信息
	sh, ok := s.shards[shardID]
	if !ok {
		s.mu.RUnlock()
		return ErrShardNotFound
	}
	s.mu.RUnlock()

	// 向 shard 中写入数据
	return sh.WritePoints(points)
}

// filterShowSeriesResult will limit the number of series returned based on the limit and the offset.
// Unlike limit and offset on SELECT statements, the limit and offset don't apply to the number of Rows, but
// to the number of total Values returned, since each Value represents a unique series.
func (e *Store) filterShowSeriesResult(limit, offset int, rows models.Rows) models.Rows {
	var filteredSeries models.Rows
	seriesCount := 0
	for _, r := range rows {
		var currentSeries [][]interface{}

		// filter the values
		for _, v := range r.Values {
			if seriesCount >= offset && seriesCount-offset < limit {
				currentSeries = append(currentSeries, v)
			}
			seriesCount++
		}

		// only add the row back in if there are some values in it
		if len(currentSeries) > 0 {
			r.Values = currentSeries
			filteredSeries = append(filteredSeries, r)
			if seriesCount > limit+offset {
				return filteredSeries
			}
		}
	}
	return filteredSeries
}

// DecodeStorePath extracts the database and retention policy names
// from a given shard or WAL path.
func DecodeStorePath(shardOrWALPath string) (database, retentionPolicy string) {
	// shardOrWALPath format: /maybe/absolute/base/then/:database/:retentionPolicy/:nameOfShardOrWAL

	// Discard the last part of the path (the shard name or the wal name).
	path, _ := filepath.Split(filepath.Clean(shardOrWALPath))

	// Extract the database and retention policy.
	path, rp := filepath.Split(filepath.Clean(path))
	_, db := filepath.Split(filepath.Clean(path))
	return db, rp
}

// relativePath will expand out the full paths passed in and return
// the relative shard path from the store
func relativePath(storePath, shardPath string) (string, error) {
	path, err := filepath.Abs(storePath)
	if err != nil {
		return "", fmt.Errorf("store abs path: %s", err)
	}

	fp, err := filepath.Abs(shardPath)
	if err != nil {
		return "", fmt.Errorf("file abs path: %s", err)
	}

	name, err := filepath.Rel(path, fp)
	if err != nil {
		return "", fmt.Errorf("file rel path: %s", err)
	}

	return name, nil
}

// measurementsFromSourcesOrDB returns a list of measurements from the
// sources passed in or, if sources is empty, a list of all
// measurement names from the database passed in.
func measurementsFromSourcesOrDB(db *DatabaseIndex, sources ...influxql.Source) (Measurements, error) {
	var measurements Measurements
	if len(sources) > 0 {
		for _, source := range sources {
			if m, ok := source.(*influxql.Measurement); ok {
				measurement := db.measurements[m.Name]
				if measurement == nil {
					continue
				}

				measurements = append(measurements, measurement)
			} else {
				return nil, errors.New("identifiers in FROM clause must be measurement names")
			}
		}
	} else {
		// No measurements specified in FROM clause so get all measurements that have series.
		for _, m := range db.Measurements() {
			if m.HasSeries() {
				measurements = append(measurements, m)
			}
		}
	}
	sort.Sort(measurements)

	return measurements, nil
}