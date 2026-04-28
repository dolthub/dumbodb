package aggregations_test

import (
	"testing"

	"go.mongodb.org/mongo-driver/v2/bson"

	"github.com/dolthub/dumbodb/internal/handler/common/aggregations"
	"github.com/dolthub/dumbodb/internal/types"
)

func TestDecimal128Sum(t *testing.T) {
	// Verify that summing Decimal128 + float64 produces the expected Decimal128 result.
	// This corresponds to the FerretDB TestAggregateGroupSumDecimalDouble integration test.
	d128, err := bson.ParseDecimal128("42.1")
	if err != nil {
		t.Fatal(err)
	}
	h, l := d128.GetBytes()
	dec := types.Decimal128{H: h, L: l}

	result := aggregations.SumNumbers(dec, float64(42.1))

	expectedDec128 := bson.NewDecimal128(3459220962935157325, 6906845732440572485)
	eh, el := expectedDec128.GetBytes()

	rd, ok := result.(types.Decimal128)
	if !ok {
		t.Errorf("Expected types.Decimal128, got %T: %v", result, result)
		return
	}

	if rd.H != eh || rd.L != el {
		t.Errorf("Wrong decimal128 value: got H=%d L=%d, want H=%d L=%d", rd.H, rd.L, eh, el)
	}
}
