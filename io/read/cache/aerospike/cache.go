package aerospike

import (
	"bytes"
	"compress/gzip"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	as "github.com/aerospike/aerospike-client-go"
	"github.com/aerospike/aerospike-client-go/types"
	"github.com/google/uuid"
	"github.com/viant/parsly/matcher"
	"github.com/viant/sqlx/io"
	"github.com/viant/sqlx/io/read/cache"
	"github.com/viant/sqlx/io/read/cache/hash"
	sio "io"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	sqlBin      = "SQL"
	argsBin     = "Args"
	dataBin     = "Data"
	compDataBin = "CData"
	typesBin    = "Type"
	fieldsBin   = "Fields"
	childBin    = "Child"
	columnBin   = "Column"
)

var cachedBins = []string{typesBin, argsBin, sqlBin, dataBin, fieldsBin, compDataBin}

type (
	Cache struct {
		recorder          cache.Recorder
		typeHolder        *cache.ScanTypeHolder
		client            *as.Client
		set               string
		namespace         string
		mux               sync.Mutex
		expirationTimeInS uint32
		allowSmart        bool
		chanSize          int
		timeoutConfig     *TimeoutConfig
	}
)

func (a *Cache) IndexBy(ctx context.Context, db *sql.DB, column, SQL string, args []interface{}) error {
	if args == nil {
		args = []interface{}{}
	}

	querySQL, isOrdered := tryOrderedSQL(SQL, column)
	rows, err := db.Query(querySQL, args...)
	if err != nil {
		return err
	}

	defer func() {
		_ = rows.Close()
	}()

	columnTypes, err := rows.ColumnTypes()
	if err != nil {
		return err
	}

	fields, err := cache.ColumnsToFields(io.TypesToColumns(columnTypes))
	if err != nil {
		return err
	}

	var values = make(chan *cache.Indexed, 512)
	errors := &Errors{}
	go func() {
		err = a.fetchAndIndexValues(fields, column, rows, values, isOrdered)
		errors.Add(err)
		close(values)
	}()

	URL, err := hash.GenerateURL(SQL, "", "", args)
	if err != nil {
		return err
	}

	argsMarshal, err := json.Marshal(args)
	if err != nil {
		return err
	}

	fieldMarshal, err := json.Marshal(fields)
	if err != nil {
		return err
	}

	argsStringified := string(argsMarshal)
	fieldsStringified := string(fieldMarshal)

	for value := range values {
		var metaBin as.BinMap
		if column == "" {
			metaBin = a.metaBin(SQL, argsStringified, fieldsStringified, column)
		} else {
			metaBin = as.BinMap{}
		}

		errors.Add(a.writeIndexData(value, URL, column, metaBin))
	}

	if err = errors.Err(); err != nil {
		return err
	}

	if column != "" {
		return a.putColumnMarker(URL, column, a.metaBin(SQL, argsStringified, fieldsStringified, column))
	}

	return nil
}

func tryOrderedSQL(SQL string, column string) (string, bool) {
	lcSQL := strings.ToLower(SQL)
	orderByIndex := strings.LastIndex(lcSQL, "order ")
	if orderByIndex != -1 && !matcher.IsWhiteSpace(lcSQL[orderByIndex-1]) {
		orderByIndex = -1
	}
	hasOrderBy := orderByIndex != -1
	if hasOrderBy {
		orderClause := string(lcSQL[orderByIndex+len("order ")])
		return SQL, strings.Contains(orderClause, strings.ToLower(column))
	}
	return SQL + " ORDER BY " + column, true
}

func (a *Cache) metaBin(SQL string, argsStringified string, fieldsStringified string, column string) as.BinMap {
	metaBin := as.BinMap{
		sqlBin:    SQL,
		argsBin:   argsStringified,
		fieldsBin: fieldsStringified,
		columnBin: column,
	}

	return metaBin
}

func (a *Cache) Rollback(ctx context.Context, entry *cache.Entry) error {
	return a.Delete(ctx, entry)
}

