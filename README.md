# go-toolkit

Shared Go packages for the HTTP services in this stack: typed
configuration, a SQL layer that speaks both MySQL and SQL Server,
spreadsheet reading and import, upload handling, and the request and
response shapes every service uses.

Everything here, `examples/` aside, is a library. Nothing starts a server
or reads a config file, and the only process-wide state is the handful of
startup settings listed under [Conventions](#conventions).

```
go get github.com/mas-ony/go-toolkit
```

## Requirements

- Go 1.27.1 or newer.
- MySQL / MariaDB, or SQL Server / Azure SQL, for `database`.
- No cgo. The SQLite store behind `whatsapp` is pure Go, built from
  Wasm, so there is no gcc at build time.

## Packages

| Package      | What it does                                       |
| ------------ | -------------------------------------------------- |
| `config`     | Typed `config.yaml` sections, validated at start   |
| `database`   | sqlx setup, MySQL/T-SQL grammar, clause builders   |
| `datetime`   | Time type that parses many formats, JSON and SQL   |
| `document`   | One-pass index of a document directory             |
| `email`      | Sends mail with attachments through an SMTP relay  |
| `fileutil`   | Upload paths, safe names, type allowlist, writes   |
| `httpclient` | JSON client with retries and typed errors          |
| `importer`   | Drives one spreadsheet import, row by row          |
| `logger`     | zerolog configured per environment                 |
| `request`    | Fiber route and query parameter helpers            |
| `response`   | The JSON envelope every handler returns            |
| `whatsapp`   | Pairs a device and sends text or documents         |
| `xlsx`       | Reads a workbook against an expected layout        |

Each package is one directory, and each carries a `doc.go` holding the
contract and the reasoning behind it. This file is the map; `doc.go` is
the detail.

## Config

`config` turns a `*viper.Viper` into typed sections, and builds that
Viper too, through `NewViper`, so the environment overrides below are
wired the same way in every service. Reading the file stays in the
application — where configuration lives is a deployment decision — so a
service can load from wherever it likes and still get the same validated
structs.

Sections, and the key prefix each is nested behind:

| Section      | Prefix                  | Covers                            |
| ------------ | ----------------------- | --------------------------------- |
| App          | `app`                   | Name, env, host, port, time zone  |
| Database     | `database`              | Driver, host, credentials, pool   |
| Notification | `notification`          | Channels in use                   |
| WhatsApp     | `notification.whatsapp` | Sync or async sending             |
| Email        | `notification.email`    | SMTP relay, TLS, sender, async    |
| Fiber        | `fiber`                 | Server and router settings        |
| Auth         | `fiber.auth`            | Auth mode                         |
| JWT          | `fiber.jwt`             | Secret and expiry                 |
| Client       | `fiber.client`          | Outbound base URL, token, retries |
| Limiter      | `fiber.limiter`         | Rate limit max, window, strategy  |
| Listen       | `fiber.listen`          | TLS, prefork, shutdown timeout    |
| Recover      | `fiber.recover`         | Stack traces on panic             |
| RequestID    | `fiber.requestid`       | Correlation header                |
| Session      | `fiber.session`         | Cookie flags and timeouts         |
| Zerolog      | `fiber.zerolog`         | Access log fields and levels      |

Every section implements `SectionConfig` — `Validate() error` and
`String() string` — so one list drives both the startup check and the
startup log line.

The Fiber sections embed the middleware's own config type, so each drops
straight into its constructor: `fiber.New(*cfg.Fiber.Config)`,
`limiter.New(*cfg.Limiter.Config)`. Two need a value only the application
holds — build the access logger from `cfg.Zerolog.WithLogger(log)` and
the recover middleware from `cfg.Recover.WithStackTraceHandler(h)`.
Skipping either is not an error: the middleware falls back to its own
stderr writer, and the service logs in two formats.

### Environment spelling

A key's environment name is the key uppercased with dots replaced by
underscores: `fiber.limiter.max` is `FIBER_LIMITER_MAX`, and the variable
overrides the file. `NewViper` wires that — the key replacer and
`AutomaticEnv` — and is the reason the package builds the instance: one
made with `viper.New()` ignores every variable, with no error to say so.
The constructor functions in this package are the authoritative list of
which keys exist at all.

### Only what a deployment supplies

`SuppliedSections` reports which sections some source actually supplied a
key for, reading both the file and the process environment. Swapping an
unsupplied section for `AbsentSection` spares it validation and keeps its
zero values out of the startup log, where `expiration=0s` reads as a
setting somebody chose rather than as no setting at all.

The environment half matches variable names by prefix, so a
`DATABASE_URL` exported for another tool marks `database` as supplied.
The section then fails validation naming the keys it wants, which is loud
rather than silent.

```go
v := config.NewViper()
v.SetConfigFile("config.yaml")
err := v.ReadInConfig()
if err != nil && !errors.Is(err, fs.ErrNotExist) {
    log.Fatal().Err(err).Msg("reading config.yaml")
}

sections := []config.Section{
    {Name: "app", Prefix: "app",
        Value: config.NewAppConfig(v)},
    {Name: "database", Prefix: "database",
        Value: config.NewDatabaseConfig(v)},
    {Name: "limiter", Prefix: "fiber.limiter",
        Value: config.NewLimiterConfig(v)},
    {Name: "email", Prefix: "notification.email",
        Value: config.NewEmailConfig(v)},
}

supplied := config.SuppliedSections(v, sections)
for i, s := range sections {
    if !supplied.Configured(s.Name) {
        sections[i].Value = config.AbsentSection{}
    }
}
for _, s := range sections {
    if err := s.Value.Validate(); err != nil {
        log.Fatal().Err(err).Str("section", s.Name).Send()
    }
    log.Info().Str("section", s.Name).
        Str("config", s.Value.String()).Send()
}
```

A missing `config.yaml` is tolerated, so a deployment can run from the
environment alone; one that will not parse is not.

A list value may be written as a YAML sequence or as a comma-separated
string, and both parse the same way. That is not cosmetic: Viper splits a
bare environment string on whitespace, so `FIBER_REQUEST_METHODS=GET,POST`
would otherwise register one method nobody sends and answer 405 to
everything.

### Example

[`examples/service`](examples/service) is a complete setup to copy into a
service:

- `config.yaml` sets every key the sections read, leaves the credentials
  to the environment, and comments every value that is not obvious.
- `internal/config/config.go` builds every section, validates the ones
  the deployment uses, and adds the checks that need two sections at
  once.
- `internal/config/config_test.go` pins each of those checks.

It is built and tested with the rest of the module, so a change that
breaks it fails `go test ./...` instead of leaving a stale example behind.

### Notification channels

`notification.channels` chooses the channels in use — `whatsapp`,
`email`, both, or neither — and each channel is configured by the section
named after it. Each carries its own `async`, because a WhatsApp document
upload and an SMTP conversation are different waits, and a deployment may
want one in the background and the other waited for.

```go
notif := config.NewNotificationConfig(v)

var mailer *email.Service  // nil: every send returns ErrNotConfigured
if notif.Enabled(config.ChannelEmail) {
    mailer, err = email.New(config.NewEmailConfig(v), log)
    if err != nil {
        return err  // chosen, so a missing section is fatal too
    }
}

var wa *whatsapp.Service  // nil: NOT safe to call, so guard each send
if notif.Enabled(config.ChannelWhatsApp) {
    wa, err = whatsapp.New(log)
    if err != nil {
        return err  // ErrNotPaired if the QR window closed
    }
}
```

Leaving a channel out of the list switches it off and keeps its section.
The two services differ when switched off: a nil email `*Service` answers
every send with `ErrNotConfigured`, while a nil WhatsApp one panics, so
WhatsApp sends sit behind `notif.Enabled(config.ChannelWhatsApp)`.

## Database

`database` opens the connection, picks the SQL grammar, qualifies table
names, and assembles the clauses a list endpoint needs. It uses
`jmoiron/sqlx`; everything it returns is a plain `*sqlx.DB` or `*sqlx.Tx`,
so a repository is free to ignore the rest of the package.

Both drivers are linked in regardless of which one the config selects:
`microsoft/go-mssqldb` for `sqlserver` and `mssql`, `go-sql-driver/mysql`
for `mysql`. The DSN never leaves the package — a driver that quotes the
connection string in a parse error would route the password to the log
aggregator, so open errors are replaced with `ErrMalformedDSN`.

```go
loc, err := time.LoadLocation(cfg.App.Location)
if err != nil {
    return err
}
time.Local = loc

db, err := database.New(cfg.Database, cfg.App)
if err != nil {
    return err
}

d, err := database.ParseDialect(cfg.Database.Driver)
if err != nil {
    return err
}
database.SetDialect(d)
database.Configure(cfg.Database.Namespace())
```

`New` takes the app section as well: `app.name` labels the session on
SQL Server, and `app.location` becomes the driver's time zone —
`timezone=` on SQL Server, `Loc` on MySQL. Set `time.Local` to the same
zone first, as above, because `datetime` resolves input without an offset
there, and two zones that disagree shift every timestamp by the
difference. `NewContext` is `New` with a context that can end the
startup ping early.

`SetDialect` and `Configure` are process-wide and belong at startup,
before the first repository is constructed. Until `SetDialect` runs,
every helper that needs the grammar panics rather than guess an engine.
`Configure` is a plain variable, so calling it while requests are in
flight is a data race; `SetDialect` is atomic, but a query built while it
changes can still mix both grammars.

### Two engines, one query

Every helper that depends on the grammar has a package-level form that
reads the selected one and a method form on `Dialect` that takes it as a
receiver. The package-level form suits a service that talks to one
engine; the method form is what lets a migration or a sync job spell SQL
for both in one process, and what lets a test cover both grammars without
touching global state.

|                   | MySQL                 | SQL Server                   |
| ----------------- | --------------------- | ---------------------------- |
| row cap           | `LIMIT n`             | `TOP (n)`                    |
| page              | `LIMIT … OFFSET …`    | `OFFSET … FETCH NEXT …`      |
| server clock      | `NOW()`               | `SYSDATETIME()`              |
| recursive CTE     | `WITH RECURSIVE x AS` | `WITH x AS`                  |
| quoted identifier | `` `name` ``          | `[name]`                     |
| generated key     | `LastInsertId()`      | `OUTPUT INSERTED.pk INTO @t` |
| LIKE escape       | `ESCAPE '!'`          | `ESCAPE '!'`                 |

`ApplyLimit`, `PageClause`, `NowExpr`, `RecursiveCTE`, `QuoteIdent`,
`InsertReturningID` and `Qualify` cover the rows that differ. The last
row agrees only because the escape is `!`: a backslash cannot be written
the same way on both, so `LikeArg` escapes a search term and
`LikePredicate` declares the matching `ESCAPE`. Anything else a query
builder writes has to be spelled identically on both.

A few more helpers keep a query portable where the obvious spelling is
not. `AsText` casts a numeric column for a `LIKE` search, which SQL
Server would otherwise fail by converting the pattern to a number.
`ExistsFlag` renders a 1-or-0 column from `EXISTS`, since T-SQL has no
boolean select-list value. `NeedsJoin` says when the requested columns
let a builder leave a `JOIN` out.

### Table names

Table references are qualified rather than left bare, because a bare name
resolves against the connection's default database and is right only by
coincidence. The prefix is rendered by whoever knows the driver — three
parts on SQL Server, two on MySQL — and `Qualify` joins it to the name,
quoting only when the name would not parse bare on both engines (`user`
is reserved in T-SQL, `rank` in MySQL).

```go
database.Qualify("app2026.dbo", "invoice")     // app2026.dbo.invoice
database.Qualify("app2026", "invoice")         // app2026.invoice
database.NamespaceForYear("app2026.dbo", 2025) // app2025.dbo
```

`NamespaceForYear` rewrites the database segment for a deployment that
keeps one database per year, `app2025` beside `app2026`, so a request can
read the year it asks for.

### Clause builders and allowlists

Client input never reaches the query text directly. `SelectClause`,
`SortClause` and `GroupClause` each take an allowlist, and a column not
in it is dropped rather than rejected — an unknown sort column yields the
fallback order, not a 400.

```go
allowed := map[string]string{
    "id":   "t.id",
    "code": "t.code",
    "name": "t.name",
}

page, limit := request.Page(c)
args := map[string]any{}

q := "SELECT " + database.SelectClause(
    request.Cols(c), allowed, []string{"code"}, "id",
) + "\nFROM " + database.Qualify(ns, "invoice") + " t"

total, err := database.CountQuery(ctx, db, q, args)
if err != nil {
    return err
}

q += database.SortClause(
    request.Sort(c), []string{"t.code", "t.name"}, "t.id",
)
q += database.PageClause(args, page, limit)

var rows []Invoice
err = database.SelectList(ctx, db, q, args, &rows)
```

`SelectClause` prepends the key column when the request leaves it out,
which is why `id` sits in the allowlist. Count before sorting and paging:
`CountQuery` wraps the query as a derived table, which SQL Server will
not accept with an `ORDER BY` inside, and a count over one page counts
only that page. The tail fragments — `GroupClause`, `SortClause` and
`PageClause` — each carry their own leading newline and keyword, so they
concatenate in order without any spacing logic at the call site.
`GetOne` returns `(found bool, err error)` rather than making the caller
match on `sql.ErrNoRows`.

### Transactions

```go
err := database.WithTx(db, func(tx *sqlx.Tx) error {
    if err := repoA.WithTx(tx).Delete(id); err != nil {
        return err
    }
    return repoB.WithTx(tx).Delete(id)
})
```

`WithTxContext` is the same with an explicit context and isolation level.
The context governs the whole transaction lifetime, not just the `BEGIN`
round trip, so a cancelled request releases its row locks immediately.

Neither nests. Calling one inside `fn` checks out a second connection,
which then waits on the locks the first still holds until the lock
timeout. Pass the `*sqlx.Tx` down instead, as above.

## HTTP

### request

Parameter helpers for Fiber v3 handlers.

```go
id, err := request.ID(c, "id")          // /items/:id, positive int
unit, err := request.QueryID(c, "unit") // ?unit=3, same rule as ID
ids := request.IDs(c, "ids")            // ?ids=1,2,3, junk skipped
page, limit := request.Page(c)          // ?page=2&limit=50
cols := request.Cols(c)                 // ?cols=code,name
sort := request.Sort(c)                 // ?sort=t.code:desc,t.id
name := request.StringPtr(c, "name")    // nil when absent or empty
year := request.IntPtr(c, "year")       // nil unless a positive int
paid := request.BoolPtr(c, "paid")      // nil unless true/false/yes/no/1/0
```

`ID` and `QueryID` are the two that can fail, each with a `*fiber.Error`
carrying 400 that the caller returns unchanged. The rest correct bad
input rather than reject it: a junk id is skipped, an unknown column is
dropped by the repository's allowlist, and a `limit` outside
`1..MaxLimit` becomes `DefaultLimit` — not the ceiling, so a client that
asked for too much notices.

`DefaultPage`, `DefaultLimit` and `MaxLimit` are package variables. A
service that serves wider pages assigns them at startup, before the first
request. Strings the helpers return are cloned, because they would
otherwise point into a request buffer the framework recycles under
keep-alive.

### response

One envelope for every response, success or failure.

```go
return c.JSON(response.OK("created", item))
return c.Status(404).JSON(response.Fail("invoice not found"))
return c.JSON(response.OKList("ok", items))
return c.JSON(response.OK("ok",
    response.NewPaginated(items, total, page, limit)))
```

A 2xx status always carries `success: true` and a non-2xx always carries
`success: false` — never HTTP 200 with a failure body, since clients
branch on the status and parsing the body is optional. `Message` must be
safe to show a user; internal error text is logged, not returned.

`OKList` and `NewPaginated` exist to dodge the typed-nil trap: a nil
slice assigned to `Data any` is a non-nil interface, so it would render
as `null` instead of `[]`.

### httpclient

A JSON client bound to one upstream service, built on the Fiber client
and configured from the `fiber.client` section.

```go
c, err := httpclient.New(cfg.Client, httpclient.Options{
    TrimPathSuffix: "/api/v1",
    Headers:        map[string]string{"X-App": "example"},
    Log:            log,
})

var out httpclient.Envelope[Invoice]
err = c.Do(ctx, fiber.MethodGet,
    c.URL("/invoices/42", nil), nil, "", &out)
```

A transport error or a 429, 500, 502, 503 or 504 is retried up to
`fiber.client.retries` more times, backing off from 500ms, doubling to a
ceiling of 8s, with no jitter. POST gets one attempt unless
`fiber.client.no_retry_methods` says otherwise, because a replayed create
whose first attempt landed writes a second row.

A non-2xx yields an `*httpclient.Error` carrying the status, the path and
whatever message the envelope held; `errors.Is` reaches `ErrNotFound`,
`ErrConflict` and `ErrUnprocessable`, the statuses worth branching on.
Error bodies are capped on the way out, and the message carried on the
error is capped shorter still.

## Spreadsheets

### xlsx

Reads a workbook against a layout the caller declares, over
`xuri/excelize`.

```go
cols := []xlsx.Column{
    {Col: "B", Captions: []string{"Code", "Kode"}, Purpose: "code"},
    {Col: "F", Captions: []string{"Amount", "Nilai"}, Purpose: "amount"},
    {Col: "H", Captions: []string{"Date", "Tanggal"}, Purpose: "date"},
}

wb, err := xlsx.Open(path, xlsx.Layout{
    Sheet:     "Data",
    HeaderRow: 6,   // GroupRow defaults to the row above
    FirstRow:  8,
})
if err != nil {
    return err
}
defer wb.Close()

if err := wb.VerifyHeaders(cols); err != nil {
    return err  // *xlsx.LayoutError, wrap it with your own advice
}

for _, row := range wb.DataRows(cols) {
    code := xlsx.Str(wb.Cell(row, "B"))
    amount, err := xlsx.Decimal(wb.Cell(row, "F"))
    date, err := xlsx.Date(wb.Cell(row, "H"), wb.Date1904())
}
```

A `Column` lists the caption spellings it accepts rather than one
canonical string, because variation in a hand-maintained header is real
and harmless — `(m2)` in one revision and `(m²)` in the next. Rejecting
those is how operators learn to bypass the check.

Coercion returns pointers. An empty cell is `nil`, which writes SQL NULL
rather than a zero that looks like data, and a cell that will not parse
is `nil` with an error quoting it as typed. `Link` resolves a cell's
hyperlink against the workbook's own directory, not the working
directory; `ResolveLink` does the same for a target and a directory the
caller supplies, and `ErrNotAFile` separates a link to a web page or
another cell from a broken one.

### importer

The half of a spreadsheet import that is the same every time: open, check
the header, parse, hand each row to whatever writes it. What a row means
and where it goes is a `Dataset`, implemented once per workbook shape.

```go
ds, ok := importer.Lookup(*dataset)
if !ok {
    return fmt.Errorf("unknown dataset %q", *dataset)
}
run := &importer.Run{
    Log: log, Opt: opt, Count: &importer.Counters{},
}
if err := ds.Connect(ctx, transport, run); err != nil {
    return err
}
outcomes, err := importer.Execute(ctx, ds, path, run)
if err != nil {
    return err
}
importer.PrintSummary(os.Stdout, path, ds, run, outcomes)
if err := importer.WriteReport(*report, ds, outcomes); err != nil {
    return err
}
```

`Execute` opens, checks, parses and imports; the command drives the rest
of the `Dataset` itself, in the interface's order. `Flags` and `Validate`
run around flag parsing, `Layout` supplies the default for `opt.Layout`,
and `Connect` runs before the workbook is parsed, so a wrong URL or a
rejected token fails in a second rather than after the whole sheet.

The unit of failure is the **row**, not the run. A cell that will not
coerce leaves its field unset and the row continues, unless the field is
wrapped in `Must`; a row that cannot be parsed at all is recorded and the
run goes on. Three things stop a run: the workbook will not open, the
header check fails, or the dataset's `Prepare` gives up. A cancelled
context ends the row loop early and returns what was done.

The header check is the one gate, and it runs before any parsing, because
a moved column is confidently wrong rather than visibly broken — the new
occupant is usually the same type and a plausible length, so it imports
cleanly as a value that is simply wrong. `Opt.SkipHeader` imports anyway,
with a warning.

`Table[T]` keeps the column letter, the accepted captions and the
coercion in one entry, so a column moves in a single edit:

```go
var table = importer.Table[Row]{
    importer.Must(importer.Text("B", "code", 50,
        func(r *Row) **string { return &r.Code },
        "Code", "Kode")),
    importer.Decimal("F", "amount", func(r *Row) **decimal.Decimal {
        return &r.Amount
    }, "Amount", "Nilai"),
}
```

`Table.Validate` is worth calling once at startup: every problem it
reports is a typo in a table nobody reads end to end, and each fails in a
way that looks like a problem with the workbook rather than with the
code.

`Register` is meant to be called from a dataset's own `init`, so adding
one is a new file and no edit to `main`. The registry is package state
with no lock, which is safe for that and nothing else, and a name
registered twice panics.

Keep the report `WriteReport` produces. For a dataset whose re-run
duplicates, its id column is the only record of what the run created.

## Files

### fileutil

Upload storage laid out per record, plus the checks that belong in front
of it.

```go
if fileutil.TooLarge(len(data)) {
    return fiber.NewError(413, "max "+fileutil.MaxFileLabel())
}
name := fileutil.SafeName(header.Filename)
if fileutil.NameTooLong(name) {
    return fiber.NewError(400, "name over "+fileutil.MaxNameLabel())
}
if !fileutil.HasAllowedExt(name) {
    return fiber.NewError(415, "unsupported file type")
}
stored, err := fileutil.Write(uploadDir, invoiceID, name, data)
```

`Write` enforces neither limit: the checks belong at the edge, where a
rejection can still be a 4xx rather than a half-finished upload. Writes
go through a temp file and a rename, so a concurrent reader sees the
complete old file or the complete new one and never a truncated prefix.
`CopyUnique` finds a free name rather than overwriting.

Extensions are allowlisted — PDF and the common image types by default;
`RegisterExts(OfficeExts)` adds the office formats and `RegisterExt` any
other — and `DetectContentType` will only report a type the allowlist
admits, so a `.pdf` full of text never comes back as `text/plain`. A text
format such as CSV also needs `RegisterContainer`, or a genuine file
sniffs as plain text and is recorded as `application/octet-stream`.
`MaxFileBytes` (20 MiB) and `MaxNameLen` (100 bytes) are variables;
`MaxNameLen` mirrors a database column width, so widen the column before
raising it.

### document

A one-pass index of a directory of documents, keyed both by full filename
and by stem, for the case where a reference in a spreadsheet says `1` and
the file on disk is `1.PDF`.

```go
idx, err := document.NewIndex(dir, true)
log.Info().Int("files", idx.Count()).
    Int("skipped", idx.Skipped()).Send()

name, data, err := idx.Load("1")
switch {
case errors.Is(err, document.ErrNotFound):
case errors.Is(err, document.ErrAmbiguous):
}
```

`ErrAmbiguous` is what happens when the same stem is stored as both a PDF
and a JPEG; the wrapped message names the candidates rather than picking
one. `Skipped` is worth logging: a directory of `.bak` files indexes as
empty, and saying so once beats every later reference reporting a miss.

The index accepts exactly the extensions `fileutil` does, so register any
extra types before building it. `ReadFile` reads an explicit path — a
resolved hyperlink, say — under the same extension gate and size cap,
with no index at all.

## Adapters

### logger

```go
log := logger.New(cfg.App.Env)
log = log.With().Str("service", "invoice").Logger()
```

Development gets a colourised console writer at debug level; staging and
production get JSON at info. Both write to **stdout**, because container
runtimes capture it by default and splitting the stream across two
descriptors makes access logs and application logs hard to correlate.

### datetime

A `time.Time` that accepts what browsers and drivers actually send —
RFC 3339 with or without fractional seconds, `datetime-local` with and
without seconds, a bare date, and the space-separated SQL timestamp — and
always marshals back as RFC 3339.

```go
type Invoice struct {
    IssuedAt *datetime.Datetime `db:"issued_at" json:"issued_at"`
}
```

It implements `sql.Scanner` and `driver.Valuer`, so sqlx reads and writes
time columns without a conversion step. Models use pointers, which gives
two routes to a JSON null — a nil pointer and a zero value — that
serialise identically and both write SQL NULL.

Input without an offset — `datetime-local`, a bare date, the SQL
timestamp — resolves in `time.Local`, which is why the process zone has
to match the driver's; see [Database](#database).

### whatsapp

A thin wrapper over `whatsmeow` with a pure-Go SQLite credential store.

```go
svc, err := whatsapp.New(log)
if err != nil {
    return err  // ErrNotPaired if the QR window closed
}
defer svc.Disconnect()

err = svc.SendText("628123456789", "Report is ready")
err = svc.SendDocument("628123456789", pdf,
    "report.pdf", "application/pdf", "Monthly report")
```

First run prints a QR code to stdout and waits to be paired — for
minutes, with nothing bounding the wait. The device credentials land in
`whatsapp.db` in the working directory, and later runs reconnect
silently. `NewContext` takes a context that bounds the wait and a store
path, so two services started from one directory need not share a
device. `ErrNotPaired` separates that first-run state from a broken
store or network, so a caller can carry on with notifications off.

A number starts with its country code. `+`, spaces and hyphens are
stripped, and anything else left over is `ErrInvalidPhone`; a number that
is empty once stripped is a silent no-op, so an unfilled column needs no
guard. `SendTextContext` and `SendDocumentContext` take a context, the
only bound on a send, and worth passing for a document, whose upload is
the slowest call here. Under `notification.whatsapp.async` no request is
left to end a send, so give it a deadline of its own:
`context.WithTimeout(context.WithoutCancel(ctx), time.Minute)`.

### email

Sends notification mail through an SMTP relay, one connection per
message, with the standard library's `net/smtp` underneath. Building the
service is shown under [Notification channels](#notification-channels).

```go
err = mailer.Send(ctx, email.Message{
    To:      []string{"Budi Santoso <budi@example.go.id>"},
    Cc:      []string{"arsip@example.go.id"},
    Subject: "Laporan realisasi Triwulan III",
    Text:    "Laporan terlampir.",
    Attachments: []email.Attachment{{
        Filename:    "realisasi.pdf",
        ContentType: "application/pdf",
        Data:        pdf,
    }},
})
err = mailer.SendText(ctx, "budi@example.go.id", "Subjek", "Isi")
```

Without any `notification.email.*` key, `New` returns `ErrNotConfigured`,
which is fatal where email is chosen. A nil `*Service` returns the same
error from every send rather than panic, so call sites need no guard.
`tls` is `starttls`, `implicit` or `none`, with no fallback: under
`starttls`, a relay that does not offer the upgrade fails the send.
Credentials go only over TLS or to a relay on loopback, with PLAIN or
LOGIN.

Every send is bounded by `notification.email.timeout` and by the
context. Under `notification.email.async`, send on
`context.WithoutCancel(ctx)` and build the `Message` from copies before
the goroutine starts, since Fiber reuses request memory once the handler
returns.

A nil error means the relay accepted the message; a missing mailbox is a
bounce, later. A refused recipient fails the whole send before anything
is uploaded, and the error unwraps to `*textproto.Error`: 4xx is worth
retrying, 5xx is not. `ErrInvalidAddress` marks a stored address that
does not parse. Each entry holds one address, never a list.

## Testing

```
go test ./...
```

Every package has a unit suite that needs no server and no configuration.
Integration suites sit behind a build tag and are skipped entirely
without it:

```
go test -tags integration -run Integration ./...
```

`database` and `datetime` share two DSN variables, and each test runs
once per engine whose DSN is set, skipping when neither is:

```
DB_TEST_MYSQL_DSN
    user:pass@tcp(host:3306)/scratch?parseTime=true&loc=Asia%2FJakarta
DB_TEST_SQLSERVER_DSN
    sqlserver://user:pass@host?database=scratch&timezone=Asia%2FJakarta
```

The zone parameters are for `datetime`, and they have to name the zone
the test process runs in (set `TZ` to be sure); a mismatch fails that
suite, since it is the misconfiguration the package exists to expose.
`database` passes with or without them.

These tests create and drop their own tables. Point the DSNs at a scratch
database, never at one holding data. `fileutil`'s integration suite also
carries a `unix` build constraint, so it runs only on Unix-like systems.

An integration file here does not mean "the same tests, now with real
I/O". The unit suites already write real workbooks, serve real HTTP and
use `t.TempDir()`. It means the claims that cannot be made without a
certificate, a clock, a second engine, a real listener, or two goroutines
racing — the ones a unit test would have to assume rather than assert.

## Conventions

- **Allowlist anything that reaches SQL text.** Client input picks from a
  list written in code; it never supplies an identifier.
- **Sentinel errors over message matching.** Callers branch with
  `errors.Is`, so a message can be reworded without breaking anyone.
- **Pointers for optional values.** A `nil` is "absent"; a zero is a
  value somebody chose. Cell coercion and query parameters both rely on
  the distinction.
- **Startup state is set once, at startup.** The SQL grammar and table
  prefix, the paging defaults, the upload limits and allowlist, and the
  import registry are process-wide. Most are plain variables read without
  synchronisation, so changing one while requests are in flight is a data
  race; `SetDialect` and the allowlist are synchronised, and changing
  them mid-flight is merely a bug.
- **The doc comment carries the reasoning.** What a function does is in
  the signature; why it does it that way, and what breaks if it changes,
  is in `doc.go` and above the declaration.