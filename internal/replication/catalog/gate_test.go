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

package catalog

import (
	"errors"
	"strings"
	"testing"

	"github.com/dolthub/dumbodb/internal/types"
	"github.com/dolthub/dumbodb/internal/util/must"
)

func TestPreflightCollectionBuildsSupportedPlan(t *testing.T) {
	validator := must.NotFail(types.NewDocument("status", must.NotFail(types.NewDocument("$in", must.NotFail(types.NewArray("open", "closed"))))))
	options := must.NotFail(types.NewDocument(
		"validator", validator,
		"validationLevel", "strict",
		"validationAction", "error",
	))
	partial := must.NotFail(types.NewDocument("active", true))
	indexes := []*types.Document{
		must.NotFail(types.NewDocument("v", int32(2), "name", "_id_", "key", must.NotFail(types.NewDocument("_id", int32(1))))),
		must.NotFail(types.NewDocument(
			"v", int32(2), "name", "account_1", "key", must.NotFail(types.NewDocument("account", int32(1))),
			"unique", true, "sparse", true, "partialFilterExpression", partial,
		)),
	}
	plan, err := PreflightCollection("sales", "orders", options, indexes)
	if err != nil {
		t.Fatal(err)
	}
	if plan.Create.Validator != validator || plan.Create.ValidationLevel != "strict" || len(plan.Indexes) != 2 {
		t.Fatalf("collection plan = %+v", plan)
	}
	index := plan.Indexes[1]
	if !index.Unique || !index.Sparse || index.PartialFilterExpression != partial || index.MatchesPartialFilter == nil {
		t.Fatalf("supported index = %+v", index)
	}
}

func TestPreflightCollectionSupportsViewAndTimeSeries(t *testing.T) {
	viewOptions := must.NotFail(types.NewDocument(
		"viewOn", "orders",
		"pipeline", must.NotFail(types.NewArray(must.NotFail(types.NewDocument("$match", must.NotFail(types.NewDocument("active", true)))))),
		"collation", must.NotFail(types.NewDocument("locale", "en")),
	))
	view, err := PreflightCollection("sales", "active_orders", viewOptions, nil)
	if err != nil {
		t.Fatal(err)
	}
	if view.Create.ViewOn != "orders" || view.Create.ViewPipeline.Len() != 1 || view.Create.Collation == nil {
		t.Fatalf("view plan = %+v", view.Create)
	}
	timeSeriesOptions := must.NotFail(types.NewDocument(
		"timeseries", must.NotFail(types.NewDocument("timeField", "at", "metaField", "device", "granularity", "minutes")),
	))
	timeSeries, err := PreflightCollection("metrics", "samples", timeSeriesOptions, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !timeSeries.Create.IsTimeSeries || timeSeries.Create.TimeField != "at" || timeSeries.Create.MetaField != "device" {
		t.Fatalf("time-series plan = %+v", timeSeries.Create)
	}
}

func TestPreflightCollectionRejectsEveryUnsupportedClass(t *testing.T) {
	tests := []struct {
		name    string
		options *types.Document
		index   *types.Document
		option  string
	}{
		{name: "capped", options: must.NotFail(types.NewDocument("capped", true)), option: "capped"},
		{name: "collection ttl", options: must.NotFail(types.NewDocument("expireAfterSeconds", int64(60))), option: "expireAfterSeconds"},
		{name: "clustered", options: must.NotFail(types.NewDocument("clusteredIndex", must.NotFail(types.NewDocument("key", must.NotFail(types.NewDocument("_id", int32(1))))))), option: "clusteredIndex"},
		{name: "pre images", options: must.NotFail(types.NewDocument("changeStreamPreAndPostImages", must.NotFail(types.NewDocument("enabled", true)))), option: "changeStreamPreAndPostImages"},
		{name: "index ttl", index: testGateIndex("ttl", "created", int32(1), "expireAfterSeconds", int64(60)), option: "expireAfterSeconds"},
		{name: "text", index: testGateIndex("text", "body", "text"), option: "key.body"},
		{name: "geo", index: testGateIndex("geo", "point", "2dsphere"), option: "key.point"},
		{name: "wildcard", index: testGateIndex("wild", "$**", int32(1)), option: "key.$**"},
		{name: "hidden", index: testGateIndex("hidden", "value", int32(1), "hidden", true), option: "hidden"},
		{name: "storage engine", index: testGateIndex("storage", "value", int32(1), "storageEngine", must.NotFail(types.NewDocument())), option: "storageEngine"},
		{name: "collation", index: testGateIndex("collated", "value", int32(1), "collation", must.NotFail(types.NewDocument("locale", "en"))), option: "collation"},
		{name: "unfinished", index: testGateIndex("building", "value", int32(1), "buildUUID", "build"), option: "buildUUID"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var indexes []*types.Document
			if test.index != nil {
				indexes = []*types.Document{test.index}
			}
			_, err := PreflightCollection("sales", "orders", test.options, indexes)
			var unsupported *UnsupportedFeatureError
			if !errors.As(err, &unsupported) {
				t.Fatalf("error = %v, want UnsupportedFeatureError", err)
			}
			if unsupported.Database != "sales" || unsupported.Collection != "orders" || unsupported.Option != test.option {
				t.Fatalf("unsupported error = %+v", unsupported)
			}
			if !strings.Contains(err.Error(), "sales.orders") {
				t.Fatalf("error does not name namespace: %v", err)
			}
		})
	}
}

func testGateIndex(name, field string, direction any, options ...any) *types.Document {
	values := []any{"v", int32(2), "name", name, "key", must.NotFail(types.NewDocument(field, direction))}
	values = append(values, options...)
	return must.NotFail(types.NewDocument(values...))
}
