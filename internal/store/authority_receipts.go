package store

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"sort"
	"unicode/utf8"
)

const (
	sqliteTableRowsV2Magic           = "R30-SQLITE-TABLE-ROWS-V2\n"
	sqliteTableRowsV2RowDomain       = "r30-sqlite-row-v2\x00"
	sqliteTableRowsV2AggregateDomain = "r30-sqlite-table-v2\x00"
)

var (
	errSQLiteTableRowsV2InvalidUTF8 = errors.New("r30-sqlite-table-rows-v2: invalid UTF-8")
	errSQLiteTableRowsV2NonFinite   = errors.New("r30-sqlite-table-rows-v2: non-finite REAL")
)

type sqliteTableRowsV2Projection struct {
	LogicalDatabaseID string
	SchemaName        string
	TableName         string
	ProjectionID      string
	ProjectionVersion string
	Columns           []string
}

type sqliteTableRowsV2ValueKind byte

const (
	sqliteTableRowsV2Null sqliteTableRowsV2ValueKind = iota
	sqliteTableRowsV2Integer
	sqliteTableRowsV2Real
	sqliteTableRowsV2Text
	sqliteTableRowsV2Blob
)

// sqliteTableRowsV2Value preserves the storage class reported by SQLite.
// Callers must not convert INTEGER to REAL, or TEXT to BLOB, before framing.
type sqliteTableRowsV2Value struct {
	kind    sqliteTableRowsV2ValueKind
	integer int64
	real    float64
	text    string
	blob    []byte
}

type sqliteTableRowsV2RowReceipt struct {
	Frame  []byte
	Digest [sha256.Size]byte
}

type sqliteTableRowsV2Receipt struct {
	ProjectionHeader []byte
	ProjectionDigest [sha256.Size]byte
	Rows             []sqliteTableRowsV2RowReceipt
	AggregateFrame   []byte
	AggregateDigest  [sha256.Size]byte
	Bytes            []byte
}

func sqliteTableRowsV2NullValue() sqliteTableRowsV2Value {
	return sqliteTableRowsV2Value{kind: sqliteTableRowsV2Null}
}

func sqliteTableRowsV2IntegerValue(value int64) sqliteTableRowsV2Value {
	return sqliteTableRowsV2Value{kind: sqliteTableRowsV2Integer, integer: value}
}

func sqliteTableRowsV2RealValue(value float64) sqliteTableRowsV2Value {
	return sqliteTableRowsV2Value{kind: sqliteTableRowsV2Real, real: value}
}

func sqliteTableRowsV2TextValue(value string) sqliteTableRowsV2Value {
	return sqliteTableRowsV2Value{kind: sqliteTableRowsV2Text, text: value}
}

func sqliteTableRowsV2BlobValue(value []byte) sqliteTableRowsV2Value {
	return sqliteTableRowsV2Value{kind: sqliteTableRowsV2Blob, blob: bytes.Clone(value)}
}

// sqliteTableRowsV2ValueFromDriver maps the concrete values returned by
// database/sql to their SQLite storage classes without numeric coercion.
func sqliteTableRowsV2ValueFromDriver(value any) (sqliteTableRowsV2Value, error) {
	switch value := value.(type) {
	case nil:
		return sqliteTableRowsV2NullValue(), nil
	case int64:
		return sqliteTableRowsV2IntegerValue(value), nil
	case float64:
		return sqliteTableRowsV2RealValue(value), nil
	case string:
		return sqliteTableRowsV2TextValue(value), nil
	case []byte:
		return sqliteTableRowsV2BlobValue(value), nil
	default:
		return sqliteTableRowsV2Value{}, fmt.Errorf("r30-sqlite-table-rows-v2: unsupported driver value type %T", value)
	}
}

func sqliteTableRowsV2ValuesFromDriver(values []any) ([]sqliteTableRowsV2Value, error) {
	framed := make([]sqliteTableRowsV2Value, len(values))
	for index, value := range values {
		converted, err := sqliteTableRowsV2ValueFromDriver(value)
		if err != nil {
			return nil, fmt.Errorf("column %d: %w", index, err)
		}
		framed[index] = converted
	}
	return framed, nil
}

func buildSQLiteTableRowsV2Receipt(projection sqliteTableRowsV2Projection, rows [][]sqliteTableRowsV2Value) (sqliteTableRowsV2Receipt, error) {
	header, err := encodeSQLiteTableRowsV2Projection(projection)
	if err != nil {
		return sqliteTableRowsV2Receipt{}, err
	}
	projectionDigest := sha256.Sum256(header)

	rowReceipts := make([]sqliteTableRowsV2RowReceipt, len(rows))
	for index, values := range rows {
		if len(values) != len(projection.Columns) {
			return sqliteTableRowsV2Receipt{}, fmt.Errorf(
				"r30-sqlite-table-rows-v2: row %d has %d values for %d projected columns",
				index, len(values), len(projection.Columns),
			)
		}
		frame, frameErr := encodeSQLiteTableRowsV2Row(projectionDigest, values)
		if frameErr != nil {
			return sqliteTableRowsV2Receipt{}, fmt.Errorf("r30-sqlite-table-rows-v2: row %d: %w", index, frameErr)
		}
		digestInput := make([]byte, 0, len(sqliteTableRowsV2RowDomain)+len(frame))
		digestInput = append(digestInput, sqliteTableRowsV2RowDomain...)
		digestInput = append(digestInput, frame...)
		rowReceipts[index] = sqliteTableRowsV2RowReceipt{Frame: frame, Digest: sha256.Sum256(digestInput)}
	}

	// bytes.Compare is unsigned bytewise ordering. Sorting the complete frame
	// makes the receipt independent of query order, keys and SQLite collations.
	sortSQLiteTableRowsV2Receipts(rowReceipts)

	aggregateFrame := make([]byte, 0, len(header)+8)
	aggregateFrame = append(aggregateFrame, header...)
	aggregateFrame = appendSQLiteTableRowsV2Uint64(aggregateFrame, uint64(len(rowReceipts)))
	for index, row := range rowReceipts {
		aggregateFrame = appendSQLiteTableRowsV2Uint64(aggregateFrame, uint64(index))
		aggregateFrame = append(aggregateFrame, row.Frame...)
		aggregateFrame = append(aggregateFrame, row.Digest[:]...)
	}

	aggregateInput := make([]byte, 0, len(sqliteTableRowsV2AggregateDomain)+len(aggregateFrame))
	aggregateInput = append(aggregateInput, sqliteTableRowsV2AggregateDomain...)
	aggregateInput = append(aggregateInput, aggregateFrame...)
	aggregateDigest := sha256.Sum256(aggregateInput)
	receiptBytes := make([]byte, 0, len(aggregateFrame)+sha256.Size)
	receiptBytes = append(receiptBytes, aggregateFrame...)
	receiptBytes = append(receiptBytes, aggregateDigest[:]...)

	return sqliteTableRowsV2Receipt{
		ProjectionHeader: bytes.Clone(header),
		ProjectionDigest: projectionDigest,
		Rows:             rowReceipts,
		AggregateFrame:   aggregateFrame,
		AggregateDigest:  aggregateDigest,
		Bytes:            receiptBytes,
	}, nil
}

