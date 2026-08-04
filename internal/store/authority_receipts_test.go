package store

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"math"
	"slices"
	"testing"
)

func TestR17RowFrameReal(t *testing.T) {
	t.Parallel()
	projection := r17ReceiptProjection("real-fixture", []string{"value"})

	positiveZero := r17BuildReceipt(t, projection, [][]sqliteTableRowsV2Value{{sqliteTableRowsV2RealValue(0)}})
	negativeZero := r17BuildReceipt(t, projection, [][]sqliteTableRowsV2Value{{sqliteTableRowsV2RealValue(math.Copysign(0, -1))}})
	if !bytes.Equal(positiveZero.Rows[0].Frame, negativeZero.Rows[0].Frame) {
		t.Fatal("positive and negative REAL zero did not canonicalize to one row frame")
	}
	if got := binary.BigEndian.Uint64(positiveZero.Rows[0].Frame[len(positiveZero.Rows[0].Frame)-8:]); got != 0 {
		t.Fatalf("canonical zero bits = 0x%016x", got)
	}

	finite := []float64{
		math.SmallestNonzeroFloat64,
		-math.SmallestNonzeroFloat64,
		math.MaxFloat64,
		-math.MaxFloat64,
	}
	for _, value := range finite {
		receipt := r17BuildReceipt(t, projection, [][]sqliteTableRowsV2Value{{sqliteTableRowsV2RealValue(value)}})
		if got := binary.BigEndian.Uint64(receipt.Rows[0].Frame[len(receipt.Rows[0].Frame)-8:]); got != math.Float64bits(value) {
			t.Fatalf("REAL %v bits = 0x%016x, want 0x%016x", value, got, math.Float64bits(value))
		}
	}

	integer := r17BuildReceipt(t, projection, [][]sqliteTableRowsV2Value{{sqliteTableRowsV2IntegerValue(1)}})
	real := r17BuildReceipt(t, projection, [][]sqliteTableRowsV2Value{{sqliteTableRowsV2RealValue(1)}})
	if bytes.Equal(integer.Rows[0].Frame, real.Rows[0].Frame) || integer.Rows[0].Digest == real.Rows[0].Digest {
		t.Fatal("INTEGER 1 and REAL 1.0 collided")
	}

	for _, value := range []float64{math.NaN(), math.Inf(1), math.Inf(-1)} {
		_, err := buildSQLiteTableRowsV2Receipt(projection, [][]sqliteTableRowsV2Value{{sqliteTableRowsV2RealValue(value)}})
		if !errors.Is(err, errSQLiteTableRowsV2NonFinite) {
			t.Fatalf("non-finite REAL %v error = %v", value, err)
		}
	}
}

func TestR17RowFrameProjection(t *testing.T) {
	t.Parallel()
	row := []sqliteTableRowsV2Value{
		sqliteTableRowsV2NullValue(),
		sqliteTableRowsV2TextValue(""),
		sqliteTableRowsV2BlobValue(nil),
	}
	baseProjection := r17ReceiptProjection("projection-a", []string{"nullable", "empty_text", "empty_blob"})
	base := r17BuildReceipt(t, baseProjection, [][]sqliteTableRowsV2Value{row})

	referenceHeader := r17ReferenceProjectionHeader(baseProjection)
	if !bytes.Equal(base.ProjectionHeader, referenceHeader) {
		t.Fatalf("projection header differs from independent encoder\n got: %x\nwant: %x", base.ProjectionHeader, referenceHeader)
	}
	if got := sha256.Sum256(referenceHeader); base.ProjectionDigest != got {
		t.Fatalf("projection digest = %x, want %x", base.ProjectionDigest, got)
	}
	if !bytes.HasPrefix(base.Bytes, []byte(sqliteTableRowsV2Magic)) {
		t.Fatal("table receipt does not begin with the v2 magic")
	}

	permutedProjection := r17ReceiptProjection("projection-a", []string{"empty_text", "nullable", "empty_blob"})
	permutedRow := []sqliteTableRowsV2Value{row[1], row[0], row[2]}
	permuted := r17BuildReceipt(t, permutedProjection, [][]sqliteTableRowsV2Value{permutedRow})
	if base.ProjectionDigest == permuted.ProjectionDigest || base.Rows[0].Digest == permuted.Rows[0].Digest {
		t.Fatal("column permutation was not bound to the projection and row digests")
	}

	tableCollisionProjection := baseProjection
	tableCollisionProjection.TableName = "authority_rows_archive"
	tableCollision := r17BuildReceipt(t, tableCollisionProjection, [][]sqliteTableRowsV2Value{row})
	if base.ProjectionDigest == tableCollision.ProjectionDigest || base.AggregateDigest == tableCollision.AggregateDigest {
		t.Fatal("table identity collision was not separated")
	}

	invalidIdentity := baseProjection
	invalidIdentity.Columns = []string{"valid", string([]byte{0xff}), "also_valid"}
	if _, err := buildSQLiteTableRowsV2Receipt(invalidIdentity, [][]sqliteTableRowsV2Value{row}); !errors.Is(err, errSQLiteTableRowsV2InvalidUTF8) {
		t.Fatalf("invalid projection UTF-8 error = %v", err)
	}
	invalidText := slices.Clone(row)
	invalidText[1] = sqliteTableRowsV2TextValue(string([]byte{0xff}))
	if _, err := buildSQLiteTableRowsV2Receipt(baseProjection, [][]sqliteTableRowsV2Value{invalidText}); !errors.Is(err, errSQLiteTableRowsV2InvalidUTF8) {
		t.Fatalf("invalid TEXT UTF-8 error = %v", err)
	}
}

