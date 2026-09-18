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

package oplog

import (
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/dolthub/dumbodb/internal/types"
	"github.com/dolthub/dumbodb/internal/util/iterator"
)

func ApplyV2Diff(preImage, diff *types.Document) (*types.Document, error) {
	if preImage == nil || diff == nil {
		return nil, errors.New("version 2 diff requires pre-image and diff documents")
	}
	if arrayHeader, _ := diff.Get("a"); arrayHeader != nil {
		return nil, errors.New("top-level version 2 diff cannot be an array diff")
	}
	if err := validateObjectDiffOrder(diff); err != nil {
		return nil, err
	}
	result := preImage.DeepCopy()
	modified := make(map[string]string)
	for _, section := range []string{"d", "u", "i"} {
		value, _ := diff.Get(section)
		if value == nil {
			continue
		}
		document, ok := value.(*types.Document)
		if !ok {
			return nil, fmt.Errorf("version 2 diff section %q must be a document", section)
		}
		iter := document.Iterator()
		for {
			field, replacement, err := iter.Next()
			if errors.Is(err, iterator.ErrIteratorDone) {
				break
			}
			if err != nil {
				iter.Close()
				return nil, err
			}
			if previous := modified[field]; previous != "" {
				iter.Close()
				return nil, fmt.Errorf("version 2 diff field %q occurs in both %q and %q", field, previous, section)
			}
			modified[field] = section
			switch section {
			case "d":
				result.Remove(field)
			case "u":
				result.Set(field, cloneDiffValue(replacement))
			case "i":
				result.Remove(field)
				result.Set(field, cloneDiffValue(replacement))
			}
		}
		iter.Close()
	}
	iter := diff.Iterator()
	defer iter.Close()
	for {
		section, value, err := iter.Next()
		if errors.Is(err, iterator.ErrIteratorDone) {
			return result, nil
		}
		if err != nil {
			return nil, err
		}
		if section == "d" || section == "u" || section == "i" {
			continue
		}
		if !strings.HasPrefix(section, "s") || len(section) == 1 {
			return nil, fmt.Errorf("unknown version 2 diff section %q", section)
		}
		field := section[1:]
		if previous := modified[field]; previous != "" {
			return nil, fmt.Errorf("version 2 diff field %q occurs in both %q and %q", field, previous, section)
		}
		modified[field] = section
		subDiff, ok := value.(*types.Document)
		if !ok {
			return nil, fmt.Errorf("version 2 sub-diff %q must be a document", section)
		}
		if header, _ := subDiff.Get("a"); header != nil && header != true {
			return nil, fmt.Errorf("array sub-diff %q must have a: true", section)
		}
		if isArrayDiff(subDiff) {
			if _, err := applyArrayDiff(types.MakeArray(0), subDiff); err != nil {
				return nil, fmt.Errorf("validating array sub-diff %q: %w", field, err)
			}
		} else {
			if _, err := ApplyV2Diff(new(types.Document), subDiff); err != nil {
				return nil, fmt.Errorf("validating object sub-diff %q: %w", field, err)
			}
		}
		preValue, _ := result.Get(field)
		if array, ok := preValue.(*types.Array); ok && isArrayDiff(subDiff) {
			updated, err := applyArrayDiff(array, subDiff)
			if err != nil {
				return nil, fmt.Errorf("applying array sub-diff %q: %w", field, err)
			}
			result.Set(field, updated)
			continue
		}
		if document, ok := preValue.(*types.Document); ok && !isArrayDiff(subDiff) {
			updated, err := ApplyV2Diff(document, subDiff)
			if err != nil {
				return nil, fmt.Errorf("applying object sub-diff %q: %w", field, err)
			}
			result.Set(field, updated)
			continue
		}
		if preValue != nil {
			result.Set(field, types.Null)
		}
	}
}

