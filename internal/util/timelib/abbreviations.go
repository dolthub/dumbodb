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

package timelib

import "strings"

type abbreviation struct {
	dst    int64
	offset int64
}

// abbreviations holds the first entry for each name in timelib 2022.10's
// timezonemap.h, which is the entry abbr_search returns for a lookup by name.
var abbreviations = map[string]abbreviation{
	"acdt": {dst: 1, offset: 37800},
	"acst": {dst: 0, offset: 34200},
	"addt": {dst: 1, offset: -7200},
	"adt":  {dst: 1, offset: -10800},
	"aedt": {dst: 1, offset: 39600},
	"aest": {dst: 0, offset: 36000},
	"ahdt": {dst: 1, offset: -32400},
	"ahst": {dst: 0, offset: -36000},
	"akdt": {dst: 1, offset: -28800},
	"akst": {dst: 0, offset: -32400},
	"amt":  {dst: 0, offset: -13840},
	"apt":  {dst: 1, offset: -10800},
	"ast":  {dst: 0, offset: -14400},
	"awdt": {dst: 1, offset: 32400},
	"awst": {dst: 0, offset: 28800},
	"awt":  {dst: 1, offset: -10800},
	"bdst": {dst: 1, offset: 7200},
	"bdt":  {dst: 1, offset: -36000},
	"bmt":  {dst: 0, offset: -14309},
	"bst":  {dst: 1, offset: 3600},
	"cast": {dst: 0, offset: 34200},
	"cat":  {dst: 0, offset: 7200},
	"cddt": {dst: 1, offset: -14400},
	"cdt":  {dst: 1, offset: -18000},
	"cemt": {dst: 1, offset: 10800},
	"cest": {dst: 1, offset: 7200},
	"cet":  {dst: 0, offset: 3600},
	"cmt":  {dst: 0, offset: -15408},
	"cpt":  {dst: 1, offset: -18000},
	"cst":  {dst: 0, offset: -21600},
	"cwt":  {dst: 1, offset: -18000},
	"chst": {dst: 0, offset: 36000},
	"dmt":  {dst: 0, offset: -1521},
	"eat":  {dst: 0, offset: 10800},
	"eddt": {dst: 1, offset: -10800},
	"edt":  {dst: 1, offset: -14400},
	"eest": {dst: 1, offset: 10800},
	"eet":  {dst: 0, offset: 7200},
	"emt":  {dst: 0, offset: -26248},
	"ept":  {dst: 1, offset: -14400},
	"est":  {dst: 0, offset: -18000},
	"ewt":  {dst: 1, offset: -14400},
	"ffmt": {dst: 0, offset: -14660},
	"fmt":  {dst: 0, offset: -4056},
	"gdt":  {dst: 1, offset: 39600},
	"gmt":  {dst: 0, offset: 0},
	"gst":  {dst: 0, offset: 36000},
	"hdt":  {dst: 1, offset: -34200},
	"hkst": {dst: 1, offset: 32400},
	"hkt":  {dst: 0, offset: 28800},
	"hmt":  {dst: 0, offset: -19776},
	"hpt":  {dst: 1, offset: -34200},
	"hst":  {dst: 0, offset: -36000},
	"hwt":  {dst: 1, offset: -34200},
	"iddt": {dst: 1, offset: 14400},
	"idt":  {dst: 1, offset: 10800},
	"imt":  {dst: 0, offset: 25025},
	"ist":  {dst: 0, offset: 7200},
	"jdt":  {dst: 1, offset: 36000},
	"jmt":  {dst: 0, offset: 8440},
	"jst":  {dst: 0, offset: 32400},
	"kdt":  {dst: 1, offset: 36000},
	"kmt":  {dst: 0, offset: 5736},
	"kst":  {dst: 0, offset: 30600},
	"lst":  {dst: 1, offset: 9394},
	"mddt": {dst: 1, offset: -18000},
	"mdst": {dst: 1, offset: 16279},
	"mdt":  {dst: 1, offset: -21600},
	"mest": {dst: 1, offset: 7200},
	"met":  {dst: 0, offset: 3600},
	"mmt":  {dst: 0, offset: 9017},
	"mpt":  {dst: 1, offset: -21600},
	"msd":  {dst: 1, offset: 14400},
	"msk":  {dst: 0, offset: 10800},
	"mst":  {dst: 0, offset: -25200},
	"mwt":  {dst: 1, offset: -21600},
	"nddt": {dst: 1, offset: -5400},
	"ndt":  {dst: 1, offset: -9052},
	"npt":  {dst: 1, offset: -9000},
	"nst":  {dst: 0, offset: -12600},
	"nwt":  {dst: 1, offset: -9000},
	"nzdt": {dst: 1, offset: 46800},
	"nzmt": {dst: 0, offset: 41400},
	"nzst": {dst: 0, offset: 43200},
	"pddt": {dst: 1, offset: -21600},
	"pdt":  {dst: 1, offset: -25200},
	"pkst": {dst: 1, offset: 21600},
	"pkt":  {dst: 0, offset: 18000},
	"plmt": {dst: 0, offset: 25590},
	"pmt":  {dst: 0, offset: -13236},
	"ppmt": {dst: 0, offset: -17340},
	"ppt":  {dst: 1, offset: -25200},
	"pst":  {dst: 0, offset: -28800},
	"pwt":  {dst: 1, offset: -25200},
	"qmt":  {dst: 0, offset: -18840},
	"rmt":  {dst: 0, offset: 5794},
	"sast": {dst: 0, offset: 7200},
	"sdmt": {dst: 0, offset: -16800},
	"sjmt": {dst: 0, offset: -20173},
	"smt":  {dst: 0, offset: -13884},
	"sst":  {dst: 0, offset: -39600},
	"tbmt": {dst: 0, offset: 10751},
	"tmt":  {dst: 0, offset: 12344},
	"uct":  {dst: 0, offset: 0},
	"utc":  {dst: 0, offset: 0},
	"wast": {dst: 1, offset: 7200},
	"wat":  {dst: 0, offset: 3600},
	"wemt": {dst: 1, offset: 7200},
	"west": {dst: 1, offset: 3600},
	"wet":  {dst: 0, offset: 0},
	"wib":  {dst: 0, offset: 25200},
	"wita": {dst: 0, offset: 28800},
	"wit":  {dst: 0, offset: 32400},
	"wmt":  {dst: 0, offset: 5040},
	"yddt": {dst: 1, offset: -25200},
	"ydt":  {dst: 1, offset: -28800},
	"ypt":  {dst: 1, offset: -28800},
	"yst":  {dst: 0, offset: -32400},
	"ywt":  {dst: 1, offset: -28800},
	"a":    {dst: 0, offset: 3600},
	"b":    {dst: 0, offset: 7200},
	"c":    {dst: 0, offset: 10800},
	"d":    {dst: 0, offset: 14400},
	"e":    {dst: 0, offset: 18000},
	"f":    {dst: 0, offset: 21600},
	"g":    {dst: 0, offset: 25200},
	"h":    {dst: 0, offset: 28800},
	"i":    {dst: 0, offset: 32400},
	"k":    {dst: 0, offset: 36000},
	"l":    {dst: 0, offset: 39600},
	"m":    {dst: 0, offset: 43200},
	"n":    {dst: 0, offset: -3600},
	"o":    {dst: 0, offset: -7200},
	"p":    {dst: 0, offset: -10800},
	"q":    {dst: 0, offset: -14400},
	"r":    {dst: 0, offset: -18000},
	"s":    {dst: 0, offset: -21600},
	"t":    {dst: 0, offset: -25200},
	"u":    {dst: 0, offset: -28800},
	"v":    {dst: 0, offset: -32400},
	"w":    {dst: 0, offset: -36000},
	"x":    {dst: 0, offset: -39600},
	"y":    {dst: 0, offset: -43200},
	"z":    {dst: 0, offset: 0},
}

// lookupAbbreviation is abbr_search: "utc" and "gmt" first, then the table.
func lookupAbbreviation(word string) (abbreviation, bool) {
	lower := strings.ToLower(word)
	if lower == "utc" || lower == "gmt" {
		return abbreviation{}, true
	}
	a, ok := abbreviations[lower]
	return a, ok
}
