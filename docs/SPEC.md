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
      extensions/artefact/card.yaml       # mutable card (§3.2)
      extensions/artefact/events.jsonl    # event log (§3.3)
      extensions/artefact/renditions/v2.md  # text of the current version, from lector (§10)
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
| `put <file\|-> [--sphere] [--title] [--date] [--seal] [--hold <by>:<ref>] [--card <yaml>] [--key <key>] [--render]` | deposits; idempotent within the sphere; returns `{id, version, status: created\|known, holders, render}` |
| `get <id> [--version vN] [--reveal] [--out <path>] [--rendition text]` | writes the payload or the text rendition |
| `stat <id>...` | card, versions, holders, renditions, locations and their last fixity check; one or many ids |
| `which <file\|->  [--sphere]` | without writing anything, the artefact that holds exactly this content: by SHA-256 among clear artefacts, by the sphere's keyed digest among sealed ones; `found: false` otherwise |
| `search <query> [--sphere] [--state] [--sort <key>[:asc\|desc]] [--meaning auto\|on\|off]` | search with a Google-like grammar, ranked by words and by meaning (§5.1); returns `{query, corrected, semantic, notes, hits[{…, snippet, via}]}` |
| `revise <id> --from <file>` | saves new content as the next version; earlier versions are kept; a sealed artefact stays sealed |
| `meta read\|write <id>...` | read: the file's own metadata (EXIF, XMP, PDF, Office, through exiftool) into the card's `embedded` block, and the content's date when the card still has the deposit date; write: the card's title, date, creator, description and tags into the file as XMP, as a new version. A sealed artefact's own metadata never enters its card |
| `card <id> --tag <t> --untag <t>`, `tags`, `ls --tag <t>` | keywords: in the card, searchable and filterable |
| `archive <id>...`, `unarchive <id>...` | the archived state: kept, replicated, reachable by id and view path, hidden from lists |
| `ls [--sphere] [--class <plan>:<node>] [--held-by <tool>[:<ref>]] [--orphaned] [--sealed] [--since] [--sort <key>[:asc\|desc]]` | lists artefacts, newest deposit first unless `--sort` |

### 5.1 Search

The grammar follows Google's. Plain words match the start of words, accents
and case ignored; the title and the file name weigh more than the card and
the text (of a text payload, or of its text rendition).

| Form | Matches |
|---|---|
| `"bail à loyer"` | these words, whole, in this order |
| `-brouillon`, `-"two words"` | excludes |
| `assura OR css` | either |
| `loyer AROUND(5) garantie` | both within 5 words |
| `title:` `file:` `text:` `card:` | the word or `"phrase"` in this field |
| `type:pdf\|image\|text\|audio\|video\|<ext>\|<media type>` | the kind of file |
| `class:4.2`, `class:perso:4.2`, `class:banque` | in the class or below; a word of a label finds its classes |
| `plan:` `tag:` `-tag:` `sphere:` | plan, tag, sphere |
| `after:` `before:` `year:` `date:` | the content's date: `YYYY`, `YYYY-MM` or `YYYY-MM-DD` |
| `is:` / `-is:` `sealed archived active pending orphaned classified unclassified` | state |

- Two rankings merge by reciprocal rank fusion: the words (bm25), and the
  meaning when the sphere has an index by meaning. Each hit says `via`:
  `words`, `meaning` or `words+meaning`.
- Phrases, fields, `OR`, `AROUND`, exclusions and filters are strict: a text
  found by meaning must satisfy them. Plain words rank without excluding.
- A plain word of 5 letters or more that matches nothing is replaced by the
  closest indexed word (edit distance 1, or 2 from 8 letters); `corrected`
  shows the query searched.
- Filters alone list what passes them, newest deposit first.
- The index by meaning is qmd's: one index `artefact-<sphere>`
  (`~/.config/qmd/artefact-<sphere>.yml`) over the text renditions,
  built by `reindex --semantic`, kept up to date by `serve` after each
  render pass. A sealed artefact has no rendition, so meaning never finds it.
  `search.semantic: none` turns it off; `search.qmd` overrides the command.

