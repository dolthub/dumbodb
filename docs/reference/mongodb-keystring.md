# MongoDB KeyString

How mongod 8.0.28 encodes values as KeyString (V1) bytes, as observed from
clustered collections. Recorded 2026-11-06 by inserting values as `_id` into
a collection created with `clusteredIndex: {key: {_id: 1}, unique: true}` and
reading `find().showRecordId()`. No MongoDB source was read.

A clustered collection's RecordId is `KeyString(_id)` without TypeBits, and
`$recordId` returns it as BinData subtype 0. `TestKeyStringMatchesMongod`
(`internal/index`) repeats the probe against `MONGOD_BIN` for a corpus of edge
cases and random values.

## Type bytes

Every encoding starts with a type byte. Types sort by it in MongoDB's BSON
comparison order.

| Byte | Type |
|---|---|
| `0x0A` | MinKey |
| `0x0F` | Undefined |
| `0x14` | Null |
| `0x1E` | NaN; also every number's type class inside an object |
| `0x1F` | Negative number of large magnitude (beyond 2^63), and -Infinity |
| `0x20`-`0x27` | Negative number whose integer part takes 8 down to 1 bytes |
| `0x28` | Negative number of magnitude below 1 |
| `0x29` | Zero (and -0.0) |
| `0x2A` | Positive number of magnitude below 1 |
| `0x2B`-`0x32` | Positive number whose integer part takes 1 to 8 bytes |
| `0x33` | Positive number of large magnitude, and +Infinity |
| `0x3C` | String |
| `0x46` | Object |
| `0x50` | Array |
| `0x5A` | BinData |
| `0x64` | ObjectId |
| `0x6E` | false; also both booleans' type class inside an object |
| `0x6F` | true |
| `0x78` | Date |
| `0x82` | Timestamp |
| `0x8C` | Regex |
| `0xF0` | MaxKey |

## Numbers

Numbers of every type that compare equal encode identically:
`int32(1)`, `int64(1)`, `1.0` and `NumberDecimal("1")` are all `2B 02`.

### Integer parts

A number with magnitude in [1, 2^63) stores its integer part shifted left one
bit, with the low bit set when a fraction follows. It uses the fewest
big-endian bytes, and the type byte is `0x2A` plus the byte count:

| Value | Bytes |
|---|---|
| `1` | `2B 02` |
| `127` | `2B FE` |
| `128` | `2C 01 00` |
| `2^53` | `31 40 00 00 00 00 00 00` |
| `2^63 - 1` | `32 FF FF FF FF FF FF FF FE` |

A fraction fills the rest of 8 bytes after the type byte, as the binary
fraction left-aligned: `2.5` is `2B 05 80 00 00 00 00 00 00`, and `255.5` is
`2C 01 FF 80 00 00 00 00 00`. A double always leaves at least the two low bits
of the fraction clear.

Negative numbers use type byte `0x28` minus the byte count, with every byte
after it complemented: `-1` is `27 FD`, and `-1.5` is `27 FC 7F FF FF FF FF FF FF`.

### Small magnitudes

Below 1, the type byte (`0x2A`, or `0x28` with complemented bytes) is followed
by 8 bytes:
- from 2^-255 up: the IEEE 754 bits shifted left two, so `0.5` is
  `2A FF 80 00 00 00 00 00 00`;
- below 2^-255: the bits of the value times 2^768, shifted left one, so the
  smallest subnormal is `2A 59 A0 00 00 00 00 00 00`.

The two forms meet at 2^-255 (`2A C0 00 00 00 00 00 00 00`).

### Large magnitudes

From 2^63, the type byte `0x33` (or `0x1F`, complemented) is followed by the
IEEE bits shifted left one, less 2^63: `2^63` is `33 07 C0 00 00 00 00 00 00`.
+Infinity is `33` and eight `FF` bytes, and -Infinity is `1F` and eight `00`
bytes. NaN is the single byte `1E`.

`int64` -2^63 encodes as the double -2^63: `1F F8 3F FF FF FF FF FF FF`.

### Decimal128

- **Equal to a double, or an integer below 2^63:** a decimal encodes as that
  number.
- **Any other decimal in the double range:** it starts from the largest double
  not above it, encoded as above. The low two bits of that encoding become flags:

  | Flag | Meaning |
  |---|---|
  | `10` | The decimal equals the double rounded up to 15 significant digits. Nothing follows. |
  | `01` | Below that value. An 8-byte continuation follows. |
  | `11` | Above that value. An 8-byte continuation follows. |

  The continuation is the decimal at the scale of the double's decade less 33,
  minus the double at that scale rounded up.
