// Copyright 2026 Dolthub, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package index_test

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"math"
	"math/big"
	"math/rand/v2"
	"net"
	"os"
	"os/exec"
	"slices"
	"strings"
	"testing"
	"time"

	mongobson "go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"

	"github.com/dolthub/dumbodb/internal/bson"
	"github.com/dolthub/dumbodb/internal/index"
	"github.com/dolthub/dumbodb/internal/types"
	"github.com/dolthub/dumbodb/internal/util/must"
)

// randomValues is how many random values TestKeyStringMatchesMongod adds to
// its corpus.
var randomValues = 3000

// TestKeyStringMatchesMongod checks EncodeValue against mongod's KeyString:
// in a clustered collection the RecordId is KeyString(_id) without TypeBits,
// and find().showRecordId() returns it. Every value that may be an _id must
// encode to the same bytes, so equality and order match mongod's by
// construction.
func TestKeyStringMatchesMongod(t *testing.T) {
	mongod := os.Getenv("MONGOD_BIN")
	if mongod == "" {
		t.Skip("set MONGOD_BIN to compare KeyString encoding with mongod")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	coll := startClusteredCollection(t, ctx, mongod)

	values := keyStringCorpus()
	r := rand.New(rand.NewPCG(206, 207))
	for range randomValues {
		values = append(values, randomIDValue(r, 0))
	}

	var rejected, duplicates int
	var dupChecks []any
	byRaw := map[string]any{}
	for _, v := range values {
		raw := idDocument(v)
		byRaw[rawKey(raw.Lookup("_id"))] = v
		_, err := coll.InsertOne(ctx, raw)
		switch {
		// mongod reports an _id equal to a stored one of another numeric
		// type as a KeyString format error rather than a duplicate key.
		case mongo.IsDuplicateKeyError(err), err != nil && strings.Contains(err.Error(), "KeyString format error"):
			duplicates++
			dupChecks = append(dupChecks, v)
		case err != nil:
			rejected++
			t.Logf("mongod rejected _id %s: %v", describe(v), err)
		}
	}

	stored := recordIDs(t, ctx, coll, mongobson.D{})
	var mismatches []string
	var prev []byte
	var misordered int
	for _, s := range stored {
		id := byRaw[s.rawID]
		got := index.EncodeID(id)
		if !bytes.Equal(got, s.recordID) && !(orderOnly(id) && leadingMatch(got, s.recordID)) {
			mismatches = append(mismatches, fmt.Sprintf("%s\n\tmongod % X\n\tours   % X", describe(id), s.recordID, got))
		}
		if _, ok := index.SplitKeyValues(s.recordID, 1); !ok {
			t.Errorf("mongod's encoding of %s does not split as one value: % X", describe(id), s.recordID)
		}
		if prev != nil && bytes.Compare(prev, got) >= 0 {
			misordered++
		}
		prev = got
	}
	for _, v := range dupChecks {
		var filter mongobson.D
		must.NoError(mongobson.Unmarshal(idDocument(v), &filter))
		existing := recordIDs(t, ctx, coll, filter)
		if len(existing) != 1 {
			t.Errorf("duplicate _id %s: found %d stored matches", describe(v), len(existing))
			continue
		}
		if got := index.EncodeID(v); !bytes.Equal(got, existing[0].recordID) && !orderOnly(v) {
			mismatches = append(mismatches, fmt.Sprintf("%s (equal to stored %s)\n\tmongod % X\n\tours   % X",
				describe(v), describe(byRaw[existing[0].rawID]), existing[0].recordID, got))
		}
	}

	t.Logf("%d values: %d stored, %d duplicates of stored values, %d rejected by mongod", len(values), len(stored), duplicates, rejected)
	if misordered > 0 {
		t.Errorf("%d of %d stored values encode out of mongod's order", misordered, len(stored))
	}
	if len(mismatches) > 0 {
		shown := mismatches[:min(len(mismatches), 25)]
		t.Errorf("%d of %d encodings differ from mongod's KeyString; first %d:\n%s",
			len(mismatches), len(stored)+len(dupChecks), len(shown), strings.Join(shown, "\n"))
	}
}

// storedID is a stored document's _id, as a rawKey, and its RecordId.
// leadingMatch reports whether an orderOnly value's encoding matches mongod's
// in length and in everything but the continuation of each band decimal: the
// type byte and leading double. A value holding one decimal is checked whole
// but for its last 8 bytes; one nesting several is checked for length alone.
func leadingMatch(got, want []byte) bool {
	if len(got) != len(want) {
		return false
	}
	if len(got) == 17 {
		return bytes.Equal(got[:9], want[:9])
	}
	return true
}

// orderOnly reports values whose continuation bytes are not compared: those
// holding an inexact Decimal128 of magnitude below 2^-255, where mongod's
// continuation comes from Decimal128 arithmetic on the 2^768-scaled value
// that is not reproduced. Leading bytes, order and uniqueness still match
// (docs/reference/mongodb-keystring.md, "Known deviation").
func orderOnly(v any) bool {
	switch v := v.(type) {
	case *types.Document:
		return slices.ContainsFunc(v.Values(), orderOnly)
	case *types.Array:
		for i := range v.Len() {
			if orderOnly(must.NotFail(v.Get(i))) {
				return true
			}
		}
		return false
	}
	d, ok := v.(types.Decimal128)
	if !ok || d.H>>61&3 == 3 {
		return false
	}
	coef := new(big.Int).Lsh(new(big.Int).SetUint64(d.H&(1<<49-1)), 64)
	coef.Or(coef, new(big.Int).SetUint64(d.L))
	if coef.Sign() == 0 {
		return false
	}
	mag := new(big.Rat).SetInt(coef)
	ten := new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(abs(int(d.H>>49&0x3FFF)-6176))), nil)
	if int(d.H>>49&0x3FFF)-6176 < 0 {
		mag.Quo(mag, new(big.Rat).SetInt(ten))
	} else {
		mag.Mul(mag, new(big.Rat).SetInt(ten))
	}
	_, exact := mag.Float64()
	return !exact && mag.Cmp(new(big.Rat).SetFloat64(math.SmallestNonzeroFloat64)) >= 0 &&
		mag.Cmp(new(big.Rat).SetFloat64(0x1p-255)) < 0
}