Sort keys: `relevance` (search only), `date` (of the content), `deposited`,
`updated`, `title`, `size`. Without a direction, dates, sizes and relevance
put the newest, largest and most relevant first, and titles go from A to Z.
Ties fall back to the newest deposit. `--limit` applies after the sort, across
spheres. In the TUI, `t` moves to the next key and `T` reverses the order.
| `render <id>... \| --missing [--sphere] [--limit] [--force]` | asks `lector` for the text rendition (§10) |
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

### 6.1 Connector

A backend is an rclone remote path, owned by one sphere:

```yaml
backends:
  gdrive-perso: { remote: "gdrive-perso:artefact", sphere: perso }
  disk:         { remote: ":local:/Volumes/Backup/artefact", sphere: perso, trusted: false }
```

- artefact embeds rclone as a library (`fs`, `operations`, `walk`), with the
  `drive` and `local` backends; more are one import away. Its rclone
  configuration lives apart, in `~/.config/artefact/rclone.conf` (mode 0600),
  or `rclone_config`.
- The operations used are put (`operations.Rcat`), stat with the provider's
  SHA-256 when it gives one (Drive does), open, delete, purge and recursive
  list.
- A sphere's files go under `<remote>/<sphere>/`, mirroring its storage root
  file for file: each backend holds a valid OCFL storage root, which can be
  copied back as it is.
- `trusted: false` marks a backend whose copies do not count towards
  `numcopies` and `mincopies`. iCloud Drive belongs there: macOS may evict a
  file from the disk at any time, and the rclone connector is experimental.
- `artefact backend add <name> --sphere <s> --type drive` runs Google
  Drive's consent in the browser, with the scope `drive.file` (the remote
  sees only the files artefact creates) and `use_trash: false` (deletions
  are final). `--remote <path>` takes any rclone remote path as it is.
- `artefact backend reconnect <name> --client-id … --client-secret …` sets
  up a Drive remote again with the user's own OAuth client (rclone's shared
  client is rate-limited and retired in 2026). Drive limits file creation
  to about one per second and per account: a first copy of 10,000 files
  takes about three hours.

### 6.2 What travels

- Every file of the storage root except the staging area: namaste, layout,
  inventories, content, card and event log.
- A sealed artefact's payload is already ciphertext. Its card and event log
  are encrypted with the sphere's key on the way and stored as `.age`, so a
  backend never sees the title or file name of a sealed artefact.
- A clear artefact travels in clear: a backend can read it. Sealing is the
  way to keep a file from a backend.

A Drive backend may name its Google account (`backends.<name>.account`).
`backend reconnect` asks Google which account the consent authorized
(`about.user`, allowed by the scope `drive.file`) and drops the token when it
is another one; every sync checks it again before copying anything. A sphere
never reaches a Drive of the wrong account, even after a consent given in a
browser logged into it.

## 7. Replication

```yaml
replication:
  perso: { numcopies: 2, mincopies: 1, backends: [gdrive-perso], every: 15m }
```

- The local store is one copy. `numcopies` is the number of copies wanted;
  `mincopies` the number that must exist before a copy is dropped.
- The index records every file sent: backend, local path, remote path, the
  SHA-256 of the local file and of what was sent, its size, when it was sent
  and last verified. Local SHA-256 are cached by size and modification time.
- `artefact sync [--sphere] [--dry-run]` sends what a backend lacks or holds
  in an older state, and removes from it what the store no longer holds.
  `serve` runs it at start and then every `every` (15m by default).
- `seal`, `redact --purge` and `erase` sync their object at once: no erased
  version outlives the operation on a backend that answers. A backend that
  fails is listed in the result and caught up by the next sync.
- `artefact fsck [--sphere] [--sample N] [--deep]` verifies local objects
  against their inventories, and copies against their recorded size and
  SHA-256 (downloaded with `--deep` when the backend gives none). A bad copy
  is dropped from the record; the next sync sends it again.
- `stat` shows each artefact's copies (complete, missing files, last
  verification); `backend ls` shows, per sphere, the artefacts below
  `numcopies` and `mincopies`.
- Removing a backend from `replication` leaves its copies in place.

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
(Time Machine), version history kept by a provider, copies made by
holders. Drive remotes made by `backend add` delete without the trash.

