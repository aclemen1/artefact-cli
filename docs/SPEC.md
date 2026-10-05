# artefact — specification

Draft 0.2, 5 October 2026. Nothing is implemented yet.

`artefact` keeps files — PDFs, images, documents, media — on behalf of the
humans and tools that produce or use them. Each file becomes an **artefact**:
a stable identity, a card that names and describes it, versions, copies on
several storage backends, a local URL, and an optional place in one or more
classification plans. `artefact` does not understand content; other tools do
(a memory, an indexer, a converter). It guarantees that the bytes exist, are
intact, are where it says, and disappear for good when asked.

## 1. Principles

1. **Identity is not content.** An artefact is named by a ULID that never
   changes. Its digest changes when it is redacted or sealed; its ULID does
   not.
2. **Open storage.** Every artefact is an OCFL 1.1 object. A store is
   readable without `artefact`.
3. **Interchangeable backends.** Local disk, Google Drive, Dropbox, iCloud,
   S3 and the rest go through one connector interface. The first connector
   wraps rclone.
4. **Copies are counted.** A replication policy says how many copies an
   artefact needs. A copy is removed only after the others are verified.
5. **Erasure is real.** `erase` removes an artefact from every backend that
   holds it, returns a proof, and leaves a tombstone that names nothing.
6. **Secrets are sealed before they travel.** A sealed artefact leaves the
   process only encrypted with age. No plaintext digest is stored.
7. **Spheres do not meet.** Each sphere has its own store, index, backends
   and keys. No deduplication, link or search crosses spheres.
8. **Agnostic.** `artefact` knows no particular user, memory or office.
   Tools reach it through the same contract.
9. **AI-native CLI.** `artefact` follows `aclemen1/ai-native-cli`: single Go
   binary, embedded skill, `artefact schema`, JSON envelope
   `{ok, result|error}`. The same actions are served over MCP and a local
   REST API.

## 2. Overview

```
 human · agent · tool (mnemo, …)
        │ CLI · MCP · REST · Go client
        ▼
 ┌──────────────── artefact (Go binary) ────────────────┐
 │ actions: put get stat seal redact hold release erase │
 │          classify sync fsck …                        │
 │                                                      │
 │ store (OCFL, per sphere) · index (SQLite, rebuildable)│
 │ replication · deciders · renditions (lector)         │
 └────────┬──────────────────────┬──────────────────────┘
          ▼                      ▼
   primary backend         other backends (rclone)
   (local disk)            Drive · Dropbox · iCloud · S3 …
```

## 3. Data model

### 3.1 Artefact

| Term | Meaning |
|---|---|
| artefact | one logical file, named by a ULID |
| version | one state of its content; versions are numbered `v1…vn` |
| payload | the file itself, in a version |
| rendition | a file derived from the payload (text, thumbnail), kept with it |
| digest | `sha256:<hex>` of a stored file; multihash-style prefix, so the algorithm can change |
| card | the artefact's metadata (§3.2) |
| holder | a tool or person that relies on the artefact (§3.4) |
| location | one copy of the object on one backend (§6) |

A new version is created when the content changes: redaction, sealing,
replacement. A card edit is not a version.

### 3.2 Card

A YAML document, one per artefact. The core fields are written by
`artefact`; the others by whoever deposits or classifies.

```yaml
id: 01K9B3T0Z6Q8M2C4R7V1X5N9PA
sphere: perso
title: Ordonnance rhumatologie, septembre 2026
date: 2026-09-18              # date of the content, not of the deposit
mime: application/pdf
size: 184233
filename: ordonnance-2026-09.pdf
version: 2
digest: sha256:3f9a…c2        # digest of the stored payload of the current version
sealed: false
dc:                           # optional Dublin Core terms
  creator: Dr X
  language: fr
classes:                      # one entry per plan (§9)
  household:
    node: "3.2.1"
    by: decider:openjev       # decider:<name> | human:<id> | rule:<id>
    confidence: 0.93
    at: 2026-10-05T14:02:11+02:00
holders:                      # §3.4
  - { by: mnemo, ref: 01K6Z8Q4V7…, at: 2026-10-05T14:01:40+02:00 }
holds: []                     # legal or manual holds that suspend disposal
x:                            # free extensions, namespaced by the writer
  mnemo: { note: "pour la caisse AVS" }
```

