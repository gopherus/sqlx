package aerospike

import (
	"context"
	"github.com/viant/sqlx/io"
	"github.com/viant/sqlx/io/read/cache"
	"github.com/viant/xunsafe"
)

type Source struct {
	cache         *Cache
	entry         *cache.Entry
	scanner       cache.ScannerFn
	columnsHolder *cache.ColumnsHolder
	xtypesHolder  *cache.XTypesHolder
}

func (s *Source) ConvertColumns() ([]io.Column, error) {
	s.ensureColumnsHolder()
	return s.columnsHolder.ConvertColumns()
}

func (s *Source) Scanner(ctx context.Context) cache.ScannerFn {
	if s.scanner != nil {
		return s.scanner
	}

	s.scanner = cache.NewScanner(s.cache.typeHolder, s.cache.recorder).New(s.entry)
	return s.scanner
}

func (s *Source) XTypes() []*xunsafe.Type {
	s.ensureXTypesHolder()

	return s.xtypesHolder.XTypes()
}

func (s *Source) CheckType(ctx context.Context, values []interface{}) (bool, error) {
	return s.cache.UpdateType(ctx, s.entry, values)
}

func (s *Source) Close(ctx context.Context) error {
	return s.cache.Close(ctx, s.entry)
}

func (s *Source) Next() bool {
	return s.entry.Next()
}

func (s *Source) Rollback(ctx context.Context) error {
	return s.cache.Delete(ctx, s.entry)
}

func (s *Source) ensureColumnsHolder() {
	if s.columnsHolder != nil {
		return
	}

	s.columnsHolder = cache.NewColumnsHolder(s.entry)
}

func (s *Source) ensureXTypesHolder() {
	if s.xtypesHolder != nil {
		return
	}

	s.xtypesHolder = cache.NewXTypeHolder(s.entry)
}