The plan lists one location per backend that holds files of the artefact;
confirmation purges each one, checks that nothing of the object is left
there, and reports each backend in the proof.

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

A decider answers: which child of this node does the artefact belong to?

| Decider | Through | Notes |
|---|---|---|
| `rule` | the plan's `rules` (`mime` glob, `filename` and `title` regular expressions) | tried first whatever the decider; the deepest matching node wins |
| `openjev` | `POST <url>/v1/systemone`, one `choice` question whose `criteria` are the children (label: scope) and `none` | default; `classify.deciders.openjev.url`; with `$JEV_API_KEY`, the same request reaches the hosted Jev |
| `acp` | one prompt in a new ACP session (`classify.deciders.acp.command`, e.g. `[herdr-acp, --agent, claude]`) | the agent may read the file (read permissions only); it answers one line of JSON |

- What a decider sees (`Facts`): file name, title, type, date, the card's
  title, date, `dc` and `x`, the first 4,000 bytes of a text payload, and
  for `acp` the path of the payload. Of a sealed artefact, the card only.
- The walk asks one question per level, among the children of the node
  chosen so far. It stops:
  - at a leaf;
  - on `none`: the artefact belongs to the current node (pending when no
    node was chosen yet);
  - below `classify.threshold` (0.8): the class stays at the current node,
    marked `pending`, for a human (`artefact ls --pending`).
- `classify <id> --plan <p> --node <notation>` records a human decision
  (`by: human:<user>`). A decider never overwrites it.
- Every decision appends a `classify` event with the steps: level, choice,
  confidence, and the five best probabilities.
- `artefact ls --class <plan>[:<notation>]` lists a class and its
  descendants.

### 9.3 Plans on disk

`artefact plan add <file.yaml> [--replace]`, `plan ls`, `plan show`,
`plan rm`, `plan export` (SKOS in Turtle, with `art:keep` and `art:then`).
Plans live in `.artefact/plans/<id>.yaml` of the store. A plan's notations
are unique; `none` is reserved.

### 9.4 Retention and disposal

- An artefact's retention is its node's rule, or else the nearest
  ancestor's. `keep` is `10y`, `6m`, `30d` or `forever`, counted from the
  content's `date` or, with `from: deposit`, from the deposit.
- With several plans, the longest keep wins; `forever` or `then: keep`
  makes an artefact never due.
- A hold (`holds` in the card) marks a due artefact `held`: its disposal is
  suspended.
- `artefact review [--plan] [--due-before]` lists what is due, with the
  rule and the node that carries it. `destroy` is never automatic: it goes
  through `erase`, with its plan and code.

## 10. Renditions

A rendition is a file derived from a payload: its text as Markdown, a
thumbnail. Text renditions come from `lector`, the converter designed in its
own project (any file to text, for agents).

- A text rendition is derived data, not content: it lives in
  `extensions/artefact/renditions/v<N>.md`, outside the OCFL versions, so a
  version stays as it was written. The card's `rendition` records the
  version, date, length, confidence and `lector` methods, or the error.
- Only the current version has a rendition. `revise`, `redact` and `seal`
  drop it; the next render makes the new one.
- `artefact render <id>...` runs `lector read <payload> --max-chars 200000`.
  A rendition of the current version is kept unless `--force`.
  `render --missing [--sphere s] [--limit n]` fills the gaps; one failure
  does not stop the batch.
- `put --render` renders before answering and returns the rendition with
  the result.
- `serve` renders the missing renditions every 5 minutes, 50 per sphere at a
  time, unless `lector.auto: false`. `lector.command` overrides the command.
- `get <id> --rendition text` returns the text.
- The full-text index and the classification deciders read the rendition
  first, then the payload of a textual file.
- A sealed artefact never reaches `lector` and has no rendition.

## 11. Local server

`artefact serve --http 127.0.0.1:7780` (or `--socket <path>`):

