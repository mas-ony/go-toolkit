package database

// Table names: namespace qualification, quoting and per-year databases.
//
// Every table reference is prefixed with a namespace instead of being left
// bare. A bare name resolves against the connection's default database,
// which is right only by coincidence and wrong as soon as a request reads a
// database other than the one the DSN names. A qualified name is right
// regardless.
//
// # The prefix is rendered by the caller
//
// The two engines disagree on how many parts a qualified name has:
//
//	SQL Server  catalog.schema.table  three parts
//	MySQL       database.table        two parts; a third is a syntax error
//
// Both read the leading segment as the database, and both let a query name
// a database other than the one the session opened; per-year scoping below
// depends on that. The prefix is therefore rendered by the code that knows
// the driver, typically the configuration layer, and passed to Configure.
// This file only joins it to a table name:
//
//	SQL Server  "app2026.dbo"  ->  app2026.dbo.invoice
//	MySQL       "app2026"      ->  app2026.invoice
//
// The prefix is used as given. Each segment must be a valid bare identifier,
// and the first must be the database itself, not a linked server.
//
// # Table names that cannot appear bare
//
// Qualify quotes a table name, through QuoteIdent in dialect.go, when the
// name would not parse bare on both engines: when it is a reserved word on
// either, or not a plain ASCII identifier. user is reserved in T-SQL but not
// in MySQL, and rank the other way round. Every other name stays bare, which
// keeps logged SQL readable, and callers always pass the plain name.
//
// # Per-year databases
//
// A deployment may keep one database per year, app2026 beside app2025, and
// let each request choose the year it reads. NamespaceForYear rewrites the
// database segment of a prefix for that year, which keeps the two halves of
// the naming problem apart:
//
//	configuration     where the tables live  "app2026.dbo"  per deployment
//	NamespaceForYear  which year to read     "app2025.dbo"  per request
//
// The year cannot be a configuration value, which stays fixed for the life
// of the process while one process serves users reading different years.
//
// The intended repository stores the base prefix and the year separately,
// applies NamespaceForYear whenever it builds a query, and offers WithYear
// and WithNamespace beside WithTx. Each returns a scoped copy, so the three
// chain in any order and a second WithYear replaces the first. Anything that
// runs before a year is known, such as authentication, uses the base prefix.

import (
	"strconv"
	"strings"
)

// tsqlReserved lists the reserved keywords of SQL Server and Azure Synapse
// Analytics, from Microsoft's "Reserved keywords (Transact-SQL)" page. The
// ODBC and future keywords listed on that page are not reserved and are left
// out.
const tsqlReserved = `
ADD ALL ALTER AND ANY AS ASC AUTHORIZATION BACKUP BEGIN BETWEEN BREAK BROWSE
BULK BY CASCADE CASE CHECK CHECKPOINT CLOSE CLUSTERED COALESCE COLLATE COLUMN
COMMIT COMPUTE CONSTRAINT CONTAINS CONTAINSTABLE CONTINUE CONVERT CREATE CROSS
CURRENT CURRENT_DATE CURRENT_TIME CURRENT_TIMESTAMP CURRENT_USER CURSOR
DATABASE DBCC DEALLOCATE DECLARE DEFAULT DELETE DENY DESC DISK DISTINCT
DISTRIBUTED DOUBLE DROP DUMP ELSE END ERRLVL ESCAPE EXCEPT EXEC EXECUTE EXISTS
EXIT EXTERNAL FETCH FILE FILLFACTOR FOR FOREIGN FREETEXT FREETEXTTABLE FROM
FULL FUNCTION GOTO GRANT GROUP HAVING HOLDLOCK IDENTITY IDENTITYCOL
IDENTITY_INSERT IF IN INDEX INNER INSERT INTERSECT INTO IS JOIN KEY KILL LEFT
LIKE LINENO LOAD MERGE NATIONAL NOCHECK NONCLUSTERED NOT NULL NULLIF OF OFF
OFFSETS ON OPEN OPENDATASOURCE OPENQUERY OPENROWSET OPENXML OPTION OR ORDER
OUTER OVER PERCENT PIVOT PLAN PRECISION PRIMARY PRINT PROC PROCEDURE PUBLIC
RAISERROR READ READTEXT RECONFIGURE REFERENCES REPLICATION RESTORE RESTRICT
RETURN REVERT REVOKE RIGHT ROLLBACK ROWCOUNT ROWGUIDCOL RULE SAVE SCHEMA
SECURITYAUDIT SELECT SEMANTICKEYPHRASETABLE SEMANTICSIMILARITYDETAILSTABLE
SEMANTICSIMILARITYTABLE SESSION_USER SET SETUSER SHUTDOWN SOME STATISTICS
SYSTEM_USER TABLE TABLESAMPLE TEXTSIZE THEN TO TOP TRAN TRANSACTION TRIGGER
TRUNCATE TRY_CONVERT TSEQUAL UNION UNIQUE UNPIVOT UPDATE UPDATETEXT USE USER
VALUES VARYING VIEW WAITFOR WHEN WHERE WHILE WITH WRITETEXT
`