func New(namespace string, setName string, client *as.Client, expirationTimeInS uint32, options ...interface{}) (*Cache, error) {
	var recorder cache.Recorder
	var allowSmart bool
	var timeoutConfig *TimeoutConfig
	for _, anOption := range options {
		switch actual := anOption.(type) {
		case cache.Recorder:
			recorder = actual
		case cache.AllowSmart:
			allowSmart = bool(actual)
		case *TimeoutConfig:
			timeoutConfig = actual
		}
	}

	return &Cache{
		client:            client,
		namespace:         namespace,
		set:               setName,
		recorder:          recorder,
		expirationTimeInS: expirationTimeInS,
		allowSmart:        allowSmart,
		timeoutConfig:     timeoutConfig,
	}, nil
}

func (a *Cache) AsSource(ctx context.Context, entry *cache.Entry) (cache.Source, error) {
	return &Source{
		cache: a,
		entry: entry,
	}, nil
}

func (a *Cache) AddValues(ctx context.Context, entry *cache.Entry, values []interface{}) error {
	if a.recorder != nil {
		a.recorder.AddValues(values)
	}

	marshal, err := json.Marshal(values)
	if err != nil {
		return err
	}

	return entry.Write(marshal)
}

func (a *Cache) Get(ctx context.Context, SQL string, args []interface{}, options ...interface{}) (*cache.Entry, error) {
	var columnsInMatcher *cache.Index
	for _, option := range options {
		switch actual := option.(type) {
		case *cache.Index:
			columnsInMatcher = actual
		}
	}

	if columnsInMatcher != nil {
		columnsInMatcher.Init()
	}

	fullMatch, columnsInMatch, errors := a.readRecords(SQL, args, columnsInMatcher)
	for _, err := range errors {
		if a.isServerNotAvailableErr(err) {
			return nil, nil
		} else if err != nil {
			return nil, err
		}
	}

	argsMarshal, err := json.Marshal(args)
	if err != nil {
		return nil, err
	}

	anEntry := &cache.Entry{
		Meta: cache.Meta{
			SQL:        SQL,
			Args:       argsMarshal,
			TimeToLive: int(time.Now().Add(time.Duration(a.expirationTimeInS)).UnixNano()),
		},
		Id: a.entryId(fullMatch, columnsInMatch),
	}

	if err = a.updateFullMatchEntry(ctx, anEntry, fullMatch, SQL, argsMarshal); err != nil {
		return nil, err
	}

	if err = a.updateColumnsInMatchEntry(anEntry, columnsInMatch, columnsInMatcher); err != nil {
		return nil, err
	}

	if err = a.updateMetaFields(anEntry, fullMatch, columnsInMatch); err != nil {
		return nil, err
	}

	return anEntry, a.updateWriter(anEntry, fullMatch, SQL, argsMarshal)
}

func (a *Cache) readRecords(SQL string, args []interface{}, matcher *cache.Index) (fullMatch *RecordMatched, columnsInMatch *RecordMatched, errors []error) {
	errors = make([]error, 2)
	wg := sync.WaitGroup{}

	wg.Add(2)
	go func(SQL string, args []interface{}, wg *sync.WaitGroup) {
		defer wg.Done()
		fullMatch, errors[0] = a.readRecord(SQL, args, nil)
	}(SQL, args, &wg)

	go func(matcher *cache.Index) {
		defer wg.Done()
		if matcher == nil {
			return
		}

		argsMarshal, err := matcher.MarshalArgs()
		if err != nil {
			errors[1] = err
			return
		}

		columnsInMatch, errors[1] = a.readRecord(matcher.SQL, matcher.Args, argsMarshal, func(aKey string) (string, error) {
			return a.columnURL(aKey, matcher.By), nil
		})
	}(matcher)
	wg.Wait()

	for i, err := range errors {
		if a.isKeyNotFoundErr(err) {
			errors[i] = nil
		}
	}

	return fullMatch, columnsInMatch, errors
}

func (a *Cache) readRecord(SQL string, args []interface{}, argsMarshal []byte, keyModifiers ...func(aKey string) (string, error)) (*RecordMatched, error) {
	var aKey string
	var err error

	if argsMarshal == nil {
		aKey, err = hash.GenerateURL(SQL, "", "", args)
	} else {
		aKey, err = hash.GenerateWithMarshal(SQL, "", "", argsMarshal)
	}

	if err != nil {
		return nil, err
	}

	for _, modifier := range keyModifiers {
		aKey, err = modifier(aKey)
		if err != nil {
			return nil, err
		}
	}

	fullMatchKey, err := a.key(aKey)
	if err != nil {
		return nil, err
	}

	policy := a.readPolicy()
	record, err := a.client.Get(policy, fullMatchKey, cachedBins...)

	return &RecordMatched{
		key:      fullMatchKey,
		record:   record,
		keyValue: aKey,
		hasKey:   err == nil,
	}, err
}