func abs(n int) int {
	if n < 0 {
		return -n
	}
	return n
}

type storedID struct {
	rawID    string
	recordID []byte
}

func rawKey(v mongobson.RawValue) string {
	return string(append([]byte{byte(v.Type)}, v.Value...))
}

func idDocument(v any) mongobson.Raw {
	return mongobson.Raw(must.NotFail(bson.FromDocumentRaw(must.NotFail(types.NewDocument("_id", v)))))
}

func recordIDs(t *testing.T, ctx context.Context, coll *mongo.Collection, filter mongobson.D) []storedID {
	t.Helper()
	cur, err := coll.Find(ctx, filter, options.Find().SetShowRecordID(true))
	must.NoError(err)
	var out []storedID
	for cur.Next(ctx) {
		_, rid, ok := cur.Current.Lookup("$recordId").BinaryOK()
		if !ok {
			t.Fatalf("$recordId is not BinData in %s", cur.Current)
		}
		out = append(out, storedID{rawID: rawKey(cur.Current.Lookup("_id")), recordID: rid})
	}
	must.NoError(cur.Err())
	return out
}

func startClusteredCollection(t *testing.T, ctx context.Context, mongod string) *mongo.Collection {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	must.NoError(err)
	addr := l.Addr().String()
	must.NoError(l.Close())
	_, port, _ := net.SplitHostPort(addr)
	cmd := exec.Command(mongod, "--port", port, "--dbpath", t.TempDir(), "--bind_ip", "127.0.0.1", "--nounixsocket")
	cmd.Stdout, cmd.Stderr = io.Discard, io.Discard
	must.NoError(cmd.Start())
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})
	client, err := mongo.Connect(ctx, options.Client().ApplyURI("mongodb://"+addr+"/?directConnection=true"))
	must.NoError(err)
	t.Cleanup(func() { _ = client.Disconnect(context.Background()) })
	for ctx.Err() == nil && client.Ping(ctx, nil) != nil {
		time.Sleep(100 * time.Millisecond)
	}
	db := client.Database("keystring")
	clustered := mongobson.D{{Key: "key", Value: mongobson.D{{Key: "_id", Value: 1}}}, {Key: "unique", Value: true}}
	must.NoError(db.CreateCollection(ctx, "ids", options.CreateCollection().SetClusteredIndex(clustered)))
	return db.Collection("ids")
}