func TestR17RowFrameOrder(t *testing.T) {
	t.Parallel()
	projection := r17ReceiptProjection("order-fixture", []string{"left", "right"})
	rows := [][]sqliteTableRowsV2Value{
		{sqliteTableRowsV2TextValue("a"), sqliteTableRowsV2IntegerValue(2)},
		{sqliteTableRowsV2TextValue("A"), sqliteTableRowsV2IntegerValue(1)},
		{sqliteTableRowsV2NullValue(), sqliteTableRowsV2BlobValue([]byte{0xff, 0x00})},
		{sqliteTableRowsV2TextValue("a"), sqliteTableRowsV2IntegerValue(2)},
	}
	forward := r17BuildReceipt(t, projection, rows)
	reverseRows := [][]sqliteTableRowsV2Value{rows[3], rows[2], rows[1], rows[0]}
	reverse := r17BuildReceipt(t, projection, reverseRows)
	if !bytes.Equal(forward.Bytes, reverse.Bytes) || forward.AggregateDigest != reverse.AggregateDigest {
		t.Fatal("table receipt depends on input query order")
	}
	if len(forward.Rows) != len(rows) {
		t.Fatalf("row count = %d, want %d", len(forward.Rows), len(rows))
	}
	for index := 1; index < len(forward.Rows); index++ {
		if bytes.Compare(forward.Rows[index-1].Frame, forward.Rows[index].Frame) > 0 {
			t.Fatalf("row frames are not in unsigned bytewise order at %d", index)
		}
	}
	duplicateCount := 0
	for _, candidate := range forward.Rows {
		if bytes.Equal(candidate.Frame, r17BuildReceipt(t, projection, [][]sqliteTableRowsV2Value{rows[0]}).Rows[0].Frame) {
			duplicateCount++
		}
	}
	if duplicateCount != 2 {
		t.Fatalf("duplicate frame multiplicity = %d, want 2", duplicateCount)
	}

	withoutDuplicate := r17BuildReceipt(t, projection, rows[:3])
	if withoutDuplicate.AggregateDigest == forward.AggregateDigest {
		t.Fatal("duplicate row multiplicity was lost from the aggregate")
	}

	for index, value := range []any{nil, int64(-7), float64(1.5), "text", []byte{0x00, 0xff}} {
		if _, err := sqliteTableRowsV2ValueFromDriver(value); err != nil {
			t.Fatalf("driver value %d (%T): %v", index, value, err)
		}
	}
	if _, err := sqliteTableRowsV2ValueFromDriver(int(1)); err == nil {
		t.Fatal("ambiguous non-driver integer type was accepted")
	}
}

func r17ReceiptProjection(id string, columns []string) sqliteTableRowsV2Projection {
	return sqliteTableRowsV2Projection{
		LogicalDatabaseID: "relayops-authority",
		SchemaName:        "main",
		TableName:         "authority_rows",
		ProjectionID:      id,
		ProjectionVersion: "2",
		Columns:           columns,
	}
}

func r17BuildReceipt(t *testing.T, projection sqliteTableRowsV2Projection, rows [][]sqliteTableRowsV2Value) sqliteTableRowsV2Receipt {
	t.Helper()
	receipt, err := buildSQLiteTableRowsV2Receipt(projection, rows)
	if err != nil {
		t.Fatal(err)
	}
	return receipt
}

// r17ReferenceProjectionHeader is intentionally separate from the production
// encoder. It is a fixture-side parser contract for projection framing.
func r17ReferenceProjectionHeader(projection sqliteTableRowsV2Projection) []byte {
	result := append([]byte(nil), []byte("R30-SQLITE-TABLE-ROWS-V2\n")...)
	for _, value := range []string{
		projection.LogicalDatabaseID,
		projection.SchemaName,
		projection.TableName,
		projection.ProjectionID,
		projection.ProjectionVersion,
	} {
		result = append(result, 1)
		result = r17ReferenceUint64(result, uint64(len(value)))
		result = append(result, []byte(value)...)
	}
	result = r17ReferenceUint64(result, uint64(len(projection.Columns)))
	for _, column := range projection.Columns {
		result = append(result, 1)
		result = r17ReferenceUint64(result, uint64(len(column)))
		result = append(result, []byte(column)...)
	}
	return result
}

func r17ReferenceUint64(dst []byte, value uint64) []byte {
	return append(dst,
		byte(value>>56), byte(value>>48), byte(value>>40), byte(value>>32),
		byte(value>>24), byte(value>>16), byte(value>>8), byte(value),
	)
}