// mysqlReserved lists the words MySQL 8.0.46 reports as reserved; run this
// query on a newer server to refresh it:
//
//	SELECT WORD FROM INFORMATION_SCHEMA.KEYWORDS WHERE RESERVED = 1
//
// Later MySQL releases reserve further words, and MariaDB reserves some that
// MySQL does not. Either gap affects only a bare name, because both engines
// read a word after a period as an identifier.
const mysqlReserved = `
ACCESSIBLE ADD ALL ALTER ANALYZE AND AS ASC ASENSITIVE BEFORE BETWEEN BIGINT
BINARY BLOB BOTH BY CALL CASCADE CASE CHANGE CHAR CHARACTER CHECK COLLATE
COLUMN CONDITION CONSTRAINT CONTINUE CONVERT CREATE CROSS CUBE CUME_DIST
CURRENT_DATE CURRENT_TIME CURRENT_TIMESTAMP CURRENT_USER CURSOR DATABASE
DATABASES DAY_HOUR DAY_MICROSECOND DAY_MINUTE DAY_SECOND DEC DECIMAL DECLARE
DEFAULT DELAYED DELETE DENSE_RANK DESC DESCRIBE DETERMINISTIC DISTINCT
DISTINCTROW DIV DOUBLE DROP DUAL EACH ELSE ELSEIF EMPTY ENCLOSED ESCAPED
EXCEPT EXISTS EXIT EXPLAIN FALSE FETCH FIRST_VALUE FLOAT FLOAT4 FLOAT8 FOR
FORCE FOREIGN FROM FULLTEXT FUNCTION GENERATED GET GRANT GROUP GROUPING GROUPS
HAVING HIGH_PRIORITY HOUR_MICROSECOND HOUR_MINUTE HOUR_SECOND IF IGNORE IN
INDEX INFILE INNER INOUT INSENSITIVE INSERT INT INT1 INT2 INT3 INT4 INT8
INTEGER INTERSECT INTERVAL INTO IO_AFTER_GTIDS IO_BEFORE_GTIDS IS ITERATE JOIN
JSON_TABLE KEY KEYS KILL LAG LAST_VALUE LATERAL LEAD LEADING LEAVE LEFT LIKE
LIMIT LINEAR LINES LOAD LOCALTIME LOCALTIMESTAMP LOCK LONG LONGBLOB LONGTEXT
LOOP LOW_PRIORITY MASTER_BIND MASTER_SSL_VERIFY_SERVER_CERT MATCH MAXVALUE
MEDIUMBLOB MEDIUMINT MEDIUMTEXT MIDDLEINT MINUTE_MICROSECOND MINUTE_SECOND MOD
MODIFIES NATURAL NOT NO_WRITE_TO_BINLOG NTH_VALUE NTILE NULL NUMERIC OF ON
OPTIMIZE OPTIMIZER_COSTS OPTION OPTIONALLY OR ORDER OUT OUTER OUTFILE OVER
PARTITION PERCENT_RANK PRECISION PRIMARY PROCEDURE PURGE RANGE RANK READ READS
READ_WRITE REAL RECURSIVE REFERENCES REGEXP RELEASE RENAME REPEAT REPLACE
REQUIRE RESIGNAL RESTRICT RETURN REVOKE RIGHT RLIKE ROW ROWS ROW_NUMBER SCHEMA
SCHEMAS SECOND_MICROSECOND SELECT SENSITIVE SEPARATOR SET SHOW SIGNAL SMALLINT
SPATIAL SPECIFIC SQL SQLEXCEPTION SQLSTATE SQLWARNING SQL_BIG_RESULT
SQL_CALC_FOUND_ROWS SQL_SMALL_RESULT SSL STARTING STORED STRAIGHT_JOIN SYSTEM
TABLE TERMINATED THEN TINYBLOB TINYINT TINYTEXT TO TRAILING TRIGGER TRUE UNDO
UNION UNIQUE UNLOCK UNSIGNED UPDATE USAGE USE USING UTC_DATE UTC_TIME
UTC_TIMESTAMP VALUES VARBINARY VARCHAR VARCHARACTER VARYING VIRTUAL WHEN WHERE
WHILE WINDOW WITH WRITE XOR YEAR_MONTH ZEROFILL
`

