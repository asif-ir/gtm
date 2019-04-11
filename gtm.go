package gtm

import (
	"context"
	"fmt"
	"github.com/pkg/errors"
	"github.com/serialx/hashring"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
	"log"
	"net"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

var opCodes = [...]string{"c", "i", "u", "d"}

type OrderingGuarantee int

type errchk interface {
	Err() error
}

const (
	Oplog     OrderingGuarantee = iota // ops sent in oplog order (strong ordering)
	Namespace                          // ops sent in oplog order within a namespace
	Document                           // ops sent in oplog order for a single document
	AnyOrder                           // ops sent as they become available
)

type QuerySource int

const (
	OplogQuerySource QuerySource = iota
	DirectQuerySource
)

type Options struct {
	After               TimestampGenerator
	Filter              OpFilter
	NamespaceFilter     OpFilter
	OpLogDisabled       bool
	OpLogDatabaseName   string
	OpLogCollectionName string
	ChannelSize         int
	BufferSize          int
	BufferDuration      time.Duration
	Ordering            OrderingGuarantee
	WorkerCount         int
	MaxWaitSecs         int
	UpdateDataAsDelta   bool
	ChangeStreamNs      []string
	DirectReadNs        []string
	DirectReadFilter    OpFilter
	DirectReadSplitMax  int32
	DirectReadConcur    int
	Unmarshal           DataUnmarshaller
	Pipe                PipelineBuilder
	Log                 *log.Logger
}

type Op struct {
	Id                interface{}            `json:"_id"`
	Operation         string                 `json:"operation"`
	Namespace         string                 `json:"namespace"`
	Data              map[string]interface{} `json:"data,omitempty"`
	Timestamp         primitive.Timestamp    `json:"timestamp"`
	Source            QuerySource            `json:"source"`
	Doc               interface{}            `json:"doc,omitempty"`
	UpdateDescription map[string]interface{} `json:"updateDescription,omitempty`
}

type OpLog struct {
	Timestamp    primitive.Timestamp    "ts"
	HistoryID    int64                  "h"
	MongoVersion int                    "v"
	Operation    string                 "op"
	Namespace    string                 "ns"
	Doc          map[string]interface{} "o"
	Update       map[string]interface{} "o2"
}

type ChangeDocNs struct {
	Database   string "db"
	Collection string "coll"
}

type ChangeDoc struct {
	DocKey            map[string]interface{} "documentKey"
	Id                interface{}            "_id"
	Operation         string                 "operationType"
	FullDoc           map[string]interface{} "fullDocument"
	Namespace         ChangeDocNs            "ns"
	Timestamp         primitive.Timestamp    "clusterTime"
	UpdateDescription map[string]interface{} "updateDescription"
}

func (cd *ChangeDoc) docId() interface{} {
	return cd.DocKey["_id"]
}

func (cd *ChangeDoc) mapTimestamp() primitive.Timestamp {
	if cd.Timestamp.T > 0 {
		// only supported in version 4.0
		return cd.Timestamp
	} else {
		t := time.Now().UTC().Unix()
		return primitive.Timestamp{T: uint32(t << 32)}
	}
}

func (cd *ChangeDoc) mapOperation() string {
	if cd.Operation == "insert" {
		return "i"
	} else if cd.Operation == "update" || cd.Operation == "replace" {
		return "u"
	} else if cd.Operation == "delete" {
		return "d"
	} else if cd.Operation == "invalidate" || cd.Operation == "drop" || cd.Operation == "dropDatabase" {
		return "c"
	} else {
		return ""
	}
}

func (cd *ChangeDoc) hasUpdate() bool {
	return cd.UpdateDescription != nil
}

func (cd *ChangeDoc) hasDoc() bool {
	return (cd.mapOperation() == "i" || cd.mapOperation() == "u") && cd.FullDoc != nil
}

func (cd *ChangeDoc) isInvalidate() bool {
	return cd.Operation == "invalidate"
}

func (cd *ChangeDoc) isDrop() bool {
	return cd.Operation == "drop"
}

func (cd *ChangeDoc) isDropDatabase() bool {
	return cd.Operation == "dropDatabase"
}

func (cd *ChangeDoc) mapNs() string {
	if cd.Namespace.Collection != "" {
		return cd.Namespace.Database + "." + cd.Namespace.Collection
	} else {
		return cd.Namespace.Database + ".cmd"
	}
}

type Doc struct {
	Id interface{} "_id"
}

type CollectionStats struct {
	Count         int32 "count"
	AvgObjectSize int32 "avgObjSize"
}

type CollectionSegment struct {
	min         interface{}
	max         interface{}
	splitKey    string
	splits      []map[string]interface{}
	subSegments []*CollectionSegment
}

func (cs *CollectionSegment) shrinkTo(next interface{}) {
	cs.max = next
}

func (cs *CollectionSegment) toSelector() bson.M {
	sel, doc := bson.M{}, bson.M{}
	if cs.min != nil {
		doc["$gte"] = cs.min
	}
	if cs.max != nil {
		doc["$lt"] = cs.max
	}
	if len(doc) > 0 {
		sel[cs.splitKey] = doc
	}
	return sel
}

func (cs *CollectionSegment) divide() {
	if len(cs.splits) == 0 {
		return
	}
	ns := &CollectionSegment{
		splitKey: cs.splitKey,
		min:      cs.min,
		max:      cs.max,
	}
	cs.subSegments = nil
	for _, split := range cs.splits {
		ns.shrinkTo(split[cs.splitKey])
		cs.subSegments = append(cs.subSegments, ns)
		ns = &CollectionSegment{
			splitKey: cs.splitKey,
			min:      ns.max,
			max:      cs.max,
		}
	}
	ns = &CollectionSegment{
		splitKey: cs.splitKey,
		min:      cs.splits[len(cs.splits)-1][cs.splitKey],
	}
	cs.subSegments = append(cs.subSegments, ns)
}

func (cs *CollectionSegment) init(c *mongo.Collection) (err error) {
	opts := &options.FindOneOptions{}
	opts.SetSort(bson.M{cs.splitKey: 1})
	doc := make(map[string]interface{})
	if err = c.FindOne(context.Background(), nil, opts).Decode(&doc); err != nil {
		return
	}
	cs.min = doc[cs.splitKey]
	opts = &options.FindOneOptions{}
	opts.SetSort(bson.M{cs.splitKey: -1})
	doc = make(map[string]interface{})
	if err = c.FindOne(context.Background(), nil, opts).Decode(&doc); err != nil {
		return
	}
	cs.max = doc[cs.splitKey]
	return
}

type OpChan chan *Op

type OpLogEntry map[string]interface{}

type OpFilter func(*Op) bool

type ShardInsertHandler func(*ShardInfo) (*mongo.Client, error)

type TimestampGenerator func(*mongo.Client, *Options) (primitive.Timestamp, error)

type DataUnmarshaller func(namespace string, cursor mongo.Cursor) (interface{}, error)

type PipelineBuilder func(namespace string, changeStream bool) ([]interface{}, error)

type OpBuf struct {
	Entries        []*Op
	BufferSize     int
	BufferDuration time.Duration
}

type OpCtx struct {
	lock             *sync.Mutex
	OpC              OpChan
	ErrC             chan error
	DirectReadWg     *sync.WaitGroup
	directReadConcWg *sync.WaitGroup
	stopC            chan bool
	allWg            *sync.WaitGroup
	seekC            chan primitive.Timestamp
	pauseC           chan bool
	resumeC          chan bool
	paused           bool
	stopped          bool
	log              *log.Logger
}

type OpCtxMulti struct {
	lock         *sync.Mutex
	contexts     []*OpCtx
	OpC          OpChan
	ErrC         chan error
	DirectReadWg *sync.WaitGroup
	opWg         *sync.WaitGroup
	stopC        chan bool
	allWg        *sync.WaitGroup
	seekC        chan primitive.Timestamp
	pauseC       chan bool
	resumeC      chan bool
	paused       bool
	stopped      bool
	log          *log.Logger
}

type ShardInfo struct {
	hostname string
}

type N struct {
	database   string
	collection string
}

func (n *N) parse(ns string) (err error) {
	parts := strings.SplitN(ns, ".", 2)
	if len(parts) != 2 {
		err = fmt.Errorf("Invalid ns: %s :expecting db.collection", ns)
	} else {
		n.database = parts[0]
		n.collection = parts[1]
	}
	return
}

func (n *N) parseForChanges(ns string) {
	if ns == "" {
		// watch the whole deployment
		n.database = ""
		n.collection = ""
		return
	}
	parts := strings.SplitN(ns, ".", 2)
	if len(parts) == 1 {
		n.database = parts[0]
		n.collection = ""
	} else {
		n.database = parts[0]
		n.collection = parts[1]
	}
	return
}

func (n *N) desc() (dsc string) {
	if n.isDatabase() {
		dsc = fmt.Sprintf("database %s", n.database)
	} else if n.isCollection() {
		dsc = fmt.Sprintf("collection %s.%s", n.database, n.collection)
	} else {
		dsc = "the deployment"
	}
	return
}

func (n *N) isDeployment() bool {
	return n.database == "" && n.collection == ""
}

func (n *N) isDatabase() bool {
	return n.database != "" && n.collection == ""
}

func (n *N) isCollection() bool {
	return n.database != "" && n.collection != ""
}

func (shard *ShardInfo) GetURL() string {
	hostParts := strings.SplitN(shard.hostname, "/", 2)
	if len(hostParts) == 2 {
		return hostParts[1] + "?replicaSet=" + hostParts[0]
	} else {
		return hostParts[0]
	}
}

func (ctx *OpCtx) isStopped() bool {
	ctx.lock.Lock()
	defer ctx.lock.Unlock()
	return ctx.stopped
}

func (ctx *OpCtx) Since(ts primitive.Timestamp) {
	ctx.lock.Lock()
	defer ctx.lock.Unlock()
	ctx.seekC <- ts
}

func (ctx *OpCtx) Pause() {
	ctx.lock.Lock()
	defer ctx.lock.Unlock()
	if !ctx.paused {
		ctx.paused = true
		ctx.pauseC <- true
	}
}

func (ctx *OpCtx) Resume() {
	ctx.lock.Lock()
	defer ctx.lock.Unlock()
	if ctx.paused {
		ctx.paused = false
		ctx.resumeC <- true
	}
}

func (ctx *OpCtx) Stop() {
	ctx.lock.Lock()
	defer ctx.lock.Unlock()
	if !ctx.stopped {
		ctx.stopped = true
		close(ctx.stopC)
		ctx.allWg.Wait()
		close(ctx.OpC)
		close(ctx.ErrC)
	}
}

func (ctx *OpCtxMulti) Since(ts primitive.Timestamp) {
	ctx.lock.Lock()
	defer ctx.lock.Unlock()
	for _, child := range ctx.contexts {
		child.Since(ts)
	}
}

func (ctx *OpCtxMulti) Pause() {
	ctx.lock.Lock()
	defer ctx.lock.Unlock()
	if !ctx.paused {
		ctx.paused = true
		ctx.pauseC <- true
		for _, child := range ctx.contexts {
			child.Pause()
		}
	}
}

func (ctx *OpCtxMulti) Resume() {
	ctx.lock.Lock()
	defer ctx.lock.Unlock()
	if ctx.paused {
		ctx.paused = false
		ctx.resumeC <- true
		for _, child := range ctx.contexts {
			child.Resume()
		}
	}
}

func (ctx *OpCtxMulti) Stop() {
	ctx.lock.Lock()
	defer ctx.lock.Unlock()
	if !ctx.stopped {
		ctx.stopped = true
		close(ctx.stopC)
		for _, c := range ctx.contexts {
			child := c
			go child.Stop()
		}
		ctx.allWg.Wait()
		ctx.opWg.Wait()
		close(ctx.OpC)
		close(ctx.ErrC)
	}
}

func positionLost(ec errchk) bool {
	err := ec.Err()
	if err != nil {
		if ce, ok := err.(mongo.CommandError); ok {
			code := ce.Code
			if code == 136 { // cursor capped position lost
				return true
			}
		}
	}
	return false
}

func cursorTimeout(ec errchk) bool {
	err := ec.Err()
	if et, ok := err.(net.Error); ok {
		return et.Timeout() || et.Temporary()
	}
	return false
}

func invalidCursor(ec errchk) bool {
	err := ec.Err()
	if err != nil {
		if ce, ok := err.(mongo.CommandError); ok {
			code := ce.Code
			if code == 43 { // cursor not found
				return true
			}
			if code == 11601 { // cursor interrupted
				return true
			}
			if code == 136 { // cursor capped position lost
				return true
			}
			if code == 237 { // cursor killed
				return true
			}
		}
	}
	return false
}

func tailShards(multi *OpCtxMulti, ctx *OpCtx, options *Options, handler ShardInsertHandler) {
	defer multi.allWg.Done()
	if options == nil {
		options = DefaultOptions()
	} else {
		options.SetDefaults()
	}
	for {
		select {
		case <-multi.stopC:
			return
		case <-multi.pauseC:
			select {
			case <-multi.stopC:
				return
			case <-multi.resumeC:
				break
			}
		case err := <-ctx.ErrC:
			if err == nil {
				break
			}
			multi.ErrC <- err
		case op := <-ctx.OpC:
			if op == nil || op.Data == nil || op.Data["host"] == nil {
				break
			}
			shardHost, ok := op.Data["host"].(string)
			if !ok {
				break
			}
			// new shard detected
			multi.lock.Lock()
			if multi.stopped {
				multi.lock.Unlock()
				break
			}
			shardInfo := &ShardInfo{
				hostname: shardHost,
			}
			shardClient, err := handler(shardInfo)
			if err != nil {
				multi.ErrC <- errors.Wrap(err, "Error calling shard handler")
				multi.lock.Unlock()
				break
			}
			shardCtx := Start(shardClient, options)
			multi.contexts = append(multi.contexts, shardCtx)
			multi.DirectReadWg.Add(1)
			multi.allWg.Add(1)
			multi.opWg.Add(2)
			go func() {
				defer multi.DirectReadWg.Done()
				shardCtx.DirectReadWg.Wait()
			}()
			go func() {
				defer multi.allWg.Done()
				shardCtx.allWg.Wait()
			}()
			go func(c OpChan) {
				defer multi.opWg.Done()
				for op := range c {
					multi.OpC <- op
				}
			}(shardCtx.OpC)
			go func(c chan error) {
				defer multi.opWg.Done()
				for err := range c {
					multi.ErrC <- err
				}
			}(shardCtx.ErrC)
			multi.lock.Unlock()
		}
	}
}

func (ctx *OpCtxMulti) AddShardListener(
	configSession *mongo.Client, shardOptions *Options, handler ShardInsertHandler) {
	opts := DefaultOptions()
	opts.NamespaceFilter = func(op *Op) bool {
		return op.Namespace == "config.shards" && op.IsInsert()
	}
	configCtx := Start(configSession, opts)
	ctx.allWg.Add(1)
	go tailShards(ctx, configCtx, shardOptions, handler)
}

func ChainOpFilters(filters ...OpFilter) OpFilter {
	return func(op *Op) bool {
		for _, filter := range filters {
			if filter(op) == false {
				return false
			}
		}
		return true
	}
}

func (this *Op) IsDrop() bool {
	if _, drop := this.IsDropDatabase(); drop {
		return true
	}
	if _, drop := this.IsDropCollection(); drop {
		return true
	}
	return false
}

func (this *Op) IsDropCollection() (string, bool) {
	if this.IsCommand() {
		if this.Data != nil {
			if val, ok := this.Data["drop"]; ok {
				return val.(string), true
			}
		}
	}
	return "", false
}

func (this *Op) IsDropDatabase() (string, bool) {
	if this.IsCommand() {
		if this.Data != nil {
			if _, ok := this.Data["dropDatabase"]; ok {
				return this.GetDatabase(), true
			}
		}
	}
	return "", false
}

func (this *Op) IsCommand() bool {
	return this.Operation == "c"
}

func (this *Op) IsInsert() bool {
	return this.Operation == "i"
}

func (this *Op) IsUpdate() bool {
	return this.Operation == "u"
}

func (this *Op) IsDelete() bool {
	return this.Operation == "d"
}

func (this *Op) IsSourceOplog() bool {
	return this.Source == OplogQuerySource
}

func (this *Op) IsSourceDirect() bool {
	return this.Source == DirectQuerySource
}

func (this *Op) ParseNamespace() []string {
	return strings.SplitN(this.Namespace, ".", 2)
}

func (this *Op) GetDatabase() string {
	return this.ParseNamespace()[0]
}

func (this *Op) GetCollection() string {
	if _, drop := this.IsDropDatabase(); drop {
		return ""
	} else if col, drop := this.IsDropCollection(); drop {
		return col
	} else {
		return this.ParseNamespace()[1]
	}
}

func (this *OpBuf) Append(op *Op) {
	this.Entries = append(this.Entries, op)
}

func (this *OpBuf) IsFull() bool {
	return len(this.Entries) >= this.BufferSize
}

func (this *OpBuf) HasOne() bool {
	return len(this.Entries) == 1
}

func (this *OpBuf) Flush(client *mongo.Client, ctx *OpCtx, options *Options) {
	if len(this.Entries) == 0 {
		return
	}
	ns := make(map[string][]interface{})
	byId := make(map[interface{}][]*Op)
	for _, op := range this.Entries {
		if op.IsUpdate() && op.Doc == nil {
			idKey := fmt.Sprintf("%s.%v", op.Namespace, op.Id)
			ns[op.Namespace] = append(ns[op.Namespace], op.Id)
			byId[idKey] = append(byId[idKey], op)
		}
	}
retry:
	for n, opIds := range ns {
		var parts = strings.SplitN(n, ".", 2)
		db, col := parts[0], parts[1]
		sel := bson.M{"_id": bson.M{"$in": opIds}}
		collection := client.Database(db).Collection(col)
		cursor, err := collection.Find(context.Background(), sel)
		if err == nil {
			for cursor.Next(context.Background()) {
				doc := make(map[string]interface{})
				if err = cursor.Decode(&doc); err == nil {
					resultId := fmt.Sprintf("%s.%v", n, doc["_id"])
					if ops, ok := byId[resultId]; ok {
						for _, o := range ops {
							o.processData(doc)
						}
					}

				}
			}
			if err = cursor.Close(context.Background()); err != nil {
				ctx.ErrC <- errors.Wrap(err, "Error finding documents to associate with ops")
			}
		} else {
			ctx.ErrC <- errors.Wrap(err, "Error finding documents to associate with ops")
			break retry
		}
	}
	for _, op := range this.Entries {
		if op.matchesFilter(options) {
			ctx.OpC <- op
		}
	}
	this.Entries = nil
}

func UpdateIsReplace(entry map[string]interface{}) bool {
	if _, ok := entry["$set"]; ok {
		return false
	} else if _, ok := entry["$unset"]; ok {
		return false
	} else {
		return true
	}
}

func (this *Op) shouldParse() bool {
	return this.IsInsert() || this.IsDelete() || this.IsUpdate() || this.IsCommand()
}

func (this *Op) matchesNsFilter(options *Options) bool {
	return options.NamespaceFilter == nil || options.NamespaceFilter(this)
}

func (this *Op) matchesFilter(options *Options) bool {
	return options.Filter == nil || options.Filter(this)
}

func (this *Op) matchesDirectFilter(options *Options) bool {
	return options.DirectReadFilter == nil || options.DirectReadFilter(this)
}

func (this *Op) processData(data interface{}) {
	if data != nil {
		this.Doc = data
		if m, ok := data.(map[string]interface{}); ok {
			this.Data = m
		}
	}
}

func (this *Op) ParseLogEntry(entry *OpLog, options *Options) (include bool, err error) {
	var rawField map[string]interface{}
	this.Operation = entry.Operation
	this.Timestamp = entry.Timestamp
	this.Namespace = entry.Namespace
	if this.shouldParse() {
		if this.IsCommand() {
			rawField = entry.Doc
			this.processData(rawField)
		}
		if this.matchesNsFilter(options) {
			if this.IsInsert() || this.IsDelete() || this.IsUpdate() {
				if this.IsUpdate() {
					rawField = entry.Update
				} else {
					rawField = entry.Doc
				}
				this.Id = rawField["_id"]
				if this.IsInsert() {
					this.processData(rawField)
				} else if this.IsUpdate() {
					rawField = entry.Doc
					if options.UpdateDataAsDelta || UpdateIsReplace(rawField) {
						this.processData(rawField)
					}
				}
				include = true
			} else if this.IsCommand() {
				include = this.IsDrop()
			}
		}
	}
	return
}

func OpLogCollectionName(client *mongo.Client, options *Options) string {
	return "oplog.rs"
}

func OpLogCollection(client *mongo.Client, options *Options) *mongo.Collection {
	localDB := client.Database(options.OpLogDatabaseName)
	return localDB.Collection(options.OpLogCollectionName)
}

func ParseTimestamp(timestamp primitive.Timestamp) (uint32, uint32) {
	return timestamp.T, timestamp.I
}

func validOps() bson.M {
	return bson.M{"op": bson.M{"$in": opCodes}}
}

func LastOpTimestamp(client *mongo.Client, o *Options) (primitive.Timestamp, error) {
	opLog := OpLog{}
	filter := validOps()
	opts := &options.FindOneOptions{}
	opts.SetSort(bson.M{"$natural": -1})
	c := OpLogCollection(client, o)
	err := c.FindOne(context.Background(), filter, opts).Decode(&opLog)
	return opLog.Timestamp, err
}

func FirstOpTimestamp(client *mongo.Client, o *Options) (primitive.Timestamp, error) {
	opLog := OpLog{}
	filter := validOps()
	opts := &options.FindOneOptions{}
	opts.SetSort(bson.M{"$natural": 1})
	c := OpLogCollection(client, o)
	err := c.FindOne(context.Background(), filter, opts).Decode(&opLog)
	return opLog.Timestamp, err
}

func GetOpLogCursor(client *mongo.Client, after primitive.Timestamp, o *Options) (*mongo.Cursor, error) {
	query := bson.M{
		"ts":          bson.M{"$gt": after},
		"op":          bson.M{"$in": opCodes},
		"fromMigrate": bson.M{"$exists": false},
	}
	opts := &options.FindOptions{}
	opts.SetSort(bson.M{"$natural": 1})
	opts.SetCursorType(options.TailableAwait)
	//opts.SetOplogReplay(true)
	//opts.SetNoCursorTimeout(true)
	collection := OpLogCollection(client, o)
	return collection.Find(context.Background(), query, opts)
}

func opDataReady(op *Op, options *Options) (ready bool) {
	if options.UpdateDataAsDelta {
		ready = true
	} else if options.Ordering == AnyOrder {
		if op.IsUpdate() {
			ready = op.Data != nil || op.Doc != nil
		} else {
			ready = true
		}
	}
	return
}

func TailOps(ctx *OpCtx, client *mongo.Client, channels []OpChan, o *Options) error {
	defer ctx.allWg.Done()
	var err error
	var cts primitive.Timestamp
	if o.After != nil {
		cts, _ = o.After(client, o)
	} else {
		cts, _ = LastOpTimestamp(client, o)
	}
	var cursor *mongo.Cursor
restart:
	if cursor != nil {
		if err = cursor.Close(context.Background()); err != nil {
			ctx.ErrC <- errors.Wrap(err, "Error tailing the oplog")
		}
	}
	cursor, err = GetOpLogCursor(client, cts, o)
	if err != nil {
		ctx.ErrC <- errors.Wrap(err, "Error establishing the oplog cursor")
		goto restart
	}
retry:
	tctx, cancel := context.WithTimeout(context.Background(), time.Duration(o.MaxWaitSecs)*time.Second)
	for cursor.Next(tctx) {
		var entry OpLog
		if err = cursor.Decode(&entry); err != nil {
			ctx.ErrC <- errors.Wrap(err, "Error decoding the oplog document")
			break
		}
		op := &Op{
			Id:        "",
			Operation: "",
			Namespace: "",
			Data:      nil,
			Timestamp: primitive.Timestamp{},
			Source:    OplogQuerySource,
		}
		ok, err := op.ParseLogEntry(&entry, o)
		if err == nil {
			if ok && op.matchesFilter(o) {
				if opDataReady(op, o) {
					ctx.OpC <- op
				} else {
					// broadcast to fetch channels
					for _, channel := range channels {
						channel <- op
					}
				}
			}
		} else {
			ctx.ErrC <- errors.Wrap(err, "Error parsing the oplog document")
		}
		select {
		case <-ctx.stopC:
			if err = cursor.Close(context.Background()); err != nil {
				ctx.ErrC <- errors.Wrap(err, "Error closing the oplog cursor")
			}
			return nil
		case ts := <-ctx.seekC:
			cts = ts
			goto restart
		case <-ctx.pauseC:
			cursor.Close(context.Background())
			<-ctx.resumeC
			select {
			case <-ctx.stopC:
				cursor.Close(context.Background())
				return nil
			case ts := <-ctx.seekC:
				cts = ts
				goto restart
			default:
				goto restart
			}
		default:
			cts = op.Timestamp
		}
	}
	err = tctx.Err()
	cancel()
	if err == context.DeadlineExceeded || err == context.Canceled {
		select {
		case <-ctx.stopC:
			cursor.Close(context.Background())
			return nil
		case ts := <-ctx.seekC:
			cts = ts
			goto restart
		case <-ctx.pauseC:
			cursor.Close(context.Background())
			select {
			case <-ctx.stopC:
				cursor.Close(context.Background())
				return nil
			case ts := <-ctx.seekC:
				cts = ts
				goto restart
			case <-ctx.resumeC:
				goto restart
			}
		default:
			goto retry
		}
	}
	if cursorTimeout(cursor) {
		goto retry
	}
	if invalidCursor(cursor) {
		if positionLost(cursor) {
			cts, _ = FirstOpTimestamp(client, o)
		}
		cursor.Close(context.Background())
		goto restart
	}
	if err = cursor.Err(); err != nil {
		cursor.Close(context.Background())
		goto restart
	}
	goto retry
	return nil
}

func DirectReadSegment(ctx *OpCtx, client *mongo.Client, ns string, o *Options, seg *CollectionSegment, stats *CollectionStats) (err error) {
	defer ctx.allWg.Done()
	defer ctx.DirectReadWg.Done()
	defer ctx.directReadConcWg.Done()
	n := &N{}
	if err = n.parse(ns); err != nil {
		ctx.ErrC <- errors.Wrap(err, "Error starting direct reads. Invalid namespace.")
		return
	}
	var batch int32 = 1000
	if stats.AvgObjectSize != 0 {
		batch = (8 * 1024 * 1024) / stats.AvgObjectSize // 8MB divided by avg doc size
		if batch < 1000 {
			// leave it up to the server
			batch = 0
		}
	}
	opts := &options.FindOptions{}
	if batch != 0 {
		opts.SetBatchSize(batch)
	}
	c := client.Database(n.database).Collection(n.collection)
	sel := seg.toSelector()
	cursor, err := c.Find(context.Background(), sel, opts)
	if err != nil {
		return
	}
retry:
	select {
	case <-ctx.stopC:
		cursor.Close(context.Background())
		return
	default:
		break
	}
	result := map[string]interface{}{}
	tctx, cancel := context.WithTimeout(context.Background(), time.Duration(o.MaxWaitSecs)*time.Second)
	for cursor.Next(tctx) {
		if err = cursor.Decode(&result); err != nil {
			ctx.ErrC <- errors.Wrap(err, "Error decoding cursor in direct reads")
			result = map[string]interface{}{}
			continue
		}
		t := time.Now().UTC().Unix()
		op := &Op{
			Id:        result["_id"],
			Operation: "i",
			Namespace: ns,
			Source:    DirectQuerySource,
			Timestamp: primitive.Timestamp{T: uint32(t)},
		}
		op.processData(result)
		if op.matchesDirectFilter(o) {
			ctx.OpC <- op
		}
		result = map[string]interface{}{}
		select {
		case <-ctx.stopC:
			cursor.Close(context.Background())
			return
		default:
			continue
		}
	}
	err = tctx.Err()
	cancel()
	if err == context.DeadlineExceeded || err == context.Canceled {
		goto retry
	}
	if err = cursor.Err(); err != nil {
		if et, ok := err.(net.Error); ok && et.Timeout() {
			goto retry
		} else {
			ctx.ErrC <- errors.Wrap(err, fmt.Sprintf(`
            Error performing direct read of collection %s
            `, ns))
		}
	}
	if err = cursor.Close(context.Background()); err != nil {
		ctx.ErrC <- errors.Wrap(err, fmt.Sprintf(`
        Error closing direct read cursor of collection %s. Will retry segment.
        `, ns))
		ctx.allWg.Add(1)
		ctx.DirectReadWg.Add(1)
		go DirectReadSegment(ctx, client, ns, o, seg, stats)
	}
	return
}

func GetCollectionStats(ctx *OpCtx, client *mongo.Client, ns string) (stats *CollectionStats, err error) {
	stats = &CollectionStats{}
	n := &N{}
	if err = n.parse(ns); err != nil {
		ctx.ErrC <- errors.Wrap(err, "Error reading collection stats. Invalid namespace.")
		return
	}
	var result *mongo.SingleResult
	cmd := bson.M{"collStats": n.collection}
	result = client.Database(n.database).RunCommand(context.Background(), cmd)
	err = result.Err()
	if err == nil {
		err = result.Decode(stats)
	}
	return
}

func ProcessDirectReads(ctx *OpCtx, client *mongo.Client, options *Options) (err error) {
	defer ctx.allWg.Done()
	defer ctx.DirectReadWg.Done()
	concur := options.DirectReadConcur
	running := 0
	for _, ns := range options.DirectReadNs {
		if concur > 0 && running >= concur {
			ctx.directReadConcWg.Wait()
			running = 0
		}
		ctx.DirectReadWg.Add(1)
		ctx.directReadConcWg.Add(1)
		ctx.allWg.Add(1)
		go DirectReadPaged(ctx, client, ns, options)
		running = running + 1
	}
	return
}

func ConsumeChangeStream(ctx *OpCtx, client *mongo.Client, ns string, o *Options) (err error) {
	defer ctx.allWg.Done()
	n := &N{}
	n.parseForChanges(ns)
	var pipeline []interface{}
	var startAt *primitive.Timestamp
	var resumeAfter interface{}
	if o.After != nil {
		if pos, err := o.After(client, o); err == nil {
			if pos.T > 0 {
				startAt = &pos
			} else if pos.T == 0 {
				if pos, err = FirstOpTimestamp(client, o); err == nil {
					startAt = &pos
				}
			}
		}
	}
	if o.Pipe != nil {
		var stages []interface{}
		if stages, err = o.Pipe(ns, true); err != nil {
			ctx.ErrC <- errors.Wrap(err, "Error building aggregation pipeline stages.")
			return
		}
		if stages != nil && len(stages) > 0 {
			pipeline = stages
		}
	}
restart:
	for {
		var stream *mongo.ChangeStream
		opts := options.ChangeStream()
		opts.SetBatchSize(int32(o.ChannelSize))
		opts.SetMaxAwaitTime(time.Duration(o.MaxWaitSecs) * time.Second)
		opts.SetFullDocument(options.UpdateLookup)
		opts.SetStartAtOperationTime(startAt)
		opts.SetResumeAfter(resumeAfter)
		if n.isDeployment() {
			stream, err = client.Watch(context.Background(), pipeline, opts)
		} else if n.isDatabase() {
			d := client.Database(n.database)
			stream, err = d.Watch(context.Background(), pipeline, opts)
		} else {
			c := client.Database(n.database).Collection(n.collection)
			stream, err = c.Watch(context.Background(), pipeline, opts)
		}
		if err != nil {
			ctx.ErrC <- errors.Wrap(err, "Error starting change stream. Will retry.")
			if stream != nil {
				stream.Close(context.Background())
			}
			select {
			case <-ctx.stopC:
				return
			default:
				goto restart
			}
		}
		ctx.log.Printf("Watching changes on %s", n.desc())
	retry:
		tctx, cancel := context.WithTimeout(context.Background(), time.Duration(o.MaxWaitSecs)*time.Second)
		for stream.Next(tctx) {
			startAt = nil
			var changeDoc ChangeDoc
			if err = stream.Decode(&changeDoc); err != nil {
				ctx.ErrC <- errors.Wrap(err, "Error decoding change doc.")
				break
			}
			resumeAfter = changeDoc.Id
			oper := changeDoc.mapOperation()
			if changeDoc.isDrop() {
				op := &Op{
					Operation: oper,
					Namespace: changeDoc.mapNs(),
					Source:    OplogQuerySource,
					Timestamp: changeDoc.mapTimestamp(),
				}
				op.Data = map[string]interface{}{"drop": n.collection}
				if op.matchesNsFilter(o) {
					ctx.OpC <- op
				}
			} else if changeDoc.isDropDatabase() {
				op := &Op{
					Operation: oper,
					Namespace: changeDoc.mapNs(),
					Source:    OplogQuerySource,
					Timestamp: changeDoc.mapTimestamp(),
				}
				op.Data = map[string]interface{}{"dropDatabase": n.database}
				if op.matchesNsFilter(o) {
					ctx.OpC <- op
				}
			} else if changeDoc.isInvalidate() {
				resumeAfter = nil
				startAt = nil
				stream.Close(context.Background())
				time.Sleep(time.Duration(5) * time.Second)
				goto restart
			} else if oper != "" {
				op := &Op{
					Id:        changeDoc.docId(),
					Operation: oper,
					Namespace: changeDoc.mapNs(),
					Source:    OplogQuerySource,
					Timestamp: changeDoc.mapTimestamp(),
				}
				if op.matchesNsFilter(o) {
					if changeDoc.hasUpdate() {
						op.UpdateDescription = changeDoc.UpdateDescription
					}
					if changeDoc.hasDoc() {
						op.processData(changeDoc.FullDoc)
						if op.matchesDirectFilter(o) {
							ctx.OpC <- op
						}
					} else if op.matchesDirectFilter(o) {
						ctx.OpC <- op
					}
				}
			}
			select {
			case <-ctx.stopC:
				stream.Close(context.Background())
				return
			case ts := <-ctx.seekC:
				resumeAfter = nil
				startAt = &ts
				stream.Close(context.Background())
				goto restart
			case <-ctx.pauseC:
				stream.Close(context.Background())
				select {
				case <-ctx.resumeC:
					goto restart
				case <-ctx.stopC:
					return
				}
			default:
				continue
			}
		}
		err = tctx.Err()
		cancel()
		if err == context.DeadlineExceeded || err == context.Canceled {
			select {
			case <-ctx.stopC:
				stream.Close(context.Background())
				return
			case ts := <-ctx.seekC:
				resumeAfter = nil
				startAt = &ts
				stream.Close(context.Background())
				goto restart
			case <-ctx.pauseC:
				stream.Close(context.Background())
				select {
				case <-ctx.resumeC:
					goto restart
				case <-ctx.stopC:
					return
				}
			default:
				goto retry
			}
		}
		if cursorTimeout(stream) {
			goto retry
		}
		if invalidCursor(stream) {
			if positionLost(stream) {
				resumeAfter = nil
				startAt = nil
			}
			stream.Close(context.Background())
			goto restart
		}
		if err = stream.Err(); err != nil {
			stream.Close(context.Background())
			goto restart
		}
		goto retry
	}
	return nil
}

func DirectReadPaged(ctx *OpCtx, client *mongo.Client, ns string, o *Options) (err error) {
	defer ctx.allWg.Done()
	defer ctx.DirectReadWg.Done()
	defer ctx.directReadConcWg.Done()
	n := &N{}
	if err = n.parse(ns); err != nil {
		ctx.ErrC <- errors.Wrap(err, "Error starting direct reads. Invalid namespace.")
		return
	}
	var stats *CollectionStats
	stats, err = GetCollectionStats(ctx, client, ns)
	if err != nil {
		ctx.ErrC <- errors.Wrap(err, fmt.Sprintf(`
		Error reading collection stats for %s. Used to calibrate batch size.
		`, ns))
	}
	c := client.Database(n.database).Collection(n.collection)
	const defaultSegmentSize = 50000
	var maxSplits int32 = o.DirectReadSplitMax
	var segmentSize int32 = defaultSegmentSize
	if stats.Count != 0 {
		segmentSize = stats.Count / (maxSplits + 1)
		if segmentSize < defaultSegmentSize {
			segmentSize = defaultSegmentSize
		}
	}

	segment := &CollectionSegment{
		splitKey: "_id",
	}
	if maxSplits <= 0 {
		ctx.allWg.Add(1)
		ctx.DirectReadWg.Add(1)
		ctx.directReadConcWg.Add(1)
		go DirectReadSegment(ctx, client, ns, o, segment, stats)
		return
	}
	var doc Doc
	var splitCount int32

	pro := bson.M{"id": 1}

	done := false

	opts := &options.FindOneOptions{}
	opts.SetSort(bson.M{"_id": 1})
	opts.SetProjection(pro)
	opts.SetSkip(int64(segmentSize))

	for !done {

		sel := bson.M{}

		if segment.min != nil {
			sel["_id"] = bson.M{"$gte": segment.min}
		}

		err = c.FindOne(context.Background(), sel, opts).Decode(&doc)

		if err == nil && doc.Id != nil {
			segment.max = doc.Id
		} else {
			done = true
		}

		ctx.allWg.Add(1)
		ctx.DirectReadWg.Add(1)
		ctx.directReadConcWg.Add(1)
		go DirectReadSegment(ctx, client, ns, o, segment, stats)

		if !done {
			segment = &CollectionSegment{
				splitKey: "_id",
				min:      segment.max,
			}
			splitCount = splitCount + 1
			if splitCount == maxSplits {
				done = true
				ctx.allWg.Add(1)
				ctx.DirectReadWg.Add(1)
				ctx.directReadConcWg.Add(1)
				go DirectReadSegment(ctx, client, ns, o, segment, stats)
			}
		}
	}
	return
}

func FetchDocuments(ctx *OpCtx, client *mongo.Client, filter OpFilter, buf *OpBuf, inOp OpChan, options *Options) error {
	defer ctx.allWg.Done()
	timer := time.NewTimer(buf.BufferDuration)
	timer.Stop()
	for {
		select {
		case <-ctx.stopC:
			return nil
		case <-timer.C:
			buf.Flush(client, ctx, options)
		case op := <-inOp:
			if op == nil {
				break
			}
			if filter(op) {
				buf.Append(op)
				if buf.IsFull() {
					timer.Stop()
					buf.Flush(client, ctx, options)
				} else if buf.HasOne() {
					if !timer.Stop() {
						select {
						case <-timer.C:
						default:
						}
					}
					timer.Reset(buf.BufferDuration)
				}
			}
		}
	}
	return nil
}

func OpFilterForOrdering(ordering OrderingGuarantee, workers []string, worker string) OpFilter {
	switch ordering {
	case AnyOrder, Document:
		ring := hashring.New(workers)
		return func(op *Op) bool {
			var key string
			if op.Id != nil {
				key = fmt.Sprintf("%v", op.Id)
			} else {
				key = op.Namespace
			}
			if who, ok := ring.GetNode(key); ok {
				return who == worker
			} else {
				return false
			}
		}
	case Namespace:
		ring := hashring.New(workers)
		return func(op *Op) bool {
			if who, ok := ring.GetNode(op.Namespace); ok {
				return who == worker
			} else {
				return false
			}
		}
	default:
		return func(op *Op) bool {
			return true
		}
	}
}

func DefaultOptions() *Options {
	return &Options{
		After:               LastOpTimestamp,
		Filter:              nil,
		NamespaceFilter:     nil,
		OpLogDatabaseName:   "local",
		OpLogCollectionName: "oplog.rs",
		ChannelSize:         2048,
		BufferSize:          50,
		BufferDuration:      time.Duration(75) * time.Millisecond,
		Ordering:            Oplog,
		WorkerCount:         10,
		MaxWaitSecs:         10,
		UpdateDataAsDelta:   false,
		DirectReadNs:        []string{},
		DirectReadFilter:    nil,
		DirectReadSplitMax:  150,
		DirectReadConcur:    0,
		Unmarshal:           defaultUnmarshaller,
		Log:                 log.New(os.Stdout, "INFO ", log.Flags()),
	}
}

func defaultUnmarshaller(namespace string, cursor mongo.Cursor) (interface{}, error) {
	var m map[string]interface{}
	if err := cursor.Decode(&m); err == nil {
		return m, nil
	} else {
		return nil, err
	}
}

func (this *Options) SetDefaults() {
	defaultOpts := DefaultOptions()
	if this.ChannelSize < 1 {
		this.ChannelSize = defaultOpts.ChannelSize
	}
	if this.BufferSize < 1 {
		this.BufferSize = defaultOpts.BufferSize
	}
	if this.BufferDuration == 0 {
		this.BufferDuration = defaultOpts.BufferDuration
	}
	if this.Ordering == Oplog {
		this.WorkerCount = 1
	}
	if this.WorkerCount < 1 {
		this.WorkerCount = 1
	}
	if this.UpdateDataAsDelta {
		this.Ordering = Oplog
		this.WorkerCount = 0
	}
	if this.Unmarshal == nil {
		this.Unmarshal = defaultOpts.Unmarshal
	}
	if this.Log == nil {
		this.Log = defaultOpts.Log
	}
	if this.DirectReadConcur == 0 {
		this.DirectReadConcur = defaultOpts.DirectReadConcur
	}
	if this.DirectReadSplitMax == 0 {
		this.DirectReadSplitMax = defaultOpts.DirectReadSplitMax
	}
	if len(this.ChangeStreamNs) == 0 {
		if this.After == nil {
			this.After = defaultOpts.After
		}
	} else {
		this.OpLogDisabled = true
	}
	if this.OpLogDatabaseName == "" {
		this.OpLogDatabaseName = defaultOpts.OpLogDatabaseName
	}
	if this.OpLogCollectionName == "" {
		this.OpLogCollectionName = defaultOpts.OpLogCollectionName
	}
	if this.MaxWaitSecs == 0 {
		this.MaxWaitSecs = defaultOpts.MaxWaitSecs
	}
}

func Tail(client *mongo.Client, options *Options) (OpChan, chan error) {
	ctx := Start(client, options)
	return ctx.OpC, ctx.ErrC
}

func GetShards(client *mongo.Client) (shardInfos []*ShardInfo) {
	// use this for sharded databases to get the shard hosts
	// use the hostnames to create multiple clients for a call to StartMulti
	col := client.Database("config").Collection("shards")
	opts := &options.FindOptions{}
	cursor, err := col.Find(context.Background(), nil, opts)
	if err != nil {
		return
	}
	for cursor.Next(context.Background()) {
		shard := map[string]interface{}{}
		if err = cursor.Decode(&shard); err != nil {
			continue
		}
		if shard["host"] == nil {
			continue
		}
		shardInfo := &ShardInfo{
			hostname: shard["host"].(string),
		}
		shardInfos = append(shardInfos, shardInfo)
	}
	return
}

func StartMulti(clients []*mongo.Client, options *Options) *OpCtxMulti {
	if options == nil {
		options = DefaultOptions()
	} else {
		options.SetDefaults()
	}

	stopC := make(chan bool, 1)
	errC := make(chan error, options.ChannelSize)
	opC := make(OpChan, options.ChannelSize)

	var directReadWg sync.WaitGroup
	var opWg sync.WaitGroup
	var allWg sync.WaitGroup
	var seekC = make(chan primitive.Timestamp, 1)
	var pauseC = make(chan bool, 1)
	var resumeC = make(chan bool, 1)

	ctxMulti := &OpCtxMulti{
		lock:         &sync.Mutex{},
		OpC:          opC,
		ErrC:         errC,
		DirectReadWg: &directReadWg,
		opWg:         &opWg,
		stopC:        stopC,
		allWg:        &allWg,
		pauseC:       pauseC,
		resumeC:      resumeC,
		seekC:        seekC,
		log:          options.Log,
	}

	ctxMulti.lock.Lock()
	defer ctxMulti.lock.Unlock()

	for _, client := range clients {
		ctx := Start(client, options)
		ctxMulti.contexts = append(ctxMulti.contexts, ctx)
		allWg.Add(1)
		directReadWg.Add(1)
		opWg.Add(2)
		go func() {
			defer directReadWg.Done()
			ctx.DirectReadWg.Wait()
		}()
		go func() {
			defer allWg.Done()
			ctx.allWg.Wait()
		}()
		go func(c OpChan) {
			defer opWg.Done()
			for op := range c {
				opC <- op
			}
		}(ctx.OpC)
		go func(c chan error) {
			defer opWg.Done()
			for err := range c {
				errC <- err
			}
		}(ctx.ErrC)
	}
	return ctxMulti
}

func Start(client *mongo.Client, options *Options) *OpCtx {
	if options == nil {
		options = DefaultOptions()
	} else {
		options.SetDefaults()
	}

	stopC := make(chan bool)
	errC := make(chan error, options.ChannelSize)
	opC := make(OpChan, options.ChannelSize)

	var inOps []OpChan
	var workerNames []string
	var directReadWg sync.WaitGroup
	var directReadConcWg sync.WaitGroup
	var allWg sync.WaitGroup

	streams := len(options.ChangeStreamNs)
	if options.OpLogDisabled == false {
		streams += 1
	}

	var seekC = make(chan primitive.Timestamp, streams)
	var pauseC = make(chan bool, streams)
	var resumeC = make(chan bool, streams)

	ctx := &OpCtx{
		lock:             &sync.Mutex{},
		OpC:              opC,
		ErrC:             errC,
		DirectReadWg:     &directReadWg,
		directReadConcWg: &directReadConcWg,
		stopC:            stopC,
		allWg:            &allWg,
		pauseC:           pauseC,
		resumeC:          resumeC,
		seekC:            seekC,
		log:              options.Log,
	}

	if options.OpLogDisabled == false {
		for i := 1; i <= options.WorkerCount; i++ {
			workerNames = append(workerNames, strconv.Itoa(i))
		}
		for i := 1; i <= options.WorkerCount; i++ {
			allWg.Add(1)
			inOp := make(OpChan, options.ChannelSize)
			inOps = append(inOps, inOp)
			buf := &OpBuf{
				BufferSize:     options.BufferSize,
				BufferDuration: options.BufferDuration,
			}
			worker := strconv.Itoa(i)
			filter := OpFilterForOrdering(options.Ordering, workerNames, worker)
			go FetchDocuments(ctx, client, filter, buf, inOp, options)
		}
	}

	if len(options.DirectReadNs) > 0 {
		directReadWg.Add(1)
		allWg.Add(1)
		go ProcessDirectReads(ctx, client, options)
	}

	for _, ns := range options.ChangeStreamNs {
		allWg.Add(1)
		go ConsumeChangeStream(ctx, client, ns, options)
	}

	if options.OpLogDisabled == false {
		allWg.Add(1)
		go TailOps(ctx, client, inOps, options)
	}

	return ctx
}
