package logstorage

import (
	"bytes"
	"math"
	"strconv"
	"sync"
	"unicode/utf8"

	"github.com/VictoriaMetrics/VictoriaMetrics/lib/bytesutil"
	"github.com/VictoriaMetrics/VictoriaMetrics/lib/encoding"
	"github.com/VictoriaMetrics/VictoriaMetrics/lib/logger"
)

type filter interface {
	// String returns string representation of the filter
	String() string

	// apply must update bm according to the filter applied to the given bs block
	apply(bs *blockSearch, bm *bitmap)
}

// streamFilter is the filter for `_stream:{...}`
type streamFilter struct {
	// f is the filter to apply
	f *StreamFilter

	// tenantIDs is the list of tenantIDs to search for streamIDs.
	tenantIDs []TenantID

	// idb is the indexdb to search for streamIDs.
	idb *indexdb

	streamIDsOnce sync.Once
	streamIDs     map[streamID]struct{}
}

func (fs *streamFilter) String() string {
	s := fs.f.String()
	if s == "{}" {
		return ""
	}
	return "_stream:" + s
}

func (fs *streamFilter) getStreamIDs() map[streamID]struct{} {
	fs.streamIDsOnce.Do(fs.initStreamIDs)
	return fs.streamIDs
}

func (fs *streamFilter) initStreamIDs() {
	streamIDs := fs.idb.searchStreamIDs(fs.tenantIDs, fs.f)
	m := make(map[streamID]struct{}, len(streamIDs))
	for i := range streamIDs {
		m[streamIDs[i]] = struct{}{}
	}
	fs.streamIDs = m
}

func (fs *streamFilter) apply(bs *blockSearch, bm *bitmap) {
	if fs.f.isEmpty() {
		return
	}
	streamIDs := fs.getStreamIDs()
	if _, ok := streamIDs[bs.bsw.bh.streamID]; !ok {
		bm.resetBits()
		return
	}
}

func matchValuesDictByAnyValue(bs *blockSearch, ch *columnHeader, bm *bitmap, values map[string]struct{}) {
	bb := bbPool.Get()
	for i, v := range ch.valuesDict.values {
		if _, ok := values[v]; ok {
			bb.B = append(bb.B, byte(i))
		}
	}
	matchEncodedValuesDict(bs, ch, bm, bb.B)
	bbPool.Put(bb)
}

func matchEncodedValuesDict(bs *blockSearch, ch *columnHeader, bm *bitmap, encodedValues []byte) {
	if len(encodedValues) == 0 {
		// Fast path - the phrase is missing in the valuesDict
		bm.resetBits()
		return
	}
	// Slow path - iterate over values
	visitValues(bs, ch, bm, func(v string) bool {
		if len(v) != 1 {
			logger.Panicf("FATAL: %s: unexpected length for dict value: got %d; want 1", bs.partPath(), len(v))
		}
		n := bytes.IndexByte(encodedValues, v[0])
		return n >= 0
	})
}

func matchMinMaxValueLen(ch *columnHeader, minLen, maxLen uint64) bool {
	bb := bbPool.Get()
	defer bbPool.Put(bb)

	bb.B = strconv.AppendUint(bb.B[:0], ch.minValue, 10)
	s := bytesutil.ToUnsafeString(bb.B)
	if maxLen < uint64(len(s)) {
		return false
	}
	bb.B = strconv.AppendUint(bb.B[:0], ch.maxValue, 10)
	s = bytesutil.ToUnsafeString(bb.B)
	return minLen <= uint64(len(s))
}

func matchBloomFilterAllTokens(bs *blockSearch, ch *columnHeader, tokens []string) bool {
	if len(tokens) == 0 {
		return true
	}
	bf := bs.getBloomFilterForColumn(ch)
	return bf.containsAll(tokens)
}

func visitValues(bs *blockSearch, ch *columnHeader, bm *bitmap, f func(value string) bool) {
	if bm.isZero() {
		// Fast path - nothing to visit
		return
	}
	values := bs.getValuesForColumn(ch)
	bm.forEachSetBit(func(idx int) bool {
		return f(values[idx])
	})
}

func isASCIILowercase(s string) bool {
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c >= utf8.RuneSelf || (c >= 'A' && c <= 'Z') {
			return false
		}
	}
	return true
}

type stringBucket struct {
	a []string
}

func (sb *stringBucket) reset() {
	clear(sb.a)
	sb.a = sb.a[:0]
}

