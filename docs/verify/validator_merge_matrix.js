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

// Manual verification harness for the merge cross-validation matrix
// (docs/verify/validators.md Scenario 6, epic workspace-h0w).
//
// Run every cell against a running DumboDB:
//     mongosh mongodb://localhost:27017 docs/verify/validator_merge_matrix.js
//
// Run a subset (substring match on the cell name):
//     mongosh mongodb://localhost:27017 --eval 'globalThis.ONLY="baseX"' docs/verify/validator_merge_matrix.js
//
// Each cell sets up its own database, performs the merge, and self-checks the
// outcome, printing PASS / FAIL. A non-zero exit code means at least one cell
// disagreed with the documented behavior.

const RUN = Date.now();
const AUTHOR = "verifier <verify@acme.com>";
const nm = (s) => `valmx_${RUN}_${s}`;
const age = (n) => ({ age: { $gte: n } }); // the validator used throughout

// ---- primitives (the steps that were repeated over and over) ----------------

function tryCmd(d, cmd) {
  try { return d.runCommand(cmd); } catch (e) { return { ok: 0, errmsg: (e && e.message) || String(e) }; }
}
function ok(d, cmd) {
  const r = tryCmd(d, cmd);
  if (r.ok !== 1) throw new Error(`setup command failed: ${JSON.stringify(cmd)} -> ${JSON.stringify(r)}`);
  return r;
}
function freshDB(name) { const d = db.getSiblingDB(name); d.dropDatabase(); return d; }
function feat(name) { return db.getSiblingDB(name + "@feature"); }
function commit(d, msg) { return ok(d, { doltCommit: 1, message: msg, author: AUTHOR }); }
function branch(d, name) { return ok(d, { doltBranch: 1, branch: name }); }
function collMod(d, validator, action) {
  const cmd = { collMod: "items", validator: validator };
  if (action) cmd.validationAction = action;
  return ok(d, cmd);
}
function merge(d, from) { return tryCmd(d, { doltMerge: 1, merge_in: from }); }
function cont(d) { return tryCmd(d, { doltMerge: 1, continue: 1 }); }
function conflicts(d, type) {
  const all = tryCmd(d, { doltConflicts: 1 }).conflicts || [];
  return type ? all.filter((c) => c.type === type) : all;
}
function resolve(d, id, resolution, value) {
  const cmd = { doltResolveConflict: 1, collection: "items", conflictId: id, resolution: resolution };
  if (value !== undefined) cmd.value = value;
  return tryCmd(d, cmd);
}
// Branch "feature" and add the age>=0 validator there (committing both).
function branchWithValidator(d, name, action) {
  branch(d, "feature");
  const f = feat(name);
  collMod(f, age(0), action);
  commit(f, "feature: require age>=0");
  return f;
}

// ---- reporting --------------------------------------------------------------

let PASS = 0, FAIL = 0;
function expect(label, cond) {
  const good = !!cond;
  print(`    ${good ? "PASS" : "FAIL"}  ${label}`);
  good ? PASS++ : FAIL++;
}
function ageOf(d, id) { const doc = d.items.findOne({ _id: id }); return doc ? doc.age : undefined; }

// ---- the matrix cells -------------------------------------------------------
// Each is [name, expectedSummary, fn]. fn returns nothing; it self-checks.

