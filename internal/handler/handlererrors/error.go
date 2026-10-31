// Copyright 2021 FerretDB Inc.
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

// Package handlererrors provides handlers errors.
package handlererrors

import (
	"errors"
	"fmt"

	"github.com/FerretDB/wire/wirebson"

	"github.com/dolthub/dumbodb/internal/bson"
)

//go:generate ../../../bin/stringer -linecomment -type ErrorCode

// ErrorCode represents wire protocol error code.
type ErrorCode int32

const (
	errUnset = ErrorCode(0) // Unset

	// For ProtocolError only.
	errInternalError = ErrorCode(1) // InternalError

	// ErrBadValue indicates wrong input.
	ErrBadValue = ErrorCode(2) // BadValue

	// ErrGraphContainsCycle indicates a view definition forms a cycle.
	ErrGraphContainsCycle = ErrorCode(5) // GraphContainsCycle

	// ErrFailedToParse indicates user input parsing failure.
	ErrFailedToParse = ErrorCode(9) // FailedToParse

	// ErrUserNotFound indicates an user was not found for the accessed database.
	ErrUserNotFound = ErrorCode(11) // UserNotFound

	// ErrUnauthorized indicates that cursor is not authorized to access another namespace.
	ErrUnauthorized = ErrorCode(13) // Unauthorized

	// ErrTypeMismatch for $sort indicates that the expression in the $sort is not an object.
	ErrTypeMismatch = ErrorCode(14) // TypeMismatch

	// ErrOverflow indicates input nested deeper than the BSON depth limit.
	ErrOverflow = ErrorCode(15) // Overflow

	// ErrProtocolError indicates SASL handshake failed.
	ErrProtocolError = ErrorCode(17) // ProtocolError

	ErrAuthenticationFailed = ErrorCode(18) // AuthenticationFailed

	ErrIllegalOperation = ErrorCode(20) // IllegalOperation

	// ErrDatabaseDifferCase indicates a database name differing only in case from an existing one.
	ErrDatabaseDifferCase = ErrorCode(13297) // DatabaseDifferCase

	// ErrInvalidBSON indicates a request document that is not valid BSON.
	ErrInvalidBSON = ErrorCode(22) // InvalidBSON

	// ErrNamespaceNotFound indicates that a collection is not found.
	ErrNamespaceNotFound = ErrorCode(26) // NamespaceNotFound

	// ErrIndexNotFound indicates that a index is not found for the given name.
	ErrIndexNotFound = ErrorCode(27) // IndexNotFound

	// ErrUnsuitableValueType indicates that field could not be created for given value.
	ErrUnsuitableValueType = ErrorCode(28) // PathNotViable

	ErrRoleNotFound = ErrorCode(31) // RoleNotFound

	// ErrConflictingUpdateOperators indicates that $set, $inc or $setOnInsert were used together.
	ErrConflictingUpdateOperators = ErrorCode(40) // ConflictingUpdateOperators

	ErrCursorNotFound = ErrorCode(43) // CursorNotFound

	// ErrNamespaceExists indicates that the collection already exists.
	ErrNamespaceExists = ErrorCode(48) // NamespaceExists

	ErrInvalidRoleModification = ErrorCode(49) // InvalidRoleModification

	ErrMaxTimeMSExpired = ErrorCode(50) // MaxTimeMSExpired

	ErrDollarPrefixedFieldName = ErrorCode(52) // DollarPrefixedFieldName

	// ErrInvalidID indicates that _id field is invalid.
	ErrInvalidID = ErrorCode(53) // InvalidIdField

	ErrEmptyName = ErrorCode(56) // EmptyFieldName

	ErrCommandNotFound = ErrorCode(59) // CommandNotFound

	// ErrImmutableField indicates that _id field is immutable.
	ErrImmutableField = ErrorCode(66) // ImmutableField

	// ErrCannotCreateIndex indicates that index creation process failed because some data are not valid.
	ErrCannotCreateIndex = ErrorCode(67) // CannotCreateIndex

	// ErrIndexAlreadyExists indicates that identical index already exists.
	ErrIndexAlreadyExists = ErrorCode(68) // IndexAlreadyExists

	ErrInvalidOptions = ErrorCode(72) // InvalidOptions

	// ErrInvalidNamespace indicates that the collection name is invalid.
	ErrInvalidNamespace = ErrorCode(73) // InvalidNamespace

	ErrNoReplicationEnabled = ErrorCode(76) // NoReplicationEnabled

	// ErrIndexOptionsConflict indicates that index build process failed due to options conflict.
	ErrIndexOptionsConflict = ErrorCode(85) // IndexOptionsConflict

	// ErrIndexKeySpecsConflict indicates that index build process failed due to key specs conflict.
	ErrIndexKeySpecsConflict = ErrorCode(86) // IndexKeySpecsConflict

	ErrOperationFailed = ErrorCode(96) // OperationFailed

	ErrWriteConflict = ErrorCode(112) // WriteConflict

	ErrCommandNotSupported = ErrorCode(115) // CommandNotSupported

	// ErrConflictingOperationInProgress refuses an operation that something
	// else on its target -- uncommitted changes on a branch -- keeps from
	// starting until it is dealt with.
	ErrConflictingOperationInProgress = ErrorCode(117) // ConflictingOperationInProgress

	// ErrConflictResolutionRequired is dumbodb's own: a merge, cherry-pick,
	// revert or rebase is paused on conflicts on the branch, and nothing else
	// may change it until they are resolved and the operation is continued or
	// aborted. Its number lies outside MongoDB's codes.
	ErrConflictResolutionRequired = ErrorCode(2000000001) // ConflictResolutionRequired

	ErrDocumentValidationFailure = ErrorCode(121) // DocumentValidationFailure

	ErrInvalidIndexSpecificationOption = ErrorCode(197) // InvalidIndexSpecificationOption

	// ErrTimeProofMismatch indicates a $clusterTime signature does not match its key.
	ErrTimeProofMismatch = ErrorCode(204) // TimeProofMismatch

	// ErrKeysNotFound indicates no signing key matches a $clusterTime signature.
	ErrKeysNotFound = ErrorCode(211) // KeysNotFound

	ErrExceededMemoryLimit = ErrorCode(146) // ExceededMemoryLimit

	ErrDurationOverflow = ErrorCode(159) // DurationOverflow

	// ErrQueryExceededMemoryLimitNoDiskUseAllowed indicates a blocking
	// aggregation stage passed its memory limit and could not spill to disk.
	ErrQueryExceededMemoryLimitNoDiskUseAllowed = ErrorCode(292) // QueryExceededMemoryLimitNoDiskUseAllowed

	// ErrViewDepthLimitExceeded indicates a view resolves through more than the
	// maximum nesting depth (20).
	ErrViewDepthLimitExceeded = ErrorCode(165) // ViewDepthLimitExceeded

	// ErrCommandNotSupportedOnView indicates that the command does not support views.
	ErrCommandNotSupportedOnView = ErrorCode(166) // CommandNotSupportedOnView

	// ErrOptionNotSupportedOnView indicates that an option, such as a
	// collation that differs from the view's, is not supported on a view.
	ErrOptionNotSupportedOnView = ErrorCode(167) // OptionNotSupportedOnView

	// ErrInvalidPipelineOperator indicates that provided aggregation operator is invalid.
	ErrInvalidPipelineOperator = ErrorCode(168) // InvalidPipelineOperator

	ErrClientMetadataCannotBeMutated = ErrorCode(186) // ClientMetadataCannotBeMutated

	// ErrNotImplemented indicates that a flag or command is not implemented.
	ErrNotImplemented = ErrorCode(238) // NotImplemented

	ErrTransactionTooOld = ErrorCode(225) // TransactionTooOld

	ErrNoSuchTransaction = ErrorCode(251) // NoSuchTransaction

	ErrOperationNotSupportedInTransaction = ErrorCode(263) // OperationNotSupportedInTransaction

	// ErrMechanismUnavailable indicates that the authentication mechanism is unavailable.
	ErrMechanismUnavailable = ErrorCode(334) // MechanismUnavailable

	// ErrUnsupportedOpQueryCommand indicates that given op query is not supported.
	ErrUnsupportedOpQueryCommand = ErrorCode(352) // UnsupportedOpQueryCommand

	// ErrExpectedObject indicates a parameter that must be an object, such as
	// createIndexes' indexes or $rand's argument.
	ErrExpectedObject = ErrorCode(10065) // Location10065

	ErrNotWritablePrimary = ErrorCode(10107) // NotWritablePrimary

	// ErrDuplicateKeyInsert indicates duplicate key violation on inserting document.
	ErrDuplicateKeyInsert = ErrorCode(11000) // DuplicateKey

	// ErrSetBadExpression indicates set expression is not object.
	ErrSetBadExpression = ErrorCode(40272) // Location40272

	// ErrStageGroupInvalidFields indicates group's fields must be an object.
	ErrStageGroupInvalidFields = ErrorCode(15947) // Location15947

	// ErrStageGroupID indicates _id for a group can only be specified once.
	ErrStageGroupID = ErrorCode(15948) // Location15948

	// ErrStageGroupMissingID indicates that group is missing an _id.
	ErrStageGroupMissingID = ErrorCode(15955) // Location15955

	// ErrStageLimitZero indicated that limit field is not positive.
	ErrStageLimitZero = ErrorCode(15958) // Location15958

	// ErrMatchBadExpression indicates match filter is not object.
	ErrMatchBadExpression = ErrorCode(15959) // Location15959

	// ErrProjectBadExpression indicates projection value wrong type.
	ErrProjectBadExpression = ErrorCode(15969) // Location15969

	// ErrSortBadExpression indicates sort expression is not object.
	ErrSortBadExpression = ErrorCode(15973) // Location15973

	// ErrSortBadValue indicates bad value in sort input.
	ErrSortBadValue = ErrorCode(15974) // Location15974

	// ErrSortBadOrder indicates bad sort order input.
	ErrSortBadOrder = ErrorCode(15975) // Location15975

	// ErrSortMissingKey indicates sort stage is missing sort key.
	ErrSortMissingKey = ErrorCode(15976) // Location15976

	// ErrGroupDuplicateFieldName indicates that duplicate field name is specified.
	ErrGroupDuplicateFieldName = ErrorCode(16406) // Location16406

	// ErrStageUnwindWrongType indicates $unwind stage argument has unexpected type.
	ErrStageUnwindWrongType = ErrorCode(15981) // Location15981

	// ErrExpressionWrongLenOfFields indicates that aggregation expression has too much fields.
	ErrExpressionWrongLenOfFields = ErrorCode(15983) // Location15983

	// ErrPathContainsEmptyElement indicates that the path contains an empty element.
	ErrPathContainsEmptyElement = ErrorCode(15998) // Location15998

	// ErrOperatorWrongLenOfArgs indicates that aggregation operator contains
	// wrong amount of arguments.
	ErrOperatorWrongLenOfArgs = ErrorCode(16020) // Location16020

	// ErrBSONObjectTooLarge indicates a reply would exceed the 16MB BSON limit.
	ErrBSONObjectTooLarge = ErrorCode(10334) // BSONObjectTooLarge

	ErrRangeStartNotNumeric = ErrorCode(34443) // Location34443
	ErrRangeStartNotInt32   = ErrorCode(34444) // Location34444
	ErrRangeEndNotNumeric   = ErrorCode(34445) // Location34445
	ErrRangeEndNotInt32     = ErrorCode(34446) // Location34446
	ErrRangeStepNotNumeric  = ErrorCode(34447) // Location34447
	ErrRangeStepNotInt32    = ErrorCode(34448) // Location34448
	ErrRangeStepZero        = ErrorCode(34449) // Location34449

	// ErrFieldPathInvalidName indicates that FieldPath is invalid.
	ErrFieldPathInvalidName = ErrorCode(16410) // Location16410

	// ErrGroupInvalidFieldPath indicates invalid path is given for group _id.
	ErrGroupInvalidFieldPath = ErrorCode(16872) // Location16872

	// ErrBadNumberToReturn indicates that invalid number to return was given for op query.
	ErrBadNumberToReturn = ErrorCode(16979) // Location16979

	// ErrGroupUndefinedVariable indicates the variable is not defined.
	ErrGroupUndefinedVariable = ErrorCode(17276) // Location17276

	// ErrInvalidArg indicates invalid argument in projection document.
	ErrInvalidArg = ErrorCode(28667) // Location28667

	// ErrSliceFirstArg for $slice indicates that the first argument is not an array.
	ErrSliceFirstArg = ErrorCode(28724) // Location28724

	// ErrSliceThirdArgNotPositive indicates a negative count after a position in $slice.
	ErrSliceThirdArgNotPositive = ErrorCode(28729) // Location28729

	// ErrReplaceRootNotObject indicates a $replaceRoot newRoot that is not an object.
	ErrReplaceRootNotObject = ErrorCode(40228) // Location40228

	// Math expression errors, named for MongoDB's checks.
	ErrCannotConvertToLong     = ErrorCode(16004)   // Location16004
	ErrModByZero               = ErrorCode(16610)   // Location16610
	ErrModNonNumeric           = ErrorCode(16611)   // Location16611
	ErrModByZeroDecimal        = ErrorCode(5733415) // Location5733415
	ErrSqrtNegative            = ErrorCode(28714)   // Location28714
	ErrAbsOfLongMin            = ErrorCode(28680)   // Location28680
	ErrLogNonNumericArgument   = ErrorCode(28756)   // Location28756
	ErrLogNonNumericBase       = ErrorCode(28757)   // Location28757
	ErrLogNonPositiveArgument  = ErrorCode(28758)   // Location28758
	ErrLogInvalidBase          = ErrorCode(28759)   // Location28759
	ErrLog10NonPositive        = ErrorCode(28761)   // Location28761
	ErrPowNonNumericBase       = ErrorCode(28762)   // Location28762
	ErrPowNonNumericExponent   = ErrorCode(28763)   // Location28763
	ErrPowZeroNegativeExponent = ErrorCode(28764)   // Location28764
	ErrMathNonNumeric          = ErrorCode(28765)   // Location28765
	ErrLnNonPositive           = ErrorCode(28766)   // Location28766
	ErrRoundNonNumeric         = ErrorCode(51081)   // Location51081
	ErrRoundPlaceNotIntegral   = ErrorCode(51082)   // Location51082
	ErrRoundPlaceOutOfRange    = ErrorCode(51083)   // Location51083
	ErrTrigOutOfBounds         = ErrorCode(50989)   // Location50989
	ErrNumericFirstArgument    = ErrorCode(51044)   // Location51044
	ErrNumericSecondArgument   = ErrorCode(51045)   // Location51045

	// Date expression errors, named for MongoDB's checks.
	ErrUnrecognizedTimeZone      = ErrorCode(40485)   // Location40485
	ErrTimeZoneNotString         = ErrorCode(40517)   // Location40517
	ErrDateFromPartsMixedNatural = ErrorCode(40489)   // Location40489
	ErrDateFromPartsNotInteger   = ErrorCode(40515)   // Location40515
	ErrDateFromPartsNoYear       = ErrorCode(40516)   // Location40516
	ErrDateFromPartsUnknownField = ErrorCode(40518)   // Location40518
	ErrDateFromPartsNotObject    = ErrorCode(40519)   // Location40519
	ErrDateFromPartsYearRange    = ErrorCode(40523)   // Location40523
	ErrDateFromPartsMixedISO     = ErrorCode(40525)   // Location40525
	ErrDatePartOutOfRange        = ErrorCode(31034)   // Location31034
	ErrDateFromPartsISOYearRange = ErrorCode(31095)   // Location31095
	ErrCannotConvertToDate       = ErrorCode(16006)   // Location16006
	ErrDateToPartsUnknownField   = ErrorCode(40520)   // Location40520
	ErrDateToPartsISONotBool     = ErrorCode(40521)   // Location40521
	ErrDateToPartsNoDate         = ErrorCode(40522)   // Location40522
	ErrDateToPartsNotObject      = ErrorCode(40524)   // Location40524
	ErrConversionFailure         = ErrorCode(241)     // ConversionFailure
	ErrDateFromStringNotObject   = ErrorCode(40540)   // Location40540
	ErrDateFromStringBadField    = ErrorCode(40541)   // Location40541
	ErrDateFromStringNoInput     = ErrorCode(40542)   // Location40542
	ErrDateFormatNotString       = ErrorCode(40684)   // Location40684
	ErrFormatUnmatchedPercent    = ErrorCode(18535)   // Location18535
	ErrFormatInvalidSpecifier    = ErrorCode(18536)   // Location18536
	ErrTimeUnitNotString         = ErrorCode(5439013) // Location5439013
	ErrOutInSubPipeline          = ErrorCode(31441)   // Location31441
	ErrUnwindNotArray            = ErrorCode(5858203) // Location5858203
	ErrCannotConvertToString     = ErrorCode(16007)   // Location16007
	ErrStageNotAllowedInFacet    = ErrorCode(40600)   // Location40600
	ErrArrayElemAtNotArray       = ErrorCode(28689)   // Location28689
	ErrArrayElemAtNotNumeric     = ErrorCode(28690)   // Location28690
	ErrArrayElemAtNotInt         = ErrorCode(28691)   // Location28691
	ErrSliceSecondNotNumeric     = ErrorCode(28725)   // Location28725
	ErrSliceSecondNotInt         = ErrorCode(28726)   // Location28726
	ErrSliceThirdNotNumeric      = ErrorCode(28727)   // Location28727
	ErrSliceThirdNotInt          = ErrorCode(28728)   // Location28728
	ErrIndexOfArrayNotArray      = ErrorCode(40090)   // Location40090
	ErrIndexOfBytesNotString     = ErrorCode(40091)   // Location40091
	ErrIndexOfBytesTokenString   = ErrorCode(40092)   // Location40092
	ErrIndexOfCPNotString        = ErrorCode(40093)   // Location40093
	ErrIndexOfCPTokenString      = ErrorCode(40094)   // Location40094
	ErrIndexOfCPBadUTF8          = ErrorCode(40095)   // Location40095
	ErrIndexNotIntegral          = ErrorCode(40096)   // Location40096
	ErrIndexNegative             = ErrorCode(40097)   // Location40097
	ErrSubstrStartNotNumeric     = ErrorCode(16034)   // Location16034
	ErrSubstrLengthNotNumeric    = ErrorCode(16035)   // Location16035
	ErrSubstrStartNegative       = ErrorCode(50752)   // Location50752
	ErrSubstrStartContinuation   = ErrorCode(28656)   // Location28656
	ErrSubstrEndMidCharacter     = ErrorCode(28657)   // Location28657
	ErrSubstrCPStartNotNumeric   = ErrorCode(34450)   // Location34450
	ErrSubstrCPStartNotInt       = ErrorCode(34451)   // Location34451
	ErrSubstrCPLengthNotNumeric  = ErrorCode(34452)   // Location34452
	ErrSubstrCPLengthNotInt      = ErrorCode(34453)   // Location34453
	ErrSubstrCPLengthNegative    = ErrorCode(34454)   // Location34454
	ErrSubstrCPStartNegative     = ErrorCode(34455)   // Location34455
	ErrSubstrCPInvalidUTF8       = ErrorCode(34456)   // Location34456
	ErrCoerceLongOutOfRange      = ErrorCode(31109)   // Location31109
	ErrDateDiffNotObject         = ErrorCode(5166301) // Location5166301
	ErrDateDiffBadField          = ErrorCode(5166302) // Location5166302
	ErrDateDiffNoStartDate       = ErrorCode(5166303) // Location5166303
	ErrDateDiffNoEndDate         = ErrorCode(5166304) // Location5166304
	ErrDateDiffNoUnit            = ErrorCode(5166305) // Location5166305
	ErrDateDiffNotDate           = ErrorCode(5166307) // Location5166307
	ErrDateDiffOverflow          = ErrorCode(5166308) // Location5166308
	ErrDateTruncShiftOverflow    = ErrorCode(5439000) // Location5439000
	ErrDateTruncResultOverflow   = ErrorCode(5439001) // Location5439001
	ErrDateTruncBinSizeOverflow  = ErrorCode(5439002) // Location5439002
	ErrDateTruncBinBoundOverflow = ErrorCode(5439004) // Location5439004
	ErrDateTruncBinSizeTooLarge  = ErrorCode(5439006) // Location5439006
	ErrDateTruncNotObject        = ErrorCode(5439007) // Location5439007
	ErrDateTruncBadField         = ErrorCode(5439008) // Location5439008
	ErrDateTruncNoDate           = ErrorCode(5439009) // Location5439009
	ErrDateTruncNoUnit           = ErrorCode(5439010) // Location5439010
	ErrDateTruncNotDate          = ErrorCode(5439012) // Location5439012
	ErrStartOfWeekNotString      = ErrorCode(5439015) // Location5439015
	ErrStartOfWeekInvalid        = ErrorCode(5439016) // Location5439016
	ErrDateTruncBinSizeNotInt    = ErrorCode(5439017) // Location5439017
	ErrDateTruncBinSizeNotPos    = ErrorCode(5439018) // Location5439018
	ErrDateAddAmountInvalid      = ErrorCode(5976500) // Location5976500
	ErrDateAddOverflow           = ErrorCode(5166406) // Location5166406
	ErrDateArithmeticNotObject   = ErrorCode(5166400) // Location5166400
	ErrDateArithmeticBadField    = ErrorCode(5166401) // Location5166401
	ErrDateArithmeticNoField     = ErrorCode(5166402) // Location5166402
	ErrDateArithmeticNotDate     = ErrorCode(5166403) // Location5166403
	ErrDateArithmeticNotInteger  = ErrorCode(5166405) // Location5166405
	ErrDateSubtractMinAmount     = ErrorCode(6045000) // Location6045000
	ErrMergeObjectsNotObject     = ErrorCode(5911200) // Location5911200
	ErrConcatArraysNotArray      = ErrorCode(28664)   // Location28664
	ErrGroupInclusionStyle       = ErrorCode(17390)   // Location17390
	ErrAddTwoDates               = ErrorCode(16612)   // Location16612
	ErrReplaceReplacementType    = ErrorCode(51744)   // Location51744
	ErrReplaceFindType           = ErrorCode(51745)   // Location51745
	ErrReplaceInputType          = ErrorCode(51746)   // Location51746
	ErrReplaceNoReplacement      = ErrorCode(51747)   // Location51747
	ErrReplaceNoFind             = ErrorCode(51748)   // Location51748
	ErrReplaceNoInput            = ErrorCode(51749)   // Location51749
	ErrReplaceUnknownField       = ErrorCode(51750)   // Location51750
	ErrReplaceNotObject          = ErrorCode(51751)   // Location51751
	ErrRegexNotObject            = ErrorCode(51103)   // Location51103
	ErrRegexInputType            = ErrorCode(51104)   // Location51104
	ErrRegexType                 = ErrorCode(51105)   // Location51105
	ErrRegexOptionsType          = ErrorCode(51106)   // Location51106
	ErrRegexOptionsTwice         = ErrorCode(51107)   // Location51107
	ErrRegexPatternNull          = ErrorCode(51109)   // Location51109
	ErrRegexOptionsNull          = ErrorCode(51110)   // Location51110
	ErrRegexInvalid              = ErrorCode(51111)   // Location51111
	ErrRegexExecution            = ErrorCode(51156)   // Location51156
	ErrRegexFindAllTooLarge      = ErrorCode(51151)   // Location51151
	ErrStringTooLarge            = ErrorCode(16493)   // Location16493
	ErrBufBuilderTooLarge        = ErrorCode(13548)   // Location13548
	ErrRegexNoInput              = ErrorCode(31022)   // Location31022
	ErrRegexNoRegex              = ErrorCode(31023)   // Location31023
	ErrRegexUnknownField         = ErrorCode(31024)   // Location31024
	ErrSortArrayNotObject        = ErrorCode(2942500) // Location2942500
	ErrSortArrayUnknownField     = ErrorCode(2942501) // Location2942501
	ErrSortArrayNoInput          = ErrorCode(2942502) // Location2942502
	ErrSortArrayNoSortBy         = ErrorCode(2942503) // Location2942503
	ErrSortArrayInputNotArray    = ErrorCode(2942504) // Location2942504
	ErrSortArrayBadPattern       = ErrorCode(2942505) // Location2942505
	ErrSortArrayBadOrder         = ErrorCode(2942506) // Location2942506
	ErrSortArrayBadSortBy        = ErrorCode(2942507) // Location2942507
	ErrBinarySizeType            = ErrorCode(51276)   // Location51276
	ErrConversionArity           = ErrorCode(50723)   // Location50723
	ErrRandInValidator           = ErrorCode(3040500) // Location3040500
	ErrRandArguments             = ErrorCode(3040501) // Location3040501
	ErrMetaNotString             = ErrorCode(17307)   // Location17307
	ErrMetaUnsupported           = ErrorCode(17308)   // Location17308
	ErrMetadataUnavailable       = ErrorCode(40218)   // Location40218
	ErrDatePartTimeZoneNotString = ErrorCode(40533)   // Location40533
	ErrDatePartUnknownOption     = ErrorCode(40535)   // Location40535
	ErrDatePartArrayLen          = ErrorCode(40536)   // Location40536
	ErrDatePartNoDate            = ErrorCode(40539)   // Location40539
	ErrDateToStringNotObject     = ErrorCode(18629)   // Location18629
	ErrDateToStringUnknownField  = ErrorCode(18534)   // Location18534
	ErrDateToStringNoDate        = ErrorCode(18628)   // Location18628
	ErrDateToStringFormatType    = ErrorCode(18533)   // Location18533
	ErrDateComponentOutOfRange   = ErrorCode(18537)   // Location18537

	// ErrOpMsgNoBody indicates an OP_MSG without a kind-0 body section.
	ErrOpMsgNoBody = ErrorCode(40587) // Location40587

	// ErrStageUnsetNoPath indicates that $unwind aggregation stage is empty.
	ErrStageUnsetNoPath = ErrorCode(31119) // Location31119

	// ErrStageUnsetArrElementInvalidType indicates that $unset stage arguments has array with unexpected type.
	ErrStageUnsetArrElementInvalidType = ErrorCode(31120) // Location31120

	// ErrStageUnsetInvalidType indicates that $unset stage arguments has unexpected type.
	ErrStageUnsetInvalidType = ErrorCode(31002) // Location31002

	// ErrSearchNotEnabled indicates that $search requires Atlas Search which is not enabled.
	ErrSearchNotEnabled = ErrorCode(31082) // SearchNotEnabled

	// ErrStageSampleInvalidArg indicates invalid argument for the aggregation $sample stage.
	ErrStageSampleInvalidArg = ErrorCode(28745) // Location28745

	// ErrStageSampleNegativeSize indicates that $sample size must not be negative.
	ErrStageSampleNegativeSize = ErrorCode(28747) // Location28747

	// ErrStageUnwindNoPath indicates that $unwind aggregation stage is empty.
	ErrStageUnwindNoPath = ErrorCode(28812) // Location28812

	// ErrStageUnwindNoPrefix indicates that $unwind aggregation stage doesn't include '$' prefix.
	ErrStageUnwindNoPrefix = ErrorCode(28818) // Location28818

	// ErrUnsetPathCollision indicates that an $unset path creates collision at another path in arguments.
	ErrUnsetPathCollision = ErrorCode(31249) // Location31249

	// ErrUnsetPathOverwrite indicates that an $unset path have overwrites another path in arguments.
	ErrUnsetPathOverwrite = ErrorCode(31250) // Location31250

	// ErrConvertFormatType indicates a $convert 'format' that is not a string.
	ErrConvertFormatType = ErrorCode(4341114) // Location4341114

	// ErrConvertFormatRequired indicates a BinData conversion without 'format'.
	ErrConvertFormatRequired = ErrorCode(4341115) // Location4341115

	// ErrConvertFormatInvalid indicates an unknown $convert 'format'.
	ErrConvertFormatInvalid = ErrorCode(4341125) // Location4341125

	// ErrConvertSubtypeNotInteger indicates a fractional BinData 'subtype'.
	ErrConvertSubtypeNotInteger = ErrorCode(4341106) // Location4341106

	// ErrConvertSubtypeInvalid indicates a 'subtype' that is no BinData type.
	ErrConvertSubtypeInvalid = ErrorCode(4341107) // Location4341107

	// ErrConvertSubtypeType indicates a non-numeric BinData 'subtype'.
	ErrConvertSubtypeType = ErrorCode(4341108) // Location4341108

	// ErrConvertSubtypeColumn indicates a conversion to BinData subtype Column.
	ErrConvertSubtypeColumn = ErrorCode(12910300) // Location12910300

	// ErrSizeNotArray indicates that the argument to $size is not an array.
	ErrSizeNotArray = ErrorCode(17124) // Location17124

	// ErrProjectionExpressionInExclusion indicates an expression in an
	// exclusion projection.
	ErrProjectionExpressionInExclusion = ErrorCode(31252) // Location31252

	// ErrProjectionInEx for $elemMatch indicates that inclusion statement found
	// while projection document already marked as exclusion.
	ErrProjectionInEx = ErrorCode(31253) // Location31253

	// ErrProjectionExIn for $elemMatch indicates that exclusion statement found
	// while projection document already marked as inclusion.
	ErrProjectionExIn = ErrorCode(31254) // Location31254

	// ErrAggregatePositionalProject indicates that positional projection cannot be used in aggregation.
	ErrAggregatePositionalProject = ErrorCode(31324) // Location31324

	// ErrAggregateInvalidExpression indicates that projection expression does not exist.
	ErrAggregateInvalidExpression = ErrorCode(31325) // Location31325

	// ErrBSONSizeRequiresDocument indicates that $bsonSize received a non-document argument.
	ErrBSONSizeRequiresDocument = ErrorCode(31393) // Location31393

	// ErrWrongPositionalOperatorLocation indicates that there can only be one positional
	// operator at the end.
	ErrWrongPositionalOperatorLocation = ErrorCode(31394) // Location31394

	// ErrExclusionPositionalProjection indicates that exclusion cannot use positional projection.
	ErrExclusionPositionalProjection = ErrorCode(31395) // Location31395

	// ErrStageCountNonString indicates that $count aggregation stage expected string.
	ErrStageCountNonString = ErrorCode(40156) // Location40156

	// ErrStageCountNonEmptyString indicates that $count aggregation stage expected non-empty string.
	ErrStageCountNonEmptyString = ErrorCode(40157) // Location40157

	// ErrStageCountBadPrefix indicates that $count stage cannot have invalid prefix.
	ErrStageCountBadPrefix = ErrorCode(40158) // Location40158

	// ErrStageCountBadValue indicates that $count stage contains invalid value.
	ErrStageCountBadValue = ErrorCode(40160) // Location40160

	// ErrAddFieldsExpressionWrongAmountOfArgs indicates that $addFields stage expression contain invalid
	// amount of arguments.
	ErrAddFieldsExpressionWrongAmountOfArgs = ErrorCode(40181) // Location40181

	// ErrBucketNotEnoughBoundaries indicates that $bucket 'boundaries' must have at least 2 values.
	ErrBucketNotEnoughBoundaries = ErrorCode(40192) // Location40192

	// ErrBucketMissingBoundaries indicates that $bucket stage specification is missing the required 'boundaries' field.
	ErrBucketMissingBoundaries = ErrorCode(40198) // Location40198

	// ErrStageGroupUnaryOperator indicates that $sum is a unary operator.
	ErrStageGroupUnaryOperator = ErrorCode(40237) // Location40237

	// ErrStageGroupUnknownOperator indicates a name that is not a $group accumulator.
	ErrStageGroupUnknownOperator = ErrorCode(15952) // Location15952

	// ErrStageGroupMultipleAccumulator indicates that group field must specify one accumulator.
	ErrStageGroupMultipleAccumulator = ErrorCode(40238) // Location40238

	// ErrBucketAutoMissingRequiredFields indicates that $bucketAuto is missing required 'groupBy' or 'buckets' fields.
	ErrBucketAutoMissingRequiredFields = ErrorCode(40246) // Location40246

	// ErrStageGroupInvalidAccumulator indicates invalid accumulator field.
	ErrStageGroupInvalidAccumulator = ErrorCode(40234) // Location40234

	// ErrStageInvalid indicates invalid aggregation pipeline stage.
	ErrStageInvalid = ErrorCode(40323) // Location40323

	// ErrStageUnrecognized indicates an unrecognized pipeline stage name.
	ErrStageUnrecognized = ErrorCode(40324) // Location40324

	// ErrEmptyFieldPath indicates that the field path is empty.
	ErrEmptyFieldPath = ErrorCode(40352) // Location40352

	// ErrInvalidFieldPath indicates that the field path is not valid.
	ErrInvalidFieldPath = ErrorCode(40353) // Location40353

	// ErrMissingField indicates that the required field in document is missing.
	// MongoDB names this "IDLFailedToParse".
	ErrMissingField = ErrorCode(40414) // IDLFailedToParse

	// ErrIDLUnknownField indicates that an unknown field was encountered in a command document.
	ErrIDLUnknownField = ErrorCode(40415) // IDLUnknownField

	// ErrChangeStreamNotSupported indicates that $changeStream is not supported on a standalone server.
	ErrChangeStreamNotSupported = ErrorCode(40573) // Location40573

	// ErrCollStatsIsNotFirstStage indicates that $collStats must be the first stage in the pipeline.
	ErrCollStatsIsNotFirstStage = ErrorCode(40602) // Location40602

	// ErrOpQueryInvalidField indicates that the field is not allowed for op query.
	ErrOpQueryInvalidField = ErrorCode(40621) // Location40621

	// ErrSetEmptyPassword indicates that a password must not be empty.
	ErrSetEmptyPassword = ErrorCode(50687) // Location50687

	// ErrStringProhibited indicates that a password contains prohibited runes.
	ErrStringProhibited = ErrorCode(50692) // Location50692

	// ErrFreeMonitoringDisabled indicates that free monitoring is disabled
	// by command-line or config file.
	ErrFreeMonitoringDisabled = ErrorCode(50840) // Location50840

	// ErrAggregateCollectionMissing indicates that an aggregation stage needs
	// a collection that does not exist.
	ErrAggregateCollectionMissing = ErrorCode(50933) // Location50933

	ErrRoleAlreadyExists = ErrorCode(51002) // Location51002

	// ErrUserAlreadyExists indicates that user already exists.
	ErrUserAlreadyExists = ErrorCode(51003) // Location51003

	// ErrValueNegative indicates that value must not be negative.
	ErrValueNegative = ErrorCode(51024) // Location51024

	// ErrRegexOptions indicates regex options error.
	ErrRegexOptions = ErrorCode(51075) // Location51075

	// ErrRegexMissingParen indicates missing parentheses in regex expression.
	ErrRegexMissingParen = ErrorCode(51091) // Location51091

	// ErrBadRegexOption indicates bad regex option value passed.
	ErrBadRegexOption = ErrorCode(51108) // Location51108

	// ErrBadPositionalProjection indicates that positional operator could not find a matching element in the array.
	ErrBadPositionalProjection = ErrorCode(51246) // Location51246

	// ErrElementMismatchPositionalProjection indicates that unexpected element was present at projection path.
	ErrElementMismatchPositionalProjection = ErrorCode(51247) // Location51247

	// ErrEmptySubProject indicates that subprojection mustn't be empty.
	ErrEmptySubProject = ErrorCode(51270) // Location51270

	// ErrEmptyProject indicates that projection specification must have at least one field.
	ErrEmptyProject = ErrorCode(51272) // Location51272

	// ErrDuplicateField indicates duplicate field is specified.
	ErrDuplicateField = ErrorCode(4822819) // Location4822819

	// ErrStageSkipBadValue indicates that $skip stage contains invalid value.
	ErrStageSkipBadValue = ErrorCode(5107200) // Location5107200

	// ErrStageLimitInvalidArg indicates invalid argument for the aggregation $limit stage.
	ErrStageLimitInvalidArg = ErrorCode(5107201) // Location5107201

	// ErrStageCollStatsInvalidArg indicates invalid argument for the aggregation $collStats stage.
	ErrStageCollStatsInvalidArg = ErrorCode(5447000) // Location5447000

	// ErrOpQueryCollectionSuffixMissing indicates that op query collection does not contain .$cmd suffix.
	ErrOpQueryCollectionSuffixMissing = ErrorCode(5739101) // Location5739101

	// ErrNOperatorNNotIntegral indicates a non-numeric 'n' for $maxN, $minN, $firstN or $lastN.
	ErrNOperatorNNotIntegral = ErrorCode(5787902) // Location5787902

	// ErrNOperatorNFractional indicates a numeric but non-integral 'n'.
	ErrNOperatorNFractional = ErrorCode(5787903) // Location5787903

	// ErrNOperatorNNotPositive indicates an 'n' below 1.
	ErrNOperatorNNotPositive = ErrorCode(5787908) // Location5787908

	// ErrNOperatorInputNotArray indicates that the input of an N operator is not an array.
	ErrNOperatorInputNotArray = ErrorCode(5788200) // Location5788200

	// ErrStageIndexedStringVectorDuplicate indicates that input to IndexedStringVector contained duplicate values.
	ErrStageIndexedStringVectorDuplicate = ErrorCode(7582300) // Location7582300

	// ErrGetFieldUnknownArgument indicates that $getField received an argument it does not accept.
	ErrGetFieldUnknownArgument = ErrorCode(3041701) // Location3041701

	// ErrGetFieldMissingField indicates that $getField was used without the required 'field' argument.
	ErrGetFieldMissingField = ErrorCode(3041702) // Location3041702

	// ErrGetFieldMissingInput indicates that the $getField full form omitted the required 'input' argument.
	ErrGetFieldMissingInput = ErrorCode(3041703) // Location3041703

	// ErrGetFieldFieldNotString indicates that the $getField 'field' argument did not evaluate to a string.
	ErrGetFieldFieldNotString = ErrorCode(3041704) // Location3041704

	// ErrSetFieldUnknownArgument indicates that $setField or $unsetField received an argument it does not accept.
	ErrSetFieldUnknownArgument = ErrorCode(4161101) // Location4161101

	// ErrSetFieldMissingField indicates that $setField or $unsetField was used without the required 'field' argument.
	ErrSetFieldMissingField = ErrorCode(4161102) // Location4161102

	// ErrSetFieldNonConstantField indicates that a $setField or $unsetField 'field'
	// argument was an expression rather than a constant.
	ErrSetFieldNonConstantField = ErrorCode(4161106) // Location4161106

	// ErrSetFieldFieldPathReference indicates that a $setField or $unsetField 'field'
	// argument was a field path or variable reference, which the operators reject.
	ErrSetFieldFieldPathReference = ErrorCode(4161108) // Location4161108

	// ErrSetFieldMissingValue indicates that $setField was used without the required 'value' argument.
	ErrSetFieldMissingValue = ErrorCode(4161103) // Location4161103

	// ErrSetFieldMissingInput indicates that $setField or $unsetField was used without the required 'input' argument.
	ErrSetFieldMissingInput = ErrorCode(4161109) // Location4161109

	// ErrSetFieldFieldNotString indicates that the $setField or $unsetField 'field'
	// argument did not evaluate to a string. MongoDB reports $unsetField failures
	// under the $setField name, which the parity suite pins.
	ErrSetFieldFieldNotString = ErrorCode(4161107) // Location4161107

	// ErrWindowDerivativeUnitRequiresDate indicates that $derivative with 'unit' requires a Date sort field.
	ErrWindowDerivativeUnitRequiresDate = ErrorCode(5624900) // Location5624900

	// ErrWindowIntegralUnitRequiresDate indicates that $integral with 'unit' requires a Date sort field.
	ErrWindowIntegralUnitRequiresDate = ErrorCode(5423901) // Location5423901

	// ErrGeoNearNotAllowedInContext indicates that $geoNear/$near/$nearSphere
	// were used in a context that does not sort (e.g. a $match stage or count).
	ErrGeoNearNotAllowedInContext = ErrorCode(5626500) // Location5626500

	ErrInvalidReplicaSetConfig               = ErrorCode(93)    // InvalidReplicaSetConfig
	ErrNotYetInitialized                     = ErrorCode(94)    // NotYetInitialized
	ErrTopologyVersionMissingProcessID       = ErrorCode(31368) // Location31368
	ErrTopologyVersionNegativeCounter        = ErrorCode(31372) // Location31372
	ErrTopologyVersionProcessIDType          = ErrorCode(31382) // Location31382
	ErrReplicaSetHeartbeatVersion            = ErrorCode(40666) // Location40666
	ErrAwaitableHelloTopologyVersionRequired = ErrorCode(51756) // Location51756
)