// DefaultNamespace is the prefix every repository starts from: constructors
// copy it into the repository they return. Configure sets it.
//
// It is empty until then, so a process that never calls Configure emits bare
// table names that resolve against the connection's default database. Any
// other default would have to name a database in source code and spell one
// engine's grammar; "app.dbo" is already a syntax error on MySQL.
var DefaultNamespace = ""

// reservedWords is the set of words reserved in T-SQL or in MySQL, in upper
// case. Keywords that are not reserved, such as STATUS and NAME, are absent,
// since both engines accept them bare.
var reservedWords = wordSet(tsqlReserved, mysqlReserved)

// wordSet builds a set from lists of words separated by white space.
func wordSet(lists ...string) map[string]struct{} {
	set := make(map[string]struct{})
	for _, list := range lists {
		for _, word := range strings.Fields(list) {
			set[word] = struct{}{}
		}
	}
	return set
}

// isPlainName reports whether name is an ASCII letter or underscore followed
// by ASCII letters, digits and underscores. Such a name is a regular
// identifier on both engines. Anything else is quoted, even where one engine
// would accept it bare under its own wider rules.
func isPlainName(name string) bool {
	if name == "" {
		return false
	}
	for i := 0; i < len(name); i++ {
		c := name[i]
		switch {
		case c == '_', 'a' <= c && c <= 'z', 'A' <= c && c <= 'Z':
		case i > 0 && '0' <= c && c <= '9':
		default:
			return false
		}
	}
	return true
}

// needsQuoting reports whether a table name has to be quoted to parse on
// both engines. A name may stay bare only if isPlainName accepts it and its
// upper-case form is not in reservedWords.
//
// SQL Server requires a reserved word to be quoted wherever it appears.
// MySQL and MariaDB read any word that follows a period as an identifier, so
// on those engines the quoting matters only when the namespace is empty.
func needsQuoting(name string) bool {
	if !isPlainName(name) {
		return true
	}
	_, reserved := reservedWords[strings.ToUpper(name)]
	return reserved
}

// trimYearSuffix removes a year from the end of a database name: exactly four
// trailing ASCII digits, preceded by a non-digit or by nothing.
//
//	"app2026"   ->  "app"       four trailing digits
//	"app"       ->  "app"       no trailing digits
//	"app_v2"    ->  "app_v2"    one digit is not a year
//	"app12026"  ->  "app12026"  five digits are not a year either
//	"app1234"   ->  "app"       any four digits count
//
// The digits are not range-checked, because a plausibility range would only
// move the guess. A database whose name ends in four digits that are not a
// year cannot be scoped by NamespaceForYear and needs renaming.
//
// Indexing bytes instead of runes is exact here. UTF-8 never uses the bytes
// of ASCII digits inside a multi-byte rune, so a byte that looks like a digit
// is one, and any other byte counts as a non-digit.
func trimYearSuffix(database string) string {
	const yearLen = 4
	n := len(database)
	if n < yearLen {
		return database
	}
	for i := n - yearLen; i < n; i++ {
		if database[i] < '0' || database[i] > '9' {
			return database
		}
	}
	if n > yearLen {
		if c := database[n-yearLen-1]; c >= '0' && c <= '9' {
			return database
		}
	}
	return database[:n-yearLen]
}