| Path | Serves |
|---|---|
| `GET /<sphere>/a/<id>[/vN]` | payload, with `Content-Type`, `ETag` = digest, ranges |
| `GET /<sphere>/a/<id>/card` | the card, JSON |
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
plans/<plan>/<notation> <label>/…/<title><ext> # by class
plans/<plan>/_unclassified/<title><ext>         # not placed yet, or pending at the top
```

- `plans/` files every active, unsealed artefact under its title (with
  `/` and `:` replaced by `-`, and the extension added when the title lacks
  it), in the folder of its node; two equal titles get the end of their id.
  A path in `plans/` changes when the artefact is classified again: cite
  `a/<id><ext>`, which never changes.

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
- serve mounts the directories of `views.mounts` once it listens, and
  unmounts them when it stops. A sphere takes one directory or a list:
  `{perso: [~/mnemo/perso/_artefacts, ~/artefacts], pro: ~/mnemo/pro/_artefacts}`.
  A directory still mounted by an earlier serve is unmounted and mounted
  again.
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

There is no default sphere. The sphere of a deposit follows the nature of
its content (a UNISIS document goes to pro, whatever office deposits it):
`put` (CLI, REST, MCP), `token create` and `mount` refuse to run without it.
Reads run over every granted sphere unless one is named.

| Surface | Granted spheres |
|---|---|
| local CLI | `spheres` from the configuration; every write names its sphere |
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

`artefact tui [--spheres <list>]`, Go and Bubble Tea v2, with the palette and
keys of office's and mnemo's TUIs. It works on the store directly, like the
CLI, within the spheres given.

Keys follow the common convention of the ecosystem's TUIs.

| Scope | Keys |
|---|---|
| move, views | `j`/`k`, `gg`/`G`, `ctrl+d`/`ctrl+u`; `enter`/`l`/`tab` detail; `esc`/`h` back (never quits); `1` list, `2` plans (`C`), `3` pending (`P`), `4` archives (`A`), `5` orphaned (`O`); `s` sphere; `t`/`T` sort; `/` search; `r` refresh; `R` sync now; `?` help; `q` quit |
| the selected artefact | `o` open read-only, `f` show in the Finder, `E` edit the card, `M` edit the content (next version), `m` place in a class, `e`/`x` archive or make active (reversible), `S` seal, `#` erase (plan and code), `X` erase a held artefact |
| new | `c` deposit a file: its path, then its sphere when none is in view |
| plans | `l` open a class, `h` close it or go up, `enter` list a class or show an artefact |
| detail | `j`/`k`, `J`/`K` scroll |

- Every input but the filter (`/`) opens in a centred modal (the shared
  `github.com/aclemen1/tuikit`), which takes every key: `c` a form (file,
  sphere without preselection, title, date), `E` the card as a form (title,
  date, file name, type, tags, Dublin Core and extensions as YAML), yes/no
  lists for seal, decrypt and new version, the code to type for an erasure.
  `ctrl+s` validates, `esc` cancels (asking first when something changed).
- The TUI watches its own binary (time and inode, every 3 s). After a
  rebuild it execs the new binary when at rest (no prompt, no card or content
  being edited, no search running), carrying over the view, the selection,
  the filter or search, P/A/O, the class filter and the open detail, and says
  « reloaded <version>»; otherwise a badge waits for rest. SIGUSR1 asks for
  the same. The header shows the version and the commit it was built from.
- As in every TUI of the ecosystem, `esc` goes back to the previous view
  (from a search or a class to the whole list) and never quits; `q` quits
  from any view, except in a field, where it is typed.
- The list shows the last 6 characters of each id (the first ones are a
  timestamp shared by artefacts deposited together); the detail shows it whole.
- `/` then enter searches in two steps: by words, shown at once, then by
  meaning when a sphere has its index; a spinner in the status line names the
  step that runs.
- `t` cycles the sort (deposited, date, updated, title, size; relevance after
  a search) and `T` reverses it.
- The sort, the sphere and the classes open in the plans tree are kept in
  `~/.local/state/artefact/tui.json` from one session to the next.
- `o` opens through `locate`: from a mounted view, else from a cached copy.
- `E` opens the editable fields (title, date, filename, mime, dc, x) as YAML;
  what changed is saved with one `card` event.
- `#` shows the erasure plan and asks for its code; any other input cancels.
  `X` plans with `--force`, the human's override for a held artefact.

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
`put --render`, or later from `get <id> --rendition text`.

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
9. **TUI** (done ahead of phases 5 to 8), then the `inbox/` folder of the
   views.

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