func (a *Cache) readPolicy() *as.BasePolicy {
	policy := as.NewPolicy()
	if a.timeoutConfig != nil {
		if a.timeoutConfig.MaxRetries != 0 {
			policy.MaxRetries = a.timeoutConfig.MaxRetries
		}

		if a.timeoutConfig.TotalTimeoutInS != 0 {
			policy.TotalTimeout = time.Second * time.Duration(a.timeoutConfig.TotalTimeoutInS)
		}
	}
	return policy
}

func (a *Cache) AssignRows(entry *cache.Entry, rows *sql.Rows) error {
	return entry.AssignRows(rows)
}

func (a *Cache) UpdateType(ctx context.Context, entry *cache.Entry, args []interface{}) (bool, error) {
	a.ensureTypeHolder(args)

	if !a.typeHolder.Match(entry) {
		return false, a.Delete(ctx, entry)
	}

	return true, nil
}

func (a *Cache) Close(ctx context.Context, entry *cache.Entry) error {
	err := entry.Close()
	if err != nil {
		_ = a.Delete(ctx, entry)
		return err
	}

	return nil
}

func (a *Cache) Delete(ctx context.Context, entry *cache.Entry) error {
	key, err := a.key(entry.Id)
	if err != nil {
		return err
	}

	return a.deleteCascade(key)
}

func (a *Cache) deleteCascade(key *as.Key) error {
	var err error
	aRecord, _ := a.client.Get(as.NewPolicy(), key, childBin)
	var ok bool
	for aRecord != nil {
		if ok, err = a.client.Delete(as.NewWritePolicy(0, a.expirationTimeInS), key); err != nil || !ok {
			return err
		}

		childKey := aRecord.Bins[childBin]
		if childKey == nil {
			return nil
		}

		key, err = a.key(childKey)
	}

	return nil
}

func (a *Cache) recordMatches(record *as.Record, SQL string, args []byte) bool {
	if record == nil {
		return false
	}

	sqlValue, ok := record.Bins[sqlBin].(string)
	if !ok || sqlValue != SQL {
		return false
	}

	argsValue, ok := record.Bins[argsBin].(string)
	if !ok || argsValue != string(args) {
		return false
	}

	return true
}

func (a *Cache) newWriter(key *as.Key, aKey string, SQL string, args []byte) *Writer {
	return &Writer{
		client:                  a.client,
		expirationTimeInSeconds: a.expirationTimeInS,
		mainKey:                 key,
		buffers:                 []*bytes.Buffer{bytes.NewBuffer(nil)},
		id:                      aKey,
		sql:                     SQL,
		args:                    string(args),
		cache:                   a,
	}
}

func (a *Cache) key(keyValue interface{}) (*as.Key, error) {
	aKey, err := as.NewKey(a.namespace, a.set, keyValue)
	return aKey, err
}

func (a *Cache) reader(key *as.Key, record *as.Record) (*Reader, error) {

	return &Reader{
		key:       key,
		client:    a.client,
		namespace: a.namespace,
		record:    record,
		set:       a.set,
	}, nil
}

func (a *Cache) ensureTypeHolder(values []interface{}) {
	if a.typeHolder != nil {
		return
	}

	a.mux.Lock()
	a.typeHolder = &cache.ScanTypeHolder{}
	a.typeHolder.InitType(values)
	a.mux.Unlock()
}

func (a *Cache) updateEntry(record *as.Record, entry *cache.Entry) error {
	return a.updateEntryFields(record, entry)
}

func (a *Cache) updateEntryFields(record *as.Record, entry *cache.Entry) error {
	fieldsValue := record.Bins[fieldsBin]
	if fieldsValue == nil {
		return nil
	}

	fieldsJSON, ok := fieldsValue.(string)
	if !ok {
		return fmt.Errorf("unexpected cache value type, expected %T got %T", fieldsJSON, fieldsValue)
	}

	var fields []*cache.Field
	if err := json.Unmarshal([]byte(fieldsJSON), &fields); err != nil {
		return err
	}

	entry.Meta.Fields = fields

	for _, field := range entry.Meta.Fields {
		if err := field.Init(); err != nil {
			return err
		}
	}

	return nil
}