- `title` defaults to the file name without extension. `date` defaults to
  the deposit date.
- `x.<namespace>` is never interpreted by `artefact`.
- A card is the only mutable part of an artefact. Each change is an event
  (§3.3).

### 3.3 Events

Every action that changes an artefact appends one event, in the PREMIS
manner: what happened, when, by whom, with what outcome.

```json
{"at":"2026-10-05T14:01:40+02:00","type":"deposit","agent":"mnemo/0.4.0","outcome":"ok","version":1}
{"at":"2026-10-05T14:03:02+02:00","type":"replicate","agent":"artefact","outcome":"ok","location":"gdrive-perso"}
{"at":"2026-10-06T03:00:12+02:00","type":"fixity","agent":"artefact","outcome":"ok"}
```

Event types: `deposit`, `version`, `seal`, `redact`, `card`, `classify`,
`hold`, `release`, `replicate`, `drop`, `fixity`, `erase`. An event never
contains a secret, and an erasure removes every event that names the erased
content (§8).

### 3.4 Holders

A holder declares that it relies on an artefact: `hold <id> --by mnemo --ref
<concept ULID>`. `release` withdraws it.

- An artefact with at least one holder cannot be erased without `--force`,
  and the erasure plan names its holders.
- An artefact without holders and without classes is **orphaned**.
  `artefact ls --orphaned` lists them; nothing is deleted automatically.
- Holders are how a tool tells `artefact` which artefacts it cites, without
  `artefact` reading the tool's data.

### 3.5 Identifiers and URLs

| Form | Example | Use |
|---|---|---|
| ULID | `01K9B3T0Z6Q8M2C4R7V1X5N9PA` | canonical identity, in every contract |
| URI | `artefact://perso/01K9B3T0…` | citation by tools, MCP resource |
| local URL, by id | `http://127.0.0.1:7780/perso/a/01K9B3T0…` | current version |
| local URL, by version | `…/perso/a/01K9B3T0…/v2` | a fixed version |
| local URL, by content | `…/perso/h/sha256/3f9a…c2` | content addressing |
| export | `ni:///sha-256;P5qL…` (RFC 6920) | interchange outside `artefact` |

The sphere is the first segment of every URI and URL. A sealed artefact has
no content URL: its payload's digest is that of the ciphertext.

## 4. Storage

### 4.1 Store

Each sphere has one **store**: an OCFL 1.1 storage root on the primary
backend (local disk by default).

```
~/artefact/
  perso/                                  # OCFL storage root
    0=ocfl_1.1
    ocfl_layout.json                      # 0004-hashed-n-tuple-storage-layout
    extensions/
    3f9/a2c/71b/3f9a2c71b…/               # one OCFL object per artefact, under the SHA-256 of its id
      0=ocfl_object_1.1
      inventory.json  inventory.json.sha512
      v1/content/payload.pdf
      v2/content/payload.pdf              # redacted version
      v2/content/renditions/text.md       # from lector (§10)
      extensions/artefact/card.yaml       # mutable card (§3.2)
      extensions/artefact/events.jsonl    # event log (§3.3)
  pro/
    (same structure)
  .artefact/                              # internal state, outside OCFL
    index-perso.sqlite
    index-pro.sqlite
    plans/<plan>.yaml
```

- The object id in `inventory.json` is the URI `artefact://<sphere>/<ulid>`.
- The inventory carries the SHA-512 digests OCFL requires and a `fixity`
  block with SHA-256, the digest used by every contract.
- The card and the event log sit in the object's `extensions/` directory,
  which OCFL leaves to the application. They are not versioned content, so
  a card edit does not create an OCFL version.
- The index is a cache: `artefact reindex` rebuilds it from the store,
  idempotency keys included (they are kept in the `deposit` event).
- A new object is written in `extensions/artefact-staging/` of the storage
  root, then renamed into place.
- The objects pass the `ocfl-go` validator; its only warning is the
  unregistered `extensions/artefact` directory.

### 4.2 Deduplication

- Within a sphere, `put` of a payload already stored returns the existing
  artefact (`status: known`). The caller can still add itself as a holder.
- Across spheres, nothing is compared. The same file deposited in `perso`
  and `pro` becomes two artefacts.