- **Integer part of 2^53 or more:** the decimal's own integer part is encoded,
  flagged `11`, and the continuation is the fraction.
- **Large magnitudes:** these have one flag bit and always carry the
  continuation.
- **Beyond the double range:** the decimal is 16 bytes, made of a range bit (1
  above), a 14-bit biased exponent and the 113-bit coefficient scaled up to 34
  digits.
- **Below 2^-255:** see [Known deviation](#known-deviation-small-decimals).

### Known deviation: small decimals

DumboDB's encoding can differ from mongod's for one class of values: a
Decimal128 that is not exactly a double and whose magnitude is below 2^-255
(about 1.7e-77). Every other value, Decimal128 or not, encodes byte for byte as
mongod encodes it.

Below 2^-255 such a decimal is the 2^768-scaled double nearest it toward zero,
shifted left one bit with the low bit set, followed by an 8-byte continuation.
A double's word there always ends in a 0 bit, so a decimal never shares a
double's bytes and sorts strictly between its truncated double and the next
one. mongod derives the continuation from Decimal128 arithmetic on the scaled
value, which rounds; DumboDB uses the exact value. The leading 9 bytes always
match, so order and uniqueness against every other value are the same, but the
continuation often differs: in the PR 209 review's sweep of about 21,400 such
decimals, 2,290 differed in the continuation, including ordinary values such
as `2.225073858507201E-308`. `TestKeyStringMatchesMongod`'s smaller corpus
checks order for every band value and exact bytes elsewhere.

| Decimal | mongod | DumboDB |
|---|---|---|
| `1E-100` | `2A B6 57 FE 5D C9 1C 0A 5F 0E D6 09 0E C7 B5 10 3F` | the same |
| `1E-310` | `2A 5F 24 D1 16 E1 CC 56 1F 16 39 B8 19 3F D8 80 16` | `... 3F D8 80 0C` |

Within DumboDB these encodings sort in numeric order and stay distinct, so
queries, sorts, indexes and `_id` uniqueness behave as in mongod. The
difference is visible only as raw bytes: the RecordId that
`find().showRecordId()` or `$recordId` returns for such an `_id`.
`TestKeyStringMatchesMongod` checks the leading 9 bytes of these values and
their order.

## Other types

| Type | Bytes |
|---|---|
| String | `3C`, the UTF-8 bytes with each `00` escaped as `00 FF`, then `00` |
| ObjectId | `64` and the 12 bytes |
| Date | `78` and the signed milliseconds XOR 2^63, big-endian |
| Timestamp | `82` and `(t << 32 \| i)`, big-endian |
| BinData | `5A`, the length (one byte below 255, else `FF` and 4 bytes big-endian), the subtype, the bytes |
| Regex | `8C`, the pattern, `00`, the options, `00` |
| Object | `46`; for each field in stored order: its value's type class, the name, `00`, the value's encoding; then `00` |
| Array | `50`, each element's encoding, then `00` |

BinData orders by length, then subtype, then bytes. Object fields keep their
stored order: `{ab: 1, a: 2}` is `46 1E 61 62 00 2B 02 1E 61 00 2B 04 00`.

Every encoding delimits itself, so values concatenate without separators.
Secondary index keys append `04` and the RecordId.

## Clustered collection behavior

- **Natural order:** `_id` order, because the collection is stored by its
  RecordIds. `sort({$natural: -1})` is reverse `_id` order.
- **Equal `_id`s of different numeric types:** inserting
  `-9.223372036854776e18` after `int64(-2^63)`, or a Decimal128 NaN after a
  double NaN, fails with "KeyString format error" rather than a duplicate key
  error.
- **Explain:** an `_id` equality plans as `EXPRESS_CLUSTERED_IXSCAN` (0 keys,
  1 document). An `_id` range plans as `CLUSTERED_IXSCAN`, examining one
  document past the range. `sort({_id: 1})` with a limit is a `LIMIT` over a
  `COLLSCAN`. Unfiltered scans count every document read in
  `totalDocsExamined`.
- **listIndexes:** reports `_id_` with `clustered: true` and `unique: true`.
- **listCollections:** reports `options.clusteredIndex`
  `{key: {_id: 1}, name: "_id_", unique: true, v: 2}`.