// Configure sets DefaultNamespace for every repository constructed after it.
// Call it once at startup, beside SetDialect and before the first repository
// is constructed:
//
//	database.Configure(prefix)
//
// Constructors copy the value, so a later call leaves existing repositories
// on the old prefix. Nothing synchronizes it, so calling it while
// constructors run on other goroutines is a data race.
//
// One prefix per process is a default, not a limit. A repository that reads
// another namespace, in a test or from a second database, receives it
// through its WithNamespace method.
//
// An empty value means bare table names. A deployment that must always
// qualify should reject an empty prefix where it reads its configuration.
func Configure(namespace string) {
	DefaultNamespace = namespace
}

// Qualify joins a namespace prefix and a table name, quoting the table first
// when needsQuoting reports that it cannot appear bare:
//
//	"app2026.dbo", "invoice"  ->  app2026.dbo.invoice
//	"app2026", "user"         ->  app2026.`user` on MySQL
//	"", "invoice"             ->  invoice
//
// Neither argument may come from a request. Both are identifiers, which
// cannot be bound parameters, so they are concatenated into the query text;
// the prefix must trace back to configuration or to an int year, and the
// table name must be written in code.
//
// The dialect is resolved ONLY when the table name has to be quoted, which
// is a tested property rather than an accident of the order of the lines
// below. A repository built from plain names therefore works before
// SetDialect has run, and only a name that cannot appear bare makes
// qualification depend on the grammar. Qualify on a Dialect takes the
// grammar as its receiver and never depends on the selection at all.
func Qualify(namespace, table string) string {
	if !needsQuoting(table) {
		return joinNamespace(namespace, table)
	}
	return joinNamespace(namespace, QuoteIdent(table))
}

// Qualify is Qualify in the grammar d rather than the selected one.
//
// Only the QUOTING differs between the two grammars. Which names need
// quoting does not: needsQuoting answers for both engines at once, so a
// name is quoted on MySQL because T-SQL reserves it and the other way
// round, and the same table resolves under either grammar.
//
// d is read only when the name needs quoting, matching Qualify, so a plain
// name is joined without d having to be a grammar at all.
func (d Dialect) Qualify(namespace, table string) string {
	if !needsQuoting(table) {
		return joinNamespace(namespace, table)
	}
	return joinNamespace(namespace, d.QuoteIdent(table))
}

// joinNamespace prefixes an already-quoted table name with a namespace, or
// returns it alone when there is none.
func joinNamespace(namespace, table string) string {
	if namespace == "" {
		return table
	}
	return namespace + "." + table
}

// NamespaceForYear rewrites the database segment of a prefix to name year:
//
//	SQL Server  "app2026.dbo" + 2025  ->  "app2025.dbo"
//	MySQL       "app2026"     + 2025  ->  "app2025"
//	either      "app.dbo"     + 2025  ->  "app2025.dbo"
//
// Only the first segment takes the year. Appending to the whole prefix would
// produce "app2026.dbo2025": a schema that does not exist, in a database
// that is still the configured year's.
//
// A year already ending the database name is replaced rather than extended,
// so "app2026" becomes "app2025", not "app20262025", and a configured name
// works with or without a year. trimYearSuffix defines what counts as one:
// four trailing digits after a non-digit. Applied again to its own result,
// the function changes nothing as long as that result still ends in such a
// year, which two kinds of input prevent. A year of another width gives a
// name such as "app25", so validate years where they enter the program. A
// name that already ends in a digit, such as "app_v2", gives "app_v22025",
// whose five trailing digits are not a year; such names need a separator
// before the year, as in "app_v2_2026".
//
// year <= 0 selects no year and returns the prefix unchanged. An empty prefix
// is returned unchanged too, because it has no database name to rewrite, and
// that failure is silent: a request for another year reads the connection's
// default database. Per-year scoping needs a configured prefix.
//
// year is an int so that the result cannot carry injected text. The prefix
// is concatenated into queries, and a positive int renders as digits only.
func NamespaceForYear(namespace string, year int) string {
	if namespace == "" || year <= 0 {
		return namespace
	}
	database, schema, hasSchema := strings.Cut(namespace, ".")
	database = trimYearSuffix(database) + strconv.Itoa(year)
	if !hasSchema {
		return database
	}
	return database + "." + schema
}