func describe(v any) string {
	return fmt.Sprintf("%T %s", v, types.FormatAnyValue(v))
}

func doc(pairs ...any) *types.Document { return must.NotFail(types.NewDocument(pairs...)) }

func arr(values ...any) *types.Array { return must.NotFail(types.NewArray(values...)) }

func dec(coefficient uint64, exponent int, negative bool) types.Decimal128 {
	h := uint64(exponent+6176) << 49
	if negative {
		h |= 1 << 63
	}
	return types.Decimal128{H: h, L: coefficient}
}

// keyStringCorpus is the hand-picked edge cases: type boundaries, numeric
// buckets and fractions (including #206's), strings with zero bytes, BinData
// lengths and subtypes, and nested documents and arrays.
func keyStringCorpus() []any {
	values := []any{
		types.MinKey, types.MaxKey, types.Null, false, true, "", "a", "a\x00", "a\x00b", "\x00", "\xc3\xa9", "\U0001F600",
		strings.Repeat("x", 2000),
		types.ObjectID{}, types.ObjectID{0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff},
		time.UnixMilli(0).UTC(), time.UnixMilli(-1).UTC(), time.UnixMilli(math.MaxInt64).UTC(), time.UnixMilli(math.MinInt64).UTC(),
		types.Timestamp(0), types.Timestamp(1), types.Timestamp(math.MaxUint64),
		math.NaN(), math.Inf(1), math.Inf(-1), 0.0, math.Copysign(0, -1),
		math.MaxFloat64, -math.MaxFloat64, math.SmallestNonzeroFloat64, -math.SmallestNonzeroFloat64, 2.2250738585072014e-308,
		0.5, 0.25, 0.1, 1.5, 2.5, 1e-20, 255.5, 256.5, 1e15 + 0.5, 4503599627370495.5,
		0x1p-255, math.Nextafter(0x1p-255, 0), 0x1.8p-256, 0x1p-256, -0x1.8p-256, dec(1, -77, false), dec(12345, -81, true),
		// Inexact decimals below 2^-255 beside the doubles nearest them.
		dec(2, -300, false), 2e-300, math.Nextafter(2e-300, 0), dec(2, -300, true), -2e-300,
		dec(1, -100, false), 1e-100, 9.999999999999999e-101, dec(1, -78, false), 1e-78,
		dec(3, -90, false), 3e-90, dec(10001552324586932, -323, false), 1.000155232458693e-307,
		-0.1, -0.10000000000000002, -1e-20, -3e-20, -0.49, -0.5, -0.75, -1.5, -2.5, -255.5, -1e15 - 0.5,
		float64(1 << 53), float64(1<<53) * 2, 9.223372036854776e18, 1.8446744073709552e19, 1e20, 1e300, -1e20, -9.3e18,
		dec(0, 0, false), dec(0, 0, true), dec(3, 0, false), dec(35, -1, false), dec(1, -1, true), dec(1, -32, false),
		dec(1, 6111, false), dec(9999999999999999999, -10, false), dec(123456789, -400, true),
		dec(1, -310, false), dec(563914762203023, -334, false), dec(16884542078229845, -336, true), dec(5, -300, false),
		dec(12345678901234567, -100, false), dec(1, -78, true),
		types.Decimal128{H: 0x7c00000000000000}, types.Decimal128{H: 0x7800000000000000}, types.Decimal128{H: 0xf800000000000000},
		types.Binary{B: []byte{}}, types.Binary{B: []byte{0xff}}, types.Binary{B: []byte{0, 0}},
		types.Binary{Subtype: 0x80, B: []byte{0}}, types.Binary{Subtype: 0x04, B: make([]byte, 16)},
		types.Binary{B: make([]byte, 255)}, types.Binary{B: make([]byte, 256)}, types.Binary{B: make([]byte, 70000)},
		doc(), doc("a", int32(1)), doc("a", int32(1), "b", int32(2)), doc("", int32(1)), doc("a", doc("b", doc())),
		doc("a", arr()), doc("a", arr(int32(1), "x", arr(doc()))), doc("a", types.Regex{Pattern: "^x", Options: "i"}),
		doc("a", types.Null), doc("a", types.MinKey, "b", types.MaxKey),
		doc("a", types.Binary{B: []byte{1}}, "b", "x\x00y"),
	}
	for _, n := range []int64{0, 1, -1, 127, 128, 255, 256, -255, -256, 1 << 31, -(1 << 31), 1 << 32, 1 << 53, 1<<53 + 1, math.MaxInt64, math.MinInt64} {
		values = append(values, n)
		if n >= math.MinInt32 && n <= math.MaxInt32 {
			values = append(values, int32(n))
		}
	}
	return values
}