func encodeSQLiteTableRowsV2Projection(projection sqliteTableRowsV2Projection) ([]byte, error) {
	header := make([]byte, 0, len(sqliteTableRowsV2Magic)+64)
	header = append(header, sqliteTableRowsV2Magic...)
	identityFields := []struct {
		name  string
		value string
	}{
		{name: "logical database ID", value: projection.LogicalDatabaseID},
		{name: "schema name", value: projection.SchemaName},
		{name: "table name", value: projection.TableName},
		{name: "projection ID", value: projection.ProjectionID},
		{name: "projection version", value: projection.ProjectionVersion},
	}
	var err error
	for _, field := range identityFields {
		header, err = appendSQLiteTableRowsV2PresentField(header, field.name, field.value)
		if err != nil {
			return nil, err
		}
	}
	header = appendSQLiteTableRowsV2Uint64(header, uint64(len(projection.Columns)))
	for index, column := range projection.Columns {
		header, err = appendSQLiteTableRowsV2PresentField(header, fmt.Sprintf("column %d name", index), column)
		if err != nil {
			return nil, err
		}
	}
	return header, nil
}

func encodeSQLiteTableRowsV2Row(projectionDigest [sha256.Size]byte, values []sqliteTableRowsV2Value) ([]byte, error) {
	frame := make([]byte, 0, 4+sha256.Size+8+len(values)*9)
	frame = append(frame, 'R', 'O', 'W', 0)
	frame = append(frame, projectionDigest[:]...)
	frame = appendSQLiteTableRowsV2Uint64(frame, uint64(len(values)))
	for index, value := range values {
		var err error
		frame, err = appendSQLiteTableRowsV2Value(frame, value)
		if err != nil {
			return nil, fmt.Errorf("column %d: %w", index, err)
		}
	}
	return frame, nil
}

func appendSQLiteTableRowsV2Value(dst []byte, value sqliteTableRowsV2Value) ([]byte, error) {
	dst = append(dst, byte(value.kind))
	switch value.kind {
	case sqliteTableRowsV2Null:
		return dst, nil
	case sqliteTableRowsV2Integer:
		return appendSQLiteTableRowsV2Uint64(dst, uint64(value.integer)), nil
	case sqliteTableRowsV2Real:
		if math.IsNaN(value.real) || math.IsInf(value.real, 0) {
			return nil, errSQLiteTableRowsV2NonFinite
		}
		bits := math.Float64bits(value.real)
		if value.real == 0 {
			bits = 0
		}
		return appendSQLiteTableRowsV2Uint64(dst, bits), nil
	case sqliteTableRowsV2Text:
		if !utf8.ValidString(value.text) {
			return nil, fmt.Errorf("%w in TEXT", errSQLiteTableRowsV2InvalidUTF8)
		}
		dst = appendSQLiteTableRowsV2Uint64(dst, uint64(len(value.text)))
		return append(dst, value.text...), nil
	case sqliteTableRowsV2Blob:
		dst = appendSQLiteTableRowsV2Uint64(dst, uint64(len(value.blob)))
		return append(dst, value.blob...), nil
	default:
		return nil, fmt.Errorf("r30-sqlite-table-rows-v2: unknown SQLite value tag 0x%02x", byte(value.kind))
	}
}

func appendSQLiteTableRowsV2PresentField(dst []byte, name, value string) ([]byte, error) {
	if !utf8.ValidString(value) {
		return nil, fmt.Errorf("%w in %s", errSQLiteTableRowsV2InvalidUTF8, name)
	}
	dst = append(dst, 0x01)
	dst = appendSQLiteTableRowsV2Uint64(dst, uint64(len(value)))
	return append(dst, value...), nil
}

func appendSQLiteTableRowsV2Uint64(dst []byte, value uint64) []byte {
	var encoded [8]byte
	binary.BigEndian.PutUint64(encoded[:], value)
	return append(dst, encoded[:]...)
}

func sortSQLiteTableRowsV2Receipts(receipts []sqliteTableRowsV2RowReceipt) {
	sort.Slice(receipts, func(left, right int) bool {
		return bytes.Compare(receipts[left].Frame, receipts[right].Frame) < 0
	})
}