const CELLS = [
  ["baseA_insertViolator_conflict", "validation conflict, resolve custom -> completes", () => {
    const n = nm("s1"); const d = freshDB(n);
    d.createCollection("items"); commit(d, "create items");
    branchWithValidator(d, n);
    d.items.insertOne({ _id: 1, age: -5 }); commit(d, "main: insert age -5");
    expect("merge conflicts (ok:0)", merge(d, "feature").ok === 0);
    const vc = conflicts(d, "validation");
    expect("one validation conflict on items", vc.length === 1 && vc[0].name === "items");
    expect("still-violating custom is rejected", resolve(d, vc[0].conflictId, "custom", { _id: 1, age: -1 }).ok === 0);
    expect("conforming custom accepted", resolve(d, vc[0].conflictId, "custom", { _id: 1, age: 5 }).ok === 1);
    expect("merge completes (ok:1)", cont(d).ok === 1);
    expect("doc fixed to age 5", ageOf(d, 1) === 5);
  }],

  ["baseA_insertConforming_clean", "clean merge, no conflict", () => {
    const n = nm("s2"); const d = freshDB(n);
    d.createCollection("items"); commit(d, "create items");
    branchWithValidator(d, n);
    d.items.insertOne({ _id: 1, age: 5 }); commit(d, "main: insert age 5");
    expect("merge is clean (ok:1)", merge(d, "feature").ok === 1);
  }],

  ["baseC_modifyToViolating_conflict_drop", "validation conflict, resolve drop", () => {
    const n = nm("s3"); const d = freshDB(n);
    d.createCollection("items"); d.items.insertOne({ _id: 1, age: 5 }); commit(d, "create + doc");
    branchWithValidator(d, n);
    d.items.updateOne({ _id: 1 }, { $set: { age: -5 } }); commit(d, "main: age -> -5");
    expect("merge conflicts (ok:0)", merge(d, "feature").ok === 0);
    const vc = conflicts(d, "validation");
    expect("one validation conflict", vc.length === 1);
    expect("drop accepted", resolve(d, vc[0].conflictId, "drop").ok === 1);
    expect("merge completes", cont(d).ok === 1);
    expect("offending doc removed", ageOf(d, 1) === undefined);
  }],

  ["baseX_unchanged_grandfathered", "grandfathered, no conflict", () => {
    const n = nm("s4"); const d = freshDB(n);
    d.createCollection("items"); d.items.insertOne({ _id: 1, age: -5 }); commit(d, "create + violating doc");
    branchWithValidator(d, n);
    d.items.insertOne({ _id: 2, age: 9 }); commit(d, "main: add conforming doc");
    expect("merge is clean (ok:1)", merge(d, "feature").ok === 1);
    expect("grandfathered doc survives (age -5)", ageOf(d, 1) === -5);
  }],

  ["baseX_oneSidedChange_error_conflict", "re-authored violator conflicts under error", () => {
    const n = nm("s5"); const d = freshDB(n);
    d.createCollection("items"); d.items.insertOne({ _id: 1, age: -5 }); commit(d, "create + violating doc");
    branchWithValidator(d, n); // action defaults to error
    d.items.updateOne({ _id: 1 }, { $set: { age: -9 } }); commit(d, "main: age -> -9");
    expect("merge conflicts (ok:0)", merge(d, "feature").ok === 0);
    const vc = conflicts(d, "validation");
    expect("one validation conflict", vc.length === 1);
    expect("drop accepted", resolve(d, vc[0].conflictId, "drop").ok === 1);
    expect("merge completes", cont(d).ok === 1);
  }],

  ["baseX_oneSidedChange_warn_allowed", "re-authored violator allowed under warn", () => {
    const n = nm("s6"); const d = freshDB(n);
    d.createCollection("items"); d.items.insertOne({ _id: 1, age: -5 }); commit(d, "create + violating doc");
    branchWithValidator(d, n, "warn");
    d.items.updateOne({ _id: 1 }, { $set: { age: -9 } }); commit(d, "main: age -> -9");
    expect("merge is clean under warn (ok:1)", merge(d, "feature").ok === 1);
    expect("value kept (age -9)", ageOf(d, 1) === -9);
  }],

  ["warn_insertViolator_allowed", "warn suppresses the conflict", () => {
    const n = nm("s7"); const d = freshDB(n);
    d.createCollection("items"); commit(d, "create items");
    branchWithValidator(d, n, "warn");
    d.items.insertOne({ _id: 1, age: -5 }); commit(d, "main: insert age -5");
    expect("merge is clean under warn (ok:1)", merge(d, "feature").ok === 1);
    expect("violating doc kept (age -5)", ageOf(d, 1) === -5);
  }],

  ["dataConflict_resolveViolating_rejected", "data conflict: violating resolution rejected", () => {
    const n = nm("s8"); const d = freshDB(n);
    d.createCollection("items"); d.items.insertOne({ _id: 1, age: 5 }); commit(d, "create + doc");
    branch(d, "feature");
    const f = feat(n);
    collMod(f, age(0));
    f.items.updateOne({ _id: 1 }, { $set: { age: 7, tag: "f" } }); // conforming
    commit(f, "feature: validator + age 7");
    d.items.updateOne({ _id: 1 }, { $set: { age: -5, tag: "m" } }); // violating
    commit(d, "main: age -5");
    expect("merge conflicts (ok:0)", merge(d, "feature").ok === 0);
    const dc = conflicts(d, "document");
    expect("one document conflict on _id:1", dc.length === 1);
    const id = dc[0].conflictId;
    expect("resolving to ours (violating) is rejected", resolve(d, id, "ours").ok === 0);
    expect("resolving to theirs (conforming) accepted", resolve(d, id, "theirs").ok === 1);
    expect("merge completes", cont(d).ok === 1);
    expect("doc is theirs (age 7)", ageOf(d, 1) === 7);
  }],

  ["baseX_dataConflict_resolveViolating_rejected", "base=X data conflict still requires conform", () => {
    const n = nm("s9"); const d = freshDB(n);
    d.createCollection("items"); d.items.insertOne({ _id: 1, age: -5 }); commit(d, "create + violating doc");
    branch(d, "feature");
    const f = feat(n);
    f.items.updateOne({ _id: 1 }, { $set: { age: -3, tag: "f" } }); // still violating, pre-validator
    collMod(f, age(0));
    commit(f, "feature: age -3 + validator");
    d.items.updateOne({ _id: 1 }, { $set: { age: -7, tag: "m" } });
    commit(d, "main: age -7");
    expect("merge conflicts (ok:0)", merge(d, "feature").ok === 0);
    const dc = conflicts(d, "document");
    expect("one document conflict", dc.length === 1);
    const id = dc[0].conflictId;
    expect("ours (violating) rejected", resolve(d, id, "ours").ok === 0);
    expect("theirs (violating) rejected", resolve(d, id, "theirs").ok === 0);
    expect("conforming custom accepted", resolve(d, id, "custom", { _id: 1, age: 2 }).ok === 1);
    expect("merge completes", cont(d).ok === 1);
    expect("doc fixed (age 2)", ageOf(d, 1) === 2);
  }],
];

// ---- runner -----------------------------------------------------------------

const only = (typeof globalThis.ONLY === "string") ? globalThis.ONLY : "";
for (const [name, summary, fn] of CELLS) {
  if (only && !name.includes(only)) continue;
  print(`\n=== ${name}`);
  print(`    expect: ${summary}`);
  try { fn(); } catch (e) { expect(`no setup error (${e.message})`, false); }
}
print(`\n---------------------------------------------`);
print(`${PASS} passed, ${FAIL} failed`);
if (typeof quit === "function") quit(FAIL === 0 ? 0 : 1);