- A sealed artefact is deduplicated by a keyed digest:
  `hmac-sha256(sphere key, plaintext)`. The plaintext SHA-256 is never
  stored, so a short secret cannot be found by trial. The HMAC key is
  derived from the sphere's age identity, so it lives only in the keychain.
  A sealed payload never matches a clear one.

### 4.3 Sealing

- Each sphere has its own age identity: public key in the configuration,
  private key in the macOS keychain (service `artefact`, account
  `age-<sphere>`), both created by `artefact key init --sphere <s>`.
- `put --seal` encrypts before anything is written. `seal <id>` encrypts an
  existing artefact: it creates a sealed version, then erases the clear
  versions (§8) from the store and every backend.
- Renditions of a sealed artefact are sealed too, or not produced.
- `get --reveal` decrypts to standard output or to `--out`. Over HTTP, a
  sealed artefact is served only to a token that carries `reveal` (§12).

## 5. Public interface

### 5.1 Actions

| Action | Effect |
|---|---|
| `put <file\|-> [--sphere] [--title] [--date] [--seal] [--hold <by>:<ref>] [--card <yaml>] [--key <key>] [--render text] [--render-timeout 30s]` | deposits; idempotent within the sphere; returns `{id, version, status: created\|known, holders, rendition}` |
| `get <id> [--version vN] [--reveal] [--out <path>] [--rendition <name>]` | writes the payload or a rendition |
| `stat <id>...` | card, versions, holders, renditions, locations and their last fixity check; one or many ids |
| `ls [--sphere] [--class <plan>:<node>] [--held-by <tool>[:<ref>]] [--orphaned] [--sealed] [--since]` | lists artefacts |
| `render <id> [--wait]` | asks `lector` for the text rendition again |
| `du [--sphere]` | count and size of artefacts, payloads and renditions |
| `card <id> --set <field>=<value> \| --merge <yaml>` | edits the card |
| `seal <id>` | sealed version, clear versions erased |
| `redact <id> --from <file>` | new version from a redacted file; with `--purge`, earlier versions are erased |
| `hold <id> --by <tool> --ref <ref>`, `release <id> --by <tool> [--ref]` | holders |
| `erase <id>... [--version vN]` | returns a plan and a single-use code |
| `erase --confirm <code>` | runs the plan, returns the proof |
| `classify <id> [--plan] [--decider]` | decides classes (§9) |
| `sync [--sphere] [--dry-run]` | applies the replication policy (§7) |
| `fsck [--sphere] [--backend] [--sample N]` | verifies digests on backends |
| `review [--plan] [--due-before]` | lists dispositions that are due (§9.4) |

Every action returns the envelope. Errors name the canonical invocation,
echo the offending values and show an example, as the ai-native-cli spec
requires.

Guarantees the actions give their callers:

- **`put` is atomic.** The object is written in a temporary directory of the
  store and renamed into place; an interrupted `put` leaves nothing.
- **`put` is replayable.** `--key <key>` is an idempotency key, scoped to the
  sphere: a second `put` with the same key and the same payload returns the
  first result. Without `--key`, the payload's digest plays that role.
- **`put` from standard input** (`put -`) accepts a payload that exists only
  in memory, including with `--seal`.
- **`put` returns the holders** already recorded, so a caller finds its own
  earlier reference to a `known` payload.
- **`hold` and `release` are idempotent.** Holding twice with the same
  `(by, ref)` records one holder; releasing an absent holder succeeds.
- **`get --out <path>`** writes the file with mode 0600; with `--reveal`, the
  clear text exists only in that file. The caller deletes it.
- **`stat` and `ls` take batches**, so a caller can audit thousands of
  references in one call.

### 5.2 Operation and meta commands

| Command | Role |
|---|---|
| `artefact init`, `artefact key init`, `artefact backend add\|ls\|rm` | setup |
| `artefact serve` | REST, MCP over HTTP, scheduled `sync` and `fsck` |
| `artefact mcp [--spheres]` | MCP over stdio |
| `artefact reindex`, `artefact stats` | upkeep |
| `artefact token create\|list\|revoke` | access (§12) |
| `artefact tui` | terminal interface (§13) |
| `artefact schema`, `artefact skill show\|install`, `artefact version` | AI-native CLI |

### 5.3 Surfaces

The actions are defined once (action specs) and served on four surfaces:

