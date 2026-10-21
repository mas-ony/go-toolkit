// Package dbdriver holds the two vocabularies config validates and database
// connects with: the names database.driver accepts and the engine each one
// selects, and the values database.encrypt accepts and what each one asks
// of each engine's driver.
//
// They live in one place because the two packages have to agree, not
// merely resemble each other. A driver name config.DatabaseConfig.Validate
// accepts has to be one database.New can open, and an encrypt value it
// accepts has to be one database.New can translate for the driver it
// opens; a value valid in one package and unknown in the other would pass
// the startup check and then fail the connection. Two copies of a
// vocabulary agree only until one of them is edited, and nothing reports
// the moment they stop.
//
// Each engine is named by the database/sql driver name database opens it
// under, so the name an engine is compared by and the name sqlx is handed
// are one constant. That is not a coincidence to undo: sqlx picks a
// placeholder style from the driver name, and only "sqlserver" has one on
// the SQL Server side, which is why a configured "mssql" is opened as
// "sqlserver" too. The database package documentation has the detail.
//
// Both lookups ignore case and surrounding space, the way every configured
// word is read, and nothing else: a spelling outside the vocabulary is not
// a value, however a driver would have read it.
//
// The package is internal because none of it is a contract with a
// service. It is the toolkit agreeing with itself, and its API moves
// whenever a caller needs it to.
package dbdriver