func getStringBucket() *stringBucket {
	v := stringBucketPool.Get()
	if v == nil {
		return &stringBucket{}
	}
	return v.(*stringBucket)
}

func putStringBucket(sb *stringBucket) {
	sb.reset()
	stringBucketPool.Put(sb)
}

var stringBucketPool sync.Pool

func getTokensSkipLast(s string) []string {
	for {
		r, runeSize := utf8.DecodeLastRuneInString(s)
		if !isTokenRune(r) {
			break
		}
		s = s[:len(s)-runeSize]
	}
	return tokenizeStrings(nil, []string{s})
}

func toUint64Range(minValue, maxValue float64) (uint64, uint64) {
	minValue = math.Ceil(minValue)
	maxValue = math.Floor(maxValue)
	return toUint64Clamp(minValue), toUint64Clamp(maxValue)
}

func toUint64Clamp(f float64) uint64 {
	if f < 0 {
		return 0
	}
	if f > math.MaxUint64 {
		return math.MaxUint64
	}
	return uint64(f)
}

func quoteFieldNameIfNeeded(s string) string {
	if isMsgFieldName(s) {
		return ""
	}
	return quoteTokenIfNeeded(s) + ":"
}

func isMsgFieldName(fieldName string) bool {
	return fieldName == "" || fieldName == "_msg"
}

func toUint8String(bs *blockSearch, bb *bytesutil.ByteBuffer, v string) string {
	if len(v) != 1 {
		logger.Panicf("FATAL: %s: unexpected length for binary representation of uint8 number: got %d; want 1", bs.partPath(), len(v))
	}
	n := uint64(v[0])
	bb.B = strconv.AppendUint(bb.B[:0], n, 10)
	return bytesutil.ToUnsafeString(bb.B)
}

func toUint16String(bs *blockSearch, bb *bytesutil.ByteBuffer, v string) string {
	if len(v) != 2 {
		logger.Panicf("FATAL: %s: unexpected length for binary representation of uint16 number: got %d; want 2", bs.partPath(), len(v))
	}
	b := bytesutil.ToUnsafeBytes(v)
	n := uint64(encoding.UnmarshalUint16(b))
	bb.B = strconv.AppendUint(bb.B[:0], n, 10)
	return bytesutil.ToUnsafeString(bb.B)
}

func toUint32String(bs *blockSearch, bb *bytesutil.ByteBuffer, v string) string {
	if len(v) != 4 {
		logger.Panicf("FATAL: %s: unexpected length for binary representation of uint32 number: got %d; want 4", bs.partPath(), len(v))
	}
	b := bytesutil.ToUnsafeBytes(v)
	n := uint64(encoding.UnmarshalUint32(b))
	bb.B = strconv.AppendUint(bb.B[:0], n, 10)
	return bytesutil.ToUnsafeString(bb.B)
}

func toUint64String(bs *blockSearch, bb *bytesutil.ByteBuffer, v string) string {
	if len(v) != 8 {
		logger.Panicf("FATAL: %s: unexpected length for binary representation of uint64 number: got %d; want 8", bs.partPath(), len(v))
	}
	b := bytesutil.ToUnsafeBytes(v)
	n := encoding.UnmarshalUint64(b)
	bb.B = strconv.AppendUint(bb.B[:0], n, 10)
	return bytesutil.ToUnsafeString(bb.B)
}

func toFloat64StringExt(bs *blockSearch, bb *bytesutil.ByteBuffer, v string) string {
	if len(v) != 8 {
		logger.Panicf("FATAL: %s: unexpected length for binary representation of floating-point number: got %d; want 8", bs.partPath(), len(v))
	}
	bb.B = toFloat64String(bb.B[:0], v)
	return bytesutil.ToUnsafeString(bb.B)
}

func toIPv4StringExt(bs *blockSearch, bb *bytesutil.ByteBuffer, v string) string {
	if len(v) != 4 {
		logger.Panicf("FATAL: %s: unexpected length for binary representation of IPv4: got %d; want 4", bs.partPath(), len(v))
	}
	bb.B = toIPv4String(bb.B[:0], v)
	return bytesutil.ToUnsafeString(bb.B)
}

func toTimestampISO8601StringExt(bs *blockSearch, bb *bytesutil.ByteBuffer, v string) string {
	if len(v) != 8 {
		logger.Panicf("FATAL: %s: unexpected length for binary representation of ISO8601 timestamp: got %d; want 8", bs.partPath(), len(v))
	}
	bb.B = toTimestampISO8601String(bb.B[:0], v)
	return bytesutil.ToUnsafeString(bb.B)
}
