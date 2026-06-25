![Logo](./img/dolt-dumbo-logo.png)

## What is DumboDB?

[MongoDB](https://github.com/mongodb/mongo) and [Git](https://git-scm.com/) had a baby, and it's named **Dumbo**. It's a document database with built-in version control, so you can track changes, branch, and merge just like you would with code  -- but for your data.

DumboDB leverages the power of the [Dolt](https://github.com/dolthub/dolt) storage engine. Dolt's [prolly trees](https://docs.dolthub.com/architecture/storage-engine/prolly-tree) enable efficient storage of data over time thanks to structural sharing between commits. This means you can have a rich history of changes without worrying about storage bloat. 

## What's in Version 0.1?
Dumbo v0.1 is Alpha quality software. We don't recommend it for production use, but it's great for testing your existing applications and seeing what they are changing over time. In a test environment, you can use DumboDB just like you would use MongoDB, but with the added ability to reset to specific snapshots in time and see the changes made by your application code.

We are actively developing new features, so please [join our Discord server](https://discord.gg/gqr7K4VNKe) and give us feedback. See the [roadmap below](#roadmap)!

### MongoDB Compatibility
DumboDB implements the MongoDB 8.0 wire protocol and is designed for high parity with core MongoDB operations. It functions as a drop-in replacement for [standard drivers](https://www.mongodb.com/docs/drivers/) and [`mongosh`](https://www.mongodb.com/docs/mongodb-shell/) in single-node environments.

#### Parity
- BSON Engine: Full support for BSON type fidelity, including nested documents, arrays, and ObjectIDs.
- Query & Update: Supports complex MQL operators (e.g., $elemMatch, $all, $rename) and atomic array modifiers ($push, $pull, $bit).
- Aggregation Framework: Implementation of multi-stage pipelines including $match, $group, $unwind, and $lookup.
- Indexing: Support for secondary indexes on top-level and nested fields to ensure query performance.

#### Limitations & Scope
- No Authentication. DumboDB does not implement any authentication. It is intended for use in trusted environments or local development. Do not expose DumboDB instances to untrusted networks. Planned for v0.6.
- Single Node: Replication (Replica Sets) and Sharding are out of scope. Support not planned.
- Ecosystem Features: Proprietary features specific to [MongoDB Atlas](https://www.mongodb.com/lp/cloud/atlas/try3) (e.g., Search Indexes, Serverless Triggers) are not supported. Support not planned.
- Capped Collections: Fixed-size collections (`capped: true`) and their oldest-first eviction are not supported; creating one is rejected with an error. Not clear if there is any place for this feature in a version-controlled database. Support not planned.
- Expiration (TTL): `expireAfterSeconds` is not supported; specifying it on a collection or index is rejected with an error rather than silently accepted. Support not planned.

If you are needing missing features, please file an [issue](https://github.com/dolthub/dumbodb/issues), or join our [Discord server](https://discord.gg/gqr7K4VNKe) to start a conversation with us. We'd love your feedback!

### Version Control Features
DumboDB's version control features are exposed via a set of custom commands (e.g. `dumboCommit`, `dumboMerge`, etc.) that you can run from any MongoDB client. These commands allow you to commit changes, create branches, merge branches, view commit history, and more.

| Command | Description |
|---------|-------------|
| [`dumboCommit`](https://github.com/dolthub/dumbodb/wiki/Commands#dumbocommit) | Commit the current working set with a message and author |
| [`dumboBranch`](https://github.com/dolthub/dumbodb/wiki/Commands#dumbobranch) | Create or delete a branch |
| [`dumboBranchStatus`](https://github.com/dolthub/dumbodb/wiki/Commands#dumbobranchstatus) | Report how many commits each target refspec is ahead and behind a base refspec |
| [`dumboMerge`](https://github.com/dolthub/dumbodb/wiki/Commands#dumbomerge) | Merge a source branch into the current branch |
| [`dumboCherryPick`](https://github.com/dolthub/dumbodb/wiki/Commands#dumbocherrypick) | Apply one commit's diff onto the current branch |
| [`dumboRebase`](https://github.com/dolthub/dumbodb/wiki/Commands#dumborebase) | Reapply branch commits onto another branch tip, rewriting history |
| [`dumboLog`](https://github.com/dolthub/dumbodb/wiki/Commands#dumbolog) | Return commit history for the current branch |
| [`dumboStatus`](https://github.com/dolthub/dumbodb/wiki/Commands#dumbostatus) | Show summary of uncommitted changes on the current branch |
| [`dumboDiff`](https://github.com/dolthub/dumbodb/wiki/Commands#dumbodiff) | Document-level diff between two states |
| [`dumboReset`](https://github.com/dolthub/dumbodb/wiki/Commands#dumboreset) | Move branch HEAD to a target commit (soft or hard) |
| [`dumboRevert`](https://github.com/dolthub/dumbodb/wiki/Commands#dumborevert) | Revert a commit, creating a new inverse commit |
| [`dumboConflicts`](https://github.com/dolthub/dumbodb/wiki/Commands#dumboconflicts) | List or inspect conflicts from an in-progress merge/cherry-pick/rebase |
| [`dumboResolveConflict`](https://github.com/dolthub/dumbodb/wiki/Commands#dumboresolveconflict) | Resolve a single document conflict (ours / theirs / custom) |
| [`dumboTag`](https://github.com/dolthub/dumbodb/wiki/Commands#dumbotag) | Create, list, or delete tags at specific commits |
| [`dumboGC`](https://github.com/dolthub/dumbodb/wiki/Commands#dumbogc) | Run garbage collection on the database's chunk store |

All commands have a `dolt*` alias (e.g. `doltCommit`, `doltMerge`). Use whichever prefix you prefer!

Full command reference: [Command Reference](https://github.com/dolthub/dumbodb/wiki/Commands)

## Install, Run, Connect
### Install DumboDB
#### Build From Source
```bash
git clone https://github.com/dolthub/dumbodb
cd dumbodb
make build                             # Binary ends up here: .runtime/bin/dumbodb
cp .runtime/bin/dumbodb /usr/local/bin # or somewhere else in your $PATH
```

#### Released Binaries
We publish pre-built binaries for Linux, MacOS, and Windows on our [GitHub Releases page](https://github.com/dolthub/dumbodb/releases). Download the appropriate binary for your platform, put it somewhere in your `PATH`. The specifics will depend on your operating system, as they have protections against running arbitrary binaries from the internet, so please refer to your OS documentation for how to run downloaded binaries. We are working on getting signed binaries published to make this easier!

### Run DumboDB
The only required argument to run DumboDB is `--data-dir`, which specifies the directory where your data will be stored. If the directory does not exist, it will be created.

```bash
$ dumbodb --data-dir /tmp/dumbodb-data
```

### Connect
Connect with any [MongoDB driver](https://www.mongodb.com/docs/drivers/), or use the
`mongosh` [shell](https://www.mongodb.com/try/download/shell):

```bash
mongosh mongodb://localhost:27017
```

This will connect you to the `test` database by default. You can specify a different database in the connection string if you like (e.g. `mongodb://localhost:27017/mydb`)

## Example Usage
All examples below are using the `mongosh` shell, which is effectively JavaScript. There are equivalent operations for any MongoDB driver in your favorite language.

### Specifying a Branch
By default, all connections target the `main` branch. This is currently hard coded. Using the `getSiblingDB()` method, you can connect to a different branch by encoding the branch name in the database name. `@` is the delimiter between the database name and the branch name. For example, to connect to a branch named `mybranch`, you would connect to the database `mydb@mybranch`:

```js
var db = db.getSiblingDB("mydb@mybranch")
```

If you specify a revision rather than a branch name, then the database returned will be read-only and reflect the state of the database at that revision. For example, if you'd like to perform reads against the parent commit of the `main` branch, you can connect to `mydb@main~1`:

```js
var db = db.getSiblingDB("mydb@main~1")
```

Alternatively you can use a commit hash to create a read-only connection to that commit:

```js
var db = db.getSiblingDB("mydb@v9ra3pmi0f6kotj5k3fganpmb3oi9t1k")
```

### Committing Changes
Say you want to stick two documents into the "items" collection, then commit them:

```js
var db = db.getSiblingDB("mydb@main")
db.items.insertOne({ _id: 1, label: "alpha", score: 10 })
db.items.insertOne({ _id: 2, label: "beta",  score: 20 })
db.runCommand({ dumboCommit: 1, message: "baseline", author: "alice <alice@acme.com>" })
```

The last command creates a new commit with the two inserted documents, and outputs the commit details:
```js
{
  commitId: 'egqis00l0vqg5kd7gbje8k1k6g7dl7ja',
  branch: 'main',
  message: 'baseline',
  author: 'alice <alice@acme.com>',
  timestamp: ISODate('2026-04-28T23:12:23.621Z'),
  committer: 'alice <alice@acme.com>',
  committerTimestamp: ISODate('2026-04-28T23:12:23.621Z'),
  ok: 1
}
```
_Note that there is no need to `add` items like you would with `git add`. DumboDB tracks all changes in the working set, and they will all be included in the next commit unless you explicitly discard them._

### Seeing Uncommitted Changes
Now let's modify those documents, and add a new one, but don't commit just yet:

```js
db.items.insertOne({ _id: 3, label: "gamma", score: 30 })
db.items.updateOne({ _id: 1 }, { $set: { score: 99 } })
db.items.deleteOne({ _id: 2 })
```

At this point, if we run `dumboStatus`, we can see a summary of the uncommitted changes:

```js
db.runCommand({dumboStatus: 1})
```
Will output the summary of your changes. Specifically, it shows that in the 'items' collection, you have 1 added document, 1 modified document, and 1 deleted document:
```js
{
  branch: 'main',
  dirty: true,
  readonly: false,
  collections: [
    {
      name: 'items',
      status: 'modified',
      added: 1,
      modified: 1,
      deleted: 1
    }
  ],
  ok: 1
}
```
If you need more detail, you can run `dumboDiff`. When called with no additional arguments, it will print all of the changes in your session. Specifically everything that is not committed. These are the changes that will be committed when you run `dumboCommit`.

```js
db.runCommand({ dumboDiff: 1 })
```
Will produce:
```js
{
  collections: [
    {
      name: 'items',
      status: 'modified',
      added: [ { _id: 3, label: 'gamma', score: 30 } ],
      removed: [ { _id: 2, label: 'beta', score: 20 } ],
      modified: [
        {
          _id: 1,
          diff: [ { type: 'modified', path: '$.score', from: 10, to: 99 } ]
        }
      ]
    }
  ],
  ok: 1
}
```

### Seeing What Changed
You can specify the `from` and `to` arguments to `dumboDiff` to see the changes between any two commits. For example, if you want to see the changes between the current state of your session and the last commit, you can run:

```js
db.runCommand({ dumboDiff: 1, from: "HEAD~1", to: "HEAD" })
```

The `dumboLog` command will show you the commit history for the current branch, starting with the most recent commit.

```js
db.runCommand({ dumboLog: 1, limit: 2 })
{
  commits: [
    {
      commitId: 'v9ra3pmi0f6kotj5k3fganpmb3oi9t1k',
      parent1:  'tqq1tn5qs0pns2j2uk5k1b2ufhqt9q3b',
      refs:     [ 'HEAD', 'main' ],
      message:  'alice order updated',
      timestamp: ISODate('2026-04-14T17:22:31.000Z'),
      author:   'alice <alice@acme.com>'
    },
    {
      commitId: 'tqq1tn5qs0pns2j2uk5k1b2ufhqt9q3b',
      parent1:  '5vi6e5t3riqpgh6fq0j1pf0r0imuqhsn',
      message:  'initial data',
      timestamp: ISODate('2026-04-14T09:00:00.000Z'),
      author:   'bob <bob@acme.com>'
    },
  ],
  ok: 1
}
```

### Branching and Merging
You can create a new branch with `dumboBranch`. In this example, you can see that a new variable `feature` is created to represent the new branch, and you can run commands against it directly:

```js
// Create the branch
db.runCommand({ dumboBranch: 1, branch: "feature" })
// "checkout"
var feature = db.getSiblingDB("mydb@feature")

feature.items.insertOne({ _id: 4, label: "delta", score: 40 })
feature.runCommand({ dumboCommit: 1, message: "add delta on feature branch", author: "alice <alice@acme.com>" })
```

The `dumboMerge` command will merge the changes from the `feature` branch back into whatever branch you are on. `db` is still connected to `main`, so when we run the merge command, it will merge `feature` into `main`, which in this case is a fast-forward merge, since `main` has not diverged from `feature`. The output of the merge command will show you the new commit that was created on `main` as a result of the merge.

```js
db.runCommand({ dumboMerge: 1, merge_in: "feature"})
{
  commitId: 'gr2iofosqge0se1dcu2b1a1u42l6udd3',
  message: 'fast-forward',
  ok: 1
}
```
There are also legitimate merges which join two commit histories, complete with conflict detection and resolution. Look at the [Command Reference](https://github.com/dolthub/dumbodb/wiki/Commands) for more details and examples.


## Acknowledgements

DumboDB is built on two open-source projects:

- **[FerretDB](https://github.com/FerretDB/FerretDB)**  -- The wire protocol, connection handling, type system, and handler packages were adapted from FerretDB v1.24.2 (Apache 2.0). See [ACKNOWLEDGEMENTS](https://github.com/dolthub/dumbodb/blob/main/ACKNOWLEDGEMENTS) for details.
- **[Dolt](https://github.com/dolthub/dolt)**  -- The version-controlled storage engine powering every commit, branch, and merge.

## Roadmap
DumboDB is in active development, and we have a lot of exciting features planned. Major milestones we are planning:

- **v0.2**: Garbage Collection and zstd compression. Reduce the footprint of your database. Simplified configuration for user details (name and email) so you don't have to specify them on every commit.
- **v0.3**: Add Clone, Push, and Pull support. This will allow you to sync your DumboDB repositories with remote servers, and collaborate with others.
- **v0.4**: Add support for Replication (as a secondary backup to your existing MongoDB instance)
- **v0.6**: Add Authentication and Authorization support.
- **v0.8**: Visualization and operations via a custom Workbench UI.
- **v1.0**: General availability release, with a focus on stability, performance, and usability improvements.