func (a *Cache) writeIndexData(args *cache.Indexed, URL string, column string, metaBin as.BinMap) error {
	if args.ColumnValue == nil && args.Column != "" {
		return nil
	}

	marshal, err := json.Marshal(args.ColumnValue)
	if err != nil {
		return err
	}

	actualKey := a.columnValueURL(column, marshal, URL)
	key, err := a.key(actualKey)
	if err != nil {
		return err
	}

	data := args.Data.Bytes()
	isCompressed := false
	if len(data) > compressionThreshold {
		compressed, ok := compress(data)
		isCompressed = ok

		if ok {
			metaBin[compDataBin] = compressed
		}
	}

	if !isCompressed {
		metaBin[dataBin] = string(data)
	}

	return a.client.Put(a.writePolicy(), key, metaBin)
}

func compress(data []byte) ([]byte, bool) {
	buffer := &bytes.Buffer{}
	gzipWriter := gzip.NewWriter(buffer)
	if _, err := sio.Copy(gzipWriter, bytes.NewBuffer(data)); err == nil {
		if err = gzipWriter.Flush(); err == nil {
			_ = gzipWriter.Close()
			return buffer.Bytes(), true
		}
	}
	return nil, false
}

func uncompress(data []byte) ([]byte, error) {
	gzipReader, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		return nil, err
	}

	defer gzipReader.Close()
	buffer := bytes.NewBuffer(nil)
	_, err = sio.Copy(buffer, gzipReader)
	if err != nil {
		return nil, err
	}

	return buffer.Bytes(), nil
}

func (a *Cache) columnValueURL(column string, columnValueMarshal []byte, URL string) string {
	if column == "" {
		return URL
	}

	return strings.ToLower(column) + "#" + strconv.Quote(string(columnValueMarshal)) + "#" + URL
}

func (a *Cache) writePolicy() *as.WritePolicy {
	policy := as.NewWritePolicy(0, a.expirationTimeInS)
	policy.SendKey = true
	policy.MaxRetries = 3
	return policy
}

func (a *Cache) putColumnMarker(URL string, column string, bin as.BinMap) error {
	aKey, err := a.key(a.columnURL(URL, column))
	if err != nil {
		return err
	}

	return a.client.Put(a.writePolicy(), aKey, bin)
}

func (a *Cache) columnURL(URL string, column string) string {
	return strings.ToLower(column) + "#" + URL
}

func (a *Cache) updateFullMatchEntry(ctx context.Context, anEntry *cache.Entry, match *RecordMatched, SQL string, argsMarshal []byte) error {
	if match == nil || !match.hasKey {
		return nil
	}

	if !a.recordMatches(match.record, SQL, argsMarshal) {
		if match.record != nil {
			_ = a.Delete(ctx, anEntry)
		}

		return nil
	}

	reader, err := a.reader(match.key, match.record)
	if err != nil {
		return err
	}

	anEntry.SetReader(reader, reader)
	return nil
}

func (a *Cache) updateColumnsInMatchEntry(entry *cache.Entry, match *RecordMatched, matcher *cache.Index) error {
	if match == nil || entry.ReadCloser != nil || !match.hasKey {
		return nil
	}

	args, err := matcher.MarshalArgs()
	if err != nil {
		return err
	}

	if !a.recordMatches(match.record, matcher.SQL, args) {
		return nil
	}

	multiReader := NewMultiReader(matcher)

	chanSize := len(matcher.In)

	readerChan := make(chan *readerWrapper, chanSize)
	if chanSize == 0 {
		close(readerChan)
	}

	for i := range matcher.In {
		a.readChan(readerChan, matcher, matcher.In[i])
	}

	counter := 0
	for reader := range readerChan {
		if reader.err != nil {
			return reader.err
		}

		if reader.reader != nil {
			multiReader.AddReader(reader.reader)
		}

		counter++
		if counter == chanSize {
			close(readerChan)
		}
	}

	entry.SetReader(multiReader, multiReader)
	return nil
}