| Surface | Form |
|---|---|
| CLI | `artefact <action> …`, JSON by default |
| MCP | one tool per action; artefacts as resources `artefact://<sphere>/<id>` |
| REST | `artefact serve`, on `127.0.0.1` or a Unix socket (§11) |
| Go client | package `github.com/aclemen1/artefact-cli/client`, over REST |

## 6. Backends

### 6.1 Connector interface

```go
type Backend interface {
    Put(ctx, key string, r io.Reader, size int64) (Location, error)
    Get(ctx, loc Location) (io.ReadCloser, error)
    Stat(ctx, loc Location) (Info, error)   // size, provider digest if any
    Delete(ctx, loc Location) error
    List(ctx, prefix string) iter.Seq2[Location, error]
}
```

- A **location** is what the backend needs to find the copy again: a path
  for disk and Dropbox, a `fileId` for Google Drive. It is recorded in the
  index and in a `replicate` event.
- Objects are copied whole: an OCFL object directory, inventory included.
- `Stat` returns the provider's digest when it has one (Drive
  `sha256Checksum`, Dropbox `content_hash`); otherwise `fsck` downloads.

### 6.2 Connectors

| Connector | Through | Notes |
|---|---|---|
| `local` | native Go | primary store by default |
| `rclone` | `github.com/rclone/rclone/fs`, pinned version | any rclone remote: Drive, Dropbox, iCloud Drive, S3, SFTP, WebDAV… |

- A backend belongs to one sphere. Its configuration names the sphere, and
  `artefact` refuses to write another sphere's object there.
- `trusted: false` marks a backend whose copies do not count towards
  `mincopies`. iCloud Drive is untrusted by default: macOS may evict a file
  from the disk at any time, and the rclone connector is experimental.
- Sealed objects are stored encrypted on every backend, whatever its own
  encryption.

## 7. Replication

```yaml
replication:
  perso: { numcopies: 2, mincopies: 1, backends: [local, gdrive-perso] }
  pro:   { numcopies: 2, mincopies: 1, backends: [local, gdrive-pro] }
  classes:
    "household:3": { numcopies: 3 }       # override for a plan node
```

- `numcopies`: copies wanted. `mincopies`: copies that must exist, verified,
  before any copy is dropped.
- `sync` copies what is missing, records each location, and drops copies on
  backends no longer listed, after verifying `mincopies` elsewhere.
- `fsck` verifies digests; a mismatch marks the location bad and `sync`
  replaces it from a good copy.
- `serve` runs `sync` after each deposit and `fsck` on a sample every night.
- `stats` reports artefacts below `numcopies` and below `mincopies`.

## 8. Erasure

Erasure is the only operation that destroys data. It runs in two steps.

1. `erase <id>…` builds the plan: versions or artefacts that disappear,
   backends and locations to purge, holders that rely on them, renditions,
   index rows. It returns the plan, its digest and a single-use code, valid
   `erase.code_ttl` (ten minutes by default); in a terminal, it asks for the
   code to be typed. An artefact with holders needs `--force`.
   `erase --plan` returns the plan without asking for the code, for a caller
   that confirms through its own interface.
2. `erase --confirm <code>` re-plans, refuses if the plan's digest changed,
   then:
   - deletes the copy on every backend and verifies the deletion;
   - for a whole artefact, deletes the OCFL object and leaves a tombstone:
     ULID, sphere, date of erasure, nothing else;
   - for a version, rewrites the object without it (§8.1);
   - removes the index rows and the events that name the erased content.

The proof lists each location purged and its verification. It names no file
and no title.