// ErrInfo represents additional optional error information.
type ErrInfo struct {
	Argument string // command's argument, operator, or aggregation pipeline stage that caused an error
}

// ProtoErr represents protocol error type.
type ProtoErr interface {
	// Error returns error representation for logging and debugging.
	error

	// Document returns error representation for returning to the client.
	Document() *wirebson.Document

	// Info returns additional error information, or nil.
	Info() *ErrInfo
}

// commandErrorConverter is an error from a package handlererrors cannot
// import that knows its client-facing CommandError.
type commandErrorConverter interface {
	CommandError() error
}

// ProtocolError converts any error to wire protocol error.
//
// Nil panics (it never should be passed),
// [*CommandError] or [*WriteErrors] (possibly wrapped) are returned unwrapped,
// any other values (including lazy errors) are returned as CommandError with InternalError code.
func ProtocolError(err error) ProtoErr {
	if err == nil {
		panic("err is nil")
	}

	var commandErr *CommandError
	if errors.As(err, &commandErr) {
		return commandErr
	}

	var writeErr *WriteErrors
	if errors.As(err, &writeErr) {
		return writeErr
	}

	var convertible commandErrorConverter
	if errors.As(err, &convertible) {
		if commandErr, ok := convertible.CommandError().(*CommandError); ok {
			return commandErr
		}
	}

	if errors.Is(err, bson.ErrNestingTooDeep) {
		msg := fmt.Sprintf("BSONObj exceeded maximum nested object depth: %d", bson.MaxNestingDepth)
		return NewCommandErrorMsg(ErrOverflow, msg).(*CommandError)
	}

	//nolint:errorlint // only *CommandError could be returned
	return NewCommandError(errInternalError, err).(*CommandError)
}
