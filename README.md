# go-toolkit

Shared Go packages for the HTTP services in this stack: typed
configuration, a SQL layer that speaks both MySQL and SQL Server,
spreadsheet reading and import, upload handling, and the request and
response shapes every service uses.

Everything here is a library. Nothing starts a server, reads a config
file, or owns process state beyond the two startup switches documented
under [Database](#database).

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

`config` turns a `*viper.Viper` into typed sections. It does not build
the Viper, read a file, or decide where configuration lives — that stays
in the application, so a service can load from wherever it likes and
still get the same validated structs.

Sections, and the key prefix each is nested behind:

| Section        | Prefix         | Covers                            |
| -------------- | -------------- | --------------------------------- |
| App            | `app`          | Name, version, env, host, port    |
| Database       | `database`     | Driver, host, credentials, pool   |
| Notification   | `notification` | Sync or async dispatch            |
| Fiber          | `fiber`        | Server and router settings        |
| Auth           | `fiber.auth`   | Auth mode                         |
| JWT            | `fiber.jwt`    | Secret and expiry                 |
| Client         | `fiber.client` | Outbound base URL, token, retries |
| Limiter        | `fiber.limiter`| Rate limit max and window         |
| Listen         | `fiber.listen` | TLS, prefork, shutdown timeout    |
| Recover        | `fiber.recover`| Stack traces on panic             |
| RequestID      | `fiber.requestid` | Correlation header            |
| Session        | `fiber.session`| Cookie flags and timeouts         |
| Zerolog        | `fiber.zerolog`| Access log fields and levels      |

Every section implements `SectionConfig` — `Validate() error` and
`String() string` — so one list drives both the startup check and the
startup log line.

### Environment spelling

A key's environment name is its prefix uppercased with dots replaced by
underscores: `fiber.limiter.max` is `FIBER_LIMITER_MAX`. The application
wires that with Viper's `AutomaticEnv` and an env key replacer; the
constructor functions in this package are the authoritative list of
which keys exist at all.

### Only what a deployment supplies

`SuppliedSections` reports which sections some source actually supplied a
key for, reading both the file and the process environment. Swapping an
unsupplied section for `AbsentSection` spares it validation and keeps its
zero values out of the startup log, where `expiration=0s` reads as a
setting somebody chose rather than as no setting at all.

```go
v := viper.New()
v.SetConfigName("config")
v.SetConfigType("yaml")
v.AddConfigPath(".")
v.AutomaticEnv()
v.SetEnvKeyReplacer(strings.NewReplacer(".", "_"))
_ = v.ReadInConfig()

sections := []config.Section{
    {Name: "app", Prefix: "app",
        Value: config.NewAppConfig(v)},
    {Name: "database", Prefix: "database",
        Value: config.NewDatabaseConfig(v)},
    {Name: "limiter", Prefix: "fiber.limiter",
        Value: config.NewLimiterConfig(v)},
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

A list value may be written as a YAML sequence or as a comma-separated
string, and both parse the same way. That is not cosmetic: Viper splits a
bare environment string on whitespace, so `FIBER_REQUEST_METHODS=GET,POST`
would otherwise register one method nobody sends and answer 405 to
everything.

### Example config.yaml

```yaml
app:
  name: example-service
  version: 1.0.0
  env: development        # development | staging | production
  host: 0.0.0.0
  port: 3000
  location: Asia/Jakarta
  upload_dir: ./uploads

database:
  driver: sqlserver       # sqlserver | mssql | mysql
  host: 127.0.0.1
  port: 1433
  username: sa
  password: secret
  catalog: app2026
  schema: dbo
  max_open_conns: 25
  max_idle_conns: 5
  conn_max_lifetime: 30m
  conn_max_idle_time: 5m

fiber:
  body_limit: 4194304
  read_timeout: 10s
  write_timeout: 10s
  limiter:
    max: 100
    expiration: 1m
  listen:
    shutdown_timeout: 10s
  zerolog:
    fields: status,method,latency
```

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

`SetDialect` and `Configure` are process-wide, set once at startup before
the first repository is constructed. Nothing synchronises them, so
calling either while requests are in flight is a data race.

### Two engines, one query

Every helper has a package-level form that reads the selected grammar and
a method form on `Dialect` that takes it as a receiver. The package-level
form suits a service that talks to one engine; the method form is what
lets a migration or a sync job spell SQL for both in one process, and
what lets a test cover both grammars without touching global state.

|                   | MySQL                  | SQL Server                  |
| ----------------- | ---------------------- | --------------------------- |
| row cap           | `LIMIT n`              | `TOP (n)`                   |
| page              | `LIMIT … OFFSET …`     | `OFFSET … FETCH NEXT …`     |
| server clock      | `NOW()`                | `SYSDATETIME()`             |
| recursive CTE     | `WITH RECURSIVE x AS`  | `WITH x AS`                 |
| quoted identifier | `` `name` ``           | `[name]`                    |
| generated key     | `LastInsertId()`       | `OUTPUT INSERTED.pk`        |

`ApplyLimit`, `PageClause`, `NowExpr`, `RecursiveCTE`, `QuoteIdent`,
`InsertReturningID` and `Qualify` cover those rows. Anything else a query
builder writes has to be spelled identically on both.

### Table names

Table references are qualified rather than left bare, because a bare name
resolves against the connection's default database and is right only by
coincidence. The prefix is rendered by whoever knows the driver — three
parts on SQL Server, two on MySQL — and `Qualify` joins it to the name,
quoting only when the name would not parse bare on both engines (`user`
is reserved in T-SQL, `rank` in MySQL).

```go
database.Qualify("app2026.dbo", "invoice")  // app2026.dbo.invoice
database.Qualify("app2026", "invoice")      // app2026.invoice
database.NamespaceForYear("app2026.dbo", 2025)
```

### Clause builders and allowlists

Client input never reaches the query text directly. `SelectClause`,
`SortClause` and `GroupClause` each take an allowlist, and a column not
in it is dropped rather than rejected — an unknown sort column yields the
fallback order, not a 400.

```go
allowed := map[string]string{
    "code": "t.code",
    "name": "t.name",
}

page, limit := request.Page(c)
args := map[string]any{}

q := "SELECT " + database.SelectClause(
    request.Cols(c), allowed, []string{"code"}, "id",
) + "\nFROM " + database.Qualify(ns, "invoice") + " t"
q += database.SortClause(
    request.Sort(c), []string{"t.code", "t.name"}, "t.id",
)
q += database.PageClause(args, page, limit)

var rows []Invoice
err := database.SelectList(ctx, db, q, args, &rows)
```

Each fragment carries its own leading newline and keyword, so they
concatenate in order without any spacing logic at the call site.
`GetOne` returns `(found bool, err error)` rather than making the caller
match on `sql.ErrNoRows`; `CountQuery` runs the paired total.

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

## HTTP

### request

Parameter helpers for Fiber v3 handlers. Each returns either a usable
value or a `*fiber.Error` the caller returns unchanged.

```go
id, err := request.ID(c, "id")        // /items/:id, positive int
ids := request.IDs(c, "ids")          // comma-separated list
page, limit := request.Page(c)        // clamped to MaxLimit
cols := request.Cols(c)               // sparse field selection
sort := request.Sort(c)               // "t.created_at:desc"
name := request.StringPtr(c, "name")  // nil when absent
```

`DefaultPage`, `DefaultLimit` and `MaxLimit` are package variables. A
service that serves wider pages assigns them at startup, before the first
request. Values returned by the list helpers are cloned, because the
pieces would otherwise point into a request buffer the framework recycles
under keep-alive.

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

Retries back off from 500ms, doubling to a ceiling of 8s, with no jitter.
A non-2xx yields an `*httpclient.Error` carrying the status, the path and
whatever message the envelope held; `errors.Is` reaches the sentinels for
the statuses worth branching on. Error bodies are capped on the way out,
and the message carried on the error is capped shorter still.

## Spreadsheets

### xlsx

Reads a workbook against a layout the caller declares, over
`xuri/excelize`.

```go
wb, err := xlsx.Open(path, xlsx.Layout{
    Sheet:     "Data",
    HeaderRow: 6,   // GroupRow defaults to the row above
    FirstRow:  8,
})
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

Coercion returns pointers, so an unparseable cell is `nil` rather than a
zero that looks like data. `Link` and `ResolveLink` resolve a hyperlink
against the workbook's own directory, not the working directory.

### importer

The half of a spreadsheet import that is the same every time: open, check
the header, parse, hand each row to whatever writes it. What a row means
and where it goes is a `Dataset`, implemented once per workbook shape.

```go
ds, _ := importer.Lookup(*dataset)
run := &importer.Run{
    Log: log, Opt: opt, Count: &importer.Counters{},
}
outcomes, err := importer.Execute(ctx, ds, path, run)
importer.PrintSummary(os.Stdout, path, ds, run, outcomes)
_ = importer.WriteReport(*report, ds, outcomes)
```

The unit of failure is the **row**, not the run. A cell that will not
coerce leaves its field unset and the row continues; a row that cannot be
parsed at all is recorded and the run goes on. Three things stop a run:
the workbook will not open, the header check fails, or the dataset's
`Prepare` gives up.

The header check is the one gate, and it runs before any parsing, because
a moved column is confidently wrong rather than visibly broken — the new
occupant is usually the same type and a plausible length, so it imports
cleanly as a value that is simply wrong.

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

`Validate` is worth calling once at startup: every problem it reports is
a typo in a table nobody reads end to end, and each fails in a way that
looks like a problem with the workbook rather than with the code.

`Register` is meant to be called from a dataset's own `init`, so adding
one is a new file and no edit to `main`. The registry is package state
with no lock, which is safe for that and nothing else.

## Files

### fileutil

Upload storage laid out per record, plus the checks that belong in front
of it.

```go
if fileutil.TooLarge(len(data)) {
    return fiber.NewError(413, "max "+fileutil.MaxFileLabel())
}
name := fileutil.SafeName(header.Filename)
if !fileutil.HasAllowedExt(name) {
    return fiber.NewError(415, "unsupported file type")
}
stored, err := fileutil.Write(uploadDir, invoiceID, name, data)
```

Writes go through a temp file and a rename, so a concurrent reader sees
the complete old file or the complete new one and never a truncated
prefix. `CopyUnique` finds a free name rather than overwriting.

Extensions are allowlisted — PDF and the common image types by default,
with `OfficeExts` and `RegisterExt` for the rest — and `DetectContentType`
will only report a type the allowlist admits, so a `.pdf` full of text
never comes back as `text/plain`. `MaxFileBytes` (20 MiB) and
`MaxNameLen` (100 bytes) are variables; `MaxNameLen` mirrors a database
column width, so widen the column before raising it.

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

First run prints a QR code to stdout and waits to be paired; the device
credentials land in `whatsapp.db` and later runs reconnect silently.
Phone numbers are digits only — no `+`, no spaces — and
`ErrInvalidPhone` says so. `SendTextContext` and `SendDocumentContext`
take a context.

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
once per engine whose DSN is set:

```
DB_TEST_MYSQL_DSN=user:pass@tcp(host:3306)/scratch
DB_TEST_SQLSERVER_DSN=sqlserver://user:pass@host?database=scratch
```

These tests create and drop their own tables. Point the DSNs at a scratch
database, never at one holding data. `fileutil`'s integration suite is
additionally tagged `unix`.

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
- **Startup state is set once, at startup.** `SetDialect`, `Configure`
  and the limit variables are read without synchronisation on the hot
  path.
- **The doc comment carries the reasoning.** What a function does is in
  the signature; why it does it that way, and what breaks if it changes,
  is in `doc.go` and above the declaration.