**Nested confirmation.** A tool that erases its own data together with
artefacts (mnemo's `forget --erase`) asks the user only once:

1. the tool releases its holds, calls `erase --plan` and folds the plan, its
   digest and the code into its own plan;
2. the user confirms the tool's plan once;
3. the tool calls `erase --confirm <code>` and folds the proof into its own
   result.

This requires a token with the `erase` right (§12). The plan's digest
depends only on the artefacts, versions and locations involved, so it does
not change between the two steps unless the store does. The tool aligns its
own code's validity on `erase.code_ttl`.

**Holders and erasure.** An artefact still held by another `(by, ref)` is not
erased, even with `--force` given by a holder: the tool releases its own
reference and leaves the artefact. `--force` is reserved to a human at the
CLI or the TUI.

Not covered, and listed in the result: backups made outside `artefact`
(Time Machine), provider trash and version history (Drive, Dropbox keep
deleted files for a while; `artefact` empties them when the API allows),
copies made by holders.

- The code has the form `XXXX-XXXX`. The pending plan is kept in
  `.artefact/erase/`, under the SHA-256 of its code: the code itself is
  never written to disk. A code is consumed by its first use, valid or not.
- Tombstones are kept in `.artefact/tombstones-<sphere>.jsonl`.
- `--force` exists on the CLI and in the TUI only; the MCP and REST surfaces
  do not offer it.

### 8.1 Rewriting an object

OCFL objects are append-only. Erasing a version breaks that rule on purpose,
and so do `seal` and `redact --purge`:

1. `artefact` builds the new object in the storage root's
   `extensions/artefact-staging/`, hard-linking the content files it keeps;
2. it moves the old object to `trash-<n>` in the same area, with a note of
   its place, and renames the new object into that place;
3. it deletes the trash. Files that only the old object held are gone.

An interruption between the two renames of step 2 is undone at the next
write to the sphere: the old object goes back to its place. The object stays
valid OCFL; its history no longer contains the erased versions. Events of
erased versions keep their place in the log without a version number; the
others are renumbered.

Deleting a file on APFS does not overwrite its blocks, and local snapshots
may keep it: the result of `seal` and `erase` lists those copies as not
covered.

## 9. Classification

### 9.1 Plans

A plan is a YAML file in `.artefact/plans/`, exportable to SKOS.

```yaml
id: household
title: Household records
sphere: perso
nodes:
  - notation: "3"
    label: Health
    scope: Medical records, prescriptions, insurance statements and claims.
    retention: { keep: 10y, from: date, then: review }
    children:
      - notation: "3.2"
        label: Insurance
        scope: Health insurance policies, statements, reimbursement claims.
        retention: { keep: 5y, from: date, then: destroy }
```

- `scope` defines the node. It is also the instruction given to a decider,
  so it is written for a reader who knows nothing else.
- `retention` is inherited by children unless they set their own. `then` is
  `keep`, `review` or `destroy`.
- A plan belongs to one sphere. An artefact may belong to several plans: each
  plan is a facet, recorded under `classes.<plan>` in the card.

### 9.2 Deciders

A decider answers: to which node of this plan does this artefact belong?

```go
type Decider interface {
    Decide(ctx, Question) (Answer, error)
}
// Question: the artefact's card, an excerpt of its text rendition,
//           the candidate nodes (notation, label, scope).
// Answer:   the chosen node or none, a probability per candidate,
//           a confidence.
```

| Decider | Through | Notes |
|---|---|---|
| `openjev` | local HTTP, `/v1/systemone` | default; typed `choice` question; text only |
| `jev` | TypeSafe API, same request format | hosted; sends content to a third party |
| `acp` | an ACP agent (herdr-acp, …) | general LLM with structured output; reads images |
| `rule` | rules in the plan (MIME type, file name, card fields) | deterministic, tried first when present |

- Classification walks the plan one level at a time: one question per level,
  among the children of the chosen node, with a `none` option.
- Below `classify.threshold` (0.8 by default), the walk stops at the parent
  node and the artefact is queued for a human (`ls --class-pending`).
- A human decision (`card <id> --set classes.<plan>.node=…`) always wins and
  is never overwritten by a decider.
- A decider never receives a sealed artefact's content, only its card.
- The probabilities are kept in the `classify` event.

### 9.3 Inputs

A decider reads the text rendition (§10). Without one, it reads the card
only.

### 9.4 Retention and disposal

- An artefact's retention is its class's rule; with several classes, the
  longest keep wins.
- A hold suspends any disposal.
- `review` lists artefacts whose retention has ended, with the rule that
  applies. `destroy` is never automatic: it goes through `erase`, with its
  plan and code.

## 10. Renditions

A rendition is a file derived from a payload: its text as Markdown, a
thumbnail. Text renditions come from `lector`, the converter designed in its
own project (any file to text, for agents).

- `artefact` asks `lector` for the text of each new version, in the
  background, and stores the result in the same OCFL version under
  `renditions/`. `render <id>` asks again, for example after `lector` learns
  a new format.
- `put --render text` waits for the rendition up to `--render-timeout` (30 s
  by default) and returns it with the result. Past the timeout, the result
  carries `rendition: pending`, and `render <id> --wait` or
  `get <id> --rendition text` delivers it later. Without `--render`, `put`
  returns at once.
- A rendition is erased with its version.
- A sealed artefact's renditions are sealed. `seal` erases the renditions of
  the clear versions along with them.

## 11. Local server

`artefact serve --http 127.0.0.1:7780` (or `--socket <path>`):

| Path | Serves |
|---|---|
| `GET /<sphere>/a/<id>[/vN]` | payload, with `Content-Type`, `ETag` = digest, ranges |
| `GET /<sphere>/a/<id>/card` | the card, JSON |
| `GET /<sphere>/a/<id>/renditions/<name>` | a rendition |
| `GET /<sphere>/h/sha256/<hex>` | the payload with this digest |
| `POST /v1/<action>` | every remote action, same parameters and envelope; a JSON body carries the parameters, and for `put` a non-JSON body is the payload with the parameters in the query |
| `/mcp` | MCP Streamable HTTP |

- Every request carries a token (§12), except from the local CLI through the
  Unix socket.

### 11.1 File views

The same tree is served two ways, read-only, so that the Finder, Obsidian
and any application open artefacts as ordinary files. Views are never the
contract: deposits, seals and erasures go through the actions.

| View | Server | Mounted by | Use |
|---|---|---|---|
| NFS | NFSv3 in the binary (`go-nfs`), on `127.0.0.1` | macOS native client (`mount_nfs`), at any mount point | fast file access for apps and agents, paths in Markdown |
| WebDAV | `/dav/` on the HTTP server | Finder, browsers, remote clients | browsing, URLs |

Tree of one sphere's view (the NFS mount root, or `/dav/<sphere>/`):

```
a/<ulid><ext>                                   # current version: the stable path to cite
a/<ulid>/v<N><ext>                              # fixed versions
a/<ulid>/renditions/text.md                     # phase 7
plans/<plan>/<notation> <label>/<title><ext>    # phase 8
```

- `<ext>` is the card's `ext`: the lower-cased extension of the deposited
  file name, fixed at deposit. Neither `card --set filename` nor `redact`
  changes it; a path in the view stays valid for the artefact's life.
- A sealed artefact never appears in a view. It is read only through
  `get --reveal`.
- `artefact serve` exports one NFS server per sphere (`views.nfs`, default
  every sphere; `views.disable_nfs` turns them off), each on its own
  loopback port, published in `.artefact/nfs.json`. A mount therefore never
  shows another sphere.
- `artefact mount <dir> --sphere <s>` mounts a view as the user, without
  administrator rights, with the options `vers=3,tcp,nolocks,rdonly,soft,
  intr,timeo=10,retrans=2,nobrowse`: `soft` keeps readers from hanging when
  serve stops. `artefact unmount <dir>` undoes it.
- serve mounts the directories of `views.mounts` (`{perso: ~/mnemo/perso/
  _artefacts}`) once it listens, and unmounts them when it stops.
- NFSv3 trusts the address and the uid: the NFS servers listen on
  `127.0.0.1` only, and any local user of the Mac can mount them. The views
  hold no sealed artefact.
- The WebDAV view answers `GET`, `HEAD`, `OPTIONS` and `PROPFIND`, and
  refuses every write (405). The Finder sends the token as the password of
  Basic authentication; any user name is accepted.
- A later phase adds a write-only `inbox/` folder to each view: a file
  dropped there becomes a `put`.

### 11.2 The `artefact:` scheme

A small macOS companion app, `Artefact.app` (`app/`, bundle id
`aero.clement.artefact`, menu-bar-less), registers the `artefact:` URL
scheme. Opening `artefact://perso/<ulid>` runs `artefact locate`, which
returns the artefact's path in a mounted view, or else a copy (mode 0600)
under `~/Library/Caches/artefact/open/<id>/`, and opens that path in the
default application. A sealed artefact is refused with a message.

`app/scripts/build-app.sh` builds and signs the app; `--install` copies it to
`/Applications` and registers the scheme. The app finds the CLI through
`$ARTEFACT_BIN`, then `~/.local/bin`, `~/go/bin`, `/opt/homebrew/bin` and
`/usr/local/bin`. It is the only part of `artefact` written in Swift.

## 12. Spheres and access

| Surface | Granted spheres |
|---|---|
| local CLI | `spheres` from the configuration, default `default_sphere` |
| MCP stdio | `artefact mcp --spheres <list>` |
| REST and MCP HTTP | the token's spheres |

- A token carries spheres and rights: `read`, `write`, `reveal`, `erase`.
  `artefact token create --name mnemo-perso --spheres perso --rights
  read,write,reveal`. It is shown once and stored as a SHA-256 hash in
  `.artefact/tokens.json`.
- On the Unix socket (mode 0600, so only the user's processes reach it), a
  request without a token has every sphere and right, like the local CLI; a
  request with a token is limited to the token. Over TCP, a token is
  required. `X-Artefact-Agent` names the caller in the event logs.
- `artefact mcp` grants `--rights read,write` by default; `reveal` and
  `erase` are given only when named.
- Each action declares the right it needs (`schema` shows it as `right`).
  Actions without one (`init`, `key`, `token`, `serve`, `mcp`, `reindex`)
  stay on the local CLI.
- Local paths (`put <file>`, `get --out`, `redact --from`) are accepted on
  the Unix socket and MCP stdio only. Over TCP, payloads travel in the
  request or the response.
- An id of an ungranted sphere is reported as missing; a URI of an
  ungranted sphere is refused explicitly.

## 13. TUI

`artefact tui`, Go and Bubble Tea v2, keys aligned on office's TUI.

- List: artefacts of a sphere, filters by plan node, holder, orphaned,
  sealed, below `numcopies`.
- Detail: card, versions, renditions, locations and fixity, events.
- Classification queue: artefacts below the threshold, with the decider's
  candidates; one key accepts, another picks a node.
- Review: dispositions due.
- Actions: open the payload, edit the card in `$EDITOR`, classify, sync,
  erase (plan shown, code typed).

## 14. Contract with mnemo

mnemo keeps its originals in `<sphere>/_originals/<aa>/<sha256>.<ext>` inside
its jj repository. With `artefact`, it keeps none.

| mnemo today | With artefact |
|---|---|
| copy into `_originals/` | `put --sphere <s> --hold mnemo:<concept ULID>` |
| `sources[].resource: /perso/_originals/…` | `sources[].resource: artefact://perso/<id>` |
| dedup by SHA-256 in the sphere | `put` returns `known`, mnemo adds its hold |
| `RevealOriginal`, `mnemo://…?reveal=1` | `get --reveal`, through a token with `reveal` |
| `--secret` deposit: original in `.age` | `put --seal` |
| seal after ingestion (`purge.go`) | `seal <id>`: no jj rewrite for files |
| `forget --erase` of an original | `release`, then `erase` of the artefact if no other holder |
| text redaction (`erase.go`) | `redact <id> --from <redacted file> --purge` |
| text extraction in `remember.go` | `lector`, kept as rendition |

**Transport and access.** mnemo uses the Go client over the Unix socket. It
serves several spheres in one process, so its token carries those spheres and
the rights `read`, `write`, `reveal` and `erase`.

**Ingestion.** mnemo's agents open originals with their own tools. Before a
task, mnemo writes each original with `get <id> --out <task dir>/…` (with
`--reveal` for a sealed one) and deletes the files at the end of the task.
The text that mnemo writes in an entry's `# Text` section comes from
`put --render text`, or later from `get <id> --rendition text`.

**Holders.** One original is often cited by several concepts. At the end of
each task, mnemo compares each concept's `sources` before and after, and
turns the difference into `hold` and `release` calls, with the concept's ULID
as `ref`. `mnemo lint` audits both directions in one batch: every
`artefact://` source has its hold (`stat <id>...`), and every hold has a
concept that cites the artefact (`ls --held-by mnemo`).

**Links.** `sources[].resource` carries the URI `artefact://<sphere>/<id>`.
In bodies, links and embedded images carry the path in the NFS view, mounted
inside the bundle at `<sphere>/_artefacts/` and ignored by jj:
`![scan](/perso/_artefacts/a/<id>.png)`. Obsidian opens them as ordinary
files. The linter treats `artefact://` sources as checked by `stat`, and
`_artefacts/` paths as resolved when the id exists.

**Duplicates.** mnemo keeps no file digest of its own. A `put` that returns
`known` also returns the holders, from which mnemo finds the existing
concept.

**Sealing.** `seal <id>` replaces the jj rewrite for files. mnemo's jj
history keeps no original file, but it keeps extracted text (an entry's
`# Text`, earlier versions of a concept): mnemo still rewrites its own
history for that text.

**Erasure.** `forget --erase` and `cancel --erase` use the nested
confirmation of §8: one plan, one code for the user. When a binary original
carries an erased passage, mnemo releases its reference and asks `erase`; if
another concept still holds it, the artefact stays and mnemo only removes its
source.

**Availability.** When `artefact` does not answer, `remember` keeps the
deposit in `.mnemo/received/` and replays the `put` later, with the
concept's ULID as `--key`.

**Keys.** mnemo keeps its own age key for inline passages and sealed bodies.
`artefact` has one key per sphere for files. A file that mnemo holds sealed
is decrypted by mnemo and passed to `put --seal -`, so the clear text exists
only in memory.

**Migration of `_originals/`**, run by mnemo, sphere by sphere, after its
owner confirms:

1. rehearse on an APFS clone of the bundle;
2. stop `mnemo serve`;
3. `put` each original with each citing concept as holder (sealed originals
   through `put --seal -`), and rewrite `sources`;
4. remove `*/_originals/**` (and the former `*/_originaux/**`) from the jj
   history, with mnemo's history rewriting;
5. check with `mnemo lint`, then restart `serve`.

**Small changes in mnemo.** `mnemo stats` reads counts and sizes from
`artefact du --sphere`; the bench's `source:` lookup uses the card's
`filename`; the MCP resource `mnemo://<sphere>/_originals/…?reveal=1`
becomes a relay to `get`.

## 15. Configuration

`~/.config/artefact/config.yaml`:

```yaml
store: ~/artefact
spheres: [perso, pro]
default_sphere: perso
keys:
  perso: age1…            # private key in the keychain
  pro:   age1…
backends:
  local:        { type: local, path: ~/artefact, spheres: [perso, pro] }
  gdrive-perso: { type: rclone, remote: "gdrive-perso:artefact", sphere: perso }
  icloud-perso: { type: rclone, remote: "icloud:artefact", sphere: perso, trusted: false }
replication:
  perso: { numcopies: 2, mincopies: 1, backends: [local, gdrive-perso] }
classify:
  decider: openjev
  threshold: 0.8
  deciders:
    openjev: { url: http://127.0.0.1:3000 }
    acp:     { command: [herdr-acp, --agent, claude] }
lector: { command: [lector] }
serve: { http: 127.0.0.1:7780, socket: ~/.local/state/artefact/sock }
```

## 16. Phases

1. **Core**: OCFL store, card, events, index, `put` `get` `stat` `ls`
   `card`, holders, CLI, `schema`, skill.
2. **Secrets and erasure**: keys per sphere, `seal`, `redact`, `erase` with
   plan, code and proof.
3. **Server and client**: `serve` (REST, Unix socket, tokens), Go client,
   MCP stdio and HTTP.
4. **File views**: NFS mount and WebDAV view, read-only; `Artefact.app` for
   the `artefact:` scheme.
5. **mnemo**: client in mnemo, migration of `_originals/`. mnemo keeps its
   own text extraction until phase 7.
6. **Backends and replication**: rclone connector, `sync`, `fsck`.
7. **Renditions**: `lector` integration, `put --render`.
8. **Classification**: plans, deciders (`rule`, `openjev`, `acp`), queue,
   `review`.
9. **TUI**, then the `inbox/` folder of the views.

## 17. References

- OCFL 1.1: <https://ocfl.io/1.1/spec/>; storage layout extension 0004
  (hashed n-tuple).
- PREMIS: <https://www.loc.gov/standards/premis/>
- RFC 6920, Naming Things with Hashes.
- Perkeep permanodes: <https://perkeep.org/doc/schema/permanode>
- git-annex `numcopies`, `mincopies`.
- rclone: <https://rclone.org/>, Go package `github.com/rclone/rclone/fs`.
- age: <https://age-encryption.org/>
- MoReq2010 (classification, disposal); SKOS: <https://www.w3.org/TR/skos-reference/>
- Jev: <https://docs.typesafe.ai/>; OpenJev: <https://huggingface.co/openjev/openjev>
- AI-native CLI: `aclemen1/ai-native-cli`; ACP: `agentclientprotocol`.