func randomIDValue(r *rand.Rand, depth int) any {
	kinds := 14
	if depth > 0 {
		kinds = 16 // arrays and regexes only below the top level
	}
	switch r.IntN(kinds) {
	case 0:
		return r.Int32() >> r.IntN(32)
	case 1:
		return r.Int64() >> r.IntN(64)
	case 2:
		return math.Float64frombits(r.Uint64())
	case 3:
		return float64(r.Int64()>>r.IntN(64)) / float64(int64(1)<<r.IntN(60))
	case 4:
		if r.IntN(2) == 0 {
			return dec(r.Uint64()>>r.IntN(64), r.IntN(80)-40, r.IntN(2) == 0)
		}
		// Up to 34 digits, anywhere in the exponent range.
		hi := r.Uint64N(0x0001ED09BEAD87C0)
		h := uint64(r.IntN(6111+6176))<<49 | hi
		if r.IntN(2) == 0 {
			h |= 1 << 63
		}
		return types.Decimal128{H: h, L: r.Uint64()}
	case 5:
		b := make([]rune, r.IntN(6))
		for i := range b {
			b[i] = []rune{0, 'a', 'b', 'z', 0xe9, 0x1f600, 0xffff}[r.IntN(7)]
		}
		return string(b)
	case 6:
		var oid types.ObjectID
		for i := range oid {
			oid[i] = byte(r.UintN(256))
		}
		return oid
	case 7:
		return r.IntN(2) == 0
	case 8:
		return time.UnixMilli(r.Int64() >> r.IntN(64)).UTC()
	case 9:
		return types.Timestamp(r.Uint64())
	case 10:
		b := make([]byte, r.IntN(300))
		for i := range b {
			b[i] = byte(r.UintN(4))
		}
		return types.Binary{Subtype: types.BinarySubtype([]byte{0, 1, 4, 5, 6, 0x80, 0xff}[r.IntN(7)]), B: b}
	case 11:
		return []any{types.Null, types.MinKey, types.MaxKey}[r.IntN(3)]
	case 12, 13:
		if depth >= 3 {
			return int32(depth)
		}
		d := doc()
		for i := range r.IntN(4) {
			d.Set(fmt.Sprintf("%c", 'a'+rune(r.IntN(3)+i)), randomIDValue(r, depth+1))
		}
		return d
	case 14:
		if depth >= 3 {
			return int32(depth)
		}
		a := arr()
		for range r.IntN(4) {
			a.Append(randomIDValue(r, depth+1))
		}
		return a
	default:
		return types.Regex{Pattern: []string{"", "a", "^x"}[r.IntN(3)], Options: []string{"", "i", "ms"}[r.IntN(3)]}
	}
}
