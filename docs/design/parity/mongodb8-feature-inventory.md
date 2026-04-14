# MongoDB 8 Feature Inventory for Parity Test Suite

*Scraped from official MongoDB 8.0 documentation — 2026-03-26*

## Scope Exclusions (Pre-decided)
Auth/AuthZ (SCRAM, x.509, LDAP, RBAC), Atlas features, Sharding, Replica set internals, Change streams, GridFS.

## 1. CRUD Commands
find, insert, update, delete, findAndModify, aggregate, getMore, killCursors, bulkWrite (NEW in 8.0), count, distinct, explain

## 2. Query Operators
### Comparison: $eq $ne $gt $gte $lt $lte $in $nin
### Logical: $and $or $not $nor
### Element: $exists $type
### Evaluation: $expr $jsonSchema $mod $regex $text $where (DEPRECATED 8.0)
### Array: $all $elemMatch $size
### Geospatial: $geoWithin $geoIntersects $near $nearSphere $box $center $centerSphere $geometry $maxDistance $minDistance $polygon
### Bitwise: $bitsAllSet $bitsAllClear $bitsAnySet $bitsAnyClear

## 3. Update Operators
### Field: $set $unset $inc $mul $rename $min $max $currentDate $setOnInsert
### Array: $ $[] $[<id>] $addToSet $pop $pull $pullAll $push $each $slice $sort $position
### Bitwise: $bit

## 4. Aggregation Pipeline Stages
$addFields/$set, $bucket, $bucketAuto, $collStats, $count, $densify*, $documents, $facet,
$fill*, $geoNear, $graphLookup, $group, $indexStats, $limit, $lookup, $match, $merge,
$out, $planCacheStats, $project, $redact, $replaceRoot/$replaceWith, $sample,
$setWindowFields, $skip, $sort, $sortByCount, $unionWith, $unset, $unwind
(*behavior changed in 8.0)

## 5. Aggregation Expressions
### Arithmetic (16): $abs $add $ceil $divide $exp $floor $ln $log $log10 $mod $multiply $pow $round $sqrt $subtract $trunc
### String (24): $concat $dateFromString $dateToString $indexOfBytes $indexOfCP $ltrim $regexFind $regexFindAll $regexMatch $replaceAll $replaceOne $rtrim $split $strLenBytes $strLenCP $strcasecmp $substrBytes $substrCP $toLower $toUpper $trim
### Date (22): $dateAdd $dateDiff $dateFromParts $dateToParts $dateSubtract $dateTrunc $dayOfMonth $dayOfWeek $dayOfYear $hour $isoDayOfWeek $isoWeek $isoWeekYear $millisecond $minute $month $second $toDate $week $year
### Array (23): $arrayElemAt $arrayToObject $concatArrays $filter $first $firstN $in $indexOfArray $isArray $last $lastN $map $maxN $minN $objectToArray $range $reduce $reverseArray $size $slice $sortArray $zip
### Comparison (7): $cmp $eq $ne $gt $gte $lt $lte
### Logical (3): $and $or $not
### Conditional (3): $cond $ifNull $switch
### Type (14): $convert* $isNumber $toBool $toDate $toDecimal $toDouble $toInt $toLong $toObjectId $toString $toUUID $type (*binData support new in 8.0)
### Object (5): $getField $mergeObjects $objectToArray $setField $unsetField
### Set (7): $allElementsTrue $anyElementTrue $setDifference $setEquals $setIntersection $setIsSubset $setUnion
### Trig (15): $sin $cos $tan $asin $acos $atan $atan2 $sinh $cosh $tanh $asinh $acosh $atanh $degreesToRadians $radiansToDegrees
### Bitwise (4): $bitAnd $bitNot $bitOr $bitXor (added 6.3)
### Misc: $literal $rand $sampleRate $function (DEPRECATED 8.0) $accumulator (DEPRECATED 8.0)

### Accumulators (for $group/$bucket/$setWindowFields):
$addToSet $avg $bottom $bottomN $count $first $firstN $last $lastN $max $maxN $median* $mergeObjects $min $minN $percentile* $push $stdDevPop $stdDevSamp $sum $top $topN
(*added 7.0)

### Window Functions (for $setWindowFields):
$covariancePop $covarianceSamp $denseRank* $derivative $documentNumber $expMovingAvg $integral $lag $lead $linearFill* $locf $rank* $shift
(*behavior changed in 8.0)

## 6. Index Types and Commands
### Types: single-field, compound, multikey, text, 2dsphere, 2d, hashed, wildcard, clustered, partial, sparse, unique, TTL, hidden
### Commands: createIndexes, dropIndexes, listIndexes, reIndex

## 7. Collection/Database Commands
create, collMod, drop, renameCollection, listCollections, validate, compact, autoCompact (NEW 8.0),
convertToCapped, collStats, count, distinct, dropDatabase, listDatabases, dbStats,
serverStatus, currentOp, killOp, explain, ping, buildInfo, hostInfo, getLog,
dataSize, planCacheStats, profile, setParameter, getParameter

## 8. Cursor Options
sort, limit, skip, hint, batchSize, allowDiskUse, noCursorTimeout, maxTimeMS (defaultMaxTimeMS NEW 8.0),
comment, tailable, awaitData, maxAwaitTimeMS, returnKey, showRecordId, min, max, collation

## 9. BSON Types (19 total)
Double, String, Object, Array, BinData, ObjectId, Boolean, Date, Null, Regex,
JavaScript, JavaScriptWithScope (deprecated), Int32, Timestamp, Int64, Decimal128,
MinKey, MaxKey, Symbol (deprecated), DBPointer (deprecated), Undefined (deprecated)

## 10. Special Collection Types
Capped collections, Time series collections, Views (standard + on-demand materialized)

## 11. Transactions
startTransaction, commitTransaction, abortTransaction, withTransaction,
readConcern (local/majority/snapshot), causal consistency

## 12. Geospatial
GeoJSON types (Point, LineString, Polygon, Multi*, GeometryCollection),
Legacy coordinates, $geoNear stage, all query operators

## 13. Text Search
Text index, $text operator, $search/$language/$caseSensitive/$diacriticSensitive,
$meta: "textScore", phrase/negation search, stemming, stop words

---
## MongoDB 8.0-Specific Changes to Prioritize
1. `bulkWrite` as top-level command (new)
2. `autoCompact` command (new)
3. Server-side JS deprecated: $where, $function, $accumulator (still works, warns)
4. $convert gained binData type conversion
5. $densify: equal bounds now produce empty set
6. $fill: linear interpolation across identical-value partitions
7. $denseRank/$rank: null and missing sortBy now treated identically
8. $near/$nearSphere/$geoNear: strict Point validation (non-Point = error)
9. readConcern snapshot on capped collections (newly supported)
10. defaultMaxTimeMS cluster parameter (new server-wide default)