func applyArrayDiff(preImage *types.Array, diff *types.Document) (*types.Array, error) {
	fields, err := diffFieldNames(diff)
	if err != nil {
		return nil, err
	}
	if len(fields) == 0 || fields[0] != "a" {
		return nil, errors.New("array diff must start with a: true")
	}
	for index, field := range fields {
		if field == "l" && index != 1 {
			return nil, errors.New("array diff l must immediately follow a")
		}
	}
	header, _ := diff.Get("a")
	if header != true {
		return nil, errors.New("array diff must start with a: true")
	}
	newLength := preImage.Len()
	if lengthValue, _ := diff.Get("l"); lengthValue != nil {
		length, ok := lengthValue.(int32)
		if !ok || length < 0 {
			return nil, errors.New("array diff l must be a non-negative integer")
		}
		newLength = int(length)
	}
	modifications := make(map[int]any)
	subDiffs := make(map[int]*types.Document)
	iter := diff.Iterator()
	defer iter.Close()
	maximumIndex := -1
	for {
		field, value, err := iter.Next()
		if errors.Is(err, iterator.ErrIteratorDone) {
			break
		}
		if err != nil {
			return nil, err
		}
		if field == "a" || field == "l" {
			continue
		}
		if len(field) < 2 || field[0] != 'u' && field[0] != 's' {
			return nil, fmt.Errorf("invalid array diff field %q", field)
		}
		index, err := strconv.Atoi(field[1:])
		if err != nil || index < 0 || strconv.Itoa(index) != field[1:] {
			return nil, fmt.Errorf("invalid array diff index %q", field[1:])
		}
		if _, ok := modifications[index]; ok {
			return nil, fmt.Errorf("array diff index %d is modified more than once", index)
		}
		if _, ok := subDiffs[index]; ok {
			return nil, fmt.Errorf("array diff index %d is modified more than once", index)
		}
		if field[0] == 'u' {
			modifications[index] = value
		} else {
			subDiff, ok := value.(*types.Document)
			if !ok {
				return nil, fmt.Errorf("array sub-diff %q must be a document", field)
			}
			subDiffs[index] = subDiff
		}
		if index > maximumIndex {
			maximumIndex = index
		}
	}
	if maximumIndex >= newLength {
		newLength = maximumIndex + 1
	}
	result := types.MakeArray(newLength)
	for index := 0; index < newLength; index++ {
		if value, ok := modifications[index]; ok {
			result.Append(cloneDiffValue(value))
			continue
		}
		preValue, getErr := preImage.Get(index)
		preExists := getErr == nil
		if subDiff, ok := subDiffs[index]; ok {
			if !preExists {
				result.Append(types.Null)
				continue
			}
			if array, ok := preValue.(*types.Array); ok && isArrayDiff(subDiff) {
				updated, err := applyArrayDiff(array, subDiff)
				if err != nil {
					return nil, err
				}
				result.Append(updated)
				continue
			}
			if document, ok := preValue.(*types.Document); ok && !isArrayDiff(subDiff) {
				updated, err := ApplyV2Diff(document, subDiff)
				if err != nil {
					return nil, err
				}
				result.Append(updated)
				continue
			}
			result.Append(types.Null)
			continue
		}
		if preExists {
			result.Append(cloneDiffValue(preValue))
		} else {
			result.Append(types.Null)
		}
	}
	return result, nil
}

func validateObjectDiffOrder(diff *types.Document) error {
	if diff.Len() == 0 {
		return errors.New("version 2 object diff cannot be empty")
	}
	previousOrder := 0
	inSubDiffs := false
	iter := diff.Iterator()
	defer iter.Close()
	for {
		field, value, err := iter.Next()
		if errors.Is(err, iterator.ErrIteratorDone) {
			return nil
		}
		if err != nil {
			return err
		}
		order := 0
		switch field {
		case "d":
			order = 1
		case "u":
			order = 2
		case "i":
			order = 3
		default:
			if strings.HasPrefix(field, "s") && len(field) > 1 {
				order = 5
				inSubDiffs = true
			} else {
				return fmt.Errorf("unknown version 2 diff section %q", field)
			}
		}
		if inSubDiffs && order != 5 || order < previousOrder || order == previousOrder && order != 5 {
			return fmt.Errorf("version 2 diff section %q is out of order", field)
		}
		if _, ok := value.(*types.Document); !ok {
			return fmt.Errorf("version 2 diff section %q must be a document", field)
		}
		previousOrder = order
	}
}

func diffFieldNames(document *types.Document) ([]string, error) {
	fields := make([]string, 0, document.Len())
	iter := document.Iterator()
	defer iter.Close()
	for {
		field, _, err := iter.Next()
		if errors.Is(err, iterator.ErrIteratorDone) {
			return fields, nil
		}
		if err != nil {
			return nil, err
		}
		fields = append(fields, field)
	}
}

func isArrayDiff(diff *types.Document) bool {
	value, _ := diff.Get("a")
	return value != nil
}

func cloneDiffValue(value any) any {
	switch value := value.(type) {
	case *types.Document:
		return value.DeepCopy()
	case *types.Array:
		return value.DeepCopy()
	default:
		return value
	}
}