func (a *Cache) updateWriter(anEntry *cache.Entry, fullMatch *RecordMatched, SQL string, argsMarshal []byte) error {
	if anEntry.ReadCloser != nil {
		return nil
	}

	anEntry.Id += uuid.New().String()
	writer := a.newWriter(fullMatch.key, fullMatch.keyValue, SQL, argsMarshal)
	anEntry.SetWriter(writer, writer)
	writer.entry = anEntry
	return nil
}

func (a *Cache) readChan(readerChan chan *readerWrapper, matcher *cache.Index, columnValue interface{}) {
	go func(matcher *cache.Index, columnValue interface{}) {
		reader, err := a.newReader(matcher, columnValue)
		readerChan <- &readerWrapper{
			err:    err,
			reader: reader,
		}
	}(matcher, columnValue)
}

func (a *Cache) newReader(matcher *cache.Index, columnValue interface{}) (*Reader, error) {
	valueMarshal, err := json.Marshal(columnValue)
	if err != nil {
		return nil, err
	}

	args, err := matcher.MarshalArgs()
	if err != nil {
		return nil, err
	}

	actualKeyValue, err := hash.GenerateWithMarshal(matcher.SQL, "", "", args)
	if err != nil {
		return nil, err
	}

	actualKeyValue = a.columnValueURL(matcher.By, valueMarshal, actualKeyValue)
	aKey, err := a.key(actualKeyValue)
	if err != nil {
		return nil, err
	}

	record, err := a.client.Get(a.readPolicy(), aKey, cachedBins...)
	if a.isKeyNotFoundErr(err) {
		return nil, nil
	} else if err != nil {
		return nil, err
	}

	return a.reader(aKey, record)
}

func (a *Cache) isKeyNotFoundErr(err error) bool {
	if err == nil {
		return false
	}

	aeroErr, ok := err.(types.AerospikeError)
	if !ok {
		return false
	}

	code := aeroErr.ResultCode()
	return code == types.KEY_NOT_FOUND_ERROR
}

func (a *Cache) isServerNotAvailableErr(err error) bool {
	if err == nil {
		return false
	}

	aeroErr, ok := err.(types.AerospikeError)
	if !ok {
		return false
	}

	code := aeroErr.ResultCode()
	return code == types.TIMEOUT || code < 0
}

func (a *Cache) entryId(fullMatch *RecordMatched, columnsInMatch *RecordMatched) string {
	if fullMatch != nil {
		return fullMatch.keyValue
	}

	if columnsInMatch != nil {
		return columnsInMatch.keyValue
	}

	return ""
}

func (a *Cache) updateMetaFields(entry *cache.Entry, match *RecordMatched, columnsInMatch *RecordMatched) error {
	var record *as.Record
	if match != nil {
		record = match.record
	}

	if record == nil && columnsInMatch != nil {
		record = columnsInMatch.record
	}

	if record == nil {
		return nil
	}

	cacheFields := record.Bins[fieldsBin]
	if cacheFields == nil {
		return fmt.Errorf("not found %v bin in cache entry ", fieldsBin)
	}

	fieldsStr, ok := cacheFields.(string)
	if !ok {
		return fmt.Errorf("expected fields to be type of %T but got %T", fieldsStr, cacheFields)
	}

	if err := json.Unmarshal([]byte(fieldsStr), &entry.Meta.Fields); err != nil {
		return err
	}

	for _, field := range entry.Meta.Fields {
		if err := field.Init(); err != nil {
			return err
		}
	}

	return nil
}

func (a *Cache) fetchAndIndexValues(fields []*cache.Field, column string, rows *sql.Rows, dest chan *cache.Indexed, ordered bool) error {
	indexSource, err := NewIndexSource(column, ordered, fields, dest)
	if err != nil {
		return err
	}

	columnIndex := indexSource.ColumnIndex()
	placeholders := NewPlaceholders(columnIndex, fields)

	for rows.Next() {
		if err = rows.Scan(placeholders.ScanPlaceholders()...); err != nil {
			return err
		}

		columnValue, ok := placeholders.ColumnValue()
		if !ok {
			continue
		}

		indexed := indexSource.Index(columnValue)
		indexed.Column = column

		if err = indexed.StringifyData(placeholders.Values()); err != nil {
			return err
		}
	}

	return indexSource.Close()
}
